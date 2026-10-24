package main

import (
	"strings"
	"testing"
	"time"
)

// noSecret fails when the password (in either of its written forms) or a token is anywhere in a
// run's output: result, progress, the published event or the stored watch document.
func noSecret(t *testing.T, o outcome, secrets ...string) {
	t.Helper()
	for _, s := range secrets {
		for _, form := range []string{s, strings.ReplaceAll(s, " ", "")} {
			if strings.Contains(o.raw, form) {
				t.Fatalf("a secret appeared in output:\n%s", o.raw)
			}
		}
	}
}

func TestThePasswordNeverAppearsInIMAPOutput(t *testing.T) {
	f := newFakeIMAP(t, "TLS")
	now := time.Now()
	leak := "my password is " + appPassword + " or " + strings.ReplaceAll(appPassword, " ", "")
	id := f.deliver(t, "INBOX", rawMail("a@example.org", "pw "+strings.ReplaceAll(appPassword, " ", ""), leak, "", now, ""), now, false)
	box := imapWatchBox{f}
	st := newSiteStore()
	mustOK(t, st.run(t, "watch", watchOpts(box, nil)))
	f.deliver(t, "INBOX", rawMail("a@example.org", "again "+strings.ReplaceAll(appPassword, " ", ""), leak, "", now, ""), now, false)

	for _, instr := range []string{"list", "search password", "read " + id} {
		o := runIt(t, instr, imapOpts(f, nil, nil))
		mustOK(t, o)
		if !strings.Contains(o.raw, "[redacted]") {
			t.Fatalf("%s: the case never echoed the password, so it proves nothing:\n%s", instr, o.raw)
		}
		noSecret(t, o, appPassword)
	}
	o := st.run(t, "watch", watchOpts(box, nil))
	if len(o.of("publish")) != 1 || len(o.of("site.put")) != 1 {
		t.Fatalf("want the event and the document, both redacted:\n%s", o.raw)
	}
	noSecret(t, o, appPassword)

	noSecret(t, runIt(t, "list", opts{conn: imapConnection(f, nil, wrongPasswd)}), wrongPasswd)
	s := newFakeSMTP(t, "TLS", "nobody@example.test")
	noSecret(t, runIt(t, "send\nTo: person@example.com\nSubject: Hi\n\n"+appPassword, opts{conn: imapConnection(f, s, appPassword), mode: "send", allowlist: []string{"@example.com"}}), appPassword)
}

func TestTheTokenNeverAppearsInGraphOutput(t *testing.T) {
	f := newFakeGraph(t)
	box := graphWatchBox{f}
	st := newSiteStore()
	mustOK(t, st.run(t, "watch", watchOpts(box, nil)))
	id := f.add("a@example.org", "token "+graphToken, "<p>Bearer "+graphToken+"</p>", time.Now(), false)
	for _, instr := range []string{"list", "read " + id, "search token"} {
		o := runIt(t, instr, graphOpts(f, nil))
		mustOK(t, o)
		if !strings.Contains(o.raw, "[redacted]") {
			t.Fatalf("%s: the case never echoed the token:\n%s", instr, o.raw)
		}
		noSecret(t, o, graphToken)
	}
	o := st.run(t, "watch", watchOpts(box, nil))
	if len(o.of("publish")) != 1 {
		t.Fatalf("want the event:\n%s", o.raw)
	}
	noSecret(t, o, graphToken)

	f.failWith("GET", "/me/mailFolders/inbox/messages", 400, `{"error":{"code":"BadRequest","message":"Authorization: Bearer `+graphToken+`"}}`)
	o = runIt(t, "list", graphOpts(f, nil))
	mustFail(t, o, "Microsoft Graph answered 400")
	noSecret(t, o, graphToken)
}

func TestTheTokenNeverAppearsInGmailWatchOutput(t *testing.T) {
	g := newGmailBox(t)
	box := gmailWatchBox{g}
	st := newSiteStore()
	mustOK(t, st.run(t, "watch", watchOpts(box, nil)))
	g.add("a@example.org", "token "+token, "Bearer "+token, time.Now(), false)
	o := st.run(t, "watch", watchOpts(box, nil))
	if len(o.of("publish")) != 1 || !strings.Contains(o.raw, "[redacted]") {
		t.Fatalf("want the event, redacted:\n%s", o.raw)
	}
	noSecret(t, o, token)
}

func TestNoCommandTakesMoreIdsThanMaxResults(t *testing.T) {
	f := newFakeIMAP(t, "TLS")
	o := imapOpts(f, nil, map[string]any{"markRead": true, "moveTo": []string{"Archive"}})
	o.maxResults = 3
	for _, instr := range []string{"mark-read 1 2 3 4", "mark-unread 1,2,3,4", "move Archive 1 2 3 4", "label Archive 1 2 3 4"} {
		mustFail(t, runIt(t, instr, o), "at most 3 ids at once (maxResults); nothing was changed.")
	}
}

func TestABatchStoresEachCommandsWatchChangeAndIsQuietOnlyForQuietChecks(t *testing.T) {
	f := newFakeIMAP(t, "TLS")
	box := imapWatchBox{f}
	st := newSiteStore()
	mustOK(t, st.run(t, "watch", watchOpts(box, nil)))
	id := box.arrive(t, "a@example.org", "A", time.Now(), false)
	o := st.run(t, "watch", watchOpts(box, nil))
	if !o.quiet {
		t.Fatal("a check that only publishes is quiet: its event wakes whoever needs it")
	}
	b := watchOpts(box, nil)
	b.more = []string{"watch"}
	o = st.run(t, "ack "+id, b)
	mustOK(t, o)
	if o.quiet {
		t.Fatal("a batch with an ack is not quiet")
	}
	if len(o.of("site.put")) != 2 {
		t.Fatalf("each command's change is stored as it happens, got %d site.put", len(o.of("site.put")))
	}
	if len(o.of("publish")) != 0 {
		t.Fatalf("the ack ran first, so the check lists nothing:\n%s", o.raw)
	}
}
