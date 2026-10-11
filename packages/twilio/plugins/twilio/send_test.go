package main

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"
)

const consented = "+15559876543"

func recip(number, status, by string, extra ...any) docIn {
	doc := map[string]any{
		"number": number, "name": "Pat", "status": status,
		"consent": map[string]any{"method": "web-form", "capturedAt": "2026-09-30", "purpose": "order updates", "evidence": "form 12", "capturedBy": "person@example.com"},
	}
	for i := 0; i < len(extra); i += 2 {
		doc[extra[i].(string)] = extra[i+1]
	}
	return docIn{ID: strings.TrimPrefix(number, "+"), Doc: doc, UpdatedBy: by}
}

// consentedSetup is a registered (toll-free verified) Full account with one active consented entry.
func consentedSetup(t *testing.T) (*fake, opts) {
	f := newFake(t)
	f.tollfree = []tollFreeVerification{{Sid: "HH1", Status: "TWILIO_APPROVED"}}
	return f, opts{
		cfg:  cfg("mode", "send", "allowConsentedRecipients", true),
		site: &siteState{recipients: []docIn{recip(consented, "active", "person@example.com")}},
	}
}

// ---- §12 item 8: the dry run sends nothing -------------------------------------------------------

func TestDryRunMakesNoPostAndPrintsTheDryRunLine(t *testing.T) {
	f := newFake(t)
	r := run1(t, nil, sms(ownCell, "Hello from the plugin"))
	wantOK(t, r, "Dry run: would send SMS to +15551234567 from +18445550100, 1 segment(s), GSM-7; nothing was sent.")
	if n := len(f.posts()); n != 0 {
		t.Fatalf("a dry run POSTed Messages.json %d time(s)", n)
	}
	if len(r.of("publish")) != 0 {
		t.Fatal("a dry run published an event")
	}
}

func TestDryRunMMSLineAndNoPost(t *testing.T) {
	f := newFake(t)
	f.media["a.jpg"] = media{200, "image/jpeg", 100000}
	f.media["b.pdf"] = media{200, "application/pdf", 20000}
	r := run1(t, nil, "mms\nTo: "+ownCell+"\nMedia: "+f.mediaURL("a.jpg")+"\nMedia: "+f.mediaURL("b.pdf")+"\n\nFlyer")
	wantOK(t, r, "Dry run: would send MMS with 2 media to +15551234567 from +18445550100, 1 segment(s), GSM-7; nothing was sent.")
	if len(f.posts()) != 0 {
		t.Fatal("a dry run MMS POSTed")
	}
}

func TestDryRunDoesNotCountTowardsCaps(t *testing.T) {
	newFake(t)
	r := runWith(t, opts{cfg: cfg("dailySendCap", 1)}, sms(ownCell, "one"), sms(ownCell, "two"))
	if !r.ok {
		t.Fatalf("dry runs were capped: %s", r.text)
	}
	if doc := r.put(colState, "poll"); doc != nil && doc["sentToday"].(float64) != 0 {
		t.Fatalf("dry runs counted: %v", doc)
	}
}

// ---- sending ----------------------------------------------------------------------------------

func TestSendPostsTheRequestShapeAndRecordsIt(t *testing.T) {
	f := newFake(t)
	r := run1(t, send(), sms("(555) 123-4567", "Hi"))
	wantOK(t, r, "Sent SM0000000000000000000000000000new1 to +15551234567, status queued, 1 segment(s), GSM-7.")
	posts := f.posts()
	if len(posts) != 1 {
		t.Fatalf("want 1 POST, got %d", len(posts))
	}
	p := posts[0]
	if p.User != testKeySid || p.Pass != testSecret {
		t.Fatal("the POST was not signed with the API key")
	}
	want := map[string]string{"To": ownCell, "From": tollFree, "Body": "Hi", "ValidityPeriod": "14400"}
	for k, v := range want {
		if p.Form.Get(k) != v {
			t.Fatalf("form %s = %q, want %q (%v)", k, p.Form.Get(k), v, p.Form)
		}
	}
	if p.Form.Has("MessagingServiceSid") {
		t.Fatal("From and MessagingServiceSid both sent")
	}
	doc := r.put(colMessages, "SM0000000000000000000000000000new1")
	if doc == nil || doc["status"] != "queued" || doc["test"] != true || doc["snippet"] != "Hi" {
		t.Fatalf("message not recorded as the setup test message: %v", doc)
	}
	st := r.put(colState, "poll")
	if st["sentToday"].(float64) != 1 || st["sentTodayByNumber"].(map[string]any)[ownCell].(float64) != 1 {
		t.Fatalf("the send was not counted: %v", st)
	}
}

func TestSendThroughMessagingService(t *testing.T) {
	f := newFake(t)
	mg := "MG" + strings.Repeat("a", 32)
	r := run1(t, cfg("mode", "send", "messagingServiceSid", mg), sms(ownCell, "Hi"))
	if !r.ok {
		t.Fatal(r.text)
	}
	p := f.posts()[0]
	if p.Form.Get("MessagingServiceSid") != mg || p.Form.Has("From") {
		t.Fatalf("want MessagingServiceSid instead of From: %v", p.Form)
	}
}

func TestSendModeWithEmptyAllowlistRefusesAnyCommand(t *testing.T) {
	newFake(t)
	for _, cmd := range []string{"check", "allowlist", sms(ownCell, "x"), "poll"} {
		r := run1(t, cfg("mode", "send", "sendAllowlist", []string{}), cmd)
		wantFail(t, r, "Refused: mode is send but sendAllowlist is empty; a person adds at least their own cell to sendAllowlist in this member's settings. Nothing was sent.")
	}
}

func TestMissingSecretsAreNamedByKey(t *testing.T) {
	newFake(t)
	for field, key := range map[string]string{"accountSid": "TWILIO_ACCOUNT_SID", "apiKeySid": "TWILIO_API_KEY_SID", "apiKeySecret": "TWILIO_API_KEY_SECRET"} {
		s := goodSecrets()
		delete(s, field)
		r := runWith(t, opts{secrets: s}, sms(ownCell, "x"))
		wantFail(t, r, "Twilio is not set up: "+key+" is not set on this platform; the operator sets it with the secret command; nothing was sent.")
	}
}

func TestSenderFallbacks(t *testing.T) {
	f := newFake(t)
	f.numbers = nil
	wantFail(t, run1(t, nil, sms(ownCell, "x")), "Twilio has no sender: the account has no SMS-capable number; buy one in the Twilio Console. Nothing was sent.")

	f.numbers = []number{num(tollFree, "PN1", true, true), num(localNumber, "PN2", true, false), num("+16175550199", "PN3", false, false)}
	wantFail(t, run1(t, nil, sms(ownCell, "x")), "Twilio has 2 numbers and fromNumber is empty: a person chooses one (+18445550100, +16175550100) in this member's settings; nothing was sent.")

	r := run1(t, cfg("fromNumber", "617 555 0100"), sms(ownCell, "x"))
	wantOK(t, r, "Dry run: would send SMS to +15551234567 from +16175550100, 1 segment(s), GSM-7; nothing was sent.")
}

func TestRecipientRefusals(t *testing.T) {
	newFake(t)
	wantFail(t, run1(t, nil, sms("12345", "x")), "Refused: 12345 is not a phone number in +country format (E.164); nothing was sent.")
	wantFail(t, run1(t, nil, sms("+447700900123", "x")), "Refused: +447700900123 is in GB, which is not on allowedCountries; nothing was sent.")
	wantFail(t, run1(t, nil, sms("+14165550123", "x")), "Refused: +14165550123 is in CA, which is not on allowedCountries; nothing was sent.")
	wantFail(t, run1(t, nil, sms("+15550001111", "x")), "Refused: +15550001111 is not on sendAllowlist or the active consented list; nothing was sent.")
	r := run1(t, cfg("allowedCountries", []string{"US", "CA"}, "sendAllowlist", []string{"+14165550123"}), sms("+14165550123", "x"))
	if !r.ok {
		t.Fatalf("CA allowed but refused: %s", r.text)
	}
}

func TestConsentedListIsUsedOnlyInSendModeWhenOn(t *testing.T) {
	_, o := consentedSetup(t)
	r := runWith(t, o, sms(consented, "Your order shipped"))
	wantOK(t, r, "Sent SM0000000000000000000000000000new1 to +15559876543, status queued, 1 segment(s), GSM-7.")

	o.cfg = cfg("allowConsentedRecipients", true) // dry run: the consented list is not used
	wantFail(t, runWith(t, o, sms(consented, "x")), "Refused: +15559876543 is not on sendAllowlist or the active consented list; nothing was sent.")
}

func TestConsentedEntryActivatedByAnAgentIsNotHonoured(t *testing.T) {
	_, o := consentedSetup(t)
	o.site.recipients = []docIn{recip(consented, "active", "TwilioTeam/Texting Assistant")}
	wantFail(t, runWith(t, o, sms(consented, "x")), "Refused: +15559876543 is not on sendAllowlist or the active consented list; nothing was sent.")
	o.site.recipients = []docIn{recip(consented, "pending", "person@example.com")}
	wantFail(t, runWith(t, o, sms(consented, "x")), "Refused: +15559876543 is not on sendAllowlist or the active consented list; nothing was sent.")
}

func TestMaxConsentedRecipientsKeepsTheOldestActivated(t *testing.T) {
	_, o := consentedSetup(t)
	o.cfg["maxConsentedRecipients"] = 1
	o.site.recipients = []docIn{
		recip(consented, "active", "person@example.com", "activatedAt", "2026-10-02T00:00:00Z"),
		recip("+15559876500", "active", "person@example.com", "activatedAt", "2026-10-01T00:00:00Z"),
	}
	wantFail(t, runWith(t, o, sms(consented, "x")), "Refused: +15559876543 is not on sendAllowlist or the active consented list; nothing was sent.")
	if r := runWith(t, o, sms("+15559876500", "x")); !r.ok {
		t.Fatal(r.text)
	}
}

func TestConsentedRefusedOnTrialAndUnregistered(t *testing.T) {
	f, o := consentedSetup(t)
	f.acct.Type = "Trial"
	wantFail(t, runWith(t, o, sms(consented, "x")), "Refused: allowConsentedRecipients is on but the account is a Trial account; only sendAllowlist numbers can be sent to.")

	f.acct.Type = "Full"
	f.tollfree = []tollFreeVerification{{Sid: "HH1", Status: "IN_REVIEW"}}
	wantFail(t, runWith(t, o, sms(consented, "x")), "Refused: allowConsentedRecipients is on but +15559876543's sender is not registered (IN_REVIEW); only sendAllowlist numbers can be sent to.")

	// The person's own cell is unaffected.
	if r := runWith(t, o, sms(ownCell, "x")); !r.ok {
		t.Fatal(r.text)
	}
}

func TestOptedOutRefusals(t *testing.T) {
	_, o := consentedSetup(t)
	o.site.recipients = []docIn{recip(consented, "opted-out", "TwilioTeam/twilio", "optOutKeyword", "STOP", "optedOutAt", "2026-10-05T10:00:00Z")}
	o.cfg["sendAllowlist"] = []string{ownCell, consented}
	wantFail(t, runWith(t, o, sms(consented, "x")), "Refused: +15559876543 opted out (STOP on 2026-10-05); nothing was sent.")

	o.site = &siteState{state: []docIn{{ID: "poll", Doc: map[string]any{"optedOut": map[string]any{ownCell: map[string]any{"keyword": "by request", "at": "2026-10-06T10:00:00Z"}}}}}}
	wantFail(t, runWith(t, o, sms(ownCell, "x")), "Refused: +15551234567 opted out (by request on 2026-10-06); nothing was sent.")
}

func TestThereAreNoQuietHours(t *testing.T) {
	_, o := consentedSetup(t)
	o.now = time.Date(2026, 10, 10, 1, 30, 0, 0, time.UTC) // Friday 21:30 in New York
	if r := runWith(t, o, sms(consented, "x")); !r.ok {
		t.Fatalf("a text to a consented recipient at night was refused: %s", r.text)
	}

	// A Texas number on a Sunday morning, and a stored 1.0.0 window, change nothing.
	texan := "+12145550123"
	o.site.recipients = []docIn{recip(texan, "active", "person@example.com", "timezone", "America/Chicago")}
	o.cfg["quietHoursStart"] = "00:00"
	o.cfg["quietHoursEnd"] = "23:59"
	o.now = time.Date(2026, 10, 11, 15, 30, 0, 0, time.UTC) // Sunday 10:30 in Chicago
	if r := runWith(t, o, sms(texan, "x")); !r.ok {
		t.Fatalf("a Sunday-morning text to a Texas number was refused: %s", r.text)
	}
}

func TestDailyCaps(t *testing.T) {
	newFake(t)
	r := runWith(t, opts{cfg: cfg("mode", "send", "dailySendCap", 2, "sendAllowlist", []string{ownCell, consented})},
		sms(ownCell, "1"), sms(consented, "2"), sms(ownCell, "3"))
	if r.ok || !strings.Contains(r.text, "3. Refused: today's sends reached dailySendCap (2); nothing was sent.") {
		t.Fatalf("daily cap not applied: %s", r.text)
	}

	r = runWith(t, opts{cfg: cfg("mode", "send", "perRecipientDailyCap", 1)}, sms(ownCell, "1"), sms(ownCell, "2"))
	if !strings.Contains(r.text, "2. Refused: today's sends to +15551234567 reached perRecipientDailyCap (1); nothing was sent.") {
		t.Fatalf("per-recipient cap not applied: %s", r.text)
	}

	// Yesterday's counts do not carry over.
	site := &siteState{state: []docIn{{ID: "poll", Doc: map[string]any{"dayKey": "2026-10-08", "sentToday": 500, "sentTodayByNumber": map[string]any{ownCell: 50}}}}}
	if r := runWith(t, opts{cfg: send(), site: site}, sms(ownCell, "x")); !r.ok {
		t.Fatal(r.text)
	}
	site.state[0].Doc.(map[string]any)["dayKey"] = "2026-10-09"
	wantFail(t, runWith(t, opts{cfg: send(), site: site}, sms(ownCell, "x")), "Refused: today's sends reached dailySendCap (500); nothing was sent.")
}

// ---- what every text carries for the campaign -----------------------------------------------------

func TestEveryTextCarriesTheOptOutLineByDefault(t *testing.T) {
	s, problems := parseSettings(map[string]json.RawMessage{})
	if problems != "" || s.OptOutFooter != "every" || s.MessagePrefix != "" {
		t.Fatalf("defaults: footer %q prefix %q problems %q", s.OptOutFooter, s.MessagePrefix, problems)
	}
}

func TestThePrefixAndTheOptOutLineGoOnEveryTextToEveryRecipient(t *testing.T) {
	f, o := consentedSetup(t)
	o.cfg["messagePrefix"] = "DJL Systems: "
	o.cfg["optOutFooter"] = "every"
	r := runWith(t, o,
		sms(consented, "Job nightly-build is blocked and needs a person: disk full"),
		sms(consented, "Again!"),
		sms(ownCell, "Your sign-in code is 123456."),
	)
	if !r.ok {
		t.Fatal(r.text)
	}
	posts := f.posts()
	want := []string{
		"DJL Systems: Job nightly-build is blocked and needs a person: disk full. Reply STOP to opt out.",
		"DJL Systems: Again! Reply STOP to opt out.",
		"DJL Systems: Your sign-in code is 123456. Reply STOP to opt out.",
	}
	for i, w := range want {
		if got := posts[i].Form.Get("Body"); got != w {
			t.Fatalf("text %d: got %q, want %q", i+1, got, w)
		}
	}
	if !strings.Contains(r.text, "As sent: "+want[0]) {
		t.Fatalf("the reply does not show the text as sent: %s", r.text)
	}
}

func TestATextThatAlreadyCarriesTheNameOrTheOptOutLineIsNotGivenItTwice(t *testing.T) {
	f, o := consentedSetup(t)
	o.cfg["messagePrefix"] = "DJL Systems: "
	o.cfg["optOutFooter"] = "every"
	sample := "DJL Systems alert: billing is down as of 9:05. Investigating now; next update by 9:30. Reply STOP to opt out."
	r := runWith(t, o, sms(consented, sample), sms(consented, "djl systems: lower case"))
	if !r.ok {
		t.Fatal(r.text)
	}
	posts := f.posts()
	if got := posts[0].Form.Get("Body"); got != sample {
		t.Fatalf("a registered sample was changed: %q", got)
	}
	if strings.Contains(r.text, "As sent: "+sample) {
		t.Fatalf("an unchanged text was shown as changed: %s", r.text)
	}
	if got := posts[1].Form.Get("Body"); got != "djl systems: lower case. Reply STOP to opt out." {
		t.Fatalf("second: %q", got)
	}
}

func TestTheOptOutLineOnTheFirstTextOnlyOrNotAtAll(t *testing.T) {
	f, o := consentedSetup(t)
	o.cfg["optOutFooter"] = "first"
	if r := runWith(t, o, sms(consented, "Hello"), sms(consented, "Again"), sms(ownCell, "Me")); !r.ok {
		t.Fatal(r.text)
	}
	posts := f.posts()
	if posts[0].Form.Get("Body") != "Hello. Reply STOP to opt out." || posts[1].Form.Get("Body") != "Again" || posts[2].Form.Get("Body") != "Me. Reply STOP to opt out." {
		t.Fatalf("first only: %q / %q / %q", posts[0].Form.Get("Body"), posts[1].Form.Get("Body"), posts[2].Form.Get("Body"))
	}

	f, o = consentedSetup(t)
	o.cfg["optOutFooter"] = "off"
	if r := runWith(t, o, sms(consented, "Hello")); !r.ok {
		t.Fatal(r.text)
	}
	if got := f.posts()[0].Form.Get("Body"); got != "Hello" {
		t.Fatalf("off: %q", got)
	}
}

func TestTheWordingCountsTowardsLengthAndSegments(t *testing.T) {
	newFake(t)
	wording := cfg("messagePrefix", "DJL Systems: ", "optOutFooter", "every")
	// 13 + 135 + ". " + 22 = 172: two segments, though the text alone is one.
	r := run1(t, wording, sms(ownCell, strings.Repeat("a", 135)))
	if !strings.HasPrefix(r.text, "Dry run: would send SMS to +15551234567 from +18445550100, 2 segment(s), GSM-7; nothing was sent.\nAs sent: DJL Systems: aaa") {
		t.Fatalf("segments after the wording: %s", r.text)
	}
	wantFail(t, run1(t, wording, sms(ownCell, strings.Repeat("a", 1580))), "Refused: the body is 1617 characters, over 1600; nothing was sent.")
}

func TestThePrefixAndPagesAreChecked(t *testing.T) {
	for _, c := range []struct {
		name  string
		value any
		want  string
	}{
		{"messagePrefix", strings.Repeat("x", 41), "`messagePrefix` must be one line of at most 40 characters"},
		{"messagePrefix", "Acme\nCo: ", "`messagePrefix` must be one line of at most 40 characters"},
		{"optOutFooter", "sometimes", "`optOutFooter` must be every, first or off"},
		{"privacyUrl", "http://example.com/privacy", "`privacyUrl` must be an https address"},
		{"termsUrl", "example.com/terms", "`termsUrl` must be an https address"},
	} {
		r := run1(t, cfg(c.name, c.value), "check")
		if r.ok || !strings.Contains(r.text, c.want) {
			t.Fatalf("%s %v: %s", c.name, c.value, r.text)
		}
	}
}

func TestBodyLengthSegmentsAndEncoding(t *testing.T) {
	newFake(t)
	wantFail(t, run1(t, nil, sms(ownCell, strings.Repeat("a", 1601))), "Refused: the body is 1601 characters, over 1600; nothing was sent.")
	wantOK(t, run1(t, nil, sms(ownCell, strings.Repeat("a", 161))), "Dry run: would send SMS to +15551234567 from +18445550100, 2 segment(s), GSM-7; nothing was sent.")
	wantOK(t, run1(t, nil, sms(ownCell, "Hi 😀")), "Dry run: would send SMS to +15551234567 from +18445550100, 1 segment(s), UCS-2; nothing was sent.")

	cases := []struct {
		body     string
		tollFree bool
		n        int
		enc      string
	}{
		{strings.Repeat("a", 160), false, 1, "GSM-7"},
		{strings.Repeat("a", 306), false, 2, "GSM-7"},
		{strings.Repeat("a", 306), true, 3, "GSM-7"}, // 152 a segment from toll-free
		{strings.Repeat("{", 80), false, 1, "GSM-7"}, // extension characters count 2
		{strings.Repeat("é", 70), false, 1, "GSM-7"},
		{strings.Repeat("ж", 70), false, 1, "UCS-2"},
		{strings.Repeat("ж", 71), false, 2, "UCS-2"},
	}
	for _, c := range cases {
		if n, enc := segments(c.body, c.tollFree); n != c.n || enc != c.enc {
			t.Errorf("segments(%d chars, tollFree=%t) = %d %s, want %d %s", len([]rune(c.body)), c.tollFree, n, enc, c.n, c.enc)
		}
	}
}

func TestCommandShapeRefusals(t *testing.T) {
	newFake(t)
	wantFail(t, run1(t, nil, "sms\nTo: "+ownCell+"\nMedia: https://x.example/a.jpg\n\nhi"), "Refused: sms takes no Media: lines; use mms for media. Nothing was sent.")
	wantFail(t, run1(t, nil, "sms\nTo: "+ownCell+", "+consented+"\n\nhi"), "Refused: one recipient per command; send to each number with its own command. Nothing was sent.")
	wantFail(t, run1(t, nil, "sms\nCc: "+ownCell+"\n\nhi"), `Refused: sms takes only To: lines, then a blank line and the body; "Cc: +15551234567" is neither. Nothing was sent.`)
	wantFail(t, run1(t, nil, "mms\nTo: "+ownCell+"\n\nhi"), "Refused: mms takes 1 to 10 Media: lines, and this has 0; nothing was sent.")
}

// ---- MMS --------------------------------------------------------------------------------------

func TestMMSRefusals(t *testing.T) {
	f := newFake(t)
	host := "127.0.0.1"
	mms := func(urls ...string) string {
		s := "mms\nTo: " + ownCell
		for _, u := range urls {
			s += "\nMedia: " + u
		}
		return s + "\n\nLook"
	}
	f.media["ok.png"] = media{200, "image/png", 1000}
	f.media["big1.jpg"] = media{200, "image/jpeg", 3 << 20}
	f.media["big2.jpg"] = media{200, "image/jpeg", 3 << 20}
	f.media["page.html"] = media{200, "text/html; charset=utf-8", 1000}
	f.media["gone.png"] = media{404, "text/plain", 0}
	f.media["huge.mp4"] = media{200, "video/mp4", 700 << 10}
	f.media["nolen.png"] = media{200, "image/png", -1}

	wantFail(t, run1(t, send(), mms(f.mediaURL("ok.png"))), "Refused: MMS sending is off because mediaUrlHosts is empty; a person adds the hosts media may come from. Nothing was sent.")

	on := cfg("mode", "send", "mediaUrlHosts", []string{host})
	wantFail(t, run1(t, on, mms("http://127.0.0.1/a.png")), "Refused: http://127.0.0.1/a.png is not https or its host is not on mediaUrlHosts; nothing was sent.")
	wantFail(t, run1(t, on, mms("https://elsewhere.example/a.png")), "Refused: https://elsewhere.example/a.png is not https or its host is not on mediaUrlHosts; nothing was sent.")
	wantFail(t, run1(t, on, mms(f.mediaURL("page.html"))), "Refused: "+f.mediaURL("page.html")+" answered text/html; Twilio accepts jpeg, png, gif, mp4, mp3, pdf and the other types listed in plugin-twilio. Nothing was sent.")
	wantFail(t, run1(t, on, mms(f.mediaURL("gone.png"))), "Refused: "+f.mediaURL("gone.png")+" answered 404; Twilio accepts jpeg, png, gif, mp4, mp3, pdf and the other types listed in plugin-twilio. Nothing was sent.")
	wantFail(t, run1(t, on, mms(f.mediaURL("huge.mp4"))), "Refused: "+f.mediaURL("huge.mp4")+" answered video/mp4 of 716800 bytes, over Twilio's 500 KB for that type; Twilio accepts jpeg, png, gif, mp4, mp3, pdf and the other types listed in plugin-twilio. Nothing was sent.")
	wantFail(t, run1(t, on, mms(f.mediaURL("big1.jpg"), f.mediaURL("big2.jpg"))), "Refused: media totals 6.0 MB, over Twilio's 5 MB per message; nothing was sent.")

	f.numbers = []number{num(tollFree, "PN1", true, false)}
	wantFail(t, run1(t, on, mms(f.mediaURL("ok.png"))), "Refused: +18445550100 cannot send MMS (its capabilities say mms=false); nothing was sent.")
	if len(f.posts()) != 0 {
		t.Fatal("a refused MMS POSTed")
	}

	// Dry run still validates the media, without mediaUrlHosts.
	wantFail(t, run1(t, nil, mms(f.mediaURL("page.html"))), "Refused: "+f.mediaURL("page.html")+" answered text/html; Twilio accepts jpeg, png, gif, mp4, mp3, pdf and the other types listed in plugin-twilio. Nothing was sent.")

	// A HEAD without a length is refused.
	f.numbers = []number{num(tollFree, "PN1", true, true)}
	r := run1(t, on, mms(f.mediaURL("nolen.png")))
	if r.ok {
		t.Fatal("media with no Content-Length was accepted")
	}
}

func TestMMSSendsMediaUrls(t *testing.T) {
	f := newFake(t)
	f.media["ok.png"] = media{200, "image/png", 1000}
	f.media["b.pdf"] = media{200, "application/pdf", 1000}
	r := run1(t, cfg("mode", "send", "mediaUrlHosts", []string{"127.0.0.1"}),
		"mms\nTo: "+ownCell+"\nMedia: "+f.mediaURL("ok.png")+"\nMedia: "+f.mediaURL("b.pdf")+"\n")
	if !r.ok {
		t.Fatal(r.text)
	}
	p := f.posts()[0]
	if got := p.Form["MediaUrl"]; len(got) != 2 || got[0] != f.mediaURL("ok.png") || got[1] != f.mediaURL("b.pdf") {
		t.Fatalf("MediaUrl: %v", got)
	}
	if p.Form.Has("Body") {
		t.Fatal("an empty body was sent")
	}
}

// ---- Twilio's answers to a send ----------------------------------------------------------------

func TestSendErrorsFromTwilio(t *testing.T) {
	f := newFake(t)
	answer := func(status, code int, msg string) {
		f.post = func(url.Values) (int, any) {
			return status, map[string]any{"code": code, "message": msg, "status": status}
		}
	}
	answer(400, 21608, "The number is unverified")
	wantFail(t, run1(t, send(), sms(ownCell, "x")), "Twilio answered 21608: this Trial account can only text verified numbers. Verify +15551234567 in the Twilio Console, or upgrade the account.")

	answer(400, 21211, "The 'To' number +15551234567 is not a valid phone number.")
	wantFail(t, run1(t, send(), sms(ownCell, "x")), "Message to +15551234567 was failed with 21211: The 'To' number +15551234567 is not a valid phone number. https://www.twilio.com/docs/api/errors/21211")

	answer(400, 21614, "'To' number is not a valid mobile number")
	wantFail(t, run1(t, send(), sms(ownCell, "x")), "Message to +15551234567 was failed with 21614: 'To' number is not a valid mobile number. https://www.twilio.com/docs/api/errors/21614")

	answer(400, 21610, "Attempt to send to unsubscribed recipient")
	r := run1(t, send(), sms(ownCell, "x"))
	wantFail(t, r, "Message to +15551234567 failed with 21610: +15551234567 has opted out (sent STOP). They must text START to receive messages again.")
	st := r.put(colState, "poll")
	if st == nil || st["optedOut"].(map[string]any)[ownCell] == nil {
		t.Fatalf("21610 did not mark the number opted out: %v", st)
	}
}

func Test21610OnSendMarksAConsentedEntryOptedOut(t *testing.T) {
	f, o := consentedSetup(t)
	f.post = func(url.Values) (int, any) {
		return 400, map[string]any{"code": 21610, "message": "unsubscribed", "status": 400}
	}
	r := runWith(t, o, sms(consented, "x"))
	doc := r.put(colRecipients, strings.TrimPrefix(consented, "+"))
	if doc == nil || doc["status"] != "opted-out" || doc["optOutKeyword"] != "STOP" || doc["name"] != "Pat" {
		t.Fatalf("entry not opted out with its fields kept: %v", doc)
	}
}

func TestHTTPErrors(t *testing.T) {
	f := newFake(t)
	r := runWith(t, opts{cfg: send(), secrets: map[string]string{"accountSid": testAccount, "apiKeySid": testKeySid, "apiKeySecret": "wrong-secret"}}, sms(ownCell, "x"))
	wantFail(t, r, "Twilio refused the API key (401). The operator checks the values of TWILIO_API_KEY_SID and TWILIO_API_KEY_SECRET with the secret command, or a person makes a new restricted key and the operator sets it.")

	f.numbersCode = 403
	wantFail(t, run1(t, nil, sms(ownCell, "x")), "Twilio refused the request (403): the API key lacks the permission /twilio/phone-numbers/active-numbers/list. A person adds it to the restricted key in the Twilio Console.")
	f.numbersCode = 0

	f.post = func(url.Values) (int, any) { return 403, map[string]any{"code": 20403, "message": "Forbidden"} }
	wantFail(t, run1(t, send(), sms(ownCell, "x")), "Twilio refused the request (403): the API key lacks the permission /twilio/messaging/messages/create. A person adds it to the restricted key in the Twilio Console.")
}

func Test429IsRetriedThenGivenUp(t *testing.T) {
	f := newFake(t)
	f.tooMany = 2 // the numbers lookup is answered 429 twice, then succeeds
	r := run1(t, send(), sms(ownCell, "x"))
	if !r.ok {
		t.Fatalf("two 429s then success should send: %s", r.text)
	}
	f.tooMany = 3
	r = run1(t, cfg("mode", "send", "fromNumber", tollFree), sms(ownCell, "x"))
	wantFail(t, r, "Twilio answered 429 Too Many Requests; tried 3 times, nothing was sent. Try again later.")
	if len(f.posts()) != 4 { // 1 sent, then 3 tries answered 429
		t.Fatalf("want 4 POSTs in all, got %d", len(f.posts()))
	}
}

func TestUnreachable(t *testing.T) {
	f := newFake(t)
	f.srv.Close()
	wantFail(t, run1(t, nil, sms(ownCell, "x")), "Could not reach api.twilio.com; try again later. Nothing was sent.")
}
