package main

import (
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"regexp"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
)

// burstQuiet is how long the burst fake waits for its server to say nothing more before it sends
// what it holds.
const burstQuiet = 25 * time.Millisecond

// burstConn is the server's side of a connection that answers pipelined commands in one burst, as
// Dovecot can: it holds every response until the server has been quiet for burstQuiet, then sends
// all the untagged responses and only after them the tagged completions. A continuation request
// ("+") goes at once, with what was held before it, since the client waits for it.
type burstConn struct {
	net.Conn
	mu       sync.Mutex
	partial  []byte // a response not yet complete
	untagged bytes.Buffer
	tagged   bytes.Buffer
	timer    *time.Timer
}

var literalAtEnd = regexp.MustCompile(`~?\{(\d+)\+?\}\r\n$`)

func newBurstConn(c net.Conn) *burstConn {
	b := &burstConn{Conn: c}
	b.timer = time.AfterFunc(time.Hour, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		b.flush()
	})
	b.timer.Stop()
	return b
}

func (b *burstConn) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.partial = append(b.partial, p...)
	for {
		n := responseLen(b.partial)
		if n == 0 {
			break
		}
		resp := b.partial[:n]
		switch {
		case bytes.HasPrefix(resp, []byte("+")):
			b.flush()
			if _, err := b.Conn.Write(resp); err != nil {
				return 0, err
			}
		case bytes.HasPrefix(resp, []byte("*")):
			b.untagged.Write(resp)
		default:
			b.tagged.Write(resp)
		}
		b.partial = append([]byte(nil), b.partial[n:]...)
	}
	b.timer.Reset(burstQuiet)
	return len(p), nil
}

// responseLen answers the length of the first complete response in buf, literals included, or 0.
func responseLen(buf []byte) int {
	at := 0
	for {
		eol := bytes.Index(buf[at:], []byte("\r\n"))
		if eol < 0 {
			return 0
		}
		end := at + eol + 2
		m := literalAtEnd.FindSubmatch(buf[at:end])
		if m == nil {
			return end
		}
		size, _ := strconv.Atoi(string(m[1]))
		if len(buf) < end+size {
			return 0
		}
		at = end + size
	}
}

// flush sends what is held: the untagged responses, then the tagged ones. b.mu is held.
func (b *burstConn) flush() {
	if b.untagged.Len()+b.tagged.Len() == 0 {
		return
	}
	out := append(b.untagged.Bytes(), b.tagged.Bytes()...)
	_, _ = b.Conn.Write(out)
	b.untagged.Reset()
	b.tagged.Reset()
}

func (b *burstConn) Close() error {
	b.mu.Lock()
	b.timer.Stop()
	b.flush()
	b.mu.Unlock()
	return b.Conn.Close()
}

type burstListener struct{ net.Listener }

func (l burstListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return newBurstConn(c), nil
}

// newBurstIMAP is the fake IMAP server over TLS, its folders those of newFakeIMAP, answering
// pipelined commands in one burst. The burst sits inside TLS, so the server cannot see the TLS
// connection itself and is told sign-in is safe: the client still only ever speaks TLS to it.
func newBurstIMAP(t *testing.T) *fakeIMAP {
	t.Helper()
	cfg := testTLS(t)
	mem := imapmemserver.New()
	user := imapmemserver.NewUser(imapUser, appPassword)
	f := &fakeIMAP{user: user, sec: "TLS", folders: []string{"INBOX", "Drafts", "Archive", "Receipts"}}
	for _, box := range f.folders {
		if err := user.Create(box, nil); err != nil {
			t.Fatal(err)
		}
	}
	mem.AddUser(user)
	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewSession(), nil, nil
		},
		Caps:         imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapIMAP4rev2: {}},
		Logger:       log.New(io.Discard, "", 0),
		InsecureAuth: true,
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(burstListener{tls.NewListener(ln, cfg)}) }()
	t.Cleanup(func() { _ = srv.Close() })
	f.port = ln.Addr().(*net.TCPAddr).Port
	return f
}

// fillInbox delivers n messages, the same on every fake: plain text, HTML only, both, and with
// an attachment and no text at all, so every way to a snippet is fetched.
func fillInbox(t *testing.T, f *fakeIMAP, n int, base time.Time) {
	t.Helper()
	for i := 0; i < n; i++ {
		when := base.Add(time.Duration(i) * time.Minute)
		from, subject := fmt.Sprintf("sender%d@example.org", i), fmt.Sprintf("Message %d", i)
		var raw string
		switch i % 4 {
		case 0:
			raw = rawMail(from, subject, fmt.Sprintf("Plain body of message %d", i), "", when, "")
		case 1:
			raw = rawMail(from, subject, "", fmt.Sprintf("<p>HTML <b>body</b> of message %d</p>", i), when, "")
		case 2:
			raw = rawMail(from, subject, fmt.Sprintf("Text part of message %d", i), "<p>ignored</p>", when, "report.pdf")
		default:
			raw = attachmentOnly(from, subject, when)
		}
		f.deliver(t, "INBOX", raw, when, i%3 == 0)
	}
}

// attachmentOnly is a message whose one part is a PDF: it has no text for a snippet.
func attachmentOnly(from, subject string, when time.Time) string {
	return fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nMIME-Version: 1.0\r\n"+
		"Content-Type: multipart/mixed; boundary=\"outer\"\r\n\r\n--outer\r\n"+
		"Content-Type: application/pdf; name=\"only.pdf\"\r\nContent-Disposition: attachment; filename=\"only.pdf\"\r\nContent-Transfer-Encoding: base64\r\n\r\n"+
		"%s\r\n--outer--\r\n", from, imapAccount, subject, when.Format(time.RFC1123Z), base64.StdEncoding.EncodeToString([]byte("%PDF-1.4 fake")))
}

// within runs a command, failing the test if it has not answered in time rather than stalling.
func within(t *testing.T, d time.Duration, run func() outcome) outcome {
	t.Helper()
	done := make(chan outcome, 1)
	go func() { done <- run() }()
	select {
	case o := <-done:
		return o
	case <-time.After(d):
		t.Fatalf("no answer in %s: the IMAP client hung on a burst of pipelined replies", d)
		return outcome{}
	}
}

func TestIMAPListsAndWatchesWhenTheServerAnswersPipelinedCommandsInOneBurst(t *testing.T) {
	for _, n := range []int{2, 6, 50} {
		t.Run(strconv.Itoa(n), func(t *testing.T) {
			base := time.Now().Add(-48 * time.Hour).Truncate(time.Second)
			one, burst := newFakeIMAP(t, "TLS"), newBurstIMAP(t)
			config := map[string]any{"maxResults": float64(100)}
			fillInbox(t, one, n, base)
			fillInbox(t, burst, n, base)

			for _, instr := range []string{"list max:100", "search from:example.org"} {
				want := runIt(t, instr, opts{conn: imapConnection(one, nil, appPassword), config: config, maxResults: 100})
				mustOK(t, want)
				contains(t, want.output, fmt.Sprintf("%d messages", n), "Plain body of message 0")
				got := within(t, 20*time.Second, func() outcome {
					return runIt(t, instr, opts{conn: imapConnection(burst, nil, appPassword), config: config, maxResults: 100})
				})
				mustOK(t, got)
				if got.output != want.output {
					t.Fatalf("%s: the burst answer differs from one-at-a-time answering.\nwant:\n%s\ngot:\n%s", instr, want.output, got.output)
				}
			}

			// A watch check: the first starts from now, the second lists what arrived since.
			later := base.Add(24 * time.Hour)
			stOne, stBurst := newSiteStore(), newSiteStore()
			mustOK(t, stOne.run(t, "watch", opts{conn: imapConnection(one, nil, appPassword), maxResults: 100}))
			mustOK(t, within(t, 20*time.Second, func() outcome {
				return stBurst.run(t, "watch", opts{conn: imapConnection(burst, nil, appPassword), maxResults: 100})
			}))
			fillInbox(t, one, n, later)
			fillInbox(t, burst, n, later)
			want := stOne.run(t, "watch", opts{conn: imapConnection(one, nil, appPassword), maxResults: 100})
			mustOK(t, want)
			got := within(t, 20*time.Second, func() outcome {
				return stBurst.run(t, "watch", opts{conn: imapConnection(burst, nil, appPassword), maxResults: 100})
			})
			mustOK(t, got)
			wp, _ := json.Marshal(event(t, want)["payload"])
			gp, _ := json.Marshal(event(t, got)["payload"])
			if got.output != want.output || string(gp) != string(wp) {
				t.Fatalf("watch: the burst answer differs from one-at-a-time answering.\nwant:\n%s\n%s\ngot:\n%s\n%s", want.output, wp, got.output, gp)
			}
		})
	}
}
