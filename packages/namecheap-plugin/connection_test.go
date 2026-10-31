package main

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
)

// The API user and API key moved to the namecheap connection (CON 7's movedTo), so an update
// carries a member's own values into one API-key connection; the manifest declares them only for that.
func TestTheManifestMovesTheKeysToTheNamecheapSlot(t *testing.T) {
	raw, err := os.ReadFile("plugin.json")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Version string `json:"version"`
		Secrets map[string]struct {
			MovedTo  string `json:"movedTo"`
			Required bool   `json:"required"`
		} `json:"secrets"`
		Connections map[string]struct {
			Providers []string `json:"providers"`
			Required  bool     `json:"required"`
		} `json:"connections"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m.Version != "3.0.0" {
		t.Fatalf("version %q", m.Version)
	}
	c, ok := m.Connections[slot]
	if !ok || !c.Required || !slices.Equal(c.Providers, []string{"apikey"}) || len(m.Connections) != 1 {
		t.Fatalf("connections = %+v; want one required slot %q with providers exactly [apikey]", m.Connections, slot)
	}
	if len(m.Secrets) != 2 {
		t.Fatalf("secrets = %+v; want apiUser and apiKey only", m.Secrets)
	}
	for _, name := range []string{"apiUser", "apiKey"} {
		s, ok := m.Secrets[name]
		if !ok || s.MovedTo != slot || s.Required {
			t.Errorf("secret %s = %+v; want movedTo %q and not required", name, s, slot)
		}
	}
}

// The run reads the API user and key from the connection, never from secrets.
func TestTheConnectionsUserAndKeyAreUsed(t *testing.T) {
	n := newHarness(t)
	n.fake.add("example.com", true, tenRecords()...)
	if r := n.run("list example.com"); !r.ok {
		t.Fatalf("list with the connection bound failed: %s", r.output)
	}

	// The same values handed as secrets, with no connection, are not used.
	n.sec = nil
	req, _ := json.Marshal(map[string]any{
		"protocol": "harness.member/1", "work": []map[string]any{{"instruction": "list example.com"}}, "config": n.cfg,
		"secrets": map[string]string{"apiUser": testUser, "apiKey": testKey}, "connections": map[string]any{}, "workingDirectory": n.dir,
	})
	var out strings.Builder
	if code := run(strings.NewReader(string(req)), &out, n.clock.clock()); code == 0 {
		t.Fatalf("a run with the keys only in secrets succeeded:\n%s", out.String())
	}
	mustContain(t, out.String(), "no Namecheap API user and API key are bound", "`namecheap` slot", "Team settings, Connections")
}

func TestAMissingConnectionIsRefusedAndNothingIsCalled(t *testing.T) {
	for name, sec := range map[string]map[string]string{
		"no connection": nil,
		"no key":        {"apiUser": testUser, "clientIp": testIP},
	} {
		t.Run(name, func(t *testing.T) {
			n := newHarness(t)
			n.fake.add("example.com", true, tenRecords()...)
			n.sec = sec
			r := n.run("ensure example.com A @ 203.0.113.99")
			if r.ok {
				t.Fatalf("ensure succeeded: %s", r.output)
			}
			mustContain(t, r.output, "Refused: no Namecheap API user and API key are bound",
				"binds a Namecheap API-key connection", "`namecheap` slot", "Team settings, Connections", "Nothing was changed.")
			if len(n.fake.commands) != 0 {
				t.Errorf("calls: %v", n.fake.commands)
			}
		})
	}
}

// The connection's Whitelisted IP wins over the clientIp setting and over the address service.
func TestTheConnectionsWhitelistedIPIsTheClientIP(t *testing.T) {
	n := newHarness(t)
	n.fake.add("example.com", true, tenRecords()...)
	n.fake.allowed = map[string]bool{"203.0.113.50": true}
	n.cfg["clientIp"] = "203.0.113.8"
	n.sec = map[string]string{"apiUser": testUser, "apiKey": testKey, "clientIp": "203.0.113.50"}
	r := n.run("setup")
	mustContain(t, r.output, "this computer's internet address is 203.0.113.50 (the namecheap connection's Whitelisted IP)")
	if r := n.run("list example.com"); !r.ok {
		t.Fatalf("list from the connection's Whitelisted IP failed: %s", r.output)
	}
}

// An Other connection without a clientIp field, no clientIp setting and no address service: the
// next step names the member's clientIp setting.
func TestSetupNamesTheClientIPSettingWhenTheConnectionHasNoIP(t *testing.T) {
	n := newHarness(t)
	old := addressURL
	addressURL = "http://127.0.0.1:1"
	t.Cleanup(func() { addressURL = old })
	n.sec = map[string]string{"apiUser": testUser, "apiKey": testKey}
	r := n.run("setup")
	mustContain(t, r.output, "MISSING  this computer's internet address could not be found",
		"The namecheap connection has no Whitelisted IP", "this member's `clientIp` setting")
}
