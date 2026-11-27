// Package billing is Sprint 26 F34: plan cards, billing settings, invoices
// and their delivery (Decision Record amendment 2026-11-23, billing,
// D-62 … D-70). Money is always whole minor units in int64 (D-64); nothing in
// this package uses floating point. Billing prices and notifies; it never
// refuses, slows or limits anything (D-62).
package billing

import "regexp"

var (
	countryRe  = regexp.MustCompile(`^[A-Z]{2}$`)
	currencyRe = regexp.MustCompile(`^[A-Z]{3}$`)
)

// iso4217 is the set of active ISO 4217 currency codes a provider may bill in
// (funds and precious-metal codes left out).
var iso4217 = map[string]bool{}

func init() {
	for _, c := range []string{
		"AED", "AFN", "ALL", "AMD", "ANG", "AOA", "ARS", "AUD", "AWG", "AZN", "BAM", "BBD", "BDT", "BGN", "BHD",
		"BIF", "BMD", "BND", "BOB", "BRL", "BSD", "BTN", "BWP", "BYN", "BZD", "CAD", "CDF", "CHF", "CLP", "CNY",
		"COP", "CRC", "CUP", "CVE", "CZK", "DJF", "DKK", "DOP", "DZD", "EGP", "ERN", "ETB", "EUR", "FJD", "FKP",
		"GBP", "GEL", "GHS", "GIP", "GMD", "GNF", "GTQ", "GYD", "HKD", "HNL", "HTG", "HUF", "IDR", "ILS", "INR",
		"IQD", "IRR", "ISK", "JMD", "JOD", "JPY", "KES", "KGS", "KHR", "KMF", "KPW", "KRW", "KWD", "KYD", "KZT",
		"LAK", "LBP", "LKR", "LRD", "LSL", "LYD", "MAD", "MDL", "MGA", "MKD", "MMK", "MNT", "MOP", "MRU", "MUR",
		"MVR", "MWK", "MXN", "MYR", "MZN", "NAD", "NGN", "NIO", "NOK", "NPR", "NZD", "OMR", "PAB", "PEN", "PGK",
		"PHP", "PKR", "PLN", "PYG", "QAR", "RON", "RSD", "RUB", "RWF", "SAR", "SBD", "SCR", "SDG", "SEK", "SGD",
		"SHP", "SLE", "SOS", "SRD", "SSP", "STN", "SVC", "SYP", "SZL", "THB", "TJS", "TMT", "TND", "TOP", "TRY",
		"TTD", "TWD", "TZS", "UAH", "UGX", "USD", "UYU", "UZS", "VED", "VES", "VND", "VUV", "WST", "XAF", "XCD",
		"XCG", "XOF", "XPF", "YER", "ZAR", "ZMW", "ZWG",
	} {
		iso4217[c] = true
	}
}

// ValidCurrency reports whether code is an active ISO 4217 code.
func ValidCurrency(code string) bool { return iso4217[code] }

// ValidCountry reports whether code has the ISO 3166-1 alpha-2 shape. A
// country outside Countries is allowed; it needs an explicit currency.
func ValidCountry(code string) bool { return countryRe.MatchString(code) }

// Country is a billing country and its default currency (D-64).
type Country struct {
	Code     string `json:"code"`
	Name     string `json:"name"`
	Currency string `json:"currency"`
}

// Countries is the built-in country → currency table the settings API serves
// (the console's country picker). Choosing one fills in its currency.
var Countries = []Country{
	{"US", "United States", "USD"},
	{"IN", "India", "INR"},
	{"GB", "United Kingdom", "GBP"},
	{"DE", "Germany", "EUR"},
	{"FR", "France", "EUR"},
	{"NL", "Netherlands", "EUR"},
	{"IE", "Ireland", "EUR"},
	{"ES", "Spain", "EUR"},
	{"IT", "Italy", "EUR"},
	{"BE", "Belgium", "EUR"},
	{"AT", "Austria", "EUR"},
	{"PT", "Portugal", "EUR"},
	{"FI", "Finland", "EUR"},
	{"CH", "Switzerland", "CHF"},
	{"SE", "Sweden", "SEK"},
	{"NO", "Norway", "NOK"},
	{"DK", "Denmark", "DKK"},
	{"PL", "Poland", "PLN"},
	{"CZ", "Czechia", "CZK"},
	{"CA", "Canada", "CAD"},
	{"MX", "Mexico", "MXN"},
	{"BR", "Brazil", "BRL"},
	{"AR", "Argentina", "ARS"},
	{"CL", "Chile", "CLP"},
	{"CO", "Colombia", "COP"},
	{"AU", "Australia", "AUD"},
	{"NZ", "New Zealand", "NZD"},
	{"JP", "Japan", "JPY"},
	{"KR", "South Korea", "KRW"},
	{"CN", "China", "CNY"},
	{"HK", "Hong Kong", "HKD"},
	{"TW", "Taiwan", "TWD"},
	{"SG", "Singapore", "SGD"},
	{"MY", "Malaysia", "MYR"},
	{"ID", "Indonesia", "IDR"},
	{"PH", "Philippines", "PHP"},
	{"TH", "Thailand", "THB"},
	{"VN", "Vietnam", "VND"},
	{"LK", "Sri Lanka", "LKR"},
	{"BD", "Bangladesh", "BDT"},
	{"PK", "Pakistan", "PKR"},
	{"NP", "Nepal", "NPR"},
	{"AE", "United Arab Emirates", "AED"},
	{"SA", "Saudi Arabia", "SAR"},
	{"QA", "Qatar", "QAR"},
	{"KW", "Kuwait", "KWD"},
	{"BH", "Bahrain", "BHD"},
	{"OM", "Oman", "OMR"},
	{"IL", "Israel", "ILS"},
	{"TR", "Türkiye", "TRY"},
	{"EG", "Egypt", "EGP"},
	{"ZA", "South Africa", "ZAR"},
	{"NG", "Nigeria", "NGN"},
	{"KE", "Kenya", "KES"},
}

// DefaultCurrency returns the currency of a country in Countries.
func DefaultCurrency(country string) (string, bool) {
	for _, c := range Countries {
		if c.Code == country {
			return c.Currency, true
		}
	}
	return "", false
}
