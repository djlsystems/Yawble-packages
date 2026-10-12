// fetch downloads one file and keeps it only when its sha256 is the one given. build.sh uses it for
// the pinned bicep binaries, so the build needs only Go and sh.
//
//	go run ./tools/fetch <url> <sha256> <out>
//
// An <out> that already holds those bytes is kept as it is, with no download.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	if len(os.Args) != 4 {
		fmt.Fprintln(os.Stderr, "usage: fetch <url> <sha256> <out>")
		os.Exit(2)
	}
	url, want, out := os.Args[1], strings.ToLower(os.Args[2]), os.Args[3]
	if sum, err := fileSum(out); err == nil && sum == want {
		return
	}
	if err := download(url, want, out); err != nil {
		fmt.Fprintln(os.Stderr, "fetch:", err)
		os.Exit(1)
	}
}

func fileSum(path string) (string, error) {
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

func download(url, want, out string) error {
	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %s", url, resp.Status)
	}
	tmp := out + ".part"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, h), resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		os.Remove(tmp)
		return fmt.Errorf("%s has sha256 %s, not the pinned %s; nothing was kept", url, got, want)
	}
	return os.Rename(tmp, out)
}
