package main

import (
	"crypto/x509"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
)

func imapOpts(i *fakeIMAP, s *fakeSMTP, config map[string]any) opts {
	return opts{conn: imapConnection(i, s, appPassword), config: config}
}

func uidOf(t *testing.T, id string) uint32 {
	t.Helper()
	_, uid, ok := parseIMAPID(id)
	if !ok {
		t.Fatalf("not an IMAP id: %q", id)
	}
	return uint32(uid)
}

func hasKeyword(t *testing.T, f *fakeIMAP, folder string, uid uint32, keyword string) bool {
	t.Helper()
	sess := imapmemserver.NewUserSession(f.user)
	defer sess.Close()
	if _, err := sess.Select(folder, &imap.SelectOptions{ReadOnly: true}); err != nil {
		t.Fatal(err)
	}
	data, err := sess.Search(imapserver.NumKindUID, &imap.SearchCriteria{UID: []imap.UIDSet{imap.UIDSetNum(imap.UID(uid))}, Flag: []imap.Flag{imap.Flag(keyword)}}, &imap.SearchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return len(data.AllUIDs()) == 1
}

func TestIMAPListShowsTheInboxNewestFirstWithSnippets(t *testing.T) {
	for _, sec := range []string{"TLS", "STARTTLS"} {
		t.Run(sec, func(t *testing.T) {
			f := newFakeIMAP(t, sec)
			now := time.Now()
			a := f.deliver(t, "INBOX", rawMail("Acme <billing@example.net>", "Invoice 42", "Your invoice is ready", "", now.Add(-2*time.Hour), ""), now.Add(-2*time.Hour), false)
			b := f.deliver(t, "INBOX", rawMail("person@example.com", "Lunch?", "", "<p>Are you <b>free</b> &amp; hungry?</p>", now.Add(-time.Hour), ""), now.Add(-time.Hour), true)
			f.deliver(t, "Receipts", rawMail("shop@example.org", "Receipt", "Thanks", "", now, ""), now, false)
			o := runIt(t, "list", imapOpts(f, nil, nil))
			mustOK(t, o)
			contains(t, o.output, "2 messages in INBOX", a, "Invoice 42", "Acme <billing@example.net>", "unread", "Your invoice is ready",
				b, "Lunch?", "| read |", "Are you free & hungry?", untrustedOpen, untrustedClose)
			if strings.Index(o.output, "Lunch?") > strings.Index(o.output, "Invoice 42") {
				t.Fatalf("want newest first:\n%s", o.output)
			}
			if strings.Contains(o.output, "Receipt |") {
				t.Fatalf("list reads the Inbox only:\n%s", o.output)
			}
			if f.flags(t, "INBOX", uidOf(t, a)) != nil {
				t.Fatal("listing must not mark a message read")
			}
		})
	}
}

func TestIMAPListTakesThePortableQuery(t *testing.T) {
	f := newFakeIMAP(t, "TLS")
	now := time.Now()
	old := f.deliver(t, "INBOX", rawMail("billing@example.net", "Invoice 1", "old", "", now.AddDate(0, 0, -20), ""), now.AddDate(0, 0, -20), false)
	inv := f.deliver(t, "INBOX", rawMail("billing@example.net", "Invoice 2", "new", "", now, ""), now, false)
	seen := f.deliver(t, "INBOX", rawMail("billing@example.net", "Invoice 3", "seen", "", now, ""), now, true)
	other := f.deliver(t, "INBOX", rawMail("person@example.com", "Invoice from Pat", "x", "", now, ""), now, false)
	rec := f.deliver(t, "Receipts", rawMail("shop@example.org", "Receipt 9", "x", "", now, ""), now, false)

	o := runIt(t, "list unread from:example.net subject:invoice since:"+now.AddDate(0, 0, -7).Format("2006-01-02"), imapOpts(f, nil, nil))
	mustOK(t, o)
	contains(t, o.output, "Invoice 2")
	for _, id := range []string{old, seen, other} {
		if strings.Contains(o.output, id+" |") {
			t.Fatalf("want only unread acme invoices of the last week, found %s:\n%s", id, o.output)
		}
	}
	_ = inv
	o = runIt(t, `list folder:Receipts subject:"Receipt 9"`, imapOpts(f, nil, nil))
	mustOK(t, o)
	contains(t, o.output, rec, "Receipt 9", "in Receipts")

	o = runIt(t, "list max:2", imapOpts(f, nil, nil))
	mustOK(t, o)
	contains(t, o.output, "2 messages")

	mustFail(t, runIt(t, "list newer_than:7d", imapOpts(f, nil, nil)), "list takes only folder:")
	mustFail(t, runIt(t, "list folder:Nowhere", imapOpts(f, nil, nil)), `No folder "Nowhere" in this mailbox.`)
}

func TestIMAPSearchTakesPortableTermsOrWordsAsText(t *testing.T) {
	f := newFakeIMAP(t, "TLS")
	now := time.Now()
	hit := f.deliver(t, "INBOX", rawMail("a@example.org", "Quarterly numbers", "the budget is attached", "", now, ""), now, false)
	f.deliver(t, "INBOX", rawMail("b@example.org", "Lunch", "pizza", "", now, ""), now, false)
	o := runIt(t, "search budget attached", imapOpts(f, nil, nil))
	mustOK(t, o)
	contains(t, o.output, hit, "Quarterly numbers", "1 messages")
	o = runIt(t, "search from:b@example.org", imapOpts(f, nil, nil))
	mustOK(t, o)
	contains(t, o.output, "Lunch", "1 messages")
}

func TestIMAPReadTurnsHTMLToTextListsLinksAndAttachmentsAndCuts(t *testing.T) {
	f := newFakeIMAP(t, "TLS")
	now := time.Now()
	html := `<html><head><style>p{color:red}</style></head><body><p>Dear customer,</p><p>Pay <a href=3D"https://pay.example.net/i/42">here</a> or <a href=3D'https://example.net/help'>get help</a>.</p><p>Total: =A342</p></body></html>`
	id := f.deliver(t, "INBOX", rawMail("Acme <billing@example.net>", "Invoice 42", "", html, now, "invoice-42.pdf"), now, false)
	o := runIt(t, "read "+id, imapOpts(f, nil, nil))
	mustOK(t, o)
	contains(t, o.output, "Message "+id, "Folders: INBOX", "From: Acme <billing@example.net>", "To: "+imapAccount, "Subject: Invoice 42",
		"Dear customer,", "Pay here [1] or get help [2].", "Total: £42", "Links:", "[1] https://pay.example.net/i/42", "[2] https://example.net/help",
		"Attachments (listed, never downloaded):", "invoice-42.pdf (application/pdf", untrustedOpen, untrustedClose)
	for _, bad := range []string{"<p>", "color:red", "href="} {
		if strings.Contains(o.output, bad) {
			t.Fatalf("want HTML turned to text, found %q:\n%s", bad, o.output)
		}
	}
	if strings.Index(o.output, "pay.example.net") > strings.LastIndex(o.output, untrustedClose) {
		t.Fatalf("links are mail content and sit inside the fence:\n%s", o.output)
	}
	if f.flags(t, "INBOX", uidOf(t, id)) != nil {
		t.Fatal("reading must not mark the message read")
	}

	long := strings.Repeat("abcdefghij", 300)
	id = f.deliver(t, "INBOX", rawMail("a@example.org", "Long", long, "", now, ""), now, false)
	o = runIt(t, "read "+id, opts{conn: imapConnection(f, nil, appPassword), maxBodyChars: 1000})
	mustOK(t, o)
	if strings.Contains(o.output, long[:1001]) || !strings.Contains(o.output, long[:1000]) {
		t.Fatalf("want the body cut to 1000 characters:\n%s", o.output)
	}
	contains(t, o.output, "Body cut at 1000 of 3000 characters (maxBodyChars).")

	plain := f.deliver(t, "INBOX", rawMail("a@example.org", "Plain", "See https://example.org/a, thanks.", "", now, ""), now, false)
	o = runIt(t, "read "+plain, imapOpts(f, nil, nil))
	mustOK(t, o)
	contains(t, o.output, "[1] https://example.org/a\n")

	mustFail(t, runIt(t, "read 999", imapOpts(f, nil, nil)), "No message 999 in this mailbox.")
	mustFail(t, runIt(t, "read 1@Nowhere", imapOpts(f, nil, nil)), "No message 1@Nowhere in this mailbox.")
	mustFail(t, runIt(t, "read abc", imapOpts(f, nil, nil)), "No message abc in this mailbox.")
}

func TestIMAPMarkReadAndUnreadNeedThePersonsSetting(t *testing.T) {
	f := newFakeIMAP(t, "TLS")
	now := time.Now()
	a := f.deliver(t, "INBOX", rawMail("a@example.org", "A", "a", "", now, ""), now, false)
	b := f.deliver(t, "Archive", rawMail("b@example.org", "B", "b", "", now, ""), now, false)

	o := runIt(t, "mark-read "+a, imapOpts(f, nil, nil))
	mustFail(t, o, "Refused: mark-read and mark-unread are off; a person turns on markRead in this member's settings. Nothing was changed.")
	if f.flags(t, "INBOX", uidOf(t, a)) != nil {
		t.Fatal("refused, so nothing changed")
	}

	on := map[string]any{"markRead": true}
	o = runIt(t, "mark-read "+a+", "+b, imapOpts(f, nil, on))
	mustOK(t, o)
	contains(t, o.output, "Marked 2 messages read")
	if f.flags(t, "INBOX", uidOf(t, a)) == nil || f.flags(t, "Archive", uidOf(t, b)) == nil {
		t.Fatal("want both marked read on the server")
	}
	mustOK(t, runIt(t, "mark-unread "+a, imapOpts(f, nil, on)))
	if f.flags(t, "INBOX", uidOf(t, a)) != nil {
		t.Fatal("want it unread again")
	}
	mustFail(t, runIt(t, "mark-read 4242", imapOpts(f, nil, on)), "No message 4242 in this mailbox.")
}

func TestIMAPMoveAndLabelOnlyToThePersonsFolders(t *testing.T) {
	f := newFakeIMAP(t, "TLS")
	now := time.Now()
	a := f.deliver(t, "INBOX", rawMail("a@example.org", "A", "a", "", now, ""), now, false)
	b := f.deliver(t, "INBOX", rawMail("b@example.org", "B", "b", "", now, ""), now, false)

	mustFail(t, runIt(t, "move Archive "+a, imapOpts(f, nil, nil)), "Refused: move and label are off; a person lists the folders and labels it may use in moveTo")
	mustFail(t, runIt(t, "label Receipts "+a, imapOpts(f, nil, nil)), "Refused: move and label are off")
	cfg := map[string]any{"moveTo": []string{"archive", "Receipts"}}
	mustFail(t, runIt(t, "move Drafts "+a, imapOpts(f, nil, cfg)), "Refused: Drafts is not on moveTo; nothing was changed.")
	if f.count(t, "INBOX") != 2 {
		t.Fatal("refused, so nothing moved")
	}

	o := runIt(t, "move Archive "+a, imapOpts(f, nil, cfg))
	mustOK(t, o)
	contains(t, o.output, "Moved 1 messages to Archive: "+a+" (now ", "@Archive)")
	if f.count(t, "INBOX") != 1 || f.count(t, "Archive") != 1 {
		t.Fatalf("want the message in Archive, INBOX %d Archive %d", f.count(t, "INBOX"), f.count(t, "Archive"))
	}
	o = runIt(t, "label Receipts "+b, imapOpts(f, nil, cfg))
	mustOK(t, o)
	contains(t, o.output, "Labelled 1 messages Receipts")
	if !hasKeyword(t, f, "INBOX", uidOf(t, b), "Receipts") || f.count(t, "INBOX") != 1 {
		t.Fatal("on a server without Gmail's labels, a label is a keyword on the message, which stays put")
	}
	mustFail(t, runIt(t, "move Archive "+a, imapOpts(f, nil, cfg)), "No message "+a+" in this mailbox.")
}

func TestIMAPDraftSavesToDraftsAndSendsNothing(t *testing.T) {
	f := newFakeIMAP(t, "TLS")
	s := newFakeSMTP(t, "TLS", imapUser)
	allow := []string{"@example.com"}
	for _, instr := range []string{"draft", "send"} {
		o := runIt(t, instr+"\nTo: person+tag@example.com\nBcc: ops@example.com\nSubject: Café plans\n\nHello world", opts{conn: imapConnection(f, s, appPassword), allowlist: allow})
		mustOK(t, o)
		contains(t, o.output, "saved in Drafts; nothing was sent.")
	}
	if n := f.count(t, "Drafts"); n != 2 {
		t.Fatalf("want 2 drafts, got %d", n)
	}
	if n := len(s.messages()); n != 0 {
		t.Fatalf("draft mode sends nothing, got %d", n)
	}
	o := runIt(t, "draft\nTo: person@example.com\nSubject: Hi\n\nBody", opts{conn: imapConnection(f, s, appPassword), mode: "send", allowlist: allow})
	mustOK(t, o)
	if n := len(s.messages()); n != 0 {
		t.Fatalf("the draft command never sends, even in send mode; got %d", n)
	}
}

func TestIMAPSendGoesOverSMTPToAllowedRecipientsOnly(t *testing.T) {
	for _, sec := range []string{"TLS", "STARTTLS"} {
		t.Run(sec, func(t *testing.T) {
			f := newFakeIMAP(t, "TLS")
			s := newFakeSMTP(t, sec, imapUser)
			conn := imapConnection(f, s, appPassword)
			conn["smtp"].(map[string]any)["security"] = sec
			allow := []string{"@example.com", "boss@example.org"}
			instr := "send\nTo: person+tag@example.com, Boss@Example.org\nCc: ops@example.com\nBcc: audit@example.com\nSubject: Café plans\n\nHello world\nSecond line"
			o := runIt(t, instr, opts{conn: conn, mode: "send", allowlist: allow})
			mustOK(t, o)
			contains(t, o.output, "Sent from "+imapAccount+" over SMTP.")
			msgs := s.messages()
			if len(msgs) != 1 {
				t.Fatalf("want one message sent, got %d", len(msgs))
			}
			m := msgs[0]
			if m.From != imapAccount || strings.Join(m.To, ",") != "person+tag@example.com,Boss@Example.org,ops@example.com,audit@example.com" {
				t.Fatalf("want every recipient, Bcc included, on the envelope; got from %q to %v", m.From, m.To)
			}
			contains(t, m.Data, "From: "+imapAccount, "To: person+tag@example.com, Boss@Example.org", "Cc: ops@example.com", "Subject: =?UTF-8?q?Caf=C3=A9_plans?=", "Message-ID: <")
			if strings.Contains(m.Data, "Bcc:") || strings.Contains(m.Data, "audit@") {
				t.Fatalf("a sent message never shows its Bcc:\n%s", m.Data)
			}
			if f.count(t, "Drafts") != 0 {
				t.Fatal("send mode does not draft")
			}

			o = runIt(t, "send\nTo: person@example.com\nCc: evil@elsewhere.test\nSubject: Hi\n\nBody", opts{conn: conn, mode: "send", allowlist: allow})
			mustFail(t, o, "Refused: evil@elsewhere.test is not on sendAllowlist; nothing was sent.")
			if len(s.messages()) != 1 {
				t.Fatal("refused, so nothing more was sent")
			}
		})
	}
}

func TestIMAPSendSignsInWithTheAddressWhenTheUsernameHasNoAt(t *testing.T) {
	f := newFakeIMAP(t, "TLS")
	s := newFakeSMTP(t, "TLS", smtpLoginFor)
	conn := imapConnection(f, s, appPassword)
	conn["username"] = icloudUser // iCloud's IMAP name; SMTP takes the full address
	o := runIt(t, "send\nTo: person@example.com\nSubject: Hi\n\nBody", opts{conn: conn, mode: "send", allowlist: []string{"@example.com"}})
	mustOK(t, o)
	if len(s.logins) != 1 || s.logins[0] != smtpLoginFor {
		t.Fatalf("want an SMTP sign-in as %s, got %v", smtpLoginFor, s.logins)
	}
}

func TestIMAPRefusedLoginAndUnreachableServers(t *testing.T) {
	f := newFakeIMAP(t, "TLS")
	o := runIt(t, "list", opts{conn: imapConnection(f, nil, wrongPasswd)})
	mustFail(t, o, "The mail server refused the sign-in for "+imapAccount+". A person updates the app password in Admin, Connections")

	s := newFakeSMTP(t, "TLS", "someone-else@example.test")
	o = runIt(t, "send\nTo: person@example.com\nSubject: Hi\n\nBody", opts{conn: imapConnection(f, s, appPassword), mode: "send", allowlist: []string{"@example.com"}})
	mustFail(t, o, "The mail server refused the sign-in for "+imapAccount+" to send.")
	contains(t, o.error, "Nothing was sent.")

	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	conn := imapConnection(f, nil, appPassword)
	conn["imap"].(map[string]any)["port"] = port
	mustFail(t, runIt(t, "list", opts{conn: conn}), "Could not reach 127.0.0.1 on port")
	conn = imapConnection(f, nil, appPassword)
	conn["smtp"] = map[string]any{"host": "127.0.0.1", "port": port, "security": "TLS"}
	o = runIt(t, "send\nTo: person@example.com\nSubject: Hi\n\nBody", opts{conn: conn, mode: "send", allowlist: []string{"@example.com"}})
	mustFail(t, o, "Could not reach 127.0.0.1 on port")
	contains(t, o.error, "Nothing was sent.")
}

func TestIMAPSMTPRejectionsSayWhetherToRetry(t *testing.T) {
	f := newFakeIMAP(t, "TLS")
	for code, words := range map[int]string{451: "try again later", 550: "The mail server refused the message: 550"} {
		s := newFakeSMTP(t, "TLS", imapUser)
		s.reject = code
		o := runIt(t, "send\nTo: person@example.com\nSubject: Hi\n\nBody", opts{conn: imapConnection(f, s, appPassword), mode: "send", allowlist: []string{"@example.com"}})
		mustFail(t, o, words)
		contains(t, o.error, "Nothing was sent.")
	}
}

// A server listening without TLS never sees the password: the plugin refuses before connecting.
func TestIMAPNeverSignsInWithoutTLS(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan []byte, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = c.Write([]byte("* OK IMAP4rev1 ready\r\n"))
		buf := make([]byte, 4096)
		n, _ := c.Read(buf)
		got <- buf[:n]
	}()
	f := &fakeIMAP{port: ln.Addr().(*net.TCPAddr).Port, sec: "NONE"}
	o := runIt(t, "list", opts{conn: imapConnection(f, nil, appPassword)})
	mustFail(t, o, "IMAP security must be TLS or STARTTLS")
	select {
	case b := <-got:
		t.Fatalf("the plugin spoke to a server in the clear: %q", b)
	case <-time.After(200 * time.Millisecond):
	}
	conn := imapConnection(f, nil, appPassword)
	conn["imap"].(map[string]any)["security"] = "TLS"
	conn["smtp"] = map[string]any{"host": "127.0.0.1", "port": 25, "security": "none"}
	o = runIt(t, "send\nTo: person@example.com\nSubject: Hi\n\nBody", opts{conn: conn, mode: "send", allowlist: []string{"@example.com"}})
	mustFail(t, o, "SMTP security must be TLS or STARTTLS")
}

func TestIMAPRefusesAServerItCannotVerify(t *testing.T) {
	f := newFakeIMAP(t, "TLS")
	saved := testRoots
	testRoots = x509.NewCertPool() // trusts nothing: the fake's certificate is now a stranger's
	t.Cleanup(func() { testRoots = saved })
	o := runIt(t, "list", imapOpts(f, nil, nil))
	mustFail(t, o, "did not prove it is that server")
}
