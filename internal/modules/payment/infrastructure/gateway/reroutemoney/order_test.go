package reroutemoney

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dujiao-next/internal/constants"
	access "github.com/reroute-money/access-toolkit/golang"
	"github.com/shopspring/decimal"
)

// newConfiguredClient 组一份可直接下单的渠道配置 | base_url 指向测试服务器
func newConfiguredClient(t *testing.T, baseURL string) *Config {
	t.Helper()
	privatePEM, publicPEM := newTestPEMKeys(t)
	raw := validConfig(privatePEM, publicPEM)
	raw["base_url"] = baseURL
	cfg, err := ParseConfig(raw)
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	return cfg
}

// envelopeOK 拼一个 SDK 认得的业务成功信封
func envelopeOK(t *testing.T, data interface{}) string {
	t.Helper()
	payload, err := json.Marshal(map[string]interface{}{"code": access.CodeOK, "data": data})
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return string(payload)
}

func collectionOrderData(redirectURL string) map[string]interface{} {
	order := map[string]interface{}{
		"uniqueIdentifier":   "col-1",
		"externalIdentifier": "DJP1001",
		"type":               string(access.OrderTypeCollection),
		"status":             string(access.OrderStatusCreated),
		"paymentAmount":      map[string]interface{}{"currency": "USD", "value": 2050},
		"receiptAmount":      map[string]interface{}{"currency": "USD", "value": 2050},
		"callbackURL":        "https://api.example.com" + constants.DefaultRerouteMoneyWebhookPath,
	}
	data := map[string]interface{}{"uniqueIdentifier": "col-1", "order": order}
	if redirectURL != "" {
		data["redirectURL"] = redirectURL
	}
	return data
}

func TestCreateCollectionOrdersLocksPaymentSide(t *testing.T) {
	var received map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Fatalf("method = %s, want PUT", r.Method)
		}
		if r.URL.Path != "/v2/collection-order" {
			t.Fatalf("path = %s, want /v2/collection-order", r.URL.Path)
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Signature ") {
			t.Fatalf("Authorization header missing: %q", r.Header.Get("Authorization"))
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if err := json.Unmarshal(body, &received); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		_, _ = w.Write([]byte(envelopeOK(t, collectionOrderData("https://checkout.example.com/col-1"))))
	}))
	defer server.Close()

	cfg := newConfiguredClient(t, server.URL)
	result, err := CreateCollectionOrder(context.Background(), cfg, CreateInput{
		ExternalIdentifier: "DJP1001",
		PaymentCurrency:    "usd",
		PaymentAmount:      decimal.RequireFromString("20.50"),
		ReceiptCurrency:    "USDT",
		CallbackURL:        "https://api.example.com" + constants.DefaultRerouteMoneyWebhookPath,
		ReturnURL:          "https://api.example.com/pay?order_no=DJP1001",
	})
	if err != nil {
		t.Fatalf("CreateCollectionOrder: %v", err)
	}

	order, _ := received["order"].(map[string]interface{})
	if order == nil {
		t.Fatalf("request has no order payload: %+v", received)
	}
	if order["externalIdentifier"] != "DJP1001" {
		t.Fatalf("externalIdentifier = %v", order["externalIdentifier"])
	}
	paymentAmount, _ := order["paymentAmount"].(map[string]interface{})
	// 付款侧锁死：币种是站点币种，金额是分单位的订单金额
	if paymentAmount["currency"] != "USD" || paymentAmount["value"] != float64(2050) {
		t.Fatalf("paymentAmount = %+v, want USD/2050", paymentAmount)
	}
	receiptAmount, _ := order["receiptAmount"].(map[string]interface{})
	// 收款侧只交币种，金额由平台按命中汇率补齐
	if receiptAmount["currency"] != "USDT" {
		t.Fatalf("receiptAmount currency = %v", receiptAmount["currency"])
	}
	if _, exists := receiptAmount["value"]; exists {
		t.Fatalf("receiptAmount must not carry a value: %+v", receiptAmount)
	}
	product, _ := order["product"].(map[string]interface{})
	if product["type"] != DefaultProductType {
		t.Fatalf("product type = %v, want %s", product["type"], DefaultProductType)
	}
	if order["callbackURL"] != "https://api.example.com"+constants.DefaultRerouteMoneyWebhookPath {
		t.Fatalf("callbackURL = %v", order["callbackURL"])
	}
	// 回跳地址是 CollectionOrder 的顶层字段，不在 order 之内，平台原样下发渠道
	if received["returnURL"] != "https://api.example.com/pay?order_no=DJP1001" {
		t.Fatalf("returnURL = %v", received["returnURL"])
	}
	if order["returnURL"] != nil {
		t.Fatalf("returnURL must not be nested in order: %+v", order)
	}

	if result.RedirectURL != "https://checkout.example.com/col-1" {
		t.Fatalf("RedirectURL = %q", result.RedirectURL)
	}
	if result.Order.UniqueIdentifier != "col-1" || result.Order.ExternalIdentifier != "DJP1001" {
		t.Fatalf("snapshot = %+v", result.Order)
	}
	if result.Order.Status != constants.PaymentStatusPending {
		t.Fatalf("Status = %q, want %q", result.Order.Status, constants.PaymentStatusPending)
	}
	if !result.Order.PaymentAmount.Equal(decimal.RequireFromString("20.50")) {
		t.Fatalf("PaymentAmount = %s", result.Order.PaymentAmount.String())
	}
}

func TestCreateCollectionOrderRejectsBadInput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("server must not be reached for invalid input")
	}))
	defer server.Close()
	cfg := newConfiguredClient(t, server.URL)

	tests := []struct {
		name  string
		input CreateInput
	}{
		{"missing external identifier", CreateInput{PaymentCurrency: "USD", PaymentAmount: decimal.NewFromInt(1)}},
		{"unsupported payment currency", CreateInput{ExternalIdentifier: "DJP1001", PaymentCurrency: "CNY", PaymentAmount: decimal.NewFromInt(1)}},
		{"sub-minor amount", CreateInput{ExternalIdentifier: "DJP1001", PaymentCurrency: "USD", PaymentAmount: decimal.RequireFromString("1.005")}},
		{"negative amount", CreateInput{ExternalIdentifier: "DJP1001", PaymentCurrency: "USD", PaymentAmount: decimal.NewFromInt(-1)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := CreateCollectionOrder(context.Background(), cfg, tt.input); !errors.Is(err, ErrConfigInvalid) {
				t.Fatalf("err = %v, want ErrConfigInvalid", err)
			}
		})
	}
}

func TestCreateCollectionOrderFallsBackToPaymentCurrency(t *testing.T) {
	var received map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &received)
		_, _ = w.Write([]byte(envelopeOK(t, collectionOrderData("https://checkout.example.com/col-1"))))
	}))
	defer server.Close()

	cfg := newConfiguredClient(t, server.URL)
	if _, err := CreateCollectionOrder(context.Background(), cfg, CreateInput{
		ExternalIdentifier: "DJP1001",
		PaymentCurrency:    "INR",
		PaymentAmount:      decimal.RequireFromString("100"),
	}); err != nil {
		t.Fatalf("CreateCollectionOrder: %v", err)
	}
	order, _ := received["order"].(map[string]interface{})
	receiptAmount, _ := order["receiptAmount"].(map[string]interface{})
	if receiptAmount["currency"] != "INR" {
		t.Fatalf("receiptAmount = %+v, want INR", receiptAmount)
	}
}

func TestCreateCollectionOrderSurfacesPlatformRejection(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"code":"ORDER_IDEMPOTENCY_CONFLICT","message":"external identifier already used"}`))
	}))
	defer server.Close()

	cfg := newConfiguredClient(t, server.URL)
	_, err := CreateCollectionOrder(context.Background(), cfg, CreateInput{
		ExternalIdentifier: "DJP1001",
		PaymentCurrency:    "USD",
		PaymentAmount:      decimal.NewFromInt(1),
	})
	if !errors.Is(err, ErrRequestFailed) {
		t.Fatalf("err = %v, want ErrRequestFailed", err)
	}
	if !strings.Contains(err.Error(), "ORDER_IDEMPOTENCY_CONFLICT") {
		t.Fatalf("platform code lost: %v", err)
	}
}

func TestCreateCollectionOrderSurfacesTransportFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html>bad gateway</html>"))
	}))
	defer server.Close()

	cfg := newConfiguredClient(t, server.URL)
	if _, err := CreateCollectionOrder(context.Background(), cfg, CreateInput{
		ExternalIdentifier: "DJP1001",
		PaymentCurrency:    "USD",
		PaymentAmount:      decimal.NewFromInt(1),
	}); !errors.Is(err, ErrRequestFailed) {
		t.Fatalf("err = %v, want ErrRequestFailed", err)
	}
}

func TestQueryCollectionOrderReadsSettledAmount(t *testing.T) {
	var requestedPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestedPath = r.URL.Path
		if r.Method != http.MethodGet {
			t.Fatalf("method = %s, want GET", r.Method)
		}
		data := collectionOrderData("")
		order, _ := data["order"].(map[string]interface{})
		order["status"] = string(access.OrderStatusSucceeded)
		order["paymentAmount"] = map[string]interface{}{"currency": "USDT", "value": float64(1500000)}
		_, _ = w.Write([]byte(envelopeOK(t, data)))
	}))
	defer server.Close()

	cfg := newConfiguredClient(t, server.URL)
	result, err := QueryCollectionOrder(context.Background(), cfg, "col-1")
	if err != nil {
		t.Fatalf("QueryCollectionOrder: %v", err)
	}
	if requestedPath != "/v2/collection-order/col-1" {
		t.Fatalf("path = %q", requestedPath)
	}
	if result.Order.Status != constants.PaymentStatusSuccess {
		t.Fatalf("Status = %q, want %q", result.Order.Status, constants.PaymentStatusSuccess)
	}
	if !result.Order.PaymentAmount.Equal(decimal.RequireFromString("1.5")) {
		t.Fatalf("PaymentAmount = %s, want 1.5", result.Order.PaymentAmount.String())
	}
	if result.Order.PaymentCurrency != "USDT" {
		t.Fatalf("PaymentCurrency = %q", result.Order.PaymentCurrency)
	}
}

func TestQueryCollectionOrderRejectsUnsafeIdentifier(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("server must not be reached for an unsafe identifier")
	}))
	defer server.Close()

	cfg := newConfiguredClient(t, server.URL)
	// 单号拼进路径且路径参与签名，带分隔符的单号会让规范串与服务端校验的地址错位
	if _, err := QueryCollectionOrder(context.Background(), cfg, "../payment-order"); !errors.Is(err, ErrConfigInvalid) {
		t.Fatalf("err = %v, want ErrConfigInvalid", err)
	}
	if _, err := QueryCollectionOrder(context.Background(), cfg, ""); !errors.Is(err, ErrConfigInvalid) {
		t.Fatalf("empty identifier err = %v, want ErrConfigInvalid", err)
	}
}

func TestCreateCollectionOrderRejectsUnparsableConfig(t *testing.T) {
	if _, err := CreateCollectionOrder(context.Background(), nil, CreateInput{}); !errors.Is(err, ErrConfigInvalid) {
		t.Fatalf("err = %v, want ErrConfigInvalid", err)
	}
	if _, err := QueryCollectionOrder(context.Background(), nil, "col-1"); !errors.Is(err, ErrConfigInvalid) {
		t.Fatalf("err = %v, want ErrConfigInvalid", err)
	}
}
