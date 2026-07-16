package proto

import (
	"encoding/binary"
	"testing"
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
	binary.LittleEndian.PutUint32(buf[offVersion:], ProtocolVersion)
	binary.LittleEndian.PutUint32(buf[offCommand:], uint32(XbcTransaction))
	binary.LittleEndian.PutUint32(buf[offSize:], 0xFFFFFF00)
	if _, err := Unmarshal(buf); err == nil {
		t.Fatal("expected error for oversized declared body size")
	}
}

// TestUnmarshalBodySizeCap ensures the MaxBodySize cap is enforced.
func TestUnmarshalBodySizeCap(t *testing.T) {
	buf := make([]byte, HeaderSize)
	binary.LittleEndian.PutUint32(buf[offVersion:], ProtocolVersion)
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
	binary.LittleEndian.PutUint32(buf[offVersion:], ProtocolVersion)
	binary.LittleEndian.PutUint32(buf[offCommand:], uint32(XbcTransaction))
	binary.LittleEndian.PutUint32(buf[offSize:], 100) // claims 100-byte body, only 10 follow
	if _, err := Unmarshal(buf); err == nil {
		t.Fatal("expected error when declared body exceeds data")
	}
}
