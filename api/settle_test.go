package api

import (
	"os"
	"testing"

	"go-xbridge/config"
	"go-xbridge/wallet"
)

// Finalize lifecycle (broadcast watch) tests: a deep-confirmed, sessionless,
// refund-reconciled tracked broadcast is dropped from the watch table outright
// and never polled again. The durable trade record stays in Store.history, so
// order visibility is unchanged. There is no archive tier and no recheck.

// settleTestNode builds an inline node with a stub BLOCK connector serving
// verboseTx, for the finalize tests. confs feeds the node config so coin-aware
// finalize depths are exercisable; nil means the default (depth
// trackedRetainDepth).
func settleTestNode(t *testing.T, confs map[string]*config.CoinConf, verbose map[string]wallet.VerboseTx) (*Node, *stubConn) {
	t.Helper()
	bc := &stubConn{ticker: "BLOCK", verboseTx: verbose}
	if confs == nil {
		confs = map[string]*config.CoinConf{}
	}
	n := newTestNode(t, confs, map[string]wallet.Connector{"BLOCK": bc})
	return n, bc
}

func verboseOut(confs int) map[string]wallet.VerboseTx {
	return map[string]wallet.VerboseTx{
		"tx1": {TxID: "tx1", Confirmations: confs, Outputs: map[uint32]wallet.VerboseTxOut{}},
	}
}

// TestFinalizeArchivesDeepSessionless proves the core finalize: a depth >=
// finalize entry with no live session is dropped from the watch table on the
// poll's apply and never polled again; the snapshot carries no settled
// section.
func TestFinalizeArchivesDeepSessionless(t *testing.T) {
	n, bc := settleTestNode(t, nil, verboseOut(6))
	n.recordBroadcast("order1", broadcastClaim, "BLOCK", "tx1", "hex")
	n.pollBroadcastConfirmations()
	if _, ok := n.trackedSnapshot()["tx1"]; ok {
		t.Fatal("deep sessionless entry still watched, want dropped")
	}
	active, settled := snapshotBroadcasts(n)
	if len(active) != 0 || len(settled) != 0 {
		t.Fatalf("snapshot split = %d active %d settled, want 0/0", len(active), len(settled))
	}
	// Next tick: nothing to poll.
	before := bc.verboseCalls
	n.pollBroadcastConfirmations()
	if bc.verboseCalls != before {
		t.Fatalf("dropped entry polled (%d -> %d), want zero", before, bc.verboseCalls)
	}
}

// TestFinalizeSkipsShallow proves depth gating: below finalize depth the entry
// stays watched (per-tick), never dropped.
func TestFinalizeSkipsShallow(t *testing.T) {
	n, _ := settleTestNode(t, nil, verboseOut(5)) // default depth 6
	n.recordBroadcast("order1", broadcastClaim, "BLOCK", "tx1", "hex")
	n.pollBroadcastConfirmations()
	if _, ok := n.trackedSnapshot()["tx1"]; !ok {
		t.Fatal("shallow entry dropped")
	}
	active, settled := snapshotBroadcasts(n)
	if len(active) != 1 || len(settled) != 0 {
		t.Fatalf("snapshot split = %d active %d settled, want 1/0", len(active), len(settled))
	}
}

// TestFinalizeCoinAwareDepth proves settleDepth honors the coin's required
// Confirmations: a 6-conf entry on a 40-conf coin stays watched.
func TestFinalizeCoinAwareDepth(t *testing.T) {
	confs := map[string]*config.CoinConf{
		"BLOCK": {Ticker: "BLOCK", Confirmations: 40},
	}
	n, bc := settleTestNode(t, confs, verboseOut(6))
	n.recordBroadcast("order1", broadcastClaim, "BLOCK", "tx1", "hex")
	n.pollBroadcastConfirmations()
	if _, ok := n.trackedSnapshot()["tx1"]; !ok {
		t.Fatal("entry dropped below the coin's required depth")
	}
	// Depth reached: finalize (drop).
	bc.verboseTx = verboseOut(40)
	n.pollBroadcastConfirmations()
	if _, ok := n.trackedSnapshot()["tx1"]; ok {
		t.Fatal("entry not dropped at the coin's required depth")
	}
}

// TestFinalizeSkipsLiveSession proves a live session keeps its entry watched
// regardless of depth, and that pruning the session releases it to finalize.
func TestFinalizeSkipsLiveSession(t *testing.T) {
	n, _ := settleTestNode(t, nil, verboseOut(6))
	n.recordBroadcast("order1", broadcastClaim, "BLOCK", "tx1", "hex")
	n.sessions["order1"] = &SwapSession{}
	n.pollBroadcastConfirmations()
	if _, ok := n.trackedSnapshot()["tx1"]; !ok {
		t.Fatal("live-session entry dropped")
	}
	delete(n.sessions, "order1")
	n.pollBroadcastConfirmations()
	if _, ok := n.trackedSnapshot()["tx1"]; ok {
		t.Fatal("sessionless entry not dropped after session prune")
	}
}

// TestFinalizeGatesUnreconciledRefund proves the refund gate: a deep refund
// entry for a live non-terminal order stays watched (scanStoredRefunds would
// re-post the refund if the entry vanished) until the order reconciles to
// "rolled back" or a terminal status.
func TestFinalizeGatesUnreconciledRefund(t *testing.T) {
	n, _ := settleTestNode(t, nil, verboseOut(6))
	var oid [32]byte
	oid[0] = 0x77
	idHex := hexEncode(oid[:])
	n.store.Add(&Order{ID: oid, Mine: true, FromCurrency: "BLOCK", ToCurrency: "BLOCK",
		DepositSent: true, RefundTx: "abcd", Status: "open"})
	n.recordBroadcast(idHex, broadcastRefund, "BLOCK", "tx1", "hex")
	n.pollBroadcastConfirmations()
	if _, ok := n.trackedSnapshot()["tx1"]; !ok {
		t.Fatal("unreconciled refund entry dropped")
	}
	n.store.Update(idHex, func(o *Order) { o.Status = "rolled back" })
	n.pollBroadcastConfirmations()
	if _, ok := n.trackedSnapshot()["tx1"]; ok {
		t.Fatal("reconciled refund entry not dropped")
	}
}

// TestFinalizeNeverRepolls proves there is no recheck: a dropped entry stays
// dropped across ticks even if the chain would report a regression — the
// entry is gone, so no wallet RPC fires for it.
func TestFinalizeNeverRepolls(t *testing.T) {
	n, bc := settleTestNode(t, nil, verboseOut(6))
	n.recordBroadcast("order1", broadcastClaim, "BLOCK", "tx1", "hex")
	n.pollBroadcastConfirmations() // drops at depth 6
	if _, ok := n.trackedSnapshot()["tx1"]; ok {
		t.Fatal("entry did not finalize")
	}
	bc.verboseTx = verboseOut(0)
	before := bc.verboseCalls
	n.pollBroadcastConfirmations()
	n.pollBroadcastConfirmations()
	if bc.verboseCalls != before {
		t.Fatalf("finalized entry repolled (%d -> %d), want zero", before, bc.verboseCalls)
	}
	if _, ok := n.trackedSnapshot()["tx1"]; ok {
		t.Fatal("finalized entry resurrected")
	}
}

// TestFinalizedPersistRoundTrip proves the persisted split: the active section
// marshals, checksums, and parses; a file written by a pre-finalize binary
// (with a settled section) still loads, and the settled section is dropped on
// restore, never re-polled.
func TestFinalizedPersistRoundTrip(t *testing.T) {
	swaps := []persistedSwap{{ID: [32]byte{0x1}}}
	active := []persistedBroadcast{{
		OrderID: "oa", Kind: broadcastDeposit, Coin: "BLOCK", TxID: "txa", Hex: "aa", Seq: 1, Confs: 2,
	}}
	settled := []persistedBroadcast{{
		OrderID: "os", Kind: broadcastClaim, Coin: "LTC", TxID: "txs", Hex: "ss", Seq: 2, Confs: 800,
	}}
	data, err := marshalSwapFile(swaps, active, settled)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	gotSwaps, gotActive, gotSettled, err := parseSwapFile(data, "test")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(gotSwaps) != 1 || len(gotActive) != 1 || len(gotSettled) != 1 {
		t.Fatalf("round trip = %d swaps %d active %d settled, want 1/1/1",
			len(gotSwaps), len(gotActive), len(gotSettled))
	}
	if gotSettled[0].TxID != "txs" || gotSettled[0].Confs != 800 {
		t.Fatalf("settled mismatch: %+v", gotSettled[0])
	}
	// New-shape file (no settled section): loads, archive empty.
	newData, err := marshalSwapFile(swaps, active, nil)
	if err != nil {
		t.Fatalf("marshal new shape: %v", err)
	}
	_, gotActive2, gotSettled2, err := parseSwapFile(newData, "test")
	if err != nil {
		t.Fatalf("new-shape load: %v", err)
	}
	if len(gotActive2) != 1 || len(gotSettled2) != 0 {
		t.Fatalf("new-shape = %d active %d settled, want 1/0", len(gotActive2), len(gotSettled2))
	}
}

// TestRestoreDropsSettledArchive proves migration: a settled section in an
// older file is dropped on restore (history keeps the audit trail), while the
// active section restores with the same skip-invalid/dedup rules.
func TestRestoreDropsSettledArchive(t *testing.T) {
	dir := t.TempDir()
	n := newPersistNode(t, dir)
	var oid [32]byte
	oid[0] = 0x61
	idHex := hexEncode(oid[:])
	n.store.Add(&Order{ID: oid, Mine: true, FromCurrency: "BLOCK", ToCurrency: "LTC",
		Status: "created", DepositSent: true})
	n.store.MoveToHistory(idHex, "finished", 0, uint64(NowMicro()))
	n.recordBroadcast(idHex, broadcastClaim, "LTC", "claim-tx", "hex")
	n.tracked["claim-tx"].Confs = 800
	if n.finalizeWatchEntries() != true {
		t.Fatal("deep sessionless claim entry did not finalize")
	}
	n.persist()

	swaps, active, settled, err := loadSwaps(swapStatePath(dir))
	if err != nil {
		t.Fatalf("loadSwaps: %v", err)
	}
	if len(swaps) != 1 || !swaps[0].Historical || swaps[0].Status != "finished" {
		t.Fatalf("persisted swaps = %+v, want one historical finished record", swaps)
	}
	if len(active) != 0 || len(settled) != 0 {
		t.Fatalf("persisted watch = %d active %d settled, want 0/0", len(active), len(settled))
	}

	// Old file with a settled section: the finalized entry is dropped, a
	// below-depth entry rejoins the watch (e.g. Confirmations was raised
	// since the file was written), history is kept either way.
	oldData, err := marshalSwapFile(swaps, nil, []persistedBroadcast{{
		OrderID: idHex, Kind: broadcastClaim, Coin: "LTC", TxID: "claim-tx", Hex: "hex", Seq: 1, Confs: 800,
	}, {
		OrderID: idHex, Kind: broadcastClaim, Coin: "LTC", TxID: "shallow-tx", Hex: "hex", Seq: 2, Confs: 1,
	}})
	if err != nil {
		t.Fatalf("marshal old shape: %v", err)
	}
	if err := os.WriteFile(swapStatePath(dir), oldData, 0o600); err != nil {
		t.Fatalf("write old shape: %v", err)
	}
	n2 := newPersistNode(t, dir)
	n2.restoreLocalSwaps(dir)
	if h := n2.store.History(); len(h) != 1 || h[0].Status != "finished" || h[0].ID != idHex {
		t.Fatalf("restored history = %+v, want the finished record", h)
	}
	if _, ok := n2.tracked["claim-tx"]; ok {
		t.Fatal("settled archive entry restored, want dropped")
	}
	if _, ok := n2.tracked["shallow-tx"]; !ok {
		t.Fatal("below-depth archive entry not restored to watch, want kept")
	}
}

// TestPruneKeepsUnreconciledRefundEntry proves the active-cap fallback keeps
// a deep refund entry whose order is still live and unreconciled: deleting it
// would let scanStoredRefunds re-post the refund every sweep (tracked =>
// never re-posted). Covers the pruneTracked cap path, distinct from
// TestFinalizeGatesUnreconciledRefund (finalize path, coin-aware depth).
func TestPruneKeepsUnreconciledRefundEntry(t *testing.T) {
	n, _ := settleTestNode(t, nil, nil)
	var oid [32]byte
	oid[0] = 0x77
	idHex := hexEncode(oid[:])
	n.store.Add(&Order{ID: oid, Mine: true, FromCurrency: "BLOCK", ToCurrency: "BLOCK",
		DepositSent: true, RefundTx: "abcd", Status: "open"})
	// Fill the table past the active cap with droppable deep sessionless
	// entries, then the protected refund entry.
	for i := 0; i < trackedCap+2; i++ {
		n.recordBroadcast("order-fill", broadcastClaim, "BLOCK", "fill-tx-"+itoa(i), "hex")
	}
	n.trackedMu.Lock()
	for _, tb := range n.tracked {
		tb.Confs = trackedRetainDepth
	}
	n.trackedMu.Unlock()
	n.recordBroadcast(idHex, broadcastRefund, "BLOCK", "refund-tx", "hex")
	n.tracked["refund-tx"].Confs = trackedRetainDepth
	n.pruneTracked()
	if _, ok := n.trackedSnapshot()["refund-tx"]; !ok {
		t.Fatal("unreconciled refund entry dropped by cap prune (re-post hazard)")
	}
}

// TestRestartPreservesHistoryAndArchive is the durability check: a finished
// order's history record survives a persist → restore round trip, and no watch
// entry comes back for it (zero polling resumes).
func TestRestartPreservesHistoryAndArchive(t *testing.T) {
	dir := t.TempDir()
	n := newPersistNode(t, dir)

	var oid [32]byte
	oid[0] = 0x61
	idHex := hexEncode(oid[:])
	n.store.Add(&Order{ID: oid, Mine: true, FromCurrency: "BLOCK", ToCurrency: "LTC",
		Status: "created", DepositSent: true})
	n.store.MoveToHistory(idHex, "finished", 0, uint64(NowMicro()))
	n.recordBroadcast(idHex, broadcastClaim, "LTC", "claim-tx", "hex")
	n.tracked["claim-tx"].Confs = 800
	if n.finalizeWatchEntries() != true {
		t.Fatal("deep sessionless claim entry did not finalize")
	}
	n.persist()

	// On disk: the history record rides the swaps array as a Historical
	// record; the watch table is empty.
	swaps, active, settled, err := loadSwaps(swapStatePath(dir))
	if err != nil {
		t.Fatalf("loadSwaps: %v", err)
	}
	if len(swaps) != 1 || !swaps[0].Historical || swaps[0].Status != "finished" {
		t.Fatalf("persisted swaps = %+v, want one historical finished record", swaps)
	}
	if len(active) != 0 || len(settled) != 0 {
		t.Fatalf("persisted watch = %d active %d settled, want 0/0", len(active), len(settled))
	}

	// Restore into a fresh node: history comes back, no watch entry.
	n2 := newPersistNode(t, dir)
	n2.restoreLocalSwaps(dir)
	if h := n2.store.History(); len(h) != 1 || h[0].Status != "finished" || h[0].ID != idHex {
		t.Fatalf("restored history = %+v, want the finished record", h)
	}
	if len(n2.trackedSnapshot()) != 0 {
		t.Fatalf("restored watch = %+v, want empty", n2.trackedSnapshot())
	}
}
