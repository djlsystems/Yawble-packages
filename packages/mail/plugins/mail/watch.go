package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

const (
	// The watch keeps one document per mailbox and folder in this site collection, never one per
	// message. The Mail solution ships the site.
	watchSite       = "mail"
	watchCollection = "watch"

	// relistLimit is how many more checks list a message nobody acknowledged; the check after
	// the last of them names it in the run's output and stops waiting for it.
	relistLimit = 3

	// eventBudget bounds the published payload's JSON: the Host drops a payload over the member's
	// excerpt limit (4,000 characters by default), and redaction can lengthen it a little.
	eventBudget = 3600
	// docBudget bounds a watch document under the site's 64 KB per document.
	docBudget = 56 << 10
)

// pendingMsg is a message the watch listed and nobody has acknowledged yet. Listed is how many
// checks have listed it; 0 is a message found but not yet listed, because the event was full.
type pendingMsg struct {
	summary
	Listed int `json:"listed"`
}

// watchDoc is the one document of one mailbox and folder.
type watchDoc struct {
	Provider  string       `json:"provider"`
	Account   string       `json:"account"`
	Folder    string       `json:"folder"`
	Position  position     `json:"position"`
	Pending   []pendingMsg `json:"pending"`
	StartedAt string       `json:"startedAt"`
	LastCheck string       `json:"lastCheck"`
	LastFound int          `json:"lastFound"`
	Checks    int          `json:"checks"`
}

// watchStore is the watch documents this run was handed, and the ones it changed.
type watchStore struct {
	missing string // why the collection could not be read, when it could not
	docs    map[string]*watchDoc
	changed []string
}

type siteDocs struct {
	Site       string  `json:"site"`
	Collection string  `json:"collection"`
	Missing    *string `json:"missing"`
	Documents  []struct {
		ID  string          `json:"id"`
		Doc json.RawMessage `json:"doc"`
	} `json:"documents"`
}

func loadWatch(sites []siteDocs) *watchStore {
	s := &watchStore{docs: map[string]*watchDoc{}, missing: "this run was not handed it"}
	for _, sd := range sites {
		if sd.Site != watchSite || sd.Collection != watchCollection {
			continue
		}
		if sd.Missing != nil {
			s.missing = *sd.Missing
			continue
		}
		s.missing = ""
		for _, d := range sd.Documents {
			var doc watchDoc
			if json.Unmarshal(d.Doc, &doc) == nil {
				s.docs[d.ID] = &doc
			}
		}
	}
	return s
}

func (s *watchStore) usable() error {
	if s.missing == "" {
		return nil
	}
	return failure(fmt.Sprintf("The watch keeps its place on the site %q, collection %q, and this team's site could not be read (%s). Install the Mail solution, which creates the site; nothing was checked.",
		watchSite, watchCollection, strings.TrimSuffix(s.missing, ".")))
}

func (s *watchStore) put(id string, doc *watchDoc) {
	s.docs[id] = doc
	if !slices.Contains(s.changed, id) {
		s.changed = append(s.changed, id)
	}
}

// watchID names the document of one mailbox and folder: a hash, so the id holds no address and
// keeps to the site's id characters whatever the folder is called.
func watchID(provider, account, folder string) string {
	h := sha256.Sum256([]byte(provider + "\n" + strings.ToLower(account) + "\n" + strings.ToLower(folder)))
	return "watch-" + hex.EncodeToString(h[:12])
}

// keep applies the watch's filters - senders and query - to a new message. unread only is the
// provider's, which can see the flag.
func (m *mailer) keep(s summary) bool {
	if len(m.settings.watchSenders) > 0 {
		from := strings.ToLower(s.From)
		ok := false
		for _, want := range m.settings.watchSenders {
			if want = strings.ToLower(strings.TrimSpace(want)); want != "" && strings.Contains(from, want) {
				ok = true
			}
		}
		if !ok {
			return false
		}
	}
	q := m.settings.watchQuery
	if q == nil {
		return true
	}
	if q.From != "" && !strings.Contains(strings.ToLower(s.From), strings.ToLower(q.From)) {
		return false
	}
	if q.Subject != "" && !strings.Contains(strings.ToLower(s.Subject), strings.ToLower(q.Subject)) {
		return false
	}
	text := strings.ToLower(s.From + " " + s.Subject + " " + s.Snippet)
	for _, w := range q.words {
		if !strings.Contains(text, strings.ToLower(w)) {
			return false
		}
	}
	return true
}

// check is one run of the watch: it lists every message that arrived since the stored position,
// lists again the ones nobody acknowledged, publishes one event for all of them, and stores the
// new position - all in the one document.
func (m *mailer) check(ctx context.Context) (string, error) {
	if err := m.watch.usable(); err != nil {
		return "", err
	}
	st := m.settings
	spec := watchSpec{folder: st.watchFolder, unreadOnly: st.watchUnreadOnly, lookback: st.watchLookbackDays, max: st.maxResults}
	id := watchID(m.p.name(), st.account, st.watchFolder)
	now := time.Now().UTC().Format(time.RFC3339)
	var notes []string

	doc := m.watch.docs[id]
	var found []summary
	if doc == nil {
		m.progress("starting the watch on " + st.watchFolder)
		pos, msgs, err := m.p.watchStart(ctx, spec)
		if err != nil {
			return "", err
		}
		doc = &watchDoc{Provider: m.p.name(), Account: st.account, Folder: st.watchFolder, Position: pos, StartedAt: now}
		found = msgs
		if spec.lookback > 0 {
			notes = append(notes, fmt.Sprintf("The watch on %s starts now, looking back %d days.", st.watchFolder, spec.lookback))
		} else {
			notes = append(notes, fmt.Sprintf("The watch on %s starts now: mail already there is not listed, and mail arriving from now on is listed by the next checks.", st.watchFolder))
		}
	} else {
		m.progress("checking " + st.watchFolder + " for new mail")
		next, msgs, note, err := m.p.watchNext(ctx, spec, doc.Position)
		if err != nil {
			return "", err
		}
		cp := *doc
		doc = &cp
		doc.Position = next
		found = msgs
		if note != "" {
			notes = append(notes, note)
		}
	}

	// Who is listed this time: every message nobody acknowledged that has not used up its
	// listings, oldest first, then what is new.
	var gaveUp []pendingMsg
	var list []pendingMsg
	for _, p := range doc.Pending {
		if p.Listed > relistLimit {
			gaveUp = append(gaveUp, p)
			continue
		}
		list = append(list, p)
	}
	newCount := 0
	for _, s := range found {
		if !m.keep(s) || slices.ContainsFunc(list, func(p pendingMsg) bool { return p.ID == s.ID }) {
			continue
		}
		s.Snippet = cutRunes(oneLine(s.Snippet), 100)
		list = append(list, pendingMsg{summary: s})
		newCount++
	}

	inEvent := m.fitEvent(doc, list)
	relisted := 0
	for i := range list {
		if i < inEvent {
			if list[i].Listed > 0 {
				relisted++
			}
			list[i].Listed++
		}
	}
	if inEvent > 0 {
		m.publish(eventPayload(doc, list[:inEvent], len(list)-inEvent, eventSnippet(doc, list[:inEvent])))
	}

	doc.Pending = list
	for len(doc.Pending) > 0 && docSize(doc) > docBudget {
		gaveUp = append(gaveUp, doc.Pending[0])
		doc.Pending = doc.Pending[1:]
	}
	doc.LastCheck, doc.LastFound = now, newCount
	doc.Checks++
	m.watch.put(id, doc)

	var out []string
	out = append(out, notes...)
	switch {
	case inEvent == 0 && len(list) == 0:
		out = append(out, fmt.Sprintf("No new mail in %s.", st.watchFolder))
	default:
		out = append(out, fmt.Sprintf("%d new and %d listed again in %s: one %s event lists %d of them.", newCount, relisted, st.watchFolder, "plugin.mail.received", inEvent))
		if rest := len(list) - inEvent; rest > 0 {
			out = append(out, fmt.Sprintf("%d more did not fit in the event and are listed by the next check.", rest))
		}
		var lines []string
		for _, p := range list[:inEvent] {
			lines = append(lines, fmt.Sprintf("%s | thread %s | %s | from %s | subject %s | listed %d of %d", p.ID, oneLine(p.Thread), oneLine(p.Date), oneLine(p.From), oneLine(p.Subject), p.Listed, relistLimit+1))
		}
		out = append(out, untrusted("new mail", strings.Join(lines, "\n")))
		out = append(out, "Acknowledge each message once handled with `ack <ids>`, or it is listed again by the next check.")
	}
	if len(gaveUp) > 0 {
		var lines []string
		for _, p := range gaveUp {
			lines = append(lines, fmt.Sprintf("%s | from %s | subject %s", p.ID, oneLine(p.From), oneLine(p.Subject)))
		}
		out = append(out, fmt.Sprintf("Not acknowledged after %d listings, and no longer listed:", relistLimit+1))
		out = append(out, untrusted("unacknowledged mail", strings.Join(lines, "\n")))
	}
	if len(notes) > 0 || len(gaveUp) > 0 {
		m.quiet = false
	}
	return strings.Join(out, "\n"), nil
}

func eventSnippet(doc *watchDoc, list []pendingMsg) int {
	for _, n := range []int{100, 40, 0} {
		if len(mustJSON(eventPayload(doc, list, 0, n))) <= eventBudget {
			return n
		}
	}
	return 0
}

// fitEvent answers how many of list, from the front, one event can carry within its budget.
func (m *mailer) fitEvent(doc *watchDoc, list []pendingMsg) int {
	n := len(list)
	for n > 0 && len(mustJSON(eventPayload(doc, list[:n], len(list)-n, 0))) > eventBudget {
		n--
	}
	return n
}

// eventPayload is plugin.mail.received: who, where, and every message listed, with a short
// snippet; never a body. listed counts this listing. ids is ready for `ack {event.ids}`.
func eventPayload(doc *watchDoc, list []pendingMsg, more, snippet int) map[string]any {
	var ids []string
	msgs := []map[string]any{}
	for _, p := range list {
		ids = append(ids, p.ID)
		m := map[string]any{"id": p.ID, "thread": p.Thread, "from": cutRunes(oneLine(p.From), 100),
			"subject": cutRunes(oneLine(p.Subject), 120), "date": p.Date, "listed": p.Listed}
		if snippet > 0 && p.Snippet != "" {
			m["snippet"] = cutRunes(p.Snippet, snippet)
		}
		msgs = append(msgs, m)
	}
	return map[string]any{"mailbox": doc.Account, "folder": doc.Folder, "count": len(list), "ids": strings.Join(ids, ", "),
		"messages": msgs, "more": more}
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func docSize(doc *watchDoc) int { return len(mustJSON(doc)) }

// ack stops listing the messages named; a message the watch is not waiting for is said so.
func (m *mailer) ack(args []string) (string, error) {
	if err := m.watch.usable(); err != nil {
		return "", err
	}
	var ids []string
	for _, w := range args {
		for _, id := range strings.Split(w, ",") {
			if id = strings.TrimSpace(id); id != "" && !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
		}
	}
	if len(ids) == 0 {
		return "", failure("ack needs the ids of the messages handled, as `ack <id>, <id>`.")
	}
	var done, unknown []string
	for _, want := range ids {
		hit := false
		for docID, doc := range m.watch.docs {
			if !strings.EqualFold(doc.Account, m.settings.account) || doc.Provider != m.p.name() {
				continue
			}
			i := slices.IndexFunc(doc.Pending, func(p pendingMsg) bool { return p.ID == want })
			if i < 0 {
				continue
			}
			cp := *doc
			cp.Pending = slices.Delete(slices.Clone(doc.Pending), i, i+1)
			m.watch.put(docID, &cp)
			hit = true
		}
		if hit {
			done = append(done, want)
		} else {
			unknown = append(unknown, want)
		}
	}
	var out []string
	if len(done) > 0 {
		out = append(out, fmt.Sprintf("Acknowledged %d: %s. They are not listed again.", len(done), strings.Join(done, ", ")))
	}
	if len(unknown) > 0 {
		out = append(out, "Not waiting for an acknowledgement (already acknowledged, never listed, or no longer listed): "+strings.Join(unknown, ", ")+".")
	}
	return strings.Join(out, "\n"), nil
}
