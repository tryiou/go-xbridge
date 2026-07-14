package p2p

import (
	"encoding/binary"
	"net"
	"testing"
)

// TestVersionMarshalShape verifies the version payload structure and that the
// well-known header fields land at the expected offsets (see docs/protocol.md
// §1.3 and Bitcoin's version message layout).
func TestVersionMarshalShape(t *testing.T) {
	remote, _ := net.ResolveTCPAddr("tcp", "1.2.3.4:41412")
	m := NewVersion(remote)
	payload := m.Marshal()

	// version(4) + services(8) + timestamp(8) + addr_recv(26) + addr_from(26)
	// + nonce(8) + varstr(userAgent) + start_height(4) + relay(1)
	wantLen := 4 + 8 + 8 + 26 + 26 + 8 + (1 + len(UserAgent)) + 4 + 1
	if len(payload) != wantLen {
		t.Fatalf("version payload len = %d, want %d", len(payload), wantLen)
	}

	ver := int32(binary.LittleEndian.Uint32(payload[0:4]))
	if ver != BitcoinProtocolVersion {
		t.Errorf("version = %d, want %d", ver, BitcoinProtocolVersion)
	}
	if m.Services != ServiceNodeNone {
		t.Errorf("services = %d, want %d", m.Services, ServiceNodeNone)
	}
	// relay bool is the final byte.
	if payload[len(payload)-1] != 0 {
		t.Errorf("relay byte = %d, want 0", payload[len(payload)-1])
	}
	// addr_recv begins at offset 20 (version 4 + services 8 + timestamp 8),
	// so its port (big-endian) sits at 20+8+16 = 44.
	gotPort := binary.BigEndian.Uint16(payload[44:46])
	if gotPort != 41412 {
		t.Errorf("addr_recv port = %d, want 41412", gotPort)
	}
	// ip(16) within addr_recv is at offset 20+8 = 28. An IPv4 peer address must
	// be serialized as a v6-mapped (::ffff:1.2.3.4).
	if payload[28+10] != 0xff || payload[28+11] != 0xff {
		t.Errorf("addr_recv not v6-mapped at offset 38")
	}
	if payload[28+12] != 1 || payload[28+13] != 2 ||
		payload[28+14] != 3 || payload[28+15] != 4 {
		t.Errorf("addr_recv IPv4 bytes wrong: got %v", payload[28+12:28+16])
	}
}

// TestVersionMarshalNilRemote ensures a nil remote (no connection yet) yields a
// zeroed addr_recv without panicking.
func TestVersionMarshalNilRemote(t *testing.T) {
	m := NewVersion(nil)
	if m.AddrRecv.Port != 0 || m.AddrRecv.IP != nil {
		t.Errorf("addr_recv should be zeroed for nil remote, got %+v", m.AddrRecv)
	}
	payload := m.Marshal()
	if len(payload) == 0 {
		t.Fatal("marshalled payload is empty")
	}
}
