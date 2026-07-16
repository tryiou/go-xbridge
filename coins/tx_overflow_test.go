package coins

import "testing"

// TestDeserializeTooManyInputs ensures a peer/wallet-supplied input count that
// would OOM (the 32-bit varint allows ~4e9) is rejected before allocation.
func TestDeserializeTooManyInputs(t *testing.T) {
	// version(4) = 0x01000000, then varint 0xfe + uint32 0xFFFFFFFE (~4.29e9 inputs).
	buf := []byte{0x01, 0x00, 0x00, 0x00, 0xfe, 0xfe, 0xff, 0xff, 0xff}
	if _, err := Deserialize(buf); err == nil {
		t.Fatal("expected error for excessive input count")
	}
}

// TestDeserializeTooManyOutputs mirrors the input guard for outputs.
func TestDeserializeTooManyOutputs(t *testing.T) {
	// version(4) + input count 0 (varint 0x00) + output count 0xfe + 0xFFFFFFFE.
	buf := []byte{0x01, 0x00, 0x00, 0x00, 0x00, 0xfe, 0xfe, 0xff, 0xff, 0xff}
	if _, err := Deserialize(buf); err == nil {
		t.Fatal("expected error for excessive output count")
	}
}
