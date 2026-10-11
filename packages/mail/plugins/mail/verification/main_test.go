package mailverify

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestMain builds the plugin from the folder above this one when MAIL_BIN does not name a built
// binary, so `go test ./...` checks the code beside it; MAIL_BIN checks a packaged binary instead.
func TestMain(m *testing.M) {
	if os.Getenv("MAIL_BIN") == "" {
		dir, err := os.MkdirTemp("", "mailverify")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		bin := filepath.Join(dir, "mail")
		build := exec.Command("go", "build", "-o", bin, ".")
		build.Dir = ".."
		build.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := build.CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "building the plugin: %v\n%s", err, out)
			os.Exit(1)
		}
		os.Setenv("MAIL_BIN", bin)
		code := m.Run()
		os.RemoveAll(dir)
		os.Exit(code)
	}
	os.Exit(m.Run())
}
