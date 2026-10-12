// Package reroutemoney 是 Reroute Money 接入点（REST /v2/*）的代收协议实现。
//
// 请求签名、信封解包与回调验签全部委托给官方 Go SDK（access-toolkit），
// 本包只负责 dujiao 的金额口径、状态映射与错误语义翻译，不自己实现任何签名逻辑。
package reroutemoney

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/dujiao-next/internal/constants"
	gatewaycommon "github.com/dujiao-next/internal/modules/payment/infrastructure/gateway/common"
	access "github.com/reroute-money/access-toolkit/golang"
	"github.com/shopspring/decimal"
)

// 配置缺省值 | 产品形态与 SDK 文档示例一致
const DefaultProductType = "ONLINE_RECEIVE"

var (
	ErrConfigInvalid    = errors.New("reroute config invalid")
	ErrRequestFailed    = errors.New("reroute request failed")
	ErrResponseInvalid  = errors.New("reroute response invalid")
	ErrSignatureInvalid = errors.New("reroute signature invalid")
)

// Config 是渠道 config_json 的 reroute 形态。
type Config struct {
	// BaseURL 接入点地址 | 留空取 SDK 默认
	BaseURL string `json:"base_url"`
	// UsingKey 用户密钥编号 | 在接入点后台创建用户密钥后拿到
	UsingKey string `json:"using_key"`
	// ActAs 租户成员编号 | 签名里的 actAs
	ActAs string `json:"act_as"`
	// ServicePrivateKey 服务私钥 PEM | 给请求签名，SDK 直接消费
	ServicePrivateKey string `json:"service_private_key"`
	// PlatformPublicKey 平台公钥 PEM | 验订单推送签名
	PlatformPublicKey string `json:"platform_public_key"`
	// ProductType 产品类型 | 代收固定 ONLINE_RECEIVE
	ProductType string `json:"product_type"`
	// ReceiptCurrency 收款币种 | 留空跟随订单币种
	ReceiptCurrency string `json:"receipt_currency"`
	// NotifyURL 推送地址 | 下单时登记，验签也以其 pathname 重建规范串
	NotifyURL string `json:"notify_url"`
	// ReturnURL 同步回跳地址 | 付款人在平台收银台完成后导回本站，留空则不回跳
	ReturnURL string `json:"return_url"`
}

// CreateInput 代收下单入参 | 金额是主单位（1.00 = 1 元），换最小单位由本包完成。
type CreateInput struct {
	// ExternalIdentifier 外部单号 | dujiao 的 gateway order no，成员级唯一并参与幂等比较
	ExternalIdentifier string
	// PaymentCurrency 付款币种 | 锁定为订单币种，平台据此按汇率补收款侧
	PaymentCurrency string
	// PaymentAmount 付款金额 | 主单位
	PaymentAmount decimal.Decimal
	// ReceiptCurrency 收款币种 | 空则取付款币种
	ReceiptCurrency string
	// CallbackURL 推送地址
	CallbackURL string
	// ReturnURL 同步回跳地址 | 已按订单拼好 query，空则平台不回跳
	ReturnURL string
}

// OrderSnapshot 平台回显的单据 | 下单响应与查询响应共用。
type OrderSnapshot struct {
	// UniqueIdentifier 收款单号 | collectionOrder 自身的 uniqueIdentifier，查单接口按它或外部单号定位；
	// collectionOrder.order.uniqueIdentifier 是另一张表的号，查单接口不认它，不能拿来当 provider_ref
	UniqueIdentifier string
	// ExternalIdentifier 外部单号
	ExternalIdentifier string
	// Status dujiao 口径状态
	Status string
	// RawStatus 平台原始状态
	RawStatus string
	// PaymentAmount 付款金额 | 主单位
	PaymentAmount decimal.Decimal
	// PaymentCurrency 付款币种
	PaymentCurrency string
	// ReceiptAmount 收款金额 | 主单位
	ReceiptAmount decimal.Decimal
	// ReceiptCurrency 收款币种
	ReceiptCurrency string
	// CreatedAt 创建时间
	CreatedAt time.Time
	// UpdatedAt 更新时间
	UpdatedAt time.Time
	// Raw 平台回显原文 | 落 provider_payload 供审计
	Raw map[string]interface{}
}

// CreateResult 代收下单结果。
type CreateResult struct {
	// RedirectURL 付款跳转地址 | 渠道未给出时为空
	RedirectURL string
	// Order 单据快照
	Order OrderSnapshot
}

// QueryResult 代收查询结果。
type QueryResult struct {
	// Order 单据快照
	Order OrderSnapshot
}

// ParseConfig 解析并归一化 config_json。
func ParseConfig(raw map[string]interface{}) (*Config, error) {
	return gatewaycommon.ParseConfig[Config](raw, ErrConfigInvalid)
}

// Normalize 归一化配置字段。
func (c *Config) Normalize() {
	c.BaseURL = strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	c.UsingKey = strings.TrimSpace(c.UsingKey)
	c.ActAs = strings.TrimSpace(c.ActAs)
	c.ServicePrivateKey = strings.TrimSpace(c.ServicePrivateKey)
	c.PlatformPublicKey = strings.TrimSpace(c.PlatformPublicKey)
	c.ProductType = strings.ToUpper(strings.TrimSpace(c.ProductType))
	c.ReceiptCurrency = strings.ToUpper(strings.TrimSpace(c.ReceiptCurrency))
	c.NotifyURL = strings.TrimSpace(c.NotifyURL)
	c.ReturnURL = strings.TrimSpace(c.ReturnURL)
	if c.BaseURL == "" {
		c.BaseURL = access.DefaultBaseURL
	}
	if c.ProductType == "" {
		c.ProductType = DefaultProductType
	}
}

// ValidateConfig 校验配置完整性 | 缺任何一把密钥或推送地址都直接失败，不做静默回落。
func ValidateConfig(cfg *Config) error {
	if cfg == nil {
		return fmt.Errorf("%w: config is nil", ErrConfigInvalid)
	}
	if cfg.UsingKey == "" {
		return fmt.Errorf("%w: using_key is required", ErrConfigInvalid)
	}
	if cfg.ActAs == "" {
		return fmt.Errorf("%w: act_as is required", ErrConfigInvalid)
	}
	if cfg.ServicePrivateKey == "" {
		return fmt.Errorf("%w: service_private_key is required", ErrConfigInvalid)
	}
	if cfg.PlatformPublicKey == "" {
		return fmt.Errorf("%w: platform_public_key is required", ErrConfigInvalid)
	}
	if cfg.ReceiptCurrency != "" && !IsSupportedCurrency(cfg.ReceiptCurrency) {
		return fmt.Errorf("%w: unsupported receipt_currency %s", ErrConfigInvalid, cfg.ReceiptCurrency)
	}
	if _, err := NotifyPath(cfg.NotifyURL); err != nil {
		return err
	}
	// 私钥在配置保存时就交给 SDK 解析，避免把密钥格式问题留到下单那一刻。
	if _, err := newClient(cfg); err != nil {
		return err
	}
	return nil
}

// IsSupportedChannelType 判定 channel_type 是否由 reroute 承接。
func IsSupportedChannelType(channelType string) bool {
	return strings.EqualFold(strings.TrimSpace(channelType), constants.PaymentChannelTypeRerouteMoney)
}

// IsSupportedCurrency 判定币种能否换算成最小单位 | 覆盖 ISO 4217 全部记数币种加链上 USDT，
// 与站点币种设置（^[A-Z]{3}$）取同一套口径；记帐货币和自造码不在表内，一律显式失败。
func IsSupportedCurrency(currency string) bool {
	_, ok := minorUnitExponent(currency)
	return ok
}

// NotifyPath 取 notify_url 的 pathname | 平台签的是 `new URL(callbackURL).pathname`，
// 与请求到达时的路径无关，因此验签必须以配置值为准。
func NotifyPath(notifyURL string) (string, error) {
	raw := strings.TrimSpace(notifyURL)
	if raw == "" {
		return "", fmt.Errorf("%w: notify_url is required", ErrConfigInvalid)
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("%w: notify_url is not a valid URL: %v", ErrConfigInvalid, err)
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("%w: notify_url must be an absolute URL", ErrConfigInvalid)
	}
	if parsed.Path == "" || parsed.Path == "/" {
		return "", fmt.Errorf("%w: notify_url has no callback path", ErrConfigInvalid)
	}
	return parsed.Path, nil
}

// CreateCollectionOrder 代收下单 | 锁付款侧金额，收款侧只交币种。
func CreateCollectionOrder(ctx context.Context, cfg *Config, input CreateInput) (*CreateResult, error) {
	client, err := newClient(cfg)
	if err != nil {
		return nil, err
	}
	externalIdentifier := strings.TrimSpace(input.ExternalIdentifier)
	if externalIdentifier == "" {
		return nil, fmt.Errorf("%w: external identifier is required", ErrConfigInvalid)
	}
	paymentCurrency := strings.ToUpper(strings.TrimSpace(input.PaymentCurrency))
	if !IsSupportedCurrency(paymentCurrency) {
		return nil, fmt.Errorf("%w: unsupported payment currency %s", ErrConfigInvalid, paymentCurrency)
	}
	receiptCurrency := strings.ToUpper(gatewaycommon.PickFirstNonEmpty(input.ReceiptCurrency, cfg.ReceiptCurrency, paymentCurrency))
	if !IsSupportedCurrency(receiptCurrency) {
		return nil, fmt.Errorf("%w: unsupported receipt currency %s", ErrConfigInvalid, receiptCurrency)
	}
	paymentMinor, err := ToMinorUnits(input.PaymentAmount, paymentCurrency)
	if err != nil {
		return nil, err
	}

	value := paymentMinor
	returnURL := strings.TrimSpace(input.ReturnURL)
	if returnURL != "" {
		if parsed, err := url.ParseRequestURI(returnURL); err != nil || parsed.Host == "" {
			return nil, fmt.Errorf("%w: return_url must be an absolute URL: %s", ErrConfigInvalid, returnURL)
		}
	}
	response, err := client.MakeCollectionOrder(ctx, access.CollectionOrder{
		Order: access.OrderRequest{
			PaymentAmount:      access.Amount{Currency: access.Currency(paymentCurrency), Value: &value},
			ReceiptAmount:      access.Amount{Currency: access.Currency(receiptCurrency)},
			ExternalIdentifier: externalIdentifier,
			Product:            access.Product{Type: cfg.ProductType},
			CallbackURL:        strings.TrimSpace(input.CallbackURL),
		},
		// 回跳地址在 order 之外，平台原样下发渠道、不校验域名，也不拼到 redirectURL 上。
		ReturnURL: returnURL,
	})
	if err != nil {
		return nil, mapAccessError(err)
	}

	snapshot, err := toSnapshot(&response.Data)
	if err != nil {
		return nil, err
	}
	return &CreateResult{RedirectURL: strings.TrimSpace(response.Data.RedirectURL), Order: snapshot}, nil
}

// QueryCollectionOrder 主动查询代收单 | identifier 为收款单号（下单回显的那个）或外部单号。
func QueryCollectionOrder(ctx context.Context, cfg *Config, identifier string) (*QueryResult, error) {
	client, err := newClient(cfg)
	if err != nil {
		return nil, err
	}
	if !IsOrderIdentifier(identifier) {
		return nil, fmt.Errorf("%w: order identifier %q is not a safe path segment", ErrConfigInvalid, identifier)
	}
	response, err := client.QueryCollectionOrder(ctx, strings.TrimSpace(identifier))
	if err != nil {
		return nil, mapAccessError(err)
	}
	snapshot, err := toSnapshot(&response.Data)
	if err != nil {
		return nil, err
	}
	return &QueryResult{Order: snapshot}, nil
}

// toSnapshot 把平台单据翻译成 dujiao 口径 | 单号取 collectionOrder 自身的 uniqueIdentifier，
// 交易事实（状态、金额、外部单号）取它一对一指向的 order。
func toSnapshot(data *access.CollectionOrderResult) (OrderSnapshot, error) {
	if data == nil {
		return OrderSnapshot{}, fmt.Errorf("%w: collection order is missing in response", ErrResponseInvalid)
	}
	uniqueIdentifier := strings.TrimSpace(data.UniqueIdentifier)
	if uniqueIdentifier == "" {
		return OrderSnapshot{}, fmt.Errorf("%w: collection order uniqueIdentifier is missing in response", ErrResponseInvalid)
	}
	order := &data.Order
	if strings.TrimSpace(order.UniqueIdentifier) == "" {
		return OrderSnapshot{}, fmt.Errorf("%w: order is missing in response", ErrResponseInvalid)
	}
	raw, err := rawMap(order)
	if err != nil {
		return OrderSnapshot{}, err
	}
	snapshot := OrderSnapshot{
		UniqueIdentifier:   uniqueIdentifier,
		ExternalIdentifier: strings.TrimSpace(order.ExternalIdentifier),
		Status:             ToPaymentStatus(order.Status),
		RawStatus:          strings.TrimSpace(string(order.Status)),
		CreatedAt:          order.CreatedAt,
		UpdatedAt:          order.UpdatedAt,
		Raw:                raw,
	}
	if order.PaymentAmount != nil {
		amount, convertErr := FromMinorUnits(order.PaymentAmount.Value, string(order.PaymentAmount.Currency))
		if convertErr != nil {
			return OrderSnapshot{}, convertErr
		}
		snapshot.PaymentCurrency = strings.ToUpper(string(order.PaymentAmount.Currency))
		snapshot.PaymentAmount = amount
	}
	if order.ReceiptAmount != nil {
		amount, convertErr := FromMinorUnits(order.ReceiptAmount.Value, string(order.ReceiptAmount.Currency))
		if convertErr != nil {
			return OrderSnapshot{}, convertErr
		}
		snapshot.ReceiptCurrency = strings.ToUpper(string(order.ReceiptAmount.Currency))
		snapshot.ReceiptAmount = amount
	}
	return snapshot, nil
}

// ToPaymentStatus 平台状态 → dujiao 状态 | 未知状态返回空串，由上层按“无需处理”忽略。
func ToPaymentStatus(status access.OrderStatus) string {
	switch status {
	case access.OrderStatusSucceeded:
		return constants.PaymentStatusSuccess
	case access.OrderStatusFailed:
		return constants.PaymentStatusFailed
	case access.OrderStatusCreated, access.OrderStatusProcessing:
		return constants.PaymentStatusPending
	default:
		return ""
	}
}

// ToMinorUnits 主单位金额 → 平台最小单位 | 精度超出最小单位时直接失败，不四舍五入付款金额。
func ToMinorUnits(amount decimal.Decimal, currency string) (int64, error) {
	exponent, ok := minorUnitExponent(currency)
	if !ok {
		return 0, fmt.Errorf("%w: unsupported currency %s", ErrConfigInvalid, currency)
	}
	if amount.IsNegative() {
		return 0, fmt.Errorf("%w: amount must not be negative", ErrConfigInvalid)
	}
	shifted := amount.Shift(exponent)
	if !shifted.Equal(shifted.Round(0)) {
		return 0, fmt.Errorf("%w: amount %s is finer than the %s minor unit", ErrConfigInvalid, amount.String(), currency)
	}
	return shifted.IntPart(), nil
}

// FromMinorUnits 平台最小单位金额 → 主单位 | 未登记的币种一律失败，不猜它的单位精度。
func FromMinorUnits(value int64, currency string) (decimal.Decimal, error) {
	exponent, ok := minorUnitExponent(currency)
	if !ok {
		return decimal.Decimal{}, fmt.Errorf("%w: unsupported currency %s in response", ErrResponseInvalid, currency)
	}
	return decimal.NewFromInt(value).Shift(-exponent), nil
}

// IsOrderIdentifier 判定单号可以安全拼进请求路径 | 路径参与签名，混入 '/' 或 '?' 会让规范串与
// 服务端实际校验的地址错位，因此只接受字母数字与 -_。
func IsOrderIdentifier(identifier string) bool {
	text := strings.TrimSpace(identifier)
	if text == "" {
		return false
	}
	for _, r := range text {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

// newClient 用 SDK 建客户端 | 私钥解析、签名、信封检查都在 SDK 内。
func newClient(cfg *Config) (*access.Client, error) {
	if cfg == nil {
		return nil, fmt.Errorf("%w: config is nil", ErrConfigInvalid)
	}
	client, err := access.NewClient(access.Config{
		BaseURL: cfg.BaseURL,
		UserKey: access.UserKey{
			UniqueIdentifier:  cfg.UsingKey,
			ServicePrivateKey: cfg.ServicePrivateKey,
			PlatformPublicKey: cfg.PlatformPublicKey,
		},
		TenantMember: access.TenantMember{UniqueIdentifier: cfg.ActAs},
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrConfigInvalid, err)
	}
	return client, nil
}

// rawMap 把平台结构体转成 map | 落 provider_payload 供后台审计，多余字段也一并保留。
func rawMap(value interface{}) (map[string]interface{}, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrResponseInvalid, err)
	}
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrResponseInvalid, err)
	}
	return raw, nil
}

// mapAccessError 把 SDK 错误翻译成 provider 统一错误语义。
func mapAccessError(err error) error {
	if err == nil {
		return nil
	}
	var businessError *access.Error
	if errors.As(err, &businessError) {
		// 业务失败（HTTP 可能仍是 200）：带上错误码，便于排查幂等冲突、汇率缺失等平台侧拒绝。
		return fmt.Errorf("%w: [%s] %s", ErrRequestFailed, businessError.Code, businessError.Message)
	}
	var statusError *access.HTTPStatusError
	if errors.As(err, &statusError) {
		return fmt.Errorf("%w: http %d", ErrRequestFailed, statusError.StatusCode)
	}
	return fmt.Errorf("%w: %v", ErrRequestFailed, err)
}
