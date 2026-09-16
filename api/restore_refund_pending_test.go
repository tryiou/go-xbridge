package api

import (
	"strings"
	"testing"

	"go-xbridge/wallet"
)

// Regression test for the live #8 stranding (2026-09-15, order a4198f2d…):
// the stall watchdog canceled a swap whose deposit had already broadcast; the
// order-only persist path stores State=csIdle for the pruned session, and
// restoreSwap's history filter read that zeroed State as "nothing to recover"
// (State < csCreatedA), filing the record into history — invisible to
// scanStoredRefunds, so the pre-signed refund could never broadcast. A
// canceled record with the deposit out and a pre-signed, never-broadcast
// refund must stay in the live store (the sweep's documented intent:
// "Refund-pending cancelled swaps are kept live so the sweep can still
// recover the deposit post-restart", persist.go restoreSwap).

func TestRestoreKeepsRefundPendingCanceledLive(t *testing.T) {
	n := newTestNode(t, alignConfs(), map[string]wallet.Connector{})
	var id [32]byte
	oid := hash20("refund-pending-canceled")
	copy(id[:], oid[:])
	ps := persistedSwap{
		ID:           id,
		FromCurrency: "LTC",
		ToCurrency:   "BTC",
		Status:       "canceled",
		State:        csIdle, // order-only persist: pruned session state is zeroed
		Role:         'A',
		DepositSent:  true,
		RefundTx:     "deadbeefrefund", // order-record refund bytes; RefundHex empty
		RefundDone:   false,
		Historical:   true, // snapshotSwaps filed it historical after the prune
	}
	n.restoreSwap(ps)
	if o := n.store.Get(hexEncode(id[:])); o == nil {
		t.Fatal("refund-pending canceled record was filed into history — " +
			"the stored-refund sweep can never recover the deposit")
	}
}

// TestRestoreHistoricalFinishedStaysHistory pins the other side: a finished
// (or refund-done) record must still go to history — the override only covers
// deposits that still owe a refund.
func TestRestoreHistoricalFinishedStaysHistory(t *testing.T) {
	n := newTestNode(t, alignConfs(), map[string]wallet.Connector{})
	var id [32]byte
	oid := hash20("finished-goes-to-history")
	copy(id[:], oid[:])
	ps := persistedSwap{
		ID:           id,
		FromCurrency: "LTC",
		ToCurrency:   "BTC",
		Status:       "finished",
		State:        csFinished,
		Role:         'A',
		DepositSent:  true,
		RefundHex:    strings.Repeat("ab", 8),
		RefundDone:   false,
		Historical:   true,
	}
	n.restoreSwap(ps)
	if o := n.store.Get(hexEncode(id[:])); o != nil {
		t.Fatal("finished record must stay in history, not the live store")
	}
}
