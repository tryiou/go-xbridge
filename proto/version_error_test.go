package proto

import (
	"encoding/binary"
	"errors"
	"strconv"
	"strings"
	"testing"

	"go-xbridge/version"
)

// wrongVersionBytes builds a header-sized buffer stamped with ver. The body
// is absent on purpose: the version gate runs before any body parsing.
func wrongVersionBytes(ver uint32) []byte {
	buf := make([]byte, HeaderSize)
	binary.LittleEndian.PutUint32(buf[offVersion:], ver)
	binary.LittleEndian.PutUint32(buf[offCommand:], uint32(XbcTransaction))
	return buf
}

// TestUnmarshalVersionErrorIsSentinel pins the classification hook the P2P
// reader uses to separate expected version drift (minority fork) from
// corrupt frames: a wrong-version header must match ErrUnsupportedVersion.
func TestUnmarshalVersionErrorIsSentinel(t *testing.T) {
	_, err := Unmarshal(wrongVersionBytes(version.XBridgeProtocolVersion - 1))
	if !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("err = %v, want errors.Is ErrUnsupportedVersion", err)
	}
}

// TestUnmarshalVersionErrorCarriesGotWant pins the got/want fields the old
// proto-layer log line carried: with library logging removed, the single
// reader-level line must still identify both sides of the mismatch. No
// literals: both numbers derive from the single source.
func TestUnmarshalVersionErrorCarriesGotWant(t *testing.T) {
	wrong := version.XBridgeProtocolVersion - 1
	_, err := Unmarshal(wrongVersionBytes(wrong))
	if err == nil {
		t.Fatal("expected error, got parsed packet")
	}
	msg := err.Error()
	if !strings.Contains(msg, strconv.FormatUint(uint64(wrong), 10)) {
		t.Errorf("err = %q, want it to carry the received version %d", msg, wrong)
	}
	if !strings.Contains(msg, strconv.FormatUint(uint64(version.XBridgeProtocolVersion), 10)) {
		t.Errorf("err = %q, want it to carry the wanted version %d", msg, version.XBridgeProtocolVersion)
	}
}
