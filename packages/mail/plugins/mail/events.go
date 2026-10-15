package main

// The events a command publishes when it changed the mailbox: sent, drafted and changed. Each
// names the message and its subject, never a body, an address or an attachment. A read publishes
// none of them, and neither does a refusal: each is published only after the mailbox took the
// change.

// eventSubject is a subject as an event carries it: one line, cut to 120 runes, as received does.
func eventSubject(s string) string { return cutRunes(oneLine(s), 120) }

// sentPayload is sent for one message the plugin sent. to is how many recipients it went to (To,
// Cc and Bcc), never who.
func sentPayload(account string, o outgoing, mk made) map[string]any {
	return map[string]any{"mailbox": account, "id": mk.ID, "thread": mk.Thread,
		"to": len(o.To) + len(o.Cc) + len(o.Bcc), "subject": eventSubject(o.Subject)}
}

// draftedPayload is drafted for one draft the plugin saved.
func draftedPayload(account string, o outgoing, mk made) map[string]any {
	return map[string]any{"mailbox": account, "id": mk.ID, "subject": eventSubject(o.Subject)}
}

// changedPayload is changed for one message a mark, move or label changed: change is read, unread,
// moved or labelled. action is always changed: none of them makes or removes a message.
func changedPayload(account, id, subject, change string) map[string]any {
	return map[string]any{"mailbox": account, "id": id, "subject": eventSubject(subject), "action": "changed", "change": change}
}

func (m *mailer) publishSent(o outgoing, mk made) {
	m.publish("sent", sentPayload(m.settings.account, o, mk))
}

func (m *mailer) publishDrafted(o outgoing, mk made) {
	m.publish("drafted", draftedPayload(m.settings.account, o, mk))
}

// publishChanged publishes changed for each message of a mark, move or label. renamed holds a moved
// message's new id where the provider gave one.
func (m *mailer) publishChanged(ids []string, subjects, renamed map[string]string, change string) {
	for _, id := range ids {
		now := id
		if n, ok := renamed[id]; ok {
			now = n
		}
		m.publish("changed", changedPayload(m.settings.account, now, subjects[id], change))
	}
}
