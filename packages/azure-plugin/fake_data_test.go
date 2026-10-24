package main

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
)

// The fake's data planes and actions: a registry's build, container apps and their environments,
// storage accounts' blobs, tables and queues, and through Azure management a vault's secret names
// and a workspace's log query. Each data-plane host answers only its own API's token, and no
// Key Vault or Log Analytics host is served at all.

// Values shaped like the short-lived permissions Azure hands out, put together at run time. No
// output may carry them.
var (
	uploadSig = "sig=" + strings.Repeat("u", 20) + "%2B" + strings.Repeat("U", 20)
	logSig    = "sig=" + strings.Repeat("g", 20) + "%2F" + strings.Repeat("G", 20)
	uploadURL = "https://acrsource" + ".blob.core.windows.net/source/upload.tar.gz?sv=2023-11-03&se=2026-10-09&" + uploadSig
	logURL    = "https://acrlogs" + ".blob.core.windows.net/logs/run.txt?sv=2023-11-03&" + logSig
)

func init() { allTokens = append(allTokens, uploadSig, logSig) }

const (
	staticIP     = "192.0.2.10"
	verifyID     = "0123456789ABCDEF0123456789ABCDEF"
	workspaceCID = "00000000-0000-0000-0000-0000000000c1"
	principalA   = "00000000-0000-0000-0000-0000000000a1"
	buildDigest  = "sha256:" + "0000000000000000000000000000000000000000000000000000000000000001"
)

// created gives a new resource what Azure would add to it.
func (f *fake) created(r *fakeResource) {
	name := r.ID[strings.LastIndex(r.ID, "/")+1:]
	typ := func(t string) bool { return strings.EqualFold(r.Type, t) }
	switch {
	case typ("Microsoft.ManagedIdentity/userAssignedIdentities"):
		r.Props["principalId"] = principalA
		r.Props["clientId"] = "00000000-0000-0000-0000-0000000000b1"
	case typ("Microsoft.App/containerApps"):
		r.Props["provisioningState"] = "Succeeded"
		r.Props["runningStatus"] = "Running"
		r.Props["latestRevisionName"] = name + "--rev1"
		r.Props["latestReadyRevisionName"] = name + "--rev1"
		r.Props["customDomainVerificationId"] = verifyID
		if cfg, ok := r.Props["configuration"].(map[string]any); ok {
			if ing, ok := cfg["ingress"].(map[string]any); ok {
				ing["fqdn"] = name + ".apps" + ".example.com"
			}
		}
	case typ("Microsoft.App/managedEnvironments"):
		r.Props["staticIp"] = staticIP
	case typ("Microsoft.OperationalInsights/workspaces"):
		r.Props["customerId"] = workspaceCID
	case typ("Microsoft.App/managedEnvironments/managedCertificates"):
		r.Props["provisioningState"] = "Succeeded"
	}
}

// addApp puts a container app with public ingress in a group, in an environment there.
func (f *fake) addApp(rg, name string, tags map[string]string) (*fakeResource, *fakeResource) {
	env := f.add(rg, "Microsoft.App/managedEnvironments", name+"-env", tags)
	f.created(env)
	ws := f.add(rg, "Microsoft.OperationalInsights/workspaces", name+"-env-logs", tags)
	f.created(ws)
	diag := env.ID + "/providers/Microsoft.Insights/diagnosticSettings/yawble-logs"
	f.resources[strings.ToLower(diag)] = &fakeResource{ID: diag, Type: "Microsoft.Insights/diagnosticSettings", Props: map[string]any{"workspaceId": ws.ID}}
	app := f.add(rg, "Microsoft.App/containerApps", name, tags)
	app.Props = map[string]any{
		"managedEnvironmentId": env.ID,
		"configuration":        map[string]any{"ingress": map[string]any{"external": true, "targetPort": 8080}},
		"template":             map[string]any{"containers": []any{map[string]any{"name": name, "image": acrRef("myregistry", name+":1.0")}}},
	}
	f.created(app)
	return app, env
}

// armAction answers the Azure Resource Manager requests that are not a plain resource: a
// registry's build actions, an app's revisions, restart and change, and an environment's
// diagnostic settings. It answers false for anything else.
func (f *fake) armAction(w http.ResponseWriter, method, path string, body []byte) bool {
	lower := strings.ToLower(path)
	switch {
	case method == http.MethodPost && strings.HasSuffix(lower, "/listbuildsourceuploadurl"):
		f.answer(w, 200, map[string]any{"uploadUrl": uploadURL, "relativePath": "source/upload.tar.gz"})
	case method == http.MethodPost && strings.HasSuffix(lower, "/schedulerun"):
		f.runPolls = 0
		f.answer(w, 200, map[string]any{"properties": map[string]any{"runId": "ca1", "status": "Queued"}})
	case method == http.MethodGet && strings.HasSuffix(lower, "/runs/ca1"):
		f.runPolls++
		status := "Running"
		if f.runPolls > 1 {
			status = f.runStatus
		}
		f.answer(w, 200, map[string]any{"properties": map[string]any{"runId": "ca1", "status": status,
			"outputImages": []map[string]any{{"repository": "my-app", "tag": "1.0", "digest": buildDigest}}}})
	case method == http.MethodPost && strings.HasSuffix(lower, "/runs/ca1/listlogsasurl"):
		f.answer(w, 200, map[string]any{"logLink": logURL})
	case method == http.MethodGet && strings.HasSuffix(lower, "/revisions"):
		app := f.resources[strings.ToLower(strings.TrimSuffix(path, "/revisions"))]
		if app == nil {
			f.answer(w, 404, armError("ResourceNotFound", "No app."))
			return true
		}
		f.answer(w, 200, map[string]any{"value": []map[string]any{{"name": app.Props["latestRevisionName"], "properties": map[string]any{
			"active": true, "createdTime": "2026-10-09T14:00:00Z", "replicas": 1, "trafficWeight": 100, "healthState": "Healthy", "runningState": "Running"}}}})
	case method == http.MethodPost && strings.HasSuffix(lower, "/restart"):
		f.answer(w, 200, nil)
	case method == http.MethodPatch:
		r := f.resources[lower]
		if r == nil {
			f.answer(w, 404, armError("ResourceNotFound", "No app."))
			return true
		}
		var in struct {
			Properties struct {
				Configuration struct {
					Ingress map[string]any `json:"ingress"`
				} `json:"configuration"`
			} `json:"properties"`
		}
		_ = json.Unmarshal(body, &in)
		if in.Properties.Configuration.Ingress != nil {
			cfg, _ := r.Props["configuration"].(map[string]any)
			cfg["ingress"] = in.Properties.Configuration.Ingress
		}
		f.answer(w, 200, map[string]any{"id": r.ID, "properties": r.Props})
	case method == http.MethodGet && strings.Contains(lower, "/providers/microsoft.keyvault/vaults/") && strings.HasSuffix(lower, "/secrets"):
		// Azure management's list of a vault's secrets: names and attributes, never a value.
		id := strings.TrimSuffix(lower, "/secrets")
		if f.resources[id] == nil {
			f.answer(w, 404, armError("ResourceNotFound", "No vault."))
			return true
		}
		var list []map[string]any
		for _, s := range f.vaultNames[id] {
			list = append(list, map[string]any{"id": f.resources[id].ID + "/secrets/" + s, "name": s, "type": "Microsoft.KeyVault/vaults/secrets",
				"properties": map[string]any{"attributes": map[string]any{"enabled": true}}})
		}
		f.answer(w, 200, map[string]any{"value": list})
	case strings.Contains(lower, "/providers/microsoft.keyvault/vaults/") && strings.Contains(lower, "/secrets/"):
		f.t.Errorf("the plugin read or changed a secret: %s %s", method, path)
		f.answer(w, 403, armError("Forbidden", "No."))
	case method == http.MethodPost && strings.HasSuffix(lower, "/api/query"):
		// Azure management's query on a workspace resource; it answers its columns as ColumnName.
		ws := f.resources[strings.TrimSuffix(lower, "/api/query")]
		if ws == nil || !strings.EqualFold(ws.Type, "Microsoft.OperationalInsights/workspaces") {
			f.answer(w, 404, armError("ResourceNotFound", "No workspace."))
			return true
		}
		var rows [][]any
		for i := len(f.logLines) - 1; i >= 0; i-- { // newest first, as the query asks
			rows = append(rows, []any{fmt.Sprintf("2026-10-09T14:%02d:00Z", i), "ContainerAppConsoleLogs", f.logLines[i]})
		}
		f.answer(w, 200, map[string]any{"tables": []map[string]any{{"name": "PrimaryResult",
			"columns": []map[string]any{{"ColumnName": "TimeGenerated", "ColumnType": "datetime"}, {"ColumnName": "$table", "ColumnType": "string"}, {"ColumnName": "Log", "ColumnType": "string"}},
			"rows":    rows}}})
	case method == http.MethodGet && strings.HasSuffix(lower, "/providers/microsoft.insights/diagnosticsettings"):
		var list []map[string]any
		for k, r := range f.resources {
			if strings.HasPrefix(k, lower+"/") {
				list = append(list, map[string]any{"id": r.ID, "properties": r.Props})
			}
		}
		f.answer(w, 200, map[string]any{"value": list})
	default:
		return false
	}
	return true
}

// ---- storage ----

type fakeStorage struct {
	mu       sync.Mutex
	blobs    map[string][]byte         // container/blob
	entities map[string]map[string]any // table|pk|rk
	messages map[string][]string       // queue -> texts
}

// addStorage puts a storage account in a group, and serves its blob, table and queue hosts.
func (f *fake) addStorage(rg, name string, tags map[string]string) (*fakeResource, *fakeStorage) {
	r := f.add(rg, "Microsoft.Storage/storageAccounts", name, tags)
	st := &fakeStorage{blobs: map[string][]byte{}, entities: map[string]map[string]any{}, messages: map[string][]string{}}
	for _, svc := range []string{"blob", "table", "queue"} {
		svc := svc
		f.hosts[strings.TrimPrefix(storageHost(name, svc), "https://")] = func(w http.ResponseWriter, req *http.Request) {
			if req.Header.Get("Authorization") != "Bearer "+tokStorage {
				w.WriteHeader(403)
				_, _ = io.WriteString(w, "<?xml version=\"1.0\"?><Error><Code>AuthenticationFailed</Code><Message>Wrong token.\nRequestId:0</Message></Error>")
				return
			}
			st.mu.Lock()
			defer st.mu.Unlock()
			st.serve(f, svc, w, req)
		}
	}
	return r, st
}

func (st *fakeStorage) serve(f *fake, svc string, w http.ResponseWriter, r *http.Request) {
	_, path, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	body, _ := io.ReadAll(r.Body)
	if len(body) == 0 {
		f.mu.Lock()
		body = f.calls[len(f.calls)-1].Body
		f.mu.Unlock()
	}
	q := r.URL.Query()
	switch svc {
	case "blob":
		container, blob, _ := strings.Cut(path, "/")
		switch {
		case r.Method == http.MethodGet && q.Get("comp") == "list":
			var names []string
			for k := range st.blobs {
				if c, b, _ := strings.Cut(k, "/"); c == container && strings.HasPrefix(b, q.Get("prefix")) {
					names = append(names, b)
				}
			}
			sort.Strings(names)
			out := "<?xml version=\"1.0\"?><EnumerationResults><Blobs>"
			for _, n := range names {
				out += fmt.Sprintf("<Blob><Name>%s</Name><Properties><Content-Length>%d</Content-Length><Last-Modified>Fri, 09 Oct 2026 14:00:00 GMT</Last-Modified></Properties></Blob>", n, len(st.blobs[container+"/"+n]))
			}
			_, _ = io.WriteString(w, out+"</Blobs><NextMarker/></EnumerationResults>")
		case r.Method == http.MethodGet:
			b, ok := st.blobs[container+"/"+blob]
			if !ok {
				w.WriteHeader(404)
				return
			}
			_, _ = w.Write(b)
		case r.Method == http.MethodPut:
			st.blobs[container+"/"+blob] = body
			w.WriteHeader(201)
		case r.Method == http.MethodDelete:
			if _, ok := st.blobs[container+"/"+blob]; !ok {
				w.WriteHeader(404)
				return
			}
			delete(st.blobs, container+"/"+blob)
			w.WriteHeader(202)
		}
	case "table":
		table, key, keyed := strings.Cut(path, "(")
		switch {
		case r.Method == http.MethodGet:
			var list []map[string]any
			var keys []string
			for k := range st.entities {
				if strings.HasPrefix(k, table+"|") {
					keys = append(keys, k)
				}
			}
			sort.Strings(keys)
			for _, k := range keys {
				list = append(list, st.entities[k])
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"value": list})
		case keyed && r.Method == http.MethodPut:
			var e map[string]any
			_ = json.Unmarshal(body, &e)
			st.entities[table+"|"+key] = e
			w.WriteHeader(204)
		case keyed && r.Method == http.MethodDelete:
			if _, ok := st.entities[table+"|"+key]; !ok {
				w.WriteHeader(404)
				return
			}
			delete(st.entities, table+"|"+key)
			w.WriteHeader(204)
		}
	case "queue":
		queue, rest, _ := strings.Cut(path, "/")
		switch {
		case r.Method == http.MethodPost && rest == "messages":
			var m struct {
				Text string `xml:"MessageText"`
			}
			_ = xml.Unmarshal(body, &m)
			st.messages[queue] = append(st.messages[queue], m.Text)
			w.WriteHeader(201)
			_, _ = io.WriteString(w, fmt.Sprintf("<QueueMessagesList><QueueMessage><MessageId>m%d</MessageId></QueueMessage></QueueMessagesList>", len(st.messages[queue])))
		case r.Method == http.MethodGet && rest == "messages":
			out := "<QueueMessagesList>"
			for i, text := range st.messages[queue] {
				out += fmt.Sprintf("<QueueMessage><MessageId>m%d</MessageId><InsertionTime>Fri, 09 Oct 2026 14:00:00 GMT</InsertionTime><DequeueCount>0</DequeueCount><PopReceipt>pop%d</PopReceipt><MessageText>%s</MessageText></QueueMessage>", i+1, i+1, text)
			}
			_, _ = io.WriteString(w, out+"</QueueMessagesList>")
		case r.Method == http.MethodDelete && strings.HasPrefix(rest, "messages/"):
			w.WriteHeader(204)
		}
	}
}

// ---- Key Vault ----

// addVault puts a vault in a group, whose secrets' names Azure management lists. Reading or
// changing a secret fails the test.
func (f *fake) addVault(rg, name string, tags map[string]string, secrets ...string) *fakeResource {
	r := f.add(rg, "Microsoft.KeyVault/vaults", name, tags)
	f.vaultNames[strings.ToLower(r.ID)] = secrets
	return r
}

// ---- a workspace's log lines, and the addresses a registry hands out ----

func (f *fake) serveLogsAndBuilds(logLines []string) {
	f.logLines = logLines
	sas := func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			f.t.Errorf("a token went to an address that carries its own permission: %s", r.URL.Path)
		}
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, "Step 1/2 : FROM scratch\nAccountKey=leaked"+strings.Repeat("k", 20)+"\nthe token "+tokARM+"\nSuccessfully pushed\n")
			return
		}
		w.WriteHeader(201)
	}
	f.hosts["acrsource"+".blob.core.windows.net"] = sas
	f.hosts["acrlogs"+".blob.core.windows.net"] = sas
}
