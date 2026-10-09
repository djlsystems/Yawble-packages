package main

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

func graphOpts(f *fakeGraph, config map[string]any) opts { return opts{conn: f.conn(), config: config} }

func TestGraphListAndSearch(t *testing.T) {
	f := newFakeGraph(t)
	now := time.Now()
	a := f.add("billing@example.net", "Invoice 42", "Your invoice is ready", now.Add(-time.Hour), false)
	b := f.add("person@example.com", "Lunch?", "Are you free", now, true)
	o := runIt(t, "list", graphOpts(f, nil))
	mustOK(t, o)
	contains(t, o.output, "2 messages in Inbox", a, "Invoice 42", "billing@example.net", "unread", "Your invoice is ready", b, "| read |", untrustedOpen)
	if strings.Index(o.output, b) > strings.Index(o.output, a) {
		t.Fatalf("want newest first:\n%s", o.output)
	}
	c := f.called("GET", "/me/mailFolders/inbox/messages")[0]
	if !strings.Contains(c.Query, "%24top=20") || !strings.Contains(c.Query, "orderby=receivedDateTime+desc") {
		t.Fatalf("want $top from maxResults and newest first, got %q", c.Query)
	}

	o = runIt(t, "list unread from:example.net subject:invoice", graphOpts(f, nil))
	mustOK(t, o)
	contains(t, o.output, "1 messages", a)
	o = runIt(t, "list folder:Archive", graphOpts(f, nil))
	mustOK(t, o)
	contains(t, o.output, "No messages in Archive.")
	mustFail(t, runIt(t, "list folder:Nowhere", graphOpts(f, nil)), `No folder "Nowhere" in this mailbox.`)

	o = runIt(t, "search invoice ready max:5", graphOpts(f, nil))
	mustOK(t, o)
	contains(t, o.output, a)
	s := f.called("GET", "/me/messages")
	last := s[len(s)-1]
	if !strings.Contains(last.Query, "%24search=%22invoice+ready%22") || !strings.Contains(last.Query, "%24top=5") {
		t.Fatalf("want the raw query as $search with $top 5, got %q", last.Query)
	}
}

func TestGraphReadTurnsHTMLToTextAndListsAttachmentsByNameOnly(t *testing.T) {
	f := newFakeGraph(t)
	id := f.add("billing@example.net", "Invoice 42", `<p>Dear customer,</p><p>Pay <a href="https://pay.example.net/42">here</a>.</p><script>steal()</script>`, time.Now(), false)
	f.byID(id).Atts = []attachment{{Name: "invoice-42.pdf", Type: "application/pdf", Size: 48213}}
	o := runIt(t, "read "+id, graphOpts(f, nil))
	mustOK(t, o)
	contains(t, o.output, "Message "+id, "Folders: Inbox", "From: Sender", "billing@example.net", "To: person@contoso.test", "Subject: Invoice 42",
		"Dear customer,", "Pay here [1].", "[1] https://pay.example.net/42", "invoice-42.pdf (application/pdf, 48213 bytes)", untrustedOpen)
	if strings.Contains(o.output, "steal()") || strings.Contains(o.output, "<p>") {
		t.Fatalf("want HTML turned to text:\n%s", o.output)
	}
	if f.byID(id).IsRead {
		t.Fatal("reading must not mark the message read")
	}
	mustFail(t, runIt(t, "read AAMkNope=", graphOpts(f, nil)), "No message AAMkNope= in this mailbox.")
}

func TestGraphMarkMoveAndLabelNeedThePersonsSettings(t *testing.T) {
	f := newFakeGraph(t)
	a := f.add("a@example.org", "A", "a", time.Now(), false)
	b := f.add("b@example.org", "B", "b", time.Now(), false)
	mustFail(t, runIt(t, "mark-read "+a, graphOpts(f, nil)), "Refused: mark-read and mark-unread are off")
	mustFail(t, runIt(t, "move Archive "+a, graphOpts(f, nil)), "Refused: move and label are off")
	if n := len(f.called("PATCH", "")) + len(f.called("POST", "")); n != 0 {
		t.Fatalf("refused, so nothing reached Graph; got %d writes", n)
	}

	cfg := map[string]any{"markRead": true, "moveTo": []string{"Archive", "Follow up"}}
	mustOK(t, runIt(t, "mark-read "+a, graphOpts(f, cfg)))
	if !f.byID(a).IsRead {
		t.Fatal("want it read")
	}
	mustOK(t, runIt(t, "mark-unread "+a, graphOpts(f, cfg)))
	if f.byID(a).IsRead {
		t.Fatal("want it unread again")
	}
	o := runIt(t, "label \"Follow up\" "+b, graphOpts(f, cfg))
	mustOK(t, o)
	if !slices.Contains(f.byID(b).Categories, "Follow up") {
		t.Fatalf("want the category on the message, got %v", f.byID(b).Categories)
	}
	o = runIt(t, "move Archive "+a, graphOpts(f, cfg))
	mustOK(t, o)
	contains(t, o.output, "Moved 1 messages to Archive: "+a+" (now AAMkAGI2MOVED")
	mustFail(t, runIt(t, "move Inbox "+b, graphOpts(f, cfg)), "Refused: Inbox is not on moveTo")
	mustFail(t, runIt(t, "mark-read AAMkNope=", graphOpts(f, cfg)), "No message AAMkNope= in this mailbox.")
}

func TestGraphDraftAndSend(t *testing.T) {
	f := newFakeGraph(t)
	allow := []string{"@example.com"}
	instr := "send\nTo: person@example.com\nBcc: ops@example.com\nSubject: Hi\n\nHello world"
	o := runIt(t, instr, opts{conn: f.conn(), allowlist: allow})
	mustOK(t, o)
	contains(t, o.output, "created in Drafts; nothing was sent.", "draft mode")
	if len(f.sent) != 0 || len(f.called("POST", "/me/messages")) != 1 {
		t.Fatal("draft mode makes one draft and sends nothing")
	}
	var draft map[string]any
	_ = json.Unmarshal(f.called("POST", "/me/messages")[0].Body, &draft)
	// THE DEFAULT IS HTML: Graph takes one body, the HTML made from the text.
	if body := draft["body"].(map[string]any); body["contentType"] != "HTML" || !strings.Contains(body["content"].(string), "<p>Hello world</p>") {
		t.Fatalf("want an HTML draft, got %v", draft)
	}
	o = runIt(t, instr, opts{conn: f.conn(), allowlist: allow, config: map[string]any{"format": "text"}})
	mustOK(t, o)
	_ = json.Unmarshal(f.called("POST", "/me/messages")[1].Body, &draft)
	if body := draft["body"].(map[string]any); body["contentType"] != "Text" || body["content"] != "Hello world" {
		t.Fatalf("format text: want a plain-text draft, got %v", draft)
	}

	o = runIt(t, instr, opts{conn: f.conn(), mode: "send", allowlist: allow})
	mustOK(t, o)
	contains(t, o.output, "Sent")
	if len(f.sent) != 1 {
		t.Fatalf("want one sendMail, got %d", len(f.sent))
	}
	msg := f.sent[0]["message"].(map[string]any)
	if msg["subject"] != "Hi" || len(msg["bccRecipients"].([]any)) != 1 {
		t.Fatalf("want subject and the Bcc recipient, got %v", msg)
	}
	o = runIt(t, "send\nTo: evil@elsewhere.test\nSubject: Hi\n\nBody", opts{conn: f.conn(), mode: "send", allowlist: allow})
	mustFail(t, o, "Refused: evil@elsewhere.test is not on sendAllowlist; nothing was sent.")
	if len(f.sent) != 1 {
		t.Fatal("refused, so nothing more was sent")
	}
}

func TestGraphErrorsSayWhatToDo(t *testing.T) {
	cases := []struct {
		status int
		body   string
		words  []string
	}{
		{401, `{"error":{"code":"InvalidAuthenticationToken","message":"expired"}}`, []string{wordsReconnectMicrosoft}},
		{403, `{"error":{"code":"ErrorAccessDenied","message":"Access is denied."}}`, []string{"Microsoft Graph refused the request (403): the connection lacks the scope " + scopeGraphReadWrite}},
		{429, `{"error":{"code":"ApplicationThrottled","message":"slow down"}}`, []string{"Microsoft Graph answered 429 Too Many Requests; try again later."}},
		{503, `{"error":{"code":"ServiceUnavailable","message":"busy"}}`, []string{"Microsoft Graph answered 503 Service Unavailable; try again later."}},
	}
	for _, c := range cases {
		f := newFakeGraph(t)
		f.failWith("GET", "/me/mailFolders/inbox/messages", c.status, c.body)
		mustFail(t, runIt(t, "list", graphOpts(f, nil)), c.words[0])
	}
	f := newFakeGraph(t)
	f.failWith("POST", "/me/sendMail", 403, `{"error":{"code":"ErrorAccessDenied","message":"Access is denied."}}`)
	o := runIt(t, "send\nTo: person@example.com\nSubject: Hi\n\nBody", opts{conn: f.conn(), mode: "send", allowlist: []string{"@example.com"}})
	mustFail(t, o, "lacks the scope "+scopeGraphSend)
	contains(t, o.error, "nothing was sent or drafted")

	f = newFakeGraph(t)
	f.failWith("GET", "/me/mailFolders/inbox/messages", http.StatusUnauthorized, `{}`)
	c := f.conn()
	c["accessToken"] = ""
	mustFail(t, runIt(t, "list", opts{conn: c}), wordsReconnectMicrosoft)
	if len(f.called("", "")) != 0 {
		t.Fatal("no token, no call")
	}
	f.srv.Close()
	mustFail(t, runIt(t, "list", graphOpts(f, nil)), "Microsoft Graph could not be reached; try again later.")
}

func TestGraphStoredLinkMustStayOnGraph(t *testing.T) {
	f := newFakeGraph(t)
	box := graphWatchBox{f}
	st := newSiteStore()
	mustOK(t, st.run(t, "watch", watchOpts(box, nil)))
	for id, raw := range st.docs {
		var doc watchDoc
		_ = json.Unmarshal(raw, &doc)
		doc.Position.Graph.Link = "https://attacker.example/steal?x=1"
		b, _ := json.Marshal(doc)
		st.docs[id] = b
	}
	o := st.run(t, "watch", watchOpts(box, nil))
	mustOK(t, o)
	contains(t, o.output, "could not be used", "starts again from now")
}
