package main

import (
	"encoding/json"
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
				Type   string                  `json:"type"`
				Fields []struct{ Name string } `json:"fields"`
			} `json:"publishes"`
		} `json:"events"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m.ID != "stripe" || m.Version != "1.0.0" {
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
	for _, s := range c.Scopes["microsoft"] {
		if s != "offline_access" && s != "https://vault.azure.net/user_impersonation" {
			t.Fatalf("the microsoft slot asks for %q, beyond the Key Vault API", s)
		}
	}
	want := map[string][]string{
		"changed": {"objectType", "id", "lookupKey", "environment", "action"},
		"refused": {"command", "reason"},
	}
	for _, e := range m.Events.Publishes {
		var names []string
		for _, f := range e.Fields {
			names = append(names, f.Name)
		}
		if !slices.Equal(names, want[e.Type]) {
			t.Fatalf("event %s fields %v", e.Type, names)
		}
		delete(want, e.Type)
	}
	if len(want) != 0 {
		t.Fatalf("events not declared: %v", want)
	}
}
