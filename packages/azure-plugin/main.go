// AZURE: a plugin member, protocol harness.member/1, that builds and runs a team's own Azure
// resources, signed in as the person through one Microsoft connection.
//
// In:  ONE JSON request on stdin - { member: { team }, work: [{ instruction }], config,
//
//	connections: { microsoft: { account, accessToken, tokens: { <API>: { accessToken } } },
//	               appSecrets: [{ kind: "apikey"|"login", account, preset, fields: { <name>: <value> } }
//	                          | { kind: "imap", account, username, password, imap: { host, port, security }, smtp: {...} }] },
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
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

type request struct {
	Member struct {
		ID   string `json:"id"`
		Team string `json:"team"`
	} `json:"member"`
	Work             []workItem                 `json:"work"`
	Config           map[string]json.RawMessage `json:"config"`
	Connections      map[string]json.RawMessage `json:"connections"`
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

// slot is the manifest's Microsoft connection slot.
const slot = "microsoft"

// keySlot is the manifest's slot of the connections a deployed app may use: a mailbox, a username and
// password, an API key or a website. It takes several connections ("multiple": true), so stdin carries it as an
// array, one entry per bound connection.
const keySlot = "appSecrets"

// keyConnection is one connection bound to the appSecrets slot: its name and its fields, each one a
// value secret=<ENV>=<connection>.<field> may hand an app. The connection is found by Name, the name a
// person gave it, which the Host sends from CON 15 on just before account; never by Account, which
// two connections of one kind share (two API keys with the preset Other are both "Other"). A
// credential connection (API key, username and password, website) carries its fields by name; a
// mailbox carries username, password and its servers, which keyConnections turns into the fields
// mailboxFields names.
type keyConnection struct {
	Kind     string            `json:"kind"`
	Name     string            `json:"name"`
	Account  string            `json:"account"`
	Preset   string            `json:"preset"`
	Fields   map[string]string `json:"fields"`
	Username string            `json:"username"`
	Password string            `json:"password"`
	Imap     *mailServer       `json:"imap"`
	Smtp     *mailServer       `json:"smtp"`
}

// mailServer is a mailbox's IMAP or SMTP server as stdin carries it.
type mailServer struct {
	Host     string          `json:"host"`
	Port     json.RawMessage `json:"port"`
	Security string          `json:"security"`
}

// mailboxFields are the fields of a mailbox connection, in the order a refusal lists them.
var mailboxFields = []string{"username", "password", "imapHost", "imapPort", "imapSecurity", "smtpHost", "smtpPort", "smtpSecurity"}

// websiteSecretFields are the fields of a website connection whose values are hidden from every
// output: the ones the Host masks, and the username. Its other fields (siteUrl, method, mayAct...)
// are settings whose values are ordinary words such as "password" or "false", which must not be
// blanked out of every sentence.
var websiteSecretFields = map[string]bool{"username": true, "password": true, "totpSecret": true, "token": true, "session": true}

// secretField answers whether a field holds a secret, so its value is hidden from every output: all
// of an API key's or a username and password's fields, a website's websiteSecretFields, and a
// mailbox's username and password (not its servers).
func (k keyConnection) secretField(name string) bool {
	switch k.Kind {
	case "imap":
		return name == "username" || name == "password"
	case "website":
		return websiteSecretFields[name]
	}
	return true
}

// normalise gives a mailbox connection its fields by name.
func (k *keyConnection) normalise() {
	if k.Kind != "imap" {
		return
	}
	f := map[string]string{"username": k.Username, "password": k.Password}
	for prefix, srv := range map[string]*mailServer{"imap": k.Imap, "smtp": k.Smtp} {
		if srv == nil {
			continue
		}
		port := strings.Trim(string(srv.Port), `"`)
		if port == "null" {
			port = ""
		}
		f[prefix+"Host"], f[prefix+"Port"], f[prefix+"Security"] = srv.Host, port, srv.Security
	}
	k.Fields = f
}

// documentsRoot holds every team's documents folder, /data/documents/<team>. Only tests change it.
var documentsRoot = "/data/documents"

// refusal is a sentence the plugin refuses a command with, word for word. A refusal publishes
// plugin.azure-plugin.refused; any other error does not.
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
	work    string   // the member's own working folder, kept between runs
	trees   []string // the run's worktrees
	now     func() time.Time

	// keys holds the connections bound to the appSecrets slot, in binding order. A field's value is
	// only ever put in a container app's configuration.
	keys []keyConnection

	// hidden holds every value that must never be written out: each API's token and each secret
	// field's value of the appSecrets connections.
	hidden []string
	// short holds the hidden values under minHidden characters. Hiding them everywhere would blank
	// common words, so they are hidden as whole words in the text Azure answers, where an echoed
	// value turns up.
	short []string
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
	var c connection
	if raw, ok := req.Connections[slot]; ok && json.Unmarshal(raw, &c) == nil {
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
	p.az = &client{tokens: tokens, progress: p.progress, quoted: p.hideShort}

	if raw, ok := req.Connections[keySlot]; ok {
		p.keys = keyConnections(raw)
		for _, k := range p.keys {
			for f, v := range k.Fields {
				if k.secretField(f) {
					p.hide(v)
				}
			}
		}
	}

	cfg, problem := parseSettings(req.Config)
	if problem != "" {
		p.fail(problem)
		return 1
	}
	p.cfg = cfg
	p.team = req.Member.Team
	p.docs = documentsFolder(req.Member.Team, req.SiteFiles)
	p.work = req.WorkingDirectory
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

	type matched struct {
		cmd  *command
		args []string
		why  error
	}
	found, anyCommand := make([]matched, len(lines)), false
	for i, line := range lines {
		cmd, args, why := match(line)
		found[i] = matched{cmd, args, why}
		anyCommand = anyCommand || cmd != nil
	}

	var texts []string
	failed, listed := false, false
	for i, line := range lines {
		cmd, args, why := found[i].cmd, found[i].args, found[i].why
		if why == errNotCommand {
			why = notCommand(line, i+1, !anyCommand && !listed)
			listed = true
		}
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

// keyConnections reads the appSecrets slot: an array of connections, or one object from a Host that
// sends a single slot's shape; anything else holds none.
func keyConnections(raw json.RawMessage) []keyConnection {
	var many []keyConnection
	if json.Unmarshal(raw, &many) != nil {
		var one keyConnection
		if json.Unmarshal(raw, &one) != nil || (one.Fields == nil && one.Kind != "imap") {
			return nil
		}
		many = []keyConnection{one}
	}
	for i := range many {
		many[i].normalise()
	}
	return many
}

// minHidden is the shortest value hidden wherever it appears; a shorter one is hidden only as a
// whole word in Azure's answers (hideShort).
const minHidden = 4

func (p *plugin) hide(value string) {
	switch {
	case value == "":
	case len(value) < minHidden:
		p.short = append(p.short, value)
	default:
		p.hidden = append(p.hidden, value)
		sort.Slice(p.hidden, func(i, j int) bool { return len(p.hidden[i]) > len(p.hidden[j]) })
	}
}

// hideShort replaces each short hidden value standing as a word of its own - not inside a longer
// run of letters and digits - in text Azure answered.
func (p *plugin) hideShort(text string) string {
	for _, v := range p.short {
		var b strings.Builder
		rest := text
		for {
			i := strings.Index(rest, v)
			if i < 0 {
				b.WriteString(rest)
				break
			}
			before, after := rest[:i], rest[i+len(v):]
			startsWord := !wordRune(firstRune(v)) || !wordRune(lastRune(before))
			endsWord := !wordRune(lastRune(v)) || !wordRune(firstRune(after))
			if startsWord && endsWord {
				b.WriteString(before + "[redacted]")
			} else {
				b.WriteString(before + v)
			}
			rest = after
		}
		text = b.String()
	}
	return text
}

func wordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

func firstRune(s string) rune {
	for _, r := range s {
		return r
	}
	return 0
}

func lastRune(s string) rune {
	r, _ := utf8.DecodeLastRuneInString(s)
	if r == utf8.RuneError {
		return 0
	}
	return r
}

// isHidden answers whether value is one this run must never write out, such as a secret's value
// typed where a name belongs.
func (p *plugin) isHidden(value string) bool {
	for _, h := range p.hidden {
		if h == value {
			return true
		}
	}
	return false
}

// credentialShaped matches what a credential looks like when it was not one of this run's tokens:
// a JWT, and a key, password or signature inside a connection string or a SAS address.
var credentialShaped = []*regexp.Regexp{
	regexp.MustCompile(`eyJ[A-Za-z0-9_-]{6,}\.[A-Za-z0-9_-]{6,}\.[A-Za-z0-9_-]*`),
	regexp.MustCompile(`(?i)(AccountKey|SharedAccessKey|SharedAccessSignature|InstrumentationKey|Password|Pwd|sig)=[^;&"\s\\]+`),
}

// emit writes one record. Every string in it goes through scrub first: no token, key or connection
// string leaves the plugin, whatever text carried it - output, error, progress row or event. Each
// string is scrubbed as its decoded text, not as JSON wrote it, so a value with characters JSON
// escapes (& < > " \) is found as surely as any other.
func (p *plugin) emit(record any) {
	line, _ := json.Marshal(record)
	_, _ = p.out.Write(append(p.scrubJSON(line), '\n'))
}

// jsonString matches one string in encoded JSON: a key or a value.
var jsonString = regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)

// scrubJSON scrubs every string value of an encoded record in its decoded form and encodes it
// again; everything else - keys' order, numbers, layout - is left as it was. The record's shape is
// the plugin's own and is never scrubbed: every object key (no record carries an object Azure
// supplied, so every key is one the plugin wrote) and the record's top-level t and type values. A
// hidden value equal to "result" or "error" so still leaves a well-formed record.
func (p *plugin) scrubJSON(doc []byte) []byte {
	var out []byte
	depth, last := 0, 0
	key := "" // the top-level key whose value comes next, if any
	for _, m := range jsonString.FindAllIndex(doc, -1) {
		between := doc[last:m[0]]
		for _, ch := range between {
			switch ch {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
		}
		out = append(out, between...)
		lit := doc[m[0]:m[1]]
		last = m[1]
		isKey := len(bytes.TrimLeft(doc[m[1]:], " \t\r\n")) > 0 && bytes.TrimLeft(doc[m[1]:], " \t\r\n")[0] == ':'
		ownType := !isKey && depth == 1 && (key == "t" || key == "type") && bytes.Equal(bytes.TrimSpace(between), []byte(":"))
		key = ""
		if isKey {
			if depth == 1 {
				_ = json.Unmarshal(lit, &key)
			}
			out = append(out, lit...)
			continue
		}
		if ownType {
			out = append(out, lit...)
			continue
		}
		out = append(out, p.scrubLiteral(lit)...)
	}
	return append(out, doc[last:]...)
}

// scrubLiteral scrubs one encoded JSON string as its decoded text.
func (p *plugin) scrubLiteral(lit []byte) []byte {
	var s string
	if json.Unmarshal(lit, &s) != nil {
		return []byte(`"[redacted]"`)
	}
	clean := p.scrub(s)
	if clean == s {
		return lit
	}
	out, _ := json.Marshal(clean)
	return out
}

func (p *plugin) scrub(text string) string {
	for _, v := range p.hidden {
		text = replaceFold(text, v, "[redacted]")
	}
	text = credentialShaped[0].ReplaceAllString(text, "[redacted]")
	return credentialShaped[1].ReplaceAllString(text, "$1=[redacted]")
}

// replaceFold replaces every occurrence of old in s, in any case. It lowers both a rune at a time
// and keeps, for each byte of the lowered text, where its rune starts and ends in s, so a rune whose
// lower case has another length (the Kelvin sign lowers to an ASCII k) cannot put a slice out of
// range; a match is widened to whole runes of s.
func replaceFold(s, old, with string) string {
	needle, _, _ := lowerMap(old, false)
	if needle == "" {
		return s
	}
	lower, start, end := lowerMap(s, true)
	if !strings.Contains(lower, needle) {
		return s
	}
	var b strings.Builder
	done := 0 // bytes of s already written
	for i := 0; i <= len(lower)-len(needle); {
		j := strings.Index(lower[i:], needle)
		if j < 0 {
			break
		}
		i += j
		from, to := start[i], end[i+len(needle)-1]
		if from < done {
			from = done
		}
		b.WriteString(s[done:from])
		b.WriteString(with)
		done = to
		i += len(needle)
	}
	b.WriteString(s[done:])
	return b.String()
}

// lowerMap lowers s a rune at a time. With offsets it also answers, for each byte of the result,
// the byte offsets in s where the rune it came from starts and ends. A byte that is not UTF-8 is
// kept as it is.
func lowerMap(s string, offsets bool) (string, []int, []int) {
	var b strings.Builder
	b.Grow(len(s))
	var start, end []int
	if offsets {
		start, end = make([]int, 0, len(s)), make([]int, 0, len(s))
	}
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		n := b.Len()
		if r == utf8.RuneError && size <= 1 {
			b.WriteByte(s[i])
		} else {
			b.WriteRune(unicode.ToLower(r))
		}
		if offsets {
			for k := n; k < b.Len(); k++ {
				start, end = append(start, i), append(end, i+size)
			}
		}
		i += size
	}
	return b.String(), start, end
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

// planned publishes plugin.azure-plugin.planned.
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

// applied publishes plugin.azure-plugin.applied for a change made in apply mode, then a resource event for
// each resource it touched.
func (p *plugin) applied(command, target, summary string, ids []string) {
	p.appliedAs(command, target, summary, ids, func(id string) string { return actionOf(command, id) })
}

// appliedAs is applied with each resource's action given.
func (p *plugin) appliedAs(command, target, summary string, ids []string, action func(id string) string) {
	if ids == nil {
		ids = []string{}
	}
	p.publish("applied", map[string]any{"command": command, "target": target, "summary": summary, "resourceIds": ids})
	p.resourceEvents(command, ids, action)
}

// whatif reports a single-change command in whatif mode: the plan event, and the answer saying
// nothing changed.
func (p *plugin) whatif(command, target, would string, counts map[string]int) string {
	p.planned(plan{command: command, target: target, summary: would, counts: counts})
	return "whatif: would " + would + ". Nothing was changed; a person sets mode to apply in this member's settings to make the change."
}
