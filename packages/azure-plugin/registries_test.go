package main

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
)

// staleKey is the setting 3.2.0 no longer declares: the old name of sharedContainerRegistries.
// The Host does not hand a value stored under it to a run; the plugin never reads it if one comes.
const staleKey = "sharedRegistries"

// hostConfig is the config block of testdata/host-request-masked.json - every setting the Host
// sends, as it sends it - with a subscription, mode apply and extra on top.
func hostConfig(t *testing.T, extra map[string]any) map[string]any {
	t.Helper()
	raw, err := os.ReadFile("testdata/host-request-masked.json")
	if err != nil {
		t.Fatal(err)
	}
	var req struct {
		Config map[string]any `json:"config"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatal(err)
	}
	if _, ok := req.Config[staleKey]; ok {
		t.Fatalf("the fixture's config still holds %s", staleKey)
	}
	cfg := req.Config
	cfg["subscription"], cfg["regions"], cfg["mode"] = testSub, []string{"westeurope"}, "apply"
	for k, v := range extra {
		cfg[k] = v
	}
	return cfg
}

// acrPushes counts the builds the fake was asked to run on a registry.
func acrPushes(w *world, registryID string) int {
	n := 0
	for _, c := range w.f.callsTo(http.MethodPost, "") {
		if strings.HasPrefix(strings.ToLower(c.Path), strings.ToLower(registryID)+"/") {
			n++
		}
	}
	return n
}

// With only sharedContainerRegistries set, a listed registry takes builds and AcrPull and an
// unlisted one is refused, as before.
func TestSharedContainerRegistriesIsTheOneList(t *testing.T) {
	w := newWorld(t)
	reg := w.foreign["registry"].ID
	other := w.f.add(foreignRG, "Microsoft.ContainerRegistry/registries", "otherregistry", map[string]string{"cost-center": "shared"})
	cfg := hostConfig(t, map[string]any{"sharedContainerRegistries": []string{"theirregistry"}})

	if r := runPlugin(t, runOpts{config: cfg}, "acr build theirregistry my-app:1.0 "+w.folder); !r.ok() {
		t.Fatalf("acr build on a listed registry: %s", r.text())
	}
	if acrPushes(w, reg) == 0 {
		t.Fatal("no build reached the listed registry")
	}
	if r := runPlugin(t, runOpts{config: cfg}, "role assign AcrPull "+w.ident.ID+" "+reg); !r.ok() {
		t.Fatalf("AcrPull on a listed registry: %s", r.text())
	}

	before := len(w.f.writes())
	r := runPlugin(t, runOpts{config: cfg}, "acr build otherregistry my-app:1.0 "+w.folder)
	if r.ok() {
		t.Fatalf("acr build on an unlisted registry was not refused: %s", r.text())
	}
	mustContain(t, r.text(), "This member builds only into registries its team owns and those in sharedContainerRegistries. Nothing was changed.")
	if len(w.f.writes()) != before || acrPushes(w, other.ID) != 0 {
		t.Fatal("a refused build changed something")
	}

	mustContain(t, runPlugin(t, runOpts{config: cfg}, "setup").text(), "- sharedContainerRegistries reachable: theirregistry")
	mustContain(t, runPlugin(t, runOpts{config: cfg}, "whoami").text(), "- sharedContainerRegistries: theirregistry")
}

// A stale sharedRegistries in a run's settings is never read: its registry is not shared, the run
// does not fail, and no answer names it - whatever it holds, even a value that is not a list.
func TestAStaleSharedRegistriesIsNeverRead(t *testing.T) {
	for name, stale := range map[string]any{
		"a registry":      []string{"staleregistry"},
		"a bad name":      []string{"no"},
		"not even a list": 42,
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			reg := w.foreign["registry"].ID
			staleReg := w.f.add(foreignRG, "Microsoft.ContainerRegistry/registries", "staleregistry", map[string]string{"cost-center": "shared"})
			cfg := hostConfig(t, map[string]any{"sharedContainerRegistries": []string{"theirregistry"}, staleKey: stale})

			var text []string
			r := runPlugin(t, runOpts{config: cfg}, "acr build theirregistry my-app:1.0 "+w.folder)
			if !r.ok() || acrPushes(w, reg) == 0 {
				t.Fatalf("acr build on the listed registry with a stale %s: %s", staleKey, r.text())
			}
			text = append(text, r.text())

			before := len(w.f.writes())
			r = runPlugin(t, runOpts{config: cfg}, "acr build staleregistry my-app:1.0 "+w.folder)
			if r.ok() {
				t.Fatalf("the stale registry took a build: %s", r.text())
			}
			mustContain(t, r.text(), "This member builds only into registries its team owns and those in sharedContainerRegistries.")
			text = append(text, r.text())
			r = runPlugin(t, runOpts{config: cfg}, "role assign AcrPull "+w.ident.ID+" "+staleReg.ID)
			if r.ok() {
				t.Fatalf("the stale registry took AcrPull: %s", r.text())
			}
			text = append(text, r.text())
			if len(w.f.writes()) != before || acrPushes(w, staleReg.ID) != 0 {
				t.Fatal("a refused change on the stale registry changed something")
			}

			for _, line := range []string{"setup", "whoami"} {
				r := runPlugin(t, runOpts{config: cfg}, line)
				if !r.ok() {
					t.Fatalf("%s with a stale %s failed: %s", line, staleKey, r.text())
				}
				text = append(text, r.text())
			}
			mustContain(t, text[3], "- sharedContainerRegistries reachable: theirregistry")
			mustContain(t, text[4], "- sharedContainerRegistries: theirregistry\n")
			all := strings.ToLower(strings.Join(text, "\n"))
			for _, gone := range []string{strings.ToLower(staleKey), "old name", "(old)"} {
				if strings.Contains(all, gone) {
					t.Errorf("an answer names %q:\n%s", gone, strings.Join(text, "\n"))
				}
			}
			if strings.Contains(strings.Join(text[3:], "\n"), "staleregistry") {
				t.Errorf("setup or whoami names the stale registry:\n%s", strings.Join(text[3:], "\n"))
			}
		})
	}
}

func TestParseSettingsReadsOnlySharedContainerRegistries(t *testing.T) {
	s, refusal := parseSettings(rawConfig(t, map[string]any{
		"sharedContainerRegistries": []string{"https://NewRegistry" + registryHost("") + "/", "newregistry", "/subscriptions/" + testSub + "/resourceGroups/rg/providers/Microsoft.ContainerRegistry/registries/idregistry"},
		staleKey:                    []string{"staleregistry"},
	}))
	if refusal != "" {
		t.Fatal(refusal)
	}
	if got := strings.Join(s.SharedRegistries, ","); got != "newregistry,idregistry" {
		t.Fatalf("shared registries = %s", got)
	}
	if s.sharedRegistry("staleregistry") {
		t.Fatal("a stale registry counts as shared")
	}
	_, refusal = parseSettings(rawConfig(t, map[string]any{"sharedContainerRegistries": []string{"no"}}))
	if refusal != `Refused: sharedContainerRegistries holds "no", which is not an Azure Container Registry name (5 to 50 letters and digits, such as myregistry), its address or its resource id.` {
		t.Fatal(refusal)
	}
	for _, stale := range []any{[]string{"no"}, 42, "x", nil} {
		if _, refusal := parseSettings(rawConfig(t, map[string]any{staleKey: stale})); refusal != "" {
			t.Fatalf("a stale %s %v refused: %s", staleKey, stale, refusal)
		}
	}
}

// No string the plugin's source writes names the old setting or calls anything old.
func TestTheSourceNeverNamesTheOldSetting(t *testing.T) {
	files, _ := os.ReadDir(".")
	fset := token.NewFileSet()
	for _, e := range files {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, e.Name(), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if n, ok := n.(*ast.BasicLit); ok && n.Kind == token.STRING {
				s, _ := strconv.Unquote(n.Value)
				l := strings.ToLower(s)
				if strings.Contains(l, strings.ToLower(staleKey)) || strings.Contains(l, "old name") || strings.Contains(l, "(old)") {
					t.Errorf("%s writes %q", fset.Position(n.Pos()), s)
				}
			}
			return true
		})
	}
}

// The README's upgrade note says to re-enter the registries; neither it nor the skill calls
// anything "old name" or "(old)", and the skill does not name the dropped setting.
func TestTheDocsSayToReEnterTheRegistries(t *testing.T) {
	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	skill, err := os.ReadFile("skills/plugin-azure-plugin.md")
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, string(readme), "# Azure 3.3.0", "### Upgrading to 3.2.0",
		"- Registries listed under `sharedRegistries`, the setting's former name, must be re-entered under **Shared container registries**.")
	if strings.Count(string(readme), staleKey) != 1 {
		t.Fatalf("the README names %s outside its upgrade note", staleKey)
	}
	if strings.Contains(string(skill), staleKey) {
		t.Fatalf("the skill names %s", staleKey)
	}
	for name, text := range map[string]string{"README": string(readme), "skill": string(skill)} {
		for _, gone := range []string{"old name", "(old)"} {
			if strings.Contains(strings.ToLower(text), gone) {
				t.Errorf("the %s says %q", name, gone)
			}
		}
	}
}

// rawConfig is config as the Host hands it to parseSettings.
func rawConfig(t *testing.T, cfg map[string]any) map[string]json.RawMessage {
	t.Helper()
	raw := map[string]json.RawMessage{}
	for k, v := range cfg {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		raw[k] = b
	}
	return raw
}
