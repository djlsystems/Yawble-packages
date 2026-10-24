package main

import "testing"

// Azure SQL resets a connection whose TDS packet arrives split across TLS records, which Go's
// dynamic record sizing does to the first writes after the handshake: the login never arrived.
func TestASQLConnectionSendsEachPacketInOneTLSRecord(t *testing.T) {
	host := sqlHost("my-sql-server")
	c := sqlTLS(host)
	if !c.DynamicRecordSizingDisabled {
		t.Fatal("dynamic record sizing is on, so a login packet is split across TLS records")
	}
	if c.ServerName != host || len(c.NextProtos) != 1 || c.NextProtos[0] != "tds/8.0" {
		t.Fatalf("tls config = %+v", c)
	}
	if maxSQLPacket > 16383 {
		t.Fatalf("a packet of %d bytes does not fit one TLS record", maxSQLPacket)
	}
}
