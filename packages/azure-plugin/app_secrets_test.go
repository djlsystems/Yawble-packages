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
// username and password or an API key. The old named secrets (the "App secrets (old)" block) are gone.

// otherValue and keyValue are further secret values, put together at run time like secretValue.
var (
	otherValue = "other-" + strings.Repeat("K3", 12) + "-value"
	keyValue   = "key-" + strings.Repeat("W5", 12) + "-value"
)

const mailUser = "bot-mailer@example.com"

// mailbox is an imap connection named name as the Host hands it over: username, password and servers.
func mailbox(name, password string) map[string]any {
	return map[string]any{
		"kind": "imap", "account": name, "username": mailUser, "password": password,
		"imap": map[string]any{"host": "imap.example.com", "port": 993, "security": "ssl"},
		"smtp": map[string]any{"host": "smtp.example.com", "port": 587, "security": "starttls"},
	}
}

func login(name, username, password string) map[string]any {
	return map[string]any{"kind": "login", "account": name, "preset": nil, "fields": map[string]string{"username": username, "password": password}}
}

func apiKey(name string, fields map[string]string) map[string]any {
	return map[string]any{"kind": "apikey", "account": name, "preset": "stripe", "fields": fields}
}

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
	if want := []string{"imap", "login", "apikey"}; strings.Join(keys.Providers, ",") != strings.Join(want, ",") || !keys.Multiple || keys.Required {
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
	o := runOpts{bound: []map[string]any{mailbox("mail", secretValue)}}

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
		bound:  []map[string]any{login("Shop login", "shopbot", otherValue), apiKey("stripe", map[string]string{"key": keyValue})},
	}, appCreateLine(`secret=SHOP_USER="shop login.username" "secret=SHOP_PASSWORD=Shop login.password" secret=STRIPE_KEY=Stripe.KEY`))
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
		bound:  []map[string]any{mailbox("mail", secretValue)},
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
		{runOpts{config: apply(nil), bound: []map[string]any{mailbox("mail", secretValue), apiKey("stripe", map[string]string{"key": keyValue})}}, "(the ones bound are mail, stripe)"},
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
		bound map[string]any
		arg   string
		want  string
	}{
		{mailbox("mail", secretValue), "secret=SMTP_PASSWORD=mail.pass",
			"Refused: the secret SMTP_PASSWORD reads mail.pass, the field pass of the connection mail, which that connection does not have or holds no value in; mail has the fields username, password, imapHost, imapPort, imapSecurity, smtpHost, smtpPort, smtpSecurity, so run containerapp create again naming one of them as mail.<field>"},
		{login("shop", "shopbot", otherValue), "secret=SHOP=shop.apiKey",
			"the field apiKey of the connection shop, which that connection does not have or holds no value in; shop has the fields password, username,"},
		{apiKey("stripe", map[string]string{"key": keyValue, "webhook": ""}), "secret=HOOK=stripe.webhook",
			"the field webhook of the connection stripe, which that connection does not have or holds no value in; stripe has the fields key,"},
	} {
		w := newWorld(t)
		r := runPlugin(t, runOpts{config: apply(nil), bound: []map[string]any{c.bound}}, appCreateLine(c.arg))
		if r.ok() {
			t.Fatalf("%s was accepted", c.arg)
		}
		mustContain(t, r.text(), c.want, "ask the person to add the field", "Nothing was changed.")
		if len(w.f.calls) != 0 {
			t.Fatalf("Azure was called %d times", len(w.f.calls))
		}
		assertNoNamedValue(t, r.raw)
	}
}

func TestABareNameNoConnectionHoldsIsRefusedNamingTheConnectionForm(t *testing.T) {
	w := newWorld(t)
	r := runPlugin(t, runOpts{config: apply(nil), bound: []map[string]any{mailbox("mail", secretValue)}}, appCreateLine("secret=HOOK=appSecret2"))
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

// A Host that still sends the old namedSecrets: 3.1.0 never reads them.
func TestOldNamedSecretsInTheRequestAreNeverRead(t *testing.T) {
	w := newWorld(t)
	r := runPlugin(t, runOpts{config: apply(nil), named: map[string]string{"MicrosoftClientSecret": secretValue}},
		appCreateLine("secret=AUTH_MICROSOFT_SECRET=MicrosoftClientSecret"))
	if r.ok() {
		t.Fatal("an old named secret was used")
	}
	mustContain(t, r.text(), "names no field of a connection bound to this member's appSecrets slot (none is bound yet)", "Nothing was changed.")
	if len(w.f.calls) != 0 {
		t.Fatal("Azure was called")
	}
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
// a mailbox's username and password are, its servers are not secrets.
func TestEveryConnectionsSecretFieldsAreHiddenFromOutput(t *testing.T) {
	newWorld(t)
	r := runPlugin(t, runOpts{bound: []map[string]any{
		mailbox("mail", secretValue), login("shop", "shopbot-"+strings.Repeat("J2", 6), otherValue), apiKey("stripe", map[string]string{"key": keyValue}),
	}}, "show "+secretValue+" "+otherValue+" "+keyValue+" "+mailUser+" shopbot-"+strings.Repeat("J2", 6))
	assertNoNamedValue(t, r.raw)
	if strings.Contains(r.raw, mailUser) || strings.Contains(r.raw, strings.Repeat("J2", 6)) {
		t.Fatalf("a username was written out:\n%s", r.raw)
	}
	k := keyConnections(json.RawMessage(mustJSON(t, []any{mailbox("mail", secretValue)})))
	if len(k) != 1 || k[0].secretField("smtpHost") || !k[0].secretField("password") || !k[0].secretField("username") {
		t.Fatal("a mailbox's username and password are not its secret fields")
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
	for _, v := range []string{otherValue, keyValue} {
		if strings.Contains(strings.ToLower(raw), strings.ToLower(v)) {
			t.Fatalf("a secret's value was written out:\n%s", raw)
		}
	}
	if strings.Contains(raw, strings.Repeat("K3", 12)) || strings.Contains(raw, strings.Repeat("W5", 12)) {
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
	start, end := strings.Index(readme, "### Migrating from 3.0.0 or earlier"), strings.Index(readme, "## New in 3.0.0")
	if start < 0 || end < start {
		t.Fatal("the README has no migration note before New in 3.0.0")
	}
	rest := readme[:start] + readme[end:]
	for _, old := range []string{"appSecret1", "appSecret<N>", "preset Other", "<app secret name>", "MicrosoftClientSecret"} {
		if strings.Contains(rest, old) {
			t.Errorf("the README outside its migration note still says %q", old)
		}
	}
}
