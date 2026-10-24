package main

import (
	"strings"
	"testing"
	"time"
)

func pollSite(lastPoll time.Time, msgs ...messageDoc) *siteState {
	s := &siteState{state: []docIn{{ID: "poll", Doc: map[string]any{"lastPollAt": lastPoll.Format(time.RFC3339), "inboundSeen": map[string]any{}}},
		{ID: "setup", Doc: map[string]any{"checkedAt": testNow.Add(-time.Minute).Format(time.RFC3339)}}}}
	for _, m := range msgs {
		s.messages = append(s.messages, docIn{ID: m.Sid, Doc: m, UpdatedBy: "Team/twilio"})
	}
	return s
}

func sentDoc(sid, to, status string, test bool) messageDoc {
	return messageDoc{Sid: sid, Direction: "out", To: to, From: tollFree, Status: status, Test: test, Created: testNow.Add(-30 * time.Minute).Format(time.RFC3339)}
}

func TestPollQuietWhenNothingChanged(t *testing.T) {
	f := newFake(t)
	f.messages["SM1"] = outMsg("SM1", ownCell, "sent", 0, "")
	r := runWith(t, opts{site: pollSite(testNow.Add(-5*time.Minute), sentDoc("SM1", ownCell, "sent", false))}, "poll")
	if !r.ok || !r.quiet || r.text != "Nothing new." {
		t.Fatalf("want a quiet ok, got ok=%t quiet=%t %q", r.ok, r.quiet, r.text)
	}
	if len(r.of("publish")) != 0 {
		t.Fatal("a quiet poll published")
	}
	if st := r.put(colState, "poll"); st == nil || st["lastPollAt"] != testNow.Format(time.RFC3339) {
		t.Fatal("lastPollAt not recorded")
	}
}

func TestPollStatusTransitions(t *testing.T) {
	f := newFake(t)
	f.messages["SM1"] = outMsg("SM1", ownCell, "sent", 0, "")
	f.messages["SM2"] = outMsg("SM2", consented, "undelivered", 30003, "Unreachable destination handset")
	f.messages["SMold"] = outMsg("SMold", ownCell, "delivered", 0, "")
	old := sentDoc("SMold", ownCell, "sent", false)
	old.Created = testNow.Add(-100 * time.Hour).Format(time.RFC3339) // beyond pollLookbackHours: not re-checked
	site := pollSite(testNow.Add(-5*time.Minute), sentDoc("SM1", ownCell, "queued", false), sentDoc("SM2", consented, "sent", false),
		sentDoc("SM3", ownCell, "delivered", false), old)

	r := runWith(t, opts{site: site}, "poll")
	if !r.ok || r.quiet {
		t.Fatalf("ok=%t quiet=%t %s", r.ok, r.quiet, r.text)
	}
	if r.put(colMessages, "SM1")["status"] != "sent" {
		t.Fatal("queued -> sent not recorded")
	}
	if r.put(colMessages, "SM2")["status"] != "undelivered" || r.put(colMessages, "SM2")["errorCode"].(float64) != 30003 {
		t.Fatal("sent -> undelivered not recorded")
	}
	for _, c := range f.calls {
		if strings.Contains(c.Path, "/Messages/SM3") || strings.Contains(c.Path, "/Messages/SMold") {
			t.Fatalf("a final or too-old message was fetched: %s", c.Path)
		}
	}
	d := r.published("delivery")
	if d == nil {
		t.Fatal("no delivery event for a failure")
	}
	failed := d["failed"].([]any)
	if len(failed) != 1 || d["dryRun"] != false {
		t.Fatalf("delivery payload: %v", d)
	}
	item := failed[0].(map[string]any)
	if item["sid"] != "SM2" || item["error_code"].(float64) != 30003 || item["error"] != "Message SM2 was undelivered with 30003: Unreachable destination handset. https://www.twilio.com/docs/api/errors/30003" {
		t.Fatalf("failed item: %v", item)
	}
	wantContains(t, r, "Poll: 2 status change(s) (0 delivered, 1 failed)")
}

func TestPollDeliveredOnlyPublishesForTheSetupTestMessage(t *testing.T) {
	f := newFake(t)
	f.messages["SM1"] = outMsg("SM1", ownCell, "delivered", 0, "")
	r := runWith(t, opts{site: pollSite(testNow.Add(-5*time.Minute), sentDoc("SM1", ownCell, "sent", false))}, "poll")
	if r.published("delivery") != nil {
		t.Fatal("a delivered-only poll published delivery")
	}

	r = runWith(t, opts{site: pollSite(testNow.Add(-5*time.Minute), sentDoc("SM1", ownCell, "sent", true))}, "poll")
	d := r.published("delivery")
	if d == nil || d["delivered"].(float64) != 1 || len(d["failed"].([]any)) != 0 {
		t.Fatalf("the setup test message's delivery was not published: %v", d)
	}
	setup := r.put(colState, "setup")
	if setup == nil || setup["firstDelivered"] == nil {
		t.Fatalf("firstDelivered not recorded: %v", setup)
	}
	wantContains(t, r, "Poll: 1 status change(s) (1 delivered, 0 failed)")
	found := false
	for _, s := range setup["steps"].([]any) {
		if s.(map[string]any)["text"] == "first message delivered to +15551234567" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the checklist does not say the first message was delivered: %v", setup["steps"])
	}
}

func TestPollInboundReceivedStopAndStart(t *testing.T) {
	f := newFake(t)
	f.pageSize = 2
	f.inbound = []message{
		inMsg("SMa", consented, "Thanks! see you then", testNow.Add(-3*time.Minute)),
		inMsg("SMb", "+15559876500", " stop ", testNow.Add(-2*time.Minute)),
		inMsg("SMc", "+15559876501", "START", testNow.Add(-2*time.Minute)),
		inMsg("SMd", ownCell, "Revoke", testNow.Add(-1*time.Minute)),
		inMsg("SMseen", consented, "old news", testNow.Add(-4*time.Minute)),
	}
	site := pollSite(testNow.Add(-5 * time.Minute))
	site.state[0].Doc.(map[string]any)["inboundSeen"] = map[string]any{"SMseen": testNow.Add(-4 * time.Minute).Format(time.RFC3339)}
	site.recipients = []docIn{
		recip("+15559876500", "active", "person@example.com"),
		recip("+15559876501", "opted-out", "Team/twilio", "optOutKeyword", "STOP", "optedOutAt", "2026-10-01T00:00:00Z"),
	}
	r := runWith(t, opts{site: site}, "poll")
	if !r.ok {
		t.Fatal(r.text)
	}

	rec := r.published("received")
	if rec == nil || rec["count"].(float64) != 1 || rec["number"] != tollFree || rec["more"].(float64) != 0 || rec["dryRun"] != false {
		t.Fatalf("received: %v", rec)
	}
	m := rec["messages"].([]any)[0].(map[string]any)
	if m["sid"] != "SMa" || m["snippet"] != "<<<untrusted sms content>>>Thanks! see you then<<<end of untrusted sms content>>>" {
		t.Fatalf("received message: %v", m)
	}

	out := r.published("optout")
	if out == nil || out["count"].(float64) != 2 {
		t.Fatalf("optout: %v", out)
	}
	doc := r.put(colRecipients, "15559876500")
	if doc == nil || doc["status"] != "opted-out" || doc["optOutKeyword"] != "STOP" {
		t.Fatalf("STOP did not opt the entry out: %v", doc)
	}
	st := r.put(colState, "poll")
	if st["optedOut"].(map[string]any)[ownCell].(map[string]any)["keyword"] != "REVOKE" {
		t.Fatalf("the allowlisted number's opt-out was not kept: %v", st["optedOut"])
	}
	seen := st["inboundSeen"].(map[string]any)
	for _, sid := range []string{"SMa", "SMb", "SMc", "SMd", "SMseen"} {
		if seen[sid] == nil {
			t.Fatalf("%s not marked seen", sid)
		}
	}

	in := r.published("optin")
	if in == nil || in["count"].(float64) != 1 {
		t.Fatalf("optin: %v", in)
	}
	// START never reactivates an entry.
	if d := r.put(colRecipients, "15559876501"); d != nil {
		t.Fatalf("START rewrote the opted-out entry: %v", d)
	}
	if r.put(colMessages, "SMa") == nil {
		t.Fatal("inbound text not kept in messages")
	}

	// The opted-out allowlisted number is now refused.
	site2 := &siteState{state: []docIn{{ID: "poll", Doc: st}}}
	wantFail(t, runWith(t, opts{site: site2}, sms(ownCell, "x")), "Refused: +15551234567 opted out (REVOKE on 2026-10-09); nothing was sent.")
}

func TestPollFirstRunLooksBackAndFillsTheChecklist(t *testing.T) {
	f := newFake(t)
	f.inbound = []message{
		inMsg("SMrecent", ownCell, "hello", testNow.Add(-10*time.Hour)),
		inMsg("SMancient", ownCell, "too old", testNow.Add(-100*time.Hour)),
	}
	r := runWith(t, opts{}, "poll")
	if !r.ok {
		t.Fatal(r.text)
	}
	rec := r.published("received")
	if rec == nil || rec["count"].(float64) != 1 || rec["messages"].([]any)[0].(map[string]any)["sid"] != "SMrecent" {
		t.Fatalf("first poll: %v", rec)
	}
	if setup := r.put(colState, "setup"); setup == nil || setup["steps"] == nil {
		t.Fatal("the first poll did not fill state/setup")
	}
}

func TestPollWithoutSecretsIsQuietAndFillsTheChecklist(t *testing.T) {
	newFake(t)
	r := runWith(t, opts{secrets: map[string]string{}}, "poll")
	if !r.ok || !r.quiet {
		t.Fatalf("ok=%t quiet=%t %s", r.ok, r.quiet, r.text)
	}
	wantOK(t, r, "Poll skipped: Twilio is not set up: accountSid has no value on this platform; a person sets the key bound to accountSid in this member's settings on the screen, or the operator runs `yawble secret set <KEY>` for that key; nothing was sent.")
	if r.put(colState, "setup") == nil {
		t.Fatal("state/setup not written")
	}
}

func TestPollPartialAndTotalFailure(t *testing.T) {
	f := newFake(t)
	f.numbersCode = 403
	f.messages["SM1"] = outMsg("SM1", ownCell, "delivered", 0, "")
	r := runWith(t, opts{site: pollSite(testNow.Add(-5*time.Minute), sentDoc("SM1", ownCell, "sent", false))}, "poll")
	if !r.ok {
		t.Fatalf("one failed step failed the poll: %s", r.text)
	}
	wantContains(t, r, "Not done: listing inbound texts: Twilio refused the request (403)")
	progress := false
	for _, p := range r.of("progress") {
		if strings.HasPrefix(p["status"].(string), "poll: listing inbound texts failed") {
			progress = true
		}
	}
	if !progress {
		t.Fatal("no progress line names the failed step")
	}

	delete(f.messages, "SM1") // status fetch now fails too (404)
	r = runWith(t, opts{site: pollSite(testNow.Add(-5*time.Minute), sentDoc("SM1", ownCell, "sent", false))}, "poll")
	if r.ok || !strings.HasPrefix(r.text, "Poll failed: reading delivery status:") || !strings.Contains(r.text, "listing inbound texts:") {
		t.Fatalf("every step failed: %s", r.text)
	}
}

func TestPollPrunesOldMessages(t *testing.T) {
	newFake(t)
	old := messageDoc{Sid: "SMold", Direction: "in", Status: "received", Created: testNow.Add(-31 * 24 * time.Hour).Format(time.RFC3339)}
	r := runWith(t, opts{site: pollSite(testNow.Add(-5*time.Minute), old)}, "poll")
	deleted := false
	for _, d := range r.of("site.delete") {
		if d["id"] == "SMold" && d["collection"] == colMessages {
			deleted = true
		}
	}
	if !deleted {
		t.Fatal("a 31-day-old message was not pruned")
	}
}

func TestReceivedEventFitsThePayloadLimit(t *testing.T) {
	f := newFake(t)
	for i := 0; i < 40; i++ {
		f.inbound = append(f.inbound, inMsg("SM"+strings.Repeat("x", 30)+string(rune('A'+i)), consented, strings.Repeat("long text ", 20), testNow.Add(-time.Duration(i+1)*time.Second)))
	}
	r := runWith(t, opts{site: pollSite(testNow.Add(-5 * time.Minute))}, "poll")
	rec := r.published("received")
	if rec == nil || rec["count"].(float64)+rec["more"].(float64) != 40 || rec["more"].(float64) == 0 {
		t.Fatalf("count/more: %v %v", rec["count"], rec["more"])
	}
	for _, p := range r.of("publish") {
		if len(mustJSON(p["payload"])) > 4000 {
			t.Fatal("a payload is over the Host's 4,000-character limit")
		}
	}
}
