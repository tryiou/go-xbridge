package p2p

import (
	"encoding/binary"
	"errors"
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

// TestUnmarshalMessageChecksumSentinel asserts a bad checksum surfaces the
// exported ErrChecksum sentinel (the connection layer drops it non-fatally,
// mirroring C++ net_processing.cpp:3138-3145).
func TestUnmarshalMessageChecksumSentinel(t *testing.T) {
	m := &Message{
		Magic:    MainnetMagic,
		Command:  "ping",
		Payload:  []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08},
		Checksum: Checksum([]byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08}),
	}
	buf := m.Marshal()
	buf[len(buf)-1] ^= 0xff // corrupt the payload so the checksum no longer matches
	_, err := UnmarshalMessage(buf)
	if !errors.Is(err, ErrChecksum) {
		t.Fatalf("err = %v, want errors.Is(ErrChecksum)", err)
	}
}
