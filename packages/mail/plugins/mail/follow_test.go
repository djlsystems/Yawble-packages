package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
)

// replyBox is one provider's fake mailbox as the replied and follow tests drive it.
type replyBox interface {
	watchBox
	// replyTo puts in Sent a reply to message id, threaded as the provider threads, sent at when.
	replyTo(t *testing.T, id, subject string, when time.Time) string
	// sendNew puts in Sent a new message that is no reply, sent at when.
	sendNew(t *testing.T, subject string, when time.Time) string
	// archive is the folder a move takes a message to.
	archive() string
}

type imapReplyBox struct {
	f   *fakeIMAP
	ids map[string]string // id -> Message-ID
	n   int
}

func (b *imapReplyBox) conn() map[string]any { return imapConnection(b.f, nil, appPassword) }
func (b *imapReplyBox) archive() string      { return "Archive" }
func (b *imapReplyBox) arrive(t *testing.T, from, subject string, when time.Time, read bool) string {
	b.n++
	mid := fmt.Sprintf("<orig-%d@example.org>", b.n)
	raw := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nMessage-ID: %s\r\nContent-Type: text/plain\r\n\r\nBody of %s\r\n",
		from, imapAccount, subject, when.Format(time.RFC1123Z), mid, subject)
	id := b.f.deliver(t, "INBOX", raw, when, read)
	b.ids[id] = mid
	return id
}

// sent stores a message the person sent, with the threading headers given ("" for none).
func (b *imapReplyBox) sent(t *testing.T, subject string, when time.Time, inReplyTo, references string) string {
	b.n++
	head := fmt.Sprintf("From: %s\r\nTo: sender@example.org\r\nSubject: %s\r\nDate: %s\r\nMessage-ID: <sent-%d@example.test>\r\n",
		imapAccount, subject, when.Format(time.RFC1123Z), b.n)
	if inReplyTo != "" {
		head += "In-Reply-To: " + inReplyTo + "\r\n"
	}
	if references != "" {
		head += "References: " + references + "\r\n"
	}
	return b.f.deliver(t, b.f.sent, head+"Content-Type: text/plain\r\n\r\nSent text\r\n", when, true)
}

func (b *imapReplyBox) replyTo(t *testing.T, id, subject string, when time.Time) string {
	return b.sent(t, subject, when, b.ids[id], "<earlier@example.org> "+b.ids[id])
}
func (b *imapReplyBox) sendNew(t *testing.T, subject string, when time.Time) string {
	return b.sent(t, subject, when, "", "")
}

type graphReplyBox struct{ f *fakeGraph }

func (b graphReplyBox) conn() map[string]any { return b.f.conn() }
func (b graphReplyBox) archive() string      { return "Archive" }
func (b graphReplyBox) arrive(t *testing.T, from, subject string, when time.Time, read bool) string {
	return b.f.add(from, subject, "<p>Body of "+subject+"</p>", when, read)
}
func (b graphReplyBox) replyTo(t *testing.T, id, subject string, when time.Time) string {
	conv := ""
	for _, m := range b.f.msgs {
		if m.ID == id {
			conv = m.Conv
		}
	}
	return b.f.sentIn(conv, subject, when)
}
func (b graphReplyBox) sendNew(t *testing.T, subject string, when time.Time) string {
	return b.f.sentIn("", subject, when)
}

type gmailReplyBox struct{ g *gmailBox }

func (b gmailReplyBox) conn() map[string]any { return b.g.conn() }
func (b gmailReplyBox) archive() string      { return "Archive" }
func (b gmailReplyBox) arrive(t *testing.T, from, subject string, when time.Time, read bool) string {
	return b.g.add(from, subject, "Body of "+subject, when, read)
}
func (b gmailReplyBox) replyTo(t *testing.T, id, subject string, when time.Time) string {
	return b.g.sentIn(b.g.byID(id).Thread, subject, when)
}
func (b gmailReplyBox) sendNew(t *testing.T, subject string, when time.Time) string {
	return b.g.sentIn("", subject, when)
}

// eachReplyProvider runs a test once per way in, each with its own fake and its Sent folder.
func eachReplyProvider(t *testing.T, test func(t *testing.T, box replyBox)) {
	t.Run("imap", func(t *testing.T) { test(t, &imapReplyBox{f: newFakeIMAPWithSent(t), ids: map[string]string{}}) })
	t.Run("graph", func(t *testing.T) { test(t, graphReplyBox{newFakeGraph(t)}) })
	t.Run("gmail", func(t *testing.T) { test(t, gmailReplyBox{newGmailBox(t)}) })
}

// setClock makes the follows go by *now for the rest of the test.
func setClock(t *testing.T, now *time.Time) {
	t.Helper()
	saved := clock
	clock = func() time.Time { return *now }
	t.Cleanup(func() { clock = saved })
}

// published answers the run's published events of one type, and checks no event carries a body.
func published(t *testing.T, o outcome, kind string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, p := range o.of("publish") {
		payload := p["payload"].(map[string]any)
		if msgs, ok := payload["messages"].([]any); ok {
			for _, m := range msgs {
				if _, has := m.(map[string]any)["body"]; has {
					t.Fatalf("an event never carries a body: %v", m)
				}
			}
		}
		if p["type"] == kind {
			out = append(out, payload)
		}
	}
	return out
}

func followed(t *testing.T, st *siteStore) []followDoc {
	t.Helper()
	var out []followDoc
	for _, raw := range st.follows {
		var f followDoc
		if err := json.Unmarshal(raw, &f); err != nil {
			t.Fatal(err)
		}
		out = append(out, f)
	}
	return out
}

func TestRepliedIsYesForAThreadedReplyInSentAndNoForANewMessageWithTheSameSubject(t *testing.T) {
	eachReplyProvider(t, func(t *testing.T, box replyBox) {
		arrived := time.Now().Add(-3 * time.Hour).Truncate(time.Second)
		id := box.arrive(t, "sender@example.org", "Quarterly plan", arrived, false)
		st := newSiteStore()
		o := watchOpts(box, nil)
		mustOK(t, st.run(t, "list", o))

		out := st.run(t, "replied "+id, o)
		mustOK(t, out)
		contains(t, out.output, "No: this mailbox has sent no reply to message "+id+".", "cannot see a new message to the sender that is not a reply, or a reply sent from another mailbox")

		// A new message with the same subject is not a reply.
		box.sendNew(t, "Re: Quarterly plan", arrived.Add(time.Hour))
		out = st.run(t, "replied "+id, o)
		mustOK(t, out)
		contains(t, out.output, "No: this mailbox has sent no reply to message "+id+".")

		sent := arrived.Add(2 * time.Hour)
		reply := box.replyTo(t, id, "Re: Quarterly plan", sent)
		out = st.run(t, "replied "+id, o)
		mustOK(t, out)
		contains(t, out.output, "Yes: message "+id+" was replied to from this mailbox, sent "+fmtTime(sent.UTC())[:16], "(reply "+reply+")",
			untrustedOpen+": the reply's subject", "Subject: Re: Quarterly plan")
	})
}

func TestRepliedDoesNotCountAMessageSentBeforeTheOriginalArrived(t *testing.T) {
	for _, tc := range []struct {
		name string
		box  func(t *testing.T) replyBox
	}{
		{"graph", func(t *testing.T) replyBox { return graphReplyBox{newFakeGraph(t)} }},
		{"gmail", func(t *testing.T) replyBox { return gmailReplyBox{newGmailBox(t)} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			box := tc.box(t)
			arrived := time.Now().Add(-time.Hour).Truncate(time.Second)
			id := box.arrive(t, "sender@example.org", "Re: Our call", arrived, false)
			// The person's own earlier message in the same thread, which this one answers.
			box.replyTo(t, id, "Our call", arrived.Add(-time.Hour))
			st := newSiteStore()
			o := watchOpts(box, nil)
			mustOK(t, st.run(t, "list", o))
			out := st.run(t, "replied "+id, o)
			mustOK(t, out)
			contains(t, out.output, "No: this mailbox has sent no reply to message "+id+".")
		})
	}
}

func TestIMAPRepliedFindsAReplyByReferencesAloneAndNeverByTheAnsweredFlag(t *testing.T) {
	box := &imapReplyBox{f: newFakeIMAPWithSent(t), ids: map[string]string{}}
	arrived := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	// The original carries \Answered, as a mail program sets it, and Sent holds no reply.
	box.n++
	mid := "<contract@example.org>"
	raw := fmt.Sprintf("From: sender@example.org\r\nTo: %s\r\nSubject: Contract\r\nDate: %s\r\nMessage-ID: %s\r\nContent-Type: text/plain\r\n\r\nTerms\r\n",
		imapAccount, arrived.Format(time.RFC1123Z), mid)
	data, err := box.f.user.Append("INBOX", literal{bytes.NewReader([]byte(raw))}, &imap.AppendOptions{Flags: []imap.Flag{imap.FlagSeen, imap.FlagAnswered}, Time: arrived})
	if err != nil {
		t.Fatal(err)
	}
	id := imapID("INBOX", data.UID)
	box.ids[id] = mid
	st := newSiteStore()
	o := watchOpts(box, nil)
	mustOK(t, st.run(t, "list", o))
	out := st.run(t, "replied "+id, o)
	mustOK(t, out)
	contains(t, out.output, "No: this mailbox has sent no reply to message "+id+".", "In-Reply-To or References header names its Message-ID")

	// A reply that names the original only in References is found.
	reply := box.sent(t, "Re: Contract", arrived.Add(time.Hour), "", box.ids[id])
	out = st.run(t, "replied "+id, o)
	mustOK(t, out)
	contains(t, out.output, "Yes: message "+id+" was replied to", "(reply "+reply+")")
}

func TestRepliedIsRefusedForAMessageMailerHasNotShown(t *testing.T) {
	g := newGmailBox(t)
	id := g.add("sender@example.org", "Plans", "Hi", time.Now(), false)
	out := newSiteStore().run(t, "replied "+id, gmailOpts(g, nil))
	mustFail(t, out, "Refused: Mailer has not listed or read message "+id+", so it does not say whether it was replied to; list or read it first.")
	if len(g.called("GET", "/messages/"+id)) != 0 || len(g.called("GET", "/threads")) != 0 {
		t.Fatal("refused before the mailbox was asked anything")
	}
	mustFail(t, runIt(t, "replied", gmailOpts(g, nil)), "replied takes one message id: `replied <id>`.")
}

// followSetup delivers a message, lists it and follows it at *now, answering its id and the store.
func followSetup(t *testing.T, box replyBox, now *time.Time, every string, config map[string]any) (string, *siteStore, opts) {
	t.Helper()
	id := box.arrive(t, "sender@example.org", "Invoice 7", time.Now().Add(-time.Hour).Truncate(time.Second), false)
	st := newSiteStore()
	o := watchOpts(box, config)
	// The watch starts before the follow, so the message arrived earlier is never a new-mail event.
	mustOK(t, st.run(t, "watch", o))
	mustOK(t, st.run(t, "list", o))
	cmd := "follow " + id
	if every != "" {
		cmd += " every:" + every
	}
	out := st.run(t, cmd, o)
	mustOK(t, out)
	contains(t, out.output, "Following message "+id+": each watch run checks it for a reply", "From: ", "Subject: Invoice 7")
	if f := followed(t, st); len(f) != 1 || f[0].ID != id || f[0].From == "" || f[0].Subject != "Invoice 7" || f[0].Since == "" || f[0].LastReminded != "" {
		t.Fatalf("want one follow with its id, from, subject and since, got %+v", f)
	}
	return id, st, o
}

func TestFollowListsAnUnansweredMessageOncePerInterval(t *testing.T) {
	eachReplyProvider(t, func(t *testing.T, box replyBox) {
		now := time.Now().UTC().Truncate(time.Second)
		setClock(t, &now)
		start := now
		id, st, o := followSetup(t, box, &now, "2h", nil)
		if f := followed(t, st); f[0].Every != 2 {
			t.Fatalf("want every 2 hours, got %d", f[0].Every)
		}

		at := func(d time.Duration) outcome {
			now = start.Add(d)
			out := st.run(t, "watch", o)
			mustOK(t, out)
			return out
		}
		for _, d := range []time.Duration{0, time.Hour, 119 * time.Minute} {
			if out := at(d); len(published(t, out, "unanswered")) != 0 {
				t.Fatalf("at %v the interval has not passed, yet:\n%s", d, out.raw)
			}
		}
		out := at(2 * time.Hour)
		evs := published(t, out, "unanswered")
		if len(evs) != 1 {
			t.Fatalf("want one unanswered event when the interval passes, got %d:\n%s", len(evs), out.raw)
		}
		ev := evs[0]
		msg := ev["messages"].([]any)[0].(map[string]any)
		if ev["ids"] != id || ev["count"] != float64(1) || ev["mailbox"] == "" || msg["id"] != id || msg["subject"] != "Invoice 7" ||
			msg["from"] == "" || msg["since"] == "" || msg["hoursWaiting"] != float64(3) {
			t.Fatalf("the event names the message, its sender, subject, since and hours waiting: %v", ev)
		}
		contains(t, out.output, "Unanswered: 1 followed messages still have no reply", id+" | from ")
		if f := followed(t, st); f[0].LastReminded != now.Format(time.RFC3339) {
			t.Fatalf("lastReminded moves on to the run that listed it, got %q", f[0].LastReminded)
		}
		for _, d := range []time.Duration{2*time.Hour + 5*time.Minute, 3 * time.Hour, 3*time.Hour + 59*time.Minute} {
			if out := at(d); len(published(t, out, "unanswered")) != 0 {
				t.Fatalf("at %v it was listed less than one interval ago:\n%s", d, out.raw)
			}
		}
		if out := at(4 * time.Hour); len(published(t, out, "unanswered")) != 1 {
			t.Fatalf("listed again one interval later:\n%s", out.raw)
		}
	})
}

func TestFollowIsNeverListedInQuietHoursAndOnceAfterThem(t *testing.T) {
	eachReplyProvider(t, func(t *testing.T, box replyBox) {
		// Quiet 22:00 to 07:00 in Tokyo (UTC+9, no daylight saving): 13:00 to 22:00 UTC.
		now := time.Now().UTC().Truncate(24 * time.Hour).Add(36 * time.Hour) // 12:00 UTC tomorrow, 21:00 in Tokyo
		setClock(t, &now)
		start := now
		cfg := map[string]any{"followQuietStart": "22:00", "followQuietEnd": "07:00", "followTimeZone": "Asia/Tokyo"}
		id, st, o := followSetup(t, box, &now, "", cfg)

		for h := 1; h <= 9; h++ { // 22:00 to 06:00 in Tokyo, every hour the follow is due
			now = start.Add(time.Duration(h) * time.Hour)
			out := st.run(t, "watch", o)
			mustOK(t, out)
			if len(published(t, out, "unanswered")) != 0 {
				t.Fatalf("hour %d is quiet, yet the follow was listed:\n%s", h, out.raw)
			}
			contains(t, out.output, "1 followed messages are due but it is quiet hours, until 07:00 Asia/Tokyo")
		}
		now = start.Add(10 * time.Hour) // 07:00 in Tokyo
		out := st.run(t, "watch", o)
		if evs := published(t, out, "unanswered"); len(evs) != 1 || evs[0]["ids"] != id {
			t.Fatalf("the first run after quiet hours lists it once:\n%s", out.raw)
		}
		now = start.Add(10*time.Hour + 5*time.Minute)
		if out := st.run(t, "watch", o); len(published(t, out, "unanswered")) != 0 {
			t.Fatalf("listed once, not on every run after quiet hours:\n%s", out.raw)
		}
	})
}

func TestAReplyRemovesTheFollowAndPublishesAnswered(t *testing.T) {
	eachReplyProvider(t, func(t *testing.T, box replyBox) {
		now := time.Now().UTC().Truncate(time.Second)
		setClock(t, &now)
		id, st, o := followSetup(t, box, &now, "", nil)
		box.replyTo(t, id, "Re: Invoice 7", time.Now().Truncate(time.Second))

		now = now.Add(3 * time.Hour) // due, but answered first
		out := st.run(t, "watch", o)
		mustOK(t, out)
		evs := published(t, out, "answered")
		if len(evs) != 1 || evs[0]["ids"] != id || evs[0]["count"] != float64(1) {
			t.Fatalf("want one answered event naming the message:\n%s", out.raw)
		}
		if msg := evs[0]["messages"].([]any)[0].(map[string]any); msg["repliedAt"] == "" || msg["subject"] != "Invoice 7" {
			t.Fatalf("the answered event says when the reply was sent: %v", msg)
		}
		if len(published(t, out, "unanswered")) != 0 {
			t.Fatalf("an answered follow is not listed as unanswered:\n%s", out.raw)
		}
		if len(out.of("site.delete")) != 1 || len(st.follows) != 0 {
			t.Fatalf("the follow is removed, got %d deletes and %d follows", len(out.of("site.delete")), len(st.follows))
		}
		contains(t, out.output, "Answered: 1 followed messages have a reply from this mailbox and are no longer followed")

		now = now.Add(3 * time.Hour)
		if out := st.run(t, "watch", o); len(out.of("publish")) != 0 {
			t.Fatalf("answered once; nothing is left to list:\n%s", out.raw)
		}
	})
}

func TestNothingDuePublishesNoEvent(t *testing.T) {
	eachReplyProvider(t, func(t *testing.T, box replyBox) {
		now := time.Now().UTC().Truncate(time.Second)
		setClock(t, &now)
		_, st, o := followSetup(t, box, &now, "4h", nil)
		now = now.Add(30 * time.Minute)
		out := st.run(t, "watch", o)
		mustOK(t, out)
		if n := len(out.of("publish")); n != 0 || !out.quiet {
			t.Fatalf("nothing due publishes no event and is quiet, got %d events quiet=%v:\n%s", n, out.quiet, out.raw)
		}
		contains(t, out.output, "Following 1 messages: none has a reply yet, and none is due a reminder.")
		if len(out.of("site.put")) != 1 || out.of("site.put")[0]["collection"] != "watch" {
			t.Fatalf("nothing due changes no follow:\n%s", out.raw)
		}
	})
}

func TestAMovedMessageKeepsItsFollow(t *testing.T) {
	eachReplyProvider(t, func(t *testing.T, box replyBox) {
		now := time.Now().UTC().Truncate(time.Second)
		setClock(t, &now)
		id, st, o := followSetup(t, box, &now, "", map[string]any{"moveTo": []string{box.archive()}})
		out := st.run(t, "move "+box.archive()+" "+id, o)
		mustOK(t, out)
		f := followed(t, st)
		if len(f) != 1 {
			t.Fatalf("the moved message keeps its follow, got %+v", f)
		}
		moved := f[0].ID
		if _, isGmail := box.(gmailReplyBox); !isGmail && moved == id {
			t.Fatalf("the follow takes the moved message's new id, still %s", moved)
		}
		contains(t, out.output, "Moved 1 messages to "+box.archive())

		// Still followed by Message-ID or thread after the move: due, then answered.
		now = now.Add(time.Hour)
		out = st.run(t, "watch", o)
		if evs := published(t, out, "unanswered"); len(evs) != 1 || evs[0]["ids"] != moved {
			t.Fatalf("the moved message is listed as unanswered by its new id:\n%s", out.raw)
		}
		if ib, ok := box.(*imapReplyBox); ok {
			ib.ids[moved] = ib.ids[id]
		}
		box.replyTo(t, moved, "Re: Invoice 7", time.Now().Truncate(time.Second))
		out = st.run(t, "watch", o)
		if evs := published(t, out, "answered"); len(evs) != 1 || evs[0]["ids"] != moved {
			t.Fatalf("a reply to the moved message is found:\n%s", out.raw)
		}
		if len(st.follows) != 0 {
			t.Fatal("answered, so no longer followed")
		}
	})
}

func TestFollowAndUnfollowTheirRefusals(t *testing.T) {
	box := &imapReplyBox{f: newFakeIMAPWithSent(t), ids: map[string]string{}}
	now := time.Now().UTC().Truncate(time.Second)
	setClock(t, &now)
	id := box.arrive(t, "sender@example.org", "Hello", time.Now().Add(-time.Hour), false)
	st := newSiteStore()
	o := watchOpts(box, nil)

	mustFail(t, st.run(t, "follow "+id, o), "Refused: Mailer has not listed or read message "+id+", so it does not follow it; list or read it first. Nothing was followed.")
	mustOK(t, st.run(t, "list", o))
	for _, every := range []string{"every:0h", "every:25h", "every:soon"} {
		mustFail(t, st.run(t, "follow "+id+" "+every, o), "every takes whole hours from 1 to 24, as in every:4h. Nothing was followed.")
	}
	mustFail(t, st.run(t, "follow "+id+" weekly", o), `follow takes only every:<hours>h after the id; "weekly" is not it.`)
	if len(st.follows) != 0 {
		t.Fatal("a refused follow keeps nothing")
	}
	// Without the follows collection nothing is followed, and the watch goes on without it.
	mustFail(t, runIt(t, "follow "+id, opts{conn: box.conn(), more: nil}), `Mailer keeps its follows on the site "mail", collection "follows", and this team's site could not be read`)

	mustOK(t, st.run(t, "follow "+id+" every:24h", o))
	out := st.run(t, "follow "+id+" every:3h", o)
	contains(t, out.output, "Already following message "+id+"; from now on")
	if f := followed(t, st); len(f) != 1 || f[0].Every != 3 {
		t.Fatalf("following again changes the interval of the one follow, got %+v", f)
	}

	out = st.run(t, "unfollow 999", o)
	mustOK(t, out)
	contains(t, out.output, "Not following message 999; nothing was changed.")
	out = st.run(t, "unfollow "+id, o)
	mustOK(t, out)
	contains(t, out.output, "Stopped following message "+id)
	if len(st.follows) != 0 || len(out.of("site.delete")) != 1 {
		t.Fatal("unfollow removes the follow")
	}
	mustFail(t, st.run(t, "unfollow", o), "unfollow takes one message id: `unfollow <id>`.")
}

func TestFollowKeepsNoMoreThanItsLimit(t *testing.T) {
	g := newGmailBox(t)
	st := newSiteStore()
	o := gmailOpts(g, map[string]any{"maxResults": 100.0})
	o.maxResults = 100
	for i := range followLimit + 1 {
		g.add("sender@example.org", fmt.Sprintf("Note %d", i), "Hi", time.Now().Add(-time.Duration(i)*time.Minute), false)
	}
	mustOK(t, st.run(t, "list max:100", o))
	for i, m := range g.msgs {
		out := st.run(t, "follow "+m.ID, o)
		if i < followLimit {
			mustOK(t, out)
			continue
		}
		mustFail(t, out, fmt.Sprintf("Refused: this mailbox already follows %d messages, the most it follows; unfollow one first.", followLimit))
	}
}
