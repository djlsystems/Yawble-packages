package main

import (
	"encoding/json"
	"sort"
	"strings"
	"time"
)

const (
	siteName       = "twilio"
	colState       = "state"
	colMessages    = "messages"
	colRecipients  = "recipients"
	messagesKept   = 2000
	messagesMaxAge = 30 * 24 * time.Hour
	seenKept       = 48 * time.Hour
)

const siteMissing = `Twilio keeps its state on the site "twilio", collections "state", "messages" and "recipients", and this team's site could not be read. A person reinstalls the Twilio solution or restores the site.`

// pollState is the document state/poll.
type pollState struct {
	LastPollAt        string                `json:"lastPollAt,omitempty"`
	InboundSeen       map[string]string     `json:"inboundSeen"`
	DayKey            string                `json:"dayKey"`
	SentToday         int                   `json:"sentToday"`
	SentTodayByNumber map[string]int        `json:"sentTodayByNumber"`
	OptedOut          map[string]optOutMark `json:"optedOut"`
	FirstMessageSent  map[string]string     `json:"firstMessageSent"`
	AutoRepliedAt     map[string]string     `json:"autoRepliedAt,omitempty"` // number -> last auto-reply
}

type optOutMark struct {
	Keyword string `json:"keyword"`
	At      string `json:"at"`
}

// messageDoc is one document of `messages`, id = the message SID. Bodies are kept only as an
// 80-character snippet.
type messageDoc struct {
	Sid          string `json:"sid"`
	Direction    string `json:"direction"`
	To           string `json:"to"`
	From         string `json:"from"`
	Status       string `json:"status"`
	ErrorCode    int    `json:"errorCode,omitempty"`
	ErrorMessage string `json:"errorMessage,omitempty"`
	Segments     int    `json:"segments"`
	Price        string `json:"price,omitempty"`
	PriceUnit    string `json:"priceUnit,omitempty"`
	Snippet      string `json:"snippet"`
	Body         string `json:"body,omitempty"` // an outbound text exactly as sent
	Media        int    `json:"media"`
	Test         bool   `json:"test,omitempty"`
	Tag          string `json:"tag,omitempty"`    // "auto-reply" for a text the plugin sent by itself
	DryRun       bool   `json:"dryRun,omitempty"` // an auto-reply dry run records the text it would send
	Created      string `json:"created"`
	Updated      string `json:"updated"`
}

// recipient is one document of `recipients` (id = E.164 without "+"). The raw document is kept so a
// write (only ever an opt-out) carries every field the page wrote.
type recipient struct {
	ID        string
	Raw       map[string]any
	UpdatedAt string
	UpdatedBy string
}

func (r *recipient) str(key string) string {
	s, _ := r.Raw[key].(string)
	return s
}

func (r *recipient) Number() string {
	if n, ok := normalise(r.str("number")); ok {
		return n
	}
	return "+" + r.ID
}
func (r *recipient) Status() string { return r.str("status") }

func (r *recipient) consent(key string) string {
	c, _ := r.Raw["consent"].(map[string]any)
	s, _ := c[key].(string)
	return s
}

// byPerson says whether a person made the last write: the platform records a person's email, and a
// member (an agent or a plugin) as Team/Member. Only a person's write can make an entry active.
func (r *recipient) byPerson() bool {
	return strings.Contains(r.UpdatedBy, "@") && !strings.Contains(r.UpdatedBy, "/")
}

func (r *recipient) activatedAt() string {
	if a := r.str("activatedAt"); a != "" {
		return a
	}
	return r.UpdatedAt
}

type store struct {
	missing    bool
	poll       pollState
	pollDirty  bool
	setup      map[string]any
	setupDirty bool
	messages   map[string]*messageDoc
	recipients map[string]*recipient
}

func loadStore(reads []siteRead) *store {
	s := &store{messages: map[string]*messageDoc{}, recipients: map[string]*recipient{}}
	found := map[string]bool{}
	for _, r := range reads {
		if r.Site != siteName {
			continue
		}
		if r.Missing != nil {
			s.missing = true
			continue
		}
		found[r.Collection] = true
		for _, d := range r.Documents {
			switch r.Collection {
			case colState:
				if d.ID == "poll" {
					_ = json.Unmarshal(d.Doc, &s.poll)
				}
				if d.ID == "setup" {
					_ = json.Unmarshal(d.Doc, &s.setup)
				}
			case colMessages:
				var m messageDoc
				if json.Unmarshal(d.Doc, &m) == nil {
					if m.Sid == "" {
						m.Sid = d.ID
					}
					s.messages[d.ID] = &m
				}
			case colRecipients:
				raw := map[string]any{}
				if json.Unmarshal(d.Doc, &raw) == nil {
					s.recipients[d.ID] = &recipient{ID: d.ID, Raw: raw, UpdatedAt: d.UpdatedAt, UpdatedBy: d.UpdatedBy}
				}
			}
		}
	}
	if !found[colState] || !found[colMessages] || !found[colRecipients] {
		s.missing = true
	}
	if s.poll.InboundSeen == nil {
		s.poll.InboundSeen = map[string]string{}
	}
	if s.poll.SentTodayByNumber == nil {
		s.poll.SentTodayByNumber = map[string]int{}
	}
	if s.poll.OptedOut == nil {
		s.poll.OptedOut = map[string]optOutMark{}
	}
	if s.poll.FirstMessageSent == nil {
		s.poll.FirstMessageSent = map[string]string{}
	}
	if s.poll.AutoRepliedAt == nil {
		s.poll.AutoRepliedAt = map[string]string{}
	}
	return s
}

// rollDay starts a new UTC day's counts.
func (s *store) rollDay(now time.Time) {
	day := now.UTC().Format("2006-01-02")
	if s.poll.DayKey != day {
		s.poll.DayKey = day
		s.poll.SentToday = 0
		s.poll.SentTodayByNumber = map[string]int{}
		s.pollDirty = true
	}
}

func (s *store) recipient(number string) *recipient {
	return s.recipients[strings.TrimPrefix(number, "+")]
}

// activeConsented is the active entries the plugin honours: activated by a person, oldest
// activation first, at most max of them.
func (s *store) activeConsented(max int) []*recipient {
	var active []*recipient
	for _, r := range s.recipients {
		if r.Status() == "active" && r.byPerson() {
			active = append(active, r)
		}
	}
	sort.Slice(active, func(i, j int) bool {
		if active[i].activatedAt() != active[j].activatedAt() {
			return active[i].activatedAt() < active[j].activatedAt()
		}
		return active[i].ID < active[j].ID
	})
	if len(active) > max {
		active = active[:max]
	}
	return active
}

// optedOut says whether a number opted out, and how: a recipients entry marked opted-out, or a
// STOP seen from it (kept in state for numbers that have no entry, such as the person's own cell).
func (s *store) optedOut(number string) (optOutMark, bool) {
	if r := s.recipient(number); r != nil && r.Status() == "opted-out" {
		kw := r.str("optOutKeyword")
		if kw == "" {
			kw = "by request"
		}
		return optOutMark{Keyword: kw, At: r.str("optedOutAt")}, true
	}
	m, ok := s.poll.OptedOut[number]
	return m, ok
}

// markOptedOut records an opt-out: in state always, and on the recipients entry when there is one
// that is not opted out already. It reports whether anything changed.
func (s *store) markOptedOut(p *plugin, number, keyword string, at time.Time) bool {
	changed := false
	stamp := at.UTC().Format(time.RFC3339)
	if _, ok := s.poll.OptedOut[number]; !ok {
		s.poll.OptedOut[number] = optOutMark{Keyword: keyword, At: stamp}
		s.pollDirty = true
		changed = true
	}
	if r := s.recipient(number); r != nil && r.Status() != "opted-out" {
		r.Raw["status"] = "opted-out"
		r.Raw["optedOutAt"] = stamp
		r.Raw["optOutKeyword"] = keyword
		p.sitePut(colRecipients, r.ID, r.Raw)
		changed = true
	}
	return changed
}

func (s *store) putMessage(p *plugin, m *messageDoc) {
	s.messages[m.Sid] = m
	p.sitePut(colMessages, m.Sid, m)
}

// prune deletes messages older than 30 days, and the oldest over 2,000.
func (s *store) prune(p *plugin, now time.Time) {
	var docs []*messageDoc
	for _, m := range s.messages {
		docs = append(docs, m)
	}
	sort.Slice(docs, func(i, j int) bool { return docs[i].Created > docs[j].Created })
	for i, m := range docs {
		t, err := time.Parse(time.RFC3339, m.Created)
		if i >= messagesKept || (err == nil && now.Sub(t) > messagesMaxAge) {
			p.siteDelete(colMessages, m.Sid)
			delete(s.messages, m.Sid)
		}
	}
}

func (s *store) flush(p *plugin) {
	if s.missing {
		return
	}
	if s.pollDirty {
		p.sitePut(colState, "poll", s.poll)
		s.pollDirty = false
	}
	if s.setupDirty {
		p.sitePut(colState, "setup", s.setup)
		s.setupDirty = false
	}
}

func snippet(body string, max int) string {
	body = strings.Join(strings.Fields(body), " ")
	r := []rune(body)
	if len(r) > max {
		return string(r[:max-1]) + "…"
	}
	return body
}

const (
	untrustedOpen  = "<<<untrusted sms content>>>"
	untrustedClose = "<<<end of untrusted sms content>>>"
)

// untrusted marks text a texter wrote. Angle-bracket runs inside it are broken up, so the text
// cannot close the marker early and pass for the plugin's own words.
func untrusted(text string) string {
	text = strings.ReplaceAll(strings.ReplaceAll(text, "<<<", "‹‹‹"), ">>>", "›››")
	return untrustedOpen + text + untrustedClose
}
