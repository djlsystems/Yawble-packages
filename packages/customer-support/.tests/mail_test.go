package customersupporttest

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// The published mail plugin, built from packages/mail-plugin, run as the Host runs it against a fake
// Gmail with the moveTo this package's setup gives it: rule 1's move to Junk and rule 3's move to
// New Customers, which the first move creates.

const fakeToken = "ya29.customer-support-fake-token"

var (
	buildOnce sync.Once
	mailBin   string
	buildErr  error
)

func mailPlugin(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "customer-support-mail")
		if err != nil {
			buildErr = err
			return
		}
		mailBin = filepath.Join(dir, "mail")
		cmd := exec.Command("go", "build", "-o", mailBin, ".")
		cmd.Dir = filepath.Join(pkgDir, "..", "mail-plugin")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			buildErr = err
			mailBin = string(out)
		}
	})
	if buildErr != nil {
		t.Fatalf("building the mail plugin: %v\n%s", buildErr, mailBin)
	}
	return mailBin
}

type fakeGmail struct {
	mu       sync.Mutex
	labels   []map[string]string
	created  []string
	modified map[string]string
}

func newFakeGmail(t *testing.T) *httptest.Server {
	t.Helper()
	g := &fakeGmail{
		labels: []map[string]string{
			{"id": "INBOX", "name": "INBOX", "type": "system"},
			{"id": "SPAM", "name": "SPAM", "type": "system"},
			{"id": "SENT", "name": "SENT", "type": "system"},
			{"id": "UNREAD", "name": "UNREAD", "type": "system"},
		},
		modified: map[string]string{},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+fakeToken {
			w.WriteHeader(401)
			return
		}
		path := strings.TrimPrefix(r.URL.Path, "/gmail/v1/users/me")
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && path == "/labels":
			json.NewEncoder(w).Encode(map[string]any{"labels": g.labels})
		case r.Method == "POST" && path == "/labels":
			var l map[string]any
			json.Unmarshal(body, &l)
			name, _ := l["name"].(string)
			g.created = append(g.created, name)
			label := map[string]string{"id": "Label_" + name, "name": name, "type": "user"}
			g.labels = append(g.labels, label)
			json.NewEncoder(w).Encode(label)
		case r.Method == "POST" && strings.HasPrefix(path, "/messages/") && strings.HasSuffix(path, "/modify"):
			id := strings.TrimSuffix(strings.TrimPrefix(path, "/messages/"), "/modify")
			g.modified[id] = string(body)
			json.NewEncoder(w).Encode(map[string]any{"id": id})
		default:
			w.WriteHeader(404)
			io.WriteString(w, `{"error":{"code":404,"message":"Requested entity was not found."}}`)
		}
	}))
	t.Cleanup(srv.Close)
	gmailState[srv.URL] = g
	return srv
}

var gmailState = map[string]*fakeGmail{}

// runMail runs one instruction with the package's setup default for moveTo.
func runMail(t *testing.T, base, instruction string, moveTo []string) (ok bool, text string) {
	t.Helper()
	req, _ := json.Marshal(map[string]any{
		"protocol": "harness.member/1", "plugin": map[string]any{"id": "mail-plugin", "version": "3.0.1"},
		"work":    []any{map[string]any{"seq": 1, "instruction": instruction}},
		"config":  map[string]any{"moveTo": moveTo},
		"secrets": map[string]any{},
		"connections": map[string]any{"mailbox": map[string]any{
			"provider": "google", "account": "support@example.com", "accessToken": fakeToken,
			"scopes": []string{
				"https://www.googleapis.com/auth/gmail.readonly",
				"https://www.googleapis.com/auth/gmail.compose",
				"https://www.googleapis.com/auth/gmail.modify",
			},
		}},
	})
	cmd := exec.Command(mailPlugin(t))
	cmd.Env = []string{"MAIL_PLUGIN_TEST_GMAIL_BASE=" + base, "PATH=/usr/bin:/bin"}
	cmd.Stdin = bytes.NewReader(req)
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	_ = cmd.Run()
	if strings.Contains(so.String()+se.String(), fakeToken) {
		t.Fatal("the token is in the plugin's output")
	}
	for _, line := range strings.Split(strings.TrimSpace(so.String()), "\n") {
		var rec struct {
			T, Output, Error string
			Ok               bool
		}
		if json.Unmarshal([]byte(line), &rec) == nil && rec.T == "result" {
			t.Logf("%s -> ok=%v %q %q", instruction, rec.Ok, rec.Output, rec.Error)
			return rec.Ok, rec.Output + rec.Error
		}
	}
	t.Fatalf("no result from the plugin:\n%s\n%s", so.String(), se.String())
	return false, ""
}

func setupMoveTo(t *testing.T) []string {
	t.Helper()
	s, _ := loadSolution(t)
	for _, q := range s.Setup.Settings {
		if q.Member == "Mailer" && q.Setting == "moveTo" {
			var v []string
			if err := json.Unmarshal(q.Default, &v); err != nil {
				t.Fatal(err)
			}
			return v
		}
	}
	t.Fatal("setup does not ask for moveTo")
	return nil
}

func TestMailerMovesByTheRules(t *testing.T) {
	moveTo := setupMoveTo(t)
	srv := newFakeGmail(t)
	g := gmailState[srv.URL]

	// Rule 3: New Customers does not exist yet; the first move creates it, the second uses it.
	ok, out := runMail(t, srv.URL, `move "New Customers" m-new-1`, moveTo)
	if !ok || !strings.Contains(out, "Created folder New Customers") {
		t.Fatalf("first move to New Customers: %q", out)
	}
	ok, out = runMail(t, srv.URL, `move "New Customers" m-new-2`, moveTo)
	if !ok || strings.Contains(out, "Created") {
		t.Fatalf("second move to New Customers: %q", out)
	}
	if len(g.created) != 1 || g.created[0] != "New Customers" {
		t.Fatalf("labels created: %v", g.created)
	}
	if !strings.Contains(g.modified["m-new-1"], "Label_New Customers") || !strings.Contains(g.modified["m-new-1"], "INBOX") {
		t.Fatalf("m-new-1 was modified with %s", g.modified["m-new-1"])
	}

	// Rule 1: Junk is the mailbox's own spam folder, found and never created; read state is untouched.
	ok, out = runMail(t, srv.URL, "move Junk m-spam", moveTo)
	if !ok {
		t.Fatalf("move to Junk: %q", out)
	}
	if body := g.modified["m-spam"]; !strings.Contains(body, "SPAM") || strings.Contains(body, "UNREAD") {
		t.Fatalf("m-spam was modified with %s", body)
	}
	if len(g.created) != 1 {
		t.Fatalf("labels created: %v", g.created)
	}

	// Nothing else may be moved, and nothing may be marked read.
	if ok, out = runMail(t, srv.URL, "move Archive m-other", moveTo); ok || !strings.Contains(out, "Refused: Archive is not on moveTo") {
		t.Fatalf("move to Archive: %q", out)
	}
	if ok, out = runMail(t, srv.URL, "mark-read m-other", moveTo); ok || !strings.Contains(out, "Refused: mark-read and mark-unread are off") {
		t.Fatalf("mark-read: %q", out)
	}
	if _, touched := g.modified["m-other"]; touched {
		t.Fatal("m-other was modified")
	}
}
