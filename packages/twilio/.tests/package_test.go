// The Twilio solution's own checks; the plugin it ships, twilio-plugin, has its tests in its package.
package twiliotest

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// pkgDir is the package folder; the plugin package it ships, twilio-plugin, sits beside it.
const pkgDir = ".."

// No sentence in the package - manifest, README, skills, site - says a key is never set in
// settings: the twilio member's settings on the screen are the first place to set one.
func TestNoSentenceSaysNeverInSettings(t *testing.T) {
	// One sentence: no full stop, and in JSON no string boundary, between the words.
	never := regexp.MustCompile("(?i)\\bnever\\b[^.\"`]*?\\bin(?:to)?\\b[^.\"`]*?\\bsettings\\b")
	checked := 0
	err := filepath.WalkDir(pkgDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".tests" || d.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		switch filepath.Ext(path) {
		case ".json", ".md", ".js", ".html":
		default:
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		checked++
		if m := never.Find(b); m != nil {
			t.Errorf("%s says %q", path, m)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked < 4 {
		t.Fatalf("checked only %d files under %s", checked, pkgDir)
	}
}

// The twilio member is hired on the plugin the package ships, built from its plugin package.
func TestTheMemberIsTheShippedPluginPackage(t *testing.T) {
	sol, err := os.ReadFile(filepath.Join(pkgDir, "solution.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(sol), `"pluginId": "twilio-plugin"`) {
		t.Fatal(`solution.json hires no member on "twilio-plugin"`)
	}
	script, err := os.ReadFile(filepath.Join(pkgDir, "plugins", "twilio-plugin", "build.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if want := `exec sh "$here/../../../twilio-plugin/build.sh"`; !strings.Contains(string(script), want) {
		t.Fatalf("plugins/twilio-plugin/build.sh does not run the plugin package's build: want %s", want)
	}
	if _, err := os.Stat(filepath.Join(pkgDir, "..", "twilio-plugin", "plugin.json")); err != nil {
		t.Fatal(err)
	}
}
