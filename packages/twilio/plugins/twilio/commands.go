package main

import (
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const commandList = "sms, mms, status <sid>, list [out|in|all] [with:<number>] [since:<YYYY-MM-DD|Nd>] [max:<n>], check, account, number, registration, allowlist, recipients [active|opted-out|pending|all], poll, optout <number>"

func unknownCommand(word string) string {
	if word == "" {
		return "No command was given. The twilio commands are: " + commandList + "."
	}
	return fmt.Sprintf("Unknown command %q. The twilio commands are: %s.", word, commandList)
}

func (p *plugin) dispatch(text string) outcome {
	text = strings.TrimSpace(text)
	first, rest, _ := strings.Cut(text, "\n")
	words := strings.Fields(first)
	if len(words) > 0 && strings.EqualFold(words[0], "twilio") {
		words = words[1:]
		text = strings.Join(words, " ") + "\n" + rest
	}
	if len(words) == 0 {
		return outcome{err: refusal(unknownCommand(""))}
	}
	cmd, args := strings.ToLower(words[0]), words[1:]
	wrap := func(s string, err error) outcome { return outcome{text: s, err: err} }

	switch cmd {
	case "sms", "mms":
		return wrap(p.send(cmd, text))
	case "status":
		return wrap(p.status(args))
	case "list":
		return wrap(p.list(args))
	case "check":
		return wrap(p.check())
	case "account":
		return wrap(p.accountCommand())
	case "number":
		return wrap(p.numberCommand())
	case "registration":
		return wrap(p.registration())
	case "allowlist":
		return wrap(p.allowlist())
	case "recipients":
		return wrap(p.recipients(args))
	case "optout":
		return wrap(p.optout(args))
	case "poll":
		return p.poll()
	}
	return outcome{err: refusal(unknownCommand(words[0]))}
}

// ---- status -----------------------------------------------------------------------------------

func (p *plugin) status(args []string) (string, error) {
	if len(args) != 1 || !(strings.HasPrefix(args[0], "SM") || strings.HasPrefix(args[0], "MM")) {
		return "", refusal("Refused: status takes one message SID (SM… or MM…).")
	}
	if err := p.needSite(); err != nil {
		return "", err
	}
	if err := p.needSecrets(); err != nil {
		return "", err
	}
	sid := args[0]
	m, err := p.api.fetchMessage(sid)
	var e *apiError
	if errors.As(err, &e) && e.Status == 404 {
		return "", refusal(fmt.Sprintf("No message %s in this account.", sid))
	}
	if err != nil {
		return "", err
	}
	p.recordStatus(m)
	line := statusLine(m)
	if m.Status == "failed" || m.Status == "undelivered" {
		line += "\n" + failureSentence(m.Sid, m.To, m.From, m.Status, m.code(), deref(m.ErrorMessage))
	}
	return line, nil
}

func statusLine(m message) string {
	dir, other := "out", "to "+m.To
	if !m.out() {
		dir, other = "in", "from "+m.From
	}
	price := "none"
	if m.Price != nil && *m.Price != "" {
		price = *m.Price + " " + m.PriceUnit
	}
	errText := "none"
	if m.code() != 0 {
		errText = strconv.Itoa(m.code())
	}
	return fmt.Sprintf("%s | %s | %s | %s | %s | segments %s | price %s | error %s",
		m.Sid, dir, other, m.Status, m.date(), orZero(m.NumSegments), price, errText)
}

// recordStatus updates (or adds) the site copy of a message. It reports whether the status changed.
func (p *plugin) recordStatus(m message) (*messageDoc, bool) {
	now := p.now().UTC().Format(time.RFC3339)
	doc, ok := p.st.messages[m.Sid]
	if !ok {
		dir := "out"
		if !m.out() {
			dir = "in"
		}
		segs, _ := strconv.Atoi(m.NumSegments)
		created := isoTime(m.DateCreated)
		doc = &messageDoc{Sid: m.Sid, Direction: dir, To: m.To, From: m.From, Segments: segs,
			Snippet: snippet(m.Body, 80), Media: m.media(), Created: created}
	}
	changed := doc.Status != m.Status || doc.ErrorCode != m.code()
	doc.Status = m.Status
	doc.ErrorCode = m.code()
	doc.ErrorMessage = deref(m.ErrorMessage)
	if m.Price != nil {
		doc.Price, doc.PriceUnit = *m.Price, m.PriceUnit
	}
	if !ok || changed {
		doc.Updated = now
		p.st.putMessage(p, doc)
	}
	if m.code() == 21610 && m.out() {
		p.st.markOptedOut(p, m.To, "STOP", p.now())
	}
	return doc, changed
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func orZero(s string) string {
	if s == "" {
		return "0"
	}
	return s
}

// ---- list -------------------------------------------------------------------------------------

func (p *plugin) list(args []string) (string, error) {
	dir, with, max := "all", "", maxResults
	since := p.now().UTC().AddDate(0, 0, -7)
	for _, a := range args {
		low := strings.ToLower(a)
		switch {
		case low == "out" || low == "in" || low == "all":
			dir = low
		case strings.HasPrefix(low, "with:"):
			n, ok := normalise(a[5:])
			if !ok {
				return "", refusal(fmt.Sprintf("Refused: %s is not a phone number in +country format (E.164); nothing was sent.", a[5:]))
			}
			with = n
		case strings.HasPrefix(low, "since:"):
			v := low[6:]
			if strings.HasSuffix(v, "d") {
				days, err := strconv.Atoi(strings.TrimSuffix(v, "d"))
				if err != nil || days < 0 || days > 400 {
					return "", refusal("Refused: since takes YYYY-MM-DD or a number of days such as 7d.")
				}
				since = p.now().UTC().AddDate(0, 0, -days)
			} else {
				t, err := time.Parse("2006-01-02", v)
				if err != nil {
					return "", refusal("Refused: since takes YYYY-MM-DD or a number of days such as 7d.")
				}
				since = t
			}
		case strings.HasPrefix(low, "max:"):
			n, err := strconv.Atoi(low[4:])
			if err != nil || n < 1 {
				return "", refusal("Refused: max takes a whole number from 1.")
			}
			if n < max {
				max = n
			}
		default:
			return "", refusal(fmt.Sprintf("Refused: list does not take %q. It takes: list [out|in|all] [with:<number>] [since:<YYYY-MM-DD|Nd>] [max:<n>].", a))
		}
	}
	if err := p.needSecrets(); err != nil {
		return "", err
	}

	base := url.Values{"DateSent>": {since.Format("2006-01-02")}}
	var filters []url.Values
	ours := []string{}
	if dir != "all" || with != "" {
		var err error
		if ours, err = p.ourNumbers(); err != nil {
			return "", err
		}
	}
	add := func(from, to string) {
		f := url.Values{}
		for k, v := range base {
			f[k] = v
		}
		if from != "" {
			f.Set("From", from)
		}
		if to != "" {
			f.Set("To", to)
		}
		filters = append(filters, f)
	}
	switch {
	case dir == "all" && with == "":
		add("", "")
	default:
		for _, our := range ours {
			if dir == "out" || dir == "all" {
				add(our, with)
			}
			if dir == "in" || dir == "all" {
				add(with, our)
			}
		}
	}

	seen := map[string]bool{}
	var msgs []message
	for _, f := range filters {
		got, err := p.api.listMessages(f, max)
		if err != nil {
			return "", err
		}
		for _, m := range got {
			if !seen[m.Sid] {
				seen[m.Sid] = true
				msgs = append(msgs, m)
			}
		}
	}
	sort.SliceStable(msgs, func(i, j int) bool { return msgs[i].date() > msgs[j].date() })
	if len(msgs) > max {
		msgs = msgs[:max]
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%d message(s) since %s, newest first.", len(msgs), since.Format("2006-01-02"))
	for _, m := range msgs {
		dir := "out"
		if !m.out() {
			dir = "in"
		}
		fmt.Fprintf(&b, "\n%s | %s | %s | from %s | to %s | %s | %s | media %d",
			m.Sid, dir, m.date(), m.From, m.To, m.Status, untrusted(snippet(m.Body, 80)), m.media())
	}
	return b.String(), nil
}

// ---- account, number, registration --------------------------------------------------------------

func last4(s string) string {
	if len(s) <= 4 {
		return "…"
	}
	return "…" + s[len(s)-4:]
}

func (p *plugin) accountCommand() (string, error) {
	if err := p.needSecrets(); err != nil {
		return "", err
	}
	a, err := p.account()
	if err != nil {
		return "", err
	}
	line := fmt.Sprintf("Account %q (%s): type %s, status %s.", a.FriendlyName, last4(p.sec.AccountSid), a.Type, a.Status)
	if bal, cur, err := p.api.fetchBalance(); err == nil && bal != "" {
		line += fmt.Sprintf(" Balance %s %s.", bal, cur)
	} else {
		line += " Balance: not readable with this key (a restricted key cannot read it)."
	}
	if strings.EqualFold(a.Type, "Trial") {
		line += " A Trial account texts only verified numbers, with a trial prefix. To upgrade, press Upgrade at the top of the Twilio Console; billing overview: " + consoleUpgrade
	}
	return line, nil
}

func (p *plugin) numberCommand() (string, error) {
	if err := p.needSecrets(); err != nil {
		return "", err
	}
	snd, err := p.resolveSender()
	if err != nil {
		return "", err
	}
	if snd.from == "" {
		ns, err := p.smsNumbers()
		if err != nil {
			return "", err
		}
		var lines []string
		lines = append(lines, fmt.Sprintf("Sending through Messaging Service %s; the account's SMS-capable numbers:", snd.service))
		for _, n := range ns {
			lines = append(lines, describeNumber(n))
		}
		return strings.Join(lines, "\n"), nil
	}
	num, err := p.numberRecord(&snd)
	if err != nil {
		return "", err
	}
	source := "fromNumber"
	if p.cfg.FromNumber == "" {
		source = "the account's only SMS-capable number, used because fromNumber is empty"
	}
	return describeNumber(*num) + " (" + source + ")", nil
}

func numberType(e164 string) string {
	if isTollFree(e164) {
		return "toll-free"
	}
	return "local"
}

func describeNumber(n number) string {
	hook := "inbound webhook: none (leave it empty; polling reads replies)"
	if n.SmsURL != "" {
		hook = "inbound webhook: set (polling still reads replies; it may stay as it is)"
	}
	return fmt.Sprintf("Number %s: %s, capabilities sms=%t mms=%t voice=%t, %s",
		n.PhoneNumber, numberType(n.PhoneNumber), n.Capabilities.SMS, n.Capabilities.MMS, n.Capabilities.Voice, hook)
}

// regLine is one registration finding, in the check's words.
type regLine struct {
	state    string // Done, Next or Waiting
	text     string
	approved bool
	status   string
}

// registrationLines reads the toll-free verification of a toll-free From number and the 10DLC
// campaign of messagingServiceSid.
func (p *plugin) registrationLines(snd *sender) ([]regLine, error) {
	var lines []regLine
	if p.cfg.MessagingServiceSid != "" {
		cs, err := p.api.campaigns(p.cfg.MessagingServiceSid)
		if err != nil {
			return nil, err
		}
		if len(cs) == 0 {
			lines = append(lines, regLine{"Next", fmt.Sprintf("registration — Messaging Service %s has no A2P 10DLC campaign; register a brand and a campaign (with privacy and terms URLs): %s", p.cfg.MessagingServiceSid, console10DLC), false, "no campaign"})
		}
		for _, c := range cs {
			switch c.CampaignStatus {
			case "VERIFIED":
				lines = append(lines, regLine{"Done", "registration — A2P 10DLC campaign " + c.Sid + " VERIFIED", true, "VERIFIED"})
			case "FAILED":
				lines = append(lines, regLine{"Next", fmt.Sprintf("registration — A2P 10DLC campaign %s FAILED; read the reason and resubmit: %s", c.Sid, console10DLC), false, "FAILED"})
			default:
				lines = append(lines, regLine{"Waiting", fmt.Sprintf("registration — A2P 10DLC campaign %s %s since %s, typically 10–15 days", c.Sid, c.CampaignStatus, isoTime(c.DateCreated)), false, c.CampaignStatus})
			}
		}
	}
	if snd.from != "" && isTollFree(snd.from) {
		num, err := p.numberRecord(snd)
		if err != nil {
			return nil, err
		}
		vs, err := p.api.tollFreeVerifications(num.Sid)
		var e *apiError
		if errors.As(err, &e) && e.Status == 403 {
			lines = append(lines, regLine{"Next", "registration — the toll-free verification status cannot be read with this key; Twilio names no restricted-key permission for it. Check it in the Console: " + consoleTollFree, false, "unreadable"})
			return lines, nil
		}
		if err != nil {
			return nil, err
		}
		if len(vs) == 0 {
			lines = append(lines, regLine{"Next", fmt.Sprintf("registration — submit toll-free verification for %s in the Twilio Console (open the number, then its Regulatory Information tab; needs an upgraded account): %s", snd.from, consoleTollFree), false, "not submitted"})
		}
		for _, v := range vs {
			switch v.Status {
			case "TWILIO_APPROVED":
				lines = append(lines, regLine{"Done", "registration — toll-free verification of " + snd.from + " TWILIO_APPROVED", true, "TWILIO_APPROVED"})
			case "TWILIO_REJECTED":
				reason := deref(v.RejectionReason)
				if reason == "" {
					reason = "no reason given"
				}
				lines = append(lines, regLine{"Next", fmt.Sprintf("registration — toll-free verification of %s TWILIO_REJECTED (%s); fix it and resubmit: %s", snd.from, reason, consoleTollFree), false, "TWILIO_REJECTED"})
			default:
				lines = append(lines, regLine{"Waiting", fmt.Sprintf("registration — toll-free verification of %s %s since %s, typically 3–5 business days", snd.from, v.Status, isoTime(v.DateCreated)), false, v.Status})
			}
		}
	}
	if len(lines) == 0 {
		lines = append(lines, regLine{"Next", fmt.Sprintf("registration — %s is a local number: US texts need an A2P 10DLC campaign on a Messaging Service, and a toll-free number is the faster path. Register a campaign, then a person sets messagingServiceSid: %s", snd.label(), console10DLC), false, "no messagingServiceSid"})
	}
	return lines, nil
}

// registered says whether the sender is registered for US sending: toll-free verified or a 10DLC
// campaign VERIFIED.
func (p *plugin) registered(snd *sender) (bool, string, error) {
	lines, err := p.registrationLines(snd)
	if err != nil {
		return false, "", err
	}
	var statuses []string
	for _, l := range lines {
		if l.approved {
			return true, l.status, nil
		}
		statuses = append(statuses, l.status)
	}
	return false, strings.Join(statuses, ", "), nil
}

func (p *plugin) registration() (string, error) {
	if err := p.needSecrets(); err != nil {
		return "", err
	}
	snd, err := p.resolveSender()
	if err != nil {
		return "", err
	}
	lines, err := p.registrationLines(&snd)
	if err != nil {
		return "", err
	}
	var out []string
	for _, l := range lines {
		out = append(out, l.state+": "+l.text)
	}
	return strings.Join(out, "\n"), nil
}

// ---- allowlist, recipients, optout -------------------------------------------------------------

func listOrNone(items []string) string {
	if len(items) == 0 {
		return "none"
	}
	return strings.Join(items, ", ")
}

func (p *plugin) allowlist() (string, error) {
	if err := p.needSite(); err != nil {
		return "", err
	}
	p.st.rollDay(p.now())
	var b strings.Builder
	fmt.Fprintf(&b, "mode: %s\n", p.cfg.Mode)
	fmt.Fprintf(&b, "sendAllowlist: %s\n", listOrNone(p.cfg.SendAllowlist))
	fmt.Fprintf(&b, "allowConsentedRecipients: %t (maxConsentedRecipients %d, %d active consented honoured)\n",
		p.cfg.AllowConsentedRecipients, p.cfg.MaxConsentedRecipients, len(p.st.activeConsented(p.cfg.MaxConsentedRecipients)))
	hosts := listOrNone(p.cfg.MediaURLHosts)
	if len(p.cfg.MediaURLHosts) == 0 {
		hosts += " (MMS is refused in send mode)"
	}
	fmt.Fprintf(&b, "mediaUrlHosts: %s\n", hosts)
	fmt.Fprintf(&b, "allowedCountries: %s\n", listOrNone(p.cfg.AllowedCountries))
	var per []string
	for n, c := range p.st.poll.SentTodayByNumber {
		per = append(per, fmt.Sprintf("%s %d", n, c))
	}
	sort.Strings(per)
	fmt.Fprintf(&b, "sends today (UTC %s): %d of dailySendCap %d; perRecipientDailyCap %d (%s)",
		p.st.poll.DayKey, p.st.poll.SentToday, p.cfg.DailySendCap, p.cfg.PerRecipientDailyCap, listOrNone(per))
	return b.String(), nil
}

func (p *plugin) recipients(args []string) (string, error) {
	filter := "all"
	if len(args) > 0 {
		filter = strings.ToLower(args[0])
	}
	if len(args) > 1 || (filter != "all" && filter != "active" && filter != "opted-out" && filter != "pending") {
		return "", refusal("Refused: recipients takes one of active, opted-out, pending or all.")
	}
	if err := p.needSite(); err != nil {
		return "", err
	}
	honoured := map[string]bool{}
	for _, r := range p.st.activeConsented(p.cfg.MaxConsentedRecipients) {
		honoured[r.ID] = true
	}
	var rs []*recipient
	for _, r := range p.st.recipients {
		if filter == "all" || r.Status() == filter {
			rs = append(rs, r)
		}
	}
	sort.Slice(rs, func(i, j int) bool { return rs[i].ID < rs[j].ID })
	total := len(rs)
	if len(rs) > maxResults {
		rs = rs[:maxResults]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d %s recipient(s)", total, filter)
	if total > len(rs) {
		fmt.Fprintf(&b, ", the first %d shown", len(rs))
	}
	b.WriteString(".")
	for _, r := range rs {
		consentDate := r.consent("capturedAt")
		if consentDate == "" {
			consentDate = "no date"
		}
		method := r.consent("method")
		if method == "" {
			method = "no method"
		}
		line := fmt.Sprintf("\n%s | %s | consent %s %s", r.Number(), r.Status(), method, consentDate)
		switch {
		case r.Status() == "active" && honoured[r.ID]:
			line += " | honoured"
		case r.Status() == "active" && !r.byPerson():
			line += " | not honoured: a person must activate it on the twilio page"
		case r.Status() == "active":
			line += " | not honoured: over maxConsentedRecipients"
		case r.Status() == "opted-out":
			kw := r.str("optOutKeyword")
			if kw == "" {
				kw = "by request"
			}
			line += fmt.Sprintf(" | opted out (%s on %s)", kw, r.str("optedOutAt"))
		}
		b.WriteString(line)
	}
	return b.String(), nil
}

func (p *plugin) optout(args []string) (string, error) {
	if len(args) != 1 {
		return "", refusal("Refused: optout takes one phone number.")
	}
	n, ok := normalise(args[0])
	if !ok {
		return "", refusal(fmt.Sprintf("Refused: %s is not a phone number in +country format (E.164); nothing was sent.", args[0]))
	}
	if err := p.needSite(); err != nil {
		return "", err
	}
	if m, out := p.st.optedOut(n); out {
		return fmt.Sprintf("%s was already opted out (%s on %s); nothing changed.", n, m.Keyword, m.At), nil
	}
	p.st.markOptedOut(p, n, "by request", p.now())
	return fmt.Sprintf("Marked %s opted out (by request); nothing will be sent to it. Only a person re-activates a number, on the twilio page.", n), nil
}
