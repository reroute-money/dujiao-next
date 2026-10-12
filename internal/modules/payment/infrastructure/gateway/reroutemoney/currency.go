package reroutemoney

import "strings"

// iso4217MinorUnits 收录 ISO 4217（发布版 2024-06-25）全部有最小单位的币种及其 10 的负指数次分位。
//
// 这份表是金额换算的唯一依据：平台收付的 Amount.value 是最小币种单位，指数错一位就是 10 倍偏差。
// 取的是 ISO 4217 的 CcyMnrUnts，不是 CLDR/Intl 的展示精度 —— 后者把 IDR/COP/HUF/PKR/MGA 等 16 个
// 币种报成 0 位，照它下单会把这些款项缩小 100 倍。
// XAU/XDR/XXX 等 13 个记帐货币在 ISO 里没有最小单位（CcyMnrUnts 为 XXX），不入表，落到显式失败。
var iso4217MinorUnits = map[string]int32{
	// 无小数分位 | 按 2 位发就多收 100 倍，JPY/KRW/VND 都在这里
	"BIF": 0, "CLP": 0, "DJF": 0, "GNF": 0, "ISK": 0,
	"JPY": 0, "KMF": 0, "KRW": 0, "PYG": 0, "RWF": 0,
	"UGX": 0, "UYI": 0, "VND": 0, "VUV": 0, "XAF": 0,
	"XOF": 0, "XPF": 0,
	// 两位分位 | 绝大多数法币
	"AED": 2, "AFN": 2, "ALL": 2, "AMD": 2, "ANG": 2,
	"AOA": 2, "ARS": 2, "AUD": 2, "AWG": 2, "AZN": 2,
	"BAM": 2, "BBD": 2, "BDT": 2, "BGN": 2, "BMD": 2,
	"BND": 2, "BOB": 2, "BOV": 2, "BRL": 2, "BSD": 2,
	"BTN": 2, "BWP": 2, "BYN": 2, "BZD": 2, "CAD": 2,
	"CDF": 2, "CHE": 2, "CHF": 2, "CHW": 2, "CNY": 2,
	"COP": 2, "COU": 2, "CRC": 2, "CUC": 2, "CUP": 2,
	"CVE": 2, "CZK": 2, "DKK": 2, "DOP": 2, "DZD": 2,
	"EGP": 2, "ERN": 2, "ETB": 2, "EUR": 2, "FJD": 2,
	"FKP": 2, "GBP": 2, "GEL": 2, "GHS": 2, "GIP": 2,
	"GMD": 2, "GTQ": 2, "GYD": 2, "HKD": 2, "HNL": 2,
	"HTG": 2, "HUF": 2, "IDR": 2, "ILS": 2, "INR": 2,
	"IRR": 2, "JMD": 2, "KES": 2, "KGS": 2, "KHR": 2,
	"KPW": 2, "KYD": 2, "KZT": 2, "LAK": 2, "LBP": 2,
	"LKR": 2, "LRD": 2, "LSL": 2, "MAD": 2, "MDL": 2,
	"MGA": 2, "MKD": 2, "MMK": 2, "MNT": 2, "MOP": 2,
	"MRU": 2, "MUR": 2, "MVR": 2, "MWK": 2, "MXN": 2,
	"MXV": 2, "MYR": 2, "MZN": 2, "NAD": 2, "NGN": 2,
	"NIO": 2, "NOK": 2, "NPR": 2, "NZD": 2, "PAB": 2,
	"PEN": 2, "PGK": 2, "PHP": 2, "PKR": 2, "PLN": 2,
	"QAR": 2, "RON": 2, "RSD": 2, "RUB": 2, "SAR": 2,
	"SBD": 2, "SCR": 2, "SDG": 2, "SEK": 2, "SGD": 2,
	"SHP": 2, "SLE": 2, "SOS": 2, "SRD": 2, "SSP": 2,
	"STN": 2, "SVC": 2, "SYP": 2, "SZL": 2, "THB": 2,
	"TJS": 2, "TMT": 2, "TOP": 2, "TRY": 2, "TTD": 2,
	"TWD": 2, "TZS": 2, "UAH": 2, "USD": 2, "USN": 2,
	"UYU": 2, "UZS": 2, "VED": 2, "VES": 2, "WST": 2,
	"XCD": 2, "YER": 2, "ZAR": 2, "ZMW": 2, "ZWG": 2,
	// 三位分位 | 第纳尔/第拉姆
	"BHD": 3, "IQD": 3, "JOD": 3, "KWD": 3, "LYD": 3,
	"OMR": 3, "TND": 3,
	// 四位分位 | CLF/UYW
	"CLF": 4, "UYW": 4,
}

// cryptoMinorUnits 登记非 ISO 4217 的币种 | 链上 USDT 的分位是 10^-6。
var cryptoMinorUnits = map[string]int32{
	"USDT": 6,
}

// minorUnitExponent 取币种最小单位指数 | 表外的币种返回 false，由调用方显式失败，不猜它的小数位。
func minorUnitExponent(currency string) (int32, bool) {
	code := strings.ToUpper(strings.TrimSpace(currency))
	if exponent, ok := cryptoMinorUnits[code]; ok {
		return exponent, true
	}
	exponent, ok := iso4217MinorUnits[code]
	return exponent, ok
}
