package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A failed deployment says why, show reads a resource with its own type's api-version, an image
// build packs portable modes, the signed-in account can read the logs the plugin archives, and an
// app is told which managed identity is its own.

// ---- a failed deployment ----

func failedOp(typ, name, code, message string) map[string]any {
	return map[string]any{"properties": map[string]any{
		"provisioningState": "Failed",
		"targetResource":    map[string]any{"resourceType": typ, "resourceName": name},
		"statusMessage":     map[string]any{"status": "Failed", "error": map[string]any{"code": code, "message": message}},
	}}
}

func TestAFailedDeploymentReportsEachFailedOperation(t *testing.T) {
	f := newFake(t)
	docFile(t, "main.json", simpleTemplate)
	f.whatIf = []map[string]any{wiChange("Create", inRG(ownedRG, "Microsoft.ManagedIdentity/userAssignedIdentities/app-id"), nil)}
	f.deployOps = []map[string]any{
		{"properties": map[string]any{"provisioningState": "Succeeded", "targetResource": map[string]any{"resourceType": "Microsoft.ManagedIdentity/userAssignedIdentities", "resourceName": "app-id"}}},
		failedOp("Microsoft.Insights/diagnosticSettings", "env1/Microsoft.Insights/to-storage-2", "Conflict",
			"Data sinks can't be reused in different settings on the same category for the same resource"),
	}

	r := runPlugin(t, runOpts{config: apply(nil)}, "deploy "+ownedRG+" main.json")
	if r.ok() {
		t.Fatalf("a failed deployment answered ok: %s", r.text())
	}
	mustContain(t, r.text(), "DeploymentFailed",
		"- Microsoft.Insights/diagnosticSettings env1/Microsoft.Insights/to-storage-2: Conflict: Data sinks can't be reused in different settings on the same category for the same resource")
	if strings.Contains(r.text(), "app-id:") {
		t.Fatalf("a succeeded operation is reported as failed:\n%s", r.text())
	}
	if len(f.callsTo(http.MethodGet, "/operations")) != 1 {
		t.Fatal("the deployment's operations were not read")
	}
}

func TestAFailedDeploymentReportsAtMostTenOperationsThenHowManyMore(t *testing.T) {
	f := newFake(t)
	docFile(t, "main.json", simpleTemplate)
	f.whatIf = []map[string]any{wiChange("Create", inRG(ownedRG, "Microsoft.ManagedIdentity/userAssignedIdentities/app-id"), nil)}
	f.deployOps = []map[string]any{}
	for i := 1; i <= 13; i++ {
		f.deployOps = append(f.deployOps, failedOp("Microsoft.Storage/storageAccounts", fmt.Sprintf("store%02d", i), "StorageAccountAlreadyTaken", "The storage account name is already taken."))
	}
	r := runPlugin(t, runOpts{config: apply(nil)}, "deploy "+ownedRG+" main.json")
	if r.ok() {
		t.Fatal("a failed deployment answered ok")
	}
	if n := strings.Count(r.text(), "\n- Microsoft.Storage/storageAccounts store"); n != 10 {
		t.Fatalf("%d failed operations listed, want 10:\n%s", n, r.text())
	}
	mustContain(t, r.text(), "store10: StorageAccountAlreadyTaken: The storage account name is already taken.", "3 more")
	if strings.Contains(r.text(), "store11") {
		t.Fatalf("an operation past the tenth is listed:\n%s", r.text())
	}
}

// ---- show ----

func TestShowOfADiagnosticSettingUsesItsOwnTypesVersion(t *testing.T) {
	w := newWorld(t)
	diag := inRG(ownedRG, "Microsoft.App/managedEnvironments/my-app-env/providers/Microsoft.Insights/diagnosticSettings/yawble-logs")
	r := runPlugin(t, runOpts{}, "show "+diag)
	if !r.ok() {
		t.Fatal(r.text())
	}
	gets := w.f.callsTo(http.MethodGet, "/diagnosticSettings/yawble-logs")
	if len(gets) != 1 || !strings.Contains(gets[0].Query, "api-version="+diagAPIVersion) {
		t.Fatalf("the setting was read %d times, want once with api-version %s", len(gets), diagAPIVersion)
	}
}

func TestShowOfATypeWithNoKnownVersionIsRefusedNamingIt(t *testing.T) {
	f := newFake(t)
	id := inRG(ownedRG, "Microsoft.Web/sites/my-site")
	r := runPlugin(t, runOpts{}, "show "+id)
	if r.ok() {
		t.Fatalf("show answered ok: %s", r.text())
	}
	mustContain(t, r.text(), "Refused", "Microsoft.Web/sites")
	if got := f.callsTo(http.MethodGet, "/sites/my-site"); len(got) != 0 {
		t.Fatalf("the resource was read with a guessed version: %v", got)
	}
	if ev := r.events("refused"); len(ev) != 1 {
		t.Fatalf("refused = %v", ev)
	}
}

// Every type the plugin creates or names resolves to a version without asking Azure.
func TestEveryTypeThePluginNamesHasAVersion(t *testing.T) {
	types := []string{
		"Microsoft.Insights/diagnosticSettings", "Microsoft.KeyVault/vaults/secrets", "Microsoft.Consumption/budgets",
		"Microsoft.Network/dnsZones/TXT", "Microsoft.Network/dnsZones/CNAME", "Microsoft.Network/dnsZones/A",
		"Microsoft.Storage/storageAccounts/blobServices/containers", "Microsoft.Storage/storageAccounts/tableServices/tables",
		"Microsoft.Storage/storageAccounts/queueServices/queues", "Microsoft.Sql/servers/firewallRules",
		"Microsoft.Resources/deployments",
	}
	for t2 := range resourceKinds {
		types = append(types, t2)
	}
	for _, typ := range types {
		if _, ok := knownVersion(typ); !ok {
			t.Errorf("%s has no known api-version", typ)
		}
	}
	// An extension resource is read with its own type's version, whatever it sits on.
	p := &plugin{az: &client{tokens: map[api]string{}}}
	for id, want := range map[string]string{
		inRG(ownedRG, "Microsoft.App/managedEnvironments/e/providers/Microsoft.Insights/diagnosticSettings/d"):   diagAPIVersion,
		rgID(ownedRG) + "/providers/Microsoft.Authorization/roleAssignments/" + zeroGUID:                         roleAPIVersion,
		inRG(ownedRG, "Microsoft.Storage/storageAccounts/s/providers/Microsoft.Authorization/roleAssignments/x"): roleAPIVersion,
		inRG(ownedRG, "Microsoft.App/managedEnvironments/e"):                                                     versionOf("Microsoft.App/managedEnvironments"),
	} {
		r, _ := parseID(id)
		if got, err := p.apiVersionFor(r); err != nil || got != want {
			t.Errorf("%s: version %q, %v; want %s", id, got, err, want)
		}
	}
}

// ---- acr build ----

func TestAcrBuildPacksPortableModesOwnedByRoot(t *testing.T) {
	w := newWorld(t)
	dir := filepath.Dir(docFile(t, "modes/Dockerfile", "FROM scratch\nCOPY . /app\n"))
	settings := docFile(t, "modes/conf/appsettings.json", "{}")
	run := docFile(t, "modes/run.sh", "#!/bin/sh\n")
	for path, mode := range map[string]os.FileMode{
		filepath.Join(dir, "Dockerfile"): 0o660, settings: 0o660, run: 0o770,
		filepath.Join(dir, "conf"): 0o770, dir: 0o770,
	} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
	}
	r := runPlugin(t, runOpts{config: apply(nil)}, "acr build myregistry my-app:1.0 "+dir)
	if !r.ok() {
		t.Fatal(r.text())
	}
	var uploaded []byte
	for _, c := range w.f.calls {
		if strings.HasPrefix(c.Host, "acrsource") && c.Method == http.MethodPut {
			uploaded = c.Body
		}
	}
	gz, err := gzip.NewReader(bytes.NewReader(uploaded))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	got := map[string]int64{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Uid != 0 || h.Gid != 0 || h.Uname != "" || h.Gname != "" {
			t.Errorf("%s is owned by %d:%d (%q:%q), want 0:0 with no names", h.Name, h.Uid, h.Gid, h.Uname, h.Gname)
		}
		got[h.Name] = h.Mode & 0o7777
	}
	want := map[string]int64{"Dockerfile": 0o644, "conf/": 0o755, "conf/appsettings.json": 0o644, "run.sh": 0o755}
	for name, mode := range want {
		if got[name] != mode {
			t.Errorf("%s packed as %o, want %o", name, got[name], mode)
		}
	}
	if len(got) != len(want) {
		t.Errorf("packed %v, want %v", got, want)
	}
}

// ---- the signed-in account reads the logs it archives ----

// readerOnly serves a storage account's blobs only once the signed-in person holds Storage Blob
// Data Reader on it, as Azure would with shared key access off.
func readerOnly(w *world, name string) *fakeStorage {
	f := w.f
	_, st := f.addStorage(ownedRG, name, ownTags())
	delete(f.resources, strings.ToLower(inRG(ownedRG, "Microsoft.Storage/storageAccounts/"+name)))
	host := strings.TrimPrefix(storageHost(name, "blob"), "https://")
	serve := f.hosts[host]
	f.hosts[host] = func(rw http.ResponseWriter, req *http.Request) {
		if !holdsReader(f, inRG(ownedRG, "Microsoft.Storage/storageAccounts/"+name)) {
			rw.WriteHeader(403)
			_, _ = io.WriteString(rw, "<?xml version=\"1.0\"?><Error><Code>AuthorizationPermissionMismatch</Code><Message>This request is not authorized.\nRequestId:0</Message></Error>")
			return
		}
		serve(rw, req)
	}
	return st
}

func holdsReader(f *fake, account string) bool {
	for _, g := range f.callsContaining(http.MethodPut, account+"/providers/Microsoft.Authorization/roleAssignments/") {
		var a struct {
			Properties map[string]string `json:"properties"`
		}
		_ = json.Unmarshal(g.Body, &a)
		if roleOf(a.Properties["roleDefinitionId"]) == roleBlobDataReader && a.Properties["principalId"] == zeroGUID && a.Properties["principalType"] == "User" {
			return true
		}
	}
	return false
}

func TestStorageCreateGrantsTheSignedInAccountBlobDataReaderAndLogsThenRead(t *testing.T) {
	w := newWorld(t)
	st := readerOnly(w, "logstore")
	account := inRG(ownedRG, "Microsoft.Storage/storageAccounts/logstore")

	r := runPlugin(t, runOpts{config: apply(nil)}, "storage create "+ownedRG+" logstore")
	if !r.ok() {
		t.Fatal(r.text())
	}
	if !holdsReader(w.f, account) {
		t.Fatal("the signed-in account was not given Storage Blob Data Reader on the account")
	}
	mustContain(t, r.text(), "Storage Blob Data Reader")
	var recorded bool
	for _, ev := range r.events("applied") {
		for _, id := range ev["resourceIds"].([]any) {
			if strings.HasPrefix(id.(string), account+"/providers/Microsoft.Authorization/roleAssignments/") {
				recorded = true
			}
		}
	}
	if !recorded {
		t.Fatalf("applied names no role assignment: %v", r.events("applied"))
	}
	var assignments int
	for _, ev := range r.events("resource") {
		if ev["resourceType"] == "role-assignment" && ev["action"] == "created" {
			assignments++
		}
	}
	if assignments != len(storageDataRoles) {
		t.Fatalf("%d role-assignment resource events, want %d: %v", assignments, len(storageDataRoles), r.events("resource"))
	}

	// An environment archiving its logs there: containerapp logs reads them without a round trip.
	env := w.f.add(ownedRG, "Microsoft.App/managedEnvironments", "log-env", ownTags())
	diag := env.ID + "/providers/Microsoft.Insights/diagnosticSettings/to-storage"
	w.f.resources[strings.ToLower(diag)] = &fakeResource{ID: diag, Type: "Microsoft.Insights/diagnosticSettings", Props: map[string]any{"storageAccountId": account}}
	app := w.f.add(ownedRG, "Microsoft.App/containerApps", "log-app", ownTags())
	app.Props = map[string]any{"managedEnvironmentId": env.ID}
	st.blobs[archiveBlob("insights-logs-containerappconsolelogs", env.ID, 14)] = []byte(
		`{"time":"2026-10-09T14:40:00Z","properties":{"ContainerAppName":"log-app","Log":"listening"}}`)

	logs := runPlugin(t, runOpts{}, "containerapp logs log-app")
	if !logs.ok() {
		t.Fatal(logs.text())
	}
	mustContain(t, logs.text(), "from storage account logstore", "14:40:00Z  listening")
}

func TestStorageCreateSaysOnceWhenItMayNotGrantTheReader(t *testing.T) {
	w := newWorld(t)
	w.f.noGrants = true
	r := runPlugin(t, runOpts{config: apply(nil)}, "storage create "+ownedRG+" newstore")
	if !r.ok() {
		t.Fatalf("the account was created, so the command answers ok: %s", r.text())
	}
	if n := strings.Count(r.text(), "Storage Blob Data Reader"); n != 1 {
		t.Fatalf("the refused grant is said %d times, want once:\n%s", n, r.text())
	}
	mustContain(t, r.text(), "Storage account newstore is ready", "Access control (IAM)")
	if n := len(w.f.callsContaining(http.MethodPut, "/roleAssignments/")); n != 1 {
		t.Fatalf("%d grants tried after the first was refused, want 1", n)
	}
}

func dscChange(env, name, storage string) map[string]any {
	return wiChange("Create", env+"/providers/Microsoft.Insights/diagnosticSettings/"+name, map[string]any{
		"properties": map[string]any{"storageAccountId": storage, "logs": []any{map[string]any{"categoryGroup": "allLogs", "enabled": true}}},
	})
}

func TestADeployedDiagnosticSettingToStorageGrantsTheReader(t *testing.T) {
	w := newWorld(t)
	docFile(t, "main.json", simpleTemplate)
	env := inRG(ownedRG, "Microsoft.App/managedEnvironments/my-app-env")
	account := inRG(ownedRG, "Microsoft.Storage/storageAccounts/mystore")
	w.f.whatIf = []map[string]any{dscChange(env, "to-storage", account)}

	r := runPlugin(t, runOpts{config: apply(nil)}, "deploy "+ownedRG+" main.json")
	if !r.ok() {
		t.Fatal(r.text())
	}
	if !holdsReader(w.f, account) {
		t.Fatal("the signed-in account was not given Storage Blob Data Reader on the archive")
	}
	mustContain(t, r.text(), "Storage Blob Data Reader", "mystore")
	ev := r.events("applied")
	if len(ev) != 1 || !strings.Contains(fmt.Sprint(ev[0]["resourceIds"]), account+"/providers/Microsoft.Authorization/roleAssignments/") {
		t.Fatalf("applied = %v", ev)
	}
	if n := len(w.f.callsContaining(http.MethodPut, "/roleAssignments/")); n != 1 {
		t.Fatalf("%d grants, want 1", n)
	}

	// In whatif it says it would, and grants nothing.
	before := len(w.f.writes())
	wi := runPlugin(t, runOpts{}, "deploy "+ownedRG+" main.json")
	mustContain(t, wi.text(), "Storage Blob Data Reader", "mystore")
	if len(w.f.writes()) != before {
		t.Fatal("whatif wrote")
	}
}

func TestADeployedDiagnosticSettingToStorageTheTeamDoesNotOwnGrantsNothing(t *testing.T) {
	w := newWorld(t)
	docFile(t, "main.json", simpleTemplate)
	env := inRG(ownedRG, "Microsoft.App/managedEnvironments/my-app-env")
	w.f.whatIf = []map[string]any{dscChange(env, "to-theirs", w.foreign["store"].ID)}

	r := runPlugin(t, runOpts{config: apply(nil)}, "deploy "+ownedRG+" main.json")
	if !r.ok() {
		t.Fatal(r.text())
	}
	if n := len(w.f.callsContaining(http.MethodPut, "/roleAssignments/")); n != 0 {
		t.Fatalf("%d grants on a storage account the team does not own", n)
	}
	if n := strings.Count(r.text(), "Storage Blob Data Reader"); n != 1 {
		t.Fatalf("the grant not made is said %d times, want once:\n%s", n, r.text())
	}
	mustContain(t, r.text(), "theirstore")
	if ev := r.events("refused"); len(ev) != 1 {
		t.Fatalf("refused = %v", ev)
	}
}

// ---- an app is told which managed identity is its own ----

func appEnv(t *testing.T, body []byte) map[string][]string {
	t.Helper()
	var app struct {
		Properties struct {
			Template struct {
				Containers []struct {
					Env []map[string]string `json:"env"`
				} `json:"containers"`
			} `json:"template"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(body, &app); err != nil || len(app.Properties.Template.Containers) != 1 {
		t.Fatalf("the app is %s", body)
	}
	out := map[string][]string{}
	for _, e := range app.Properties.Template.Containers[0].Env {
		out[e["name"]] = append(out[e["name"]], e["value"])
	}
	return out
}

func TestContainerAppCreateSetsAzureClientIDToItsIdentity(t *testing.T) {
	w := newWorld(t)
	r := runPlugin(t, runOpts{config: apply(nil)}, "containerapp create "+ownedRG+" new-app my-app-env "+acrRef("myregistry", "my-app:1.0")+" env=MODE=prod")
	if !r.ok() {
		t.Fatal(r.text())
	}
	put := w.f.callsTo(http.MethodPut, "/containerApps/new-app")
	identity := w.f.resources[strings.ToLower(inRG(ownedRG, "Microsoft.ManagedIdentity/userAssignedIdentities/new-app-id"))]
	clientID := identity.Props["clientId"].(string)
	env := appEnv(t, put[0].Body)
	if got := env["AZURE_CLIENT_ID"]; len(got) != 1 || got[0] != clientID {
		t.Fatalf("AZURE_CLIENT_ID = %v, want %s", got, clientID)
	}
	if got := env["MODE"]; len(got) != 1 || got[0] != "prod" {
		t.Fatalf("MODE = %v", got)
	}
	mustContain(t, r.text(), "AZURE_CLIENT_ID", clientID, "new-app-id")
}

func TestContainerAppCreateKeepsTheCommandsOwnAzureClientID(t *testing.T) {
	w := newWorld(t)
	own := strings.Repeat("0", 8) + "-0000-0000-0000-0000000000c9"
	r := runPlugin(t, runOpts{config: apply(nil)}, "containerapp create "+ownedRG+" new-app my-app-env "+acrRef("myregistry", "my-app:1.0")+" env=AZURE_CLIENT_ID="+own)
	if !r.ok() {
		t.Fatal(r.text())
	}
	env := appEnv(t, w.f.callsTo(http.MethodPut, "/containerApps/new-app")[0].Body)
	if got := env["AZURE_CLIENT_ID"]; len(got) != 1 || got[0] != own {
		t.Fatalf("AZURE_CLIENT_ID = %v, want the command's own %s", got, own)
	}
	mustContain(t, r.text(), "AZURE_CLIENT_ID")
}
