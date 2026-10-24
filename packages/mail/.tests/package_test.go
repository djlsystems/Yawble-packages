// The Mail solution's own checks; the plugin it ships, mail-plugin, has its tests in its package.
package mailtest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pkgDir is the package folder; the plugin package it ships, mail-plugin, sits beside it.
const pkgDir = ".."

func readJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

// The description says a reply is threaded, as the plugin's does.
func TestTheDescriptionSaysAReplyIsThreaded(t *testing.T) {
	var sol struct{ Description string }
	readJSON(t, filepath.Join(pkgDir, "solution.json"), &sol)
	if !strings.Contains(sol.Description, "reply") || !strings.Contains(sol.Description, "threaded") {
		t.Fatalf("solution.json's description must say a reply is threaded: %q", sol.Description)
	}
}

// Mailer is hired on the plugin the package ships, and that plugin is built from its plugin package.
func TestMailerIsTheShippedPluginPackage(t *testing.T) {
	var sol struct {
		Members []struct {
			Name     string `json:"name"`
			PluginID string `json:"pluginId"`
		} `json:"members"`
	}
	readJSON(t, filepath.Join(pkgDir, "solution.json"), &sol)
	var plugin struct{ ID string }
	readJSON(t, filepath.Join(pkgDir, "..", "mail-plugin", "plugin.json"), &plugin)
	for _, m := range sol.Members {
		if m.Name == "Mailer" {
			if m.PluginID != plugin.ID {
				t.Fatalf("Mailer is hired on %q; the plugin package is %q", m.PluginID, plugin.ID)
			}
			script, err := os.ReadFile(filepath.Join(pkgDir, "plugins", plugin.ID, "build.sh"))
			if err != nil {
				t.Fatal(err)
			}
			if want := `exec sh "$here/../../../` + plugin.ID + `/build.sh"`; !strings.Contains(string(script), want) {
				t.Fatalf("plugins/%s/build.sh does not run the plugin package's build: want %s", plugin.ID, want)
			}
			return
		}
	}
	t.Fatal("no member Mailer")
}
