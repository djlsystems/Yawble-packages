package main

import (
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
)

// record is one host record. Host is "@" for the domain itself, otherwise the name below it in
// lower case ("www", "_dmarc"). MXPref is set only on MX records.
type record struct {
	Type   string `json:"type"`
	Host   string `json:"host"`
	Value  string `json:"value"`
	MXPref int    `json:"mxpref,omitempty"`
	TTL    int    `json:"ttl"`
}

// The types the commands change. Every other type in a set - URL, URL301 and FRAME redirects,
// NS, ALIAS, MXE - is written back exactly as it was read.
var changeable = map[string]bool{"A": true, "AAAA": true, "CNAME": true, "MX": true, "TXT": true, "CAA": true}

// byValue types may hold several records on one host, told apart by value: SPF and verification
// TXT records side by side, a mailbox's several MX hosts, several CAA issuers.
var byValue = map[string]bool{"TXT": true, "MX": true, "CAA": true}

const (
	defaultTTL    = 1799 // Namecheap's "Automatic"
	minTTL        = 60
	maxTTL        = 60000
	defaultMXPref = 10
)

var (
	hostPattern = regexp.MustCompile(`^(\*|[a-z0-9_]([a-z0-9_-]{0,61}[a-z0-9_])?)(\.(\*|[a-z0-9_]([a-z0-9_-]{0,61}[a-z0-9_])?))*$`)
	namePattern = regexp.MustCompile(`^([a-z0-9_]([a-z0-9_-]{0,61}[a-z0-9])?\.)*[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.?$`)
	caaPattern  = regexp.MustCompile(`^\d{1,3} (issue|issuewild|iodef) \S.*$`)
)

// normaliseHost turns what a person or Namecheap writes for a host into the form the set uses:
// "", "@" and the domain itself are "@"; "www.example.com" under example.com is "www".
func normaliseHost(h, domain string) string {
	h = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h)), ".")
	switch {
	case h == "" || h == "@" || h == domain:
		return "@"
	case strings.HasSuffix(h, "."+domain):
		return strings.TrimSuffix(h, "."+domain)
	}
	return h
}

// caaValue is a CAA value in the one form Namecheap's setHosts accepts: flag, tag and value with
// the value unquoted, "0 issue ca.example.net". Namecheap refuses the value quoted, yet getHosts
// answers it quoted ("0 issue \"ca.example.net\""), so every CAA value read, typed or restored is
// put in this form before it is compared or written back.
func caaValue(v string) string {
	parts := strings.SplitN(strings.TrimSpace(v), " ", 3)
	if len(parts) != 3 {
		return strings.TrimSpace(v)
	}
	value := strings.TrimSpace(parts[2])
	if len(value) >= 2 && strings.HasPrefix(value, `"`) && strings.HasSuffix(value, `"`) {
		if unquoted, err := strconv.Unquote(value); err == nil {
			value = unquoted
		} else {
			value = value[1 : len(value)-1]
		}
	}
	return parts[0] + " " + strings.ToLower(parts[1]) + " " + value
}

// sameValue compares two values the way DNS does: TXT exactly, CAA in Namecheap's one form, names
// without case or a trailing dot.
func sameValue(typ, a, b string) bool {
	switch typ {
	case "CAA":
		return caaValue(a) == caaValue(b)
	case "TXT", "URL", "URL301", "FRAME":
		return a == b
	}
	norm := func(s string) string { return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(s)), ".") }
	return norm(a) == norm(b)
}

// same says whether two records are the same record, TTL aside.
func same(a, b record) bool {
	return a.Type == b.Type && a.Host == b.Host && sameValue(a.Type, a.Value, b.Value) && (a.Type != "MX" || a.MXPref == b.MXPref)
}

func (r record) String() string {
	v := r.Value
	if r.Type == "TXT" || r.Type == "CAA" {
		v = strconv.Quote(v)
	}
	s := fmt.Sprintf("%s %s %s", r.Type, r.Host, v)
	if r.Type == "MX" {
		s += fmt.Sprintf(" pref %d", r.MXPref)
	}
	return s + fmt.Sprintf(" ttl %d", r.TTL)
}

// checkValue says what is wrong with a value for its type, or "".
func checkValue(typ, value string) string {
	switch typ {
	case "A":
		if ip := net.ParseIP(value); ip == nil || ip.To4() == nil || strings.Contains(value, ":") {
			return "an A record's value is an IPv4 address, such as 203.0.113.10"
		}
	case "AAAA":
		if ip := net.ParseIP(value); ip == nil || ip.To4() != nil {
			return "an AAAA record's value is an IPv6 address, such as 2001:db8::10"
		}
	case "CNAME", "MX":
		if !namePattern.MatchString(strings.ToLower(value)) || !strings.Contains(value, ".") {
			return fmt.Sprintf("a %s record's value is a host name, such as target.example.net", typ)
		}
	case "TXT":
		if value == "" || len(value) > 2048 {
			return "a TXT record's value is 1 to 2048 characters"
		}
	case "CAA":
		if !caaPattern.MatchString(value) {
			return `a CAA record's value is a flag, a tag and a value, such as '0 issue ca.example.net'`
		}
	}
	return ""
}

// change is what a command does to a set: the records added, changed (before and after) and
// removed, and the whole set afterwards.
type change struct {
	before, after []record
	added         []record
	changed       [][2]record
	removed       []record
}

func (c change) none() bool { return len(c.added)+len(c.changed)+len(c.removed) == 0 }

// ensure plans `ensure`: the record is there afterwards, and nothing else in the set moves.
// ttl and mxpref are 0 when not given.
func ensure(domain string, set []record, want record, ttlGiven, prefGiven bool) (change, error) {
	c := change{before: set, after: append([]record{}, set...)}

	var sameHost, sameType []int
	for i, r := range set {
		if r.Host != want.Host {
			continue
		}
		sameHost = append(sameHost, i)
		if r.Type == want.Type {
			sameType = append(sameType, i)
		}
	}
	cnameHere := false
	for _, i := range sameHost {
		if set[i].Type == "CNAME" {
			cnameHere = true
		}
	}

	// THE RECORD IS THERE: only its TTL or MX preference may change, and only when given.
	for _, i := range sameType {
		r := set[i]
		if !sameValue(r.Type, r.Value, want.Value) {
			continue
		}
		updated := r
		if ttlGiven {
			updated.TTL = want.TTL
		}
		if prefGiven && r.Type == "MX" {
			updated.MXPref = want.MXPref
		}
		if updated != r {
			c.after[i] = updated
			c.changed = append(c.changed, [2]record{r, updated})
		}
		return c, nil
	}

	// ONE SPF AND ONE DMARC PER HOST: a second one makes mail checks fail, so it is never added
	// beside the first.
	if want.Type == "TXT" {
		if kind := singular(want.Value); kind != "" {
			for _, i := range sameType {
				if singular(set[i].Value) == kind {
					return c, refusal(fmt.Sprintf("Refused: %s already has an %s record (%s), and a host holds only one. Merge the two into one value, or remove the old one first with `remove %s TXT %s %s`, then ensure again. Nothing was changed.",
						want.Host, kind, set[i].String(), domain, want.Host, strconv.Quote(set[i].Value)))
				}
			}
		}
	}

	if !ttlGiven {
		want.TTL = defaultTTL
	}
	if want.Type == "MX" && !prefGiven {
		want.MXPref = defaultMXPref
	}

	if want.Type == "CNAME" && len(sameHost) > len(sameType) {
		return c, refusal(fmt.Sprintf("Refused: %s already has other records (%s), and a CNAME cannot sit beside them; remove them first. Nothing was changed.", want.Host, describe(set, sameHost)))
	}
	if want.Type != "CNAME" && cnameHere {
		return c, refusal(fmt.Sprintf("Refused: %s is a CNAME, and a %s record cannot sit beside it; remove the CNAME first. Nothing was changed.", want.Host, want.Type))
	}

	// ONE VALUE PER HOST for A, AAAA and CNAME: the one record there takes the new value. Several
	// are left for a person to choose from, never guessed at.
	if !byValue[want.Type] && len(sameType) > 0 {
		if len(sameType) > 1 {
			return c, refusal(fmt.Sprintf("Refused: %s has %d %s records (%s); remove the ones not wanted with `remove`, naming each value, then ensure again. Nothing was changed.", want.Host, len(sameType), want.Type, describe(set, sameType)))
		}
		i := sameType[0]
		updated := set[i]
		updated.Value = want.Value
		if ttlGiven {
			updated.TTL = want.TTL
		}
		c.after[i] = updated
		c.changed = append(c.changed, [2]record{set[i], updated})
		return c, nil
	}

	c.after = append(c.after, want)
	c.added = append(c.added, want)
	return c, nil
}

// singular names the TXT records a host may hold only one of.
func singular(value string) string {
	v := strings.ToLower(strings.TrimSpace(value))
	switch {
	case strings.HasPrefix(v, "v=spf1 ") || v == "v=spf1":
		return "SPF"
	case strings.HasPrefix(v, "v=dmarc1"):
		return "DMARC"
	}
	return ""
}

// remove plans `remove`: the named record goes, nothing else. Without a value, the host must hold
// exactly one record of the type.
func remove(set []record, typ, host, value string, valueGiven bool) (change, error) {
	c := change{before: set}
	var match []int
	for i, r := range set {
		if r.Type == typ && r.Host == host && (!valueGiven || sameValue(typ, r.Value, value)) {
			match = append(match, i)
		}
	}
	if len(match) > 1 && !valueGiven {
		return c, refusal(fmt.Sprintf("Refused: %s has %d %s records (%s); name the value of the one to remove. Nothing was changed.", host, len(match), typ, describe(set, match)))
	}
	gone := map[int]bool{}
	for _, i := range match {
		gone[i] = true
		c.removed = append(c.removed, set[i])
	}
	for i, r := range set {
		if !gone[i] {
			c.after = append(c.after, r)
		}
	}
	return c, nil
}

// replace plans `restore`: the set becomes the saved one.
func replace(set, saved []record) change {
	c := change{before: set, after: saved}
	matched := map[int]bool{}
	for _, s := range saved {
		found := false
		for i, r := range set {
			if !matched[i] && same(r, s) {
				matched[i], found = true, true
				if r.TTL != s.TTL {
					c.changed = append(c.changed, [2]record{r, s})
				}
				break
			}
		}
		if !found {
			c.added = append(c.added, s)
		}
	}
	for i, r := range set {
		if !matched[i] {
			c.removed = append(c.removed, r)
		}
	}
	return c
}

// missing lists the records of want that are not in got.
func missing(want, got []record) []record {
	var out []record
	used := map[int]bool{}
	for _, w := range want {
		found := false
		for i, g := range got {
			if !used[i] && same(w, g) {
				used[i], found = true, true
				break
			}
		}
		if !found {
			out = append(out, w)
		}
	}
	return out
}

func describe(set []record, idx []int) string {
	var parts []string
	for _, i := range idx {
		parts = append(parts, set[i].String())
	}
	return strings.Join(parts, "; ")
}

func lines(set []record) string {
	if len(set) == 0 {
		return "  (no records)"
	}
	var b strings.Builder
	for i, r := range set {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString("  " + r.String())
	}
	return b.String()
}

func strs(rs []record) []string {
	out := []string{}
	for _, r := range rs {
		out = append(out, r.String())
	}
	return out
}

// emailType is the EmailType setHosts is sent with: MX while the set holds MX records, MXE while
// it holds MXE ones, and otherwise what the domain had (forwarding, a Namecheap mailbox, none).
func emailType(read string, set []record) string {
	has := func(t string) bool {
		for _, r := range set {
			if r.Type == t {
				return true
			}
		}
		return false
	}
	switch {
	case has("MX"):
		return "MX"
	case has("MXE"):
		return "MXE"
	case strings.EqualFold(read, "MX") || strings.EqualFold(read, "MXE"):
		return "NONE"
	}
	return read
}
