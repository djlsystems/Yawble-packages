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
// A backup holds the domain's record set before the change and nothing else: no key, no user.
const backupFolder = "namecheap-backups"

var backupIDPattern = regexp.MustCompile(`^\d{8}T\d{6}Z(-\d{1,3})?$`)

type backup struct {
	ID        string   `json:"id"`
	Domain    string   `json:"domain"`
	TakenAt   string   `json:"takenAt"`
	Command   string   `json:"command"`
	EmailType string   `json:"emailType"`
	Records   []record `json:"records"`
}

func (p *plugin) backupDir(domain string) string {
	return filepath.Join(p.dir, backupFolder, domain)
}

// saveBackup writes the set before a change and answers its id: the time it was taken (UTC), with
// -2, -3 ... when one was already taken that second.
func (p *plugin) saveBackup(domain, command string, set *hostSet) (string, error) {
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
		b := backup{ID: id, Domain: domain, TakenAt: now.Format(time.RFC3339), Command: command, EmailType: set.EmailType, Records: set.Records}
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

// recentBackups names the newest backups of a domain, newest first.
func (p *plugin) recentBackups(domain string, max int) []string {
	entries, _ := os.ReadDir(p.backupDir(domain))
	var ids []string
	for _, e := range entries {
		id := strings.TrimSuffix(e.Name(), ".json")
		if backupIDPattern.MatchString(id) {
			ids = append(ids, id)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))
	if len(ids) > max {
		ids = ids[:max]
	}
	return ids
}
