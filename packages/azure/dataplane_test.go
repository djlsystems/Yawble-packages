package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// world is the team's owned resources, and someone else's, for the data-plane tests.
type world struct {
	f        *fake
	store    *fakeStorage
	ident    *fakeResource
	sql      *fakeSQL
	folder   string // a build folder in the team's documents
	sqlFile  string
	upload   string
	foreign  map[string]*fakeResource
	vaultURL string
}

func newWorld(t *testing.T) *world {
	f := newFake(t)
	w := &world{f: f, foreign: map[string]*fakeResource{}}
	_, w.store = f.addStorage(ownedRG, "mystore", ownTags())
	f.add(ownedRG, "Microsoft.ContainerRegistry/registries", "myregistry", ownTags())
	f.addVault(ownedRG, "myvault", ownTags(), "db-password")
	f.addApp(ownedRG, "my-app", ownTags())
	f.add(ownedRG, "Microsoft.Sql/servers", "my-sql", ownTags())
	addDB(f, ownedRG, "my-sql", "my-db", nil)
	f.add(ownedRG, "Microsoft.Network/dnsZones", "example.com", ownTags())
	w.ident = f.add(ownedRG, "Microsoft.ManagedIdentity/userAssignedIdentities", "app-id", ownTags())
	w.ident.Props["principalId"] = principalA
	f.serveLogsAndBuilds([]string{"started", "listening on 8080"})

	theirs := map[string]string{"cost-center": "shared"}
	w.foreign["store"], _ = f.addStorage(foreignRG, "theirstore", theirs)
	w.foreign["registry"] = f.add(foreignRG, "Microsoft.ContainerRegistry/registries", "theirregistry", theirs)
	w.foreign["vault"] = f.addVault(foreignRG, "theirvault", theirs, "db-password")
	w.foreign["app"], w.foreign["env"] = f.addApp(foreignRG, "their-app", theirs)
	w.foreign["sql"] = f.add(foreignRG, "Microsoft.Sql/servers", "their-sql", theirs)
	w.foreign["db"] = addDB(f, foreignRG, "their-sql", "their-db", nil)
	w.foreign["ident"] = f.add(foreignRG, "Microsoft.ManagedIdentity/userAssignedIdentities", "their-id", theirs)
	w.foreign["ident"].Props["principalId"] = "00000000-0000-0000-0000-0000000000f1"

	w.folder = filepath.Dir(docFile(t, "app/Dockerfile", "FROM scratch\nCOPY . /app\n"))
	docFile(t, "app/main.go", "package main\n")
	docFile(t, "app/.dockerignore", "# build output\nbin\n*.log\n!keep.log\n")
	docFile(t, "app/bin/app", "binary")
	docFile(t, "app/debug.log", "noise")
	docFile(t, "app/keep.log", "kept")
	w.sqlFile = docFile(t, "sql/schema.sql", "CREATE TABLE orders (id int);\nGO\nSELECT id, name FROM orders;\n")
	w.upload = docFile(t, "report.txt", "a report")
	w.sql = newFakeSQL(t)
	w.vaultURL = "https://myvault" + ".vault.azure.net/secrets/db-password"
	resolve(t, nil)
	return w
}

// acrRef is an image in a registry.
func acrRef(registry, image string) string { return registryHost(registry) + "/" + image }

// addDB puts a database on a server.
func addDB(f *fake, rg, server, name string, tags map[string]string) *fakeResource {
	d := f.add(rg, "Microsoft.Sql/servers", server+"/databases/"+name, tags)
	d.Type = "Microsoft.Sql/servers/databases"
	return d
}

// resolve makes the DNS answer what records holds, by name; nothing else resolves.
func resolve(t *testing.T, records map[string][]string) {
	oldTXT, oldCNAME, oldHost := lookupTXT, lookupCNAME, lookupHost
	missing := errors.New("no such host")
	lookupTXT = func(name string) ([]string, error) {
		if v, ok := records["TXT "+name]; ok {
			return v, nil
		}
		return nil, missing
	}
	lookupCNAME = func(name string) (string, error) {
		if v, ok := records["CNAME "+name]; ok {
			return v[0] + ".", nil
		}
		return "", missing
	}
	lookupHost = func(name string) ([]string, error) {
		if v, ok := records["A "+name]; ok {
			return v, nil
		}
		return nil, missing
	}
	t.Cleanup(func() { lookupTXT, lookupCNAME, lookupHost = oldTXT, oldCNAME, oldHost })
}

func bound(host string) map[string][]string {
	return map[string][]string{"TXT asuid." + host: {verifyID}, "CNAME " + host: {"my-app.apps" + ".example.com"}}
}

// newCommands is one line of every changing command this card adds, each on what the team owns.
func (w *world) newCommands() []string {
	return []string{
		"storage create " + ownedRG + " newstore",
		"storage container create mystore files",
		"storage table create mystore orders",
		"storage queue create mystore jobs",
		"blob put mystore files report.txt " + w.upload,
		"blob delete mystore files report.txt",
		`table upsert mystore orders p1 r1 '{"status":"open"}'`,
		"table delete mystore orders p1 r1",
		"queue send mystore jobs hello there",
		"queue receive mystore jobs",
		"queue delete mystore jobs m1 pop1",
		"keyvault create " + ownedRG + " newvault",
		"acr create " + ownedRG + " newregistry",
		"acr build myregistry my-app:1.0 " + w.folder,
		"sql server create " + ownedRG + " new-sql",
		"sql db create my-sql new-db S0",
		"sql exec my-sql/my-db " + w.sqlFile,
		"sql grant my-sql/my-db " + w.ident.ID + " db_datareader db_datawriter",
		"containerapp env create " + ownedRG + " new-env",
		"containerapp create " + ownedRG + " new-app my-app-env " + acrRef("myregistry", "my-app:1.0") + " port=8080 env=MODE=prod secret=DB_PASSWORD=" + w.vaultURL,
		"containerapp restart my-app",
		"domain bind my-app www.example.com",
	}
}

// ---- whatif sends no write ----

func TestWhatifSendsNoWriteForEveryNewCommand(t *testing.T) {
	w := newWorld(t)
	cfg := baseConfig(map[string]any{"allowDelete": true})
	for _, records := range []map[string][]string{nil, bound("www.example.com")} {
		resolve(t, records)
		for _, line := range w.newCommands() {
			before := len(w.f.calls)
			r := runPlugin(t, runOpts{config: cfg}, line)
			if !r.ok() {
				t.Fatalf("%s: %s", line, r.text())
			}
			mustContain(t, r.text(), "whatif", "Nothing was changed")
			if ev := r.events("planned"); len(ev) != 1 || ev[0]["mode"] != "whatif" {
				t.Fatalf("%s: planned = %v", line, ev)
			}
			if ev := r.events("applied"); len(ev) != 0 {
				t.Fatalf("%s published applied in whatif: %v", line, ev)
			}
			for _, c := range w.f.calls[before:] {
				if c.Method != http.MethodGet {
					t.Fatalf("%s in whatif sent %s %s %s", line, c.Method, c.Host, c.Path)
				}
			}
		}
	}
	if len(w.f.writes()) != 0 || len(w.sql.dials) != 0 {
		t.Fatalf("whatif wrote %v, or opened SQL to %v", w.f.writes(), w.sql.dials)
	}
}

// ---- each refuses an unowned target ----

func TestEveryNewCommandRefusesWhatTheTeamDoesNotOwn(t *testing.T) {
	w := newWorld(t)
	w.f.addApp(ownedRG, "mine", ownTags())
	cfg := apply(map[string]any{"allowDelete": true})
	theirs := "https://theirvault" + ".vault.azure.net/secrets/db-password"
	for _, line := range []string{
		"storage create " + foreignRG + " newstore",
		"storage container create theirstore files",
		"storage table create theirstore orders",
		"storage queue create theirstore jobs",
		"blob put theirstore files report.txt " + w.upload,
		"blob delete theirstore files report.txt",
		`table upsert theirstore orders p1 r1 '{"status":"open"}'`,
		"table delete theirstore orders p1 r1",
		"queue send theirstore jobs hello",
		"queue receive theirstore jobs",
		"queue delete theirstore jobs m1 pop1",
		"keyvault create " + foreignRG + " newvault",
		"acr create " + foreignRG + " newregistry",
		"acr build theirregistry my-app:1.0 " + w.folder,
		"sql server create " + foreignRG + " new-sql",
		"sql db create their-sql new-db S0",
		"sql exec their-sql/their-db " + w.sqlFile,
		"sql grant their-sql/their-db " + w.ident.ID + " db_datareader",
		"sql grant my-sql/my-db " + w.foreign["ident"].ID + " db_datareader",
		"containerapp env create " + foreignRG + " new-env",
		"containerapp create " + foreignRG + " new-app my-app-env " + acrRef("myregistry", "my-app:1.0") + "",
		"containerapp create " + ownedRG + " new-app their-app-env " + acrRef("myregistry", "my-app:1.0") + "",
		"containerapp create " + ownedRG + " new-app mine-env " + acrRef("theirregistry", "my-app:1.0") + "",
		"containerapp create " + ownedRG + " new-app mine-env " + acrRef("myregistry", "my-app:1.0") + " secret=DB_PASSWORD=" + theirs,
		"containerapp restart their-app",
		"domain bind their-app www.example.com",
	} {
		r := runPlugin(t, runOpts{config: cfg}, line)
		if r.ok() || !strings.HasPrefix(r.text(), "Refused") {
			t.Fatalf("%s was not refused:\n%s", line, r.text())
		}
		if ev := r.events("refused"); len(ev) != 1 {
			t.Fatalf("%s: refused events = %v", line, ev)
		}
	}
	if wr := w.f.writes(); len(wr) != 0 || len(w.sql.dials) != 0 {
		t.Fatalf("a refused command wrote %v, or opened SQL to %v", wr, w.sql.dials)
	}
}

// ---- the shared exceptions allow only what the spec allows ----

func TestADatabaseIsCreatedOnASharedServerAndSQLRunsOnlyOnTaggedOnes(t *testing.T) {
	w := newWorld(t)
	cfg := apply(map[string]any{"sharedSqlServers": []string{"their-sql"}, "allowDelete": true})
	w.sql.reply = func(sql string) []byte {
		if strings.HasPrefix(sql, "SELECT") {
			return selectReply(3)
		}
		return tdsDone(0x10, 0)
	}

	r := runPlugin(t, runOpts{config: cfg}, "sql db create their-sql team-db S0")
	if !r.ok() {
		t.Fatalf("sql db create on a shared server: %s", r.text())
	}
	put := w.f.callsTo(http.MethodPut, "/servers/their-sql/databases/team-db")
	if len(put) != 1 {
		t.Fatalf("database PUTs = %v", put)
	}
	var body struct {
		Tags map[string]string `json:"tags"`
		SKU  struct{ Name string }
	}
	_ = json.Unmarshal(put[0].Body, &body)
	if body.Tags["yawble-owner"] != testTeam || body.Tags["yawble-plugin"] != "azure" || body.SKU.Name != "S0" {
		t.Fatalf("the database was created as %s", put[0].Body)
	}
	if ev := r.events("applied"); len(ev) != 1 || ev[0]["command"] != "sql db create" {
		t.Fatalf("applied = %v", ev)
	}

	// SQL runs on the new, tagged database...
	r = runPlugin(t, runOpts{config: cfg}, "sql exec their-sql/team-db "+w.sqlFile)
	if !r.ok() {
		t.Fatalf("sql exec on the team's database: %s", r.text())
	}
	mustContain(t, r.text(), "Batch 1: 0 rows affected.", "Batch 2: 3 rows:\nid | name\n1 | row x\n2 | row xx\n3 | row ")
	if l := w.sql.logins; len(l) != 1 || l[0].database != "team-db" || l[0].server != sqlHost("their-sql") || l[0].token != tokSQL {
		t.Fatalf("logins = %+v", l)
	}

	// ...never on an untagged one on the same server, nor on the server itself.
	for _, line := range []string{
		"sql exec their-sql/their-db " + w.sqlFile,
		"sql grant their-sql/their-db " + w.ident.ID + " db_datareader",
		"delete " + w.foreign["sql"].ID,
		"delete " + w.foreign["db"].ID,
		"role assign Contributor " + w.ident.ID + " " + w.foreign["sql"].ID,
	} {
		r = runPlugin(t, runOpts{config: cfg}, line)
		if r.ok() || !strings.HasPrefix(r.text(), "Refused") {
			t.Fatalf("%s on the shared server was not refused:\n%s", line, r.text())
		}
	}
	mustContain(t, runPlugin(t, runOpts{config: cfg}, "sql exec their-sql/their-db "+w.sqlFile).text(), "does not carry this team's tags", "Nothing was run")
	if len(w.sql.logins) != 1 {
		t.Fatalf("SQL ran on a database the team does not own: %+v", w.sql.batches)
	}

	// A database of the same name that is not the team's is refused, not adopted.
	r = runPlugin(t, runOpts{config: cfg}, "sql db create their-sql their-db S0")
	mustContain(t, r.text(), "Refused: database their-db already exists on their-sql and is not this team's")
}

func TestASharedRegistryTakesBuildsAndAcrPullOnly(t *testing.T) {
	w := newWorld(t)
	w.f.addApp(ownedRG, "mine", ownTags())
	cfg := apply(map[string]any{"sharedRegistries": []string{"theirregistry"}, "allowDelete": true})
	reg := w.foreign["registry"].ID

	r := runPlugin(t, runOpts{config: cfg}, "acr build theirregistry my-app:1.0 "+w.folder)
	if !r.ok() {
		t.Fatalf("acr build on a shared registry: %s", r.text())
	}
	r = runPlugin(t, runOpts{config: cfg}, "containerapp create "+ownedRG+" new-app mine-env "+acrRef("theirregistry", "my-app:1.0")+"")
	if !r.ok() {
		t.Fatalf("containerapp create from a shared registry: %s", r.text())
	}
	grants := w.f.callsTo(http.MethodPut, "")
	var onRegistry []string
	for _, c := range grants {
		if strings.HasPrefix(c.Path, reg+"/providers/Microsoft.Authorization/roleAssignments/") {
			onRegistry = append(onRegistry, string(c.Body))
		}
	}
	if len(onRegistry) != 1 || !strings.Contains(onRegistry[0], roleAcrPull) || !strings.Contains(onRegistry[0], principalA) {
		t.Fatalf("grants on the shared registry = %v", onRegistry)
	}

	before := len(w.f.writes())
	for _, line := range []string{
		"role assign Contributor " + w.ident.ID + " " + reg,
		"delete " + reg,
		"acr create " + foreignRG + " theirregistry",
	} {
		if r := runPlugin(t, runOpts{config: cfg}, line); r.ok() || !strings.HasPrefix(r.text(), "Refused") {
			t.Fatalf("%s on a shared registry was not refused:\n%s", line, r.text())
		}
	}
	if len(w.f.writes()) != before {
		t.Fatalf("a refused change to the shared registry wrote")
	}
}

// ---- storage: shared key access off, never a key ----

func TestStorageCreateTurnsSharedKeyAccessOff(t *testing.T) {
	w := newWorld(t)
	r := runPlugin(t, runOpts{config: apply(nil)}, "storage create "+ownedRG+" newstore")
	if !r.ok() {
		t.Fatal(r.text())
	}
	put := w.f.callsTo(http.MethodPut, "/storageAccounts/newstore")
	if len(put) != 1 {
		t.Fatalf("PUTs = %v", put)
	}
	var body struct {
		Tags       map[string]string `json:"tags"`
		Properties map[string]any    `json:"properties"`
	}
	_ = json.Unmarshal(put[0].Body, &body)
	if body.Properties["allowSharedKeyAccess"] != false || body.Properties["allowBlobPublicAccess"] != false ||
		body.Properties["defaultToOAuthAuthentication"] != true || body.Tags["yawble-owner"] != testTeam {
		t.Fatalf("the account was created as %s", put[0].Body)
	}
	mustContain(t, r.text(), "shared key access off")

	// The signed-in person is given the three data roles on that account, and on nothing else.
	grants := w.f.callsContaining(http.MethodPut, "/storageAccounts/newstore/providers/Microsoft.Authorization/roleAssignments/")
	if len(grants) != 3 {
		t.Fatalf("role assignment PUTs = %d, want 3", len(grants))
	}
	roles := map[string]bool{}
	for _, g := range grants {
		var a struct {
			Properties struct {
				RoleDefinitionID string `json:"roleDefinitionId"`
				PrincipalID      string `json:"principalId"`
				PrincipalType    string `json:"principalType"`
			} `json:"properties"`
		}
		_ = json.Unmarshal(g.Body, &a)
		if a.Properties.PrincipalID != zeroGUID || a.Properties.PrincipalType != "User" {
			t.Fatalf("granted to %s (%s)", a.Properties.PrincipalID, a.Properties.PrincipalType)
		}
		roles[roleOf(a.Properties.RoleDefinitionID)] = true
	}
	for _, want := range storageDataRoles {
		if !roles[want] {
			t.Fatalf("%s was not granted; got %v", grantable[want], roles)
		}
	}
	if all := w.f.callsContaining(http.MethodPut, "/roleAssignments/"); len(all) != 3 {
		t.Fatalf("a role was granted elsewhere: %d assignments", len(all))
	}
	mustContain(t, r.text(), "Storage Blob, Table and Queue Data Contributor")
}

func TestNoCodeAsksAzureForAKeyOrASecretValue(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		raw, _ := os.ReadFile(name)
		lower := strings.ToLower(string(raw))
		for _, bad := range []string{"listkeys", "sharedkeys", "listconnectionstrings", "listcredentials", "regeneratekey", "/secrets/\" +"} {
			if strings.Contains(lower, bad) {
				t.Errorf("%s holds %q", name, bad)
			}
		}
	}
}

func TestBlobsTablesAndQueuesInApply(t *testing.T) {
	w := newWorld(t)
	cfg := apply(map[string]any{"allowDelete": true})
	run := func(line string) string {
		t.Helper()
		r := runPlugin(t, runOpts{config: cfg}, line)
		if !r.ok() {
			t.Fatalf("%s: %s", line, r.text())
		}
		return r.text()
	}

	mustContain(t, run("blob put mystore files reports/report.txt "+w.upload), "Uploaded report.txt to mystore/files/reports/report.txt (8 bytes)")
	mustContain(t, run("blob list mystore files"), "- reports/report.txt (8 bytes")
	mustContain(t, run("blob get mystore files reports/report.txt copies/back.txt"), "to copies/back.txt in the team's documents")
	if raw, _ := os.ReadFile(filepath.Join(documentsRoot, testTeam, "copies", "back.txt")); string(raw) != "a report" {
		t.Fatalf("blob get wrote %q", raw)
	}
	if r := runPlugin(t, runOpts{config: cfg}, "blob get mystore files reports/report.txt ../../escape.txt"); r.ok() {
		t.Fatalf("blob get wrote outside the documents: %s", r.text())
	}
	mustContain(t, run("blob delete mystore files reports/report.txt"), "Deleted blob mystore/files/reports/report.txt")

	mustContain(t, run(`table upsert mystore orders p1 "r'1" '{"status":"open"}'`), "Wrote entity (p1, r'1)")
	mustContain(t, run("table query mystore orders PartitionKey eq 'p1'"), `"status":"open"`)
	mustContain(t, run(`table delete mystore orders p1 "r'1"`), "Deleted entity (p1, r'1)")
	if c := w.f.callsTo(http.MethodDelete, "(PartitionKey='p1',RowKey='r''1')"); len(c) != 1 {
		t.Fatalf("a quote in a key was not doubled: %v", w.f.callsTo(http.MethodDelete, ")"))
	}

	mustContain(t, run("queue send mystore jobs build the report"), "Sent the message to mystore/jobs as message m1")
	mustContain(t, run("queue peek mystore jobs"), "- m1 (", "): build the report")
	mustContain(t, run("queue receive mystore jobs"), "- m1, pop receipt pop1: build the report")
	mustContain(t, run("queue delete mystore jobs m1 pop1"), "Deleted message m1 from mystore/jobs")

	// Reads go anywhere the person can read; changes only to the team's own accounts.
	mustContain(t, run("blob list theirstore files"), "holds no blobs")

	// Each storage host got the storage token, and only it.
	for _, c := range w.f.calls {
		if strings.HasPrefix(c.Host, "mystore.") && c.Token != tokStorage {
			t.Fatalf("%s got another API's token", c.Host)
		}
	}
}

func TestDeletesOfBlobsEntitiesAndMessagesNeedAllowDelete(t *testing.T) {
	w := newWorld(t)
	for _, mode := range []string{"whatif", "apply"} {
		for _, line := range []string{"blob delete mystore files a.txt", "table delete mystore orders p1 r1", "queue delete mystore jobs m1 pop1"} {
			r := runPlugin(t, runOpts{config: baseConfig(map[string]any{"mode": mode})}, line)
			mustContain(t, r.text(), "Refused: allowDelete is off")
		}
	}
	if len(w.f.writes()) != 0 {
		t.Fatalf("a refused delete wrote")
	}
}

// ---- Key Vault ----

func TestKeyVaultListsSecretNamesOnlyAndCreatesAnRBACVault(t *testing.T) {
	w := newWorld(t)
	r := runPlugin(t, runOpts{}, "keyvault secrets myvault")
	mustContain(t, r.text(), "Secrets in Key Vault myvault, by name only (1):\n- db-password")

	r = runPlugin(t, runOpts{config: apply(nil)}, "keyvault create "+ownedRG+" newvault")
	if !r.ok() {
		t.Fatal(r.text())
	}
	put := w.f.callsTo(http.MethodPut, "/vaults/newvault")
	var body struct {
		Properties map[string]any `json:"properties"`
	}
	_ = json.Unmarshal(put[0].Body, &body)
	if body.Properties["enableRbacAuthorization"] != true || body.Properties["tenantId"] != zeroGUID {
		t.Fatalf("the vault was created as %s", put[0].Body)
	}
}

// ---- registries ----

func TestAcrBuildUploadsTheFolderLessDockerignoreAndAnswersTheDigest(t *testing.T) {
	w := newWorld(t)
	r := runPlugin(t, runOpts{config: apply(nil)}, "acr build myregistry my-app:1.0 "+w.folder)
	if !r.ok() {
		t.Fatal(r.text())
	}
	mustContain(t, r.text(), "Built and pushed "+acrRef("myregistry", "my-app:1.0")+" in run ca1", "Digest: "+buildDigest,
		"Pin it as "+acrRef("myregistry", "my-app@"+buildDigest), "Successfully pushed", "AccountKey=[redacted]", "the token [redacted]")

	var uploaded []byte
	for _, c := range w.f.calls {
		if strings.HasPrefix(c.Host, "acrsource") && c.Method == http.MethodPut {
			uploaded = c.Body
			if c.Token != "" {
				t.Fatalf("the upload carried a token")
			}
		}
	}
	got := tarNames(t, uploaded)
	want := []string{".dockerignore", "Dockerfile", "keep.log", "main.go"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("uploaded %v, want %v", got, want)
	}
	run := w.f.callsTo(http.MethodPost, "/scheduleRun")
	var body map[string]any
	_ = json.Unmarshal(run[0].Body, &body)
	if body["type"] != "DockerBuildRequest" || body["sourceLocation"] != "source/upload.tar.gz" || body["isPushEnabled"] != true {
		t.Fatalf("scheduleRun = %s", run[0].Body)
	}
	if ev := r.events("applied"); len(ev) != 1 || !strings.Contains(ev[0]["summary"].(string), buildDigest) {
		t.Fatalf("applied = %v", ev)
	}
}

func TestAFailedBuildSaysSoWithTheLogTail(t *testing.T) {
	w := newWorld(t)
	w.f.runStatus = "Failed"
	r := runPlugin(t, runOpts{config: apply(nil)}, "acr build myregistry my-app:1.0 "+w.folder)
	if r.ok() {
		t.Fatal("a failed build answered ok")
	}
	mustContain(t, r.text(), "the build ca1 of my-app:1.0 ended failed", "Step 1/2")
}

func TestAcrBuildRefusesAFolderOutsideTheTeamsFolders(t *testing.T) {
	w := newWorld(t)
	outside := t.TempDir()
	_ = os.WriteFile(filepath.Join(outside, "Dockerfile"), []byte("FROM scratch\n"), 0o644)
	r := runPlugin(t, runOpts{config: apply(nil)}, "acr build myregistry my-app:1.0 "+outside)
	mustContain(t, r.text(), "is outside the team's documents folder and this run's worktrees")
	if len(w.f.writes()) != 0 {
		t.Fatal("a refused build wrote")
	}
}

func tarNames(t *testing.T, raw []byte) []string {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("the upload is not gzip: %v", err)
	}
	tr := tar.NewReader(gz)
	var names []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			names = append(names, h.Name)
		}
	}
	sort.Strings(names)
	return names
}

func TestDockerignorePatterns(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, ".dockerignore"), []byte("node_modules\n**/*.tmp\ndocs/*.md\n!docs/README.md\n# a comment\n"), 0o644)
	d := readDockerignore(dir)
	for path, out := range map[string]bool{
		"node_modules": true, "node_modules/x/y.js": true, "a/b/c.tmp": true, "c.tmp": true,
		"docs/guide.md": true, "docs/README.md": false, "src/main.go": false, "docs/sub/x.md": false,
	} {
		if d.excludes(path) != out {
			t.Errorf("excludes(%s) = %v", path, !out)
		}
	}
}

// ---- container apps ----

func TestContainerAppCreatePullsWithItsOwnIdentityAndReadsSecretsFromKeyVault(t *testing.T) {
	w := newWorld(t)
	r := runPlugin(t, runOpts{config: apply(nil)}, "containerapp create "+ownedRG+" new-app my-app-env "+acrRef("myregistry", "my-app:1.0")+" port=8080 env=MODE=prod secret=DB_PASSWORD="+w.vaultURL)
	if !r.ok() {
		t.Fatal(r.text())
	}
	mustContain(t, r.text(), "Container app new-app runs "+acrRef("myregistry", "my-app:1.0")+"", "pulling with its own identity new-app-id", "It answers at https://"+"new-app.apps")

	identity := inRG(ownedRG, "Microsoft.ManagedIdentity/userAssignedIdentities/new-app-id")
	var roles []string
	for _, c := range w.f.callsTo(http.MethodPut, "") {
		if strings.Contains(c.Path, "/roleAssignments/") {
			var a struct{ Properties map[string]string }
			_ = json.Unmarshal(c.Body, &a)
			if a.Properties["principalId"] != principalA || a.Properties["principalType"] != "ServicePrincipal" {
				t.Fatalf("a grant to %v", a.Properties)
			}
			roles = append(roles, a.Properties["roleDefinitionId"][strings.LastIndex(a.Properties["roleDefinitionId"], "/")+1:])
		}
	}
	sort.Strings(roles)
	if want := []string{roleKeyVaultSecretsUser, roleAcrPull}; strings.Join(roles, ",") != strings.Join([]string{want[0], want[1]}, ",") && strings.Join(roles, ",") != strings.Join([]string{want[1], want[0]}, ",") {
		t.Fatalf("roles granted = %v", roles)
	}

	put := w.f.callsTo(http.MethodPut, "/containerApps/new-app")
	raw := string(put[0].Body)
	for _, want := range []string{
		`"server":"` + registryHost("myregistry") + `"`, `"identity":"` + identity + `"`,
		`"keyVaultUrl":"` + w.vaultURL + `"`, `"name":"DB_PASSWORD","secretRef":"db-password"`, `"name":"MODE","value":"prod"`,
		`"targetPort":8080`, `"yawble-owner":"` + testTeam + `"`,
	} {
		if !strings.Contains(raw, want) {
			t.Fatalf("the app lacks %s:\n%s", want, raw)
		}
	}
	if strings.Contains(raw, "passwordSecretRef") || strings.Contains(raw, `"username"`) {
		t.Fatalf("the app carries a registry password:\n%s", raw)
	}
}

func TestContainerAppCreateRefusesSecretsOutsideKeyVault(t *testing.T) {
	w := newWorld(t)
	for line, want := range map[string]string{
		"env=DB_PASSWORD=hunter2":                     "DB_PASSWORD looks like a secret",
		"env=CONN=Server=x;Password=" + "p" + "w1":    "CONN looks like a secret",
		"secret=DB_PASSWORD=hunter2":                  "takes a Key Vault secret's address",
		"secret=API_TOKEN=" + w.vaultURL + "-missing": "Key Vault myvault holds no secret db-password-missing",
		"image=" + acrRef("myregistry", "x:1"):        "is not port=, cpu=, env= or secret=",
		"cpu=0.3":                                     "is not a size Azure runs",
		"cpu=4":                                       "is not a size Azure runs",
	} {
		r := runPlugin(t, runOpts{config: apply(nil)}, "containerapp create "+ownedRG+" new-app my-app-env "+acrRef("myregistry", "my-app:1.0")+" "+line)
		mustContain(t, r.text(), "Refused", want)
	}
	r := runPlugin(t, runOpts{config: apply(nil)}, "containerapp create "+ownedRG+" new-app my-app-env registry.example.com/nginx:1")
	mustContain(t, r.text(), "Refused", "is not an image in an Azure container registry")
	if len(w.f.writes()) != 0 {
		t.Fatal("a refused app wrote")
	}
}

func TestContainerAppEnvCreateSendsLogsToItsOwnWorkspace(t *testing.T) {
	w := newWorld(t)
	r := runPlugin(t, runOpts{config: apply(nil)}, "containerapp env create "+ownedRG+" new-env")
	if !r.ok() {
		t.Fatal(r.text())
	}
	ws := inRG(ownedRG, "Microsoft.OperationalInsights/workspaces/new-env-logs")
	env := inRG(ownedRG, "Microsoft.App/managedEnvironments/new-env")
	diag := w.f.callsTo(http.MethodPut, "/diagnosticSettings/yawble-logs")
	if len(diag) != 1 || !strings.HasPrefix(diag[0].Path, env+"/providers/") || !strings.Contains(string(diag[0].Body), ws) {
		t.Fatalf("diagnostic settings = %v", diag)
	}
	if ev := r.events("applied"); len(ev) != 1 || len(ev[0]["resourceIds"].([]any)) != 3 {
		t.Fatalf("applied = %v", ev)
	}
}

func TestContainerAppReadsAndRestart(t *testing.T) {
	w := newWorld(t)
	r := runPlugin(t, runOpts{}, "containerapp show their-app")
	mustContain(t, r.text(), "Container app their-app:", "not owned by this team (read only)", "address: their-app.apps")
	mustContain(t, runPlugin(t, runOpts{}, "containerapp revisions my-app").text(), "- my-app--rev1: active, running, healthy, 1 replicas, 100% of traffic")
	r = runPlugin(t, runOpts{}, "containerapp logs my-app 60")
	mustContain(t, r.text(), "Logs of my-app, the last 60 minutes (2 lines", "14:00:00Z  started\n2026-10-09T14:01:00Z  listening on 8080")
	ws := inRG(ownedRG, "Microsoft.OperationalInsights/workspaces/my-app-env-logs")
	q := w.f.callsTo(http.MethodPost, "/api/query")
	if len(q) != 1 || q[0].Host != armHost || q[0].Path != ws+"/api/query" || q[0].Token != tokARM ||
		q[0].Query != "api-version="+logsAPIVersion || !strings.Contains(string(q[0].Body), `"timespan":"PT60M"`) {
		t.Fatalf("the logs query = %+v", q)
	}
	r = runPlugin(t, runOpts{config: apply(nil)}, "containerapp restart my-app")
	mustContain(t, r.text(), "Restarted revision my-app--rev1 of my-app.")
	if c := w.f.callsTo(http.MethodPost, "/revisions/my-app--rev1/restart"); len(c) != 1 {
		t.Fatalf("restart calls = %v", c)
	}
}

// ---- domain bind ----

func TestDomainBindSetsRecordsInAnOwnedZoneThenBindsOnceTheyResolve(t *testing.T) {
	w := newWorld(t)
	cfg := apply(nil)
	app := inRG(ownedRG, "Microsoft.App/containerApps/my-app")

	r := runPlugin(t, runOpts{config: cfg}, "domain bind my-app www.example.com")
	if !r.ok() {
		t.Fatal(r.text())
	}
	mustContain(t, r.text(), "Set the records www.example.com needs in DNS zone example.com:", "- TXT asuid.www.example.com "+verifyID, "- CNAME www.example.com my-app.apps", "Run domain bind my-app www.example.com again")
	zone := inRG(ownedRG, "Microsoft.Network/dnsZones/example.com")
	if len(w.f.callsTo(http.MethodPut, zone+"/TXT/asuid.www")) != 1 || len(w.f.callsTo(http.MethodPut, zone+"/CNAME/www")) != 1 {
		t.Fatalf("records set: %v", w.f.writes())
	}
	if len(w.f.callsTo(http.MethodPut, "/managedCertificates/mc-www-example-com")) != 0 {
		t.Fatal("a certificate was asked for before the records resolve")
	}

	resolve(t, bound("www.example.com"))
	r = runPlugin(t, runOpts{config: cfg}, "domain bind my-app www.example.com")
	if !r.ok() {
		t.Fatal(r.text())
	}
	mustContain(t, r.text(), "Bound www.example.com to container app my-app with a managed certificate")
	cert := w.f.callsTo(http.MethodPut, "/managedCertificates/mc-www-example-com")
	if len(cert) != 1 || !strings.Contains(string(cert[0].Body), `"domainControlValidation":"CNAME"`) || !strings.Contains(string(cert[0].Body), `"subjectName":"www.example.com"`) {
		t.Fatalf("certificate = %v", cert)
	}
	patches := w.f.callsTo(http.MethodPatch, app)
	if len(patches) != 2 || !strings.Contains(string(patches[0].Body), `"bindingType":"Disabled"`) || !strings.Contains(string(patches[1].Body), `"bindingType":"SniEnabled"`) {
		t.Fatalf("app patches = %v", patches)
	}

	r = runPlugin(t, runOpts{config: cfg}, "domain bind my-app www.example.com")
	mustContain(t, r.text(), "already bound")
}

func TestDomainBindInAZoneElsewhereSaysWhatToSet(t *testing.T) {
	w := newWorld(t)
	r := runPlugin(t, runOpts{config: apply(nil)}, "domain bind my-app example.org")
	mustContain(t, r.text(), "example.org does not point at my-app yet. A person sets these records at the DNS host of example.org:",
		"- TXT asuid.example.org "+verifyID, "- A example.org "+staticIP)
	if len(w.f.writes()) != 0 {
		t.Fatal("domain bind wrote while the records do not resolve")
	}
}

// ---- SQL ----

func TestSQLServerCreateAcceptsOnlyEntraSignIns(t *testing.T) {
	w := newWorld(t)
	r := runPlugin(t, runOpts{config: apply(nil)}, "sql server create "+ownedRG+" new-sql")
	if !r.ok() {
		t.Fatal(r.text())
	}
	put := w.f.callsTo(http.MethodPut, "/servers/new-sql")
	var body struct {
		Properties map[string]any `json:"properties"`
	}
	_ = json.Unmarshal(put[0].Body, &body)
	admin, _ := body.Properties["administrators"].(map[string]any)
	if admin["azureADOnlyAuthentication"] != true || admin["sid"] != zeroGUID || admin["login"] != "person@example.com" {
		t.Fatalf("server = %s", put[0].Body)
	}
	if _, ok := body.Properties["administratorLoginPassword"]; ok {
		t.Fatal("the server has a SQL password")
	}
}

func TestSQLGrantMakesTheTeamsIdentityAUserWithListedRolesOnly(t *testing.T) {
	w := newWorld(t)
	r := runPlugin(t, runOpts{config: apply(nil)}, "sql grant my-sql/my-db "+w.ident.ID+" db_datareader db_datawriter")
	if !r.ok() {
		t.Fatal(r.text())
	}
	if len(w.sql.batches) != 1 {
		t.Fatalf("batches = %v", w.sql.batches)
	}
	mustContain(t, w.sql.batches[0], "CREATE USER [app-id] FROM EXTERNAL PROVIDER WITH OBJECT_ID = '"+principalA+"'",
		"ALTER ROLE [db_datareader] ADD MEMBER [app-id]", "ALTER ROLE [db_datawriter] ADD MEMBER [app-id]")

	for line, want := range map[string]string{
		"sql grant my-sql/my-db " + w.ident.ID + " db_owner":      "db_owner is not a database role this member grants",
		"sql grant my-sql/my-db person@example.com db_datareader": "is a user",
		"sql grant my-sql/my-db " + zeroGUID + " db_datareader":   "bare principal id",
	} {
		mustContain(t, runPlugin(t, runOpts{config: apply(nil)}, line).text(), "Refused", want)
	}
	if len(w.sql.batches) != 1 {
		t.Fatalf("a refused grant ran SQL: %v", w.sql.batches)
	}
}

func TestSQLErrorsStopTheFileAndSayWhichBatch(t *testing.T) {
	w := newWorld(t)
	w.sql.reply = func(sql string) []byte {
		if strings.HasPrefix(sql, "SELECT") {
			return append(tdsError(208, "Invalid object name 'orders'."), tdsDone(0x02, 0)...)
		}
		return tdsDone(0x10, 0)
	}
	r := runPlugin(t, runOpts{config: apply(nil)}, "sql exec my-sql/my-db "+w.sqlFile)
	mustContain(t, r.text(), "batch 2 of 2 failed", "error 208 (level 14, line 0): Invalid object name 'orders'.", "The batches before it ran:\nBatch 1: 0 rows affected.")
}

func TestSQLSignInWithTheWrongTokenIsRefusedWithTheNextStep(t *testing.T) {
	w := newWorld(t)
	tokens := map[string]string{"https://management.azure.com/": tokARM, "https://database.windows.net/": tokOther}
	r := runPlugin(t, runOpts{config: apply(nil), tokens: tokens}, "sql exec my-sql/my-db "+w.sqlFile)
	mustContain(t, r.text(), "Azure SQL refused the sign-in: error 18456", "needs to be the server's Microsoft Entra administrator")
}

func TestSQLResultsAreCappedAt200Rows(t *testing.T) {
	w := newWorld(t)
	w.sql.reply = func(string) []byte { return selectReply(250) }
	file := docFile(t, "q.sql", "SELECT id, name FROM orders")
	r := runPlugin(t, runOpts{config: apply(nil)}, "sql exec my-sql/my-db "+file)
	mustContain(t, r.text(), "250 rows (the first 200 shown)", "\n200 | ")
	if strings.Contains(r.text(), "\n201 | ") {
		t.Fatal("more than 200 rows were shown")
	}
}

func TestTDSReadsTheTypesAzureSQLAnswersWith(t *testing.T) {
	dt2 := []byte{8, 0, 0, 0, 0, 0, 0, 0, 0} // 8 bytes: 5 of time (scale 7), 3 of date
	// 13:30:15 is 48615 s = 486150000000 units of 100 ns.
	units := uint64(486150000000)
	for i := 0; i < 5; i++ {
		dt2[1+i] = byte(units >> (8 * i))
	}
	days := 739897 // 2026-10-09 from 0001-01-01
	dt2[6], dt2[7], dt2[8] = byte(days), byte(days>>8), byte(days>>16)
	guid := append([]byte{16}, 0x33, 0x22, 0x11, 0x00, 0x55, 0x44, 0x77, 0x66, 0x88, 0x99, 0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF)
	dec := []byte{5, 0, 0x39, 0x30, 0, 0} // -123.45 at scale 2
	stream := tdsColumns(tdsColumn{"n", colInt}, tdsColumn{"b", colBit}, tdsColumn{"d", colDecimal}, tdsColumn{"t", colDT2}, tdsColumn{"g", colGUID}, tdsColumn{"s", colNVarchar})
	stream = append(stream, tdsRow(vInt(-7), []byte{1, 1}, dec, dt2, guid, vNVarchar("héllo"))...)
	// An NBCROW with every column NULL.
	stream = append(stream, 0xD2, 0x3F)
	stream = append(stream, tdsRow(vNull, vNull, vNull, vNull, vNull, []byte{0xFF, 0xFF})...)
	stream = append(stream, tdsDone(0x10, 3)...)

	out := readTokens(stream)
	if out.bad != "" || len(out.sets) != 1 {
		t.Fatalf("bad %q, sets %d", out.bad, len(out.sets))
	}
	rows := out.sets[0].rows
	want := []string{"-7", "1", "-123.45", "2026-10-09 13:30:15", "00112233" + "-4455-6677-8899-" + "AABBCCDDEEFF", "héllo"}
	if strings.Join(rows[0], "|") != strings.Join(want, "|") {
		t.Fatalf("row = %v, want %v", rows[0], want)
	}
	for _, v := range append(rows[1], rows[2]...) {
		if v != "NULL" {
			t.Fatalf("null rows = %v %v", rows[1], rows[2])
		}
	}
}

// ---- setup checks the providers these commands use ----

func TestSetupChecksTheDataPlaneProviders(t *testing.T) {
	f := newFake(t)
	f.providers["Microsoft.ContainerRegistry"] = "NotRegistered"
	f.providers["Microsoft.Sql"] = "NotRegistered"
	f.providers["Microsoft.Storage"] = "NotRegistered"
	f.providers["Microsoft.Insights"] = "NotRegistered"
	r := runPlugin(t, runOpts{}, "setup")
	mustContain(t, r.text(), "providers not registered: Microsoft.Storage, Microsoft.ContainerRegistry, Microsoft.Sql, Microsoft.Insights; whatif registers nothing")
}

// An app is 0.5 CPU and 1 GiB unless cpu= names another size, which gets twice as many GiB.
func TestContainerAppCreateTakesASize(t *testing.T) {
	for cpu, want := range map[string]string{"": `"resources":{"cpu":0.5,"memory":"1Gi"}`, " cpu=0.25": `"resources":{"cpu":0.25,"memory":"0.5Gi"}`, " cpu=1.5": `"resources":{"cpu":1.5,"memory":"3Gi"}`} {
		w := newWorld(t)
		r := runPlugin(t, runOpts{config: apply(nil)}, "containerapp create "+ownedRG+" new-app my-app-env "+acrRef("myregistry", "my-app:1.0")+" port=8080"+cpu)
		if !r.ok() {
			t.Fatal(r.text())
		}
		put := w.f.callsTo(http.MethodPut, "/containerApps/new-app")
		if len(put) != 1 || !strings.Contains(string(put[0].Body), want) {
			t.Fatalf("cpu%q: the app was created as %s", cpu, put[len(put)-1].Body)
		}
	}
	newWorld(t)
	r := runPlugin(t, runOpts{}, "containerapp create "+ownedRG+" new-app my-app-env "+acrRef("myregistry", "my-app:1.0")+" cpu=0.25")
	mustContain(t, r.text(), "0.25 CPU and 0.5 GiB of memory")
}
