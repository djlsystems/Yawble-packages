package main

import (
	"errors"
	"fmt"
	"strings"
)

// THE ONE HARD RULE. Namecheap's setHosts replaces a domain's whole record set, so every change
// goes through here:
//
//  1. the domain is in `domains`, or the command is refused;
//  2. the current set is read, and the command is refused when the read fails or the domain does
//     not use Namecheap DNS;
//  3. the command plans its change from that set, touching only the named record;
//  4. dry run answers the set before and after and stops;
//  5. the set before is saved as a backup in the member's workspace;
//  6. the whole set after is written, read back, and every record it holds - every record that was
//     there and is not the one changed - must be in what Namecheap answers;
//  7. the domain's own name servers are asked whether they serve the change (live.go).

// readSet reads a listed domain's set, refusing as the rule says.
func (p *plugin) readSet(domain string) (*hostSet, error) {
	if !p.cfg.listed(domain) {
		return nil, notListed(domain)
	}
	if p.sec.APIUser == "" || p.sec.APIKey == "" {
		return nil, refusal("Refused: no Namecheap API user and API key are bound: " + noKeys + ", then runs setup. Nothing was changed.")
	}
	set, err := p.api.getHosts(domain)
	if err != nil {
		var r refusal
		if errors.As(err, &r) {
			return nil, refusal(string(r) + " Nothing was changed.")
		}
		return nil, refusal(fmt.Sprintf("Refused: %s's records could not be read (%s), so nothing was changed. Run setup to see what is missing.", domain, err.Error()))
	}
	if !set.OurDNS {
		return nil, refusal(fmt.Sprintf("Refused: %s does not use Namecheap DNS, so its records are not Namecheap's to change. Nothing was changed.", domain))
	}
	return set, nil
}

// managedMail names a mail setting in which Namecheap itself serves the domain's mail records
// (MX, and SPF with them), so none of them is in the host records this plugin reads and writes.
// Any other setting (none, MX, MXE) is the host records' own.
func managedMail(emailType string) (string, bool) {
	switch strings.ToUpper(emailType) {
	case "OX":
		return "Namecheap Private Email", true
	case "FWD":
		return "Namecheap email forwarding", true
	case "GMAIL":
		return "Google Workspace through Namecheap", true
	}
	return "", false
}

// mailSwitch refuses a change that would move a domain off a mail setting Namecheap manages: an MX
// record added to a Private Email or forwarding domain switches it to custom MX, which stops the
// mail records Namecheap serves for it. restore is exempt: it puts back the setting a backup was
// saved with, on purpose.
func mailSwitch(command, domain, from, to string) error {
	name, managed := managedMail(from)
	if command == "restore" || !managed || strings.EqualFold(from, to) {
		return nil
	}
	return refusal(fmt.Sprintf("Refused: %s's mail setting is %s (%s), so Namecheap serves its mail records itself. This change would switch it to %s and stop them, so it is not made. A person who means to move the domain's mail changes its mail setting in Namecheap (Domain List, Manage, Advanced DNS, Mail Settings) first. Nothing was changed.", domain, name, from, orNone(to)))
}

func notListed(domain string) error {
	return refusal(fmt.Sprintf("Refused: %s is not in this member's domains setting, so it may not read or change it. A person adds it to domains to allow it. Nothing was changed.", domain))
}

// apply carries out a planned change, or describes it in dry run.
// mailBase is the mail setting the set after builds on: what the domain has, or for a restore
// what the backup was saved with.
func (p *plugin) apply(command, domain string, set *hostSet, c change, mailBase string) (string, error) {
	newEmail := emailType(mailBase, c.after)
	if c.none() {
		return fmt.Sprintf("%s: already as asked; nothing was changed (%d records).", domain, len(c.before)), nil
	}
	if err := mailSwitch(command, domain, set.EmailType, newEmail); err != nil {
		return "", err
	}

	summary := summarise(c)
	mail := ""
	if !strings.EqualFold(newEmail, set.EmailType) {
		mail = fmt.Sprintf("\nMail setting (EmailType): %s -> %s.", orNone(set.EmailType), newEmail)
	}

	if p.cfg.Mode != "apply" {
		return fmt.Sprintf("Dry run for %s: nothing was changed. %s%s\nBefore (%d records):\n%s\nAfter (%d records):\n%s\nTo make the change, a person sets this member's mode to apply; then send the same command again.",
			domain, summary, mail, len(c.before), lines(c.before), len(c.after), lines(c.after)), nil
	}

	id, err := p.saveBackup(domain, command, set)
	if err != nil {
		return "", err
	}
	p.progress(fmt.Sprintf("writing %d records for %s (backup %s)", len(c.after), domain, id))
	if err := p.api.setHosts(domain, newEmail, c.after); err != nil {
		return "", fmt.Errorf("The write to %s failed (%s). The set before is saved as backup %s; check it with `list %s` and, if anything is lost, `restore %s %s`.", domain, err.Error(), id, domain, domain, id)
	}
	got, err := p.api.getHosts(domain)
	if err != nil {
		return "", fmt.Errorf("%s was written, but reading it back failed (%s), so it is not known whether every record is still there. The set before is backup %s; check with `list %s` and restore with `restore %s %s` if anything is lost.", domain, err.Error(), id, domain, domain, id)
	}
	if lost := missing(c.after, got.Records); len(lost) > 0 {
		return "", fmt.Errorf("%s was written, but reading it back shows %d record(s) missing:\n%s\nThe set before is backup %s; `restore %s %s` puts it back.", domain, len(lost), lines(lost), id, domain, id)
	}

	lives, liveText := p.confirm(domain, c)
	allLive := true
	for _, l := range lives {
		allLive = allLive && l
	}

	changed := []string{}
	for _, pair := range c.changed {
		changed = append(changed, pair[0].String()+" -> "+pair[1].String())
	}
	p.publish("changed", map[string]any{
		"domain":   domain,
		"command":  command,
		"added":    strs(c.added),
		"changed":  changed,
		"removed":  strs(c.removed),
		"backupId": id,
		"live":     allLive,
	})
	p.publishRecords(domain, c, id, lives)
	return fmt.Sprintf("%s: %s%s\nBackup %s holds the set before; `restore %s %s` puts it back.\nNow (%d records, every earlier record checked present):\n%s\n%s",
		domain, summary, mail, id, domain, id, len(got.Records), lines(got.Records), liveText), nil
}

// recordEventCap is the most record events one command publishes; past it, changed covers the rest.
const recordEventCap = 25

// publishRecords publishes one record event per record a written and checked change added,
// changed or removed, up to recordEventCap. value is the record's public DNS value as written;
// live is whether the name servers serve the change (lives is in the same order: added, changed,
// removed).
func (p *plugin) publishRecords(domain string, c change, backupID string, lives []bool) {
	type one struct {
		r      record
		action string
		live   bool
	}
	var all []one
	for _, r := range c.added {
		all = append(all, one{r, "added", false})
	}
	for _, pair := range c.changed {
		all = append(all, one{pair[1], "changed", false})
	}
	for _, r := range c.removed {
		all = append(all, one{r, "removed", false})
	}
	for i := range all {
		all[i].live = i < len(lives) && lives[i]
	}
	if len(all) > recordEventCap {
		all = all[:recordEventCap]
	}
	for _, o := range all {
		p.publish("record", map[string]any{
			"domain":     domain,
			"recordType": o.r.Type,
			"host":       o.r.Host,
			"value":      o.r.Value,
			"action":     o.action,
			"backupId":   backupID,
			"live":       o.live,
		})
	}
}

func summarise(c change) string {
	var parts []string
	for _, r := range c.added {
		parts = append(parts, "adds "+r.String())
	}
	for _, pair := range c.changed {
		parts = append(parts, "changes "+pair[0].String()+" to "+pair[1].String())
	}
	for _, r := range c.removed {
		parts = append(parts, "removes "+r.String())
	}
	return strings.Join(parts, "; ") + "."
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}
