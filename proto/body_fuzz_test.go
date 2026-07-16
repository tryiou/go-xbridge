package proto

import "testing"

// FuzzDecodeBody feeds random command codes + random body bytes into the body
// decoder. Every XBridge command body parser is reachable from untrusted input
// and must return an error (not panic) on garbage. Out-of-range commands fall
// through the switch to the error path.
func FuzzDecodeBody(f *testing.F) {
	f.Add(int(XbcTransaction), []byte{})
	f.Add(int(XbcTransactionHoldApply), []byte{0x01, 0x02})
	f.Add(int(XbcTransactionCreatedA), make([]byte, 200))
	f.Fuzz(func(t *testing.T, cmd int, data []byte) {
		_, _ = DecodeBody(XBridgeCommand(cmd), data)
	})
}
