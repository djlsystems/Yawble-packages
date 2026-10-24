package main

import (
	"strings"
	"unicode/utf16"
)

const (
	gsmBasic     = "@£$¥èéùìòÇ\nØø\rÅåΔ_ΦΓΛΩΠΨΣΘΞÆæßÉ !\"#¤%&'()*+,-./0123456789:;<=>?¡ABCDEFGHIJKLMNOPQRSTUVWXYZÄÖÑÜ§¿abcdefghijklmnopqrstuvwxyzäöñüà"
	gsmExtension = "^{}\\[~]|€\f"
)

// segments counts the SMS segments of a body and names its encoding. A single segment holds 160
// GSM-7 or 70 UCS-2 characters; a longer message 153 or 67 a segment, 152 or 66 from a US/CA
// toll-free number. One character outside GSM-7 makes the whole message UCS-2.
func segments(body string, tollFree bool) (int, string) {
	if body == "" {
		return 0, "GSM-7"
	}
	gsm := 0
	for _, r := range body {
		switch {
		case strings.ContainsRune(gsmBasic, r):
			gsm++
		case strings.ContainsRune(gsmExtension, r):
			gsm += 2
		default:
			gsm = -1
		}
		if gsm < 0 {
			break
		}
	}
	if gsm >= 0 {
		return count(gsm, 160, 153, tollFree), "GSM-7"
	}
	units := len(utf16.Encode([]rune(body)))
	return count(units, 70, 67, tollFree), "UCS-2"
}

func count(units, single, multi int, tollFree bool) int {
	if units <= single {
		return 1
	}
	if tollFree {
		multi--
	}
	return (units + multi - 1) / multi
}
