package main

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ASKING THE DOMAIN'S OWN NAME SERVERS. A resolver caches what it was told, a "no such name" too
// (for the zone's negative TTL), so the plugin never asks one whether a record is served: it sends
// each question straight to the domain's authoritative name servers, without recursion, and takes
// only an authoritative answer. This file is the small part of the DNS wire format that needs
// (RFC 1035, with an EDNS0 OPT record so long TXT answers fit, and TCP when an answer is truncated).

// nameServer is one of a domain's authoritative name servers: its name, and the address it is
// asked at (host:port), empty when its address could not be found.
type nameServer struct {
	Name, Addr string
}

// The defaults only tests change.
var (
	// registrarServers are Namecheap BasicDNS's name servers, asked when a domain's NS records
	// cannot be read.
	registrarServers = []string{"dns1.registrar-servers.com", "dns2.registrar-servers.com"}
	authServers      = lookupAuthServers
	liveWait         = 60 * time.Second // how long a change waits for its records to be served
	liveBudget       = 5 * time.Minute  // the most one run waits for name servers, over all its changes
	livePoll         = 5 * time.Second  // how often it asks again meanwhile
	dnsTimeout       = 3 * time.Second  // one question to one server
)

// lookupAuthServers names a domain's authoritative name servers from its NS records, and finds
// each one's address. Only the servers' names and addresses come through the resolver; no record
// of the domain's own is looked up that way.
func lookupAuthServers(domain string) ([]nameServer, error) {
	var names []string
	if ns, err := net.LookupNS(domain); err == nil {
		for _, n := range ns {
			if h := normaliseDomain(n.Host); h != "" {
				names = append(names, h)
			}
		}
	}
	if len(names) == 0 {
		names = append(names, registrarServers...)
	}
	sort.Strings(names)
	var out []nameServer
	for _, n := range names {
		s := nameServer{Name: n}
		if ips, err := net.LookupIP(n); err == nil && len(ips) > 0 {
			ip := ips[0]
			for _, candidate := range ips {
				if candidate.To4() != nil {
					ip = candidate
					break
				}
			}
			s.Addr = net.JoinHostPort(ip.String(), "53")
		}
		out = append(out, s)
	}
	return out, nil
}

const (
	typeA     uint16 = 1
	typeNS    uint16 = 2
	typeCNAME uint16 = 5
	typeSOA   uint16 = 6
	typeMX    uint16 = 15
	typeTXT   uint16 = 16
	typeAAAA  uint16 = 28
	typeOPT   uint16 = 41
	typeCAA   uint16 = 257

	rcodeNXDomain = 3
)

var dnsTypes = map[string]uint16{"A": typeA, "NS": typeNS, "CNAME": typeCNAME, "SOA": typeSOA, "MX": typeMX, "TXT": typeTXT, "AAAA": typeAAAA, "CAA": typeCAA}

func typeName(t uint16) string {
	for name, n := range dnsTypes {
		if n == t {
			return name
		}
	}
	return "TYPE" + strconv.Itoa(int(t))
}

// rr is one resource record of an answer, its data in the plugin's own words: an address, a name
// without its trailing dot, a TXT record's strings joined, a CAA record as "flag tag value".
type rr struct {
	Name   string // lower case, no trailing dot
	Type   uint16
	TTL    uint32
	Value  string
	Pref   int    // MX
	SOAMin uint32 // SOA: the MINIMUM field, the zone's negative TTL
}

type dnsReply struct {
	ID        uint16
	Rcode     int
	AA, TC    bool
	Answer    []rr
	Authority []rr
}

// encodeName writes a domain name as DNS labels.
func encodeName(name string) ([]byte, error) {
	name = strings.TrimSuffix(name, ".")
	var b []byte
	if name != "" {
		for _, label := range strings.Split(name, ".") {
			if label == "" || len(label) > 63 {
				return nil, fmt.Errorf("%q is not a domain name", name)
			}
			b = append(b, byte(len(label)))
			b = append(b, label...)
		}
	}
	b = append(b, 0)
	if len(b) > 255 {
		return nil, fmt.Errorf("%q is too long for a domain name", name)
	}
	return b, nil
}

// buildQuery is one question, recursion not desired, with an OPT record offering 4096-byte UDP.
func buildQuery(id uint16, name string, qtype uint16) ([]byte, error) {
	qname, err := encodeName(name)
	if err != nil {
		return nil, err
	}
	msg := make([]byte, 12, 12+len(qname)+4+11)
	binary.BigEndian.PutUint16(msg[0:], id)
	binary.BigEndian.PutUint16(msg[4:], 1)  // one question
	binary.BigEndian.PutUint16(msg[10:], 1) // one additional: the OPT record
	msg = append(msg, qname...)
	msg = binary.BigEndian.AppendUint16(msg, qtype)
	msg = binary.BigEndian.AppendUint16(msg, 1) // class IN
	msg = append(msg, 0)                        // OPT: the root name
	msg = binary.BigEndian.AppendUint16(msg, typeOPT)
	msg = binary.BigEndian.AppendUint16(msg, 4096)
	msg = append(msg, 0, 0, 0, 0, 0, 0) // extended rcode and flags, no options
	return msg, nil
}

var errMalformed = errors.New("the answer could not be read")

// readName reads a possibly compressed name at off, and answers it and the offset after it.
func readName(msg []byte, off int) (string, int, error) {
	var labels []string
	end := -1
	for jumps := 0; ; {
		if off >= len(msg) {
			return "", 0, errMalformed
		}
		n := int(msg[off])
		switch {
		case n == 0:
			if end < 0 {
				end = off + 1
			}
			return strings.ToLower(strings.Join(labels, ".")), end, nil
		case n&0xC0 == 0xC0:
			if off+1 >= len(msg) || jumps > 20 {
				return "", 0, errMalformed
			}
			if end < 0 {
				end = off + 2
			}
			off = int(binary.BigEndian.Uint16(msg[off:]) & 0x3FFF)
			jumps++
		case n&0xC0 != 0:
			return "", 0, errMalformed
		default:
			if off+1+n > len(msg) {
				return "", 0, errMalformed
			}
			labels = append(labels, string(msg[off+1:off+1+n]))
			off += 1 + n
		}
	}
}

func parseReply(msg []byte) (*dnsReply, error) {
	if len(msg) < 12 {
		return nil, errMalformed
	}
	flags := binary.BigEndian.Uint16(msg[2:])
	r := &dnsReply{
		ID:    binary.BigEndian.Uint16(msg[0:]),
		Rcode: int(flags & 0x000F),
		AA:    flags&0x0400 != 0,
		TC:    flags&0x0200 != 0,
	}
	qd, an, ns := int(binary.BigEndian.Uint16(msg[4:])), int(binary.BigEndian.Uint16(msg[6:])), int(binary.BigEndian.Uint16(msg[8:]))
	off := 12
	for i := 0; i < qd; i++ {
		_, next, err := readName(msg, off)
		if err != nil || next+4 > len(msg) {
			return nil, errMalformed
		}
		off = next + 4
	}
	for i := 0; i < an+ns; i++ {
		rec, next, err := readRR(msg, off)
		if err != nil {
			return nil, err
		}
		off = next
		if i < an {
			r.Answer = append(r.Answer, rec)
		} else {
			r.Authority = append(r.Authority, rec)
		}
	}
	return r, nil
}

func readRR(msg []byte, off int) (rr, int, error) {
	name, off, err := readName(msg, off)
	if err != nil || off+10 > len(msg) {
		return rr{}, 0, errMalformed
	}
	rec := rr{Name: name, Type: binary.BigEndian.Uint16(msg[off:]), TTL: binary.BigEndian.Uint32(msg[off+4:])}
	size := int(binary.BigEndian.Uint16(msg[off+8:]))
	start := off + 10
	end := start + size
	if end > len(msg) {
		return rr{}, 0, errMalformed
	}
	data := msg[start:end]
	switch rec.Type {
	case typeA, typeAAAA:
		if len(data) != 4 && len(data) != 16 {
			return rr{}, 0, errMalformed
		}
		rec.Value = net.IP(data).String()
	case typeCNAME, typeNS:
		if rec.Value, _, err = readName(msg, start); err != nil {
			return rr{}, 0, err
		}
	case typeMX:
		if len(data) < 3 {
			return rr{}, 0, errMalformed
		}
		rec.Pref = int(binary.BigEndian.Uint16(data))
		if rec.Value, _, err = readName(msg, start+2); err != nil {
			return rr{}, 0, err
		}
	case typeTXT:
		var b strings.Builder
		for i := 0; i < len(data); {
			n := int(data[i])
			if i+1+n > len(data) {
				return rr{}, 0, errMalformed
			}
			b.Write(data[i+1 : i+1+n])
			i += 1 + n
		}
		rec.Value = b.String()
	case typeCAA:
		if len(data) < 2 || 2+int(data[1]) > len(data) {
			return rr{}, 0, errMalformed
		}
		tag := strings.ToLower(string(data[2 : 2+int(data[1])]))
		rec.Value = fmt.Sprintf("%d %s %s", data[0], tag, string(data[2+int(data[1]):]))
	case typeSOA:
		_, next, err := readName(msg, start) // the primary name server
		if err != nil {
			return rr{}, 0, err
		}
		if _, next, err = readName(msg, next); err != nil { // the mailbox
			return rr{}, 0, err
		}
		if next+20 > end {
			return rr{}, 0, errMalformed
		}
		rec.SOAMin = binary.BigEndian.Uint32(msg[next+16:])
	}
	return rec, end, nil
}

// exchange asks one server one question: over UDP, and again over TCP when the answer is
// truncated. The answer must carry the question's id.
func exchange(server nameServer, name string, qtype uint16) (*dnsReply, error) {
	if server.Addr == "" {
		return nil, errors.New("its address could not be found")
	}
	var idb [2]byte
	_, _ = rand.Read(idb[:])
	id := binary.BigEndian.Uint16(idb[:])
	query, err := buildQuery(id, name, qtype)
	if err != nil {
		return nil, err
	}

	reply, err := exchangeUDP(server.Addr, id, query)
	if err == nil && reply.TC {
		reply, err = exchangeTCP(server.Addr, id, query)
	}
	return reply, err
}

func exchangeUDP(addr string, id uint16, query []byte) (*dnsReply, error) {
	conn, err := net.DialTimeout("udp", addr, dnsTimeout)
	if err != nil {
		return nil, errors.New("it could not be reached")
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(dnsTimeout))
	if _, err := conn.Write(query); err != nil {
		return nil, errors.New("it could not be reached")
	}
	buf := make([]byte, 65535)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			return nil, fmt.Errorf("it did not answer within %s", dnsTimeout)
		}
		reply, err := parseReply(buf[:n])
		if err != nil || reply.ID != id {
			continue // not the answer to this question
		}
		return reply, nil
	}
}

func exchangeTCP(addr string, id uint16, query []byte) (*dnsReply, error) {
	conn, err := net.DialTimeout("tcp", addr, dnsTimeout)
	if err != nil {
		return nil, errors.New("it could not be reached over TCP")
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(dnsTimeout))
	framed := binary.BigEndian.AppendUint16(nil, uint16(len(query)))
	if _, err := conn.Write(append(framed, query...)); err != nil {
		return nil, errors.New("it could not be reached over TCP")
	}
	var size [2]byte
	if _, err := io.ReadFull(conn, size[:]); err != nil {
		return nil, fmt.Errorf("it did not answer within %s", dnsTimeout)
	}
	buf := make([]byte, binary.BigEndian.Uint16(size[:]))
	if _, err := io.ReadFull(conn, buf); err != nil {
		return nil, fmt.Errorf("it did not answer within %s", dnsTimeout)
	}
	reply, err := parseReply(buf)
	if err != nil {
		return nil, err
	}
	if reply.ID != id {
		return nil, errMalformed
	}
	return reply, nil
}

// serverAnswer is what one authoritative server serves for one name and type.
type serverAnswer struct {
	Server  nameServer
	Records []rr  // of the type asked
	Other   []rr  // at the name, of another type (a CNAME, when another type was asked)
	NoName  bool  // NXDOMAIN: no record of any type at the name
	NegTTL  int   // from the SOA in an empty answer's authority section; -1 when there was none
	Err     error // no usable answer
}

// ask asks one server for one name and type, and takes only an authoritative answer.
func ask(server nameServer, name string, qtype uint16) serverAnswer {
	a := serverAnswer{Server: server, NegTTL: -1}
	reply, err := exchange(server, name, qtype)
	switch {
	case err != nil:
		a.Err = err
		return a
	case reply.Rcode != 0 && reply.Rcode != rcodeNXDomain:
		a.Err = fmt.Errorf("it answered error code %d", reply.Rcode)
		return a
	case !reply.AA:
		a.Err = errors.New("its answer was not authoritative")
		return a
	}
	a.NoName = reply.Rcode == rcodeNXDomain
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	for _, rec := range reply.Answer {
		if rec.Name != name {
			continue
		}
		if rec.Type == qtype {
			a.Records = append(a.Records, rec)
		} else {
			a.Other = append(a.Other, rec)
		}
	}
	for _, rec := range reply.Authority {
		if rec.Type == typeSOA {
			a.NegTTL = negativeTTL(rec)
		}
	}
	return a
}

// negativeTTL is how long a resolver caches "no such name" from a zone: the lower of the SOA
// record's own TTL and its MINIMUM field (RFC 2308).
func negativeTTL(soa rr) int {
	return int(min(soa.TTL, soa.SOAMin))
}

// servedString writes a served record the way record.String writes a configured one.
func servedString(r rr) string {
	v := r.Value
	if r.Type == typeTXT || r.Type == typeCAA {
		v = strconv.Quote(v)
	}
	s := typeName(r.Type) + " " + v
	if r.Type == typeMX {
		s += fmt.Sprintf(" pref %d", r.Pref)
	}
	return s + fmt.Sprintf(" ttl %d", r.TTL)
}
