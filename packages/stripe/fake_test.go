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

// The fakes: one TLS server answering Stripe's API under /v1 and every Key Vault under
// /vault/<name>, so a test sees every request the plugin makes and nothing reaches a network.

const (
	accountA   = "acct_1TestAccountAaaa"
	accountB   = "acct_1TestAccountBbbb"
	vaultToken = "eyJ0eXAiOiJKV1QifakeVaultAccessToken0000"
	allowedKV  = "payments-vault"
	otherKV    = "other-vault"
)

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
	vaultCalls  []call
	products    map[string]map[string]any
	prices      []map[string]any
	endpoints   []map[string]any
	events      []map[string]any
	idem        map[string][]byte
	secrets     map[string][]string // vault/name -> versions' values
	vaultStatus map[string]int      // vault -> status to answer
	nextID      int
	webhookSecs []string // every signing secret the fake handed out
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

	oldBase, oldVault, oldClient := stripeBase, vaultBase, httpClient
	stripeBase = f.srv.URL
	vaultBase = func(vault string) string { return f.srv.URL + "/vault/" + vault }
	httpClient = f.srv.Client()
	t.Cleanup(func() { stripeBase, vaultBase, httpClient = oldBase, oldVault, oldClient })
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

	if strings.HasPrefix(r.URL.Path, "/vault/") {
		f.serveVault(w, r, raw)
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
		if k.missing[resource] || (level == "read" && k.missing[resource+":read"]) {
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
		f.answer(w, 200, e)
	case strings.HasPrefix(p, "/v1/webhook_endpoints/"):
		id := strings.TrimPrefix(p, "/v1/webhook_endpoints/")
		for i, e := range f.endpoints {
			if e["id"] != id {
				continue
			}
			switch m {
			case http.MethodDelete:
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

func (f *fake) serveVault(w http.ResponseWriter, r *http.Request, raw []byte) {
	rest := strings.TrimPrefix(r.URL.Path, "/vault/")
	vault, path, _ := strings.Cut(rest, "/")
	f.vaultCalls = append(f.vaultCalls, call{Method: r.Method, Path: r.URL.Path})
	if r.Header.Get("Authorization") != "Bearer "+vaultToken {
		w.WriteHeader(401)
		return
	}
	if st := f.vaultStatus[vault]; st != 0 {
		w.WriteHeader(st)
		return
	}
	name, ok := strings.CutPrefix(path, "secrets/")
	if r.Method != http.MethodPut || !ok || r.URL.Query().Get("api-version") == "" {
		w.WriteHeader(400)
		return
	}
	var body struct {
		Value string `json:"value"`
	}
	if json.Unmarshal(raw, &body) != nil || body.Value == "" {
		w.WriteHeader(400)
		return
	}
	key := vault + "/" + name
	f.secrets[key] = append(f.secrets[key], body.Value)
	version := fmt.Sprintf("v%031d", len(f.secrets[key]))
	// Key Vault echoes the value in its answer; the plugin must read the id only.
	f.answer(w, 200, map[string]any{"value": body.Value, "id": "https://" + vault + ".vault.azure.net/secrets/" + name + "/" + version})
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
	key     string
	config  map[string]any
	vault   bool // bind the microsoft connection
	workdir string
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
	if o.vault {
		conns["microsoft"] = map[string]any{"provider": "microsoft", "account": "person@example.com", "accessToken": vaultToken}
	}
	cfg := o.config
	if cfg == nil {
		cfg = map[string]any{}
	}
	req := map[string]any{
		"protocol":         "harness.member/1",
		"work":             work,
		"config":           cfg,
		"secrets":          map[string]any{"apiKey": o.key},
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
