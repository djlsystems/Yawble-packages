package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// monitorEvents is a plugin's events with one event, sent, whose monitor is the hint given.
func monitorEvents(hint string) string {
	return `{ "publishes": [ { "type": "sent", "summary": "A message was sent.", "fields": [
    { "name": "to", "kind": "string" },
    { "name": "status", "kind": "string" },
    { "name": "attempts", "kind": "number" },
    { "name": "recipients", "kind": "list" }
  ], "monitor": ` + hint + ` } ] }`
}

func monitorProblems(hint string) []string {
	return checkEvents("hello", []byte(monitorEvents(hint)))
}

// wantRefused checks the hint is refused with a problem naming the plugin, the event and the field.
func wantRefused(t *testing.T, hint, field string) {
	t.Helper()
	wantProblem(t, monitorProblems(hint), "plugin hello event sent: monitor."+field+": ")
}

func TestAGoodMonitorHintIsAccepted(t *testing.T) {
	for _, hint := range []string{
		`{ "icon": "send", "action": "created" }`,
		`{ "icon": "mail", "iconBy": { "field": "status", "icons": { "bounced": "error", "queued": "schedule" } },
		   "actionField": "status", "label": "Sent to {to} after {attempts} tries" }`,
	} {
		if problems := monitorProblems(hint); len(problems) != 0 {
			t.Errorf("%s: %q", hint, problems)
		}
	}
}

func TestNoMonitorIsValid(t *testing.T) {
	if problems := checkEvents("hello", []byte(`{ "publishes": [ { "type": "sent", "fields": [] } ] }`)); len(problems) != 0 {
		t.Fatal(problems)
	}
	if problems := checkEvents("hello", nil); len(problems) != 0 {
		t.Fatal(problems)
	}
}

func TestAMonitorIconMustBeAMaterialSymbol(t *testing.T) {
	wantRefused(t, `{ "icon": "not_a_symbol_at_all", "action": "created" }`, "icon")
	wantRefused(t, `{ "icon": "Send", "action": "created" }`, "icon")
	wantRefused(t, `{ "action": "created" }`, "icon")
}

func TestAnIconByIconMustBeAMaterialSymbol(t *testing.T) {
	wantRefused(t, `{ "icon": "mail", "iconBy": { "field": "status", "icons": { "bounced": "no_such_icon" } }, "action": "created" }`, "iconBy.icons.bounced")
}

func TestAMonitorGivesExactlyOneOfActionAndActionField(t *testing.T) {
	wantRefused(t, `{ "icon": "mail", "action": "created", "actionField": "status" }`, "action")
	wantRefused(t, `{ "icon": "mail" }`, "action")
}

func TestAMonitorActionIsCreatedChangedOrDeleted(t *testing.T) {
	wantRefused(t, `{ "icon": "mail", "action": "added" }`, "action")
}

func TestAnActionFieldMustBeADeclaredStringField(t *testing.T) {
	wantRefused(t, `{ "icon": "mail", "actionField": "missing" }`, "actionField")
	wantRefused(t, `{ "icon": "mail", "actionField": "attempts" }`, "actionField")
}

func TestAnIconByFieldMustBeADeclaredStringField(t *testing.T) {
	wantRefused(t, `{ "icon": "mail", "iconBy": { "field": "recipients", "icons": {} }, "action": "created" }`, "iconBy.field")
	wantRefused(t, `{ "icon": "mail", "iconBy": { "field": "missing", "icons": {} }, "action": "created" }`, "iconBy.field")
}

func TestALabelPlaceholderMustBeADeclaredStringOrNumberField(t *testing.T) {
	wantRefused(t, `{ "icon": "mail", "action": "created", "label": "To {recipients}" }`, "label")
	wantRefused(t, `{ "icon": "mail", "action": "created", "label": "To {missing}" }`, "label")
}

func TestALabelIsAtMost120Characters(t *testing.T) {
	ok := strings.Repeat("é", maxLabel)
	if problems := monitorProblems(`{ "icon": "mail", "action": "created", "label": "` + ok + `" }`); len(problems) != 0 {
		t.Fatal(problems)
	}
	wantRefused(t, `{ "icon": "mail", "action": "created", "label": "`+ok+`x" }`, "label")
}

func TestTheIconListIsTheGeneratedMaterialSymbolsList(t *testing.T) {
	if !strings.Contains(materialSymbolsFile, "# Regenerate: scripts/pkgtool/material-symbols.sh ") {
		t.Fatal("material-symbols-outlined.txt does not say how it was generated")
	}
	if len(materialSymbols) < 3000 || !materialSymbols["home"] || materialSymbols["# Regenerate: scripts/pkgtool/material-symbols.sh"] {
		t.Fatalf("material-symbols-outlined.txt holds %d names; expected the whole Outlined set", len(materialSymbols))
	}
}

// The build and the check both read a plugin's manifest; a bad hint is refused by each.
func TestABadMonitorIsRefusedByTheBuildAndTheCheck(t *testing.T) {
	manifest := strings.Replace(goPlugin, `"connections":`, `"events": `+monitorEvents(`{ "icon": "nope_nope", "action": "created" }`)+`, "connections":`, 1)
	dist := t.TempDir()
	z := buildZip(t, dist, "hello-1.2.3.zip", map[string]string{
		"plugin.json": manifest, "bin/linux-x64/hello*": "x64", "bin/linux-arm64/hello*": "arm64",
	})
	if _, err := readEntry(z); err == nil || !strings.Contains(err.Error(), "plugin hello event sent: monitor.icon") {
		t.Fatalf("want the zip refused for its monitor icon, got %v", err)
	}
	dir := filepath.Join(t.TempDir(), "hello")
	write(t, dir, map[string]string{"plugin.json": manifest})
	if _, err := readPackage(dir); err == nil || !strings.Contains(err.Error(), "plugin hello event sent: monitor.icon") {
		t.Fatalf("want the package refused for its monitor icon, got %v", err)
	}
}
