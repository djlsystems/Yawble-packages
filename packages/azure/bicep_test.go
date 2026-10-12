package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestThePinnedBicepCompiles runs the real compiler build.sh keeps in .bicep-cache, when a build has
// fetched it; it needs no network.
func TestThePinnedBicepCompiles(t *testing.T) {
	arch := map[string]string{"amd64": "x64", "arm64": "arm64"}[runtime.GOARCH]
	matches, _ := filepath.Glob(filepath.Join(".bicep-cache", "*", "bicep-linux-"+arch))
	if runtime.GOOS != "linux" || len(matches) == 0 {
		t.Skip("no pinned bicep in .bicep-cache; build.sh fetches it")
	}
	bin, _ := filepath.Abs(matches[0])
	old := bicepPath
	bicepPath = func() string { return bin }
	t.Cleanup(func() { bicepPath = old })

	dir := t.TempDir()
	src := filepath.Join(dir, "main.bicep")
	_ = os.WriteFile(src, []byte("param name string = 'app-id'\nresource id 'Microsoft.ManagedIdentity/userAssignedIdentities@2023-01-31' = {\n  name: name\n  location: 'westeurope'\n}\n"), 0o644)
	out, err := compileBicep(src)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "deploymentTemplate.json") || !strings.Contains(string(out), "Microsoft.ManagedIdentity/userAssignedIdentities") {
		t.Fatalf("compiled = %s", out)
	}

	_ = os.WriteFile(src, []byte("resource broken 'nope' = {\n"), 0o644)
	if _, err := compileBicep(src); err == nil || !strings.Contains(err.Error(), "bicep could not compile main.bicep") {
		t.Fatalf("a broken file = %v", err)
	}
}
