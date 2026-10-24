package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeSQL is an Azure SQL server speaking TDS 8.0 on a local TLS listener: it accepts a login only
// with the Azure SQL token, records each batch, and answers what reply makes of it.
type fakeSQL struct {
	t       *testing.T
	ln      net.Listener
	mu      sync.Mutex
	dials   []string // host each connection was opened to
	logins  []sqlLogin
	batches []string
	reply   func(sql string) []byte
}

type sqlLogin struct{ server, database, token string }

func newFakeSQL(t *testing.T) *fakeSQL {
	// Borrow httptest's certificate, and a pool that trusts it.
	srv := httptest.NewUnstartedServer(http.NotFoundHandler())
	srv.StartTLS()
	cert := srv.TLS.Certificates[0]
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	srv.Close()

	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"tds/8.0"}})
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeSQL{t: t, ln: ln, reply: func(string) []byte { return tdsDone(0x10, 1) }}
	t.Cleanup(func() { ln.Close() })
	go s.accept()

	old := sqlDial
	sqlDial = func(host string, port int) (net.Conn, error) {
		s.mu.Lock()
		s.dials = append(s.dials, host)
		s.mu.Unlock()
		return tls.Dial("tcp", ln.Addr().String(), &tls.Config{RootCAs: pool, ServerName: "example.com", NextProtos: []string{"tds/8.0"}})
	}
	t.Cleanup(func() { sqlDial = old })
	return s
}

func (s *fakeSQL) accept() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.serve(c)
	}
}

func readMessage(c net.Conn) (byte, []byte, error) {
	var out []byte
	hdr := make([]byte, 8)
	for {
		if _, err := io.ReadFull(c, hdr); err != nil {
			return 0, nil, err
		}
		body := make([]byte, int(binary.BigEndian.Uint16(hdr[2:]))-8)
		if _, err := io.ReadFull(c, body); err != nil {
			return 0, nil, err
		}
		out = append(out, body...)
		if hdr[1]&1 != 0 {
			return hdr[0], out, nil
		}
	}
}

func writeMessage(c net.Conn, payload []byte) {
	hdr := []byte{tdsReply, 1, 0, 0, 0, 0, 1, 0}
	binary.BigEndian.PutUint16(hdr[2:], uint16(len(payload)+8))
	_, _ = c.Write(append(hdr, payload...))
}

func (s *fakeSQL) serve(c net.Conn) {
	defer c.Close()
	if tc, ok := c.(*tls.Conn); ok {
		if err := tc.Handshake(); err != nil || tc.ConnectionState().NegotiatedProtocol != "tds/8.0" {
			s.t.Errorf("the SQL connection did not open TLS with tds/8.0 first")
			return
		}
	}
	typ, pre, err := readMessage(c)
	if err != nil || typ != tdsPrelogin {
		s.t.Errorf("the first message was not PRELOGIN")
		return
	}
	if !preloginFedAuth(pre) {
		s.t.Errorf("PRELOGIN did not ask for a federated login")
	}
	writeMessage(c, prelogin()) // FEDAUTHREQUIRED: 1, so the login must echo it

	typ, login, err := readMessage(c)
	if err != nil || typ != tdsLogin {
		s.t.Errorf("the second message was not LOGIN7")
		return
	}
	l, echo := parseLogin7(login)
	s.mu.Lock()
	s.logins = append(s.logins, l)
	s.mu.Unlock()
	if l.token != tokSQL || !echo {
		writeMessage(c, append(tdsError(18456, "Login failed for user '<token-identified principal>'."), tdsDone(0x02, 0)...))
		return
	}
	writeMessage(c, append(append([]byte{0xAD, 1, 0, 0}, tdsEnvPacketSize("8000")...), tdsDone(0, 0)...))

	for {
		typ, batch, err := readMessage(c)
		if err != nil {
			return
		}
		if typ != tdsBatch || len(batch) < 22 || binary.LittleEndian.Uint32(batch) != 22 {
			s.t.Errorf("a batch without its headers")
			return
		}
		sql := fromUCS2(batch[22:])
		s.mu.Lock()
		s.batches = append(s.batches, sql)
		s.mu.Unlock()
		writeMessage(c, s.reply(sql))
	}
}

// parseLogin7 reads the server name, database and token from a LOGIN7 message, and whether the
// federated authentication feature echoed the server.
func parseLogin7(b []byte) (sqlLogin, bool) {
	field := func(i int) string {
		off, n := int(binary.LittleEndian.Uint16(b[36+i*4:])), int(binary.LittleEndian.Uint16(b[38+i*4:]))
		return fromUCS2(b[off : off+n*2])
	}
	l := sqlLogin{server: field(4), database: field(8)}
	if field(1) != "" || field(2) != "" {
		l.token = "a user name or password"
	}
	ext := int(binary.LittleEndian.Uint32(b[int(binary.LittleEndian.Uint16(b[56:])):]))
	echo := false
	for ext < len(b) && b[ext] != 0xFF {
		id, n := b[ext], int(binary.LittleEndian.Uint32(b[ext+1:]))
		data := b[ext+5 : ext+5+n]
		if id == 0x02 && data[0]>>1 == 0x01 {
			echo = data[0]&1 == 1
			tn := int(binary.LittleEndian.Uint32(data[1:]))
			if l.token == "" {
				l.token = fromUCS2(data[5 : 5+tn])
			}
		}
		ext += 5 + n
	}
	return l, echo
}

// ---- token streams ----

func tdsDone(status uint16, count uint64) []byte {
	b := []byte{0xFD, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	binary.LittleEndian.PutUint16(b[1:], status)
	binary.LittleEndian.PutUint64(b[5:], count)
	return b
}

func tdsError(number uint32, text string) []byte {
	msg := make([]byte, 6)
	binary.LittleEndian.PutUint32(msg, number)
	msg[4], msg[5] = 1, 14
	msg = append(msg, byte(len([]rune(text))), 0)
	msg = append(msg, ucs2(text)...)
	msg = append(msg, 0, 0, 0, 0, 0, 0)
	out := []byte{0xAA, 0, 0}
	binary.LittleEndian.PutUint16(out[1:], uint16(len(msg)))
	return append(out, msg...)
}

func tdsEnvPacketSize(size string) []byte {
	v := append([]byte{4, byte(len(size))}, ucs2(size)...)
	v = append(v, 4)
	v = append(v, ucs2("4096")...)
	out := []byte{0xE3, 0, 0}
	binary.LittleEndian.PutUint16(out[1:], uint16(len(v)))
	return append(out, v...)
}

// tdsColumn is one column: its name and its TYPE_INFO bytes.
type tdsColumn struct {
	name     string
	typeInfo []byte
}

var (
	colInt      = []byte{0x26, 4}
	colNVarchar = []byte{0xE7, 0x40, 0x00, 0x09, 0x04, 0xD0, 0x00, 0x34}
	colBit      = []byte{0x68, 1}
	colDecimal  = []byte{0x6A, 5, 9, 2}
	colDT2      = []byte{0x2A, 7}
	colGUID     = []byte{0x24, 16}
)

func tdsColumns(cols ...tdsColumn) []byte {
	out := []byte{0x81, byte(len(cols)), 0}
	for _, c := range cols {
		out = append(out, 0, 0, 0, 0, 0x09, 0x00)
		out = append(out, c.typeInfo...)
		out = append(out, byte(len([]rune(c.name))))
		out = append(out, ucs2(c.name)...)
	}
	return out
}

func tdsRow(values ...[]byte) []byte {
	out := []byte{0xD1}
	for _, v := range values {
		out = append(out, v...)
	}
	return out
}

func vInt(n int32) []byte {
	b := []byte{4, 0, 0, 0, 0}
	binary.LittleEndian.PutUint32(b[1:], uint32(n))
	return b
}

func vNVarchar(s string) []byte {
	u := ucs2(s)
	return append([]byte{byte(len(u)), byte(len(u) >> 8)}, u...)
}

var vNull = []byte{0} // a byte-length type's NULL

func selectReply(rows int) []byte {
	out := tdsColumns(tdsColumn{"id", colInt}, tdsColumn{"name", colNVarchar})
	for i := 1; i <= rows; i++ {
		out = append(out, tdsRow(vInt(int32(i)), vNVarchar("row "+strings.Repeat("x", i%3)))...)
	}
	return append(out, tdsDone(0x10, uint64(rows))...)
}
