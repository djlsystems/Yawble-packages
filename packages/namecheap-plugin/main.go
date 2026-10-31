// NAMECHEAP: a plugin member, protocol harness.member/1, that adds and changes DNS records for
// domains registered at Namecheap that use Namecheap's DNS.
//
// In:  ONE JSON request on stdin - { work: [{ instruction }], config,
//
//	connections: { namecheap: { kind, account, preset, fields: { apiUser, apiKey, clientIp } } },
//	workingDirectory, ... }.
//
// Out: JSON Lines on stdout - progress, publish, then exactly one result.
//
// One command per instruction; the first word is the command (commands.go lists them). Namecheap's
// setHosts replaces a domain's whole record set, so every change reads the set first, changes only
// the named record, saves the set before the change as a backup in the member's workspace, writes
// the whole set back and reads it again to check nothing was lost (change.go). Dry run, the default,
// answers the set before and after and changes nothing.
//
// Self-contained: standard library only, built with CGO_ENABLED=0 (build.sh). The Namecheap and
// address-service URLs are package variables only tests change; the shipped binary has no way to
// point elsewhere.
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
	Work             []workItem                 `json:"work"`
	Config           map[string]json.RawMessage `json:"config"`
	Connections      map[string]connection      `json:"connections"`
	WorkingDirectory string                     `json:"workingDirectory"`
}

type workItem struct {
	Instruction *string         `json:"instruction"`
	Payload     json.RawMessage `json:"payload"`
}

// connection is a bound slot on stdin: for the namecheap slot an API-key connection, its fields
// named as the Namecheap preset names them.
type connection struct {
	Kind   string            `json:"kind"`
	Preset string            `json:"preset"`
	Fields map[string]string `json:"fields"`
}

// slot is the manifest's one connection slot: the Namecheap account's API user and key, and the
// address its key is whitelisted for. The credential is set in Team settings, Connections, never
// in this member's settings.
const slot = "namecheap"

// refusal is a sentence the plugin fails with, word for word.
type refusal string

func (r refusal) Error() string { return string(r) }

// clock is what a run needs from time: tests pass a fake one that never really sleeps.
type clock struct {
	now   func() time.Time
	sleep func(time.Duration)
}

type plugin struct {
	out  io.Writer
	cfg  settings
	sec  secrets
	api  *client
	dir  string // the member's workspace: backups and the call log live here
	time clock
	// waited is how long this run has waited for name servers so far (live.go), up to liveBudget.
	waited time.Duration
}

func main() {
	os.Exit(run(os.Stdin, os.Stdout, clock{now: time.Now, sleep: time.Sleep}))
}

func run(in io.Reader, out io.Writer, c clock) int {
	p := &plugin{out: out, time: c}

	var req request
	raw, err := io.ReadAll(in)
	if err == nil {
		err = json.Unmarshal(raw, &req)
	}
	if err != nil {
		p.fail("The request on stdin could not be read.")
		return 1
	}

	if c, ok := req.Connections[slot]; ok {
		p.sec = secrets{
			APIUser:  strings.TrimSpace(c.Fields["apiUser"]),
			APIKey:   strings.TrimSpace(c.Fields["apiKey"]),
			ClientIP: strings.TrimSpace(c.Fields["clientIp"]),
		}
	}
	cfg, problem := parseSettings(req.Config)
	if problem != "" {
		p.fail(problem)
		return 1
	}
	p.cfg = cfg
	p.dir = req.WorkingDirectory
	if p.dir == "" {
		p.dir, _ = os.Getwd()
	}
	p.api = newClient(p)

	if len(req.Work) == 0 {
		p.fail(unknownCommand(""))
		return 1
	}
	p.progress(fmt.Sprintf("namecheap: %d command(s), mode %s", len(req.Work), p.cfg.Mode))

	var texts []string
	failed := false
	for i, item := range req.Work {
		prefix := ""
		if len(req.Work) > 1 {
			prefix = fmt.Sprintf("%d. ", i+1)
		}
		text, err := p.dispatch(instructionText(item))
		if err != nil {
			failed = true
			text = err.Error()
		}
		texts = append(texts, prefix+text)
	}

	answer := strings.Join(texts, "\n\n")
	if failed {
		p.fail(answer)
		return 1
	}
	p.emit(map[string]any{"t": "result", "ok": true, "output": p.scrub(answer)})
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

// scrub takes the API key out of anything the plugin writes. The plugin never puts it in a
// sentence; this is the second lock, should Namecheap ever echo it back.
func (p *plugin) scrub(s string) string {
	if len(p.sec.APIKey) >= 4 {
		s = strings.ReplaceAll(s, p.sec.APIKey, "[apiKey]")
	}
	return s
}

func (p *plugin) emit(record any) {
	line, _ := json.Marshal(record)
	_, _ = p.out.Write(append([]byte(p.scrub(string(line))), '\n'))
}

func (p *plugin) progress(status string) {
	p.emit(map[string]any{"t": "progress", "status": status})
}

func (p *plugin) fail(sentence string) {
	p.emit(map[string]any{"t": "result", "ok": false, "error": sentence})
}

func (p *plugin) publish(suffix string, payload map[string]any) {
	p.emit(map[string]any{"t": "publish", "type": suffix, "payload": payload})
}
