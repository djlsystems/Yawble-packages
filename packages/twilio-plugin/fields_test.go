package main

import (
	"encoding/json"
	"os"
	"testing"
	"unicode/utf8"
)

// Every setting and secret has a short label for the settings screen; a secret that names an
// account or a key rather than granting access is shown (masked false), and a secret the plugin
// cannot work without is required.
func TestEveryFieldHasItsLabelAndEverySecretItsMasking(t *testing.T) {
	raw, err := os.ReadFile("plugin.json")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Config  map[string]struct{ Label *string } `json:"config"`
		Secrets map[string]struct {
			Label    *string
			Masked   *bool
			Required bool
		} `json:"secrets"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	settings := map[string]string{
		"mode":                     "Mode",
		"sendAllowlist":            "Send allowlist",
		"fromNumber":               "From number",
		"messagePrefix":            "Message prefix",
		"messagingServiceSid":      "Messaging Service SID",
		"allowConsentedRecipients": "Text consented recipients",
		"maxConsentedRecipients":   "Most consented recipients",
		"dailySendCap":             "Daily send cap",
		"perRecipientDailyCap":     "Daily cap per recipient",
		"mediaUrlHosts":            "Media URL hosts",
		"allowedCountries":         "Allowed countries",
		"optOutFooter":             "Opt-out footer",
		"privacyUrl":               "Privacy policy URL",
		"termsUrl":                 "Terms URL",
		"autoReply":                "Auto-reply",
		"autoReplyText":            "Auto-reply text",
		"autoReplyOncePerHours":    "Auto-reply once per hours",
		"ownerCommands":            "Owner commands",
		"ownerNumbers":             "Owner numbers",
		"ownerKeywords":            "Owner keywords",
	}
	type secret struct {
		label            string
		masked, required bool
	}
	secrets := map[string]secret{
		"accountSid":   {"Account SID", false, false},
		"apiKeySid":    {"API key SID", false, false},
		"apiKeySecret": {"API key secret", true, false},
	}
	if len(m.Config) != len(settings) || len(m.Secrets) != len(secrets) {
		t.Fatalf("%d settings and %d secrets; the labels cover %d and %d", len(m.Config), len(m.Secrets), len(settings), len(secrets))
	}
	short := func(field, label string) {
		if n := utf8.RuneCountInString(label); n < 1 || n > 80 {
			t.Errorf("%s's label is %d characters; the Host takes 1 to 80", field, n)
		}
	}
	for name, f := range m.Config {
		want, ok := settings[name]
		if !ok || f.Label == nil || *f.Label != want {
			t.Errorf("setting %s: label %v, want %q", name, f.Label, want)
			continue
		}
		short(name, *f.Label)
	}
	for name, f := range m.Secrets {
		want, ok := secrets[name]
		if !ok || f.Label == nil || *f.Label != want.label {
			t.Errorf("secret %s: label %v, want %q", name, f.Label, want.label)
			continue
		}
		short(name, *f.Label)
		masked := f.Masked == nil || *f.Masked
		if f.Masked != nil && *f.Masked {
			t.Errorf("secret %s says masked true; masked is the default, so only masked false is written", name)
		}
		if masked != want.masked || f.Required != want.required {
			t.Errorf("secret %s: masked %v required %v, want masked %v required %v", name, masked, f.Required, want.masked, want.required)
		}
	}
}
