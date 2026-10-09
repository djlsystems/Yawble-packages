package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// watchBox is one provider's fake mailbox as the watch tests drive it.
type watchBox interface {
	conn() map[string]any
	// arrive delivers a message to the Inbox, received at when, and answers its id.
	arrive(t *testing.T, from, subject string, when time.Time, read bool) string
}

type imapWatchBox struct{ f *fakeIMAP }

func (b imapWatchBox) conn() map[string]any { return imapConnection(b.f, nil, appPassword) }
func (b imapWatchBox) arrive(t *testing.T, from, subject string, when time.Time, read bool) string {
	return b.f.deliver(t, "INBOX", rawMail(from, subject, "Body of "+subject, "", when, ""), when, read)
}

type graphWatchBox struct{ f *fakeGraph }

func (b graphWatchBox) conn() map[string]any { return b.f.conn() }
func (b graphWatchBox) arrive(t *testing.T, from, subject string, when time.Time, read bool) string {
	return b.f.add(from, subject, "<p>Body of "+subject+"</p>", when, read)
}

type gmailWatchBox struct{ g *gmailBox }

func (b gmailWatchBox) conn() map[string]any { return b.g.conn() }
func (b gmailWatchBox) arrive(t *testing.T, from, subject string, when time.Time, read bool) string {
	return b.g.add(from, subject, "Body of "+subject, when, read)
}

// eachProvider runs a test once per way in, each with its own fake.
func eachProvider(t *testing.T, test func(t *testing.T, box watchBox)) {
	t.Run("imap", func(t *testing.T) { test(t, imapWatchBox{newFakeIMAP(t, "TLS")}) })
	t.Run("graph", func(t *testing.T) { test(t, graphWatchBox{newFakeGraph(t)}) })
	t.Run("gmail", func(t *testing.T) { test(t, gmailWatchBox{newGmailBox(t)}) })
}

// siteStore is the Host's side of the watch's site collection: it keeps what a run writes with
// site.put and hands it to the next run, as a restart does. Nothing else carries over.
type siteStore struct {
	docs map[string]json.RawMessage
	puts []string // the id of every site.put, in order
}

func newSiteStore() *siteStore { return &siteStore{docs: map[string]json.RawMessage{}} }

func (s *siteStore) sites() []any {
	var docs []any
	for id, d := range s.docs {
		docs = append(docs, map[string]any{"id": id, "doc": d, "updatedAt": "2026-10-07T12:00:00Z", "updatedBy": "Mail/Mailer"})
	}
	if docs == nil {
		docs = []any{}
	}
	return []any{map[string]any{"site": "mail", "collection": "watch", "documents": docs, "total": len(docs), "cut": nil, "missing": nil}}
}

func (s *siteStore) apply(t *testing.T, o outcome) {
	t.Helper()
	for _, r := range o.of("site.put") {
		if r["site"] != "mail" || r["collection"] != "watch" {
			t.Fatalf("a site.put outside the watch's collection: %v", r)
		}
		id, _ := r["id"].(string)
		b, _ := json.Marshal(r["doc"])
		if len(b) > 64<<10 {
			t.Fatalf("a watch document over the site's 64 KB: %d bytes", len(b))
		}
		s.docs[id] = b
		s.puts = append(s.puts, id)
	}
}

// runAt runs one instruction as a fresh process with the store's documents, then keeps what it wrote.
func (s *siteStore) run(t *testing.T, instr string, o opts) outcome {
	t.Helper()
	o.sites = s.sites()
	out := runIt(t, instr, o)
	s.apply(t, out)
	return out
}

func watchOpts(box watchBox, config map[string]any) opts {
	return opts{conn: box.conn(), config: config}
}

// event answers the one plugin.mail.received a run published, failing on more than one.
func event(t *testing.T, o outcome) map[string]any {
	t.Helper()
	pubs := o.of("publish")
	if len(pubs) != 1 {
		t.Fatalf("want exactly one published event, got %d:\n%s", len(pubs), o.raw)
	}
	if pubs[0]["type"] != "received" {
		t.Fatalf("want the received event, got %v", pubs[0]["type"])
	}
	return pubs[0]["payload"].(map[string]any)
}

func eventIDs(t *testing.T, payload map[string]any) []string {
	t.Helper()
	var ids []string
	for _, m := range payload["messages"].([]any) {
		msg := m.(map[string]any)
		if _, has := msg["body"]; has {
			t.Fatalf("an event never carries a body: %v", msg)
		}
		ids = append(ids, msg["id"].(string))
	}
	return ids
}

func sameIDs(t *testing.T, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("want ids %v, got %v", want, got)
	}
}

func TestWatchStartsFromNowOnAFullMailbox(t *testing.T) {
	eachProvider(t, func(t *testing.T, box watchBox) {
		old := time.Now().Add(-48 * time.Hour)
		for i := range 30 {
			box.arrive(t, "old@example.org", fmt.Sprintf("Old %d", i), old.Add(time.Duration(i)*time.Minute), false)
		}
		st := newSiteStore()
		o := st.run(t, "watch", watchOpts(box, nil))
		mustOK(t, o)
		if n := len(o.of("publish")); n != 0 {
			t.Fatalf("the first check lists nothing already there, got %d events:\n%s", n, o.raw)
		}
		contains(t, o.output, "starts now")
		if len(st.docs) != 1 {
			t.Fatalf("want one position document, got %d", len(st.docs))
		}

		o = st.run(t, "watch", watchOpts(box, nil))
		mustOK(t, o)
		if n := len(o.of("publish")); n != 0 || !o.quiet {
			t.Fatalf("a check with nothing new publishes nothing and is quiet, got %d events quiet=%v:\n%s", n, o.quiet, o.raw)
		}
		contains(t, o.output, "No new mail in INBOX.")
	})
}

func TestWatchPublishesOneEventPerCheckListingEveryNewMessage(t *testing.T) {
	eachProvider(t, func(t *testing.T, box watchBox) {
		st := newSiteStore()
		mustOK(t, st.run(t, "watch", watchOpts(box, nil)))
		var want []string
		for i := range 4 {
			want = append(want, box.arrive(t, "person@example.com", fmt.Sprintf("New %d", i), time.Now().Add(time.Duration(i)*time.Second), false))
		}
		o := st.run(t, "watch", watchOpts(box, nil))
		mustOK(t, o)
		ev := event(t, o)
		sameIDs(t, eventIDs(t, ev), want...)
		if ev["count"] != float64(4) || ev["mailbox"] == "" || ev["folder"] != "INBOX" || ev["ids"] != strings.Join(want, ", ") {
			t.Fatalf("want count 4, the mailbox, the folder and the ids, got %v", ev)
		}
		first := ev["messages"].([]any)[0].(map[string]any)
		for _, k := range []string{"id", "from", "subject", "date", "snippet"} {
			if first[k] == nil || first[k] == "" {
				t.Fatalf("want %s on each listed message, got %v", k, first)
			}
		}
		if !strings.Contains(first["snippet"].(string), "Body of New 0") {
			t.Fatalf("want a short snippet, got %q", first["snippet"])
		}
		contains(t, o.output, "4 new and 0 listed again", untrustedOpen)
		for _, id := range st.puts {
			if id != st.puts[0] {
				t.Fatalf("one document per mailbox and folder, never per message: %v", st.puts)
			}
		}
	})
}

func TestWatchKeepsItsPositionAndAcknowledgementsAcrossRestarts(t *testing.T) {
	eachProvider(t, func(t *testing.T, box watchBox) {
		st := newSiteStore()
		mustOK(t, st.run(t, "watch", watchOpts(box, nil)))
		a := box.arrive(t, "a@example.org", "First", time.Now(), false)
		b := box.arrive(t, "b@example.org", "Second", time.Now().Add(time.Second), false)
		sameIDs(t, eventIDs(t, event(t, st.run(t, "watch", watchOpts(box, nil)))), a, b)

		// Each run is a new process: only the stored document says what was listed.
		o := st.run(t, "ack "+a+", "+b, watchOpts(box, nil))
		mustOK(t, o)
		contains(t, o.output, "Acknowledged 2")

		c := box.arrive(t, "c@example.org", "Third", time.Now().Add(2*time.Second), false)
		o = st.run(t, "watch", watchOpts(box, nil))
		sameIDs(t, eventIDs(t, event(t, o)), c)

		o = st.run(t, "ack "+c+" nope-1", watchOpts(box, nil))
		mustOK(t, o)
		contains(t, o.output, "Acknowledged 1: "+c, "Not waiting for an acknowledgement", "nope-1")
		o = st.run(t, "watch", watchOpts(box, nil))
		if n := len(o.of("publish")); n != 0 {
			t.Fatalf("nothing new and everything acknowledged: no event, got\n%s", o.raw)
		}
		if len(st.docs) != 1 {
			t.Fatalf("want one position document, got %d", len(st.docs))
		}
	})
}

func TestWatchListsAnUnacknowledgedMessageAgainUpTo3TimesThenNamesIt(t *testing.T) {
	eachProvider(t, func(t *testing.T, box watchBox) {
		st := newSiteStore()
		mustOK(t, st.run(t, "watch", watchOpts(box, nil)))
		id := box.arrive(t, "boss@example.org", "Please answer", time.Now(), false)
		for listing := 1; listing <= 1+relistLimit; listing++ {
			o := st.run(t, "watch", watchOpts(box, nil))
			ev := event(t, o)
			sameIDs(t, eventIDs(t, ev), id)
			if got := ev["messages"].([]any)[0].(map[string]any)["listed"]; got != float64(listing) {
				t.Fatalf("listing %d: want listed %d, got %v", listing, listing, got)
			}
			if strings.Contains(o.output, "Not acknowledged") {
				t.Fatalf("listing %d names it too early:\n%s", listing, o.output)
			}
		}
		o := st.run(t, "watch", watchOpts(box, nil))
		mustOK(t, o)
		if n := len(o.of("publish")); n != 0 {
			t.Fatalf("after 3 more listings it is not listed again, got\n%s", o.raw)
		}
		contains(t, o.output, "Not acknowledged after 4 listings", id, "Please answer")
		if o.quiet {
			t.Fatal("a run naming unacknowledged mail must not be quiet")
		}
		o = st.run(t, "watch", watchOpts(box, nil))
		if strings.Contains(o.output, id) {
			t.Fatalf("named once, then forgotten:\n%s", o.output)
		}
	})
}

func TestWatchAcknowledgedMessageIsNotListedAgain(t *testing.T) {
	eachProvider(t, func(t *testing.T, box watchBox) {
		st := newSiteStore()
		mustOK(t, st.run(t, "watch", watchOpts(box, nil)))
		id := box.arrive(t, "boss@example.org", "Hello", time.Now(), false)
		sameIDs(t, eventIDs(t, event(t, st.run(t, "watch", watchOpts(box, nil)))), id)
		mustOK(t, st.run(t, "ack "+id, watchOpts(box, nil)))
		if o := st.run(t, "watch", watchOpts(box, nil)); len(o.of("publish")) != 0 {
			t.Fatalf("an acknowledged message is not listed again:\n%s", o.raw)
		}
	})
}

func TestWatchLooksBackOnlyAsFarAsTheSetting(t *testing.T) {
	eachProvider(t, func(t *testing.T, box watchBox) {
		box.arrive(t, "old@example.org", "Ten days old", time.Now().AddDate(0, 0, -10), false)
		recent := box.arrive(t, "new@example.org", "Two days old", time.Now().AddDate(0, 0, -2), false)
		st := newSiteStore()
		o := st.run(t, "watch", watchOpts(box, map[string]any{"watchLookbackDays": 5}))
		mustOK(t, o)
		sameIDs(t, eventIDs(t, event(t, o)), recent)
		contains(t, o.output, "looking back 5 days")
	})
}

func TestWatchUnreadOnlyAndSendersFilters(t *testing.T) {
	eachProvider(t, func(t *testing.T, box watchBox) {
		st := newSiteStore()
		cfg := map[string]any{"watchSenders": []string{"@example.com"}}
		mustOK(t, st.run(t, "watch", watchOpts(box, cfg)))
		box.arrive(t, "person@example.com", "Already read", time.Now(), true)
		box.arrive(t, "stranger@example.org", "Not a watched sender", time.Now().Add(time.Second), false)
		want := box.arrive(t, "ops@example.com", "Watched and unread", time.Now().Add(2*time.Second), false)
		sameIDs(t, eventIDs(t, event(t, st.run(t, "watch", watchOpts(box, cfg)))), want)

		cfg = map[string]any{"watchUnreadOnly": false, "watchQuery": "subject:invoice"}
		st = newSiteStore()
		mustOK(t, st.run(t, "watch", watchOpts(box, cfg)))
		box.arrive(t, "a@example.org", "Lunch", time.Now().Add(3*time.Second), true)
		inv := box.arrive(t, "b@example.org", "Invoice 7", time.Now().Add(4*time.Second), true)
		sameIDs(t, eventIDs(t, event(t, st.run(t, "watch", watchOpts(box, cfg)))), inv)
	})
}

func TestWatchReadsNoMoreThanMaxResultsPerCheck(t *testing.T) {
	eachProvider(t, func(t *testing.T, box watchBox) {
		st := newSiteStore()
		o := watchOpts(box, nil)
		o.maxResults = 2
		mustOK(t, st.run(t, "watch", o))
		var ids []string
		for i := range 5 {
			ids = append(ids, box.arrive(t, "a@example.org", fmt.Sprintf("Burst %d", i), time.Now().Add(time.Duration(i)*time.Second), false))
		}
		got := eventIDs(t, event(t, st.run(t, "watch", o)))
		sameIDs(t, got, ids[0], ids[1])
		mustOK(t, st.run(t, "ack "+strings.Join(got, ","), o))
		got = eventIDs(t, event(t, st.run(t, "watch", o)))
		sameIDs(t, got, ids[2], ids[3])
		mustOK(t, st.run(t, "ack "+strings.Join(got, ","), o))
		sameIDs(t, eventIDs(t, event(t, st.run(t, "watch", o))), ids[4])
	})
}

func TestWatchEventStaysWithinThePayloadLimit(t *testing.T) {
	eachProvider(t, func(t *testing.T, box watchBox) {
		st := newSiteStore()
		o := watchOpts(box, nil)
		o.maxResults = 60
		mustOK(t, st.run(t, "watch", o))
		long := strings.Repeat("A long subject line ", 8)
		var ids []string
		for i := range 40 {
			ids = append(ids, box.arrive(t, fmt.Sprintf("sender%02d@example.org", i), fmt.Sprintf("%s %d", long, i), time.Now().Add(time.Duration(i)*time.Second), false))
		}
		var seen []string
		for check := 0; check < 10 && len(seen) < len(ids); check++ {
			out := st.run(t, "watch", o)
			pub := out.of("publish")
			if len(pub) != 1 {
				t.Fatalf("check %d: want one event, got %d", check, len(pub))
			}
			b, _ := json.Marshal(pub[0]["payload"])
			if len(b) > 4000 {
				t.Fatalf("check %d: the payload is %d characters, over the Host's 4,000", check, len(b))
			}
			got := eventIDs(t, pub[0]["payload"].(map[string]any))
			seen = append(seen, got...)
			mustOK(t, st.run(t, "ack "+strings.Join(got, ","), o))
		}
		sameIDs(t, seen, ids...)
	})
}

func TestWatchWithoutItsSiteFailsAndChecksNothing(t *testing.T) {
	f := newFakeIMAP(t, "TLS")
	missing := "No such site."
	o := runIt(t, "watch", opts{conn: imapConnection(f, nil, appPassword),
		sites: []any{map[string]any{"site": "mail", "collection": "watch", "documents": []any{}, "total": 0, "missing": missing}}})
	mustFail(t, o, `The watch keeps its place on the site "mail"`)
	contains(t, o.error, "No such site", "Install the Mail solution")
	if len(o.of("site.put")) != 0 || len(o.of("publish")) != 0 {
		t.Fatalf("nothing stored, nothing published:\n%s", o.raw)
	}
}

func TestIMAPWatchStartsAgainWhenTheFolderIsRenumbered(t *testing.T) {
	f := newFakeIMAP(t, "TLS")
	box := imapWatchBox{f}
	st := newSiteStore()
	mustOK(t, st.run(t, "watch", watchOpts(box, nil)))
	box.arrive(t, "a@example.org", "Before", time.Now(), false)
	f.renumber(t, "INBOX")
	box.arrive(t, "a@example.org", "After renumbering", time.Now(), false)
	o := st.run(t, "watch", watchOpts(box, nil))
	mustOK(t, o)
	contains(t, o.output, "UIDVALIDITY changed", "starts again from now")
	if len(o.of("publish")) != 0 || o.quiet {
		t.Fatalf("a renumbered folder lists nothing (its UIDs mean nothing now) and says so:\n%s", o.raw)
	}
	later := box.arrive(t, "a@example.org", "Later", time.Now(), false)
	sameIDs(t, eventIDs(t, event(t, st.run(t, "watch", watchOpts(box, nil)))), later)
}

func TestGmailWatchStartsAgainWhenTheHistoryHasExpired(t *testing.T) {
	g := newGmailBox(t)
	box := gmailWatchBox{g}
	st := newSiteStore()
	mustOK(t, st.run(t, "watch", watchOpts(box, nil)))
	box.arrive(t, "a@example.org", "Lost", time.Now(), false)
	g.oldest = 1_000_000
	o := st.run(t, "watch", watchOpts(box, nil))
	mustOK(t, o)
	contains(t, o.output, "no longer has the history", "starts again from now")
	g.oldest = 0
	later := box.arrive(t, "a@example.org", "Later", time.Now(), false)
	sameIDs(t, eventIDs(t, event(t, st.run(t, "watch", watchOpts(box, nil)))), later)
}

func TestGraphWatchDoesNotAnnounceAChangedMessageAgain(t *testing.T) {
	f := newFakeGraph(t)
	box := graphWatchBox{f}
	st := newSiteStore()
	cfg := map[string]any{"markRead": true, "watchUnreadOnly": false}
	mustOK(t, st.run(t, "watch", watchOpts(box, cfg)))
	id := box.arrive(t, "a@example.org", "Once", time.Now(), false)
	sameIDs(t, eventIDs(t, event(t, st.run(t, "watch", watchOpts(box, cfg)))), id)
	mustOK(t, st.run(t, "ack "+id, watchOpts(box, cfg)))
	// Marking it read changes it, so Graph's delta answers it again; it is still not new.
	mustOK(t, st.run(t, "mark-read "+id, watchOpts(box, cfg)))
	if o := st.run(t, "watch", watchOpts(box, cfg)); len(o.of("publish")) != 0 {
		t.Fatalf("a changed message is not new mail:\n%s", o.raw)
	}
}

func TestGraphWatchStartsAgainWhenTheDeltaTokenIsGone(t *testing.T) {
	f := newFakeGraph(t)
	box := graphWatchBox{f}
	st := newSiteStore()
	mustOK(t, st.run(t, "watch", watchOpts(box, nil)))
	box.arrive(t, "a@example.org", "Lost", time.Now(), false)
	f.failOnce("GET", "/me/mailFolders/inbox/messages/delta", 410, `{"error":{"code":"SyncStateNotFound","message":"The sync state is gone."}}`)
	o := st.run(t, "watch", watchOpts(box, nil))
	mustOK(t, o)
	contains(t, o.output, "no longer knows the stored position", "starts again from now")
	later := box.arrive(t, "a@example.org", "Later", time.Now().Add(2*time.Second), false)
	sameIDs(t, eventIDs(t, event(t, st.run(t, "watch", watchOpts(box, nil)))), later)
}
