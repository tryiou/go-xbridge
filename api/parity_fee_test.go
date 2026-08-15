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
	// The deposit fee uses nOut=3 (minTxFee1(nIn,3), C++
	// xbridgesession.cpp:1994/:2526). 2in/3out, FeePerByte=5 → (384+102)*5 = 2430.
	if got := estimateFee(cc, 2, 3); got != 2430 {
		t.Errorf("estimateFee(2in,3out,FeePerByte=5) = %d, want 2430", got)
	}
	// Default (no conf / zero FeePerByte) falls back to 2 sat/vB.
	want := uint64((192*2 + 34*3) * 2)
	if got := estimateFee(nil, 2, 3); got != want {
		t.Errorf("estimateFee default = %d, want %d", got, want)
	}
}

// TestNodeBlockContext confirms TakeOrder's anti-replay block context is read
// from each coin's connector (height + first 8 ASCII chars of the display-hex
// tip hash, C++ xbridgeapp.cpp:2420-2424), and that a missing connector is an
// error (a mandatory context, unlike the old zeros-and-continue).
func TestNodeBlockContext(t *testing.T) {
	node := newWalletTestCtx().Node
	h, hash, err := node.blockContext("BTC")
	if err != nil {
		t.Fatalf("blockContext(BTC): %v", err)
	}
	if h != 100 {
		t.Errorf("blockContext(BTC) height = %d, want 100", h)
	}
	// stubConn's GetBlockHash is [32]byte{0xab}; display order reverses it so
	// the hex string ends in "ab" and the first 8 chars are 8x '0' (0x30).
	if got, want := hash, [8]byte{0x30, 0x30, 0x30, 0x30, 0x30, 0x30, 0x30, 0x30}; got != want {
		t.Errorf("blockContext(BTC) hash = %q (%x), want %q", hash, hash[:], want)
	}
	// A missing connector is an error, not a silent zero context.
	if _, _, err := node.blockContext("DOGE"); err == nil {
		t.Errorf("blockContext(DOGE) = nil error, want error")
	}
}
