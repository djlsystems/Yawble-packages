package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"strconv"
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

// The fake mail servers listen on 127.0.0.1 only, with a certificate from a CA made for the test
// run; the plugin trusts that CA through testRoots and nothing else.

const (
	imapUser     = "person@example.test"
	appPassword  = "qwer tyui opas dfgh" // written as Google shows an app password
	wrongPasswd  = "zzzz zzzz zzzz zzzz"
	imapAccount  = "person@example.test"
	icloudUser   = "person"
	smtpLoginFor = "person@example.test"
)

var (
	caOnce    sync.Once
	serverTLS *tls.Config
	caPEM     []byte
)

func testTLS(t *testing.T) *tls.Config {
	t.Helper()
	caOnce.Do(func() {
		caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "mail plugin test CA"},
			NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), IsCA: true,
			KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true}
		caDER, _ := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
		ca, _ := x509.ParseCertificate(caDER)
		key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		tmpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "127.0.0.1"},
			NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
			IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, DNSNames: []string{"localhost"},
			KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
		der, _ := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
		serverTLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
		pool := x509.NewCertPool()
		pool.AddCert(ca)
		testRoots = pool
		caPEM = caDER
	})
	return serverTLS
}

// fakeIMAP is an in-memory IMAP server (go-imap's imapmemserver) behind TLS or STARTTLS.
type fakeIMAP struct {
	user *imapmemserver.User
	port int
	sec  string
}

type quietLogger struct{}

func (quietLogger) Printf(string, ...any) {}
func (quietLogger) Println(...any)        {}

func newFakeIMAP(t *testing.T, security string) *fakeIMAP {
	t.Helper()
	cfg := testTLS(t)
	mem := imapmemserver.New()
	user := imapmemserver.NewUser(imapUser, appPassword)
	for _, box := range []string{"INBOX", "Drafts", "Archive", "Receipts"} {
		if err := user.Create(box, nil); err != nil {
			t.Fatal(err)
		}
	}
	mem.AddUser(user)
	opts := &imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewSession(), nil, nil
		},
		Caps:   imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapIMAP4rev2: {}},
		Logger: log.New(io.Discard, "", 0),
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if security == "TLS" {
		ln = tls.NewListener(ln, cfg)
	} else {
		opts.TLSConfig = cfg
	}
	srv := imapserver.New(opts)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return &fakeIMAP{user: user, port: ln.Addr().(*net.TCPAddr).Port, sec: security}
}

type literal struct{ *bytes.Reader }

func (l literal) Size() int64 { return int64(l.Reader.Size()) }

// deliver puts a message in a folder, as arrival would, and answers its id as the plugin writes it.
func (f *fakeIMAP) deliver(t *testing.T, folder string, raw string, when time.Time, seen bool) string {
	t.Helper()
	var flags []imap.Flag
	if seen {
		flags = []imap.Flag{imap.FlagSeen}
	}
	data, err := f.user.Append(folder, literal{bytes.NewReader([]byte(raw))}, &imap.AppendOptions{Flags: flags, Time: when})
	if err != nil {
		t.Fatal(err)
	}
	return imapID(folder, data.UID)
}

// renumber deletes and re-creates a folder with the same messages, which changes its
// UIDVALIDITY, as a server does when it rebuilds a folder.
func (f *fakeIMAP) renumber(t *testing.T, folder string) {
	t.Helper()
	if err := f.user.Delete(folder); err != nil {
		t.Fatal(err)
	}
	if err := f.user.Create(folder, nil); err != nil {
		t.Fatal(err)
	}
}

// flags answers a message's flags, read straight from the server.
func (f *fakeIMAP) flags(t *testing.T, folder string, uid uint32) []string {
	t.Helper()
	sess := imapmemserver.NewUserSession(f.user)
	defer sess.Close()
	if _, err := sess.Select(folder, &imap.SelectOptions{ReadOnly: true}); err != nil {
		t.Fatal(err)
	}
	data, err := sess.Search(imapserver.NumKindUID, &imap.SearchCriteria{UID: []imap.UIDSet{imap.UIDSetNum(imap.UID(uid))}, Flag: []imap.Flag{imap.FlagSeen}}, &imap.SearchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	if len(data.AllUIDs()) == 1 {
		out = append(out, string(imap.FlagSeen))
	}
	return out
}

// count answers how many messages a folder holds.
func (f *fakeIMAP) count(t *testing.T, folder string) int {
	t.Helper()
	st, err := f.user.Status(folder, &imap.StatusOptions{NumMessages: true})
	if err != nil {
		t.Fatal(err)
	}
	return int(*st.NumMessages)
}

// fakeSMTP is go-smtp's server behind TLS or STARTTLS, signing in with PLAIN only, keeping what
// it is given.
type fakeSMTP struct {
	port   int
	user   string
	mu     sync.Mutex
	logins []string
	sent   []smtpMessage
	reject int // when set, RCPT answers this code
}

type smtpMessage struct {
	From string
	To   []string
	Data string
}

type smtpSession struct {
	f    *fakeSMTP
	msg  smtpMessage
	auth bool
}

func (s *smtpSession) AuthMechanisms() []string { return []string{sasl.Plain} }
func (s *smtpSession) Auth(mech string) (sasl.Server, error) {
	return sasl.NewPlainServer(func(identity, username, password string) error {
		s.f.mu.Lock()
		s.f.logins = append(s.f.logins, username)
		s.f.mu.Unlock()
		if username != s.f.user || password != appPassword {
			return smtp.ErrAuthFailed
		}
		s.auth = true
		return nil
	}), nil
}
func (s *smtpSession) Mail(from string, _ *smtp.MailOptions) error {
	if !s.auth {
		return smtp.ErrAuthRequired
	}
	s.msg = smtpMessage{From: from}
	return nil
}
func (s *smtpSession) Rcpt(to string, _ *smtp.RcptOptions) error {
	if s.f.reject != 0 {
		return &smtp.SMTPError{Code: s.f.reject, Message: "mailbox unavailable"}
	}
	s.msg.To = append(s.msg.To, to)
	return nil
}
func (s *smtpSession) Data(r io.Reader) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	s.msg.Data = string(b)
	s.f.mu.Lock()
	s.f.sent = append(s.f.sent, s.msg)
	s.f.mu.Unlock()
	return nil
}
func (s *smtpSession) Reset()        { s.msg = smtpMessage{} }
func (s *smtpSession) Logout() error { return nil }

func newFakeSMTP(t *testing.T, security, user string) *fakeSMTP {
	t.Helper()
	cfg := testTLS(t)
	f := &fakeSMTP{user: user}
	srv := smtp.NewServer(smtp.BackendFunc(func(*smtp.Conn) (smtp.Session, error) { return &smtpSession{f: f}, nil }))
	srv.Domain = "127.0.0.1"
	srv.ErrorLog = log.New(io.Discard, "", 0)
	srv.ReadTimeout, srv.WriteTimeout = 10*time.Second, 10*time.Second
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if security == "TLS" {
		ln = tls.NewListener(ln, cfg)
	} else {
		srv.TLSConfig = cfg
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	f.port = ln.Addr().(*net.TCPAddr).Port
	return f
}

func (f *fakeSMTP) messages() []smtpMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]smtpMessage(nil), f.sent...)
}

// imapConnection is the grant a slot bound to an app-password mailbox receives.
func imapConnection(i *fakeIMAP, s *fakeSMTP, password string) map[string]any {
	c := map[string]any{"kind": "imap", "account": imapAccount, "username": imapUser, "password": password,
		"imap": map[string]any{"host": "127.0.0.1", "port": i.port, "security": i.sec},
		"smtp": map[string]any{"host": "127.0.0.1", "port": 1, "security": "TLS"}}
	if s != nil {
		sec := "TLS"
		if s.port != 0 {
			c["smtp"] = map[string]any{"host": "127.0.0.1", "port": s.port, "security": sec}
		}
	}
	return c
}

// rawMail writes a message as a server stores it. html, when set, makes it multipart/alternative;
// attach adds a PDF attachment.
func rawMail(from, subject, text, html string, when time.Time, attach string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nMessage-ID: <%d.%s@example.test>\r\nMIME-Version: 1.0\r\n",
		from, imapAccount, subject, when.Format(time.RFC1123Z), when.UnixNano(), strconv.Itoa(len(subject)))
	if html == "" && attach == "" {
		b.WriteString("Content-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n")
		b.WriteString(strings.ReplaceAll(text, "\n", "\r\n") + "\r\n")
		return b.String()
	}
	b.WriteString("Content-Type: multipart/mixed; boundary=\"outer\"\r\n\r\n--outer\r\n")
	b.WriteString("Content-Type: multipart/alternative; boundary=\"alt\"\r\n\r\n")
	if text != "" {
		b.WriteString("--alt\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: base64\r\n\r\n")
		b.WriteString(base64.StdEncoding.EncodeToString([]byte(text)) + "\r\n")
	}
	if html != "" {
		b.WriteString("--alt\r\nContent-Type: text/html; charset=iso-8859-1\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n")
		b.WriteString(html + "\r\n")
	}
	b.WriteString("--alt--\r\n")
	if attach != "" {
		b.WriteString("--outer\r\nContent-Type: application/pdf; name=\"" + attach + "\"\r\nContent-Disposition: attachment; filename=\"" + attach + "\"\r\nContent-Transfer-Encoding: base64\r\n\r\n")
		b.WriteString(base64.StdEncoding.EncodeToString([]byte("%PDF-1.4 fake")) + "\r\n")
	}
	b.WriteString("--outer--\r\n")
	return b.String()
}
