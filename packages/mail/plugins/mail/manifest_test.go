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
	Version string `json:"version"`
	Config  map[string]struct {
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
	if m.Version != "2.0.2" {
		t.Fatalf("want version 2.0.2, got %s", m.Version)
	}
	off := map[string]string{"mode": `"draft"`, "sendAllowlist": `[]`, "markRead": `false`, "moveTo": `[]`}
	for name, def := range off {
		f, ok := m.Config[name]
		if !ok || f.SetBy != "person" || string(f.Default) != def {
			t.Fatalf("%s must be setBy person with default %s, got %+v", name, def, f)
		}
	}
	for name, f := range m.Config {
		if _, safety := off[name]; !safety && f.SetBy == "person" {
			t.Fatalf("only the safety settings are a person's; %s is too", name)
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

func TestTheManifestTakesAllThreeWaysInAndReadsTheWatchCollection(t *testing.T) {
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
	if len(m.Reads) != 1 || m.Reads[0].Site != watchSite || m.Reads[0].Collection != watchCollection {
		t.Fatalf("want reads of %s/%s, got %+v", watchSite, watchCollection, m.Reads)
	}
}

func TestTheManifestDeclaresTheEventWithTheFieldsItCarries(t *testing.T) {
	m := readManifest(t)
	if len(m.Events.Publishes) != 1 || m.Events.Publishes[0].Type != "received" {
		t.Fatalf("want one event, received, got %+v", m.Events.Publishes)
	}
	var declared []string
	for _, f := range m.Events.Publishes[0].Fields {
		declared = append(declared, f.Name)
	}
	var carried []string
	for k := range eventPayload(&watchDoc{}, nil, 0, 0) {
		carried = append(carried, k)
	}
	sort.Strings(declared)
	sort.Strings(carried)
	if !slices.Equal(declared, carried) {
		t.Fatalf("event fields declared %v, carried %v", declared, carried)
	}
}
