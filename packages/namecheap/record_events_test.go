package main

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
)

// ---- plugin.namecheap.record and its Monitor hint ----------------------------------------------------

type manifestEvent struct {
	Type       string                        `json:"type"`
	HighVolume bool                          `json:"highVolume"`
	Monitor    map[string]any                `json:"monitor"`
	Fields     []struct{ Name, Kind string } `json:"fields"`
}

func manifestEvents(t *testing.T) map[string]manifestEvent {
	t.Helper()
	raw, err := os.ReadFile("plugin.json")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Events struct {
			Publishes []manifestEvent `json:"publishes"`
		} `json:"events"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	out := map[string]manifestEvent{}
	for _, e := range m.Events.Publishes {
		out[e.Type] = e
	}
	return out
}

func fieldNames(e manifestEvent) []string {
	var names []string
	for _, f := range e.Fields {
		names = append(names, f.Name)
	}
	return names
}

func TestTheManifestDeclaresTheRecordEventAndItsMonitorHint(t *testing.T) {
	events := manifestEvents(t)
	rec, ok := events["record"]
	if !ok {
		t.Fatalf("no record event in plugin.json: %v", events)
	}
	if !rec.HighVolume {
		t.Errorf("record is not highVolume")
	}
	want := []string{"domain", "recordType", "host", "value", "action", "backupId"}
	if got := fieldNames(rec); !reflect.DeepEqual(got, want) {
		t.Errorf("record fields %v, want %v", got, want)
	}
	for _, f := range rec.Fields {
		if f.Kind != "string" {
			t.Errorf("record field %s is %s, want string", f.Name, f.Kind)
		}
	}
	wantHint := map[string]any{"icon": "dns", "actionField": "action", "label": "{recordType} {host}.{domain}"}
	if !reflect.DeepEqual(rec.Monitor, wantHint) {
		t.Errorf("record monitor %v, want %v", rec.Monitor, wantHint)
	}

	// changed keeps its fields and stays the summary; neither it nor refused has a hint.
	if got := fieldNames(events["changed"]); !reflect.DeepEqual(got, []string{"domain", "command", "added", "changed", "removed", "backupId"}) {
		t.Errorf("changed fields %v", got)
	}
	for _, typ := range []string{"changed", "refused"} {
		if e, ok := events[typ]; !ok || e.Monitor != nil {
			t.Errorf("%s: present %t, monitor %v", typ, ok, e.Monitor)
		}
	}
}

func records(r runResult) []map[string]any {
	var out []map[string]any
	for _, e := range r.events {
		if e["type"] == "record" {
			p, _ := e["payload"].(map[string]any)
			out = append(out, p)
		}
	}
	return out
}

func TestApplyPublishesOneRecordEventPerRecordAddedChangedOrRemoved(t *testing.T) {
	n := newHarness(t)
	n.fake.add("example.com", true, tenRecords()...)
	for _, tc := range []struct {
		cmd  string
		want map[string]any
	}{
		{`ensure example.com TXT _acme-challenge "token-placeholder-123"`,
			map[string]any{"recordType": "TXT", "host": "_acme-challenge", "value": "token-placeholder-123", "action": "added"}},
		{"ensure example.com A blog 203.0.113.99",
			map[string]any{"recordType": "A", "host": "blog", "value": "203.0.113.99", "action": "changed"}},
		{"remove example.com TXT _dmarc",
			map[string]any{"recordType": "TXT", "host": "_dmarc", "value": "v=DMARC1; p=none; rua=mailto:dmarc@example.com", "action": "removed"}},
	} {
		r := n.run(tc.cmd)
		if !r.ok {
			t.Fatalf("%s: %s", tc.cmd, r.output)
		}
		recs := records(r)
		if len(recs) != 1 {
			t.Fatalf("%s: record events %v", tc.cmd, recs)
		}
		summary := r.event("changed")
		tc.want["domain"] = "example.com"
		tc.want["backupId"] = summary["backupId"]
		if !reflect.DeepEqual(recs[0], tc.want) {
			t.Errorf("%s: record %v, want %v", tc.cmd, recs[0], tc.want)
		}
	}
}

func TestNoRecordEventForAListADryRunARefusalOrAFailedWrite(t *testing.T) {
	n := newHarness(t)
	n.fake.add("example.com", true, tenRecords()...)
	n.fake.add("example.org", true, tenRecords()...)
	check := func(what string, r runResult) {
		t.Helper()
		if recs := records(r); len(recs) != 0 {
			t.Errorf("%s published record events: %v", what, recs)
		}
	}
	check("list", n.run("list example.com"))
	check("setup", n.run("setup"))
	check("refusal", n.run("ensure example.org A @ 203.0.113.99"))
	check("no change", n.run("ensure example.com A blog 203.0.113.11 600"))
	n.fake.failSetHosts = true
	check("failed write", n.run("ensure example.com A www2 203.0.113.12"))
	n.fake.failSetHosts = false

	delete(n.cfg, "mode")
	for _, cmd := range []string{"ensure example.com A @ 203.0.113.99", "remove example.com TXT _dmarc", "remove example.com URL301 old"} {
		check("dry run "+cmd, n.run(cmd))
	}
	backup := n.backups("example.com")
	if len(backup) == 0 {
		t.Fatal("the failed write saved no backup to restore")
	}
	{
		check("dry-run restore", n.run("restore example.com "+backup[0]))
	}
}

func TestRecordEventsStopAtTwentyFiveAndChangedCoversTheRest(t *testing.T) {
	n := newHarness(t)
	n.fake.add("example.com", true)
	if r := n.run("ensure example.com A www 203.0.113.1"); !r.ok {
		t.Fatal(r.output)
	}
	empty := n.backups("example.com")[0]

	var many []xmlHost
	for i := 1; i <= 30; i++ {
		many = append(many, hr(fmt.Sprintf("h%d", i), "A", fmt.Sprintf("203.0.113.%d", i), 1799))
	}
	n.fake.add("example.com", true, many...)
	r := n.run("restore example.com " + empty)
	if !r.ok {
		t.Fatalf("restore: %s", r.output)
	}
	summary := r.event("changed")
	if removed, _ := summary["removed"].([]any); len(removed) != 30 {
		t.Fatalf("changed removed %d, want 30: %v", len(removed), summary)
	}
	recs := records(r)
	if len(recs) != recordEventCap || recordEventCap != 25 {
		t.Fatalf("record events %d, want 25", len(recs))
	}
	for i, rec := range recs {
		if rec["action"] != "removed" || rec["host"] != fmt.Sprintf("h%d", i+1) || rec["backupId"] != summary["backupId"] {
			t.Errorf("record %d: %v", i+1, rec)
		}
	}
}

func TestNoRecordEventHoldsACredential(t *testing.T) {
	n := newHarness(t)
	n.fake.add("example.com", true, tenRecords()...)
	n.fake.echoKey = true
	var all []map[string]any
	for _, cmd := range []string{"ensure example.com A @ 203.0.113.99", "remove example.com TXT _dmarc", `ensure example.com TXT _acme-challenge "token-placeholder-123"`} {
		r := n.run(cmd)
		if !r.ok {
			t.Fatalf("%s: %s", cmd, r.output)
		}
		all = append(all, records(r)...)
	}
	if len(all) != 3 {
		t.Fatalf("record events %v", all)
	}
	allowed := map[string]bool{"domain": true, "recordType": true, "host": true, "value": true, "action": true, "backupId": true}
	for _, rec := range all {
		for k, v := range rec {
			s := fmt.Sprint(v)
			if !allowed[k] || strings.Contains(s, testKey) || strings.Contains(s, testUser) {
				t.Errorf("record field %s = %q", k, s)
			}
		}
	}
}
