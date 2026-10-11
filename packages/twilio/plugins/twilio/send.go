package main

import (
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const optOutFooter = "Reply STOP to opt out."

const (
	maxMedia        = 10
	maxMediaTotal   = 5 << 20
	maxImageBytes   = 5 << 20
	maxOtherBytes   = 500 << 10
	maxBodyChars    = 1600
	acceptedTypes   = "jpeg, png, gif, mp4, mp3, pdf"
	consoleNumbers  = "https://console.twilio.com/us1/develop/phone-numbers/manage/incoming"
	consoleBuy      = "https://console.twilio.com/us1/develop/phone-numbers/manage/search"
	consoleKeys     = "https://console.twilio.com/us1/account/keys-credentials/api-keys"
	consoleUpgrade  = "https://console.twilio.com/us1/billing/manage-billing/billing-overview"
	consoleTollFree = "https://console.twilio.com/us1/develop/phone-numbers/manage/incoming"
	console10DLC    = "https://console.twilio.com/us1/develop/sms/regulatory-compliance/a2p-onboarding"
)

var imageTypes = set("image/jpeg", "image/jpg", "image/gif", "image/png")

var otherTypes = set("video/mpeg4", "video/mp4", "video/mpeg", "video/webm", "video/h263",
	"audio/ogg", "audio/mpeg", "audio/mp4", "audio/mp3", "audio/3gpp", "application/pdf")

type sendRequest struct {
	kind  string // SMS or MMS
	to    string
	media []string
	body  string
}

// parseSend reads the lines of an sms or mms command: header lines (To:, and Media: for mms), a
// blank line, then the body.
func parseSend(kind, text string) (sendRequest, error) {
	req := sendRequest{kind: strings.ToUpper(kind)}
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	var to []string
	i := 1
	// The first line is the command; anything after it on that line is ignored.
	for ; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			i++
			break
		}
		name, value, ok := strings.Cut(line, ":")
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "to":
			to = append(to, strings.TrimSpace(value))
		case "media":
			req.media = append(req.media, strings.TrimSpace(value))
		default:
			if !ok {
				return req, refusal(fmt.Sprintf("Refused: %q is not a To: or Media: line; put a blank line before the body. Nothing was sent.", line))
			}
			return req, refusal(fmt.Sprintf("Refused: %s takes only To: lines%s, then a blank line and the body; %q is neither. Nothing was sent.", kind, map[bool]string{true: " and Media: lines", false: ""}[kind == "mms"], line))
		}
	}
	if i < len(lines) {
		req.body = strings.TrimRight(strings.Join(lines[i:], "\n"), " \n\t")
	}
	switch {
	case len(to) == 0:
		return req, refusal(fmt.Sprintf("Refused: %s needs a To: line with one phone number; nothing was sent.", kind))
	case len(to) > 1 || strings.ContainsAny(to[0], ",;"):
		return req, refusal("Refused: one recipient per command; send to each number with its own command. Nothing was sent.")
	}
	req.to = to[0]
	if kind == "sms" {
		if len(req.media) > 0 {
			return req, refusal("Refused: sms takes no Media: lines; use mms for media. Nothing was sent.")
		}
		if strings.TrimSpace(req.body) == "" {
			return req, refusal("Refused: sms needs a body after a blank line; nothing was sent.")
		}
	}
	if kind == "mms" && (len(req.media) < 1 || len(req.media) > maxMedia) {
		return req, refusal(fmt.Sprintf("Refused: mms takes 1 to 10 Media: lines, and this has %d; nothing was sent.", len(req.media)))
	}
	return req, nil
}

// sender is who a message goes from: a number, or a Messaging Service.
type sender struct {
	from    string
	service string
	num     *number // the From number's record, when it was read
}

func (s sender) label() string {
	if s.from != "" {
		return s.from
	}
	return s.service
}

func (p *plugin) missingSecret() string {
	switch {
	case p.sec.AccountSid == "":
		return "TWILIO_ACCOUNT_SID"
	case p.sec.APIKeySid == "":
		return "TWILIO_API_KEY_SID"
	case p.sec.APIKeySecret == "":
		return "TWILIO_API_KEY_SECRET"
	}
	return ""
}

func (p *plugin) needSecrets() error {
	if key := p.missingSecret(); key != "" {
		return refusal(fmt.Sprintf("Twilio is not set up: %s is not set on this platform; the operator sets it with the secret command; nothing was sent.", key))
	}
	return nil
}

func (p *plugin) needSite() error {
	if p.st.missing {
		return refusal(siteMissing)
	}
	return nil
}

// smsNumbers is the account's SMS-capable numbers, read once a run.
func (p *plugin) smsNumbers() ([]number, error) {
	if p.numbersLoaded {
		return p.numbers, nil
	}
	all, err := p.api.listNumbers("")
	if err != nil {
		return nil, err
	}
	p.numbers = nil
	for _, n := range all {
		if n.Capabilities.SMS {
			p.numbers = append(p.numbers, n)
		}
	}
	p.numbersLoaded = true
	return p.numbers, nil
}

func numberList(ns []number) string {
	var s []string
	for _, n := range ns {
		s = append(s, n.PhoneNumber)
	}
	return strings.Join(s, ", ")
}

// resolveSender finds the sender: fromNumber, else messagingServiceSid, else the account's only
// SMS-capable number.
func (p *plugin) resolveSender() (sender, error) {
	if p.cfg.FromNumber != "" {
		return sender{from: p.cfg.FromNumber}, nil
	}
	if p.cfg.MessagingServiceSid != "" {
		return sender{service: p.cfg.MessagingServiceSid}, nil
	}
	ns, err := p.smsNumbers()
	if err != nil {
		return sender{}, err
	}
	switch len(ns) {
	case 0:
		return sender{}, refusal("Twilio has no sender: the account has no SMS-capable number; buy one in the Twilio Console. Nothing was sent.")
	case 1:
		return sender{from: ns[0].PhoneNumber, num: &ns[0]}, nil
	}
	return sender{}, refusal(fmt.Sprintf("Twilio has %d numbers and fromNumber is empty: a person chooses one (%s) in this member's settings; nothing was sent.", len(ns), numberList(ns)))
}

// numberRecord reads the From number's record (capabilities, SID, webhook).
func (p *plugin) numberRecord(s *sender) (*number, error) {
	if s.num != nil || s.from == "" {
		return s.num, nil
	}
	ns, err := p.api.listNumbers(s.from)
	if err != nil {
		return nil, err
	}
	for i := range ns {
		if ns[i].PhoneNumber == s.from {
			s.num = &ns[i]
			return s.num, nil
		}
	}
	return nil, refusal(fmt.Sprintf("Refused: fromNumber %s is not a number on this Twilio account; nothing was sent. A person corrects fromNumber in this member's settings; the account's numbers are at %s", s.from, consoleNumbers))
}

// ourNumbers is the numbers inbound texts arrive at: fromNumber, else every SMS-capable number.
func (p *plugin) ourNumbers() ([]string, error) {
	if p.cfg.FromNumber != "" {
		return []string{p.cfg.FromNumber}, nil
	}
	ns, err := p.smsNumbers()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, n := range ns {
		out = append(out, n.PhoneNumber)
	}
	return out, nil
}

func (p *plugin) account() (account, error) {
	if p.acct != nil {
		return *p.acct, nil
	}
	a, err := p.api.fetchAccount()
	if err != nil {
		return a, err
	}
	p.acct = &a
	return a, nil
}

// send runs the outward-action rule (§7.1) in order, failing at the first refusal, then either
// reports the dry run or POSTs.
func (p *plugin) send(kind, text string) (string, error) {
	req, err := parseSend(kind, text)
	if err != nil {
		return "", err
	}
	if err := p.needSite(); err != nil {
		return "", err
	}

	// 1. Secrets, a sender (and, in send mode, an allowlist: checked at run).
	if err := p.needSecrets(); err != nil {
		return "", err
	}
	snd, err := p.resolveSender()
	if err != nil {
		return "", err
	}

	// 2. E.164, and a country on allowedCountries.
	to, ok := normalise(req.to)
	if !ok {
		return "", refusal(fmt.Sprintf("Refused: %s is not a phone number in +country format (E.164); nothing was sent.", req.to))
	}
	if c := country(to); !p.cfg.countryAllowed(c) {
		return "", refusal(fmt.Sprintf("Refused: %s is in %s, which is not on allowedCountries; nothing was sent.", to, c))
	}

	// 3. On sendAllowlist, or an active consented entry (send mode, allowConsentedRecipients on).
	allowlisted := p.cfg.onAllowlist(to)
	var consented *recipient
	if !allowlisted {
		if p.cfg.AllowConsentedRecipients && p.cfg.Mode == "send" {
			a, err := p.account()
			if err != nil {
				return "", err
			}
			if strings.EqualFold(a.Type, "Trial") {
				return "", refusal("Refused: allowConsentedRecipients is on but the account is a Trial account; only sendAllowlist numbers can be sent to.")
			}
			approved, status, err := p.registered(&snd)
			if err != nil {
				return "", err
			}
			if !approved {
				return "", refusal(fmt.Sprintf("Refused: allowConsentedRecipients is on but %s's sender is not registered (%s); only sendAllowlist numbers can be sent to.", to, status))
			}
			for _, r := range p.st.activeConsented(p.cfg.MaxConsentedRecipients) {
				if r.Number() == to {
					consented = r
				}
			}
		}
		if consented == nil {
			return "", refusal(fmt.Sprintf("Refused: %s is not on sendAllowlist or the active consented list; nothing was sent.", to))
		}
	}

	// 4. Not opted out.
	if m, out := p.st.optedOut(to); out {
		date := m.At
		if t, err := time.Parse(time.RFC3339, m.At); err == nil {
			date = t.Format("2006-01-02")
		}
		return "", refusal(fmt.Sprintf("Refused: %s opted out (%s on %s); nothing was sent.", to, m.Keyword, date))
	}

	// 5. Today's caps. Dry runs do not count.
	p.st.rollDay(p.now())
	if p.st.poll.SentToday >= p.cfg.DailySendCap {
		return "", refusal(fmt.Sprintf("Refused: today's sends reached dailySendCap (%d); nothing was sent.", p.cfg.DailySendCap))
	}
	if p.st.poll.SentTodayByNumber[to] >= p.cfg.PerRecipientDailyCap {
		return "", refusal(fmt.Sprintf("Refused: today's sends to %s reached perRecipientDailyCap (%d); nothing was sent.", to, p.cfg.PerRecipientDailyCap))
	}

	// 7. MMS media: hosts, then what each URL answers to HEAD.
	if req.kind == "MMS" {
		if err := p.checkMedia(req.media); err != nil {
			return "", err
		}
		// 8. The From number can send MMS.
		num, err := p.numberRecord(&snd)
		if err != nil {
			return "", err
		}
		if num != nil && !num.Capabilities.MMS {
			return "", refusal(fmt.Sprintf("Refused: %s cannot send MMS (its capabilities say mms=false); nothing was sent.", snd.from))
		}
	}

	// 9. Body length, segments and encoding.
	body, footer := p.compliant(req.body, to)
	if n := utf8.RuneCountInString(body); n > maxBodyChars {
		return "", refusal(fmt.Sprintf("Refused: the body is %d characters, over 1600; nothing was sent.", n))
	}
	segs, encoding := segments(body, isTollFree(snd.from))

	// 10. Dry run reports and stops; send POSTs.
	what := "SMS"
	if req.kind == "MMS" {
		what = fmt.Sprintf("MMS with %d media", len(req.media))
	}
	if p.cfg.Mode != "send" {
		return fmt.Sprintf("Dry run: would send %s to %s from %s, %d segment(s), %s; nothing was sent.", what, to, snd.label(), segs, encoding) + asSent(req.body, body), nil
	}

	form := url.Values{"To": {to}, "ValidityPeriod": {strconv.Itoa(validitySeconds)}}
	if snd.from != "" && p.cfg.MessagingServiceSid == "" {
		form.Set("From", snd.from)
	} else {
		form.Set("MessagingServiceSid", p.cfg.MessagingServiceSid)
	}
	if body != "" {
		form.Set("Body", body)
	}
	for _, m := range req.media {
		form.Add("MediaUrl", m)
	}
	m, err := p.api.sendMessage(form)
	if err != nil {
		return "", p.sendFailure(err, to, snd)
	}

	now := p.now().UTC()
	stamp := now.Format(time.RFC3339)
	p.st.poll.SentToday++
	p.st.poll.SentTodayByNumber[to]++
	if footer {
		p.st.poll.FirstMessageSent[to] = stamp
	}
	p.st.pollDirty = true
	p.sent = true

	from := m.From
	if from == "" {
		from = snd.from
	}
	doc := &messageDoc{
		Sid: m.Sid, Direction: "out", To: to, From: from, Status: m.Status, Segments: segs,
		Snippet: snippet(req.body, 80), Media: len(req.media), Created: stamp, Updated: stamp,
		Test: allowlisted && p.st.setup["firstDelivered"] == nil,
	}
	p.st.putMessage(p, doc)
	status := m.Status
	if status == "" {
		status = "queued"
	}
	return fmt.Sprintf("Sent %s to %s, status %s, %d segment(s), %s.", m.Sid, to, status, segs, encoding) + asSent(req.body, body), nil
}

// compliant is the text as it is sent: the business name the campaign registered in front
// (messagePrefix) and the opt-out line at the end (optOutFooter), to EVERY recipient - the
// sendAllowlist's numbers are people a campaign texts too. Deterministic, so neither the agent
// writing a text nor the person asking for it can leave them out. A text that already opens with
// the name, or already says "Reply STOP", is not given it twice. footer says whether the opt-out
// line was added, which "first" records per number.
func (p *plugin) compliant(text, to string) (body string, footer bool) {
	body = text
	if prefix := p.cfg.MessagePrefix; prefix != "" && !startsWithName(body, prefix) {
		body = prefix + body
	}
	switch p.cfg.OptOutFooter {
	case "every":
		footer = true
	case "first":
		footer = p.st.poll.FirstMessageSent[to] == ""
	}
	if footer && strings.Contains(strings.ToLower(body), "reply stop") {
		footer = false
	}
	if footer {
		trimmed := strings.TrimRight(body, " \n")
		switch {
		case trimmed == "":
			body = trimmed
		case strings.HasSuffix(trimmed, ".") || strings.HasSuffix(trimmed, "!") || strings.HasSuffix(trimmed, "?"):
			body = trimmed + " "
		default:
			body = trimmed + ". "
		}
		body += optOutFooter
	}
	return body, footer
}

// startsWithName says whether a text already opens with the prefix's name: "DJL Systems alert: ..."
// opens with "DJL Systems: " once its punctuation is set aside, in any case.
func startsWithName(text, prefix string) bool {
	name := strings.TrimRight(prefix, " :;,.-–—|")
	return name != "" && strings.HasPrefix(strings.ToLower(strings.TrimLeft(text, " ")), strings.ToLower(name))
}

// asSent shows the text as it was (or would be) sent, when the prefix or the opt-out line changed it.
func asSent(text, body string) string {
	if body == text {
		return ""
	}
	return "\nAs sent: " + body
}

// sendFailure words a refused POST. Twilio gave the message no SID, so the sentences that name one
// name the recipient instead ("Message to +1555…").
func (p *plugin) sendFailure(err error, to string, snd sender) error {
	var e *apiError
	if !errors.As(err, &e) || e.Code == 0 || e.Status == 401 || e.Status == 403 || e.Status == 429 {
		return err
	}
	if e.Code == 21610 {
		p.st.markOptedOut(p, to, "STOP", p.now())
	}
	return refusal(failureSentence("to "+to, to, snd.label(), "failed", e.Code, e.Message))
}

// failureSentence words a failed or undelivered message by its Twilio error code (§8).
func failureSentence(sid, to, from, status string, code int, message string) string {
	switch code {
	case 21608:
		return fmt.Sprintf("Twilio answered 21608: this Trial account can only text verified numbers. Verify %s in the Twilio Console, or upgrade the account.", to)
	case 30034:
		return fmt.Sprintf("Message %s failed with 30034: %s is not registered for A2P 10DLC, so US carriers block it. Finish registration (ask Texting Assistant \"what's next for registration\").", sid, from)
	case 30032:
		return fmt.Sprintf("Message %s failed with 30032: the toll-free number %s is not verified yet.", sid, from)
	case 21610:
		return fmt.Sprintf("Message %s failed with 21610: %s has opted out (sent STOP). They must text START to receive messages again.", sid, to)
	}
	return fmt.Sprintf("Message %s was %s with %d: %s. %s", sid, status, code, strings.TrimSuffix(strings.TrimSpace(message), "."), errorDocs(code))
}

// checkMedia checks each media URL: https, on mediaUrlHosts in send mode, and a HEAD answering 200
// with an accepted type and a size within Twilio's limits; then the total.
func (p *plugin) checkMedia(urls []string) error {
	if p.cfg.Mode == "send" && len(p.cfg.MediaURLHosts) == 0 {
		return refusal("Refused: MMS sending is off because mediaUrlHosts is empty; a person adds the hosts media may come from. Nothing was sent.")
	}
	var total int64
	for _, raw := range urls {
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || (p.cfg.Mode == "send" && !p.cfg.mediaHostAllowed(u.Hostname())) {
			return refusal(fmt.Sprintf("Refused: %s is not https or its host is not on mediaUrlHosts; nothing was sent.", raw))
		}
		answered := func(what string) error {
			return refusal(fmt.Sprintf("Refused: %s answered %s; Twilio accepts %s and the other types listed in plugin-twilio. Nothing was sent.", raw, what, acceptedTypes))
		}
		req, _ := http.NewRequest(http.MethodHead, raw, nil)
		resp, err := httpClient.Do(req)
		if err != nil {
			return answered("nothing (it could not be reached)")
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return answered(fmt.Sprintf("%d", resp.StatusCode))
		}
		ctype, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
		ctype = strings.ToLower(ctype)
		limit := int64(maxOtherBytes)
		switch {
		case imageTypes[ctype]:
			limit = maxImageBytes
		case otherTypes[ctype]:
		default:
			if ctype == "" {
				ctype = "no Content-Type"
			}
			return answered(ctype)
		}
		if resp.ContentLength < 0 {
			return answered(ctype + " with no Content-Length")
		}
		if resp.ContentLength > limit {
			return answered(fmt.Sprintf("%s of %d bytes, over Twilio's %d KB for that type", ctype, resp.ContentLength, limit>>10))
		}
		total += resp.ContentLength
	}
	if total > maxMediaTotal {
		return refusal(fmt.Sprintf("Refused: media totals %.1f MB, over Twilio's 5 MB per message; nothing was sent.", float64(total)/(1<<20)))
	}
	return nil
}
