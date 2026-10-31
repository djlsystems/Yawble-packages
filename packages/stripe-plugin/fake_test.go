package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// The fakes: one TLS server answering Stripe's API under /v1 and Azure Resource Manager under
// /subscriptions (the subscriptions list, each subscription's Key Vaults and a vault's secrets), so a
// test sees every request the plugin makes and nothing reaches a network. A second server stands for
// any host other than Azure management, which a nextLink may name and the plugin must not follow.

const (
	accountA   = "acct_1TestAccountAaaa"
	accountB   = "acct_1TestAccountBbbb"
	armToken   = "eyJ0eXAiOiJKV1QifakeManagementAccessToken0"
	graphToken = "eyJ0eXAiOiJKV1QifakeGraphAccessToken00000"
	allowedKV  = "payments-vault"
	otherKV    = "other-vault"

	// Placeholder subscriptions: the first holds other-vault, the second payments-vault, and the
	// subscriptions list names the second on its next page.
	subOne = "00000000-0000-0000-0000-000000000001"
	subTwo = "00000000-0000-0000-0000-000000000002"
)

// allowedVaultID is the resource id the fake answers for payments-vault, its name in another case.
const allowedVaultID = "/subscriptions/" + subTwo + "/resourceGroups/rg-example/providers/Microsoft.KeyVault/vaults/Payments-Vault"

const otherVaultID = "/subscriptions/" + subOne + "/resourceGroups/rg-other/providers/Microsoft.KeyVault/vaults/other-vault"

// Fake keys are put together at run time, so no file holds a value shaped like a real key.
var (
	keyA      = makeKey("rk", "test", "a")
	keyB      = makeKey("rk", "test", "b")
	keyLive   = makeKey("rk", "live", "c")
	secretKey = makeKey("sk", "test", "d")
)

// makeKey returns kind_mode_ followed by a filler made of tag.
func makeKey(kind, mode, tag string) string {
	return kind + "_" + mode + "_" + "51Fake" + strings.Repeat(tag, 32)
}

var testNow = time.Date(2026, 10, 9, 15, 0, 0, 0, time.UTC)

type call struct {
	Method, Path string
	Form         url.Values
	Idem         string
	Query        string // the raw query, for Azure management's calls
	Body         string // the raw body, for Azure management's calls
}

type fakeKey struct {
	account string
	missing map[string]bool // permission resources the key lacks, as setup names them
}

type fake struct {
	t   *testing.T
	srv *httptest.Server
	mu  sync.Mutex

	keys        map[string]fakeKey
	calls       []call
	vaultCalls  []call // every call to Azure management
	foreign     *httptest.Server
	foreignHits int
	products    map[string]map[string]any
	prices      []map[string]any
	endpoints   []map[string]any
	events      []map[string]any
	idem        map[string][]byte
	secrets     map[string][]string // vault/name -> versions' values
	vaultStatus map[string]int      // vault -> status to answer on its secrets
	nextID      int
	webhookSecs []string // every signing secret the fake handed out
	noSecret    bool     // answer a new endpoint without its signing secret
	deleteFails bool     // answer 500 to removing a webhook endpoint
	payment     map[string]any
}

func newFake(t *testing.T) *fake {
	f := &fake{
		t: t,
		keys: map[string]fakeKey{
			keyA:    {account: accountA},
			keyB:    {account: accountB},
			keyLive: {account: accountA},
		},
		products:    map[string]map[string]any{},
		idem:        map[string][]byte{},
		secrets:     map[string][]string{},
		vaultStatus: map[string]int{},
	}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	f.foreign = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.foreignHits++
		f.mu.Unlock()
		f.answer(w, 200, map[string]any{"value": []any{}})
	}))
	t.Cleanup(f.foreign.Close)

	oldBase, oldARM, oldClient := stripeBase, armBase, httpClient
	stripeBase = f.srv.URL
	armBase = f.srv.URL
	// The two servers share one test certificate authority, so one client trusts both.
	httpClient = f.srv.Client()
	t.Cleanup(func() { stripeBase, armBase, httpClient = oldBase, oldARM, oldClient })
	return f
}

func (f *fake) id(prefix string) string {
	f.nextID++
	return fmt.Sprintf("%s%024d", prefix, f.nextID)
}

func (f *fake) answer(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func stripeError(status int, message string) map[string]any {
	return map[string]any{"error": map[string]any{"type": "invalid_request_error", "message": message}}
}

// permissionFor says which permission a Stripe path needs, as setup names it.
func permissionFor(method, path string) string {
	write := method != http.MethodGet
	switch {
	case strings.HasPrefix(path, "/v1/products"):
		return map[bool]string{true: "Products:write", false: "Products:read"}[write]
	case strings.HasPrefix(path, "/v1/prices"):
		return map[bool]string{true: "Prices:write", false: "Prices:read"}[write]
	case strings.HasPrefix(path, "/v1/webhook_endpoints"):
		return map[bool]string{true: "Webhook Endpoints:write", false: "Webhook Endpoints:read"}[write]
	case strings.HasPrefix(path, "/v1/checkout/sessions"):
		return "Checkout Sessions:read"
	case strings.HasPrefix(path, "/v1/payment_intents"):
		return "PaymentIntents:read"
	case strings.HasPrefix(path, "/v1/charges"):
		return "Charges:read"
	case strings.HasPrefix(path, "/v1/customers"):
		return "Customers:read"
	case strings.HasPrefix(path, "/v1/events"):
		return "Events:read"
	case strings.HasPrefix(path, "/v1/balance"):
		return "Balance:read"
	}
	return ""
}

func (f *fake) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	raw, _ := io.ReadAll(r.Body)

	if strings.HasPrefix(r.URL.Path, "/subscriptions") {
		f.serveARM(w, r, raw)
		return
	}

	form, _ := url.ParseQuery(string(raw))
	if r.Method == http.MethodGet || r.Method == http.MethodDelete {
		form = r.URL.Query()
	}
	f.calls = append(f.calls, call{Method: r.Method, Path: r.URL.Path, Form: form, Idem: r.Header.Get("Idempotency-Key")})

	key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	k, ok := f.keys[key]
	if !ok {
		f.answer(w, 401, stripeError(401, "Invalid API Key provided: rk_test_********"+key[len(key)-4:]))
		return
	}
	masked := key[:8] + "*****" + key[len(key)-4:]
	if r.URL.Path == "/v1/account" {
		f.answer(w, 403, stripeError(403, fmt.Sprintf("The provided key '%s' does not have the required permissions for this endpoint on account '%s'. Having the 'rak_accounts_kyc_basic_read' permission would allow this request to continue.", masked, k.account)))
		return
	}
	if perm := permissionFor(r.Method, r.URL.Path); perm != "" {
		resource, level, _ := strings.Cut(perm, ":")
		if k.missing[resource] || (level == "read" && k.missing[resource+":read"]) || (level == "write" && k.missing[resource+":write"]) {
			f.answer(w, 403, stripeError(403, fmt.Sprintf("The provided key '%s' does not have the required permissions for this endpoint on account '%s'.", masked, k.account)))
			return
		}
	}

	if idem := r.Header.Get("Idempotency-Key"); idem != "" {
		if prev, ok := f.idem[idem]; ok {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(prev)
			return
		}
		rec := httptest.NewRecorder()
		f.route(rec, r, form)
		if rec.Code/100 == 2 {
			f.idem[idem] = rec.Body.Bytes()
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())
		return
	}
	f.route(w, r, form)
}

func (f *fake) route(w http.ResponseWriter, r *http.Request, form url.Values) {
	p, m := r.URL.Path, r.Method
	missing := func() { f.answer(w, 404, stripeError(404, "No such object")) }
	list := func(items []map[string]any) {
		f.answer(w, 200, map[string]any{"object": "list", "data": items, "has_more": false})
	}
	switch {
	case p == "/v1/products" && m == http.MethodPost:
		id := form.Get("id")
		if _, exists := f.products[id]; exists {
			f.answer(w, 400, stripeError(400, "Product already exists."))
			return
		}
		prod := map[string]any{"id": id, "object": "product", "name": form.Get("name"), "description": form.Get("description"), "active": true}
		f.products[id] = prod
		f.answer(w, 200, prod)
	case strings.HasPrefix(p, "/v1/products/"):
		id := strings.TrimPrefix(p, "/v1/products/")
		prod, ok := f.products[id]
		if !ok {
			missing()
			return
		}
		if m == http.MethodPost {
			for _, k := range []string{"name", "description"} {
				if form.Has(k) {
					prod[k] = form.Get(k)
				}
			}
			if form.Has("active") {
				prod["active"] = form.Get("active") == "true"
			}
		}
		f.answer(w, 200, prod)
	case p == "/v1/prices" && m == http.MethodGet:
		var found []map[string]any
		for _, pr := range f.prices {
			for _, k := range form["lookup_keys[]"] {
				if pr["lookup_key"] == k {
					found = append(found, pr)
				}
			}
		}
		list(found)
	case p == "/v1/prices" && m == http.MethodPost:
		lk := form.Get("lookup_key")
		for _, pr := range f.prices {
			if lk != "" && pr["lookup_key"] == lk {
				if form.Get("transfer_lookup_key") != "true" {
					f.answer(w, 400, stripeError(400, "A price with this lookup key already exists."))
					return
				}
				pr["lookup_key"] = nil
			}
		}
		var amount int64
		fmt.Sscan(form.Get("unit_amount"), &amount)
		pr := map[string]any{"id": f.id("price_"), "object": "price", "product": form.Get("product"), "unit_amount": amount,
			"currency": form.Get("currency"), "type": "one_time", "active": true, "lookup_key": lk}
		f.prices = append(f.prices, pr)
		f.answer(w, 200, pr)
	case strings.HasPrefix(p, "/v1/prices/") && m == http.MethodPost:
		id := strings.TrimPrefix(p, "/v1/prices/")
		for _, pr := range f.prices {
			if pr["id"] == id {
				if form.Has("active") {
					pr["active"] = form.Get("active") == "true"
				}
				f.answer(w, 200, pr)
				return
			}
		}
		missing()
	case p == "/v1/webhook_endpoints" && m == http.MethodGet:
		var newest []map[string]any
		for i := len(f.endpoints) - 1; i >= 0; i-- {
			e := map[string]any{}
			for k, v := range f.endpoints[i] {
				if k != "secret" { // Stripe answers the secret on create only
					e[k] = v
				}
			}
			newest = append(newest, e)
		}
		list(newest)
	case p == "/v1/webhook_endpoints" && m == http.MethodPost:
		secret := "whsec" + "_" + fmt.Sprintf("FakeSigningSecret%016d", len(f.webhookSecs)+1)
		f.webhookSecs = append(f.webhookSecs, secret)
		e := map[string]any{"id": f.id("we_"), "object": "webhook_endpoint", "url": form.Get("url"),
			"enabled_events": form["enabled_events[]"], "status": "enabled", "secret": secret}
		f.endpoints = append(f.endpoints, e)
		if f.noSecret {
			withheld := map[string]any{}
			for k, v := range e {
				if k != "secret" {
					withheld[k] = v
				}
			}
			f.answer(w, 200, withheld)
			return
		}
		f.answer(w, 200, e)
	case strings.HasPrefix(p, "/v1/webhook_endpoints/"):
		id := strings.TrimPrefix(p, "/v1/webhook_endpoints/")
		for i, e := range f.endpoints {
			if e["id"] != id {
				continue
			}
			switch m {
			case http.MethodDelete:
				if f.deleteFails {
					f.answer(w, 500, stripeError(500, "An error occurred with our connection to Stripe."))
					return
				}
				f.endpoints = append(f.endpoints[:i], f.endpoints[i+1:]...)
				f.answer(w, 200, map[string]any{"id": id, "deleted": true})
			case http.MethodPost:
				if form.Has("enabled_events[]") {
					e["enabled_events"] = form["enabled_events[]"]
				}
				f.answer(w, 200, map[string]any{"id": id, "url": e["url"], "enabled_events": e["enabled_events"]})
			}
			return
		}
		missing()
	case strings.HasPrefix(p, "/v1/payment_intents/"):
		if f.payment == nil || f.payment["id"] != strings.TrimPrefix(p, "/v1/payment_intents/") {
			missing()
			return
		}
		f.answer(w, 200, f.payment)
	case p == "/v1/events":
		items := f.events
		if len(items) > 100 {
			items = items[:100]
		}
		limit := 10
		fmt.Sscan(form.Get("limit"), &limit)
		more := len(items) > limit
		if more {
			items = items[:limit]
		}
		f.answer(w, 200, map[string]any{"object": "list", "data": items, "has_more": more})
	case m == http.MethodGet && (p == "/v1/checkout/sessions" || p == "/v1/payment_intents" || p == "/v1/charges" || p == "/v1/customers"):
		list(nil)
	case p == "/v1/balance":
		f.answer(w, 200, map[string]any{"object": "balance"})
	default:
		missing()
	}
}

// vaultNames maps each fake vault's resource id, in lower case, to its name as keyVaults lists it.
var vaultNames = map[string]string{
	strings.ToLower(allowedVaultID): allowedKV,
	strings.ToLower(otherVaultID):   otherKV,
}

func (f *fake) serveARM(w http.ResponseWriter, r *http.Request, raw []byte) {
	f.vaultCalls = append(f.vaultCalls, call{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Body: string(raw)})
	if r.Header.Get("Authorization") != "Bearer "+armToken {
		f.answer(w, 401, map[string]any{"error": map[string]any{"code": "InvalidAuthenticationToken"}})
		return
	}
	q := r.URL.Query()
	version := q.Get("api-version")
	path := r.URL.Path

	switch {
	case path == "/subscriptions" && r.Method == http.MethodGet:
		if version != "2022-12-01" {
			w.WriteHeader(400)
			return
		}
		if q.Get("page") == "2" {
			f.answer(w, 200, map[string]any{"value": []any{
				map[string]any{"id": "/subscriptions/" + subTwo, "subscriptionId": subTwo, "state": "Enabled"},
			}})
			return
		}
		f.answer(w, 200, map[string]any{
			"value":    []any{map[string]any{"id": "/subscriptions/" + subOne, "subscriptionId": subOne, "state": "Enabled"}},
			"nextLink": f.srv.URL + "/subscriptions?api-version=2022-12-01&page=2",
		})
		return
	case strings.HasSuffix(path, "/resources") && r.Method == http.MethodGet:
		if version != "2021-04-01" || q.Get("$filter") != "resourceType eq 'Microsoft.KeyVault/vaults'" || strings.Contains(r.URL.RawQuery, "+") {
			w.WriteHeader(400)
			return
		}
		switch strings.TrimSuffix(strings.TrimPrefix(path, "/subscriptions/"), "/resources") {
		case subOne:
			// Its next page is on another host, which the plugin must not follow with the token.
			f.answer(w, 200, map[string]any{
				"value":    []any{map[string]any{"id": otherVaultID, "name": "other-vault", "type": "Microsoft.KeyVault/vaults"}},
				"nextLink": f.foreign.URL + "/subscriptions/" + subOne + "/resources?page=2",
			})
		case subTwo:
			f.answer(w, 200, map[string]any{"value": []any{
				map[string]any{"id": allowedVaultID, "name": "Payments-Vault", "type": "Microsoft.KeyVault/vaults"},
			}})
		default:
			w.WriteHeader(404)
		}
		return
	}

	vaultPath, name, ok := strings.Cut(path, "/secrets/")
	vault, known := vaultNames[strings.ToLower(vaultPath)]
	if !ok || !known || name == "" || strings.Contains(name, "/") {
		f.answer(w, 404, map[string]any{"error": map[string]any{"code": "ResourceNotFound"}})
		return
	}
	if version != "2023-07-01" {
		w.WriteHeader(400)
		return
	}
	if st := f.vaultStatus[vault]; st != 0 {
		f.answer(w, st, map[string]any{"error": map[string]any{"code": "AuthorizationFailed"}})
		return
	}
	key := vault + "/" + name
	uri := "https://" + vault + ".vault.azure.net/secrets/" + name
	switch r.Method {
	case http.MethodGet:
		versions := f.secrets[key]
		if len(versions) == 0 {
			f.answer(w, 404, map[string]any{"error": map[string]any{"code": "ResourceNotFound"}})
			return
		}
		// Azure management answers a secret's properties, never its value.
		f.answer(w, 200, map[string]any{"id": path, "name": name, "properties": map[string]any{"secretUri": uri}})
	case http.MethodPut:
		var body struct {
			Properties struct {
				Value       string `json:"value"`
				ContentType string `json:"contentType"`
			} `json:"properties"`
		}
		if json.Unmarshal(raw, &body) != nil || body.Properties.Value == "" {
			w.WriteHeader(400)
			return
		}
		f.secrets[key] = append(f.secrets[key], body.Properties.Value)
		v := fmt.Sprintf("v%031d", len(f.secrets[key]))
		f.answer(w, 200, map[string]any{"id": path, "name": name, "type": "Microsoft.KeyVault/vaults/secrets",
			"properties": map[string]any{"contentType": body.Properties.ContentType, "secretUri": uri, "secretUriWithVersion": uri + "/" + v}})
	default:
		w.WriteHeader(405)
	}
}

// vaultWrites are the calls to Azure management that are not a GET.
func (f *fake) vaultWrites() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []call
	for _, c := range f.vaultCalls {
		if c.Method != http.MethodGet {
			out = append(out, c)
		}
	}
	return out
}

// sent are the requests to Stripe and to Azure management that are not a GET.
func (f *fake) sent() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []call
	for _, c := range append(append([]call{}, f.calls...), f.vaultCalls...) {
		if c.Method != http.MethodGet {
			out = append(out, c)
		}
	}
	return out
}

// writes are the requests that would change something in Stripe.
func (f *fake) writes() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []call
	for _, c := range f.calls {
		if c.Method != http.MethodGet && !strings.Contains(c.Path, probeID) {
			out = append(out, c)
		}
	}
	return out
}

// ---- running the plugin ----

type runOpts struct {
	key      string
	keyField string // the stripe connection's field holding the key: key (the Stripe preset) by default
	noKey    bool   // bind no stripe connection at all
	config   map[string]any
	vault    bool // bind the microsoft connection, its token as accessToken
	tokens   bool // with vault: Azure management's token in tokens, and another API's as accessToken
	workdir  string
}

type records struct {
	raw      string
	lines    []map[string]any
	result   map[string]any
	publish  []map[string]any
	progress []string
}

func (r records) ok() bool { return r.result["ok"] == true }

func (r records) text() string {
	if s, ok := r.result["output"].(string); ok {
		return s
	}
	s, _ := r.result["error"].(string)
	return s
}

func (r records) events(suffix string) []map[string]any {
	var out []map[string]any
	for _, p := range r.publish {
		if p["type"] == suffix {
			out = append(out, p["payload"].(map[string]any))
		}
	}
	return out
}

func runPlugin(t *testing.T, o runOpts, instructions ...string) records {
	t.Helper()
	if o.key == "" {
		o.key = keyA
	}
	if o.workdir == "" {
		o.workdir = t.TempDir()
	}
	var work []any
	for i, in := range instructions {
		work = append(work, map[string]any{"seq": i + 1, "instruction": in, "payload": map[string]any{"instruction": in}})
	}
	conns := map[string]any{}
	if !o.noKey {
		field, preset := o.keyField, "stripe"
		if field == "" {
			field = "key"
		} else if field != "key" {
			preset = "other"
		}
		conns[keySlot] = map[string]any{"kind": "apikey", "account": "Shop - stripe", "preset": preset, "fields": map[string]any{field: o.key}}
	}
	if o.vault && o.tokens {
		conns["microsoft"] = map[string]any{"provider": "microsoft", "account": "person@example.com", "accessToken": graphToken,
			"tokens": map[string]any{
				"https://graph.microsoft.com/":  map[string]any{"accessToken": graphToken, "scopes": []string{"User.Read"}},
				"https://management.azure.com/": map[string]any{"accessToken": armToken, "scopes": []string{"https://management.azure.com/user_impersonation"}},
			}}
	} else if o.vault {
		conns["microsoft"] = map[string]any{"provider": "microsoft", "account": "person@example.com", "accessToken": armToken}
	}
	cfg := o.config
	if cfg == nil {
		cfg = map[string]any{}
	}
	req := map[string]any{
		"protocol":         "harness.member/1",
		"work":             work,
		"config":           cfg,
		"secrets":          map[string]any{},
		"connections":      conns,
		"workingDirectory": o.workdir,
	}
	b, _ := json.Marshal(req)
	var out bytes.Buffer
	run(bytes.NewReader(b), &out, func() time.Time { return testNow })

	rec := records{raw: out.String()}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("not a JSON line: %q", line)
		}
		rec.lines = append(rec.lines, m)
		switch m["t"] {
		case "result":
			if rec.result != nil {
				t.Fatalf("two results:\n%s", out.String())
			}
			rec.result = m
		case "publish":
			rec.publish = append(rec.publish, m)
		case "progress":
			rec.progress = append(rec.progress, m["status"].(string))
		}
	}
	if rec.result == nil {
		t.Fatalf("no result:\n%s", out.String())
	}
	return rec
}

// setUp runs setup once so the account is kept in workdir, and answers workdir.
func setUp(t *testing.T, key string) string {
	t.Helper()
	dir := t.TempDir()
	if r := runPlugin(t, runOpts{key: key, workdir: dir}, "setup"); !r.ok() {
		t.Fatalf("setup failed: %s", r.text())
	}
	return dir
}

// filesHolding answers every file under dir whose content holds any of values.
func filesHolding(t *testing.T, dir string, values ...string) []string {
	t.Helper()
	var found []string
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, _ := os.ReadFile(path)
		for _, v := range values {
			if bytes.Contains(raw, []byte(v)) {
				found = append(found, path)
			}
		}
		return nil
	})
	sort.Strings(found)
	return found
}
