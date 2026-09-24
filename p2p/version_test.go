package p2p

import (
	"bytes"
	"encoding/binary"
	"net"
	"testing"

	"go-xbridge/version"
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
	wantLen := 4 + 8 + 8 + 26 + 26 + 8 + (1 + len(version.UserAgent)) + 4 + 1 + 1
	if len(payload) != wantLen {
		t.Fatalf("version payload len = %d, want %d", len(payload), wantLen)
	}

	ver := int32(binary.LittleEndian.Uint32(payload[0:4]))
	if ver != version.BitcoinProtocolVersion {
		t.Errorf("version = %d, want %d", ver, version.BitcoinProtocolVersion)
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
	// CAddress is the 26-byte form: services(8) then ip(16), so the port
	// (big-endian) sits at 20+8+16 = 44.
	gotPort := binary.BigEndian.Uint16(payload[44:46])
	if gotPort != 41412 {
		t.Errorf("addr_recv port = %d, want 41412", gotPort)
	}
	// ip(16) within addr_recv is at offset 20+8 = 28. An IPv4 peer address
	// must be serialized as a v6-mapped (::ffff:1.2.3.4).
	if payload[28+10] != 0xff || payload[28+11] != 0xff {
		t.Errorf("addr_recv not v6-mapped at offset 38")
	}
	if payload[28+12] != 1 || payload[28+13] != 2 ||
		payload[28+14] != 3 || payload[28+15] != 4 {
		t.Errorf("addr_recv IPv4 bytes wrong: got %v", payload[28+12:28+16])
	}
	// The version message's addrs carry NO per-addr nTime (that 4-byte field
	// lives only in addr/getaddr records). The address block is exactly 26+26
	// bytes; there must be no 4-byte nTime prefix on addr_recv (offset 20).
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

// TestMarshalNetAddr26 verifies a version-message CAddress serializes to
// exactly 26 bytes (services || ip || port, NO per-addr nTime). The nTime
// role in the version message is filled by the message-level timestamp field,
// not by the embedded addrs. (The 30-byte nTime-bearing form is only for
// addr/getaddr records; see p2p/addr.go.)
func TestMarshalNetAddr26(t *testing.T) {
	a := NetAddr{
		Services: 0x0102030405060708,
		IP:       net.ParseIP("1.2.3.4"),
		Port:     41412,
	}
	b := marshalNetAddr(a)
	if len(b) != 26 {
		t.Fatalf("marshalNetAddr len = %d, want 26", len(b))
	}
	if got := binary.LittleEndian.Uint64(b[0:8]); got != 0x0102030405060708 {
		t.Errorf("services = %#x, want 0x0102030405060708", got)
	}
	if b[8+10] != 0xff || b[8+11] != 0xff {
		t.Errorf("ip not v6-mapped: %v", b[8:24])
	}
	if b[8+12] != 1 || b[8+13] != 2 || b[8+14] != 3 || b[8+15] != 4 {
		t.Errorf("ipv4 bytes wrong: %v", b[8+12:24])
	}
	if got := binary.BigEndian.Uint16(b[24:26]); got != 41412 {
		t.Errorf("port = %d, want 41412", got)
	}
}

// TestNetAddrRoundTrip verifies marshal/unmarshal of a 26-byte version
// CAddress and that a buffer shorter than 26 bytes is rejected.
func TestNetAddrRoundTrip(t *testing.T) {
	in := NetAddr{
		Services: 9,
		IP:       net.ParseIP("8.8.4.4"),
		Port:     18332,
	}
	b := marshalNetAddr(in)
	out, n, err := unmarshalNetAddr(b)
	if err != nil {
		t.Fatalf("unmarshalNetAddr: %v", err)
	}
	if n != 26 {
		t.Errorf("consumed = %d, want 26", n)
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
	if _, _, err := unmarshalNetAddr(b[:25]); err == nil {
		t.Error("unmarshalNetAddr(<26 bytes) should error")
	}
}

// TestMarshalVarStrFullCompactSize verifies marshalVarStr emits the complete
// CompactSize range, mirroring C++ WriteCompactSize (serialize.h:255-273). The
// old writer only handled the single-byte and 0xFD (uint16) forms; a string
// longer than 0xFFFF wrapped its length in uint16, so a 0x10000-byte string
// produced a 0xFD-prefixed zero-length header (corrupt). Each boundary must
// round-trip through unmarshalVarStr and carry the correct prefix byte.
func TestMarshalVarStrFullCompactSize(t *testing.T) {
	cases := []struct {
		name string
		n    int
	}{
		{"single-byte-max", 0xFC},                 // 1-byte prefix (len < 0xFD)
		{"uint16-min", 0xFD},                      // 0xFD + uint16 lower bound
		{"uint16-max", 0xFFFF},                    // 0xFD + uint16 upper bound
		{"uint32-min", 0x10000},                   // 0xFE + uint32 lower bound (old code wrapped to zero length)
		{"uint32-mid", 0x10001},                   // just past the wrap point
		{"uint32-large", 0x01000000},              // 16 MiB, well into the 0xFE range
		{"uint32-max", 0xFFFFFFFF},                // 0xFE + uint32 upper bound (prefix-only, 4 GiB payload)
		{"uint64-min-representable", 0x100000000}, // 0xFF + uint64 (4 GiB — no payload copy)
	}
	// allocatablePayload is the largest varstr payload a test allocates for a
	// real round-trip; larger cases assert the header via writeVarInt only.
	const allocatablePayload = 16 << 20 // 16 MiB
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var b []byte
			if c.n <= allocatablePayload {
				// Real payload for the affordable cases.
				s := make([]byte, c.n)
				for i := range s {
					s[i] = byte(i)
				}
				b = marshalVarStr(string(s))
				got, n, err := unmarshalVarStr(b)
				if err != nil {
					t.Fatalf("unmarshalVarStr: %v", err)
				}
				if n != len(b) {
					t.Errorf("consumed = %d, want %d (no trailing bytes)", n, len(b))
				}
				if len(got) != c.n {
					t.Errorf("round-trip length = %d, want %d", len(got), c.n)
				}
				if !bytes.Equal([]byte(got), s) {
					t.Error("round-trip content differs from the payload")
				}
			} else {
				// 4 GiB+ payloads are not allocatable; assert the 0xFE/0xFF
				// headers via writeVarInt directly (0xFE + uint32 LE,
				// 0xFF + uint64 LE).
				got := writeVarInt(c.n)
				wantFirst := byte(0xFE)
				var wantLen uint64
				if c.n > 0xFFFFFFFF {
					wantFirst = 0xFF
					wantLen = uint64(c.n)
				} else {
					wantLen = uint64(c.n)
				}
				if got[0] != wantFirst {
					t.Errorf("prefix byte = 0x%02x, want 0x%02x", got[0], wantFirst)
				}
				if c.n <= 0xFFFFFFFF {
					if l := binary.LittleEndian.Uint32(got[1:5]); uint64(l) != wantLen {
						t.Errorf("0xFE length field = %d, want %d", l, wantLen)
					}
				} else if l := binary.LittleEndian.Uint64(got[1:9]); l != wantLen {
					t.Errorf("0xFF length field = %d, want %d", l, wantLen)
				}
			}
		})
	}
}

// TestMarshalVarStrPrefixBytes locks in the exact prefix byte per CompactSize
// range, so a regression to the 0xFD-only writer is caught directly (the
// uint32-min case previously emitted 0xFD + a wrapped uint16).
func TestMarshalVarStrPrefixBytes(t *testing.T) {
	prefixFor := func(n int) byte {
		b := marshalVarStr(string(make([]byte, n)))
		return b[0]
	}
	if got := prefixFor(0x10); got != 0x10 {
		t.Errorf("len 0x10 prefix = 0x%02x, want single-byte", got)
	}
	if got := prefixFor(0xFD); got != 0xFD {
		t.Errorf("len 0xFD prefix = 0x%02x, want 0xFD", got)
	}
	if got := prefixFor(0x10000); got != 0xFE {
		t.Errorf("len 0x10000 prefix = 0x%02x, want 0xFE (was 0xFD + wrapped length)", got)
	}
}
