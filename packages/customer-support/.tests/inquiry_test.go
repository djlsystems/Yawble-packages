package customersupporttest

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// Inquiry documents, as support-playbook states them: the id is inq- and the message's counted
// key with every other character turned into -, the document records mailbox, folder and
// messageId, and a 1.0.0 document (no mailbox) is reached only from a mailbox in legacyMailboxes.

type inquiryDoc struct {
	MessageID, Mailbox, Folder, From, Subject, Status string
	Reminders                                         int
}

var notIDChar = regexp.MustCompile(`[^A-Za-z0-9_-]`)

// inquiryID is the playbook's document id for a message.
func inquiryID(mailbox, folder, id string) string {
	return "inq-" + notIDChar.ReplaceAllString(countedKey(mailbox, folder, id), "-")
}

// findInquiry is the playbook's lookup. folder is "" for an unanswered or answered event, which
// names none; from and subject are the listed message's.
func findInquiry(docs map[string]*inquiryDoc, legacy []string, mailbox, folder, id, from, subject string) *inquiryDoc {
	mailbox = strings.ToLower(mailbox)
	for _, d := range docs {
		if d.Mailbox == mailbox && d.MessageID == id && (folder == "" && d.From == from && d.Subject == subject || folder != "" && d.Folder == folder) {
			return d
		}
	}
	inLegacy := slices.ContainsFunc(legacy, func(w string) bool {
		mb, f, _ := strings.Cut(w, "|")
		return mb == mailbox && (folder == "" || f == folder)
	})
	if d := docs["inq-"+notIDChar.ReplaceAllString(id, "-")]; inLegacy && d != nil && d.Mailbox == "" && d.MessageID == id && d.From == from && d.Subject == subject {
		return d
	}
	return nil
}

// rule2 is rule 2's add for a new-mail event: the inquiry is added as open, with no lookup (a
// message rule 2 matches has not been counted, so it has no document yet).
func rule2(docs map[string]*inquiryDoc, legacy []string, ev mailEvent, id string) {
	docs[inquiryID(ev.mailbox, ev.folder, id)] = &inquiryDoc{MessageID: id, Mailbox: strings.ToLower(ev.mailbox), Folder: ev.folder, Status: "open"}
}

// remind is the Unanswered inquiry step for one listed message: add a reminder, or add the
// inquiry, in the folder Mailer watches for that mailbox, when there is none.
func remind(docs map[string]*inquiryDoc, legacy []string, mailbox, watchedFolder, id, from, subject string) {
	if d := findInquiry(docs, legacy, mailbox, "", id, from, subject); d != nil {
		d.Reminders++
		return
	}
	docs[inquiryID(mailbox, watchedFolder, id)] = &inquiryDoc{MessageID: id, Mailbox: strings.ToLower(mailbox), Folder: watchedFolder, From: from, Subject: subject, Status: "open", Reminders: 1}
}

func TestPlaybookInquiryRule(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(pkgDir, "skills", "support-playbook.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Join(strings.Fields(string(data)), " ")
	for _, w := range []string{
		"its id `inq-` and the message's `counted` key with every character other than letters, digits, `-` and `_` turned into `-`",
		"(message `42` in `INBOX` of `support@example.com` is `inq-support-example-com-INBOX-42`)",
		"It records the `mailbox` in lower case, the `folder` and the `messageId`",
		"A document written by 1.0.0 is `inq-` and the id alone and has no `mailbox` or `folder`: keep it as it is",
		"the document with that `messageId`, the event's `mailbox` and, for a new-mail event, its `folder`",
		"only when the event's mailbox (and, for new mail, its folder) is in `legacyMailboxes`, the 1.0.0 document",
		"whose `from` and `subject` are the listed sender and subject",
		"An event from any other mailbox, or listing another sender or subject, never reads or changes a 1.0.0 document",
		"the 1.0.0 ones, only when that mailbox is in `legacyMailboxes`",
	} {
		if !strings.Contains(text, w) {
			t.Errorf("the playbook does not say %q", w)
		}
	}
	if got := inquiryID("support@example.com", "INBOX", "42"); got != "inq-support-example-com-INBOX-42" {
		t.Errorf("model id %q differs from the playbook's example", got)
	}
}

// The same id in another folder of the same mailbox is another inquiry.
func TestInquiryPerFolder(t *testing.T) {
	docs := map[string]*inquiryDoc{}
	rule2(docs, nil, mailEvent{"Shop@example.com", "INBOX", []string{"42"}, []string{"42"}}, "42")
	rule2(docs, nil, mailEvent{"shop@example.com", "Archive", []string{"42"}, []string{"42"}}, "42")
	if len(docs) != 2 || docs["inq-shop-example-com-INBOX-42"] == nil || docs["inq-shop-example-com-Archive-42"] == nil {
		t.Fatalf("inquiries %v, want one per folder", docs)
	}
	if d := findInquiry(docs, nil, "shop@example.com", "Archive", "42", "", ""); d == nil || d.Folder != "Archive" {
		t.Fatalf("Archive's 42 found %+v", d)
	}
}

// A's 1.0.0 inquiry is kept and reached from A; an event from B with the same id never touches it,
// whether or not B is in legacyMailboxes. Here B was watched before the update, so migrate puts B
// in legacyMailboxes, and only the 1.0.0 document's own sender and subject reach it.
func TestOtherMailboxLeaves100InquiryAlone(t *testing.T) {
	old := &inquiryDoc{MessageID: "7", From: "pat@example.com", Subject: "Inquiry about delivery", Status: "open"}
	docs := map[string]*inquiryDoc{"inq-7": old}
	_, legacy := migrate([]string{"7"}, []string{"a@example.com|INBOX", "b@example.com|INBOX"}, mailEvent{mailbox: "b@example.com", folder: "INBOX"})
	if !slices.Equal(legacy, []string{"a@example.com|INBOX", "b@example.com|INBOX"}) {
		t.Fatalf("legacyMailboxes %v", legacy)
	}

	remind(docs, legacy, "b@example.com", "INBOX", "7", "sam@example.net", "Inquiry: bulk pricing")
	if old.Reminders != 0 || old.Status != "open" || old.Mailbox != "" {
		t.Fatalf("B's reminder changed A's 1.0.0 inquiry: %+v", old)
	}
	if d := docs["inq-b-example-com-INBOX-7"]; d == nil || d.Reminders != 1 {
		t.Fatalf("B's 7 has no inquiry of its own: %+v", d)
	}
	rule2(docs, legacy, mailEvent{"b@example.com", "Archive", []string{"7"}, []string{"7"}}, "7")
	if old.Reminders != 0 || old.Mailbox != "" || docs["inq-b-example-com-Archive-7"] == nil {
		t.Fatalf("B's new inquiry 7 did not get its own document: old %+v", old)
	}
	if d := findInquiry(map[string]*inquiryDoc{"inq-7": old}, legacy, "b@example.com", "", "7", "sam@example.net", "Inquiry: bulk pricing"); d == old {
		t.Fatal("B's answered event would close A's 1.0.0 inquiry")
	}
	if d := findInquiry(docs, []string{"a@example.com|INBOX"}, "b@example.com", "", "7", "pat@example.com", "Inquiry about delivery"); d == old {
		t.Fatal("B, not in legacyMailboxes, reached A's 1.0.0 inquiry")
	}

	remind(docs, legacy, "A@example.com", "INBOX", "7", "pat@example.com", "Inquiry about delivery")
	if old.Reminders != 1 || len(docs) != 3 {
		t.Fatalf("A's reminder: old %+v, %d documents", old, len(docs))
	}
}
