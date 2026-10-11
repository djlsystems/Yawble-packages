package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

var (
	optOutKeywords = set("STOP", "STOPALL", "UNSUBSCRIBE", "CANCEL", "END", "QUIT", "REVOKE", "OPTOUT")
	optInKeywords  = set("START", "UNSTOP", "YES")
	helpKeywords   = set("HELP", "INFO")
)

const autoReplyTag = "auto-reply"

// A published payload must stay under the Host's excerpt limit (4,000 characters by default).
const payloadBudget = 3500

const setupRefresh = time.Hour

// ---- check ------------------------------------------------------------------------------------

type step struct {
	Step  string `json:"step"`
	State string `json:"state"`
	Text  string `json:"text"`
}

func (s step) line() string { return s.State + ": " + s.Text }

// check runs every setup check in order and prints the checklist (§11.2); it also keeps the result
// in state/setup for the twilio page.
func (p *plugin) check() (string, error) {
	steps := p.setupSteps()
	var lines []string
	for _, s := range steps {
		lines = append(lines, s.line())
	}
	if p.st.missing {
		lines = append(lines, "Next: site — "+siteMissing)
	}
	return strings.Join(lines, "\n"), nil
}

func (p *plugin) setupSteps() []step {
	var steps []step
	add := func(name, state, text string) { steps = append(steps, step{name, state, text}) }

	// 1. Secrets, by key name only: set or missing, and the last 4 characters of the SIDs.
	keys := []struct{ key, value string }{
		{"TWILIO_ACCOUNT_SID", p.sec.AccountSid}, {"TWILIO_API_KEY_SID", p.sec.APIKeySid}, {"TWILIO_API_KEY_SECRET", p.sec.APIKeySecret},
	}
	var set, missing []string
	for _, k := range keys {
		switch {
		case k.value == "":
			missing = append(missing, k.key)
		case k.key == "TWILIO_API_KEY_SECRET":
			set = append(set, k.key+" set")
		default:
			set = append(set, fmt.Sprintf("%s set (%s)", k.key, last4(k.value)))
		}
	}
	network := len(missing) == 0
	if network {
		add("secrets", "Done", "secrets — "+strings.Join(set, ", "))
	} else {
		add("secrets", "Next", fmt.Sprintf("secrets — %s missing; the operator sets each with the secret command on the machine that runs the platform (yawble secret set %s), never in chat, a page or settings. The values are on %s",
			strings.Join(missing, ", "), strings.Join(missing, ", yawble secret set "), consoleKeys))
		add("credentials", "Next", "credentials — set the missing secrets, then run check again")
	}

	// 2-3. Credentials and account type.
	if network {
		a, err := p.account()
		if err != nil {
			add("credentials", "Next", "credentials — "+err.Error())
			network = false
		} else {
			add("credentials", "Done", fmt.Sprintf("credentials — the API key signs in to account %s", last4(p.sec.AccountSid)))
			if strings.EqualFold(a.Type, "Trial") {
				add("account", "Next", fmt.Sprintf("account type — Trial (status %s): texts reach only verified numbers and carry a trial prefix; to upgrade, press Upgrade at the top of the Twilio Console; billing overview: %s", a.Status, consoleUpgrade))
			} else {
				add("account", "Done", fmt.Sprintf("account type — %s, status %s", a.Type, a.Status))
			}
		}
	}

	// 4-6. Number, capabilities, registration.
	if network {
		snd, err := p.resolveSender()
		switch {
		case err != nil && strings.HasPrefix(err.Error(), "Twilio has no sender"):
			add("number", "Next", "buy a number — the account has no SMS-capable number; buy one (toll-free recommended): "+consoleBuy)
		case err != nil && strings.Contains(err.Error(), "fromNumber is empty"):
			ns, _ := p.smsNumbers()
			add("number", "Next", fmt.Sprintf("choose fromNumber — the account has %d SMS-capable numbers (%s); a person sets fromNumber to one in this member's settings", len(ns), numberList(ns)))
		case err != nil:
			add("number", "Next", "number — "+err.Error())
		default:
			num, err := p.numberRecord(&snd)
			switch {
			case err != nil:
				add("number", "Next", "number — "+err.Error())
			case num == nil:
				add("number", "Done", "number — sending through Messaging Service "+snd.service)
			default:
				source := "fromNumber"
				if p.cfg.FromNumber == "" {
					source = "the account's only SMS-capable number"
				}
				add("number", "Done", fmt.Sprintf("number — %s, %s (%s)", num.PhoneNumber, numberType(num.PhoneNumber), source))
				if num.Capabilities.SMS {
					add("capabilities", "Done", fmt.Sprintf("capabilities — sms=%t mms=%t", num.Capabilities.SMS, num.Capabilities.MMS))
				} else {
					add("capabilities", "Next", fmt.Sprintf("capabilities — %s cannot send SMS (sms=false); choose an SMS-capable number: %s", num.PhoneNumber, consoleNumbers))
				}
			}
			if lines, err := p.registrationLines(&snd); err != nil {
				add("registration", "Next", "registration — "+err.Error())
			} else {
				for _, l := range lines {
					add("registration", l.state, l.text)
				}
			}
		}
	}

	// 7-8. Mode and allowlist.
	if p.cfg.Mode == "send" {
		add("mode", "Done", "mode — send: texts to sendAllowlist numbers are sent for real")
	} else {
		add("mode", "Done", "mode — dryRun: sends are checked and reported, never made; a person sets mode to send for real texts")
	}
	if len(p.cfg.SendAllowlist) == 0 {
		add("allowlist", "Next", "allowlist — sendAllowlist is empty; a person adds their own cell in this member's settings")
	} else {
		add("allowlist", "Done", "allowlist — "+strings.Join(p.cfg.SendAllowlist, ", "))
	}

	// 9. What every text carries for the campaign: the business name and the opt-out line.
	state, text := p.complianceStep()
	add("compliance", state, text)

	// 10. The auto-reply, when it is on.
	if p.cfg.AutoReply {
		if p.cfg.AutoReplyText == "" {
			add("auto-reply", "Next", "auto-reply — autoReply is on but autoReplyText is empty, so no text is answered; a person sets autoReplyText in this member's settings")
		} else {
			add("auto-reply", "Done", fmt.Sprintf("auto-reply — on: a new text is answered with %q, at most once in %d hours to each number, through every check a send has", p.cfg.AutoReplyText, p.cfg.AutoReplyOncePerHours))
		}
	}

	// 11. Owner commands, when they are on.
	if p.cfg.OwnerCommands {
		switch {
		case len(p.cfg.OwnerKeywords) == 0:
			add("owner commands", "Next", "owner commands — ownerCommands is on but ownerKeywords is empty, so no text is a command; a person sets ownerKeywords in this member's settings")
		case len(p.cfg.owners()) == 0:
			add("owner commands", "Next", "owner commands — ownerCommands is on but ownerNumbers and sendAllowlist are both empty, so no text is a command; a person sets ownerNumbers in this member's settings")
		default:
			add("owner commands", "Done", fmt.Sprintf("owner commands — on: a text from %s that is only %s publishes plugin.twilio.command with that keyword", strings.Join(p.cfg.owners(), ", "), strings.Join(p.cfg.OwnerKeywords, ", ")))
		}
	}

	// 12. The first real message to the person's own phone.
	state, text = p.firstMessageStep()
	add("first message", state, text)

	p.st.setup = map[string]any{"checkedAt": p.now().UTC().Format(time.RFC3339), "steps": steps, "firstDelivered": p.st.setup["firstDelivered"]}
	p.st.setupDirty = true
	return steps
}

// complianceStep says what every text carries, so a person can hold it against the sample messages
// the campaign registered, and what it costs: the characters it adds, and so the longest GSM-7 text
// that still goes as one segment.
func (p *plugin) complianceStep() (string, string) {
	var said, missing []string
	added := 0
	if p.cfg.MessagePrefix != "" {
		said = append(said, fmt.Sprintf("every text starts with %q (unless it already starts with that name)", p.cfg.MessagePrefix))
		added += utf8.RuneCountInString(p.cfg.MessagePrefix)
	} else {
		missing = append(missing, `messagePrefix is empty: carriers expect the business name the campaign registered in each text, such as "Acme Plumbing: "`)
	}
	switch p.cfg.OptOutFooter {
	case "every":
		said = append(said, fmt.Sprintf("every text ends with %q", optOutFooter))
		added += 1 + utf8.RuneCountInString(optOutFooter)
	case "first":
		said = append(said, fmt.Sprintf("the first text to each number ends with %q", optOutFooter))
	default:
		missing = append(missing, `optOutFooter is off: no text says how to opt out, which a campaign's messages are expected to`)
	}
	if added > 0 {
		said = append(said, fmt.Sprintf("that adds %d characters, so a GSM-7 text of up to %d characters stays one segment", added, 160-added))
	}
	if p.cfg.PrivacyURL != "" {
		said = append(said, "privacy policy "+p.cfg.PrivacyURL)
	}
	if p.cfg.TermsURL != "" {
		said = append(said, "terms "+p.cfg.TermsURL)
	}
	if len(missing) > 0 {
		return "Next", "compliance — " + strings.Join(append(missing, said...), "; ") + "; a person sets these in this member's settings"
	}
	return "Done", "compliance — " + strings.Join(said, "; ") + "; they should match the sample messages the campaign registered"
}

func (p *plugin) firstMessageStep() (string, string) {
	if fd, ok := p.st.setup["firstDelivered"].(map[string]any); ok {
		return "Done", fmt.Sprintf("first message delivered to %v", fd["to"])
	}
	var latest *messageDoc
	for _, m := range p.st.messages {
		if m.Test && (latest == nil || m.Created > latest.Created) {
			latest = m
		}
	}
	switch {
	case latest == nil:
		return "Next", "first message — send one text to your own cell: a dry run first, then a person sets mode to send"
	case latest.Status == "failed" || latest.Status == "undelivered":
		return "Next", "first message — " + failureSentence(latest.Sid, latest.To, latest.From, latest.Status, latest.ErrorCode, latest.ErrorMessage)
	}
	return "Waiting", fmt.Sprintf("first message — %s to %s is %s since %s; the next poll reads delivery, typically within minutes", latest.Sid, latest.To, latest.Status, latest.Created)
}

// ---- poll -------------------------------------------------------------------------------------

func (p *plugin) poll() outcome {
	if err := p.needSite(); err != nil {
		return outcome{err: err}
	}
	now := p.now().UTC()
	if err := p.needSecrets(); err != nil {
		// Before the operator has set the secrets the schedule still runs; it fills the checklist
		// and wakes nobody.
		p.setupSteps()
		return outcome{text: "Poll skipped: " + err.Error(), quiet: true}
	}

	var failures []string
	stepFailed := func(what string, err error) {
		failures = append(failures, what+": "+err.Error())
		p.progress("poll: " + what + " failed: " + err.Error())
	}

	// 1. Delivery status of recent outbound messages that are not final.
	changes, delivered, autoChanges := 0, 0, 0
	var failed []map[string]any
	testDelivered := false
	var pending []*messageDoc
	for _, m := range p.st.messages {
		created, err := time.Parse(time.RFC3339, m.Created)
		if m.Direction == "out" && !m.DryRun && !finalStatus(m.Status) && (err != nil || now.Sub(created) <= pollLookback) {
			pending = append(pending, m)
		}
	}
	sort.Slice(pending, func(i, j int) bool { return pending[i].Created < pending[j].Created })
	statusOK := true
	for _, m := range pending {
		msg, err := p.api.fetchMessage(m.Sid)
		if err != nil {
			stepFailed("reading delivery status", err)
			statusOK = false
			break
		}
		doc, changed := p.recordStatus(msg)
		if !changed {
			continue
		}
		changes++
		if doc.Tag == autoReplyTag && msg.Status != "failed" && msg.Status != "undelivered" && msg.Status != "canceled" {
			autoChanges++
		}
		switch msg.Status {
		case "delivered":
			delivered++
			if doc.Test && p.st.setup["firstDelivered"] == nil {
				if p.st.setup == nil {
					p.st.setup = map[string]any{}
				}
				p.st.setup["firstDelivered"] = map[string]any{"sid": doc.Sid, "to": doc.To, "at": now.Format(time.RFC3339)}
				p.st.setupDirty = true
				testDelivered = true
			}
		case "failed", "undelivered", "canceled":
			failed = append(failed, map[string]any{
				"sid": msg.Sid, "to": msg.To, "status": msg.Status, "error_code": msg.code(),
				"error": failureSentence(msg.Sid, msg.To, msg.From, msg.Status, msg.code(), deref(msg.ErrorMessage)),
			})
		}
	}

	// 2-3. New inbound texts, with the opt-out and opt-in keywords.
	var received, optouts, optins, commands []map[string]any
	var answerable []message
	replied := map[string]bool{}
	inboundOK := true
	newInbound := 0
	ours, err := p.ourNumbers()
	if err != nil {
		stepFailed("listing inbound texts", err)
		inboundOK = false
	} else {
		lookback := pollLookback
		from := now.Add(-24 * time.Hour)
		if last, err := time.Parse(time.RFC3339, p.st.poll.LastPollAt); err == nil && last.Add(-time.Hour).Before(from) {
			from = last.Add(-time.Hour)
		}
		if p.st.poll.LastPollAt == "" || from.Before(now.Add(-lookback)) {
			from = now.Add(-lookback)
		}
		var inbound []message
		for _, our := range ours {
			got, err := p.api.listMessages(url.Values{"To": {our}, "DateSent>": {from.Format("2006-01-02")}}, 1000)
			if err != nil {
				stepFailed("listing inbound texts", err)
				inboundOK = false
				break
			}
			for _, m := range got {
				if m.out() || p.st.poll.InboundSeen[m.Sid] != "" {
					continue
				}
				if t, ok := twilioTime(m.date()); ok && t.Before(from) {
					continue
				}
				inbound = append(inbound, m)
			}
		}
		sort.SliceStable(inbound, func(i, j int) bool { return inbound[i].date() < inbound[j].date() })
		for _, m := range inbound {
			newInbound++
			p.st.poll.InboundSeen[m.Sid] = m.date()
			p.st.pollDirty = true
			p.recordStatus(m)
			at, _ := twilioTime(m.date())
			word := strings.ToUpper(strings.TrimSpace(m.Body))
			command := p.ownerCommand(m)
			switch {
			case optOutKeywords[word]:
				if p.st.markOptedOut(p, m.From, word, at) {
					optouts = append(optouts, map[string]any{"number": m.From, "keyword": word, "date": m.date()})
				}
			case optInKeywords[word]:
				if _, out := p.st.optedOut(m.From); out {
					optins = append(optins, map[string]any{"number": m.From, "keyword": word, "date": m.date()})
					// Twilio lifts its own block on START. A number with no consented entry (the
					// person's own cell) may be texted again; a consented entry stays opted-out
					// until a person re-activates it on the page.
					if p.st.recipient(m.From) == nil {
						delete(p.st.poll.OptedOut, m.From)
					}
				} else {
					received = append(received, inboundItem(m))
				}
			case command != "":
				// The event names the keyword only, never the rest of the text.
				commands = append(commands, map[string]any{"command": command, "from": m.From, "at": m.date(), "messageSid": m.Sid})
			default:
				received = append(received, inboundItem(m))
				// HELP and INFO are Twilio's to answer, like STOP and START.
				if !helpKeywords[word] {
					answerable = append(answerable, m)
				}
			}
		}
		replied = p.autoReply(answerable, now)
		keep := seenKept
		if lookback+24*time.Hour > keep {
			keep = lookback + 24*time.Hour
		}
		for sid, d := range p.st.poll.InboundSeen {
			if t, err := time.Parse(time.RFC3339, d); err != nil || now.Sub(t) > keep {
				delete(p.st.poll.InboundSeen, sid)
				p.st.pollDirty = true
			}
		}
	}

	p.st.prune(p, now)

	// 4. Events: at most one of each type.
	published := false
	if len(received) > 0 {
		number := strings.Join(ours, ", ")
		payload := func(items []map[string]any) map[string]any {
			out := map[string]any{"number": number, "count": len(items), "messages": items, "more": len(received) - len(items)}
			if p.cfg.AutoReply {
				out["autoReplied"] = len(replied)
			}
			return out
		}
		for _, item := range received {
			if replied[item["sid"].(string)] {
				item["autoReplied"] = true
			}
		}
		items, _ := fit(received, payload)
		p.publish("received", payload(items))
		published = true
	}
	if len(optouts) > 0 {
		items, _ := fit(optouts, func(items []map[string]any) map[string]any {
			return map[string]any{"count": len(optouts), "numbers": items}
		})
		p.publish("optout", map[string]any{"count": len(optouts), "numbers": items})
		published = true
	}
	if len(optins) > 0 {
		items, _ := fit(optins, func(items []map[string]any) map[string]any {
			return map[string]any{"count": len(optins), "numbers": items}
		})
		p.publish("optin", map[string]any{"count": len(optins), "numbers": items})
		published = true
	}
	// One event per owner command. It wakes its own trigger, so a poll whose only news is commands
	// stays quiet.
	for _, c := range commands {
		p.publish("command", c)
	}
	if len(failed) > 0 || testDelivered {
		items, _ := fit(failed, func(items []map[string]any) map[string]any {
			return map[string]any{"delivered": delivered, "failed": items}
		})
		if items == nil {
			items = []map[string]any{}
		}
		p.publish("delivery", map[string]any{"delivered": delivered, "failed": items})
		published = true
	}

	// The checklist page fills from the first poll and stays fresh.
	if checked, err := time.Parse(time.RFC3339, fmt.Sprint(p.st.setup["checkedAt"])); err != nil || now.Sub(checked) > setupRefresh || testDelivered {
		p.setupSteps()
	}

	p.st.poll.LastPollAt = now.Format(time.RFC3339)
	p.st.pollDirty = true

	if !statusOK && !inboundOK {
		return outcome{err: refusal("Poll failed: " + strings.Join(failures, " "))}
	}
	text := fmt.Sprintf("Poll: %d status change(s) (%d delivered, %d failed), %d new inbound text(s), %d opt-out(s), %d opt-in(s).",
		changes, delivered, len(failed), newInbound, len(optouts), len(optins))
	if p.cfg.AutoReply {
		text += fmt.Sprintf(" Auto-replied to %d.", len(replied))
	}
	if p.cfg.OwnerCommands {
		text += fmt.Sprintf(" %d owner command(s).", len(commands))
	}
	if len(failures) > 0 {
		text += " Not done: " + strings.Join(failures, " ")
	}
	// A poll whose only news is how its own auto-replies went, or owner commands, stays quiet.
	quiet := !published && changes-autoChanges == 0 && newInbound-len(commands) == 0 && len(failures) == 0
	if quiet && len(commands) == 0 {
		text = "Nothing new."
	}
	return outcome{text: text, quiet: quiet}
}

// autoReply answers new texts with autoReplyText through the send path, at most once in
// autoReplyOncePerHours to each number, and returns the SIDs it answered. A text it does not answer
// is counted by reason in one progress line, which names no number and quotes no text.
func (p *plugin) autoReply(texts []message, now time.Time) map[string]bool {
	replied := map[string]bool{}
	if !p.cfg.AutoReply || p.cfg.AutoReplyText == "" || len(texts) == 0 {
		return replied
	}
	window := time.Duration(p.cfg.AutoReplyOncePerHours) * time.Hour
	skipped := map[string]int{}
	total := 0
	for _, m := range texts {
		if last, err := time.Parse(time.RFC3339, p.st.poll.AutoRepliedAt[m.From]); err == nil && now.Sub(last) < window {
			skipped[fmt.Sprintf("answered in the last %d hours", p.cfg.AutoReplyOncePerHours)]++
			total++
			continue
		}
		if _, err := p.deliver(sendRequest{kind: "SMS", to: m.From, body: p.cfg.AutoReplyText}, m.Sid); err != nil {
			var s skip
			var r refusal
			switch {
			case errors.As(err, &s):
				skipped[s.why]++
			case errors.As(err, &r):
				skipped["refused by the send checks or Twilio"]++
			default:
				skipped["Twilio could not be reached"]++
			}
			total++
			continue
		}
		replied[m.Sid] = true
		p.st.poll.AutoRepliedAt[m.From] = now.Format(time.RFC3339)
		p.st.pollDirty = true
	}
	for number, at := range p.st.poll.AutoRepliedAt {
		if t, err := time.Parse(time.RFC3339, at); err != nil || now.Sub(t) > window {
			delete(p.st.poll.AutoRepliedAt, number)
			p.st.pollDirty = true
		}
	}
	line := fmt.Sprintf("poll: auto-reply answered %d text(s)", len(replied))
	if p.cfg.Mode != "send" {
		line += " as a dry run"
	}
	if total > 0 {
		var reasons []string
		for why, n := range skipped {
			reasons = append(reasons, fmt.Sprintf("%d %s", n, why))
		}
		sort.Strings(reasons)
		line += fmt.Sprintf(", skipped %d: %s", total, strings.Join(reasons, ", "))
	}
	p.progress(line + ".")
	return replied
}

// ownerCommand is the keyword an inbound text is, or "": with ownerCommands on, from an owner
// number, the whole body trimmed, in any case and without trailing ? ! or ., equal to one of
// ownerKeywords. An opt-out, opt-in or help keyword is never a command.
func (p *plugin) ownerCommand(m message) string {
	if !p.cfg.OwnerCommands || !p.cfg.isOwner(m.From) {
		return ""
	}
	word := strings.ToLower(strings.TrimSpace(strings.TrimRight(strings.TrimSpace(m.Body), "?!.")))
	if up := strings.ToUpper(word); optOutKeywords[up] || optInKeywords[up] || helpKeywords[up] {
		return ""
	}
	for _, k := range p.cfg.OwnerKeywords {
		if word == k {
			return k
		}
	}
	return ""
}

func inboundItem(m message) map[string]any {
	return map[string]any{"sid": m.Sid, "from": m.From, "date": m.date(), "snippet": untrusted(snippet(m.Body, 160)), "media": m.media()}
}

// fit keeps as many items as fit the payload budget, in order; it returns them and how many did
// not fit.
func fit(items []map[string]any, payload func([]map[string]any) map[string]any) ([]map[string]any, int) {
	n := len(items)
	for n > 0 {
		raw, _ := json.Marshal(payload(items[:n]))
		if len(raw) <= payloadBudget {
			break
		}
		n--
	}
	return items[:n], len(items) - n
}
