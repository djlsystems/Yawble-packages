package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const graphToken = "EwBwA8l6BAAUgraph-secret-test-access-token"

// fakeGraph is a small Microsoft Graph mailbox: folders, messages, a change counter for delta,
// and every request kept. fail answers a canned error for a method and path prefix.
type fakeGraph struct {
	t       *testing.T
	srv     *httptest.Server
	mu      sync.Mutex
	msgs    []*gMsg
	seq     int
	nextID  int
	folders map[string]string // id -> display name
	calls   []call
	fail    map[string]func(w http.ResponseWriter)
	once    map[string]func(w http.ResponseWriter)
	sent    []map[string]any
	// ignoreLimits answers as a server that ignores $top and odata.maxpagesize would.
	ignoreLimits bool
}

type gMsg struct {
	ID, Conv, From, FromName, Subject, Body, BodyType, Folder string
	To                                                        []string
	Received                                                  time.Time
	IsRead, IsDraft                                           bool
	Categories                                                []string
	Atts                                                      []attachment
	seq                                                       int
}

func newFakeGraph(t *testing.T) *fakeGraph {
	t.Helper()
	f := &fakeGraph{t: t, folders: map[string]string{"inbox": "Inbox", "drafts": "Drafts", "sentitems": "Sent Items", "AQMk-archive": "Archive", "AQMk-receipts": "Receipts"},
		fail: map[string]func(http.ResponseWriter){}, once: map[string]func(http.ResponseWriter){}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	t.Setenv(graphBaseEnv, f.srv.URL)
	return f
}

func (f *fakeGraph) conn() map[string]any {
	return map[string]any{"provider": "microsoft", "account": "person@contoso.test", "accessToken": graphToken,
		"expiresAt": "2026-10-07T20:00:00Z", "scopes": []string{scopeGraphReadWrite, scopeGraphSend, "offline_access"}}
}

func (f *fakeGraph) failWith(method, prefix string, status int, body string) {
	f.fail[method+" "+prefix] = func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

// failOnce answers the canned error to the next matching request only.
func (f *fakeGraph) failOnce(method, prefix string, status int, body string) {
	f.once[method+" "+prefix] = func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

// add puts a message in a folder ("inbox" by default) as arrival would, and answers its id.
func (f *fakeGraph) add(from, subject, body string, received time.Time, read bool) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	f.seq++
	m := &gMsg{ID: fmt.Sprintf("AAMkAGI2THVSAAA%04d=", f.nextID), Conv: fmt.Sprintf("AAQkConv%04d=", f.nextID), From: from, FromName: "Sender " + strconv.Itoa(f.nextID),
		Subject: subject, Body: body, BodyType: "html", Folder: "inbox", To: []string{"person@contoso.test"}, Received: received.UTC().Truncate(time.Second), IsRead: read, seq: f.seq}
	f.msgs = append(f.msgs, m)
	return m.ID
}

func (f *fakeGraph) byID(id string) *gMsg {
	for _, m := range f.msgs {
		if m.ID == id {
			return m
		}
	}
	return nil
}

func (f *fakeGraph) called(method, prefix string) []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []call
	for _, c := range f.calls {
		if (method == "" || c.Method == method) && strings.HasPrefix(c.Path, "/v1.0"+prefix) {
			out = append(out, c)
		}
	}
	return out
}

func (m *gMsg) json(full bool) map[string]any {
	preview := m.Body
	if len(preview) > 60 {
		preview = preview[:60]
	}
	out := map[string]any{"id": m.ID, "conversationId": m.Conv, "subject": m.Subject, "receivedDateTime": m.Received.Format(time.RFC3339),
		"isRead": m.IsRead, "isDraft": m.IsDraft, "bodyPreview": preview, "parentFolderId": m.Folder, "categories": m.Categories,
		"from": map[string]any{"emailAddress": map[string]string{"name": m.FromName, "address": m.From}}}
	if full {
		var to []any
		for _, a := range m.To {
			to = append(to, map[string]any{"emailAddress": map[string]string{"name": "", "address": a}})
		}
		out["toRecipients"] = to
		out["ccRecipients"] = []any{}
		out["body"] = map[string]string{"contentType": m.BodyType, "content": m.Body}
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func graphNotFound(w http.ResponseWriter) {
	writeJSON(w, 404, map[string]any{"error": map[string]string{"code": "ErrorItemNotFound", "message": "The specified object was not found in the store."}})
}

// matches applies the few $filter clauses the plugin writes.
func graphMatches(m *gMsg, filter string) bool {
	if filter == "" {
		return true
	}
	for _, c := range strings.Split(filter, " and ") {
		c = strings.TrimSpace(c)
		switch {
		case strings.HasPrefix(c, "receivedDateTime ge "):
			t, _ := time.Parse(time.RFC3339, strings.TrimPrefix(c, "receivedDateTime ge "))
			if m.Received.Before(t) {
				return false
			}
		case c == "isRead eq false":
			if m.IsRead {
				return false
			}
		case strings.HasPrefix(c, "from/emailAddress/address eq "):
			if !strings.EqualFold(m.From, strings.Trim(strings.TrimPrefix(c, "from/emailAddress/address eq "), "'")) {
				return false
			}
		case strings.HasPrefix(c, "contains(from/emailAddress/address,"):
			v := strings.Trim(strings.TrimSuffix(strings.TrimPrefix(c, "contains(from/emailAddress/address,"), ")"), "'")
			if !strings.Contains(strings.ToLower(m.From), strings.ToLower(v)) {
				return false
			}
		case strings.HasPrefix(c, "contains(subject,"):
			v := strings.Trim(strings.TrimSuffix(strings.TrimPrefix(c, "contains(subject,"), ")"), "'")
			if !strings.Contains(strings.ToLower(m.Subject), strings.ToLower(v)) {
				return false
			}
		default:
			panic("fake Graph cannot read the filter clause " + c)
		}
	}
	return true
}

// folderOf takes a folder id or one of Graph's well-known names.
func (f *fakeGraph) folderOf(id string) (string, bool) {
	if id == "archive" {
		id = "AQMk-archive"
	}
	if id == "" {
		return "", false
	}
	_, ok := f.folders[id]
	return id, ok
}

func (f *fakeGraph) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call{r.Method, r.URL.Path, r.URL.RawQuery, body})
	if r.Header.Get("Authorization") != "Bearer "+graphToken {
		writeJSON(w, 401, map[string]any{"error": map[string]string{"code": "InvalidAuthenticationToken", "message": "Access token is empty."}})
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/v1.0")
	for k, h := range f.once {
		method, prefix, _ := strings.Cut(k, " ")
		if method == r.Method && strings.HasPrefix(path, prefix) {
			delete(f.once, k)
			h(w)
			return
		}
	}
	for k, h := range f.fail {
		method, prefix, _ := strings.Cut(k, " ")
		if method == r.Method && strings.HasPrefix(path, prefix) {
			h(w)
			return
		}
	}
	q := r.URL.Query()
	parts := strings.Split(strings.Trim(path, "/"), "/")
	switch {
	case r.Method == "GET" && path == "/me/mailFolders":
		var v []any
		for id, name := range f.folders {
			v = append(v, map[string]string{"id": id, "displayName": name})
		}
		writeJSON(w, 200, map[string]any{"value": v})
	case r.Method == "GET" && len(parts) == 3 && parts[1] == "mailFolders":
		if _, ok := f.folderOf(parts[2]); !ok {
			graphNotFound(w)
			return
		}
		id, _ := f.folderOf(parts[2])
		writeJSON(w, 200, map[string]any{"id": id, "displayName": f.folders[id]})
	case r.Method == "GET" && len(parts) == 4 && parts[1] == "mailFolders" && parts[3] == "messages":
		folder, ok := f.folderOf(parts[2])
		if !ok {
			graphNotFound(w)
			return
		}
		f.listMessages(w, q, func(m *gMsg) bool { return m.Folder == folder })
	case r.Method == "GET" && path == "/me/messages":
		f.listMessages(w, q, func(*gMsg) bool { return true })
	case r.Method == "GET" && len(parts) == 5 && parts[3] == "messages" && parts[4] == "delta":
		f.delta(w, r, parts[2])
	case len(parts) >= 3 && parts[1] == "messages":
		m := f.byID(parts[2])
		if m == nil {
			graphNotFound(w)
			return
		}
		switch {
		case r.Method == "GET" && len(parts) == 3:
			writeJSON(w, 200, m.json(true))
		case r.Method == "GET" && len(parts) == 4 && parts[3] == "attachments":
			if q.Get("$select") != "name,contentType,size" {
				writeJSON(w, 400, map[string]any{"error": map[string]string{"code": "BadRequest", "message": "the fake wants only names, types and sizes"}})
				return
			}
			var v []any
			for _, a := range m.Atts {
				v = append(v, map[string]any{"name": a.Name, "contentType": a.Type, "size": a.Size})
			}
			writeJSON(w, 200, map[string]any{"value": v})
		case r.Method == "PATCH" && len(parts) == 3:
			var p map[string]json.RawMessage
			_ = json.Unmarshal(body, &p)
			if v, ok := p["isRead"]; ok {
				_ = json.Unmarshal(v, &m.IsRead)
			}
			if v, ok := p["categories"]; ok {
				_ = json.Unmarshal(v, &m.Categories)
			}
			f.seq++
			m.seq = f.seq
			writeJSON(w, 200, m.json(false))
		case r.Method == "POST" && len(parts) == 4 && parts[3] == "move":
			var p struct {
				DestinationID string `json:"destinationId"`
			}
			_ = json.Unmarshal(body, &p)
			dest, ok := f.folderOf(p.DestinationID)
			if !ok {
				graphNotFound(w)
				return
			}
			f.nextID++
			f.seq++
			m.Folder, m.ID, m.seq = dest, fmt.Sprintf("AAMkAGI2MOVED%04d=", f.nextID), f.seq
			writeJSON(w, 201, m.json(false))
		default:
			graphNotFound(w)
		}
	case r.Method == "POST" && path == "/me/messages":
		var p map[string]any
		_ = json.Unmarshal(body, &p)
		f.nextID++
		subject, _ := p["subject"].(string)
		m := &gMsg{ID: fmt.Sprintf("AAMkDRAFT%04d=", f.nextID), Subject: subject, Folder: "drafts", IsDraft: true, IsRead: true, Received: time.Now().UTC()}
		f.msgs = append(f.msgs, m)
		writeJSON(w, 201, m.json(false))
	case r.Method == "POST" && path == "/me/sendMail":
		var p map[string]any
		_ = json.Unmarshal(body, &p)
		f.sent = append(f.sent, p)
		w.WriteHeader(202)
	default:
		graphNotFound(w)
	}
}

func (f *fakeGraph) listMessages(w http.ResponseWriter, q url.Values, in func(*gMsg) bool) {
	var list []*gMsg
	search := strings.Trim(q.Get("$search"), `"`)
	for _, m := range f.msgs {
		if !in(m) || !graphMatches(m, q.Get("$filter")) {
			continue
		}
		text, hit := strings.ToLower(m.Subject+" "+m.Body+" "+m.From), true
		for _, w := range strings.Fields(strings.ToLower(search)) {
			hit = hit && strings.Contains(text, w)
		}
		if !hit {
			continue
		}
		list = append(list, m)
	}
	if search != "" && q.Get("$orderby") != "" {
		writeJSON(w, 400, map[string]any{"error": map[string]string{"code": "BadRequest", "message": "$search cannot be ordered"}})
		return
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].Received.After(list[j].Received) })
	top, _ := strconv.Atoi(q.Get("$top"))
	if top > 0 && len(list) > top && !f.ignoreLimits {
		list = list[:top]
	}
	var v []any
	for _, m := range list {
		v = append(v, m.json(false))
	}
	writeJSON(w, 200, map[string]any{"value": v})
}

// delta: a token is "<seq>|<since unix>"; a page token adds "|<offset>". A first round takes the
// $filter's receivedDateTime as since; later rounds answer what changed after the token's seq.
func (f *fakeGraph) delta(w http.ResponseWriter, r *http.Request, folderID string) {
	folder, ok := f.folderOf(folderID)
	if !ok {
		graphNotFound(w)
		return
	}
	q := r.URL.Query()
	after, since, offset := 0, time.Time{}, 0
	switch {
	case q.Get("$deltatoken") != "":
		p := strings.Split(q.Get("$deltatoken"), "|")
		after, _ = strconv.Atoi(p[0])
		u, _ := strconv.ParseInt(p[1], 10, 64)
		since = time.Unix(u, 0)
	case q.Get("$skiptoken") != "":
		p := strings.Split(q.Get("$skiptoken"), "|")
		after, _ = strconv.Atoi(p[0])
		u, _ := strconv.ParseInt(p[1], 10, 64)
		since = time.Unix(u, 0)
		offset, _ = strconv.Atoi(p[2])
	default:
		filter := q.Get("$filter")
		if !strings.HasPrefix(filter, "receivedDateTime ge ") {
			writeJSON(w, 400, map[string]any{"error": map[string]string{"code": "BadRequest", "message": "the fake wants a receivedDateTime filter"}})
			return
		}
		since, _ = time.Parse(time.RFC3339, strings.TrimPrefix(filter, "receivedDateTime ge "))
	}
	var changed []*gMsg
	for _, m := range f.msgs {
		if m.Folder == folder && m.seq > after && !m.Received.Before(since) {
			changed = append(changed, m)
		}
	}
	slices.SortFunc(changed, func(a, b *gMsg) int { return a.seq - b.seq })
	size := len(changed)
	if pref := r.Header.Get("Prefer"); strings.HasPrefix(pref, "odata.maxpagesize=") && !f.ignoreLimits {
		size, _ = strconv.Atoi(strings.TrimPrefix(pref, "odata.maxpagesize="))
	}
	page := changed[min(offset, len(changed)):]
	if len(page) > size {
		page = page[:size]
	}
	var v []any
	for _, m := range page {
		v = append(v, m.json(false))
	}
	base := f.srv.URL + "/v1.0/me/mailFolders/" + folderID + "/messages/delta?"
	out := map[string]any{"value": v}
	if offset+len(page) < len(changed) {
		out["@odata.nextLink"] = base + url.Values{"$skiptoken": {fmt.Sprintf("%d|%d|%d", after, since.Unix(), offset+len(page))}}.Encode()
	} else {
		out["@odata.deltaLink"] = base + url.Values{"$deltatoken": {fmt.Sprintf("%d|%d", f.seq, since.Unix())}}.Encode()
	}
	writeJSON(w, 200, out)
}
