package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Nameserver changes: nameservers (domains.dns.getList), setcustom (domains.dns.setCustom),
// setdefault (domains.dns.setDefault), and restore of a nameserver backup, against the fake
// Namecheap API only.

const (
	nsGetList    = "namecheap.domains.dns.getList"
	nsSetCustom  = "namecheap.domains.dns.setCustom"
	nsSetDefault = "namecheap.domains.dns.setDefault"
)

var customNS = []string{"ns1.example.net", "ns2.example.net"}

// nsHarness is a harness with example.com in the account on BasicDNS and nameserver changes on.
func nsHarness(t *testing.T) *harness {
	n := newHarness(t)
	n.fake.add("example.com", true, hr("@", "A", "203.0.113.10", 1799), hr("www", "CNAME", "app.example.net.", 1799))
	n.cfg[allowSetting] = true
	return n
}

func (f *fakeNamecheap) sent() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.commands {
		if c == nsSetCustom || c == nsSetDefault || c == setHosts {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeNamecheap) paramsOf(command string) []map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []map[string]string
	for i, c := range f.commands {
		if c == command {
			out = append(out, f.params[i])
		}
	}
	return out
}

func (f *fakeNamecheap) delegation(domain string) (bool, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d := f.domains[domain]
	return d.ourDNS, append([]string{}, d.nameservers...)
}

func nsEvents(r runResult) []map[string]any {
	var out []map[string]any
	for _, e := range r.events {
		if e["type"] == "nameservers" {
			p, _ := e["payload"].(map[string]any)
			out = append(out, p)
		}
	}
	return out
}

// noEvents fails when the run published anything at all.
func noEvents(t *testing.T, r runResult) {
	t.Helper()
	if len(r.events) != 0 {
		t.Errorf("published %v; a read, a dry run, a refusal and a no-op publish nothing", r.events)
	}
}

// ---- reading, in each mode -------------------------------------------------------------------------

func TestNameserversReadsEachMode(t *testing.T) {
	cases := []struct {
		name   string
		ourDNS bool
		ns     []string
		want   []string
	}{
		{"BasicDNS", true, nil, []string{"example.com is on Namecheap BasicDNS: dns1.registrar-servers.com, dns2.registrar-servers.com.", "Namecheap serves its host records"}},
		{"other Namecheap DNS", true, []string{"pdns1.example.net", "pdns2.example.net"}, []string{"example.com is on other Namecheap DNS: pdns1.example.net, pdns2.example.net.", "flags it as neither BasicDNS, PremiumDNS nor FreeDNS"}},
		{"custom", false, []string{"NS1.Example.NET", "ns2.example.net"}, []string{"example.com is on custom nameservers: ns1.example.net, ns2.example.net.", "Namecheap host records are not served"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n := newHarness(t)
			n.fake.add("example.com", c.ourDNS)
			n.fake.domains["example.com"].nameservers = c.ns
			n.cfg["mode"] = "dryrun"
			r := n.run("nameservers example.com")
			if !r.ok {
				t.Fatalf("read failed: %s", r.output)
			}
			mustContain(t, r.output, c.want...)
			mustContain(t, r.output, "Nameserver changes are off")
			if got := n.fake.paramsOf(nsGetList); len(got) != 1 || got[0]["SLD"] != "example" || got[0]["TLD"] != "com" {
				t.Errorf("getList params %v", got)
			}
			if s := n.fake.sent(); len(s) != 0 {
				t.Errorf("a read sent %v", s)
			}
			noEvents(t, r)
		})
	}
}

// ---- dry run ---------------------------------------------------------------------------------------

func TestSetcustomInDryRunSendsNothingAndSaysBeforeAndAfter(t *testing.T) {
	n := nsHarness(t)
	n.cfg["mode"] = "dryrun"
	r := n.run("setcustom example.com ns1.example.net ns2.example.net")
	if !r.ok {
		t.Fatalf("dry run failed: %s", r.output)
	}
	mustContain(t, r.output,
		"Dry run for example.com: nothing was changed.",
		"from Namecheap BasicDNS: dns1.registrar-servers.com, dns2.registrar-servers.com",
		"to   custom nameservers: ns1.example.net, ns2.example.net",
		"its Namecheap host records are no longer served",
		"up to 48 hours",
		"sets this member's mode to apply")
	if s := n.fake.sent(); len(s) != 0 {
		t.Errorf("dry run sent %v", s)
	}
	if len(n.backups("example.com")) != 0 {
		t.Errorf("dry run saved a backup: %v", n.backups("example.com"))
	}
	noEvents(t, r)
}

func TestSetdefaultInDryRunSendsNothing(t *testing.T) {
	n := nsHarness(t)
	n.fake.domains["example.com"].ourDNS = false
	n.fake.domains["example.com"].nameservers = customNS
	n.cfg["mode"] = "dryrun"
	r := n.run("setdefault example.com")
	mustContain(t, r.output, "Dry run for example.com: nothing was changed.",
		"from custom nameservers: ns1.example.net, ns2.example.net",
		"to   Namecheap BasicDNS", "Namecheap serves example.com's host records again", "up to 48 hours")
	if s := n.fake.sent(); len(s) != 0 {
		t.Errorf("dry run sent %v", s)
	}
	noEvents(t, r)
}

// ---- apply -----------------------------------------------------------------------------------------

func TestSetcustomSendsTheNameserversBacksUpFirstAndPublishesOnce(t *testing.T) {
	n := nsHarness(t)
	r := n.run("setcustom example.com NS1.example.net. ns2.example.net,ns3.example.net")
	if !r.ok {
		t.Fatalf("setcustom failed: %s", r.output)
	}
	sets := n.fake.paramsOf(nsSetCustom)
	if len(sets) != 1 {
		t.Fatalf("setCustom sent %d times", len(sets))
	}
	want := map[string]string{"SLD": "example", "TLD": "com", "Nameservers": "ns1.example.net,ns2.example.net,ns3.example.net"}
	if !reflect.DeepEqual(sets[0], want) {
		t.Errorf("setCustom params %v, want %v", sets[0], want)
	}
	if !reflect.DeepEqual(n.resolved, []string{"ns1.example.net", "ns2.example.net", "ns3.example.net"}) {
		t.Errorf("resolved %v", n.resolved)
	}

	// The backups were written before the change was sent: the nameserver backup holds BasicDNS.
	ids := n.backups("example.com")
	if len(ids) != 2 {
		t.Fatalf("backups %v; want a record backup and a nameserver backup", ids)
	}
	var nsb backup
	var nsID string
	for _, id := range ids {
		raw, _ := os.ReadFile(filepath.Join(n.dir, backupFolder, "example.com", id+".json"))
		var b backup
		_ = json.Unmarshal(raw, &b)
		if b.Kind == kindNameservers {
			nsb, nsID = b, id
		} else if len(b.Records) != 2 {
			t.Errorf("record backup %s holds %d records", id, len(b.Records))
		}
	}
	if nsb.NSMode != nsBasic || !reflect.DeepEqual(nsb.Nameservers, basicDNS) || nsb.Command != "setcustom" {
		t.Errorf("nameserver backup %+v", nsb)
	}
	i := strings.Index(strings.Join(n.fake.commands, " "), nsSetCustom)
	if before := strings.Join(n.fake.commands, " ")[:i]; !strings.Contains(before, getHosts) || !strings.Contains(before, nsGetList) {
		t.Errorf("calls before setCustom: %s", before)
	}

	mustContain(t, r.output,
		"example.com: nameservers changed",
		"to   custom nameservers: ns1.example.net, ns2.example.net, ns3.example.net (read back from Namecheap)",
		"`restore example.com "+nsID+"` puts them back in one command",
		"its Namecheap host records are no longer served",
		"up to 48 hours")

	ev := nsEvents(r)
	if len(ev) != 1 || len(r.events) != 1 {
		t.Fatalf("events %v", r.events)
	}
	e := ev[0]
	if e["domain"] != "example.com" || e["command"] != "setcustom" || e["action"] != "changed" || e["modeBefore"] != "Namecheap BasicDNS" || e["modeAfter"] != "custom nameservers" || e["backupId"] != nsID {
		t.Errorf("event %v", e)
	}
	if after, _ := e["after"].([]any); len(after) != 3 {
		t.Errorf("event after %v", e["after"])
	}
}

func TestSetdefaultSendsSetDefault(t *testing.T) {
	n := nsHarness(t)
	n.fake.domains["example.com"].ourDNS = false
	n.fake.domains["example.com"].nameservers = customNS
	r := n.run("setdefault example.com")
	if !r.ok {
		t.Fatalf("setdefault failed: %s", r.output)
	}
	sets := n.fake.paramsOf(nsSetDefault)
	if len(sets) != 1 || !reflect.DeepEqual(sets[0], map[string]string{"SLD": "example", "TLD": "com"}) {
		t.Fatalf("setDefault params %v", sets)
	}
	if our, _ := n.fake.delegation("example.com"); !our {
		t.Errorf("not on BasicDNS after setdefault")
	}
	if n.fake.count(getHosts) != 0 {
		t.Errorf("read host records of a domain on custom nameservers")
	}
	mustContain(t, r.output, "to   Namecheap BasicDNS", "Namecheap serves example.com's host records again", "up to 48 hours")
	if ev := nsEvents(r); len(ev) != 1 || ev[0]["modeAfter"] != "Namecheap BasicDNS" || ev[0]["command"] != "setdefault" {
		t.Errorf("events %v", r.events)
	}
	if len(n.resolved) != 0 {
		t.Errorf("setdefault resolved %v", n.resolved)
	}
}

// ---- the rollback, in each direction ---------------------------------------------------------------

func backupID(t *testing.T, r runResult) string {
	t.Helper()
	ev := nsEvents(r)
	if len(ev) != 1 {
		t.Fatalf("no nameservers event: %s", r.output)
	}
	return ev[0]["backupId"].(string)
}

func TestRestoreUndoesEachDirection(t *testing.T) {
	cases := []struct {
		name         string
		startBasic   bool
		start        []string
		command      string
		restoreSends string
	}{
		{"BasicDNS to custom", true, nil, "setcustom example.com ns3.example.net ns4.example.net", nsSetDefault},
		{"custom to BasicDNS", false, customNS, "setdefault example.com", nsSetCustom},
		{"custom to custom", false, customNS, "setcustom example.com ns3.example.net ns4.example.net", nsSetCustom},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n := nsHarness(t)
			n.fake.domains["example.com"].ourDNS = c.startBasic
			n.fake.domains["example.com"].nameservers = c.start
			firstLine := func(s string) string { return strings.SplitN(s, "\n", 2)[0] }
			before := firstLine(n.run("nameservers example.com").output)

			r := n.run(c.command)
			if !r.ok {
				t.Fatalf("%s failed: %s", c.command, r.output)
			}
			id := backupID(t, r)
			mustContain(t, r.output, "`restore example.com "+id+"`")

			sentBefore := len(n.fake.paramsOf(c.restoreSends))
			back := n.run("restore example.com " + id)
			if !back.ok {
				t.Fatalf("restore failed: %s", back.output)
			}
			if len(n.fake.paramsOf(c.restoreSends)) != sentBefore+1 {
				t.Errorf("restore did not send %s: %v", c.restoreSends, n.fake.commands)
			}
			if c.restoreSends == nsSetCustom {
				p := n.fake.paramsOf(nsSetCustom)
				if got := p[len(p)-1]["Nameservers"]; got != "ns1.example.net,ns2.example.net" {
					t.Errorf("restore sent Nameservers %q", got)
				}
			}
			our, ns := n.fake.delegation("example.com")
			if our != c.startBasic || (!our && !reflect.DeepEqual(ns, c.start)) {
				t.Errorf("after restore: ourDNS %t nameservers %v", our, ns)
			}
			if after := firstLine(n.run("nameservers example.com").output); after != before {
				t.Errorf("read after restore:\n%s\nwant:\n%s", after, before)
			}
			if ev := nsEvents(back); len(ev) != 1 || ev[0]["command"] != "restore" {
				t.Errorf("restore events %v", back.events)
			}
			// The restore kept what it replaced, so it can be undone the same way.
			mustContain(t, back.output, "Backup ", "puts them back in one command")

			// Restoring again is a no-op that says so.
			again := n.run("restore example.com " + id)
			mustContain(t, again.output, "already holds backup "+id+"'s nameservers", "nothing was changed")
			noEvents(t, again)
		})
	}
}

func TestARecordRestoreDoesNotTakeANameserverBackupAndListShowsOnlyRecordBackups(t *testing.T) {
	n := nsHarness(t)
	r := n.run("setcustom example.com ns1.example.net ns2.example.net")
	id := backupID(t, r)
	n.run("setdefault example.com")
	l := n.run("list example.com")
	if strings.Contains(l.output, id) {
		t.Errorf("list names the nameserver backup %s:\n%s", id, l.output)
	}
	if hosts := n.fake.hosts("example.com"); len(hosts) != 2 {
		t.Errorf("records %v", hosts)
	}
}

// ---- refusals, by name, before anything is sent -----------------------------------------------------

func TestNameserverRefusals(t *testing.T) {
	cases := []struct {
		name, command string
		prepare       func(n *harness)
		want          string
	}{
		{"not in the account", "setcustom example.com ns1.example.net ns2.example.net",
			func(n *harness) { delete(n.fake.domains, "example.com") },
			"example.com is not in this Namecheap account (Namecheap answered error 2019166: Domain not found)"},
		{"another account's domain", "setdefault example.com",
			func(n *harness) { n.fake.notOwned = "example.com" },
			"example.com is not in this Namecheap account (Namecheap answered error 2016166: Domain is not associated with your account)"},
		{"not listed", "setcustom example.org ns1.example.net ns2.example.net", nil,
			"example.org is not in this member's domains setting"},
		{"one nameserver", "setcustom example.com ns1.example.net", nil,
			"1 nameserver(s) named; a domain takes 2 to 12 nameservers"},
		{"thirteen nameservers", "setcustom example.com ns1.example.net,ns2.example.net,ns3.example.net,ns4.example.net,ns5.example.net,ns6.example.net,ns7.example.net,ns8.example.net,ns9.example.net,ns10.example.net,ns11.example.net,ns12.example.net,ns13.example.net", nil,
			"13 nameserver(s) named; a domain takes 2 to 12 nameservers"},
		{"an address", "setcustom example.com ns1.example.net 192.0.2.1", nil,
			`"192.0.2.1" is not a hostname`},
		{"a URL", "setcustom example.com ns1.example.net https://ns2.example.net", nil,
			`"https://ns2.example.net" is not a hostname`},
		{"a single label", "setcustom example.com ns1.example.net localhost", nil,
			`"localhost" is not a hostname`},
		{"a trailing hyphen", "setcustom example.com ns1.example.net ns2.example-", nil,
			`"ns2.example-" is not a hostname`},
		{"a trailing hyphen in a middle label", "setcustom example.com ns1.example.net ns2-.example.net", nil,
			`"ns2-.example.net" is not a hostname`},
		{"a duplicate by case and trailing dot", "setcustom example.com ns1.example.net NS1.Example.Net.", nil,
			`ns1.example.net is named twice (as "ns1.example.net" and "NS1.Example.Net.")`},
		{"does not resolve", "setcustom example.com ns1.example.net ns9.example.net",
			func(n *harness) { n.noAddress["ns9.example.net"] = true },
			"ns9.example.net does not resolve to an address"},
		{"BasicDNS's own names", "setcustom example.com dns1.registrar-servers.com dns2.registrar-servers.com", nil,
			"`setdefault example.com` puts the domain back on BasicDNS"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n := nsHarness(t)
			if c.prepare != nil {
				c.prepare(n)
			}
			r := n.run(c.command)
			if r.ok {
				t.Fatalf("not refused: %s", r.output)
			}
			mustContain(t, r.output, "Refused: ", c.want, "Nothing was changed.")
			if s := n.fake.sent(); len(s) != 0 {
				t.Errorf("sent %v", s)
			}
			if len(n.backups("example.com")) != 0 {
				t.Errorf("a refusal saved a backup")
			}
			noEvents(t, r)
		})
	}
}

func TestTheMexicanRegistryCapIsUsed(t *testing.T) {
	n := nsHarness(t)
	n.cfg["domains"] = []string{"example.com.mx"}
	n.fake.add("example.com.mx", true)
	r := n.run("setcustom example.com.mx ns1.example.net ns2.example.net ns3.example.net ns4.example.net ns5.example.net ns6.example.net")
	mustContain(t, r.output, "6 nameserver(s) named; a domain takes 2 to 5 nameservers (the .MX registry's limit)")
}

// ---- the setting -----------------------------------------------------------------------------------

func TestWithTheSettingOffChangesAreRefusedInEveryModeAndReadsWork(t *testing.T) {
	for _, mode := range []string{"dryrun", "apply"} {
		t.Run(mode, func(t *testing.T) {
			n := nsHarness(t)
			n.fake.domains["example.com"].ourDNS = false
			n.fake.domains["example.com"].nameservers = customNS
			delete(n.cfg, allowSetting)
			n.cfg["mode"] = mode
			for _, cmd := range []string{"setcustom example.com ns3.example.net ns4.example.net", "setdefault example.com"} {
				r := n.run(cmd)
				if r.ok {
					t.Fatalf("%s not refused: %s", cmd, r.output)
				}
				mustContain(t, r.output, `"Allow nameserver changes" (allowNameserverChanges)`, "Nothing was changed.")
				noEvents(t, r)
			}
			if len(n.fake.commands) != 0 {
				t.Errorf("a refused change called Namecheap: %v", n.fake.commands)
			}
			r := n.run("nameservers example.com")
			if !r.ok {
				t.Fatalf("read refused with the setting off: %s", r.output)
			}
			mustContain(t, r.output, "custom nameservers: ns1.example.net, ns2.example.net")
		})
	}
}

func TestWithTheSettingOffANameserverRestoreIsRefused(t *testing.T) {
	n := nsHarness(t)
	id := backupID(t, n.run("setcustom example.com ns1.example.net ns2.example.net"))
	n.cfg[allowSetting] = false
	r := n.run("restore example.com " + id)
	if r.ok {
		t.Fatalf("restore not refused: %s", r.output)
	}
	mustContain(t, r.output, `"Allow nameserver changes"`)
	noEvents(t, r)
	if our, _ := n.fake.delegation("example.com"); our {
		t.Errorf("restored with the setting off")
	}
}

func TestTheManifestDeclaresTheSettingAndTheNameserversEvent(t *testing.T) {
	raw, err := os.ReadFile("plugin.json")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Config map[string]map[string]any `json:"config"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	s := m.Config[allowSetting]
	if s["type"] != "bool" || s["default"] != false || s["setBy"] != "person" {
		t.Errorf("setting %s = %v", allowSetting, s)
	}
	e, ok := manifestEvents(t)["nameservers"]
	if !ok {
		t.Fatal("no nameservers event")
	}
	if e.HighVolume || e.Monitor["action"] != "changed" || e.Monitor["icon"] != "dns" {
		t.Errorf("nameservers event %+v", e)
	}
	want := []string{"domain", "command", "action", "modeBefore", "modeAfter", "before", "after", "backupId"}
	if got := fieldNames(e); !reflect.DeepEqual(got, want) {
		t.Errorf("fields %v, want %v", got, want)
	}
}

// ---- no-ops ----------------------------------------------------------------------------------------

func TestTheSameNameserversAreANoOp(t *testing.T) {
	n := nsHarness(t)
	n.fake.domains["example.com"].ourDNS = false
	n.fake.domains["example.com"].nameservers = customNS
	r := n.run("setcustom example.com NS2.example.net. ns1.example.net")
	if !r.ok {
		t.Fatalf("no-op failed: %s", r.output)
	}
	mustContain(t, r.output, "example.com is already on custom nameservers: ns1.example.net, ns2.example.net; nothing was changed.")
	if s := n.fake.sent(); len(s) != 0 {
		t.Errorf("a no-op sent %v", s)
	}
	if len(n.backups("example.com")) != 0 {
		t.Errorf("a no-op saved a backup")
	}
	noEvents(t, r)
}

func TestSetdefaultOnBasicDNSIsANoOp(t *testing.T) {
	n := nsHarness(t)
	r := n.run("setdefault example.com")
	mustContain(t, r.output, "example.com is already on Namecheap BasicDNS", "nothing was changed.")
	if s := n.fake.sent(); len(s) != 0 {
		t.Errorf("a no-op sent %v", s)
	}
	noEvents(t, r)
}

// ---- a change Namecheap does not make ---------------------------------------------------------------

func TestAChangeNotReadBackFailsNamingTheBackup(t *testing.T) {
	n := nsHarness(t)
	n.fake.ignoreSetNS = true
	r := n.run("setcustom example.com ns1.example.net ns2.example.net")
	if r.ok {
		t.Fatalf("succeeded: %s", r.output)
	}
	ids := n.backups("example.com")
	mustContain(t, r.output, "Namecheap now answers Namecheap BasicDNS", "`restore example.com ")
	if len(ids) != 2 {
		t.Errorf("backups %v", ids)
	}
	noEvents(t, r)
}

func TestAFailedSendFailsNamingTheBackup(t *testing.T) {
	n := nsHarness(t)
	n.fake.failSetNS = true
	r := n.run("setcustom example.com ns1.example.net ns2.example.net")
	mustContain(t, r.output, "The nameserver change for example.com failed (Namecheap answered error 3050900", "`restore example.com ")
	noEvents(t, r)
}

// ---- PremiumDNS and FreeDNS ------------------------------------------------------------------------

// premiumHarness is nsHarness with example.com on Namecheap PremiumDNS: getList answers
// IsUsingOurDNS="true" IsPremiumDNS="true" with PremiumDNS's nameservers.
func premiumHarness(t *testing.T) *harness {
	n := nsHarness(t)
	n.fake.domains["example.com"].premiumDNS = true
	n.fake.domains["example.com"].nameservers = []string{"pdns1.example.net", "pdns2.example.net"}
	return n
}

// freeHarness is nsHarness with example.com on Namecheap FreeDNS: getList answers 2019166 and
// getInfo answers ProviderType="FreeDNS" with its nameservers, as the real API does.
func freeHarness(t *testing.T) *harness {
	n := nsHarness(t)
	n.fake.domains["example.com"].freeDNS = true
	n.fake.domains["example.com"].nameservers = []string{"freedns1.example.net", "freedns2.example.net", "freedns3.example.net", "freedns4.example.net"}
	return n
}

func TestNameserversReadsPremiumDNS(t *testing.T) {
	n := premiumHarness(t)
	r := n.run("nameservers example.com")
	if !r.ok {
		t.Fatalf("read failed: %s", r.output)
	}
	mustContain(t, r.output, "example.com is on Namecheap PremiumDNS: pdns1.example.net, pdns2.example.net.", "cannot put it back on PremiumDNS")
	if strings.Contains(r.output, "does not say which") {
		t.Errorf("the read still says getList cannot tell them apart:\n%s", r.output)
	}
	if n.fake.count("namecheap.domains.getInfo") != 0 {
		t.Errorf("a PremiumDNS read called getInfo")
	}
	noEvents(t, r)
}

func TestNameserversReadsFreeDNSThroughGetInfo(t *testing.T) {
	n := freeHarness(t)
	n.cfg[allowSetting] = false
	r := n.run("nameservers example.com")
	if !r.ok {
		t.Fatalf("read failed: %s", r.output)
	}
	mustContain(t, r.output,
		"example.com is on Namecheap FreeDNS: freedns1.example.net, freedns2.example.net, freedns3.example.net, freedns4.example.net.",
		"read with domains.getInfo")
	if strings.Contains(r.output, "not in this Namecheap account") {
		t.Errorf("a FreeDNS domain read as absent:\n%s", r.output)
	}
	info := n.fake.paramsOf("namecheap.domains.getInfo")
	if len(info) != 1 || info[0]["DomainName"] != "example.com" {
		t.Errorf("getInfo params %v", info)
	}
	if s := n.fake.sent(); len(s) != 0 {
		t.Errorf("a read sent %v", s)
	}
	noEvents(t, r)
}

func TestAnAbsentDomainIsStillRefusedAfterGetInfo(t *testing.T) {
	n := nsHarness(t)
	delete(n.fake.domains, "example.com")
	r := n.run("nameservers example.com")
	if r.ok {
		t.Fatalf("not refused: %s", r.output)
	}
	mustContain(t, r.output, "Refused: example.com is not in this Namecheap account", "Nothing was changed.")
	if n.fake.count("namecheap.domains.getInfo") != 1 {
		t.Errorf("getInfo was not asked before refusing: %v", n.fake.commands)
	}
}

func TestAChangeOffFreeDNSSaysWhatItDoes(t *testing.T) {
	n := freeHarness(t)
	n.cfg["mode"] = "dryrun"
	r := n.run("setdefault example.com")
	if !r.ok {
		t.Fatalf("dry run failed: %s", r.output)
	}
	mustContain(t, r.output,
		"from Namecheap FreeDNS: freedns1.example.net",
		"to   Namecheap BasicDNS",
		"`restore` cannot put it back there afterwards",
		"re-selects it in the Namecheap portal")
	if s := n.fake.sent(); len(s) != 0 {
		t.Errorf("dry run sent %v", s)
	}
	noEvents(t, r)
}

func TestAChangeOffPremiumDNSWarnsAndItsRestoreIsRefused(t *testing.T) {
	for _, c := range []struct {
		name    string
		harness func(*testing.T) *harness
		mode    string
	}{
		{"PremiumDNS", premiumHarness, "Namecheap PremiumDNS"},
		{"FreeDNS", freeHarness, "Namecheap FreeDNS"},
	} {
		t.Run(c.name, func(t *testing.T) {
			n := c.harness(t)
			r := n.run("setcustom example.com ns1.example.net ns2.example.net")
			if !r.ok {
				t.Fatalf("setcustom failed: %s", r.output)
			}
			id := backupID(t, r)
			mustContain(t, r.output,
				"from "+c.mode,
				"`restore` cannot put the domain back on "+c.mode,
				"re-selects it in the Namecheap portal")
			var rid string
			for _, b := range n.backups("example.com") {
				if b != id {
					rid = b
				}
			}
			if rid == "" {
				t.Fatalf("no record backup beside %s: %v", id, n.backups("example.com"))
			}

			before := len(n.fake.sent())
			back := n.run("restore example.com " + id)
			if back.ok {
				t.Fatalf("restore not refused: %s", back.output)
			}
			mustContain(t, back.output,
				"Refused: backup "+id+" was taken while example.com was on "+c.mode,
				"re-selects "+c.mode+" for example.com in the Namecheap portal",
				"record backup "+rid+": `restore example.com "+rid+"`",
				"Nothing was changed.")
			if s := n.fake.sent(); len(s) != before {
				t.Errorf("a refused restore sent %v", s[before:])
			}
			if our, ns := n.fake.delegation("example.com"); our || !reflect.DeepEqual(ns, customNS) {
				t.Errorf("delegation changed: %t %v", our, ns)
			}
			noEvents(t, back)
		})
	}
}

func TestAFreeDNSChangeGoesAheadWhenItsHostRecordsCannotBeRead(t *testing.T) {
	n := freeHarness(t)
	n.fake.failGetHosts = true
	r := n.run("setcustom example.com ns1.example.net ns2.example.net")
	if !r.ok {
		t.Fatalf("setcustom failed: %s", r.output)
	}
	mustContain(t, r.output, "so no record backup was saved")
	if len(n.backups("example.com")) != 1 {
		t.Errorf("backups %v; want only the nameserver backup", n.backups("example.com"))
	}
	back := n.run("restore example.com " + backupID(t, r))
	mustContain(t, back.output, "No record backup was saved with it.")
}
