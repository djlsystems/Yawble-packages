package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

// testdata/yawble-ai-contents.json is each package's contents as the live yawble.ai catalog served
// them (https://yawble.ai/api/marketplace/catalog.json, generatedAt in the file), which yawble.ai
// derives from the uploaded zip (Yawble-web marketplace/admin/package.ts). testdata/yawble-ai-zips/
// <id>-<version>/ holds the manifests of the zip that catalog downloads for each of them
// (solution.json and plugins/<id>/plugin.json, or plugin.json), checked against its sha256. Re-record
// both from that address when a published version's contents change.
type liveContents struct {
	Source      string `json:"source"`
	GeneratedAt string `json:"generatedAt"`
	Packages    map[string]struct {
		Kind     string          `json:"kind"`
		Version  string          `json:"version"`
		Contents json.RawMessage `json:"contents"`
	} `json:"packages"`
}

// sourceContents is what the catalog's contents would be for the package folder dir, read from the
// manifests the build copies into its zip as they are: plugin.json at a plugin package's root, or
// solution.json and the plugin.json of each plugin the solution ships (plugins/<id>/), wherever in
// packages/ that plugin's source is.
func sourceContents(t *testing.T, packages, dir string) (kind, id, version string, c contents) {
	t.Helper()
	p, err := readPackage(dir)
	if err != nil {
		t.Fatal(err)
	}
	if p.Kind == "plugin" {
		c, err = pluginContents(readRaw(t, filepath.Join(dir, "plugin.json")))
		if err != nil {
			t.Fatal(err)
		}
		return p.Kind, p.ID, p.Version, c
	}
	shipped, err := solutionPlugins(packages, dir)
	if err != nil {
		t.Fatal(err)
	}
	var manifests []json.RawMessage
	for _, s := range shipped {
		manifests = append(manifests, readRaw(t, filepath.Join(s.manifest, "plugin.json")))
	}
	c, err = solutionContents(readRaw(t, filepath.Join(dir, "solution.json")), manifests)
	if err != nil {
		t.Fatal(err)
	}
	return p.Kind, p.ID, p.Version, c
}

func readRaw(t *testing.T, path string) json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func asJSON(t *testing.T, v any) any {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// pkgtool reads the zips yawble.ai serves to exactly the contents yawble.ai derived from them: each
// published version of this repository's packages, as uploaded. It compares every package the
// recorded catalog lists, so a fixture that lost one fails.
func TestContentsMatchYawbleAI(t *testing.T) {
	live := readLive(t)
	for id, want := range live.Packages {
		dir := filepath.Join("testdata", "yawble-ai-zips", id+"-"+want.Version)
		var got contents
		var err error
		if want.Kind == "plugin" {
			got, err = pluginContents(readRaw(t, filepath.Join(dir, "plugin.json")))
		} else {
			// The zip's plugins/<id>/ in the order the zip holds them, as yawble.ai reads them.
			shipped, _ := filepath.Glob(filepath.Join(dir, "plugins", "*", "plugin.json"))
			sort.Strings(shipped)
			var manifests []json.RawMessage
			for _, m := range shipped {
				manifests = append(manifests, readRaw(t, m))
			}
			got, err = solutionContents(readRaw(t, filepath.Join(dir, "solution.json")), manifests)
		}
		if err != nil {
			t.Fatalf("%s %s: %v", id, want.Version, err)
		}
		compareContents(t, id, want.Version, got, want.Contents)
	}
	if len(live.Packages) == 0 {
		t.Fatal("testdata/yawble-ai-contents.json lists no package")
	}
}

// The contents pkgtool writes for this repository's own packages are what yawble.ai serves for the
// same id and version. A package at an id or version yawble.ai does not have yet is not compared: it
// is new, and is published after this check passes.
func TestContentsOfPublishedVersionsMatchYawbleAI(t *testing.T) {
	live := readLive(t)
	packages, err := filepath.Abs(filepath.Join("..", "..", "packages"))
	if err != nil {
		t.Fatal(err)
	}
	dirs, err := os.ReadDir(packages)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range dirs {
		if !d.IsDir() || d.Name()[0] == '.' {
			continue
		}
		kind, id, version, got := sourceContents(t, packages, filepath.Join(packages, d.Name()))
		want, ok := live.Packages[id]
		if !ok || want.Version != version || want.Kind != kind {
			t.Logf("%s %s %s: yawble.ai serves no such version yet (%s); not compared", kind, id, version, live.GeneratedAt)
			continue
		}
		compareContents(t, id, version, got, want.Contents)
	}
}

func readLive(t *testing.T) liveContents {
	t.Helper()
	var live liveContents
	if err := json.Unmarshal(readRaw(t, filepath.Join("testdata", "yawble-ai-contents.json")), &live); err != nil {
		t.Fatal(err)
	}
	return live
}

func compareContents(t *testing.T, id, version string, got contents, want json.RawMessage) {
	t.Helper()
	var wantContents any
	if err := json.Unmarshal(want, &wantContents); err != nil {
		t.Fatal(err)
	}
	if g := asJSON(t, got); !reflect.DeepEqual(g, wantContents) {
		gj, _ := json.MarshalIndent(g, "", "  ")
		wj, _ := json.MarshalIndent(wantContents, "", "  ")
		t.Errorf("%s %s: pkgtool's contents differ from yawble.ai's\npkgtool:\n%s\nlive catalog:\n%s", id, version, gj, wj)
	}
}

// Yawble-web's own fixture for contents (test/marketplace/contents-fixtures.ts): a solution with a
// pluginMinimums entry, a member's own setting, a bound key, labels and a secret marked not masked,
// read to exactly the contents yawble.ai's test expects.
func TestContentsAsYawbleWebReadsThem(t *testing.T) {
	inbox := `{
	  "schemaVersion": 1, "id": "inbox", "name": "Inbox", "version": "2.3.1",
	  "description": "Watches a mailbox. It never sends.", "protocol": "harness.member/1",
	  "executable": { "path": "main.py" }, "requires": ["python3"],
	  "config": {
	    "folder": { "type": "string", "label": "Folder to watch", "default": "INBOX", "required": true, "description": "The folder." },
	    "moveTo": { "type": "list", "default": ["Junk", "Later"], "description": "Where it may move mail." }
	  },
	  "secrets": {
	    "appPassword": { "label": "App password", "required": true, "description": "The app password." },
	    "relayHost": { "masked": false, "description": "The relay." }
	  },
	  "connections": { "mailbox": { "description": "The mailbox.", "providers": ["imap", "google"], "required": true } },
	  "events": { "publishes": [
	    { "type": "received", "summary": "New mail arrived.", "fields": [] },
	    { "type": "answered", "summary": "A followed message was answered.", "fields": [] }
	  ] },
	  "skills": ["skills/inbox.md"]
	}`
	texter := `{
	  "schemaVersion": 1, "id": "texter", "name": "Texter", "version": "1.2.0", "description": "Sends texts.",
	  "protocol": "harness.member/1", "executable": { "path": "main.py" }, "requires": ["python3"],
	  "config": { "toNumber": { "type": "string", "label": "Your number", "description": "Where texts go." } },
	  "secrets": { "token": { "label": "Account token", "required": true, "description": "The token." } },
	  "skills": ["skills/texter.md"]
	}`
	desk := `{
	  "format": 1, "id": "front-desk", "name": "Front Desk", "version": "1.0.0",
	  "description": "Answers the front desk. It texts you about new mail.\n\nIt files nothing.",
	  "pluginMinimums": { "inbox": "2.1.0" },
	  "members": [
	    { "name": "Manager", "role": "manager", "instructions": "Run the desk." },
	    { "name": "Inbox watcher", "pluginId": "inbox" },
	    { "name": "Texts", "pluginId": "texter", "settings": { "toNumber": "5551234567" }, "secrets": { "token": "TEXTER_TOKEN" } }
	  ],
	  "triggers": [
	    { "name": "Check mail", "kind": "schedule", "schedule": { "everySeconds": 300 }, "member": "Inbox watcher", "instruction": "watch" },
	    { "name": "Resume arrives", "kind": "folder", "folder": { "path": "Resume", "glob": "*" }, "member": "Manager", "instruction": "Read it." }
	  ],
	  "skills": ["desk-playbook"], "sites": ["desk"],
	  "inputs": {
	    "connections": [{ "member": "Inbox watcher", "slot": "mailbox", "description": "Your mailbox.", "required": true }],
	    "settings": [{ "member": "Texts", "setting": "toNumber", "description": "Your cell.", "required": true }],
	    "documents": []
	  }
	}`
	wantDesk := `{
	  "plugins": [{ "id": "inbox", "version": "2.3.1", "minimum": "2.1.0" }, { "id": "texter", "version": "1.2.0", "minimum": "1.2.0" }],
	  "members": [
	    { "name": "Manager", "role": "manager", "agent": true },
	    { "name": "Inbox watcher", "role": "inbox", "agent": false },
	    { "name": "Texts", "role": "texter", "agent": false }
	  ],
	  "skills": ["desk-playbook"],
	  "triggers": [{ "name": "Check mail", "words": "every 5 minutes" }, { "name": "Resume arrives", "words": "when a file arrives in Resume/" }],
	  "sites": ["desk"],
	  "settings": [{ "name": "toNumber", "label": "Your number", "kind": "string", "required": true, "default": "5551234567" }],
	  "secrets": [{ "name": "TEXTER_TOKEN", "label": "Account token", "required": true, "masked": true }],
	  "connections": [{ "slot": "mailbox", "providers": ["imap", "google"], "required": true }],
	  "events": []
	}`
	wantInbox := `{
	  "plugins": [], "members": [], "skills": ["inbox"], "triggers": [], "sites": [],
	  "settings": [
	    { "name": "folder", "label": "Folder to watch", "kind": "string", "required": true, "default": "INBOX" },
	    { "name": "moveTo", "label": "", "kind": "list", "required": false, "default": "Junk, Later" }
	  ],
	  "secrets": [
	    { "name": "appPassword", "label": "App password", "required": true, "masked": true },
	    { "name": "relayHost", "label": "", "required": false, "masked": false }
	  ],
	  "connections": [{ "slot": "mailbox", "providers": ["imap", "google"], "required": true }],
	  "events": [{ "type": "received", "summary": "New mail arrived." }, { "type": "answered", "summary": "A followed message was answered." }]
	}`
	same := func(name string, got contents, want string) {
		t.Helper()
		var w any
		if err := json.Unmarshal([]byte(want), &w); err != nil {
			t.Fatal(err)
		}
		if g := asJSON(t, got); !reflect.DeepEqual(g, w) {
			gj, _ := json.MarshalIndent(g, "", "  ")
			t.Errorf("%s contents:\n%s", name, gj)
		}
	}
	got, err := solutionContents(json.RawMessage(desk), []json.RawMessage{json.RawMessage(inbox), json.RawMessage(texter)})
	if err != nil {
		t.Fatal(err)
	}
	same("front-desk", got, wantDesk)
	got, err = pluginContents(json.RawMessage(inbox))
	if err != nil {
		t.Fatal(err)
	}
	same("inbox", got, wantInbox)
}

func TestTriggerInWords(t *testing.T) {
	for raw, want := range map[string]string{
		`{"kind":"schedule","schedule":{"everySeconds":60}}`:                                                   "every minute",
		`{"kind":"schedule","schedule":{"everySeconds":7200}}`:                                                 "every 2 hours",
		`{"kind":"schedule","schedule":{"everySeconds":90}}`:                                                   "every 90 seconds",
		`{"kind":"schedule","schedule":{"cron":"0 30 7 * * 1-5"}}`:                                             "Monday to Friday at 07:30 (UTC)",
		`{"kind":"schedule","schedule":{"cron":"0 0 9 * * 1,3","timezone":"Europe/London"}}`:                   "on Mondays and Wednesdays at 09:00 (Europe/London)",
		`{"kind":"schedule","schedule":{"cron":"0 0 9 * * *"}}`:                                                "every day at 09:00 (UTC)",
		`{"kind":"schedule","schedule":{"cron":"0 */5 * * * *"}}`:                                              "on the schedule 0 */5 * * * * (UTC)",
		`{"kind":"folder","folder":{"path":"In/","glob":"*.pdf"}}`:                                             "when a file matching *.pdf arrives in In/",
		`{"kind":"event","event":{"type":"plugin.twilio.command","filter":"command eq summary"}}`:              "when the twilio plugin announces command with command is summary",
		`{"kind":"event","event":{"type":"plugin.twilio-plugin.command","filter":"command eq summary"}}`:       "when the twilio-plugin plugin announces command with command is summary",
		`{"kind":"event","event":{"type":"plugin.twilio-plugin.received","filter":"source contains /twilio"}}`: "when the twilio-plugin plugin announces received with source containing /twilio",
		`{"kind":"event","event":{"type":"site.action","filter":"siteAction eq desk/approve"}}`:                "when someone presses approve on the desk site",
		`{"kind":"event","event":{"type":"workflow.completed"}}`:                                               "when workflow.completed happens",
		`{"kind":"manual"}`: "when it is fired",
	} {
		if got := triggerInWords(json.RawMessage(raw)); got != want {
			t.Errorf("%s: %q, want %q", raw, got, want)
		}
	}
}

func TestDefaultInWords(t *testing.T) {
	for raw, want := range map[string]string{
		``: "", `null`: "", `"draft"`: "draft", `true`: "On", `false`: "Off", `1799`: "1799", `0.5`: "0.5",
		`1e3`: "1000", `["Junk", "", "New Customers"]`: "Junk, New Customers", `{"a": 1}`: `{"a":1}`,
	} {
		if got := defaultInWords(json.RawMessage(raw)); got != want {
			t.Errorf("%s: %q, want %q", raw, got, want)
		}
	}
}
