package proto

import (
	"encoding/binary"
	"fmt"
	"testing"

	"go-xbridge/version"
)

// TestUnmarshalTruncatedHeader rejects anything shorter than the 129-byte header.
func TestUnmarshalTruncatedHeader(t *testing.T) {
	if _, err := Unmarshal([]byte{0x01, 0x02}); err == nil {
		t.Fatal("expected error for data shorter than packet header")
	}
}

// TestUnmarshalBodySizeOverflow ensures a near-max uint32 body size is rejected
// (the uint64 length check must not wrap and admit a truncated/oversized body).
func TestUnmarshalBodySizeOverflow(t *testing.T) {
	buf := make([]byte, HeaderSize)
	binary.LittleEndian.PutUint32(buf[offVersion:], version.XBridgeProtocolVersion)
	binary.LittleEndian.PutUint32(buf[offCommand:], uint32(XbcTransaction))
	binary.LittleEndian.PutUint32(buf[offSize:], 0xFFFFFF00)
	if _, err := Unmarshal(buf); err == nil {
		t.Fatal("expected error for oversized declared body size")
	}
}

// TestUnmarshalBodySizeCap ensures the MaxBodySize cap is enforced.
func TestUnmarshalBodySizeCap(t *testing.T) {
	buf := make([]byte, HeaderSize)
	binary.LittleEndian.PutUint32(buf[offVersion:], version.XBridgeProtocolVersion)
	binary.LittleEndian.PutUint32(buf[offCommand:], uint32(XbcTransaction))
	binary.LittleEndian.PutUint32(buf[offSize:], MaxBodySize+1)
	if _, err := Unmarshal(buf); err == nil {
		t.Fatal("expected error when body size exceeds MaxBodySize")
	}
}

// TestUnmarshalBodyExceedsData ensures a declared body longer than the buffer
// is rejected rather than slicing out of range.
func TestUnmarshalBodyExceedsData(t *testing.T) {
	buf := make([]byte, HeaderSize+10)
	binary.LittleEndian.PutUint32(buf[offVersion:], version.XBridgeProtocolVersion)
	binary.LittleEndian.PutUint32(buf[offCommand:], uint32(XbcTransaction))
	binary.LittleEndian.PutUint32(buf[offSize:], 100) // claims 100-byte body, only 10 follow
	if _, err := Unmarshal(buf); err == nil {
		t.Fatal("expected error when declared body exceeds data")
	}
}

// TestUnmarshalTrailingBytes rejects bytes beyond the declared body, matching
// C++ XBridgePacket::copyFrom's exact-length check (xbridgepacket.h:489-493).
func TestUnmarshalTrailingBytes(t *testing.T) {
	body := []byte{0x01, 0x02, 0x03}
	buf := make([]byte, HeaderSize+len(body)+4) // 4 stray trailing bytes
	binary.LittleEndian.PutUint32(buf[offVersion:], version.XBridgeProtocolVersion)
	binary.LittleEndian.PutUint32(buf[offCommand:], uint32(XbcTransaction))
	binary.LittleEndian.PutUint32(buf[offSize:], uint32(len(body)))
	copy(buf[BodyOffset:], body)
	if _, err := Unmarshal(buf); err == nil {
		t.Fatal("expected error for trailing bytes after the declared body")
	}

	// Exact-length packets still parse.
	p, err := Unmarshal(buf[:HeaderSize+len(body)])
	if err != nil {
		t.Fatalf("exact-length packet: %v", err)
	}
	if string(p.Body) != string(body) {
		t.Fatalf("body = %x, want %x", p.Body, body)
	}
}

// TestUnmarshalRejectsWrongVersion locks in the inbound protocol-version gate:
// C++ drops any packet whose header version differs from
// XBRIDGE_PROTOCOL_VERSION before parsing the body or verifying the signature
// (Session::checkXBridgePacketVersion, xbridgesession.cpp:343-368, called from
// App::onMessageReceived/onBroadcastReceived, xbridgeapp.cpp:648,737). The
// wrong-version packet is rejected with the version error even when its
// declared body is otherwise oversized, proving the gate fires before body
// validation.
func TestUnmarshalRejectsWrongVersion(t *testing.T) {
	for _, ver := range []uint32{0, 54, 56, 0xFFFFFFFF} {
		buf := make([]byte, HeaderSize)
		binary.LittleEndian.PutUint32(buf[offVersion:], ver)
		binary.LittleEndian.PutUint32(buf[offCommand:], uint32(XbcTransaction))
		if _, err := Unmarshal(buf); err == nil {
			t.Fatalf("version %d: expected error, got parsed packet", ver)
		}
	}

	// A wrong-version packet with an absurd declared body must report the
	// version mismatch, not the body-size error: the gate runs first. The
	// message carries both sides of the mismatch (derived, no literals).
	wrong := version.XBridgeProtocolVersion - 1
	buf := make([]byte, HeaderSize)
	binary.LittleEndian.PutUint32(buf[offVersion:], wrong)
	binary.LittleEndian.PutUint32(buf[offCommand:], uint32(XbcTransaction))
	binary.LittleEndian.PutUint32(buf[offSize:], MaxBodySize+1)
	want := fmt.Sprintf("xbridge: unsupported protocol version (got %d, want %d)", wrong, version.XBridgeProtocolVersion)
	if _, err := Unmarshal(buf); err == nil || err.Error() != want {
		t.Fatalf("wrong-version oversized-body: err = %v, want %q", err, want)
	}

	// Control: a matching-version packet still parses.
	buf = make([]byte, HeaderSize)
	binary.LittleEndian.PutUint32(buf[offVersion:], version.XBridgeProtocolVersion)
	binary.LittleEndian.PutUint32(buf[offCommand:], uint32(XbcTransaction))
	if _, err := Unmarshal(buf); err != nil {
		t.Fatalf("protocol version %d: %v", version.XBridgeProtocolVersion, err)
	}
}
