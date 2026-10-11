// TWILIO: a plugin member, protocol harness.member/1, that sends SMS and MMS through Twilio, reads
// delivery status and replies by polling, and keeps its state on the team's `twilio` site.
//
// In:  ONE JSON request on stdin - { work: [{ instruction }], config, secrets, sites, ... }.
// Out: JSON Lines on stdout - progress, site.put / site.delete, publish, then exactly one result.
//
// One command per instruction; the first word is the command (commands.go lists them). Real sends
// happen only in mode=send, only to sendAllowlist or a person-activated consented recipient, and
// only after every check of the outward-action rule (send.go). Dry run never POSTs Messages.json.
//
// Self-contained: standard library only, built with CGO_ENABLED=0 (build.sh). The Twilio base URLs
// are package variables only tests change; the shipped binary has no way to point elsewhere.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

type request struct {
	Work    []workItem                 `json:"work"`
	Config  map[string]json.RawMessage `json:"config"`
	Secrets map[string]string          `json:"secrets"`
	Sites   []siteRead                 `json:"sites"`
}

type workItem struct {
	Instruction *string         `json:"instruction"`
	Payload     json.RawMessage `json:"payload"`
}

type siteRead struct {
	Site       string    `json:"site"`
	Collection string    `json:"collection"`
	Documents  []siteDoc `json:"documents"`
	Total      int       `json:"total"`
	Cut        *string   `json:"cut"`
	Missing    *string   `json:"missing"`
}

type siteDoc struct {
	ID        string          `json:"id"`
	Doc       json.RawMessage `json:"doc"`
	UpdatedAt string          `json:"updatedAt"`
	UpdatedBy string          `json:"updatedBy"`
}

// refusal is a sentence the plugin fails with, word for word.
type refusal string

func (r refusal) Error() string { return string(r) }

// outcome is one command's answer: its text, or the error it failed with; quiet when nothing
// happened that anyone needs to be woken for (a poll that found nothing).
type outcome struct {
	text  string
	err   error
	quiet bool
}

type plugin struct {
	out  io.Writer
	cfg  settings
	sec  secrets
	st   *store
	api  *client
	now  func() time.Time
	sent bool // a real send happened in this run

	numbers       []number
	numbersLoaded bool
	acct          *account
}

func main() { os.Exit(run(os.Stdin, os.Stdout, time.Now)) }

func run(in io.Reader, out io.Writer, now func() time.Time) int {
	p := &plugin{out: out, now: now}

	var req request
	raw, err := io.ReadAll(in)
	if err == nil {
		err = json.Unmarshal(raw, &req)
	}
	if err != nil {
		p.fail("The request on stdin could not be read: " + err.Error())
		return 1
	}

	cfg, problem := parseSettings(req.Config)
	if problem != "" {
		p.fail(problem)
		return 1
	}
	p.cfg = cfg
	p.sec = secrets{
		AccountSid:   strings.TrimSpace(req.Secrets["accountSid"]),
		APIKeySid:    strings.TrimSpace(req.Secrets["apiKeySid"]),
		APIKeySecret: strings.TrimSpace(req.Secrets["apiKeySecret"]),
	}
	p.api = &client{account: p.sec.AccountSid, keySid: p.sec.APIKeySid, keySecret: p.sec.APIKeySecret}
	p.st = loadStore(req.Sites)

	// SEND MODE NEEDS AN ALLOWLIST, whatever the command.
	if p.cfg.Mode == "send" && len(p.cfg.SendAllowlist) == 0 {
		p.fail("Refused: mode is send but sendAllowlist is empty; a person adds at least their own cell to sendAllowlist in this member's settings. Nothing was sent.")
		return 1
	}

	if len(req.Work) == 0 {
		p.fail(unknownCommand(""))
		return 1
	}

	p.progress(fmt.Sprintf("twilio: %d command(s), mode %s", len(req.Work), p.cfg.Mode))

	var texts, errs []string
	quiet := true
	for i, item := range req.Work {
		text := instructionText(item)
		o := p.dispatch(text)
		prefix := ""
		if len(req.Work) > 1 {
			prefix = fmt.Sprintf("%d. ", i+1)
		}
		if o.err != nil {
			errs = append(errs, prefix+o.err.Error())
			texts = append(texts, prefix+o.err.Error())
			quiet = false
			continue
		}
		if !o.quiet {
			quiet = false
		}
		texts = append(texts, prefix+o.text)
	}

	p.st.flush(p)

	if len(errs) > 0 {
		if len(req.Work) > 1 {
			p.fail(strings.Join(texts, "\n\n"))
		} else {
			p.fail(errs[0])
		}
		return 1
	}
	p.emit(map[string]any{"t": "result", "ok": true, "output": strings.Join(texts, "\n\n"), "quiet": quiet})
	return 0
}

func instructionText(item workItem) string {
	if item.Instruction != nil {
		return *item.Instruction
	}
	var payload struct {
		Instruction string `json:"instruction"`
	}
	if len(item.Payload) > 0 && json.Unmarshal(item.Payload, &payload) == nil {
		return payload.Instruction
	}
	return ""
}

func (p *plugin) emit(record any) {
	line, _ := json.Marshal(record)
	_, _ = p.out.Write(append(line, '\n'))
}

func (p *plugin) progress(status string) {
	p.emit(map[string]any{"t": "progress", "status": status})
}

func (p *plugin) fail(sentence string) {
	p.emit(map[string]any{"t": "result", "ok": false, "error": sentence})
}

func (p *plugin) publish(suffix string, payload map[string]any) {
	payload["dryRun"] = false
	p.emit(map[string]any{"t": "publish", "type": suffix, "payload": payload})
}

func (p *plugin) sitePut(collection, id string, doc any) {
	p.emit(map[string]any{"t": "site.put", "site": siteName, "collection": collection, "id": id, "doc": doc})
}

func (p *plugin) siteDelete(collection, id string) {
	p.emit(map[string]any{"t": "site.delete", "site": siteName, "collection": collection, "id": id})
}
