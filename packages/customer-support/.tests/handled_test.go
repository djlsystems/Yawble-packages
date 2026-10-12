package customersupporttest

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// What the Manager has handled is kept by support-playbook as prose, so these tests hold the
// playbook to the rule it states for `counted` and then run that rule, as written, over the
// mailbox switches Change setup allows: keys are <mailbox>|<folder>|<id>, a 1.0.0 bare id is
// filed under every mailbox and folder the mail site's watch collection shows (and the event's
// own), and each mailbox and folder keeps its own last 200.

const countedCap = 200

type mailEvent struct {
	mailbox, folder string
	ids             []string
	inquiries       []string // the ids rule 2 matches
}

// countedKey is the playbook's key for a message.
func countedKey(mailbox, folder, id string) string {
	return strings.ToLower(mailbox) + "|" + folder + "|" + id
}

// migrate is the playbook's "Ids from 1.0.0": while counted holds bare ids and the mail site's
// watch collection shows any mailbox and folder (watches, as <account>|<folder>), every bare id
// becomes a key of each of them and of the new-mail event's own, in order, placed before the
// keys already there (they are the oldest); legacy is the list of those mailboxes and folders, the
// playbook's legacyMailboxes. With no watch to read, counted is left as it is.
func migrate(counted, watches []string, ev mailEvent) (out, legacy []string) {
	var keyed, bare []string
	for _, c := range counted {
		if strings.Contains(c, "|") {
			keyed = append(keyed, c)
		} else {
			bare = append(bare, c)
		}
	}
	if len(bare) == 0 || len(watches) == 0 {
		return counted, nil
	}
	for _, w := range append(slices.Clone(watches), ev.mailbox+"|"+ev.folder) {
		mailbox, folder, _ := strings.Cut(w, "|")
		if w = strings.ToLower(mailbox) + "|" + folder; !slices.Contains(legacy, w) {
			legacy = append(legacy, w)
		}
	}
	var filed []string
	for _, w := range legacy {
		for _, id := range bare {
			filed = append(filed, w+"|"+id)
		}
	}
	return append(filed, keyed...), legacy
}

// handle applies the playbook's New mail steps 1-2 to counted, with the mail site showing
// watches: it files 1.0.0 bare ids (migrate), and answers the ids counted and the ids alerted. A
// bare id still in counted, because no watch could be read, is handled in every mailbox.
func handle(counted, watches []string, ev mailEvent) (out []string, countedIDs, alerted []string) {
	out, _ = migrate(counted, watches, ev)
	out = slices.Clone(out)
	for _, id := range ev.ids {
		k := countedKey(ev.mailbox, ev.folder, id)
		if slices.Contains(out, k) || slices.Contains(out, id) {
			continue
		}
		out = append(out, k)
		countedIDs = append(countedIDs, id)
		if slices.Contains(ev.inquiries, id) {
			alerted = append(alerted, id)
		}
	}
	prefix := countedKey(ev.mailbox, ev.folder, "")
	for n := countIn(out, prefix); n > countedCap; n-- {
		i := slices.IndexFunc(out, func(k string) bool { return strings.HasPrefix(k, prefix) })
		out = slices.Delete(out, i, i+1)
	}
	return out, countedIDs, alerted
}

func countIn(keys []string, prefix string) int {
	n := 0
	for _, k := range keys {
		if strings.HasPrefix(k, prefix) {
			n++
		}
	}
	return n
}

// The playbook states the key, the 1.0.0 rule and the per-mailbox cap the model above runs.
func TestPlaybookCountedRule(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(pkgDir, "skills", "support-playbook.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Join(strings.Fields(string(data)), " ")
	for _, w := range []string{
		"`<mailbox>|<folder>|<id>`",
		"the event's `mailbox` in lower case, its `folder` as written and the message's id",
		"a message whose key is already in `counted` is not sorted, counted or texted again",
		"Keep the last 200 keys of each mailbox and folder",
		"An entry with no `|` is a bare id written by Customer Support 1.0.0",
		"read the collection `watch` on the site `mail`",
		"For each of those documents, and for a new-mail event's own `mailbox` and `folder`, add a key for every bare id, in the same order",
		"Put these filed keys before every key already in `counted`: they are the oldest, so a mailbox's cap drops them before any of its own keys",
		"set `legacyMailboxes` to those mailboxes and folders, each as `<mailbox>|<folder>`",
		"A bare id is never dropped and never counted again in any mailbox it could be from",
		"If the `watch` collection cannot be read or has no document, leave the bare ids as they are, take a bare id as already in `counted` for every mailbox and folder",
		"Turn any bare ids in `counted` into keys",
		"whose key is not in `counted`",
	} {
		if !strings.Contains(text, w) {
			t.Errorf("the playbook does not say %q", w)
		}
	}
	// The example running document uses the key, not a bare id.
	m := regexp.MustCompile(`"counted": \[([^\]]*)\]`).FindStringSubmatch(string(data))
	if m == nil {
		t.Fatal("the playbook has no example counted list")
	}
	for _, k := range regexp.MustCompile(`"([^"]*)"`).FindAllStringSubmatch(m[1], -1) {
		if strings.Count(k[1], "|") != 2 {
			t.Errorf("example counted entry %q is not <mailbox>|<folder>|<id>", k[1])
		}
	}
}

func TestSwitchingMailboxCountsTheNewOnesMail(t *testing.T) {
	var counted []string
	counted, got, alerted := handle(counted, nil, mailEvent{"Shop@Example.com", "INBOX", []string{"1", "2"}, []string{"2"}})
	if !slices.Equal(got, []string{"1", "2"}) || !slices.Equal(alerted, []string{"2"}) {
		t.Fatalf("first mailbox: counted %v alerted %v", got, alerted)
	}
	// Change setup binds a second mailbox; its first ids are the ones already handled.
	counted, got, alerted = handle(counted, nil, mailEvent{"help@other.example", "INBOX", []string{"1", "2"}, []string{"2"}})
	if !slices.Equal(got, []string{"1", "2"}) || !slices.Equal(alerted, []string{"2"}) {
		t.Fatalf("second mailbox: counted %v alerted %v, want both counted and 2 alerted", got, alerted)
	}
	// Relisted before ack: nothing twice.
	counted, got, _ = handle(counted, nil, mailEvent{"help@other.example", "INBOX", []string{"1", "2"}, []string{"2"}})
	if len(got) != 0 {
		t.Fatalf("relisted second mailbox counted %v again", got)
	}
	// Switching back: the first mailbox's mail is not counted again, its new mail is.
	counted, got, alerted = handle(counted, nil, mailEvent{"shop@example.com", "INBOX", []string{"1", "2", "3"}, []string{"2"}})
	if !slices.Equal(got, []string{"3"}) || len(alerted) != 0 {
		t.Fatalf("back to the first mailbox: counted %v alerted %v, want only 3", got, alerted)
	}
	// Another folder of the same mailbox is its own.
	_, got, _ = handle(counted, nil, mailEvent{"shop@example.com", "Support", []string{"1"}, nil})
	if !slices.Equal(got, []string{"1"}) {
		t.Fatalf("other folder: counted %v", got)
	}
}

// shopWatch is the mail site's watch collection of a team that only ever watched shop@'s INBOX.
var shopWatch = []string{"shop@example.com|INBOX"}

func TestCountedFrom100IsReadWithoutRecounting(t *testing.T) {
	legacy := []string{"40", "41", "42"} // as 1.0.0 wrote it
	counted, got, alerted := handle(legacy, shopWatch, mailEvent{"shop@example.com", "INBOX", []string{"41", "42", "43"}, []string{"42", "43"}})
	if !slices.Equal(got, []string{"43"}) || !slices.Equal(alerted, []string{"43"}) {
		t.Fatalf("after the update: counted %v alerted %v, want only 43", got, alerted)
	}
	want := []string{"shop@example.com|INBOX|40", "shop@example.com|INBOX|41", "shop@example.com|INBOX|42", "shop@example.com|INBOX|43"}
	if !slices.Equal(counted, want) {
		t.Fatalf("counted after migration: %v, want %v", counted, want)
	}
	// A later switch does not inherit the 1.0.0 ids.
	counted, got, _ = handle(counted, shopWatch, mailEvent{"help@other.example", "INBOX", []string{"40", "41"}, nil})
	if !slices.Equal(got, []string{"40", "41"}) {
		t.Fatalf("second mailbox after migration: counted %v", got)
	}
	// And switching back still knows them.
	_, got, _ = handle(counted, shopWatch, mailEvent{"shop@example.com", "INBOX", []string{"40", "42"}, nil})
	if len(got) != 0 {
		t.Fatalf("back to the first mailbox: counted %v again", got)
	}
}

func TestCountedCapIsPerMailbox(t *testing.T) {
	counted, _, _ := handle(nil, nil, mailEvent{"shop@example.com", "INBOX", []string{"1", "2"}, nil})
	var ids []string
	for i := 0; i < countedCap+50; i++ {
		ids = append(ids, "b"+strconv.Itoa(i))
	}
	counted, _, _ = handle(counted, nil, mailEvent{"help@other.example", "INBOX", ids, nil})
	if countIn(counted, "help@other.example|INBOX|") != countedCap {
		t.Fatalf("second mailbox keeps %d keys", countIn(counted, "help@other.example|INBOX|"))
	}
	if countIn(counted, "shop@example.com|INBOX|") != 2 {
		t.Fatal("the first mailbox's keys were dropped by the second's mail")
	}
}

// 1.0.0 handled A; the person switched to B; the first 1.0.1 event is B's. The mail site's watch
// collection shows both, so A's bare ids are filed under both: B's colliding id stays skipped
// once (the accepted cost), and switching back to A re-counts and re-alerts nothing.
func TestMigrationWhenFirstEventIsAnotherMailbox(t *testing.T) {
	legacy := []string{"7", "8"} // A's, as 1.0.0 wrote them
	watches := []string{"A@example.com|INBOX", "b@example.com|INBOX"}
	b := mailEvent{"b@example.com", "INBOX", []string{"8", "9"}, []string{"9"}}
	counted, legacyMailboxes := migrate(legacy, watches, b)
	if !slices.Equal(legacyMailboxes, []string{"a@example.com|INBOX", "b@example.com|INBOX"}) {
		t.Fatalf("legacyMailboxes %v", legacyMailboxes)
	}
	counted, got, alerted := handle(legacy, watches, b)
	if !slices.Equal(got, []string{"9"}) || !slices.Equal(alerted, []string{"9"}) {
		t.Fatalf("B: counted %v alerted %v, want only 9", got, alerted)
	}
	for _, id := range legacy {
		for _, mb := range []string{"a@example.com", "b@example.com"} {
			if !slices.Contains(counted, countedKey(mb, "INBOX", id)) {
				t.Fatalf("bare id %s not filed under %s: %v", id, mb, counted)
			}
		}
	}
	// Back on A: Mailer lists A's unacknowledged 7 and 8 again, with a new 10.
	_, got, alerted = handle(counted, watches, mailEvent{"a@example.com", "INBOX", []string{"7", "8", "10"}, []string{"7", "8"}})
	if !slices.Equal(got, []string{"10"}) || len(alerted) != 0 {
		t.Fatalf("back on A: counted %v alerted %v, want only 10 and no alert", got, alerted)
	}
}

// With no watch to read, the bare ids stay and count as handled everywhere, as in 1.0.0, until an
// event can read the watches.
func TestMigrationWaitsForTheWatches(t *testing.T) {
	legacy := []string{"7", "8"}
	counted, got, _ := handle(legacy, nil, mailEvent{"b@example.com", "INBOX", []string{"8", "9"}, nil})
	if !slices.Equal(got, []string{"9"}) || !slices.Contains(counted, "7") || !slices.Contains(counted, "8") {
		t.Fatalf("unreadable watches: counted %v, list %v", got, counted)
	}
	_, got, _ = handle(counted, []string{"a@example.com|INBOX", "b@example.com|INBOX"}, mailEvent{"a@example.com", "INBOX", []string{"7", "8"}, nil})
	if len(got) != 0 {
		t.Fatalf("A after the watches were read: counted %v again", got)
	}
}
