package smsbower

// SMSBower uses the numeric country catalogue shared by SMS-Activate-compatible
// APIs. Keep these ISO-like search codes separate from the numeric IDs required
// by price and purchase requests. XK is the commonly used user-assigned code
// for Kosovo.
var countryISOByID = []string{
	"RU", "UA", "KZ", "CN", "PH", "MM", "ID", "MY", "KE", "TZ",
	"VN", "KG", "US", "IL", "HK", "PL", "GB", "MG", "CD", "NG",
	"MO", "EG", "IN", "IE", "KH", "LA", "HT", "CI", "GM", "RS",
	"YE", "ZA", "RO", "CO", "EE", "AZ", "CA", "MA", "GH", "AR",
	"UZ", "CM", "TD", "DE", "LT", "HR", "SE", "IQ", "NL", "LV",
	"AT", "BY", "TH", "SA", "MX", "TW", "ES", "IR", "DZ", "SI",
	"BD", "SN", "TR", "CZ", "LK", "PE", "PK", "NZ", "GN", "ML",
	"VE", "ET", "MN", "BR", "AF", "UG", "AO", "CY", "FR", "PG",
	"MZ", "NP", "BE", "BG", "HU", "MD", "IT", "PY", "HN", "TN",
	"NI", "TL", "BO", "CR", "GT", "AE", "ZW", "PR", "SD", "TG",
	"KW", "SV", "LY", "JM", "TT", "EC", "SZ", "OM", "BA", "DO",
	"SY", "QA", "PA", "CU", "MR", "SL", "JO", "PT", "BB", "BI",
	"BJ", "BN", "BS", "BW", "BZ", "CF", "DM", "GD", "GE", "GR",
	"GW", "GY", "IS", "KM", "KN", "LR", "LS", "MW", "NA", "NE",
	"RW", "SK", "SR", "TJ", "MC", "BH", "RE", "ZM", "AM", "SO",
	"CG", "CL", "BF", "LB", "GA", "AL", "UY", "MU", "BT", "MV",
	"GP", "TM", "GF", "FI", "LC", "LU", "VC", "GQ", "DJ", "AG",
	"KY", "ME", "DK", "CH", "NO", "AU", "ER", "SS", "ST", "AW",
	"MS", "AI", "JP", "MK", "SC", "NC", "CV", "US", "PS", "FJ",
	"KR", "KP", "EH", "SB", "JE", "BM", "SG", "TO", "WS", "MT",
	"LI", "GI", "FO", "XK", "NU",
}

func countryISOCode(id int) string {
	if id < 0 || id >= len(countryISOByID) {
		return ""
	}
	return countryISOByID[id]
}
