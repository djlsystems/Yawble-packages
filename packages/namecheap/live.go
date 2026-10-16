package main

import (
	"fmt"
	"net"
	"strings"
	"time"
)

// LIVE ON THE NAME SERVERS. Namecheap answering a write says it holds the record, not that its
// name servers serve it yet. After every change in apply mode the plugin asks the domain's own
// authoritative servers directly (dns.go) for each record added, changed or removed, polling every
// livePoll for up to liveWait, and says per record whether it is live. A record not yet live is
// not a failure: Namecheap holds it, and check asks again later.

// fqdn is a host of a domain as a full name.
func fqdn(host, domain string) string {
	if host == "@" {
		return domain
	}
	return host + "." + domain
}

func waitWords(d time.Duration) string {
	return fmt.Sprintf("%d s", int(d/time.Second))
}

func serverNames(servers []nameServer) string {
	var names []string
	for _, s := range servers {
		names = append(names, s.Name)
	}
	return strings.Join(names, ", ")
}

// expectation is one record a change must see served (or no longer served, for a removal).
type expectation struct {
	verb     string // the words summarise uses: "adds TXT ...", "removes ..."
	r        record
	present  bool // false: the record was removed and must no longer be served
	checkTTL bool // only the TTL changed, so the served TTL must be the new one
	live     bool
	lagging  []string // the servers that do not serve it as asked yet, with why when they did not answer
}

// servedMatches says whether a served record is the configured one.
func servedMatches(r record, s rr, checkTTL bool) bool {
	if typeName(s.Type) != r.Type || (r.Type == "MX" && s.Pref != r.MXPref) || (checkTTL && int(s.TTL) != r.TTL) {
		return false
	}
	if r.Type == "A" || r.Type == "AAAA" {
		a, b := net.ParseIP(r.Value), net.ParseIP(s.Value)
		return a != nil && b != nil && a.Equal(b)
	}
	return sameValue(r.Type, r.Value, s.Value)
}

// expectations lists a change's records in the order publishRecords publishes them.
func expectations(c change) []*expectation {
	var out []*expectation
	for _, r := range c.added {
		out = append(out, &expectation{verb: "adds " + r.String(), r: r, present: true})
	}
	for _, pair := range c.changed {
		ttlOnly := same(pair[0], pair[1])
		out = append(out, &expectation{verb: "changes " + pair[0].String() + " to " + pair[1].String(), r: pair[1], present: true, checkTTL: ttlOnly})
	}
	for _, r := range c.removed {
		out = append(out, &expectation{verb: "removes " + r.String(), r: r})
	}
	return out
}

// confirm waits for a written change's records on the domain's authoritative name servers, and
// answers whether each is live (in the order of expectations) and the words that say so.
func (p *plugin) confirm(domain string, c change) ([]bool, string) {
	all := expectations(c)
	var pending []*expectation
	for _, e := range all {
		if changeable[e.r.Type] {
			pending = append(pending, e)
		}
	}

	servers, err := authServers(domain)
	if err == nil && len(servers) == 0 {
		err = fmt.Errorf("none was found")
	}
	var b strings.Builder
	if err != nil {
		fmt.Fprintf(&b, "Not checked on the name servers: %s's name servers could not be found (%s). `check %s <host> <type>` asks them later.", domain, err.Error(), domain)
		return make([]bool, len(all)), b.String()
	}

	// The wait is liveWait, or what is left of the run's liveBudget when several changes share it.
	wait := min(liveWait, max(liveBudget-p.waited, 0))
	if len(pending) > 0 {
		p.progress(fmt.Sprintf("asking %s's name servers for %d record(s), for up to %s", domain, len(pending), waitWords(wait)))
	}
	start := p.time.now()
	deadline := start.Add(wait)
	defer func() { p.waited += p.time.now().Sub(start) }()
	for len(pending) > 0 {
		answers := map[string]serverAnswer{} // one question per server, name and type each round
		var still []*expectation
		for _, e := range pending {
			e.lagging = nil
			name := fqdn(e.r.Host, domain)
			for _, s := range servers {
				key := s.Name + " " + name + " " + e.r.Type
				a, ok := answers[key]
				if !ok {
					a = ask(s, name, dnsTypes[e.r.Type])
					answers[key] = a
				}
				if a.Err != nil {
					e.lagging = append(e.lagging, fmt.Sprintf("%s (no answer: %s)", s.Name, a.Err.Error()))
					continue
				}
				served := false
				for _, rec := range a.Records {
					if servedMatches(e.r, rec, e.checkTTL) {
						served = true
					}
				}
				if served != e.present {
					e.lagging = append(e.lagging, s.Name)
				}
			}
			if len(e.lagging) == 0 {
				e.live = true
			} else {
				still = append(still, e)
			}
		}
		pending = still
		if len(pending) == 0 || !p.time.now().Before(deadline) {
			break
		}
		p.time.sleep(livePoll)
	}

	fmt.Fprintf(&b, "On the name servers (%s), asked directly:", serverNames(servers))
	lives := make([]bool, len(all))
	notLive := false
	for i, e := range all {
		lives[i] = e.live
		switch {
		case !changeable[e.r.Type]:
			fmt.Fprintf(&b, "\n  not checked: %s (a URL redirect or %s record is not served under its own type)", e.verb, e.r.Type)
		case e.live:
			fmt.Fprintf(&b, "\n  live on the name servers: %s", e.verb)
		default:
			notLive = true
			still := ""
			if !e.present {
				still = " (it still answers the record)"
			}
			fmt.Fprintf(&b, "\n  not yet served by %s after %s: %s%s", strings.Join(e.lagging, " and "), waitWords(wait), e.verb, still)
		}
	}
	if notLive {
		fmt.Fprintf(&b, "\nNot yet live is not a failure: Namecheap holds the change, and its name servers usually serve it within minutes. `check %s <host> <type>` asks them again; look nothing up any other way until check shows it live.", domain)
	}
	return lives, b.String()
}

// ---- check -----------------------------------------------------------------------------------

// check asks each authoritative name server directly what it serves for a name and type, and
// the zone's negative TTL from its SOA. It changes nothing and publishes no changed event.
func (p *plugin) check(args []string) (string, error) {
	domain, err := p.domainArg(args, "check <domain> <host> <type>", 3, 3)
	if err != nil {
		return "", err
	}
	if !p.cfg.listed(domain) {
		return "", notListed(domain)
	}
	host := normaliseHost(args[1], domain)
	if problem := p.checkHost(host); problem != "" {
		return "", refusal(problem)
	}
	typ := strings.ToUpper(args[2])
	if !changeable[typ] {
		return "", refusal(fmt.Sprintf("Refused: check asks for A, AAAA, CNAME, MX, TXT or CAA records, not %s. Nothing was changed.", args[2]))
	}

	servers, err := authServers(domain)
	if err == nil && len(servers) == 0 {
		err = fmt.Errorf("none was found")
	}
	if err != nil {
		return "", refusal(fmt.Sprintf("Refused: %s's name servers could not be found (%s), so nothing could be asked. Nothing was changed.", domain, err.Error()))
	}

	name := fqdn(host, domain)
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s on %s's name servers, asked directly (this changes nothing):", typ, name, domain)
	answered := 0
	var notServing, negs []string
	negSeen := map[int]bool{}
	for _, s := range servers {
		a := ask(s, name, dnsTypes[typ])
		switch {
		case a.Err != nil:
			fmt.Fprintf(&b, "\n  %s: no answer (%s)", s.Name, a.Err.Error())
			notServing = append(notServing, s.Name)
			continue
		case len(a.Records) > 0:
			var parts []string
			for _, rec := range a.Records {
				parts = append(parts, servedString(rec))
			}
			fmt.Fprintf(&b, "\n  %s: %s", s.Name, strings.Join(parts, "; "))
		case len(a.Other) > 0:
			var parts []string
			for _, rec := range a.Other {
				parts = append(parts, servedString(rec))
			}
			fmt.Fprintf(&b, "\n  %s: serves no %s record; the name holds %s", s.Name, typ, strings.Join(parts, "; "))
			notServing = append(notServing, s.Name)
		case a.NoName:
			fmt.Fprintf(&b, "\n  %s: serves nothing - no such name (NXDOMAIN)", s.Name)
			notServing = append(notServing, s.Name)
		default:
			fmt.Fprintf(&b, "\n  %s: serves nothing - the name exists but holds no %s record", s.Name, typ)
			notServing = append(notServing, s.Name)
		}
		answered++
		soa := ask(s, domain, typeSOA)
		for _, rec := range soa.Records {
			neg := negativeTTL(rec)
			negs = append(negs, fmt.Sprintf("%s %d s", s.Name, neg))
			negSeen[neg] = true
		}
	}
	if answered == 0 {
		return "", refusal(fmt.Sprintf("Refused: none of %s's name servers (%s) answered, so nothing is known about %s %s. Nothing was changed.", domain, serverNames(servers), typ, name))
	}

	switch {
	case len(negs) == 0:
		b.WriteString("\nNegative TTL: no name server answered the zone's SOA.")
	case len(negSeen) == 1:
		for neg := range negSeen {
			fmt.Fprintf(&b, "\nNegative TTL (from the SOA): %d s. A look-up of a name before it exists is cached as no such name for up to that long by whatever resolver asked.", neg)
		}
	default:
		fmt.Fprintf(&b, "\nNegative TTL (from the SOA): %s. A look-up of a name before it exists is cached as no such name for up to that long by whatever resolver asked.", strings.Join(negs, ", "))
	}
	if len(notServing) == 0 {
		b.WriteString("\nServed by every name server: live on the name servers.")
	} else {
		fmt.Fprintf(&b, "\nNot served by %s.", strings.Join(notServing, " and "))
	}
	return b.String(), nil
}
