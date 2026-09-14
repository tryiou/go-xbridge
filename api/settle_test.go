package api

import (
	"testing"

	"go-xbridge/config"
	"go-xbridge/wallet"
)

// Settle lifecycle (broadcast watch archive tier) tests: a deep-confirmed,
// sessionless, refund-reconciled tracked broadcast settles out of the per-tick
// poll into the archive (hourly recheck); any regression re-activates it.

// settleTestNode builds an inline node with a stub BLOCK connector serving
// verboseTx, for the settle tests. confs feeds the node config so coin-aware
// settle depths are exercisable; nil means the default (depth
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

// TestSettleArchivesDeepSessionless proves the core settle: a depth >= settle
// entry with no live session settles on the poll's apply, stops the per-tick
// poll, and moves to the persisted settled section.
func TestSettleArchivesDeepSessionless(t *testing.T) {
	n, bc := settleTestNode(t, nil, verboseOut(6))
	n.recordBroadcast("order1", broadcastClaim, "BLOCK", "tx1", "hex")
	n.pollBroadcastConfirmations()
	got := n.trackedSnapshot()
	tb, ok := got["tx1"]
	if !ok {
		t.Fatal("entry lost")
	}
	if !tb.Settled || tb.Confs != 6 {
		t.Fatalf("settled = %v confs = %d, want settled at 6", tb.Settled, tb.Confs)
	}
	active, settled := snapshotBroadcasts(n)
	if len(active) != 0 || len(settled) != 1 {
		t.Fatalf("snapshot split = %d active %d settled, want 0/1", len(active), len(settled))
	}
	// Next tick within the recheck window: the archived entry is not polled.
	n.settledPollAt = uint64(NowMicro())
	before := bc.verboseCalls
	n.pollBroadcastConfirmations()
	if bc.verboseCalls != before {
		t.Fatalf("settled entry polled within recheck window (%d -> %d)", before, bc.verboseCalls)
	}
}

// TestSettleSkipsShallow proves depth gating: below settleDepth the entry
// stays active (per-tick watch), never archived.
func TestSettleSkipsShallow(t *testing.T) {
	n, _ := settleTestNode(t, nil, verboseOut(5)) // default depth 6
	n.recordBroadcast("order1", broadcastClaim, "BLOCK", "tx1", "hex")
	n.pollBroadcastConfirmations()
	got := n.trackedSnapshot()
	if got["tx1"].Settled {
		t.Fatal("shallow entry settled")
	}
	active, settled := snapshotBroadcasts(n)
	if len(active) != 1 || len(settled) != 0 {
		t.Fatalf("snapshot split = %d active %d settled, want 1/0", len(active), len(settled))
	}
}

// TestSettleCoinAwareDepth proves settleDepth honors the coin's required
// Confirmations: a 6-conf entry on a 40-conf coin stays active.
func TestSettleCoinAwareDepth(t *testing.T) {
	confs := map[string]*config.CoinConf{
		"BLOCK": {Ticker: "BLOCK", Confirmations: 40},
	}
	n, bc := settleTestNode(t, confs, verboseOut(6))
	n.recordBroadcast("order1", broadcastClaim, "BLOCK", "tx1", "hex")
	n.pollBroadcastConfirmations()
	if n.trackedSnapshot()["tx1"].Settled {
		t.Fatal("entry settled below the coin's required depth")
	}
	// Depth reached: settle.
	bc.verboseTx = verboseOut(40)
	n.pollBroadcastConfirmations()
	if !n.trackedSnapshot()["tx1"].Settled {
		t.Fatal("entry not settled at the coin's required depth")
	}
}

// TestSettleSkipsLiveSession proves a live session keeps its entry active
// regardless of depth, and that pruning the session releases it to settle.
func TestSettleSkipsLiveSession(t *testing.T) {
	n, _ := settleTestNode(t, nil, verboseOut(6))
	n.recordBroadcast("order1", broadcastClaim, "BLOCK", "tx1", "hex")
	n.sessions["order1"] = &SwapSession{}
	n.pollBroadcastConfirmations()
	if n.trackedSnapshot()["tx1"].Settled {
		t.Fatal("live-session entry settled")
	}
	delete(n.sessions, "order1")
	n.pollBroadcastConfirmations()
	if !n.trackedSnapshot()["tx1"].Settled {
		t.Fatal("sessionless entry not settled after session prune")
	}
}

// TestSettleGatesUnreconciledRefund proves the refund gate: a deep refund
// entry for a live non-terminal order stays active (scanStoredRefunds would
// re-post the refund if the entry vanished) until the order reconciles to
// "rolled back" or a terminal status.
func TestSettleGatesUnreconciledRefund(t *testing.T) {
	n, _ := settleTestNode(t, nil, verboseOut(6))
	var oid [32]byte
	oid[0] = 0x77
	idHex := hexEncode(oid[:])
	n.store.Add(&Order{ID: oid, Mine: true, FromCurrency: "BLOCK", ToCurrency: "BLOCK",
		DepositSent: true, RefundTx: "abcd", Status: "open"})
	n.recordBroadcast(idHex, broadcastRefund, "BLOCK", "tx1", "hex")
	n.pollBroadcastConfirmations()
	if n.trackedSnapshot()["tx1"].Settled {
		t.Fatal("unreconciled refund entry settled")
	}
	n.store.Update(idHex, func(o *Order) { o.Status = "rolled back" })
	n.pollBroadcastConfirmations()
	if !n.trackedSnapshot()["tx1"].Settled {
		t.Fatal("reconciled refund entry not settled")
	}
}

// TestSettleDemoteOnRegression proves the hourly recheck safety net: a
// recheck-observed regression below settle depth re-activates the entry, and
// the rebroadcast sweep immediately regains ownership of a 0-conf regression.
func TestSettleDemoteOnRegression(t *testing.T) {
	n, bc := settleTestNode(t, nil, verboseOut(6))
	n.recordBroadcast("order1", broadcastClaim, "BLOCK", "tx1", "hex")
	n.pollBroadcastConfirmations() // settles at depth 6
	if !n.trackedSnapshot()["tx1"].Settled {
		t.Fatal("entry did not settle")
	}
	// The chain lost the tx (SPV reset / deep reorg): the recheck observes 0.
	bc.verboseTx = verboseOut(0)
	n.settledPollAt = uint64(NowMicro()) - settleRecheckInterval - 1
	n.pollBroadcastConfirmations()
	got := n.trackedSnapshot()["tx1"]
	if got.Settled || got.Confs != 0 {
		t.Fatalf("demote = settled %v confs %d, want reactivated at 0", got.Settled, got.Confs)
	}
	// Reactivated: the rebroadcast sweep owns the 0-conf entry again.
	old := uint64(NowMicro()) - uint64(n.rebroadcastAfterMicro("BLOCK")+1)
	n.tracked["tx1"].FirstSeenMicro = old
	n.rebroadcastUnconfirmed()
	if n.tracked["tx1"].Attempts != 1 {
		t.Fatalf("rebroadcast attempts = %d after demote, want 1 (sweep regained ownership)",
			n.tracked["tx1"].Attempts)
	}
}

// TestSettledPersistRoundTrip proves the persisted split: active and settled
// sections marshal, checksum, and parse independently, and a file written by
// a pre-settled binary (no settled section — identical bytes to
// marshalSwapFile with nil) still loads with an empty archive.
func TestSettledPersistRoundTrip(t *testing.T) {
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
	// Pre-settled file (old two-section envelope): loads, archive empty.
	oldData, err := marshalSwapFile(swaps, active, nil)
	if err != nil {
		t.Fatalf("marshal old shape: %v", err)
	}
	_, gotActive2, gotSettled2, err := parseSwapFile(oldData, "test")
	if err != nil {
		t.Fatalf("old-shape load: %v", err)
	}
	if len(gotActive2) != 1 || len(gotSettled2) != 0 {
		t.Fatalf("old-shape = %d active %d settled, want 1/0", len(gotActive2), len(gotSettled2))
	}
}

// TestRestoreSettledTracked proves the archive section restores directly into
// the settled tier, with the same skip-invalid/dedup rules as the active
// section and a sequence that keeps new records sortable after it.
func TestRestoreSettledTracked(t *testing.T) {
	n, _ := settleTestNode(t, nil, nil)
	n.restoreSettledTracked([]persistedBroadcast{
		{OrderID: "o1", Kind: broadcastDeposit, Coin: "BLOCK", TxID: "tx1", Hex: "aa", Seq: 5, Confs: 700},
		{OrderID: "o2", Kind: broadcastClaim, Coin: "BLOCK", TxID: "", Hex: "bb"},
		{OrderID: "o1", Kind: broadcastDeposit, Coin: "BLOCK", TxID: "tx1", Hex: "aa", Seq: 5, Confs: 700},
	})
	got := n.trackedSnapshot()
	if len(got) != 1 || !got["tx1"].Settled || got["tx1"].Confs != 700 {
		t.Fatalf("restored = %+v, want one settled entry at confs 700", got)
	}
	n.recordBroadcast("o9", broadcastClaim, "BLOCK", "tx9", "hex")
	if got := n.trackedSnapshot(); got["tx9"].Seq <= 5 {
		t.Fatalf("new seq = %d, want > 5 (after restored max)", got["tx9"].Seq)
	}
}

// TestPruneSettledArchiveCap proves the archive bound: settled entries beyond
// maxSettledWatch drop oldest (lowest Seq) first, and the active-cap rule
// never touches settled entries (their bound is separate).
func TestPruneSettledArchiveCap(t *testing.T) {
	n, _ := settleTestNode(t, nil, nil)
	const total = maxSettledWatch + 3
	for i := 0; i < total; i++ {
		n.recordBroadcast("order-arch", broadcastClaim, "BLOCK", "arch-tx-"+itoa(i), "hex")
	}
	n.trackedMu.Lock()
	for _, tb := range n.tracked {
		tb.Settled = true
		tb.Confs = trackedRetainDepth
	}
	n.trackedMu.Unlock()
	n.pruneTracked()
	got := n.trackedSnapshot()
	if len(got) != maxSettledWatch {
		t.Fatalf("tracked = %d, want %d after archive prune", len(got), maxSettledWatch)
	}
	if _, ok := got["arch-tx-0"]; ok {
		t.Fatal("oldest settled entry kept")
	}
	if _, ok := got["arch-tx-"+itoa(total-1)]; !ok {
		t.Fatal("newest settled entry dropped")
	}
}

// TestPruneKeepsUnreconciledRefundEntry proves the active-cap fallback keeps
// a deep refund entry whose order is still live and unreconciled: deleting it
// would let scanStoredRefunds re-post the refund every sweep.
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

// TestRestartPreservesHistoryAndArchive is the durability check for both
// lifecycle rules: a finished order's history record and a settled broadcast
// entry survive a persist → restore round trip, and the restored archive
// entry comes back settled (no per-tick polling resumes for it).
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
	if n.updateSettled() != true {
		t.Fatal("deep sessionless claim entry did not settle")
	}
	n.persist()

	// On disk: the history record rides the swaps array as a Historical
	// record; the settled entry rides the settled section.
	swaps, active, settled, err := loadSwaps(swapStatePath(dir))
	if err != nil {
		t.Fatalf("loadSwaps: %v", err)
	}
	if len(swaps) != 1 || !swaps[0].Historical || swaps[0].Status != "finished" {
		t.Fatalf("persisted swaps = %+v, want one historical finished record", swaps)
	}
	if len(active) != 0 || len(settled) != 1 || settled[0].TxID != "claim-tx" {
		t.Fatalf("persisted watch = %d active %d settled, want 0/1 claim-tx", len(active), len(settled))
	}

	// Restore into a fresh node: history and settled archive both come back.
	n2 := newPersistNode(t, dir)
	n2.restoreLocalSwaps(dir)
	if h := n2.store.History(); len(h) != 1 || h[0].Status != "finished" || h[0].ID != idHex {
		t.Fatalf("restored history = %+v, want the finished record", h)
	}
	tb, ok := n2.tracked["claim-tx"]
	if !ok {
		t.Fatal("settled archive entry not restored")
	}
	if !tb.Settled || tb.Confs != 800 {
		t.Fatalf("restored archive entry = settled %v confs %d, want settled at 800", tb.Settled, tb.Confs)
	}
}
