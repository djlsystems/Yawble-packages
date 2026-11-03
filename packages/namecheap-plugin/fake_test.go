package main

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net"
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

// A fake Namecheap API: getList, getInfo, getHosts, setHosts and the nameserver calls (dns.getList,
// dns.setCustom, dns.setDefault) over the account's domains, answered as Namecheap's API reference
// shows them, with the checks
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
	// nameservers is what dns.getList names: empty means BasicDNS's two when ourDNS, and is the
	// custom nameservers when not.
	nameservers []string
	// premiumDNS is getList's IsPremiumDNS. freeDNS is a domain on Namecheap FreeDNS: as the real
	// API does (Namecheap's Go SDK, "FreeDNS domain handling"), dns.getList answers it 2019166 and
	// domains.getInfo answers DnsDetails ProviderType="FreeDNS" with its nameservers.
	premiumDNS bool
	freeDNS    bool
}

type fakeNamecheap struct {
	t  *testing.T
	mu sync.Mutex

	domains   map[string]*fakeDomain
	accessOff bool
	allowed   map[string]bool

	failGetHosts bool                // every getHosts answers an error
	failSetHosts bool                // every setHosts answers an error
	failSetNS    bool                // every setCustom and setDefault answers an error
	ignoreSetNS  bool                // setCustom and setDefault answer Updated="true" and change nothing
	notOwned     string              // a domain dns.getList says is not associated with the account
	params       []map[string]string // each call's form, without the global parameters
	dropOnSet    string              // a host name setHosts silently loses
	echoKey      bool                // error answers carry the key, as a careless API might

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
<ApiResponse xmlns="http://api.namecheap.com/xml.response" Status="ERROR"><Errors><Error Number="%s">%s</Error></Errors><Warnings /><RequestedCommand /></ApiResponse>`, number, xmlEscape(message))
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
	own := map[string]string{}
	for k := range r.PostForm {
		switch k {
		case "ApiUser", "ApiKey", "UserName", "ClientIp", "Command":
		default:
			own[k] = r.PostForm.Get(k)
		}
	}
	f.params = append(f.params, own)
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
	if cmd == "namecheap.domains.getInfo" {
		domain = r.PostForm.Get("DomainName")
	}
	d := f.domains[domain]
	ok := func(inner string) {
		fmt.Fprintf(w, `<?xml version="1.0" encoding="utf-8"?>
<ApiResponse xmlns="http://api.namecheap.com/xml.response" Status="OK"><Errors /><Warnings /><RequestedCommand>%s</RequestedCommand><CommandResponse Type="%s">%s</CommandResponse></ApiResponse>`, strings.ToLower(cmd), cmd, inner)
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
	case "namecheap.domains.dns.getList":
		if domain == f.notOwned {
			f.fail(w, "2016166", "Domain is not associated with your account")
			return
		}
		if d == nil || d.freeDNS {
			f.fail(w, "2019166", "Domain name not found")
			return
		}
		names := d.nameservers
		if d.ourDNS && len(names) == 0 {
			names = []string{"dns1.registrar-servers.com", "dns2.registrar-servers.com"}
		}
		var b strings.Builder
		fmt.Fprintf(&b, `<DomainDNSGetListResult Domain="%s" IsUsingOurDNS="%t" IsPremiumDNS="%t" IsUsingFreeDNS="false">`, domain, d.ourDNS, d.premiumDNS)
		for _, n := range names {
			fmt.Fprintf(&b, "<Nameserver>%s</Nameserver>", xmlEscape(n))
		}
		b.WriteString("</DomainDNSGetListResult>")
		ok(b.String())
	case "namecheap.domains.getInfo":
		if d == nil {
			f.fail(w, "2019166", "Domain not found")
			return
		}
		provider := "CUSTOM"
		if d.freeDNS {
			provider = "FreeDNS"
		} else if d.ourDNS {
			provider = "ENOM"
		}
		var b strings.Builder
		fmt.Fprintf(&b, `<DomainGetInfoResult Status="Ok" ID="1706717" DomainName="%s" OwnerName="%s" IsOwner="true" IsPremium="false"><DomainDetails><CreatedDate>11/26/2021</CreatedDate><ExpiredDate>11/26/2027</ExpiredDate><NumYears>0</NumYears></DomainDetails><LockDetails /><Whoisguard Enabled="NotAlloted"><ID>0</ID></Whoisguard>`, domain, testUser)
		fmt.Fprintf(&b, `<PremiumDnsSubscription><UseAutoRenew>false</UseAutoRenew><SubscriptionId>-1</SubscriptionId><CreatedDate>0001-01-01T00:00:00</CreatedDate><ExpirationDate>0001-01-01T00:00:00</ExpirationDate><IsActive>%t</IsActive></PremiumDnsSubscription>`, d.premiumDNS)
		fmt.Fprintf(&b, `<DnsDetails ProviderType="%s" IsUsingOurDNS="%t" HostCount="%d" EmailType="No Email Service" DynamicDNSStatus="false" IsFailover="false">`, provider, d.ourDNS, len(d.hosts))
		for _, n := range d.nameservers {
			fmt.Fprintf(&b, "<Nameserver>%s</Nameserver>", xmlEscape(n))
		}
		b.WriteString("</DnsDetails><Modificationrights /></DomainGetInfoResult>")
		ok(b.String())
	case "namecheap.domains.dns.setCustom", "namecheap.domains.dns.setDefault":
		if d == nil {
			f.fail(w, "2019166", "Domain not found")
			return
		}
		if f.failSetNS {
			f.fail(w, "3050900", "Unknown error from Enom")
			return
		}
		result := "DomainDNSSetDefaultResult"
		if cmd == "namecheap.domains.dns.setCustom" {
			result = "DomainDNSSetCustomResult"
			list := r.PostForm.Get("Nameservers")
			if list == "" {
				f.fail(w, "2011166", "Parameter Nameservers is missing")
				return
			}
			if !f.ignoreSetNS {
				d.ourDNS, d.nameservers, d.premiumDNS, d.freeDNS = false, strings.Split(list, ","), false, false
			}
		} else if !f.ignoreSetNS {
			d.ourDNS, d.nameservers, d.premiumDNS, d.freeDNS = true, nil, false, false
		}
		ok(fmt.Sprintf(`<%s Domain="%s" Updated="true" />`, result, domain))
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
	sec   map[string]string // the namecheap connection's fields; nil binds no connection
	// noAddress names the nameservers the fake resolver finds no address for; every other name
	// resolves. The real resolver is never used.
	noAddress map[string]bool
	resolved  []string // each name the fake resolver was asked
}

func newHarness(t *testing.T) *harness {
	f := newFake(t)
	c := &fakeClock{t: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
	f.clock = c
	ns1, ns2 := newFakeDNS(t, "ns1.example.net", f), newFakeDNS(t, "ns2.example.net", f)
	oldServers := authServers
	authServers = func(string) ([]nameServer, error) { return []nameServer{ns1.server(), ns2.server()}, nil }
	t.Cleanup(func() { authServers = oldServers })
	h := &harness{
		t: t, fake: f, ns1: ns1, ns2: ns2, dir: t.TempDir(), clock: c,
		cfg:       map[string]any{"domains": []string{"example.com"}, "mode": "apply"},
		sec:       map[string]string{"apiUser": testUser, "apiKey": testKey},
		noAddress: map[string]bool{},
	}
	oldResolve := resolveHost
	resolveHost = func(name string) ([]string, error) {
		h.resolved = append(h.resolved, name)
		if h.noAddress[name] {
			return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
		}
		return []string{"192.0.2.53"}, nil
	}
	t.Cleanup(func() { resolveHost = oldResolve })
	return h
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
		"protocol": "harness.member/1", "work": work, "config": h.cfg, "connections": h.connections(), "workingDirectory": h.dir,
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

// connections is the request's connections: the namecheap slot bound to an API-key connection with
// the Namecheap preset holding h.sec, or none when h.sec is nil.
func (h *harness) connections() map[string]any {
	if h.sec == nil {
		return map[string]any{}
	}
	return map[string]any{slot: map[string]any{"kind": "apikey", "account": "Namecheap", "preset": "namecheap", "fields": h.sec}}
}
