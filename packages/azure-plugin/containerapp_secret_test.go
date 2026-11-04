package main

import (
	"encoding/json"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// containerapp secret set: secrets from the team's connections onto an app that exists, such as one
// Bicep deployed with 0 to 2 replicas and a liveness probe, changing nothing else of it.

// bicepApp makes the world's my-app what a Bicep deploy leaves: scale 0 to 2 with an HTTP rule, a
// liveness probe, a registry pulled with its identity, ingress with a custom domain, a sidecar,
// a value secret (keptValue) and a Key Vault secret, and env reading both and one plain value.
var keptValue = "kept-" + strings.Repeat("J8", 12) + "-value"

func bicepApp(w *world) *fakeResource {
	app := w.f.resources[strings.ToLower(inRG(ownedRG, "Microsoft.App/containerApps/my-app"))]
	identity := inRG(ownedRG, "Microsoft.ManagedIdentity/userAssignedIdentities/my-app-id")
	app.Props["configuration"] = map[string]any{
		"activeRevisionsMode": "Single",
		"ingress": map[string]any{"external": true, "targetPort": 8080, "fqdn": "my-app.apps.example.com",
			"customDomains": []any{map[string]any{"name": "www.example.com", "bindingType": "SniEnabled", "certificateId": "cert-1"}}},
		"registries": []any{map[string]any{"server": registryHost("myregistry"), "identity": identity}},
		"secrets": []any{
			map[string]any{"name": "db-conn", "value": keptValue},
			map[string]any{"name": "queue-conn", "keyVaultUrl": "https://myvault" + ".vault.azure.net/secrets/queue-conn", "identity": identity},
		},
	}
	app.Props["template"] = map[string]any{
		"containers": []any{
			map[string]any{
				"name": "my-app", "image": acrRef("myregistry", "my-app:2.4"),
				"resources": map[string]any{"cpu": 0.5, "memory": "1Gi"},
				"env": []any{
					map[string]any{"name": "PLAIN", "value": "hello"},
					map[string]any{"name": "DB_CONN", "secretRef": "db-conn"},
					map[string]any{"name": "QUEUE_CONN", "secretRef": "queue-conn"},
				},
				"probes": []any{map[string]any{"type": "Liveness", "httpGet": map[string]any{"path": "/healthz", "port": 8080}, "periodSeconds": 10}},
			},
			map[string]any{"name": "log-shipper", "image": acrRef("myregistry", "shipper:1.0"), "env": []any{map[string]any{"name": "LEVEL", "value": "info"}}},
		},
		"scale": map[string]any{"minReplicas": 0, "maxReplicas": 2,
			"rules": []any{map[string]any{"name": "http", "http": map[string]any{"metadata": map[string]any{"concurrentRequests": "50"}}}}},
	}
	w.f.allowListSecrets = true
	return app
}

// snapshot is a deep copy of a resource's properties, as JSON decodes them.
func snapshot(t *testing.T, props map[string]any) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(mustJSON(t, props), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func sub(m map[string]any, path ...string) any {
	var v any = m
	for _, k := range path {
		mm, _ := v.(map[string]any)
		v = mm[k]
	}
	return v
}

const setLine = `containerapp secret set my-app secret=SMTP_PASSWORD=mail.password "secret=STRIPE_KEY=stripe live.key"`

func setBound() []hostObject {
	return []hostObject{mailbox("mail", "bot@example.test", secretValue), apiKey("stripe live", "Stripe", "stripe", map[string]string{"key": keyValue})}
}

// assertNoSetValue fails when any value this card's tests use was written out.
func assertNoSetValue(t *testing.T, raw string) {
	t.Helper()
	assertNoNamedValue(t, raw)
	if strings.Contains(strings.ToLower(raw), strings.ToLower(keptValue)) || strings.Contains(raw, strings.Repeat("J8", 12)) {
		t.Fatalf("a kept secret's value was written out:\n%s", raw)
	}
}

func TestSecretSetKeepsScaleProbesImageIngressAndTheOtherSecretsAndEnv(t *testing.T) {
	w := newWorld(t)
	app := bicepApp(w)
	before := snapshot(t, app.Props)

	r := runPlugin(t, runOpts{config: apply(nil), bound: setBound()}, setLine)
	if !r.ok() {
		t.Fatal(r.text())
	}
	assertNoSetValue(t, r.raw)
	mustContain(t, r.text(), "Container app my-app now reads SMTP_PASSWORD from mail.password as secret smtp-password, STRIPE_KEY from stripe live.key as secret stripe-key",
		"Key Vault secrets queue-conn", "secrets db-conn", "scale, probes, ingress, image, registries and identity are as they were")

	// One PATCH of the app, touching configuration.secrets and template's containers and scale only; no PUT.
	if puts := w.f.callsTo(http.MethodPut, "/containerApps/my-app"); len(puts) != 0 {
		t.Fatalf("the app was PUT %d times", len(puts))
	}
	patches := w.f.callsTo(http.MethodPatch, "/containerApps/my-app")
	if len(patches) != 1 {
		t.Fatalf("the app was patched %d times", len(patches))
	}
	var body map[string]any
	if err := json.Unmarshal(patches[0].Body, &body); err != nil {
		t.Fatal(err)
	}
	keys := func(v any) []string {
		var out []string
		for k := range v.(map[string]any) {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	if got := keys(body); !reflect.DeepEqual(got, []string{"properties"}) {
		t.Fatalf("the PATCH sets %v", got)
	}
	if got := keys(body["properties"]); !reflect.DeepEqual(got, []string{"configuration", "template"}) {
		t.Fatalf("the PATCH sets properties %v", got)
	}
	if got := keys(sub(body, "properties", "configuration")); !reflect.DeepEqual(got, []string{"secrets"}) {
		t.Fatalf("the PATCH sets configuration %v", got)
	}
	if got := keys(sub(body, "properties", "template")); !reflect.DeepEqual(got, []string{"containers", "scale"}) {
		t.Fatalf("the PATCH sets template %v", got)
	}
	// scale goes back exactly as the GET answered it, so a template taken as a unit keeps 0 to 2.
	if !reflect.DeepEqual(sub(body, "properties", "template", "scale"), sub(before, "template", "scale")) {
		t.Fatalf("the PATCH's scale %v is not the GET's %v", sub(body, "properties", "template", "scale"), sub(before, "template", "scale"))
	}
	// The kept values were read from the app itself, once, and went nowhere but back to it.
	if ls := w.f.callsTo(http.MethodPost, "/containerApps/my-app/listSecrets"); len(ls) != 1 {
		t.Fatalf("listSecrets was called %d times", len(ls))
	}
	for _, c := range w.f.calls {
		for _, v := range []string{secretValue, keyValue, keptValue} {
			if strings.Contains(string(c.Body), v) && !(c.Method == http.MethodPatch && strings.HasSuffix(c.Path, "/containerApps/my-app")) {
				t.Fatalf("a value went to %s %s", c.Method, c.Path)
			}
		}
	}

	after := snapshot(t, app.Props)
	for _, path := range [][]string{
		{"template", "scale"}, {"configuration", "ingress"}, {"configuration", "registries"}, {"configuration", "activeRevisionsMode"}, {"managedEnvironmentId"},
	} {
		if !reflect.DeepEqual(sub(before, path...), sub(after, path...)) {
			t.Fatalf("%s changed: %v -> %v", strings.Join(path, "."), sub(before, path...), sub(after, path...))
		}
	}
	scale := sub(after, "template", "scale").(map[string]any)
	if scale["minReplicas"] != float64(0) || scale["maxReplicas"] != float64(2) {
		t.Fatalf("scale = %v", scale)
	}
	bc, ac := anyList(sub(before, "template", "containers")), anyList(sub(after, "template", "containers"))
	if len(ac) != 2 || !reflect.DeepEqual(bc[1], ac[1]) {
		t.Fatalf("the sidecar changed: %v", ac)
	}
	for _, k := range []string{"name", "image", "resources", "probes"} {
		if !reflect.DeepEqual(bc[0][k], ac[0][k]) {
			t.Fatalf("the container's %s changed: %v -> %v", k, bc[0][k], ac[0][k])
		}
	}
	env := map[string]map[string]any{}
	var order []string
	for _, e := range anyList(ac[0]["env"]) {
		env[str(e["name"])] = e
		order = append(order, str(e["name"]))
	}
	if strings.Join(order, ",") != "PLAIN,DB_CONN,QUEUE_CONN,SMTP_PASSWORD,STRIPE_KEY" ||
		env["PLAIN"]["value"] != "hello" || env["DB_CONN"]["secretRef"] != "db-conn" || env["QUEUE_CONN"]["secretRef"] != "queue-conn" ||
		env["SMTP_PASSWORD"]["secretRef"] != "smtp-password" || env["STRIPE_KEY"]["secretRef"] != "stripe-key" {
		t.Fatalf("env = %v", anyList(ac[0]["env"]))
	}
	secrets := anyList(sub(app.Props, "configuration", "secrets")) // the fake's own copy holds values
	got := map[string]map[string]any{}
	var names []string
	for _, s := range secrets {
		got[str(s["name"])] = s
		names = append(names, str(s["name"]))
	}
	if strings.Join(names, ",") != "db-conn,queue-conn,smtp-password,stripe-key" ||
		got["db-conn"]["value"] != keptValue ||
		got["queue-conn"]["keyVaultUrl"] != "https://myvault"+".vault.azure.net/secrets/queue-conn" || got["queue-conn"]["value"] != nil || got["queue-conn"]["identity"] == nil ||
		got["smtp-password"]["value"] != secretValue || got["stripe-key"]["value"] != keyValue {
		t.Fatalf("the app's secrets are not the kept ones and the new ones (names %v)", names)
	}
	applied := r.events("applied")
	if len(applied) != 1 || applied[0]["command"] != "containerapp secret set" {
		t.Fatalf("applied = %v", applied)
	}
}

func TestSecretSetSendsInitContainersAndVolumesBackAsTheGETAnswered(t *testing.T) {
	w := newWorld(t)
	app := bicepApp(w)
	tpl := app.Props["template"].(map[string]any)
	tpl["initContainers"] = []any{map[string]any{"name": "migrate", "image": acrRef("myregistry", "mig:1"), "args": []any{"--up"}}}
	tpl["volumes"] = []any{map[string]any{"name": "data", "storageType": "EmptyDir"}}
	tpl["containers"].([]any)[0].(map[string]any)["volumeMounts"] = []any{map[string]any{"volumeName": "data", "mountPath": "/data"}}
	before := snapshot(t, app.Props)

	r := runPlugin(t, runOpts{config: apply(nil), bound: setBound()}, setLine)
	if !r.ok() {
		t.Fatal(r.text())
	}
	patches := w.f.callsTo(http.MethodPatch, "/containerApps/my-app")
	if len(patches) != 1 {
		t.Fatalf("the app was patched %d times", len(patches))
	}
	var body map[string]any
	if err := json.Unmarshal(patches[0].Body, &body); err != nil {
		t.Fatal(err)
	}
	var got []string
	for k := range sub(body, "properties", "template").(map[string]any) {
		got = append(got, k)
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, []string{"containers", "initContainers", "scale", "volumes"}) {
		t.Fatalf("the PATCH sets template %v", got)
	}
	for _, k := range []string{"scale", "initContainers", "volumes"} {
		if !reflect.DeepEqual(sub(body, "properties", "template", k), sub(before, "template", k)) {
			t.Fatalf("the PATCH's %s %v is not the GET's %v", k, sub(body, "properties", "template", k), sub(before, "template", k))
		}
	}
	if !reflect.DeepEqual(anyList(sub(body, "properties", "template", "containers"))[0]["volumeMounts"], anyList(sub(before, "template", "containers"))[0]["volumeMounts"]) {
		t.Fatal("the container's volume mounts changed")
	}
	if sub(body, "properties", "configuration", "ingress") != nil || sub(body, "properties", "configuration", "registries") != nil {
		t.Fatal("the PATCH carries ingress or registries")
	}
	if strings.Contains(string(patches[0].Body), "null") {
		t.Fatal("the PATCH carries a null")
	}
}

func TestSecretSetInWhatifPlansByNameAndNeitherReadsNorWrites(t *testing.T) {
	w := newWorld(t)
	bicepApp(w)
	w.f.allowListSecrets = false // whatif never reads the app's secrets
	r := runPlugin(t, runOpts{bound: setBound()}, setLine)
	if !r.ok() {
		t.Fatal(r.text())
	}
	assertNoSetValue(t, r.raw)
	mustContain(t, r.text(), "set on container app my-app, container my-app: SMTP_PASSWORD from mail.password as secret smtp-password",
		"add secrets smtp-password, stripe-key", "add environment variables SMTP_PASSWORD, STRIPE_KEY",
		"keep its other Key Vault secrets queue-conn and secrets db-conn", "Nothing was changed")
	if len(w.f.writes()) != 0 {
		t.Fatalf("whatif wrote: %v", w.f.writes())
	}
}

func TestSecretSetReplacesANamedSecretsValueAndSaysToRestart(t *testing.T) {
	w := newWorld(t)
	app := bicepApp(w)
	r := runPlugin(t, runOpts{config: apply(nil), bound: setBound()}, "containerapp secret set my-app secret=DB_CONN=mail.password")
	if !r.ok() {
		t.Fatal(r.text())
	}
	assertNoSetValue(t, r.raw)
	mustContain(t, r.text(), "run containerapp restart my-app")
	secrets := anyList(sub(app.Props, "configuration", "secrets"))
	if len(secrets) != 2 || secrets[0]["name"] != "db-conn" || secrets[0]["value"] != secretValue || secrets[1]["keyVaultUrl"] == nil {
		t.Fatal("db-conn was not replaced in place, or queue-conn was not kept")
	}
	// Nothing value-held is kept, so the app's values were not read at all.
	if ls := w.f.callsTo(http.MethodPost, "/listSecrets"); len(ls) != 0 {
		t.Fatalf("listSecrets was called %d times", len(ls))
	}
}

// No ARM write - nor any ARM request at all - before every connection is found.
func noARM(t *testing.T, w *world) {
	t.Helper()
	if len(w.f.writes()) != 0 || len(w.f.callsContaining(http.MethodGet, "/containerApps/")) != 0 {
		t.Fatalf("Azure was asked before the refusal: %v", w.f.calls)
	}
}

func TestSecretSetRefusesAConnectionNotBoundByNameWithNoARMWrite(t *testing.T) {
	w := newWorld(t)
	bicepApp(w)
	r := runPlugin(t, runOpts{config: apply(nil), bound: setBound()}, "containerapp secret set my-app secret=SMTP_PASSWORD=mailbox.password")
	if r.ok() {
		t.Fatal("a connection not bound was accepted")
	}
	mustContain(t, r.text(), "Refused: the secret SMTP_PASSWORD reads mailbox.password, the field password of the connection mailbox, which is not bound to this member's appSecrets slot (the ones bound are mail, stripe live)",
		"ask the person to add the connection mailbox to the team in Team settings -> Connections if it is not there yet, then bind it to the Azure member's appSecrets slot (Connections a deployed app may use) in that member's Member settings",
		"then run containerapp secret set again. Nothing was changed.")
	assertNoSetValue(t, r.raw)
	noARM(t, w)
}

func TestSecretSetRefusesAMissingFieldByNameWithNoARMWrite(t *testing.T) {
	w := newWorld(t)
	bicepApp(w)
	r := runPlugin(t, runOpts{config: apply(nil), bound: setBound()}, "containerapp secret set my-app secret=SMTP_PASSWORD=mail.password \"secret=STRIPE_KEY=stripe live.secret\"")
	if r.ok() {
		t.Fatal("a missing field was accepted")
	}
	mustContain(t, r.text(), "Refused: the secret STRIPE_KEY reads stripe live.secret, the field secret of the connection stripe live, which that connection does not have or holds no value in; stripe live has the field key, so run containerapp secret set again naming it as stripe live.key",
		"Nothing was changed.")
	assertNoSetValue(t, r.raw)
	noARM(t, w)
}

func TestSecretSetTakesOnlyConnectionFields(t *testing.T) {
	w := newWorld(t)
	bicepApp(w)
	for line, want := range map[string]string{
		"containerapp secret set my-app secret=DB_PASSWORD=" + w.vaultURL:    "secret=DB_PASSWORD names a Key Vault address, and containerapp secret set takes only <connection>.<field>",
		"containerapp secret set my-app secret=SMTP_PASSWORD=" + secretValue: "Refused: secret=SMTP_PASSWORD takes <connection>.<field>",
		"containerapp secret set my-app secret=STRIPE_KEY=appSecret1":        "Refused: secret=STRIPE_KEY takes <connection>.<field>",
		"containerapp secret set my-app":                                     "containerapp secret set takes <app>",
		"containerapp secret set my-app port=80 secret=A=mail.password":      "port=80 is not secret= or container=",
	} {
		r := runPlugin(t, runOpts{config: apply(nil), bound: setBound()}, line)
		if r.ok() {
			t.Fatalf("%s was accepted", line)
		}
		mustContain(t, r.text(), want)
		assertNoSetValue(t, r.raw)
		noARM(t, w)
	}
}

func TestSecretSetRefusesAnAppTheTeamDoesNotOwn(t *testing.T) {
	w := newWorld(t)
	r := runPlugin(t, runOpts{config: apply(nil), bound: setBound()}, "containerapp secret set their-app secret=SMTP_PASSWORD=mail.password")
	if r.ok() {
		t.Fatal("an app the team does not own was changed")
	}
	mustContain(t, r.text(), "This member changes only what its team owns. Nothing was changed.")
	if len(w.f.writes()) != 0 {
		t.Fatal("a write was made")
	}
}

func TestSecretSetPicksTheContainerNamedOrRefusesToGuess(t *testing.T) {
	w := newWorld(t)
	app := bicepApp(w)
	cs := app.Props["template"].(map[string]any)["containers"].([]any)
	cs[0].(map[string]any)["name"] = "web" // no container is named as the app now
	r := runPlugin(t, runOpts{config: apply(nil), bound: setBound()}, "containerapp secret set my-app secret=SMTP_PASSWORD=mail.password")
	if r.ok() {
		t.Fatal("a container was guessed")
	}
	mustContain(t, r.text(), "runs the containers web, log-shipper, so say which one reads the secrets with container=<name>")
	if len(w.f.writes()) != 0 {
		t.Fatal("a write was made")
	}
	r = runPlugin(t, runOpts{config: apply(nil), bound: setBound()}, "containerapp secret set my-app container=log-shipper secret=SMTP_PASSWORD=mail.password")
	if !r.ok() {
		t.Fatal(r.text())
	}
	after := anyList(app.Props["template"].(map[string]any)["containers"])
	if len(anyList(after[0]["env"])) != 3 || len(anyList(after[1]["env"])) != 2 || anyList(after[1]["env"])[1]["secretRef"] != "smtp-password" {
		t.Fatalf("containers = %v", after)
	}
}

// Azure's answer to the PATCH quoting a value does not carry it out.
func TestSecretSetNeverWritesAValueOutEvenWhenAzureQuotesIt(t *testing.T) {
	w := newWorld(t)
	bicepApp(w)
	refuseARM(w.f, func(method, path string) bool {
		return method == http.MethodPatch && strings.HasSuffix(path, "/containerApps/my-app")
	}, 400, "ContainerAppSecretInvalid", "secret smtp-password value "+secretValue+" and db-conn value "+keptValue+" are invalid")
	r := runPlugin(t, runOpts{config: apply(nil), bound: setBound()}, setLine)
	if r.ok() {
		t.Fatal("a refused PATCH was reported as done")
	}
	mustContain(t, r.text(), "ContainerAppSecretInvalid")
	assertNoSetValue(t, r.raw)
}
