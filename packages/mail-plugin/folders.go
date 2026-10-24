package main

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// roleNames are the common names of special-use folders. Each provider finds them by role; a
// missing one is never created.
var roleNames = []string{"inbox", "drafts", "draft", "sent", "sent items", "sentitems", "sent mail", "junk", "junk email",
	"junkemail", "spam", "trash", "bin", "deleted", "deleted items", "deleteditems", "outbox", "all", "all mail", "starred", "important"}

func isRoleName(name string) bool { return slices.Contains(roleNames, strings.ToLower(name)) }

// moveToEntry is target as the person wrote it in moveTo.
func (m *mailer) moveToEntry(target string) string {
	for _, f := range m.settings.moveTo {
		if strings.EqualFold(f, target) {
			return f
		}
	}
	return target
}

// ensureFolder creates the folder or label move or label needs when the mailbox does not have it,
// and answers its name when it did. target is already on moveTo. A special-use name is left for
// the provider to find by role or fail on, as is a label the provider keeps as a mark on the
// message rather than a folder. A nested name is used only when it exists.
func (m *mailer) ensureFolder(ctx context.Context, cmd, target string) (string, error) {
	if cmd == "label" {
		uses, err := m.p.labelUsesFolder(ctx)
		if err != nil || !uses {
			return "", err
		}
	}
	found, err := m.p.hasFolder(ctx, target)
	if err != nil || found || isRoleName(target) {
		return "", err
	}
	name := m.moveToEntry(target)
	_, sep, err := m.p.folders(ctx)
	if err != nil {
		return "", err
	}
	if sep != "" && strings.Contains(name, sep) {
		return "", failure("Refused: Mailer creates only top-level folders, so a person creates the nested folder " + strconv.Quote(name) + " in their mail app; nothing was changed.")
	}
	m.progress("creating " + strconv.Quote(name))
	if err := m.p.createFolder(ctx, name); err != nil {
		return "", err
	}
	return name, nil
}

// folders lists the mailbox's folders, then says for each moveTo entry whether it exists, is
// created on first use, or must be made by the person. It changes nothing.
func (m *mailer) folders(ctx context.Context) (string, error) {
	m.progress("listing folders")
	all, sep, err := m.p.folders(ctx)
	if err != nil {
		return "", err
	}
	kind := "Folders"
	if m.p.name() == "Gmail" {
		kind = "Labels"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s in this mailbox (%d), with the special-use role where one has it:", kind, len(all))
	for _, f := range all {
		b.WriteString("\n" + oneLine(f.Name))
		if f.Role != "" {
			b.WriteString(" (" + f.Role + ")")
		}
	}
	if len(m.settings.moveTo) == 0 {
		b.WriteString("\nmoveTo is empty: move and label are off.")
		return b.String(), nil
	}
	b.WriteString("\nmoveTo:")
	for _, name := range m.settings.moveTo {
		found, err := m.p.hasFolder(ctx, name)
		if err != nil {
			return "", err
		}
		mark := "created on first use"
		switch {
		case found:
			mark = "exists"
		case isRoleName(name):
			mark = "missing: Mailer never creates a special-use folder"
		case sep != "" && strings.Contains(name, sep):
			mark = "nested: create it in your mail app"
		}
		b.WriteString("\n" + name + ": " + mark)
	}
	return b.String(), nil
}
