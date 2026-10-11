package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

const (
	ownerCell = "+15550100011"
	otherCell = "+15550100022"
)

func ownerCfg(kv ...any) map[string]any {
	c := cfg("ownerCommands", true, "sendAllowlist", []string{ownerCell})
	for i := 0; i < len(kv); i += 2 {
		c[kv[i].(string)] = kv[i+1]
	}
	return c
}

func commandsOf(r result) []map[string]any {
	var out []map[string]any
	for _, rec := range r.of("publish") {
		if rec["type"] == "command" {
			out = append(out, rec["payload"].(map[string]any))
		}
	}
	return out
}

func TestOwnerCommandPublishesTheKeywordOnly(t *testing.T) {
	f := newFake(t)
	at := testNow.Add(-2 * time.Minute)
	f.inbound = []message{inMsg("SMcmd", ownerCell, "  Summary? ", at)}
	r := runWith(t, opts{cfg: ownerCfg(), site: pollSite(testNow.Add(-5 * time.Minute))}, "poll")
	if !r.ok {
		t.Fatal(r.text)
	}
	got := commandsOf(r)
	if len(got) != 1 {
		t.Fatalf("want one command event, got %v", got)
	}
	want := map[string]any{"command": "summary", "from": ownerCell, "at": at.Format(time.RFC3339), "messageSid": "SMcmd", "dryRun": false}
	if len(got[0]) != len(want) {
		t.Fatalf("command event: want %v, got %v", want, got[0])
	}
	for k, v := range want {
		if got[0][k] != v {
			t.Fatalf("command event %s: want %v, got %v", k, v, got[0][k])
		}
	}
	if r.published("received") != nil {
		t.Fatal("a command was also published as a received text")
	}
	// The event wakes its trigger; the poll itself wakes nobody.
	if !r.quiet {
		t.Fatalf("a poll that found only a command is not quiet: %q", r.text)
	}
	wantOK(t, r, "Poll: 0 status change(s) (0 delivered, 0 failed), 1 new inbound text(s), 0 opt-out(s), 0 opt-in(s). 1 owner command(s).")
}

func TestOwnerCommandReportedInThePollText(t *testing.T) {
	f := newFake(t)
	f.inbound = []message{
		inMsg("SMcmd", ownerCell, "STATUS!", testNow.Add(-3*time.Minute)),
		inMsg("SMtxt", otherCell, "hello", testNow.Add(-2*time.Minute)),
	}
	r := runWith(t, opts{cfg: ownerCfg(), site: pollSite(testNow.Add(-5 * time.Minute))}, "poll")
	wantOK(t, r, "Poll: 0 status change(s) (0 delivered, 0 failed), 2 new inbound text(s), 0 opt-out(s), 0 opt-in(s). 1 owner command(s).")
	if r.quiet {
		t.Fatal("a poll with an ordinary text is quiet")
	}
	if c := commandsOf(r); len(c) != 1 || c[0]["command"] != "status" {
		t.Fatalf("command events: %v", c)
	}
	rec := r.published("received")
	if rec == nil || rec["count"].(float64) != 1 || rec["messages"].([]any)[0].(map[string]any)["sid"] != "SMtxt" {
		t.Fatalf("received: %v", rec)
	}
}

func TestOwnerCommandOtherSenderOrMoreWordsIsAnOrdinaryText(t *testing.T) {
	f := newFake(t)
	f.inbound = []message{
		inMsg("SMa", otherCell, "Summary?", testNow.Add(-3*time.Minute)),
		inMsg("SMb", ownerCell, "summary please", testNow.Add(-2*time.Minute)),
	}
	r := runWith(t, opts{cfg: ownerCfg(), site: pollSite(testNow.Add(-5 * time.Minute))}, "poll")
	if !r.ok {
		t.Fatal(r.text)
	}
	if c := commandsOf(r); len(c) != 0 {
		t.Fatalf("want no command, got %v", c)
	}
	rec := r.published("received")
	if rec == nil || rec["count"].(float64) != 2 {
		t.Fatalf("want both as received texts, got %v", rec)
	}
	if !strings.Contains(rec["messages"].([]any)[1].(map[string]any)["snippet"].(string), "summary please") {
		t.Fatalf("the received text changed: %v", rec)
	}
	if r.quiet {
		t.Fatal("a poll with received texts is quiet")
	}
}

func TestOwnerCommandNeverStopStartOrHelp(t *testing.T) {
	f := newFake(t)
	f.inbound = []message{
		inMsg("SMa", ownerCell, "Help!", testNow.Add(-4*time.Minute)),
		inMsg("SMb", ownerCell, "START", testNow.Add(-3*time.Minute)),
		inMsg("SMc", ownerCell, "STOP", testNow.Add(-2*time.Minute)),
	}
	c := ownerCfg("ownerKeywords", "stop, start, help, summary")
	r := runWith(t, opts{cfg: c, site: pollSite(testNow.Add(-5 * time.Minute))}, "poll")
	if !r.ok {
		t.Fatal(r.text)
	}
	if got := commandsOf(r); len(got) != 0 {
		t.Fatalf("a STOP, START or HELP text became a command: %v", got)
	}
	out := r.published("optout")
	if out == nil || out["numbers"].([]any)[0].(map[string]any)["number"] != ownerCell {
		t.Fatalf("STOP did not opt out: %v", out)
	}
	if rec := r.published("received"); rec == nil || rec["count"].(float64) != 2 {
		t.Fatalf("Help! and START are ordinary texts here: %v", rec)
	}
}

func TestOwnerCommandsOffChangesNothing(t *testing.T) {
	f := newFake(t)
	f.inbound = []message{inMsg("SMa", ownerCell, "summary", testNow.Add(-2*time.Minute))}
	r := runWith(t, opts{cfg: cfg("sendAllowlist", []string{ownerCell}, "ownerNumbers", []string{ownerCell}), site: pollSite(testNow.Add(-5 * time.Minute))}, "poll")
	wantOK(t, r, "Poll: 0 status change(s) (0 delivered, 0 failed), 1 new inbound text(s), 0 opt-out(s), 0 opt-in(s).")
	if c := commandsOf(r); len(c) != 0 {
		t.Fatalf("ownerCommands off published %v", c)
	}
	if rec := r.published("received"); rec == nil || rec["count"].(float64) != 1 {
		t.Fatalf("received: %v", rec)
	}
	if r.quiet {
		t.Fatal("a received text was quiet")
	}
	r = runWith(t, opts{cfg: cfg("sendAllowlist", []string{ownerCell}), site: pollSite(testNow.Add(-5 * time.Minute))}, "check")
	if strings.Contains(r.text, "owner commands") {
		t.Fatalf("check names owner commands while they are off:\n%s", r.text)
	}
}

func TestAutoReplyNeverAnswersAnOwnerCommand(t *testing.T) {
	f := newFake(t)
	f.inbound = []message{inMsg("SMa", ownerCell, "updates.", testNow.Add(-2*time.Minute))}
	c := autoCfg("ownerCommands", true, "sendAllowlist", []string{ownerCell})
	r := runWith(t, opts{cfg: c, site: pollSite(testNow.Add(-5 * time.Minute))}, "poll")
	if !r.ok {
		t.Fatal(r.text)
	}
	if len(f.posts()) != 0 {
		t.Fatalf("auto-reply answered a command: %v", f.posts())
	}
	if got := commandsOf(r); len(got) != 1 || got[0]["command"] != "updates" {
		t.Fatalf("command events: %v", got)
	}
	wantContains(t, r, "Auto-replied to 0.", "1 owner command(s).")

	// The same number's ordinary text is still answered.
	f.inbound = append(f.inbound, inMsg("SMb", ownerCell, "updates on the roof job?", testNow.Add(-time.Minute)))
	r = runWith(t, opts{cfg: c, site: nextSite(r)}, "poll")
	if len(f.posts()) != 1 || f.posts()[0].Form.Get("To") != ownerCell {
		t.Fatalf("want one auto-reply to the ordinary text, got %v", f.posts())
	}
	if got := commandsOf(r); len(got) != 0 {
		t.Fatalf("the ordinary text became a command: %v", got)
	}
}

func TestOwnerNumbersFallBackToSendAllowlist(t *testing.T) {
	f := newFake(t)
	f.inbound = []message{inMsg("SMa", ownerCell, "summary", testNow.Add(-2*time.Minute))}
	r := runWith(t, opts{cfg: ownerCfg("ownerNumbers", []string{}), site: pollSite(testNow.Add(-5 * time.Minute))}, "poll")
	if got := commandsOf(r); len(got) != 1 || got[0]["from"] != ownerCell {
		t.Fatalf("an empty ownerNumbers did not fall back to sendAllowlist: %v", got)
	}

	// Once ownerNumbers is set, sendAllowlist alone does not make a number an owner.
	r = runWith(t, opts{cfg: ownerCfg("ownerNumbers", []string{otherCell}), site: pollSite(testNow.Add(-5 * time.Minute))}, "poll")
	if got := commandsOf(r); len(got) != 0 {
		t.Fatalf("a number only on sendAllowlist sent a command: %v", got)
	}
	if rec := r.published("received"); rec == nil {
		t.Fatal("the text is not a received text")
	}
}

func TestOwnerCommandsCheckStep(t *testing.T) {
	newFake(t)
	r := runWith(t, opts{cfg: ownerCfg(), site: pollSite(testNow.Add(-5 * time.Minute))}, "check")
	wantContains(t, r, "Done: owner commands — on: a text from "+ownerCell+" that is only summary, status, updates publishes plugin.twilio.command")
	r = runWith(t, opts{cfg: ownerCfg("ownerKeywords", ""), site: pollSite(testNow.Add(-5 * time.Minute))}, "check")
	wantContains(t, r, "Next: owner commands — ownerCommands is on but ownerKeywords is empty")
}

func TestOwnerKeywordsValidation(t *testing.T) {
	parse := func(kv ...any) (settings, string) {
		raw := map[string]json.RawMessage{}
		for i := 0; i < len(kv); i += 2 {
			raw[kv[i].(string)] = mustJSON(kv[i+1])
		}
		return parseSettings(raw)
	}

	s, problem := parse()
	if problem != "" || strings.Join(s.OwnerKeywords, "|") != "summary|status|updates" || s.OwnerCommands {
		t.Fatalf("defaults: %v %q", s.OwnerKeywords, problem)
	}
	s, problem = parse("ownerKeywords", " Summary , Job 2,status ")
	if problem != "" || strings.Join(s.OwnerKeywords, "|") != "summary|job 2|status" {
		t.Fatalf("good keywords: %v %q", s.OwnerKeywords, problem)
	}
	s, problem = parse("ownerKeywords", "")
	if problem != "" || len(s.OwnerKeywords) != 0 {
		t.Fatalf("empty keywords: %v %q", s.OwnerKeywords, problem)
	}

	many := strings.TrimSuffix(strings.Repeat("k, ", 21), ", ")
	for value, want := range map[string]string{
		"summary!":                      "`ownerKeywords` holds \"summary!\"; each keyword is 1 to 32 letters, digits and spaces, separated by commas",
		"summary,,status":               "`ownerKeywords` holds \"\"; each keyword is 1 to 32 letters, digits and spaces",
		strings.Repeat("a", 33):         "`ownerKeywords` holds \"" + strings.Repeat("a", 33) + "\"",
		"status, sum-mary":              "`ownerKeywords` holds \"sum-mary\"",
		many:                            "`ownerKeywords` holds more than 20 keywords",
		strings.Repeat("a", 32):         "",
		strings.Repeat("k, ", 19) + "k": "",
	} {
		_, problem := parse("ownerKeywords", value)
		if want == "" && problem != "" || want != "" && !strings.Contains(problem, want) {
			t.Fatalf("ownerKeywords %q: want %q, got %q", value, want, problem)
		}
	}

	_, problem = parse("ownerNumbers", []string{"not a number"})
	if !strings.Contains(problem, "`ownerNumbers` holds \"not a number\", which is not a phone number") {
		t.Fatalf("ownerNumbers: %q", problem)
	}
	s, problem = parse("ownerNumbers", []string{"555 010 0011"})
	if problem != "" || len(s.OwnerNumbers) != 1 || s.OwnerNumbers[0] != ownerCell {
		t.Fatalf("ownerNumbers normalised: %v %q", s.OwnerNumbers, problem)
	}

	// A bad keyword stops the run with the settings sentence.
	newFake(t)
	r := run1(t, ownerCfg("ownerKeywords", "summary?"), "poll")
	if r.ok || !strings.Contains(r.text, "This member's settings cannot be used: `ownerKeywords` holds \"summary?\"") {
		t.Fatalf("a bad keyword was accepted: %q", r.text)
	}
}
