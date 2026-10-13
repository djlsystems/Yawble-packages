package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Key Vault and the apps' logs are reached through Azure management only, and an app may read a
// secret from this member's own keys.

func TestTheMicrosoftSlotAsksForAzureManagementSQLAndStorageOnly(t *testing.T) {
	m := readManifest(t)
	want := []string{
		"https://management.azure.com/user_impersonation",
		"https://database.windows.net/user_impersonation",
		"https://storage.azure.com/user_impersonation",
		"offline_access",
	}
	if got := m.Connections[slot].Scopes["microsoft"]; !slices.Equal(got, want) {
		t.Fatalf("scopes = %v, want %v", got, want)
	}
	if len(m.Secrets) != appSecretFields {
		t.Fatalf("secrets = %d, want %d", len(m.Secrets), appSecretFields)
	}
	for n := 1; n <= appSecretFields; n++ {
		field := "appSecret" + string(rune('0'+n))
		s, ok := m.Secrets[field]
		if !ok || s.Required || !strings.Contains(s.Description, "secret=<NAME>="+field) || !strings.Contains(s.Description, "on the screen") {
			t.Fatalf("%s = %+v", field, s)
		}
	}
}

// TestNoCodeCallsKeyVaultOrLogAnalytics scans the plugin's own code: no string names the Key Vault
// API or Log Analytics' host. A vault's address stays only as the form of a Key Vault reference,
// which Azure reads with the app's identity.
func TestNoCodeCallsKeyVaultOrLogAnalytics(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		raw, _ := os.ReadFile(name)
		text := string(raw)
		for _, bad := range []string{`"https://vault.azure.net`, "api.loganalytics" + ".io", "loganalytics" + ".io/v1", `.vault.azure.net/secrets?`, `.vault.azure.net" + "/secrets`} {
			if strings.Contains(text, bad) {
				t.Fatalf("%s holds %q", name, bad)
			}
		}
	}
	for a := range apiNames {
		if strings.Contains(string(a), "vault") || strings.Contains(string(a), "loganalytics") {
			t.Fatalf("the client knows the API %s", a)
		}
	}
}

func TestKeyVaultSecretsAreListedByNameThroughAzureManagement(t *testing.T) {
	w := newWorld(t)
	w.f.addVault(ownedRG, "busyvault", ownTags(), "zeta", "alpha")
	r := runPlugin(t, runOpts{}, "keyvault secrets busyvault")
	if !r.ok() {
		t.Fatal(r.text())
	}
	mustContain(t, r.text(), "Secrets in Key Vault busyvault, by name only (2):\n- alpha\n- zeta")
	vault := inRG(ownedRG, "Microsoft.KeyVault/vaults/busyvault")
	got := w.f.callsTo(http.MethodGet, "/secrets")
	if len(got) != 1 || got[0].Host != armHost || got[0].Path != vault+"/secrets" || got[0].Token != tokARM || got[0].Query != "api-version="+versionOf("Microsoft.KeyVault/vaults") {
		t.Fatalf("the secrets list = %+v", got)
	}
	for _, c := range w.f.calls {
		if c.Host != armHost {
			t.Fatalf("a request went to %s", c.Host)
		}
	}
	mustContain(t, runPlugin(t, runOpts{}, "keyvault secrets nosuchvault").text(), "Refused: there is no Key Vault nosuchvault")
}

func TestAVaultTheAccountCannotListSaysItNeedsReader(t *testing.T) {
	w := newWorld(t)
	v := w.f.addVault(ownedRG, "lockedvault", ownTags(), "x")
	w.f.hosts[armHost] = func(rw http.ResponseWriter, req *http.Request) {
		if strings.EqualFold(strings.TrimPrefix(req.URL.Path, "/"+armHost), v.ID+"/secrets") {
			w.f.answer(rw, 403, armError("AuthorizationFailed", "No access."))
			return
		}
		w.f.mu.Lock()
		defer w.f.mu.Unlock()
		w.f.arm(rw, req.Method, strings.TrimPrefix(req.URL.Path, "/"+armHost), nil)
	}
	r := runPlugin(t, runOpts{}, "keyvault secrets lockedvault")
	mustContain(t, r.text(), "Refused", "needs Reader (or more) on the vault")
}

func TestLogsAreQueriedThroughAzureManagementWithColumnNameColumns(t *testing.T) {
	w := newWorld(t)
	r := runPlugin(t, runOpts{}, "containerapp logs my-app 15")
	if !r.ok() {
		t.Fatal(r.text())
	}
	mustContain(t, r.text(), "Logs of my-app, the last 15 minutes (2 lines", "2026-10-09T14:00:00Z  started\n2026-10-09T14:01:00Z  listening on 8080")
	q := w.f.callsTo(http.MethodPost, "/api/query")
	if len(q) != 1 {
		t.Fatalf("queries = %v", q)
	}
	var body map[string]string
	_ = json.Unmarshal(q[0].Body, &body)
	if body["timespan"] != "PT15M" || !strings.Contains(body["query"], "ContainerAppName == 'my-app'") {
		t.Fatalf("the query = %s", q[0].Body)
	}
	if q[0].Path != inRG(ownedRG, "Microsoft.OperationalInsights/workspaces/my-app-env-logs")+"/api/query" || q[0].Query != "api-version=2017-01-01-preview" {
		t.Fatalf("the query went to %s?%s", q[0].Path, q[0].Query)
	}
}

func TestLogsOfAnEnvironmentThatNamesOnlyACustomerIDFindTheWorkspaceByListing(t *testing.T) {
	w := newWorld(t)
	f := w.f
	env := f.add(ownedRG, "Microsoft.App/managedEnvironments", "la-env", ownTags())
	env.Props["appLogsConfiguration"] = map[string]any{"destination": "log-analytics",
		"logAnalyticsConfiguration": map[string]any{"customerId": strings.ToUpper("00000000-0000-0000-0000-0000000000c2")}}
	ws := f.add(ownedRG, "Microsoft.OperationalInsights/workspaces", "la-logs", ownTags())
	ws.Props["customerId"] = "00000000-0000-0000-0000-0000000000c2"
	app := f.add(ownedRG, "Microsoft.App/containerApps", "la-app", ownTags())
	app.Props = map[string]any{"managedEnvironmentId": env.ID}
	f.logLines = []string{"hello"}

	r := runPlugin(t, runOpts{}, "containerapp logs la-app")
	if !r.ok() {
		t.Fatal(r.text())
	}
	mustContain(t, r.text(), "Logs of la-app, the last 30 minutes (1 lines", "  hello")
	if q := f.callsTo(http.MethodPost, "/api/query"); len(q) != 1 || q[0].Path != ws.ID+"/api/query" {
		t.Fatalf("the query = %+v", q)
	}
	if l := f.callsTo(http.MethodGet, "/providers/Microsoft.OperationalInsights/workspaces"); len(l) != 1 {
		t.Fatalf("the workspaces were listed %d times", len(l))
	}

	ws.Props["customerId"] = "00000000-0000-0000-0000-0000000000c3"
	mustContain(t, runPlugin(t, runOpts{}, "containerapp logs la-app").text(), "Refused", "which this sign-in cannot see in the subscription")
}

// secretValue is put together at run time, so no file holds it whole.
var secretValue = "app-" + strings.Repeat("Q7", 12) + "-value"

func TestAnAppSecretFromTheMembersOwnKeysGoesOnlyIntoTheAppsConfiguration(t *testing.T) {
	w := newWorld(t)
	line := "containerapp create " + ownedRG + " new-app my-app-env " + acrRef("myregistry", "my-app:1.0") + " port=8080 secret=STRIPE_KEY=appSecret1 secret=DB_PASSWORD=" + w.vaultURL
	o := runOpts{secrets: map[string]string{"appSecret1": secretValue}, secretKeys: map[string]string{"appSecret1": "SHOP_STRIPE_KEY"}}

	r := runPlugin(t, o, line)
	if !r.ok() {
		t.Fatal(r.text())
	}
	mustContain(t, r.text(), "1 Key Vault secrets and 1 secrets from this member's keys", "Nothing was changed")
	if len(w.f.writes()) != 0 {
		t.Fatal("whatif wrote")
	}
	assertNoValue(t, r.raw)

	o.config = apply(nil)
	r = runPlugin(t, o, line)
	if !r.ok() {
		t.Fatal(r.text())
	}
	assertNoValue(t, r.raw)
	if ev := r.events("applied"); len(ev) != 1 || !strings.Contains(ev[0]["summary"].(string), "1 secrets from this member's keys") {
		t.Fatalf("applied = %v", ev)
	}
	put := w.f.callsTo(http.MethodPut, "/containerApps/new-app")
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
	var inline map[string]any
	for _, s := range app.Properties.Configuration.Secrets {
		if s["name"] == "stripe-key" {
			inline = s
		}
	}
	if inline == nil || inline["value"] != secretValue || inline["keyVaultUrl"] != nil || inline["identity"] != nil {
		t.Fatalf("the app's secrets = %v", app.Properties.Configuration.Secrets)
	}
	found := false
	for _, e := range app.Properties.Template.Containers[0].Env {
		if e["name"] == "STRIPE_KEY" {
			found = e["secretRef"] == "stripe-key" && e["value"] == nil
		}
	}
	if !found {
		t.Fatalf("the app's env = %v", app.Properties.Template.Containers[0].Env)
	}
	// Only the app's own body carries the value.
	for _, c := range w.f.calls {
		if strings.Contains(string(c.Body), secretValue) && !strings.HasSuffix(c.Path, "/containerApps/new-app") {
			t.Fatalf("the value went to %s %s", c.Method, c.Path)
		}
	}
}

func TestAnAppSecretFromAnUnboundOrUnsetKeyIsRefusedNamingTheKey(t *testing.T) {
	w := newWorld(t)
	base := "containerapp create " + ownedRG + " new-app my-app-env " + acrRef("myregistry", "my-app:1.0") + " "
	for _, c := range []struct {
		o    runOpts
		arg  string
		want []string
	}{
		{runOpts{config: apply(nil), secretKeys: map[string]string{"appSecret2": "SHOP_WEBHOOK_SECRET"}}, "secret=HOOK=appSecret2",
			[]string{"appSecret2, which is bound to the key SHOP_WEBHOOK_SECRET, and that key is not set", "A person sets SHOP_WEBHOOK_SECRET on the screen first (this member's settings)"}},
		{runOpts{config: apply(nil)}, "secret=HOOK=appSecret3",
			[]string{"appSecret3, which this member does not bind to a key", "sets it on the screen first (this member's settings)"}},
		{runOpts{config: apply(nil), secrets: map[string]string{"appSecret1": secretValue}}, "secret=HOOK=appSecret9",
			[]string{"names no field of this member; its own keys are appSecret1 to appSecret8"}},
		{runOpts{config: apply(nil), secrets: map[string]string{"appSecret1": secretValue}}, "secret=HOOK=appSecret01",
			[]string{"names no field of this member"}},
	} {
		r := runPlugin(t, c.o, base+c.arg)
		if r.ok() {
			t.Fatalf("%s was accepted", c.arg)
		}
		mustContain(t, r.text(), append([]string{"Refused"}, c.want...)...)
		assertNoValue(t, r.raw)
	}
	r := runPlugin(t, runOpts{config: apply(nil)}, base+"env=API_TOKEN=x"+strings.Repeat("y", 8))
	mustContain(t, r.text(), "API_TOKEN looks like a secret", "secret=API_TOKEN=https://<vault>.vault.azure.net/secrets/<name>", "secret=API_TOKEN=appSecret<1-8>")
	if len(w.f.writes()) != 0 {
		t.Fatal("a refused app wrote")
	}
}

func TestAnAppSecretValueIsRedactedWhereverItWouldBeWritten(t *testing.T) {
	newFake(t)
	// A value an instruction quotes back is hidden like a token.
	r := runPlugin(t, runOpts{secrets: map[string]string{"appSecret4": secretValue}}, "show "+secretValue)
	assertNoValue(t, r.raw)
}

func assertNoValue(t *testing.T, raw string) {
	t.Helper()
	if strings.Contains(strings.ToLower(raw), strings.ToLower(secretValue)) || strings.Contains(raw, strings.Repeat("Q7", 12)) {
		t.Fatalf("an app secret's value was written out:\n%s", raw)
	}
}
