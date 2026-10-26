package ofx

// currencyMinorUnits follows ISO 4217 minor units (SIX List One, 2026-09-17).
// https://www.six-group.com/dam/download/financial-information/data-center/iso-currrency/lists/list-one.xml
// BGN retains its documented two units after withdrawal (SIX notice effective 2026-01-01).
// Unknown codes and codes with no defined minor unit must not be guessed as cents.
func currencyMinorUnits(currency string) (int, bool) {
	switch currency {
	case "BIF", "CLP", "DJF", "GNF", "ISK", "JPY", "KMF", "KRW",
		"PYG", "RWF", "UGX", "UYI", "VND", "VUV", "XAF", "XOF",
		"XPF":
		return 0, true
	case "AED", "AFN", "ALL", "AMD", "AOA", "ARS", "AUD", "AWG",
		"AZN", "BAM", "BBD", "BDT", "BGN", "BMD", "BND", "BOB",
		"BOV", "BRL", "BSD", "BTN", "BWP", "BYN", "BZD", "CAD",
		"CDF", "CHE", "CHF", "CHW", "CNY", "COP", "COU", "CRC",
		"CUP", "CVE", "CZK", "DKK", "DOP", "DZD", "EGP", "ERN",
		"ETB", "EUR", "FJD", "FKP", "GBP", "GEL", "GHS", "GIP",
		"GMD", "GTQ", "GYD", "HKD", "HNL", "HTG", "HUF", "IDR",
		"ILS", "INR", "IRR", "JMD", "KES", "KGS", "KHR", "KPW",
		"KYD", "KZT", "LAK", "LBP", "LKR", "LRD", "LSL", "MAD",
		"MDL", "MGA", "MKD", "MMK", "MNT", "MOP", "MRU", "MUR",
		"MVR", "MWK", "MXN", "MXV", "MYR", "MZN", "NAD", "NGN",
		"NIO", "NOK", "NPR", "NZD", "PAB", "PEN", "PGK", "PHP",
		"PKR", "PLN", "QAR", "RON", "RSD", "RUB", "SAR", "SBD",
		"SCR", "SDG", "SEK", "SGD", "SHP", "SLE", "SOS", "SRD",
		"SSP", "STN", "SVC", "SYP", "SZL", "THB", "TJS", "TMT",
		"TOP", "TRY", "TTD", "TWD", "TZS", "UAH", "USD", "USN",
		"UYU", "UZS", "VED", "VES", "WST", "XAD", "XCD", "XCG",
		"YER", "ZAR", "ZMW", "ZWG":
		return 2, true
	case "BHD", "IQD", "JOD", "KWD", "LYD", "OMR", "TND":
		return 3, true
	case "CLF", "UYW":
		return 4, true
	default:
		return 0, false
	}
}
