package main

import (
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Storage: accounts and their containers, tables and queues are created through Azure Resource
// Manager; blobs, entities and messages go to the account's own addresses with the Azure Storage
// token. Accounts are created with shared key access off, and the plugin never asks for a key.

const (
	blobVersion  = "2023-11-03"
	queueVersion = "2023-11-03"
	tableVersion = "2019-02-02"
	maxBlobBytes = 256 << 20
)

var (
	storageName   = regexp.MustCompile(`^[a-z0-9]{3,24}$`)
	containerName = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9]|-[a-z0-9]){2,62}$`)
	tableName     = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]{2,62}$`)
	queueName     = containerName
	storageSKUs   = []string{"Standard_LRS", "Standard_ZRS", "Standard_GRS", "Standard_RAGRS", "Standard_GZRS", "Standard_RAGZRS", "Premium_LRS", "Premium_ZRS"}
)

// storageSuffix is each service's address after the account's name.
var storageSuffix = map[string]string{"blob": ".blob.core.windows.net", "table": ".table.core.windows.net", "queue": ".queue.core.windows.net"}

// storageHost is an account's address for one service: blob, table or queue.
func storageHost(account, service string) string {
	return "https://" + account + storageSuffix[service]
}

func init() {
	register(
		command{words: []string{"storage", "create"}, usage: "<resource group> <account> [sku]", run: (*plugin).storageCreate},
		command{words: []string{"storage", "container", "create"}, usage: "<account> <container>", run: storageChild("container")},
		command{words: []string{"storage", "table", "create"}, usage: "<account> <table>", run: storageChild("table")},
		command{words: []string{"storage", "queue", "create"}, usage: "<account> <queue>", run: storageChild("queue")},
		command{words: []string{"blob", "list"}, usage: "<account> <container> [prefix]", run: (*plugin).blobList},
		command{words: []string{"blob", "get"}, usage: "<account> <container> <blob> [path in the team's documents]", run: (*plugin).blobGet},
		command{words: []string{"blob", "put"}, usage: "<account> <container> <blob> <file>", run: (*plugin).blobPut},
		command{words: []string{"blob", "delete"}, usage: "<account> <container> <blob>", run: (*plugin).blobDelete},
		command{words: []string{"table", "query"}, usage: "<account> <table> [filter]", run: (*plugin).tableQuery},
		command{words: []string{"table", "upsert"}, usage: "<account> <table> <partition key> <row key> '<JSON properties>'", run: (*plugin).tableUpsert},
		command{words: []string{"table", "delete"}, usage: "<account> <table> <partition key> <row key>", run: (*plugin).tableDelete},
		command{words: []string{"queue", "send"}, usage: "<account> <queue> <text>", run: (*plugin).queueSend},
		command{words: []string{"queue", "peek"}, usage: "<account> <queue> [count]", run: (*plugin).queuePeek},
		command{words: []string{"queue", "receive"}, usage: "<account> <queue> [count]", run: (*plugin).queueReceive},
		command{words: []string{"queue", "delete"}, usage: "<account> <queue> <message id> <pop receipt>", run: (*plugin).queueDelete},
	)
}

// storageDataRoles are what the signed-in person is given on a storage account the team creates:
// Storage Blob Data Reader first, which containerapp logs needs to read the logs an environment
// archives there, then the three Data Contributor roles the data commands use.
var storageDataRoles = []string{roleBlobDataReader, roleBlobDataContributor, roleTableDataContributor, roleQueueDataContributor}

func (p *plugin) storageCreate(args []string) (string, error) {
	if err := argsExactly(args, 2, 3, "storage create", "<resource group> <account> [sku]"); err != nil {
		return "", err
	}
	if err := p.needSubscription(); err != nil {
		return "", err
	}
	rg, name, sku := args[0], args[1], "Standard_LRS"
	if !storageName.MatchString(name) {
		return "", refusef("Refused: %q is not a storage account name: 3 to 24 lower-case letters and digits, unique in all of Azure.", name)
	}
	if len(args) == 3 {
		found := false
		for _, s := range storageSKUs {
			if strings.EqualFold(s, args[2]) {
				sku, found = s, true
			}
		}
		if !found {
			return "", refusef("Refused: %s is not a storage sku; use one of %s.", args[2], strings.Join(storageSKUs, ", "))
		}
	}
	id := rgPath(p.cfg.Subscription, rg) + "/providers/Microsoft.Storage/storageAccounts/" + name
	if err := p.mustOwn(change{id: id, kind: changeCreate}); err != nil {
		return "", err
	}
	what := fmt.Sprintf("create storage account %s (%s) in %s with shared key access off, tagged for this team, and give the signed-in person Storage Blob Data Reader and Storage Blob, Table and Queue Data Contributor on it", name, sku, rg)
	if !p.cfg.apply() {
		return p.whatif("storage create", id, what, map[string]int{"create": 1}), nil
	}
	location, err := p.rgLocation(rg)
	if err != nil {
		return "", err
	}
	body := map[string]any{
		"location": location, "kind": "StorageV2", "sku": map[string]any{"name": sku}, "tags": p.ownTags(),
		"properties": map[string]any{
			"allowSharedKeyAccess":         false,
			"defaultToOAuthAuthentication": true,
			"allowBlobPublicAccess":        false,
			"minimumTlsVersion":            "TLS1_2",
			"supportsHttpsTrafficOnly":     true,
		},
	}
	if _, _, err := p.put(id, body, "the storage account "+name); err != nil {
		return "", err
	}
	// THE PERSON READS AND WRITES IT TOO. An Owner of the subscription holds no data rights on a
	// storage account, and with shared key access off every blob, table and queue command this
	// member runs signed in as them would be refused, as would containerapp logs reading the logs
	// an environment archives there. So the account it creates, and only that, grants the
	// signed-in person Storage Blob Data Reader and the three Data Contributor roles; it never
	// grants a user anything on any other scope.
	ids := []string{id}
	oid, _, account, err := p.signedIn()
	if err != nil {
		return "", err
	}
	note := ""
	for _, role := range storageDataRoles {
		assignment, _, err := p.grantRoleTo(id, role, oid, "User")
		if isStatus(err, http.StatusForbidden) {
			// Said once: the same refusal would answer every role after it.
			note = " " + mayNotGrant(account, id, storageDataRoles)
			break
		}
		if err != nil {
			p.applied("storage create", id, "partly: "+what, ids)
			return "", fmt.Errorf("storage account %s was created, but %s could not be given %s on it: %w; run storage create again", name, account, grantable[role], err)
		}
		ids = append(ids, assignment)
	}
	p.applied("storage create", id, what, ids)
	if note != "" {
		return fmt.Sprintf("Storage account %s is ready in %s, with shared key access off: everything reaches it signed in.%s Grant a managed identity a Storage Data Contributor role with role assign for an app to use it.", name, rg, note), nil
	}
	return fmt.Sprintf("Storage account %s is ready in %s, with shared key access off: everything reaches it signed in. %s holds Storage Blob Data Reader (so containerapp logs reads the logs archived there) and Storage Blob, Table and Queue Data Contributor on it, which can take a few minutes to apply. Grant a managed identity a Storage Data Contributor role with role assign for an app to use it.", name, rg, account), nil
}

// mayNotGrant is the one sentence for roles Azure refused to let the signed-in account give itself
// on a storage account, with the fix.
func mayNotGrant(account, scope string, roles []string) string {
	names := make([]string, len(roles))
	for i, r := range roles {
		names[i] = grantable[r]
	}
	storage := scope[strings.LastIndex(scope, "/")+1:]
	return fmt.Sprintf("%s may not assign roles on storage account %s, so it was not given %s there. A person with Owner, User Access Administrator or Role Based Access Control Administrator on it gives %s those roles in the Azure portal (the storage account, Access control (IAM), Add role assignment); until then blob commands and containerapp logs reading the logs archived there are refused.",
		account, storage, strings.Join(names, ", "), account)
}

// grantSignedInReader gives the signed-in account Storage Blob Data Reader on a storage account the
// team owns that one of its environments archives logs to, so containerapp logs can read them. It
// answers the assignment's id, "" when none was made, and the sentence for the command's output.
// The guard judges the grant as any other; only a storage account the team owns takes it, and the
// principal is the signed-in account read from its own token, never a user or group it is told of.
func (p *plugin) grantSignedInReader(command, storageID, env string) (assignment, said string, err error) {
	storage := storageID[strings.LastIndex(storageID, "/")+1:]
	why, err := p.guard(change{id: storageID, kind: changeGrant, role: roleBlobDataReader})
	if err != nil {
		return "", "", err
	}
	oid, _, account, signErr := p.signedIn()
	if signErr != nil {
		account = "the signed-in account"
	}
	if why != "" {
		reason := fmt.Sprintf("%s was not given Storage Blob Data Reader on storage account %s, where environment %s archives its logs: %s, and this member grants only on what its team owns. Its owner gives %s that role in the Azure portal (the storage account, Access control (IAM), Add role assignment) for containerapp logs to read them.", account, storage, env, why, account)
		p.refuse(command, reason)
		return "", reason, nil
	}
	if signErr != nil {
		return "", signErr.Error(), nil
	}
	assignment, _, err = p.grantRoleTo(storageID, roleBlobDataReader, oid, "User")
	if isStatus(err, http.StatusForbidden) {
		return "", mayNotGrant(account, storageID, []string{roleBlobDataReader}), nil
	}
	if err != nil {
		return "", fmt.Sprintf("%s could not be given Storage Blob Data Reader on storage account %s: %v; run %s again.", account, storage, err, command), nil
	}
	return assignment, fmt.Sprintf("%s holds Storage Blob Data Reader on storage account %s, where environment %s archives its logs, so containerapp logs reads them (the role can take a few minutes to apply).", account, storage, env), nil
}

// storageChild creates a blob container, a table or a queue in an account the team owns.
func storageChild(kind string) func(p *plugin, args []string) (string, error) {
	return func(p *plugin, args []string) (string, error) {
		words := "storage " + kind + " create"
		if err := argsExactly(args, 2, 2, words, "<account> <"+kind+">"); err != nil {
			return "", err
		}
		if err := p.needSubscription(); err != nil {
			return "", err
		}
		form, path := containerName, "/blobServices/default/containers/"
		switch kind {
		case "table":
			form, path = tableName, "/tableServices/default/tables/"
		case "queue":
			form, path = queueName, "/queueServices/default/queues/"
		}
		name := args[1]
		if !form.MatchString(name) {
			return "", refusef("Refused: %q is not a %s name Azure accepts.", name, kind)
		}
		account, err := p.findByName("Microsoft.Storage/storageAccounts", args[0], "storage account")
		if err != nil {
			return "", err
		}
		id := account.ID + path + name
		if err := p.mustOwn(change{id: id, kind: changeCreate}); err != nil {
			return "", err
		}
		what := fmt.Sprintf("create %s %s in storage account %s", kind, name, account.Name)
		if !p.cfg.apply() {
			return p.whatif(words, id, what, map[string]int{"create": 1}), nil
		}
		r, _ := parseID(account.ID)
		version := versionOf(r.typ())
		if _, err := p.az.arm(http.MethodPut, id+"?api-version="+version, map[string]any{"properties": map[string]any{}}, nil); err != nil {
			return "", err
		}
		p.applied(words, id, what, []string{id})
		return fmt.Sprintf("Created %s %s in storage account %s.", kind, name, account.Name), nil
	}
}

// storageAccount finds an account by name. A change (write is true) must be to an account the team
// owns; a read goes anywhere the person can read.
func (p *plugin) storageAccount(name string, write bool) (resource, error) {
	if !storageName.MatchString(name) {
		return resource{}, refusef("Refused: %q is not a storage account name.", name)
	}
	if err := p.needSubscription(); err != nil {
		return resource{}, err
	}
	account, err := p.findByName("Microsoft.Storage/storageAccounts", name, "storage account")
	if err != nil {
		return resource{}, err
	}
	if write {
		if err := p.mustOwn(change{id: account.ID, kind: changeModify}); err != nil {
			return resource{}, err
		}
	}
	return account, nil
}

// needDelete refuses a delete while allowDelete is off, in either mode.
func (p *plugin) needDelete() error {
	if !p.cfg.AllowDelete {
		return refusal("Refused: allowDelete is off, so this member deletes nothing, in whatif or apply. A person turns allowDelete on in this member's settings.")
	}
	return nil
}

// ---- blobs ----

// blobURL is a blob's address, each part of its name escaped.
func blobURL(account, container, blob string) string {
	parts := strings.Split(blob, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return storageHost(account, "blob") + "/" + container + "/" + strings.Join(parts, "/")
}

func checkBlobArgs(container, blob string) error {
	if !containerName.MatchString(container) && container != "$web" && container != "$root" {
		return refusef("Refused: %q is not a container name.", container)
	}
	if blob == "" || len(blob) > 1024 || strings.HasPrefix(blob, "/") {
		return refusef("Refused: %q is not a blob name.", blob)
	}
	return nil
}

func (p *plugin) blobList(args []string) (string, error) {
	if err := argsExactly(args, 2, 3, "blob list", "<account> <container> [prefix]"); err != nil {
		return "", err
	}
	if err := checkBlobArgs(args[1], "x"); err != nil {
		return "", err
	}
	account, err := p.storageAccount(args[0], false)
	if err != nil {
		return "", err
	}
	q := url.Values{"restype": {"container"}, "comp": {"list"}, "maxresults": {"200"}}
	if len(args) == 3 {
		q.Set("prefix", args[2])
	}
	ans, err := p.az.send(apiStorage, http.MethodGet, storageHost(account.Name, "blob")+"/"+args[1]+"?"+q.Encode(), nil, map[string]string{"x-ms-version": blobVersion})
	if err != nil {
		return "", err
	}
	var list struct {
		Blobs []struct {
			Name       string `xml:"Name"`
			Properties struct {
				Length   int64  `xml:"Content-Length"`
				Modified string `xml:"Last-Modified"`
			} `xml:"Properties"`
		} `xml:"Blobs>Blob"`
		NextMarker string `xml:"NextMarker"`
	}
	if err := xml.Unmarshal(ans.body, &list); err != nil {
		return "", fmt.Errorf("Azure Storage's blob list could not be read")
	}
	if len(list.Blobs) == 0 {
		return fmt.Sprintf("Container %s in %s holds no blobs%s.", args[1], account.Name, prefixNote(args)), nil
	}
	var lines []string
	for _, b := range list.Blobs {
		lines = append(lines, fmt.Sprintf("- %s (%d bytes, %s)", b.Name, b.Properties.Length, b.Properties.Modified))
	}
	more := ""
	if list.NextMarker != "" {
		more = "\nThe first 200 are shown; give a prefix to see others."
	}
	return fmt.Sprintf("Blobs in %s/%s%s (%d):\n%s%s", account.Name, args[1], prefixNote(args), len(lines), strings.Join(lines, "\n"), more), nil
}

func prefixNote(args []string) string {
	if len(args) == 3 {
		return " starting " + args[2]
	}
	return ""
}

func (p *plugin) blobGet(args []string) (string, error) {
	if err := argsExactly(args, 3, 4, "blob get", "<account> <container> <blob> [path in the team's documents]"); err != nil {
		return "", err
	}
	if err := checkBlobArgs(args[1], args[2]); err != nil {
		return "", err
	}
	dest := filepath.Base(args[2])
	if len(args) == 4 {
		dest = args[3]
	}
	target, rel, err := p.documentsTarget(dest, "blob get")
	if err != nil {
		return "", err
	}
	account, err := p.storageAccount(args[0], false)
	if err != nil {
		return "", err
	}
	ans, err := p.az.send(apiStorage, http.MethodGet, blobURL(account.Name, args[1], args[2]), nil, map[string]string{"x-ms-version": blobVersion})
	if err != nil {
		return "", err
	}
	if len(ans.body) > maxBlobBytes {
		return "", refusef("Refused: %s is larger than %d MB, the most blob get copies.", args[2], maxBlobBytes>>20)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return "", fmt.Errorf("the folder for %s could not be made", dest)
	}
	if real, err := filepath.EvalSymlinks(filepath.Dir(target)); err != nil || !p.insideTeamFolders(real) {
		return "", refusef("Refused: %s leads outside the team's documents folder.", dest)
	}
	if err := os.WriteFile(target, ans.body, 0o644); err != nil {
		return "", fmt.Errorf("%s could not be written", dest)
	}
	return fmt.Sprintf("Copied %s/%s/%s (%d bytes) to %s in the team's documents.", account.Name, args[1], args[2], len(ans.body), rel), nil
}

func (p *plugin) blobPut(args []string) (string, error) {
	if err := argsExactly(args, 4, 4, "blob put", "<account> <container> <blob> <file>"); err != nil {
		return "", err
	}
	if err := checkBlobArgs(args[1], args[2]); err != nil {
		return "", err
	}
	file, err := p.allowedPath(args[3], "uploads only files from those")
	if err != nil {
		return "", err
	}
	info, err := os.Stat(file)
	if err != nil || !info.Mode().IsRegular() {
		return "", refusef("Refused: %s is not a file.", args[3])
	}
	if info.Size() > maxBlobBytes {
		return "", refusef("Refused: %s is larger than %d MB, the most blob put uploads.", args[3], maxBlobBytes>>20)
	}
	account, err := p.storageAccount(args[0], true)
	if err != nil {
		return "", err
	}
	target := account.ID + "/blobServices/default/containers/" + args[1]
	what := fmt.Sprintf("upload %s (%d bytes) to %s/%s/%s", filepath.Base(file), info.Size(), account.Name, args[1], args[2])
	if !p.cfg.apply() {
		return p.whatif("blob put", target, what, map[string]int{"modify": 1}), nil
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return "", refusef("Refused: %s cannot be read.", args[3])
	}
	headers := map[string]string{"x-ms-version": blobVersion, "x-ms-blob-type": "BlockBlob", "Content-Type": "application/octet-stream"}
	if _, err := p.az.send(apiStorage, http.MethodPut, blobURL(account.Name, args[1], args[2]), raw, headers); err != nil {
		return "", err
	}
	p.applied("blob put", target, what, []string{target})
	return fmt.Sprintf("Uploaded %s to %s/%s/%s (%d bytes).", filepath.Base(file), account.Name, args[1], args[2], len(raw)), nil
}

func (p *plugin) blobDelete(args []string) (string, error) {
	if err := argsExactly(args, 3, 3, "blob delete", "<account> <container> <blob>"); err != nil {
		return "", err
	}
	if err := p.needDelete(); err != nil {
		return "", err
	}
	if err := checkBlobArgs(args[1], args[2]); err != nil {
		return "", err
	}
	account, err := p.storageAccount(args[0], true)
	if err != nil {
		return "", err
	}
	target := account.ID + "/blobServices/default/containers/" + args[1]
	what := fmt.Sprintf("delete blob %s/%s/%s", account.Name, args[1], args[2])
	if !p.cfg.apply() {
		return p.whatif("blob delete", target, what, map[string]int{"delete": 1}), nil
	}
	_, err = p.az.send(apiStorage, http.MethodDelete, blobURL(account.Name, args[1], args[2]), nil, map[string]string{"x-ms-version": blobVersion})
	if isNotFound(err) {
		return fmt.Sprintf("Blob %s/%s/%s was already gone.", account.Name, args[1], args[2]), nil
	}
	if err != nil {
		return "", err
	}
	p.applied("blob delete", target, what, []string{target})
	return fmt.Sprintf("Deleted blob %s/%s/%s.", account.Name, args[1], args[2]), nil
}

// ---- tables ----

var tableHeaders = map[string]string{"x-ms-version": tableVersion, "Accept": "application/json;odata=nometadata", "DataServiceVersion": "3.0;NetFx"}

// entityURL is one entity's address; a quote in a key is doubled, as OData asks.
func entityURL(account, table, pk, rk string) string {
	q := func(s string) string { return url.PathEscape(strings.ReplaceAll(s, "'", "''")) }
	return fmt.Sprintf("%s/%s(PartitionKey='%s',RowKey='%s')", storageHost(account, "table"), table, q(pk), q(rk))
}

func checkTable(table string) error {
	if !tableName.MatchString(table) {
		return refusef("Refused: %q is not a table name.", table)
	}
	return nil
}

func (p *plugin) tableQuery(args []string) (string, error) {
	if len(args) < 2 {
		return "", refusal("Refused: table query takes <account> <table> [filter], such as PartitionKey eq 'orders'.")
	}
	if err := checkTable(args[1]); err != nil {
		return "", err
	}
	account, err := p.storageAccount(args[0], false)
	if err != nil {
		return "", err
	}
	q := url.Values{"$top": {"200"}}
	if len(args) > 2 {
		q.Set("$filter", strings.Join(args[2:], " "))
	}
	var out struct {
		Value []map[string]any `json:"value"`
	}
	if _, err := p.az.data(apiStorage, http.MethodGet, storageHost(account.Name, "table")+"/"+args[1]+"()?"+q.Encode(), nil, tableHeaders, &out); err != nil {
		return "", err
	}
	if len(out.Value) == 0 {
		return fmt.Sprintf("No entities in %s/%s match.", account.Name, args[1]), nil
	}
	var lines []string
	for _, e := range out.Value {
		raw, _ := json.Marshal(e)
		lines = append(lines, "- "+string(raw))
	}
	return fmt.Sprintf("Entities in %s/%s (%d, at most 200):\n%s", account.Name, args[1], len(lines), strings.Join(lines, "\n")), nil
}

func (p *plugin) tableUpsert(args []string) (string, error) {
	if len(args) < 5 {
		return "", refusal(`Refused: table upsert takes <account> <table> <partition key> <row key> <JSON properties>, the JSON in single quotes, such as '{"status":"open"}'.`)
	}
	if err := checkTable(args[1]); err != nil {
		return "", err
	}
	var entity map[string]any
	if err := json.Unmarshal([]byte(strings.Join(args[4:], " ")), &entity); err != nil {
		return "", refusal(`Refused: the properties must be one JSON object in single quotes, such as '{"status":"open"}'.`)
	}
	entity["PartitionKey"], entity["RowKey"] = args[2], args[3]
	account, err := p.storageAccount(args[0], true)
	if err != nil {
		return "", err
	}
	target := account.ID + "/tableServices/default/tables/" + args[1]
	what := fmt.Sprintf("insert or replace entity (%s, %s) in %s/%s", args[2], args[3], account.Name, args[1])
	if !p.cfg.apply() {
		return p.whatif("table upsert", target, what, map[string]int{"modify": 1}), nil
	}
	if _, err := p.az.send(apiStorage, http.MethodPut, entityURL(account.Name, args[1], args[2], args[3]), entity, tableHeaders); err != nil {
		return "", err
	}
	p.applied("table upsert", target, what, []string{target})
	return fmt.Sprintf("Wrote entity (%s, %s) in %s/%s.", args[2], args[3], account.Name, args[1]), nil
}

func (p *plugin) tableDelete(args []string) (string, error) {
	if err := argsExactly(args, 4, 4, "table delete", "<account> <table> <partition key> <row key>"); err != nil {
		return "", err
	}
	if err := p.needDelete(); err != nil {
		return "", err
	}
	if err := checkTable(args[1]); err != nil {
		return "", err
	}
	account, err := p.storageAccount(args[0], true)
	if err != nil {
		return "", err
	}
	target := account.ID + "/tableServices/default/tables/" + args[1]
	what := fmt.Sprintf("delete entity (%s, %s) in %s/%s", args[2], args[3], account.Name, args[1])
	if !p.cfg.apply() {
		return p.whatif("table delete", target, what, map[string]int{"delete": 1}), nil
	}
	headers := map[string]string{"If-Match": "*"}
	for k, v := range tableHeaders {
		headers[k] = v
	}
	_, err = p.az.send(apiStorage, http.MethodDelete, entityURL(account.Name, args[1], args[2], args[3]), nil, headers)
	if isNotFound(err) {
		return fmt.Sprintf("Entity (%s, %s) in %s/%s was already gone.", args[2], args[3], account.Name, args[1]), nil
	}
	if err != nil {
		return "", err
	}
	p.applied("table delete", target, what, []string{target})
	return fmt.Sprintf("Deleted entity (%s, %s) in %s/%s.", args[2], args[3], account.Name, args[1]), nil
}

// ---- queues ----

type queueMessages struct {
	Messages []struct {
		ID         string `xml:"MessageId"`
		Inserted   string `xml:"InsertionTime"`
		Dequeued   int    `xml:"DequeueCount"`
		PopReceipt string `xml:"PopReceipt"`
		Text       string `xml:"MessageText"`
	} `xml:"QueueMessage"`
}

func checkQueue(queue string) error {
	if !queueName.MatchString(queue) {
		return refusef("Refused: %q is not a queue name.", queue)
	}
	return nil
}

// messageText is a message's text: base64 is how the plugin sends it, and how Azure Functions and
// most libraries read and write it; other text is shown as it is.
func messageText(s string) string {
	if raw, err := base64.StdEncoding.DecodeString(s); err == nil && isPrintable(raw) {
		return string(raw)
	}
	return s
}

func isPrintable(b []byte) bool {
	for _, r := range string(b) {
		if r == '�' || (r < 32 && r != '\n' && r != '\t' && r != '\r') {
			return false
		}
	}
	return true
}

func (p *plugin) queueSend(args []string) (string, error) {
	if len(args) < 3 {
		return "", refusal("Refused: queue send takes <account> <queue> <text>.")
	}
	if err := checkQueue(args[1]); err != nil {
		return "", err
	}
	text := strings.Join(args[2:], " ")
	encoded := base64.StdEncoding.EncodeToString([]byte(text))
	if len(encoded) > 64<<10 {
		return "", refusal("Refused: the message is larger than 64 KB, Azure's limit for a queue message.")
	}
	account, err := p.storageAccount(args[0], true)
	if err != nil {
		return "", err
	}
	target := account.ID + "/queueServices/default/queues/" + args[1]
	what := fmt.Sprintf("send a message of %d characters to %s/%s", len(text), account.Name, args[1])
	if !p.cfg.apply() {
		return p.whatif("queue send", target, what, map[string]int{"modify": 1}), nil
	}
	var body strings.Builder
	body.WriteString("<QueueMessage><MessageText>")
	_ = xml.EscapeText(&body, []byte(encoded))
	body.WriteString("</MessageText></QueueMessage>")
	ans, err := p.az.send(apiStorage, http.MethodPost, storageHost(account.Name, "queue")+"/"+args[1]+"/messages", []byte(body.String()),
		map[string]string{"x-ms-version": queueVersion, "Content-Type": "application/xml"})
	if err != nil {
		return "", err
	}
	var sent queueMessages
	_ = xml.Unmarshal(ans.body, &sent)
	id := ""
	if len(sent.Messages) > 0 {
		id = " as message " + sent.Messages[0].ID
	}
	p.applied("queue send", target, what, []string{target})
	return fmt.Sprintf("Sent the message to %s/%s%s.", account.Name, args[1], id), nil
}

// queueCount reads an optional count of messages, 1 to 32.
func queueCount(args []string, words string) (int, error) {
	if len(args) < 3 {
		return 1, nil
	}
	n, err := strconv.Atoi(args[2])
	if err != nil || n < 1 || n > 32 {
		return 0, refusef("Refused: %s takes a count from 1 to 32.", words)
	}
	return n, nil
}

func (p *plugin) queuePeek(args []string) (string, error) {
	if err := argsExactly(args, 2, 3, "queue peek", "<account> <queue> [count]"); err != nil {
		return "", err
	}
	if err := checkQueue(args[1]); err != nil {
		return "", err
	}
	n, err := queueCount(args, "queue peek")
	if err != nil {
		return "", err
	}
	account, err := p.storageAccount(args[0], false)
	if err != nil {
		return "", err
	}
	msgs, err := p.queueGet(account.Name, args[1], url.Values{"peekonly": {"true"}, "numofmessages": {strconv.Itoa(n)}})
	if err != nil {
		return "", err
	}
	if len(msgs.Messages) == 0 {
		return fmt.Sprintf("Queue %s/%s is empty.", account.Name, args[1]), nil
	}
	var lines []string
	for _, m := range msgs.Messages {
		lines = append(lines, fmt.Sprintf("- %s (%s, dequeued %d times): %s", m.ID, m.Inserted, m.Dequeued, messageText(m.Text)))
	}
	return fmt.Sprintf("The first messages in %s/%s, left in the queue:\n%s", account.Name, args[1], strings.Join(lines, "\n")), nil
}

func (p *plugin) queueReceive(args []string) (string, error) {
	if err := argsExactly(args, 2, 3, "queue receive", "<account> <queue> [count]"); err != nil {
		return "", err
	}
	if err := checkQueue(args[1]); err != nil {
		return "", err
	}
	n, err := queueCount(args, "queue receive")
	if err != nil {
		return "", err
	}
	account, err := p.storageAccount(args[0], true)
	if err != nil {
		return "", err
	}
	target := account.ID + "/queueServices/default/queues/" + args[1]
	what := fmt.Sprintf("receive up to %d messages from %s/%s, hiding them from other readers for 5 minutes", n, account.Name, args[1])
	if !p.cfg.apply() {
		return p.whatif("queue receive", target, what, map[string]int{"modify": 1}), nil
	}
	msgs, err := p.queueGet(account.Name, args[1], url.Values{"numofmessages": {strconv.Itoa(n)}, "visibilitytimeout": {"300"}})
	if err != nil {
		return "", err
	}
	if len(msgs.Messages) == 0 {
		return fmt.Sprintf("Queue %s/%s had no message to receive.", account.Name, args[1]), nil
	}
	var lines []string
	for _, m := range msgs.Messages {
		lines = append(lines, fmt.Sprintf("- %s, pop receipt %s: %s", m.ID, m.PopReceipt, messageText(m.Text)))
	}
	p.applied("queue receive", target, fmt.Sprintf("received %d messages from %s/%s", len(lines), account.Name, args[1]), []string{target})
	return fmt.Sprintf("Received %d messages from %s/%s. Each comes back in 5 minutes unless it is deleted with queue delete <account> <queue> <message id> <pop receipt>:\n%s",
		len(lines), account.Name, args[1], strings.Join(lines, "\n")), nil
}

func (p *plugin) queueGet(account, queue string, q url.Values) (queueMessages, error) {
	var msgs queueMessages
	ans, err := p.az.send(apiStorage, http.MethodGet, storageHost(account, "queue")+"/"+queue+"/messages?"+q.Encode(), nil, map[string]string{"x-ms-version": queueVersion})
	if err != nil {
		return msgs, err
	}
	if err := xml.Unmarshal(ans.body, &msgs); err != nil {
		return msgs, fmt.Errorf("Azure Storage's messages could not be read")
	}
	return msgs, nil
}

func (p *plugin) queueDelete(args []string) (string, error) {
	if err := argsExactly(args, 4, 4, "queue delete", "<account> <queue> <message id> <pop receipt>"); err != nil {
		return "", err
	}
	if err := p.needDelete(); err != nil {
		return "", err
	}
	if err := checkQueue(args[1]); err != nil {
		return "", err
	}
	account, err := p.storageAccount(args[0], true)
	if err != nil {
		return "", err
	}
	target := account.ID + "/queueServices/default/queues/" + args[1]
	what := fmt.Sprintf("delete message %s from %s/%s", args[2], account.Name, args[1])
	if !p.cfg.apply() {
		return p.whatif("queue delete", target, what, map[string]int{"delete": 1}), nil
	}
	u := storageHost(account.Name, "queue") + "/" + args[1] + "/messages/" + url.PathEscape(args[2]) + "?popreceipt=" + url.QueryEscape(args[3])
	_, err = p.az.send(apiStorage, http.MethodDelete, u, nil, map[string]string{"x-ms-version": queueVersion})
	if isNotFound(err) {
		return fmt.Sprintf("Message %s in %s/%s was already gone, or its pop receipt is out of date.", args[2], account.Name, args[1]), nil
	}
	if err != nil {
		return "", err
	}
	p.applied("queue delete", target, what, []string{target})
	return fmt.Sprintf("Deleted message %s from %s/%s.", args[2], account.Name, args[1]), nil
}
