package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// fakeDNS is one name server: it answers the records it holds, by "<type> <name>", every NS
// question with ns, and "no such host" for anything else. A server that is down answers nothing.
type fakeDNS struct {
	records map[string][]string
	ns      []string
	down    bool
}

func (d *fakeDNS) answer(typ, name string) ([]string, error) {
	name = strings.TrimSuffix(name, ".")
	if d.down {
		return nil, &net.DNSError{Err: "i/o timeout", Name: name, IsTimeout: true}
	}
	if typ == "NS" && len(d.ns) > 0 {
		return d.ns, nil
	}
	if v, ok := d.records[typ+" "+name]; ok {
		return v, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
}

func (d *fakeDNS) LookupTXT(_ context.Context, name string) ([]string, error) {
	return d.answer("TXT", name)
}

func (d *fakeDNS) LookupCNAME(_ context.Context, name string) (string, error) {
	v, err := d.answer("CNAME", name)
	if err != nil {
		return "", err
	}
	return v[0] + ".", nil
}

func (d *fakeDNS) LookupHost(_ context.Context, name string) ([]string, error) {
	return d.answer("A", name)
}

func (d *fakeDNS) LookupNS(_ context.Context, name string) ([]*net.NS, error) {
	hosts, err := d.answer("NS", name)
	var out []*net.NS
	for _, h := range hosts {
		out = append(out, &net.NS{Host: h + "."})
	}
	return out, err
}

// useDNS makes each server answer as its fake, by address ("" is the machine's own resolver, "*"
// every server not named); a server with no fake is down. It answers the servers asked, in order.
func useDNS(t *testing.T, servers map[string]*fakeDNS) *[]string {
	asked := &[]string{}
	var mu sync.Mutex
	old := resolverAt
	resolverAt = func(server string) resolver {
		mu.Lock()
		*asked = append(*asked, server)
		mu.Unlock()
		if d, ok := servers[server]; ok {
			return d
		}
		if d, ok := servers["*"]; ok {
			return d
		}
		return &fakeDNS{down: true}
	}
	t.Cleanup(func() { resolverAt = old })
	return asked
}

// refuseARM makes Azure answer status, code and message to the requests refuse picks; every other
// request is answered as usual.
func refuseARM(f *fake, refuse func(method, path string) bool, status int, code, message string) {
	f.hosts[armHost] = func(w http.ResponseWriter, r *http.Request) {
		_, path, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
		path = "/" + path
		if refuse(r.Method, path) {
			f.answer(w, status, armError(code, message))
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.arm(w, r.Method, path, f.calls[len(f.calls)-1].Body)
	}
}

func TestDomainBindReadsTheRecordsFromTheDomainsOwnNameServers(t *testing.T) {
	w := newWorld(t)
	stale := &fakeDNS{} // the machine's resolver holds "no such host" for everything
	asked := useDNS(t, map[string]*fakeDNS{
		"":        stale,
		"1.1.1.1": {ns: []string{"dns1.registrar.test", "dns2.registrar.test"}},
		"8.8.8.8": {ns: []string{"dns1.registrar.test", "dns2.registrar.test"}},
		"dns1.registrar.test": {records: map[string][]string{
			"TXT asuid.example.org": {verifyID},
			"A example.org":         {staticIP},
		}},
	})

	r := runPlugin(t, runOpts{config: apply(nil)}, "domain bind my-app example.org")
	if !r.ok() {
		t.Fatal(r.text())
	}
	mustContain(t, r.text(), "Bound example.org to container app my-app with a managed certificate")
	if !contains(*asked, "dns1.registrar.test") {
		t.Fatalf("the domain's name server was never asked; asked %v", *asked)
	}
	cert := w.f.callsTo(http.MethodPut, "/managedCertificates/mc-example-org")
	if len(cert) != 1 || !strings.Contains(string(cert[0].Body), `"domainControlValidation":"HTTP"`) {
		t.Fatalf("certificate = %v", cert)
	}
}

func TestDomainBindFallsBackToAPublicResolverWhenTheNameServersDoNotAnswer(t *testing.T) {
	w := newWorld(t)
	useDNS(t, map[string]*fakeDNS{
		"":        {},
		"1.1.1.1": {ns: []string{"dns1.registrar.test"}, records: bound("www.example.com")},
		// dns1.registrar.test is down
	})
	r := runPlugin(t, runOpts{config: apply(nil)}, "domain bind my-app www.example.com")
	if !r.ok() {
		t.Fatal(r.text())
	}
	mustContain(t, r.text(), "Bound www.example.com to container app my-app")
	if len(w.f.callsTo(http.MethodPut, "/managedCertificates/mc-www-example-com")) != 1 {
		t.Fatal("no certificate was asked for")
	}
}

func TestDomainBindWithTheNameServersSayingNoNameAsksForTheRecords(t *testing.T) {
	w := newWorld(t)
	// A public resolver would answer, but the domain's own server is the authority and has none.
	useDNS(t, map[string]*fakeDNS{
		"":                    {records: map[string][]string{"TXT asuid.example.org": {verifyID}, "A example.org": {staticIP}}},
		"1.1.1.1":             {ns: []string{"dns1.registrar.test"}},
		"dns1.registrar.test": {},
	})
	r := runPlugin(t, runOpts{config: apply(nil)}, "domain bind my-app example.org")
	mustContain(t, r.text(), "example.org does not point at my-app yet", "- TXT asuid.example.org "+verifyID)
	if len(w.f.writes()) != 0 {
		t.Fatal("domain bind wrote while the records are missing")
	}
}

func TestDomainBindWhenAzureHasNotSeenTheRecordsSaysRunAgain(t *testing.T) {
	w := newWorld(t)
	resolve(t, bound("www.example.com"))
	app := inRG(ownedRG, "Microsoft.App/containerApps/my-app")
	refuseARM(w.f, func(method, path string) bool { return method == http.MethodPatch && strings.EqualFold(path, app) },
		http.StatusBadRequest, "InvalidCustomHostNameValidation",
		"A TXT record pointing from asuid.www.example.com to "+verifyID+" was not found.")

	r := runPlugin(t, runOpts{config: apply(nil)}, "domain bind my-app www.example.com")
	if r.ok() {
		t.Fatalf("the bind reported success: %s", r.text())
	}
	mustContain(t, r.text(), "the records are in place; Azure has not seen them yet - run domain bind again in a few minutes")
	if strings.Contains(strings.ToLower(r.text()), "person") || strings.Contains(r.text(), "sets these records") {
		t.Fatalf("asked someone to set records that exist: %s", r.text())
	}
	if len(w.f.callsTo(http.MethodPut, "/managedCertificates/mc-www-example-com")) != 0 {
		t.Fatal("a certificate was asked for after Azure refused the domain")
	}
}

func TestDomainBindWhenTheCertificateWaitsOnAzuresCheckSaysRunAgain(t *testing.T) {
	w := newWorld(t)
	resolve(t, bound("www.example.com"))
	refuseARM(w.f, func(method, path string) bool {
		return method == http.MethodPut && strings.HasSuffix(path, "/managedCertificates/mc-www-example-com")
	}, http.StatusBadRequest, "RequireCustomHostnameInEnvironment",
		"Creating managed certificate requires hostname 'www.example.com' added as a custom hostname to a container app.")

	r := runPlugin(t, runOpts{config: apply(nil)}, "domain bind my-app www.example.com")
	mustContain(t, r.text(), "run domain bind again in a few minutes")
	if strings.Contains(strings.ToLower(r.text()), "person") {
		t.Fatalf("named a person: %s", r.text())
	}
}

func TestDomainBindKeepsTheAppsOtherDomainsInBothPatches(t *testing.T) {
	w := newWorld(t)
	app := inRG(ownedRG, "Microsoft.App/containerApps/my-app")
	firstCert := inRG(ownedRG, "Microsoft.App/managedEnvironments/my-app-env/managedCertificates/mc-example-com")
	ingress := w.f.resources[strings.ToLower(app)].Props["configuration"].(map[string]any)["ingress"].(map[string]any)
	ingress["customDomains"] = []any{map[string]any{"name": "example.com", "bindingType": "SniEnabled", "certificateId": firstCert}}
	resolve(t, bound("www.example.com"))

	r := runPlugin(t, runOpts{config: apply(nil)}, "domain bind my-app www.example.com")
	if !r.ok() {
		t.Fatal(r.text())
	}
	patches := w.f.callsTo(http.MethodPatch, app)
	if len(patches) != 2 {
		t.Fatalf("app patches = %v", patches)
	}
	first := map[string]any{"name": "example.com", "bindingType": "SniEnabled", "certificateId": firstCert}
	for i, binding := range []string{"Disabled", "SniEnabled"} {
		var body struct {
			Properties struct {
				Configuration struct {
					Ingress struct {
						CustomDomains []map[string]any `json:"customDomains"`
					} `json:"ingress"`
				} `json:"configuration"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(patches[i].Body, &body); err != nil {
			t.Fatal(err)
		}
		domains := body.Properties.Configuration.Ingress.CustomDomains
		if len(domains) != 2 || !reflect.DeepEqual(domains[0], first) {
			t.Fatalf("patch %d lost or changed example.com: %v", i+1, domains)
		}
		if domains[1]["name"] != "www.example.com" || domains[1]["bindingType"] != binding {
			t.Fatalf("patch %d binds %v, want www.example.com %s", i+1, domains[1], binding)
		}
	}
}

func TestAnyListReadsBothShapes(t *testing.T) {
	d := map[string]any{"name": "example.com"}
	for _, v := range []any{[]any{d}, []map[string]any{d}} {
		if got := anyList(v); len(got) != 1 || got[0]["name"] != "example.com" {
			t.Fatalf("anyList(%T) = %v", v, got)
		}
	}
	if got := anyList(nil); got != nil {
		t.Fatalf("anyList(nil) = %v", got)
	}
}

func TestNotSeenYetKeepsOtherErrors(t *testing.T) {
	denied := &apiError{api: apiARM, status: http.StatusForbidden, code: "AuthorizationFailed", message: "no access to the hostname"}
	if got := notSeenYet(denied, "my-app", "www.example.com"); got != error(denied) {
		t.Fatalf("a refused permission became %v", got)
	}
	other := errors.New("Azure answered 500: something broke")
	if got := notSeenYet(other, "my-app", "www.example.com"); got != other {
		t.Fatalf("an unrelated error became %v", got)
	}
}
