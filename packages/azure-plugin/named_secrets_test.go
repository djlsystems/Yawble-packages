package main

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
)

// otherValue is a second named secret's value, put together at run time like secretValue.
var otherValue = "other-" + strings.Repeat("K3", 12) + "-value"

func TestTheManifestKeepsTheOldAppSecretsBlockBesideTheEightMovedKeys(t *testing.T) {
	var m struct {
		Secrets      map[string]json.RawMessage `json:"secrets"`
		NamedSecrets *struct {
			Label       string `json:"label"`
			Description string `json:"description"`
			Max         int    `json:"max"`
		} `json:"namedSecrets"`
	}
	raw := readManifestBytes(t)
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	n := m.NamedSecrets
	if n == nil || n.Label != "App secrets (old)" || n.Max != 20 || !strings.Contains(n.Description, "the name is how the team refers to it") || !strings.Contains(n.Description, "Team settings -> Connections") {
		t.Fatalf("namedSecrets = %+v", n)
	}
	if len(m.Secrets) != appSecretFields {
		t.Fatalf("secrets = %d, want the %d app secret keys still", len(m.Secrets), appSecretFields)
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

func TestANamedSecretGoesIntoTheAppsSecretsByItsName(t *testing.T) {
	w := newWorld(t)
	line := "containerapp create " + ownedRG + " new-app my-app-env " + acrRef("myregistry", "my-app:1.0") + " port=8080 secret=AUTH_MICROSOFT_SECRET=MicrosoftClientSecret"
	o := runOpts{named: map[string]string{"MicrosoftClientSecret": secretValue, "GoogleClientSecret": otherValue}}

	r := runPlugin(t, o, line)
	if !r.ok() {
		t.Fatal(r.text())
	}
	mustContain(t, r.text(), "0 Key Vault secrets and 1 secrets from this member's connections", "Nothing was changed")
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
	s := secretNamed(secrets, "auth-microsoft-secret")
	if s == nil || s["value"] != secretValue || s["keyVaultUrl"] != nil || s["identity"] != nil {
		t.Fatalf("the app's secrets = %v", secrets)
	}
	found := false
	for _, e := range env {
		if e["name"] == "AUTH_MICROSOFT_SECRET" {
			found = e["secretRef"] == "auth-microsoft-secret" && e["value"] == nil
		}
	}
	if !found {
		t.Fatalf("the app's env = %v", env)
	}
	// Only the app's own body carries the value, and only the value it names.
	for _, c := range w.f.calls {
		if strings.Contains(string(c.Body), otherValue) {
			t.Fatalf("a secret the command does not name went to %s %s", c.Method, c.Path)
		}
		if strings.Contains(string(c.Body), secretValue) && !strings.HasSuffix(c.Path, "/containerApps/new-app") {
			t.Fatalf("the value went to %s %s", c.Method, c.Path)
		}
	}
}

func TestANamedSecretMatchesItsNameIgnoringCase(t *testing.T) {
	w := newWorld(t)
	r := runPlugin(t, runOpts{config: apply(nil), named: map[string]string{"MicrosoftClientSecret": secretValue}},
		"containerapp create "+ownedRG+" new-app my-app-env "+acrRef("myregistry", "my-app:1.0")+" secret=AUTH_MICROSOFT_SECRET=microsoftclientsecret")
	if !r.ok() {
		t.Fatal(r.text())
	}
	assertNoNamedValue(t, r.raw)
	if s := secretNamed(mustSecrets(t, w), "auth-microsoft-secret"); s == nil || s["value"] != secretValue {
		t.Fatal("the named secret was not put in the app")
	}
}

func mustSecrets(t *testing.T, w *world) []map[string]any {
	t.Helper()
	s, _ := appPut(t, w)
	return s
}

func TestANameTheMemberDoesNotHoldIsRefusedBeforeAnyAzureCall(t *testing.T) {
	base := "containerapp create " + ownedRG + " new-app my-app-env " + acrRef("myregistry", "my-app:1.0") + " "
	for _, c := range []struct {
		o   runOpts
		arg string
	}{
		{runOpts{config: apply(nil), named: map[string]string{"GoogleClientSecret": otherValue}}, "secret=AUTH_MICROSOFT_SECRET=MicrosoftClientSecret"},
		{runOpts{config: apply(nil), named: map[string]string{}}, "secret=AUTH_MICROSOFT_SECRET=MicrosoftClientSecret"},
		// A Host that sends no namedSecrets at all.
		{runOpts{config: apply(nil)}, "secret=AUTH_MICROSOFT_SECRET=MicrosoftClientSecret"},
		// A held name beside the missing one changes nothing either.
		{runOpts{config: apply(nil), named: map[string]string{"GoogleClientSecret": otherValue}}, "secret=AUTH_GOOGLE_SECRET=GoogleClientSecret secret=AUTH_MICROSOFT_SECRET=MicrosoftClientSecret"},
	} {
		w := newWorld(t)
		r := runPlugin(t, c.o, base+c.arg)
		if r.ok() {
			t.Fatalf("%s was accepted", c.arg)
		}
		mustContain(t, r.text(), "Refused: secret=AUTH_MICROSOFT_SECRET=MicrosoftClientSecret names no app secret this member holds",
			"ask the person to add an API-key connection (preset Other) with a field named MicrosoftClientSecret in Team settings -> Connections",
			"bind it to the Azure member's appSecrets slot",
			"never to send its value in a message", "nothing was changed")
		if strings.Count(strings.TrimSuffix(r.text(), "."), ". ") != 0 {
			t.Fatalf("the refusal is not one sentence: %q", r.text())
		}
		if len(w.f.calls) != 0 {
			t.Fatalf("Azure was called %d times, first %s %s", len(w.f.calls), w.f.calls[0].Method, w.f.calls[0].Path)
		}
		if ev := r.events("refused"); len(ev) != 1 {
			t.Fatalf("refused events = %v", ev)
		}
		assertNoNamedValue(t, r.raw)
	}
}

func TestASecretsValueTypedWhereItsNameBelongsIsRefusedWithoutWritingItOut(t *testing.T) {
	w := newWorld(t)
	value := "Plain" + strings.Repeat("Z9", 10)
	r := runPlugin(t, runOpts{config: apply(nil), named: map[string]string{"MicrosoftClientSecret": value}},
		"containerapp create "+ownedRG+" new-app my-app-env "+acrRef("myregistry", "my-app:1.0")+" secret=AUTH_MICROSOFT_SECRET="+value)
	if r.ok() {
		t.Fatal("a value in place of a name was accepted")
	}
	mustContain(t, r.text(), "Refused", "never a value")
	if strings.Contains(r.raw, value) || strings.Contains(r.raw, "with a field named") {
		t.Fatalf("the value was written out or asked for by name:\n%s", r.raw)
	}
	if len(w.f.calls) != 0 {
		t.Fatal("Azure was called")
	}
}

func TestAMovedAppSecretNStillResolvesBesideAnOldNamedSecret(t *testing.T) {
	w := newWorld(t)
	line := "containerapp create " + ownedRG + " new-app my-app-env " + acrRef("myregistry", "my-app:1.0") + " secret=STRIPE_KEY=appSecret1 secret=AUTH_MICROSOFT_SECRET=MicrosoftClientSecret"
	r := runPlugin(t, runOpts{
		config: apply(nil),
		moved:  map[string]string{"appSecret1": otherValue},
		named:  map[string]string{"MicrosoftClientSecret": secretValue},
	}, line)
	if !r.ok() {
		t.Fatal(r.text())
	}
	assertNoNamedValue(t, r.raw)
	secrets := mustSecrets(t, w)
	if s := secretNamed(secrets, "stripe-key"); s == nil || s["value"] != otherValue {
		t.Fatalf("appSecret1 did not resolve: %v", secretNamed(secrets, "stripe-key") != nil)
	}
	if s := secretNamed(secrets, "auth-microsoft-secret"); s == nil || s["value"] != secretValue {
		t.Fatal("the named secret did not resolve")
	}
}

func TestEveryNamedSecretsValueIsHiddenFromOutput(t *testing.T) {
	newWorld(t)
	r := runPlugin(t, runOpts{named: map[string]string{"MicrosoftClientSecret": secretValue, "GoogleClientSecret": otherValue}}, "show "+secretValue+" "+otherValue)
	assertNoNamedValue(t, r.raw)
}

func assertNoNamedValue(t *testing.T, raw string) {
	t.Helper()
	assertNoValue(t, raw)
	if strings.Contains(strings.ToLower(raw), strings.ToLower(otherValue)) || strings.Contains(raw, strings.Repeat("K3", 12)) {
		t.Fatalf("a named secret's value was written out:\n%s", raw)
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
