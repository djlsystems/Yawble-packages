package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// The fake: one TLS server answering every Azure host the plugin calls, as /<host>/<path>, so a
// test sees every request and nothing reaches a network. Azure Resource Manager is modelled here;
// a data-plane host gets a handler in hosts.

const (
	testSub   = "00000000-0000-0000-0000-000000000000"
	otherSub  = "00000000-0000-0000-0000-000000000001"
	testTeam  = "my-team"
	ownedRG   = "my-project-rg"
	foreignRG = "someone-elses-rg"
	armHost   = "management.azure.com"
)

// Fake tokens are put together at run time, so no file holds a value shaped like a real token.
var (
	tokARM     = fakeJWT("m")
	tokSQL     = fakeJWT("q")
	tokStorage = fakeJWT("s")
	tokOther   = fakeJWT("v") // a token for no API the plugin asks for
	allTokens  = []string{tokARM, tokSQL, tokStorage, tokOther}
)

func fakeJWT(tag string) string {
	enc := func(v map[string]any) string {
		raw, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	head := enc(map[string]any{"alg": "none", "typ": "JWT", "kid": strings.Repeat(tag, 24)})
	body := enc(map[string]any{"oid": zeroGUID, "tid": zeroGUID, "upn": "person@example.com", "uti": strings.Repeat(tag, 40)})
	return head + "." + body + "." + strings.Repeat(strings.ToUpper(tag), 20)
}

const zeroGUID = "00000000-0000-0000-0000-000000000000"

var testNow = time.Date(2026, 10, 9, 15, 0, 0, 0, time.UTC)

type call struct {
	Host, Method, Path, Query string
	Token                     string
	Body                      []byte
}

type fakeResource struct {
	ID, Type, Location string
	Tags               map[string]string
	Props              map[string]any
	Principal          string // the system-assigned identity's principal
}

type fake struct {
	t   *testing.T
	srv *httptest.Server
	mu  sync.Mutex

	calls      []call
	subs       map[string]string // id -> display name
	subState   string
	rgs        map[string]*fakeResource // lower-case name -> group
	resources  map[string]*fakeResource // lower-case id -> resource
	actions    []string                 // the person's allowed actions on the subscription
	notActions []string
	providers  map[string]string // namespace -> registration state
	locations  []string
	whatIf     []map[string]any // the changes the next what-if answers
	echoToken  bool             // answer the subscription read with an error quoting the token
	hosts      map[string]http.HandlerFunc
	runPolls   int                   // how often a registry build was read
	runStatus  string                // how a registry build ends
	logLines   []string              // what a workspace query answers, oldest first
	vaultNames map[string][]string   // lower-case vault id -> its secrets' names
	deployOps  []map[string]any      // when set, the next deployment fails and its operations are these
	noGrants   bool                  // every role assignment is refused, as for an account that may not assign roles
	granted    map[string]bool       // lower-case role assignment ids made so far; Azure answers 409 to one again
	afterPut   func(r *fakeResource) // when set, changes a resource after a PUT, as Azure may
}

func newFake(t *testing.T) *fake {
	f := &fake{
		t:          t,
		subs:       map[string]string{testSub: "My Subscription"},
		subState:   "Enabled",
		rgs:        map[string]*fakeResource{},
		resources:  map[string]*fakeResource{},
		actions:    []string{"*"},
		providers:  map[string]string{},
		locations:  []string{"westeurope", "eastus2"},
		hosts:      map[string]http.HandlerFunc{},
		runStatus:  "Succeeded",
		vaultNames: map[string][]string{},
		granted:    map[string]bool{},
	}
	for _, ns := range providers {
		f.providers[ns] = "Registered"
	}
	f.addRG(ownedRG, ownTags())
	f.addRG(foreignRG, map[string]string{"cost-center": "shared"})
	f.srv = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)

	oldEndpoint, oldClient, oldEvery, oldSleep, oldRoot := endpoint, httpClient, pollEvery, sleep, documentsRoot
	endpoint = func(u string) string {
		rest := strings.TrimPrefix(u, "https://")
		return f.srv.URL + "/" + rest
	}
	httpClient = f.srv.Client()
	pollEvery = 0
	sleep = func(time.Duration) {}
	documentsRoot = t.TempDir()
	t.Cleanup(func() {
		endpoint, httpClient, pollEvery, sleep, documentsRoot = oldEndpoint, oldClient, oldEvery, oldSleep, oldRoot
	})
	return f
}

func ownTags() map[string]string {
	return map[string]string{"yawble-owner": testTeam, "yawble-plugin": "azure"}
}

func rgID(name string) string { return "/subscriptions/" + testSub + "/resourceGroups/" + name }

func (f *fake) addRG(name string, tags map[string]string) {
	f.rgs[strings.ToLower(name)] = &fakeResource{ID: rgID(name), Type: "Microsoft.Resources/resourceGroups", Location: "westeurope", Tags: tags}
}

// add puts a resource in a group: rg, provider type such as Microsoft.Storage/storageAccounts, name.
func (f *fake) add(rg, typ, name string, tags map[string]string) *fakeResource {
	id := rgID(rg) + "/providers/" + typ + "/" + name
	r := &fakeResource{ID: id, Type: typ, Location: "westeurope", Tags: tags, Props: map[string]any{}}
	f.resources[strings.ToLower(id)] = r
	return r
}

// writes are the requests that could change something: anything but GET, and not a what-if.
func (f *fake) writes() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []call
	for _, c := range f.calls {
		if c.Method != http.MethodGet && !strings.HasSuffix(c.Path, "/whatIf") {
			out = append(out, c)
		}
	}
	return out
}

func (f *fake) callsTo(method, suffix string) []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []call
	for _, c := range f.calls {
		if c.Method == method && strings.HasSuffix(strings.ToLower(c.Path), strings.ToLower(suffix)) {
			out = append(out, c)
		}
	}
	return out
}

// callsContaining answers the calls of a method whose path holds the text, in any case.
func (f *fake) callsContaining(method, text string) []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []call
	for _, c := range f.calls {
		if c.Method == method && strings.Contains(strings.ToLower(c.Path), strings.ToLower(text)) {
			out = append(out, c)
		}
	}
	return out
}

func (f *fake) answer(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if body != nil {
		_ = json.NewEncoder(w).Encode(body)
	}
}

func armError(code, message string) map[string]any {
	return map[string]any{"error": map[string]any{"code": code, "message": message}}
}

func (f *fake) serve(w http.ResponseWriter, r *http.Request) {
	host, path, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	path = "/" + path
	body, _ := io.ReadAll(r.Body)
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	f.mu.Lock()
	f.calls = append(f.calls, call{Host: host, Method: r.Method, Path: path, Query: r.URL.RawQuery, Token: token, Body: body})
	f.mu.Unlock()

	// NEVER A KEY: no request of any test asks Azure for a key, a connection string or a secret.
	if l := strings.ToLower(path); strings.Contains(l, "listkeys") || strings.Contains(l, "sharedkeys") ||
		strings.Contains(l, "listconnectionstrings") || strings.Contains(l, "listcredentials") || strings.Contains(l, "listsecrets") {
		f.t.Errorf("the plugin asked for a key: %s %s", r.Method, path)
	}
	if h, ok := f.hosts[host]; ok {
		h(w, r)
		return
	}
	if host != armHost {
		f.t.Errorf("a request went to %s, which the fake does not serve", host)
		f.answer(w, http.StatusBadGateway, nil)
		return
	}
	if token != tokARM {
		f.answer(w, http.StatusUnauthorized, armError("InvalidAuthenticationToken", "The access token is invalid."))
		return
	}
	if r.URL.Query().Get("api-version") == "" && !strings.Contains(path, "/operations/") {
		f.answer(w, http.StatusBadRequest, armError("MissingApiVersionParameter", "The api-version query parameter is required."))
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.arm(w, r.Method, path, body)
}

func (f *fake) arm(w http.ResponseWriter, method, path string, body []byte) {
	lower := strings.ToLower(path)
	subPrefix := "/subscriptions/" + testSub
	switch {
	case lower == "/subscriptions" && method == http.MethodGet:
		var list []map[string]any
		for id, name := range f.subs {
			list = append(list, map[string]any{"subscriptionId": id, "displayName": name, "state": f.subState})
		}
		f.answer(w, 200, map[string]any{"value": list})
		return
	case strings.HasPrefix(lower, "/subscriptions/") && strings.Count(lower, "/") == 2 && method == http.MethodGet:
		id := strings.TrimPrefix(lower, "/subscriptions/")
		if f.echoToken {
			f.answer(w, 403, armError("AuthorizationFailed", "The client with token "+tokARM+" has no access."))
			return
		}
		name, ok := f.subs[id]
		if !ok {
			f.answer(w, 404, armError("SubscriptionNotFound", "The subscription could not be found."))
			return
		}
		f.answer(w, 200, map[string]any{"subscriptionId": id, "displayName": name, "state": f.subState})
		return
	case !strings.HasPrefix(lower, subPrefix):
		f.answer(w, 403, armError("AuthorizationFailed", "No access to that subscription."))
		return
	}
	rest := path[len(subPrefix):]
	lrest := strings.ToLower(rest)

	switch {
	case lrest == "/operations/whatif-result":
		f.answer(w, 200, map[string]any{"status": "Succeeded", "properties": map[string]any{"changes": f.whatIf}})
	case lrest == "/operations/deploy-status":
		if f.deployOps != nil {
			f.answer(w, 200, map[string]any{"status": "Failed", "error": map[string]any{"code": "DeploymentFailed", "message": "At least one resource deployment operation failed. Please list deployment operations for details."}})
			return
		}
		f.answer(w, 200, map[string]any{"status": "Succeeded"})
	case lrest == "/locations":
		var list []map[string]any
		for _, l := range f.locations {
			list = append(list, map[string]any{"name": l})
		}
		f.answer(w, 200, map[string]any{"value": list})
	case lrest == "/providers/microsoft.authorization/permissions":
		f.answer(w, 200, map[string]any{"value": []map[string]any{{"actions": f.actions, "notActions": f.notActions}}})
	case strings.HasPrefix(lrest, "/providers/") && strings.Count(lrest, "/") == 2:
		ns := rest[len("/providers/"):]
		if method == http.MethodGet {
			for k, v := range f.providers {
				if strings.EqualFold(k, ns) {
					f.answer(w, 200, map[string]any{"namespace": k, "registrationState": v})
					return
				}
			}
			f.answer(w, 200, map[string]any{"namespace": ns, "registrationState": "NotRegistered", "resourceTypes": []any{}})
		}
	case strings.HasPrefix(lrest, "/providers/") && strings.HasSuffix(lrest, "/register") && method == http.MethodPost:
		ns := strings.TrimSuffix(rest[len("/providers/"):], "/register")
		f.providers[ns] = "Registering"
		f.answer(w, 200, map[string]any{"namespace": ns, "registrationState": "Registering"})
	case lrest == "/resourcegroups":
		var list []map[string]any
		for _, g := range f.rgs {
			list = append(list, map[string]any{"id": g.ID, "name": g.ID[strings.LastIndex(g.ID, "/")+1:], "type": g.Type, "location": g.Location, "tags": g.Tags})
		}
		f.answer(w, 200, map[string]any{"value": list})
	case strings.HasPrefix(lrest, "/providers/") && strings.Count(lrest, "/") == 3 && method == http.MethodGet:
		// a list of one type across the subscription: /providers/Microsoft.Sql/servers
		typ := strings.TrimPrefix(lrest, "/providers/")
		var list []map[string]any
		for _, r := range f.resources {
			if strings.EqualFold(r.Type, typ) {
				list = append(list, map[string]any{"id": r.ID, "name": r.ID[strings.LastIndex(r.ID, "/")+1:], "type": r.Type, "location": r.Location, "tags": r.Tags, "properties": r.Props})
			}
		}
		f.answer(w, 200, map[string]any{"value": list})
	case strings.HasPrefix(lrest, "/resourcegroups/"):
		f.group(w, method, path, body)
	default:
		f.answer(w, 404, armError("NotFound", "No route for "+path))
	}
}

// group answers everything under /subscriptions/<s>/resourceGroups/<rg>.
func (f *fake) group(w http.ResponseWriter, method, path string, body []byte) {
	parts := strings.Split(path, "/") // "", subscriptions, s, resourceGroups, rg, ...
	rgName := parts[4]
	g := f.rgs[strings.ToLower(rgName)]
	tail := strings.Join(parts[5:], "/")
	ltail := strings.ToLower(tail)

	switch {
	case tail == "":
		switch method {
		case http.MethodGet:
			if g == nil {
				f.answer(w, 404, armError("ResourceGroupNotFound", "Resource group '"+rgName+"' could not be found."))
				return
			}
			f.answer(w, 200, map[string]any{"id": g.ID, "name": rgName, "location": g.Location, "tags": g.Tags})
		case http.MethodPut:
			var in struct {
				Location string            `json:"location"`
				Tags     map[string]string `json:"tags"`
			}
			_ = json.Unmarshal(body, &in)
			f.addRG(rgName, in.Tags)
			f.rgs[strings.ToLower(rgName)].Location = in.Location
			f.answer(w, 201, map[string]any{"id": rgID(rgName), "name": rgName, "location": in.Location, "tags": in.Tags})
		case http.MethodDelete:
			if g == nil {
				f.answer(w, 404, nil)
				return
			}
			delete(f.rgs, strings.ToLower(rgName))
			w.Header().Set("Location", "https://"+armHost+"/subscriptions/"+testSub+"/operationresults/del?api-version=2021-04-01")
			f.answer(w, 202, nil)
		}
		return
	case g == nil:
		f.answer(w, 404, armError("ResourceGroupNotFound", "Resource group '"+rgName+"' could not be found."))
		return
	case ltail == "resources":
		var list []map[string]any
		for _, r := range f.resources {
			if strings.HasPrefix(strings.ToLower(r.ID), strings.ToLower(g.ID)+"/") {
				item := map[string]any{"id": r.ID, "name": r.ID[strings.LastIndex(r.ID, "/")+1:], "type": r.Type, "location": r.Location, "tags": r.Tags}
				if r.Principal != "" {
					item["identity"] = map[string]any{"type": "SystemAssigned", "principalId": r.Principal}
				}
				list = append(list, item)
			}
		}
		f.answer(w, 200, map[string]any{"value": list})
		return
	case strings.HasPrefix(ltail, "providers/microsoft.resources/deployments/"):
		f.deployment(w, method, path, body)
		return
	}

	// A resource, its tags or an extension on it.
	id := path
	if f.armAction(w, method, path, body) {
		return
	}
	if i := strings.Index(strings.ToLower(id), "/providers/microsoft.resources/tags/default"); i > 0 {
		r := f.resources[strings.ToLower(id[:i])]
		if r == nil {
			f.answer(w, 404, armError("ResourceNotFound", "The resource was not found."))
			return
		}
		f.answer(w, 200, map[string]any{"properties": map[string]any{"tags": r.Tags}})
		return
	}
	if strings.Contains(strings.ToLower(id), "/providers/microsoft.authorization/roleassignments/") && method == http.MethodPut {
		if f.noGrants {
			f.answer(w, 403, armError("AuthorizationFailed", "The client does not have authorization to perform action 'Microsoft.Authorization/roleAssignments/write'."))
			return
		}
		if f.granted[strings.ToLower(id)] {
			f.answer(w, 409, armError("RoleAssignmentExists", "The role assignment already exists."))
			return
		}
		f.granted[strings.ToLower(id)] = true
		f.answer(w, 201, map[string]any{"id": id})
		return
	}
	if strings.Contains(strings.ToLower(id), "/providers/microsoft.consumption/budgets/") && method == http.MethodPut {
		f.answer(w, 201, map[string]any{"id": id})
		return
	}
	r := f.resources[strings.ToLower(id)]
	switch method {
	case http.MethodGet:
		if r == nil {
			f.answer(w, 404, armError("ResourceNotFound", "The resource was not found."))
			return
		}
		out := map[string]any{"id": r.ID, "type": r.Type, "location": r.Location, "tags": r.Tags, "properties": r.Props}
		if r.Principal != "" {
			out["identity"] = map[string]any{"type": "SystemAssigned", "principalId": r.Principal}
		}
		f.answer(w, 200, out)
	case http.MethodPut:
		var in struct {
			Tags       map[string]string `json:"tags"`
			Properties map[string]any    `json:"properties"`
		}
		_ = json.Unmarshal(body, &in)
		var where struct {
			Location string `json:"location"`
		}
		_ = json.Unmarshal(body, &where)
		idx := strings.Index(strings.ToLower(id), "/providers/")
		typ := id[idx+len("/providers/"):]
		segs := strings.Split(typ, "/")
		typeParts := []string{segs[0]}
		for i := 1; i < len(segs); i += 2 {
			typeParts = append(typeParts, segs[i])
		}
		if where.Location == "" {
			where.Location = "global"
		}
		if in.Properties == nil {
			in.Properties = map[string]any{}
		}
		nr := &fakeResource{ID: id, Type: strings.Join(typeParts, "/"), Location: where.Location, Tags: in.Tags, Props: in.Properties}
		if strings.EqualFold(nr.Type, "Microsoft.Network/dnsZones") {
			nr.Props = map[string]any{"nameServers": []string{"ns1-01.azure-dns.example", "ns2-01.azure-dns.example"}}
		}
		f.created(nr)
		if f.afterPut != nil {
			f.afterPut(nr)
		}
		f.resources[strings.ToLower(id)] = nr
		status := 201
		if r != nil {
			status = 200 // a change, as Azure answers one
		}
		f.answer(w, status, map[string]any{"id": id, "tags": in.Tags, "properties": nr.Props})
	case http.MethodDelete:
		if r == nil {
			f.answer(w, 404, nil)
			return
		}
		delete(f.resources, strings.ToLower(id))
		f.answer(w, 200, nil)
	}
}

// deployment answers a what-if and a deployment, both long-running.
func (f *fake) deployment(w http.ResponseWriter, method, path string, body []byte) {
	opBase := "https://" + armHost + "/subscriptions/" + testSub + "/operations/"
	switch {
	case strings.HasSuffix(path, "/whatIf") && method == http.MethodPost:
		w.Header().Set("Location", opBase+"whatif-result")
		w.Header().Set("Retry-After", "1")
		f.answer(w, 202, nil)
	case method == http.MethodPut:
		w.Header().Set("Azure-AsyncOperation", opBase+"deploy-status")
		f.answer(w, 201, map[string]any{"properties": map[string]any{"provisioningState": "Accepted"}})
	case method == http.MethodGet && strings.HasSuffix(path, "/operations"):
		f.answer(w, 200, map[string]any{"value": f.deployOps})
	case method == http.MethodGet:
		if f.deployOps != nil {
			f.answer(w, 200, map[string]any{"properties": map[string]any{"provisioningState": "Failed",
				"error": map[string]any{"code": "DeploymentFailed", "message": "At least one resource deployment operation failed."}}})
			return
		}
		var ids []map[string]any
		for _, c := range f.whatIf {
			if t, _ := c["changeType"].(string); t == "Create" || t == "Modify" {
				ids = append(ids, map[string]any{"id": c["resourceId"]})
			}
		}
		f.answer(w, 200, map[string]any{"properties": map[string]any{"provisioningState": "Succeeded", "outputResources": ids}})
	default:
		f.answer(w, 404, nil)
	}
}

// ---- running the plugin ----

type runOpts struct {
	config     map[string]any
	secrets    map[string]string // the request's secrets: field -> value
	secretKeys map[string]string // the request's secretKeys: field -> key name
	noConnect  bool              // no microsoft slot bound
	tokens     map[string]string // the slot's tokens map; nil: one per API
	trees      []string
}

type result struct {
	t       *testing.T
	raw     string
	records []map[string]any
	code    int
}

func baseConfig(extra map[string]any) map[string]any {
	cfg := map[string]any{"subscription": testSub, "regions": []string{"westeurope"}}
	for k, v := range extra {
		cfg[k] = v
	}
	return cfg
}

func runPlugin(t *testing.T, o runOpts, instruction string) *result {
	t.Helper()
	cfg := o.config
	if cfg == nil {
		cfg = baseConfig(nil)
	}
	conns := map[string]any{}
	if !o.noConnect {
		tokens := o.tokens
		if tokens == nil {
			tokens = map[string]string{
				"https://management.azure.com/": tokARM,
				"https://database.windows.net/": tokSQL,
				"https://storage.azure.com/":    tokStorage,
			}
		}
		tm := map[string]any{}
		first := ""
		for k, v := range tokens {
			tm[k] = map[string]any{"accessToken": v, "expiresAt": "2026-10-09T16:00:00Z", "scopes": []string{k + "user_impersonation"}}
			if k == "https://management.azure.com/" {
				first = v
			}
		}
		conns["microsoft"] = map[string]any{
			"provider": "microsoft", "account": "person@example.com", "accessToken": first,
			"expiresAt": "2026-10-09T16:00:00Z", "scopes": []string{"offline_access"}, "tokens": tm,
		}
	}
	var trees []map[string]any
	for _, tr := range o.trees {
		trees = append(trees, map[string]any{"repo": "my-repo", "path": tr, "clonePath": tr})
	}
	req := map[string]any{
		"protocol":         "harness.member/1",
		"plugin":           map[string]any{"id": "azure-plugin", "version": "2.0.0"},
		"member":           map[string]any{"id": testTeam + "/Azure", "team": testTeam, "name": "Azure"},
		"work":             []map[string]any{{"seq": 1, "instruction": instruction, "payload": map[string]any{"instruction": instruction}}},
		"config":           cfg,
		"secrets":          orEmpty(o.secrets),
		"secretKeys":       orEmpty(o.secretKeys),
		"connections":      conns,
		"workingDirectory": t.TempDir(),
		"worktrees":        trees,
		"siteFiles":        []any{},
		"sites":            []any{},
	}
	in, _ := json.Marshal(req)
	var out bytes.Buffer
	code := run(bytes.NewReader(in), &out, func() time.Time { return testNow })
	r := &result{t: t, raw: out.String(), code: code}
	for _, line := range strings.Split(strings.TrimSpace(r.raw), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("a line is not a JSON record: %q", line)
		}
		r.records = append(r.records, rec)
	}
	// EVERY RUN: no output, event or progress row holds any of the fake tokens, in any case.
	assertNoToken(t, r.raw)
	if last := r.records[len(r.records)-1]; last["t"] != "result" {
		t.Fatalf("the last record is not the result: %v", last)
	}
	return r
}

func orEmpty(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

func assertNoToken(t *testing.T, raw string) {
	t.Helper()
	lower := strings.ToLower(raw)
	for _, tok := range allTokens {
		if strings.Contains(lower, strings.ToLower(tok)) {
			t.Fatalf("a token was written out:\n%s", raw)
		}
		// Nor a large piece of one.
		for _, part := range strings.Split(tok, ".") {
			if len(part) >= 16 && strings.Contains(lower, strings.ToLower(part)) {
				t.Fatalf("part of a token was written out:\n%s", raw)
			}
		}
	}
}

func (r *result) result() map[string]any { return r.records[len(r.records)-1] }

func (r *result) ok() bool { return r.result()["ok"] == true }

func (r *result) text() string {
	res := r.result()
	if s, ok := res["output"].(string); ok {
		return s
	}
	s, _ := res["error"].(string)
	return s
}

func (r *result) events(suffix string) []map[string]any {
	var out []map[string]any
	for _, rec := range r.records {
		if rec["t"] == "publish" && rec["type"] == suffix {
			out = append(out, rec["payload"].(map[string]any))
		}
	}
	return out
}

func (r *result) progressRows() []string {
	var out []string
	for _, rec := range r.records {
		if rec["t"] == "progress" {
			out = append(out, rec["status"].(string))
		}
	}
	return out
}

func mustContain(t *testing.T, text string, parts ...string) {
	t.Helper()
	for _, p := range parts {
		if !strings.Contains(text, p) {
			t.Fatalf("answer lacks %q:\n%s", p, text)
		}
	}
}

// docFile writes a file in the team's documents folder and answers its path.
func docFile(t *testing.T, name, content string) string {
	t.Helper()
	dir := filepath.Join(documentsRoot, testTeam)
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

const simpleTemplate = `{
  "$schema": "https://schema.management.azure.com/schemas/2019-04-01/deploymentTemplate.json#",
  "contentVersion": "1.0.0.0",
  "parameters": { "name": { "type": "string" } },
  "resources": [
    { "type": "Microsoft.ManagedIdentity/userAssignedIdentities", "apiVersion": "2023-01-31", "name": "[parameters('name')]", "location": "westeurope" }
  ]
}`

func wiChange(changeType, id string, after map[string]any) map[string]any {
	c := map[string]any{"resourceId": id, "changeType": changeType}
	if after != nil {
		c["after"] = after
	}
	return c
}

func inRG(rg, rest string) string { return rgID(rg) + "/providers/" + rest }

func sortedKeys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// A guest account's unique name carries its home's prefix, which the person's name never shows.
func TestAGuestsSignInNameDropsItsHomePrefix(t *testing.T) {
	enc := func(v map[string]any) string {
		raw, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	tok := enc(map[string]any{"alg": "none"}) + "." + enc(map[string]any{"oid": zeroGUID, "tid": zeroGUID, "unique_name": "home.example#guest@example.com"}) + ".x"
	p := &plugin{az: &client{tokens: map[api]string{apiARM: tok}}}
	if _, _, name, err := p.signedIn(); err != nil || name != "guest@example.com" {
		t.Fatalf("name = %q, %v", name, err)
	}
}
