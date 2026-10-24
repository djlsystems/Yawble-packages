package main

import (
	"strings"
	"testing"
	"time"
)

const replyText = "Thanks for your text, we will call you back today."

func autoCfg(kv ...any) map[string]any {
	c := cfg("mode", "send", "autoReply", true, "autoReplyText", replyText)
	for i := 0; i < len(kv); i += 2 {
		c[kv[i].(string)] = kv[i+1]
	}
	return c
}

// autoReplyLine is the poll's one auto-reply progress line, or "".
func autoReplyLine(t *testing.T, r result) string {
	t.Helper()
	var lines []string
	for _, p := range r.of("progress") {
		if s := p["status"].(string); strings.HasPrefix(s, "poll: auto-reply") {
			lines = append(lines, s)
		}
	}
	if len(lines) > 1 {
		t.Fatalf("more than one auto-reply progress line: %v", lines)
	}
	if len(lines) == 0 {
		return ""
	}
	return lines[0]
}

// nextSite carries a poll's state into the next poll.
func nextSite(r result, msgs ...messageDoc) *siteState {
	s := pollSite(testNow)
	s.state[0].Doc = r.put(colState, "poll")
	for _, m := range msgs {
		s.messages = append(s.messages, docIn{ID: m.Sid, Doc: m, UpdatedBy: "Team/twilio"})
	}
	return s
}

func TestAutoReplyOffSendsNothing(t *testing.T) {
	f := newFake(t)
	f.inbound = []message{inMsg("SMa", ownCell, "are you open?", testNow.Add(-2*time.Minute))}
	r := runWith(t, opts{cfg: cfg("mode", "send", "autoReplyText", replyText), site: pollSite(testNow.Add(-5 * time.Minute))}, "poll")
	if !r.ok {
		t.Fatal(r.text)
	}
	if len(f.posts()) != 0 {
		t.Fatalf("autoReply off sent %d text(s)", len(f.posts()))
	}
	rec := r.published("received")
	if rec == nil {
		t.Fatal("no received event")
	}
	if _, ok := rec["autoReplied"]; ok {
		t.Fatalf("autoReply off added autoReplied to the event: %v", rec)
	}
	if _, ok := rec["messages"].([]any)[0].(map[string]any)["autoReplied"]; ok {
		t.Fatalf("autoReply off marked a message: %v", rec)
	}
	if _, ok := r.put(colState, "poll")["autoRepliedAt"]; ok {
		t.Fatal("autoReply off kept auto-reply times")
	}
	if autoReplyLine(t, r) != "" || strings.Contains(r.text, "Auto-replied") {
		t.Fatalf("autoReply off reported auto-replies: %s", r.text)
	}
	wantOK(t, r, "Poll: 0 status change(s) (0 delivered, 0 failed), 1 new inbound text(s), 0 opt-out(s), 0 opt-in(s).")
}

func TestAutoReplyAnswersOncePerWindowThroughTheSendPath(t *testing.T) {
	f := newFake(t)
	f.inbound = []message{
		inMsg("SMa", ownCell, "are you open?", testNow.Add(-3*time.Minute)),
		inMsg("SMb", ownCell, "hello??", testNow.Add(-2*time.Minute)),
	}
	c := autoCfg("messagePrefix", "Acme: ", "optOutFooter", "every")
	r := runWith(t, opts{cfg: c, site: pollSite(testNow.Add(-5 * time.Minute))}, "poll")
	if !r.ok {
		t.Fatal(r.text)
	}
	want := "Acme: " + replyText + " " + optOutFooter
	posts := f.posts()
	if len(posts) != 1 || posts[0].Form.Get("To") != ownCell || posts[0].Form.Get("Body") != want {
		t.Fatalf("want one reply to %s with %q, got %v", ownCell, want, posts)
	}
	if posts[0].Form.Get("ValidityPeriod") == "" {
		t.Fatal("the reply did not go through the send path")
	}

	doc := r.put(colMessages, "SM0000000000000000000000000000new1")
	if doc == nil || doc["body"] != want || doc["tag"] != autoReplyTag || doc["direction"] != "out" || doc["test"] != nil {
		t.Fatalf("the record is not the auto-reply as sent: %v", doc)
	}

	rec := r.published("received")
	if rec == nil || rec["count"].(float64) != 2 || rec["autoReplied"].(float64) != 1 {
		t.Fatalf("received: %v", rec)
	}
	items := rec["messages"].([]any)
	if items[0].(map[string]any)["autoReplied"] != true || items[1].(map[string]any)["autoReplied"] != nil {
		t.Fatalf("the answered text is not marked: %v", items)
	}
	st := r.put(colState, "poll")
	if st["autoRepliedAt"].(map[string]any)[ownCell] != testNow.Format(time.RFC3339) {
		t.Fatalf("the auto-reply time is not kept: %v", st["autoRepliedAt"])
	}
	if st["sentToday"].(float64) != 1 {
		t.Fatalf("the reply did not count against the caps: %v", st["sentToday"])
	}
	if line := autoReplyLine(t, r); line != "poll: auto-reply answered 1 text(s), skipped 1: 1 answered in the last 24 hours." {
		t.Fatalf("progress line: %q", line)
	}
	wantContains(t, r, "Auto-replied to 1.")

	// Two hours later the window still holds, and it survives the run through the site's state.
	f.inbound = []message{inMsg("SMc", ownCell, "still there?", testNow.Add(2*time.Hour-time.Minute))}
	r2 := runWith(t, opts{cfg: c, site: nextSite(r), now: testNow.Add(2 * time.Hour)}, "poll")
	if len(f.posts()) != 1 {
		t.Fatal("a second auto-reply went out inside the window")
	}
	if rec := r2.published("received"); rec == nil || rec["autoReplied"].(float64) != 0 {
		t.Fatalf("received: %v", rec)
	}

	// After the window, the next text is answered again.
	f.inbound = []message{inMsg("SMd", ownCell, "next day", testNow.Add(25*time.Hour-time.Minute))}
	runWith(t, opts{cfg: c, site: nextSite(r2), now: testNow.Add(25 * time.Hour)}, "poll")
	if len(f.posts()) != 2 {
		t.Fatalf("want a reply after the window, got %d post(s) in all", len(f.posts()))
	}
}

func TestAutoReplySkipsOptedOutUnallowedAndKeywordSenders(t *testing.T) {
	f := newFake(t)
	stranger, optedOut := "+15550001111", "+15550002222"
	f.inbound = []message{
		inMsg("SMa", stranger, "who is this? secret words", testNow.Add(-4*time.Minute)),
		inMsg("SMb", optedOut, "hello again", testNow.Add(-4*time.Minute)),
		inMsg("SMc", ownCell, "help", testNow.Add(-3*time.Minute)),
		inMsg("SMd", ownCell, " Info ", testNow.Add(-3*time.Minute)),
		inMsg("SMe", ownCell, "START", testNow.Add(-2*time.Minute)),
		inMsg("SMf", "+15550003333", "STOP", testNow.Add(-2*time.Minute)),
	}
	site := pollSite(testNow.Add(-5 * time.Minute))
	site.state[0].Doc.(map[string]any)["optedOut"] = map[string]any{optedOut: map[string]any{"keyword": "STOP", "at": "2026-10-01T00:00:00Z"}}
	r := runWith(t, opts{cfg: autoCfg("sendAllowlist", []string{ownCell, optedOut}), site: site}, "poll")
	if !r.ok {
		t.Fatal(r.text)
	}
	if len(f.posts()) != 0 {
		t.Fatalf("a refused or keyword sender was answered: %v", f.posts())
	}
	rec := r.published("received")
	if rec == nil || rec["autoReplied"].(float64) != 0 {
		t.Fatalf("received: %v", rec)
	}
	line := autoReplyLine(t, r)
	if line != "poll: auto-reply answered 0 text(s), skipped 2: 1 not on sendAllowlist or the active consented list, 1 opted out." {
		t.Fatalf("progress line: %q", line)
	}
	for _, leak := range []string{"+1555", "secret", "hello"} {
		if strings.Contains(line, leak) {
			t.Fatalf("the progress line names a number or quotes a text: %q", line)
		}
	}
	for _, m := range r.of("site.put") {
		if m["collection"] == colMessages && m["doc"].(map[string]any)["tag"] != nil {
			t.Fatalf("a skipped text was recorded as an auto-reply: %v", m["doc"])
		}
	}
}

func TestAutoReplyDryRunRecordsWithoutSending(t *testing.T) {
	f := newFake(t)
	f.inbound = []message{inMsg("SMa", ownCell, "are you open?", testNow.Add(-2*time.Minute))}
	r := runWith(t, opts{cfg: autoCfg("mode", "dryRun", "messagePrefix", "Acme: "), site: pollSite(testNow.Add(-5 * time.Minute))}, "poll")
	if !r.ok {
		t.Fatal(r.text)
	}
	if len(f.posts()) != 0 {
		t.Fatal("a dry run sent a text")
	}
	doc := r.put(colMessages, "dryrun-SMa")
	if doc == nil || doc["body"] != "Acme: "+replyText || doc["tag"] != autoReplyTag || doc["dryRun"] != true || doc["status"] != "dry run" || doc["to"] != ownCell {
		t.Fatalf("the dry run is not recorded: %v", doc)
	}
	if rec := r.published("received"); rec == nil || rec["autoReplied"].(float64) != 1 {
		t.Fatalf("received: %v", rec)
	}
	if st := r.put(colState, "poll"); st["sentToday"].(float64) != 0 {
		t.Fatal("a dry run counted against the caps")
	}
	if line := autoReplyLine(t, r); line != "poll: auto-reply answered 1 text(s) as a dry run." {
		t.Fatalf("progress line: %q", line)
	}

	// The next poll does not ask Twilio about the dry run.
	f.inbound = nil
	var kept messageDoc
	kept.Sid, kept.Direction, kept.Status, kept.DryRun, kept.Tag, kept.Created = "dryrun-SMa", "out", "dry run", true, autoReplyTag, testNow.Format(time.RFC3339)
	r2 := runWith(t, opts{cfg: autoCfg("mode", "dryRun"), site: nextSite(r, kept), now: testNow.Add(10 * time.Minute)}, "poll")
	for _, c := range f.calls {
		if strings.Contains(c.Path, "dryrun-") {
			t.Fatalf("the dry run record was fetched: %s", c.Path)
		}
	}
	if !r2.quiet {
		t.Fatalf("want a quiet poll, got %s", r2.text)
	}
}

func TestAPollWhoseOnlyNewsIsItsAutoRepliesStaysQuiet(t *testing.T) {
	f := newFake(t)
	f.messages["SM1"] = outMsg("SM1", ownCell, "delivered", 0, "")
	reply := sentDoc("SM1", ownCell, "queued", false)
	reply.Tag = autoReplyTag
	r := runWith(t, opts{cfg: autoCfg(), site: pollSite(testNow.Add(-5*time.Minute), reply)}, "poll")
	if !r.ok || !r.quiet || len(r.of("publish")) != 0 {
		t.Fatalf("want a quiet poll, got ok=%t quiet=%t %s", r.ok, r.quiet, r.text)
	}
	if r.put(colMessages, "SM1")["status"] != "delivered" {
		t.Fatal("the auto-reply's delivery was not recorded")
	}

	// A failed auto-reply is news.
	f.messages["SM1"] = outMsg("SM1", ownCell, "undelivered", 30003, "Unreachable destination handset")
	r = runWith(t, opts{cfg: autoCfg(), site: pollSite(testNow.Add(-5*time.Minute), reply)}, "poll")
	if r.quiet || r.published("delivery") == nil {
		t.Fatalf("a failed auto-reply was kept quiet: %s", r.text)
	}
}

func TestAutoReplyOnWithAnEmptyTextIsANextStepAndSendsNothing(t *testing.T) {
	f := newFake(t)
	r := runWith(t, opts{cfg: cfg("autoReply", true), site: pollSite(testNow)}, "check")
	wantContains(t, r, "\nNext: auto-reply — autoReply is on but autoReplyText is empty")

	f.inbound = []message{inMsg("SMa", ownCell, "are you open?", testNow.Add(-2*time.Minute))}
	r = runWith(t, opts{cfg: autoCfg("autoReplyText", ""), site: pollSite(testNow.Add(-5 * time.Minute))}, "poll")
	if len(f.posts()) != 0 {
		t.Fatal("an empty auto-reply was sent")
	}
	if rec := r.published("received"); rec == nil || rec["autoReplied"].(float64) != 0 {
		t.Fatalf("received: %v", rec)
	}

	r = runWith(t, opts{cfg: autoCfg(), site: pollSite(testNow)}, "check")
	wantContains(t, r, "\nDone: auto-reply — on: a new text is answered with \""+replyText+"\", at most once in 24 hours to each number")
	r = runWith(t, opts{site: pollSite(testNow)}, "check")
	if strings.Contains(r.text, "auto-reply") {
		t.Fatalf("check mentions the auto-reply while it is off: %s", r.text)
	}
}

func TestAutoReplySettingsAreChecked(t *testing.T) {
	newFake(t)
	r := run1(t, autoCfg("autoReplyText", strings.Repeat("x", 301)), "check")
	if r.ok || !strings.Contains(r.text, "`autoReplyText` must be at most 300 characters") {
		t.Fatalf("a 301-character text was accepted: %s", r.text)
	}
	for _, h := range []any{0, 721, 1.5} {
		r = run1(t, autoCfg("autoReplyOncePerHours", h), "check")
		if r.ok || !strings.Contains(r.text, "`autoReplyOncePerHours` must be a whole number from 1 to 720") {
			t.Fatalf("autoReplyOncePerHours %v was accepted: %s", h, r.text)
		}
	}
}
