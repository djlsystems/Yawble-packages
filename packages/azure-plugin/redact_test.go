package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// Values with characters JSON escapes (& < > " \), quoted back in an Azure error, are hidden in every
// form: as typed and as JSON writes them. One for each source of a hidden value: a connection's
// field, a value secret kept from the app's listSecrets, and a deploy param= value.

// escapedForms is a value as typed and each form JSON may write it in.
func escapedForms(t *testing.T, v string) []string {
	t.Helper()
	plain, _ := json.Marshal(v) // & < > as \u0026 \u003c \u003e, " and \ escaped
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return []string{v, strings.Trim(string(plain), `"`), strings.Trim(strings.TrimSpace(b.String()), `"`)}
}

func assertNoForm(t *testing.T, out, label string, values ...string) {
	t.Helper()
	for _, v := range values {
		for _, form := range escapedForms(t, v) {
			if strings.Contains(out, form) {
				t.Fatalf("%s: the value was written out as %q:\n%s", label, form, out)
			}
		}
	}
}

func TestValuesJSONEscapesAreHiddenFromConnectionAndListSecretsSources(t *testing.T) {
	pw := `Pw&<rd>"q\z-` + strings.Repeat("M3", 6)
	kept := `kv&k<e>p"t\-` + strings.Repeat("N4", 6)
	w := newWorld(t)
	app := bicepApp(w)
	app.Props["configuration"].(map[string]any)["secrets"].([]any)[0].(map[string]any)["value"] = kept
	refuseARM(w.f, func(method, path string) bool {
		return method == http.MethodPatch && strings.HasSuffix(path, "/containerApps/my-app")
	}, 400, "ContainerAppSecretInvalid", "secret smtp-password value "+pw+" and db-conn value "+kept+" are invalid")
	bound := []hostObject{mailbox("mail", "bot@example.test", pw), apiKey("stripe live", "Stripe", "stripe", map[string]string{"key": keyValue})}
	r := runPlugin(t, runOpts{config: apply(nil), bound: bound}, setLine)
	if r.ok() {
		t.Fatal("a refused PATCH was reported as done")
	}
	mustContain(t, r.text(), "ContainerAppSecretInvalid", "secret smtp-password value [redacted] and db-conn value [redacted] are invalid")
	assertNoForm(t, r.raw, "secret set", pw, kept)
	for _, tail := range []string{strings.Repeat("M3", 6), strings.Repeat("N4", 6)} {
		if strings.Contains(r.raw, tail) {
			t.Fatalf("part of a value was written out:\n%s", r.raw)
		}
	}
}

func TestValuesJSONEscapesAreHiddenFromADeployParam(t *testing.T) {
	pw := `Dp&<x>"\-` + strings.Repeat("K9", 6)
	f := newFake(t)
	docFile(t, "app.json", secureTemplate)
	params := docFile(t, "app.parameters.json", `{"parameters": {"name": {"value": "app-id"}}}`)
	refuseARM(f, func(method, path string) bool { return method == http.MethodPost && strings.HasSuffix(path, "/whatIf") },
		400, "InvalidTemplate", "parameter smtpPassword value '"+pw+"' is invalid")
	r := runPlugin(t, runOpts{config: apply(nil), bound: []hostObject{mailbox("mail", "bot@example.test", pw)}},
		"deploy "+ownedRG+" app.json "+params+" param=smtpPassword=mail.password")
	if r.ok() {
		t.Fatal("a refused what-if was reported as done")
	}
	mustContain(t, r.text(), "InvalidTemplate", "value '[redacted]' is invalid")
	assertNoForm(t, r.raw, "deploy param=", pw)
	if strings.Contains(r.raw, strings.Repeat("K9", 6)) {
		t.Fatalf("part of a value was written out:\n%s", r.raw)
	}
}

// A value under four characters is hidden as a word of its own in what Azure answers, but not as
// part of a longer word, nor in the plugin's own text.
func TestAShortValueIsHiddenWhereAzureQuotesIt(t *testing.T) {
	w := newWorld(t)
	app := bicepApp(w)
	app.Props["configuration"].(map[string]any)["secrets"].([]any)[0].(map[string]any)["value"] = "Zq9"
	refuseARM(w.f, func(method, path string) bool {
		return method == http.MethodPatch && strings.HasSuffix(path, "/containerApps/my-app")
	}, 400, "ContainerAppSecretInvalid", "db-conn value Zq9 is too short; Zq9x is not it")
	r := runPlugin(t, runOpts{config: apply(nil), bound: setBound()}, setLine)
	if r.ok() {
		t.Fatal("a refused PATCH was reported as done")
	}
	mustContain(t, r.text(), "db-conn value [redacted] is too short; Zq9x is not it")
	if strings.Contains(strings.ReplaceAll(r.raw, "Zq9x", ""), "Zq9") {
		t.Fatalf("a short value was written out:\n%s", r.raw)
	}
}

func TestScrubJSONKeepsOrderNumbersAndUntouchedStrings(t *testing.T) {
	p := &plugin{}
	p.hide(`a&b<c>"d\e`)
	got := string(p.scrubJSON([]byte(`{"z":1.50,"a":"x a\u0026b\u003cc\u003e\"d\\e y","k":"plain \u00e9","n":[true,null]}`)))
	want := `{"z":1.50,"a":"x [redacted] y","k":"plain \u00e9","n":[true,null]}`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}
