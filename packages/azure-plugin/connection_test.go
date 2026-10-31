package main

import (
	"os"
	"strings"
	"testing"
)

// From 3.0.0 the app secrets are held by the API-key connections bound to the multiple appSecrets
// slot; appSecret1 to appSecret8 are declared only as moved there (CON 7).

func TestEveryAppSecretFieldIsMovedToTheMultipleAPIKeySlot(t *testing.T) {
	m := readManifest(t)
	keys, ok := m.Connections[keySlot]
	if !ok || len(keys.Providers) != 1 || keys.Providers[0] != "apikey" || !keys.Multiple || keys.Required {
		t.Fatalf("appSecrets = %+v", keys)
	}
	if ms, ok := m.Connections[slot]; !ok || !ms.Required || ms.Providers[0] != "microsoft" {
		t.Fatalf("the microsoft slot = %+v", ms)
	}
	for name, s := range m.Secrets {
		if s.MovedTo != keySlot || s.Required {
			t.Errorf("secret %s is not moved to %s: %+v", name, keySlot, s)
		}
	}
}

func appCreateLine(secrets string) string {
	return "containerapp create " + ownedRG + " new-app my-app-env " + acrRef("myregistry", "my-app:1.0") + " " + secrets
}

func TestAFieldOfAnyBoundConnectionResolvesByItsNameIgnoringCase(t *testing.T) {
	w := newWorld(t)
	r := runPlugin(t, runOpts{
		config: apply(nil),
		moved:  map[string]string{"appSecret1": otherValue},
		keys:   map[string]map[string]string{"Shop auth": {"MicrosoftClientSecret": secretValue}},
	}, appCreateLine("secret=AUTH_MICROSOFT_SECRET=microsoftclientsecret secret=STRIPE_KEY=appSecret1"))
	if !r.ok() {
		t.Fatal(r.text())
	}
	assertNoNamedValue(t, r.raw)
	secrets := mustSecrets(t, w)
	if s := secretNamed(secrets, "auth-microsoft-secret"); s == nil || s["value"] != secretValue {
		t.Fatal("the second connection's field was not put in the app")
	}
	if s := secretNamed(secrets, "stripe-key"); s == nil || s["value"] != otherValue {
		t.Fatal("the moved appSecret1 was not put in the app")
	}
}

func TestAConnectionFieldWinsOverAnOldNamedSecretOfTheSameName(t *testing.T) {
	w := newWorld(t)
	r := runPlugin(t, runOpts{
		config: apply(nil),
		keys:   map[string]map[string]string{"Shop auth": {"MicrosoftClientSecret": secretValue}},
		named:  map[string]string{"MicrosoftClientSecret": otherValue},
	}, appCreateLine("secret=AUTH_MICROSOFT_SECRET=MicrosoftClientSecret"))
	if !r.ok() {
		t.Fatal(r.text())
	}
	assertNoNamedValue(t, r.raw)
	if s := secretNamed(mustSecrets(t, w), "auth-microsoft-secret"); s == nil || s["value"] != secretValue {
		t.Fatal("the old named secret won over the connection")
	}
}

func TestAFieldTwoConnectionsHoldWithDifferentValuesIsRefusedNamingBoth(t *testing.T) {
	w := newWorld(t)
	r := runPlugin(t, runOpts{
		config: apply(nil),
		keys:   map[string]map[string]string{"Shop auth": {"ClientSecret": secretValue}, "Blog auth": {"clientsecret": otherValue}},
	}, appCreateLine("secret=AUTH=ClientSecret"))
	if r.ok() {
		t.Fatal("an ambiguous field was accepted")
	}
	mustContain(t, r.text(), "Refused: secret=AUTH=ClientSecret is a field of two connections bound to this member's appSecrets slot, Blog auth and Shop auth", "rename the field", "nothing was changed")
	assertNoNamedValue(t, r.raw)
	if len(w.f.calls) != 0 {
		t.Fatal("Azure was called")
	}

	// The same value in two connections is not ambiguous.
	w = newWorld(t)
	r = runPlugin(t, runOpts{
		config: apply(nil),
		keys:   map[string]map[string]string{"Shop auth": {"ClientSecret": secretValue}, "Blog auth": {"ClientSecret": secretValue}},
	}, appCreateLine("secret=AUTH=ClientSecret"))
	if !r.ok() {
		t.Fatal(r.text())
	}
}

func TestValuesSentUnderSecretsAreIgnored(t *testing.T) {
	w := newWorld(t)
	r := runPlugin(t, runOpts{config: apply(nil), sent: map[string]string{"appSecret1": secretValue}}, appCreateLine("secret=STRIPE_KEY=appSecret1"))
	if r.ok() {
		t.Fatal("a value under secrets was used")
	}
	mustContain(t, r.text(), "names no app secret this member holds", "Team settings -> Connections")
	if len(w.f.calls) != 0 {
		t.Fatal("Azure was called")
	}
}

func TestTheSkillAndReadmeSayWhereTheAppSecretsAreSet(t *testing.T) {
	for _, name := range []string{"README.md", "skills/plugin-azure-plugin.md"} {
		text := string(readFile(t, name))
		if !strings.Contains(text, "Team settings -> Connections") || !strings.Contains(text, "appSecrets") {
			t.Errorf("%s does not say the app secrets are set in Team settings -> Connections", name)
		}
		for _, old := range []string{"binds the field to a key", "binds it to a key", "sets that key on the screen", "under App secrets on the"} {
			if strings.Contains(text, old) {
				t.Errorf("%s still says %q", name, old)
			}
		}
	}
}

func readFile(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
