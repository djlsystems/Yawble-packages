package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// No sentence in the plugin package - code, manifest, README, skill - says a key is never set in
// settings: this member's settings on the screen are the first place to set one.
func TestNoSentenceSaysNeverInSettings(t *testing.T) {
	// One sentence: no full stop, and in Go or JSON no string boundary, between the words.
	never := regexp.MustCompile("(?i)\\bnever\\b[^.\"`]*?\\bin(?:to)?\\b[^.\"`]*?\\bsettings\\b")
	pkg, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	err = filepath.WalkDir(pkg, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "bin" || d.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		switch filepath.Ext(path) {
		case ".go", ".json", ".md", ".js", ".html":
		default:
			return nil
		}
		if strings.HasSuffix(path, "_test.go") {
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
	if checked < 10 {
		t.Fatalf("checked only %d files under %s", checked, pkg)
	}
}
