package main

import (
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"math/big"
	"net"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"
)

// A small client for Azure SQL's own protocol, TDS 8.0: TLS from the first byte, then a login that
// carries the Azure SQL token from the person's sign-in (no SQL password exists), then batches of
// SQL. It reads the column types Azure SQL answers with into text, for showing; it is not a driver.

// sqlDial opens the TLS connection to a SQL host. Only tests change it.
var sqlDial = func(host string, port int) (net.Conn, error) {
	d := &net.Dialer{Timeout: 30 * time.Second}
	return tls.DialWithDialer(d, "tcp", net.JoinHostPort(host, strconv.Itoa(port)), &tls.Config{
		ServerName: host, NextProtos: []string{"tds/8.0"}, MinVersion: tls.VersionTLS12,
	})
}

const (
	tdsBatch    = 0x01
	tdsReply    = 0x04
	tdsLogin    = 0x10
	tdsPrelogin = 0x12

	maxSQLRows     = 200
	maxReplyBytes  = 64 << 20
	maxCellRunes   = 200
	sqlBatchWindow = 4 * time.Minute
)

type tdsConn struct {
	conn       net.Conn
	packetSize int
}

// sqlConnect signs in to one database, following Azure SQL's redirect to the node that holds it.
func sqlConnect(host, database, token string) (*tdsConn, error) {
	port := 1433
	for hop := 0; hop < 3; hop++ {
		c, route, err := tdsOpen(host, port, database, token)
		if err != nil {
			return nil, err
		}
		if route == nil {
			return c, nil
		}
		c.conn.Close()
		host, port = route.host, route.port
	}
	return nil, fmt.Errorf("Azure SQL redirected the connection too many times")
}

type tdsRoute struct {
	host string
	port int
}

func tdsOpen(host string, port int, database, token string) (*tdsConn, *tdsRoute, error) {
	conn, err := sqlDial(host, port)
	if err != nil {
		return nil, nil, fmt.Errorf("Azure SQL at %s could not be reached: %v", host, err)
	}
	c := &tdsConn{conn: conn, packetSize: 4096}
	_ = conn.SetDeadline(time.Now().Add(60 * time.Second))
	fail := func(err error) (*tdsConn, *tdsRoute, error) {
		conn.Close()
		return nil, nil, err
	}

	// PRELOGIN: version, encryption (TLS already carries everything), and that the login is federated.
	if err := c.send(tdsPrelogin, prelogin()); err != nil {
		return fail(err)
	}
	reply, err := c.receive()
	if err != nil {
		return fail(err)
	}
	echo := preloginFedAuth(reply)

	if err := c.send(tdsLogin, login7(host, database, token, echo)); err != nil {
		return fail(err)
	}
	reply, err = c.receive()
	if err != nil {
		return fail(err)
	}
	out := readTokens(reply)
	if len(out.errors) > 0 {
		return fail(fmt.Errorf("Azure SQL refused the sign-in: %s", strings.Join(out.errors, "; ")))
	}
	if out.route != nil {
		return c, out.route, nil
	}
	if !out.loggedIn {
		return fail(fmt.Errorf("Azure SQL did not accept the sign-in"))
	}
	if out.packetSize >= 512 && out.packetSize <= 32767 {
		c.packetSize = out.packetSize
	}
	return c, nil, nil
}

func (c *tdsConn) close() { c.conn.Close() }

// exec runs one batch and answers what it returned.
func (c *tdsConn) exec(sql string) (*tdsOutcome, error) {
	_ = c.conn.SetDeadline(time.Now().Add(sqlBatchWindow))
	// ALL_HEADERS: the transaction descriptor header, outside any transaction.
	payload := make([]byte, 22)
	binary.LittleEndian.PutUint32(payload[0:], 22)
	binary.LittleEndian.PutUint32(payload[4:], 18)
	binary.LittleEndian.PutUint16(payload[8:], 2)
	binary.LittleEndian.PutUint32(payload[18:], 1)
	payload = append(payload, ucs2(sql)...)
	if err := c.send(tdsBatch, payload); err != nil {
		return nil, err
	}
	reply, err := c.receive()
	if err != nil {
		return nil, err
	}
	out := readTokens(reply)
	if out.bad != "" {
		return out, fmt.Errorf("Azure SQL's answer could not be read: %s", out.bad)
	}
	return out, nil
}

// send writes one message, cut into packets.
func (c *tdsConn) send(typ byte, payload []byte) error {
	room := c.packetSize - 8
	for id := byte(1); ; id++ {
		n, last := len(payload), true
		if n > room {
			n, last = room, false
		}
		hdr := []byte{typ, 0, 0, 0, 0, 0, id, 0}
		if last {
			hdr[1] = 1
		}
		binary.BigEndian.PutUint16(hdr[2:], uint16(n+8))
		if _, err := c.conn.Write(append(hdr, payload[:n]...)); err != nil {
			return fmt.Errorf("the connection to Azure SQL broke: %v", err)
		}
		payload = payload[n:]
		if last {
			return nil
		}
	}
}

// receive reads one message, every packet up to the one marked last.
func (c *tdsConn) receive() ([]byte, error) {
	var out []byte
	hdr := make([]byte, 8)
	for {
		if _, err := io.ReadFull(c.conn, hdr); err != nil {
			return nil, fmt.Errorf("the connection to Azure SQL broke: %v", err)
		}
		n := int(binary.BigEndian.Uint16(hdr[2:])) - 8
		if n < 0 {
			return nil, fmt.Errorf("Azure SQL sent a broken packet")
		}
		if len(out)+n > maxReplyBytes {
			return nil, fmt.Errorf("the answer is larger than %d MB; select fewer rows or columns", maxReplyBytes>>20)
		}
		body := make([]byte, n)
		if _, err := io.ReadFull(c.conn, body); err != nil {
			return nil, fmt.Errorf("the connection to Azure SQL broke: %v", err)
		}
		out = append(out, body...)
		if hdr[1]&1 != 0 {
			return out, nil
		}
	}
}

// ---- the login ----

func prelogin() []byte {
	type opt struct {
		id   byte
		data []byte
	}
	opts := []opt{
		{0x00, []byte{1, 0, 0, 0, 0, 0}}, // VERSION
		{0x01, []byte{0x01}},             // ENCRYPTION: on
		{0x02, []byte{0}},                // INSTOPT
		{0x03, []byte{0, 0, 0, 0}},       // THREADID
		{0x04, []byte{0}},                // MARS: off
		{0x06, []byte{0x01}},             // FEDAUTHREQUIRED
	}
	head := len(opts)*5 + 1
	var h, d []byte
	for _, o := range opts {
		off := head + len(d)
		h = append(h, o.id, byte(off>>8), byte(off), byte(len(o.data)>>8), byte(len(o.data)))
		d = append(d, o.data...)
	}
	return append(append(h, 0xFF), d...)
}

// preloginFedAuth reads whether the server asks the login to echo its FEDAUTHREQUIRED answer.
func preloginFedAuth(b []byte) bool {
	for i := 0; i+5 <= len(b) && b[i] != 0xFF; i += 5 {
		if b[i] == 0x06 {
			off, n := int(b[i+1])<<8|int(b[i+2]), int(b[i+3])<<8|int(b[i+4])
			return n == 1 && off < len(b) && b[off] == 1
		}
	}
	return false
}

// login7 is the LOGIN7 message: no user name or password, the database, and the token in the
// federated authentication feature.
func login7(server, database, token string, echo bool) []byte {
	const fixed = 94
	fields := [][]byte{
		ucs2("yawble"),       // host name
		nil,                  // user name
		nil,                  // password
		ucs2("yawble-azure"), // app name
		ucs2(server),         // server name
		nil,                  // extension: set below
		ucs2("yawble-azure"), // client interface name
		nil,                  // language
		ucs2(database),       // database
	}
	b := make([]byte, fixed)
	binary.LittleEndian.PutUint32(b[4:], 0x74000004) // TDS 7.4 login, inside TDS 8.0's TLS
	binary.LittleEndian.PutUint32(b[8:], 4096)
	binary.LittleEndian.PutUint32(b[12:], 0x07000000)
	b[24] = 0xE0 // fUseDB, fDatabase, fSetLang
	b[25] = 0x03 // fLanguage, fODBC
	b[27] = 0x10 // fExtension
	binary.LittleEndian.PutUint32(b[32:], 0x0409)

	data := []byte{}
	extAt := 0
	for i, f := range fields {
		pos := 36 + i*4
		off := fixed + len(data)
		if i == 5 {
			extAt = off
			binary.LittleEndian.PutUint16(b[pos:], uint16(off))
			binary.LittleEndian.PutUint16(b[pos+2:], 4)
			data = append(data, 0, 0, 0, 0)
			continue
		}
		binary.LittleEndian.PutUint16(b[pos:], uint16(off))
		binary.LittleEndian.PutUint16(b[pos+2:], uint16(len(f)/2))
		data = append(data, f...)
	}
	// ClientID (6 bytes) at 72, then SSPI, AtchDBFile and ChangePassword: empty, pointing at the end.
	for _, pos := range []int{78, 82, 86} {
		binary.LittleEndian.PutUint16(b[pos:], uint16(fixed+len(data)))
	}
	featureAt := fixed + len(data)
	binary.LittleEndian.PutUint32(data[extAt-fixed:], uint32(featureAt))

	tok := ucs2(token)
	opts := byte(0x01 << 1) // the security-token library
	if echo {
		opts |= 1
	}
	feature := []byte{0x02, 0, 0, 0, 0, opts, 0, 0, 0, 0}
	binary.LittleEndian.PutUint32(feature[1:], uint32(5+len(tok)))
	binary.LittleEndian.PutUint32(feature[6:], uint32(len(tok)))
	feature = append(append(feature, tok...), 0xFF)
	data = append(data, feature...)

	out := append(b, data...)
	binary.LittleEndian.PutUint32(out[0:], uint32(len(out)))
	return out
}

func ucs2(s string) []byte {
	u := utf16.Encode([]rune(s))
	b := make([]byte, len(u)*2)
	for i, c := range u {
		binary.LittleEndian.PutUint16(b[i*2:], c)
	}
	return b
}

func fromUCS2(b []byte) string {
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = binary.LittleEndian.Uint16(b[i*2:])
	}
	return string(utf16.Decode(u))
}

// ---- the answer ----

// tdsOutcome is what a batch answered: its result sets, the rows its statements changed, and its
// errors and messages.
type tdsOutcome struct {
	sets       []*sqlSet
	affected   int64
	counted    bool
	errors     []string
	messages   []string
	loggedIn   bool
	route      *tdsRoute
	packetSize int
	bad        string
}

type sqlSet struct {
	columns []string
	types   []colType
	rows    [][]string
	more    int64 // rows past the cap, left out
	total   int64
}

type colType struct {
	id      byte
	kind    int // lenFixed, lenByte, lenShort, lenText, lenPLP, lenVariant
	size    int
	prec    byte
	scale   byte
	isNChar bool
}

const (
	lenFixed = iota
	lenByte
	lenShort
	lenText
	lenPLP
	lenVariant
)

// rd reads a token stream; past its end it answers zeros and remembers that it ran out.
type rd struct {
	b   []byte
	i   int
	out bool
}

func (r *rd) take(n int) []byte {
	if n < 0 || r.i+n > len(r.b) {
		r.out, r.i = true, len(r.b)
		return make([]byte, max(n, 0))
	}
	s := r.b[r.i : r.i+n]
	r.i += n
	return s
}
func (r *rd) u8() byte     { return r.take(1)[0] }
func (r *rd) u16() uint16  { return binary.LittleEndian.Uint16(r.take(2)) }
func (r *rd) u32() uint32  { return binary.LittleEndian.Uint32(r.take(4)) }
func (r *rd) u64() uint64  { return binary.LittleEndian.Uint64(r.take(8)) }
func (r *rd) bstr() string { return fromUCS2(r.take(int(r.u8()) * 2)) }
func (r *rd) usstr() string {
	return fromUCS2(r.take(int(r.u16()) * 2))
}

func readTokens(b []byte) *tdsOutcome {
	out := &tdsOutcome{}
	r := &rd{b: b}
	var cur *sqlSet
	for r.i < len(r.b) && !r.out && out.bad == "" {
		tok := r.u8()
		switch tok {
		case 0x81: // COLMETADATA
			n := r.u16()
			if n == 0xFFFF {
				continue
			}
			cur = &sqlSet{}
			out.sets = append(out.sets, cur)
			for i := 0; i < int(n) && !r.out; i++ {
				r.take(6) // user type, flags
				t, ok := readTypeInfo(r)
				if !ok {
					out.bad = fmt.Sprintf("a column of type 0x%02X", t.id)
					break
				}
				cur.types = append(cur.types, t)
				cur.columns = append(cur.columns, r.bstr())
			}
		case 0xD1, 0xD2: // ROW, NBCROW
			if cur == nil {
				out.bad = "a row before its columns"
				break
			}
			var nulls []byte
			if tok == 0xD2 {
				nulls = r.take((len(cur.types) + 7) / 8)
			}
			row := make([]string, len(cur.types))
			for i, t := range cur.types {
				if nulls != nil && nulls[i/8]&(1<<(i%8)) != 0 {
					row[i] = "NULL"
					continue
				}
				row[i] = readValue(r, t)
			}
			cur.total++
			if len(cur.rows) < maxSQLRows {
				cur.rows = append(cur.rows, row)
			} else {
				cur.more++
			}
		case 0xFD, 0xFE, 0xFF: // DONE, DONEPROC, DONEINPROC
			status := r.u16()
			r.u16()
			count := int64(r.u64())
			if status&0x10 != 0 {
				if cur != nil {
					cur = nil
				} else {
					out.affected += count
					out.counted = true
				}
			}
		case 0xAA, 0xAB: // ERROR, INFO
			n := int(r.u16())
			msg := &rd{b: r.take(n)}
			number := int32(msg.u32())
			msg.u8()
			class := msg.u8()
			text := msg.usstr()
			msg.bstr()
			msg.bstr()
			line := msg.u32()
			if tok == 0xAA {
				out.errors = append(out.errors, fmt.Sprintf("error %d (level %d, line %d): %s", number, class, line, text))
			} else if len(out.messages) < 50 {
				out.messages = append(out.messages, text)
			}
		case 0xAD: // LOGINACK
			r.take(int(r.u16()))
			out.loggedIn = true
		case 0xE3: // ENVCHANGE
			env := &rd{b: r.take(int(r.u16()))}
			switch env.u8() {
			case 4: // packet size
				out.packetSize, _ = strconv.Atoi(env.bstr())
			case 20: // routing
				env.u16()
				if env.u8() == 0 {
					port := int(env.u16())
					out.route = &tdsRoute{host: env.usstr(), port: port}
				}
			}
		case 0xAE: // FEATUREEXTACK
			for !r.out {
				if r.u8() == 0xFF {
					break
				}
				r.take(int(r.u32()))
			}
		case 0xEE, 0xE4: // FEDAUTHINFO, SESSIONSTATE
			r.take(int(r.u32()))
		case 0xA9, 0xA5, 0xA4: // ORDER, COLINFO, TABNAME
			r.take(int(r.u16()))
		case 0x79: // RETURNSTATUS
			r.u32()
		case 0xAC: // RETURNVALUE
			r.u16()
			r.bstr()
			r.u8()
			r.take(6)
			t, ok := readTypeInfo(r)
			if !ok {
				out.bad = fmt.Sprintf("a value of type 0x%02X", t.id)
				break
			}
			readValue(r, t)
		default:
			out.bad = fmt.Sprintf("token 0x%02X", tok)
		}
	}
	if r.out && out.bad == "" {
		out.bad = "it ended early"
	}
	return out
}

var fixedSizes = map[byte]int{0x1F: 0, 0x30: 1, 0x32: 1, 0x34: 2, 0x38: 4, 0x3A: 4, 0x3B: 4, 0x3C: 8, 0x3D: 8, 0x3E: 8, 0x7A: 4, 0x7F: 8}

func readTypeInfo(r *rd) (colType, bool) {
	t := colType{id: r.u8()}
	if n, ok := fixedSizes[t.id]; ok {
		t.kind, t.size = lenFixed, n
		return t, true
	}
	switch t.id {
	case 0x24, 0x26, 0x68, 0x6D, 0x6E, 0x6F, 0x2F, 0x27, 0x2D, 0x25:
		t.kind, t.size = lenByte, int(r.u8())
	case 0x6A, 0x6C: // DECIMALN, NUMERICN
		t.kind, t.size = lenByte, int(r.u8())
		t.prec, t.scale = r.u8(), r.u8()
	case 0x28: // DATEN
		t.kind = lenByte
	case 0x29, 0x2A, 0x2B: // TIMEN, DATETIME2N, DATETIMEOFFSETN
		t.kind, t.scale = lenByte, r.u8()
	case 0xA5, 0xAD: // BIGVARBINARY, BIGBINARY
		t.size = int(r.u16())
		t.kind = lenShort
	case 0xA7, 0xAF, 0xE7, 0xEF: // BIGVARCHAR, BIGCHAR, NVARCHAR, NCHAR
		t.size = int(r.u16())
		r.take(5)
		t.kind = lenShort
		t.isNChar = t.id == 0xE7 || t.id == 0xEF
	case 0x23, 0x63, 0x22: // TEXT, NTEXT, IMAGE
		t.kind, t.size = lenText, int(r.u32())
		if t.id != 0x22 {
			r.take(5)
		}
		t.isNChar = t.id == 0x63
		for parts := int(r.u8()); parts > 0; parts-- {
			r.usstr()
		}
		return t, true
	case 0xF1: // XML
		t.kind, t.isNChar = lenPLP, true
		if r.u8() == 1 {
			r.bstr()
			r.bstr()
			r.usstr()
		}
		return t, true
	case 0xF0: // UDT
		r.u16()
		r.bstr()
		r.bstr()
		r.bstr()
		r.usstr()
		t.kind = lenPLP
		return t, true
	case 0x62: // SQL_VARIANT
		t.kind, t.size = lenVariant, int(r.u32())
		return t, true
	default:
		return t, false
	}
	if t.kind == lenShort && t.size == 0xFFFF {
		t.kind = lenPLP
	}
	return t, true
}

func readValue(r *rd, t colType) string {
	var data []byte
	switch t.kind {
	case lenFixed:
		if t.id == 0x1F {
			return "NULL"
		}
		data = r.take(t.size)
	case lenByte:
		n := int(r.u8())
		legacy := t.id == 0x2F || t.id == 0x27 || t.id == 0x2D || t.id == 0x25
		if (legacy && n == 0xFF) || (!legacy && n == 0) {
			return "NULL"
		}
		data = r.take(n)
	case lenShort:
		n := int(r.u16())
		if n == 0xFFFF {
			return "NULL"
		}
		data = r.take(n)
	case lenText:
		ptr := int(r.u8())
		if ptr == 0 {
			return "NULL"
		}
		r.take(ptr + 8)
		data = r.take(int(r.u32()))
	case lenPLP:
		total := r.u64()
		if total == math.MaxUint64 {
			return "NULL"
		}
		for !r.out {
			n := int(r.u32())
			if n == 0 {
				break
			}
			chunk := r.take(n)
			if len(data) < 64<<10 {
				data = append(data, chunk...)
			}
		}
	case lenVariant:
		n := int(r.u32())
		if n == 0 {
			return "NULL"
		}
		r.take(n)
		return "(sql_variant)"
	}
	return formatValue(t, data)
}

func formatValue(t colType, b []byte) string {
	le := binary.LittleEndian
	switch t.id {
	case 0x30, 0x32, 0x34, 0x38, 0x7F, 0x26, 0x68: // integers and bits
		switch len(b) {
		case 1:
			return strconv.Itoa(int(b[0]))
		case 2:
			return strconv.Itoa(int(int16(le.Uint16(b))))
		case 4:
			return strconv.Itoa(int(int32(le.Uint32(b))))
		case 8:
			return strconv.FormatInt(int64(le.Uint64(b)), 10)
		}
	case 0x3B, 0x3E, 0x6D: // floats
		if len(b) == 4 {
			return strconv.FormatFloat(float64(math.Float32frombits(le.Uint32(b))), 'g', -1, 32)
		}
		if len(b) == 8 {
			return strconv.FormatFloat(math.Float64frombits(le.Uint64(b)), 'g', -1, 64)
		}
	case 0x3C, 0x7A, 0x6E: // money
		var v int64
		if len(b) == 4 {
			v = int64(int32(le.Uint32(b)))
		} else if len(b) == 8 {
			v = int64(le.Uint32(b))<<32 | int64(le.Uint32(b[4:]))
		}
		return decimalText(big.NewInt(v), 4)
	case 0x3D, 0x3A, 0x6F: // datetime, smalldatetime
		base := time.Date(1900, 1, 1, 0, 0, 0, 0, time.UTC)
		if len(b) == 4 {
			return base.AddDate(0, 0, int(le.Uint16(b))).Add(time.Duration(le.Uint16(b[2:])) * time.Minute).Format("2006-01-02 15:04")
		}
		if len(b) == 8 {
			ticks := time.Duration(le.Uint32(b[4:])) * time.Second / 300
			return base.AddDate(0, 0, int(int32(le.Uint32(b)))).Add(ticks).Format("2006-01-02 15:04:05.000")
		}
	case 0x24: // uniqueidentifier
		if len(b) == 16 {
			o := []byte{b[3], b[2], b[1], b[0], b[5], b[4], b[7], b[6]}
			o = append(o, b[8:]...)
			h := hex.EncodeToString(o)
			return strings.ToUpper(h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:])
		}
	case 0x6A, 0x6C: // decimal, numeric
		if len(b) > 1 {
			mag := make([]byte, len(b)-1)
			for i := range mag {
				mag[i] = b[len(b)-1-i]
			}
			v := new(big.Int).SetBytes(mag)
			if b[0] == 0 {
				v.Neg(v)
			}
			return decimalText(v, int(t.scale))
		}
	case 0x28: // date
		return dateOf(b).Format("2006-01-02")
	case 0x29: // time
		return timeOf(b, t.scale)
	case 0x2A: // datetime2
		if len(b) > 3 {
			return dateOf(b[len(b)-3:]).Format("2006-01-02") + " " + timeOf(b[:len(b)-3], t.scale)
		}
	case 0x2B: // datetimeoffset, stored in UTC
		if len(b) > 5 {
			off := int(int16(le.Uint16(b[len(b)-2:])))
			day := dateOf(b[len(b)-5 : len(b)-2])
			clock := timeNanos(b[:len(b)-5], t.scale)
			at := day.Add(clock).Add(time.Duration(off) * time.Minute)
			return at.Format("2006-01-02 15:04:05.9999999") + fmt.Sprintf(" %+03d:%02d", off/60, abs(off%60))
		}
	case 0xE7, 0xEF, 0x63, 0xF1: // national text, xml
		return clip(fromUCS2(b))
	case 0xA7, 0xAF, 0x2F, 0x27, 0x23: // text
		if t.isNChar {
			return clip(fromUCS2(b))
		}
		if utf8.Valid(b) {
			return clip(string(b))
		}
		r := make([]rune, len(b))
		for i, c := range b {
			r[i] = rune(c)
		}
		return clip(string(r))
	case 0xA5, 0xAD, 0x2D, 0x25, 0x22, 0xF0: // binary
		if len(b) > 64 {
			return "0x" + strings.ToUpper(hex.EncodeToString(b[:64])) + "..."
		}
		return "0x" + strings.ToUpper(hex.EncodeToString(b))
	}
	return fmt.Sprintf("(a value of type 0x%02X)", t.id)
}

func dateOf(b []byte) time.Time {
	if len(b) < 3 {
		return time.Time{}
	}
	days := int(b[0]) | int(b[1])<<8 | int(b[2])<<16
	return time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, days)
}

func timeNanos(b []byte, scale byte) time.Duration {
	var v uint64
	for i := len(b) - 1; i >= 0; i-- {
		v = v<<8 | uint64(b[i])
	}
	for s := scale; s < 9; s++ {
		v *= 10
	}
	return time.Duration(v)
}

func timeOf(b []byte, scale byte) string {
	d := timeNanos(b, scale)
	return time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC).Add(d).Format("15:04:05.9999999")
}

func decimalText(v *big.Int, scale int) string {
	neg := v.Sign() < 0
	s := new(big.Int).Abs(v).String()
	if scale > 0 {
		for len(s) <= scale {
			s = "0" + s
		}
		s = s[:len(s)-scale] + "." + s[len(s)-scale:]
	}
	if neg {
		s = "-" + s
	}
	return s
}

func clip(s string) string {
	s = strings.NewReplacer("\r\n", " ", "\n", " ", "\t", " ").Replace(s)
	if utf8.RuneCountInString(s) > maxCellRunes {
		return string([]rune(s)[:maxCellRunes]) + "..."
	}
	return s
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
