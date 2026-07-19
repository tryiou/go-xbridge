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

	// version(4) + services(8) + timestamp(8) + addr_recv(30) + addr_from(30)
	// + nonce(8) + varstr(userAgent) + start_height(4) + relay(1) + fxrouter(1)
	wantLen := 4 + 8 + 8 + 30 + 30 + 8 + (1 + len(UserAgent)) + 4 + 1 + 1
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
	// addr_recv begins at offset 20 (version 4 + services 8 + timestamp 8). Each
	// CAddress leads with a 4-byte nTime, then services(8) then ip(16), so the
	// port (big-endian) sits at 20+4+8+16 = 48.
	gotPort := binary.BigEndian.Uint16(payload[48:50])
	if gotPort != 41412 {
		t.Errorf("addr_recv port = %d, want 41412", gotPort)
	}
	// ip(16) within addr_recv is at offset 20+4+8 = 32. An IPv4 peer address
	// must be serialized as a v6-mapped (::ffff:1.2.3.4).
	if payload[32+10] != 0xff || payload[32+11] != 0xff {
		t.Errorf("addr_recv not v6-mapped at offset 42")
	}
	if payload[32+12] != 1 || payload[32+13] != 2 ||
		payload[32+14] != 3 || payload[32+15] != 4 {
		t.Errorf("addr_recv IPv4 bytes wrong: got %v", payload[32+12:32+16])
	}
	// addr_recv nTime is a non-zero 4-byte LE timestamp at offset 20.
	if binary.LittleEndian.Uint32(payload[20:24]) == 0 {
		t.Errorf("addr_recv nTime should be non-zero")
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

// TestMarshalNetAddr30 verifies a CAddress serializes to exactly 30 bytes with
// the 4-byte LE nTime first, then services(8 LE), ip(16), port(2 BE), matching
// C++ CAddress::SerializationOp for stream version >= CADDR_TIME_VERSION.
func TestMarshalNetAddr30(t *testing.T) {
	a := NetAddr{
		Timestamp: 0x11223344,
		Services:  0x0102030405060708,
		IP:        net.ParseIP("1.2.3.4"),
		Port:      41412,
	}
	b := marshalNetAddr(a)
	if len(b) != 30 {
		t.Fatalf("marshalNetAddr len = %d, want 30", len(b))
	}
	if got := binary.LittleEndian.Uint32(b[0:4]); got != 0x11223344 {
		t.Errorf("nTime = %#x, want 0x11223344", got)
	}
	if got := binary.LittleEndian.Uint64(b[4:12]); got != 0x0102030405060708 {
		t.Errorf("services = %#x, want 0x0102030405060708", got)
	}
	if b[12+10] != 0xff || b[12+11] != 0xff {
		t.Errorf("ip not v6-mapped: %v", b[12:28])
	}
	if b[12+12] != 1 || b[12+13] != 2 || b[12+14] != 3 || b[12+15] != 4 {
		t.Errorf("ipv4 bytes wrong: %v", b[12+12:28])
	}
	if got := binary.BigEndian.Uint16(b[28:30]); got != 41412 {
		t.Errorf("port = %d, want 41412", got)
	}
}

// TestNetAddrRoundTrip verifies marshal/unmarshal of a 30-byte CAddress and
// that a buffer shorter than 30 bytes is rejected.
func TestNetAddrRoundTrip(t *testing.T) {
	in := NetAddr{
		Timestamp: 0x5a5a5a5a,
		Services:  9,
		IP:        net.ParseIP("8.8.4.4"),
		Port:      18332,
	}
	b := marshalNetAddr(in)
	out, n, err := unmarshalNetAddr(b)
	if err != nil {
		t.Fatalf("unmarshalNetAddr: %v", err)
	}
	if n != 30 {
		t.Errorf("consumed = %d, want 30", n)
	}
	if out.Timestamp != in.Timestamp {
		t.Errorf("timestamp = %#x, want %#x", out.Timestamp, in.Timestamp)
	}
	if out.Services != in.Services {
		t.Errorf("services = %d, want %d", out.Services, in.Services)
	}
	if !out.IP.Equal(in.IP) {
		t.Errorf("ip = %v, want %v", out.IP, in.IP)
	}
	if out.Port != in.Port {
		t.Errorf("port = %d, want %d", out.Port, in.Port)
	}
	if _, _, err := unmarshalNetAddr(b[:29]); err == nil {
		t.Error("unmarshalNetAddr(<30 bytes) should error")
	}
}

// TestVersionAddrTimestampRoundTrip verifies NewVersion stamps both addrs with
// a non-zero nTime and that UnmarshalVersion recovers them.
func TestVersionAddrTimestampRoundTrip(t *testing.T) {
	remote, _ := net.ResolveTCPAddr("tcp", "1.2.3.4:41412")
	m := NewVersion(remote)
	if m.AddrRecv.Timestamp == 0 || m.AddrFrom.Timestamp == 0 {
		t.Fatalf("addr timestamps should be stamped: recv=%d from=%d",
			m.AddrRecv.Timestamp, m.AddrFrom.Timestamp)
	}
	got, err := UnmarshalVersion(m.Marshal())
	if err != nil {
		t.Fatalf("UnmarshalVersion: %v", err)
	}
	if got.AddrRecv.Timestamp != m.AddrRecv.Timestamp {
		t.Errorf("addr_recv nTime = %d, want %d", got.AddrRecv.Timestamp, m.AddrRecv.Timestamp)
	}
	if got.AddrFrom.Timestamp != m.AddrFrom.Timestamp {
		t.Errorf("addr_from nTime = %d, want %d", got.AddrFrom.Timestamp, m.AddrFrom.Timestamp)
	}
}
