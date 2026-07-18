package api

import (
	"testing"

	"go-xbridge/config"
)

// TestEstimateFeeMatchesCppVsize verifies the deposit fee uses C++'s virtual
// size (192 vbytes/input, 34/output) so Go-built deposits land inside a C++
// counterparty's counterpartyFees >= fee*0.95 acceptance band.
func TestEstimateFeeMatchesCppVsize(t *testing.T) {
	// 1in/1out, FeePerByte=5 → (192*1 + 34*1)*5 = 1130.
	cc := &config.CoinConf{Ticker: "BTC", FeePerByte: 5}
	if got := estimateFee(cc, 1, 1); got != 1130 {
		t.Errorf("estimateFee(1in,1out,FeePerByte=5) = %d, want 1130", got)
	}
	// Larger tx: 3in/2out, FeePerByte=5 → (192*3 + 34*2)*5 = 3220.
	if got := estimateFee(cc, 3, 2); got != 3220 {
		t.Errorf("estimateFee(3in,2out,FeePerByte=5) = %d, want 3220", got)
	}
	// Default (no conf / zero FeePerByte) falls back to 2 sat/vB.
	want := uint64((192*2 + 34*3) * 2)
	if got := estimateFee(nil, 2, 3); got != want {
		t.Errorf("estimateFee default = %d, want %d", got, want)
	}
}

// TestNodeBlockContext confirms TakeOrder's anti-replay block context is read
// from each coin's connector (height + first 8 bytes of the tip hash), and that
// a missing connector yields zeros rather than an error.
func TestNodeBlockContext(t *testing.T) {
	node := newWalletTestCtx().Node
	h, hash := node.blockContext("BTC")
	if h != 100 {
		t.Errorf("blockContext(BTC) height = %d, want 100", h)
	}
	if hash[0] != 0xab {
		t.Errorf("blockContext(BTC) hash[0] = 0x%x, want 0xab", hash[0])
	}
	// Missing connector yields zeros (order still accepted).
	zh, zhash := node.blockContext("DOGE")
	if zh != 0 || zhash != ([8]byte{}) {
		t.Errorf("blockContext(DOGE) = (%d, %v), want (0, zero)", zh, zhash)
	}
}
