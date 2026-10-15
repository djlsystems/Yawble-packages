package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
)

// TestTheResourceEventCarriesTheMonitorHint reads the resource event's fields and its hint as the
// Monitor reads them: icon is the fallback, iconBy maps each resourceType to its icon, then the action and the label.
func TestTheResourceEventCarriesTheMonitorHint(t *testing.T) {
	raw, err := os.ReadFile("plugin.json")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Events struct {
			Publishes []struct {
				Type       string `json:"type"`
				HighVolume bool   `json:"highVolume"`
				Fields     []struct {
					Name string `json:"name"`
					Kind string `json:"kind"`
				} `json:"fields"`
				Monitor *struct {
					Icon   string `json:"icon"`
					IconBy struct {
						Field string            `json:"field"`
						Icons map[string]string `json:"icons"`
					} `json:"iconBy"`
					ActionField string `json:"actionField"`
					Label       string `json:"label"`
				} `json:"monitor"`
			} `json:"publishes"`
		} `json:"events"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range m.Events.Publishes {
		if e.Type != "resource" {
			if e.Monitor != nil {
				t.Fatalf("%s carries a Monitor hint; only resource does", e.Type)
			}
			continue
		}
		found = true
		if !e.HighVolume {
			t.Fatal("resource is not highVolume")
		}
		fields := map[string]bool{}
		for _, f := range e.Fields {
			if f.Kind != "string" {
				t.Fatalf("field %s is %s", f.Name, f.Kind)
			}
			fields[f.Name] = true
		}
		h := e.Monitor
		if h == nil {
			t.Fatal("resource has no Monitor hint")
		}
		if h.Icon != "cloud" || h.IconBy.Field != "resourceType" || h.ActionField != "action" || h.Label != "{name}" {
			t.Fatalf("hint = %+v", *h)
		}
		if !fields[h.IconBy.Field] || !fields[h.ActionField] || !fields[strings.Trim(h.Label, "{}")] {
			t.Fatalf("the hint names a field the event does not declare: %+v", *h)
		}
		kinds := map[string]bool{"other": true}
		for _, k := range resourceKinds {
			kinds[k] = true
		}
		for k := range kinds {
			if h.IconBy.Icons[k] == "" {
				t.Fatalf("no icon for %s", k)
			}
		}
		if len(h.IconBy.Icons) != len(kinds) {
			t.Fatalf("icons for kinds the plugin does not name: %v", h.IconBy.Icons)
		}
		for kind, icon := range map[string]string{
			"resource-group": "folder", "sql-server": "dns", "sql-database": "database", "container-app": "deployed_code",
			"container-registry": "inventory_2", "key-vault": "key", "secret": "key", "managed-identity": "badge",
			"role-assignment": "admin_panel_settings", "container-app-environment": "lan", "storage-account": "hard_drive",
			"log-analytics": "monitoring", "budget": "savings", "dns-zone": "language", "other": "cloud",
		} {
			if h.IconBy.Icons[kind] != icon {
				t.Fatalf("%s's icon is %q, want %q", kind, h.IconBy.Icons[kind], icon)
			}
		}
	}
	if !found {
		t.Fatal("no resource event declared")
	}
}

func TestResourceKindIsReadFromTheIdAlone(t *testing.T) {
	sub := "/subscriptions/" + testSub
	rg := sub + "/resourceGroups/web"
	for id, want := range map[string][2]string{
		rg: {"resource-group", "web"},
		rg + "/providers/Microsoft.Sql/servers/s1":                                                                           {"sql-server", "s1"},
		rg + "/providers/Microsoft.Sql/servers/s1/databases/db1":                                                             {"sql-database", "db1"},
		rg + "/providers/Microsoft.App/containerApps/app1":                                                                   {"container-app", "app1"},
		rg + "/providers/Microsoft.App/managedEnvironments/env1":                                                             {"container-app-environment", "env1"},
		rg + "/providers/Microsoft.ContainerRegistry/registries/reg1":                                                        {"container-registry", "reg1"},
		rg + "/providers/Microsoft.KeyVault/vaults/kv1":                                                                      {"key-vault", "kv1"},
		rg + "/providers/Microsoft.KeyVault/vaults/kv1/secrets/db-password":                                                  {"secret", "db-password"},
		rg + "/providers/Microsoft.Storage/storageAccounts/st1":                                                              {"storage-account", "st1"},
		rg + "/providers/Microsoft.ManagedIdentity/userAssignedIdentities/id1":                                               {"managed-identity", "id1"},
		rg + "/providers/Microsoft.Storage/storageAccounts/st1/providers/Microsoft.Authorization/roleAssignments/ra1":        {"role-assignment", "ra1"},
		rg + "/providers/Microsoft.Authorization/roleAssignments/ra2":                                                        {"role-assignment", "ra2"},
		rg + "/providers/Microsoft.OperationalInsights/workspaces/logs1":                                                     {"log-analytics", "logs1"},
		rg + "/providers/Microsoft.Consumption/budgets/yawble-monthly":                                                       {"budget", "yawble-monthly"},
		rg + "/providers/Microsoft.Network/dnsZones/example.org":                                                             {"dns-zone", "example.org"},
		rg + "/providers/Microsoft.Network/dnsZones/example.org/CNAME/www":                                                   {"other", "www"},
		rg + "/providers/Microsoft.App/managedEnvironments/env1/providers/Microsoft.Insights/diagnosticSettings/yawble-logs": {"other", "yawble-logs"},
		rg + "/providers/Microsoft.Web/sites/site1/":                                                                         {"other", "site1"},
		"not-an-id/thing": {"other", "thing"},
	} {
		if kind, name := resourceKind(id); kind != want[0] || name != want[1] {
			t.Errorf("%s: %s %s, want %s %s", id, kind, name, want[0], want[1])
		}
	}
}

// resourcesOf reads each resource event as kind name action.
func resourcesOf(r *result) []string {
	var out []string
	for _, e := range r.events("resource") {
		out = append(out, fmt.Sprintf("%s %s %s %s", e["resourceType"], e["name"], e["action"], e["command"]))
	}
	return out
}

func TestApplyPublishesOneResourceEventPerResourceBesideTheSummary(t *testing.T) {
	newFake(t)
	cfg := apply(map[string]any{"monthlyBudget": 100, "budgetEmails": []string{"person@example.com"}})
	r := runPlugin(t, runOpts{config: cfg}, "rg create web westeurope")
	if !r.ok() {
		t.Fatal(r.text())
	}
	if ev := r.events("applied"); len(ev) != 1 {
		t.Fatalf("applied = %v", ev)
	}
	got := resourcesOf(r)
	if strings.Join(got, "; ") != "resource-group web created rg create; budget yawble-monthly created rg create" {
		t.Fatalf("resource events = %v", got)
	}
	ev := r.events("resource")[0]
	if ev["resourceId"] != rgID("web") {
		t.Fatalf("resourceId = %v", ev["resourceId"])
	}

	r = runPlugin(t, runOpts{config: apply(map[string]any{"allowDelete": true})}, "delete "+rgID("web"))
	if !r.ok() {
		t.Fatal(r.text())
	}
	if got := resourcesOf(r); strings.Join(got, "; ") != "resource-group web deleted delete" {
		t.Fatalf("resource events of delete = %v", got)
	}
}

func TestWhatifReadsAndRefusalsPublishNoResourceEvent(t *testing.T) {
	f := newFake(t)
	f.add(ownedRG, "Microsoft.Storage/storageAccounts", "mystore", ownTags())
	for _, run := range []struct {
		cfg  map[string]any
		line string
	}{
		{baseConfig(nil), "rg create web westeurope"},
		{apply(nil), "rg create web eastus2"},
		{apply(nil), "rg create " + foreignRG + " westeurope"},
		{apply(nil), "delete " + ownedRG},
		{apply(nil), "storage list"},
		{apply(nil), "rg list"},
	} {
		r := runPlugin(t, runOpts{config: run.cfg}, run.line)
		if ev := r.events("resource"); len(ev) != 0 {
			t.Fatalf("%s: resource events %v", run.line, ev)
		}
	}
}

func TestADeploymentPublishesAtMost25ResourceEvents(t *testing.T) {
	f := newFake(t)
	var changes []map[string]any
	for i := 0; i < 30; i++ {
		changes = append(changes, wiChange("Create", inRG(ownedRG, fmt.Sprintf("Microsoft.ManagedIdentity/userAssignedIdentities/id-%02d", i)), nil))
	}
	f.whatIf = changes
	tpl := docFile(t, "main.json", simpleTemplate)
	r := runPlugin(t, runOpts{config: apply(nil)}, "deploy "+ownedRG+" "+tpl)
	if !r.ok() {
		t.Fatal(r.text())
	}
	applied := r.events("applied")
	if len(applied) != 1 || len(applied[0]["resourceIds"].([]any)) != 30 {
		t.Fatalf("applied = %v", applied)
	}
	got := resourcesOf(r)
	if len(got) != 25 || got[0] != "managed-identity id-00 created deploy" || got[24] != "managed-identity id-24 created deploy" {
		t.Fatalf("resource events = %d: %v", len(got), got)
	}
}

func TestADeploymentNamesEachResourceByItsIdAndNoValue(t *testing.T) {
	f := newFake(t)
	value := "kv-" + strings.Repeat("Z9", 10) + "-value"
	vault := inRG(ownedRG, "Microsoft.KeyVault/vaults/kv1")
	f.whatIf = []map[string]any{
		wiChange("Create", vault+"/secrets/db-password", map[string]any{"properties": map[string]any{"value": value}}),
		wiChange("Modify", vault, nil),
		wiChange("NoChange", inRG(ownedRG, "Microsoft.ManagedIdentity/userAssignedIdentities/same"), nil),
		wiChange("Create", inRG(ownedRG, "Microsoft.Web/sites/site1"), nil),
	}
	tpl := docFile(t, "main.json", simpleTemplate)
	r := runPlugin(t, runOpts{config: apply(nil)}, "deploy "+ownedRG+" "+tpl)
	if !r.ok() {
		t.Fatal(r.text())
	}
	got := strings.Join(resourcesOf(r), "; ")
	if got != "secret db-password created deploy; key-vault kv1 changed deploy; other site1 created deploy" {
		t.Fatalf("resource events = %s", got)
	}
	for _, e := range r.events("resource") {
		raw, _ := json.Marshal(e)
		if strings.Contains(string(raw), value) {
			t.Fatalf("a resource event holds the secret's value: %s", raw)
		}
	}
}

func TestAContainerAppsResourceEventsHoldNoSecret(t *testing.T) {
	w := newWorld(t)
	line := "containerapp create " + ownedRG + " new-app my-app-env " + acrRef("myregistry", "my-app:1.0") + " port=8080 secret=STRIPE_KEY=appSecret1 secret=DB_PASSWORD=" + w.vaultURL
	r := runPlugin(t, runOpts{config: apply(nil), secrets: map[string]string{"appSecret1": secretValue}, secretKeys: map[string]string{"appSecret1": "SHOP_STRIPE_KEY"}}, line)
	if !r.ok() {
		t.Fatal(r.text())
	}
	got := resourcesOf(r)
	if len(got) != 4 || got[0] != "managed-identity new-app-id created containerapp create" ||
		!strings.HasPrefix(got[1], "role-assignment ") || !strings.HasPrefix(got[2], "role-assignment ") ||
		got[3] != "container-app new-app created containerapp create" {
		t.Fatalf("resource events = %v", got)
	}
	for _, e := range r.events("resource") {
		raw, _ := json.Marshal(e)
		for _, bad := range []string{secretValue, w.vaultURL, "SHOP_STRIPE_KEY", "DB_PASSWORD"} {
			if strings.Contains(string(raw), bad) {
				t.Fatalf("a resource event holds %q: %s", bad, raw)
			}
		}
	}
}

// TestEachChangingCommandNamesWhatItTouched runs every changing command in apply mode: each resource
// it creates, changes or deletes is one event, and a data write (a blob, an entity, a message, SQL,
// an image) or a restart publishes its summary alone.
func TestEachChangingCommandNamesWhatItTouched(t *testing.T) {
	w := newWorld(t)
	cfg := apply(map[string]any{"allowDelete": true})
	ra := "role-assignment * created "
	want := map[string]string{
		"storage create":           "storage-account newstore created storage create; " + ra + "storage create; " + ra + "storage create; " + ra + "storage create",
		"storage container create": "other files created storage container create",
		"storage table create":     "other orders created storage table create",
		"storage queue create":     "other jobs created storage queue create",
		"keyvault create":          "key-vault newvault created keyvault create",
		"acr create":               "container-registry newregistry created acr create",
		"sql server create":        "sql-server new-sql created sql server create",
		"sql db create":            "sql-database new-db created sql db create",
		"containerapp env create":  "log-analytics new-env-logs created containerapp env create; container-app-environment new-env created containerapp env create; other yawble-logs created containerapp env create",
		"containerapp create":      "managed-identity new-app-id created containerapp create; " + ra + "containerapp create; " + ra + "containerapp create; container-app new-app created containerapp create",
		"domain bind":              "other asuid.www changed domain bind; other www changed domain bind",
	}
	for _, line := range w.newCommands() {
		r := runPlugin(t, runOpts{config: cfg}, line)
		if !r.ok() {
			t.Fatalf("%s: %s", line, r.text())
		}
		var got []string
		for _, g := range resourcesOf(r) {
			if f := strings.Fields(g); f[0] == "role-assignment" {
				f[1] = "*"
				g = strings.Join(f, " ")
			}
			got = append(got, g)
		}
		command := ""
		for c := range want {
			if strings.HasPrefix(line, c+" ") && len(c) > len(command) {
				command = c
			}
		}
		if strings.Join(got, "; ") != want[command] {
			t.Fatalf("%s: resource events %v, want %s", line, got, want[command])
		}
		if len(got) > 0 && len(r.events("applied")) == 0 {
			t.Fatalf("%s: resource events without the applied summary", line)
		}
	}
}
