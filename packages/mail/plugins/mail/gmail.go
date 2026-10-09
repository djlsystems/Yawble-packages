package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"html"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	// gmailBaseEnv points the plugin at a fake Gmail. Only the tests set it: the Host passes a
	// plugin an allowlisted environment that never carries it, and no setting can change the base.
	gmailBaseEnv     = "MAIL_PLUGIN_TEST_GMAIL_BASE"
	defaultGmailBase = "https://gmail.googleapis.com"

	scopeReadonly = "https://www.googleapis.com/auth/gmail.readonly"
	scopeCompose  = "https://www.googleapis.com/auth/gmail.compose"
	scopeModify   = "https://www.googleapis.com/auth/gmail.modify"

	wordsReconnect = "The Gmail connection needs reconnecting in Admin, Connections."
)

var gmailWords = httpWords{provider: "Gmail", reconnect: wordsReconnect}

// gmail is the mailbox over Gmail's API, with a Google connection's access token.
type gmail struct {
	api    *restClient
	labels []gmailLabel // read once per run
}

func newGmail(token string) *gmail {
	base := defaultGmailBase
	if v := os.Getenv(gmailBaseEnv); v != "" {
		base = v
	}
	return &gmail{api: newRESTClient(strings.TrimRight(base, "/")+"/gmail/v1/users/me", token)}
}

func (g *gmail) name() string { return "Gmail" }
func (g *gmail) close()       {}

type gmailLabel struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (g *gmail) allLabels(ctx context.Context) ([]gmailLabel, error) {
	if g.labels != nil {
		return g.labels, nil
	}
	var v struct {
		Labels []gmailLabel `json:"labels"`
	}
	if err := g.api.call(ctx, "GET", "/labels", nil, nil, &v); err != nil {
		return nil, failure(gmailWords.describe(err, scopeReadonly, false))
	}
	g.labels = v.Labels
	return v.Labels, nil
}

// resolveLabel finds a label by id or by name; a name with spaces may be written with dashes, as
// Gmail's search does.
func (g *gmail) resolveLabel(ctx context.Context, want string) (id, name string, err error) {
	all, err := g.allLabels(ctx)
	if err != nil {
		return "", "", err
	}
	for _, l := range all {
		if l.ID == want {
			return l.ID, l.Name, nil
		}
	}
	norm := func(s string) string { return strings.ToLower(strings.NewReplacer(" ", "-", "/", "-").Replace(s)) }
	for _, l := range all {
		if norm(l.Name) == norm(want) {
			return l.ID, l.Name, nil
		}
	}
	return "", "", failure("No label " + strconv.Quote(want) + " in this mailbox.")
}

type gmailHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type gmailPart struct {
	MimeType string        `json:"mimeType"`
	Filename string        `json:"filename"`
	Headers  []gmailHeader `json:"headers"`
	Body     struct {
		Data         string `json:"data"`
		Size         int    `json:"size"`
		AttachmentID string `json:"attachmentId"`
	} `json:"body"`
	Parts []gmailPart `json:"parts"`
}

type gmailMessage struct {
	ID       string    `json:"id"`
	ThreadID string    `json:"threadId"`
	LabelIDs []string  `json:"labelIds"`
	Snippet  string    `json:"snippet"`
	Payload  gmailPart `json:"payload"`
}

func (p gmailPart) header(name string) string {
	for _, h := range p.Headers {
		if strings.EqualFold(h.Name, name) {
			return h.Value
		}
	}
	return ""
}

func (g *gmail) list(ctx context.Context, q query) ([]summary, string, error) {
	params := url.Values{"maxResults": {strconv.Itoa(q.Max)}}
	where := "matching the search"
	if q.Raw != "" {
		params.Set("q", q.Raw)
	} else {
		var terms []string
		if !q.AnyFolder {
			folder := q.Folder
			if folder == "" {
				folder = "INBOX"
			}
			id, name, err := g.resolveLabel(ctx, folder)
			if err != nil {
				return nil, "", err
			}
			params.Set("labelIds", id)
			where = "in " + name
		}
		if q.Unread {
			terms = append(terms, "is:unread")
		}
		if q.From != "" {
			terms = append(terms, "from:"+gmailQuote(q.From))
		}
		if q.Subject != "" {
			terms = append(terms, "subject:"+gmailQuote(q.Subject))
		}
		if !q.Since.IsZero() {
			terms = append(terms, "after:"+q.Since.Format("2006/01/02"))
		}
		if len(terms) > 0 {
			params.Set("q", strings.Join(terms, " "))
			where = strings.TrimPrefix(where+" matching the query", "matching the search ")
		}
	}
	var page struct {
		Messages []struct {
			ID string `json:"id"`
		} `json:"messages"`
	}
	if err := g.api.call(ctx, "GET", "/messages", params, nil, &page); err != nil {
		return nil, "", failure(gmailWords.describe(err, scopeReadonly, false))
	}
	ids := make([]string, 0, len(page.Messages))
	for _, m := range page.Messages {
		ids = append(ids, m.ID)
	}
	// Gmail may answer with more ids than asked for; fetch no more than q.Max of them.
	out, err := g.summaries(ctx, ids[:min(len(ids), q.Max)])
	return out, where, err
}

// gmailQuote writes a search value in one word, quoting it when it holds a space.
func gmailQuote(s string) string {
	if strings.ContainsAny(s, " \t\"") {
		return `"` + strings.ReplaceAll(s, `"`, "") + `"`
	}
	return s
}

// summaries fetches each message's headers, a few at once, keeping the order given. A message
// removed between the list and the fetch is left out.
func (g *gmail) summaries(ctx context.Context, ids []string) ([]summary, error) {
	got := make([]*gmailMessage, len(ids))
	errs := make([]error, len(ids))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for i, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			mq := url.Values{"format": {"metadata"}, "metadataHeaders": {"From", "Subject", "Date"}}
			var msg gmailMessage
			if err := g.api.call(ctx, "GET", "/messages/"+url.PathEscape(id), mq, nil, &msg); err != nil {
				errs[i] = err
				return
			}
			got[i] = &msg
		}()
	}
	wg.Wait()
	var out []summary
	for i, msg := range got {
		if err := errs[i]; err != nil {
			if isStatus(err, 404) {
				continue
			}
			return nil, failure(gmailWords.describe(err, scopeReadonly, false))
		}
		out = append(out, summary{
			ID: msg.ID, Thread: msg.ThreadID, Date: msg.Payload.header("Date"), From: msg.Payload.header("From"),
			Subject: msg.Payload.header("Subject"), Unread: slices.Contains(msg.LabelIDs, "UNREAD"),
			Snippet: html.UnescapeString(msg.Snippet),
		})
	}
	return out, nil
}

func (g *gmail) read(ctx context.Context, id string) (*fullMessage, error) {
	var msg gmailMessage
	err := g.api.call(ctx, "GET", "/messages/"+url.PathEscape(id), url.Values{"format": {"full"}}, nil, &msg)
	var ae *apiError
	if errors.As(err, &ae) && (ae.status == 404 || (ae.status == 400 && strings.Contains(strings.ToLower(ae.message), "invalid id"))) {
		return nil, failure("No message " + id + " in this mailbox.")
	}
	if err != nil {
		return nil, failure(gmailWords.describe(err, scopeReadonly, false))
	}
	all, err := g.allLabels(ctx)
	if err != nil {
		return nil, err
	}
	names := map[string]string{}
	for _, l := range all {
		names[l.ID] = l.Name
	}
	m := &fullMessage{ID: msg.ID, Thread: msg.ThreadID, From: msg.Payload.header("From"), To: msg.Payload.header("To"),
		Cc: msg.Payload.header("Cc"), Date: msg.Payload.header("Date"), Subject: msg.Payload.header("Subject"),
		MessageID: msg.Payload.header("Message-ID"), References: msg.Payload.header("References"), ReplyTo: msg.Payload.header("Reply-To")}
	for _, l := range msg.LabelIDs {
		if n := names[l]; n != "" {
			m.Folders = append(m.Folders, n)
		} else {
			m.Folders = append(m.Folders, l)
		}
	}
	m.Text, _ = gmailFindPart(msg.Payload, "text/plain")
	m.HTML, _ = gmailFindPart(msg.Payload, "text/html")
	for _, a := range gmailAttachments(msg.Payload) {
		m.Attachments = append(m.Attachments, attachment{Name: a.Filename, Type: a.MimeType, Size: int64(a.Body.Size)})
	}
	return m, nil
}

func gmailFindPart(p gmailPart, mimeType string) (string, bool) {
	if strings.EqualFold(p.MimeType, mimeType) && p.Filename == "" && p.Body.Data != "" {
		b, err := base64.URLEncoding.DecodeString(p.Body.Data)
		if err != nil {
			b, err = base64.RawURLEncoding.DecodeString(strings.TrimRight(p.Body.Data, "="))
		}
		if err == nil {
			s := string(b)
			if !utf8.ValidString(s) {
				s = strings.ToValidUTF8(s, "�")
			}
			return strings.ReplaceAll(s, "\r\n", "\n"), true
		}
	}
	for _, c := range p.Parts {
		if s, ok := gmailFindPart(c, mimeType); ok {
			return s, ok
		}
	}
	return "", false
}

func gmailAttachments(p gmailPart) []gmailPart {
	var out []gmailPart
	if p.Filename != "" {
		out = append(out, p)
	}
	for _, c := range p.Parts {
		out = append(out, gmailAttachments(c)...)
	}
	return out
}

func (g *gmail) modify(ctx context.Context, ids []string, add, remove []string) error {
	for _, id := range ids {
		body := map[string][]string{}
		if len(add) > 0 {
			body["addLabelIds"] = add
		}
		if len(remove) > 0 {
			body["removeLabelIds"] = remove
		}
		if err := g.api.call(ctx, "POST", "/messages/"+url.PathEscape(id)+"/modify", nil, body, nil); err != nil {
			if isStatus(err, 404) || isStatus(err, 400) {
				return failure("No message " + id + " in this mailbox.")
			}
			return failure(gmailWords.describe(err, scopeModify, false))
		}
	}
	return nil
}

func (g *gmail) setRead(ctx context.Context, ids []string, read bool) error {
	if read {
		return g.modify(ctx, ids, nil, []string{"UNREAD"})
	}
	return g.modify(ctx, ids, []string{"UNREAD"}, nil)
}

// move gives each message the folder's label and takes it out of the Inbox, which is what moving
// is in Gmail; a move to the Inbox only adds it back.
func (g *gmail) move(ctx context.Context, ids []string, folder string) (map[string]string, error) {
	id, _, err := g.resolveLabel(ctx, folder)
	if err != nil {
		return nil, err
	}
	var remove []string
	if id != "INBOX" {
		remove = []string{"INBOX"}
	}
	return nil, g.modify(ctx, ids, []string{id}, remove)
}

func (g *gmail) label(ctx context.Context, ids []string, label string) error {
	id, _, err := g.resolveLabel(ctx, label)
	if err != nil {
		return err
	}
	return g.modify(ctx, ids, []string{id}, nil)
}

func (g *gmail) draft(ctx context.Context, o outgoing) (string, error) {
	raw := base64.URLEncoding.EncodeToString([]byte(buildMessage(o, false)))
	var draft struct {
		ID      string `json:"id"`
		Message struct {
			ID string `json:"id"`
		} `json:"message"`
	}
	message := map[string]string{"raw": raw}
	if o.Thread != "" {
		// A reply goes into the original's thread; its subject and References keep it there.
		message["threadId"] = o.Thread
	}
	if err := g.api.call(ctx, "POST", "/drafts", nil, map[string]any{"message": message}, &draft); err != nil {
		return "", failure(gmailWords.describe(err, scopeCompose, true))
	}
	return fmt.Sprintf("Draft %s created (message %s); nothing was sent.", draft.ID, draft.Message.ID), nil
}

func (g *gmail) send(ctx context.Context, o outgoing) (string, error) {
	raw := base64.URLEncoding.EncodeToString([]byte(buildMessage(o, false)))
	var sent struct {
		ID       string `json:"id"`
		ThreadID string `json:"threadId"`
	}
	message := map[string]string{"raw": raw}
	if o.Thread != "" {
		message["threadId"] = o.Thread
	}
	if err := g.api.call(ctx, "POST", "/messages/send", nil, message, &sent); err != nil {
		return "", failure(gmailWords.describe(err, scopeCompose, true))
	}
	return fmt.Sprintf("Sent: message %s, thread %s.", sent.ID, sent.ThreadID), nil
}

func (g *gmail) profileHistory(ctx context.Context) (string, error) {
	var p struct {
		HistoryID string `json:"historyId"`
	}
	if err := g.api.call(ctx, "GET", "/profile", nil, nil, &p); err != nil {
		return "", failure(gmailWords.describe(err, scopeReadonly, false))
	}
	if p.HistoryID == "" {
		return "", failure("Gmail's profile gave no historyId, so the watch cannot start.")
	}
	return p.HistoryID, nil
}

func (g *gmail) watchStart(ctx context.Context, w watchSpec) (position, []summary, error) {
	hid, err := g.profileHistory(ctx)
	if err != nil {
		return position{}, nil, err
	}
	pos := position{Gmail: &gmailPosition{HistoryID: hid}}
	if w.lookback <= 0 {
		return pos, nil, nil
	}
	q := query{Folder: w.folder, Unread: w.unreadOnly, Since: time.Now().AddDate(0, 0, -w.lookback), Max: w.max}
	msgs, _, err := g.list(ctx, q)
	if err != nil {
		return position{}, nil, err
	}
	slices.Reverse(msgs) // oldest first
	return pos, msgs, nil
}

func (g *gmail) watchNext(ctx context.Context, w watchSpec, pos position) (position, []summary, string, error) {
	if pos.Gmail == nil || pos.Gmail.HistoryID == "" {
		p, _, err := g.watchStart(ctx, watchSpec{folder: w.folder})
		return p, nil, "The stored position was not Gmail's; the watch starts again from now.", err
	}
	labelID, _, err := g.resolveLabel(ctx, w.folder)
	if err != nil {
		return position{}, nil, "", err
	}
	var ids []string
	seen := map[string]bool{}
	for _, id := range pos.Gmail.Taken {
		seen[id] = true
	}
	taken := pos.Gmail.Taken // what the record being read has already listed
	next := pos.Gmail.HistoryID
	pageToken := ""
	for {
		params := url.Values{"startHistoryId": {pos.Gmail.HistoryID}, "historyTypes": {"messageAdded"},
			"labelId": {labelID}, "maxResults": {strconv.Itoa(min(w.max, 500))}}
		if pageToken != "" {
			params.Set("pageToken", pageToken)
		}
		var h struct {
			History []struct {
				ID            string `json:"id"`
				MessagesAdded []struct {
					Message struct {
						ID       string   `json:"id"`
						LabelIDs []string `json:"labelIds"`
					} `json:"message"`
				} `json:"messagesAdded"`
			} `json:"history"`
			HistoryID     string `json:"historyId"`
			NextPageToken string `json:"nextPageToken"`
		}
		err := g.api.call(ctx, "GET", "/history", params, nil, &h)
		if isStatus(err, 404) {
			// Gmail keeps history for a limited time; a position older than that is gone.
			p, _, err := g.watchStart(ctx, watchSpec{folder: w.folder})
			return p, nil, "Gmail no longer has the history from the stored position; the watch starts again from now, and mail that arrived meanwhile is not listed.", err
		}
		if err != nil {
			return position{}, nil, "", failure(gmailWords.describe(err, scopeReadonly, false))
		}
		for _, rec := range h.History {
			for _, a := range rec.MessagesAdded {
				m := a.Message
				if seen[m.ID] || !slices.Contains(m.LabelIDs, labelID) || slices.Contains(m.LabelIDs, "DRAFT") {
					continue
				}
				if w.unreadOnly && !slices.Contains(m.LabelIDs, "UNREAD") {
					continue
				}
				if len(ids) >= w.max {
					// This record added more than one check may list: the next check reads it
					// again and skips what was taken from it.
					msgs, err := g.summaries(ctx, ids)
					return position{Gmail: &gmailPosition{HistoryID: next, Taken: taken}}, msgs, "", err
				}
				seen[m.ID] = true
				ids = append(ids, m.ID)
				taken = append(taken, m.ID)
			}
			next, taken = rec.ID, nil
			if len(ids) >= w.max {
				// The rest wait for the next check, which starts after this record.
				msgs, err := g.summaries(ctx, ids)
				return position{Gmail: &gmailPosition{HistoryID: next}}, msgs, "", err
			}
		}
		if h.NextPageToken == "" {
			if h.HistoryID != "" {
				next = h.HistoryID
			}
			break
		}
		pageToken = h.NextPageToken
	}
	msgs, err := g.summaries(ctx, ids)
	return position{Gmail: &gmailPosition{HistoryID: next}}, msgs, "", err
}
