package reroutemoneyadapter

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	paymentcontract "github.com/dujiao-next/internal/modules/payment/contract"
	gatewaycommon "github.com/dujiao-next/internal/modules/payment/infrastructure/gateway/common"

	"github.com/dujiao-next/internal/constants"
	"github.com/dujiao-next/internal/modules/payment/infrastructure/gateway/reroutemoney"
	"github.com/dujiao-next/internal/shared/jsonmap"
	"github.com/dujiao-next/internal/shared/money"
)

// rerouteMoneyAdapter 是 Reroute Money 的 GatewayProvider + GatewayWebhooker + GatewayCapturer 实现。
//
// 渠道只有一种 channel_type（reroute-money），代收单走平台收银台跳转，
// 到账以异步推送为准，主动查单作为推送丢失时的兜底。
type rerouteMoneyAdapter struct{}

// NewRerouteMoneyAdapter 实例化 reroute.money adapter。
func NewRerouteMoneyAdapter() paymentcontract.GatewayProvider { return &rerouteMoneyAdapter{} }

var (
	_ paymentcontract.GatewayProvider  = (*rerouteMoneyAdapter)(nil)
	_ paymentcontract.GatewayWebhooker = (*rerouteMoneyAdapter)(nil)
	_ paymentcontract.GatewayCapturer  = (*rerouteMoneyAdapter)(nil)
)

// Type 返回 provider 标识。reroute.money 只有一种 channel_type，仍按多 channel 形式留空后缀，
// 让 Registry 落到 `reroute_money:` 回落键上。
func (a *rerouteMoneyAdapter) Type() string {
	return constants.PaymentProviderRerouteMoney + ":"
}

func (a *rerouteMoneyAdapter) parseConfig(raw jsonmap.JSON) (*reroutemoney.Config, error) {
	cfg, err := reroutemoney.ParseConfig(raw)
	if err != nil {
		return nil, mapRerouteMoneyError(err)
	}
	if err := reroutemoney.ValidateConfig(cfg); err != nil {
		return nil, mapRerouteMoneyError(err)
	}
	return cfg, nil
}

// ValidateConfig 校验 channel.ConfigJSON。
//
// 第二参数在两条链路上语义不同：后台保存渠道时传 channel_type，主动查单时传 interaction_mode
// （见 payment_service_capture.go:89）。命中 qr/redirect 视为交互方式放行，其余按渠道类型判定。
func (a *rerouteMoneyAdapter) ValidateConfig(raw jsonmap.JSON, parameter string) error {
	value := strings.ToLower(strings.TrimSpace(parameter))
	switch value {
	case "", constants.PaymentInteractionQR, constants.PaymentInteractionRedirect:
	default:
		if !reroutemoney.IsSupportedChannelType(value) {
			return fmt.Errorf("%w: reroute channel_type %s", paymentcontract.ErrGatewayUnsupportedChannel, value)
		}
	}
	cfg, err := a.parseConfig(raw)
	if err != nil {
		return err
	}
	// 验签规范串的 path 取配置 notify_url 的 pathname，填错地址会让每笔推送都验不过，
	// 支付永久停在待支付，所以保存阶段就要求它指向本站入口。
	if !reroutemoney.IsWebhookRoute(cfg.NotifyURL) {
		return fmt.Errorf("%w: notify_url 必须指向 %s", paymentcontract.ErrGatewayConfigInvalid, constants.DefaultRerouteMoneyWebhookPath)
	}
	return nil
}

// CreatePayment 创建代收单 | 锁付款侧（站点币种 + 订单金额），收款侧只交币种，由平台按命中汇率补齐。
func (a *rerouteMoneyAdapter) CreatePayment(ctx context.Context, raw jsonmap.JSON, input paymentcontract.GatewayCreateInput) (*paymentcontract.GatewayCreateResult, error) {
	if input.ChannelType != "" && !reroutemoney.IsSupportedChannelType(input.ChannelType) {
		return nil, fmt.Errorf("%w: reroute channel_type %s", paymentcontract.ErrGatewayUnsupportedChannel, input.ChannelType)
	}
	cfg, err := a.parseConfig(raw)
	if err != nil {
		return nil, err
	}

	callCtx, cancel := gatewaycommon.WithDefaultTimeout(ctx)
	defer cancel()

	// 回跳地址与其他 redirect 渠道同构：service 层按当前租户域名给出 input.ReturnURL，
	// 主站下单时为空则回落渠道配置的 return_url，最后 append 结果页所需的 order_no/marker 等 query。
	returnURL := strings.TrimSpace(input.ReturnURL)
	if returnURL == "" {
		returnURL = strings.TrimSpace(cfg.ReturnURL)
	}
	returnURL = gatewaycommon.AppendQueryParams(returnURL, input.ReturnURLQuery)

	result, err := reroutemoney.CreateCollectionOrder(callCtx, cfg, reroutemoney.CreateInput{
		ExternalIdentifier: input.OrderNo,
		PaymentCurrency:    input.Currency,
		PaymentAmount:      input.Amount.Decimal,
		ReceiptCurrency:    cfg.ReceiptCurrency,
		CallbackURL:        cfg.NotifyURL,
		ReturnURL:          returnURL,
	})
	if err != nil {
		return nil, mapRerouteMoneyError(err)
	}
	redirectURL := strings.TrimSpace(result.RedirectURL)
	if redirectURL == "" {
		// 没有跳转地址付款人无从付款，静默建单只会留下一笔永不达成的待支付。
		return nil, fmt.Errorf("%w: 渠道未返回付款跳转地址", paymentcontract.ErrGatewayResponseInvalid)
	}

	payload := jsonmap.JSON{}
	for key, value := range result.Order.Raw {
		payload[key] = value
	}
	payload["redirect_url"] = redirectURL
	paymentCurrency := strings.ToUpper(strings.TrimSpace(input.Currency))
	payload[paymentcontract.GatewayPayloadFiatCurrencySent] = paymentCurrency

	return &paymentcontract.GatewayCreateResult{
		ProviderRef: result.Order.UniqueIdentifier,
		RedirectURL: redirectURL,
		QRCodeURL:   redirectURL,
		Payload:     payload,
		// 提交给平台的就是订单本身的站点币种金额（收款侧才由汇率换算），
		// 因此回写的 AmountSent/CurrencySent 直接取入参，保证回调事实校验对得上。
		AmountSent:   input.Amount.Decimal.String(),
		CurrencySent: paymentCurrency,
	}, nil
}

// ParseWebhook 验签并解析平台订单推送 | 签名校验复用 SDK，规范串 path 取配置 notify_url。
func (a *rerouteMoneyAdapter) ParseWebhook(_ context.Context, raw jsonmap.JSON, headers map[string]string, body []byte, now time.Time) (*paymentcontract.GatewayCallbackResult, error) {
	cfg, err := reroutemoney.ParseConfig(raw)
	if err != nil {
		return nil, mapRerouteMoneyError(err)
	}
	data, err := reroutemoney.ParseNotify(cfg, headers, body, now)
	if err != nil {
		return nil, mapRerouteMoneyError(err)
	}

	payload := jsonmap.JSON{}
	for key, value := range data.Raw {
		payload[key] = value
	}
	if data.Type != "" {
		payload["order_type"] = data.Type
	}
	if data.RawStatus != "" {
		payload["order_status"] = data.RawStatus
	}

	return &paymentcontract.GatewayCallbackResult{
		OrderNo:     data.ExternalIdentifier,
		ProviderRef: data.UniqueIdentifier,
		Status:      data.Status,
		Amount:      money.FromDecimal(data.PaymentAmount),
		Currency:    data.PaymentCurrency,
		PaidAt:      data.PaidAt,
		Payload:     payload,
	}, nil
}

// QueryPayment 主动查询代收单（实现 GatewayCapturer）| 推送丢失时由前台轮询兜底到账。
func (a *rerouteMoneyAdapter) QueryPayment(ctx context.Context, raw jsonmap.JSON, providerRef string) (*paymentcontract.GatewayQueryResult, error) {
	cfg, err := a.parseConfig(raw)
	if err != nil {
		return nil, err
	}

	callCtx, cancel := gatewaycommon.WithDefaultTimeout(ctx)
	defer cancel()

	result, err := reroutemoney.QueryCollectionOrder(callCtx, cfg, providerRef)
	if err != nil {
		return nil, mapRerouteMoneyError(err)
	}

	payload := jsonmap.JSON{}
	for key, value := range result.Order.Raw {
		payload[key] = value
	}
	payload["order_status"] = result.Order.RawStatus

	var paidAt *time.Time
	if result.Order.Status == constants.PaymentStatusSuccess {
		updatedAt := result.Order.UpdatedAt
		paidAt = &updatedAt
	}

	return &paymentcontract.GatewayQueryResult{
		ProviderRef: result.Order.UniqueIdentifier,
		Status:      result.Order.Status,
		Amount:      money.FromDecimal(result.Order.PaymentAmount),
		Currency:    result.Order.PaymentCurrency,
		PaidAt:      paidAt,
		Payload:     payload,
	}, nil
}

// mapRerouteMoneyError 把 reroutemoney 包的 sentinel error 映射为 provider 统一错误。
func mapRerouteMoneyError(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, reroutemoney.ErrConfigInvalid):
		return fmt.Errorf("%w: %v", paymentcontract.ErrGatewayConfigInvalid, err)
	case errors.Is(err, reroutemoney.ErrRequestFailed):
		return fmt.Errorf("%w: %v", paymentcontract.ErrGatewayRequestFailed, err)
	case errors.Is(err, reroutemoney.ErrResponseInvalid):
		return fmt.Errorf("%w: %v", paymentcontract.ErrGatewayResponseInvalid, err)
	case errors.Is(err, reroutemoney.ErrSignatureInvalid):
		return fmt.Errorf("%w: %v", paymentcontract.ErrGatewaySignatureInvalid, err)
	default:
		return err
	}
}
