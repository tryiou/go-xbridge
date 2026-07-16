package p2p

import "testing"

// FuzzUnmarshalMessage hammers the untrusted inbound parser with arbitrary
// bytes. The wire entrypoint must reject every malformed frame via an error —
// never panic, OOM, or read OOB. Seed corpus covers empty/short/oversized
// attempts.
func FuzzUnmarshalMessage(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0x00, 0x01, 0x02, 0x03})
	f.Add([]byte("xbridge-packet"))
	f.Add(make([]byte, 1400))
	f.Fuzz(func(t *testing.T, data []byte) {
		// Return value intentionally ignored; we only assert no panic.
		_, _ = UnmarshalMessage(data)
	})
}
