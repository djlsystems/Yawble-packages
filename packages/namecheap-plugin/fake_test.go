package main

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// A fake Namecheap API: getList, getHosts and setHosts over the account's domains, with the checks
// Namecheap makes on every call (API user and key, API access, the allow-list). No test reaches the
// network.

const (
	testUser = "apiuser-placeholder"
	testKey  = "0123456789abcdef0123456789abcdef"
	testIP   = "203.0.113.7"
)

type fakeDomain struct {
	ourDNS    bool
	emailType string
	hosts     []xmlHost
}

type fakeNamecheap struct {
	t  *testing.T
	mu sync.Mutex

	domains   map[string]*fakeDomain
	accessOff bool
	allowed   map[string]bool

	failGetHosts bool   // every getHosts answers an error
	failSetHosts bool   // every setHosts answers an error
	dropOnSet    string // a host name setHosts silently loses
	echoKey      bool   // error answers carry the key, as a careless API might

	commands []string    // each call's Command
	at       []time.Time // each call's time on the test clock
	clock    *fakeClock
}

func newFake(t *testing.T) *fakeNamecheap {
	f := &fakeNamecheap{t: t, domains: map[string]*fakeDomain{}, allowed: map[string]bool{testIP: true}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	ipSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, testIP)
	}))
	oldProd, oldSandbox, oldAddr := productionURL, sandboxURL, addressURL
	productionURL, sandboxURL, addressURL = srv.URL+"/xml.response", srv.URL+"/sandbox/xml.response", ipSrv.URL
	t.Cleanup(func() {
		srv.Close()
		ipSrv.Close()
		productionURL, sandboxURL, addressURL = oldProd, oldSandbox, oldAddr
	})
	return f
}

func (f *fakeNamecheap) add(name string, ourDNS bool, hosts ...xmlHost) {
	f.domains[name] = &fakeDomain{ourDNS: ourDNS, emailType: "NONE", hosts: hosts}
}

func hr(name, typ, address string, ttl int) xmlHost {
	return xmlHost{Name: name, Type: typ, Address: address, MXPref: "10", TTL: strconv.Itoa(ttl)}
}

func (f *fakeNamecheap) hosts(domain string) []xmlHost {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]xmlHost{}, f.domains[domain].hosts...)
}

func (f *fakeNamecheap) count(command string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.commands {
		if c == command {
			n++
		}
	}
	return n
}

func (f *fakeNamecheap) fail(w http.ResponseWriter, number, message string) {
	if f.echoKey {
		message += " (key " + testKey + ")"
	}
	fmt.Fprintf(w, `<?xml version="1.0" encoding="utf-8"?>
<ApiResponse Status="ERROR"><Errors><Error Number="%s">%s</Error></Errors><Warnings /><RequestedCommand /></ApiResponse>`, number, xmlEscape(message))
}

func xmlEscape(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func (f *fakeNamecheap) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		f.t.Errorf("a %s reached Namecheap; every call is a POST", r.Method)
	}
	if strings.Contains(r.URL.RawQuery, "ApiKey") {
		f.t.Errorf("the API key was sent in a URL")
	}
	_ = r.ParseForm()
	f.mu.Lock()
	defer f.mu.Unlock()
	cmd := r.PostForm.Get("Command")
	f.commands = append(f.commands, cmd)
	if f.clock != nil {
		f.at = append(f.at, f.clock.t)
	}

	if f.accessOff || r.PostForm.Get("ApiUser") != testUser || r.PostForm.Get("ApiKey") != testKey || r.PostForm.Get("UserName") != testUser {
		f.fail(w, "1011102", "API Key is invalid or API access has not been enabled")
		return
	}
	if ip := r.PostForm.Get("ClientIp"); !f.allowed[ip] {
		f.fail(w, "1011150", "Invalid request IP: "+ip)
		return
	}

	domain := r.PostForm.Get("SLD") + "." + r.PostForm.Get("TLD")
	d := f.domains[domain]
	ok := func(inner string) {
		fmt.Fprintf(w, `<?xml version="1.0" encoding="utf-8"?>
<ApiResponse Status="OK"><Errors /><Warnings /><RequestedCommand>%s</RequestedCommand><CommandResponse Type="%s">%s</CommandResponse></ApiResponse>`, strings.ToLower(cmd), cmd, inner)
	}

	switch cmd {
	case "namecheap.domains.getList":
		var names []string
		for n := range f.domains {
			names = append(names, n)
		}
		sort.Strings(names)
		var b strings.Builder
		b.WriteString("<DomainGetListResult>")
		for i, n := range names {
			fmt.Fprintf(&b, `<Domain ID="%d" Name="%s" User="%s" IsExpired="false" IsLocked="false" AutoRenew="false" WhoisGuard="ENABLED" IsPremium="false" IsOurDNS="%t"/>`, i+1, n, testUser, f.domains[n].ourDNS)
		}
		fmt.Fprintf(&b, "</DomainGetListResult><Paging><TotalItems>%d</TotalItems><CurrentPage>1</CurrentPage><PageSize>100</PageSize></Paging>", len(names))
		ok(b.String())
	case "namecheap.domains.dns.getHosts":
		if f.failGetHosts {
			f.fail(w, "2030166", "Edit permission for domain is not supported")
			return
		}
		if d == nil {
			f.fail(w, "2019166", "Domain not found")
			return
		}
		var b strings.Builder
		fmt.Fprintf(&b, `<DomainDNSGetHostsResult Domain="%s" EmailType="%s" IsUsingOurDNS="%t">`, domain, d.emailType, d.ourDNS)
		for i, x := range d.hosts {
			fmt.Fprintf(&b, `<host HostId="%d" Name="%s" Type="%s" Address="%s" MXPref="%s" TTL="%s" AssociatedAppTitle="" FriendlyName="" IsActive="true" IsDDNSEnabled="false"/>`,
				i+1, xmlEscape(x.Name), x.Type, xmlAttr(x.Address), x.MXPref, x.TTL)
		}
		b.WriteString("</DomainDNSGetHostsResult>")
		ok(b.String())
	case "namecheap.domains.dns.setHosts":
		if f.failSetHosts {
			f.fail(w, "2050900", "Unknown error when setting hosts")
			return
		}
		if d == nil {
			f.fail(w, "2019166", "Domain not found")
			return
		}
		var hosts []xmlHost
		for i := 1; r.PostForm.Get("HostName"+strconv.Itoa(i)) != "" || r.PostForm.Get("RecordType"+strconv.Itoa(i)) != ""; i++ {
			n := strconv.Itoa(i)
			x := xmlHost{Name: r.PostForm.Get("HostName" + n), Type: r.PostForm.Get("RecordType" + n), Address: r.PostForm.Get("Address" + n), MXPref: r.PostForm.Get("MXPref" + n), TTL: r.PostForm.Get("TTL" + n)}
			if x.MXPref == "" {
				x.MXPref = "10"
			}
			if x.TTL == "" {
				x.TTL = "1800"
			}
			if strings.EqualFold(x.Name, f.dropOnSet) {
				continue
			}
			// AS THE REAL API DOES (measured on 13 October 2026): a CAA record's value is written
			// unquoted, "0 issue ca.example.net", refused quoted, and read back quoted.
			if strings.EqualFold(x.Type, "CAA") {
				parts := strings.SplitN(x.Address, " ", 3)
				if len(parts) != 3 || strings.HasPrefix(parts[2], `"`) {
					f.fail(w, "2050900", fmt.Sprintf("CAA record with tag %q has incorrect value.", parts[min(1, len(parts)-1)]))
					return
				}
				x.Address = parts[0] + " " + parts[1] + " " + strconv.Quote(parts[2])
			}
			hosts = append(hosts, x)
		}
		d.hosts = hosts
		if e := r.PostForm.Get("EmailType"); e != "" {
			d.emailType = e
		}
		ok(fmt.Sprintf(`<DomainDNSSetHostsResult Domain="%s" IsSuccess="true"><Warnings /></DomainDNSSetHostsResult>`, domain))
	default:
		f.fail(w, "1010900", "Unknown command")
	}
}

func xmlAttr(s string) string {
	return strings.NewReplacer("&", "&amp;", `"`, "&quot;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// ---- running the plugin --------------------------------------------------------------------------

type fakeClock struct {
	t     time.Time
	slept []time.Duration
}

func (c *fakeClock) clock() clock {
	return clock{
		now:   func() time.Time { return c.t },
		sleep: func(d time.Duration) { c.slept = append(c.slept, d); c.t = c.t.Add(d) },
	}
}

type harness struct {
	t     *testing.T
	fake  *fakeNamecheap
	ns1   *fakeDNS // the test domains' two authoritative name servers
	ns2   *fakeDNS
	dir   string
	clock *fakeClock
	cfg   map[string]any
	sec   map[string]string
}

func newHarness(t *testing.T) *harness {
	f := newFake(t)
	c := &fakeClock{t: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
	f.clock = c
	ns1, ns2 := newFakeDNS(t, "ns1.example.net", f), newFakeDNS(t, "ns2.example.net", f)
	oldServers := authServers
	authServers = func(string) ([]nameServer, error) { return []nameServer{ns1.server(), ns2.server()}, nil }
	t.Cleanup(func() { authServers = oldServers })
	return &harness{
		t: t, fake: f, ns1: ns1, ns2: ns2, dir: t.TempDir(), clock: c,
		cfg: map[string]any{"domains": []string{"example.com"}, "mode": "apply"},
		sec: map[string]string{"apiUser": testUser, "apiKey": testKey},
	}
}

type runResult struct {
	code    int
	raw     string
	ok      bool
	output  string // the result's output or error
	events  []map[string]any
	records []map[string]any
}

func (h *harness) run(instructions ...string) runResult {
	h.t.Helper()
	return h.runWith(h.clock.clock(), instructions...)
}

func (h *harness) runWith(c clock, instructions ...string) runResult {
	h.t.Helper()
	var work []map[string]any
	for _, in := range instructions {
		work = append(work, map[string]any{"instruction": in})
	}
	req, _ := json.Marshal(map[string]any{
		"protocol": "harness.member/1", "work": work, "config": h.cfg, "secrets": h.sec, "workingDirectory": h.dir,
	})
	var out bytes.Buffer
	code := run(bytes.NewReader(req), &out, c)
	h.clock.t = h.clock.t.Add(time.Second)

	res := runResult{code: code, raw: out.String()}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			h.t.Fatalf("a line that is not JSON: %q", line)
		}
		res.records = append(res.records, rec)
		switch rec["t"] {
		case "publish":
			res.events = append(res.events, rec)
		case "result":
			res.ok, _ = rec["ok"].(bool)
			if res.ok {
				res.output, _ = rec["output"].(string)
			} else {
				res.output, _ = rec["error"].(string)
			}
		}
	}
	return res
}

func (r runResult) event(typ string) map[string]any {
	for _, e := range r.events {
		if e["type"] == typ {
			p, _ := e["payload"].(map[string]any)
			return p
		}
	}
	return nil
}

func (h *harness) backups(domain string) []string {
	entries, _ := os.ReadDir(filepath.Join(h.dir, backupFolder, domain))
	var ids []string
	for _, e := range entries {
		ids = append(ids, strings.TrimSuffix(e.Name(), ".json"))
	}
	return ids
}

func mustContain(t *testing.T, got string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("missing %q in:\n%s", w, got)
		}
	}
}
