// AZURE: a plugin member, protocol harness.member/1, that builds and runs a team's own Azure
// resources, signed in as the person through one Microsoft connection.
//
// In:  ONE JSON request on stdin - { member: { team }, work: [{ instruction }], config,
//
//	connections: { microsoft: { account, accessToken, tokens: { <API>: { accessToken } } } },
//	workingDirectory, worktrees: [{ path }], siteFiles: [{ folder }], ... }.
//
// Out: JSON Lines on stdout - progress, publish, then exactly one result.
//
// One command per line of an instruction; the first words are the command (commands.go holds the
// dispatcher, each command file registers its own). Changes happen only in mode=apply, and only to
// what the team owns (guard.go); whatif answers what it would do and changes nothing.
//
// Self-contained: standard library only, built with CGO_ENABLED=0 (build.sh), next to a pinned
// bicep binary. The Azure addresses are reached through endpoint (client.go), which only tests
// change; the shipped binary has no way to point elsewhere.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

type request struct {
	Member struct {
		ID   string `json:"id"`
		Team string `json:"team"`
	} `json:"member"`
	Work             []workItem                 `json:"work"`
	Config           map[string]json.RawMessage `json:"config"`
	Connections      map[string]connection      `json:"connections"`
	WorkingDirectory string                     `json:"workingDirectory"`
	Worktrees        []struct {
		Path string `json:"path"`
	} `json:"worktrees"`
	SiteFiles []struct {
		Folder string `json:"folder"`
	} `json:"siteFiles"`
}

type workItem struct {
	Instruction *string         `json:"instruction"`
	Payload     json.RawMessage `json:"payload"`
}

// connection is a Microsoft slot as stdin carries it: the first API's token in accessToken, and
// one token per API the slot's scopes name in tokens, keyed by the API's address.
type connection struct {
	Provider    string              `json:"provider"`
	Account     string              `json:"account"`
	AccessToken string              `json:"accessToken"`
	Tokens      map[string]apiToken `json:"tokens"`
}

type apiToken struct {
	AccessToken string `json:"accessToken"`
}

// slot is the manifest's one connection slot.
const slot = "microsoft"

// documentsRoot holds every team's documents folder, /data/documents/<team>. Only tests change it.
var documentsRoot = "/data/documents"

// refusal is a sentence the plugin refuses a command with, word for word. A refusal publishes
// plugin.azure.refused; any other error does not.
type refusal string

func (r refusal) Error() string { return string(r) }

func refusef(format string, args ...any) refusal { return refusal(fmt.Sprintf(format, args...)) }

type plugin struct {
	out     io.Writer
	cfg     settings
	team    string // the team id every created resource is tagged with
	account string // the signed-in account's name, as the connection names it
	az      *client
	docs    string   // the team's documents folder
	trees   []string // the run's worktrees
	now     func() time.Time

	// hidden holds every value that must never be written out: each API's token.
	hidden []string
	// tags caches the tags read this run, by lower-case resource id; nil for a resource not found.
	tags map[string]map[string]string
}

func main() { os.Exit(run(os.Stdin, os.Stdout, time.Now)) }

func run(in io.Reader, out io.Writer, now func() time.Time) int {
	p := &plugin{out: out, now: now, tags: map[string]map[string]string{}}

	var req request
	raw, err := io.ReadAll(in)
	if err == nil {
		err = json.Unmarshal(raw, &req)
	}
	if err != nil {
		p.fail("The request on stdin could not be read.")
		return 1
	}

	tokens := map[api]string{}
	if c, ok := req.Connections[slot]; ok {
		p.account = c.Account
		for k, t := range c.Tokens {
			tokens[api(strings.ToLower(strings.TrimSuffix(k, "/"))+"/")] = t.AccessToken
			p.hide(t.AccessToken)
		}
		// A connection with one API carries only its own token: Azure management's.
		if _, ok := tokens[apiARM]; !ok && c.AccessToken != "" && len(c.Tokens) == 0 {
			tokens[apiARM] = c.AccessToken
		}
		p.hide(c.AccessToken)
	}
	p.az = &client{tokens: tokens, progress: p.progress}

	cfg, problem := parseSettings(req.Config)
	if problem != "" {
		p.fail(problem)
		return 1
	}
	p.cfg = cfg
	p.team = req.Member.Team
	p.docs = documentsFolder(req.Member.Team, req.SiteFiles)
	for _, t := range req.Worktrees {
		if t.Path != "" {
			p.trees = append(p.trees, t.Path)
		}
	}

	var lines []string
	for _, item := range req.Work {
		for _, line := range strings.Split(instructionText(item), "\n") {
			if line = strings.TrimSpace(line); line != "" {
				lines = append(lines, line)
			}
		}
	}
	if len(lines) == 0 {
		p.fail(usage(""))
		return 1
	}

	var texts []string
	failed := false
	for i, line := range lines {
		cmd, args, why := match(line)
		name := commandName(line)
		if cmd != nil {
			name = strings.Join(cmd.words, " ")
		}
		p.progress(fmt.Sprintf("azure: %s, mode %s", name, p.cfg.Mode))
		var answer string
		if cmd == nil {
			err = why
		} else {
			answer, err = cmd.run(p, args)
		}
		prefix := ""
		if len(lines) > 1 {
			prefix = fmt.Sprintf("%d. ", i+1)
		}
		if err != nil {
			failed = true
			if r, ok := err.(refusal); ok {
				p.refuse(name, string(r))
			}
			texts = append(texts, prefix+err.Error())
			continue
		}
		texts = append(texts, prefix+answer)
	}

	if failed {
		p.fail(strings.Join(texts, "\n\n"))
		return 1
	}
	p.emit(map[string]any{"t": "result", "ok": true, "output": strings.Join(texts, "\n\n")})
	return 0
}

// documentsFolder is the team's documents folder: the folder above a site's files folder when the
// run names one, /data/documents/<team> otherwise.
func documentsFolder(team string, sites []struct {
	Folder string `json:"folder"`
}) string {
	for _, s := range sites {
		dir := filepath.Clean(s.Folder) // <documents>/sites/<site>/files
		if filepath.IsAbs(dir) && filepath.Base(dir) == "files" && filepath.Base(filepath.Dir(filepath.Dir(dir))) == "sites" {
			return filepath.Dir(filepath.Dir(filepath.Dir(dir)))
		}
	}
	if team == "" {
		return ""
	}
	return filepath.Join(documentsRoot, team)
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

func (p *plugin) hide(value string) {
	if len(value) >= 4 {
		p.hidden = append(p.hidden, value)
		sort.Slice(p.hidden, func(i, j int) bool { return len(p.hidden[i]) > len(p.hidden[j]) })
	}
}

// credentialShaped matches what a credential looks like when it was not one of this run's tokens:
// a JWT, and a key, password or signature inside a connection string or a SAS address.
var credentialShaped = []*regexp.Regexp{
	regexp.MustCompile(`eyJ[A-Za-z0-9_-]{6,}\.[A-Za-z0-9_-]{6,}\.[A-Za-z0-9_-]*`),
	regexp.MustCompile(`(?i)\b(AccountKey|SharedAccessKey|SharedAccessSignature|InstrumentationKey|Password|Pwd|sig)=[^;&"\s\\]+`),
}

// emit writes one record. Every record goes through scrub first: no token, key or connection string
// leaves the plugin, whatever text carried it - output, error, progress row or event.
func (p *plugin) emit(record any) {
	line, _ := json.Marshal(record)
	_, _ = p.out.Write(append([]byte(p.scrub(string(line))), '\n'))
}

func (p *plugin) scrub(text string) string {
	for _, v := range p.hidden {
		text = replaceFold(text, v, "[redacted]")
	}
	text = credentialShaped[0].ReplaceAllString(text, "[redacted]")
	return credentialShaped[1].ReplaceAllString(text, "$1=[redacted]")
}

// replaceFold replaces every occurrence of old in s, in any case.
func replaceFold(s, old, with string) string {
	lower, needle := strings.ToLower(s), strings.ToLower(old)
	if len(lower) != len(s) || !strings.Contains(lower, needle) {
		return strings.ReplaceAll(s, old, with)
	}
	var b strings.Builder
	for {
		i := strings.Index(lower, needle)
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:i])
		b.WriteString(with)
		s, lower = s[i+len(old):], lower[i+len(old):]
	}
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

func (p *plugin) refuse(command, reason string) {
	p.publish("refused", map[string]any{"command": command, "reason": reason})
}

// plan is what a changing command would do, or what a deployment's what-if found.
type plan struct {
	command  string
	target   string
	summary  string
	counts   map[string]int // create, modify, delete, unchanged
	refusals []string
}

// planned publishes plugin.azure.planned.
func (p *plugin) planned(pl plan) {
	if pl.counts == nil {
		pl.counts = map[string]int{}
	}
	refusals := pl.refusals
	if refusals == nil {
		refusals = []string{}
	}
	p.publish("planned", map[string]any{
		"command": pl.command, "target": pl.target, "summary": pl.summary, "mode": p.cfg.Mode,
		"create": pl.counts["create"], "modify": pl.counts["modify"],
		"delete": pl.counts["delete"], "unchanged": pl.counts["unchanged"],
		"refusals": refusals,
	})
}

// applied publishes plugin.azure.applied for a change made in apply mode.
func (p *plugin) applied(command, target, summary string, ids []string) {
	if ids == nil {
		ids = []string{}
	}
	p.publish("applied", map[string]any{"command": command, "target": target, "summary": summary, "resourceIds": ids})
}

// whatif reports a single-change command in whatif mode: the plan event, and the answer saying
// nothing changed.
func (p *plugin) whatif(command, target, would string, counts map[string]int) string {
	p.planned(plan{command: command, target: target, summary: would, counts: counts})
	return "whatif: would " + would + ". Nothing was changed; a person sets mode to apply in this member's settings to make the change."
}
