package api

import (
	"testing"

	"go-xbridge/wallet"
)

// Finalized completed orders must never be polled again: a deep-confirmed,
// sessionless, refund-reconciled broadcast is dropped from the watch table
// (history remains the audit trail) and never re-polled. Restore of a settled
// archive section from an older file is covered in settle_test.go
// (TestRestoreDropsSettledArchive).

func TestFinalizedDeepEntryDroppedNeverPolled(t *testing.T) {
	bc := &stubConn{ticker: "BLOCK", verboseTx: map[string]wallet.VerboseTx{
		"tx1": {TxID: "tx1", Confirmations: 800, Outputs: map[uint32]wallet.VerboseTxOut{}},
	}}
	n := newTestNode(t, nil, map[string]wallet.Connector{"BLOCK": bc})
	n.recordBroadcast("order1", broadcastClaim, "BLOCK", "tx1", "hex")
	n.pollBroadcastConfirmations()
	if _, ok := n.trackedSnapshot()["tx1"]; ok {
		t.Fatal("deep sessionless reconciled entry still watched, want dropped")
	}
	before := bc.verboseCalls
	n.pollBroadcastConfirmations()
	if bc.verboseCalls != before {
		t.Fatalf("dropped entry polled (%d -> %d), want zero polling", before, bc.verboseCalls)
	}
	active, settled := snapshotBroadcasts(n)
	if len(active) != 0 || len(settled) != 0 {
		t.Fatalf("snapshot = %d active %d settled, want 0/0", len(active), len(settled))
	}
}

func TestFinalizedLiveAndRefundEntriesKept(t *testing.T) {
	bc := &stubConn{ticker: "BLOCK", verboseTx: map[string]wallet.VerboseTx{
		"live-tx":   {TxID: "live-tx", Confirmations: 800, Outputs: map[uint32]wallet.VerboseTxOut{}},
		"refund-tx": {TxID: "refund-tx", Confirmations: 800, Outputs: map[uint32]wallet.VerboseTxOut{}},
	}}
	n := newTestNode(t, nil, map[string]wallet.Connector{"BLOCK": bc})
	n.recordBroadcast("order-live", broadcastClaim, "BLOCK", "live-tx", "hex")
	n.sessions["order-live"] = &SwapSession{}
	var oid [32]byte
	oid[0] = 0x77
	idHex := hexEncode(oid[:])
	n.store.Add(&Order{ID: oid, Mine: true, FromCurrency: "BLOCK", ToCurrency: "BLOCK",
		DepositSent: true, RefundTx: "abcd", Status: "open"})
	n.recordBroadcast(idHex, broadcastRefund, "BLOCK", "refund-tx", "hex")
	n.pollBroadcastConfirmations()
	got := n.trackedSnapshot()
	if _, ok := got["live-tx"]; !ok {
		t.Fatal("live-session deep entry dropped, want kept")
	}
	if _, ok := got["refund-tx"]; !ok {
		t.Fatal("unreconciled refund deep entry dropped, want kept")
	}
}
