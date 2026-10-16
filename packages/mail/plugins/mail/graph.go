package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	// graphBaseEnv points the plugin at a fake Graph. Only the tests set it, as with Gmail's.
	graphBaseEnv     = "MAIL_PLUGIN_TEST_GRAPH_BASE"
	defaultGraphBase = "https://graph.microsoft.com"

	scopeGraphReadWrite = "https://graph.microsoft.com/Mail.ReadWrite"
	scopeGraphSend      = "https://graph.microsoft.com/Mail.Send"

	wordsReconnectMicrosoft = "The Microsoft connection needs reconnecting in Admin, Connections."

	graphSummaryFields = "id,conversationId,from,subject,receivedDateTime,isRead,bodyPreview"
	graphRecentKeep    = 200
)

var graphWords = httpWords{provider: "Microsoft Graph", reconnect: wordsReconnectMicrosoft}

// graph is the mailbox over Microsoft Graph, with a Microsoft connection's access token.
type graph struct {
	api *restClient
	top []graphFolder // the top-level folders, read once per run
}

func newGraph(token string) *graph {
	base := defaultGraphBase
	if v := os.Getenv(graphBaseEnv); v != "" {
		base = v
	}
	return &graph{api: newRESTClient(strings.TrimRight(base, "/")+"/v1.0", token)}
}

func (g *graph) name() string { return "Microsoft Graph" }
func (g *graph) close()       {}

// graphWellKnown are the folder names Graph takes in place of an id.
var graphWellKnown = map[string]string{
	"inbox": "inbox", "drafts": "drafts", "sent": "sentitems", "sentitems": "sentitems", "sent items": "sentitems",
	"archive": "archive", "junk": "junkemail", "junkemail": "junkemail", "junk email": "junkemail",
	"deleted": "deleteditems", "deleteditems": "deleteditems", "deleted items": "deleteditems", "trash": "deleteditems", "outbox": "outbox",
}

type graphFolder struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
}

// folderID finds a folder: a well-known name, else a top-level folder by its display name, else
// a nested one by its path of display names.
func (g *graph) folderID(ctx context.Context, name string) (string, error) {
	id, found, err := g.findFolder(ctx, name)
	if err == nil && !found {
		err = failure("No folder " + strconv.Quote(name) + " in this mailbox.")
	}
	return id, err
}

func (g *graph) topFolders(ctx context.Context) ([]graphFolder, error) {
	if g.top != nil {
		return g.top, nil
	}
	top, err := graphPages[graphFolder](ctx, g, "/me/mailFolders", url.Values{"$top": {"100"}, "$select": {"id,displayName"}}, "folders", 0)
	if err != nil {
		return nil, graphFailure(err)
	}
	g.top = append([]graphFolder{}, top...)
	return g.top, nil
}

const (
	// graphMaxPages and graphMaxEntries bound how far one collection is read through its
	// nextLinks; past either the command fails, saying how far it read, rather than going on with
	// part of the list.
	graphMaxPages   = 50
	graphMaxEntries = 5000
)

// graphPages reads a collection page by page, following @odata.nextLink until there is none, or
// until it holds want entries when want is above 0. what names the entries in the failure words.
func graphPages[T any](ctx context.Context, g *graph, path string, params url.Values, what string, want int) ([]T, error) {
	var out []T
	link, pages := path, 0
	for {
		var page struct {
			Value    []T    `json:"value"`
			NextLink string `json:"@odata.nextLink"`
		}
		if err := g.api.call(ctx, "GET", link, params, nil, &page); err != nil {
			return nil, err
		}
		pages++
		out = append(out, page.Value...)
		if page.NextLink == "" || (want > 0 && len(out) >= want) {
			if len(out) > graphMaxEntries {
				return nil, graphTooMany(what, pages, len(out))
			}
			return out, nil
		}
		if pages >= graphMaxPages || len(out) >= graphMaxEntries {
			return nil, graphTooMany(what, pages, len(out))
		}
		// The nextLink carries the query; asking it again would repeat it.
		link, params = page.NextLink, nil
	}
}

func graphTooMany(what string, pages, entries int) error {
	return failure(fmt.Sprintf("Microsoft Graph still had more %s after %d pages and %d entries, more than Mailer reads in one go (%d pages or %d entries), so it stopped there; nothing was changed.",
		what, pages, entries, graphMaxPages, graphMaxEntries))
}

// graphFailure is a call's error in the run's words; a failure already worded passes through.
func graphFailure(err error) error {
	if _, ok := err.(failure); ok {
		return err
	}
	return failure(graphWords.describe(err, scopeGraphReadWrite, false))
}

// findFolder is folderID answering found false for a folder the mailbox does not have.
func (g *graph) findFolder(ctx context.Context, name string) (string, bool, error) {
	if name == "" {
		return "inbox", true, nil
	}
	if id, ok := graphWellKnown[strings.ToLower(name)]; ok {
		return id, true, nil
	}
	top, err := g.topFolders(ctx)
	if err != nil {
		return "", false, err
	}
	match := func(list []graphFolder, want string) (string, bool) {
		for _, f := range list {
			if strings.EqualFold(f.DisplayName, want) {
				return f.ID, true
			}
		}
		return "", false
	}
	if id, ok := match(top, name); ok {
		return id, true, nil
	}
	path := strings.Split(name, "/")
	if len(path) < 2 {
		return "", false, nil
	}
	// A path is resolved level by level, each level's children read through all their pages.
	id, ok := match(top, path[0])
	if wk, known := graphWellKnown[strings.ToLower(path[0])]; !ok && known {
		id, ok = wk, true
	}
	for _, part := range path[1:] {
		if !ok {
			return "", false, nil
		}
		children, err := graphPages[graphFolder](ctx, g, "/me/mailFolders/"+url.PathEscape(id)+"/childFolders",
			url.Values{"$top": {"100"}, "$select": {"id,displayName"}}, "folders in "+strconv.Quote(name), 0)
		if isStatus(err, 404) {
			return "", false, nil
		}
		if err != nil {
			return "", false, graphFailure(err)
		}
		id, ok = match(children, part)
	}
	return id, ok, nil
}

func (g *graph) hasFolder(ctx context.Context, name string) (bool, error) {
	_, found, err := g.findFolder(ctx, name)
	return found, err
}

// labelUsesFolder is false: Graph's labels are categories on the message.
func (g *graph) labelUsesFolder(context.Context) (bool, error) { return false, nil }

// graphRoles are the well-known folders the folders command names a role for.
var graphRoles = [][2]string{{"inbox", "inbox"}, {"drafts", "drafts"}, {"sentitems", "sent"}, {"junkemail", "junk"},
	{"deleteditems", "trash"}, {"archive", "archive"}, {"outbox", "outbox"}}

// folders answers the top-level folders, each well-known one with its role.
func (g *graph) folders(ctx context.Context) ([]mailFolder, string, error) {
	top, err := g.topFolders(ctx)
	if err != nil {
		return nil, "", err
	}
	roles := map[string]string{}
	for _, r := range graphRoles {
		var f graphFolder
		// A mailbox without one of them answers 404; the role is only shown, so any failure leaves it out.
		if g.api.call(ctx, "GET", "/me/mailFolders/"+r[0], url.Values{"$select": {"id"}}, nil, &f) == nil && f.ID != "" {
			roles[f.ID] = r[1]
		}
	}
	var out []mailFolder
	for _, f := range top {
		out = append(out, mailFolder{Name: f.DisplayName, Role: roles[f.ID]})
	}
	return out, "/", nil
}

// createFolder creates a mail folder at the top level of the mailbox. Graph answers 409
// ErrorFolderExists when the folder was made meanwhile.
func (g *graph) createFolder(ctx context.Context, name string) error {
	err := g.api.call(ctx, "POST", "/me/mailFolders", nil, map[string]any{"displayName": name}, nil)
	if e, ok := err.(*apiError); ok && (e.reason == "ErrorFolderExists" || e.status == 409) {
		err = nil
	}
	if err != nil {
		return failure(graphWords.describe(err, scopeGraphReadWrite, false) + " The folder " + strconv.Quote(name) + " was not created; nothing was changed.")
	}
	g.top = nil
	return nil
}

type graphAddress struct {
	EmailAddress struct {
		Name    string `json:"name"`
		Address string `json:"address"`
	} `json:"emailAddress"`
}

func (a *graphAddress) String() string {
	if a == nil {
		return ""
	}
	if a.EmailAddress.Name != "" && a.EmailAddress.Name != a.EmailAddress.Address {
		return a.EmailAddress.Name + " <" + a.EmailAddress.Address + ">"
	}
	return a.EmailAddress.Address
}

func graphAddresses(list []graphAddress) string {
	var out []string
	for i := range list {
		out = append(out, list[i].String())
	}
	return strings.Join(out, ", ")
}

type graphMessage struct {
	ID               string         `json:"id"`
	ConversationID   string         `json:"conversationId"`
	From             *graphAddress  `json:"from"`
	ToRecipients     []graphAddress `json:"toRecipients"`
	CcRecipients     []graphAddress `json:"ccRecipients"`
	ReplyTo          []graphAddress `json:"replyTo"`
	Subject          string         `json:"subject"`
	ReceivedDateTime time.Time      `json:"receivedDateTime"`
	SentDateTime     time.Time      `json:"sentDateTime"`
	InternetMsgID    string         `json:"internetMessageId"`
	IsRead           bool           `json:"isRead"`
	IsDraft          bool           `json:"isDraft"`
	BodyPreview      string         `json:"bodyPreview"`
	ParentFolderID   string         `json:"parentFolderId"`
	Categories       []string       `json:"categories"`
	Body             *struct {
		ContentType string `json:"contentType"`
		Content     string `json:"content"`
	} `json:"body"`
	Removed *struct{} `json:"@removed"`
}

func (m graphMessage) summary() summary {
	return summary{ID: m.ID, Thread: m.ConversationID, From: m.From.String(), Subject: m.Subject,
		Date: fmtTime(m.ReceivedDateTime), Unread: !m.IsRead, Snippet: m.BodyPreview}
}

// odataQuote writes a string literal for $filter.
func odataQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func (g *graph) list(ctx context.Context, q query) ([]summary, string, error) {
	params := url.Values{"$top": {strconv.Itoa(q.Max)}, "$select": {graphSummaryFields}}
	folder, where := "inbox", "in Inbox"
	if q.Raw != "" {
		// Graph's own search language (KQL); it cannot be ordered, so Graph's relevance order stands.
		params.Set("$search", `"`+strings.ReplaceAll(q.Raw, `"`, `\"`)+`"`)
		where = "matching the search"
	} else {
		id, err := g.folderID(ctx, q.Folder)
		if err != nil {
			return nil, "", err
		}
		folder = id
		if q.Folder != "" {
			where = "in " + q.Folder
		}
		// Graph orders by receivedDateTime only when the filter names it first.
		since := q.Since
		if since.IsZero() {
			since = time.Date(1900, 1, 1, 0, 0, 0, 0, time.UTC)
		}
		filter := []string{"receivedDateTime ge " + since.UTC().Format(time.RFC3339)}
		if q.Unread {
			filter = append(filter, "isRead eq false")
		}
		if q.From != "" {
			if strings.Contains(q.From, "@") && !strings.HasPrefix(q.From, "@") {
				filter = append(filter, "from/emailAddress/address eq "+odataQuote(strings.ToLower(q.From)))
			} else {
				filter = append(filter, "contains(from/emailAddress/address,"+odataQuote(strings.ToLower(q.From))+")")
			}
		}
		if q.Subject != "" {
			filter = append(filter, "contains(subject,"+odataQuote(q.Subject)+")")
		}
		params.Set("$filter", strings.Join(filter, " and "))
		params.Set("$orderby", "receivedDateTime desc")
	}
	path := "/me/mailFolders/" + url.PathEscape(folder) + "/messages"
	if q.AnyFolder || q.Raw != "" {
		path = "/me/messages"
		if q.Raw == "" {
			where = "in any folder"
		}
	}
	msgs, err := graphPages[graphMessage](ctx, g, path, params, "messages", q.Max)
	if err != nil {
		if isStatus(err, 404) {
			return nil, "", failure("No folder " + strconv.Quote(q.Folder) + " in this mailbox.")
		}
		return nil, "", graphFailure(err)
	}
	var out []summary
	for _, m := range msgs {
		out = append(out, m.summary())
	}
	return out, where, nil
}

func (g *graph) read(ctx context.Context, id string) (*fullMessage, error) {
	var m graphMessage
	params := url.Values{"$select": {"id,conversationId,internetMessageId,from,toRecipients,ccRecipients,replyTo,subject,receivedDateTime,isRead,body,parentFolderId,categories"}}
	if err := g.api.call(ctx, "GET", "/me/messages/"+url.PathEscape(id), params, nil, &m); err != nil {
		if isStatus(err, 404) || isStatus(err, 400) {
			return nil, failure("No message " + id + " in this mailbox.")
		}
		return nil, failure(graphWords.describe(err, scopeGraphReadWrite, false))
	}
	full := &fullMessage{ID: m.ID, Thread: m.ConversationID, From: m.From.String(), To: graphAddresses(m.ToRecipients),
		Cc: graphAddresses(m.CcRecipients), ReplyTo: graphAddresses(m.ReplyTo), Date: fmtTime(m.ReceivedDateTime), Subject: m.Subject,
		MessageID: m.InternetMsgID, Arrived: m.ReceivedDateTime}
	if m.ParentFolderID != "" {
		full.Folders = append(full.Folders, g.folderName(ctx, m.ParentFolderID))
	}
	full.Folders = append(full.Folders, m.Categories...)
	if m.Body != nil {
		if strings.EqualFold(m.Body.ContentType, "html") {
			full.HTML = m.Body.Content
		} else {
			full.Text = m.Body.Content
		}
	}
	var atts struct {
		Value []struct {
			Name        string `json:"name"`
			ContentType string `json:"contentType"`
			Size        int64  `json:"size"`
		} `json:"value"`
	}
	// Only the names, types and sizes: the content is never asked for.
	if err := g.api.call(ctx, "GET", "/me/messages/"+url.PathEscape(id)+"/attachments", url.Values{"$select": {"name,contentType,size"}}, nil, &atts); err != nil {
		return nil, failure(graphWords.describe(err, scopeGraphReadWrite, false))
	}
	for _, a := range atts.Value {
		full.Attachments = append(full.Attachments, attachment{Name: a.Name, Type: a.ContentType, Size: a.Size})
	}
	return full, nil
}

// folderName is a folder's display name, or its id when the folder cannot be read.
func (g *graph) folderName(ctx context.Context, id string) string {
	var f struct {
		DisplayName string `json:"displayName"`
	}
	if g.api.call(ctx, "GET", "/me/mailFolders/"+url.PathEscape(id), url.Values{"$select": {"displayName"}}, nil, &f) != nil || f.DisplayName == "" {
		return id
	}
	return f.DisplayName
}

func (g *graph) patch(ctx context.Context, ids []string, body map[string]any) error {
	for _, id := range ids {
		if err := g.api.call(ctx, "PATCH", "/me/messages/"+url.PathEscape(id), nil, body, nil); err != nil {
			if isStatus(err, 404) || isStatus(err, 400) {
				return failure("No message " + id + " in this mailbox.")
			}
			return failure(graphWords.describe(err, scopeGraphReadWrite, false))
		}
	}
	return nil
}

func (g *graph) setRead(ctx context.Context, ids []string, read bool) error {
	return g.patch(ctx, ids, map[string]any{"isRead": read})
}

func (g *graph) move(ctx context.Context, ids []string, folder string) (map[string]string, error) {
	dest, err := g.folderID(ctx, folder)
	if err != nil {
		return nil, err
	}
	moved := map[string]string{}
	for _, id := range ids {
		var m graphMessage
		if err := g.api.call(ctx, "POST", "/me/messages/"+url.PathEscape(id)+"/move", nil, map[string]string{"destinationId": dest}, &m); err != nil {
			if isStatus(err, 404) || isStatus(err, 400) {
				return moved, failure("No message " + id + " in this mailbox.")
			}
			return moved, failure(graphWords.describe(err, scopeGraphReadWrite, false))
		}
		if m.ID != "" && m.ID != id {
			moved[id] = m.ID
		}
	}
	return moved, nil
}

// label adds an Outlook category: Graph's labels.
func (g *graph) label(ctx context.Context, ids []string, label string) error {
	for _, id := range ids {
		var m graphMessage
		if err := g.api.call(ctx, "GET", "/me/messages/"+url.PathEscape(id), url.Values{"$select": {"categories"}}, nil, &m); err != nil {
			if isStatus(err, 404) || isStatus(err, 400) {
				return failure("No message " + id + " in this mailbox.")
			}
			return failure(graphWords.describe(err, scopeGraphReadWrite, false))
		}
		if slices.Contains(m.Categories, label) {
			continue
		}
		if err := g.patch(ctx, []string{id}, map[string]any{"categories": append(m.Categories, label)}); err != nil {
			return err
		}
	}
	return nil
}

func graphRecipients(addrs []string) []map[string]any {
	out := []map[string]any{}
	for _, a := range addrs {
		out = append(out, map[string]any{"emailAddress": map[string]string{"address": a}})
	}
	return out
}

func graphOutgoing(o outgoing) map[string]any {
	// Graph takes one body: the HTML when there is one (Outlook makes its own text part), else the text.
	body := map[string]string{"contentType": "Text", "content": o.Body}
	if o.HTML != "" {
		body = map[string]string{"contentType": "HTML", "content": o.HTML}
	}
	return map[string]any{
		"subject":       o.Subject,
		"body":          body,
		"toRecipients":  graphRecipients(o.To),
		"ccRecipients":  graphRecipients(o.Cc),
		"bccRecipients": graphRecipients(o.Bcc),
	}
}

// reply makes a reply draft as Outlook does, with createReply (createReplyAll when the person
// asked for all), so Graph threads it into the original's conversation; then sets its recipients
// to the ones checked against the allowlist, and its subject and body to the reply's: the body
// replaces the quote Outlook puts in the draft.
func (g *graph) reply(ctx context.Context, o outgoing) (string, error) {
	action := "/createReply"
	if o.ReplyAll {
		action = "/createReplyAll"
	}
	var d graphMessage
	if err := g.api.call(ctx, "POST", "/me/messages/"+url.PathEscape(o.ReplyOf)+action, nil, map[string]any{}, &d); err != nil {
		if isStatus(err, 404) {
			return "", failure("No message " + o.ReplyOf + " in this mailbox; nothing was sent.")
		}
		return "", failure(graphWords.describe(err, scopeGraphReadWrite, true))
	}
	if err := g.api.call(ctx, "PATCH", "/me/messages/"+url.PathEscape(d.ID), nil, graphOutgoing(o), nil); err != nil {
		return "", failure(strings.TrimSuffix(graphWords.describe(err, scopeGraphReadWrite, false), ".") +
			"; nothing was sent, and the unfinished reply " + d.ID + " is left in Drafts.")
	}
	return d.ID, nil
}

func (g *graph) draft(ctx context.Context, o outgoing) (string, made, error) {
	if o.ReplyOf != "" {
		id, err := g.reply(ctx, o)
		if err != nil {
			return "", made{}, err
		}
		return fmt.Sprintf("Draft %s created in Drafts, in the original's conversation; nothing was sent.", id), made{ID: id, Thread: o.Thread}, nil
	}
	var m graphMessage
	if err := g.api.call(ctx, "POST", "/me/messages", nil, graphOutgoing(o), &m); err != nil {
		return "", made{}, failure(graphWords.describe(err, scopeGraphReadWrite, true))
	}
	return fmt.Sprintf("Draft %s created in Drafts; nothing was sent.", m.ID), made{ID: m.ID, Thread: m.ConversationID}, nil
}

func (g *graph) send(ctx context.Context, o outgoing) (string, made, error) {
	if o.ReplyOf != "" {
		id, err := g.reply(ctx, o)
		if err != nil {
			return "", made{}, err
		}
		if err := g.api.call(ctx, "POST", "/me/messages/"+url.PathEscape(id)+"/send", nil, nil, nil); err != nil {
			return "", made{}, failure(strings.TrimSuffix(graphWords.describe(err, scopeGraphSend, false), ".") + "; nothing was sent, and the reply is left as draft " + id + ".")
		}
		// Sending moves the draft to Sent Items under a new id Graph does not answer, so only the
		// conversation is known.
		return "Sent: Microsoft Graph sent the reply in the original's conversation; a copy is in Sent Items.", made{Thread: o.Thread}, nil
	}
	body := map[string]any{"message": graphOutgoing(o), "saveToSentItems": true}
	if err := g.api.call(ctx, "POST", "/me/sendMail", nil, body, nil); err != nil {
		return "", made{}, failure(graphWords.describe(err, scopeGraphSend, true))
	}
	// sendMail answers no id and no conversation.
	return "Sent: Microsoft Graph accepted the message; a copy is in Sent Items.", made{}, nil
}

// subjects fetches only each message's subject.
func (g *graph) subjects(ctx context.Context, ids []string) map[string]string {
	out := map[string]string{}
	for _, id := range ids {
		var m graphMessage
		if g.api.call(ctx, "GET", "/me/messages/"+url.PathEscape(id), url.Values{"$select": {"subject"}}, nil, &m) == nil {
			out[id] = m.Subject
		}
	}
	return out
}

func recentKey(id string) string {
	h := sha256.Sum256([]byte(id))
	return hex.EncodeToString(h[:6])
}

// watchStart runs a delta round over messages received from the look-back on: with no look-back
// that is none, so the round ends at once with a deltaLink that sees only what arrives later.
func (g *graph) watchStart(ctx context.Context, w watchSpec) (position, []summary, error) {
	folder, err := g.folderID(ctx, w.folder)
	if err != nil {
		return position{}, nil, err
	}
	since := time.Now().UTC().Truncate(time.Second).AddDate(0, 0, -max(w.lookback, 0))
	start := position{Graph: &graphPosition{Since: since}}
	params := url.Values{"$select": {graphSummaryFields + ",isDraft"}, "$filter": {"receivedDateTime ge " + since.Format(time.RFC3339)}}
	start.Graph.Link = "/me/mailFolders/" + url.PathEscape(folder) + "/messages/delta?" + params.Encode()
	next, msgs, _, err := g.watchNext(ctx, w, start)
	return next, msgs, err
}

func (g *graph) watchNext(ctx context.Context, w watchSpec, pos position) (position, []summary, string, error) {
	if pos.Graph == nil || pos.Graph.Link == "" {
		p, _, err := g.watchStart(ctx, watchSpec{folder: w.folder, max: w.max})
		return p, nil, "The stored position was not Microsoft Graph's; the watch starts again from now.", err
	}
	cur := *pos.Graph
	recent := map[string]bool{}
	for _, k := range cur.Recent {
		recent[k] = true
	}
	var out []summary
	link, full := cur.Link, false
	for {
		var page struct {
			Value     []graphMessage `json:"value"`
			NextLink  string         `json:"@odata.nextLink"`
			DeltaLink string         `json:"@odata.deltaLink"`
		}
		err := g.api.call(ctx, "GET", link, nil, nil, &page, "Prefer", "odata.maxpagesize="+strconv.Itoa(w.max))
		if isStatus(err, 410) || (err != nil && isGraphSyncReset(err)) {
			p, _, err := g.watchStart(ctx, watchSpec{folder: w.folder, max: w.max})
			return p, nil, "Microsoft Graph no longer knows the stored position; the watch starts again from now, and mail that arrived meanwhile is not listed.", err
		}
		if err != nil {
			if _, ok := err.(*apiError); !ok {
				if _, ok := err.(unreachable); !ok {
					p, _, err := g.watchStart(ctx, watchSpec{folder: w.folder, max: w.max})
					return p, nil, "The stored position could not be used; the watch starts again from now.", err
				}
			}
			return position{}, nil, "", failure(graphWords.describe(err, scopeGraphReadWrite, false))
		}
		for _, m := range page.Value {
			k := recentKey(m.ID)
			if m.Removed != nil || m.IsDraft || recent[k] || m.ReceivedDateTime.Before(cur.Since) {
				continue
			}
			if len(out) >= w.max {
				// The server sent more than maxpagesize: the next check reads this page again
				// and skips what it lists now, which Recent remembers.
				cur.Link, full = link, true
				break
			}
			recent[k] = true
			cur.Recent = append(cur.Recent, k)
			if w.unreadOnly && m.IsRead {
				continue
			}
			out = append(out, m.summary())
		}
		if len(cur.Recent) > graphRecentKeep {
			cur.Recent = cur.Recent[len(cur.Recent)-graphRecentKeep:]
		}
		switch {
		case full:
			return position{Graph: &cur}, out, "", nil
		case page.DeltaLink != "":
			cur.Link = page.DeltaLink
			return position{Graph: &cur}, out, "", nil
		case page.NextLink != "" && len(out) > 0:
			// A page holds at most max messages; the rest of the round waits for the next check.
			cur.Link = page.NextLink
			return position{Graph: &cur}, out, "", nil
		case page.NextLink != "":
			link = page.NextLink
		default:
			return position{Graph: &cur}, out, "", nil
		}
	}
}

func isGraphSyncReset(err error) bool {
	e, ok := err.(*apiError)
	return ok && (strings.Contains(strings.ToLower(e.reason), "syncstate") || strings.Contains(strings.ToLower(e.reason), "resyncrequired"))
}

// replied looks in Sent Items for a message of the original's conversation sent after it arrived.
func (g *graph) replied(ctx context.Context, o original) (*sentReply, error) {
	if o.Thread == "" {
		return nil, failure("The message has no conversation, so Mailer cannot tell whether it was replied to.")
	}
	params := url.Values{"$filter": {"conversationId eq " + odataQuote(o.Thread)}, "$select": {"id,conversationId,subject,sentDateTime,isDraft"}, "$top": {"50"}}
	msgs, err := graphPages[graphMessage](ctx, g, "/me/mailFolders/sentitems/messages", params, "sent messages in the conversation", 0)
	if err != nil {
		return nil, graphFailure(err)
	}
	var first *sentReply
	for _, m := range msgs {
		if m.IsDraft || m.ConversationID != o.Thread || !m.SentDateTime.After(o.Arrived) || (first != nil && !m.SentDateTime.Before(first.Sent)) {
			continue
		}
		first = &sentReply{ID: m.ID, Subject: m.Subject, Sent: m.SentDateTime}
	}
	return first, nil
}
