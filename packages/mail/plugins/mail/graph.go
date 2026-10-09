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
	api     *restClient
	folders map[string]string // display name, lower case -> id, read once per run
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
	"deleted": "deleteditems", "deleteditems": "deleteditems", "deleted items": "deleteditems", "outbox": "outbox",
}

// folderID finds a folder: a well-known name, else a top-level folder by its display name.
func (g *graph) folderID(ctx context.Context, name string) (string, error) {
	if name == "" {
		return "inbox", nil
	}
	if id, ok := graphWellKnown[strings.ToLower(name)]; ok {
		return id, nil
	}
	if g.folders == nil {
		var v struct {
			Value []struct {
				ID          string `json:"id"`
				DisplayName string `json:"displayName"`
			} `json:"value"`
		}
		if err := g.api.call(ctx, "GET", "/me/mailFolders", url.Values{"$top": {"100"}, "$select": {"id,displayName"}}, nil, &v); err != nil {
			return "", failure(graphWords.describe(err, scopeGraphReadWrite, false))
		}
		g.folders = map[string]string{}
		for _, f := range v.Value {
			g.folders[strings.ToLower(f.DisplayName)] = f.ID
		}
	}
	if id, ok := g.folders[strings.ToLower(name)]; ok {
		return id, nil
	}
	return "", failure("No folder " + strconv.Quote(name) + " in this mailbox.")
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
	Subject          string         `json:"subject"`
	ReceivedDateTime time.Time      `json:"receivedDateTime"`
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
	var v struct {
		Value []graphMessage `json:"value"`
	}
	if err := g.api.call(ctx, "GET", path, params, nil, &v); err != nil {
		if isStatus(err, 404) {
			return nil, "", failure("No folder " + strconv.Quote(q.Folder) + " in this mailbox.")
		}
		return nil, "", failure(graphWords.describe(err, scopeGraphReadWrite, false))
	}
	var out []summary
	for _, m := range v.Value {
		out = append(out, m.summary())
	}
	return out, where, nil
}

func (g *graph) read(ctx context.Context, id string) (*fullMessage, error) {
	var m graphMessage
	params := url.Values{"$select": {"id,conversationId,from,toRecipients,ccRecipients,subject,receivedDateTime,isRead,body,parentFolderId,categories"}}
	if err := g.api.call(ctx, "GET", "/me/messages/"+url.PathEscape(id), params, nil, &m); err != nil {
		if isStatus(err, 404) || isStatus(err, 400) {
			return nil, failure("No message " + id + " in this mailbox.")
		}
		return nil, failure(graphWords.describe(err, scopeGraphReadWrite, false))
	}
	full := &fullMessage{ID: m.ID, Thread: m.ConversationID, From: m.From.String(), To: graphAddresses(m.ToRecipients),
		Cc: graphAddresses(m.CcRecipients), Date: fmtTime(m.ReceivedDateTime), Subject: m.Subject}
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

func (g *graph) draft(ctx context.Context, o outgoing) (string, error) {
	var m graphMessage
	if err := g.api.call(ctx, "POST", "/me/messages", nil, graphOutgoing(o), &m); err != nil {
		return "", failure(graphWords.describe(err, scopeGraphReadWrite, true))
	}
	return fmt.Sprintf("Draft %s created in Drafts; nothing was sent.", m.ID), nil
}

func (g *graph) send(ctx context.Context, o outgoing) (string, error) {
	body := map[string]any{"message": graphOutgoing(o), "saveToSentItems": true}
	if err := g.api.call(ctx, "POST", "/me/sendMail", nil, body, nil); err != nil {
		return "", failure(graphWords.describe(err, scopeGraphSend, true))
	}
	return "Sent: Microsoft Graph accepted the message; a copy is in Sent Items.", nil
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
