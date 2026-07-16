package p2p

import (
	"encoding/binary"
	"testing"
)

// TestUnmarshalMessageTooShort rejects anything shorter than the 24-byte frame.
func TestUnmarshalMessageTooShort(t *testing.T) {
	if _, err := UnmarshalMessage([]byte{0x01, 0x02, 0x03}); err == nil {
		t.Fatal("expected error for short message")
	}
}

// TestUnmarshalMessageLengthOverflow ensures a near-max uint32 payload length
// is rejected (the uint64 check must not wrap and admit a truncated payload).
func TestUnmarshalMessageLengthOverflow(t *testing.T) {
	// 24-byte frame (magic 4 + command 12 + length 4 + checksum 4).
	buf := make([]byte, 24)
	binary.LittleEndian.PutUint32(buf[16:20], 0xFFFFFF00) // > MaxPayloadSize
	if _, err := UnmarshalMessage(buf); err == nil {
		t.Fatal("expected error for oversized declared payload")
	}
}

// TestUnmarshalMessagePayloadShort ensures a declared payload longer than the
// buffer is rejected rather than slicing out of range.
func TestUnmarshalMessagePayloadShort(t *testing.T) {
	buf := make([]byte, 24+5)
	copy(buf[0:4], []byte{0x01, 0x02, 0x03, 0x04})
	binary.LittleEndian.PutUint32(buf[16:20], 100) // claims 100-byte payload, only 5 follow
	if _, err := UnmarshalMessage(buf); err == nil {
		t.Fatal("expected error when payload shorter than declared")
	}
}
