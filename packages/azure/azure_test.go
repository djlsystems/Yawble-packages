package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func apply(extra map[string]any) map[string]any {
	cfg := baseConfig(map[string]any{"mode": "apply"})
	for k, v := range extra {
		cfg[k] = v
	}
	return cfg
}

// ---- setup ----

func TestSetupNotSignedInSaysToSignIn(t *testing.T) {
	f := newFake(t)
	r := runPlugin(t, runOpts{noConnect: true}, "setup")
	if !r.ok() {
		t.Fatalf("setup failed: %s", r.text())
	}
	mustContain(t, r.text(), "- signed in: no", "Next:", "Sign in with Microsoft on this member's microsoft connection slot")
	if len(f.calls) != 0 {
		t.Fatalf("setup called Azure with no sign-in: %v", f.calls)
	}
}

func TestSetupWithNoSubscriptionListsTheReadableOnes(t *testing.T) {
	newFake(t)
	r := runPlugin(t, runOpts{config: map[string]any{}}, "setup")
	mustContain(t, r.text(),
		"- signed in: yes, as person@example.com",
		"- subscription: not set; this sign-in can read "+testSub+" (My Subscription)",
		"Put one of these subscription ids in this member's subscription setting")
}

func TestSetupNamesEachMissingStep(t *testing.T) {
	f := newFake(t)
	f.actions = []string{"*/read"} // a Reader: no resource groups, no registering
	f.providers["Microsoft.App"] = "NotRegistered"
	f.providers["Microsoft.OperationalInsights"] = "NotRegistered"
	f.add(ownedRG, "Microsoft.Sql/servers", "found-sql", nil)
	cfg := baseConfig(map[string]any{
		"regions":          []string{"westeurope", "atlantis"},
		"sharedSqlServers": []string{"found-sql", "lost-sql"},
		"sharedRegistries": []string{"lostregistry"},
		"monthlyBudget":    50,
	})
	tokens := map[string]string{"https://management.azure.com/": tokARM, "https://database.windows.net/": tokSQL}

	r := runPlugin(t, runOpts{config: cfg, tokens: tokens}, "setup")

	if !r.ok() {
		t.Fatalf("setup failed: %s", r.text())
	}
	mustContain(t, r.text(),
		"- tokens: none for Azure Storage",
		"Reconnect the Microsoft connection in Admin, Connections and accept Azure Storage;",
		"- subscription: My Subscription ("+testSub+"), readable",
		"- role: does not allow creating resource groups",
		"give this account the Contributor role on the subscription",
		"- regions: atlantis not offered to this subscription",
		"- sharedSqlServers reachable: found-sql",
		"- sharedSqlServers not reachable: lost-sql",
		"Check sharedSqlServers in this member's settings: this sign-in cannot see the SQL server lost-sql",
		"- sharedRegistries not reachable: lostregistry",
		"Check sharedRegistries in this member's settings: this sign-in cannot see the registry lostregistry",
		"- providers not registered: Microsoft.App, Microsoft.OperationalInsights; this account's role cannot register them",
		"Ask the subscription's owner to register Microsoft.App, Microsoft.OperationalInsights",
		"budgetEmails is empty",
		"Put the addresses that get the budget alerts in budgetEmails")
	if w := f.writes(); len(w) != 0 {
		t.Fatalf("setup wrote: %v", w)
	}
}

func TestSetupWithEverythingInPlaceIsReady(t *testing.T) {
	newFake(t)
	r := runPlugin(t, runOpts{}, "setup")
	mustContain(t, r.text(), "- role: allows creating resource groups", "- providers registered: Microsoft.App, Microsoft.OperationalInsights", "Ready:")
	if strings.Contains(r.text(), "Next:") {
		t.Fatalf("setup named a step with everything in place:\n%s", r.text())
	}
}

func TestSetupRegistersProvidersOnlyInApply(t *testing.T) {
	f := newFake(t)
	f.providers["Microsoft.App"] = "NotRegistered"

	r := runPlugin(t, runOpts{}, "setup")
	mustContain(t, r.text(), "providers not registered: Microsoft.App; whatif registers nothing", "a person sets mode to apply and runs setup again")
	if w := f.writes(); len(w) != 0 {
		t.Fatalf("setup in whatif wrote: %v", w)
	}
	if ev := r.events("planned"); len(ev) != 1 || ev[0]["summary"] != "register Microsoft.App" {
		t.Fatalf("planned = %v", ev)
	}

	r = runPlugin(t, runOpts{config: apply(nil)}, "setup")
	mustContain(t, r.text(), "providers registering now: Microsoft.App")
	if reg := f.callsTo(http.MethodPost, "/providers/Microsoft.App/register"); len(reg) != 1 {
		t.Fatalf("register calls = %v", reg)
	}
}

// ---- whatif changes nothing ----

func TestWhatifChangesNothingForEveryChangingCommand(t *testing.T) {
	f := newFake(t)
	f.add(ownedRG, "Microsoft.Network/dnsZones", "example.com", ownTags())
	ident := f.add(ownedRG, "Microsoft.ManagedIdentity/userAssignedIdentities", "app-id", ownTags())
	ident.Props["principalId"] = "00000000-0000-0000-0000-00000000000a"
	f.add(ownedRG, "Microsoft.Storage/storageAccounts", "mystore", ownTags())
	tpl := docFile(t, "infra/main.json", simpleTemplate)
	f.whatIf = []map[string]any{wiChange("Create", inRG(ownedRG, "Microsoft.ManagedIdentity/userAssignedIdentities/new-id"), nil)}

	cfg := baseConfig(map[string]any{"allowDelete": true})
	for _, line := range []string{
		"rg create new-rg westeurope",
		"deploy " + ownedRG + " " + tpl,
		"dns zone create " + ownedRG + " example.org",
		"dns record set example.com www CNAME app.example.net",
		"dns record delete example.com www CNAME",
		"role assign \"Storage Blob Data Contributor\" " + ident.ID + " " + inRG(ownedRG, "Microsoft.Storage/storageAccounts/mystore"),
		"delete " + inRG(ownedRG, "Microsoft.Storage/storageAccounts/mystore"),
		"delete " + ownedRG,
	} {
		r := runPlugin(t, runOpts{config: cfg}, line)
		if !r.ok() {
			t.Fatalf("%s: %s", line, r.text())
		}
		if !strings.Contains(r.text(), "whatif") || !strings.Contains(r.text(), "Nothing was") && !strings.Contains(r.text(), "nothing was deployed") {
			t.Fatalf("%s did not say it changed nothing:\n%s", line, r.text())
		}
		if ev := r.events("planned"); len(ev) != 1 {
			t.Fatalf("%s: planned events = %v", line, ev)
		}
		if ev := r.events("applied"); len(ev) != 0 {
			t.Fatalf("%s: applied in whatif: %v", line, ev)
		}
	}
	if w := f.writes(); len(w) != 0 {
		t.Fatalf("whatif sent writes to Azure: %v", w)
	}
}

// ---- rg create ----

func TestRgCreateTagsThePrefixedGroupAndSetsTheBudget(t *testing.T) {
	f := newFake(t)
	cfg := apply(map[string]any{"namePrefix": "team1-", "monthlyBudget": 100, "budgetEmails": []string{"person@example.com"}})

	r := runPlugin(t, runOpts{config: cfg}, "rg create web westeurope")

	if !r.ok() {
		t.Fatalf("rg create failed: %s", r.text())
	}
	mustContain(t, r.text(), "Resource group team1-web is ready in westeurope", "budget of 100 a month")
	g := f.rgs["team1-web"]
	if g == nil || g.Tags["yawble-owner"] != testTeam || g.Tags["yawble-plugin"] != "azure" {
		t.Fatalf("group = %+v", g)
	}
	budgets := f.callsTo(http.MethodPut, "/providers/Microsoft.Consumption/budgets/yawble-monthly")
	if len(budgets) != 1 {
		t.Fatalf("budget calls = %v", budgets)
	}
	var b struct {
		Properties struct {
			Amount        float64 `json:"amount"`
			Notifications map[string]struct {
				Threshold     int      `json:"threshold"`
				ContactEmails []string `json:"contactEmails"`
			} `json:"notifications"`
		} `json:"properties"`
	}
	_ = json.Unmarshal(budgets[0].Body, &b)
	if b.Properties.Amount != 100 || b.Properties.Notifications["actual80"].Threshold != 80 || b.Properties.Notifications["actual100"].Threshold != 100 {
		t.Fatalf("budget = %s", budgets[0].Body)
	}
	if ev := r.events("applied"); len(ev) != 1 || len(ev[0]["resourceIds"].([]any)) != 2 {
		t.Fatalf("applied = %v", ev)
	}
}

func TestRgCreateRefusesARegionNotListedAndAGroupItDoesNotOwn(t *testing.T) {
	f := newFake(t)
	r := runPlugin(t, runOpts{config: apply(nil)}, "rg create web eastus2")
	mustContain(t, r.text(), "eastus2 is not in this member's regions")

	r = runPlugin(t, runOpts{config: apply(nil)}, "rg create "+foreignRG+" westeurope")
	if r.ok() {
		t.Fatal("an existing group of someone else's was taken")
	}
	mustContain(t, r.text(), "resource group "+foreignRG+" does not carry this team's tags")
	if w := f.writes(); len(w) != 0 {
		t.Fatalf("writes = %v", w)
	}
	if ev := r.events("refused"); len(ev) != 1 || ev[0]["command"] != "rg create" {
		t.Fatalf("refused = %v", ev)
	}
}

// ---- deploy ----

func TestDeployTouchingOneUnownedResourceIsRefusedWholeNamingIt(t *testing.T) {
	f := newFake(t)
	f.add(foreignRG, "Microsoft.Storage/storageAccounts", "theirstore", nil)
	tpl := docFile(t, "main.json", simpleTemplate)
	unowned := inRG(foreignRG, "Microsoft.Storage/storageAccounts/theirstore")
	f.whatIf = []map[string]any{
		wiChange("Create", inRG(ownedRG, "Microsoft.ManagedIdentity/userAssignedIdentities/app-id"), nil),
		wiChange("Modify", unowned, map[string]any{"properties": map[string]any{"allowSharedKeyAccess": false}}),
		wiChange("NoChange", inRG(ownedRG, "Microsoft.KeyVault/vaults/my-vault"), nil),
	}

	r := runPlugin(t, runOpts{config: apply(nil)}, "deploy "+ownedRG+" "+tpl)

	if r.ok() {
		t.Fatalf("the deployment was accepted: %s", r.text())
	}
	mustContain(t, r.text(), "none of it is deployed", unowned+" does not carry this team's tags")
	if puts := f.callsTo(http.MethodPut, ""); len(puts) != 0 {
		t.Fatalf("something was deployed: %v", puts)
	}
	if ev := r.events("refused"); len(ev) != 1 || !strings.Contains(ev[0]["reason"].(string), unowned) {
		t.Fatalf("refused = %v", ev)
	}
	pl := r.events("planned")
	if len(pl) != 1 || pl[0]["create"] != float64(1) || pl[0]["modify"] != float64(1) || pl[0]["unchanged"] != float64(1) {
		t.Fatalf("planned = %v", pl)
	}
	if refs := pl[0]["refusals"].([]any); len(refs) != 1 || !strings.Contains(refs[0].(string), unowned) {
		t.Fatalf("planned refusals = %v", refs)
	}
}

func TestDeployWithANestedDeploymentIntoAnotherGroupIsRefusedWhole(t *testing.T) {
	f := newFake(t)
	tpl := docFile(t, "nested.json", simpleTemplate)
	nested := inRG(foreignRG, "Microsoft.Resources/deployments/inner")
	inner := inRG(foreignRG, "Microsoft.ManagedIdentity/userAssignedIdentities/sneaky")
	f.whatIf = []map[string]any{
		wiChange("Create", inRG(ownedRG, "Microsoft.ManagedIdentity/userAssignedIdentities/app-id"), map[string]any{"tags": ownTags()}),
		wiChange("Create", nested, nil),
		wiChange("Create", inner, map[string]any{"tags": ownTags()}),
	}

	r := runPlugin(t, runOpts{config: apply(nil)}, "deploy "+ownedRG+" "+tpl)

	if r.ok() {
		t.Fatalf("the deployment was accepted: %s", r.text())
	}
	mustContain(t, r.text(), nested+" would be created in resource group "+foreignRG, inner+" would be created in resource group "+foreignRG)
	if w := f.writes(); len(w) != 0 {
		t.Fatalf("writes = %v", w)
	}
}

func TestDeployInApplyDeploysExactlyTheCheckedBytesAndParameters(t *testing.T) {
	f := newFake(t)
	docFile(t, "main.json", simpleTemplate)
	params := docFile(t, "main.parameters.json", `{"parameters": {"name": {"value": "app-id"}}}`)
	id := inRG(ownedRG, "Microsoft.ManagedIdentity/userAssignedIdentities/app-id")
	f.whatIf = []map[string]any{wiChange("Create", id, nil)}

	r := runPlugin(t, runOpts{config: apply(nil)}, "deploy "+ownedRG+" main.json "+params)

	if !r.ok() {
		t.Fatalf("deploy failed: %s", r.text())
	}
	mustContain(t, r.text(), "Deployed main.json into "+ownedRG, "1 created")
	wi := f.callsTo(http.MethodPost, "/whatIf")
	puts := f.callsTo(http.MethodPut, "")
	if len(wi) != 1 || len(puts) != 1 {
		t.Fatalf("what-if %d, deploy %d", len(wi), len(puts))
	}
	if !strings.HasPrefix(wi[0].Path, strings.TrimSuffix(puts[0].Path, "")) {
		t.Fatalf("the what-if %s and the deployment %s differ in name", wi[0].Path, puts[0].Path)
	}
	var checked, deployed struct {
		Properties struct {
			Mode       string          `json:"mode"`
			Template   json.RawMessage `json:"template"`
			Parameters json.RawMessage `json:"parameters"`
		} `json:"properties"`
		Tags map[string]string `json:"tags"`
	}
	_ = json.Unmarshal(wi[0].Body, &checked)
	_ = json.Unmarshal(puts[0].Body, &deployed)
	if string(checked.Properties.Template) != string(deployed.Properties.Template) || string(checked.Properties.Parameters) != string(deployed.Properties.Parameters) {
		t.Fatalf("deployed bytes differ from the checked ones:\n%s\n%s", wi[0].Body, puts[0].Body)
	}
	if deployed.Properties.Mode != "Incremental" || deployed.Tags["yawble-owner"] != testTeam {
		t.Fatalf("deployment = %s", puts[0].Body)
	}
	if !strings.Contains(string(deployed.Properties.Parameters), `"app-id"`) {
		t.Fatalf("parameters = %s", deployed.Properties.Parameters)
	}
	ev := r.events("applied")
	if len(ev) != 1 || ev[0]["resourceIds"].([]any)[0] != id {
		t.Fatalf("applied = %v", ev)
	}
}

func TestDeployCompilesBicepWithThePinnedCompiler(t *testing.T) {
	f := newFake(t)
	old := compileBicep
	var compiled string
	compileBicep = func(_ *plugin, path string) ([]byte, error) { compiled = path; return []byte(simpleTemplate), nil }
	t.Cleanup(func() { compileBicep = old })
	tpl := docFile(t, "main.bicep", "param name string\n")
	f.whatIf = []map[string]any{wiChange("Create", inRG(ownedRG, "Microsoft.ManagedIdentity/userAssignedIdentities/app-id"), nil)}

	r := runPlugin(t, runOpts{}, "deploy "+ownedRG+" "+tpl)

	if !r.ok() || compiled == "" {
		t.Fatalf("deploy failed or compiled nothing: %s", r.text())
	}
	mustContain(t, r.text(), "What-if of main.bicep into "+ownedRG, "1 to create", "nothing was deployed")
}

func TestDeployRefusesATemplateOutsideTheTeamsFolders(t *testing.T) {
	f := newFake(t)
	outside := filepath.Join(t.TempDir(), "main.json")
	_ = os.WriteFile(outside, []byte(simpleTemplate), 0o644)
	tree := t.TempDir()
	inTree := filepath.Join(tree, "infra.json")
	_ = os.WriteFile(inTree, []byte(simpleTemplate), 0o644)
	link := docFile(t, "placeholder", "")
	_ = os.Remove(link)
	_ = os.Symlink(outside, link+".json")

	for _, p := range []string{outside, link + ".json", filepath.Join(documentsRoot, testTeam, "..", "main.json")} {
		r := runPlugin(t, runOpts{trees: []string{tree}}, "deploy "+ownedRG+" "+p)
		if r.ok() {
			t.Fatalf("%s was deployed", p)
		}
		mustContain(t, r.text(), "Refused:")
	}
	if len(f.callsTo(http.MethodPost, "/whatIf")) != 0 {
		t.Fatal("a template outside the folders reached Azure")
	}
	f.whatIf = []map[string]any{}
	r := runPlugin(t, runOpts{trees: []string{tree}}, "deploy "+ownedRG+" "+inTree)
	if !r.ok() {
		t.Fatalf("a worktree template was refused: %s", r.text())
	}
}

func TestDeployRefusesTemplatesThatReadKeysOrHoldSecrets(t *testing.T) {
	f := newFake(t)
	withKeys := strings.Replace(simpleTemplate, `"resources"`, `"outputs": {"k": {"type": "string", "value": "[listKeys('x', '2023-01-01').keys[0].value]"}}, "resources"`, 1)
	tpl := docFile(t, "keys.json", withKeys)
	r := runPlugin(t, runOpts{}, "deploy "+ownedRG+" "+tpl)
	mustContain(t, r.text(), "calls listKeys", "App secrets go in Key Vault")

	plain := docFile(t, "main.json", simpleTemplate)
	params := docFile(t, "p.json", `{"parameters": {"dbPassword": {"value": "not-a-real-one"}}}`)
	r = runPlugin(t, runOpts{}, "deploy "+ownedRG+" "+plain+" "+params)
	mustContain(t, r.text(), "gives parameter dbPassword a value in the file")
	if len(f.calls) != 0 {
		t.Fatalf("a refused template reached Azure: %v", f.calls)
	}
}

func TestDeployRefusesEveryListFunctionWithNothingWritten(t *testing.T) {
	for _, call := range []string{
		"list(resourceId('Microsoft.Storage/storageAccounts', 'theirstore'), '2023-01-01').keys[0].value",
		"listAuthKeys(resourceId('Microsoft.DataFactory/factories/integrationRuntimes', 'f', 'r'), '2018-06-01').authKey1",
		"listClusterAdminCredential(resourceId('Microsoft.ContainerService/managedClusters', 'c'), '2024-01-01').kubeconfigs[0].value",
		"LIST (resourceId('Microsoft.Web/sites/config', 's', 'appsettings'), '2023-01-01').properties",
	} {
		f := newFake(t)
		f.whatIf = []map[string]any{
			wiChange("Create", inRG(ownedRG, "Microsoft.ManagedIdentity/userAssignedIdentities/app-id"), map[string]any{"tags": ownTags()}),
		}
		withList := strings.Replace(simpleTemplate, `"resources"`, `"outputs": {"k": {"type": "string", "value": "[`+call+`]"}}, "resources"`, 1)
		tpl := docFile(t, "list.json", withList)

		r := runPlugin(t, runOpts{config: apply(nil)}, "deploy "+ownedRG+" "+tpl)

		name := strings.TrimSpace(call[:strings.Index(call, "(")])
		if r.ok() {
			t.Fatalf("a template calling %s was accepted: %s", name, r.text())
		}
		mustContain(t, r.text(), "calls "+name, "App secrets go in Key Vault")
		if w := f.writes(); len(w) != 0 {
			t.Fatalf("%s: writes = %v", name, w)
		}
	}
}

// escapedL is the letter l written as a JSON escape, built at run time so no source decodes it.
var escapedL = string([]byte{92, 'u', '0', '0', '6', 'c'})

func TestDeployRefusesAListFunctionWrittenWithJSONEscapesInAValue(t *testing.T) {
	f := newFake(t)
	f.whatIf = []map[string]any{
		wiChange("Create", inRG(ownedRG, "Microsoft.ManagedIdentity/userAssignedIdentities/app-id"), map[string]any{"tags": ownTags()}),
	}
	call := escapedL + "ist" + "Keys(resourceId('Microsoft.Storage/storageAccounts', 's'), '2023-01-01').keys[0].value"
	withList := strings.Replace(simpleTemplate, `"resources"`, `"outputs": {"k": {"type": "string", "value": "[`+call+`]"}}, "resources"`, 1)
	tpl := docFile(t, "escaped.json", withList)

	r := runPlugin(t, runOpts{config: apply(nil)}, "deploy "+ownedRG+" "+tpl)

	if r.ok() {
		t.Fatalf("a template calling an escaped list function was accepted: %s", r.text())
	}
	mustContain(t, r.text(), "calls list"+"Keys", "App secrets go in Key Vault")
	if len(f.calls) != 0 {
		t.Fatalf("a refused template reached Azure: %v", f.calls)
	}
}

func TestDeployRefusesAListFunctionWithAUnicodeSpaceBeforeTheBracket(t *testing.T) {
	for name, space := range map[string]rune{
		"no-break space":      0x00A0,
		"ideographic space":   0x3000,
		"line separator":      0x2028,
		"zero-width no-break": 0xFEFF,
		"vertical tab":        0x000B,
		"next line":           0x0085,
		"zero-width space":    0x200B,
	} {
		f := newFake(t)
		f.whatIf = []map[string]any{
			wiChange("Create", inRG(ownedRG, "Microsoft.ManagedIdentity/userAssignedIdentities/app-id"), map[string]any{"tags": ownTags()}),
		}
		call := "list" + "Keys" + string(space) + "(resourceId('Microsoft.Storage/storageAccounts', 's'), '2023-01-01').keys[0].value"
		value, _ := json.Marshal("[" + call + "]")
		withList := strings.Replace(simpleTemplate, `"resources"`, `"outputs": {"k": {"type": "string", "value": `+string(value)+`}}, "resources"`, 1)
		tpl := docFile(t, "space.json", withList)

		r := runPlugin(t, runOpts{config: apply(nil)}, "deploy "+ownedRG+" "+tpl)

		if r.ok() {
			t.Fatalf("%s: a template calling a list function was accepted: %s", name, r.text())
		}
		mustContain(t, r.text(), "calls list"+"Keys", "App secrets go in Key Vault")
		if len(f.calls) != 0 {
			t.Fatalf("%s: a refused template reached Azure: %v", name, f.calls)
		}
	}
}

func TestDeployRefusesAListFunctionWrittenWithJSONEscapesInAKey(t *testing.T) {
	for name, extra := range map[string]string{
		"variables": `"variables": {"` + escapedL + `istSecrets(x)": "v"}, `,
		"outputs":   `"outputs": {"[` + escapedL + `ist(x)]": {"type": "string", "value": "v"}}, `,
	} {
		f := newFake(t)
		f.whatIf = []map[string]any{
			wiChange("Create", inRG(ownedRG, "Microsoft.ManagedIdentity/userAssignedIdentities/app-id"), map[string]any{"tags": ownTags()}),
		}
		tpl := docFile(t, "key.json", strings.Replace(simpleTemplate, `"resources"`, extra+`"resources"`, 1))

		r := runPlugin(t, runOpts{config: apply(nil)}, "deploy "+ownedRG+" "+tpl)

		if r.ok() {
			t.Fatalf("%s: a key naming an escaped list function was accepted: %s", name, r.text())
		}
		mustContain(t, r.text(), "calls list", "App secrets go in Key Vault")
		if len(f.calls) != 0 {
			t.Fatalf("%s: a refused template reached Azure: %v", name, f.calls)
		}
	}
}

func TestListFunctionInRefusesJSONThatDoesNotParse(t *testing.T) {
	for _, raw := range []string{`{"resources": [`, `{"a": "b"} {`, `{"a": "b"`} {
		if _, ok := listFunctionIn([]byte(raw)); ok {
			t.Fatalf("%q parsed", raw)
		}
	}
	if m, ok := listFunctionIn([]byte(`{"a": ["play` + escapedL + `ist(x)", "the ` + escapedL + `isting"]}`)); !ok || m != "" {
		t.Fatalf("plain words matched: %q %v", m, ok)
	}
}

func TestDeployRefusesAnIgnoredResourceOutsideWhatTheTeamOwns(t *testing.T) {
	f := newFake(t)
	f.add(foreignRG, "Microsoft.Storage/storageAccounts", "theirstore", nil)
	tpl := docFile(t, "main.json", simpleTemplate)
	ignored := inRG(foreignRG, "Microsoft.Storage/storageAccounts/theirstore")
	f.whatIf = []map[string]any{
		wiChange("Create", inRG(ownedRG, "Microsoft.ManagedIdentity/userAssignedIdentities/app-id"), map[string]any{"tags": ownTags()}),
		wiChange("Ignore", ignored, nil),
	}

	r := runPlugin(t, runOpts{config: apply(nil)}, "deploy "+ownedRG+" "+tpl)

	if r.ok() {
		t.Fatalf("the deployment was accepted: %s", r.text())
	}
	mustContain(t, r.text(), "none of it is deployed", ignored+" does not carry this team's tags")
	if w := f.writes(); len(w) != 0 {
		t.Fatalf("writes = %v", w)
	}
	if ev := r.events("refused"); len(ev) != 1 || !strings.Contains(ev[0]["reason"].(string), ignored) {
		t.Fatalf("refused = %v", ev)
	}
}

func TestDeployAllowsAnIgnoredResourceTheTeamOwns(t *testing.T) {
	f := newFake(t)
	f.add(foreignRG, "Microsoft.Storage/storageAccounts", "ourstore", ownTags())
	tpl := docFile(t, "main.json", simpleTemplate)
	f.whatIf = []map[string]any{
		wiChange("Create", inRG(ownedRG, "Microsoft.ManagedIdentity/userAssignedIdentities/app-id"), map[string]any{"tags": ownTags()}),
		wiChange("Ignore", inRG(ownedRG, "Microsoft.KeyVault/vaults/my-vault"), nil),
		wiChange("Ignore", inRG(foreignRG, "Microsoft.Storage/storageAccounts/ourstore"), nil),
	}

	r := runPlugin(t, runOpts{config: apply(nil)}, "deploy "+ownedRG+" "+tpl)

	if !r.ok() {
		t.Fatalf("the deployment was refused: %s", r.text())
	}
	if puts := f.callsTo(http.MethodPut, ""); len(puts) != 1 {
		t.Fatalf("puts = %v", puts)
	}
	if pl := r.events("planned"); len(pl) != 1 || pl[0]["unchanged"] != float64(2) {
		t.Fatalf("planned = %v", pl)
	}
}

func TestDeployRefusesSharedKeyStorageCustomRolesAndBadGrants(t *testing.T) {
	f := newFake(t)
	tpl := docFile(t, "main.json", simpleTemplate)
	store := inRG(ownedRG, "Microsoft.Storage/storageAccounts/mystore")
	scope := store + "/providers/Microsoft.Authorization/roleAssignments/00000000-0000-0000-0000-000000000002"
	userGrant := store + "/providers/Microsoft.Authorization/roleAssignments/00000000-0000-0000-0000-000000000003"
	ownerGrant := rgID(ownedRG) + "/providers/Microsoft.Authorization/roleAssignments/00000000-0000-0000-0000-000000000004"
	f.whatIf = []map[string]any{
		wiChange("Create", store, map[string]any{"properties": map[string]any{"allowSharedKeyAccess": true}}),
		wiChange("Create", scope, map[string]any{"properties": map[string]any{
			"roleDefinitionId": "/subscriptions/" + testSub + "/providers/Microsoft.Authorization/roleDefinitions/" + roleBlobDataContributor,
			"principalId":      "00000000-0000-0000-0000-0000000000ff", "principalType": "ServicePrincipal"}}),
		wiChange("Create", userGrant, map[string]any{"properties": map[string]any{
			"roleDefinitionId": "/subscriptions/" + testSub + "/providers/Microsoft.Authorization/roleDefinitions/" + roleBlobDataContributor,
			"principalId":      "[reference('x').principalId]", "principalType": "User"}}),
		wiChange("Create", ownerGrant, map[string]any{"properties": map[string]any{
			"roleDefinitionId": "/subscriptions/" + testSub + "/providers/Microsoft.Authorization/roleDefinitions/8e3af657-a8ff-443c-a75c-2fe8c4bcb635",
			"principalId":      "[reference('x').principalId]", "principalType": "ServicePrincipal"}}),
	}

	r := runPlugin(t, runOpts{}, "deploy "+ownedRG+" "+tpl)

	mustContain(t, r.text(),
		store+" does not set allowSharedKeyAccess to false",
		scope+" grants to principal 00000000-0000-0000-0000-0000000000ff, which is not a managed identity",
		userGrant+" does not set principalType to ServicePrincipal",
		ownerGrant+" grants role 8e3af657-a8ff-443c-a75c-2fe8c4bcb635, which is not one this member grants")
	if r.ok() {
		t.Fatal("the deployment was accepted")
	}
}

func TestDeployAllowsATaggedDatabaseOnASharedServerOnly(t *testing.T) {
	f := newFake(t)
	f.addRG("shared-data-rg", nil)
	f.add("shared-data-rg", "Microsoft.Sql/servers", "shared-sql", nil)
	tpl := docFile(t, "db.json", simpleTemplate)
	db := inRG("shared-data-rg", "Microsoft.Sql/servers/shared-sql/databases/appdb")
	cfg := baseConfig(map[string]any{"sharedSqlServers": []string{"shared-sql"}})

	f.whatIf = []map[string]any{wiChange("Create", db, map[string]any{"tags": ownTags()})}
	if r := runPlugin(t, runOpts{config: cfg}, "deploy "+ownedRG+" "+tpl); !r.ok() {
		t.Fatalf("a tagged database on a shared server was refused: %s", r.text())
	}

	f.whatIf = []map[string]any{wiChange("Create", db, nil)}
	r := runPlugin(t, runOpts{config: cfg}, "deploy "+ownedRG+" "+tpl)
	mustContain(t, r.text(), db+" is a new database on shared server shared-sql without this team's tags")

	f.whatIf = []map[string]any{wiChange("Modify", inRG("shared-data-rg", "Microsoft.Sql/servers/shared-sql"), nil)}
	r = runPlugin(t, runOpts{config: cfg}, "deploy "+ownedRG+" "+tpl)
	mustContain(t, r.text(), "shared-sql does not carry this team's tags")
}

func TestDeployIntoAGroupItDoesNotOwnIsRefusedBeforeTheWhatIf(t *testing.T) {
	f := newFake(t)
	tpl := docFile(t, "main.json", simpleTemplate)
	r := runPlugin(t, runOpts{}, "deploy "+foreignRG+" "+tpl)
	mustContain(t, r.text(), "resource group "+foreignRG+" does not carry this team's tags")
	if len(f.callsTo(http.MethodPost, "/whatIf")) != 0 {
		t.Fatal("a what-if ran in a group the team does not own")
	}
}

// ---- role assignments ----

func TestRoleAssignRefusesARoleOutsideTheListAUserAndAGroup(t *testing.T) {
	f := newFake(t)
	ident := f.add(ownedRG, "Microsoft.ManagedIdentity/userAssignedIdentities", "app-id", ownTags())
	ident.Props["principalId"] = "00000000-0000-0000-0000-00000000000a"
	scope := rgID(ownedRG)

	for line, want := range map[string]string{
		"role assign Owner " + ident.ID + " " + scope:                                            "Owner is not a role this member grants",
		"role assign \"User Access Administrator\" " + ident.ID + " " + scope:                    "is not a role this member grants",
		"role assign Contributor person@example.com " + scope:                                    "person@example.com is a user",
		"role assign Contributor 00000000-0000-0000-0000-00000000000b " + scope:                  "is a bare principal id, which could be a user or a group",
		"role assign Contributor " + ident.ID + " " + rgID(foreignRG):                            "resource group " + foreignRG + " does not carry this team's tags",
		"role assign Contributor " + inRG(foreignRG, "Microsoft.Web/sites/theirs") + " " + scope: "this member grants roles only to managed identities it created",
	} {
		r := runPlugin(t, runOpts{config: apply(nil)}, line)
		if r.ok() {
			t.Fatalf("%s was granted", line)
		}
		mustContain(t, r.text(), want)
		if ev := r.events("refused"); len(ev) != 1 || ev[0]["command"] != "role assign" {
			t.Fatalf("%s: refused = %v", line, ev)
		}
	}
	if w := f.writes(); len(w) != 0 {
		t.Fatalf("writes = %v", w)
	}
}

func TestRoleAssignGrantsAnOwnedIdentityAndAcrPullOnASharedRegistry(t *testing.T) {
	f := newFake(t)
	ident := f.add(ownedRG, "Microsoft.ManagedIdentity/userAssignedIdentities", "app-id", ownTags())
	ident.Props["principalId"] = "00000000-0000-0000-0000-00000000000a"
	f.addRG("shared-rg", nil)
	reg := f.add("shared-rg", "Microsoft.ContainerRegistry/registries", "sharedregistry", nil)
	cfg := apply(map[string]any{"sharedRegistries": []string{"sharedregistry"}})

	r := runPlugin(t, runOpts{config: cfg}, "role assign AcrPull "+ident.ID+" "+reg.ID)
	if !r.ok() {
		t.Fatalf("AcrPull on a shared registry was refused: %s", r.text())
	}
	puts := f.callsTo(http.MethodPut, "")
	if len(puts) != 1 || !strings.HasPrefix(puts[0].Path, reg.ID+"/providers/Microsoft.Authorization/roleAssignments/") {
		t.Fatalf("puts = %v", puts)
	}
	var body struct {
		Properties map[string]string `json:"properties"`
	}
	_ = json.Unmarshal(puts[0].Body, &body)
	if body.Properties["principalType"] != "ServicePrincipal" || body.Properties["principalId"] != "00000000-0000-0000-0000-00000000000a" ||
		!strings.HasSuffix(body.Properties["roleDefinitionId"], roleAcrPull) {
		t.Fatalf("assignment = %s", puts[0].Body)
	}

	r = runPlugin(t, runOpts{config: cfg}, "role assign Contributor "+ident.ID+" "+reg.ID)
	mustContain(t, r.text(), "sharedregistry does not carry this team's tags")
}

// ---- delete ----

func TestDeleteIsRefusedWithAllowDeleteOffInEitherMode(t *testing.T) {
	f := newFake(t)
	for _, cfg := range []map[string]any{baseConfig(nil), apply(nil)} {
		r := runPlugin(t, runOpts{config: cfg}, "delete "+ownedRG)
		if r.ok() {
			t.Fatal("a delete ran with allowDelete off")
		}
		mustContain(t, r.text(), "allowDelete is off")
		if ev := r.events("refused"); len(ev) != 1 || ev[0]["command"] != "delete" {
			t.Fatalf("refused = %v", ev)
		}
	}
	if len(f.calls) != 0 {
		t.Fatalf("a delete with allowDelete off reached Azure: %v", f.calls)
	}
}

func TestDeleteIsRefusedForAnUntaggedResource(t *testing.T) {
	f := newFake(t)
	theirs := f.add(foreignRG, "Microsoft.Storage/storageAccounts", "theirstore", map[string]string{"yawble-owner": "another-team", "yawble-plugin": "azure"})
	cfg := apply(map[string]any{"allowDelete": true})

	for _, target := range []string{theirs.ID, foreignRG} {
		r := runPlugin(t, runOpts{config: cfg}, "delete "+target)
		if r.ok() {
			t.Fatalf("%s was deleted", target)
		}
		mustContain(t, r.text(), "does not carry this team's tags")
	}
	if w := f.writes(); len(w) != 0 {
		t.Fatalf("writes = %v", w)
	}
}

func TestDeleteRemovesAnOwnedResourceInApply(t *testing.T) {
	f := newFake(t)
	mine := f.add(foreignRG, "Microsoft.Storage/storageAccounts", "mystore", ownTags())
	r := runPlugin(t, runOpts{config: apply(map[string]any{"allowDelete": true})}, "delete "+mine.ID)
	if !r.ok() {
		t.Fatalf("delete failed: %s", r.text())
	}
	if d := f.callsTo(http.MethodDelete, ""); len(d) != 1 || d[0].Path != mine.ID {
		t.Fatalf("deletes = %v", d)
	}
	if ev := r.events("applied"); len(ev) != 1 {
		t.Fatalf("applied = %v", ev)
	}
}

// ---- dns ----

func TestDnsZoneAndRecordInApply(t *testing.T) {
	f := newFake(t)
	r := runPlugin(t, runOpts{config: apply(nil)}, "dns zone create "+ownedRG+" example.com")
	mustContain(t, r.text(), "DNS zone example.com is ready", "ns1-01.azure-dns.example")
	zone := f.resources[strings.ToLower(inRG(ownedRG, "Microsoft.Network/dnsZones/example.com"))]
	if zone == nil || zone.Tags["yawble-owner"] != testTeam {
		t.Fatalf("zone = %+v", zone)
	}

	r = runPlugin(t, runOpts{config: apply(nil)}, "dns record set example.com www A 192.0.2.10 ttl=300")
	if !r.ok() {
		t.Fatalf("record set failed: %s", r.text())
	}
	puts := f.callsTo(http.MethodPut, "/A/www")
	if len(puts) != 1 || !strings.Contains(string(puts[0].Body), `"ipv4Address":"192.0.2.10"`) || !strings.Contains(string(puts[0].Body), `"TTL":300`) {
		t.Fatalf("record puts = %v", puts)
	}
}

func TestDnsRecordInAZoneItDoesNotOwnIsRefused(t *testing.T) {
	f := newFake(t)
	f.add(foreignRG, "Microsoft.Network/dnsZones", "example.net", nil)
	r := runPlugin(t, runOpts{config: apply(nil)}, "dns record set example.net www CNAME app.example.com")
	if r.ok() {
		t.Fatal("a record was set in someone else's zone")
	}
	mustContain(t, r.text(), "does not carry this team's tags")
	if w := f.writes(); len(w) != 0 {
		t.Fatalf("writes = %v", w)
	}
}

// ---- reads ----

func TestReadsWorkAnywhere(t *testing.T) {
	f := newFake(t)
	f.add(foreignRG, "Microsoft.Storage/storageAccounts", "theirstore", nil).Props["primaryKey"] = "pretend-value"
	r := runPlugin(t, runOpts{}, "list")
	mustContain(t, r.text(), ownedRG+" (Microsoft.Resources/resourceGroups, westeurope) [owned]", foreignRG)
	r = runPlugin(t, runOpts{}, "list "+foreignRG)
	mustContain(t, r.text(), "theirstore (Microsoft.Storage/storageAccounts")
	r = runPlugin(t, runOpts{}, "show "+inRG(foreignRG, "Microsoft.Storage/storageAccounts/theirstore"))
	mustContain(t, r.text(), "not owned by this team (read only)", `"primaryKey": "[hidden]"`)
	if strings.Contains(r.raw, "pretend-value") {
		t.Fatal("show printed a key's value")
	}
	r = runPlugin(t, runOpts{config: baseConfig(map[string]any{"sharedSqlServers": []string{"my-sql-server"}})}, "whoami")
	mustContain(t, r.text(), "- account: person@example.com", "My Subscription ("+testSub+", Enabled)", "- mode: whatif", "- allowDelete: off", "- sharedSqlServers: my-sql-server")
	if w := f.writes(); len(w) != 0 {
		t.Fatalf("reads wrote: %v", w)
	}
}

// ---- tokens ----

func TestEachAPIGetsOnlyItsOwnToken(t *testing.T) {
	f := newFake(t)
	var seen []string
	f.hosts["sql.example.test"] = func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		w.WriteHeader(200)
	}
	p := &plugin{az: &client{tokens: map[api]string{apiARM: tokARM, apiSQL: tokSQL}}}
	if _, err := p.az.send(apiSQL, http.MethodGet, "https://sql.example.test/x", nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0] != tokSQL {
		t.Fatal("the SQL request did not carry the SQL token")
	}
	if _, err := p.az.send(apiStorage, http.MethodGet, "https://acct.blob.example.test/x", nil, nil); err == nil || !strings.Contains(err.Error(), "no token for Azure Storage") {
		t.Fatalf("a request with no token for its API = %v", err)
	}
	runPlugin(t, runOpts{}, "list")
	for _, c := range f.calls {
		if c.Host == armHost && c.Token != tokARM {
			t.Fatal("an Azure management request carried another API's token")
		}
	}
}

func TestATokenAzureEchoesNeverReachesOutputEventsOrProgress(t *testing.T) {
	f := newFake(t)
	f.echoToken = true
	r := runPlugin(t, runOpts{}, "whoami") // runPlugin scans every line for every token
	mustContain(t, r.text(), "[redacted]")

	// A refused command's event and every progress row, for each fake token.
	r = runPlugin(t, runOpts{}, "setup")
	for _, row := range r.progressRows() {
		assertNoToken(t, row)
	}
	p := &plugin{}
	for _, tok := range allTokens {
		p.hide(tok)
	}
	for _, tok := range allTokens {
		for _, text := range []string{tok, strings.ToUpper(tok), "AccountKey=" + strings.Repeat("k", 30) + ";", "Password=" + strings.Repeat("p", 12)} {
			got := p.scrub("before " + text + " after")
			assertNoToken(t, got)
			if strings.Contains(got, strings.Repeat("k", 30)) || strings.Contains(got, strings.Repeat("p", 12)) {
				t.Fatalf("scrub kept a credential: %s", got)
			}
		}
	}
}

// ---- dispatcher and settings ----

func TestAnUnknownCommandListsTheCommands(t *testing.T) {
	newFake(t)
	r := runPlugin(t, runOpts{}, "launch rockets")
	mustContain(t, r.text(), `"launch" is not a command`, "- deploy <resource group>", "- rg create <name> <region>", "- setup")
	if ev := r.events("refused"); len(ev) != 1 || ev[0]["command"] != "launch" {
		t.Fatalf("refused = %v", ev)
	}
}

func TestSeveralLinesRunInOrder(t *testing.T) {
	newFake(t)
	r := runPlugin(t, runOpts{}, "whoami\nlist")
	mustContain(t, r.text(), "1. Azure member:", "2. Resource groups in")
}

func TestBadSettingsAreRefusedInWords(t *testing.T) {
	newFake(t)
	for cfg, want := range map[string]map[string]any{
		"subscription must be a subscription id": {"subscription": "my subscription"},
		"mode must be whatif or apply":           {"mode": "yolo"},
		"is not a SQL server name":               {"sharedSqlServers": []string{"bad name!"}},
		"is not an email address":                {"budgetEmails": []string{"nobody"}},
	} {
		r := runPlugin(t, runOpts{config: want}, "whoami")
		if r.ok() {
			t.Fatalf("%v was accepted", want)
		}
		mustContain(t, r.text(), cfg)
	}
}

func TestParseIDReadsExtensionsAndChildren(t *testing.T) {
	r, ok := parseID(inRG(ownedRG, "Microsoft.Sql/servers/s1/databases/d1"))
	if !ok || r.typ() != "Microsoft.Sql/servers/databases" || r.name() != "d1" || r.parentID() != inRG(ownedRG, "Microsoft.Sql/servers/s1") {
		t.Fatalf("child = %+v", r)
	}
	ext := inRG(ownedRG, "Microsoft.Storage/storageAccounts/a") + "/providers/Microsoft.Authorization/roleAssignments/x"
	r, ok = parseID(ext)
	if !ok || r.scope != inRG(ownedRG, "Microsoft.Storage/storageAccounts/a") || r.ext == nil {
		t.Fatalf("extension = %+v", r)
	}
	r, ok = parseID(rgID(ownedRG) + "/providers/Microsoft.Authorization/roleAssignments/x")
	if !ok || r.scope != rgID(ownedRG) {
		t.Fatalf("group extension = %+v", r)
	}
	if _, ok := parseID("not-an-id"); ok {
		t.Fatal("a name parsed as an id")
	}
	_ = fmt.Sprint()
}
