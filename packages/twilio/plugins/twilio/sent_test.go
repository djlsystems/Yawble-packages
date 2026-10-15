package main

import (
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"
)

// sentOf is every sent event a run published, in order.
func sentOf(r result) []map[string]any {
	var out []map[string]any
	for _, rec := range r.of("publish") {
		if rec["type"] == "sent" {
			out = append(out, rec["payload"].(map[string]any))
		}
	}
	return out
}

// wantSent checks a sent event carries exactly the declared fields, with these values, and never
// the text.
func wantSent(t *testing.T, ev map[string]any, to, sid string, autoReply bool, texts ...string) {
	t.Helper()
	keys := slices.Sorted(maps.Keys(ev))
	if !slices.Equal(keys, []string{"autoReply", "dryRun", "number", "sid", "to"}) {
		t.Fatalf("sent carries %v", keys)
	}
	if ev["number"] != tollFree || ev["to"] != to || ev["sid"] != sid || ev["autoReply"] != autoReply || ev["dryRun"] != false {
		t.Fatalf("sent = %v, want to %s sid %s autoReply %t", ev, to, sid, autoReply)
	}
	raw := string(mustJSON(ev))
	for _, text := range texts {
		if strings.Contains(raw, text) {
			t.Fatalf("sent carries the text %q: %s", text, raw)
		}
	}
}

// numberedPosts makes each POST answer with its own SID: SM…01, SM…02, …
func numberedPosts(f *fake) {
	n := 0
	f.post = func(form url.Values) (int, any) {
		n++
		return 201, map[string]any{"sid": fmt.Sprintf("SM%032d", n), "status": "queued", "from": form.Get("From"), "to": form.Get("To"), "num_segments": "1"}
	}
}

func TestASentTextPublishesOneSentEventWithoutItsText(t *testing.T) {
	f := newFake(t)
	r := run1(t, cfg("mode", "send", "messagePrefix", "Acme: ", "optOutFooter", "every"), sms("(555) 123-4567", "Your table is ready"))
	if !r.ok || len(f.posts()) != 1 {
		t.Fatalf("want one text sent: %s", r.text)
	}
	sent := sentOf(r)
	if len(sent) != 1 {
		t.Fatalf("want one sent event, got %d", len(sent))
	}
	wantSent(t, sent[0], ownCell, "SM0000000000000000000000000000new1", false, "table", "Acme", "Reply STOP")
}

func TestEachAutoReplySentPublishesItsOwnSentEvent(t *testing.T) {
	f := newFake(t)
	numberedPosts(f)
	second, stranger := "+15550004444", "+15550001111"
	f.inbound = []message{
		inMsg("SMa", ownCell, "are you open?", testNow.Add(-3*time.Minute)),
		inMsg("SMb", second, "what time today?", testNow.Add(-3*time.Minute)),
		inMsg("SMc", stranger, "who is this?", testNow.Add(-2*time.Minute)),
		inMsg("SMd", ownCell, "hello??", testNow.Add(-2*time.Minute)),
	}
	r := runWith(t, opts{cfg: autoCfg("sendAllowlist", []string{ownCell, second}), site: pollSite(testNow.Add(-5 * time.Minute))}, "poll")
	if !r.ok {
		t.Fatal(r.text)
	}
	posts := f.posts()
	sent := sentOf(r)
	// Two texts went out (ownCell once in its window, second once); the stranger was refused.
	if len(posts) != 2 || len(sent) != len(posts) {
		t.Fatalf("want one sent event per text sent: %d post(s), %d sent event(s)", len(posts), len(sent))
	}
	for i, p := range posts {
		wantSent(t, sent[i], p.Form.Get("To"), fmt.Sprintf("SM%032d", i+1), true, replyText, "open", "today")
	}
	if sent[0]["to"] == sent[1]["to"] {
		t.Fatalf("both auto-replies went to %v", sent[0]["to"])
	}
}

func TestADryRunPublishesNoSentEvent(t *testing.T) {
	f := newFake(t)
	r := run1(t, nil, sms(ownCell, "Hello"))
	if !r.ok || len(sentOf(r)) != 0 {
		t.Fatalf("a dry run sms published sent: %v", sentOf(r))
	}

	// A dry-run auto-reply is recorded and sends nothing, so it publishes nothing either.
	f.inbound = []message{inMsg("SMa", ownCell, "are you open?", testNow.Add(-2*time.Minute))}
	r = runWith(t, opts{cfg: autoCfg("mode", "dryRun"), site: pollSite(testNow.Add(-5 * time.Minute))}, "poll")
	if !r.ok || r.put(colMessages, "dryrun-SMa") == nil {
		t.Fatalf("the dry-run auto-reply was not recorded: %s", r.text)
	}
	if len(f.posts()) != 0 || len(sentOf(r)) != 0 {
		t.Fatalf("a dry run sent %d text(s) and published %d sent event(s)", len(f.posts()), len(sentOf(r)))
	}
}

func TestATextTwilioRefusesPublishesNoSentEvent(t *testing.T) {
	f := newFake(t)
	f.post = func(url.Values) (int, any) {
		return 400, map[string]any{"code": 21211, "message": "The 'To' number is not a valid phone number.", "status": 400}
	}
	r := run1(t, send(), sms(ownCell, "x"))
	if r.ok || len(sentOf(r)) != 0 {
		t.Fatalf("a refused text published sent: ok=%t %v", r.ok, sentOf(r))
	}
}
