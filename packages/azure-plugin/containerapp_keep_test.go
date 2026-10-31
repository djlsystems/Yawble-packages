package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// containerapp create on an app that exists updates it, keeping what the command does not set.

// boundApp gives the world's my-app two custom domains, an IP restriction, a queue scale rule
// reading a Key Vault secret, and a Key Vault secret nothing refers to any more.
func boundApp(w *world) (www, api string) {
	app := w.f.resources[strings.ToLower(inRG(ownedRG, "Microsoft.App/containerApps/my-app"))]
	env := inRG(ownedRG, "Microsoft.App/managedEnvironments/my-app-env")
	www, api = env+"/managedCertificates/www-cert", env+"/managedCertificates/api-cert"
	cfg := app.Props["configuration"].(map[string]any)
	cfg["ingress"] = map[string]any{
		"external": true, "targetPort": 8080, "fqdn": "my-app.apps.example.com",
		"customDomains": []any{
			map[string]any{"name": "www.example.com", "bindingType": "SniEnabled", "certificateId": www},
			map[string]any{"name": "api.example.com", "bindingType": "SniEnabled", "certificateId": api},
		},
		"ipSecurityRestrictions": []any{map[string]any{"name": "office", "ipAddressRange": "192.0.2.0/24", "action": "Allow"}},
	}
	cfg["secrets"] = []any{
		map[string]any{"name": "queue-conn", "keyVaultUrl": "https://myvault" + ".vault.azure.net/secrets/queue-conn", "identity": inRG(ownedRG, "Microsoft.ManagedIdentity/userAssignedIdentities/my-app-id")},
		map[string]any{"name": "old-key", "keyVaultUrl": "https://myvault" + ".vault.azure.net/secrets/old-key", "identity": inRG(ownedRG, "Microsoft.ManagedIdentity/userAssignedIdentities/my-app-id")},
	}
	app.Props["template"].(map[string]any)["scale"] = map[string]any{
		"minReplicas": 1, "maxReplicas": 3,
		"rules": []any{map[string]any{"name": "jobs", "azureQueue": map[string]any{"queueName": "jobs", "queueLength": 5,
			"auth": []any{map[string]any{"secretRef": "queue-conn", "triggerParameter": "connection"}}}}},
	}
	return www, api
}

func appAgain(w *world) string {
	return "containerapp create " + ownedRG + " my-app my-app-env " + acrRef("myregistry", "my-app:1.1") + " port=8080 secret=DB_PASSWORD=" + w.vaultURL
}

func TestContainerAppCreateAgainKeepsTheAppsCustomDomains(t *testing.T) {
	w := newWorld(t)
	www, api := boundApp(w)
	r := runPlugin(t, runOpts{config: apply(nil)}, appAgain(w))
	if !r.ok() {
		t.Fatal(r.text())
	}
	mustContain(t, r.text(), "Container app my-app existed and was updated", "Custom domains still bound: www.example.com, api.example.com.")
	if strings.Contains(r.text(), "no longer bound") {
		t.Fatalf("the answer says a domain was lost:\n%s", r.text())
	}

	put := w.f.callsTo(http.MethodPut, "/containerApps/my-app")
	if len(put) != 1 {
		t.Fatalf("%d PUTs of the app", len(put))
	}
	var body struct {
		Properties struct {
			Configuration struct {
				Ingress struct {
					TargetPort    int `json:"targetPort"`
					CustomDomains []struct {
						Name, BindingType, CertificateID string
					} `json:"customDomains"`
					IPSecurityRestrictions []map[string]any `json:"ipSecurityRestrictions"`
				} `json:"ingress"`
				Secrets []map[string]any `json:"secrets"`
			} `json:"configuration"`
			Template struct {
				Scale struct {
					MinReplicas int              `json:"minReplicas"`
					MaxReplicas int              `json:"maxReplicas"`
					Rules       []map[string]any `json:"rules"`
				} `json:"scale"`
			} `json:"template"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(put[0].Body, &body); err != nil {
		t.Fatal(err)
	}
	ing := body.Properties.Configuration.Ingress
	if len(ing.CustomDomains) != 2 ||
		ing.CustomDomains[0].Name != "www.example.com" || ing.CustomDomains[0].BindingType != "SniEnabled" || ing.CustomDomains[0].CertificateID != www ||
		ing.CustomDomains[1].Name != "api.example.com" || ing.CustomDomains[1].BindingType != "SniEnabled" || ing.CustomDomains[1].CertificateID != api {
		t.Fatalf("the PUT's custom domains = %+v", ing.CustomDomains)
	}
	if ing.TargetPort != 8080 || len(ing.IPSecurityRestrictions) != 1 || ing.IPSecurityRestrictions[0]["name"] != "office" {
		t.Fatalf("the PUT's ingress lost its settings: %s", put[0].Body)
	}
	scale := body.Properties.Template.Scale
	if scale.MinReplicas != 0 || scale.MaxReplicas != 1 || len(scale.Rules) != 1 || scale.Rules[0]["name"] != "jobs" {
		t.Fatalf("the PUT's scale = %+v", scale)
	}
	var secrets []string
	for _, s := range body.Properties.Configuration.Secrets {
		secrets = append(secrets, s["name"].(string))
	}
	// db-password is the new secret, queue-conn is kept for the scale rule reading it, and old-key,
	// which nothing names or reads any more, is removed.
	if strings.Join(secrets, ",") != "db-password,queue-conn" {
		t.Fatalf("the PUT's secrets = %v", secrets)
	}
	applied := r.events("applied")
	if len(applied) != 1 || !strings.Contains(applied[0]["summary"].(string), "update container app my-app (exists)") ||
		!strings.Contains(applied[0]["summary"].(string), "Custom domains still bound: www.example.com, api.example.com.") {
		t.Fatalf("applied = %v", applied)
	}
}

func TestContainerAppCreateAgainSaysWhatItKeepsInWhatif(t *testing.T) {
	w := newWorld(t)
	boundApp(w)
	r := runPlugin(t, runOpts{}, appAgain(w))
	if !r.ok() {
		t.Fatal(r.text())
	}
	mustContain(t, r.text(), "update container app my-app (exists)", "keeps its custom domains www.example.com, api.example.com", "ingress ipSecurityRestrictions", "scale rules")
	if strings.Contains(r.text(), "create container app my-app") {
		t.Fatalf("the plan says an existing app is created:\n%s", r.text())
	}
	if len(w.f.writes()) != 0 {
		t.Fatalf("whatif wrote: %v", w.f.writes())
	}
}

func TestContainerAppCreateAgainNamesADomainAzureNoLongerBinds(t *testing.T) {
	w := newWorld(t)
	boundApp(w)
	w.f.afterPut = func(r *fakeResource) {
		if !strings.EqualFold(r.Type, "Microsoft.App/containerApps") {
			return
		}
		ing := r.Props["configuration"].(map[string]any)["ingress"].(map[string]any)
		var keep []any
		for _, d := range anyList(ing["customDomains"]) {
			if d["name"] != "api.example.com" {
				keep = append(keep, d)
			}
		}
		ing["customDomains"] = keep
	}
	r := runPlugin(t, runOpts{config: apply(nil)}, appAgain(w))
	if !r.ok() {
		t.Fatal(r.text())
	}
	mustContain(t, r.text(), "Custom domains still bound: www.example.com.", "api.example.com is no longer bound; run domain bind my-app api.example.com again.")
}

func TestContainerAppCreatePublishesCreatedThenChanged(t *testing.T) {
	w := newWorld(t)
	line := "containerapp create " + ownedRG + " new-app my-app-env " + acrRef("myregistry", "my-app:1.0") + " port=8080 secret=DB_PASSWORD=" + w.vaultURL
	events := func() []string {
		r := runPlugin(t, runOpts{config: apply(nil)}, line)
		if !r.ok() {
			t.Fatal(r.text())
		}
		var got []string
		for _, g := range resourcesOf(r) {
			f := strings.Fields(g)
			got = append(got, f[0]+" "+f[2])
		}
		return got
	}
	if got := strings.Join(events(), "; "); got != "managed-identity created; role-assignment created; role-assignment created; container-app created" {
		t.Fatalf("a new app's resource events = %s", got)
	}
	if got := strings.Join(events(), "; "); got != "managed-identity changed; role-assignment changed; role-assignment changed; container-app changed" {
		t.Fatalf("an existing app's resource events = %s", got)
	}
}

func TestContainerAppCreateOnAnExistingAppOfAnotherTeamIsRefused(t *testing.T) {
	w := newWorld(t)
	w.f.add(ownedRG, "Microsoft.App/containerApps", "stray-app", map[string]string{"cost-center": "shared"})
	r := runPlugin(t, runOpts{config: apply(nil)}, "containerapp create "+ownedRG+" stray-app my-app-env "+acrRef("myregistry", "my-app:1.0"))
	if r.ok() {
		t.Fatal("an app without the team's tags was changed")
	}
	mustContain(t, r.text(), "container app stray-app already exists", "does not carry this team's tags")
	if len(w.f.writes()) != 0 {
		t.Fatalf("a refusal wrote: %v", w.f.writes())
	}
}

func TestContainerAppCreateRefusesToDropAValueSecretAKeptSettingReads(t *testing.T) {
	w := newWorld(t)
	boundApp(w)
	app := w.f.resources[strings.ToLower(inRG(ownedRG, "Microsoft.App/containerApps/my-app"))]
	app.Props["configuration"].(map[string]any)["secrets"] = []any{map[string]any{"name": "queue-conn"}} // Azure never answers the value
	r := runPlugin(t, runOpts{config: apply(nil)}, appAgain(w))
	if r.ok() {
		t.Fatal("a value secret a scale rule reads was dropped")
	}
	mustContain(t, r.text(), "its secret queue-conn", "QUEUE_CONN", "Nothing was changed")
	if len(w.f.callsTo(http.MethodPut, "/containerApps/my-app")) != 0 {
		t.Fatal("the app was PUT")
	}

	value := "q" + strings.Repeat("v", 12)
	r = runPlugin(t, runOpts{config: apply(nil), moved: map[string]string{"appSecret2": value}}, appAgain(w)+" secret=QUEUE_CONN=appSecret2")
	if !r.ok() {
		t.Fatal(r.text())
	}
}
