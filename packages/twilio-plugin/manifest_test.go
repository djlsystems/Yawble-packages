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
	if m.ID != "twilio-plugin" || m.Version != "3.0.0" {
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

// The Twilio credential is an API-key connection on the twilio slot. Each field the plugin once
// took as a secret is declared moved to that slot, so an update carries its value over; none is a
// secret the version asks for.
func TestTheCredentialFieldsMoveToTheTwilioSlot(t *testing.T) {
	raw, err := os.ReadFile("plugin.json")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Secrets map[string]struct {
			Required bool   `json:"required"`
			MovedTo  string `json:"movedTo"`
		} `json:"secrets"`
		Connections map[string]struct {
			Providers []string        `json:"providers"`
			Scopes    json.RawMessage `json:"scopes"`
			Multiple  bool            `json:"multiple"`
		} `json:"connections"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	c, ok := m.Connections[slot]
	if !ok || len(m.Connections) != 1 || !slices.Equal(c.Providers, []string{"apikey"}) || c.Scopes != nil || c.Multiple {
		t.Fatalf("connections = %+v", m.Connections)
	}
	fields := []string{"accountSid", "apiKeySid", "apiKeySecret"}
	if len(m.Secrets) != len(fields) {
		t.Fatalf("secrets %v, want only the moved %v", m.Secrets, fields)
	}
	for _, f := range fields {
		if s, ok := m.Secrets[f]; !ok || s.MovedTo != slot || s.Required {
			t.Errorf("secret %s = %+v, want movedTo %q and not required", f, s, slot)
		}
	}
}
