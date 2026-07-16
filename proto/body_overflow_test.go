package proto

import (
	"encoding/binary"
	"testing"
)

// TestUnmarshalUtxoArrayOverflow confirms a crafted UTXO count (0xFFFFFFFF) is
// rejected before any slice allocation, even when the body has no following
// UTXO bytes. This guards the OOM the old unbounded make([]UtxoEntry, n) allowed.
func TestUnmarshalUtxoArrayOverflow(t *testing.T) {
	b := &OrderBody{
		ID:             [32]byte{1},
		From:           [20]byte{2},
		FromCurrency:   "BTC",
		FromAmount:     1,
		To:             [20]byte{3},
		ToCurrency:     "LTC",
		ToAmount:       2,
		Created:        9,
		BlockHash:      [32]byte{4},
		PartialAllowed: false,
		MinFromAmount:  1,
	}
	body := b.Marshal()
	// The marshalled body ends with the utxo count (Uint32). Replace that
	// trailing zero count with a hostile 0xFFFFFFFF so the decoder reads it
	// directly (appending after a valid zero-count body would just be ignored
	// as trailing bytes).
	count := make([]byte, 4)
	binary.LittleEndian.PutUint32(count, 0xFFFFFFFF)
	mal := append(body[:len(body)-4], count...)
	if _, err := DecodeBody(XbcTransaction, mal); err == nil {
		t.Fatal("expected error for utxo count exceeding body length")
	}
}
