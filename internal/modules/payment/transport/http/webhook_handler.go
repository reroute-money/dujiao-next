package paymenthttp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	paymentdomain "github.com/dujiao-next/internal/modules/payment/domain"

	ginutil "github.com/dujiao-next/internal/platform/http/ginutil"

	"github.com/dujiao-next/internal/constants"
	"github.com/dujiao-next/internal/i18n"
	"github.com/dujiao-next/internal/platform/http/response"
	"github.com/dujiao-next/internal/shared/jsonmap"

	"github.com/gin-gonic/gin"
)

const maxWebhookBodyBytes = 1 << 20

// WebhookCallbackInput webhook 回调输入。
type WebhookCallbackInput struct {
	ChannelID uint
	Headers   map[string]string
	Body      []byte
	Context   context.Context
}

// PaymentWebhookService webhook 处理端口。
type PaymentWebhookService interface {
	HandlePaypalWebhook(input WebhookCallbackInput) (*paymentdomain.Payment, string, error)
	HandleStripeWebhook(input WebhookCallbackInput) (*paymentdomain.Payment, string, error)
	HandleDujiaoPayWebhook(input WebhookCallbackInput) (*paymentdomain.Payment, string, error)
	HandleRerouteMoneyWebhook(input WebhookCallbackInput) (*paymentdomain.Payment, string, error)
}

// ExceptionAlerter 支付异常告警入队端口。
type ExceptionAlerter interface {
	EnqueuePaymentExceptionAlert(method, path, clientIP string, data jsonmap.JSON) error
}

// PaypalWebhookQuery PayPal webhook 查询参数。
type PaypalWebhookQuery struct {
	ChannelID uint `form:"channel_id" binding:"required"`
}

// StripeWebhookQuery Stripe webhook 查询参数。
type StripeWebhookQuery struct {
	ChannelID uint `form:"channel_id"`
}

// DujiaoPayWebhookQuery DujiaoPay webhook 查询参数。
type DujiaoPayWebhookQuery struct {
	ChannelID uint `form:"channel_id"`
}

// RerouteMoneyWebhookQuery reroute-money 推送查询参数 | channel_id 缺失时由验签辨认渠道。
type RerouteMoneyWebhookQuery struct {
	ChannelID uint `form:"channel_id"`
}

// WebhookHandler 处理支付 webhook HTTP。
type WebhookHandler struct {
	webhooks PaymentWebhookService
	alerts   ExceptionAlerter
}

func NewWebhookHandler(webhooks PaymentWebhookService, alerts ExceptionAlerter) *WebhookHandler {
	if webhooks == nil {
		panic("payment webhook handler: webhooks is nil")
	}
	return &WebhookHandler{webhooks: webhooks, alerts: alerts}
}

// PaypalWebhook PayPal webhook 回调。
func (h *WebhookHandler) PaypalWebhook(c *gin.Context) {
	log := ginutil.RequestLog(c)
	var query PaypalWebhookQuery
	if err := c.ShouldBindQuery(&query); err != nil {
		log.Warnw("paypal_webhook_query_invalid", "error", err)
		ginutil.RespondError(c, response.CodeBadRequest, "error.bad_request", err)
		return
	}
	body, err := readWebhookBody(c)
	if err != nil {
		log.Warnw("paypal_webhook_body_read_failed", "channel_id", query.ChannelID, "error", err)
		ginutil.RespondError(c, response.CodeBadRequest, "error.bad_request", err)
		return
	}
	log.Infow("paypal_webhook_received",
		"channel_id", query.ChannelID,
		"client_ip", c.ClientIP(),
		"body_size", len(body),
		"paypal_transmission_id", strings.TrimSpace(c.GetHeader("Paypal-Transmission-Id")),
		"paypal_transmission_time", strings.TrimSpace(c.GetHeader("Paypal-Transmission-Time")),
		"paypal_auth_algo", strings.TrimSpace(c.GetHeader("Paypal-Auth-Algo")),
	)
	payment, eventType, err := h.webhooks.HandlePaypalWebhook(WebhookCallbackInput{
		ChannelID: query.ChannelID,
		Headers:   collectRequestHeaders(c),
		Body:      body,
		Context:   c.Request.Context(),
	})
	if err != nil {
		log.Warnw("paypal_webhook_handle_failed",
			"channel_id", query.ChannelID,
			"event_type", eventType,
			"error", err,
		)
		h.enqueuePaymentExceptionAlert(c, jsonmap.JSON{
			"alert_type":  "paypal_webhook_handle_failed",
			"alert_level": "error",
			"message":     strings.TrimSpace(err.Error()),
			"provider":    constants.PaymentChannelTypePaypal,
		})
		respondPaymentCallbackError(c, err)
		return
	}
	respondWebhookSuccess(c, log, "paypal_webhook", query.ChannelID, eventType, payment)
}

// StripeWebhook Stripe webhook 回调。
func (h *WebhookHandler) StripeWebhook(c *gin.Context) {
	log := ginutil.RequestLog(c)
	var query StripeWebhookQuery
	_ = c.ShouldBindQuery(&query)

	body, err := readWebhookBody(c)
	if err != nil {
		log.Warnw("stripe_webhook_body_read_failed", "channel_id", query.ChannelID, "error", err)
		ginutil.RespondError(c, response.CodeBadRequest, "error.bad_request", err)
		return
	}
	log.Infow("stripe_webhook_received",
		"channel_id", query.ChannelID,
		"client_ip", c.ClientIP(),
		"body_size", len(body),
	)

	payment, eventType, err := h.webhooks.HandleStripeWebhook(WebhookCallbackInput{
		ChannelID: query.ChannelID,
		Headers:   collectRequestHeaders(c),
		Body:      body,
		Context:   c.Request.Context(),
	})
	if err != nil {
		log.Warnw("stripe_webhook_handle_failed",
			"channel_id", query.ChannelID,
			"event_type", eventType,
			"error", err,
		)
		h.enqueuePaymentExceptionAlert(c, jsonmap.JSON{
			"alert_type":  "stripe_webhook_handle_failed",
			"alert_level": "error",
			"message":     strings.TrimSpace(err.Error()),
			"provider":    constants.PaymentChannelTypeStripe,
		})
		respondPaymentCallbackError(c, err)
		return
	}
	respondWebhookSuccess(c, log, "stripe_webhook", query.ChannelID, eventType, payment)
}

// DujiaoPayWebhook DujiaoPay webhook 回调。
func (h *WebhookHandler) DujiaoPayWebhook(c *gin.Context) {
	log := ginutil.RequestLog(c)
	var query DujiaoPayWebhookQuery
	_ = c.ShouldBindQuery(&query)

	body, err := readWebhookBody(c)
	if err != nil {
		log.Warnw("dujiaopay_webhook_body_read_failed", "channel_id", query.ChannelID, "error", err)
		ginutil.RespondError(c, response.CodeBadRequest, "error.bad_request", err)
		return
	}
	log.Infow("dujiaopay_webhook_received",
		"channel_id", query.ChannelID,
		"client_ip", c.ClientIP(),
		"body_size", len(body),
		"dujiaopay_webhook_id", strings.TrimSpace(c.GetHeader("DJP-Webhook-ID")),
		"dujiaopay_webhook_timestamp", strings.TrimSpace(c.GetHeader("DJP-Webhook-Timestamp")),
	)

	payment, eventType, err := h.webhooks.HandleDujiaoPayWebhook(WebhookCallbackInput{
		ChannelID: query.ChannelID,
		Headers:   collectRequestHeaders(c),
		Body:      body,
		Context:   c.Request.Context(),
	})
	if err != nil {
		log.Warnw("dujiaopay_webhook_handle_failed",
			"channel_id", query.ChannelID,
			"event_type", eventType,
			"error", err,
		)
		h.enqueuePaymentExceptionAlert(c, jsonmap.JSON{
			"alert_type":  "dujiaopay_webhook_handle_failed",
			"alert_level": "error",
			"message":     strings.TrimSpace(err.Error()),
			"provider":    constants.PaymentProviderDujiaoPay,
		})
		respondPaymentCallbackError(c, err)
		return
	}
	respondWebhookSuccess(c, log, "dujiaopay_webhook", query.ChannelID, eventType, payment)
}

// RerouteMoneyWebhook 处理 Reroute Money 订单推送。
func (h *WebhookHandler) RerouteMoneyWebhook(c *gin.Context) {
	log := ginutil.RequestLog(c)
	var query RerouteMoneyWebhookQuery
	_ = c.ShouldBindQuery(&query)

	body, err := readWebhookBody(c)
	if err != nil {
		log.Warnw("reroute_money_webhook_body_read_failed", "channel_id", query.ChannelID, "error", err)
		respondRerouteMoneyWebhookFailure(c, http.StatusBadRequest, err)
		return
	}
	log.Infow("reroute_money_webhook_received",
		"channel_id", query.ChannelID,
		"client_ip", c.ClientIP(),
		"body_size", len(body),
	)

	payment, status, err := h.webhooks.HandleRerouteMoneyWebhook(WebhookCallbackInput{
		ChannelID: query.ChannelID,
		Headers:   collectRequestHeaders(c),
		Body:      body,
		Context:   c.Request.Context(),
	})
	if err != nil {
		log.Warnw("reroute_money_webhook_handle_failed",
			"channel_id", query.ChannelID,
			"status", status,
			"error", err,
		)
		h.enqueuePaymentExceptionAlert(c, jsonmap.JSON{
			"alert_type":  "reroute_money_webhook_handle_failed",
			"alert_level": "error",
			"message":     strings.TrimSpace(err.Error()),
			"provider":    constants.PaymentProviderRerouteMoney,
		})
		respondRerouteMoneyWebhookFailure(c, http.StatusServiceUnavailable, err)
		return
	}
	respondWebhookSuccess(c, log, "reroute_money_webhook", query.ChannelID, status, payment)
}

func readWebhookBody(c *gin.Context) ([]byte, error) {
	if c == nil || c.Request == nil || c.Request.Body == nil {
		return nil, nil
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxWebhookBodyBytes)
	return io.ReadAll(c.Request.Body)
}

func collectRequestHeaders(c *gin.Context) map[string]string {
	headers := make(map[string]string)
	for key, values := range c.Request.Header {
		if len(values) == 0 {
			continue
		}
		headers[key] = values[0]
	}
	return headers
}

type webhookLogger interface {
	Infow(msg string, keysAndValues ...interface{})
}

func respondWebhookSuccess(c *gin.Context, log webhookLogger, prefix string, channelID uint, eventType string, payment *paymentdomain.Payment) {
	if payment == nil {
		log.Infow(prefix+"_accepted_no_payment",
			"channel_id", channelID,
			"event_type", eventType,
		)
		response.Success(c, gin.H{
			"accepted":   true,
			"event_type": eventType,
			"updated":    false,
		})
		return
	}
	log.Infow(prefix+"_processed",
		"channel_id", channelID,
		"event_type", eventType,
		"payment_id", payment.ID,
		"status", payment.Status,
	)
	response.Success(c, gin.H{
		"accepted":   true,
		"event_type": eventType,
		"updated":    true,
		"payment_id": payment.ID,
		"status":     payment.Status,
	})
}

func (h *WebhookHandler) enqueuePaymentExceptionAlert(c *gin.Context, data jsonmap.JSON) {
	if h == nil || h.alerts == nil || c == nil || c.Request == nil {
		return
	}
	path := ""
	if c.Request.URL != nil {
		path = strings.TrimSpace(c.Request.URL.Path)
	}
	if err := h.alerts.EnqueuePaymentExceptionAlert(
		strings.TrimSpace(c.Request.Method),
		path,
		strings.TrimSpace(c.ClientIP()),
		data,
	); err != nil {
		ginutil.RequestLog(c).Warnw("enqueue_payment_exception_alert_failed", "error", err)
	}
}

func respondPaymentCallbackError(c *gin.Context, err error) {
	respondWithMappedError(c, err, paymentCallbackErrorRules, response.CodeInternal, "error.payment_callback_failed")
}

// rerouteMoneyWebhookPermanentErrors 是重投也救不回来的失败：伪造/错配的签名、金额币种对不上、
// 渠道或配置不存在。其余错误（查单、落库）按 transientStatus 回非 2xx，让平台继续重投。
var rerouteMoneyWebhookPermanentErrors = []error{
	ErrPaymentInvalid,
	ErrPaymentStatusInvalid,
	ErrPaymentAmountMismatch,
	ErrPaymentCurrencyMismatch,
	ErrPaymentChannelConfigInvalid,
	ErrPaymentChannelNotFound,
	ErrPaymentGatewayRequestFailed,
	ErrPaymentGatewayResponseInvalid,
	ErrPaymentProviderNotSupported,
}

// respondRerouteMoneyWebhookFailure 用真实 HTTP 状态码应答 reroute-money 推送失败。
//
// 平台只按 HTTP 状态判定投递结果并据此重投，而 dujiao 的业务错误响应恒为 200，
// 直接把失败交给 response.Error 会让平台以为投递成功，支付永久停在待支付。
func respondRerouteMoneyWebhookFailure(c *gin.Context, transientStatus int, err error) {
	httpStatus := transientStatus
	for _, permanent := range rerouteMoneyWebhookPermanentErrors {
		if errors.Is(err, permanent) {
			httpStatus = http.StatusBadRequest
			break
		}
	}
	// 业务码跟着 HTTP 状态走，避免出现 400 配 500 这种自相矛盾的响应
	code := response.CodeInternal
	if httpStatus == http.StatusBadRequest {
		code = response.CodeBadRequest
	}
	ginutil.RequestLog(c).Errorw("reroute_money_webhook_failed",
		"http_status", httpStatus,
		"code", code,
		"error", err,
	)
	response.ErrorWithHTTPStatus(
		c,
		httpStatus,
		code,
		i18n.T(i18n.ResolveLocale(c), "error.payment_callback_failed"),
	)
}

var paymentCallbackErrorRules = concatMappedErrors(
	[]mappedError{
		{target: ErrPaymentInvalid, code: response.CodeBadRequest, key: "error.payment_invalid"},
		{target: ErrPaymentNotFound, code: response.CodeNotFound, key: "error.payment_not_found"},
		{target: ErrPaymentStatusInvalid, code: response.CodeBadRequest, key: "error.payment_status_invalid"},
		{target: ErrPaymentAmountMismatch, code: response.CodeBadRequest, key: "error.payment_amount_mismatch"},
		{target: ErrPaymentCurrencyMismatch, code: response.CodeBadRequest, key: "error.payment_currency_mismatch"},
		{target: ErrPaymentChannelNotFound, code: response.CodeNotFound, key: "error.payment_channel_not_found"},
	},
	paymentProviderGatewayErrorRules,
)

// EpayRedirectPaymentLookup 只读取渲染易支付跳转表单所需的支付记录。
type EpayRedirectPaymentLookup interface {
	GetByID(id uint) (*paymentdomain.Payment, error)
}

// EpayRedirectHandler 把已保存的 v2 跳转参数渲染成自动提交的 POST 表单。
type EpayRedirectHandler struct {
	payments EpayRedirectPaymentLookup
}

func NewEpayRedirectHandler(payments EpayRedirectPaymentLookup) *EpayRedirectHandler {
	if payments == nil {
		panic("epay redirect handler: payments is nil")
	}
	return &EpayRedirectHandler{payments: payments}
}

type epayRedirectView struct {
	Action string
	Fields []epayRedirectField
}

type epayRedirectField struct {
	Name  string
	Value string
}

var epayRedirectTemplate = template.Must(template.New("epay-redirect").Parse(`<!DOCTYPE html>
<html>
<head><meta charset="utf-8"></head>
<body>
<form method="post" action="{{.Action}}">
{{- range .Fields}}
<input type="hidden" name="{{.Name}}" value="{{.Value}}">
{{- end}}
<input type="submit">
</form>
<script>document.forms[0].submit()</script>
</body>
</html>`))

// EpayRedirect 渲染自动提交的 POST 表单。不匹配的请求返回 404，且不包含表单。
func (h *EpayRedirectHandler) EpayRedirect(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	paymentID, token, ok := epayRedirectQuery(c)
	if !ok {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	payment, err := h.payments.GetByID(paymentID)
	if err != nil || !epayRedirectPayable(payment, time.Now()) {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	action, fields, accepted := epayRedirectForm(payment.ProviderPayload, token)
	if !accepted {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	var body bytes.Buffer
	if err := epayRedirectTemplate.Execute(&body, epayRedirectView{Action: action, Fields: fields}); err != nil {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	c.Data(http.StatusOK, "text/html; charset=utf-8", body.Bytes())
}

func epayRedirectQuery(c *gin.Context) (uint, string, bool) {
	rawID := strings.TrimSpace(c.Query("payment_id"))
	token := strings.TrimSpace(c.Query("token"))
	parsed, err := strconv.ParseUint(rawID, 10, 64)
	if err != nil || parsed == 0 || token == "" || uint64(uint(parsed)) != parsed {
		return 0, "", false
	}
	return uint(parsed), token, true
}

func epayRedirectPayable(payment *paymentdomain.Payment, now time.Time) bool {
	if payment == nil {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(payment.ProviderType), constants.PaymentProviderEpay) {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(payment.InteractionMode), constants.PaymentInteractionRedirect) {
		return false
	}
	if payment.Status != constants.PaymentStatusPending {
		return false
	}
	if payment.ExpiredAt != nil && !payment.ExpiredAt.After(now) {
		return false
	}
	if payment.SupersededAt != nil || payment.SupersededByPaymentID != nil {
		return false
	}
	return true
}

func epayRedirectForm(payload jsonmap.JSON, providedToken string) (string, []epayRedirectField, bool) {
	if payload == nil {
		return "", nil, false
	}
	method, _ := payload["submit_method"].(string)
	if !strings.EqualFold(strings.TrimSpace(method), http.MethodPost) {
		return "", nil, false
	}
	storedToken, _ := payload["token"].(string)
	if !epayRedirectTokenMatch(storedToken, providedToken) {
		return "", nil, false
	}
	endpoint, _ := payload["endpoint"].(string)
	endpoint = strings.TrimSpace(endpoint)
	if !httpEndpoint(endpoint) {
		return "", nil, false
	}
	params, ok := epayRedirectParams(payload["params"])
	if !ok {
		return "", nil, false
	}
	keys := make([]string, 0, len(params))
	for key, value := range params {
		if strings.TrimSpace(key) == "" || strings.TrimSpace(value) == "" {
			continue
		}
		keys = append(keys, key)
	}
	if len(keys) == 0 {
		return "", nil, false
	}
	sort.Strings(keys)
	fields := make([]epayRedirectField, 0, len(keys))
	for _, key := range keys {
		fields = append(fields, epayRedirectField{Name: key, Value: params[key]})
	}
	return endpoint, fields, true
}

func epayRedirectParams(raw interface{}) (map[string]string, bool) {
	params, ok := raw.(map[string]interface{})
	if !ok {
		return nil, false
	}
	out := make(map[string]string, len(params))
	for key, value := range params {
		text, ok := value.(string)
		if !ok {
			return nil, false
		}
		out[key] = text
	}
	return out, true
}

func httpEndpoint(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil {
		return false
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return false
	}
	return parsed.Host != ""
}

func epayRedirectTokenMatch(stored, provided string) bool {
	if stored == "" || provided == "" {
		return false
	}
	sumStored := sha256.Sum256([]byte(stored))
	sumProvided := sha256.Sum256([]byte(provided))
	return subtle.ConstantTimeCompare(sumStored[:], sumProvided[:]) == 1
}
