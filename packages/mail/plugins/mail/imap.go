package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/smtp"
	"net/textproto"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"encoding/base64"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-message/charset"
)

const (
	// caFileEnv names a PEM file of extra roots to trust. Only the black-box tests set it, to
	// trust their fake servers; the Host's allowlisted environment never carries it.
	caFileEnv = "MAIL_PLUGIN_TEST_CA_FILE"

	netTimeout = 30 * time.Second
	// A text part larger than this is fetched only in part: read cuts the body far below it.
	maxTextPartBytes = 4 << 20
	snippetChars     = 160
)

// testRoots, when set, are the only roots trusted. The plugin's own tests set it in-process.
var testRoots *x509.CertPool

func tlsConfig(host string) *tls.Config {
	cfg := &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
	if testRoots != nil {
		cfg.RootCAs = testRoots
	} else if f := os.Getenv(caFileEnv); f != "" {
		if pem, err := os.ReadFile(f); err == nil {
			pool := x509.NewCertPool()
			pool.AppendCertsFromPEM(pem)
			cfg.RootCAs = pool
		}
	}
	return cfg
}

type endpoint struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Security string `json:"security"`
}

func (e endpoint) addr() string { return net.JoinHostPort(e.Host, strconv.Itoa(e.Port)) }

// imapGrant is what a slot bound to a mailbox with an app password receives on stdin.
type imapGrant struct {
	Account  string
	Username string
	Password string
	IMAP     endpoint
	SMTP     endpoint
}

// imapBox is the mailbox over IMAP, with mail sent over SMTP. It connects on first use and keeps
// the one connection for the run.
type imapBox struct {
	g        imapGrant
	c        *imapclient.Client
	selected string
	writable bool
	boxes    []*imap.ListData
}

func newIMAP(g imapGrant) *imapBox { return &imapBox{g: g} }

func (b *imapBox) name() string { return "IMAP" }

func (b *imapBox) close() {
	if b.c != nil {
		_ = b.c.Logout().Wait()
		_ = b.c.Close()
		b.c = nil
	}
}

func (b *imapBox) wordsRefused() string {
	return "The mail server refused the sign-in for " + b.g.Account + ". A person updates the app password in Admin, Connections (for Gmail, an app password needs 2-Step Verification)."
}

func (b *imapBox) wordsUnreachable(e endpoint) string {
	return fmt.Sprintf("Could not reach %s on port %d; try again later.", e.Host, e.Port)
}

func securityOK(s string) bool {
	return strings.EqualFold(s, "TLS") || strings.EqualFold(s, "STARTTLS")
}

func (b *imapBox) connect() error {
	if b.c != nil {
		return nil
	}
	e := b.g.IMAP
	if e.Host == "" || e.Port == 0 {
		return failure("The mailbox connection has no IMAP server; a person fixes it in Admin, Connections.")
	}
	opts := &imapclient.Options{
		TLSConfig:   tlsConfig(e.Host),
		WordDecoder: &mime.WordDecoder{CharsetReader: charset.Reader},
		Dialer:      &net.Dialer{Timeout: netTimeout},
	}
	var c *imapclient.Client
	var err error
	switch {
	case strings.EqualFold(e.Security, "TLS"):
		c, err = imapclient.DialTLS(e.addr(), opts)
	case strings.EqualFold(e.Security, "STARTTLS"):
		c, err = imapclient.DialStartTLS(e.addr(), opts)
	default:
		// Never in the clear: the password would cross the network readable.
		return failure("The mailbox connection's IMAP security must be TLS or STARTTLS; a person fixes it in Admin, Connections.")
	}
	if err != nil {
		var tlsErr *tls.CertificateVerificationError
		if errors.As(err, &tlsErr) {
			return failure(fmt.Sprintf("%s on port %d did not prove it is that server (its certificate was not trusted); nothing was sent to it.", e.Host, e.Port))
		}
		return failure(b.wordsUnreachable(e))
	}
	if err := c.Login(b.g.Username, b.g.Password).Wait(); err != nil {
		_ = c.Close()
		var ie *imap.Error
		if errors.As(err, &ie) {
			return failure(b.wordsRefused())
		}
		return failure(b.wordsUnreachable(e))
	}
	b.c = c
	return nil
}

// imapFailure puts an IMAP command's error in the run's words.
func (b *imapBox) imapFailure(what string, err error) error {
	if f, ok := err.(failure); ok {
		return f
	}
	var ie *imap.Error
	if errors.As(err, &ie) {
		return failure(fmt.Sprintf("The IMAP server refused to %s: %s.", what, strings.TrimSuffix(oneLine(ie.Text), ".")))
	}
	return failure(fmt.Sprintf("The IMAP connection failed while trying to %s; try again later.", what))
}

func (b *imapBox) mailboxes() ([]*imap.ListData, error) {
	if b.boxes != nil {
		return b.boxes, nil
	}
	if err := b.connect(); err != nil {
		return nil, err
	}
	boxes, err := b.c.List("", "*", nil).Collect()
	if err != nil {
		return nil, b.imapFailure("list its folders", err)
	}
	b.boxes = boxes
	return boxes, nil
}

// specialUse maps the common names of folders to the attribute servers mark them with.
var specialUse = map[string]imap.MailboxAttr{
	"drafts": imap.MailboxAttrDrafts, "sent": imap.MailboxAttrSent, "archive": imap.MailboxAttrArchive,
	"junk": imap.MailboxAttrJunk, "spam": imap.MailboxAttrJunk, "trash": imap.MailboxAttrTrash,
	"all": imap.MailboxAttrAll, "all mail": imap.MailboxAttrAll, "starred": imap.MailboxAttrFlagged,
}

// folder finds a folder by its name (any case), or by a common name for a special-use one, so
// Drafts and Sent are found on Gmail as [Gmail]/Drafts and [Gmail]/Sent Mail.
func (b *imapBox) folder(name string) (string, error) {
	if name == "" || strings.EqualFold(name, "INBOX") {
		return "INBOX", nil
	}
	boxes, err := b.mailboxes()
	if err != nil {
		return "", err
	}
	for _, m := range boxes {
		if strings.EqualFold(m.Mailbox, name) {
			return m.Mailbox, nil
		}
	}
	if attr, ok := specialUse[strings.ToLower(name)]; ok {
		for _, m := range boxes {
			if slices.Contains(m.Attrs, attr) {
				return m.Mailbox, nil
			}
		}
	}
	return "", failure("No folder " + strconv.Quote(name) + " in this mailbox.")
}

func (b *imapBox) open(folder string, write bool) (*imap.SelectData, error) {
	if err := b.connect(); err != nil {
		return nil, err
	}
	if b.selected == folder && (b.writable || !write) {
		return nil, nil
	}
	data, err := b.c.Select(folder, &imap.SelectOptions{ReadOnly: !write}).Wait()
	if err != nil {
		b.selected = ""
		return nil, b.imapFailure("open the folder "+strconv.Quote(folder), err)
	}
	b.selected, b.writable = folder, write
	return data, nil
}

// examine opens a folder read-only and always answers its status.
func (b *imapBox) examine(folder string) (*imap.SelectData, error) {
	if err := b.connect(); err != nil {
		return nil, err
	}
	data, err := b.c.Select(folder, &imap.SelectOptions{ReadOnly: true}).Wait()
	if err != nil {
		b.selected = ""
		return nil, b.imapFailure("open the folder "+strconv.Quote(folder), err)
	}
	b.selected, b.writable = folder, false
	return data, nil
}

// An IMAP message id is its UID, followed by @ and the folder (escaped) when not the Inbox.
func imapID(folder string, uid imap.UID) string {
	if folder == "INBOX" {
		return strconv.FormatUint(uint64(uid), 10)
	}
	return strconv.FormatUint(uint64(uid), 10) + "@" + url.PathEscape(folder)
}

func parseIMAPID(id string) (folder string, uid imap.UID, ok bool) {
	n, f, hasFolder := strings.Cut(id, "@")
	v, err := strconv.ParseUint(n, 10, 32)
	if err != nil || v == 0 {
		return "", 0, false
	}
	folder = "INBOX"
	if hasFolder {
		if folder, err = url.PathUnescape(f); err != nil || folder == "" {
			return "", 0, false
		}
	}
	return folder, imap.UID(v), true
}

// byFolder groups ids by folder, refusing one that is not an IMAP id.
func byFolder(ids []string) (map[string][]imap.UID, []string, error) {
	groups := map[string][]imap.UID{}
	var order []string
	for _, id := range ids {
		f, uid, ok := parseIMAPID(id)
		if !ok {
			return nil, nil, failure("No message " + id + " in this mailbox.")
		}
		if _, seen := groups[f]; !seen {
			order = append(order, f)
		}
		groups[f] = append(groups[f], uid)
	}
	return groups, order, nil
}

// present checks that every uid is in the selected folder, naming the first missing.
func (b *imapBox) present(folder string, uids []imap.UID) error {
	data, err := b.c.UIDSearch(&imap.SearchCriteria{UID: []imap.UIDSet{imap.UIDSetNum(uids...)}}, nil).Wait()
	if err != nil {
		return b.imapFailure("search the folder", err)
	}
	have := data.AllUIDs()
	for _, u := range uids {
		if !slices.Contains(have, u) {
			return failure("No message " + imapID(folder, u) + " in this mailbox.")
		}
	}
	return nil
}

func (b *imapBox) list(ctx context.Context, q query) ([]summary, string, error) {
	folder, err := b.folder(q.Folder)
	if err != nil {
		return nil, "", err
	}
	if _, err := b.open(folder, false); err != nil {
		return nil, "", err
	}
	where := "in " + folder
	crit := &imap.SearchCriteria{}
	if q.Raw != "" {
		// IMAP has no search language beyond SEARCH's keys: each word is looked for as text.
		crit.Text = strings.Fields(q.Raw)
		where = "in " + folder + " matching the search"
	} else {
		if q.Unread {
			crit.NotFlag = []imap.Flag{imap.FlagSeen}
		}
		if q.From != "" {
			crit.Header = append(crit.Header, imap.SearchCriteriaHeaderField{Key: "From", Value: q.From})
		}
		if q.Subject != "" {
			crit.Header = append(crit.Header, imap.SearchCriteriaHeaderField{Key: "Subject", Value: q.Subject})
		}
		if !q.Since.IsZero() {
			crit.Since = q.Since
		}
		if q.Unread || q.From != "" || q.Subject != "" || !q.Since.IsZero() {
			where += " matching the query"
		}
	}
	data, err := b.c.UIDSearch(crit, nil).Wait()
	if err != nil {
		return nil, "", b.imapFailure("search the folder", err)
	}
	uids := data.AllUIDs()
	slices.Sort(uids)
	if len(uids) > q.Max {
		uids = uids[len(uids)-q.Max:]
	}
	out, err := b.summaries(folder, uids)
	slices.Reverse(out) // newest first
	return out, where, err
}

// summaries fetches the headers and a snippet of each uid in the selected folder, oldest first.
func (b *imapBox) summaries(folder string, uids []imap.UID) ([]summary, error) {
	if len(uids) == 0 {
		return nil, nil
	}
	msgs, err := b.c.Fetch(imap.UIDSetNum(uids...), &imap.FetchOptions{
		UID: true, Flags: true, Envelope: true, InternalDate: true, BodyStructure: &imap.FetchItemBodyStructure{Extended: true},
	}).Collect()
	if err != nil {
		return nil, b.imapFailure("read the messages", err)
	}
	slices.SortFunc(msgs, func(x, y *imapclient.FetchMessageBuffer) int { return int(x.UID) - int(y.UID) })

	// One snippet fetch per message, pipelined: each message's text part has its own path.
	type pending struct {
		cmd     *imapclient.FetchCommand
		section *imap.FetchItemBodySection
		part    *imap.BodyStructureSinglePart
	}
	snips := make([]pending, len(msgs))
	for i, m := range msgs {
		path, part := textPart(m.BodyStructure, "plain")
		if part == nil {
			path, part = textPart(m.BodyStructure, "html")
		}
		if part == nil {
			continue
		}
		sec := &imap.FetchItemBodySection{Part: path, Peek: true, Partial: &imap.SectionPartial{Offset: 0, Size: 2048}}
		snips[i] = pending{b.c.Fetch(imap.UIDSetNum(m.UID), &imap.FetchOptions{UID: true, BodySection: []*imap.FetchItemBodySection{sec}}), sec, part}
	}
	out := make([]summary, 0, len(msgs))
	for i, m := range msgs {
		s := envelopeSummary(folder, m)
		if p := snips[i]; p.cmd != nil {
			got, err := p.cmd.Collect()
			if err == nil && len(got) == 1 {
				text := decodePart(got[0].FindBodySection(p.section), p.part, true)
				if p.part.Subtype == "html" || strings.EqualFold(p.part.Subtype, "html") {
					text = htmlToText(text)
				}
				s.Snippet = cutRunes(oneLine(text), snippetChars)
			}
		}
		out = append(out, s)
	}
	return out, nil
}

func envelopeSummary(folder string, m *imapclient.FetchMessageBuffer) summary {
	s := summary{ID: imapID(folder, m.UID), Unread: !slices.Contains(m.Flags, imap.FlagSeen)}
	date := m.InternalDate
	if m.Envelope != nil {
		s.From = addressList(m.Envelope.From)
		s.Subject = m.Envelope.Subject
		s.Thread = m.Envelope.MessageID
		if !m.Envelope.Date.IsZero() {
			date = m.Envelope.Date
		}
	}
	s.Date = fmtTime(date)
	return s
}

func addressList(list []imap.Address) string {
	var out []string
	for _, a := range list {
		addr := a.Addr()
		if a.Name != "" {
			out = append(out, a.Name+" <"+addr+">")
		} else {
			out = append(out, addr)
		}
	}
	return strings.Join(out, ", ")
}

// textPart finds the first text/<subtype> part that is not an attachment.
func textPart(bs imap.BodyStructure, subtype string) ([]int, *imap.BodyStructureSinglePart) {
	var path []int
	var found *imap.BodyStructureSinglePart
	if bs == nil {
		return nil, nil
	}
	bs.Walk(func(p []int, part imap.BodyStructure) bool {
		if found != nil {
			return false
		}
		if sp, ok := part.(*imap.BodyStructureSinglePart); ok {
			if strings.EqualFold(sp.Type, "text") && strings.EqualFold(sp.Subtype, subtype) && !isAttachment(sp) {
				path, found = slices.Clone(p), sp
			}
			return false
		}
		return true
	})
	return path, found
}

func isAttachment(sp *imap.BodyStructureSinglePart) bool {
	if d := sp.Disposition(); d != nil && strings.EqualFold(d.Value, "attachment") {
		return true
	}
	return sp.Filename() != ""
}

// decodePart turns a part's bytes, in its transfer encoding and charset, into text. partial says
// the bytes may stop mid-way, so a broken last piece is dropped instead of failing.
func decodePart(raw []byte, part *imap.BodyStructureSinglePart, partial bool) string {
	var data []byte
	switch strings.ToLower(part.Encoding) {
	case "base64":
		clean := bytes.Map(func(r rune) rune {
			if r == '\r' || r == '\n' || r == ' ' || r == '\t' {
				return -1
			}
			return r
		}, raw)
		if partial {
			clean = clean[:len(clean)/4*4]
		}
		d, err := base64.StdEncoding.DecodeString(string(clean))
		if err != nil {
			d, _ = base64.RawStdEncoding.DecodeString(strings.TrimRight(string(clean), "="))
		}
		data = d
	case "quoted-printable":
		data, _ = io.ReadAll(quotedprintable.NewReader(bytes.NewReader(raw)))
	default:
		data = raw
	}
	if cs := part.Params["charset"]; cs != "" && !strings.EqualFold(cs, "utf-8") && !strings.EqualFold(cs, "us-ascii") {
		if r, err := charset.Reader(cs, bytes.NewReader(data)); err == nil {
			if d, err := io.ReadAll(r); err == nil {
				data = d
			}
		}
	}
	s := string(data)
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "�")
	}
	return strings.ReplaceAll(s, "\r\n", "\n")
}

func (b *imapBox) read(ctx context.Context, id string) (*fullMessage, error) {
	folder, uid, ok := parseIMAPID(id)
	if !ok {
		return nil, failure("No message " + id + " in this mailbox.")
	}
	if _, err := b.open(folder, false); err != nil {
		if strings.Contains(err.Error(), "open the folder") {
			return nil, failure("No message " + id + " in this mailbox.")
		}
		return nil, err
	}
	msgs, err := b.c.Fetch(imap.UIDSetNum(uid), &imap.FetchOptions{
		UID: true, Flags: true, Envelope: true, InternalDate: true, BodyStructure: &imap.FetchItemBodyStructure{Extended: true},
	}).Collect()
	if err != nil {
		return nil, b.imapFailure("read the message", err)
	}
	var m *imapclient.FetchMessageBuffer
	for _, x := range msgs {
		if x.UID == uid {
			m = x
		}
	}
	if m == nil {
		return nil, failure("No message " + id + " in this mailbox.")
	}
	s := envelopeSummary(folder, m)
	full := &fullMessage{ID: s.ID, Thread: s.Thread, From: s.From, Date: s.Date, Subject: s.Subject, Folders: []string{folder}}
	if m.Envelope != nil {
		full.To, full.Cc = addressList(m.Envelope.To), addressList(m.Envelope.Cc)
	}
	for _, sub := range []string{"plain", "html"} {
		path, part := textPart(m.BodyStructure, sub)
		if part == nil {
			continue
		}
		sec := &imap.FetchItemBodySection{Part: path, Peek: true}
		partial := part.Size > maxTextPartBytes
		if partial {
			sec.Partial = &imap.SectionPartial{Offset: 0, Size: maxTextPartBytes}
		}
		got, err := b.c.Fetch(imap.UIDSetNum(uid), &imap.FetchOptions{UID: true, BodySection: []*imap.FetchItemBodySection{sec}}).Collect()
		if err != nil {
			return nil, b.imapFailure("read the message", err)
		}
		if len(got) == 1 {
			text := decodePart(got[0].FindBodySection(sec), part, partial)
			if sub == "plain" {
				full.Text = text
			} else {
				full.HTML = text
			}
		}
		if full.Text != "" {
			break
		}
	}
	if m.BodyStructure != nil {
		m.BodyStructure.Walk(func(_ []int, part imap.BodyStructure) bool {
			if sp, ok := part.(*imap.BodyStructureSinglePart); ok {
				if isAttachment(sp) {
					name := sp.Filename()
					if name == "" {
						name = "(no name)"
					}
					full.Attachments = append(full.Attachments, attachment{Name: name, Type: sp.MediaType(), Size: int64(sp.Size)})
				}
				return false
			}
			return true
		})
	}
	return full, nil
}

func (b *imapBox) setRead(ctx context.Context, ids []string, read bool) error {
	groups, order, err := byFolder(ids)
	if err != nil {
		return err
	}
	op := imap.StoreFlagsAdd
	if !read {
		op = imap.StoreFlagsDel
	}
	for _, f := range order {
		if _, err := b.open(f, true); err != nil {
			return err
		}
		if err := b.present(f, groups[f]); err != nil {
			return err
		}
		flags := &imap.StoreFlags{Op: op, Silent: true, Flags: []imap.Flag{imap.FlagSeen}}
		if err := b.c.Store(imap.UIDSetNum(groups[f]...), flags, nil).Close(); err != nil {
			return b.imapFailure("change the messages' read state", err)
		}
	}
	return nil
}

func (b *imapBox) move(ctx context.Context, ids []string, folder string) (map[string]string, error) {
	dest, err := b.folder(folder)
	if err != nil {
		return nil, err
	}
	groups, order, err := byFolder(ids)
	if err != nil {
		return nil, err
	}
	moved := map[string]string{}
	for _, f := range order {
		if f == dest {
			continue
		}
		if _, err := b.open(f, true); err != nil {
			return moved, err
		}
		uids := groups[f]
		if err := b.present(f, uids); err != nil {
			return moved, err
		}
		data, err := b.c.Move(imap.UIDSetNum(uids...), dest).Wait()
		if err != nil {
			return moved, b.imapFailure("move the messages to "+strconv.Quote(dest), err)
		}
		if data != nil && data.SourceUIDs != nil && data.DestUIDs != nil {
			src, ok1 := data.SourceUIDs.(imap.UIDSet)
			dst, ok2 := data.DestUIDs.(imap.UIDSet)
			if ok1 && ok2 {
				s, _ := src.Nums()
				d, _ := dst.Nums()
				for i := range min(len(s), len(d)) {
					moved[imapID(f, s[i])] = imapID(dest, d[i])
				}
			}
		}
	}
	return moved, nil
}

// label on Gmail's IMAP is a copy into the label's folder; on any other server, where a copy
// would duplicate the message, it is a keyword flag on the message.
func (b *imapBox) label(ctx context.Context, ids []string, label string) error {
	if err := b.connect(); err != nil {
		return err
	}
	groups, order, err := byFolder(ids)
	if err != nil {
		return err
	}
	gmailLabels := b.c.Caps().Has(imap.Cap("X-GM-EXT-1"))
	dest := ""
	if gmailLabels {
		if dest, err = b.folder(label); err != nil {
			return err
		}
	}
	keyword := strings.Map(func(r rune) rune {
		if r <= ' ' || r > '~' || strings.ContainsRune(`(){%*"\]`, r) {
			return '_'
		}
		return r
	}, label)
	for _, f := range order {
		if _, err := b.open(f, true); err != nil {
			return err
		}
		if err := b.present(f, groups[f]); err != nil {
			return err
		}
		set := imap.UIDSetNum(groups[f]...)
		if gmailLabels {
			if _, err := b.c.Copy(set, dest).Wait(); err != nil {
				return b.imapFailure("label the messages", err)
			}
			continue
		}
		flags := &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.Flag(keyword)}}
		if err := b.c.Store(set, flags, nil).Close(); err != nil {
			return b.imapFailure("label the messages", err)
		}
	}
	return nil
}

func (b *imapBox) draft(ctx context.Context, o outgoing) (string, error) {
	drafts, err := b.folder("Drafts")
	if err != nil {
		return "", failure("This mailbox has no Drafts folder, so no draft was saved; nothing was sent.")
	}
	o.From = b.g.Account
	raw := []byte(buildMessage(o, false))
	cmd := b.c.Append(drafts, int64(len(raw)), &imap.AppendOptions{Flags: []imap.Flag{imap.FlagDraft, imap.FlagSeen}, Time: time.Now()})
	if _, err := cmd.Write(raw); err != nil {
		return "", failure("The IMAP connection failed while saving the draft; nothing was sent.")
	}
	if err := cmd.Close(); err != nil {
		return "", failure("The IMAP connection failed while saving the draft; nothing was sent.")
	}
	data, err := cmd.Wait()
	if err != nil {
		return "", failure(strings.TrimSuffix(b.imapFailure("save the draft", err).Error(), ".") + "; nothing was sent.")
	}
	if data != nil && data.UID != 0 {
		return fmt.Sprintf("Draft %s saved in %s; nothing was sent.", imapID(drafts, data.UID), drafts), nil
	}
	return "Draft saved in " + drafts + "; nothing was sent.", nil
}

// send sends over SMTP, signing in with the username, or with the address when the username
// has no @ (iCloud). Bcc recipients get the message but no Bcc line.
func (b *imapBox) send(ctx context.Context, o outgoing) (string, error) {
	e := b.g.SMTP
	if e.Host == "" || e.Port == 0 {
		return "", failure("The mailbox connection has no SMTP server; a person fixes it in Admin, Connections. Nothing was sent.")
	}
	if !securityOK(e.Security) {
		return "", failure("The mailbox connection's SMTP security must be TLS or STARTTLS; a person fixes it in Admin, Connections. Nothing was sent.")
	}
	o.From = b.g.Account
	user := b.g.Username
	if !strings.Contains(user, "@") {
		user = b.g.Account
	}
	unreachable := failure(fmt.Sprintf("Could not reach %s on port %d; try again later. Nothing was sent.", e.Host, e.Port))
	cfg := tlsConfig(e.Host)
	dialer := &net.Dialer{Timeout: netTimeout}
	var conn net.Conn
	var err error
	if strings.EqualFold(e.Security, "TLS") {
		conn, err = tls.DialWithDialer(dialer, "tcp", e.addr(), cfg)
	} else {
		conn, err = dialer.Dial("tcp", e.addr())
	}
	if err != nil {
		var tlsErr *tls.CertificateVerificationError
		if errors.As(err, &tlsErr) {
			return "", failure(fmt.Sprintf("%s on port %d did not prove it is that server (its certificate was not trusted); nothing was sent.", e.Host, e.Port))
		}
		return "", unreachable
	}
	_ = conn.SetDeadline(time.Now().Add(2 * netTimeout))
	c, err := smtp.NewClient(conn, e.Host)
	if err != nil {
		_ = conn.Close()
		return "", unreachable
	}
	defer c.Close()
	if strings.EqualFold(e.Security, "STARTTLS") {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return "", failure(e.Host + " does not offer STARTTLS, so the password was not sent; nothing was sent.")
		}
		if err := c.StartTLS(cfg); err != nil {
			var tlsErr *tls.CertificateVerificationError
			if errors.As(err, &tlsErr) {
				return "", failure(fmt.Sprintf("%s on port %d did not prove it is that server (its certificate was not trusted); nothing was sent.", e.Host, e.Port))
			}
			return "", unreachable
		}
	}
	if err := c.Auth(smtp.PlainAuth("", user, b.g.Password, e.Host)); err != nil {
		var pe *textproto.Error
		if errors.As(err, &pe) && pe.Code >= 500 {
			return "", failure("The mail server refused the sign-in for " + b.g.Account + " to send. A person updates the app password in Admin, Connections. Nothing was sent.")
		}
		return "", failure("The mail server did not accept the sign-in to send; try again later. Nothing was sent.")
	}
	refused := func(err error) error {
		var pe *textproto.Error
		if errors.As(err, &pe) {
			if pe.Code >= 400 && pe.Code < 500 {
				return failure(fmt.Sprintf("The mail server answered %d (%s); try again later. Nothing was sent.", pe.Code, oneLine(pe.Msg)))
			}
			return failure(fmt.Sprintf("The mail server refused the message: %d %s. Nothing was sent.", pe.Code, oneLine(pe.Msg)))
		}
		return unreachable
	}
	if err := c.Mail(b.g.Account); err != nil {
		return "", refused(err)
	}
	for _, r := range append(append(append([]string{}, o.To...), o.Cc...), o.Bcc...) {
		if err := c.Rcpt(r); err != nil {
			_ = c.Reset()
			return "", refused(err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return "", refused(err)
	}
	if _, err := io.WriteString(w, buildMessage(o, true)); err != nil {
		return "", unreachable
	}
	if err := w.Close(); err != nil {
		return "", refused(err)
	}
	_ = c.Quit()
	return "Sent from " + b.g.Account + " over SMTP.", nil
}

func (b *imapBox) watchStart(ctx context.Context, w watchSpec) (position, []summary, error) {
	folder, err := b.folder(w.folder)
	if err != nil {
		return position{}, nil, err
	}
	data, err := b.examine(folder)
	if err != nil {
		return position{}, nil, err
	}
	last := uint32(0)
	if data.UIDNext > 0 {
		last = uint32(data.UIDNext) - 1
	} else if data.NumMessages > 0 {
		all, err := b.c.UIDSearch(&imap.SearchCriteria{}, nil).Wait()
		if err != nil {
			return position{}, nil, b.imapFailure("search the folder", err)
		}
		for _, u := range all.AllUIDs() {
			last = max(last, uint32(u))
		}
	}
	pos := position{IMAP: &imapPosition{UIDValidity: data.UIDValidity, LastUID: last}}
	if w.lookback <= 0 {
		return pos, nil, nil
	}
	crit := &imap.SearchCriteria{Since: time.Now().AddDate(0, 0, -w.lookback)}
	if w.unreadOnly {
		crit.NotFlag = []imap.Flag{imap.FlagSeen}
	}
	found, err := b.c.UIDSearch(crit, nil).Wait()
	if err != nil {
		return position{}, nil, b.imapFailure("search the folder", err)
	}
	uids := found.AllUIDs()
	slices.Sort(uids)
	if len(uids) > w.max {
		uids = uids[len(uids)-w.max:]
	}
	msgs, err := b.summaries(folder, uids)
	return pos, msgs, err
}

func (b *imapBox) watchNext(ctx context.Context, w watchSpec, pos position) (position, []summary, string, error) {
	if pos.IMAP == nil {
		p, _, err := b.watchStart(ctx, watchSpec{folder: w.folder})
		return p, nil, "The stored position was not IMAP's; the watch starts again from now.", err
	}
	folder, err := b.folder(w.folder)
	if err != nil {
		return position{}, nil, "", err
	}
	data, err := b.examine(folder)
	if err != nil {
		return position{}, nil, "", err
	}
	if data.UIDValidity != pos.IMAP.UIDValidity {
		// The server renumbered the folder: the stored UID means nothing any more.
		p, _, err := b.watchStart(ctx, watchSpec{folder: w.folder})
		return p, nil, "The folder " + folder + " was renumbered by the server (its UIDVALIDITY changed); the watch starts again from now, and mail that arrived meanwhile is not listed.", err
	}
	if data.UIDNext > 0 && uint32(data.UIDNext) <= pos.IMAP.LastUID+1 {
		return pos, nil, "", nil
	}
	found, err := b.c.UIDSearch(&imap.SearchCriteria{UID: []imap.UIDSet{{imap.UIDRange{Start: imap.UID(pos.IMAP.LastUID + 1), Stop: 0}}}}, nil).Wait()
	if err != nil {
		return position{}, nil, "", b.imapFailure("search the folder", err)
	}
	var uids []imap.UID
	for _, u := range found.AllUIDs() {
		// n:* always matches the last message, even one at or below n.
		if uint32(u) > pos.IMAP.LastUID {
			uids = append(uids, u)
		}
	}
	slices.Sort(uids)
	if len(uids) > w.max {
		uids = uids[:w.max]
	}
	next := *pos.IMAP
	if len(uids) == 0 {
		return position{IMAP: &next}, nil, "", nil
	}
	next.LastUID = uint32(uids[len(uids)-1])
	msgs, err := b.summaries(folder, uids)
	if err != nil {
		return position{}, nil, "", err
	}
	if w.unreadOnly {
		msgs = slices.DeleteFunc(msgs, func(s summary) bool { return !s.Unread })
	}
	return position{IMAP: &next}, msgs, "", nil
}
