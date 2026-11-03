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

// answerARM makes the fake answer the requests match picks with answer, and every other ARM
// request as it would.
func answerARM(f *fake, match func(method, path string) bool, answer func(w http.ResponseWriter, method string)) {
	f.hosts[armHost] = func(w http.ResponseWriter, r *http.Request) {
		_, path, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
		path = "/" + path
		if match(r.Method, path) {
			answer(w, r.Method)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.arm(w, r.Method, path, f.calls[len(f.calls)-1].Body)
	}
}

// assertShortHidden fails when the short value appears anywhere in the plugin's output.
func assertShortHidden(t *testing.T, r *result, short string) {
	t.Helper()
	if strings.Contains(r.raw, short) {
		t.Fatalf("the short value %q was written out:\n%s", short, r.raw)
	}
	mustContain(t, r.text(), "[redacted]")
}

// D3: a short value is hidden in every error text Azure supplies, not only a non-2xx answer: an
// async operation that ends Failed, a deployment's own error and a failed deployment operation.
func TestAShortValueIsHiddenInAnAsyncOperationFailure(t *testing.T) {
	w := newWorld(t)
	bicepApp(w)
	opURL := "https://" + armHost + "/subscriptions/" + testSub + "/operations/secret-set-op"
	answerARM(w.f, func(method, path string) bool {
		return method == http.MethodPatch && strings.HasSuffix(path, "/containerApps/my-app") || strings.HasSuffix(path, "/operations/secret-set-op")
	}, func(rw http.ResponseWriter, method string) {
		if method == http.MethodPatch {
			rw.Header().Set("Azure-AsyncOperation", opURL)
			w.f.answer(rw, 202, nil)
			return
		}
		w.f.answer(rw, 200, map[string]any{"status": "Failed", "error": map[string]any{"code": "Bad", "message": "secret value Zq9 refused"}})
	})
	bound := []hostObject{mailbox("mail", "bot@example.test", "Zq9"), apiKey("stripe live", "Stripe", "stripe", map[string]string{"key": keyValue})}
	r := runPlugin(t, runOpts{config: apply(nil), bound: bound}, setLine)
	if r.ok() {
		t.Fatal("a failed operation was reported as done")
	}
	mustContain(t, r.text(), "ended failed: Bad secret value [redacted] refused")
	assertShortHidden(t, r, "Zq9")
}

func TestAShortParamValueIsHiddenInADeploymentsOwnError(t *testing.T) {
	f := newFake(t)
	docFile(t, "app.json", secureTemplate)
	params := docFile(t, "app.parameters.json", `{"parameters": {"name": {"value": "app-id"}}}`)
	f.whatIf = []map[string]any{wiChange("Create", inRG(ownedRG, "Microsoft.ManagedIdentity/userAssignedIdentities/app-id"), nil)}
	answerARM(f, func(method, path string) bool {
		return method == http.MethodGet && strings.Contains(path, "/deployments/") && !strings.Contains(path, "/operations")
	}, func(rw http.ResponseWriter, _ string) {
		f.answer(rw, 200, map[string]any{"properties": map[string]any{"provisioningState": "Failed",
			"error": map[string]any{"code": "InvalidParameter", "message": "password Zq9 rejected"}}})
	})
	r := runPlugin(t, runOpts{config: apply(nil), bound: []hostObject{mailbox("mail", "bot@example.test", "Zq9")}},
		"deploy "+ownedRG+" app.json "+params+" param=smtpPassword=mail.password")
	if r.ok() {
		t.Fatal("a failed deployment was reported as done")
	}
	mustContain(t, r.text(), "ended Failed: InvalidParameter password [redacted] rejected")
	assertShortHidden(t, r, "Zq9")
}

func TestAShortParamValueIsHiddenInAFailedDeploymentOperation(t *testing.T) {
	f := newFake(t)
	docFile(t, "app.json", secureTemplate)
	params := docFile(t, "app.parameters.json", `{"parameters": {"name": {"value": "app-id"}}}`)
	f.whatIf = []map[string]any{wiChange("Create", inRG(ownedRG, "Microsoft.ManagedIdentity/userAssignedIdentities/app-id"), nil)}
	f.deployOps = []map[string]any{failedOp("Microsoft.App/containerApps", "app", "Bad", "password Zq9 rejected")}
	r := runPlugin(t, runOpts{config: apply(nil), bound: []hostObject{mailbox("mail", "bot@example.test", "Zq9")}},
		"deploy "+ownedRG+" app.json "+params+" param=smtpPassword=mail.password")
	if r.ok() {
		t.Fatal("a failed deployment was reported as done")
	}
	mustContain(t, r.text(), "Microsoft.App/containerApps app: Bad: password [redacted] rejected")
	assertShortHidden(t, r, "Zq9")
}

// The plugin's own text keeps an ordinary word that is also a short hidden value.
func TestAShortValueDoesNotBlankThePluginsOwnText(t *testing.T) {
	w := newWorld(t)
	bicepApp(w)
	bound := []hostObject{mailbox("mail", "bot@example.test", "app"), apiKey("stripe live", "Stripe", "stripe", map[string]string{"key": keyValue})}
	r := runPlugin(t, runOpts{config: apply(nil), bound: bound}, setLine)
	if !r.ok() {
		t.Fatal(r.text())
	}
	mustContain(t, r.text(), "Container app my-app now reads")
}

// D4: a value whose lower case has another byte length (the Kelvin sign lowers to an ASCII k)
// is hidden in any case and never puts a slice out of range.
func TestScrubHidesAValueWhoseLowerCaseChangesLength(t *testing.T) {
	p := &plugin{}
	p.hide("Kelvin-Pw99")
	for _, text := range []string{"x kelvin-pw99", "kelvin-pw99", "Kelvin-Pw99 x", "KELVIN-PW99", "a Kelvin-Pw99 b kelvin-pw99", "KK kelvin-pw9"} {
		got := p.scrub(text)
		if strings.Contains(strings.ToLower(got), "elvin-pw99") {
			t.Errorf("scrub(%q) = %q: the value is shown", text, got)
		}
	}
	if got := p.scrub("KK kelvin-pw9"); got != "KK kelvin-pw9" {
		t.Errorf("text without the value changed: %q", got)
	}
	if got := p.scrub("a Kelvin-Pw99 b"); got != "a [redacted] b" {
		t.Errorf("got %q", got)
	}
}

// N1: a hidden value equal to a record's key or its type ("result", "error") leaves the record
// well formed, and is still hidden where it stands as data.
func TestAValueEqualToARecordKeyOrTypeLeavesTheRecordWellFormed(t *testing.T) {
	for _, v := range []string{"result", "error"} {
		t.Run(v, func(t *testing.T) {
			w := newWorld(t)
			bicepApp(w)
			refuseARM(w.f, func(method, path string) bool {
				return method == http.MethodPatch && strings.HasSuffix(path, "/containerApps/my-app")
			}, 400, "Bad", "secret value "+v+" refused")
			bound := []hostObject{mailbox("mail", "bot@example.test", v), apiKey("stripe live", "Stripe", "stripe", map[string]string{"key": keyValue})}
			r := runPlugin(t, runOpts{config: apply(nil), bound: bound}, setLine)
			lines := strings.Split(strings.TrimSpace(r.raw), "\n")
			var last map[string]any
			if err := json.Unmarshal([]byte(lines[len(lines)-1]), &last); err != nil {
				t.Fatalf("the last record is not JSON: %s", lines[len(lines)-1])
			}
			if last["t"] != "result" || last["ok"] != false {
				t.Fatalf("the result record is malformed: %s", lines[len(lines)-1])
			}
			e, _ := last["error"].(string)
			if !strings.Contains(e, "secret value [redacted] refused") || strings.Contains(e, " "+v+" ") {
				t.Fatalf("the value is not hidden as data: %q", e)
			}
		})
	}
	p := &plugin{}
	p.hide("result")
	p.hide("progress")
	got := string(p.scrubJSON([]byte(`{"t":"result","type":"progress","payload":{"type":"result","x":"a result"},"l":["result"]}`)))
	want := `{"t":"result","type":"progress","payload":{"type":"[redacted]","x":"a [redacted]"},"l":["[redacted]"]}`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}
