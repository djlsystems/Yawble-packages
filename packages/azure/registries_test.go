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

// moveLine is the start of the one sentence that names the old setting.
const moveLine = "sharedRegistries is the old name of sharedContainerRegistries and still holds "

// A shared registry under either name takes builds and AcrPull; only the old name gets the line
// that says to move it.
func TestASharedRegistryUnderEitherNameTakesBuildsAndAcrPull(t *testing.T) {
	for _, key := range []string{"sharedContainerRegistries", "sharedRegistries"} {
		t.Run(key, func(t *testing.T) {
			w := newWorld(t)
			reg := w.foreign["registry"].ID
			cfg := apply(map[string]any{key: []string{"theirregistry"}})

			if r := runPlugin(t, runOpts{config: cfg}, "acr build theirregistry my-app:1.0 "+w.folder); !r.ok() {
				t.Fatalf("acr build on a shared registry: %s", r.text())
			}
			if r := runPlugin(t, runOpts{config: cfg}, "role assign AcrPull "+w.ident.ID+" "+reg); !r.ok() {
				t.Fatalf("AcrPull on a shared registry: %s", r.text())
			}
			var grants int
			for _, c := range w.f.callsTo(http.MethodPut, "") {
				if strings.HasPrefix(c.Path, reg+"/providers/Microsoft.Authorization/roleAssignments/") && strings.Contains(string(c.Body), roleAcrPull) {
					grants++
				}
			}
			if grants != 1 {
				t.Fatalf("AcrPull grants on the shared registry = %d", grants)
			}

			setup := runPlugin(t, runOpts{config: cfg}, "setup").text()
			whoami := runPlugin(t, runOpts{config: cfg}, "whoami").text()
			mustContain(t, setup, "- sharedContainerRegistries reachable: theirregistry")
			mustContain(t, whoami, "- sharedContainerRegistries: theirregistry")
			want := moveLine + "theirregistry: a person moves them to sharedContainerRegistries in this member's settings and empties sharedRegistries."
			for name, text := range map[string]string{"setup": setup, "whoami": whoami} {
				if has := strings.Contains(text, want); has != (key == "sharedRegistries") {
					t.Fatalf("%s with %s: move line present = %v:\n%s", name, key, has, text)
				}
			}
		})
	}
}

func TestBothRegistrySettingsAreReadTogether(t *testing.T) {
	s, refusal := parseSettings(rawConfig(t, map[string]any{
		"sharedContainerRegistries": []string{"newregistry", "bothregistry"},
		"sharedRegistries":          []string{"https://OldRegistry.azurecr.io/", "bothregistry", "/subscriptions/" + testSub + "/resourceGroups/rg/providers/Microsoft.ContainerRegistry/registries/idregistry"},
	}))
	if refusal != "" {
		t.Fatal(refusal)
	}
	if got := strings.Join(s.SharedRegistries, ","); got != "newregistry,bothregistry,oldregistry,idregistry" {
		t.Fatalf("shared registries = %s", got)
	}
	if got := strings.Join(s.OldRegistries, ","); got != "oldregistry,bothregistry,idregistry" {
		t.Fatalf("old registries = %s", got)
	}
	for _, key := range []string{"sharedContainerRegistries", "sharedRegistries"} {
		_, refusal := parseSettings(rawConfig(t, map[string]any{key: []string{"no"}}))
		if refusal != `Refused: sharedContainerRegistries holds "no", which is not an Azure Container Registry name (5 to 50 letters and digits, such as myregistry), its address or its resource id.` {
			t.Fatalf("%s: %s", key, refusal)
		}
	}
}

// TestOnlyTheMoveLineNamesTheOldSetting reads every string the plugin's source writes, and every
// answer a member on the old setting gets: sharedRegistries appears only in the line that says to
// move it.
func TestOnlyTheMoveLineNamesTheOldSetting(t *testing.T) {
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
			switch n := n.(type) {
			case *ast.ValueSpec: // the constant naming the old key is the one literal allowed
				if len(n.Names) == 1 && n.Names[0].Name == "oldRegistriesSetting" {
					return false
				}
			case *ast.BasicLit:
				if s, _ := strconv.Unquote(n.Value); n.Kind == token.STRING && strings.Contains(s, "sharedRegistries") {
					t.Errorf("%s writes %q", fset.Position(n.Pos()), s)
				}
			case *ast.FuncDecl: // the old key is used to read the setting and to say to move it, nowhere else
				if n.Name.Name != "parseSettings" && n.Name.Name != "moveRegistries" {
					ast.Inspect(n, func(m ast.Node) bool {
						if id, ok := m.(*ast.Ident); ok && id.Name == "oldRegistriesSetting" {
							t.Errorf("%s: %s names the old setting", fset.Position(id.Pos()), n.Name.Name)
						}
						return true
					})
				}
			}
			return true
		})
	}

	w := newWorld(t)
	cfg := apply(map[string]any{"sharedRegistries": []string{"theirregistry"}})
	var text []string
	for _, line := range []string{
		"setup",
		"whoami",
		"acr build myotherregistry my-app:1.0 " + w.folder,
		"containerapp create " + ownedRG + " new-app mine-env " + acrRef("otherregistry", "my-app:1.0"),
		"containerapp create " + ownedRG + " new-app mine-env docker.io/library/nginx:1",
		"role assign Contributor " + w.ident.ID + " " + w.foreign["registry"].ID,
	} {
		text = append(text, runPlugin(t, runOpts{config: cfg}, line).text())
	}
	text = append(text, runPlugin(t, runOpts{config: apply(map[string]any{"sharedRegistries": []string{"no"}})}, "whoami").text())
	for _, l := range strings.Split(strings.Join(text, "\n"), "\n") {
		if strings.Contains(l, "sharedRegistries") && !strings.HasPrefix(strings.TrimPrefix(l, "- "), moveLine) {
			t.Errorf("a line names the old setting: %s", l)
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
