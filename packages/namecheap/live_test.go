package main

import (
	"strings"
	"testing"
	"time"
)

// ---- live on the name servers, after ensure, remove and restore --------------------------------------

func liveFlags(r runResult) []any {
	var out []any
	for _, e := range r.events {
		if e["type"] == "record" {
			p, _ := e["payload"].(map[string]any)
			out = append(out, p["live"])
		}
	}
	return out
}

func polls(c *fakeClock) int {
	n := 0
	for _, d := range c.slept {
		if d == livePoll {
			n++
		}
	}
	return n
}

func TestEnsureSaysLiveWhenTheNameServersServeTheRecord(t *testing.T) {
	n := newHarness(t)
	n.fake.add("example.com", true, tenRecords()...)
	r := n.run(`ensure example.com TXT asuid "token-placeholder-123" 300`)
	if !r.ok {
		t.Fatalf("ensure failed: %s", r.output)
	}
	mustContain(t, r.output,
		"On the name servers (ns1.example.net, ns2.example.net), asked directly:",
		`live on the name servers: adds TXT asuid "token-placeholder-123" ttl 300`)
	if strings.Contains(r.output, "not yet served") {
		t.Errorf("says not yet served:\n%s", r.output)
	}
	if ev := r.event("changed"); ev["live"] != true {
		t.Errorf("changed live = %v", ev["live"])
	}
	if got := liveFlags(r); len(got) != 1 || got[0] != true {
		t.Errorf("record live = %v", got)
	}
	for _, ns := range []*fakeDNS{n.ns1, n.ns2} {
		if q := ns.questions(); len(q) == 0 || q[0] != "asuid.example.com TXT" {
			t.Errorf("%s was asked %v", ns.name, q)
		}
	}
	if polls(n.clock) != 0 {
		t.Errorf("waited although the record was live at once: %v", n.clock.slept)
	}
}

func TestEnsureSaysNotYetServedAfterTheWaitAndDoesNotFail(t *testing.T) {
	n := newHarness(t)
	n.fake.add("example.com", true, tenRecords()...)
	n.ns2.freeze("example.com") // ns2 has not picked the change up
	r := n.run(`ensure example.com TXT asuid "token-placeholder-123"`)
	if !r.ok || r.code != 0 {
		t.Fatalf("a record not yet live failed the command: %s", r.output)
	}
	mustContain(t, r.output,
		`not yet served by ns2.example.net after 60 s: adds TXT asuid "token-placeholder-123" ttl 1799`,
		"Not yet live is not a failure",
		"`check example.com <host> <type>`")
	if strings.Contains(r.output, "live on the name servers: adds") {
		t.Errorf("says live:\n%s", r.output)
	}
	if ev := r.event("changed"); ev == nil || ev["live"] != false {
		t.Errorf("changed event %v", ev)
	}
	if got := liveFlags(r); len(got) != 1 || got[0] != false {
		t.Errorf("record live = %v", got)
	}
	// It polled every 5 s for the 60 s, on the test clock: the test itself did not wait.
	if p := polls(n.clock); p != 12 {
		t.Errorf("%d polls, want 12 (every 5 s for 60 s): %v", p, n.clock.slept)
	}
	if q := n.ns2.questions(); len(q) != 13 {
		t.Errorf("ns2 was asked %d times, want 13: %v", len(q), q)
	}
}

func TestTheWaitIsInjectable(t *testing.T) {
	n := newHarness(t)
	n.fake.add("example.com", true)
	old := liveWait
	liveWait = 10 * time.Second
	defer func() { liveWait = old }()
	n.ns1.freeze("example.com")
	r := n.run("ensure example.com A @ 203.0.113.10")
	mustContain(t, r.output, "not yet served by ns1.example.net after 10 s: adds A @ 203.0.113.10 ttl 1799")
	if p := polls(n.clock); p != 2 {
		t.Errorf("%d polls, want 2", p)
	}
}

func TestEnsureTurnsLiveWhenTheServerCatchesUpWithinTheWait(t *testing.T) {
	n := newHarness(t)
	n.fake.add("example.com", true)
	n.ns2.freeze("example.com")
	thaw := &thawingClock{fakeClock: n.clock, after: 3, thaw: func() {
		n.ns2.mu.Lock()
		n.ns2.frozen = nil
		n.ns2.mu.Unlock()
	}}
	r := n.runWith(thaw.clock(), "ensure example.com CNAME www app.example.net")
	mustContain(t, r.output, "live on the name servers: adds CNAME www app.example.net ttl 1799")
	if thaw.sleeps != 3 {
		t.Errorf("slept %d times, want 3", thaw.sleeps)
	}
}

// thawingClock runs a function on the nth sleep: a name server that picks the change up meanwhile.
type thawingClock struct {
	*fakeClock
	after, sleeps int
	thaw          func()
}

func (c *thawingClock) clock() clock {
	inner := c.fakeClock.clock()
	return clock{now: inner.now, sleep: func(d time.Duration) {
		inner.sleep(d)
		if d == livePoll {
			c.sleeps++
			if c.sleeps == c.after {
				c.thaw()
			}
		}
	}}
}

func TestAServerThatDoesNotAnswerIsNamedAndTheCommandStillSucceeds(t *testing.T) {
	n := newHarness(t)
	n.fake.add("example.com", true)
	n.ns1.silent = true
	old := dnsTimeout
	dnsTimeout = 50 * time.Millisecond
	defer func() { dnsTimeout = old }()
	r := n.run("ensure example.com A @ 203.0.113.10")
	if !r.ok {
		t.Fatal(r.output)
	}
	mustContain(t, r.output, "not yet served by ns1.example.net (no answer: it did not answer within 50ms) after 60 s")
}

func TestNameServersThatCannotBeFoundAreSaidAndTheCommandStillSucceeds(t *testing.T) {
	n := newHarness(t)
	n.fake.add("example.com", true)
	authServers = func(string) ([]nameServer, error) { return nil, nil }
	r := n.run("ensure example.com A @ 203.0.113.10")
	if !r.ok {
		t.Fatal(r.output)
	}
	mustContain(t, r.output, "Not checked on the name servers: example.com's name servers could not be found (none was found)")
	if ev := r.event("changed"); ev["live"] != false {
		t.Errorf("changed live = %v", ev["live"])
	}
}

func TestATTLChangeIsLiveOnlyOnceTheNewTTLIsServed(t *testing.T) {
	n := newHarness(t)
	n.fake.add("example.com", true, hr("@", "A", "203.0.113.10", 1799))
	n.ns1.freeze("example.com")
	r := n.run("ensure example.com A @ 203.0.113.10 300")
	mustContain(t, r.output, "not yet served by ns1.example.net after 60 s: changes A @ 203.0.113.10 ttl 1799 to A @ 203.0.113.10 ttl 300")
}

func TestRemoveConfirmsTheRecordIsGoneFromTheNameServers(t *testing.T) {
	n := newHarness(t)
	n.fake.add("example.com", true, tenRecords()...)
	r := n.run("remove example.com TXT _dmarc")
	if !r.ok {
		t.Fatal(r.output)
	}
	mustContain(t, r.output, `live on the name servers: removes TXT _dmarc "v=DMARC1; p=none; rua=mailto:dmarc@example.com" ttl 1799`)
	if ev := r.event("changed"); ev["live"] != true {
		t.Errorf("changed live = %v", ev["live"])
	}
	if got := liveFlags(r); len(got) != 1 || got[0] != true {
		t.Errorf("record live = %v", got)
	}

	n.ns2.freeze("example.com")
	r = n.run("remove example.com A blog")
	if !r.ok {
		t.Fatal(r.output)
	}
	mustContain(t, r.output, "not yet served by ns2.example.net after 60 s: removes A blog 203.0.113.11 ttl 600 (it still answers the record)")
	if ev := r.event("changed"); ev["live"] != false {
		t.Errorf("changed live = %v", ev["live"])
	}
	if got := liveFlags(r); len(got) != 1 || got[0] != false {
		t.Errorf("record live = %v", got)
	}
}

func TestRemovingARedirectSaysItIsNotChecked(t *testing.T) {
	n := newHarness(t)
	n.fake.add("example.com", true, tenRecords()...)
	r := n.run("remove example.com URL301 old")
	if !r.ok {
		t.Fatal(r.output)
	}
	mustContain(t, r.output, "not checked: removes URL301 old http://www.example.com/ ttl 1799")
	if got := liveFlags(r); len(got) != 1 || got[0] != false {
		t.Errorf("record live = %v", got)
	}
}

func TestRestoreConfirmsTheSameWay(t *testing.T) {
	n := newHarness(t)
	n.fake.add("example.com", true, tenRecords()...)
	r := n.run(`ensure example.com TXT asuid "token-placeholder-123"`)
	if !r.ok {
		t.Fatal(r.output)
	}
	id := r.event("changed")["backupId"].(string)

	r = n.run("restore example.com " + id)
	if !r.ok {
		t.Fatal(r.output)
	}
	mustContain(t, r.output, `live on the name servers: removes TXT asuid "token-placeholder-123" ttl 1799`)
	if ev := r.event("changed"); ev["live"] != true || ev["command"] != "restore" {
		t.Errorf("changed event %v", ev)
	}

	// Put it back, with ns1 still serving the restored set: the record restore adds is not live.
	r = n.run(`ensure example.com TXT asuid "token-placeholder-123"`)
	again := r.event("changed")["backupId"].(string)
	n.ns1.freeze("example.com")
	r = n.run("restore example.com " + again)
	if !r.ok {
		t.Fatal(r.output)
	}
	mustContain(t, r.output, `not yet served by ns1.example.net after 60 s: removes TXT asuid "token-placeholder-123" ttl 1799 (it still answers the record)`)
	if got := liveFlags(r); len(got) != 1 || got[0] != false {
		t.Errorf("record live = %v", got)
	}
}

func TestOneRunWaitsNoLongerThanItsBudget(t *testing.T) {
	n := newHarness(t)
	n.fake.add("example.com", true)
	old := liveBudget
	liveBudget = 90 * time.Second
	defer func() { liveBudget = old }()
	n.ns2.freeze("example.com")
	r := n.run("ensure example.com A @ 203.0.113.10", "ensure example.com A www 203.0.113.10", "ensure example.com A api 203.0.113.10")
	mustContain(t, r.output,
		"1. example.com: adds A @",
		"not yet served by ns2.example.net after 60 s: adds A @",
		"not yet served by ns2.example.net after 30 s: adds A www",
		"not yet served by ns2.example.net after 0 s: adds A api")
}

func TestDryRunAsksNoNameServer(t *testing.T) {
	n := newHarness(t)
	n.fake.add("example.com", true)
	n.cfg["mode"] = "dryrun"
	r := n.run("ensure example.com A @ 203.0.113.10")
	if !r.ok || len(n.ns1.questions())+len(n.ns2.questions()) != 0 {
		t.Errorf("dry run asked the name servers: %v %v\n%s", n.ns1.questions(), n.ns2.questions(), r.output)
	}
}

// ---- check -------------------------------------------------------------------------------------------

func TestCheckAnswersEachServersRecordsTTLAndTheNegativeTTL(t *testing.T) {
	n := newHarness(t)
	n.fake.add("example.com", true, hr("asuid", "TXT", "token-old", 1799))
	n.ns2.freeze("example.com")
	n.fake.domains["example.com"].hosts = []xmlHost{hr("asuid", "TXT", "token-placeholder-123", 300)}

	r := n.run("check example.com asuid TXT")
	if !r.ok {
		t.Fatal(r.output)
	}
	mustContain(t, r.output,
		"TXT asuid.example.com on example.com's name servers, asked directly (this changes nothing):",
		`ns1.example.net: TXT "token-placeholder-123" ttl 300`,
		`ns2.example.net: TXT "token-old" ttl 1799`,
		"Negative TTL (from the SOA): 3601 s.")
	if len(r.events) != 0 {
		t.Errorf("check published %v", r.events)
	}
	if len(n.fake.commands) != 0 {
		t.Errorf("check called Namecheap's API: %v", n.fake.commands)
	}
	if len(n.backups("example.com")) != 0 {
		t.Errorf("check took a backup")
	}
}

func TestCheckSaysAServerServesNothing(t *testing.T) {
	n := newHarness(t)
	n.fake.add("example.com", true, hr("www", "CNAME", "app.example.net.", 300), hr("@", "A", "203.0.113.10", 1799))
	r := n.run("check example.com asuid.www TXT")
	if !r.ok {
		t.Fatal(r.output)
	}
	mustContain(t, r.output,
		"ns1.example.net: serves nothing - no such name (NXDOMAIN)",
		"ns2.example.net: serves nothing - no such name (NXDOMAIN)",
		"Negative TTL (from the SOA): 3601 s.",
		"Not served by ns1.example.net and ns2.example.net.")

	r = n.run("check example.com @ TXT")
	mustContain(t, r.output, "ns1.example.net: serves nothing - the name exists but holds no TXT record")

	r = n.run("check example.com www TXT")
	mustContain(t, r.output, "ns1.example.net: serves no TXT record; the name holds CNAME app.example.net ttl 300")

	r = n.run("check example.com www.example.com CNAME")
	mustContain(t, r.output, "ns2.example.net: CNAME app.example.net ttl 300", "Served by every name server: live on the name servers.")
	if len(r.events) != 0 {
		t.Errorf("check published %v", r.events)
	}
}

func TestCheckNamesTheNegativeTTLOfEachServerWhenTheyDiffer(t *testing.T) {
	n := newHarness(t)
	n.fake.add("example.com", true)
	n.ns2.soaTTL = 600
	r := n.run("check example.com @ A")
	mustContain(t, r.output, "Negative TTL (from the SOA): ns1.example.net 3601 s, ns2.example.net 600 s.")
}

func TestCheckRefusesWhatItCannotAsk(t *testing.T) {
	n := newHarness(t)
	n.fake.add("example.com", true)
	for cmd, want := range map[string]string{
		"check example.org @ A":      "example.org is not in this member's domains setting",
		"check example.com @ NS":     "check asks for A, AAAA, CNAME, MX, TXT or CAA records, not NS",
		"check example.com @":        "Usage: check <domain> <host> <type>",
		"check example.com bad! TXT": `"bad!" is not a host`,
	} {
		r := n.run(cmd)
		if r.ok {
			t.Errorf("%s: not refused:\n%s", cmd, r.output)
		}
		mustContain(t, r.output, want)
		if r.event("refused") == nil || r.event("changed") != nil {
			t.Errorf("%s: events %v", cmd, r.events)
		}
	}

	n.ns1.silent, n.ns2.silent = true, true
	old := dnsTimeout
	dnsTimeout = 50 * time.Millisecond
	defer func() { dnsTimeout = old }()
	r := n.run("check example.com @ A")
	if r.ok {
		t.Fatalf("a check no server answered succeeded:\n%s", r.output)
	}
	mustContain(t, r.output, "none of example.com's name servers (ns1.example.net, ns2.example.net) answered")
}

// ---- defaultTtl ----------------------------------------------------------------------------------------

func TestDefaultTTLIsUsedWhenEnsureNamesNoTTLAndAGivenTTLWins(t *testing.T) {
	n := newHarness(t)
	n.fake.add("example.com", true)
	n.cfg["defaultTtl"] = 300
	r := n.run("ensure example.com A @ 203.0.113.10", "ensure example.com TXT @ verify-token 600")
	if !r.ok {
		t.Fatal(r.output)
	}
	got := n.fake.hosts("example.com")
	if len(got) != 2 || got[0].TTL != "300" || got[1].TTL != "600" {
		t.Fatalf("records %+v, want ttl 300 (the setting) then 600 (given)", got)
	}
	mustContain(t, r.output, "adds A @ 203.0.113.10 ttl 300", `adds TXT @ "verify-token" ttl 600`)

	n.cfg = map[string]any{"domains": []string{"example.com"}, "mode": "apply"}
	r = n.run("ensure example.com A www 203.0.113.10")
	mustContain(t, r.output, "adds A www 203.0.113.10 ttl 1799")
}

func TestDefaultTTLOutOfRangeIsRefusedNamingTheField(t *testing.T) {
	for _, v := range []any{59, 60001, 300.5, "300"} {
		n := newHarness(t)
		n.fake.add("example.com", true)
		n.cfg["defaultTtl"] = v
		r := n.run("ensure example.com A @ 203.0.113.10")
		if r.ok {
			t.Errorf("defaultTtl %v: not refused", v)
		}
		mustContain(t, r.output, "`defaultTtl` must be a whole number from 60 to 60000", "nothing was changed")
		if len(n.fake.commands) != 0 {
			t.Errorf("defaultTtl %v: Namecheap was called", v)
		}
	}
	for _, v := range []any{60, 60000} {
		n := newHarness(t)
		n.fake.add("example.com", true)
		n.cfg["defaultTtl"] = v
		if r := n.run("ensure example.com A @ 203.0.113.10"); !r.ok {
			t.Errorf("defaultTtl %v refused: %s", v, r.output)
		}
	}
}

// ---- the wire format -----------------------------------------------------------------------------------

func TestACompressedNameIsRead(t *testing.T) {
	// "example.com" at 12, then "www" pointing at it.
	msg := make([]byte, 12)
	name, _ := encodeName("Example.com")
	msg = append(msg, name...)
	msg = append(msg, 3, 'w', 'w', 'w', 0xC0, 12)
	got, next, err := readName(msg, 12+len(name))
	if err != nil || got != "www.example.com" || next != len(msg) {
		t.Fatalf("%q %d %v", got, next, err)
	}
	loop := append(make([]byte, 12), 0xC0, 12)
	if _, _, err := readName(loop, 12); err == nil {
		t.Errorf("a pointer loop was read")
	}
}
