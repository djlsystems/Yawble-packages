package main

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestThePinnedBicepCompiles runs the real compiler when one is kept in .bicep-cache/<version>/ (a
// person copies it there to run this test); it needs no network, and skips otherwise.
func TestThePinnedBicepCompiles(t *testing.T) {
	pin, ok := bicepPins[runtime.GOARCH]
	cached, _ := filepath.Abs(filepath.Join(".bicep-cache", bicepVersion, pin.file))
	if sum, err := fileSHA256(cached); runtime.GOOS != "linux" || !ok || err != nil || sum != pin.sha256 {
		t.Skip("no pinned bicep in .bicep-cache")
	}
	work := t.TempDir()
	dir := filepath.Join(work, ".bicep", bicepVersion)
	_ = os.MkdirAll(dir, 0o755)
	if err := os.Symlink(cached, filepath.Join(dir, pin.file)); err != nil {
		t.Fatal(err)
	}
	p := &plugin{out: &strings.Builder{}, work: work}

	src := filepath.Join(t.TempDir(), "main.bicep")
	_ = os.WriteFile(src, []byte("param name string = 'app-id'\nresource id 'Microsoft.ManagedIdentity/userAssignedIdentities@2023-01-31' = {\n  name: name\n  location: 'westeurope'\n}\n"), 0o644)
	out, err := compileBicep(p, src)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "deploymentTemplate.json") || !strings.Contains(string(out), "Microsoft.ManagedIdentity/userAssignedIdentities") {
		t.Fatalf("compiled = %s", out)
	}

	_ = os.WriteFile(src, []byte("resource broken 'nope' = {\n"), 0o644)
	if _, err := compileBicep(p, src); err == nil || !strings.Contains(err.Error(), "bicep could not compile main.bicep") {
		t.Fatalf("a broken file = %v", err)
	}
}

// pinFake points the bicep pin at a fake release binary the fake serves.
func pinFake(t *testing.T, f *fake, served []byte) {
	sum := sha256.Sum256([]byte("the pinned compiler"))
	oldPins, oldArch := bicepPins, bicepArch
	bicepPins = map[string]struct{ file, sha256 string }{"amd64": {"bicep-linux-x64", hex.EncodeToString(sum[:])}}
	bicepArch = "amd64"
	t.Cleanup(func() { bicepPins, bicepArch = oldPins, oldArch })
	f.hosts["github.com"] = func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("the release download carried a token")
		}
		if !strings.HasSuffix(r.URL.Path, "/Azure/bicep/releases/download/"+bicepVersion+"/bicep-linux-x64") {
			w.WriteHeader(404)
			return
		}
		_, _ = w.Write(served)
	}
}

func TestBicepIsFetchedOnceAndCheckedBySHA256(t *testing.T) {
	f := newFake(t)
	pinFake(t, f, []byte("the pinned compiler"))
	work := t.TempDir()
	p := &plugin{out: &strings.Builder{}, work: work}

	bin, err := p.bicep()
	if err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(bin); string(raw) != "the pinned compiler" || !strings.HasPrefix(bin, work) {
		t.Fatalf("kept %s = %q", bin, raw)
	}
	if _, err := p.bicep(); err != nil || len(f.calls) != 1 {
		t.Fatalf("a second use downloaded again: %d downloads, %v", len(f.calls), err)
	}

	// A kept binary that changed is fetched again.
	_ = os.WriteFile(bin, []byte("changed"), 0o755)
	if _, err := p.bicep(); err != nil || len(f.calls) != 2 {
		t.Fatalf("a changed binary was used: %d downloads, %v", len(f.calls), err)
	}
}

func TestABicepDownloadWithTheWrongSHA256IsRefused(t *testing.T) {
	f := newFake(t)
	pinFake(t, f, []byte("something else"))
	p := &plugin{out: &strings.Builder{}, work: t.TempDir()}

	_, err := p.bicep()
	if err == nil || !strings.Contains(err.Error(), "is not the pinned "+bicepVersion) {
		t.Fatalf("err = %v", err)
	}
	if matches, _ := filepath.Glob(filepath.Join(p.work, ".bicep", "*", "*")); len(matches) != 0 {
		t.Fatalf("kept %v", matches)
	}
}
