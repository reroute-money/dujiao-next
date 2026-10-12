package reroutemoney

import "testing"

func TestMinorUnitExponentCoversSiteCurrencyChoices(t *testing.T) {
	// 后台「系统设置 → 店铺币种」的下拉来自 Intl.supportedValuesOf('currency')，
	// 这里至少兜住它拿不到数据时的内置候选表（frontend/admin/src/views/admin/Settings.vue）。
	for _, code := range []string{
		"CNY", "USD", "EUR", "GBP", "JPY", "KRW", "HKD", "TWD", "SGD", "AUD",
		"CAD", "CHF", "NZD", "SEK", "NOK", "DKK", "AED", "SAR", "MYR", "THB",
		"PHP", "IDR", "VND", "INR", "RUB", "TRY", "ZAR", "BRL", "MXN", "ARS",
	} {
		if !IsSupportedCurrency(code) {
			t.Fatalf("IsSupportedCurrency(%s) = false, 后台可选币种必须能换算", code)
		}
	}

	tests := map[string]int32{
		"USD":    2,
		"CNY":    2,
		"IDR":    2, // CLDR 报 0 位，照它下单会把金额缩小 100 倍
		"HUF":    2,
		"JPY":    0,
		"KRW":    0,
		"BHD":    3,
		"IQD":    3,
		"CLF":    4,
		"usdt":   6,
		" USDT ": 6,
	}
	for code, want := range tests {
		got, ok := minorUnitExponent(code)
		if !ok {
			t.Fatalf("minorUnitExponent(%q) missing", code)
		}
		if got != want {
			t.Fatalf("minorUnitExponent(%q) = %d, want %d", code, got, want)
		}
	}

	for _, code := range []string{"AAA", "XAU", "XDR", "XXX", "", "USDD"} {
		if _, ok := minorUnitExponent(code); ok {
			t.Fatalf("minorUnitExponent(%q) should be unregistered", code)
		}
	}
}
