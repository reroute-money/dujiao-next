package reroutemoneyadapter

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha512"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/dujiao-next/internal/constants"
	paymentcontract "github.com/dujiao-next/internal/modules/payment/contract"
	"github.com/dujiao-next/internal/modules/payment/infrastructure/gateway/reroutemoney"
	"github.com/dujiao-next/internal/shared/jsonmap"
	"github.com/dujiao-next/internal/shared/money"
	access "github.com/reroute-money/access-toolkit/golang"
	"github.com/shopspring/decimal"
)

// notifyURL 后台必须登记的推送地址 | 验签规范串的 path 由它推导
const notifyURL = "https://api.example.com" + constants.DefaultRerouteMoneyWebhookPath

// newKeyPair 生成一把密钥 | 私钥给请求签名并用于伪造平台推送，公钥配到渠道里验签
func newKeyPair(t *testing.T) (*rsa.PrivateKey, string, string) {
	t.Helper()
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	privatePEM := string(pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(privateKey),
	}))
	publicDER, err := x509.MarshalPKIXPublicKey(&privateKey.PublicKey)
	if err != nil {
		t.Fatalf("marshal public key: %v", err)
	}
	return privateKey, privatePEM, string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER}))
}

// pushConfig 组一份可直接下单的 config_json | 服务私钥与平台公钥取自同一对密钥，
// 这样同一把私钥既能签请求也能签推送，验签链路可被完整走通。
func pushConfig(t *testing.T, baseURL string) (jsonmap.JSON, *rsa.PrivateKey) {
	t.Helper()
	privateKey, privatePEM, publicPEM := newKeyPair(t)
	return jsonmap.JSON{
		"base_url":            baseURL,
		"using_key":           "using-key-1",
		"act_as":              "member-1",
		"service_private_key": privatePEM,
		"platform_public_key": publicPEM,
		"receipt_currency":    "USDT",
		"notify_url":          notifyURL,
	}, privateKey
}

// validJSON 组一份可直接下单的 config_json | baseURL 指向测试服务器
func validJSON(t *testing.T, baseURL string) jsonmap.JSON {
	t.Helper()
	cfg, _ := pushConfig(t, baseURL)
	return cfg
}

// collectionResponse 平台建单/查单的信封响应 | redirectURL 为空时不写该字段
func collectionResponse(t *testing.T, status access.OrderStatus, redirectURL string) string {
	t.Helper()
	order := map[string]interface{}{
		// 订单层的平台单号：查单接口不认它，provider_ref 不能取它
		"uniqueIdentifier":   "ord-1",
		"externalIdentifier": "DJP1001",
		"type":               string(access.OrderTypeCollection),
		"status":             string(status),
		"paymentAmount":      map[string]interface{}{"currency": "USD", "value": 2050},
		"receiptAmount":      map[string]interface{}{"currency": "USDT", "value": 20480000},
		"callbackURL":        notifyURL,
	}
	// 收款单号在 collectionOrder 这一层，才是 adapter 存回 provider_ref 的那个号
	data := map[string]interface{}{"uniqueIdentifier": "col-1", "order": order}
	if redirectURL != "" {
		data["redirectURL"] = redirectURL
	}
	payload, err := json.Marshal(map[string]interface{}{"code": access.CodeOK, "data": data})
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return string(payload)
}

// signPush 复刻平台出站推送的签名 | 规范串的 path 是 notify_url 的 pathname
func signPush(t *testing.T, privateKey *rsa.PrivateKey, generationTime int64, body []byte) map[string]string {
	t.Helper()
	path, err := reroutemoney.NotifyPath(notifyURL)
	if err != nil {
		t.Fatalf("NotifyPath: %v", err)
	}
	content := fmt.Sprintf("POST %s\r\n\r\nGeneration-Time: %d\r\nAct-As: %s\r\n\r\n%s", path, generationTime, "member-1", string(body))
	digest := sha512.Sum512([]byte(content))
	signature, err := rsa.SignPKCS1v15(rand.Reader, privateKey, crypto.SHA512, digest[:])
	if err != nil {
		t.Fatalf("sign push: %v", err)
	}
	parameters := url.Values{}
	parameters.Set("usingKey", "using-key-1")
	parameters.Set("actAs", "member-1")
	parameters.Set("generationTime", fmt.Sprintf("%d", generationTime))
	parameters.Set("requestSignature", base64.StdEncoding.EncodeToString(signature))
	return map[string]string{"Authorization": "Signature " + parameters.Encode()}
}

func TestRerouteMoneyAdapter_Type(t *testing.T) {
	// 只有一种 channel_type，后缀仍留空，让 Registry 落到 `reroute_money:` 回落键
	if got := NewRerouteMoneyAdapter().Type(); got != constants.PaymentProviderRerouteMoney+":" {
		t.Fatalf("Type() = %q, want %q", got, constants.PaymentProviderRerouteMoney+":")
	}
}

func TestRerouteMoneyAdapter_ValidateConfig(t *testing.T) {
	a := NewRerouteMoneyAdapter()
	cfgJSON := validJSON(t, "https://access.example.com")

	if err := a.ValidateConfig(cfgJSON, constants.PaymentChannelTypeRerouteMoney); err != nil {
		t.Fatalf("reroute-money should pass: %v", err)
	}
	// 第二参数在主动查单链路上是 interaction_mode，不能被 channel_type 校验挡掉
	for _, mode := range []string{"", constants.PaymentInteractionQR, constants.PaymentInteractionRedirect} {
		if err := a.ValidateConfig(cfgJSON, mode); err != nil {
			t.Fatalf("interaction mode %q should pass: %v", mode, err)
		}
	}
	if err := a.ValidateConfig(cfgJSON, "tron-usdt"); !errors.Is(err, paymentcontract.ErrGatewayUnsupportedChannel) {
		t.Fatalf("err = %v, want ErrGatewayUnsupportedChannel", err)
	}
}

func TestRerouteMoneyAdapter_ValidateConfigRequiresCompleteCredentials(t *testing.T) {
	a := NewRerouteMoneyAdapter()
	for _, key := range []string{"using_key", "act_as", "service_private_key", "platform_public_key", "notify_url"} {
		cfgJSON := validJSON(t, "https://access.example.com")
		delete(cfgJSON, key)
		if err := a.ValidateConfig(cfgJSON, constants.PaymentChannelTypeRerouteMoney); !errors.Is(err, paymentcontract.ErrGatewayConfigInvalid) {
			t.Fatalf("missing %s err = %v, want ErrGatewayConfigInvalid", key, err)
		}
	}

	// 收款币种必须能换算成最小单位，表外的码在保存阶段就拦下，不留到下单那一刻
	unsupported := validJSON(t, "https://access.example.com")
	unsupported["receipt_currency"] = "AAA"
	if err := a.ValidateConfig(unsupported, constants.PaymentChannelTypeRerouteMoney); !errors.Is(err, paymentcontract.ErrGatewayConfigInvalid) {
		t.Fatalf("unsupported receipt_currency err = %v", err)
	}
}

func TestRerouteMoneyAdapter_ValidateConfigRequiresWebhookRoute(t *testing.T) {
	a := NewRerouteMoneyAdapter()
	cfgJSON := validJSON(t, "https://access.example.com")
	cfgJSON["notify_url"] = "https://api.example.com/api/v1/payments/callback"
	err := a.ValidateConfig(cfgJSON, constants.PaymentChannelTypeRerouteMoney)
	if !errors.Is(err, paymentcontract.ErrGatewayConfigInvalid) {
		t.Fatalf("err = %v, want ErrGatewayConfigInvalid", err)
	}
	if !strings.Contains(err.Error(), constants.DefaultRerouteMoneyWebhookPath) {
		t.Fatalf("error must name the expected path, got %v", err)
	}
}

func TestRerouteMoneyAdapter_CreatePaymentLocksSentFacts(t *testing.T) {
	var received map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/collection-order" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		_, _ = w.Write([]byte(collectionResponse(t, access.OrderStatusCreated, "https://checkout.example.com/col-1")))
	}))
	defer server.Close()

	result, err := NewRerouteMoneyAdapter().CreatePayment(context.Background(), validJSON(t, server.URL), paymentcontract.GatewayCreateInput{
		OrderNo:     "DJP1001",
		Amount:      money.FromDecimal(decimal.RequireFromString("20.50")),
		Currency:    "usd",
		ChannelType: constants.PaymentChannelTypeRerouteMoney,
	})
	if err != nil {
		t.Fatalf("CreatePayment: %v", err)
	}
	if result.ProviderRef != "col-1" {
		t.Fatalf("ProviderRef = %q, want col-1", result.ProviderRef)
	}
	if result.RedirectURL != "https://checkout.example.com/col-1" {
		t.Fatalf("RedirectURL = %q", result.RedirectURL)
	}
	// 平台不给二维码，qr 模式由前台把跳转地址渲染成付款码，所以两个字段同值
	if result.QRCodeURL != result.RedirectURL {
		t.Fatalf("QRCodeURL = %q, want %q", result.QRCodeURL, result.RedirectURL)
	}
	// 回写的付款事实取入参原值，回调事实校验才能对上（小数尾零不影响金额比较）
	if got := decimal.RequireFromString(result.AmountSent); got.Cmp(decimal.RequireFromString("20.50")) != 0 {
		t.Fatalf("AmountSent = %q, want 20.50", result.AmountSent)
	}
	if result.CurrencySent != "USD" {
		t.Fatalf("CurrencySent = %q, want USD", result.CurrencySent)
	}
	if got := result.Payload[paymentcontract.GatewayPayloadFiatCurrencySent]; got != "USD" {
		t.Fatalf("fiat currency snapshot = %#v, want USD", got)
	}
	if got := result.Payload["redirect_url"]; got != "https://checkout.example.com/col-1" {
		t.Fatalf("payload redirect_url = %#v", got)
	}
	if got := result.Payload["uniqueIdentifier"]; got != "ord-1" {
		t.Fatalf("payload must keep the platform order echo, got %#v", got)
	}

	order, _ := received["order"].(map[string]interface{})
	if order == nil {
		t.Fatalf("no order in request: %+v", received)
	}
	paymentAmount, _ := order["paymentAmount"].(map[string]interface{})
	if paymentAmount["currency"] != "USD" || paymentAmount["value"] != float64(2050) {
		t.Fatalf("paymentAmount = %+v, want USD/2050", paymentAmount)
	}
	if order["callbackURL"] != notifyURL {
		t.Fatalf("callbackURL = %v, want %s", order["callbackURL"], notifyURL)
	}
	// 渠道未配置 return_url 时不下发该字段，平台也就不做同步回跳
	if value, exists := received["returnURL"]; exists {
		t.Fatalf("returnURL must be absent without config, got %v", value)
	}
}

func TestRerouteMoneyAdapter_CreatePaymentSendsReturnURL(t *testing.T) {
	var received map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/collection-order" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		_, _ = w.Write([]byte(collectionResponse(t, access.OrderStatusCreated, "https://checkout.example.com/col-1")))
	}))
	defer server.Close()

	cfg := validJSON(t, server.URL)
	cfg["return_url"] = "https://shop.example.com/pay"
	a := NewRerouteMoneyAdapter()

	// 主站下单：input.ReturnURL 为空，回落到渠道配置的 return_url，再 append 结果页需要的 query
	if _, err := a.CreatePayment(context.Background(), cfg, paymentcontract.GatewayCreateInput{
		OrderNo:        "DJP1001",
		Amount:         money.FromDecimal(decimal.RequireFromString("20.50")),
		Currency:       "USD",
		ReturnURLQuery: map[string]string{"biz_type": "order", "order_no": "DJP1001", "marker": "m-1"},
	}); err != nil {
		t.Fatalf("CreatePayment: %v", err)
	}
	returned, _ := received["returnURL"].(string)
	parsed, err := url.Parse(returned)
	if err != nil {
		t.Fatalf("returnURL %q is not a URL: %v", returned, err)
	}
	if parsed.Scheme+"://"+parsed.Host+parsed.Path != "https://shop.example.com/pay" {
		t.Fatalf("returnURL base = %q, want https://shop.example.com/pay", returned)
	}
	query := parsed.Query()
	for key, want := range map[string]string{"biz_type": "order", "order_no": "DJP1001", "marker": "m-1"} {
		if got := query.Get(key); got != want {
			t.Fatalf("returnURL query %s = %q, want %q", key, got, want)
		}
	}

	// 分销站下单：service 层按租户域名给出 input.ReturnURL，它优先于渠道配置
	if _, err := a.CreatePayment(context.Background(), cfg, paymentcontract.GatewayCreateInput{
		OrderNo:        "DJP1001",
		Amount:         money.FromDecimal(decimal.RequireFromString("20.50")),
		Currency:       "USD",
		ReturnURL:      "https://tenant.example.com/pay",
		ReturnURLQuery: map[string]string{"order_no": "DJP1001"},
	}); err != nil {
		t.Fatalf("CreatePayment with tenant return url: %v", err)
	}
	returned, _ = received["returnURL"].(string)
	parsed, err = url.Parse(returned)
	if err != nil {
		t.Fatalf("tenant returnURL %q is not a URL: %v", returned, err)
	}
	if parsed.Scheme+"://"+parsed.Host+parsed.Path != "https://tenant.example.com/pay" {
		t.Fatalf("tenant returnURL base = %q, want https://tenant.example.com/pay", returned)
	}
	if got := parsed.Query().Get("order_no"); got != "DJP1001" {
		t.Fatalf("tenant returnURL query order_no = %q, want DJP1001", got)
	}
}

func TestRerouteMoneyAdapter_CreatePaymentRejectsUnsupportedChannelAndCurrency(t *testing.T) {
	a := NewRerouteMoneyAdapter()
	cfgJSON := validJSON(t, "https://access.example.com")

	if _, err := a.CreatePayment(context.Background(), cfgJSON, paymentcontract.GatewayCreateInput{
		OrderNo:  "DJP1001",
		Amount:   money.FromDecimal(decimal.NewFromInt(1)),
		Currency: "USD",
		// 未登记的路径会改变下单请求的签名串，channel_type 也不接受非 reroute-money
		ChannelType: "dogeusdt",
	}); !errors.Is(err, paymentcontract.ErrGatewayUnsupportedChannel) {
		t.Fatalf("err = %v, want ErrGatewayUnsupportedChannel", err)
	}

	// 站点币种必须能换算成最小单位，表外的码无法建单
	if _, err := a.CreatePayment(context.Background(), cfgJSON, paymentcontract.GatewayCreateInput{
		OrderNo:     "DJP1001",
		Amount:      money.FromDecimal(decimal.NewFromInt(1)),
		Currency:    "AAA",
		ChannelType: constants.PaymentChannelTypeRerouteMoney,
	}); !errors.Is(err, paymentcontract.ErrGatewayConfigInvalid) {
		t.Fatalf("err = %v, want ErrGatewayConfigInvalid", err)
	}
}

func TestRerouteMoneyAdapter_CreatePaymentRejectsMissingRedirect(t *testing.T) {
	// 没有跳转地址付款人无从付款，静默建单只会留下一笔永不达成的待支付
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(collectionResponse(t, access.OrderStatusCreated, "")))
	}))
	defer server.Close()

	_, err := NewRerouteMoneyAdapter().CreatePayment(context.Background(), validJSON(t, server.URL), paymentcontract.GatewayCreateInput{
		OrderNo:     "DJP1001",
		Amount:      money.FromDecimal(decimal.RequireFromString("20.50")),
		Currency:    "USD",
		ChannelType: constants.PaymentChannelTypeRerouteMoney,
	})
	if !errors.Is(err, paymentcontract.ErrGatewayResponseInvalid) {
		t.Fatalf("err = %v, want ErrGatewayResponseInvalid", err)
	}
}

func TestRerouteMoneyAdapter_CreatePaymentSurfacesPlatformRejection(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"code":"RATE_NOT_FOUND","message":"no rate for USD to USDT"}`))
	}))
	defer server.Close()

	_, err := NewRerouteMoneyAdapter().CreatePayment(context.Background(), validJSON(t, server.URL), paymentcontract.GatewayCreateInput{
		OrderNo:     "DJP1001",
		Amount:      money.FromDecimal(decimal.RequireFromString("20.50")),
		Currency:    "USD",
		ChannelType: constants.PaymentChannelTypeRerouteMoney,
	})
	if !errors.Is(err, paymentcontract.ErrGatewayRequestFailed) {
		t.Fatalf("err = %v, want ErrGatewayRequestFailed", err)
	}
	if !strings.Contains(err.Error(), "RATE_NOT_FOUND") {
		t.Fatalf("platform code lost: %v", err)
	}
}

func TestRerouteMoneyAdapter_ParseWebhookMapsVerifiedPush(t *testing.T) {
	cfgJSON, privateKey := pushConfig(t, "https://access.example.com")
	now := time.Now().UTC().Truncate(time.Second)
	body, err := json.Marshal(map[string]interface{}{
		"uniqueIdentifier": "col-1",
		"order": map[string]interface{}{
			"uniqueIdentifier":   "ord-1",
			"externalIdentifier": "DJP1001",
			"type":               string(access.OrderTypeCollection),
			"status":             string(access.OrderStatusSucceeded),
			"paymentAmount":      map[string]interface{}{"currency": "USD", "value": 2050},
			"callbackURL":        notifyURL,
			"createdAt":          now.Add(-time.Minute).Format(time.RFC3339Nano),
			"updatedAt":          now.Format(time.RFC3339Nano),
		},
	})
	if err != nil {
		t.Fatalf("marshal push: %v", err)
	}

	webhooker, ok := NewRerouteMoneyAdapter().(paymentcontract.GatewayWebhooker)
	if !ok {
		t.Fatalf("reroute-money adapter must implement GatewayWebhooker")
	}
	result, err := webhooker.ParseWebhook(context.Background(), cfgJSON, signPush(t, privateKey, now.UnixMilli(), body), body, now)
	if err != nil {
		t.Fatalf("ParseWebhook: %v", err)
	}
	if result.OrderNo != "DJP1001" {
		t.Fatalf("OrderNo = %q, want DJP1001", result.OrderNo)
	}
	// 推送顶层是单据号，与下单回显、查单认的是同一个号，落进 provider_ref
	if result.ProviderRef != "col-1" {
		t.Fatalf("ProviderRef = %q, want col-1", result.ProviderRef)
	}
	if result.Payload["uniqueIdentifier"] != "col-1" {
		t.Fatalf("payload must record the pushed document number, got %#v", result.Payload["uniqueIdentifier"])
	}
	nested := result.Payload["order"].(map[string]interface{})
	if nested["uniqueIdentifier"] != "ord-1" {
		t.Fatalf("payload must keep the order-layer number for auditing, got %#v", nested["uniqueIdentifier"])
	}
	if result.Status != constants.PaymentStatusSuccess {
		t.Fatalf("Status = %q, want %q", result.Status, constants.PaymentStatusSuccess)
	}
	if result.Amount.Decimal.Cmp(decimal.RequireFromString("20.50")) != 0 {
		t.Fatalf("Amount = %s, want 20.50", result.Amount.String())
	}
	if result.Currency != "USD" {
		t.Fatalf("Currency = %q, want USD", result.Currency)
	}
	if result.PaidAt == nil || !result.PaidAt.Equal(now) {
		t.Fatalf("PaidAt = %v, want %s", result.PaidAt, now)
	}
	if result.Payload["order_type"] != string(access.OrderTypeCollection) {
		t.Fatalf("payload order_type = %#v", result.Payload["order_type"])
	}
	if result.Payload["order_status"] != string(access.OrderStatusSucceeded) {
		t.Fatalf("payload order_status = %#v", result.Payload["order_status"])
	}

	// 报文改了却不重签：验签必须失败，且翻译成统一签名错误
	tampered, _ := json.Marshal(map[string]interface{}{
		"uniqueIdentifier": "col-1",
		"order": map[string]interface{}{
			"uniqueIdentifier":   "ord-1",
			"externalIdentifier": "DJP1001",
			"type":               string(access.OrderTypeCollection),
			"status":             string(access.OrderStatusSucceeded),
			"paymentAmount":      map[string]interface{}{"currency": "USD", "value": 1},
			"callbackURL":        notifyURL,
		},
	})
	if _, err := webhooker.ParseWebhook(context.Background(), cfgJSON, signPush(t, privateKey, now.UnixMilli(), body), tampered, now); !errors.Is(err, paymentcontract.ErrGatewaySignatureInvalid) {
		t.Fatalf("err = %v, want ErrGatewaySignatureInvalid", err)
	}
}

func TestRerouteMoneyAdapter_ParseWebhookIgnoresNonCollectionPush(t *testing.T) {
	cfgJSON, privateKey := pushConfig(t, "https://access.example.com")
	now := time.Now().UTC().Truncate(time.Second)
	body, err := json.Marshal(map[string]interface{}{
		"uniqueIdentifier": "pay-1",
		"order": map[string]interface{}{
			"uniqueIdentifier":   "ord-2",
			"externalIdentifier": "DJP1001",
			"type":               string(access.OrderTypePayment),
			"status":             string(access.OrderStatusSucceeded),
		},
	})
	if err != nil {
		t.Fatalf("marshal push: %v", err)
	}

	result, err := NewRerouteMoneyAdapter().(paymentcontract.GatewayWebhooker).
		ParseWebhook(context.Background(), cfgJSON, signPush(t, privateKey, now.UnixMilli(), body), body, now)
	if err != nil {
		t.Fatalf("ParseWebhook: %v", err)
	}
	// 代付单的成功与本站订单收款无关，状态留空让上层按“无需处理”忽略
	if result.Status != "" {
		t.Fatalf("Status = %q, want empty so the push is ignored", result.Status)
	}
	if result.Payload["order_type"] != string(access.OrderTypePayment) {
		t.Fatalf("payload order_type = %#v", result.Payload["order_type"])
	}
}

func TestRerouteMoneyAdapter_QueryPaymentReturnsSettledFacts(t *testing.T) {
	var requestedPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestedPath = r.URL.Path
		if r.Method != http.MethodGet {
			t.Fatalf("method = %s, want GET", r.Method)
		}
		_, _ = w.Write([]byte(collectionResponse(t, access.OrderStatusSucceeded, "")))
	}))
	defer server.Close()

	capturer, ok := NewRerouteMoneyAdapter().(paymentcontract.GatewayCapturer)
	if !ok {
		t.Fatalf("reroute-money adapter must implement GatewayCapturer")
	}
	result, err := capturer.QueryPayment(context.Background(), validJSON(t, server.URL), "col-1")
	if err != nil {
		t.Fatalf("QueryPayment: %v", err)
	}
	if requestedPath != "/v2/collection-order/col-1" {
		t.Fatalf("path = %q", requestedPath)
	}
	if result.Status != constants.PaymentStatusSuccess {
		t.Fatalf("Status = %q, want %q", result.Status, constants.PaymentStatusSuccess)
	}
	if result.ProviderRef != "col-1" || result.Currency != "USD" {
		t.Fatalf("result = %+v", result)
	}
	if result.Amount.Decimal.Cmp(decimal.RequireFromString("20.50")) != 0 {
		t.Fatalf("Amount = %s, want 20.50", result.Amount.String())
	}
	// 成功查单要把更新时间当到账时间，供 capture 链路落库
	if result.PaidAt == nil {
		t.Fatalf("PaidAt must be set for a settled order")
	}
	if result.Payload["order_status"] != string(access.OrderStatusSucceeded) {
		t.Fatalf("payload order_status = %#v", result.Payload["order_status"])
	}
}

func TestRerouteMoneyAdapter_QueryPaymentRejectsUnsafeProviderRef(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("server must not be reached for an unsafe identifier")
	}))
	defer server.Close()

	capturer := NewRerouteMoneyAdapter().(paymentcontract.GatewayCapturer)
	if _, err := capturer.QueryPayment(context.Background(), validJSON(t, server.URL), "../payment-order"); !errors.Is(err, paymentcontract.ErrGatewayConfigInvalid) {
		t.Fatalf("err = %v, want ErrGatewayConfigInvalid", err)
	}
}

func TestMapRerouteMoneyError(t *testing.T) {
	tests := []struct {
		err  error
		want error
	}{
		{fmt.Errorf("%w: boom", reroutemoney.ErrConfigInvalid), paymentcontract.ErrGatewayConfigInvalid},
		{fmt.Errorf("%w: boom", reroutemoney.ErrRequestFailed), paymentcontract.ErrGatewayRequestFailed},
		{fmt.Errorf("%w: boom", reroutemoney.ErrResponseInvalid), paymentcontract.ErrGatewayResponseInvalid},
		{fmt.Errorf("%w: boom", reroutemoney.ErrSignatureInvalid), paymentcontract.ErrGatewaySignatureInvalid},
	}
	for _, tt := range tests {
		if got := mapRerouteMoneyError(tt.err); !errors.Is(got, tt.want) {
			t.Fatalf("mapRerouteMoneyError(%v) = %v, want %v", tt.err, got, tt.want)
		}
	}
	if mapRerouteMoneyError(nil) != nil {
		t.Fatalf("nil must stay nil")
	}
	// 未知错误原样返回，不要把它套进错误的语义里
	unknown := errors.New("boom")
	if got := mapRerouteMoneyError(unknown); !errors.Is(got, unknown) {
		t.Fatalf("unknown error = %v, want passthrough", got)
	}
}
