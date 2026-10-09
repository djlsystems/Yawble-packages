package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// gmailBox is the fake Gmail with a mailbox behind it: messages with labels, and a history that
// records each arrival, so the watch can be driven. The canned-route fake (newFake) stays for
// the answers a test wants to dictate.
type gmailBox struct {
	t       *testing.T
	srv     *httptest.Server
	mu      sync.Mutex
	msgs    []*gbMsg
	hist    []gbHist
	histID  int
	oldest  int // a startHistoryId below this answers 404, as an expired one does
	nextID  int
	calls   []call
	fail    map[string]func(w http.ResponseWriter)
	drafts  int
	sent    int
	modifys []string
	// oneRecord answers history with every added message in a single record.
	oneRecord bool
	// ignoreMax answers a list with every match, as a server that ignores maxResults would.
	ignoreMax bool
}

type gbMsg struct {
	ID, Thread, From, Subject, Body string
	Labels                          []string
	Received                        time.Time
}

type gbHist struct {
	ID     int
	MsgID  string
	Labels []string
}

var gbLabels = []gmailLabel{{"INBOX", "INBOX"}, {"UNREAD", "UNREAD"}, {"SENT", "SENT"}, {"DRAFT", "DRAFT"}, {"Label_7", "Receipts"}, {"Label_8", "Archive"}}

func newGmailBox(t *testing.T) *gmailBox {
	t.Helper()
	g := &gmailBox{t: t, histID: 1000, fail: map[string]func(http.ResponseWriter){}}
	g.srv = httptest.NewServer(http.HandlerFunc(g.serve))
	t.Cleanup(g.srv.Close)
	t.Setenv(gmailBaseEnv, g.srv.URL)
	return g
}

func (g *gmailBox) conn() map[string]any {
	return map[string]any{"provider": "google", "account": "person@example.test", "accessToken": token,
		"expiresAt": "2026-10-07T20:00:00Z", "scopes": []string{scopeReadonly, scopeCompose, scopeModify}}
}

func (g *gmailBox) failWith(method, prefix string, status int, body string) {
	g.fail[method+" "+prefix] = func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

// add delivers a message to the Inbox and records its arrival in the history.
func (g *gmailBox) add(from, subject, body string, received time.Time, read bool) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.nextID++
	labels := []string{"INBOX"}
	if !read {
		labels = append(labels, "UNREAD")
	}
	m := &gbMsg{ID: fmt.Sprintf("19a%05x", g.nextID), Thread: fmt.Sprintf("19t%05x", g.nextID), From: from, Subject: subject, Body: body,
		Labels: labels, Received: received}
	g.msgs = append(g.msgs, m)
	g.histID += 7
	g.hist = append(g.hist, gbHist{ID: g.histID, MsgID: m.ID, Labels: slices.Clone(labels)})
	return m.ID
}

func (g *gmailBox) byID(id string) *gbMsg {
	for _, m := range g.msgs {
		if m.ID == id {
			return m
		}
	}
	return nil
}

func (g *gmailBox) called(method, prefix string) []call {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []call
	for _, c := range g.calls {
		if (method == "" || c.Method == method) && strings.HasPrefix(c.Path, "/gmail/v1/users/me"+prefix) {
			out = append(out, c)
		}
	}
	return out
}

func (m *gbMsg) json(full bool) map[string]any {
	headers := []map[string]string{{"name": "From", "value": m.From}, {"name": "Subject", "value": m.Subject},
		{"name": "Date", "value": m.Received.Format(time.RFC1123Z)}, {"name": "To", "value": "person@example.test"}}
	payload := map[string]any{"mimeType": "text/plain", "headers": headers}
	if full {
		payload["body"] = map[string]any{"data": b64(m.Body), "size": len(m.Body)}
	}
	snippet := m.Body
	if len(snippet) > 50 {
		snippet = snippet[:50]
	}
	return map[string]any{"id": m.ID, "threadId": m.Thread, "labelIds": m.Labels, "snippet": snippet, "payload": payload}
}

// gbMatches applies the few search terms the plugin writes.
func gbMatches(m *gbMsg, q string) bool {
	for _, term := range splitWords(q) {
		k, v, _ := strings.Cut(term, ":")
		switch k {
		case "is":
			if v == "unread" && !slices.Contains(m.Labels, "UNREAD") {
				return false
			}
		case "after":
			t, err := time.Parse("2006/01/02", v)
			if err != nil || m.Received.Before(t) {
				return false
			}
		case "from":
			if !strings.Contains(strings.ToLower(m.From), strings.ToLower(v)) {
				return false
			}
		case "subject":
			if !strings.Contains(strings.ToLower(m.Subject), strings.ToLower(v)) {
				return false
			}
		default:
			if !strings.Contains(strings.ToLower(m.Subject+" "+m.Body), strings.ToLower(term)) {
				return false
			}
		}
	}
	return true
}

func (g *gmailBox) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, call{r.Method, r.URL.Path, r.URL.RawQuery, body})
	if r.Header.Get("Authorization") != "Bearer "+token {
		writeJSON(w, 401, map[string]any{"error": map[string]any{"code": 401, "message": "Invalid Credentials"}})
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/gmail/v1/users/me")
	for k, h := range g.fail {
		method, prefix, _ := strings.Cut(k, " ")
		if method == r.Method && strings.HasPrefix(path, prefix) {
			h(w)
			return
		}
	}
	notFound := func() {
		writeJSON(w, 404, map[string]any{"error": map[string]any{"code": 404, "message": "Requested entity was not found."}})
	}
	q := r.URL.Query()
	parts := strings.Split(strings.Trim(path, "/"), "/")
	switch {
	case r.Method == "GET" && path == "/labels":
		writeJSON(w, 200, map[string]any{"labels": gbLabels})
	case r.Method == "GET" && path == "/profile":
		writeJSON(w, 200, map[string]any{"emailAddress": "person@example.test", "historyId": strconv.Itoa(g.histID)})
	case r.Method == "GET" && path == "/messages":
		var list []*gbMsg
		for _, m := range g.msgs {
			if l := q.Get("labelIds"); l != "" && !slices.Contains(m.Labels, l) {
				continue
			}
			if !gbMatches(m, q.Get("q")) {
				continue
			}
			list = append(list, m)
		}
		sort.SliceStable(list, func(i, j int) bool { return list[i].Received.After(list[j].Received) })
		if n, _ := strconv.Atoi(q.Get("maxResults")); n > 0 && len(list) > n && !g.ignoreMax {
			list = list[:n]
		}
		var v []any
		for _, m := range list {
			v = append(v, map[string]string{"id": m.ID, "threadId": m.Thread})
		}
		writeJSON(w, 200, map[string]any{"messages": v})
	case r.Method == "GET" && len(parts) == 2 && parts[0] == "messages":
		m := g.byID(parts[1])
		if m == nil {
			notFound()
			return
		}
		writeJSON(w, 200, m.json(q.Get("format") == "full"))
	case r.Method == "POST" && len(parts) == 3 && parts[0] == "messages" && parts[2] == "modify":
		m := g.byID(parts[1])
		if m == nil {
			notFound()
			return
		}
		var p struct {
			Add    []string `json:"addLabelIds"`
			Remove []string `json:"removeLabelIds"`
		}
		_ = json.Unmarshal(body, &p)
		for _, l := range p.Add {
			if !slices.Contains(m.Labels, l) {
				m.Labels = append(m.Labels, l)
			}
		}
		m.Labels = slices.DeleteFunc(m.Labels, func(l string) bool { return slices.Contains(p.Remove, l) })
		g.modifys = append(g.modifys, m.ID)
		g.histID++ // a label change is history too, but not an arrival
		writeJSON(w, 200, m.json(false))
	case r.Method == "POST" && path == "/drafts":
		g.drafts++
		writeJSON(w, 200, map[string]any{"id": "r-" + strconv.Itoa(g.drafts), "message": map[string]string{"id": "m-d" + strconv.Itoa(g.drafts)}})
	case r.Method == "POST" && path == "/messages/send":
		g.sent++
		writeJSON(w, 200, map[string]any{"id": "m-s" + strconv.Itoa(g.sent), "threadId": "t-s" + strconv.Itoa(g.sent)})
	case r.Method == "GET" && path == "/history":
		start, _ := strconv.Atoi(q.Get("startHistoryId"))
		if start < g.oldest {
			notFound()
			return
		}
		if q.Get("historyTypes") != "messageAdded" {
			writeJSON(w, 400, map[string]any{"error": map[string]any{"code": 400, "message": "the fake answers messageAdded only"}})
			return
		}
		var recs []gbHist
		for _, h := range g.hist {
			if h.ID > start && (q.Get("labelId") == "" || slices.Contains(h.Labels, q.Get("labelId"))) {
				recs = append(recs, h)
			}
		}
		offset, _ := strconv.Atoi(q.Get("pageToken"))
		recs = recs[min(offset, len(recs)):]
		out := map[string]any{"historyId": strconv.Itoa(g.histID)}
		if n, _ := strconv.Atoi(q.Get("maxResults")); n > 0 && len(recs) > n && !g.oneRecord {
			recs = recs[:n]
			out["nextPageToken"] = strconv.Itoa(offset + n)
		}
		var v []any
		for _, h := range recs {
			v = append(v, map[string]any{"id": strconv.Itoa(h.ID), "messagesAdded": []any{map[string]any{"message": map[string]any{"id": h.MsgID, "labelIds": h.Labels}}}})
		}
		if g.oneRecord && len(v) > 0 {
			var added []any
			for _, r := range v {
				added = append(added, r.(map[string]any)["messagesAdded"].([]any)...)
			}
			v = []any{map[string]any{"id": strconv.Itoa(recs[len(recs)-1].ID), "messagesAdded": added}}
		}
		if len(v) > 0 {
			out["history"] = v
		}
		writeJSON(w, 200, out)
	default:
		notFound()
	}
}
