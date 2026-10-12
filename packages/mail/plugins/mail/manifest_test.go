package main

import (
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
)

type manifest struct {
	Version     string `json:"version"`
	Description string `json:"description"`
	Config      map[string]struct {
		Type    string          `json:"type"`
		Default json.RawMessage `json:"default"`
		SetBy   string          `json:"setBy"`
	} `json:"config"`
	Connections map[string]struct {
		Providers []string            `json:"providers"`
		Scopes    map[string][]string `json:"scopes"`
	} `json:"connections"`
	Reads  []struct{ Site, Collection string } `json:"reads"`
	Events struct {
		Publishes []struct {
			Type   string `json:"type"`
			Fields []struct {
				Name string `json:"name"`
			} `json:"fields"`
		} `json:"publishes"`
	} `json:"events"`
}

func readManifest(t *testing.T) manifest {
	t.Helper()
	b, err := os.ReadFile("plugin.json")
	if err != nil {
		t.Fatal(err)
	}
	var m manifest
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("plugin.json: %v", err)
	}
	return m
}

func TestTheManifestKeepsTheSafetySettingsThePersonsAndOff(t *testing.T) {
	m := readManifest(t)
	if m.Version != "2.2.1" {
		t.Fatalf("want version 2.2.1, got %s", m.Version)
	}
	off := map[string]string{"mode": `"draft"`, "sendAllowlist": `[]`, "markRead": `false`, "moveTo": `[]`}
	for name, def := range off {
		f, ok := m.Config[name]
		if !ok || f.SetBy != "person" || string(f.Default) != def {
			t.Fatalf("%s must be setBy person with default %s, got %+v", name, def, f)
		}
	}
	// The follow quiet hours are the person's too: nobody on the team moves a person's quiet hours.
	quiet := map[string]string{"followQuietStart": `""`, "followQuietEnd": `""`, "followTimeZone": `"UTC"`}
	for name, def := range quiet {
		f, ok := m.Config[name]
		if !ok || f.SetBy != "person" || string(f.Default) != def {
			t.Fatalf("%s must be setBy person with default %s, got %+v", name, def, f)
		}
	}
	for name, f := range m.Config {
		_, safety := off[name]
		_, hours := quiet[name]
		if !safety && !hours && f.SetBy == "person" {
			t.Fatalf("only the safety settings and the quiet hours are a person's; %s is too", name)
		}
	}
}

func TestTheManifestDeclaresEverySettingTheCodeReadsAndNoOther(t *testing.T) {
	m := readManifest(t)
	var code []string
	rt := reflect.TypeOf(request{}.Config)
	for i := range rt.NumField() {
		code = append(code, strings.Split(rt.Field(i).Tag.Get("json"), ",")[0])
	}
	var declared []string
	for k := range m.Config {
		declared = append(declared, k)
	}
	sort.Strings(code)
	sort.Strings(declared)
	if !slices.Equal(code, declared) {
		t.Fatalf("settings read %v, declared %v", code, declared)
	}
}

func TestTheManifestTakesAllThreeWaysInAndReadsTheWatchAndFollowsCollections(t *testing.T) {
	m := readManifest(t)
	slot, ok := m.Connections[slot]
	if !ok || !slices.Equal(slot.Providers, []string{"imap", "microsoft", "google"}) {
		t.Fatalf("want the mailbox slot to admit imap, microsoft and google, got %+v", slot)
	}
	if _, has := slot.Scopes["imap"]; has {
		t.Fatal("scopes do not apply to imap; an imap entry refuses the plugin")
	}
	for _, s := range []string{scopeReadonly, scopeCompose, scopeModify} {
		if !slices.Contains(slot.Scopes["google"], s) {
			t.Fatalf("google needs %s", s)
		}
	}
	for _, s := range []string{scopeGraphReadWrite, scopeGraphSend} {
		if !slices.Contains(slot.Scopes["microsoft"], s) {
			t.Fatalf("microsoft needs %s", s)
		}
	}
	if len(m.Reads) != 2 || m.Reads[0].Site != watchSite || m.Reads[0].Collection != watchCollection ||
		m.Reads[1].Site != watchSite || m.Reads[1].Collection != followsCollection {
		t.Fatalf("want reads of %s/%s and %s/%s, got %+v", watchSite, watchCollection, watchSite, followsCollection, m.Reads)
	}
}

func TestTheManifestDeclaresTheEventsWithTheFieldsTheyCarry(t *testing.T) {
	m := readManifest(t)
	follow := followPayload("", nil, nil)
	payloads := map[string]map[string]any{"received": eventPayload(&watchDoc{}, nil, 0, 0), "unanswered": follow, "answered": follow}
	var types []string
	for _, e := range m.Events.Publishes {
		types = append(types, e.Type)
		var declared []string
		for _, f := range e.Fields {
			declared = append(declared, f.Name)
		}
		var carried []string
		for k := range payloads[e.Type] {
			carried = append(carried, k)
		}
		sort.Strings(declared)
		sort.Strings(carried)
		if !slices.Equal(declared, carried) {
			t.Fatalf("%s: event fields declared %v, carried %v", e.Type, declared, carried)
		}
	}
	if !slices.Equal(types, []string{"received", "unanswered", "answered"}) {
		t.Fatalf("want the events received, unanswered and answered, got %v", types)
	}
}

// The plugin and the solution say the same version, so an installed 2.0.3 sees the update, and both
// say a reply is threaded.
func TestTheSolutionShipsThisVersionAndSaysAReplyIsThreaded(t *testing.T) {
	m := readManifest(t)
	b, err := os.ReadFile("../../solution.json")
	if err != nil {
		t.Fatal(err)
	}
	var sol struct {
		Version, Description string
	}
	if err := json.Unmarshal(b, &sol); err != nil {
		t.Fatalf("solution.json: %v", err)
	}
	if sol.Version != m.Version {
		t.Fatalf("the solution is %s, the plugin %s", sol.Version, m.Version)
	}
	for what, text := range map[string]string{"plugin.json": m.Description, "solution.json": sol.Description} {
		if !strings.Contains(text, "reply") || !strings.Contains(text, "threaded") {
			t.Fatalf("%s's description must say a reply is threaded: %q", what, text)
		}
	}
}
