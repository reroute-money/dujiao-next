package reroutemoney

import (
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
	"net/url"
	"testing"
	"time"

	"github.com/dujiao-next/internal/constants"
	access "github.com/reroute-money/access-toolkit/golang"
	"github.com/shopspring/decimal"
)

// newPlatformKey 生成一把平台密钥 | 私钥用于伪造推送签名，公钥配到渠道里验签
func newPlatformKey(t *testing.T) (*rsa.PrivateKey, string) {
	t.Helper()
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	der, err := x509.MarshalPKIXPublicKey(&privateKey.PublicKey)
	if err != nil {
		t.Fatalf("marshal public key: %v", err)
	}
	publicPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
	return privateKey, publicPEM
}

// signNotify 复刻平台出站推送的签名 | 规范串与 access 包内 signatureContent 同源
func signNotify(t *testing.T, privateKey *rsa.PrivateKey, usingKey, actAs, path string, generationTime int64, body []byte) string {
	t.Helper()
	content := fmt.Sprintf("POST %s\r\n\r\nGeneration-Time: %d\r\nAct-As: %s\r\n\r\n%s", path, generationTime, actAs, string(body))
	digest := sha512.Sum512([]byte(content))
	signature, err := rsa.SignPKCS1v15(rand.Reader, privateKey, crypto.SHA512, digest[:])
	if err != nil {
		t.Fatalf("sign notify: %v", err)
	}
	parameters := url.Values{}
	parameters.Set("usingKey", usingKey)
	parameters.Set("actAs", actAs)
	parameters.Set("generationTime", fmt.Sprintf("%d", generationTime))
	parameters.Set("requestSignature", base64.StdEncoding.EncodeToString(signature))
	return "Signature " + parameters.Encode()
}

// signedHeaders 按渠道配置的 notify_url 签出推送头 | 平台签的是 pathname，不是整个 URL
func signedHeaders(t *testing.T, privateKey *rsa.PrivateKey, notifyURL string, generationTime int64, body []byte) map[string]string {
	t.Helper()
	path, err := NotifyPath(notifyURL)
	if err != nil {
		t.Fatalf("NotifyPath(%q): %v", notifyURL, err)
	}
	return map[string]string{
		"Authorization": signNotify(t, privateKey, "using-key-1", "-", path, generationTime, body),
	}
}

// notifyConfig 组一份只关心验签要素的渠道配置
func notifyConfig(t *testing.T, publicPEM, notifyURL string) *Config {
	t.Helper()
	cfg := &Config{PlatformPublicKey: publicPEM, NotifyURL: notifyURL}
	cfg.Normalize()
	if _, err := NotifyPath(cfg.NotifyURL); err != nil {
		t.Fatalf("notify config guard: %v", err)
	}
	return cfg
}

// notifyBody 平台推送报文 | 报文就是 Order 实体本身，不带 {code,data} 信封
func notifyBody(t *testing.T, payload map[string]interface{}) []byte {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal notify body: %v", err)
	}
	return body
}

// succeededCollectionNotify 一笔标准的代收成功推送
func succeededCollectionNotify(now time.Time) map[string]interface{} {
	return map[string]interface{}{
		"uniqueIdentifier":   "col-1",
		"externalIdentifier": "DJP1001",
		"type":               string(access.OrderTypeCollection),
		"status":             string(access.OrderStatusSucceeded),
		"paymentAmount":      map[string]interface{}{"currency": "USD", "value": 2050},
		"receiptAmount":      map[string]interface{}{"currency": "USDT", "value": 20500000},
		"callbackURL":        "https://api.example.com" + constants.DefaultRerouteMoneyWebhookPath,
		"createdAt":          now.Add(-time.Minute).UTC().Format(time.RFC3339Nano),
		"updatedAt":          now.UTC().Format(time.RFC3339Nano),
	}
}

func TestParseNotifyVerifiesSignedCollectionPush(t *testing.T) {
	privateKey, publicPEM := newPlatformKey(t)
	now := time.Now().UTC().Truncate(time.Second)
	body := notifyBody(t, succeededCollectionNotify(now))
	notifyURL := "https://api.example.com" + constants.DefaultRerouteMoneyWebhookPath
	cfg := notifyConfig(t, publicPEM, notifyURL)
	headers := signedHeaders(t, privateKey, notifyURL, now.UnixMilli(), body)
	data, err := ParseNotify(cfg, headers, body, now)
	if err != nil {
		t.Fatalf("ParseNotify: %v", err)
	}
	if data.UniqueIdentifier != "col-1" || data.ExternalIdentifier != "DJP1001" {
		t.Fatalf("identifiers = %+v", data)
	}
	if data.Status != constants.PaymentStatusSuccess {
		t.Fatalf("Status = %q, want %q", data.Status, constants.PaymentStatusSuccess)
	}
	if data.Type != string(access.OrderTypeCollection) {
		t.Fatalf("Type = %q", data.Type)
	}
	if data.RawStatus != string(access.OrderStatusSucceeded) {
		t.Fatalf("RawStatus = %q", data.RawStatus)
	}
	if data.PaymentCurrency != "USD" {
		t.Fatalf("PaymentCurrency = %q", data.PaymentCurrency)
	}
	if !data.PaymentAmount.Equal(decimal.RequireFromString("20.50")) {
		t.Fatalf("PaymentAmount = %s, want 20.50", data.PaymentAmount.String())
	}
	if data.PaidAt == nil || !data.PaidAt.Equal(now) {
		t.Fatalf("PaidAt = %v, want %s", data.PaidAt, now)
	}
	if data.Raw["externalIdentifier"] != "DJP1001" {
		t.Fatalf("Raw = %+v", data.Raw)
	}
}

func TestParseNotifyReadsHeaderCaseInsensitively(t *testing.T) {
	privateKey, publicPEM := newPlatformKey(t)
	now := time.Now().UTC().Truncate(time.Second)
	body := notifyBody(t, succeededCollectionNotify(now))
	notifyURL := "https://api.example.com" + constants.DefaultRerouteMoneyWebhookPath
	cfg := notifyConfig(t, publicPEM, notifyURL)

	// 反代与部分 HTTP 客户端会把头名转成不同大小写，取头必须不敏感
	lowercase := signedHeaders(t, privateKey, notifyURL, now.UnixMilli(), body)
	headers := map[string]string{"authorization": lowercase["Authorization"]}
	if _, err := ParseNotify(cfg, headers, body, now); err != nil {
		t.Fatalf("lowercase header must still verify: %v", err)
	}
}

func TestParseNotifyUsesConfiguredPathNotRequestPath(t *testing.T) {
	// 平台签的是 `new URL(callbackURL).pathname`，站点挂在反代子路径下时请求路径与它不同，
	// 验签必须以配置值为准，否则每一笔推送都会被判成伪造。
	privateKey, publicPEM := newPlatformKey(t)
	now := time.Now().UTC().Truncate(time.Second)
	body := notifyBody(t, succeededCollectionNotify(now))
	notifyURL := "https://api.example.com/shop" + constants.DefaultRerouteMoneyWebhookPath
	cfg := notifyConfig(t, publicPEM, notifyURL)
	headers := signedHeaders(t, privateKey, notifyURL, now.UnixMilli(), body)
	if _, err := ParseNotify(cfg, headers, body, now); err != nil {
		t.Fatalf("sub-path notify url must verify: %v", err)
	}
}

func TestParseNotifyRejectsTamperedBody(t *testing.T) {
	privateKey, publicPEM := newPlatformKey(t)
	now := time.Now().UTC().Truncate(time.Second)
	body := notifyBody(t, succeededCollectionNotify(now))
	notifyURL := "https://api.example.com" + constants.DefaultRerouteMoneyWebhookPath
	cfg := notifyConfig(t, publicPEM, notifyURL)
	// 签名覆盖原始报文，改动金额后必须验不过
	headers := signedHeaders(t, privateKey, notifyURL, now.UnixMilli(), body)
	tampered := notifyBody(t, map[string]interface{}{
		"uniqueIdentifier":   "col-1",
		"externalIdentifier": "DJP1001",
		"type":               string(access.OrderTypeCollection),
		"status":             string(access.OrderStatusSucceeded),
		"paymentAmount":      map[string]interface{}{"currency": "USD", "value": 1},
		"callbackURL":        notifyURL,
	})
	if _, err := ParseNotify(cfg, headers, tampered, now); !errors.Is(err, ErrSignatureInvalid) {
		t.Fatalf("err = %v, want ErrSignatureInvalid", err)
	}
}

func TestParseNotifyRejectsStaleAndMissingSignature(t *testing.T) {
	privateKey, publicPEM := newPlatformKey(t)
	now := time.Now().UTC().Truncate(time.Second)
	body := notifyBody(t, succeededCollectionNotify(now))
	notifyURL := "https://api.example.com" + constants.DefaultRerouteMoneyWebhookPath
	cfg := notifyConfig(t, publicPEM, notifyURL)

	stale := now.Add(-access.DefaultNotifyMaxSkew - time.Minute)
	staleHeaders := signedHeaders(t, privateKey, notifyURL, stale.UnixMilli(), body)
	if _, err := ParseNotify(cfg, staleHeaders, body, now); !errors.Is(err, ErrSignatureInvalid) {
		t.Fatalf("stale signature err = %v, want ErrSignatureInvalid", err)
	}

	for _, headers := range []map[string]string{nil, {}, {"Authorization": ""}, {"Authorization": "Bearer token"}} {
		if _, err := ParseNotify(cfg, headers, body, now); !errors.Is(err, ErrSignatureInvalid) {
			t.Fatalf("missing authorization err = %v, want ErrSignatureInvalid", err)
		}
	}
}

func TestParseNotifyRejectsUnparsablePublicKey(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	body := notifyBody(t, succeededCollectionNotify(now))
	cfg := notifyConfig(t, "not-a-pem", "https://api.example.com"+constants.DefaultRerouteMoneyWebhookPath)

	// 签名参数格式合法，只有公钥本身配错，才能落到 ErrConfigInvalid 而不是验签失败
	parameters := url.Values{}
	parameters.Set("usingKey", "using-key-1")
	parameters.Set("actAs", "-")
	parameters.Set("generationTime", fmt.Sprintf("%d", now.UnixMilli()))
	parameters.Set("requestSignature", base64.StdEncoding.EncodeToString([]byte("not-a-real-signature")))
	headers := map[string]string{"Authorization": "Signature " + parameters.Encode()}
	if _, err := ParseNotify(cfg, headers, body, now); !errors.Is(err, ErrConfigInvalid) {
		t.Fatalf("err = %v, want ErrConfigInvalid", err)
	}
}

func TestParseNotifyIgnoresNonCollectionPush(t *testing.T) {
	// 代付单可能推到同一个地址，它的成功与本站订单收款无关，状态留空让上层按“无需处理”忽略
	privateKey, publicPEM := newPlatformKey(t)
	now := time.Now().UTC().Truncate(time.Second)
	body := notifyBody(t, map[string]interface{}{
		"uniqueIdentifier":   "pay-1",
		"type":               string(access.OrderTypePayment),
		"status":             string(access.OrderStatusSucceeded),
		"externalIdentifier": "DJP1001",
	})
	notifyURL := "https://api.example.com" + constants.DefaultRerouteMoneyWebhookPath
	cfg := notifyConfig(t, publicPEM, notifyURL)
	headers := signedHeaders(t, privateKey, notifyURL, now.UnixMilli(), body)
	data, err := ParseNotify(cfg, headers, body, now)
	if err != nil {
		t.Fatalf("ParseNotify: %v", err)
	}
	if data.Status != "" {
		t.Fatalf("Status = %q, want empty so the push is ignored", data.Status)
	}
	if data.Type != string(access.OrderTypePayment) {
		t.Fatalf("Type = %q", data.Type)
	}
	if data.PaidAt != nil {
		t.Fatalf("PaidAt = %v, want nil for a non-collection push", data.PaidAt)
	}
}

func TestParseNotifyRequiresPaymentAmountOnCollectionPush(t *testing.T) {
	privateKey, publicPEM := newPlatformKey(t)
	now := time.Now().UTC().Truncate(time.Second)
	notifyURL := "https://api.example.com" + constants.DefaultRerouteMoneyWebhookPath
	cfg := notifyConfig(t, publicPEM, notifyURL)

	tests := []struct {
		name     string
		payload  map[string]interface{}
		accepted error
	}{
		{
			name: "missing payment amount",
			payload: map[string]interface{}{
				"uniqueIdentifier":   "col-1",
				"externalIdentifier": "DJP1001",
				"type":               string(access.OrderTypeCollection),
				"status":             string(access.OrderStatusSucceeded),
			},
			accepted: ErrResponseInvalid,
		},
		{
			name: "unregistered currency",
			payload: map[string]interface{}{
				"uniqueIdentifier":   "col-1",
				"externalIdentifier": "DJP1001",
				"type":               string(access.OrderTypeCollection),
				"status":             string(access.OrderStatusFailed),
				"paymentAmount":      map[string]interface{}{"currency": "AAA", "value": 100},
			},
			accepted: ErrResponseInvalid,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := notifyBody(t, tt.payload)
			if _, err := ParseNotify(cfg, signedHeaders(t, privateKey, notifyURL, now.UnixMilli(), body), body, now); !errors.Is(err, tt.accepted) {
				t.Fatalf("err = %v, want %v", err, tt.accepted)
			}
			// 报文换了却不重签，必须被判成伪造
			swapped := notifyBody(t, succeededCollectionNotify(now))
			if _, err := ParseNotify(cfg, signedHeaders(t, privateKey, notifyURL, now.UnixMilli(), swapped), body, now); !errors.Is(err, ErrSignatureInvalid) {
				t.Fatalf("body swapped without re-signing must not verify: %v", err)
			}
		})
	}
}

func TestParseNotifyFailedPushHasNoPaidAt(t *testing.T) {
	privateKey, publicPEM := newPlatformKey(t)
	now := time.Now().UTC().Truncate(time.Second)
	payload := succeededCollectionNotify(now)
	payload["status"] = string(access.OrderStatusFailed)
	body := notifyBody(t, payload)
	notifyURL := "https://api.example.com" + constants.DefaultRerouteMoneyWebhookPath
	cfg := notifyConfig(t, publicPEM, notifyURL)

	data, err := ParseNotify(cfg, signedHeaders(t, privateKey, notifyURL, now.UnixMilli(), body), body, now)
	if err != nil {
		t.Fatalf("ParseNotify: %v", err)
	}
	if data.Status != constants.PaymentStatusFailed {
		t.Fatalf("Status = %q, want %q", data.Status, constants.PaymentStatusFailed)
	}
	if data.PaidAt != nil {
		t.Fatalf("PaidAt = %v, want nil for a failed order", data.PaidAt)
	}
}

func TestParseNotifyRejectsUnparsableBody(t *testing.T) {
	privateKey, publicPEM := newPlatformKey(t)
	now := time.Now().UTC().Truncate(time.Second)
	body := []byte(`<html>gateway error</html>`)
	notifyURL := "https://api.example.com" + constants.DefaultRerouteMoneyWebhookPath
	cfg := notifyConfig(t, publicPEM, notifyURL)

	if _, err := ParseNotify(cfg, signedHeaders(t, privateKey, notifyURL, now.UnixMilli(), body), body, now); !errors.Is(err, ErrResponseInvalid) {
		t.Fatalf("err = %v, want ErrResponseInvalid", err)
	}
}

func TestParseNotifyGuardsItsInputs(t *testing.T) {
	now := time.Now()
	body := notifyBody(t, succeededCollectionNotify(now))
	if _, err := ParseNotify(nil, map[string]string{}, body, now); !errors.Is(err, ErrConfigInvalid) {
		t.Fatalf("nil config err = %v, want ErrConfigInvalid", err)
	}
	cfg := &Config{PlatformPublicKey: "not-a-pem"}
	cfg.Normalize()
	if _, err := ParseNotify(cfg, map[string]string{}, body, now); !errors.Is(err, ErrConfigInvalid) {
		t.Fatalf("missing notify_url err = %v, want ErrConfigInvalid", err)
	}
}

func TestPickHeader(t *testing.T) {
	headers := map[string]string{"X-Trace-Id": "1"}
	if got := pickHeader(headers, "Authorization"); got != "" {
		t.Fatalf("pickHeader = %q, want empty", got)
	}
	if got := pickHeader(map[string]string{"AUTHORIZATION": "sig"}, "Authorization"); got != "sig" {
		t.Fatalf("pickHeader = %q, want sig", got)
	}
	if got := pickHeader(nil, "Authorization"); got != "" {
		t.Fatalf("pickHeader(nil) = %q", got)
	}
}
