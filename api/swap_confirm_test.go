package api

// Confirm/finish C++-parity tests. The contract (validated against
// ./blocknet/src/xbridge/): a trader's own claim broadcast IS the finish —
// `state = trFinished` immediately after the redeem succeeds
// (xbridgesession.cpp:3002 maker ConfirmA, :3185 taker ConfirmB); the hub's
// Finished packet only re-sets the terminal state and moves the descriptor to
// history (:3805-3824). Wrong-role packets are INFO-level silent drops
// (LogOrderMsg + return true: :1937-1941, :2434-2438, :2901-2905) — never
// errors, never cancels — and processTransactionConfirmB has NO role gate at
// all (:3104-3199). The own-deposit mempool watch (xbridgeapp.cpp:3340-3457)
// finishes a taker independently of any hub packet.

import (
	"encoding/hex"
	"strings"

	"go-xbridge/coins"
	"testing"

	"go-xbridge/proto"
	"go-xbridge/wallet"
)

// TestMakerFinishesOnClaimBroadcast (T1, maker): the ConfirmA redeem
// broadcast is the finish — csFinished + order status finished with NO
// Finished packet involved; the ConfirmedA hub receipt still went out.
func TestMakerFinishesOnClaimBroadcast(t *testing.T) {
	_, _, makerSession, _, _, orderID, _, _, _ := blindTakerFixture(t)
	if makerSession.state != csFinished {
		t.Fatalf("maker state = %s, want finished after its own redeem broadcast", makerSession.state.String())
	}
	o := makerSession.n.store.Get(hexEncode(orderID[:]))
	if o == nil || o.Status != "finished" {
		t.Fatalf("live order = %+v, want status finished", o)
	}
}

// TestTakerFinishesOnClaimBroadcast (T2, taker): same parity for ConfirmB —
// the taker's successful redeem broadcast moves the session terminal without
// any Finished packet.
func TestTakerFinishesOnClaimBroadcast(t *testing.T) {
	_, takerNode, _, takerSession, hub, orderID, makerPayTxID, tkLtcConn, mkLtcConn := blindTakerFixture(t)
	// Backend catches up so the claim build succeeds (payTx visible).
	hx, ok := mkLtcConn.rawTx[makerPayTxID]
	if !ok {
		t.Fatal("maker payTx missing from maker connector")
	}
	tkLtcConn.rawTx[makerPayTxID] = hx
	if _, _, err := takerSession.OnConfirmB(&proto.ConfirmBBody{HubAddress: hub, ID: orderID, APayTxID: makerPayTxID}); err != nil {
		t.Fatalf("OnConfirmB: %v", err)
	}
	if takerSession.state != csFinished {
		t.Fatalf("taker state = %s, want finished after its own redeem broadcast", takerSession.state.String())
	}
	o := takerNode.store.Get(hexEncode(orderID[:]))
	if o == nil || o.Status != "finished" {
		t.Fatalf("live order = %+v, want status finished", o)
	}
}

// TestOnFinishedAfterLocalFinishIsSingleHistoryEntry (T3): the hub's late
// Finished packet after a local finish is the C++ idempotent housekeeping
// (:3820-3824) — exactly one history entry, live book emptied.
func TestOnFinishedAfterLocalFinishIsSingleHistoryEntry(t *testing.T) {
	makerNode, _, makerSession, _, _, orderID, _, _, _ := blindTakerFixture(t)
	if makerSession.state != csFinished {
		t.Fatalf("precondition: maker state = %s, want finished", makerSession.state.String())
	}
	if _, _, err := makerSession.OnFinished(&proto.FinishedBody{ID: orderID}); err != nil {
		t.Fatalf("OnFinished: %v", err)
	}
	if got := makerNode.store.Get(hexEncode(orderID[:])); got != nil {
		t.Fatalf("order still live after OnFinished: %+v", got)
	}
	if got := makerNode.store.HistoryOrder(hexEncode(orderID[:])); got == nil || got.Status != "finished" {
		t.Fatalf("history = %+v, want one finished entry", got)
	}
}

// TestWrongRoleCreateBOnMakerIsSilentDrop (T4): C++ :2434-2438 — a wrong-role
// CreateB is LogOrderMsg INFO + return true: no error, no cancel, no state
// change. The pre-fix Go path returned fmt.Errorf (ERROR log per retransmit).
func TestWrongRoleCreateBOnMakerIsSilentDrop(t *testing.T) {
	n, s, _ := setupSwapPair(t) // maker session at csMaker
	before := s.state
	orderID := hexEncode(s.id[:])
	_, _, err := s.OnCreateB(&proto.CreateBBody{
		HubAddress: s.hub, ID: s.id,
		APubKey: [33]byte{1}, ADepositTxID: strings.Repeat("cd", 32),
		HashedSecret: [20]byte{2}, ALockTime: 900,
	})
	if err != nil {
		t.Fatalf("wrong-role CreateB must be a silent drop, got error: %v", err)
	}
	if s.state != before {
		t.Fatalf("state changed on wrong-role CreateB: %s -> %s", before, s.state)
	}
	if o := n.store.Get(orderID); o != nil && o.Status == "canceled" {
		t.Fatal("wrong-role CreateB canceled the order (C++ never cancels here)")
	}
}

// TestConfirmAOnTakerIsSilentDropAfterStateGate (T5): C++ :2893-2905 — the
// state drop precedes the role gate and both are silent drops. A taker
// receiving a ConfirmA retransmit is dropped with no error.
func TestConfirmAOnTakerIsSilentDropAfterStateGate(t *testing.T) {
	_, _, _, takerSession, _, _, _, _, _ := blindTakerFixture(t)
	before := takerSession.state
	_, _, err := takerSession.OnConfirmA(&proto.ConfirmABody{
		HubAddress: takerSession.hub, ID: takerSession.id,
		BDepositTxID: takerSession.theirDepositTxID, BLockTime: takerSession.theirLockTime,
	})
	if err != nil {
		t.Fatalf("ConfirmA on taker must be a silent drop, got error: %v", err)
	}
	if takerSession.state != before {
		t.Fatalf("state changed on wrong-role ConfirmA: %s -> %s", before, takerSession.state)
	}
}

// TestConfirmBOnFinishedMakerDropsAtStateGate (T6): C++ :3152 — the state
// drop is the only post-signature guard for ConfirmB (no role gate); a
// post-redeem retransmit on a finished maker is dropped before any claim
// work (no rebuild, no second broadcast).
func TestConfirmBOnFinishedMakerDropsAtStateGate(t *testing.T) {
	_, _, makerSession, _, _, _, _, _, _ := blindTakerFixture(t)
	if makerSession.state != csFinished {
		t.Fatalf("precondition: maker state = %s, want finished", makerSession.state.String())
	}
	before := makerSession.state
	claim := makerSession.claimTxID
	_, _, err := makerSession.OnConfirmB(&proto.ConfirmBBody{
		HubAddress: makerSession.hub, ID: makerSession.id, APayTxID: strings.Repeat("ef", 32),
	})
	if err != nil {
		t.Fatalf("ConfirmB on finished maker must be a silent drop, got error: %v", err)
	}
	if makerSession.state != before || makerSession.claimTxID != claim {
		t.Fatalf("finished session mutated: state %s -> %s, claimTxID %q -> %q", before, makerSession.state, claim, makerSession.claimTxID)
	}
}

// TestSetOrderStatusBumpsUpdated (T7): a descriptor state change refreshes
// its timestamp (C++ descriptor state changes update the record), so
// dxGetOrder's updated_at tracks session progress.
func TestSetOrderStatusBumpsUpdated(t *testing.T) {
	n, s, _ := setupSwapPair(t)
	orderID := hexEncode(s.id[:])
	n.store.Update(orderID, func(o *Order) { o.Updated = 5 })
	s.setOrderStatus("hold")
	o := n.store.Get(orderID)
	if o == nil {
		t.Fatal("order vanished")
	}
	if o.Status != "hold" {
		t.Fatalf("status = %q, want hold", o.Status)
	}
	if o.Updated <= 5 {
		t.Fatalf("Updated = %d, want > 5 after status change", o.Updated)
	}
}

// TestOwnWatchRecoversSecretAndFinishes (T9): the C++ own-deposit spend watch
// (xbridgeapp.cpp:3378-3431) — the taker, stranded pre-claim with no ConfirmB,
// finds the spender of its own deposit in the mempool, recovers the secret,
// and finishes through the identical ConfirmB path.
func TestOwnWatchRecoversSecretAndFinishes(t *testing.T) {
	_, takerNode, _, takerSession, _, orderID, makerPayTxID, tkLtcConn, mkLtcConn := blindTakerFixture(t)
	if takerSession.state != csCreatedB || takerSession.secret != [33]byte{} {
		t.Fatalf("precondition: state = %s, secret set = %v", takerSession.state.String(), takerSession.secret != [33]byte{})
	}
	// Backend catches up: the maker's payTx becomes fetchable, and it is the
	// mempool entry spending our deposit.
	hx, ok := mkLtcConn.rawTx[makerPayTxID]
	if !ok {
		t.Fatal("maker payTx missing from maker connector")
	}
	tkLtcConn.rawTx[makerPayTxID] = hx
	tkLtcConn.mempoolTxids = []string{makerPayTxID}
	takerNode.watchOwnDepositSpends()
	if takerSession.state != csFinished {
		t.Fatalf("state = %s, want finished after own-deposit watch recovery", takerSession.state.String())
	}
	if takerSession.claimTxID == "" {
		t.Fatal("claim never broadcast by the watch recovery")
	}
	o := takerNode.store.Get(hexEncode(orderID[:]))
	if o == nil || o.Status != "finished" {
		t.Fatalf("live order = %+v, want status finished", o)
	}
}

// TestOwnWatchIgnoresNonSpenders (T10): a mempool full of unrelated
// transactions must not progress the session (vin-bound extraction, C++
// isUTXOSpentInTx :3419); the scanned txids feed the incremental seen-set.
func TestOwnWatchIgnoresNonSpenders(t *testing.T) {
	_, takerNode, _, takerSession, _, _, _, _, _ := blindTakerFixture(t)
	tkLtc := takerNode.cfg().Connectors[takerSession.srcCur].(*fakeConnector)
	// Unrelated mempool entries: fetchable bodies that do not spend our
	// deposit outpoint (mempool churn — bodies the wallet cannot serve — is
	// covered by TestOwnWatchScanMarksOnlyFetchedTxids: a failed fetch is
	// never marked seen, matching C++ which rescans every mempool tx next
	// tick).
	unrelated := &coins.Tx{Version: 1}
	unrelatedHex := hex.EncodeToString(unrelated.Serialize())
	txidA, txidB := strings.Repeat("11", 32), strings.Repeat("22", 32)
	tkLtc.setRawTx(txidA, unrelatedHex)
	tkLtc.setRawTx(txidB, unrelatedHex)
	tkLtc.mempoolTxids = []string{txidA, txidB}
	takerNode.watchOwnDepositSpends()
	if takerSession.state != csCreatedB || takerSession.claimTxID != "" {
		t.Fatalf("unrelated mempool progressed the session: state = %s, claimTxID = %q",
			takerSession.state.String(), takerSession.claimTxID)
	}
	seen := takerNode.ownWatchSeen[hexEncode(takerSession.id[:])]
	if len(seen) == 0 {
		t.Fatal("scanned txids not recorded in the seen-set (next sweep would refetch)")
	}
}

// TestOwnWatchSingleFlight (T11): a session with an in-flight scan is not
// double-enqueued (pendingOwnWatch guard, mirroring pendingWatch).
func TestOwnWatchSingleFlight(t *testing.T) {
	_, n, _, takerSession, _, _, _, _, _ := blindTakerFixture(t)
	n.engineRunning.Store(true)
	n.tasks = make(chan workTask, 1)
	n.pendingOwnWatch = map[string]bool{} // pre-start node: init defensively
	orderID := hexEncode(takerSession.id[:])
	n.pendingOwnWatch[orderID] = true
	n.watchOwnDepositSpends()
	if !n.pendingOwnWatch[orderID] {
		t.Fatal("guard lost: pendingOwnWatch cleared without a task completing")
	}
	if len(n.tasks) != 0 {
		t.Fatal("double-enqueued a scan despite the single-flight guard")
	}
	n.engineRunning.Store(false)
}

// TestOwnWatchExcludesMaker (T12): the C++ mempool scan is role-B only
// (:3378) — the maker generates its own secret and finishes via ConfirmA.
func TestOwnWatchExcludesMaker(t *testing.T) {
	n, _, makerSession, _, _, _, _, _, _ := blindTakerFixture(t)
	if makerSession.state != csFinished {
		t.Fatalf("precondition: maker state = %s, want finished", makerSession.state.String())
	}
	n.engineRunning.Store(true)
	n.tasks = make(chan workTask, 1)
	n.watchOwnDepositSpends()
	select {
	case <-n.tasks:
		t.Fatal("maker session entered the own-deposit watch (C++ scan is taker-only)")
	default:
	}
	n.engineRunning.Store(false)
}

// Compile-time interface guard for the fake used by the watch tests.
var _ wallet.Connector = (*fakeConnector)(nil)

// TestRefundSweepSkipsPostClaimSessions: C++ parity (xbridgeapp.cpp:3444) —
// the locktime refund path only runs while the counterparty deposit is NOT
// yet redeemed. A session past its own claim broadcast must never broadcast
// its (already-unspendable) refund: the input was consumed by the
// counterparty's claim, so the attempt can only end in a -25 reject and a
// bogus rollback-failed status (live-proven on restored order fda21ce4).
func TestRefundSweepSkipsPostClaimSessions(t *testing.T) {
	makerNode, _, makerSession, _, _, _, _, _, _ := blindTakerFixture(t)
	if makerSession.state != csFinished {
		t.Fatalf("precondition: maker state = %s, want finished", makerSession.state.String())
	}
	// Reconstruct the pre-fix restore shape: claim broadcast recorded but the
	// session parked at the confirmed state with a pre-signed refund and an
	// expired locktime.
	conn := makerNode.cfg().Connectors[makerSession.srcCur].(*fakeConnector)
	before := len(conn.broadcasts)
	makerSession.state = csConfirmedA
	makerSession.refundHex = "deadbeef"
	makerSession.ourLockTime = 1 // long expired
	makerNode.scanRefunds()
	if len(conn.broadcasts) != before {
		t.Fatalf("refund sweep broadcast for a post-claim session (%d broadcasts, want %d)",
			len(conn.broadcasts), before)
	}
}

// TestClaimAdoptionReconcilesConfirmedState: a restored session parked at the
// confirmed state (claim broadcast succeeded, hub Finished never observed)
// reconciles through the adoption replay — claim verified on-chain, session
// terminal, no repost of already-confirmed bytes.
func TestClaimAdoptionReconcilesConfirmedState(t *testing.T) {
	makerNode, takerNode, _, takerSession, _, _, makerPayTxID, tkLtcConn, mkLtcConn := blindTakerFixture(t)
	_ = makerNode
	hx, ok := mkLtcConn.rawTx[makerPayTxID]
	if !ok {
		t.Fatal("maker payTx missing from maker connector")
	}
	tkLtcConn.rawTx[makerPayTxID] = hx
	if _, _, err := takerSession.OnConfirmB(&proto.ConfirmBBody{HubAddress: takerSession.hub, ID: takerSession.id, APayTxID: makerPayTxID}); err != nil {
		t.Fatalf("OnConfirmB: %v", err)
	}
	if takerSession.state != csFinished {
		t.Fatalf("precondition: state = %s, want finished", takerSession.state.String())
	}
	// Simulate the pre-fix restore shape: confirmed state with a due retry
	// schedule and the claim already confirmed on-chain.
	claimID := takerSession.claimTxID
	if claimID == "" || takerSession.claimHex == "" || takerSession.claimCur == "" {
		t.Fatalf("precondition broken: claimTxID=%q claimHex=%d bytes claimCur=%q",
			claimID, len(takerSession.claimHex), takerSession.claimCur)
	}
	if takerSession.await {
		t.Fatal("precondition broken: await still set after the inline claim")
	}
	tkBtc := takerNode.cfg().Connectors[takerSession.dstCur].(*fakeConnector)
	tkBtc.verboseTx = map[string]wallet.VerboseTx{claimID: {TxID: claimID, Confirmations: 3}}
	before := len(tkBtc.broadcasts)
	takerSession.state = csConfirmedB
	takerSession.claimRetryAt = 1
	takerNode.retryFailedClaimBuilds(NowMicro())
	if takerSession.state != csFinished {
		t.Fatalf("state = %s, want finished after reconciliation adoption", takerSession.state.String())
	}
	if len(tkBtc.broadcasts) != before {
		t.Fatal("adoption rebroadcast a confirmed claim (must adopt, not resend)")
	}
	if takerSession.claimRetryAt != 0 {
		t.Fatal("retry schedule not consumed by the reconciliation")
	}
}
