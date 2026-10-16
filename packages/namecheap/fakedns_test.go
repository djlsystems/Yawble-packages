package main

import (
	"encoding/binary"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// A fake authoritative name server, one per name server a test domain has: a real UDP listener on
// the loopback address that answers DNS questions, authoritatively and without recursion, from the
// fake Namecheap's records. frozen makes it keep serving the records it had then, as a name server
// that has not picked a change up yet. No test reaches the network.

const fakeNegativeTTL = 3601 // the SOA MINIMUM Namecheap's zones carry

type fakeDNS struct {
	t      *testing.T
	name   string
	nc     *fakeNamecheap
	conn   net.PacketConn
	mu     sync.Mutex
	frozen map[string][]xmlHost // domain -> the records served while frozen; nil: serve live
	silent bool                 // answers nothing, as a server that cannot be reached
	asked  []string             // each question, as "name TYPE"
	soaTTL uint32
}

func newFakeDNS(t *testing.T, name string, nc *fakeNamecheap) *fakeDNS {
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	d := &fakeDNS{t: t, name: name, nc: nc, conn: conn, soaTTL: 3601}
	go d.serve()
	t.Cleanup(func() { conn.Close() })
	return d
}

func (d *fakeDNS) server() nameServer {
	return nameServer{Name: d.name, Addr: d.conn.LocalAddr().String()}
}

// freeze makes the server keep serving the records the domain has now.
func (d *fakeDNS) freeze(domain string) {
	hosts := d.nc.hosts(domain)
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.frozen == nil {
		d.frozen = map[string][]xmlHost{}
	}
	d.frozen[domain] = hosts
}

func (d *fakeDNS) questions() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string{}, d.asked...)
}

func (d *fakeDNS) serve() {
	buf := make([]byte, 4096)
	for {
		n, from, err := d.conn.ReadFrom(buf)
		if err != nil {
			return
		}
		if reply := d.answer(buf[:n]); reply != nil {
			_, _ = d.conn.WriteTo(reply, from)
		}
	}
}

func (d *fakeDNS) answer(q []byte) []byte {
	if len(q) < 12 || binary.BigEndian.Uint16(q[2:])&0x0100 != 0 {
		d.t.Errorf("%s: a question with recursion desired, or none; the plugin asks authoritative servers only", d.name)
		return nil
	}
	name, off, err := readName(q, 12)
	if err != nil || off+4 > len(q) {
		return nil
	}
	qtype := binary.BigEndian.Uint16(q[off:])
	question := q[12 : off+4]

	d.mu.Lock()
	d.asked = append(d.asked, name+" "+typeName(qtype))
	silent := d.silent
	frozen, isFrozen := d.frozen[d.zoneOf(name)]
	d.mu.Unlock()
	if silent {
		return nil
	}

	zone := d.zoneOf(name)
	rcode := 0
	var answers [][]byte
	var authority [][]byte
	if zone == "" {
		rcode = 5 // REFUSED: not a zone this server holds
	} else {
		hosts := frozen
		if !isFrozen {
			hosts = d.nc.hosts(zone)
		}
		exists := name == zone
		for _, h := range hosts {
			if fqdn(normaliseHost(h.Name, zone), zone) != name || !changeable[strings.ToUpper(h.Type)] {
				continue
			}
			exists = true
			t := dnsTypes[strings.ToUpper(h.Type)]
			if t == qtype || t == typeCNAME {
				answers = append(answers, rrBytes(name, t, h))
			}
		}
		if qtype == typeSOA && name == zone {
			answers = append(answers, d.soa(zone))
		}
		if len(answers) == 0 {
			authority = append(authority, d.soa(zone))
			if !exists {
				rcode = rcodeNXDomain
			}
		}
	}

	msg := make([]byte, 12)
	copy(msg, q[:2])
	flags := uint16(0x8000 | 0x0400 | rcode) // a response, authoritative
	binary.BigEndian.PutUint16(msg[2:], flags)
	binary.BigEndian.PutUint16(msg[4:], 1)
	binary.BigEndian.PutUint16(msg[6:], uint16(len(answers)))
	binary.BigEndian.PutUint16(msg[8:], uint16(len(authority)))
	msg = append(msg, question...)
	for _, a := range append(answers, authority...) {
		msg = append(msg, a...)
	}
	return msg
}

func (d *fakeDNS) zoneOf(name string) string {
	d.nc.mu.Lock()
	defer d.nc.mu.Unlock()
	for zone := range d.nc.domains {
		if name == zone || strings.HasSuffix(name, "."+zone) {
			return zone
		}
	}
	return ""
}

func (d *fakeDNS) soa(zone string) []byte {
	mname, _ := encodeName(d.name)
	rname, _ := encodeName("hostmaster." + zone)
	data := append(append([]byte{}, mname...), rname...)
	for _, v := range []uint32{2026101601, 43200, 3600, 604800, fakeNegativeTTL} {
		data = binary.BigEndian.AppendUint32(data, v)
	}
	return rrWire(zone, typeSOA, d.soaTTL, data)
}

func rrWire(name string, t uint16, ttl uint32, data []byte) []byte {
	b, _ := encodeName(name)
	b = binary.BigEndian.AppendUint16(b, t)
	b = binary.BigEndian.AppendUint16(b, 1)
	b = binary.BigEndian.AppendUint32(b, ttl)
	b = binary.BigEndian.AppendUint16(b, uint16(len(data)))
	return append(b, data...)
}

// rrBytes serves a Namecheap host record as Namecheap's name servers do.
func rrBytes(name string, t uint16, h xmlHost) []byte {
	ttl, _ := strconv.Atoi(h.TTL)
	var data []byte
	switch t {
	case typeA:
		data = net.ParseIP(h.Address).To4()
	case typeAAAA:
		data = net.ParseIP(h.Address).To16()
	case typeCNAME:
		data, _ = encodeName(h.Address)
	case typeMX:
		pref, _ := strconv.Atoi(h.MXPref)
		target, _ := encodeName(h.Address)
		data = append(binary.BigEndian.AppendUint16(nil, uint16(pref)), target...)
	case typeTXT:
		v := h.Address
		for len(v) > 255 {
			data = append(append(data, 255), v[:255]...)
			v = v[255:]
		}
		data = append(append(data, byte(len(v))), v...)
	case typeCAA:
		parts := strings.SplitN(caaValue(h.Address), " ", 3)
		flag, _ := strconv.Atoi(parts[0])
		data = append([]byte{byte(flag), byte(len(parts[1]))}, parts[1]...)
		data = append(data, parts[2]...)
	}
	return rrWire(name, t, uint32(ttl), data)
}
