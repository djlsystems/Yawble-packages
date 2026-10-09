package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"html"
	"mime"
	"net/mail"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-message/charset"
)

const (
	// The messages Mailer has shown - listed, searched or read - are remembered in the watch's
	// collection, one document per mailbox, so a reply in a later run can answer one of them.
	// The document holds short hashes of the ids and the mailbox's own address, as the watch's
	// documents do; never a message id, a sender or any content.
	seenPrefix = "seen-"
	seenLimit  = 500
)

type seenDoc struct {
	Provider string   `json:"provider"`
	Account  string   `json:"account"`
	Shown    []string `json:"shown"` // oldest first
	Updated  string   `json:"updated"`
}

func (m *mailer) hashOf(s string) string {
	h := sha256.Sum256([]byte(m.p.name() + "\n" + strings.ToLower(m.settings.account) + "\n" + s))
	return hex.EncodeToString(h[:8])
}

func (m *mailer) seenID() string { return seenPrefix + m.hashOf("")[:16] }

// show records the ids of messages this run listed or read: in the run, and on the site when the
// site is there, so a later run may reply to them.
func (m *mailer) show(ids ...string) {
	for _, id := range ids {
		m.shown[id] = true
	}
	if m.watch.usable() != nil {
		return
	}
	key := m.seenID()
	doc := m.watch.seen[key]
	if doc == nil {
		doc = &seenDoc{Provider: m.p.name(), Account: m.settings.account}
	}
	changed := false
	for _, id := range ids {
		if h := m.hashOf(id); !slices.Contains(doc.Shown, h) {
			doc.Shown, changed = append(doc.Shown, h), true
		}
	}
	if !changed {
		return
	}
	if len(doc.Shown) > seenLimit {
		doc.Shown = doc.Shown[len(doc.Shown)-seenLimit:]
	}
	doc.Updated = time.Now().UTC().Format(time.RFC3339)
	m.watch.seen[key] = doc
	m.watch.change(key)
}

// wasShown is whether Mailer showed the message: in this run, in an earlier one, or in the
// watch's list of mail waiting for an acknowledgement.
func (m *mailer) wasShown(id string) bool {
	if m.shown[id] {
		return true
	}
	if doc := m.watch.seen[m.seenID()]; doc != nil && slices.Contains(doc.Shown, m.hashOf(id)) {
		return true
	}
	for _, doc := range m.watch.docs {
		if doc.Provider != m.p.name() || !strings.EqualFold(doc.Account, m.settings.account) {
			continue
		}
		if slices.ContainsFunc(doc.Pending, func(p pendingMsg) bool { return p.ID == id }) {
			return true
		}
	}
	return false
}

var (
	reReplyPrefix = regexp.MustCompile(`(?i)^re\s*(\[\d+\])?\s*:\s*`)
	reMsgID       = regexp.MustCompile(`<[^<>\s]+>`)
	reAnyAddress  = regexp.MustCompile(`[^\s<>()\[\]\\,;:"]+@[^\s<>()\[\]\\,;:"]+`)
)

// replySubject is "Re: " and the original's subject, without the Re: it may already have, so a
// reply to a reply is not "Re: Re:".
func replySubject(s string) string {
	s = oneLine(s)
	for {
		t := reReplyPrefix.ReplaceAllString(s, "")
		if t == s {
			break
		}
		s = t
	}
	return strings.TrimSpace("Re: " + s)
}

// addresses are the plain addresses in a header's value: "Name <address>, address".
func addresses(v string) []string {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	var out []string
	p := &mail.AddressParser{WordDecoder: &mime.WordDecoder{CharsetReader: charset.Reader}}
	if list, err := p.ParseList(v); err == nil {
		for _, a := range list {
			out = append(out, a.Address)
		}
		return out
	}
	return reAnyAddress.FindAllString(v, -1)
}

// addUnique appends the addresses not yet in list and not the mailbox's own, ignoring case.
func addUnique(list []string, own string, more ...string) []string {
	for _, a := range more {
		if strings.EqualFold(a, own) || slices.ContainsFunc(list, func(b string) bool { return strings.EqualFold(a, b) }) {
			continue
		}
		list = append(list, a)
	}
	return list
}

// quoteLines writes text as a quote: each line after "> ".
func quoteLines(text string) string {
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight("> "+l, " ")
	}
	return strings.Join(lines, "\n")
}

// reply drafts, or in send mode sends, a reply to a message Mailer showed: to its Reply-To, else
// its From (and with all, its other recipients), every one checked against the allowlist, with the
// subject "Re: " and the original's, threaded as the provider threads. The original is quoted
// below the reply only with quote. draft makes a draft even in send mode.
func (m *mailer) reply(ctx context.Context, args []string, rest string) (string, error) {
	if len(args) == 0 {
		return "", failure("reply takes a message id, one a list, search or read showed: `reply <id>`, then the reply's text on the lines below.")
	}
	id := args[0]
	var all, quote, draftOnly bool
	for _, w := range args[1:] {
		switch strings.ToLower(w) {
		case "all":
			all = true
		case "quote":
			quote = true
		case "draft":
			draftOnly = true
		default:
			return "", failure(`reply takes only "all", "quote" and "draft" after the id; ` + strconv.Quote(w) + " is not one of them.")
		}
	}
	body := strings.TrimRight(strings.TrimLeft(rest, "\n"), " \t\n")
	if strings.TrimSpace(body) == "" {
		return "", failure("reply needs the text of the reply on the lines below `reply <id>`; nothing was drafted or sent.")
	}
	if !m.wasShown(id) {
		return "", failure("Refused: Mailer has not listed or read message " + id + ", so it does not reply to it; list or read it first. Nothing was drafted or sent.")
	}

	m.progress("reading the message to reply to")
	orig, err := m.p.read(ctx, id)
	if err != nil {
		return "", err
	}
	to := addUnique(nil, "", addresses(orig.ReplyTo)...)
	if len(to) == 0 {
		to = addUnique(nil, "", addresses(orig.From)...)
	}
	if len(to) == 0 {
		return "", failure("Refused: message " + id + " names no address to reply to; nothing was sent.")
	}
	var cc []string
	if all {
		cc = addUnique(nil, m.settings.account, append(addresses(orig.To), addresses(orig.Cc)...)...)
		cc = slices.DeleteFunc(cc, func(a string) bool {
			return slices.ContainsFunc(to, func(b string) bool { return strings.EqualFold(a, b) })
		})
	}
	if err := m.checkRecipients(append(slices.Clone(to), cc...)); err != nil {
		return "", err
	}

	o := outgoing{To: to, Cc: cc, Subject: replySubject(orig.Subject), Body: body, Thread: orig.Thread, ReplyOf: orig.ID, ReplyAll: all}
	refs := reMsgID.FindAllString(orig.References, -1)
	if mid := reMsgID.FindString(orig.MessageID); mid != "" {
		o.InReplyTo = mid
		if !slices.Contains(refs, mid) {
			refs = append(refs, mid)
		}
	}
	o.References = strings.Join(refs, " ")

	var quoteHead, quoted string
	if quote {
		quoted = strings.TrimSpace(orig.Text)
		if quoted == "" {
			quoted = htmlToText(orig.HTML)
		}
		quoted = strings.ReplaceAll(quoted, "\r\n", "\n")
		if r := []rune(quoted); len(r) > m.settings.maxBodyChars {
			quoted = string(r[:m.settings.maxBodyChars]) + "\n[cut]"
		}
		quoteHead = "On " + oneLine(orig.Date) + ", " + oneLine(orig.From) + " wrote:"
		o.Body = body + "\n\n" + quoteHead + "\n" + quoteLines(quoted)
	}
	if m.settings.format == "html" {
		o.HTML = renderHTML(body)
		if quote && o.HTML != "" {
			// The quote is shown as the text it is, never rendered: it was written by whoever sent it.
			q := "<p>" + html.EscapeString(quoteHead) + "</p>\n<blockquote style=\"margin:0 0 0 .8ex;border-left:1px solid #ccc;padding-left:1ex\">" +
				strings.ReplaceAll(html.EscapeString(quoted), "\n", "<br>\n") + "</blockquote>\n"
			o.HTML = strings.Replace(o.HTML, "</body></html>", q+"</body></html>", 1)
		}
	}

	var out string
	if m.settings.mode == "send" && !draftOnly {
		m.progress("sending one reply")
		out, err = m.p.send(ctx, o)
	} else {
		m.progress("creating one reply draft")
		out, err = m.p.draft(ctx, o)
		if err == nil {
			out += " The person sends the draft from their mailbox."
		}
	}
	if err != nil {
		return "", err
	}
	what := "the reply's recipients and subject"
	seen := "To: " + strings.Join(to, ", ") + "\n"
	if len(cc) > 0 {
		seen += "Cc: " + strings.Join(cc, ", ") + "\n"
	}
	seen += "Subject: " + o.Subject
	if quote {
		what += ", and the original quoted below the reply"
		seen += "\n\n" + quoteHead + "\n" + quoteLines(quoted)
	}
	return "Reply to message " + id + ", threaded to it. " + out + "\n" + untrusted(what, seen), nil
}
