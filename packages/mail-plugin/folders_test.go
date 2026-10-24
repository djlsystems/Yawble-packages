package main

import (
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
)

// A move or label to a moveTo entry the mailbox does not have creates it at the top level, once,
// spelled as moveTo has it; anything else is refused or fails as before, with nothing created.

var newCustomers = map[string]any{"moveTo": []string{"New Customers", "Archive", "Clients/Acme", "Junk", "Sent", "Trash"}}

func gmailCreates(g *gmailBox) []call { return g.called("POST", "/labels") }

func TestGmailCreatesAMissingMoveToLabelOnceAndMoves(t *testing.T) {
	g := newGmailBox(t)
	a := g.add("a@example.org", "A", "a", time.Now(), false)
	b := g.add("b@example.org", "B", "b", time.Now(), false)
	o := runIt(t, "move \"new customers\" "+a, opts{conn: g.conn(), config: newCustomers, more: []string{"move \"New Customers\" " + b}})
	mustOK(t, o)
	contains(t, o.output, "Created folder New Customers; moved 1 messages: "+a+".", "Moved 1 messages to New Customers: "+b+".")
	creates := gmailCreates(g)
	if len(creates) != 1 {
		t.Fatalf("want one labels.create in the run, got %d", len(creates))
	}
	contains(t, string(creates[0].Body), `"name":"New Customers"`, `"labelListVisibility":"labelShow"`, `"messageListVisibility":"show"`)
	id := g.labels[len(g.labels)-1].ID
	for _, m := range []string{a, b} {
		if l := g.byID(m).Labels; !slices.Contains(l, id) || slices.Contains(l, "INBOX") {
			t.Fatalf("want %s moved into the new label, got %v", m, l)
		}
	}
	if n := len(g.called("GET", "/labels")); n != 2 {
		t.Fatalf("want the labels read once, and once more after the create; got %d reads", n)
	}

	// A later run finds it and creates nothing.
	c := g.add("c@example.org", "C", "c", time.Now(), false)
	o = runIt(t, "move \"New Customers\" "+c, gmailOpts(g, newCustomers))
	mustOK(t, o)
	contains(t, o.output, "Moved 1 messages to New Customers: "+c+".")
	if len(gmailCreates(g)) != 1 {
		t.Fatal("the label exists, so nothing more is created")
	}
}

func TestGmailLabelCreatesAMissingMoveToLabel(t *testing.T) {
	g := newGmailBox(t)
	a := g.add("a@example.org", "A", "a", time.Now(), false)
	o := runIt(t, "label \"new customers\" "+a, gmailOpts(g, newCustomers))
	mustOK(t, o)
	contains(t, o.output, "Created label New Customers; labelled 1 messages: "+a+".")
	if l := g.byID(a).Labels; !slices.Contains(l, g.labels[len(g.labels)-1].ID) || !slices.Contains(l, "INBOX") {
		t.Fatalf("a label adds, and leaves the Inbox: %v", l)
	}
}

func TestGmailTargetNotOnMoveToIsRefusedAndNothingCreated(t *testing.T) {
	g := newGmailBox(t)
	a := g.add("a@example.org", "A", "a", time.Now(), false)
	for _, cmd := range []string{"move", "label"} {
		mustFail(t, runIt(t, cmd+" Prospects "+a, gmailOpts(g, newCustomers)), "Refused: Prospects is not on moveTo; nothing was changed.")
	}
	if len(g.calls) != 0 {
		t.Fatalf("refused before anything is looked up or created; got %d calls", len(g.calls))
	}
}

func TestGmailCreateRaceLooksUpAgainAndMoves(t *testing.T) {
	g := newGmailBox(t)
	g.raceLabel = true
	a := g.add("a@example.org", "A", "a", time.Now(), false)
	o := runIt(t, "move \"New Customers\" "+a, gmailOpts(g, newCustomers))
	mustOK(t, o)
	contains(t, o.output, "moved 1 messages: "+a+".")
	if l := g.byID(a).Labels; !slices.Contains(l, g.labels[len(g.labels)-1].ID) {
		t.Fatalf("want it moved into the label made meanwhile, got %v", l)
	}
}

func TestGmailFailedCreateMovesNothingInGmailsWords(t *testing.T) {
	g := newGmailBox(t)
	g.failWith("POST", "/labels", 400, `{"error":{"code":400,"message":"Invalid label name"}}`)
	a := g.add("a@example.org", "A", "a", time.Now(), false)
	mustFail(t, runIt(t, "move \"New Customers\" "+a, gmailOpts(g, newCustomers)),
		`Gmail answered 400 Bad Request: Invalid label name. The label "New Customers" was not created; nothing was changed.`)
	if len(g.modifys) != 0 {
		t.Fatal("the create failed, so nothing moved")
	}
}

func TestGmailMissingNestedIsRefusedAndNothingCreated(t *testing.T) {
	g := newGmailBox(t)
	a := g.add("a@example.org", "A", "a", time.Now(), false)
	mustFail(t, runIt(t, "move Clients/Acme "+a, gmailOpts(g, newCustomers)),
		`Refused: Mailer creates only top-level folders, so a person creates the nested folder "Clients/Acme" in their mail app; nothing was changed.`)
	if len(gmailCreates(g)) != 0 || len(g.modifys) != 0 {
		t.Fatal("refused, so nothing created or moved")
	}
	// Once it exists, it is used.
	g.labels = append(g.labels, gmailLabel{"Label_9", "Clients/Acme"})
	mustOK(t, runIt(t, "move Clients/Acme "+a, gmailOpts(g, newCustomers)))
	if !slices.Contains(g.byID(a).Labels, "Label_9") || len(gmailCreates(g)) != 0 {
		t.Fatalf("want it moved to the existing nested label, got %v", g.byID(a).Labels)
	}
}

func TestGmailJunkSentTrashResolveByRoleAndAreNeverCreated(t *testing.T) {
	g := newGmailBox(t)
	g.labels = append(g.labels, gmailLabel{"SPAM", "SPAM"})
	a := g.add("a@example.org", "A", "a", time.Now(), false)
	b := g.add("b@example.org", "B", "b", time.Now(), false)
	mustOK(t, runIt(t, "move Junk "+a, gmailOpts(g, newCustomers)))
	if !slices.Contains(g.byID(a).Labels, "SPAM") {
		t.Fatalf("want Junk to be SPAM, got %v", g.byID(a).Labels)
	}
	mustOK(t, runIt(t, "label Sent "+b, gmailOpts(g, newCustomers)))
	if !slices.Contains(g.byID(b).Labels, "SENT") {
		t.Fatalf("want Sent to be SENT, got %v", g.byID(b).Labels)
	}
	// This mailbox has no TRASH label: it fails as before and is not created.
	mustFail(t, runIt(t, "move Trash "+b, gmailOpts(g, newCustomers)), `No label "Trash" in this mailbox.`)
	if len(gmailCreates(g)) != 0 {
		t.Fatal("a special-use name is never created")
	}
}

func TestGmailFoldersListsLabelsAndMarksMoveTo(t *testing.T) {
	g := newGmailBox(t)
	g.labels = append(g.labels, gmailLabel{"SPAM", "SPAM"}, gmailLabel{"CATEGORY_SOCIAL", "CATEGORY_SOCIAL"})
	o := runIt(t, "folders", gmailOpts(g, newCustomers))
	mustOK(t, o)
	want := "Labels in this mailbox (6), with the special-use role where one has it:\nINBOX (inbox)\nSENT (sent)\nDRAFT (drafts)\nReceipts\nArchive\nSPAM (junk)\n" +
		"moveTo:\nNew Customers: created on first use\nArchive: exists\nClients/Acme: nested: create it in your mail app\nJunk: exists\nSent: exists\n" +
		"Trash: missing: Mailer never creates a special-use folder"
	if o.output != want {
		t.Fatalf("folders said:\n%s\nwant:\n%s", o.output, want)
	}
	if len(g.called("POST", "")) != 0 {
		t.Fatal("folders changes nothing")
	}
	o = runIt(t, "folders", gmailOpts(g, nil))
	mustOK(t, o)
	contains(t, o.output, "moveTo is empty: move and label are off.")
}

func TestListSearchAndWatchNeverCreateAFolder(t *testing.T) {
	g := newGmailBox(t)
	cfg := map[string]any{"moveTo": []string{"New Customers"}, "watchFolder": "New Customers"}
	mustFail(t, runIt(t, "list folder:\"New Customers\"", gmailOpts(g, cfg)), `No label "New Customers" in this mailbox.`)
	mustFail(t, runIt(t, "search in:\"New Customers\" unread", gmailOpts(g, cfg)), `No label "New Customers" in this mailbox.`)
	runIt(t, "watch", gmailOpts(g, cfg))
	if len(gmailCreates(g)) != 0 {
		t.Fatal("only move and label create")
	}
}

func graphCreates(f *fakeGraph) []call {
	var out []call
	for _, c := range f.called("POST", "/me/mailFolders") {
		if c.Path == "/v1.0/me/mailFolders" {
			out = append(out, c)
		}
	}
	return out
}

func TestGraphCreatesAMissingMoveToFolderOnceAndMoves(t *testing.T) {
	f := newFakeGraph(t)
	a := f.add("a@example.org", "A", "a", time.Now(), false)
	b := f.add("b@example.org", "B", "b", time.Now(), false)
	o := runIt(t, "move \"new customers\" "+a, opts{conn: f.conn(), config: newCustomers, more: []string{"move \"New Customers\" " + b}})
	mustOK(t, o)
	contains(t, o.output, "Created folder New Customers; moved 1 messages: "+a+" (now AAMkAGI2MOVED", "Moved 1 messages to New Customers: "+b+" (now ")
	creates := graphCreates(f)
	if len(creates) != 1 {
		t.Fatalf("want one POST /me/mailFolders in the run, got %d", len(creates))
	}
	contains(t, string(creates[0].Body), `"displayName":"New Customers"`)
	var id string
	for k, v := range f.folders {
		if v == "New Customers" {
			id = k
		}
	}
	for _, m := range f.msgs {
		if m.Subject != "" && m.Folder != id {
			t.Fatalf("want every message in the new folder, %s is in %s", m.Subject, m.Folder)
		}
	}
}

func TestGraphTargetNotOnMoveToIsRefusedAndNothingCreated(t *testing.T) {
	f := newFakeGraph(t)
	a := f.add("a@example.org", "A", "a", time.Now(), false)
	for _, cmd := range []string{"move", "label"} {
		mustFail(t, runIt(t, cmd+" Prospects "+a, graphOpts(f, newCustomers)), "Refused: Prospects is not on moveTo; nothing was changed.")
	}
	if len(f.calls) != 0 {
		t.Fatalf("refused before anything is looked up or created; got %d calls", len(f.calls))
	}
}

func TestGraphLabelIsACategoryAndCreatesNoFolder(t *testing.T) {
	f := newFakeGraph(t)
	a := f.add("a@example.org", "A", "a", time.Now(), false)
	o := runIt(t, "label \"New Customers\" "+a, graphOpts(f, newCustomers))
	mustOK(t, o)
	contains(t, o.output, "Labelled 1 messages New Customers")
	if len(graphCreates(f)) != 0 {
		t.Fatal("a category needs no folder")
	}
}

func TestGraphCreateRaceLooksUpAgainAndMoves(t *testing.T) {
	f := newFakeGraph(t)
	f.raceFolder = true
	a := f.add("a@example.org", "A", "a", time.Now(), false)
	o := runIt(t, "move \"New Customers\" "+a, graphOpts(f, newCustomers))
	mustOK(t, o)
	contains(t, o.output, "moved 1 messages: "+a)
	if n := len(f.called("GET", "/me/mailFolders")); n < 2 {
		t.Fatalf("want the folders read again after the race, got %d reads", n)
	}
	if m := f.msgs[0]; f.folders[m.Folder] != "New Customers" {
		t.Fatalf("want it in the folder made meanwhile, got %s", m.Folder)
	}
}

func TestGraphFailedCreateMovesNothingInGraphsWords(t *testing.T) {
	f := newFakeGraph(t)
	f.failWith("POST", "/me/mailFolders", 400, `{"error":{"code":"ErrorQuotaExceeded","message":"The folder limit for this mailbox was reached."}}`)
	a := f.add("a@example.org", "A", "a", time.Now(), false)
	mustFail(t, runIt(t, "move \"New Customers\" "+a, graphOpts(f, newCustomers)),
		`Microsoft Graph answered 400 Bad Request: The folder limit for this mailbox was reached. The folder "New Customers" was not created; nothing was changed.`)
	if len(f.called("POST", "/me/messages")) != 0 || f.msgs[0].Folder != "inbox" {
		t.Fatal("the create failed, so nothing moved")
	}
}

func TestGraphMissingNestedIsRefusedAndNothingCreated(t *testing.T) {
	f := newFakeGraph(t)
	a := f.add("a@example.org", "A", "a", time.Now(), false)
	f.folders["AQMk-clients"] = "Clients"
	mustFail(t, runIt(t, "move Clients/Acme "+a, graphOpts(f, newCustomers)),
		`Refused: Mailer creates only top-level folders, so a person creates the nested folder "Clients/Acme" in their mail app; nothing was changed.`)
	if len(graphCreates(f)) != 0 || f.msgs[0].Folder != "inbox" {
		t.Fatal("refused, so nothing created or moved")
	}
	// Once it exists, it is used.
	f.folders["AQMk-acme"] = "Acme"
	f.parent = map[string]string{"AQMk-acme": "AQMk-clients"}
	mustOK(t, runIt(t, "move Clients/Acme "+a, graphOpts(f, newCustomers)))
	if f.msgs[0].Folder != "AQMk-acme" || len(graphCreates(f)) != 0 {
		t.Fatalf("want it moved to the existing nested folder, got %s", f.msgs[0].Folder)
	}
}

func TestGraphJunkSentTrashResolveByRoleAndAreNeverCreated(t *testing.T) {
	f := newFakeGraph(t)
	f.folders["junkemail"], f.folders["deleteditems"] = "Junk Email", "Deleted Items"
	a := f.add("a@example.org", "A", "a", time.Now(), false)
	b := f.add("b@example.org", "B", "b", time.Now(), false)
	c := f.add("c@example.org", "C", "c", time.Now(), false)
	mustOK(t, runIt(t, "move Junk "+a, graphOpts(f, newCustomers)))
	mustOK(t, runIt(t, "move Sent "+b, graphOpts(f, newCustomers)))
	mustOK(t, runIt(t, "move Trash "+c, graphOpts(f, newCustomers)))
	var got []string
	for _, m := range f.msgs {
		got = append(got, m.Folder)
	}
	if !slices.Equal(got, []string{"junkemail", "sentitems", "deleteditems"}) {
		t.Fatalf("want each in its well-known folder, got %v", got)
	}
	if len(graphCreates(f)) != 0 {
		t.Fatal("a special-use name is never created")
	}
}

func TestGraphFoldersListsFoldersAndMarksMoveTo(t *testing.T) {
	f := newFakeGraph(t)
	o := runIt(t, "folders", graphOpts(f, newCustomers))
	mustOK(t, o)
	contains(t, o.output, "Folders in this mailbox (5), with the special-use role where one has it:", "\nInbox (inbox)\n", "\nDrafts (drafts)\n",
		"\nSent Items (sent)\n", "\nArchive (archive)\n", "\nReceipts\n",
		"moveTo:\nNew Customers: created on first use\nArchive: exists\nClients/Acme: nested: create it in your mail app\nJunk: exists\nSent: exists\nTrash: exists")
	if n := len(f.called("POST", "")) + len(f.called("PATCH", "")); n != 0 {
		t.Fatalf("folders changes nothing; got %d writes", n)
	}
}

// imapFolders is what a folderSession keeps: the folders LIST shows and their special-use
// attributes, the CREATE and SUBSCRIBE commands it was given, and how it answers CREATE.
type imapFolders struct {
	mu         sync.Mutex
	names      []string
	roles      map[string]imap.MailboxAttr
	creates    []string
	subscribes []string
	failCreate string // when set, CREATE answers NO with this text
	race       bool   // CREATE finds the folder made meanwhile and answers [ALREADYEXISTS]
	prefix     string // the personal namespace's prefix, with "." its delimiter when set
}

type folderSession struct {
	imapserver.Session
	f *fakeIMAP
	x *imapFolders
}

func (s folderSession) List(w *imapserver.ListWriter, ref string, patterns []string, options *imap.ListOptions) error {
	s.x.mu.Lock()
	defer s.x.mu.Unlock()
	for _, name := range s.x.names {
		data := &imap.ListData{Mailbox: name, Delim: '/'}
		if s.x.prefix != "" {
			data.Delim = '.'
		}
		if a, ok := s.x.roles[name]; ok {
			data.Attrs = []imap.MailboxAttr{a}
		}
		if err := w.WriteList(data); err != nil {
			return err
		}
	}
	return nil
}

func (s folderSession) Create(name string, options *imap.CreateOptions) error {
	s.x.mu.Lock()
	defer s.x.mu.Unlock()
	s.x.creates = append(s.x.creates, name)
	switch {
	case slices.Contains(s.x.names, name):
		// As servers without the ALREADYEXISTS response code answer.
		return &imap.Error{Type: imap.StatusResponseTypeNo, Text: "Mailbox already exists"}
	case s.x.failCreate != "":
		return &imap.Error{Type: imap.StatusResponseTypeNo, Text: s.x.failCreate}
	case s.x.race:
		if err := s.f.user.Create(name, nil); err != nil {
			return err
		}
		s.x.names = append(s.x.names, name)
		return &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeAlreadyExists, Text: "Mailbox already exists"}
	}
	if err := s.Session.Create(name, options); err != nil {
		return err
	}
	s.x.names = append(s.x.names, name)
	return nil
}

func (s folderSession) Subscribe(name string) error {
	s.x.mu.Lock()
	s.x.subscribes = append(s.x.subscribes, name)
	s.x.mu.Unlock()
	return s.Session.Subscribe(name)
}

func (s folderSession) Move(w *imapserver.MoveWriter, numSet imap.NumSet, dest string) error {
	return s.Session.(imapserver.SessionMove).Move(w, numSet, dest)
}

func (s folderSession) Namespace() (*imap.NamespaceData, error) {
	if s.x.prefix != "" {
		return &imap.NamespaceData{Personal: []imap.NamespaceDescriptor{{Prefix: s.x.prefix, Delim: '.'}}}, nil
	}
	return s.Session.(imapserver.SessionNamespace).Namespace()
}

// newFoldersIMAP is the fake IMAP server with a folderSession, its folders those of newFakeIMAP
// and extra, each in roles marked with its special-use attribute.
func newFoldersIMAP(t *testing.T, roles map[string]imap.MailboxAttr, extra ...string) (*fakeIMAP, *imapFolders) {
	t.Helper()
	x := &imapFolders{roles: roles}
	f := newFakeIMAPWrapped(t, "TLS", "", func(f *fakeIMAP, s imapserver.Session) imapserver.Session {
		return folderSession{s, f, x}
	})
	for _, name := range extra {
		if err := f.user.Create(name, nil); err != nil {
			t.Fatal(err)
		}
	}
	x.names = append(slices.Clone(f.folders), extra...)
	return f, x
}

func TestIMAPCreatesAMissingMoveToFolderOnceAndMoves(t *testing.T) {
	f, x := newFoldersIMAP(t, nil)
	now := time.Now()
	a := f.deliver(t, "INBOX", rawMail("a@example.org", "A", "a", "", now, ""), now, false)
	b := f.deliver(t, "INBOX", rawMail("b@example.org", "B", "b", "", now, ""), now, false)
	o := runIt(t, "move \"new customers\" "+a, opts{conn: imapConnection(f, nil, appPassword), config: newCustomers, more: []string{"move \"New Customers\" " + b}})
	mustOK(t, o)
	contains(t, o.output, "Created folder New Customers; moved 1 messages: "+a+" (now ", "Moved 1 messages to New Customers: "+b+" (now ")
	if !slices.Equal(x.creates, []string{"New Customers"}) || !slices.Equal(x.subscribes, []string{"New Customers"}) {
		t.Fatalf("want one CREATE and SUBSCRIBE of New Customers, got %v and %v", x.creates, x.subscribes)
	}
	if f.count(t, "INBOX") != 0 || f.count(t, "New Customers") != 2 {
		t.Fatalf("want both in the new folder, INBOX %d New Customers %d", f.count(t, "INBOX"), f.count(t, "New Customers"))
	}
}

// On a server whose personal namespace has a prefix (INBOX. on Courier and some Dovecot setups),
// a folder Mailer made is INBOX.<name>, and later runs find it without creating it again.
func TestIMAPPrefixedNamespaceExistingFolderIsUsedAndNothingCreated(t *testing.T) {
	f, x := newFoldersIMAP(t, nil, "INBOX.New Customers")
	x.prefix = "INBOX."
	now := time.Now()
	a := f.deliver(t, "INBOX", rawMail("a@example.org", "A", "a", "", now, ""), now, false)
	o := runIt(t, "move \"New Customers\" "+a, imapOpts(f, nil, newCustomers))
	mustOK(t, o)
	contains(t, o.output, "Moved 1 messages to New Customers: "+a+" (now ", "INBOX.New%20Customers)")
	if strings.Contains(o.output, "Created folder") || len(x.creates) != 0 || len(x.subscribes) != 0 {
		t.Fatalf("the folder exists, so nothing is created; got %q, CREATE %v", o.output, x.creates)
	}
	if f.count(t, "INBOX.New Customers") != 1 {
		t.Fatal("want it moved into the existing folder")
	}
}

func TestIMAPPrefixedNamespaceCreatesOnceAndALaterRunFindsIt(t *testing.T) {
	f, x := newFoldersIMAP(t, nil)
	x.prefix = "INBOX."
	now := time.Now()
	a := f.deliver(t, "INBOX", rawMail("a@example.org", "A", "a", "", now, ""), now, false)
	o := runIt(t, "move \"New Customers\" "+a, imapOpts(f, nil, newCustomers))
	mustOK(t, o)
	contains(t, o.output, "Created folder New Customers; moved 1 messages: "+a+" (now ")
	if !slices.Equal(x.creates, []string{"INBOX.New Customers"}) || !slices.Equal(x.subscribes, []string{"INBOX.New Customers"}) {
		t.Fatalf("want one CREATE and SUBSCRIBE of INBOX.New Customers, got %v and %v", x.creates, x.subscribes)
	}
	for _, from := range []string{"b@example.org", "c@example.org"} {
		m := f.deliver(t, "INBOX", rawMail(from, "B", "b", "", now, ""), now, false)
		o = runIt(t, "move \"New Customers\" "+m, imapOpts(f, nil, newCustomers))
		mustOK(t, o)
		contains(t, o.output, "Moved 1 messages to New Customers: "+m+" (now ")
	}
	if len(x.creates) != 1 || len(x.subscribes) != 1 || strings.Contains(o.output, "Created folder") {
		t.Fatalf("later runs find the folder and create nothing; got CREATE %v", x.creates)
	}
	if f.count(t, "INBOX") != 0 || f.count(t, "INBOX.New Customers") != 3 {
		t.Fatalf("want all three moved, INBOX %d INBOX.New Customers %d", f.count(t, "INBOX"), f.count(t, "INBOX.New Customers"))
	}
}

// A server answering CREATE of an existing folder with a plain NO, without [ALREADYEXISTS], is
// never asked to create a folder that is there. A label there is a keyword and needs no folder.
func TestIMAPPrefixedNamespaceExistingFolderNeverMeetsAPlainNo(t *testing.T) {
	f, x := newFoldersIMAP(t, nil, "INBOX.New Customers")
	x.prefix = "INBOX."
	now := time.Now()
	for _, cmd := range []string{"move", "label"} {
		a := f.deliver(t, "INBOX", rawMail("a@example.org", "A", "a", "", now, ""), now, false)
		mustOK(t, runIt(t, cmd+" \"New Customers\" "+a, imapOpts(f, nil, newCustomers)))
	}
	if len(x.creates) != 0 {
		t.Fatalf("want no CREATE for an existing folder, got %v", x.creates)
	}
	if f.count(t, "INBOX.New Customers") != 1 {
		t.Fatalf("want the moved one there, got %d", f.count(t, "INBOX.New Customers"))
	}
}

func TestIMAPTargetNotOnMoveToIsRefusedAndNothingCreated(t *testing.T) {
	f, x := newFoldersIMAP(t, nil)
	now := time.Now()
	a := f.deliver(t, "INBOX", rawMail("a@example.org", "A", "a", "", now, ""), now, false)
	for _, cmd := range []string{"move", "label"} {
		mustFail(t, runIt(t, cmd+" Prospects "+a, imapOpts(f, nil, newCustomers)), "Refused: Prospects is not on moveTo; nothing was changed.")
	}
	if len(x.creates) != 0 || f.count(t, "INBOX") != 1 {
		t.Fatal("refused, so nothing created or moved")
	}
}

func TestIMAPCreateRaceLooksUpAgainAndMoves(t *testing.T) {
	f, x := newFoldersIMAP(t, nil)
	x.race = true
	now := time.Now()
	a := f.deliver(t, "INBOX", rawMail("a@example.org", "A", "a", "", now, ""), now, false)
	o := runIt(t, "move \"New Customers\" "+a, imapOpts(f, nil, newCustomers))
	mustOK(t, o)
	contains(t, o.output, "moved 1 messages: "+a)
	if f.count(t, "New Customers") != 1 {
		t.Fatal("want it moved into the folder made meanwhile")
	}
}

func TestIMAPFailedCreateMovesNothingInTheServersWords(t *testing.T) {
	f, x := newFoldersIMAP(t, nil)
	x.failCreate = "Quota exceeded: too many folders"
	now := time.Now()
	a := f.deliver(t, "INBOX", rawMail("a@example.org", "A", "a", "", now, ""), now, false)
	mustFail(t, runIt(t, "move \"New Customers\" "+a, imapOpts(f, nil, newCustomers)),
		`The IMAP server refused to create the folder "New Customers": Quota exceeded: too many folders. Nothing was changed.`)
	if f.count(t, "INBOX") != 1 {
		t.Fatal("the create failed, so nothing moved")
	}
}

func TestIMAPMissingNestedIsRefusedAndNothingCreated(t *testing.T) {
	f, x := newFoldersIMAP(t, nil, "Clients")
	now := time.Now()
	a := f.deliver(t, "INBOX", rawMail("a@example.org", "A", "a", "", now, ""), now, false)
	mustFail(t, runIt(t, "move Clients/Acme "+a, imapOpts(f, nil, newCustomers)),
		`Refused: Mailer creates only top-level folders, so a person creates the nested folder "Clients/Acme" in their mail app; nothing was changed.`)
	if len(x.creates) != 0 || f.count(t, "INBOX") != 1 {
		t.Fatal("refused, so nothing created or moved")
	}
	// Once it exists, it is used.
	if err := f.user.Create("Clients/Acme", nil); err != nil {
		t.Fatal(err)
	}
	x.names = append(x.names, "Clients/Acme")
	mustOK(t, runIt(t, "move Clients/Acme "+a, imapOpts(f, nil, newCustomers)))
	if f.count(t, "Clients/Acme") != 1 || len(x.creates) != 0 {
		t.Fatal("want it moved to the existing nested folder")
	}
}

func TestIMAPJunkSentTrashResolveByRoleAndAreNeverCreated(t *testing.T) {
	roles := map[string]imap.MailboxAttr{"Bulk Mail": imap.MailboxAttrJunk, "Sent Messages": imap.MailboxAttrSent}
	f, x := newFoldersIMAP(t, roles, "Bulk Mail", "Sent Messages")
	now := time.Now()
	a := f.deliver(t, "INBOX", rawMail("a@example.org", "A", "a", "", now, ""), now, false)
	b := f.deliver(t, "INBOX", rawMail("b@example.org", "B", "b", "", now, ""), now, false)
	mustOK(t, runIt(t, "move Junk "+a, imapOpts(f, nil, newCustomers)))
	mustOK(t, runIt(t, "move Sent "+b, imapOpts(f, nil, newCustomers)))
	if f.count(t, "Bulk Mail") != 1 || f.count(t, "Sent Messages") != 1 {
		t.Fatal("want Junk and Sent found by their special-use attributes")
	}
	// This mailbox has no Trash: it fails as before and is not created.
	c := f.deliver(t, "INBOX", rawMail("c@example.org", "C", "c", "", now, ""), now, false)
	mustFail(t, runIt(t, "move Trash "+c, imapOpts(f, nil, newCustomers)), `No folder "Trash" in this mailbox.`)
	if len(x.creates) != 0 {
		t.Fatalf("a special-use name is never created, got %v", x.creates)
	}
}

func TestIMAPFoldersListsFoldersAndMarksMoveTo(t *testing.T) {
	roles := map[string]imap.MailboxAttr{"Bulk Mail": imap.MailboxAttrJunk, "Sent Messages": imap.MailboxAttrSent}
	f, x := newFoldersIMAP(t, roles, "Bulk Mail", "Sent Messages")
	o := runIt(t, "folders", imapOpts(f, nil, newCustomers))
	mustOK(t, o)
	want := "Folders in this mailbox (6), with the special-use role where one has it:\nINBOX (inbox)\nDrafts\nArchive\nReceipts\nBulk Mail (junk)\nSent Messages (sent)\n" +
		"moveTo:\nNew Customers: created on first use\nArchive: exists\nClients/Acme: nested: create it in your mail app\nJunk: exists\nSent: exists\n" +
		"Trash: missing: Mailer never creates a special-use folder"
	if o.output != want {
		t.Fatalf("folders said:\n%s\nwant:\n%s", o.output, want)
	}
	if len(x.creates) != 0 {
		t.Fatal("folders changes nothing")
	}
	mustFail(t, runIt(t, "folders all", imapOpts(f, nil, nil)), "folders takes nothing")
	if !strings.Contains(usage, "`folders`") {
		t.Fatal("the usage names folders")
	}
}
