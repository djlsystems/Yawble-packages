package main

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
)

// From 3.1.0 a deployed app's secret comes from a connection bound to the appSecrets slot
// ("Connections a deployed app may use"), named secret=<ENV>=<connection>.<field>: a mailbox, a
// username and password, an API key or a website, found by the name the Host sends just before
// account - never by account. The old named secrets (the "App secrets (old)" block) are gone.

// otherValue, keyValue, siteValue and tokenValue are further secret values, put together at run
// time like secretValue.
var (
	otherValue = "other-" + strings.Repeat("K3", 12) + "-value"
	keyValue   = "key-" + strings.Repeat("W5", 12) + "-value"
	siteValue  = "site-" + strings.Repeat("Q7", 12) + "-value"
	tokenValue = "tok-" + strings.Repeat("V4", 12) + "-value"
)

const mailUser = "bot-mailer@example.com"

func TestTheManifestHasNoAppSecretsSectionOfAnyKind(t *testing.T) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(readManifestBytes(t), &m); err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"namedSecrets", "secrets"} {
		if _, ok := m[gone]; ok {
			t.Errorf("the manifest still has a %s block", gone)
		}
	}
	raw := string(readManifestBytes(t))
	for _, old := range []string{"App secrets (old)", "App secret 1", "appSecret1"} {
		if strings.Contains(raw, old) {
			t.Errorf("the manifest still says %q", old)
		}
	}
	keys := readManifest(t).Connections[keySlot]
	if !strings.Contains(raw, `"description": "Connections a deployed app may use: `) {
		t.Error("the appSecrets slot does not say what it is for")
	}
	if want := []string{"imap", "login", "apikey", "website"}; strings.Join(keys.Providers, ",") != strings.Join(want, ",") || !keys.Multiple || keys.Required {
		t.Fatalf("appSecrets = %+v", keys)
	}
}

// appPut reads the app's secrets and its container's env from the one PUT of new-app.
func appPut(t *testing.T, w *world) (secrets []map[string]any, env []map[string]any) {
	t.Helper()
	put := w.f.callsTo(http.MethodPut, "/containerApps/new-app")
	if len(put) != 1 {
		t.Fatalf("the app was put %d times", len(put))
	}
	var app struct {
		Properties struct {
			Configuration struct {
				Secrets []map[string]any `json:"secrets"`
			} `json:"configuration"`
			Template struct {
				Containers []struct {
					Env []map[string]any `json:"env"`
				} `json:"containers"`
			} `json:"template"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(put[0].Body, &app); err != nil {
		t.Fatal(err)
	}
	return app.Properties.Configuration.Secrets, app.Properties.Template.Containers[0].Env
}

func secretNamed(secrets []map[string]any, name string) map[string]any {
	for _, s := range secrets {
		if s["name"] == name {
			return s
		}
	}
	return nil
}

func mustSecrets(t *testing.T, w *world) []map[string]any {
	t.Helper()
	s, _ := appPut(t, w)
	return s
}

// mustInline checks the app holds secret name with value, inline, and env var env reading it.
func mustInline(t *testing.T, secrets, env []map[string]any, envName, name, value string) {
	t.Helper()
	s := secretNamed(secrets, name)
	if s == nil || s["value"] != value || s["keyVaultUrl"] != nil || s["identity"] != nil {
		t.Fatalf("the app's secret %s is not the connection's value (present %v)", name, s != nil)
	}
	for _, e := range env {
		if e["name"] == envName {
			if e["secretRef"] != name || e["value"] != nil {
				t.Fatalf("%s = %v", envName, e)
			}
			return
		}
	}
	t.Fatalf("the app has no %s", envName)
}

// onlyTheAppCarries fails when a value went anywhere but the app's own body.
func onlyTheAppCarries(t *testing.T, w *world, values ...string) {
	t.Helper()
	for _, c := range w.f.calls {
		for _, v := range values {
			if strings.Contains(string(c.Body), v) && !strings.HasSuffix(c.Path, "/containerApps/new-app") {
				t.Fatalf("a value went to %s %s", c.Method, c.Path)
			}
		}
	}
}

func TestAMailboxConnectionsPasswordGoesIntoTheAppByConnectionAndField(t *testing.T) {
	w := newWorld(t)
	line := appCreateLine("secret=SMTP_PASSWORD=mail.password secret=SMTP_USER=Mail.username secret=SMTP_HOST=mail.smtpHost secret=SMTP_PORT=MAIL.SMTPPORT")
	o := runOpts{bound: []hostObject{mailbox("mail", "bot@example.test", secretValue)}}

	r := runPlugin(t, o, line)
	if !r.ok() {
		t.Fatal(r.text())
	}
	mustContain(t, r.text(), "0 Key Vault secrets and 4 secrets from this member's connections", "Nothing was changed")
	if len(w.f.writes()) != 0 {
		t.Fatal("whatif wrote")
	}
	assertNoNamedValue(t, r.raw)

	o.config = apply(nil)
	r = runPlugin(t, o, line)
	if !r.ok() {
		t.Fatal(r.text())
	}
	assertNoNamedValue(t, r.raw)
	secrets, env := appPut(t, w)
	mustInline(t, secrets, env, "SMTP_PASSWORD", "smtp-password", secretValue)
	mustInline(t, secrets, env, "SMTP_USER", "smtp-user", mailUser)
	mustInline(t, secrets, env, "SMTP_HOST", "smtp-host", "smtp.example.com")
	mustInline(t, secrets, env, "SMTP_PORT", "smtp-port", "587")
	onlyTheAppCarries(t, w, secretValue, mailUser)
}

func TestAUsernameAndPasswordConnectionAndAnAPIKeyConnectionGoIntoTheApp(t *testing.T) {
	w := newWorld(t)
	r := runPlugin(t, runOpts{
		config: apply(nil),
		bound:  []hostObject{login("Shop login", "shopbot", otherValue), apiKey("stripe live", "Stripe", "stripe", map[string]string{"key": keyValue})},
	}, appCreateLine(`secret=SHOP_USER="shop login.username" "secret=SHOP_PASSWORD=Shop login.password" secret=STRIPE_KEY="Stripe Live.KEY"`))
	if !r.ok() {
		t.Fatal(r.text())
	}
	assertNoNamedValue(t, r.raw)
	secrets, env := appPut(t, w)
	mustInline(t, secrets, env, "SHOP_USER", "shop-user", "shopbot")
	mustInline(t, secrets, env, "SHOP_PASSWORD", "shop-password", otherValue)
	mustInline(t, secrets, env, "STRIPE_KEY", "stripe-key", keyValue)
	onlyTheAppCarries(t, w, otherValue, keyValue)
}

// The 3.0.0 form, a bare appSecret<N>, still resolves against a bound connection's field of that
// name, so a live deploy line keeps working.
func TestTheOldAppSecretNFormStillResolvesFromABoundConnection(t *testing.T) {
	w := newWorld(t)
	r := runPlugin(t, runOpts{
		config: apply(nil),
		moved:  map[string]string{"appSecret1": otherValue},
		bound:  []hostObject{mailbox("mail", "bot@example.test", secretValue)},
	}, appCreateLine("secret=STRIPE_KEY=appSecret1 secret=SMTP_PASSWORD=mail.password"))
	if !r.ok() {
		t.Fatal(r.text())
	}
	assertNoNamedValue(t, r.raw)
	secrets, env := appPut(t, w)
	mustInline(t, secrets, env, "STRIPE_KEY", "stripe-key", otherValue)
	mustInline(t, secrets, env, "SMTP_PASSWORD", "smtp-password", secretValue)
}

func TestAConnectionNotBoundIsRefusedNamingItAndChangesNothing(t *testing.T) {
	for _, c := range []struct {
		o     runOpts
		bound string
	}{
		{runOpts{config: apply(nil), bound: []hostObject{mailbox("mail", "bot@example.test", secretValue), apiKey("stripe live", "Stripe", "stripe", map[string]string{"key": keyValue})}}, "(the ones bound are mail, stripe live)"},
		{runOpts{config: apply(nil)}, "(none is bound yet)"},
	} {
		w := newWorld(t)
		r := runPlugin(t, c.o, appCreateLine("secret=SMTP_PASSWORD=mailbox.password"))
		if r.ok() {
			t.Fatal("a connection not bound was accepted")
		}
		mustContain(t, r.text(), "Refused: the secret SMTP_PASSWORD reads mailbox.password, the field password of the connection mailbox, which is not bound to this member's appSecrets slot "+c.bound,
			"ask the person to bind the connection mailbox to the Azure member's appSecrets slot (Connections a deployed app may use) in Team settings -> Connections",
			"never to send its value in a message", "Nothing was changed.")
		if len(w.f.calls) != 0 {
			t.Fatalf("Azure was called %d times", len(w.f.calls))
		}
		if ev := r.events("refused"); len(ev) != 1 {
			t.Fatalf("refused events = %v", ev)
		}
		assertNoNamedValue(t, r.raw)
	}
}

func TestAFieldTheConnectionLacksIsRefusedNamingItsFieldsAndChangesNothing(t *testing.T) {
	for _, c := range []struct {
		bound hostObject
		arg   string
		want  string
	}{
		{mailbox("mail", "bot@example.test", secretValue), "secret=SMTP_PASSWORD=mail.pass",
			"Refused: the secret SMTP_PASSWORD reads mail.pass, the field pass of the connection mail, which that connection does not have or holds no value in; mail has the fields username, password, imapHost, imapPort, imapSecurity, smtpHost, smtpPort, smtpSecurity, so run containerapp create again naming another field as mail.<field>, or ask the person to add the field pass to it in Team settings -> Connections."},
		{login("shop", "shopbot", otherValue), "secret=SHOP=shop.apiKey",
			"the field apiKey of the connection shop, which that connection does not have or holds no value in; shop has the fields password, url, username,"},
		{apiKey("stripe", "Stripe", "stripe", map[string]string{"key": keyValue, "webhook": ""}), "secret=HOOK=stripe.webhook",
			"the field webhook of the connection stripe, which that connection does not have or holds no value in; stripe has the field key, so run containerapp create again naming it as stripe.key, or ask the person"},
	} {
		w := newWorld(t)
		r := runPlugin(t, runOpts{config: apply(nil), bound: []hostObject{c.bound}}, appCreateLine(c.arg))
		if r.ok() {
			t.Fatalf("%s was accepted", c.arg)
		}
		mustContain(t, r.text(), c.want, "ask the person to add the field", "Nothing was changed.")
		mustNotContain(t, r.text(), "the fields key,")
		if len(w.f.calls) != 0 {
			t.Fatalf("Azure was called %d times", len(w.f.calls))
		}
		assertNoNamedValue(t, r.raw)
	}
}

func TestABareNameNoConnectionHoldsIsRefusedNamingTheConnectionForm(t *testing.T) {
	w := newWorld(t)
	r := runPlugin(t, runOpts{config: apply(nil), bound: []hostObject{mailbox("mail", "bot@example.test", secretValue)}}, appCreateLine("secret=HOOK=appSecret2"))
	if r.ok() {
		t.Fatal("a bare name no connection holds was accepted")
	}
	mustContain(t, r.text(), "Refused: the secret HOOK reads appSecret2, which names no field of a connection bound to this member's appSecrets slot (bound now: mail)",
		"secret= takes a Key Vault secret's address or <connection>.<field>, such as mail.password", "Nothing was changed.")
	if len(w.f.calls) != 0 {
		t.Fatal("Azure was called")
	}
	assertNoNamedValue(t, r.raw)
}

// 3.1.0 declares no namedSecrets, so the Host sends none; nor does the plugin read any.
func TestOldNamedSecretsAreNeverRead(t *testing.T) {
	for _, name := range []string{"main.go", "containerapp.go"} {
		if strings.Contains(string(readFile(t, name)), "namedSecrets") {
			t.Errorf("%s still reads namedSecrets", name)
		}
	}
}

func TestASecretsValueTypedWhereItsNameBelongsIsRefusedWithoutWritingItOut(t *testing.T) {
	for _, value := range []string{"Plain" + strings.Repeat("Z9", 10), "Plain." + strings.Repeat("Z9", 10)} {
		w := newWorld(t)
		r := runPlugin(t, runOpts{config: apply(nil), keys: map[string]map[string]string{"stripe": {"key": value}}},
			appCreateLine("secret=AUTH_MICROSOFT_SECRET="+value))
		if r.ok() {
			t.Fatal("a value in place of a name was accepted")
		}
		mustContain(t, r.text(), "Refused", "never a value")
		if strings.Contains(r.raw, value) || strings.Contains(r.raw, "Z9Z9Z9") {
			t.Fatalf("the value was written out:\n%s", r.raw)
		}
		if len(w.f.calls) != 0 {
			t.Fatal("Azure was called")
		}
	}
}

// Every secret field of every kind of connection is hidden from output, whatever quotes it back;
// a mailbox's username and password are, its servers are not secrets; a website's masked fields and
// username are, its settings (method "password", mayAct "false") are not, so those words still read.
// A connection's name is a name, not a secret, and is shown.
func TestEveryConnectionsSecretFieldsAreHiddenFromOutput(t *testing.T) {
	newWorld(t)
	shopUser := "shopbot-" + strings.Repeat("J2", 6)
	r := runPlugin(t, runOpts{bound: []hostObject{
		mailbox("mail", "bot@example.test", secretValue), login("shop", shopUser, otherValue),
		apiKey("stripe live", "Stripe", "stripe", map[string]string{"key": keyValue}), website("Customer portal", siteValue, tokenValue),
	}}, "show "+secretValue+" "+otherValue+" "+keyValue+" "+siteValue+" "+tokenValue+" "+mailUser+" "+shopUser+" portal-bot")
	assertNoNamedValue(t, r.raw)
	for _, v := range []string{mailUser, strings.Repeat("J2", 6), "portal-bot"} {
		if strings.Contains(r.raw, v) {
			t.Fatalf("a username was written out:\n%s", r.raw)
		}
	}
	k := keyConnections(json.RawMessage(mustJSON(t, []any{mailbox("mail", "bot@example.test", secretValue), website("Customer portal", siteValue, tokenValue)})))
	if len(k) != 2 || k[0].secretField("smtpHost") || !k[0].secretField("password") || !k[0].secretField("username") {
		t.Fatal("a mailbox's username and password are not its secret fields")
	}
	for f, secret := range map[string]bool{"password": true, "token": true, "username": true, "totpSecret": true, "session": true, "siteUrl": false, "method": false, "mayAct": false, "tokenHeader": false} {
		if k[1].secretField(f) != secret {
			t.Errorf("a website's %s: secret = %v", f, !secret)
		}
	}
	if k[0].Name != "mail" || k[1].Name != "Customer portal" {
		t.Fatalf("names = %q, %q", k[0].Name, k[1].Name)
	}
}

// The name lookup hides as the field lookup does: a refusal for a website or two Other keys names
// connections and fields, never a value, and a run that resolves one writes none out.
func TestTheNameLookupWritesNoValueOut(t *testing.T) {
	newWorld(t)
	o := runOpts{config: apply(nil), keys: map[string]map[string]string{"Shop webhooks": {"key": otherValue}, "Blog webhooks": {"key": keyValue}},
		bound: []hostObject{website("Customer portal", siteValue, tokenValue)}}
	for _, line := range []string{
		appCreateLine(`"secret=PORTAL=Customer portal.secret"`),
		appCreateLine(`secret=HOOK=Other.key`),
		appCreateLine(`"secret=PORTAL=customer portal.password" "secret=SHOP=shop webhooks.key"`),
	} {
		r := runPlugin(t, o, line)
		assertNoNamedValue(t, r.raw)
		if strings.Contains(r.raw, strings.Repeat("Q7", 12)) || strings.Contains(r.raw, strings.Repeat("V4", 12)) {
			t.Fatalf("a website's value was written out:\n%s", r.raw)
		}
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// assertNoNamedValue fails when any of the tests' secret values was written out.
func assertNoNamedValue(t *testing.T, raw string) {
	t.Helper()
	assertNoValue(t, raw)
	for _, v := range []string{otherValue, keyValue, siteValue, tokenValue} {
		if strings.Contains(strings.ToLower(raw), strings.ToLower(v)) {
			t.Fatalf("a secret's value was written out:\n%s", raw)
		}
	}
	if strings.Contains(raw, strings.Repeat("K3", 12)) || strings.Contains(raw, strings.Repeat("W5", 12)) ||
		strings.Contains(raw, strings.Repeat("Q7", 12)) || strings.Contains(raw, strings.Repeat("V4", 12)) {
		t.Fatalf("a secret's value was written out:\n%s", raw)
	}
}

func readManifestBytes(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile("plugin.json")
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// The README and the skill show only <connection>.<field>; the old forms appear only in the README's
// migration note, and nowhere in the skill.
func TestTheDocsShowOnlyTheConnectionFieldForm(t *testing.T) {
	skill := string(readFile(t, "skills/plugin-azure-plugin.md"))
	readme := string(readFile(t, "README.md"))
	for name, text := range map[string]string{"README.md": readme, "skills/plugin-azure-plugin.md": skill} {
		for _, want := range []string{"secret=<NAME>=<connection>.<field>", "secret=SMTP_PASSWORD=mail.password", "Connections a deployed app may use"} {
			if !strings.Contains(text, want) {
				t.Errorf("%s lacks %q", name, want)
			}
		}
	}
	for _, old := range []string{"appSecret1", "appSecret<N>", "appSecretN", "App secrets (old)", "named secret", "preset Other", "<app secret name>"} {
		if strings.Contains(strings.ToLower(skill), strings.ToLower(old)) {
			t.Errorf("the skill still says %q", old)
		}
	}
	start, end := strings.Index(readme, "### Migrating from an earlier version"), strings.Index(readme, "## New in 3.0.0")
	if start < 0 || end < start {
		t.Fatal("the README has no migration note before New in 3.0.0")
	}
	note := readme[start:end]
	if !strings.Contains(note, "Secret values from 2.x are not carried over to 3.1.0.") || !strings.Contains(note, "secret=STRIPE_KEY=appSecret1") {
		t.Error("the migration note does not say 2.x values are not carried over, or lost the appSecretN note")
	}
	// No migration slot, no route through 3.0.0, nothing about old secrets on the member's settings.
	for _, gone := range []string{"App secrets (old)", "update to 3.0.0", "3.0.0 first", "App secrets by a name the team chooses, on"} {
		if strings.Contains(readme, gone) {
			t.Errorf("the README still says %q", gone)
		}
	}
	for _, want := range []string{"website", "never its account"} {
		if !strings.Contains(readme, want) || !strings.Contains(skill, want) {
			t.Errorf("the README or the skill lacks %q", want)
		}
	}
	// The main App secrets section names every kind the slot takes, and the fields the Host sends.
	for _, want := range []string{"a mailbox, a username and password, an API key or a website the team already has",
		"a username and password: `username`, `password` and `url`", "a website: the fields it holds a value in, such as `siteUrl`"} {
		if !strings.Contains(readme, want) {
			t.Errorf("the README lacks %q", want)
		}
	}
	rest := readme[:start] + readme[end:]
	for _, old := range []string{"appSecret1", "appSecret<N>", "preset Other", "<app secret name>", "MicrosoftClientSecret"} {
		if strings.Contains(rest, old) {
			t.Errorf("the README outside its migration note still says %q", old)
		}
	}
}
