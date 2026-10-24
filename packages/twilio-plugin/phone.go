package main

import (
	"strings"
)

// normalise turns a phone number into E.164: "+" and 8-15 digits. Ten digits (a US number as people
// write it, with or without spaces, dashes, dots or brackets) become +1 and those digits. Anything
// else is not a phone number.
func normalise(raw string) (string, bool) {
	s := strings.TrimSpace(raw)
	plus := strings.HasPrefix(s, "+")
	var digits strings.Builder
	for i, r := range s {
		switch {
		case r >= '0' && r <= '9':
			digits.WriteRune(r)
		case r == '+' && i == 0:
		case r == ' ' || r == '-' || r == '.' || r == '(' || r == ')':
		default:
			return "", false
		}
	}
	d := digits.String()
	if !plus {
		if len(d) == 10 && d[0] >= '2' {
			return "+1" + d, true
		}
		return "", false
	}
	if len(d) < 8 || len(d) > 15 || d[0] == '0' {
		return "", false
	}
	if d[0] == '1' && len(d) != 11 {
		return "", false
	}
	return "+" + d, true
}

var canadianAreaCodes = set("204", "226", "236", "249", "250", "257", "263", "289", "306", "343", "354",
	"365", "367", "368", "382", "387", "403", "416", "418", "428", "431", "437", "438", "450", "460", "468",
	"474", "506", "514", "519", "548", "579", "581", "584", "587", "600", "604", "613", "639", "647", "672",
	"683", "705", "709", "742", "753", "778", "780", "782", "807", "819", "825", "867", "873", "879", "902",
	"905", "942")

// Other countries of the North American Numbering Plan; US territories (PR, USVI, Guam, the
// Northern Marianas, American Samoa) are left out and read as US.
var otherNANP = map[string]string{
	"242": "BS", "246": "BB", "264": "AI", "268": "AG", "284": "VG", "345": "KY", "441": "BM",
	"473": "GD", "649": "TC", "658": "JM", "876": "JM", "664": "MS", "721": "SX", "758": "LC",
	"767": "DM", "784": "VC", "809": "DO", "829": "DO", "849": "DO", "868": "TT", "869": "KN",
}

var callingCodes = []struct{ prefix, country string }{
	{"44", "GB"}, {"33", "FR"}, {"49", "DE"}, {"61", "AU"}, {"52", "MX"}, {"91", "IN"}, {"971", "AE"},
	{"353", "IE"}, {"34", "ES"}, {"39", "IT"}, {"31", "NL"}, {"64", "NZ"}, {"81", "JP"}, {"86", "CN"},
	{"55", "BR"}, {"27", "ZA"}, {"7", "RU"}, {"46", "SE"}, {"47", "NO"}, {"45", "DK"}, {"41", "CH"},
}

// country names an E.164 number's country by ISO code: US or CA for +1 by area code, a known
// country, or "the country with calling code +<code>".
func country(e164 string) string {
	d := strings.TrimPrefix(e164, "+")
	if strings.HasPrefix(d, "1") && len(d) == 11 {
		area := d[1:4]
		if canadianAreaCodes[area] {
			return "CA"
		}
		if c, ok := otherNANP[area]; ok {
			return c
		}
		return "US"
	}
	for _, c := range callingCodes {
		if strings.HasPrefix(d, c.prefix) {
			return c.country
		}
	}
	code := d
	if len(code) > 3 {
		code = code[:3]
	}
	return "the country with calling code +" + code
}

var tollFreeAreaCodes = set("800", "833", "844", "855", "866", "877", "888")

// isTollFree says whether a +1 number is a US/CA toll-free number (8XX).
func isTollFree(e164 string) bool {
	return strings.HasPrefix(e164, "+1") && len(e164) == 12 && tollFreeAreaCodes[e164[2:5]]
}

func set(items ...string) map[string]bool {
	m := make(map[string]bool, len(items))
	for _, i := range items {
		m[i] = true
	}
	return m
}
