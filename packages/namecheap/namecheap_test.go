package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	setHosts = "namecheap.domains.dns.setHosts"
	getHosts = "namecheap.domains.dns.getHosts"
)

func tenRecords() []xmlHost {
	return []xmlHost{
		hr("@", "A", "203.0.113.10", 1799),
		hr("www", "CNAME", "app.example.net.", 1799),
		hr("@", "AAAA", "2001:db8::10", 1799),
		hr("@", "MX", "mx1.example.net.", 3600),
		hr("@", "TXT", "v=spf1 include:_spf.example.net ~all", 1799),
		hr("_dmarc", "TXT", "v=DMARC1; p=none; rua=mailto:dmarc@example.com", 1799),
		hr("@", "CAA", `0 issue "ca.example.net"`, 1799),
		hr("blog", "A", "203.0.113.11", 600),
		hr("old", "URL301", "http://www.example.com/", 1799),
		hr("mail", "CNAME", "mail.example.net.", 1799),
	}
}

func sameHosts(t *testing.T, want, got []xmlHost) {
	t.Helper()
	if len(want) != len(got) {
		t.Fatalf("%d records, want %d:\n%v", len(got), len(want), got)
	}
	for i := range want {
		if want[i] != got[i] {
			t.Errorf("record %d is %+v, want %+v", i+1, got[i], want[i])
		}
	}
}

// ---- setup ---------------------------------------------------------------------------------------

func TestSetupNamesTheKeysTheAddressAndTheDomainsWhenNothingIsSet(t *testing.T) {
	n := newHarness(t)
	n.sec = map[string]string{}
	n.cfg = map[string]any{}
	r := n.run("setup")
	if !r.ok {
		t.Fatalf("setup failed: %s", r.output)
	}
	mustContain(t, r.output,
		"MISSING  the API user or API key is not set",
		"this computer's internet address is "+testIP,
		testIP+" is on Namecheap's API allow-list (checked once the key is set)",
		"MISSING  no domains are listed",
		"Next step: Paste the Namecheap API user",
		"Then: List the domains",
		"mode     dryrun")
	if len(n.fake.commands) != 0 {
		t.Errorf("setup called Namecheap without a key: %v", n.fake.commands)
	}
}

func TestSetupSaysAPIAccessIsOffAndStillNamesTheAddress(t *testing.T) {
	n := newHarness(t)
	n.fake.accessOff = true
	r := n.run("setup")
	mustContain(t, r.output,
		"MISSING  API access is off for the account, or the API user or key is wrong",
		"Next step: Turn API access on in Namecheap",
		"Then: Add "+testIP+" to Namecheap's API allow-list")
}

func TestSetupNamesTheAddressToAllowList(t *testing.T) {
	n := newHarness(t)
	n.fake.add("example.com", true)
	n.fake.allowed = map[string]bool{}
	r := n.run("setup")
	mustContain(t, r.output,
		"ok       API access is on for the account",
		"MISSING  "+testIP+" is not on Namecheap's API allow-list",
		"Next step: Add "+testIP+" to Namecheap's API allow-list: Profile, Tools, Namecheap API Access, Whitelisted IPs")
}

func TestSetupChecksEachDomainIsInTheAccountAndOnNamecheapDNS(t *testing.T) {
	n := newHarness(t)
	n.fake.add("example.com", true)
	n.fake.add("example.net", false)
	n.cfg["domains"] = []string{"example.com", "example.net", "Example.org."}
	r := n.run("setup")
	mustContain(t, r.output,
		"ok       example.com is in the account and uses Namecheap DNS",
		"MISSING  example.net is in the account but does not use Namecheap DNS",
		"MISSING  example.org is not in this Namecheap account",
		"Nameservers, choose Namecheap BasicDNS")
	if strings.Contains(r.output, "Ready") {
		t.Errorf("setup says ready with domains missing:\n%s", r.output)
	}
}

func TestSetupSaysReadyWhenEveryCheckPasses(t *testing.T) {
	n := newHarness(t)
	n.fake.add("example.com", true)
	n.cfg["clientIp"] = "203.0.113.7"
	old := addressURL
	addressURL = "http://address.invalid/"
	defer func() { addressURL = old }()
	r := n.run("setup")
	mustContain(t, r.output, "(from the clientIp setting)", "Ready: every check passed", "mode     apply")
	if strings.Contains(r.output, "MISSING") {
		t.Errorf("setup:\n%s", r.output)
	}
}

// ---- the hard rule ---------------------------------------------------------------------------------

func TestEnsureOnTenRecordsKeepsAllTenAndAddsOne(t *testing.T) {
	n := newHarness(t)
	n.fake.add("example.com", true, tenRecords()...)
	r := n.run(`ensure example.com TXT _acme-challenge "token-placeholder-123"`)
	if !r.ok {
		t.Fatalf("ensure failed: %s", r.output)
	}
	got := n.fake.hosts("example.com")
	sameHosts(t, tenRecords(), got[:10])
	if len(got) != 11 || got[10].Name != "_acme-challenge" || got[10].Address != "token-placeholder-123" || got[10].Type != "TXT" {
		t.Fatalf("the eleventh record is not the new one: %+v", got)
	}
	ids := n.backups("example.com")
	if len(ids) != 1 {
		t.Fatalf("backups: %v", ids)
	}
	mustContain(t, r.output, "Backup "+ids[0], "every earlier record checked present", "(11 records")
	ev := r.event("changed")
	if ev == nil || ev["backupId"] != ids[0] || ev["domain"] != "example.com" {
		t.Fatalf("changed event: %v", r.events)
	}
	if added, _ := ev["added"].([]any); len(added) != 1 || !strings.Contains(added[0].(string), "_acme-challenge") {
		t.Errorf("added: %v", ev["added"])
	}
	if n.fake.count(getHosts) != 2 || n.fake.count(setHosts) != 1 {
		t.Errorf("calls: %v", n.fake.commands)
	}
}

func TestAVerificationTXTSitsBesideTheSPF(t *testing.T) {
	n := newHarness(t)
	spf := hr("@", "TXT", "v=spf1 include:_spf.example.net ~all", 1799)
	n.fake.add("example.com", true, spf)
	r := n.run(`ensure example.com TXT @ 'site-verification=placeholder-abc'`)
	if !r.ok {
		t.Fatalf("ensure failed: %s", r.output)
	}
	got := n.fake.hosts("example.com")
	if len(got) != 2 || got[0] != spf || got[1].Address != "site-verification=placeholder-abc" || got[1].Name != "@" {
		t.Fatalf("records: %+v", got)
	}
	// The same SPF again is already there: nothing is written.
	r = n.run(`ensure example.com TXT example.com "v=spf1 include:_spf.example.net ~all"`)
	mustContain(t, r.output, "already as asked; nothing was changed")
	if n.fake.count(setHosts) != 1 {
		t.Errorf("a second write: %v", n.fake.commands)
	}
}

func TestAFailedReadChangesNothing(t *testing.T) {
	n := newHarness(t)
	n.fake.add("example.com", true, tenRecords()...)
	n.fake.failGetHosts = true
	for _, cmd := range []string{"ensure example.com A www2 203.0.113.12", "remove example.com A blog", "list example.com"} {
		r := n.run(cmd)
		if r.ok || r.code != 1 {
			t.Fatalf("%s did not fail: %s", cmd, r.output)
		}
		mustContain(t, r.output, "Refused: example.com's records could not be read", "nothing was changed")
		if ev := r.event("refused"); ev == nil || ev["reason"] != r.output {
			t.Errorf("refused event: %v", r.events)
		}
	}
	if n.fake.count(setHosts) != 0 || len(n.backups("example.com")) != 0 {
		t.Errorf("a failed read wrote: %v, backups %v", n.fake.commands, n.backups("example.com"))
	}
	sameHosts(t, tenRecords(), n.fake.hosts("example.com"))
}

func TestADomainNotListedIsRefused(t *testing.T) {
	n := newHarness(t)
	n.fake.add("example.com", true)
	n.fake.add("example.org", true, tenRecords()...)
	for _, cmd := range []string{"ensure example.org A @ 203.0.113.12", "list example.org", "remove example.org A blog", "restore example.org 20260101T000000Z"} {
		r := n.run(cmd)
		if r.ok {
			t.Fatalf("%s was not refused", cmd)
		}
		mustContain(t, r.output, "Refused: example.org is not in this member's domains setting")
		ev := r.event("refused")
		if ev == nil || ev["command"] != strings.Fields(cmd)[0] || ev["domain"] != "example.org" {
			t.Errorf("refused event: %v", r.events)
		}
	}
	if len(n.fake.commands) != 0 {
		t.Errorf("an unlisted domain reached Namecheap: %v", n.fake.commands)
	}
}

func TestADomainNotOnNamecheapDNSIsRefused(t *testing.T) {
	n := newHarness(t)
	n.fake.add("example.com", false, tenRecords()...)
	r := n.run("ensure example.com A @ 203.0.113.12")
	if r.ok {
		t.Fatal("not refused")
	}
	mustContain(t, r.output, "Refused: example.com does not use Namecheap DNS")
	if n.fake.count(setHosts) != 0 || len(n.backups("example.com")) != 0 {
		t.Errorf("wrote: %v", n.fake.commands)
	}
}

func TestDryRunChangesNothing(t *testing.T) {
	n := newHarness(t)
	n.fake.add("example.com", true, tenRecords()...)
	delete(n.cfg, "mode") // the default
	for _, cmd := range []string{
		"ensure example.com A @ 203.0.113.99",
		"ensure example.com MX @ mx2.example.net 3600 20",
		"remove example.com TXT _dmarc",
		"remove example.com URL301 old",
	} {
		r := n.run(cmd)
		if !r.ok {
			t.Fatalf("%s: %s", cmd, r.output)
		}
		mustContain(t, r.output, "Dry run for example.com: nothing was changed.", "Before (10 records):", "After (", "sets this member's mode to apply")
		if r.event("changed") != nil {
			t.Errorf("dry run published changed")
		}
	}
	if n.fake.count(setHosts) != 0 || len(n.backups("example.com")) != 0 {
		t.Errorf("dry run wrote: %v", n.fake.commands)
	}
	sameHosts(t, tenRecords(), n.fake.hosts("example.com"))
}

func TestRestorePutsBackTheSavedSet(t *testing.T) {
	n := newHarness(t)
	n.fake.add("example.com", true, tenRecords()...)
	r := n.run("ensure example.com A @ 203.0.113.99 300")
	if !r.ok {
		t.Fatal(r.output)
	}
	first := n.backups("example.com")[0]
	for _, cmd := range []string{"remove example.com TXT _dmarc", "ensure example.com MX @ mx2.example.net 3600 20"} {
		if r := n.run(cmd); !r.ok {
			t.Fatalf("%s: %s", cmd, r.output)
		}
	}
	if n.fake.domains["example.com"].emailType != "MX" {
		t.Errorf("an MX write did not set EmailType MX")
	}

	r = n.run("restore example.com " + first)
	if !r.ok {
		t.Fatalf("restore: %s", r.output)
	}
	got := n.fake.hosts("example.com")
	want := tenRecords()
	if len(got) != len(want) {
		t.Fatalf("restored %d records: %+v", len(got), got)
	}
	for i := range want {
		// Namecheap answers TTL and MXPref as numbers; the restore writes what was read.
		if got[i].Name != want[i].Name || got[i].Type != want[i].Type || got[i].Address != want[i].Address || got[i].TTL != want[i].TTL {
			t.Errorf("record %d: %+v, want %+v", i+1, got[i], want[i])
		}
	}
	ev := r.event("changed")
	if ev == nil || ev["command"] != "restore" {
		t.Fatalf("events: %v", r.events)
	}
	// The restore saved what it replaced, so it can be undone too.
	if len(n.backups("example.com")) != 4 {
		t.Errorf("backups: %v", n.backups("example.com"))
	}
	if ev["backupId"] == first {
		t.Errorf("the restore's backup is the one it restored")
	}
}

func TestRestoreRefusesAnIDThatIsNotABackup(t *testing.T) {
	n := newHarness(t)
	n.fake.add("example.com", true)
	for _, id := range []string{"../../secrets", "20260101T000000Z"} {
		r := n.run("restore example.com " + id)
		if r.ok {
			t.Fatalf("restore %s was not refused", id)
		}
	}
	if len(n.fake.commands) != 0 {
		t.Errorf("calls: %v", n.fake.commands)
	}
}

func TestTheAPIKeyIsInNoOutputEventOrBackupFile(t *testing.T) {
	n := newHarness(t)
	n.fake.add("example.com", true, tenRecords()...)
	n.fake.echoKey = true
	var all strings.Builder
	for _, cmd := range []string{"setup", "list example.com", "ensure example.com A @ 203.0.113.99", "remove example.com TXT _dmarc"} {
		all.WriteString(n.run(cmd).raw)
	}
	n.fake.failSetHosts = true
	all.WriteString(n.run("ensure example.com A www2 203.0.113.12").raw)
	n.fake.failSetHosts = false
	n.fake.allowed = map[string]bool{}
	all.WriteString(n.run("setup").raw)
	all.WriteString(n.run("list example.com").raw)

	if strings.Contains(all.String(), testKey) {
		t.Errorf("the API key is in the output:\n%s", all.String())
	}
	if !strings.Contains(all.String(), `"type":"changed"`) || !strings.Contains(all.String(), `"type":"refused"`) {
		t.Errorf("the runs did not publish both events")
	}
	files := 0
	_ = filepath.WalkDir(n.dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		files++
		raw, _ := os.ReadFile(path)
		if strings.Contains(string(raw), testKey) || strings.Contains(string(raw), testUser) {
			t.Errorf("%s holds a credential", path)
		}
		return nil
	})
	if len(n.backups("example.com")) != 3 || files < 4 {
		t.Errorf("backups %v, files %d", n.backups("example.com"), files)
	}
}

// ---- what each change does ------------------------------------------------------------------------

func TestAReadBackMissingARecordFailsNamingTheBackup(t *testing.T) {
	n := newHarness(t)
	n.fake.add("example.com", true, tenRecords()...)
	n.fake.dropOnSet = "blog"
	r := n.run("ensure example.com A www2 203.0.113.12")
	if r.ok {
		t.Fatal("a lost record went unnoticed")
	}
	id := n.backups("example.com")[0]
	mustContain(t, r.output, "1 record(s) missing", "A blog 203.0.113.11", "restore example.com "+id)
	n.fake.dropOnSet = ""
	if r := n.run("restore example.com " + id); !r.ok {
		t.Fatal(r.output)
	}
	sameHosts(t, tenRecords(), n.fake.hosts("example.com"))
}

func TestEnsureChangesTheOneAOnAHostAndRefusesToGuessAmongSeveral(t *testing.T) {
	n := newHarness(t)
	n.fake.add("example.com", true, hr("@", "A", "203.0.113.10", 1799), hr("lb", "A", "203.0.113.20", 1799), hr("lb", "A", "203.0.113.21", 1799))
	r := n.run("ensure example.com A @ 203.0.113.99")
	mustContain(t, r.output, "changes A @ 203.0.113.10 ttl 1799 to A @ 203.0.113.99 ttl 1799")
	if got := n.fake.hosts("example.com"); len(got) != 3 || got[0].Address != "203.0.113.99" {
		t.Fatalf("%+v", got)
	}
	r = n.run("ensure example.com A lb 203.0.113.22")
	if r.ok {
		t.Fatal("guessed among two A records")
	}
	mustContain(t, r.output, "lb has 2 A records")
}

func TestCNAMEDoesNotSitBesideOtherRecords(t *testing.T) {
	n := newHarness(t)
	n.fake.add("example.com", true, hr("www", "CNAME", "app.example.net.", 1799), hr("@", "TXT", "v=spf1 -all", 1799))
	if r := n.run("ensure example.com TXT www verify-placeholder"); r.ok {
		t.Error("a TXT beside a CNAME")
	}
	if r := n.run("ensure example.com CNAME @ app.example.net"); r.ok {
		t.Error("a CNAME beside a TXT")
	}
	r := n.run("ensure example.com CNAME www other.example.net")
	mustContain(t, r.output, "changes CNAME www app.example.net. ttl 1799 to CNAME www other.example.net ttl 1799")
}

func TestRemoveTakesOnlyTheNamedRecord(t *testing.T) {
	n := newHarness(t)
	spf, ver := hr("@", "TXT", "v=spf1 -all", 1799), hr("@", "TXT", "verify-placeholder", 1799)
	n.fake.add("example.com", true, spf, ver, hr("old", "URL301", "http://www.example.com/", 1799))
	if r := n.run("remove example.com TXT @"); r.ok {
		t.Error("removed one of two TXT records without a value")
	}
	r := n.run("remove example.com TXT @ verify-placeholder")
	if !r.ok {
		t.Fatal(r.output)
	}
	got := n.fake.hosts("example.com")
	if len(got) != 2 || got[0] != spf || got[1].Type != "URL301" {
		t.Fatalf("%+v", got)
	}
	r = n.run("remove example.com TXT @ not-there")
	mustContain(t, r.output, "there is no such TXT record on @; nothing was changed")
	for _, cmd := range []string{"ensure example.com URL301 old http://www.example.net/", "ensure example.com FRAME @ http://www.example.net/"} {
		if r := n.run(cmd); r.ok || !strings.Contains(r.output, "never adds or changes") {
			t.Errorf("%s: %s", cmd, r.output)
		}
	}
	if len(n.fake.hosts("example.com")) != 2 {
		t.Errorf("a refused redirect changed the set: %+v", n.fake.hosts("example.com"))
	}
}

func TestBadArgumentsAreRefusedBeforeAnyCall(t *testing.T) {
	n := newHarness(t)
	n.fake.add("example.com", true)
	for _, cmd := range []string{
		"ensure example.com A @ not-an-address",
		"ensure example.com AAAA @ 203.0.113.1",
		"ensure example.com NS @ ns1.example.net",
		"ensure example.com TXT @ \"unclosed",
		"ensure example.com A @ 203.0.113.1 5",
		"ensure example.com A @ 203.0.113.1 300 10",
		"ensure example.com CAA @ issue-only",
		"ensure example.com A",
		"frobnicate",
	} {
		if r := n.run(cmd); r.ok {
			t.Errorf("%s was accepted: %s", cmd, r.output)
		}
	}
	if len(n.fake.commands) != 0 {
		t.Errorf("calls: %v", n.fake.commands)
	}
}

func TestCallsStayUnderTwentyAMinute(t *testing.T) {
	n := newHarness(t)
	n.fake.add("example.com", true)
	var cmds []string
	for i := 0; i < 10; i++ {
		cmds = append(cmds, "ensure example.com TXT @ verify-"+string(rune('a'+i)))
	}
	r := n.run(cmds...) // 30 calls in one run
	if !r.ok {
		t.Fatal(r.output)
	}
	if len(n.fake.at) != 30 || len(n.clock.slept) == 0 {
		t.Fatalf("%d calls, slept %v", len(n.fake.at), n.clock.slept)
	}
	for i := range n.fake.at {
		inMinute := 0
		for j := i; j < len(n.fake.at) && n.fake.at[j].Sub(n.fake.at[i]) < time.Minute; j++ {
			inMinute++
		}
		if inMinute >= 20 {
			t.Fatalf("%d calls in a minute from call %d", inMinute, i+1)
		}
	}
	if len(n.fake.hosts("example.com")) != 10 {
		t.Errorf("records: %d", len(n.fake.hosts("example.com")))
	}
}

func TestSandboxUsesTheSandboxAPI(t *testing.T) {
	n := newHarness(t)
	n.fake.add("example.com", true)
	n.cfg["sandbox"] = true
	old := productionURL
	productionURL = "http://production.invalid/"
	defer func() { productionURL = old }()
	if r := n.run("list example.com"); !r.ok {
		t.Fatal(r.output)
	}
}

func TestSettingsThatCannotBeUsedAreNamed(t *testing.T) {
	n := newHarness(t)
	n.cfg["mode"] = "yes"
	n.cfg["clientIp"] = "2001:db8::1"
	n.cfg["domains"] = []string{"not a domain"}
	r := n.run("setup")
	mustContain(t, r.output, "`mode` must be dryrun or apply", "`clientIp` must be an IPv4 address", "`domains` holds \"not a domain\"")
}

func TestASecondSPFOnAHostIsRefused(t *testing.T) {
	n := newHarness(t)
	n.fake.add("example.com", true, hr("@", "TXT", "v=spf1 include:_spf.example.net ~all", 1799))
	r := n.run(`ensure example.com TXT @ "v=spf1 include:mail.example.org -all"`)
	if r.ok {
		t.Fatal("a second SPF record was accepted")
	}
	mustContain(t, r.output, "already has an SPF record", `remove example.com TXT @ "v=spf1 include:_spf.example.net ~all"`)
	if n.fake.count(setHosts) != 0 {
		t.Errorf("wrote: %v", n.fake.commands)
	}
}

// Namecheap takes a CAA value unquoted and answers it quoted: a CAA record typed either way is
// written unquoted, checked present after the write, found again by a second ensure, and removed.
func TestACAARecordIsWrittenUnquotedReadBackQuotedAndFoundAgain(t *testing.T) {
	for _, typed := range []string{`'0 issue "ca.example.net"'`, `"0 issue ca.example.net"`} {
		n := newHarness(t)
		n.fake.add("example.com", true, hr("@", "A", "203.0.113.10", 1799))
		r := n.run(`ensure example.com CAA @ ` + typed)
		if !r.ok {
			t.Fatalf("ensure %s failed: %s", typed, r.output)
		}
		mustContain(t, r.output, "every earlier record checked present")
		got := n.fake.hosts("example.com")
		if len(got) != 2 || got[1].Type != "CAA" || got[1].Address != `0 issue "ca.example.net"` {
			t.Fatalf("records after %s: %+v", typed, got)
		}
		if again := n.run(`ensure example.com CAA @ ` + typed); !again.ok || n.fake.count(setHosts) != 1 {
			t.Fatalf("a second ensure of %s wrote again: %s", typed, again.output)
		}
		if gone := n.run(`remove example.com CAA @ ` + typed); !gone.ok || len(n.fake.hosts("example.com")) != 1 {
			t.Fatalf("remove %s: %s %+v", typed, gone.output, n.fake.hosts("example.com"))
		}
	}
}

// A Private Email domain's mail records are Namecheap's own: an MX record would switch its mail
// setting to custom MX, so it is refused in dry run and apply alike and nothing is written; any
// other record is written with the mail setting kept; a restore may still put back a backup's.
func TestAChangeThatWouldSwitchAPrivateEmailDomainsMailSettingIsRefused(t *testing.T) {
	for _, mode := range []string{"dryrun", "apply"} {
		n := newHarness(t)
		n.fake.add("example.com", true, hr("privateemail._domainkey", "TXT", "v=DKIM1; k=rsa; p=placeholder", 1800))
		n.fake.domains["example.com"].emailType = "OX"
		n.cfg["mode"] = mode
		r := n.run(`ensure example.com MX @ mx.example.net 1799 10`)
		if r.ok || !strings.Contains(r.output, "Namecheap Private Email (OX)") || !strings.Contains(r.output, "Nothing was changed") {
			t.Fatalf("%s: an MX on a Private Email domain was not refused: %s", mode, r.output)
		}
		if n.fake.count(setHosts) != 0 || n.fake.domains["example.com"].emailType != "OX" {
			t.Fatalf("%s: the refused change wrote: %v, mail setting %s", mode, n.fake.commands, n.fake.domains["example.com"].emailType)
		}
	}

	n := newHarness(t)
	n.fake.add("example.com", true, hr("privateemail._domainkey", "TXT", "v=DKIM1; k=rsa; p=placeholder", 1800))
	n.fake.domains["example.com"].emailType = "OX"
	n.cfg["mode"] = "apply"
	if r := n.run(`ensure example.com TXT _dmarc "v=DMARC1; p=none"`); !r.ok {
		t.Fatalf("a DMARC record on a Private Email domain was refused: %s", r.output)
	}
	if got := n.fake.domains["example.com"].emailType; got != "OX" {
		t.Fatalf("a TXT write changed the mail setting to %s", got)
	}
}

// ---- URL redirects ---------------------------------------------------------------------------------

func redirectRecords() []xmlHost {
	return []xmlHost{
		hr("@", "URL", "https://www.example.net", 1799),
		hr("@", "TXT", "v=spf1 -all", 1799),
		hr("www", "CNAME", "app.example.net.", 1799),
		hr("go", "FRAME", "http://www.example.net/", 1799),
	}
}

// A URL redirect on @ is removed like any record: a backup first, the rest of the set written back
// as it was, the removal in the changed event; an A then goes on @, and restore brings the redirect
// back.
func TestRemoveTakesAwayAURLRedirectWithABackupAndRestorePutsItBack(t *testing.T) {
	n := newHarness(t)
	n.fake.add("example.com", true, redirectRecords()...)
	r := n.run("remove example.com URL @")
	if !r.ok {
		t.Fatalf("remove: %s", r.output)
	}
	sameHosts(t, redirectRecords()[1:], n.fake.hosts("example.com"))
	ids := n.backups("example.com")
	if len(ids) != 1 {
		t.Fatalf("backups: %v", ids)
	}
	ev := r.event("changed")
	if ev == nil || ev["command"] != "remove" || ev["backupId"] != ids[0] {
		t.Fatalf("changed event: %v", r.events)
	}
	removed, _ := ev["removed"].([]any)
	added, _ := ev["added"].([]any)
	if len(removed) != 1 || removed[0] != "URL @ https://www.example.net ttl 1799" || len(added) != 0 {
		t.Fatalf("changed event removed %v, added %v", ev["removed"], ev["added"])
	}
	mustContain(t, r.output, "removes URL @ https://www.example.net", "Backup "+ids[0])

	if r := n.run("ensure example.com A @ 203.0.113.20"); !r.ok {
		t.Fatalf("an A on @ after the redirect went: %s", r.output)
	}
	if r := n.run("remove example.com FRAME go http://www.example.net/"); !r.ok {
		t.Fatalf("a FRAME named by value: %s", r.output)
	}

	r = n.run("restore example.com " + ids[0])
	if !r.ok {
		t.Fatalf("restore: %s", r.output)
	}
	got, want := n.fake.hosts("example.com"), redirectRecords()
	if len(got) != len(want) {
		t.Fatalf("restored %d records: %+v", len(got), got)
	}
	for i := range want {
		if got[i].Name != want[i].Name || got[i].Type != want[i].Type || got[i].Address != want[i].Address || got[i].TTL != want[i].TTL {
			t.Errorf("record %d: %+v, want %+v", i+1, got[i], want[i])
		}
	}
}

func TestRemovingAURLRedirectInDryRunChangesNothing(t *testing.T) {
	n := newHarness(t)
	n.fake.add("example.com", true, redirectRecords()...)
	n.cfg["mode"] = "dryrun"
	r := n.run("remove example.com URL @ https://www.example.net")
	if !r.ok {
		t.Fatal(r.output)
	}
	mustContain(t, r.output, "Dry run for example.com: nothing was changed. removes URL @ https://www.example.net", "Before (4 records):", "After (3 records):")
	if n.fake.count(setHosts) != 0 || len(n.backups("example.com")) != 0 || r.event("changed") != nil {
		t.Errorf("dry run wrote: %v", n.fake.commands)
	}
	sameHosts(t, redirectRecords(), n.fake.hosts("example.com"))
}

// A domain with no redirect on the host is not touched: the remove says so and writes nothing.
func TestRemovingAURLRedirectThatIsNotThereChangesNothing(t *testing.T) {
	n := newHarness(t)
	n.fake.add("example.com", true, tenRecords()...)
	r := n.run("remove example.com URL @")
	mustContain(t, r.output, "there is no such URL record on @; nothing was changed")
	if n.fake.count(setHosts) != 0 || len(n.backups("example.com")) != 0 {
		t.Errorf("wrote: %v", n.fake.commands)
	}
	sameHosts(t, tenRecords(), n.fake.hosts("example.com"))
}

// An A, AAAA or CNAME on a host holding a redirect is refused, naming the redirect and the exact
// remove command, in dry run and apply alike; a TXT beside it is still added.
func TestEnsureOverAURLRedirectNamesTheRemoveCommand(t *testing.T) {
	for _, mode := range []string{"dryrun", "apply"} {
		n := newHarness(t)
		n.fake.add("example.com", true, redirectRecords()...)
		n.cfg["mode"] = mode
		for _, c := range []struct{ cmd, says string }{
			{"ensure example.com A @ 203.0.113.20", "Refused: @ holds a URL redirect record (URL @ https://www.example.net ttl 1799), and an A record cannot sit beside it"},
			{"ensure example.com AAAA @ 2001:db8::20", "an AAAA record cannot sit beside it"},
			{"ensure example.com CNAME go app.example.net", "remove it first with `remove example.com FRAME go http://www.example.net/`"},
		} {
			r := n.run(c.cmd)
			if r.ok {
				t.Fatalf("%s %s was accepted: %s", mode, c.cmd, r.output)
			}
			mustContain(t, r.output, c.says, "When a person has approved replacing the redirect", "Nothing was changed.")
		}
		r := n.run("ensure example.com A @ 203.0.113.20")
		mustContain(t, r.output, "`remove example.com URL @ https://www.example.net`")
		if n.fake.count(setHosts) != 0 || len(n.backups("example.com")) != 0 {
			t.Fatalf("%s: a refused ensure wrote: %v", mode, n.fake.commands)
		}
	}
	n := newHarness(t)
	n.fake.add("example.com", true, redirectRecords()...)
	if r := n.run(`ensure example.com TXT @ "site-verification=placeholder"`); !r.ok {
		t.Fatalf("a TXT beside a redirect: %s", r.output)
	}
}
