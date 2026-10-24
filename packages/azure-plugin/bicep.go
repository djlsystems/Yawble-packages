package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// The bicep compiler is pinned by version and by the sha256 of each platform's release binary. It is
// too large to ship inside the package (about 120 MB a platform, over the Marketplace's 100 MB
// limit for a whole package), so the plugin downloads its own platform's binary from the release the
// first time a Bicep file is deployed, refuses it unless its sha256 is the pinned one, and keeps it
// in the member's working folder. Every later use checks the sha256 again before running it.
const bicepVersion = "v0.48.1"

var (
	bicepRelease = "https://github.com/Azure/bicep/releases/download/" + bicepVersion + "/"
	bicepPins    = map[string]struct{ file, sha256 string }{
		"amd64": {"bicep-linux-x64", "b09ec25a9d376c1f8e33ede6ed22b587f915ad68488d5db77a6f9541748c7f6e"},
		"arm64": {"bicep-linux-arm64", "9cbf6a211137e894a863fbc842e355c2197fc2124a7bda902afe1bceffa35245"},
	}
	bicepArch = runtime.GOARCH
)

// bicep answers the path of the pinned compiler, downloading it the first time.
func (p *plugin) bicep() (string, error) {
	pin, ok := bicepPins[bicepArch]
	if !ok {
		return "", fmt.Errorf("there is no pinned bicep compiler for %s; deploy an ARM JSON template instead", bicepArch)
	}
	if p.work == "" {
		return "", fmt.Errorf("this run names no working folder to keep the bicep compiler in; deploy an ARM JSON template instead")
	}
	dir := filepath.Join(p.work, ".bicep", bicepVersion)
	bin := filepath.Join(dir, pin.file)
	if sum, err := fileSHA256(bin); err == nil {
		if sum == pin.sha256 {
			return bin, nil
		}
		_ = os.Remove(bin) // not the pinned binary: fetch it again
	}
	p.progress("azure: downloading the pinned bicep compiler " + bicepVersion + " (about 120 MB, once)")
	ans, err := anonymous(http.MethodGet, bicepRelease+pin.file, nil, nil, 15*time.Minute)
	if err != nil {
		return "", fmt.Errorf("the bicep compiler could not be downloaded from its release: %v; try again, or deploy an ARM JSON template", err)
	}
	sum := sha256.Sum256(ans.body)
	if hex.EncodeToString(sum[:]) != pin.sha256 {
		return "", fmt.Errorf("the downloaded bicep compiler is not the pinned %s (its sha256 differs), so it is not used", bicepVersion)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("the bicep compiler could not be kept in the working folder")
	}
	tmp := bin + ".part"
	if err := os.WriteFile(tmp, ans.body, 0o755); err != nil {
		return "", fmt.Errorf("the bicep compiler could not be kept in the working folder")
	}
	if err := os.Rename(tmp, bin); err != nil {
		return "", fmt.Errorf("the bicep compiler could not be kept in the working folder")
	}
	return bin, nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
