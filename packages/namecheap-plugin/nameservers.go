package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"
)

// NAMESERVERS. A domain's delegation: Namecheap BasicDNS, PremiumDNS or FreeDNS, or custom
// nameservers. `nameservers` reads it (domains.dns.getList) in every mode; `setcustom` and
// `setdefault` change it (domains.dns.setCustom, domains.dns.setDefault) under the same rules as a
// record change, and only while a person has turned on allowNameserverChanges:
//
//  1. the setting is on, or the command is refused, in dry run too;
//  2. the domain is in `domains`, the names are checked (count, hostname, duplicates), and the
//     current delegation is read, which refuses a domain not in the account;
//  3. the same nameservers, or setdefault on BasicDNS, is a no-op that says so;
//  4. each custom nameserver must resolve (resolveHost, which tests replace);
//  5. dry run answers the nameservers before and after and stops;
//  6. the delegation before is saved as a backup (backup.go, kind "nameservers"), and, when the
//     domain is on Namecheap DNS, its host records as a record backup too;
//  7. the change is sent, read back and checked; only then is plugin.namecheap-plugin.nameservers
//     published. `restore <domain> <backup id>` of a nameserver backup puts the delegation back,
//     whichever way it went, except to PremiumDNS or FreeDNS: the API can only set custom
//     nameservers or BasicDNS, so restoring such a backup is refused and the person re-selects it
//     in the Namecheap portal.

const (
	// minNameservers: Namecheap's "How to Change DNS For a Domain" (knowledge base article 767)
	// asks for at least two nameservers.
	minNameservers = 2
	// maxNameservers is this plugin's own guard, not a documented Namecheap limit: Namecheap's API
	// reference for domains.dns.setCustom documents Nameservers only as a comma-separated string of
	// at most 1200 characters, with no count. 12 is the count of the registry interface behind it
	// (eNom's NS1 to NS12, known only from third-party sources); for registries outside
	// tldNameserverCap a lower registry limit is left to Namecheap's own error. See the README.
	maxNameservers = 12
	// maxNameserverChars is setCustom's documented MaxLength for Nameservers.
	maxNameserverChars = 1200
)

// tldNameserverCap is the registry caps article 767 names for some TLDs, below maxNameservers.
var tldNameserverCap = map[string]int{"mx": 5, "to": 4, "ca": 6}

// basicDNS is Namecheap BasicDNS's nameservers, as getList answers them.
var basicDNS = []string{"dns1.registrar-servers.com", "dns2.registrar-servers.com"}

// resolveHost finds a nameserver's addresses. Only tests replace it; no test uses real DNS.
var resolveHost = func(name string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return net.DefaultResolver.LookupHost(ctx, name)
}

const allowSetting = "allowNameserverChanges"

// The modes a backup records, and how the plugin says them.
const (
	nsBasic   = "basicdns"
	nsPremium = "premiumdns"
	nsFree    = "freedns"
	nsOther   = "namecheap" // Namecheap's DNS, with neither flag and not BasicDNS's nameservers
	nsCustom  = "custom"
)

func modeWords(mode string) string {
	switch mode {
	case nsBasic:
		return "Namecheap BasicDNS"
	case nsPremium:
		return "Namecheap PremiumDNS"
	case nsFree:
		return "Namecheap FreeDNS"
	case nsOther:
		return "other Namecheap DNS"
	}
	return "custom nameservers"
}

// portalOnly says whether a mode can only be selected in the Namecheap portal: the API sets
// custom nameservers (setCustom) or BasicDNS (setDefault), nothing else.
func portalOnly(mode string) bool {
	return mode == nsPremium || mode == nsFree || mode == nsOther
}

// nsState is a domain's delegation as getList answers it (IsUsingOurDNS, IsPremiumDNS,
// IsUsingFreeDNS and the nameservers), or getInfo for a FreeDNS domain.
type nsState struct {
	OurDNS   bool
	Premium  bool
	FreeDNS  bool
	Servers  []string // lower case, no trailing dot, in Namecheap's order
	FromInfo bool     // read through domains.getInfo, because getList does not answer for FreeDNS
}

func (s nsState) mode() string {
	switch {
	case !s.OurDNS:
		return nsCustom
	case s.FreeDNS:
		return nsFree
	case s.Premium:
		return nsPremium
	case len(s.Servers) == 0 || sameServers(s.Servers, basicDNS):
		return nsBasic
	}
	return nsOther
}

func (s nsState) String() string {
	if len(s.Servers) == 0 {
		return modeWords(s.mode()) + " (Namecheap names no nameservers)"
	}
	return modeWords(s.mode()) + ": " + strings.Join(s.Servers, ", ")
}

// nsTarget is what a change asks for: BasicDNS, or these servers as custom nameservers.
type nsTarget struct {
	basic   bool
	servers []string
}

func (t nsTarget) String() string {
	if t.basic {
		return modeWords(nsBasic) + ": " + strings.Join(basicDNS, ", ")
	}
	return modeWords(nsCustom) + ": " + strings.Join(t.servers, ", ")
}

func (t nsTarget) mode() string {
	if t.basic {
		return nsBasic
	}
	return nsCustom
}

// holds says whether the domain already has what the target asks for.
func (t nsTarget) holds(s nsState) bool {
	if t.basic {
		return s.mode() == nsBasic
	}
	return !s.OurDNS && sameServers(s.Servers, t.servers)
}

func sameServers(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x, y := append([]string{}, a...), append([]string{}, b...)
	sort.Strings(x)
	sort.Strings(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

// ---- reading ---------------------------------------------------------------------------------

// nsDomain reads a nameserver command's domain and checks it may be used: listed, and keys bound.
func (p *plugin) nsDomain(args []string, usage string, min, max int) (string, error) {
	domain, err := p.domainArg(args, usage, min, max)
	if err != nil {
		return "", err
	}
	if !p.cfg.listed(domain) {
		return "", notListed(domain)
	}
	if p.sec.APIUser == "" || p.sec.APIKey == "" {
		return "", refusal("Refused: no Namecheap API user and API key are bound: " + noKeys + ", then runs setup. Nothing was changed.")
	}
	return domain, nil
}

// readNS reads a domain's delegation, refusing by name a domain that is not in the account.
func (p *plugin) readNS(domain string) (*nsState, error) {
	s, err := p.api.getNameservers(domain)
	if err == nil {
		return s, nil
	}
	var e *apiError
	if errors.As(err, &e) && (e.Number == "2019166" || e.Number == "2016166") {
		return nil, refusal(fmt.Sprintf("Refused: %s is not in this Namecheap account (%s), so its nameservers are not this account's to read or change. Nothing was changed.", domain, e.Error()))
	}
	var r refusal
	if errors.As(err, &r) {
		return nil, refusal(string(r) + " Nothing was changed.")
	}
	return nil, refusal(fmt.Sprintf("Refused: %s's nameservers could not be read (%s), so nothing was changed. Run setup to see what is missing.", domain, err.Error()))
}

func (p *plugin) nameservers(args []string) (string, error) {
	domain, err := p.nsDomain(args, "nameservers <domain>", 1, 1)
	if err != nil {
		return "", err
	}
	s, err := p.readNS(domain)
	if err != nil {
		return "", err
	}
	out := fmt.Sprintf("%s is on %s.", domain, s)
	switch s.mode() {
	case nsBasic:
		out += fmt.Sprintf("\nNamecheap serves its host records (`list %s` shows them).", domain)
	case nsPremium:
		out += fmt.Sprintf("\nNamecheap PremiumDNS serves its host records (`list %s` shows them). This plugin can move it to custom nameservers or BasicDNS, but cannot put it back on PremiumDNS: that is re-selected in the Namecheap portal.", domain)
	case nsFree:
		out += ("\nNamecheap FreeDNS serves it. This plugin can move it to custom nameservers or BasicDNS, but cannot put it back on FreeDNS: that is re-selected in the Namecheap portal.")
		if s.FromInfo {
			out += " (Namecheap's domains.dns.getList does not answer for a FreeDNS domain, so this was read with domains.getInfo.)"
		}
	case nsOther:
		out += "\nNamecheap's own DNS, but getList flags it as neither BasicDNS, PremiumDNS nor FreeDNS and names other nameservers than BasicDNS's."
	case nsCustom:
		out += "\nIts Namecheap host records are not served while it is on custom nameservers; the nameservers above answer for it."
	}
	if p.cfg.AllowNameserverChanges {
		out += "\nNameserver changes are on (setting \"Allow nameserver changes\"); mode " + p.cfg.Mode + "."
	} else {
		out += "\nNameserver changes are off: a person turns on \"Allow nameserver changes\" to allow setcustom and setdefault."
	}
	if ids := p.recentBackups(domain, 5, kindNameservers); len(ids) > 0 {
		out += "\nNewest nameserver backups: " + strings.Join(ids, ", ") + "."
	}
	return out, nil
}

// ---- changing --------------------------------------------------------------------------------

func (p *plugin) nsAllowed() error {
	if p.cfg.AllowNameserverChanges {
		return nil
	}
	return refusal(fmt.Sprintf("Refused: nameserver changes are off for this member. A person turns on the setting \"Allow nameserver changes\" (%s) in this member's settings to allow setcustom, setdefault and restoring a nameserver backup, in dry run as well as apply. Reading with `nameservers <domain>` works without it. Nothing was changed.", allowSetting))
}

// checkNameservers normalises and checks a list of custom nameservers for a domain, without
// sending anything: each is a hostname, none is named twice, there are 2 to the limit, and the
// list fits setCustom's 1200 characters.
func checkNameservers(domain string, words []string) ([]string, error) {
	var names []string
	seen := map[string]string{}
	for _, w := range words {
		for _, part := range strings.Split(w, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			n := normaliseDomain(part)
			if len(n) > 253 || !domainPattern.MatchString(n) {
				return nil, refusal(fmt.Sprintf("Refused: %q is not a hostname; a nameserver is a name such as ns1.example.net, not an address or a URL. Nothing was changed.", part))
			}
			if first, dup := seen[n]; dup {
				return nil, refusal(fmt.Sprintf("Refused: %s is named twice (as %q and %q); nameservers are compared without case and without a trailing dot. Nothing was changed.", n, first, part))
			}
			seen[n] = part
			names = append(names, n)
		}
	}
	limit, why := maxNameservers, ""
	tld := domain[strings.LastIndex(domain, ".")+1:]
	if c, ok := tldNameserverCap[tld]; ok {
		limit, why = c, fmt.Sprintf(" (the .%s registry's limit)", strings.ToUpper(tld))
	}
	if len(names) < minNameservers || len(names) > limit {
		return nil, refusal(fmt.Sprintf("Refused: %d nameserver(s) named; a domain takes %d to %d nameservers%s. Nothing was changed.", len(names), minNameservers, limit, why))
	}
	if n := len(strings.Join(names, ",")); n > maxNameserverChars {
		return nil, refusal(fmt.Sprintf("Refused: the nameservers come to %d characters; Namecheap takes at most %d. Nothing was changed.", n, maxNameserverChars))
	}
	if sameServers(names, basicDNS) {
		return nil, refusal(fmt.Sprintf("Refused: those are Namecheap BasicDNS's own nameservers; `setdefault %s` puts the domain back on BasicDNS. Nothing was changed.", domain))
	}
	return names, nil
}

// unresolved refuses, naming each, the nameservers that do not resolve to an address.
func unresolved(names []string) error {
	var bad []string
	for _, n := range names {
		if addrs, err := resolveHost(n); err != nil || len(addrs) == 0 {
			bad = append(bad, n)
		}
	}
	if len(bad) == 0 {
		return nil
	}
	return refusal(fmt.Sprintf("Refused: %s does not resolve to an address, so the domain would be delegated to a nameserver nobody can reach. Check the name with the DNS provider. Nothing was changed.", strings.Join(bad, " and ")))
}

func (p *plugin) setcustom(args []string) (string, error) {
	const usage = "setcustom <domain> <nameserver> <nameserver> [more nameservers], by name, separated by spaces or commas"
	domain, err := p.domainArg(args, usage, 2, 1000)
	if err != nil {
		return "", err
	}
	if err := p.nsAllowed(); err != nil {
		return "", err
	}
	if domain, err = p.nsDomain(args, usage, 2, 1000); err != nil {
		return "", err
	}
	names, err := checkNameservers(domain, args[1:])
	if err != nil {
		return "", err
	}
	cur, err := p.readNS(domain)
	if err != nil {
		return "", err
	}
	to := nsTarget{servers: names}
	if to.holds(*cur) {
		return fmt.Sprintf("%s is already on %s; nothing was changed.", domain, cur), nil
	}
	if err := unresolved(names); err != nil {
		return "", err
	}
	return p.changeNS("setcustom", domain, cur, to)
}

func (p *plugin) setdefault(args []string) (string, error) {
	const usage = "setdefault <domain>"
	if _, err := p.domainArg(args, usage, 1, 1); err != nil {
		return "", err
	}
	if err := p.nsAllowed(); err != nil {
		return "", err
	}
	domain, err := p.nsDomain(args, usage, 1, 1)
	if err != nil {
		return "", err
	}
	cur, err := p.readNS(domain)
	if err != nil {
		return "", err
	}
	to := nsTarget{basic: true}
	if to.holds(*cur) {
		return fmt.Sprintf("%s is already on %s; nothing was changed.", domain, cur), nil
	}
	return p.changeNS("setdefault", domain, cur, to)
}

// restoreNS puts back the delegation a nameserver backup holds: setDefault for a backup taken on
// BasicDNS, setCustom with the saved nameservers otherwise.
func (p *plugin) restoreNS(domain string, b *backup) (string, error) {
	if err := p.nsAllowed(); err != nil {
		return "", err
	}
	if p.sec.APIUser == "" || p.sec.APIKey == "" {
		return "", refusal("Refused: no Namecheap API user and API key are bound: " + noKeys + ", then runs setup. Nothing was changed.")
	}
	if portalOnly(b.NSMode) {
		return "", refusal(fmt.Sprintf("Refused: backup %s was taken while %s was on %s (%s). Namecheap's API can only set custom nameservers or BasicDNS, so restoring it would leave the domain on custom nameservers, not on %s. %s Nothing was changed.",
			b.ID, domain, modeWords(b.NSMode), strings.Join(b.Nameservers, ", "), modeWords(b.NSMode), portalSteps(domain, b.NSMode, b.RecordBackup)))
	}
	to := nsTarget{basic: b.NSMode == nsBasic}
	if !to.basic {
		names, err := checkNameservers(domain, b.Nameservers)
		if err != nil {
			return "", refusal(fmt.Sprintf("Refused: backup %s's nameservers cannot be set: %s", b.ID, strings.TrimPrefix(err.Error(), "Refused: ")))
		}
		to.servers = names
	}
	cur, err := p.readNS(domain)
	if err != nil {
		return "", err
	}
	if to.holds(*cur) {
		return fmt.Sprintf("%s already holds backup %s's nameservers (%s); nothing was changed.", domain, b.ID, cur), nil
	}
	if !to.basic {
		if err := unresolved(to.servers); err != nil {
			return "", err
		}
	}
	return p.changeNS("restore", domain, cur, to)
}

// nsWarnings is what every change answer says plainly about what the change does to the domain.
func nsWarnings(domain string, to nsTarget) string {
	var w []string
	if to.basic {
		w = append(w, fmt.Sprintf("On Namecheap BasicDNS, Namecheap serves %s's host records again (`list %s` shows them), and whatever the custom nameservers held is no longer served. Namecheap may not keep host records across a nameserver switch: check `list %s` afterwards and restore the record backup this plugin saved, if one is missing.", domain, domain, domain))
	} else {
		w = append(w, fmt.Sprintf("While %s is on custom nameservers, its Namecheap host records are no longer served: the custom nameservers answer for it, so every record it needs (web, mail, verification) must already be set up there. Namecheap's URL redirects, email forwarding and Dynamic DNS stop working for it too.", domain))
	}
	w = append(w, "Delegation changes can take up to 48 hours to spread: until then some resolvers still ask the old nameservers.")
	return strings.Join(w, "\n")
}

// portalNote is what a change off PremiumDNS or FreeDNS says before it is made.
func portalNote(domain, mode string) string {
	return fmt.Sprintf("%s is on %s now. `restore` cannot put it back there afterwards: Namecheap's API can only set custom nameservers or BasicDNS, so going back to %s means a person re-selects it in the Namecheap portal (Domain List, Manage, Nameservers). The host records saved as a record backup stay restorable with `restore` once the domain is on Namecheap DNS.", domain, modeWords(mode), modeWords(mode))
}

// portalNoteAfter is what an applied change off PremiumDNS or FreeDNS says: the domain was on
// that mode before the change, and restore cannot put it back there.
func portalNoteAfter(domain, mode string) string {
	return fmt.Sprintf("%s was on %s before this change. `restore` cannot put it back there: Namecheap's API can only set custom nameservers or BasicDNS, so going back means %s", domain, modeWords(mode), lowerFirst(portalSelect(domain, mode)))
}

// portalSelect is the portal step that puts a domain back on a mode only the portal selects.
// "Other Namecheap DNS" is not a mode anyone can pick, so for it the person picks the
// Namecheap DNS option the domain used before.
func portalSelect(domain, mode string) string {
	if mode == nsOther {
		return fmt.Sprintf("A person re-selects the Namecheap DNS option %s used before (BasicDNS, PremiumDNS or FreeDNS) in the Namecheap portal (Domain List, Manage, Nameservers).", domain)
	}
	return fmt.Sprintf("A person re-selects %s for %s in the Namecheap portal (Domain List, Manage, Nameservers).", modeWords(mode), domain)
}

// portalSteps is how a domain goes back to a mode only the portal selects: the portal step, then
// the record backup saved with the change and its restore command, if there is one.
func portalSteps(domain, mode, recordBackup string) string {
	records := " No record backup was saved with it."
	if recordBackup != "" {
		records = fmt.Sprintf(" Its host records from before the change are record backup %s: `restore %s %s` puts them back once the domain is on Namecheap DNS again.", recordBackup, domain, recordBackup)
	}
	return "To go back, " + lowerFirst(portalSelect(domain, mode)) + records
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

// nsUndo is how a failed or unconfirmed change says the nameservers before go back: `restore`
// with the nameserver backup, or, off a mode only the portal selects, the portal step.
func nsUndo(domain, id, recordBackup, mode string) string {
	if portalOnly(mode) {
		return fmt.Sprintf("Backup %s records the nameservers before, but `restore` cannot put the domain back on %s: Namecheap's API can only set custom nameservers or BasicDNS. %s", id, modeWords(mode), portalSteps(domain, mode, recordBackup))
	}
	return ""
}

// changeNS carries out a planned delegation change, or describes it in dry run.
func (p *plugin) changeNS(command, domain string, cur *nsState, to nsTarget) (string, error) {
	warnings := nsWarnings(domain, to)
	if p.cfg.Mode != "apply" {
		if portalOnly(cur.mode()) {
			warnings = portalNote(domain, cur.mode()) + "\n" + warnings
		}
		extra := ""
		if cur.FreeDNS {
			extra = "\nIn apply, the host records are saved as a record backup too, before the change, if Namecheap's API answers them for this FreeDNS domain; if it does not, the change goes ahead without a record backup and the answer says so."
		} else if cur.OurDNS {
			extra = "\nIn apply, the host records are saved as a record backup too, before the change."
		}
		return fmt.Sprintf("Dry run for %s: nothing was changed. The nameservers would change\nfrom %s\nto   %s%s\n%s\nTo make the change, a person sets this member's mode to apply; then send the same command again.",
			domain, cur, to, extra, warnings), nil
	}

	// The host records, while Namecheap serves them, are kept too: a switch may not carry them.
	recordsNote, rid := "", ""
	if cur.OurDNS {
		set, err := p.api.getHosts(domain)
		switch {
		case err != nil && cur.FreeDNS:
			recordsNote = fmt.Sprintf("\nNamecheap's API did not answer %s's FreeDNS host records (%s), so no record backup was saved; FreeDNS keeps them in the Namecheap portal.", domain, errorWords(err))
		case err != nil:
			return "", refusal(fmt.Sprintf("Refused: %s's host records could not be read to save them before the nameserver change (%s). Nothing was changed.", domain, errorWords(err)))
		default:
			if rid, err = p.saveBackup(domain, command, set); err != nil {
				return "", err
			}
			recordsNote = fmt.Sprintf("\nRecord backup %s holds its %d host records from before the change; `restore %s %s` puts them back while the domain is on Namecheap DNS.", rid, len(set.Records), domain, rid)
		}
	}
	id, err := p.saveNSBackup(domain, command, cur, rid)
	if err != nil {
		return "", err
	}

	p.progress(fmt.Sprintf("changing %s's nameservers to %s (backup %s)", domain, to.mode(), id))
	if to.basic {
		err = p.api.setDefault(domain)
	} else {
		err = p.api.setCustom(domain, to.servers)
	}
	portal := nsUndo(domain, id, rid, cur.mode())
	if err != nil {
		if portal != "" {
			return "", fmt.Errorf("The nameserver change for %s failed (%s). `nameservers %s` shows what it has now. %s", domain, err.Error(), domain, portal)
		}
		return "", fmt.Errorf("The nameserver change for %s failed (%s). The nameservers before are saved as backup %s; `nameservers %s` shows what it has now and `restore %s %s` puts them back.", domain, err.Error(), id, domain, domain, id)
	}
	got, err := p.api.getNameservers(domain)
	if err != nil {
		if portal != "" {
			return "", fmt.Errorf("%s's nameserver change was sent, but reading it back failed (%s), so it is not known what the domain is on now; check with `nameservers %s`. %s", domain, err.Error(), domain, portal)
		}
		return "", fmt.Errorf("%s's nameserver change was sent, but reading it back failed (%s), so it is not known what the domain is on now. The nameservers before are backup %s; check with `nameservers %s` and put them back with `restore %s %s`.", domain, err.Error(), id, domain, domain, id)
	}
	if !to.holds(*got) {
		if portal != "" {
			return "", fmt.Errorf("%s's nameserver change was sent, but Namecheap now answers %s, not %s. %s", domain, got, to, portal)
		}
		return "", fmt.Errorf("%s's nameserver change was sent, but Namecheap now answers %s, not %s. The nameservers before are backup %s; `restore %s %s` puts them back.", domain, got, to, id, domain, id)
	}
	if portalOnly(cur.mode()) {
		warnings = portalNoteAfter(domain, cur.mode()) + "\n" + warnings
	}

	p.publish("nameservers", map[string]any{
		"domain":     domain,
		"command":    command,
		"action":     "changed",
		"modeBefore": modeWords(cur.mode()),
		"modeAfter":  modeWords(got.mode()),
		"before":     nonNil(cur.Servers),
		"after":      nonNil(got.Servers),
		"backupId":   id,
	})
	undo := fmt.Sprintf("Backup %s holds the nameservers before; `restore %s %s` puts them back in one command.", id, domain, id)
	if portalOnly(cur.mode()) {
		undo = fmt.Sprintf("Backup %s records the nameservers before, but `restore` cannot put the domain back on %s (see below).", id, modeWords(cur.mode()))
	}
	return fmt.Sprintf("%s: nameservers changed\nfrom %s\nto   %s (read back from Namecheap).\n%s%s\n%s",
		domain, cur, got, undo, recordsNote, warnings), nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
