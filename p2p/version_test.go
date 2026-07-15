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
	// + nonce(8) + varstr(userAgent) + start_height(4) + relay(1) + fxrouter(1)
	wantLen := 4 + 8 + 8 + 26 + 26 + 8 + (1 + len(UserAgent)) + 4 + 1 + 1
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
	// fxrouter bool is the final byte; relay is the second-to-last.
	if payload[len(payload)-1] != 0 {
		t.Errorf("fxrouter byte = %d, want 0", payload[len(payload)-1])
	}
	if payload[len(payload)-2] != 0 {
		t.Errorf("relay byte = %d, want 0", payload[len(payload)-2])
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

// TestVersionFXRouterRoundTrip verifies the trailing fXRouter byte marshals and
// unmarshals independently of Relay, and that a payload without the fXRouter
// byte (older peers / best-effort parsing) still parses with fXRouter=false.
func TestVersionFXRouterRoundTrip(t *testing.T) {
	remote, _ := net.ResolveTCPAddr("tcp", "1.2.3.4:41412")
	m := NewVersion(remote)
	payload := m.Marshal()

	got, err := UnmarshalVersion(payload)
	if err != nil {
		t.Fatalf("UnmarshalVersion: %v", err)
	}
	if got.FXRouter != false {
		t.Errorf("fXRouter = %v, want false", got.FXRouter)
	}
	if got.Relay != false {
		t.Errorf("relay = %v, want false", got.Relay)
	}

	// A payload that explicitly sets fXRouter=true must round-trip.
	m.FXRouter = true
	got2, err := UnmarshalVersion(m.Marshal())
	if err != nil {
		t.Fatalf("UnmarshalVersion(true): %v", err)
	}
	if !got2.FXRouter {
		t.Errorf("fXRouter = %v, want true", got2.FXRouter)
	}

	// Back-compat: drop the final byte (no fXRouter). Relay stays readable and
	// fXRouter defaults to false (so discovery is enabled, as C++ requires).
	short := payload[:len(payload)-1]
	got3, err := UnmarshalVersion(short)
	if err != nil {
		t.Fatalf("UnmarshalVersion(short): %v", err)
	}
	if got3.FXRouter != false {
		t.Errorf("short fXRouter = %v, want false", got3.FXRouter)
	}
	if got3.Relay != false {
		t.Errorf("short relay = %v, want false", got3.Relay)
	}
}
