package main

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // followTimeZone needs the zone database, which a host may not have
)

const (
	// Each followed message is one document in this collection of the mail site.
	followsCollection = "follows"
	followPrefix      = "follow-"

	// followLimit is how many messages one mailbox may follow at once: every watch run checks
	// each of them for a reply.
	followLimit = 50
)

// clock is the time the follows go by; the tests set it.
var clock = time.Now

// followDoc is one followed message. ID is its id as Mailer last knew it; MessageID and Thread
// are what replied looks for, so a message that moved keeps its follow. Since is when it arrived,
// Followed when the follow began, LastReminded when a watch run last listed it as unanswered.
type followDoc struct {
	Provider     string `json:"provider"`
	Account      string `json:"account"`
	ID           string `json:"id"`
	MessageID    string `json:"messageId"`
	Thread       string `json:"thread"`
	From         string `json:"from"`
	Subject      string `json:"subject"`
	Since        string `json:"since"`
	Every        int    `json:"every"`
	Followed     string `json:"followed"`
	LastReminded string `json:"lastReminded"`
}

func (s *watchStore) loadFollows(sd siteDocs) {
	if sd.Missing != nil {
		s.followsMissing = *sd.Missing
		return
	}
	s.followsMissing = ""
	for _, d := range sd.Documents {
		var doc followDoc
		if json.Unmarshal(d.Doc, &doc) == nil {
			s.follows[d.ID] = &doc
		}
	}
}

func (s *watchStore) followsUsable() error {
	if s.followsMissing == "" {
		return nil
	}
	return failure(fmt.Sprintf("Mailer keeps its follows on the site %q, collection %q, and this team's site could not be read (%s). Install or update the Mail solution, which creates the site; nothing was changed.",
		watchSite, followsCollection, strings.TrimSuffix(s.followsMissing, ".")))
}

func (s *watchStore) putFollow(id string, doc *followDoc) {
	s.follows[id] = doc
	s.followsDeleted = slices.DeleteFunc(s.followsDeleted, func(x string) bool { return x == id })
	if !slices.Contains(s.followsChanged, id) {
		s.followsChanged = append(s.followsChanged, id)
	}
}

func (s *watchStore) deleteFollow(id string) {
	delete(s.follows, id)
	s.followsChanged = slices.DeleteFunc(s.followsChanged, func(x string) bool { return x == id })
	if !slices.Contains(s.followsDeleted, id) {
		s.followsDeleted = append(s.followsDeleted, id)
	}
}

// followID names a message's follow by its Message-ID, so the follow outlives a move that gives
// the message a new id. A message without one is named by its id.
func (m *mailer) followID(msg *fullMessage) string {
	key := "message-id\n" + reMsgID.FindString(msg.MessageID)
	if reMsgID.FindString(msg.MessageID) == "" {
		key = "id\n" + msg.ID
	}
	return followPrefix + m.hashOf(key)[:16]
}

// myFollows are this mailbox's follows, in the order they began.
func (m *mailer) myFollows() []string {
	var ids []string
	for id, f := range m.watch.follows {
		if f.Provider == m.p.name() && strings.EqualFold(f.Account, m.settings.account) {
			ids = append(ids, id)
		}
	}
	slices.SortFunc(ids, func(a, b string) int {
		if c := strings.Compare(m.watch.follows[a].Followed, m.watch.follows[b].Followed); c != 0 {
			return c
		}
		return strings.Compare(a, b)
	})
	return ids
}

// replyRule says what replied counts as a reply on this provider, and what it cannot see.
func (m *mailer) replyRule() string {
	var rule string
	switch m.p.name() {
	case "Gmail":
		rule = "A reply is a message labelled SENT in the message's thread, dated after it arrived."
	case "Microsoft Graph":
		rule = "A reply is a message in Sent Items in the message's conversation, sent after it arrived."
	default:
		rule = "A reply is a message in Sent, sent after the message arrived, whose In-Reply-To or References header names its Message-ID."
	}
	return rule + " Mailer cannot see a new message to the sender that is not a reply, or a reply sent from another mailbox: either reads as not replied."
}

// repliedCmd says whether the person replied, from this mailbox, to a message Mailer showed.
func (m *mailer) repliedCmd(ctx context.Context, args []string) (string, error) {
	if len(args) != 1 {
		return "", failure("replied takes one message id: `replied <id>`.")
	}
	id := args[0]
	if !m.wasShown(id) {
		return "", failure("Refused: Mailer has not listed or read message " + id + ", so it does not say whether it was replied to; list or read it first.")
	}
	m.progress("reading the message")
	orig, err := m.p.read(ctx, id)
	if err != nil {
		return "", err
	}
	m.progress("looking in Sent for a reply")
	r, err := m.p.replied(ctx, original{MessageID: orig.MessageID, Thread: orig.Thread, Arrived: orig.Arrived})
	if err != nil {
		return "", err
	}
	if r == nil {
		return "No: this mailbox has sent no reply to message " + id + ". " + m.replyRule(), nil
	}
	return fmt.Sprintf("Yes: message %s was replied to from this mailbox, sent %s (reply %s).\n%s", id, fmtTime(r.Sent), r.ID,
		untrusted("the reply's subject", "Subject: "+oneLine(r.Subject))), nil
}

// parseEvery reads every:<hours>h, 1 to 24.
func parseEvery(word string) (int, error) {
	v, ok := strings.CutPrefix(strings.ToLower(word), "every:")
	if !ok {
		return 0, failure("follow takes only every:<hours>h after the id; " + strconv.Quote(word) + " is not it. Nothing was followed.")
	}
	n, err := strconv.Atoi(strings.TrimSuffix(v, "h"))
	if err != nil || n < 1 || n > 24 {
		return 0, failure("every takes whole hours from 1 to 24, as in every:4h. Nothing was followed.")
	}
	return n, nil
}

// follow keeps a message Mailer showed in the follows collection, so each watch run checks it for
// a reply and lists it as unanswered once per interval.
func (m *mailer) follow(ctx context.Context, args []string) (string, error) {
	if len(args) < 1 || len(args) > 2 {
		return "", failure("follow takes a message id and, if wanted, how often to remind: `follow <id> [every:<hours>h]`.")
	}
	every := 1
	if len(args) == 2 {
		n, err := parseEvery(args[1])
		if err != nil {
			return "", err
		}
		every = n
	}
	if err := m.watch.followsUsable(); err != nil {
		return "", err
	}
	id := args[0]
	if !m.wasShown(id) {
		return "", failure("Refused: Mailer has not listed or read message " + id + ", so it does not follow it; list or read it first. Nothing was followed.")
	}
	m.progress("reading the message to follow")
	orig, err := m.p.read(ctx, id)
	if err != nil {
		return "", err
	}
	if m.p.name() == "IMAP" && reMsgID.FindString(orig.MessageID) == "" {
		return "", failure("Refused: message " + id + " has no Message-ID, so no reply can name it; nothing was followed.")
	}
	docID := m.followID(orig)
	existing := m.watch.follows[docID]
	if existing == nil && len(m.myFollows()) >= followLimit {
		return "", failure(fmt.Sprintf("Refused: this mailbox already follows %d messages, the most it follows; unfollow one first. Nothing was followed.", followLimit))
	}
	now := clock().UTC().Format(time.RFC3339)
	doc := &followDoc{Provider: m.p.name(), Account: m.settings.account, ID: orig.ID, MessageID: reMsgID.FindString(orig.MessageID),
		Thread: orig.Thread, From: cutRunes(oneLine(orig.From), 200), Subject: cutRunes(oneLine(orig.Subject), 200),
		Since: orig.Arrived.UTC().Format(time.RFC3339), Every: every, Followed: now}
	if orig.Arrived.IsZero() {
		doc.Since = now
	}
	head := "Following message " + orig.ID
	if existing != nil {
		doc.Followed, doc.LastReminded = existing.Followed, existing.LastReminded
		head = "Already following message " + orig.ID + "; from now on"
	}
	m.watch.putFollow(docID, doc)
	return fmt.Sprintf("%s: each watch run checks it for a reply, and lists it as unanswered every %d hours until there is one. %s\n%s",
		head, every, m.replyRule(), untrusted("the followed message", "From: "+doc.From+"\nSubject: "+doc.Subject)), nil
}

// unfollow stops following a message: named by the id Mailer knows it by, or, for a message that
// moved, by any id that reads as the same Message-ID.
func (m *mailer) unfollow(ctx context.Context, args []string) (string, error) {
	if len(args) != 1 {
		return "", failure("unfollow takes one message id: `unfollow <id>`.")
	}
	if err := m.watch.followsUsable(); err != nil {
		return "", err
	}
	id := args[0]
	docID := ""
	for _, k := range m.myFollows() {
		if m.watch.follows[k].ID == id {
			docID = k
		}
	}
	if docID == "" {
		if orig, err := m.p.read(ctx, id); err == nil {
			if _, ok := m.watch.follows[m.followID(orig)]; ok {
				docID = m.followID(orig)
			}
		}
	}
	if docID == "" {
		return "Not following message " + id + "; nothing was changed.", nil
	}
	m.watch.deleteFollow(docID)
	return "Stopped following message " + id + ": the watch no longer checks it for a reply.", nil
}

// followMoved gives each followed message that moved its new id.
func (m *mailer) followMoved(moved map[string]string) {
	if len(moved) == 0 || m.watch.followsMissing != "" {
		return
	}
	for _, k := range m.myFollows() {
		f := m.watch.follows[k]
		if n, ok := moved[f.ID]; ok {
			cp := *f
			cp.ID = n
			m.watch.putFollow(k, &cp)
		}
	}
}

// quietHours says whether now is inside the person's quiet hours, and when they end. A setting
// that cannot be read is named in notes and not applied.
func (m *mailer) quietHours(now time.Time) (quiet bool, until string, notes []string) {
	st := m.settings
	loc, err := time.LoadLocation(st.followTimeZone)
	if err != nil {
		notes = append(notes, fmt.Sprintf("followTimeZone %q is not a time zone name such as Europe/London; quiet hours go by UTC.", st.followTimeZone))
		loc = time.UTC
	}
	if st.followQuietStart == "" || st.followQuietEnd == "" {
		return false, "", notes
	}
	start, err1 := time.Parse("15:04", st.followQuietStart)
	end, err2 := time.Parse("15:04", st.followQuietEnd)
	if err1 != nil || err2 != nil {
		return false, "", append(notes, "followQuietStart and followQuietEnd take a time as HH:MM, as in 22:00; quiet hours are not applied.")
	}
	s, e := start.Hour()*60+start.Minute(), end.Hour()*60+end.Minute()
	local := now.In(loc)
	cur := local.Hour()*60 + local.Minute()
	switch {
	case s == e:
		quiet = false
	case s < e:
		quiet = cur >= s && cur < e
	default: // across midnight
		quiet = cur >= s || cur < e
	}
	return quiet, st.followQuietEnd + " " + loc.String(), notes
}

// due is whether a follow's interval has passed since it was last listed, or since it began.
func due(f *followDoc, now time.Time) bool {
	ref := f.LastReminded
	if ref == "" {
		ref = f.Followed
	}
	t, err := time.Parse(time.RFC3339, ref)
	return err != nil || now.Sub(t) >= time.Duration(max(f.Every, 1))*time.Hour
}

func hoursWaiting(f *followDoc, now time.Time) int {
	t, err := time.Parse(time.RFC3339, f.Since)
	if err != nil {
		return 0
	}
	return max(int(now.Sub(t).Hours()), 0)
}

// followPayload is plugin.mail.unanswered or plugin.mail.answered: the mailbox and each follow
// listed; never a body. extra adds each message's own fields: hoursWaiting, or the reply's time.
func followPayload(account string, list []*followDoc, extra func(i int) (string, any)) map[string]any {
	var ids []string
	msgs := []map[string]any{}
	for i, f := range list {
		ids = append(ids, f.ID)
		e := map[string]any{"id": f.ID, "from": cutRunes(f.From, 100), "subject": cutRunes(f.Subject, 120), "since": f.Since}
		k, v := extra(i)
		e[k] = v
		msgs = append(msgs, e)
	}
	return map[string]any{"mailbox": account, "count": len(list), "ids": strings.Join(ids, ", "), "messages": msgs}
}

// fitFollows answers how many of n follows, from the front, one event carries within its budget.
func fitFollows(n int, build func(n int) map[string]any) int {
	for n > 0 && len(mustJSON(build(n))) > eventBudget {
		n--
	}
	return n
}

// checkFollows is the follows' part of a watch run: a follow with a reply in Sent is removed and
// listed as answered; one without, whose interval has passed, is listed as unanswered outside the
// person's quiet hours. It answers the lines the run's output adds.
func (m *mailer) checkFollows(ctx context.Context) []string {
	if m.watch.followsMissing != "" {
		return nil
	}
	keys := m.myFollows()
	if len(keys) == 0 {
		return nil
	}
	now := clock().UTC()
	quiet, until, notes := m.quietHours(now)
	out := slices.Clone(notes)
	m.progress(fmt.Sprintf("checking %d followed messages for a reply", len(keys)))

	type found struct {
		key   string
		reply *sentReply
	}
	var answered []found
	var dueKeys, problems []string
	held := 0
	for _, k := range keys {
		f := m.watch.follows[k]
		since, _ := time.Parse(time.RFC3339, f.Since)
		r, err := m.p.replied(ctx, original{MessageID: f.MessageID, Thread: f.Thread, Arrived: since})
		switch {
		case err != nil:
			problems = append(problems, f.ID+": "+err.Error())
		case r != nil:
			answered = append(answered, found{k, r})
		case !due(f, now):
		case quiet:
			held++
		default:
			dueKeys = append(dueKeys, k)
		}
	}

	if len(answered) > 0 {
		docs := make([]*followDoc, len(answered))
		for i, a := range answered {
			docs[i] = m.watch.follows[a.key]
		}
		build := func(n int) map[string]any {
			return followPayload(m.settings.account, docs[:n], func(i int) (string, any) {
				return "repliedAt", answered[i].reply.Sent.UTC().Format(time.RFC3339)
			})
		}
		// A follow that does not fit in the event stays followed, and the next run lists it.
		n := fitFollows(len(docs), build)
		if n > 0 {
			m.publish("answered", build(n))
		}
		var lines []string
		for i := range n {
			lines = append(lines, fmt.Sprintf("%s | from %s | subject %s | replied %s", docs[i].ID, docs[i].From, docs[i].Subject, fmtTime(answered[i].reply.Sent)))
			m.watch.deleteFollow(answered[i].key)
		}
		out = append(out, fmt.Sprintf("Answered: %d followed messages have a reply from this mailbox and are no longer followed; one plugin.mail.answered event lists them.", n),
			untrusted("answered mail", strings.Join(lines, "\n")))
	}

	if len(dueKeys) > 0 {
		docs := make([]*followDoc, len(dueKeys))
		for i, k := range dueKeys {
			docs[i] = m.watch.follows[k]
		}
		build := func(n int) map[string]any {
			return followPayload(m.settings.account, docs[:n], func(i int) (string, any) { return "hoursWaiting", hoursWaiting(docs[i], now) })
		}
		// A follow that does not fit in the event keeps its lastReminded, so the next run lists it.
		n := fitFollows(len(docs), build)
		if n > 0 {
			m.publish("unanswered", build(n))
		}
		var lines []string
		for i := range n {
			lines = append(lines, fmt.Sprintf("%s | from %s | subject %s | waiting since %s (%d hours)", docs[i].ID, docs[i].From, docs[i].Subject, docs[i].Since, hoursWaiting(docs[i], now)))
			cp := *docs[i]
			cp.LastReminded = now.Format(time.RFC3339)
			m.watch.putFollow(dueKeys[i], &cp)
		}
		out = append(out, fmt.Sprintf("Unanswered: %d followed messages still have no reply from this mailbox; one plugin.mail.unanswered event lists them.", n),
			untrusted("unanswered mail", strings.Join(lines, "\n")))
	}
	if held > 0 {
		out = append(out, fmt.Sprintf("%d followed messages are due but it is quiet hours, until %s; the first check after lists them.", held, until))
	}
	if len(problems) > 0 {
		out = append(out, fmt.Sprintf("Could not check %d followed messages, which stay followed: %s", len(problems), strings.Join(problems, "; ")))
	}
	if len(answered) == 0 && len(dueKeys) == 0 && held == 0 && len(problems) == 0 {
		out = append(out, fmt.Sprintf("Following %d messages: none has a reply yet, and none is due a reminder.", len(keys)))
	}
	if len(notes) > 0 || len(problems) > 0 {
		m.quiet = false
	}
	return out
}
