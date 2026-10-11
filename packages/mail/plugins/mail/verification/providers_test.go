// Black-box checks of the built binary over the two ways in 2.0 adds: an IMAP app-password
// mailbox (an in-memory IMAP server and an SMTP server, both behind TLS with a CA made for the
// run) and Microsoft Graph (httptest). Each run is a separate process; the watch's document is
// carried from one run to the next only as the Host would, through the run's site data.
package mailverify

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"
)

const (
	appPw    = "vrfy pass wxyz 0123"
	graphTok = "EwB4A8l6BAAU-verify-graph-token-0123456789"
	account  = "person@example.test"
)

// tlsFor makes a CA and a certificate for 127.0.0.1, and writes the CA where the plugin's test
// root variable can name it.
func tlsFor(t *testing.T) (*tls.Config, string) {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caT := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "verify CA"}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(time.Hour), IsCA: true, KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true}
	caDER, _ := x509.CreateCertificate(rand.Reader, caT, caT, &caKey.PublicKey, caKey)
	ca, _ := x509.ParseCertificate(caDER)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leafT := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "127.0.0.1"}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(time.Hour), IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, _ := x509.CreateCertificate(rand.Reader, leafT, ca, &key.PublicKey, caKey)
	file := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(file, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}, file
}

type mailServers struct {
	user     *imapmemserver.User
	imapPort int
	smtpPort int
	caFile   string
	mu       sync.Mutex
	sent     []string
}

type lit struct{ *bytes.Reader }

func (l lit) Size() int64 { return int64(l.Reader.Size()) }

type smtpSess struct {
	s    *mailServers
	auth bool
}

func (x *smtpSess) AuthMechanisms() []string { return []string{sasl.Plain} }
func (x *smtpSess) Auth(string) (sasl.Server, error) {
	return sasl.NewPlainServer(func(_, u, p string) error {
		if u != account || p != appPw {
			return smtp.ErrAuthFailed
		}
		x.auth = true
		return nil
	}), nil
}
func (x *smtpSess) Mail(string, *smtp.MailOptions) error {
	if !x.auth {
		return smtp.ErrAuthRequired
	}
	return nil
}
func (x *smtpSess) Rcpt(string, *smtp.RcptOptions) error { return nil }
func (x *smtpSess) Data(r io.Reader) error {
	b, _ := io.ReadAll(r)
	x.s.mu.Lock()
	x.s.sent = append(x.s.sent, string(b))
	x.s.mu.Unlock()
	return nil
}
func (x *smtpSess) Reset()        {}
func (x *smtpSess) Logout() error { return nil }

func newMailServers(t *testing.T) *mailServers {
	t.Helper()
	cfg, caFile := tlsFor(t)
	s := &mailServers{caFile: caFile}
	mem := imapmemserver.New()
	s.user = imapmemserver.NewUser(account, appPw)
	for _, b := range []string{"INBOX", "Drafts", "Archive"} {
		_ = s.user.Create(b, nil)
	}
	mem.AddUser(s.user)
	isrv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewSession(), nil, nil
		},
		Caps: imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapIMAP4rev2: {}}, Logger: log.New(io.Discard, "", 0),
	})
	iln, _ := net.Listen("tcp", "127.0.0.1:0")
	go func() { _ = isrv.Serve(tls.NewListener(iln, cfg)) }()
	t.Cleanup(func() { _ = isrv.Close() })
	s.imapPort = iln.Addr().(*net.TCPAddr).Port

	ssrv := smtp.NewServer(smtp.BackendFunc(func(*smtp.Conn) (smtp.Session, error) { return &smtpSess{s: s}, nil }))
	ssrv.Domain, ssrv.ErrorLog = "127.0.0.1", log.New(io.Discard, "", 0)
	sln, _ := net.Listen("tcp", "127.0.0.1:0")
	go func() { _ = ssrv.Serve(tls.NewListener(sln, cfg)) }()
	t.Cleanup(func() { _ = ssrv.Close() })
	s.smtpPort = sln.Addr().(*net.TCPAddr).Port
	return s
}

func (s *mailServers) deliver(t *testing.T, from, subject, body string, when time.Time) string {
	t.Helper()
	raw := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n%s\r\n", from, account, subject, when.Format(time.RFC1123Z), body)
	d, err := s.user.Append("INBOX", lit{bytes.NewReader([]byte(raw))}, &imap.AppendOptions{Time: when})
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprint(d.UID)
}

func (s *mailServers) conn(pw string) map[string]any {
	return map[string]any{"kind": "imap", "account": account, "username": account, "password": pw,
		"imap": map[string]any{"host": "127.0.0.1", "port": s.imapPort, "security": "TLS"},
		"smtp": map[string]any{"host": "127.0.0.1", "port": s.smtpPort, "security": "TLS"}}
}

// req2 is one 2.0 run of the binary: any connection, settings, the watch's documents, env.
type req2 struct {
	conn   map[string]any
	config map[string]any
	docs   map[string]json.RawMessage
	env    []string
}

type res2 struct {
	ok        bool
	exit      int
	output    string
	error     string
	quiet     bool
	publishes []map[string]any
	puts      map[string]json.RawMessage
	all       string
}

func run2(t *testing.T, instr string, r req2, secrets ...string) res2 {
	t.Helper()
	config := map[string]any{"sendAllowlist": []string{}}
	for k, v := range r.config {
		config[k] = v
	}
	var docs []any
	for id, d := range r.docs {
		docs = append(docs, map[string]any{"id": id, "doc": d})
	}
	if docs == nil {
		docs = []any{}
	}
	body, _ := json.Marshal(map[string]any{
		"protocol": "harness.member/1", "plugin": map[string]any{"id": "mail", "version": "2.0.0"},
		"work":   []any{map[string]any{"seq": 1, "instruction": instr}},
		"config": config, "secrets": map[string]any{}, "connections": map[string]any{"mailbox": r.conn},
		"sites": []any{map[string]any{"site": "mail", "collection": "watch", "documents": docs, "total": len(docs)}},
	})
	cmd := exec.Command(os.Getenv("MAIL_BIN"))
	cmd.Env = append([]string{"PATH=/usr/bin:/bin"}, r.env...)
	cmd.Stdin = bytes.NewReader(body)
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	err := cmd.Run()
	out := res2{puts: map[string]json.RawMessage{}, all: so.String() + se.String()}
	if ee, ok := err.(*exec.ExitError); ok {
		out.exit = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	for _, s := range secrets {
		for _, form := range []string{s, strings.ReplaceAll(s, " ", "")} {
			if strings.Contains(out.all, form) {
				t.Fatalf("SECRET LEAKED:\n%s", out.all)
			}
		}
	}
	for _, l := range strings.Split(strings.TrimSpace(so.String()), "\n") {
		var rec map[string]json.RawMessage
		if json.Unmarshal([]byte(l), &rec) != nil {
			t.Fatalf("not a JSON record: %q", l)
		}
		var typ string
		_ = json.Unmarshal(rec["t"], &typ)
		switch typ {
		case "result":
			_ = json.Unmarshal(rec["ok"], &out.ok)
			_ = json.Unmarshal(rec["output"], &out.output)
			_ = json.Unmarshal(rec["error"], &out.error)
			_ = json.Unmarshal(rec["quiet"], &out.quiet)
		case "publish":
			var p map[string]any
			_ = json.Unmarshal(rec["payload"], &p)
			out.publishes = append(out.publishes, p)
		case "site.put":
			var id string
			_ = json.Unmarshal(rec["id"], &id)
			out.puts[id] = rec["doc"]
		}
	}
	t.Logf("exit=%d ok=%v output=%q error=%q publishes=%d puts=%d", out.exit, out.ok, out.output, out.error, len(out.publishes), len(out.puts))
	return out
}

func keep(docs map[string]json.RawMessage, r res2) map[string]json.RawMessage {
	if docs == nil {
		docs = map[string]json.RawMessage{}
	}
	for id, d := range r.puts {
		docs[id] = d
	}
	return docs
}

func TestIMAPCommandsBlackBox(t *testing.T) {
	s := newMailServers(t)
	env := []string{"MAIL_PLUGIN_TEST_CA_FILE=" + s.caFile}
	id := s.deliver(t, "Acme <billing@example.net>", "Invoice 42", "Pay at https://pay.example.net/42 - pw "+appPw, time.Now())
	base := req2{conn: s.conn(appPw), env: env}

	r := run2(t, "list", base, appPw)
	if !r.ok || !strings.Contains(r.output, id+" | ") || !strings.Contains(r.output, "Invoice 42") {
		t.Fatalf("list: %+v", r)
	}
	r = run2(t, "read "+id, base, appPw)
	if !r.ok || !strings.Contains(r.output, "[1] https://pay.example.net/42") || !strings.Contains(r.output, "[redacted]") {
		t.Fatalf("read: %+v", r)
	}
	r = run2(t, "send\nTo: person@example.com\nBcc: ops@example.com\nSubject: Hi\n\nBody", req2{conn: s.conn(appPw), env: env,
		config: map[string]any{"sendAllowlist": []string{"@example.com"}}}, appPw)
	if !r.ok || !strings.Contains(r.output, "saved in Drafts; nothing was sent.") || len(s.sent) != 0 {
		t.Fatalf("draft mode: %+v sent=%d", r, len(s.sent))
	}
	r = run2(t, "send\nTo: person@example.com\nBcc: ops@example.com\nSubject: Hi\n\nBody", req2{conn: s.conn(appPw), env: env,
		config: map[string]any{"mode": "send", "sendAllowlist": []string{"@example.com"}}}, appPw)
	if !r.ok || len(s.sent) != 1 || strings.Contains(s.sent[0], "Bcc:") {
		t.Fatalf("send mode: %+v sent=%v", r, s.sent)
	}
	r = run2(t, "mark-read "+id, base, appPw)
	if r.ok || !strings.Contains(r.error, "Refused: mark-read and mark-unread are off") {
		t.Fatalf("mark-read off by default: %+v", r)
	}
	r = run2(t, "move Archive "+id, base, appPw)
	if r.ok || !strings.Contains(r.error, "Refused: move and label are off") {
		t.Fatalf("move off by default: %+v", r)
	}
	r = run2(t, "move Archive "+id, req2{conn: s.conn(appPw), env: env, config: map[string]any{"moveTo": []string{"Archive"}}}, appPw)
	if !r.ok || !strings.Contains(r.output, "Moved 1 messages to Archive") {
		t.Fatalf("move: %+v", r)
	}
	r = run2(t, "delete "+id, base, appPw)
	if r.ok || !strings.Contains(r.error, "There is no delete command") {
		t.Fatalf("delete: %+v", r)
	}
	r = run2(t, "list", req2{conn: s.conn("wrong pass word 99"), env: env}, appPw, "wrong pass word 99")
	if r.ok || !strings.Contains(r.error, "refused the sign-in for "+account) {
		t.Fatalf("refused login: %+v", r)
	}
	r = run2(t, "list", req2{conn: s.conn(appPw)}, appPw)
	if r.ok || !strings.Contains(r.error, "did not prove it is that server") {
		t.Fatalf("without the test CA the fake is a stranger, and is refused: %+v", r)
	}
}

func TestIMAPWatchBlackBox(t *testing.T) {
	s := newMailServers(t)
	env := []string{"MAIL_PLUGIN_TEST_CA_FILE=" + s.caFile}
	for i := range 25 {
		s.deliver(t, "old@example.org", fmt.Sprintf("Old %d", i), "old", time.Now().Add(-time.Hour))
	}
	var docs map[string]json.RawMessage
	r := run2(t, "watch", req2{conn: s.conn(appPw), env: env, docs: docs}, appPw)
	docs = keep(docs, r)
	if !r.ok || len(r.publishes) != 0 || len(docs) != 1 {
		t.Fatalf("starts from now on a full mailbox, one document: %+v", r)
	}
	a := s.deliver(t, "a@example.org", "New A", "hello "+appPw, time.Now())
	b := s.deliver(t, "b@example.org", "New B", "hello", time.Now())
	r = run2(t, "watch", req2{conn: s.conn(appPw), env: env, docs: docs}, appPw)
	docs = keep(docs, r)
	if len(r.publishes) != 1 || r.publishes[0]["ids"] != a+", "+b || len(docs) != 1 {
		t.Fatalf("one event listing both: %+v", r.publishes)
	}
	r = run2(t, "ack "+a, req2{conn: s.conn(appPw), env: env, docs: docs}, appPw)
	docs = keep(docs, r)
	for listing := 2; listing <= 4; listing++ {
		r = run2(t, "watch", req2{conn: s.conn(appPw), env: env, docs: docs}, appPw)
		docs = keep(docs, r)
		if len(r.publishes) != 1 || r.publishes[0]["ids"] != b {
			t.Fatalf("listing %d: only the unacknowledged one: %+v", listing, r.publishes)
		}
	}
	r = run2(t, "watch", req2{conn: s.conn(appPw), env: env, docs: docs}, appPw)
	if len(r.publishes) != 0 || !strings.Contains(r.output, "Not acknowledged after 4 listings") || !strings.Contains(r.output, "New B") {
		t.Fatalf("named after 3 more listings: %+v", r)
	}
}

// graphFake answers just enough Graph for list and the watch.
type graphFake struct {
	srv  *httptest.Server
	mu   sync.Mutex
	msgs []map[string]any
	seq  int
}

func newGraphFake(t *testing.T) *graphFake {
	g := &graphFake{}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") != "Bearer "+graphTok {
			w.WriteHeader(401)
			io.WriteString(w, `{"error":{"code":"InvalidAuthenticationToken","message":"bad"}}`)
			return
		}
		switch {
		case r.URL.Path == "/v1.0/me/mailFolders/inbox/messages":
			json.NewEncoder(w).Encode(map[string]any{"value": g.msgs})
		case r.URL.Path == "/v1.0/me/mailFolders/inbox/messages/delta":
			after := 0
			fmt.Sscan(r.URL.Query().Get("$deltatoken"), &after)
			var v []any
			if r.URL.Query().Get("$deltatoken") != "" {
				for i, m := range g.msgs {
					if i+1 > after {
						v = append(v, m)
					}
				}
			}
			json.NewEncoder(w).Encode(map[string]any{"value": v,
				"@odata.deltaLink": g.srv.URL + "/v1.0/me/mailFolders/inbox/messages/delta?$deltatoken=" + fmt.Sprint(len(g.msgs))})
		case r.URL.Path == "/v1.0/me/mailFolders/inbox/messages" || strings.HasPrefix(r.URL.Path, "/v1.0/me/messages"):
			w.WriteHeader(404)
			io.WriteString(w, `{"error":{"code":"ErrorItemNotFound","message":"gone"}}`)
		default:
			w.WriteHeader(404)
			io.WriteString(w, `{"error":{"code":"ErrorItemNotFound","message":"no route"}}`)
		}
	}))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *graphFake) add(subject string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	id := fmt.Sprintf("AAMkVERIFY%03d=", len(g.msgs)+1)
	g.msgs = append(g.msgs, map[string]any{"id": id, "conversationId": "c" + id, "subject": subject, "isRead": false,
		"receivedDateTime": time.Now().UTC().Add(time.Minute).Format(time.RFC3339), "bodyPreview": "preview " + graphTok,
		"from": map[string]any{"emailAddress": map[string]string{"name": "Pat", "address": "person@example.com"}}})
	return id
}

func TestGraphBlackBox(t *testing.T) {
	g := newGraphFake(t)
	env := []string{"MAIL_PLUGIN_TEST_GRAPH_BASE=" + g.srv.URL}
	conn := map[string]any{"provider": "microsoft", "account": "person@contoso.test", "accessToken": graphTok}
	var docs map[string]json.RawMessage
	r := run2(t, "watch", req2{conn: conn, env: env}, graphTok)
	docs = keep(docs, r)
	if !r.ok || len(r.publishes) != 0 {
		t.Fatalf("first check starts from now: %+v", r)
	}
	id := g.add("Hello from Graph")
	r = run2(t, "list", req2{conn: conn, env: env}, graphTok)
	if !r.ok || !strings.Contains(r.output, id) || !strings.Contains(r.output, "[redacted]") {
		t.Fatalf("list: %+v", r)
	}
	r = run2(t, "watch", req2{conn: conn, env: env, docs: docs}, graphTok)
	if len(r.publishes) != 1 || r.publishes[0]["ids"] != id {
		t.Fatalf("one event with the new message: %+v", r.publishes)
	}
	bad := map[string]any{"provider": "microsoft", "account": "person@contoso.test", "accessToken": "EwB-some-other-revoked-token-000"}
	r = run2(t, "list", req2{conn: bad, env: env}, graphTok, "EwB-some-other-revoked-token-000")
	if r.ok || !strings.Contains(r.error, "The Microsoft connection needs reconnecting in Admin, Connections.") {
		t.Fatalf("401: %+v", r)
	}
}
