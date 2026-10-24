package main

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"
)

func gmailOpts(g *gmailBox, config map[string]any) opts { return opts{conn: g.conn(), config: config} }

func TestGmailPortableListAndSearchEveryFolder(t *testing.T) {
	g := newGmailBox(t)
	now := time.Now()
	a := g.add("billing@example.net", "Invoice 42", "ready", now.Add(-time.Hour), false)
	g.add("person@example.com", "Lunch", "free?", now, true)
	o := runIt(t, "list unread from:example.net since:2026-01-01", gmailOpts(g, nil))
	mustOK(t, o)
	contains(t, o.output, "1 messages in INBOX matching the query", a)
	c := g.called("GET", "/messages")[0]
	if !strings.Contains(c.Query, "labelIds=INBOX") || !strings.Contains(c.Query, "q=is%3Aunread+from%3Aexample.net+after%3A2026%2F01%2F01") {
		t.Fatalf("want the portable query in Gmail's words, got %q", c.Query)
	}
	o = runIt(t, `search subject:"Invoice 42"`, gmailOpts(g, nil))
	mustOK(t, o)
	contains(t, o.output, a)
	var all []call
	for _, c := range g.called("GET", "/messages") {
		if strings.HasSuffix(c.Path, "/messages") {
			all = append(all, c)
		}
	}
	if q := all[len(all)-1].Query; strings.Contains(q, "labelIds") || !strings.Contains(q, "subject%3A%22Invoice+42%22") {
		t.Fatalf("a search with no folder looks in every folder, got %q", q)
	}
}

func TestGmailMarkMoveLabelAndDraftCommand(t *testing.T) {
	g := newGmailBox(t)
	a := g.add("a@example.org", "A", "a", time.Now(), false)
	mustFail(t, runIt(t, "mark-read "+a, gmailOpts(g, nil)), "Refused: mark-read and mark-unread are off")
	mustFail(t, runIt(t, "label Receipts "+a, gmailOpts(g, nil)), "Refused: move and label are off")
	if len(g.modifys) != 0 {
		t.Fatal("refused, so nothing changed")
	}
	cfg := map[string]any{"markRead": true, "moveTo": []string{"Receipts", "Archive"}}
	mustOK(t, runIt(t, "mark-read "+a, gmailOpts(g, cfg)))
	if slices.Contains(g.byID(a).Labels, "UNREAD") {
		t.Fatal("want UNREAD removed")
	}
	mustOK(t, runIt(t, "mark-unread "+a, gmailOpts(g, cfg)))
	if !slices.Contains(g.byID(a).Labels, "UNREAD") {
		t.Fatal("want UNREAD back")
	}
	mustOK(t, runIt(t, "label Receipts "+a, gmailOpts(g, cfg)))
	if l := g.byID(a).Labels; !slices.Contains(l, "Label_7") || !slices.Contains(l, "INBOX") {
		t.Fatalf("a label adds, and leaves the Inbox: %v", l)
	}
	mustOK(t, runIt(t, "move Archive "+a, gmailOpts(g, cfg)))
	if l := g.byID(a).Labels; !slices.Contains(l, "Label_8") || slices.Contains(l, "INBOX") {
		t.Fatalf("a move takes it out of the Inbox into the folder: %v", l)
	}
	mustFail(t, runIt(t, "mark-read nope1", gmailOpts(g, cfg)), "No message nope1 in this mailbox.")

	o := runIt(t, "draft\nTo: person@example.com\nSubject: Hi\n\nBody", opts{conn: g.conn(), mode: "send", allowlist: []string{"@example.com"}})
	mustOK(t, o)
	contains(t, o.output, "Draft r-1 created")
	if g.sent != 0 || g.drafts != 1 {
		t.Fatalf("the draft command drafts even in send mode; drafts %d sent %d", g.drafts, g.sent)
	}
}

func TestGmailModifyNeedsTheModifyScope(t *testing.T) {
	g := newGmailBox(t)
	a := g.add("a@example.org", "A", "a", time.Now(), false)
	g.failWith("POST", "/messages/", 403, `{"error":{"code":403,"message":"Request had insufficient authentication scopes.","errors":[{"reason":"insufficientPermissions"}]}}`)
	mustFail(t, runIt(t, "mark-read "+a, gmailOpts(g, map[string]any{"markRead": true})), "lacks the scope "+scopeModify)
}

func TestGmailWatchErrorsKeepThePosition(t *testing.T) {
	for _, c := range []struct {
		status int
		words  string
	}{{401, wordsReconnect}, {403, "lacks the scope " + scopeReadonly}, {429, "try again later"}, {500, "try again later"}} {
		g := newGmailBox(t)
		box := gmailWatchBox{g}
		st := newSiteStore()
		mustOK(t, st.run(t, "watch", watchOpts(box, nil)))
		before := string(st.docs[st.puts[0]])
		box.arrive(t, "a@example.org", "Waiting", time.Now(), false)
		g.failWith("GET", "/history", c.status, `{"error":{"code":`+itoaT(c.status)+`,"message":"no"}}`)
		o := st.run(t, "watch", watchOpts(box, nil))
		mustFail(t, o, c.words)
		if string(st.docs[st.puts[0]]) != before || len(o.of("publish")) != 0 {
			t.Fatalf("%d: a failed check stores nothing and publishes nothing", c.status)
		}
		delete(g.fail, "GET /history")
		ev := event(t, st.run(t, "watch", watchOpts(box, nil)))
		if ev["count"] != float64(1) {
			t.Fatalf("%d: the message waits for the next good check, got %v", c.status, ev)
		}
	}
}

func itoaT(n int) string { b, _ := json.Marshal(n); return string(b) }
