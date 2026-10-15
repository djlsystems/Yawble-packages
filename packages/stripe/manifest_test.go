package main

import (
	"encoding/json"
	"maps"
	"os"
	"slices"
	"testing"
)

func TestTheManifestDeclaresWhatThePluginUses(t *testing.T) {
	raw, err := os.ReadFile("plugin.json")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		ID      string `json:"id"`
		Version string `json:"version"`
		Config  map[string]struct {
			Default any    `json:"default"`
			SetBy   string `json:"setBy"`
		} `json:"config"`
		Secrets     map[string]struct{ Required bool } `json:"secrets"`
		Connections map[string]struct {
			Providers []string            `json:"providers"`
			Scopes    map[string][]string `json:"scopes"`
			Required  bool                `json:"required"`
		} `json:"connections"`
		Events struct {
			Publishes []struct {
				Type    string                        `json:"type"`
				Fields  []struct{ Name, Kind string } `json:"fields"`
				Monitor *struct {
					Icon   string `json:"icon"`
					IconBy *struct {
						Field string            `json:"field"`
						Icons map[string]string `json:"icons"`
					} `json:"iconBy"`
					Action      *string `json:"action"`
					ActionField string  `json:"actionField"`
					Label       string  `json:"label"`
				} `json:"monitor"`
			} `json:"publishes"`
		} `json:"events"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m.ID != "stripe" || m.Version != "1.2.0" {
		t.Fatalf("id %q version %q", m.ID, m.Version)
	}
	for _, name := range []string{"mode", "keyVaults", "expectedAccount"} {
		if m.Config[name].SetBy != "person" {
			t.Fatalf("%s is not set by a person only", name)
		}
	}
	if m.Config["mode"].Default != "dryrun" {
		t.Fatalf("mode defaults to %v", m.Config["mode"].Default)
	}
	if !m.Secrets["apiKey"].Required {
		t.Fatal("apiKey is not a required secret")
	}
	c, ok := m.Connections[slot]
	if !ok || c.Required || !slices.Equal(c.Providers, []string{"microsoft"}) {
		t.Fatalf("connection slot = %+v", c)
	}
	// Azure management only: a sign-in that asks for one API is granted with one device code.
	if want := []string{"https://management.azure.com/user_impersonation", "offline_access"}; !slices.Equal(c.Scopes["microsoft"], want) || len(c.Scopes) != 1 {
		t.Fatalf("the microsoft slot asks for %v, want exactly %v", c.Scopes, want)
	}
	want := map[string][]string{
		"changed": {"objectType", "id", "lookupKey", "environment", "action"},
		"refused": {"command", "reason"},
	}
	for _, e := range m.Events.Publishes {
		var names []string
		kinds := map[string]string{}
		for _, f := range e.Fields {
			names = append(names, f.Name)
			kinds[f.Name] = f.Kind
		}
		if !slices.Equal(names, want[e.Type]) {
			t.Fatalf("event %s fields %v", e.Type, names)
		}
		delete(want, e.Type)

		// Only a change in Stripe is hinted for the Monitor; a refusal is not.
		if e.Type != "changed" {
			if e.Monitor != nil {
				t.Fatalf("event %s carries a monitor hint: %+v", e.Type, *e.Monitor)
			}
			continue
		}
		h := e.Monitor
		if h == nil {
			t.Fatal("changed carries no monitor hint")
		}
		if h.Icon != "payments" || h.Action != nil || h.ActionField != "action" || h.Label != "{lookupKey}" {
			t.Fatalf("changed's hint = %+v", *h)
		}
		icons := map[string]string{"product": "sell", "price": "payments", "webhook_endpoint": "webhook"}
		if h.IconBy == nil || h.IconBy.Field != "objectType" || !maps.Equal(h.IconBy.Icons, icons) {
			t.Fatalf("changed's iconBy = %+v, want objectType mapped to %v", h.IconBy, icons)
		}
		// The fields the hint names are declared strings.
		for _, f := range []string{"objectType", "action", "lookupKey"} {
			if kinds[f] != "string" {
				t.Fatalf("changed's %s is %q, not a string field", f, kinds[f])
			}
		}
	}
	if len(want) != 0 {
		t.Fatalf("events not declared: %v", want)
	}
}
