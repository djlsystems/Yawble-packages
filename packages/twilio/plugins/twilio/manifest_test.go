package main

import (
	"encoding/json"
	"os"
	"slices"
	"testing"
)

// The manifest declares the sent event the plugin publishes, with its Monitor hint; no other event
// is hinted.
func TestTheManifestDeclaresTheSentEventAndItsMonitorHint(t *testing.T) {
	raw, err := os.ReadFile("plugin.json")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		ID      string `json:"id"`
		Version string `json:"version"`
		Events  struct {
			Publishes []struct {
				Type       string                        `json:"type"`
				HighVolume bool                          `json:"highVolume"`
				Fields     []struct{ Name, Kind string } `json:"fields"`
				Monitor    *struct {
					Icon        string          `json:"icon"`
					IconBy      json.RawMessage `json:"iconBy"`
					Action      string          `json:"action"`
					ActionField *string         `json:"actionField"`
					Label       string          `json:"label"`
				} `json:"monitor"`
			} `json:"publishes"`
		} `json:"events"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m.ID != "twilio" || m.Version != "1.5.0" {
		t.Fatalf("id %q version %q", m.ID, m.Version)
	}
	var types []string
	for _, e := range m.Events.Publishes {
		types = append(types, e.Type)
		if e.Type != "sent" {
			if e.Monitor != nil {
				t.Fatalf("event %s carries a monitor hint: %+v", e.Type, *e.Monitor)
			}
			continue
		}
		var names []string
		kinds := map[string]string{}
		for _, f := range e.Fields {
			names = append(names, f.Name)
			kinds[f.Name] = f.Kind
		}
		if !slices.Equal(names, []string{"number", "to", "sid", "autoReply", "dryRun"}) {
			t.Fatalf("sent fields %v", names)
		}
		for _, f := range []string{"number", "to", "sid"} {
			if kinds[f] != "string" {
				t.Fatalf("sent's %s is %q, not a string field", f, kinds[f])
			}
		}
		if kinds["autoReply"] != "boolean" {
			t.Fatalf("sent's autoReply is %q", kinds["autoReply"])
		}
		if !e.HighVolume {
			t.Fatal("sent is not highVolume: one poll can auto-reply to several texts")
		}
		h := e.Monitor
		if h == nil {
			t.Fatal("sent carries no monitor hint")
		}
		if h.Icon != "sms" || h.IconBy != nil || h.Action != "created" || h.ActionField != nil || h.Label != "{to}" {
			t.Fatalf("sent's hint = %+v", *h)
		}
	}
	if !slices.Equal(types, []string{"sent", "received", "optout", "optin", "delivery", "command"}) {
		t.Fatalf("events %v", types)
	}
}
