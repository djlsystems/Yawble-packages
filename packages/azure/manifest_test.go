package main

import (
	"encoding/json"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
)

type manifest struct {
	ID      string `json:"id"`
	Version string `json:"version"`
	Config  map[string]struct {
		Type        string   `json:"type"`
		Enum        []string `json:"enum"`
		Default     any      `json:"default"`
		SetBy       string   `json:"setBy"`
		Advanced    bool     `json:"advanced"`
		Description string   `json:"description"`
	} `json:"config"`
	Secrets map[string]struct {
		Description string `json:"description"`
		Required    bool   `json:"required"`
	} `json:"secrets"`
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
	Skills []string `json:"skills"`
}

func readManifest(t *testing.T) manifest {
	t.Helper()
	raw, err := os.ReadFile("plugin.json")
	if err != nil {
		t.Fatal(err)
	}
	var m manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestTheManifestDeclaresWhatThePluginUses(t *testing.T) {
	m := readManifest(t)
	if m.ID != "azure" || m.Version != "1.1.2" {
		t.Fatalf("id %q version %q", m.ID, m.Version)
	}
	settingNames := []string{"subscription", "regions", "mode", "allowDelete", "sharedSqlServers", "sharedContainerRegistries", "sharedRegistries", "monthlyBudget", "budgetEmails", "namePrefix"}
	for _, name := range settingNames {
		c, ok := m.Config[name]
		if !ok || c.SetBy != "person" {
			t.Fatalf("%s is missing or not set by a person only", name)
		}
		// One plain sentence each.
		if strings.Count(strings.TrimSuffix(c.Description, "."), ". ") != 0 || !strings.HasSuffix(c.Description, ".") {
			t.Fatalf("%s's description is not one sentence: %q", name, c.Description)
		}
	}
	if len(m.Config) != len(settingNames) {
		t.Fatalf("settings = %d, want %d", len(m.Config), len(settingNames))
	}
	if m.Config["mode"].Default != "whatif" || m.Config["allowDelete"].Default != false || m.Config["namePrefix"].Default != "" || !m.Config["namePrefix"].Advanced {
		t.Fatalf("defaults: mode %v, allowDelete %v, namePrefix %v", m.Config["mode"].Default, m.Config["allowDelete"].Default, m.Config["namePrefix"].Default)
	}

	// sharedContainerRegistries is the setting; sharedRegistries, its old name, stays declared so the
	// Host still hands a stored value to the run.
	if d := m.Config["sharedContainerRegistries"].Description; m.Config["sharedContainerRegistries"].Advanced ||
		!strings.HasPrefix(d, "Existing Azure Container Registries (ACR), by name such as myregistry,") || !strings.Contains(d, "Container App's image is pulled from one of them.") {
		t.Fatalf("sharedContainerRegistries: %+v", m.Config["sharedContainerRegistries"])
	}
	if old := m.Config["sharedRegistries"]; !old.Advanced || old.Type != "list" || !strings.Contains(old.Description, "old name of sharedContainerRegistries") {
		t.Fatalf("sharedRegistries: %+v", old)
	}

	// Regions are chosen from a list, eastus2 unless a person picks others.
	if r := m.Config["regions"]; !slices.Contains(r.Enum, "eastus2") || !slices.Equal(listOf(r.Default), []string{"eastus2"}) {
		t.Fatalf("regions: enum %v, default %v", r.Enum, r.Default)
	}

	if len(m.Connections) != 1 {
		t.Fatalf("connection slots = %d, want one", len(m.Connections))
	}
	c, ok := m.Connections[slot]
	if !ok || !c.Required || !slices.Equal(c.Providers, []string{"microsoft"}) {
		t.Fatalf("connection slot = %+v", c)
	}
	// One scope per API the client knows, keyed as stdin keys tokens: the part before the scope's
	// last path segment.
	apis := map[string]bool{}
	for _, s := range c.Scopes["microsoft"] {
		if s == "offline_access" {
			continue
		}
		apis[s[:strings.LastIndex(s, "/")+1]] = true
	}
	for a := range apiNames {
		if !apis[string(a)] {
			t.Fatalf("no scope for %s", a)
		}
		delete(apis, string(a))
	}
	if len(apis) != 0 {
		t.Fatalf("scopes for APIs the plugin does not use: %v", apis)
	}

	want := map[string][]string{
		"planned":  {"command", "target", "summary", "mode", "create", "modify", "delete", "unchanged", "refusals"},
		"applied":  {"command", "target", "summary", "resourceIds"},
		"resource": {"resourceId", "resourceType", "name", "action", "command"},
		"refused":  {"command", "reason"},
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
	if !slices.Equal(m.Skills, []string{"skills/plugin-azure.md"}) {
		t.Fatalf("skills = %v", m.Skills)
	}
	if _, err := os.Stat(m.Skills[0]); err != nil {
		t.Fatal(err)
	}
}

func TestEveryPublishedPayloadHasExactlyTheDeclaredFields(t *testing.T) {
	m := readManifest(t)
	declared := map[string][]string{}
	for _, e := range m.Events.Publishes {
		for _, f := range e.Fields {
			declared[e.Type] = append(declared[e.Type], f.Name)
		}
		sort.Strings(declared[e.Type])
	}
	f := newFake(t)
	f.whatIf = []map[string]any{wiChange("Create", inRG(ownedRG, "Microsoft.ManagedIdentity/userAssignedIdentities/x"), nil)}
	tpl := docFile(t, "main.json", simpleTemplate)
	seen := map[string]bool{}
	for _, run := range []struct {
		cfg  map[string]any
		line string
	}{
		{baseConfig(nil), "rg create web westeurope"},
		{apply(nil), "rg create web westeurope"},
		{apply(nil), "deploy " + ownedRG + " " + tpl},
		{apply(nil), "delete " + ownedRG},
	} {
		r := runPlugin(t, runOpts{config: run.cfg}, run.line)
		for _, rec := range r.records {
			if rec["t"] != "publish" {
				continue
			}
			typ := rec["type"].(string)
			var keys []string
			for k := range rec["payload"].(map[string]any) {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			if !slices.Equal(keys, declared[typ]) {
				t.Fatalf("%s payload keys %v, declared %v", typ, keys, declared[typ])
			}
			seen[typ] = true
		}
	}
	if len(seen) != 4 {
		t.Fatalf("events seen = %v", seen)
	}
}

// TestNothingInThePackageNamesARealTenant keeps the package generic: every GUID is all zeros or a
// built-in role's id, which is the same in every tenant.
func TestNothingInThePackageNamesARealTenant(t *testing.T) {
	guid := regexp.MustCompile(`(?i)[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	allowed := map[string]bool{"8e3af657-a8ff-443c-a75c-2fe8c4bcb635": true} // Owner, which the tests refuse
	for id := range grantable {
		allowed[id] = true
	}
	files, _ := os.ReadDir(".")
	for _, e := range files {
		if e.IsDir() {
			continue
		}
		raw, _ := os.ReadFile(e.Name())
		for _, g := range guid.FindAllString(string(raw), -1) {
			if !allowed[strings.ToLower(g)] && strings.Trim(strings.ReplaceAll(g, "-", ""), "0") != "" && !strings.HasPrefix(g, "00000000-0000-0000-0000-") {
				t.Fatalf("%s holds the GUID %s, which is neither all zeros nor a built-in role", e.Name(), g)
			}
		}
	}
}

// listOf reads a JSON list of strings, as a manifest default arrives.
func listOf(v any) []string {
	items, _ := v.([]any)
	out := make([]string, 0, len(items))
	for _, i := range items {
		s, _ := i.(string)
		out = append(out, s)
	}
	return out
}
