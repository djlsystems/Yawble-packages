package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// The fake Twilio: one TLS server answering the REST API, the messaging API and media URLs, so a
// test sees every request the plugin makes.

const (
	testAccount = "ACaaaaaaaaaaaaaaaaaaaaaaaaaaaa1234"
	testKeySid  = "SKbbbbbbbbbbbbbbbbbbbbbbbbbbbb5678"
	testSecret  = "s3cr3t-value-never-printed-xyz"
	ownCell     = "+15551234567"
	tollFree    = "+18445550100"
	localNumber = "+16175550100"
)

var testNow = time.Date(2026, 10, 9, 15, 0, 0, 0, time.UTC) // a Thursday, 11:00 in New York

type call struct {
	Method, Path string
	Query, Form  url.Values
	User, Pass   string
}

type media struct {
	status int
	ctype  string
	length int64
}

type fake struct {
	t   *testing.T
	srv *httptest.Server
	mu  sync.Mutex

	calls []call

	acct        account
	acctStatus  int
	numbers     []number
	numbersCode int
	messages    map[string]message // fetch by SID
	inbound     []message          // listed for To=<our number>, paged by pageSize
	pageSize    int
	listAll     []message // listed with no To filter
	post        func(form url.Values) (int, any)
	tooMany     int // answer this many 429s first, to any request
	campaigns   []campaign
	tollfree    []tollFreeVerification
	tollfreeErr int
	media       map[string]media
	balance     bool
}

func newFake(t *testing.T) *fake {
	f := &fake{t: t, messages: map[string]message{}, media: map[string]media{}, pageSize: 50}
	f.acct = account{Sid: testAccount, FriendlyName: "Home", Type: "Full", Status: "active"}
	f.numbers = []number{num(tollFree, "PN0001", true, true)}
	f.post = func(form url.Values) (int, any) {
		return 201, map[string]any{"sid": "SM0000000000000000000000000000new1", "status": "queued", "from": form.Get("From"), "to": form.Get("To"), "num_segments": "1"}
	}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)

	oldAPI, oldMsg, oldClient, oldSleep := apiBase, messagingBase, httpClient, sleep
	apiBase, messagingBase, httpClient = f.srv.URL, f.srv.URL+"/messaging", f.srv.Client()
	sleep = func(time.Duration) {}
	t.Cleanup(func() { apiBase, messagingBase, httpClient, sleep = oldAPI, oldMsg, oldClient, oldSleep })
	return f
}

func num(phone, sid string, sms, mms bool) number {
	n := number{Sid: sid, PhoneNumber: phone}
	n.Capabilities.SMS, n.Capabilities.MMS, n.Capabilities.Voice = sms, mms, true
	return n
}

func (f *fake) mediaURL(name string) string { return f.srv.URL + "/media/" + name }

func twilioDate(t time.Time) string { return t.UTC().Format(time.RFC1123Z) }

func (f *fake) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_ = r.ParseForm()
	user, pass, _ := r.BasicAuth()
	form := url.Values{}
	if r.Method == http.MethodPost {
		form = r.PostForm
	}
	f.calls = append(f.calls, call{r.Method, r.URL.Path, r.URL.Query(), form, user, pass})

	reply := func(status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	twilioErr := func(status, code int, msg string) {
		reply(status, map[string]any{"code": code, "message": msg, "status": status})
	}

	if strings.HasPrefix(r.URL.Path, "/media/") {
		m, ok := f.media[strings.TrimPrefix(r.URL.Path, "/media/")]
		if !ok {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Type", m.ctype)
		if m.length >= 0 {
			w.Header().Set("Content-Length", fmt.Sprint(m.length))
		}
		w.WriteHeader(m.status)
		return
	}

	if f.tooMany > 0 {
		f.tooMany--
		twilioErr(429, 20429, "Too Many Requests")
		return
	}
	if user != testKeySid || pass != testSecret {
		twilioErr(401, 20003, "Authenticate")
		return
	}

	acctPath := "/2010-04-01/Accounts/" + testAccount
	switch p := r.URL.Path; {
	case p == acctPath+".json":
		if f.acctStatus != 0 {
			twilioErr(f.acctStatus, 20403, "Forbidden")
			return
		}
		reply(200, f.acct)
	case p == acctPath+"/Balance.json":
		if !f.balance {
			twilioErr(403, 20403, "Forbidden")
			return
		}
		reply(200, map[string]any{"balance": "12.34", "currency": "USD"})
	case p == acctPath+"/IncomingPhoneNumbers.json":
		if f.numbersCode != 0 {
			twilioErr(f.numbersCode, 20403, "Forbidden")
			return
		}
		out := []number{}
		for _, n := range f.numbers {
			if q := r.URL.Query().Get("PhoneNumber"); q == "" || q == n.PhoneNumber {
				out = append(out, n)
			}
		}
		reply(200, map[string]any{"incoming_phone_numbers": out})
	case p == acctPath+"/Messages.json" && r.Method == http.MethodPost:
		status, body := f.post(form)
		reply(status, body)
	case p == acctPath+"/Messages.json":
		q := r.URL.Query()
		var pool []message
		if q.Get("To") == "" && q.Get("From") == "" {
			pool = f.listAll
		} else {
			for _, m := range append(append([]message{}, f.inbound...), f.listAll...) {
				if (q.Get("To") == "" || q.Get("To") == m.To) && (q.Get("From") == "" || q.Get("From") == m.From) {
					pool = append(pool, m)
				}
			}
		}
		page := 0
		fmt.Sscan(q.Get("Page"), &page)
		start, end := page*f.pageSize, (page+1)*f.pageSize
		if start > len(pool) {
			start = len(pool)
		}
		if end > len(pool) {
			end = len(pool)
		}
		var next *string
		if end < len(pool) {
			nq := url.Values{}
			for k, v := range q {
				nq[k] = v
			}
			nq.Set("Page", fmt.Sprint(page+1))
			s := acctPath + "/Messages.json?" + nq.Encode()
			next = &s
		}
		reply(200, map[string]any{"messages": pool[start:end], "next_page_uri": next})
	case strings.HasPrefix(p, acctPath+"/Messages/"):
		sid := strings.TrimSuffix(strings.TrimPrefix(p, acctPath+"/Messages/"), ".json")
		m, ok := f.messages[sid]
		if !ok {
			twilioErr(404, 20404, "The requested resource was not found")
			return
		}
		reply(200, m)
	case strings.HasPrefix(p, "/messaging/v1/Services/") && strings.HasSuffix(p, "/Compliance/Usa2p"):
		reply(200, map[string]any{"compliance": f.campaigns})
	case p == "/messaging/v1/Tollfree/Verifications":
		if f.tollfreeErr != 0 {
			twilioErr(f.tollfreeErr, 20403, "Forbidden")
			return
		}
		reply(200, map[string]any{"verifications": f.tollfree})
	default:
		twilioErr(404, 20404, "not found: "+p)
	}
}

func (f *fake) posts() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []call
	for _, c := range f.calls {
		if c.Method == http.MethodPost && strings.HasSuffix(c.Path, "/Messages.json") {
			out = append(out, c)
		}
	}
	return out
}

// ---- running the plugin -----------------------------------------------------------------------

type docIn struct {
	ID        string
	Doc       any
	UpdatedBy string
	UpdatedAt string
}

type siteState struct {
	missing    bool
	state      []docIn
	messages   []docIn
	recipients []docIn
}

type result struct {
	records []map[string]any
	ok      bool
	text    string // output, or the error
	quiet   bool
	code    int
}

func (r result) of(kind string) []map[string]any {
	var out []map[string]any
	for _, rec := range r.records {
		if rec["t"] == kind {
			out = append(out, rec)
		}
	}
	return out
}

func (r result) published(suffix string) map[string]any {
	for _, rec := range r.of("publish") {
		if rec["type"] == suffix {
			return rec["payload"].(map[string]any)
		}
	}
	return nil
}

// put finds the last site.put to a collection and id.
func (r result) put(collection, id string) map[string]any {
	var doc map[string]any
	for _, rec := range r.of("site.put") {
		if rec["collection"] == collection && rec["id"] == id {
			doc = rec["doc"].(map[string]any)
		}
	}
	return doc
}

type opts struct {
	cfg     map[string]any
	secrets map[string]string // the fields of the Twilio connection bound to slot
	unbound bool              // no connection bound to slot at all
	legacy  map[string]string // values handed under secrets, which the plugin must ignore
	site    *siteState
	now     time.Time
}

func goodSecrets() map[string]string {
	return map[string]string{"accountSid": testAccount, "apiKeySid": testKeySid, "apiKeySecret": testSecret}
}

func runWith(t *testing.T, o opts, instructions ...string) result {
	t.Helper()
	if o.cfg == nil {
		o.cfg = map[string]any{}
	}
	if _, ok := o.cfg["sendAllowlist"]; !ok {
		o.cfg["sendAllowlist"] = []string{ownCell}
	}
	// A test that does not name the opt-out line sends its text as given; the compliance tests
	// name it, and TestEveryTextCarriesTheOptOutLineByDefault reads the real default.
	if _, ok := o.cfg["optOutFooter"]; !ok {
		o.cfg["optOutFooter"] = "off"
	}
	if o.secrets == nil {
		o.secrets = goodSecrets()
	}
	if o.site == nil {
		o.site = &siteState{}
	}
	if o.now.IsZero() {
		o.now = testNow
	}
	collection := func(name string, docs []docIn) map[string]any {
		if o.site.missing {
			return map[string]any{"site": siteName, "collection": name, "documents": []any{}, "total": 0, "missing": "No such site."}
		}
		out := []any{}
		for _, d := range docs {
			by := d.UpdatedBy
			if by == "" {
				by = "person@example.com"
			}
			at := d.UpdatedAt
			if at == "" {
				at = "2026-10-01T12:00:00Z"
			}
			out = append(out, map[string]any{"id": d.ID, "doc": d.Doc, "updatedAt": at, "updatedBy": by})
		}
		return map[string]any{"site": siteName, "collection": name, "documents": out, "total": len(out)}
	}
	var work []any
	for _, ins := range instructions {
		work = append(work, map[string]any{"seq": 1, "instruction": ins})
	}
	req := map[string]any{
		"protocol": "harness.member/1",
		"work":     work,
		"config":   o.cfg,
		"secrets":  o.legacy,
		"sites": []any{
			collection(colState, o.site.state), collection(colMessages, o.site.messages), collection(colRecipients, o.site.recipients),
		},
	}
	if !o.unbound {
		req["connections"] = map[string]any{slot: map[string]any{
			"kind": "apikey", "account": "Shop Twilio", "preset": "twilio", "fields": o.secrets,
		}}
	}
	raw, _ := json.Marshal(req)
	var out bytes.Buffer
	code := run(bytes.NewReader(raw), &out, func() time.Time { return o.now })

	res := result{code: code}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("a line that is not a record: %q", line)
		}
		res.records = append(res.records, rec)
	}
	results := res.of("result")
	if len(results) != 1 {
		t.Fatalf("want exactly one result record, got %d: %s", len(results), out.String())
	}
	last := res.records[len(res.records)-1]
	if last["t"] != "result" {
		t.Fatalf("the result must be the last record: %s", out.String())
	}
	res.ok, _ = last["ok"].(bool)
	res.quiet, _ = last["quiet"].(bool)
	if res.ok {
		res.text, _ = last["output"].(string)
	} else {
		res.text, _ = last["error"].(string)
	}
	if strings.Contains(out.String(), testSecret) {
		t.Fatalf("the API key secret was written out: %s", out.String())
	}
	return res
}

func run1(t *testing.T, cfg map[string]any, instruction string) result {
	t.Helper()
	return runWith(t, opts{cfg: cfg}, instruction)
}

func wantFail(t *testing.T, r result, sentence string) {
	t.Helper()
	if r.ok {
		t.Fatalf("want failure %q, got ok: %q", sentence, r.text)
	}
	if r.text != sentence {
		t.Fatalf("wrong sentence\nwant: %s\ngot:  %s", sentence, r.text)
	}
}

func wantOK(t *testing.T, r result, text string) {
	t.Helper()
	if !r.ok {
		t.Fatalf("want ok %q, got failure: %q", text, r.text)
	}
	if r.text != text {
		t.Fatalf("wrong output\nwant: %s\ngot:  %s", text, r.text)
	}
}

func wantContains(t *testing.T, r result, parts ...string) {
	t.Helper()
	for _, p := range parts {
		if !strings.Contains(r.text, p) {
			t.Fatalf("want %q in:\n%s", p, r.text)
		}
	}
}

// wantBareLinks fails when a URL in the text has punctuation stuck to its end: the text is
// read as plain text, so a link must end at a space, a line end or the end of the text.
func wantBareLinks(t *testing.T, r result) {
	t.Helper()
	for _, m := range bareLinkRe.FindAllString(r.text, -1) {
		if strings.ContainsAny(m[len(m)-1:], ").,;:") {
			t.Fatalf("punctuation stuck to the link %q in:\n%s", m, r.text)
		}
	}
}

var bareLinkRe = regexp.MustCompile(`https?://[^\s]+`)

func sms(to, body string) string { return "sms\nTo: " + to + "\n\n" + body }

func send() map[string]any { return map[string]any{"mode": "send"} }

func cfg(kv ...any) map[string]any {
	m := map[string]any{}
	for i := 0; i < len(kv); i += 2 {
		m[kv[i].(string)] = kv[i+1]
	}
	return m
}

func mustJSON(v any) []byte {
	raw, _ := json.Marshal(v)
	return raw
}
