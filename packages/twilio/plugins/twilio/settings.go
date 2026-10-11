package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// FIXED, not settings: how long Twilio may hold a queued text before dropping it rather than
// sending it late (its ValidityPeriod), the most texts list and recipients show, and how long sent
// texts are re-checked for delivery and how far back the first poll reads replies.
const (
	validitySeconds = 14400
	maxResults      = 20
	pollLookback    = 72 * time.Hour
	maxPrefixChars  = 40
)

// settings are the manifest's config, with the manifest's defaults applied again here so a request
// that leaves a field out still gets the safe value. A field the manifest no longer has (quiet hours,
// and the three now fixed above) is ignored when a request still carries it.
type settings struct {
	Mode                     string
	FromNumber               string
	MessagingServiceSid      string
	SendAllowlist            []string
	AllowConsentedRecipients bool
	MaxConsentedRecipients   int
	DailySendCap             int
	PerRecipientDailyCap     int
	MessagePrefix            string
	OptOutFooter             string
	PrivacyURL               string
	TermsURL                 string
	MediaURLHosts            []string
	AllowedCountries         []string
}

type secrets struct {
	AccountSid, APIKeySid, APIKeySecret string
}

var (
	mgPattern = regexp.MustCompile(`^MG[0-9a-fA-F]{32}$`)
)

func parseSettings(raw map[string]json.RawMessage) (settings, string) {
	s := settings{
		Mode:                   "dryRun",
		MaxConsentedRecipients: 25,
		DailySendCap:           500,
		PerRecipientDailyCap:   50,
		OptOutFooter:           "every",
		AllowedCountries:       []string{"US"},
	}

	var problems []string
	str := func(name string, into *string) {
		if v, ok := raw[name]; ok && string(v) != "null" {
			if json.Unmarshal(v, into) != nil {
				problems = append(problems, fmt.Sprintf("`%s` must be text", name))
			}
			*into = strings.TrimSpace(*into)
		}
	}
	list := func(name string, into *[]string) {
		if v, ok := raw[name]; ok && string(v) != "null" {
			var items []string
			if json.Unmarshal(v, &items) != nil {
				problems = append(problems, fmt.Sprintf("`%s` must be a list", name))
				return
			}
			out := []string{}
			for _, item := range items {
				if t := strings.TrimSpace(item); t != "" {
					out = append(out, t)
				}
			}
			*into = out
		}
	}
	boolean := func(name string, into *bool) {
		if v, ok := raw[name]; ok && string(v) != "null" {
			if json.Unmarshal(v, into) != nil {
				problems = append(problems, fmt.Sprintf("`%s` must be on or off", name))
			}
		}
	}
	integer := func(name string, into *int, min, max int) {
		if v, ok := raw[name]; ok && string(v) != "null" {
			var f float64
			if json.Unmarshal(v, &f) != nil || f != float64(int(f)) || int(f) < min || int(f) > max {
				problems = append(problems, fmt.Sprintf("`%s` must be a whole number from %d to %d", name, min, max))
				return
			}
			*into = int(f)
		}
	}

	str("mode", &s.Mode)
	str("fromNumber", &s.FromNumber)
	str("messagingServiceSid", &s.MessagingServiceSid)
	list("sendAllowlist", &s.SendAllowlist)
	boolean("allowConsentedRecipients", &s.AllowConsentedRecipients)
	integer("maxConsentedRecipients", &s.MaxConsentedRecipients, 1, 500)
	integer("dailySendCap", &s.DailySendCap, 1, 2000)
	integer("perRecipientDailyCap", &s.PerRecipientDailyCap, 1, 200)
	// THE PREFIX KEEPS ITS SPACES: "Acme: " is how it is joined to the text, so it is read raw.
	if v, ok := raw["messagePrefix"]; ok && string(v) != "null" {
		if json.Unmarshal(v, &s.MessagePrefix) != nil {
			problems = append(problems, "`messagePrefix` must be text")
		}
	}
	str("optOutFooter", &s.OptOutFooter)
	str("privacyUrl", &s.PrivacyURL)
	str("termsUrl", &s.TermsURL)
	list("mediaUrlHosts", &s.MediaURLHosts)
	list("allowedCountries", &s.AllowedCountries)

	if strings.TrimSpace(s.MessagePrefix) == "" {
		s.MessagePrefix = ""
	} else {
		s.MessagePrefix = strings.TrimLeft(s.MessagePrefix, " ")
		if strings.ContainsAny(s.MessagePrefix, "\r\n") || utf8.RuneCountInString(s.MessagePrefix) > maxPrefixChars {
			problems = append(problems, fmt.Sprintf("`messagePrefix` must be one line of at most %d characters", maxPrefixChars))
		}
	}
	if s.OptOutFooter != "every" && s.OptOutFooter != "first" && s.OptOutFooter != "off" {
		problems = append(problems, "`optOutFooter` must be every, first or off")
	}
	for _, u := range []struct{ name, value string }{{"privacyUrl", s.PrivacyURL}, {"termsUrl", s.TermsURL}} {
		if parsed, err := url.Parse(u.value); u.value != "" && (err != nil || parsed.Scheme != "https" || parsed.Host == "") {
			problems = append(problems, fmt.Sprintf("`%s` must be an https address, such as https://example.com/%s", u.name, strings.TrimSuffix(u.name, "Url")))
		}
	}
	if s.Mode != "dryRun" && s.Mode != "send" {
		problems = append(problems, "`mode` must be dryRun or send")
	}
	if s.FromNumber != "" {
		if n, ok := normalise(s.FromNumber); ok {
			s.FromNumber = n
		} else {
			problems = append(problems, "`fromNumber` must be one number in +country format (E.164), such as +18445551234")
		}
	}
	if s.MessagingServiceSid != "" && !mgPattern.MatchString(s.MessagingServiceSid) {
		problems = append(problems, "`messagingServiceSid` must be MG followed by 32 hex characters")
	}
	// THE ALLOWLIST TAKES 10 US DIGITS TOO: the install asks for a cell and a person types it as
	// they know it. Each entry is normalised; one that is not a number is a settings problem.
	allow := []string{}
	for _, entry := range s.SendAllowlist {
		n, ok := normalise(entry)
		if !ok {
			problems = append(problems, fmt.Sprintf("`sendAllowlist` holds %q, which is not a phone number in +country format (E.164)", entry))
			continue
		}
		allow = append(allow, n)
	}
	s.SendAllowlist = allow
	if len(s.SendAllowlist) > 20 {
		problems = append(problems, "`sendAllowlist` holds more than 20 numbers")
	}
	if len(s.MediaURLHosts) > 20 {
		problems = append(problems, "`mediaUrlHosts` holds more than 20 hosts")
	}
	for i, h := range s.MediaURLHosts {
		s.MediaURLHosts[i] = strings.ToLower(strings.TrimSuffix(h, "."))
	}
	for i, c := range s.AllowedCountries {
		s.AllowedCountries[i] = strings.ToUpper(c)
		if s.AllowedCountries[i] != "US" && s.AllowedCountries[i] != "CA" {
			problems = append(problems, "`allowedCountries` takes only US and CA")
		}
	}

	if len(problems) > 0 {
		return s, "This member's settings cannot be used: " + strings.Join(problems, "; ") + ". A person corrects them in this member's settings; nothing was sent."
	}
	return s, ""
}

func (s settings) onAllowlist(number string) bool {
	for _, n := range s.SendAllowlist {
		if n == number {
			return true
		}
	}
	return false
}

func (s settings) countryAllowed(country string) bool {
	for _, c := range s.AllowedCountries {
		if c == country {
			return true
		}
	}
	return false
}

func (s settings) mediaHostAllowed(host string) bool {
	host = strings.ToLower(host)
	for _, h := range s.MediaURLHosts {
		if h == host {
			return true
		}
	}
	return false
}
