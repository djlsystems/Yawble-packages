// MAIL: list, search, read, mark, move, label, draft and send mail, and watch a folder, over a
// mailbox the Host connects - IMAP and SMTP with an app password, Microsoft Graph or the Gmail
// API - protocol harness.member/1.
//
// In:  ONE JSON request on stdin - { work: [{ instruction }], config: { ... }, connections:
//
//	{ mailbox: { kind: "imap", account, username, password, imap, smtp } or { provider,
//	accessToken, scopes, ... } }, sites: [ the watch's collection ], ... }.
//
// Out: JSON Lines on stdout - progress, then any publish and site.put records, then exactly one
// result.
//
// The password and the access token go only to the mail server: the IMAP and SMTP sign-in, or
// the Authorization header. They are never written to stdout or stderr: every string this plugin
// emits passes through redact first, so even a server answer or an email that echoes one is
// printed without it.
//
// mode, sendAllowlist, markRead and moveTo are a person's settings. Nothing in an instruction or
// in mail content changes them; the plugin reads them from config only.
package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"strconv"
	"strings"
)

const (
	slot = "mailbox"

	untrustedOpen  = "<<<untrusted mail content"
	untrustedClose = "<<<end of untrusted mail content>>>"

	wordsNoMailbox  = "No mailbox is connected: a person binds a connection (an IMAP app password, Microsoft or Google) to the mailbox slot in this member's settings."
	wordsEmptyAllow = "Mail is set to send but sendAllowlist is empty; a person must add the addresses or domains it may send to in this member's settings."
)

type connection struct {
	Kind        string   `json:"kind"`
	Provider    string   `json:"provider"`
	Account     string   `json:"account"`
	AccessToken string   `json:"accessToken"`
	Scopes      []string `json:"scopes"`
	Username    string   `json:"username"`
	Password    string   `json:"password"`
	IMAP        endpoint `json:"imap"`
	SMTP        endpoint `json:"smtp"`
}

type request struct {
	Work []struct {
		Instruction string `json:"instruction"`
		Payload     struct {
			Instruction string `json:"instruction"`
		} `json:"payload"`
	} `json:"work"`
	Config struct {
		Mode              string   `json:"mode"`
		Format            string   `json:"format"`
		SendAllowlist     []string `json:"sendAllowlist"`
		MarkRead          *bool    `json:"markRead"`
		MoveTo            []string `json:"moveTo"`
		MaxResults        *float64 `json:"maxResults"`
		MaxBodyChars      *float64 `json:"maxBodyChars"`
		WatchFolder       string   `json:"watchFolder"`
		WatchUnreadOnly   *bool    `json:"watchUnreadOnly"`
		WatchSenders      []string `json:"watchSenders"`
		WatchQuery        string   `json:"watchQuery"`
		WatchLookbackDays *float64 `json:"watchLookbackDays"`
	} `json:"config"`
	Connections map[string]connection `json:"connections"`
	Sites       []siteDocs            `json:"sites"`
}

// watchQuery is the watch's optional query: the portable terms from: and subject:, and words
// any of which must appear in the sender, subject or snippet.
type watchQuery struct {
	From, Subject string
	words         []string
}

// settings are the person's settings, read from config and held within the manifest's bounds.
type settings struct {
	mode              string
	format            string // "html" (the default): plain text and an HTML part made from it; "text": plain text only
	allowlist         []string
	markRead          bool
	moveTo            []string
	maxResults        int
	maxBodyChars      int
	watchFolder       string
	watchUnreadOnly   bool
	watchSenders      []string
	watchQuery        *watchQuery
	watchLookbackDays int
	account           string
}

func readSettings(req request) settings {
	c := req.Config
	s := settings{mode: strings.ToLower(strings.TrimSpace(c.Mode)), maxResults: 20, maxBodyChars: 20000,
		watchFolder: strings.TrimSpace(c.WatchFolder), watchUnreadOnly: true}
	if s.mode == "" {
		s.mode = "draft"
	}
	s.format = "html"
	if strings.EqualFold(strings.TrimSpace(c.Format), "text") {
		s.format = "text"
	}
	for _, e := range c.SendAllowlist {
		if e = strings.ToLower(strings.TrimSpace(e)); e != "" {
			s.allowlist = append(s.allowlist, e)
		}
	}
	s.markRead = c.MarkRead != nil && *c.MarkRead
	for _, f := range c.MoveTo {
		if f = strings.TrimSpace(f); f != "" {
			s.moveTo = append(s.moveTo, f)
		}
	}
	if v := c.MaxResults; v != nil {
		s.maxResults = clamp(int(*v), 1, 100)
	}
	if v := c.MaxBodyChars; v != nil {
		s.maxBodyChars = clamp(int(*v), 1000, 200000)
	}
	if s.watchFolder == "" {
		s.watchFolder = "INBOX"
	}
	if c.WatchUnreadOnly != nil {
		s.watchUnreadOnly = *c.WatchUnreadOnly
	}
	for _, f := range c.WatchSenders {
		if f = strings.TrimSpace(f); f != "" {
			s.watchSenders = append(s.watchSenders, f)
		}
	}
	if q := strings.TrimSpace(c.WatchQuery); q != "" {
		wq := &watchQuery{}
		for _, w := range splitWords(q) {
			if v, ok := strings.CutPrefix(strings.ToLower(w), "from:"); ok {
				wq.From = v
			} else if v, ok := strings.CutPrefix(strings.ToLower(w), "subject:"); ok {
				wq.Subject = v
			} else {
				wq.words = append(wq.words, w)
			}
		}
		s.watchQuery = wq
	}
	if v := c.WatchLookbackDays; v != nil {
		s.watchLookbackDays = clamp(int(*v), 0, 30)
	}
	return s
}

func clamp(v, lo, hi int) int {
	return max(lo, min(v, hi))
}

func main() { os.Exit(run(context.Background(), os.Stdin, os.Stdout)) }

// redactor replaces each secret in every string of a record, at any depth.
type redactor struct{ secrets []string }

func (r *redactor) add(s string) {
	// A real token or app password is far longer than 8 characters; a shorter one cannot be told
	// apart from ordinary words, and replacing it would rewrite the output instead of protecting
	// anything. An app password is often written in groups with spaces; both forms are caught.
	for _, v := range []string{s, strings.ReplaceAll(s, " ", "")} {
		if len(v) >= 8 {
			r.secrets = append(r.secrets, v)
		}
	}
}

func (r *redactor) str(s string) string {
	for _, v := range r.secrets {
		s = strings.ReplaceAll(s, v, "[redacted]")
	}
	return s
}

func (r *redactor) value(v any) any {
	switch x := v.(type) {
	case string:
		return r.str(x)
	case map[string]any:
		for k, e := range x {
			x[k] = r.value(e)
		}
		return x
	case []any:
		for i, e := range x {
			x[i] = r.value(e)
		}
		return x
	}
	return v
}

func run(ctx context.Context, in io.Reader, out io.Writer) int {
	red := &redactor{}
	emit := func(record map[string]any) {
		// Through JSON and back, so a struct inside (a watch document) is redacted as well.
		var generic map[string]any
		_ = json.Unmarshal(mustJSON(record), &generic)
		for k, v := range generic {
			if k != "t" {
				generic[k] = red.value(v)
			}
		}
		line, _ := json.Marshal(generic)
		_, _ = out.Write(append(line, '\n'))
	}
	fail := func(words string) int {
		emit(map[string]any{"t": "result", "ok": false, "error": words})
		return 1
	}

	var req request
	raw, err := io.ReadAll(in)
	if err != nil || json.Unmarshal(raw, &req) != nil {
		return fail("The request on stdin could not be read.")
	}
	conn, bound := req.Connections[slot]
	red.add(conn.AccessToken)
	red.add(conn.Password)

	s := readSettings(req)
	if s.mode != "draft" && s.mode != "send" {
		return fail("Mail's mode must be draft or send; a person sets it in this member's settings.")
	}
	// The house rule: a member in send mode with no allowlist is refused on every run, whatever
	// it was asked to do.
	if s.mode == "send" && len(s.allowlist) == 0 {
		return fail(wordsEmptyAllow)
	}
	if !bound {
		return fail(wordsNoMailbox)
	}

	var p provider
	kind := strings.ToLower(conn.Kind)
	if kind == "" {
		kind = strings.ToLower(conn.Provider)
	}
	switch kind {
	case "imap":
		if conn.Password == "" {
			return fail("The mailbox connection has no app password; a person updates it in Admin, Connections.")
		}
		user := conn.Username
		if user == "" {
			user = conn.Account
		}
		p = newIMAP(imapGrant{Account: conn.Account, Username: user, Password: conn.Password, IMAP: conn.IMAP, SMTP: conn.SMTP})
	case "microsoft":
		if conn.AccessToken == "" {
			return fail(wordsReconnectMicrosoft)
		}
		p = newGraph(conn.AccessToken)
	case "google", "":
		if conn.AccessToken == "" {
			return fail(wordsReconnect)
		}
		p = newGmail(conn.AccessToken)
	default:
		return fail("The mailbox slot is bound to a " + conn.Kind + conn.Provider + " connection; Mail takes an IMAP, Microsoft or Google one.")
	}
	defer p.close()
	s.account = conn.Account

	m := &mailer{
		p:        p,
		settings: s,
		progress: func(status string) { emit(map[string]any{"t": "progress", "status": status}) },
		watch:    loadWatch(req.Sites),
		publish: func(payload map[string]any) {
			emit(map[string]any{"t": "publish", "type": "received", "payload": payload})
		},
		quiet: true,
	}

	var outputs, failures []string
	for i, w := range req.Work {
		instr := w.Instruction
		if strings.TrimSpace(instr) == "" {
			instr = w.Payload.Instruction
		}
		text, err := m.do(ctx, instr)
		// What a command changed in the watch is stored at once, so a later failure in the same
		// batch does not undo an acknowledgement or a published check.
		for _, id := range m.watch.changed {
			emit(map[string]any{"t": "site.put", "site": watchSite, "collection": watchCollection, "id": id, "doc": m.watch.docs[id]})
		}
		m.watch.changed = nil
		prefix := ""
		if len(req.Work) > 1 {
			prefix = "Instruction " + strconv.Itoa(i+1) + ": "
		}
		if err != nil {
			failures = append(failures, prefix+err.Error())
		} else {
			outputs = append(outputs, prefix+text)
		}
	}
	if len(req.Work) == 0 {
		failures = append(failures, "No instruction was given. "+usage)
	}
	if len(failures) > 0 {
		return fail(strings.Join(append(failures, outputs...), "\n\n"))
	}
	result := map[string]any{"t": "result", "ok": true, "output": strings.Join(outputs, "\n\n")}
	if m.quiet {
		// Only watch checks that found nothing a person must see; an event they published still
		// fires its triggers.
		result["quiet"] = true
	}
	emit(result)
	return 0
}
