package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Backups live in the member's workspace, one file per change: namecheap-backups/<domain>/<id>.json.
// A backup holds the domain's record set before the change and nothing else: no key, no user. A
// nameserver backup (kind "nameservers") holds instead the delegation before a nameserver change:
// its mode and nameservers. restore tells them apart by kind.
const backupFolder = "namecheap-backups"

var backupIDPattern = regexp.MustCompile(`^\d{8}T\d{6}Z(-\d{1,3})?$`)

type backup struct {
	ID        string   `json:"id"`
	Domain    string   `json:"domain"`
	TakenAt   string   `json:"takenAt"`
	Command   string   `json:"command"`
	EmailType string   `json:"emailType"`
	Records   []record `json:"records"`
	// Kind is "nameservers" for a nameserver backup, empty for a record backup.
	Kind        string   `json:"kind,omitempty"`
	NSMode      string   `json:"nameserverMode,omitempty"` // basicdns, premiumdns, freedns, namecheap or custom
	Nameservers []string `json:"nameservers,omitempty"`
	// RecordBackup is the record backup taken with a nameserver backup, when there was one.
	RecordBackup string `json:"recordBackup,omitempty"`
}

const kindNameservers = "nameservers"

func (p *plugin) backupDir(domain string) string {
	return filepath.Join(p.dir, backupFolder, domain)
}

// saveBackup writes the set before a change and answers its id: the time it was taken (UTC), with
// -2, -3 ... when one was already taken that second.
func (p *plugin) saveBackup(domain, command string, set *hostSet) (string, error) {
	return p.writeBackup(domain, backup{Command: command, EmailType: set.EmailType, Records: set.Records})
}

// saveNSBackup writes a domain's delegation before a nameserver change and answers its id.
func (p *plugin) saveNSBackup(domain, command string, s *nsState, recordBackup string) (string, error) {
	return p.writeBackup(domain, backup{Command: command, Records: []record{}, Kind: kindNameservers, NSMode: s.mode(), Nameservers: nonNil(s.Servers), RecordBackup: recordBackup})
}

// writeBackup writes b, given its id, domain and time here.
func (p *plugin) writeBackup(domain string, b backup) (string, error) {
	dir := p.backupDir(domain)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", refusal("Refused: the backup could not be saved in this member's workspace, so nothing was changed.")
	}
	now := p.time.now().UTC()
	base := now.Format("20060102T150405Z")
	for n := 1; n < 1000; n++ {
		id := base
		if n > 1 {
			id = fmt.Sprintf("%s-%d", base, n)
		}
		path := filepath.Join(dir, id+".json")
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if os.IsExist(err) {
			continue
		}
		if err != nil {
			break
		}
		b.ID, b.Domain, b.TakenAt = id, domain, now.Format(time.RFC3339)
		raw, _ := json.MarshalIndent(b, "", "  ")
		_, werr := f.Write(append(raw, '\n'))
		cerr := f.Close()
		if werr != nil || cerr != nil {
			_ = os.Remove(path)
			break
		}
		return id, nil
	}
	return "", refusal("Refused: the backup could not be saved in this member's workspace, so nothing was changed.")
}

func (p *plugin) loadBackup(domain, id string) (*backup, error) {
	if !backupIDPattern.MatchString(id) {
		return nil, refusal(fmt.Sprintf("Refused: %q is not a backup id; a backup id looks like 20260101T120000Z, as a change answers it. Nothing was changed.", id))
	}
	raw, err := os.ReadFile(filepath.Join(p.backupDir(domain), id+".json"))
	if err != nil {
		return nil, refusal(fmt.Sprintf("Refused: there is no backup %s for %s in this member's workspace. Nothing was changed.", id, domain))
	}
	var b backup
	if json.Unmarshal(raw, &b) != nil || b.Domain != domain {
		return nil, refusal(fmt.Sprintf("Refused: backup %s for %s could not be read. Nothing was changed.", id, domain))
	}
	return &b, nil
}

// recentBackups names the newest backups of a domain of one kind ("" for records), newest first.
func (p *plugin) recentBackups(domain string, max int, kind string) []string {
	entries, _ := os.ReadDir(p.backupDir(domain))
	var ids []string
	for _, e := range entries {
		id := strings.TrimSuffix(e.Name(), ".json")
		if backupIDPattern.MatchString(id) && p.backupKind(domain, id) == kind {
			ids = append(ids, id)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))
	if len(ids) > max {
		ids = ids[:max]
	}
	return ids
}

func (p *plugin) backupKind(domain, id string) string {
	raw, err := os.ReadFile(filepath.Join(p.backupDir(domain), id+".json"))
	if err != nil {
		return ""
	}
	var b struct {
		Kind string `json:"kind"`
	}
	_ = json.Unmarshal(raw, &b)
	return b.Kind
}
