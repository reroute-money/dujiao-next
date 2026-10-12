package reroutemoney

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/dujiao-next/internal/constants"
	access "github.com/reroute-money/access-toolkit/golang"
	"github.com/shopspring/decimal"
)

// NotifyData 平台订单推送的解析结果。
//
// 推送报文是业务单据本身（代收推收款单、代付推付款单），交易事实在包裹的 order 那一层。
// 顶层 uniqueIdentifier 就是下单时回显、查单接口认得的单据号，与 provider_ref 存的同一个号；
// order.uniqueIdentifier 是另一张表（orders）的号，只能用来对账，拿它查单必然查空。
type NotifyData struct {
	// UniqueIdentifier 单据号 | 代收是收款单号、代付是付款单号
	UniqueIdentifier string
	// ExternalIdentifier 外部单号 | 即 dujiao 的 gateway order no
	ExternalIdentifier string
	// Type 单据种类
	Type string
	// Status dujiao 口径状态 | 空串表示本次推送无需处理
	Status string
	// RawStatus 平台原始状态
	RawStatus string
	// PaymentAmount 付款金额 | 主单位
	PaymentAmount decimal.Decimal
	// PaymentCurrency 付款币种
	PaymentCurrency string
	// CallbackURL 本次交易实际使用的回调地址
	CallbackURL string
	// PaidAt 到账时间 | 仅成功推送给出
	PaidAt *time.Time
	// Raw 推送原文
	Raw map[string]interface{}
}

// ParseNotify 验签并解析平台订单推送。
//
// 规范串的 path 取配置 notify_url 的 pathname，而不是请求自己的 URL.Path：平台签的是
// `new URL(callbackURL).pathname`，站点挂在反代子路径下时两者会错开。
func ParseNotify(cfg *Config, headers map[string]string, body []byte, now time.Time) (*NotifyData, error) {
	if cfg == nil {
		return nil, fmt.Errorf("%w: config is nil", ErrConfigInvalid)
	}
	callbackPath, err := NotifyPath(cfg.NotifyURL)
	if err != nil {
		return nil, err
	}
	if err := access.VerifyMerchantNotify(cfg.PlatformPublicKey, access.NotifyVerification{
		CallbackPath:  callbackPath,
		Authorization: pickHeader(headers, "Authorization"),
		Body:          body,
		Now:           now,
	}); err != nil {
		return nil, mapNotifyError(err)
	}

	notify, err := access.ParseMerchantNotify(body)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrResponseInvalid, err)
	}
	raw, err := rawMap(notify)
	if err != nil {
		return nil, err
	}
	// 交易事实都在包裹的订单那一层，单据那一头只贡献单号与收款方式
	order := &notify.Order
	data := &NotifyData{
		UniqueIdentifier:   strings.TrimSpace(notify.UniqueIdentifier),
		ExternalIdentifier: strings.TrimSpace(order.ExternalIdentifier),
		Type:               strings.TrimSpace(string(order.Type)),
		Status:             ToPaymentStatus(order.Status),
		RawStatus:          strings.TrimSpace(string(order.Status)),
		CallbackURL:        strings.TrimSpace(order.CallbackURL),
		Raw:                raw,
	}

	// 代付单可能复用同一个推送地址，但它的成功与本站订单收款无关，按“无需处理”忽略。
	if order.Type != access.OrderTypeCollection {
		data.Status = ""
		return data, nil
	}

	// 单据号会被上层存成 provider_ref，空号等于把兜底查单废掉，宁可报错让平台重投。
	if data.UniqueIdentifier == "" {
		return nil, fmt.Errorf("%w: collection order uniqueIdentifier is missing in notify", ErrResponseInvalid)
	}

	if order.PaymentAmount == nil {
		return nil, fmt.Errorf("%w: paymentAmount is missing in notify", ErrResponseInvalid)
	}
	currency := strings.ToUpper(string(order.PaymentAmount.Currency))
	amount, err := FromMinorUnits(order.PaymentAmount.Value, currency)
	if err != nil {
		return nil, err
	}
	data.PaymentAmount = amount
	data.PaymentCurrency = currency
	if order.Status == access.OrderStatusSucceeded {
		paidAt := order.UpdatedAt
		data.PaidAt = &paidAt
	}
	return data, nil
}

// pickHeader 大小写不敏感取请求头。
func pickHeader(headers map[string]string, name string) string {
	if len(headers) == 0 {
		return ""
	}
	if value, ok := headers[name]; ok {
		return value
	}
	for key, value := range headers {
		if strings.EqualFold(strings.TrimSpace(key), name) {
			return value
		}
	}
	return ""
}

// mapNotifyError 把 SDK 验签错误翻译成 provider 统一错误语义。
// 401/403 之类不会出现在这里：推送是平台主动发起的，只有签名与报文两类失败。
func mapNotifyError(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, access.ErrPublicKeyInvalid):
		return fmt.Errorf("%w: %v", ErrConfigInvalid, err)
	case errors.Is(err, access.ErrNotifyBodyInvalid):
		return fmt.Errorf("%w: %v", ErrResponseInvalid, err)
	default:
		return fmt.Errorf("%w: %v", ErrSignatureInvalid, err)
	}
}

// IsWebhookRoute 判定 notify_url 是否指向本站的 reroute 推送入口 | 路径填错时验签必然失败，
// 支付会一直停在待支付，所以配置保存阶段就把它挡下来。
func IsWebhookRoute(notifyURL string) bool {
	path, err := NotifyPath(notifyURL)
	if err != nil {
		return false
	}
	return strings.HasSuffix(path, constants.DefaultRerouteMoneyWebhookPath)
}
