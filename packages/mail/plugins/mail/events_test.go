package main

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"
)

// The sent, drafted and changed events: published only for what the mailbox took, one per message,
// and never with a body, an address list or an attachment.

const eventBody = "The quarterly numbers are attached below."

var eventFields = map[string][]string{
	"sent":    {"mailbox", "id", "thread", "to", "subject"},
	"drafted": {"mailbox", "id", "subject"},
	"changed": {"mailbox", "id", "subject", "action", "change"},
}

// mailboxEvents answers the run's sent, drafted and changed payloads by type, checking each carries
// exactly its declared fields and none of the body or the recipients' addresses.
func mailboxEvents(t *testing.T, o outcome, recipients ...string) map[string][]map[string]any {
	t.Helper()
	out := map[string][]map[string]any{}
	for _, p := range o.of("publish") {
		kind, _ := p["type"].(string)
		want, ours := eventFields[kind]
		if !ours {
			continue
		}
		payload, _ := p["payload"].(map[string]any)
		if got := slices.Sorted(maps.Keys(payload)); !slices.Equal(got, slices.Sorted(slices.Values(want))) {
			t.Fatalf("%s carries %v, want %v", kind, got, want)
		}
		raw, _ := json.Marshal(payload)
		for _, s := range append([]string{eventBody, "quarterly"}, recipients...) {
			if strings.Contains(strings.ToLower(string(raw)), strings.ToLower(s)) {
				t.Fatalf("%s carries %q: %s", kind, s, raw)
			}
		}
		out[kind] = append(out[kind], payload)
	}
	return out
}

func noMailboxEvents(t *testing.T, o outcome, what string) {
	t.Helper()
	if evs := mailboxEvents(t, o); len(evs) != 0 {
		t.Fatalf("%s published %v:\n%s", what, evs, o.raw)
	}
}

// eventBox is one provider's fake mailbox as the event tests drive it.
type eventBox struct {
	opts    func(config map[string]any) opts
	add     func(subject string) string // delivers a message to the Inbox and answers its id
	mailbox string
	smtp    *fakeSMTP // the IMAP box's SMTP server; nil for the others
}

func eventBoxes(t *testing.T) map[string]func(t *testing.T) eventBox {
	return map[string]func(t *testing.T) eventBox{
		"IMAP": func(t *testing.T) eventBox {
			f := newFakeIMAPWithSent(t)
			s := newFakeSMTP(t, "TLS", imapUser)
			return eventBox{mailbox: imapAccount, smtp: s,
				opts: func(c map[string]any) opts { return imapOpts(f, s, c) },
				add: func(subject string) string {
					now := time.Now()
					return f.deliver(t, "INBOX", rawReplyable("sender@example.com", "", subject, eventBody, now), now, false)
				}}
		},
		"Gmail": func(t *testing.T) eventBox {
			g := newGmailBox(t)
			return eventBox{mailbox: "person@example.test",
				opts: func(c map[string]any) opts { return opts{conn: g.conn(), config: c} },
				add:  func(subject string) string { return g.add("sender@example.com", subject, eventBody, time.Now(), false) }}
		},
		"Graph": func(t *testing.T) eventBox {
			f := newFakeGraph(t)
			return eventBox{mailbox: "person@contoso.test",
				opts: func(c map[string]any) opts { return graphOpts(f, c) },
				add:  func(subject string) string { return f.add("sender@example.com", subject, eventBody, time.Now(), false) }}
		},
	}
}

const eventEmail = "send\nTo: ana@example.com, ben@example.com\nCc: cy@example.com\nBcc: di@example.com\nSubject: Q3 numbers\n\n" + eventBody

var eventRecipients = []string{"ana@example.com", "ben@example.com", "cy@example.com", "di@example.com"}

func TestSentAndDraftedArePublishedOnlyForWhatWasSentOrSaved(t *testing.T) {
	for name, mk := range eventBoxes(t) {
		t.Run(name, func(t *testing.T) {
			b := mk(t)

			// send in send mode sends: one sent, counting every recipient, naming none.
			o := b.opts(nil)
			o.mode, o.allowlist = "send", []string{"@example.com"}
			out := runIt(t, eventEmail, o)
			mustOK(t, out)
			evs := mailboxEvents(t, out, eventRecipients...)
			if len(evs["sent"]) != 1 || len(evs["drafted"]) != 0 || len(evs["changed"]) != 0 {
				t.Fatalf("send mode: want one sent, got %v", evs)
			}
			sent := evs["sent"][0]
			if sent["mailbox"] != b.mailbox || sent["to"] != float64(4) || sent["subject"] != "Q3 numbers" {
				t.Fatalf("sent = %v", sent)
			}
			if b.smtp != nil {
				if msgs := b.smtp.messages(); len(msgs) != 1 || sent["id"] == "" || !strings.Contains(msgs[0].Data, "Message-ID: "+sent["id"].(string)+"\r\n") {
					t.Fatalf("over SMTP the sent id is the Message-ID it sent with: %v", sent)
				}
			}

			// The draft command, and send in draft mode, save a draft: drafted, never sent.
			for _, instr := range []string{"draft" + strings.TrimPrefix(eventEmail, "send"), eventEmail} {
				o := b.opts(nil)
				o.allowlist = []string{"@example.com"}
				out := runIt(t, instr, o)
				mustOK(t, out)
				evs := mailboxEvents(t, out, eventRecipients...)
				if len(evs["drafted"]) != 1 || len(evs["sent"]) != 0 {
					t.Fatalf("%q in draft mode: want one drafted, got %v", strings.SplitN(instr, "\n", 2)[0], evs)
				}
				if d := evs["drafted"][0]; d["mailbox"] != b.mailbox || d["subject"] != "Q3 numbers" || d["id"] == "" || !strings.Contains(out.output, d["id"].(string)) {
					t.Fatalf("drafted = %v, output %q", d, out.output)
				}
			}

			// reply sends in send mode, drafts with draft.
			id := b.add("Plans")
			for _, c := range []struct {
				reply, want string
			}{{"reply " + id, "sent"}, {"reply " + id + " draft", "drafted"}} {
				o := b.opts(nil)
				o.mode, o.allowlist = "send", []string{"@example.com"}
				o.more = []string{c.reply + "\n" + eventBody}
				out := runIt(t, "read "+id, o)
				mustOK(t, out)
				evs := mailboxEvents(t, out, "sender@example.com")
				if len(evs[c.want]) != 1 || len(evs) != 1 {
					t.Fatalf("%s: want one %s, got %v", c.reply, c.want, evs)
				}
				if evs[c.want][0]["subject"] != "Re: Plans" {
					t.Fatalf("%s: subject %v", c.reply, evs[c.want][0]["subject"])
				}
				if c.want == "sent" && evs["sent"][0]["to"] != float64(1) {
					t.Fatalf("a reply to the sender goes to one: %v", evs["sent"][0])
				}
			}
		})
	}
}

func TestARefusedOrFailedSendPublishesNothing(t *testing.T) {
	for name, mk := range eventBoxes(t) {
		t.Run(name, func(t *testing.T) {
			b := mk(t)
			for _, mode := range []string{"send", "draft"} {
				o := b.opts(nil)
				o.mode, o.allowlist = mode, []string{"ana@example.com"}
				out := runIt(t, eventEmail, o)
				mustFail(t, out, "is not on sendAllowlist")
				noMailboxEvents(t, out, mode+" to a recipient off the allowlist")
			}
			// A reply to a message Mailer never showed is refused.
			id := b.add("Plans")
			o := b.opts(nil)
			o.allowlist = []string{"@example.com"}
			out := runIt(t, "reply "+id+"\nOK.", o)
			mustFail(t, out, "Refused")
			noMailboxEvents(t, out, "a refused reply")
		})
	}

	// The mailbox refusing the send publishes no sent.
	f := newFakeGraph(t)
	f.failWith("POST", "/me/sendMail", 500, `{"error":{"code":"ErrorInternalServerError","message":"down"}}`)
	o := graphOpts(f, nil)
	o.mode, o.allowlist = "send", []string{"@example.com"}
	out := runIt(t, eventEmail, o)
	if out.ok {
		t.Fatalf("want the send to fail:\n%s", out.raw)
	}
	noMailboxEvents(t, out, "a send Graph refused")

	g := newGmailBox(t)
	g.failWith("POST", "/drafts", 500, `{"error":{"code":500,"message":"down"}}`)
	out = runIt(t, eventEmail, opts{conn: g.conn(), allowlist: []string{"@example.com"}})
	if out.ok {
		t.Fatalf("want the draft to fail:\n%s", out.raw)
	}
	noMailboxEvents(t, out, "a draft Gmail refused")
}

func TestChangedIsPublishedOncePerMessageMarkedMovedOrLabelled(t *testing.T) {
	long := strings.Repeat("Ä", 150) + "  \t second part"
	for name, mk := range eventBoxes(t) {
		t.Run(name, func(t *testing.T) {
			b := mk(t)
			a, c := b.add("Invoice 42"), b.add(long)
			config := map[string]any{"markRead": true, "moveTo": []string{"Archive", "Receipts"}}
			for _, cmd := range []struct{ instr, change string }{
				{"mark-read " + a + "," + c, "read"},
				{"mark-unread " + a + " " + c, "unread"},
				{"label Receipts " + a + " " + c, "labelled"},
				{"move Archive " + a + " " + c, "moved"},
			} {
				out := runIt(t, cmd.instr, b.opts(config))
				mustOK(t, out)
				evs := mailboxEvents(t, out)
				if len(evs) != 1 || len(evs["changed"]) != 2 {
					t.Fatalf("%s: want two changed, got %v\n%s", cmd.instr, evs, out.raw)
				}
				for i, want := range []string{"Invoice 42", eventSubject(long)} {
					e := evs["changed"][i]
					if e["action"] != "changed" || e["change"] != cmd.change || e["subject"] != want || e["mailbox"] != b.mailbox {
						t.Fatalf("%s: changed = %v", cmd.instr, e)
					}
				}
				if cmd.change == "moved" {
					// Each event names the message where it is now: the move's answer names the new id.
					for _, e := range evs["changed"] {
						if !strings.Contains(out.output, e["id"].(string)) {
							t.Fatalf("moved id %v not in %q", e["id"], out.output)
						}
					}
				} else if evs["changed"][0]["id"] != a || evs["changed"][1]["id"] != c {
					t.Fatalf("%s: ids %v", cmd.instr, evs["changed"])
				}
			}
		})
	}
	if s := eventSubject("Weekly\n  report " + long); len([]rune(s)) != 121 || !strings.HasPrefix(s, "Weekly report ÄÄ") || !strings.HasSuffix(s, "…") {
		t.Fatalf("a subject is one line cut to 120 runes, as received cuts it: %q", s)
	}
}

func TestReadsRefusalsAndFollowsPublishNoneOfTheNewEvents(t *testing.T) {
	for name, mk := range eventBoxes(t) {
		t.Run(name, func(t *testing.T) {
			b := mk(t)
			id := b.add("Plans")
			o := b.opts(nil)
			o.sites = []any{collection("watch", nil), collection("follows", nil)}
			o.more = []string{"search subject:Plans", "read " + id, "folders", "replied " + id, "follow " + id, "unfollow " + id, "watch", "ack " + id}
			out := runIt(t, "list", o)
			mustOK(t, out)
			noMailboxEvents(t, out, "a read")

			// Refused: markRead and moveTo off, a folder off moveTo, every delete word.
			for _, instr := range []string{"mark-read " + id, "mark-unread " + id, "move Archive " + id, "label Receipts " + id} {
				out := runIt(t, instr, b.opts(nil))
				mustFail(t, out, "Refused")
				noMailboxEvents(t, out, instr+" while it is off")
			}
			out = runIt(t, "move Trash "+id, b.opts(map[string]any{"moveTo": []string{"Archive"}}))
			mustFail(t, out, "is not on moveTo")
			noMailboxEvents(t, out, "a move off moveTo")
			for _, w := range []string{"delete", "trash", "expunge", "remove"} {
				out := runIt(t, w+" "+id, b.opts(map[string]any{"markRead": true, "moveTo": []string{"Archive"}}))
				mustFail(t, out, "There is no delete command")
				noMailboxEvents(t, out, w)
			}
		})
	}
}
