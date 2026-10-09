package reroutemoney

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dujiao-next/internal/constants"
	access "github.com/reroute-money/access-toolkit/golang"
	"github.com/shopspring/decimal"
)

// newTestPEMKeys 生成一对测试用 RSA 密钥 | 私钥用 PKCS#1，公钥用 SPKI，与平台下发格式一致
func newTestPEMKeys(t *testing.T) (string, string) {
	t.Helper()
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	privatePEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(privateKey)})
	publicDER, err := x509.MarshalPKIXPublicKey(&privateKey.PublicKey)
	if err != nil {
		t.Fatalf("marshal public key: %v", err)
	}
	publicPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER})
	return string(privatePEM), string(publicPEM)
}

// validConfig 返回一份字段齐全的渠道配置
func validConfig(privatePEM, publicPEM string) map[string]interface{} {
	return map[string]interface{}{
		"base_url":            "https://access.example.com",
		"using_key":           "using-key-1",
		"act_as":              "member-1",
		"service_private_key": privatePEM,
		"platform_public_key": publicPEM,
		"notify_url":          "https://api.example.com" + constants.DefaultRerouteMoneyWebhookPath,
	}
}

func TestParseConfigAppliesDefaults(t *testing.T) {
	privatePEM, publicPEM := newTestPEMKeys(t)
	cfg, err := ParseConfig(validConfig(privatePEM, publicPEM))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if cfg.BaseURL != "https://access.example.com" {
		t.Fatalf("BaseURL = %q", cfg.BaseURL)
	}
	if cfg.ProductType != DefaultProductType {
		t.Fatalf("ProductType = %q, want %q", cfg.ProductType, DefaultProductType)
	}
	if cfg.ReceiptCurrency != "" {
		t.Fatalf("ReceiptCurrency = %q, want empty", cfg.ReceiptCurrency)
	}
	if err := ValidateConfig(cfg); err != nil {
		t.Fatalf("ValidateConfig: %v", err)
	}
}

func TestParseConfigNormalizesAndTrimsBaseURL(t *testing.T) {
	privatePEM, publicPEM := newTestPEMKeys(t)
	raw := validConfig(privatePEM, publicPEM)
	raw["base_url"] = "https://access.example.com/"
	raw["using_key"] = "  using-key-1 "
	raw["receipt_currency"] = "usdt"
	raw["product_type"] = "online_receive"

	cfg, err := ParseConfig(raw)
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if cfg.BaseURL != "https://access.example.com" {
		t.Fatalf("BaseURL = %q", cfg.BaseURL)
	}
	if cfg.UsingKey != "using-key-1" {
		t.Fatalf("UsingKey = %q", cfg.UsingKey)
	}
	if cfg.ReceiptCurrency != "USDT" {
		t.Fatalf("ReceiptCurrency = %q, want USDT", cfg.ReceiptCurrency)
	}
	if cfg.ProductType != "ONLINE_RECEIVE" {
		t.Fatalf("ProductType = %q", cfg.ProductType)
	}
}

func TestParseConfigRejectsMissingRaw(t *testing.T) {
	if _, err := ParseConfig(nil); !errors.Is(err, ErrConfigInvalid) {
		t.Fatalf("err = %v, want ErrConfigInvalid", err)
	}
}

func TestValidateConfigRequiresEveryCredential(t *testing.T) {
	privatePEM, publicPEM := newTestPEMKeys(t)
	for _, field := range []string{"using_key", "act_as", "service_private_key", "platform_public_key", "notify_url"} {
		t.Run(field, func(t *testing.T) {
			raw := validConfig(privatePEM, publicPEM)
			raw[field] = ""
			cfg, err := ParseConfig(raw)
			if err != nil {
				t.Fatalf("ParseConfig: %v", err)
			}
			if err := ValidateConfig(cfg); !errors.Is(err, ErrConfigInvalid) {
				t.Fatalf("err = %v, want ErrConfigInvalid", err)
			}
		})
	}
}

func TestValidateConfigRejectsUnregisteredReceiptCurrency(t *testing.T) {
	privatePEM, publicPEM := newTestPEMKeys(t)
	raw := validConfig(privatePEM, publicPEM)
	raw["receipt_currency"] = "cny"
	cfg, err := ParseConfig(raw)
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if err := ValidateConfig(cfg); !errors.Is(err, ErrConfigInvalid) {
		t.Fatalf("err = %v, want ErrConfigInvalid", err)
	}
}

func TestValidateConfigRejectsUnparsablePrivateKey(t *testing.T) {
	_, publicPEM := newTestPEMKeys(t)
	raw := validConfig("not-a-pem", publicPEM)
	cfg, err := ParseConfig(raw)
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	// 私钥格式在保存渠道时就要暴露，不能留到买家付款那一刻
	if err := ValidateConfig(cfg); !errors.Is(err, ErrConfigInvalid) {
		t.Fatalf("err = %v, want ErrConfigInvalid", err)
	}
}

func TestValidateConfigNilConfig(t *testing.T) {
	if err := ValidateConfig(nil); !errors.Is(err, ErrConfigInvalid) {
		t.Fatalf("err = %v, want ErrConfigInvalid", err)
	}
}

func TestNotifyPath(t *testing.T) {
	tests := []struct {
		name string
		url  string
		path string
	}{
		{"absolute", "https://api.example.com" + constants.DefaultRerouteMoneyWebhookPath, constants.DefaultRerouteMoneyWebhookPath},
		{"with query", "https://api.example.com" + constants.DefaultRerouteMoneyWebhookPath + "?a=1", constants.DefaultRerouteMoneyWebhookPath},
		{"behind sub path", "https://api.example.com/shop" + constants.DefaultRerouteMoneyWebhookPath, "/shop" + constants.DefaultRerouteMoneyWebhookPath},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path, err := NotifyPath(tt.url)
			if err != nil {
				t.Fatalf("NotifyPath(%q): %v", tt.url, err)
			}
			if path != tt.path {
				t.Fatalf("path = %q, want %q", path, tt.path)
			}
		})
	}

	for _, raw := range []string{"", "   ", "/api/v1/payments/webhook/reroute-money", "https://api.example.com", "https://api.example.com/", "not a url"} {
		if _, err := NotifyPath(raw); !errors.Is(err, ErrConfigInvalid) {
			t.Fatalf("NotifyPath(%q) err = %v, want ErrConfigInvalid", raw, err)
		}
	}
}

func TestIsWebhookRoute(t *testing.T) {
	if !IsWebhookRoute("https://api.example.com" + constants.DefaultRerouteMoneyWebhookPath) {
		t.Fatal("default webhook path must be accepted")
	}
	if !IsWebhookRoute("https://api.example.com/shop" + constants.DefaultRerouteMoneyWebhookPath) {
		t.Fatal("path behind a reverse proxy prefix must be accepted")
	}
	for _, raw := range []string{"", "https://api.example.com/api/v1/payments/callback", "https://api.example.com/webhook/reroute-money/extra"} {
		if IsWebhookRoute(raw) {
			t.Fatalf("IsWebhookRoute(%q) must be false", raw)
		}
	}
}

func TestIsSupportedChannelType(t *testing.T) {
	if !IsSupportedChannelType(constants.PaymentChannelTypeRerouteMoney) {
		t.Fatal("reroute-money must be supported")
	}
	if !IsSupportedChannelType(" Reroute-Money ") {
		t.Fatal("channel type match must be case and space insensitive")
	}
	if IsSupportedChannelType("usdt") || IsSupportedChannelType("") {
		t.Fatal("other channel types must not be routed to reroute")
	}
}

func TestIsOrderIdentifier(t *testing.T) {
	for _, ok := range []string{"order-1", "ORDER_1", "abc123"} {
		if !IsOrderIdentifier(ok) {
			t.Fatalf("IsOrderIdentifier(%q) must be true", ok)
		}
	}
	// 单号会拼进请求路径并参与签名串，任何分隔符都会让规范串与服务端校验的地址错位
	for _, bad := range []string{"", "  ", "order/1", "order?1", "order#1", "order 1", "order%2F1"} {
		if IsOrderIdentifier(bad) {
			t.Fatalf("IsOrderIdentifier(%q) must be false", bad)
		}
	}
}

func TestMinorUnitExponents(t *testing.T) {
	tests := []struct {
		amount   string
		currency string
		minor    int64
	}{
		{"1.00", "INR", 100},
		{"20.50", "USD", 2050},
		{"1.5", "USDT", 1500000},
		{"0", "USDT", 0},
	}
	for _, tt := range tests {
		minor, err := ToMinorUnits(decimal.RequireFromString(tt.amount), tt.currency)
		if err != nil {
			t.Fatalf("ToMinorUnits(%s %s): %v", tt.amount, tt.currency, err)
		}
		if minor != tt.minor {
			t.Fatalf("ToMinorUnits(%s %s) = %d, want %d", tt.amount, tt.currency, minor, tt.minor)
		}
		back, err := FromMinorUnits(minor, tt.currency)
		if err != nil {
			t.Fatalf("FromMinorUnits(%d %s): %v", minor, tt.currency, err)
		}
		if !back.Equal(decimal.RequireFromString(tt.amount)) {
			t.Fatalf("round trip = %s, want %s", back.String(), tt.amount)
		}
	}
}

func TestToMinorUnitsRejectsAmountsItCannotRepresent(t *testing.T) {
	if _, err := ToMinorUnits(decimal.RequireFromString("1.005"), "USD"); !errors.Is(err, ErrConfigInvalid) {
		t.Fatalf("sub-minor precision err = %v, want ErrConfigInvalid", err)
	}
	if _, err := ToMinorUnits(decimal.RequireFromString("-1"), "USD"); !errors.Is(err, ErrConfigInvalid) {
		t.Fatalf("negative err = %v, want ErrConfigInvalid", err)
	}
	if _, err := ToMinorUnits(decimal.RequireFromString("1"), "CNY"); !errors.Is(err, ErrConfigInvalid) {
		t.Fatalf("unknown currency err = %v, want ErrConfigInvalid", err)
	}
	// USDT 的最小单位是 10^-6，比它更细的金额依然非法
	if _, err := ToMinorUnits(decimal.RequireFromString("0.0000001"), "USDT"); !errors.Is(err, ErrConfigInvalid) {
		t.Fatalf("sub-minor USDT err = %v, want ErrConfigInvalid", err)
	}
}

func TestFromMinorUnitsRejectsUnknownCurrency(t *testing.T) {
	// 平台回了没登记过的币种时不能猜它的单位精度，宁可让这笔回调失败
	amount, err := FromMinorUnits(100, "CNY")
	if !errors.Is(err, ErrResponseInvalid) {
		t.Fatalf("err = %v, want ErrResponseInvalid", err)
	}
	if !amount.IsZero() {
		t.Fatalf("amount = %s, want zero", amount.String())
	}
}

func TestIsSupportedCurrency(t *testing.T) {
	for _, currency := range []string{"INR", "usd", " USDT "} {
		if !IsSupportedCurrency(currency) {
			t.Fatalf("IsSupportedCurrency(%q) must be true", currency)
		}
	}
	for _, currency := range []string{"", "CNY", "HKD"} {
		if IsSupportedCurrency(currency) {
			t.Fatalf("IsSupportedCurrency(%q) must be false", currency)
		}
	}
}

func TestToPaymentStatus(t *testing.T) {
	tests := []struct {
		status access.OrderStatus
		want   string
	}{
		{access.OrderStatusSucceeded, constants.PaymentStatusSuccess},
		{access.OrderStatusFailed, constants.PaymentStatusFailed},
		{access.OrderStatusCreated, constants.PaymentStatusPending},
		{access.OrderStatusProcessing, constants.PaymentStatusPending},
		{access.OrderStatus("REFUNDED"), ""},
	}
	for _, tt := range tests {
		if got := ToPaymentStatus(tt.status); got != tt.want {
			t.Fatalf("ToPaymentStatus(%s) = %q, want %q", tt.status, got, tt.want)
		}
	}
}

func TestNewClientUsesSDKDefaults(t *testing.T) {
	privatePEM, publicPEM := newTestPEMKeys(t)
	cfg, err := ParseConfig(validConfig(privatePEM, publicPEM))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	cfg.BaseURL = ""
	cfg.Normalize()
	if cfg.BaseURL != access.DefaultBaseURL {
		t.Fatalf("BaseURL = %q, want %q", cfg.BaseURL, access.DefaultBaseURL)
	}
	if _, err := newClient(cfg); err != nil {
		t.Fatalf("newClient: %v", err)
	}
	if _, err := newClient(nil); !errors.Is(err, ErrConfigInvalid) {
		t.Fatalf("newClient(nil) err = %v, want ErrConfigInvalid", err)
	}
}

func TestMapAccessErrorTranslatesEnvelopeFailure(t *testing.T) {
	business := error(&access.Error{Code: "IDEMPOTENT_CONFLICT", Message: "duplicated order"})
	translated := mapAccessError(business)
	if !errors.Is(translated, ErrRequestFailed) {
		t.Fatalf("err = %v, want ErrRequestFailed", translated)
	}
	if !strings.Contains(translated.Error(), "IDEMPOTENT_CONFLICT") {
		t.Fatalf("translated error loses the platform code: %v", translated)
	}

	status := error(&access.HTTPStatusError{StatusCode: 502, Body: "bad gateway"})
	if translated := mapAccessError(status); !errors.Is(translated, ErrRequestFailed) || !strings.Contains(translated.Error(), "502") {
		t.Fatalf("status err = %v, want ErrRequestFailed with 502", translated)
	}

	plain := errors.New("dial timeout")
	if translated := mapAccessError(plain); !errors.Is(translated, ErrRequestFailed) {
		t.Fatalf("plain err = %v, want ErrRequestFailed", translated)
	}
	if mapAccessError(nil) != nil {
		t.Fatal("mapAccessError(nil) must stay nil")
	}
}

func TestRawMapKeepsEveryPlatformField(t *testing.T) {
	raw, err := rawMap(map[string]interface{}{"uniqueIdentifier": "order-1", "status": "SUCCEEDED"})
	if err != nil {
		t.Fatalf("rawMap: %v", err)
	}
	if raw["uniqueIdentifier"] != "order-1" {
		t.Fatalf("raw = %+v", raw)
	}
	if _, err := rawMap(func() {}); !errors.Is(err, ErrResponseInvalid) {
		t.Fatalf("unmarshalable value err = %v, want ErrResponseInvalid", err)
	}
}

func TestToSnapshotTranslatesMinorUnits(t *testing.T) {
	value := int64(12_345)
	updatedAt := time.Now().UTC().Truncate(time.Second)
	snapshot, err := toSnapshot(&access.OrderResult{
		UniqueIdentifier:   "order-1",
		ExternalIdentifier: "DJP1001",
		Status:             access.OrderStatusSucceeded,
		PaymentAmount:      &access.Money{Currency: access.CurrencyUSD, Value: value},
		ReceiptAmount:      &access.Money{Currency: access.CurrencyUSDT, Value: value},
		UpdatedAt:          updatedAt,
	})
	if err != nil {
		t.Fatalf("toSnapshot: %v", err)
	}
	if snapshot.Status != constants.PaymentStatusSuccess {
		t.Fatalf("Status = %q", snapshot.Status)
	}
	if !snapshot.PaymentAmount.Equal(decimal.RequireFromString("123.45")) {
		t.Fatalf("PaymentAmount = %s, want 123.45", snapshot.PaymentAmount.String())
	}
	if snapshot.PaymentCurrency != "USD" {
		t.Fatalf("PaymentCurrency = %q", snapshot.PaymentCurrency)
	}
	if !snapshot.ReceiptAmount.Equal(decimal.RequireFromString("0.012345")) {
		t.Fatalf("ReceiptAmount = %s, want 0.012345", snapshot.ReceiptAmount.String())
	}
	if snapshot.RawStatus != string(access.OrderStatusSucceeded) {
		t.Fatalf("RawStatus = %q", snapshot.RawStatus)
	}
	if snapshot.Raw["uniqueIdentifier"] != "order-1" {
		t.Fatalf("Raw = %+v", snapshot.Raw)
	}
	if !snapshot.UpdatedAt.Equal(updatedAt) {
		t.Fatalf("UpdatedAt = %s", snapshot.UpdatedAt.String())
	}
}

func TestToSnapshotRejectsUnregisteredCurrency(t *testing.T) {
	value := int64(100)
	if _, err := toSnapshot(&access.OrderResult{
		PaymentAmount: &access.Money{Currency: access.Currency("CNY"), Value: value},
	}); !errors.Is(err, ErrResponseInvalid) {
		t.Fatalf("err = %v, want ErrResponseInvalid", err)
	}
}

func TestToSnapshotNilOrder(t *testing.T) {
	if _, err := toSnapshot(nil); !errors.Is(err, ErrResponseInvalid) {
		t.Fatalf("err = %v, want ErrResponseInvalid", err)
	}
}
