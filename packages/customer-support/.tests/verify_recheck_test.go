package customersupporttest

import (
	"slices"
	"strconv"
	"testing"
)

// Tester's re-check of the F1/F2 fix (14a5472), run over the developer's models of the playbook
// (migrate and handle in handled_test.go, findInquiry and remind in inquiry_test.go). Since c9fce40
// both tests assert the R1 and R3 fixes.

// F1 and F2 together: 1.0.0 handled A (inquiry inq-7 is A's), the person switched to B, then the
// update. The first 1.0.1 event is B's and the watch collection shows both, so B is in
// legacyMailboxes too. Nothing is re-counted on A, and B's own followed inquiry is found first.
// 1.0.0 documents do not say which mailbox they belong to, so a B event whose message has no B
// document reaches A's 1.0.0 inquiry only with its sender and subject (R3).
func TestRecheckSwitchedBeforeUpdateLegacyInquiry(t *testing.T) {
	legacy := []string{"7", "8"}
	watches := []string{"a@example.com|INBOX", "b@example.com|INBOX"}
	b := mailEvent{"b@example.com", "INBOX", []string{"8", "9"}, []string{"9"}}
	_, legacyMailboxes := migrate(legacy, watches, b)
	if !slices.Contains(legacyMailboxes, "b@example.com|INBOX") {
		t.Fatalf("legacyMailboxes %v: B, watched before the update, should be a candidate", legacyMailboxes)
	}

	old := &inquiryDoc{MessageID: "7", From: "pat@example.com", Subject: "Delivery", Status: "open"}
	docs := map[string]*inquiryDoc{"inq-7": old}
	rule2(docs, legacyMailboxes, b, "9")
	if d := findInquiry(docs, legacyMailboxes, "b@example.com", "", "9", "", ""); d == old {
		t.Fatal("B's own 9 resolved to A's 1.0.0 inquiry")
	}
	// A from A is still reached.
	if d := findInquiry(docs, legacyMailboxes, "a@example.com", "", "7", "pat@example.com", "Delivery"); d != old {
		t.Fatalf("A's 1.0.0 inquiry not reached from A: %+v", d)
	}
	// B lists a message 7, from another sender, that it has no document for.
	if d := findInquiry(docs, legacyMailboxes, "b@example.com", "", "7", "sam@example.net", "Other"); d == old {
		t.Fatal("with B in legacyMailboxes, B's message 7 (another sender and subject) resolves to A's 1.0.0 inquiry")
	}
	// A's 7 has left the 1.0.0 counted list (last 200) but inq-7 is kept: B's new inquiry 7 is
	// counted and gets a document of its own; A's is left as it was.
	rule2(docs, legacyMailboxes, mailEvent{"b@example.com", "INBOX", []string{"7"}, []string{"7"}}, "7")
	if docs["inq-b-example-com-INBOX-7"] == nil {
		t.Fatal("B's new inquiry 7 got no document of its own; it resolves to A's 1.0.0 inq-7")
	}
	if *old != (inquiryDoc{MessageID: "7", From: "pat@example.com", Subject: "Delivery", Status: "open"}) {
		t.Fatalf("A's 1.0.0 inquiry changed: %+v", old)
	}
}

// Fallback, then migration: the watches cannot be read on B's first events, so B's keys are
// written while the 1.0.0 bare ids stay; when the watches can be read, the filed keys go before
// B's, so with a full 1.0.0 list B's cap trims filed keys and never B's own (R1).
func TestRecheckFallbackThenMigrationTrimsNewKeys(t *testing.T) {
	var legacy []string
	for i := 0; i < countedCap; i++ {
		legacy = append(legacy, "L"+strconv.Itoa(i))
	}
	b := func(ids ...string) mailEvent { return mailEvent{"b@example.com", "INBOX", ids, ids} }
	counted, got, _ := handle(legacy, nil, b("b1", "b2"))
	if !slices.Equal(got, []string{"b1", "b2"}) {
		t.Fatalf("B with unreadable watches: counted %v", got)
	}
	watches := []string{"a@example.com|INBOX", "b@example.com|INBOX"}
	counted, got, _ = handle(counted, watches, b("b3"))
	if !slices.Equal(got, []string{"b3"}) {
		t.Fatalf("B after watches read: counted %v", got)
	}
	_, got, alerted := handle(counted, watches, b("b1"))
	if len(got) != 0 || len(alerted) != 0 {
		t.Fatalf("b1, already counted and alerted, is counted %v and alerted %v again when Mailer relists it", got, alerted)
	}
	// Nothing of A is lost or re-counted on this path.
	_, got, _ = handle(counted, watches, mailEvent{"a@example.com", "INBOX", []string{"L0", "L199"}, nil})
	if len(got) != 0 {
		t.Fatalf("A re-counted %v", got)
	}
}
