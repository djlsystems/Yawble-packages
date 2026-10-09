package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const tag = "catalog-2026.10.08.1"

var at = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

// write lays out files under dir; a name ending in * is made executable (the * is dropped).
func write(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		mode := os.FileMode(0o644)
		if strings.HasSuffix(name, "*") {
			name, mode = strings.TrimSuffix(name, "*"), 0o755
		}
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
}

const goPlugin = `{
  "schemaVersion": 1, "id": "hello", "name": "Hello", "version": "1.2.3",
  "description": "Says hello. It never sends anything.",
  "protocol": "harness.member/1",
  "executable": { "path": "bin/linux-x64/hello" },
  "platforms": { "linux-x64": "bin/linux-x64/hello", "linux-arm64": "bin/linux-arm64/hello" },
  "config": {
    "greeting": { "type": "string", "default": "hi", "description": "What it says." },
    "allow": { "type": "list", "setBy": "person", "description": "Who it may greet." }
  },
  "secrets": { "token": { "description": "The service token." } },
  "connections": { "account": { "description": "The account to greet from.", "providers": ["google", "microsoft"], "required": true } }
}`

const scriptPlugin = `{
  "schemaVersion": 1, "id": "board", "name": "Board", "version": "0.2.0",
  "description": "A stand-in board.",
  "protocol": "harness.member/1",
  "executable": { "path": "board" },
  "requires": ["python3"],
  "secrets": { "apiId": { "description": "The board's id." }, "apiKey": { "description": "The board's key." } }
}`

const solution = `{
  "format": 1, "id": "team", "name": "Team", "version": "2.0.0",
  "description": "A whole team! It greets people and reads a board.",
  "members": [
    { "name": "Boss", "role": "manager" },
    { "name": "Greeter", "pluginId": "hello", "secrets": { "token": "HELLO_TOKEN" } },
    { "name": "Reader", "pluginId": "board", "secrets": { "apiKey": "BOARD_KEY", "apiId": "BOARD_ID" } }
  ],
  "inputs": {
    "connections": [ { "member": "greeter", "slot": "account", "description": "Your account.", "required": true } ],
    "settings": [ { "member": "Greeter", "setting": "allow", "description": "Who may be greeted.", "required": false } ],
    "documents": [ { "folder": "Resume", "description": "Your resume.", "required": true } ]
  }
}`

func buildZip(t *testing.T, dist, name string, files map[string]string) string {
	t.Helper()
	src := t.TempDir()
	write(t, src, files)
	z := filepath.Join(dist, name)
	if err := writeZip(src, z); err != nil {
		t.Fatal(err)
	}
	return z
}

func readCatalog(t *testing.T, dist string) (catalog, map[string]any) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dist, "catalog.json"))
	if err != nil {
		t.Fatal(err)
	}
	var c catalog
	var raw map[string]any
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	return c, raw
}

func TestTheSameFolderZipsToTheSameBytesWithTheFolderAsTheRoot(t *testing.T) {
	files := map[string]string{"solution.json": "{}", "tools/run.py*": "print(1)", "sites/a/index.html": "<p>"}
	a, b := t.TempDir(), t.TempDir()
	write(t, a, files)
	write(t, b, files)
	old := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)
	for _, p := range []string{"solution.json", "tools/run.py", "sites/a/index.html"} {
		os.Chtimes(filepath.Join(a, p), old, old)
	}
	out := t.TempDir()
	if err := writeZip(a, filepath.Join(out, "a.zip")); err != nil {
		t.Fatal(err)
	}
	if err := writeZip(b, filepath.Join(out, "b.zip")); err != nil {
		t.Fatal(err)
	}
	za, _ := os.ReadFile(filepath.Join(out, "a.zip"))
	zb, _ := os.ReadFile(filepath.Join(out, "b.zip"))
	if !bytes.Equal(za, zb) {
		t.Fatal("the same content made different zips")
	}
	zr, err := zip.NewReader(bytes.NewReader(za), int64(len(za)))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	modes := map[string]os.FileMode{}
	for _, f := range zr.File {
		names = append(names, f.Name)
		modes[f.Name] = f.Mode().Perm()
	}
	want := []string{"sites/", "sites/a/", "sites/a/index.html", "solution.json", "tools/", "tools/run.py"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("entries %v, want %v", names, want)
	}
	if modes["tools/run.py"] != 0o755 || modes["solution.json"] != 0o644 {
		t.Fatalf("modes %v", modes)
	}
	for _, f := range zr.File {
		if !f.Modified.Equal(zipTime) {
			t.Fatalf("%s is dated %v, not the fixed %v", f.Name, f.Modified, zipTime)
		}
	}
}

func TestALinkIsRefusedFromAZip(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, map[string]string{"plugin.json": "{}"})
	os.Symlink("/etc/passwd", filepath.Join(dir, "leak"))
	if err := writeZip(dir, filepath.Join(t.TempDir(), "x.zip")); err == nil || !strings.Contains(err.Error(), "link") {
		t.Fatalf("got %v, want a link refusal", err)
	}
}

func TestStageLeavesOutPluginSourcesAndDotFiles(t *testing.T) {
	src := filepath.Join(t.TempDir(), "team")
	write(t, src, map[string]string{
		"solution.json": solution, "README.md": "r", ".gitignore": "bin/",
		"tools/go.py*": "x", "plugins/hello/main.go": "package main", "plugins/hello/build.sh*": "",
	})
	out := filepath.Join(t.TempDir(), "team")
	if err := stage(src, out); err != nil {
		t.Fatal(err)
	}
	var got []string
	filepath.WalkDir(out, func(p string, d os.DirEntry, err error) error {
		if !d.IsDir() {
			r, _ := filepath.Rel(out, p)
			got = append(got, filepath.ToSlash(r))
		}
		return nil
	})
	if want := []string{"README.md", "solution.json", "tools/go.py"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("staged %v, want %v", got, want)
	}
	if info, _ := os.Stat(filepath.Join(out, "tools/go.py")); info.Mode()&0o111 == 0 {
		t.Fatalf("tools/go.py lost its execute bit: %v", info.Mode())
	}
}

func TestInfoRefusesAFolderNotNamedAfterItsId(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "hello-world")
	write(t, dir, map[string]string{"plugin.json": goPlugin})
	if _, err := readPackage(dir); err == nil || !strings.Contains(err.Error(), "name the folder after the id") {
		t.Fatalf("got %v", err)
	}
	good := filepath.Join(t.TempDir(), "hello")
	write(t, good, map[string]string{"plugin.json": goPlugin})
	if p, err := readPackage(good); err != nil || p != (pkg{"plugin", "hello", "1.2.3"}) {
		t.Fatalf("got %+v, %v", p, err)
	}
}

func TestAPluginZipsEntryIsReadFromItsManifestAndTheZip(t *testing.T) {
	dist := t.TempDir()
	z := buildZip(t, dist, "hello-1.2.3.zip", map[string]string{
		"plugin.json": goPlugin, "bin/linux-x64/hello*": "x64", "bin/linux-arm64/hello*": "arm64",
	})
	if err := writeCatalog(tag, dist, at); err != nil {
		t.Fatal(err)
	}
	c, _ := readCatalog(t, dist)
	data, _ := os.ReadFile(z)
	sum := sha256.Sum256(data)
	want := entry{
		ID: "hello", Kind: "plugin", Name: "Hello", Version: "1.2.3",
		Summary: "Says hello.", Description: "Says hello. It never sends anything.",
		Needs: needs{
			Connections: []connectionNeed{{"account", []string{"google", "microsoft"}, true, "The account to greet from."}},
			Secrets:     []secretNeed{{"token", "The service token."}},
			Inputs:      []inputNeed{{"allow", "setting", false, "Who it may greet."}},
			Runtimes:    []string{},
		},
		Plugins:   []pluginRef{{"hello", "1.2.3"}},
		Platforms: []string{"linux-x64", "linux-arm64"},
		Download: download{
			URL:    "https://github.com/djlsystems/Yawble-packages/releases/download/catalog-2026.10.08.1/hello-1.2.3.zip",
			SHA256: hex.EncodeToString(sum[:]), Bytes: int64(len(data)),
		},
		Source: "https://github.com/djlsystems/Yawble-packages/tree/main/packages/hello",
	}
	if c.Schema != 1 || c.GeneratedAt != "2026-10-08T12:00:00Z" || len(c.Packages) != 1 {
		t.Fatalf("catalog %+v", c)
	}
	if !reflect.DeepEqual(c.Packages[0], want) {
		t.Fatalf("entry\n got %+v\nwant %+v", c.Packages[0], want)
	}
}

func TestASolutionZipsEntryReadsItsPluginsAndWhatOnlyAPersonProvides(t *testing.T) {
	dist := t.TempDir()
	buildZip(t, dist, "team-2.0.0.zip", map[string]string{
		"solution.json":                        solution,
		"plugins/hello/plugin.json":            goPlugin,
		"plugins/hello/bin/linux-x64/hello*":   "x64",
		"plugins/hello/bin/linux-arm64/hello*": "arm64",
		"plugins/board/plugin.json":            scriptPlugin,
		"plugins/board/board*":                 "#!/usr/bin/env python3",
	})
	if err := writeCatalog(tag, dist, at); err != nil {
		t.Fatal(err)
	}
	c, _ := readCatalog(t, dist)
	e := c.Packages[0]
	if e.Kind != "solution" || e.Summary != "A whole team!" {
		t.Fatalf("kind %q summary %q", e.Kind, e.Summary)
	}
	wantNeeds := needs{
		Connections: []connectionNeed{{"account", []string{"google", "microsoft"}, true, "Your account."}},
		Secrets: []secretNeed{
			{"HELLO_TOKEN", "The service token."}, {"BOARD_KEY", "The board's key."}, {"BOARD_ID", "The board's id."},
		},
		Inputs: []inputNeed{
			{"Resume", "documents", true, "Your resume."}, {"allow", "setting", false, "Who may be greeted."},
		},
		Runtimes: []string{"python3"},
	}
	if !reflect.DeepEqual(e.Needs, wantNeeds) {
		t.Fatalf("needs\n got %+v\nwant %+v", e.Needs, wantNeeds)
	}
	if want := []pluginRef{{"board", "0.2.0"}, {"hello", "1.2.3"}}; !reflect.DeepEqual(e.Plugins, want) {
		t.Fatalf("plugins %+v", e.Plugins)
	}
	// The script plugin has no platforms and narrows nothing.
	if want := []string{"linux-x64", "linux-arm64"}; !reflect.DeepEqual(e.Platforms, want) {
		t.Fatalf("platforms %v", e.Platforms)
	}
}

func TestAFieldAPackageHasNothingForIsAnEmptyListNeverNullOrInvented(t *testing.T) {
	dist := t.TempDir()
	buildZip(t, dist, "board-0.2.0.zip", map[string]string{"plugin.json": strings.Replace(scriptPlugin, `"requires": ["python3"],`, "", 1), "board*": "#!"})
	if err := writeCatalog(tag, dist, at); err != nil {
		t.Fatal(err)
	}
	_, raw := readCatalog(t, dist)
	e := raw["packages"].([]any)[0].(map[string]any)
	n := e["needs"].(map[string]any)
	for name, v := range map[string]any{"connections": n["connections"], "inputs": n["inputs"], "runtimes": n["runtimes"], "platforms": e["platforms"]} {
		if l, ok := v.([]any); !ok || len(l) != 0 {
			t.Errorf("%s is %#v, want []", name, v)
		}
	}
}

func TestTheCatalogRefusesWhatItCannotReadTruthfully(t *testing.T) {
	cases := []struct {
		name, zip string
		files     map[string]string
		tag, want string
	}{
		{"a zip not named after its manifest", "hello-1.0.0.zip",
			map[string]string{"plugin.json": goPlugin, "bin/linux-x64/hello*": "", "bin/linux-arm64/hello*": ""}, tag, "must be named hello-1.2.3.zip"},
		{"a platform the build did not make", "hello-1.2.3.zip",
			map[string]string{"plugin.json": goPlugin, "bin/linux-x64/hello*": ""}, tag, "bin/linux-arm64/hello for linux-arm64, which the build did not make"},
		{"HTML in the description", "board-0.2.0.zip",
			map[string]string{"plugin.json": strings.Replace(scriptPlugin, "A stand-in board.", "A <b>bold</b> board.", 1), "board*": ""}, tag, "holds HTML"},
		{"a tag that is not catalog-<date>.<n>", "board-0.2.0.zip",
			map[string]string{"plugin.json": scriptPlugin, "board*": ""}, "v1", "is not catalog-"},
		{"a plugin member whose plugin is not shipped", "team-2.0.0.zip",
			map[string]string{"solution.json": solution, "plugins/board/plugin.json": scriptPlugin, "plugins/board/board*": ""}, tag, "plugin hello, which the zip does not hold"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dist := t.TempDir()
			buildZip(t, dist, c.zip, c.files)
			err := writeCatalog(c.tag, dist, at)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want %q", err, c.want)
			}
			if _, statErr := os.Stat(filepath.Join(dist, "catalog.json")); statErr == nil {
				t.Fatal("a refused catalog was written")
			}
		})
	}
}

func TestTheSummaryIsTheDescriptionsFirstSentence(t *testing.T) {
	for in, want := range map[string]string{
		"One sentence only":      "One sentence only",
		"First one. Second one.": "First one.",
		"An app password (Gmail, iCloud): it reads. Then more.": "An app password (Gmail, iCloud): it reads.",
		"Version 2.0.2 ships. More.":                            "Version 2.0.2 ships.",
		"First paragraph\n\nSecond paragraph.":                  "First paragraph",
		"  Spread\n  over lines. Next.":                         "Spread over lines.",
	} {
		if got := firstSentence(in); got != want {
			t.Errorf("firstSentence(%q) = %q, want %q", in, got, want)
		}
	}
}
