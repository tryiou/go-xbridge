package api

// Counterparty-deposit watch: vanish countdown, flap tolerance, claim and
// watchdog stand-downs (C++ watchForSpentDeposit). In-memory fixtures — no
// live hub, no network.
import (
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"go-xbridge/coins"
	"go-xbridge/proto"
	"go-xbridge/wallet"
)

// errTxOutConn fails GetTxOut, simulating a wallet that cannot answer the
// unspent probe (transient: the watch must skip the round, never cancel).
type errTxOutConn struct {
	wallet.Connector
}

func (e errTxOutConn) GetTxOut(string, uint32) (wallet.Utxo, bool, error) {
	return wallet.Utxo{}, false, errNotFound
}

// watchTakerFixture builds a taker session in the pre-claim window (own
// deposit broadcast, counterparty A-deposit validated) with a live order.
func watchTakerFixture(t *testing.T, btc wallet.Connector) (*Node, *captureXConn, string) {
	t.Helper()
	alignInitCoins(t)
	ltc := &fakeConnector{ticker: "LTC", funding: wallet.Utxo{TxID: strings.Repeat("bb", 32), Amount: 5e8},
		changeAddr: addrFor(48, "c"), blockHeight: 1000, rawTx: map[string]string{}}
	n := newTestNode(t, alignConfs(), map[string]wallet.Connector{"BTC": btc, "LTC": ltc})
	cc := &captureXConn{}
	n.conn = cc

	var id [32]byte
	oid := hash20("align-deposit-watch")
	copy(id[:], oid[:])
	idHex := hexEncode(id[:])
	tPriv, tPub := newKey(t)
	o := &Order{ID: id, FromCurrency: "BTC", ToCurrency: "LTC", FromAmount: 2.5e6,
		ToAmount: 2e6, Mine: true, Status: "created",
		RefundTx: "deadbeef", DepositSent: true}
	n.newTakerSession(withUsedCoins(t, n, o, []wallet.Utxo{ltc.funding}),
		TakeOrderParams{FromAddress: addrFor(48, "t-src"), ToAddress: addrFor(0, "t-dst")}, arr32(tPriv), toArr33(tPub))
	// Production TakeOrder records our per-trade M pubkey on the order; the
	// cancel path authenticates our own Cancel packet against it
	// (handleRemoteCancel iCanceled, C++ :3351-3357).
	o.MakerKey = hexEncode(tPub)
	s := n.sessions[idHex]
	s.state = csCreatedB
	s.theirDepositTxID = strings.Repeat("cc", 32)
	s.theirDepositVout = 0
	s.theirLockTime = 1030
	s.theirSecretHash = hash20("align-secret")
	s.refundHex = "deadbeef"
	// The fixture docstring promises a VALIDATED counterparty deposit; the
	// build path records validation via theirP2SHNative (checkCounterpartyDeposit
	// → applyCreatedB/applyConfirmedA). The watch may only cancel sessions
	// whose deposit validated at least once.
	s.theirP2SHNative = 1
	return n, cc, idHex
}

// TestDepositWatchCancelsOnSpentDeposit pins the fund-safety watch (C++
// watchForSpentDeposit / xbridgeapp.cpp:3441): a validated counterparty
// deposit that stays vanished past the countdown is wire-cancelled with the
// taker's deposit reason (crBadADepositTx) and rolled back, instead of
// stalling or claiming into the void.
func TestDepositWatchCancelsOnSpentDeposit(t *testing.T) {
	btc := &fakeConnector{ticker: "BTC", funding: wallet.Utxo{TxID: strings.Repeat("aa", 32), Amount: 5e8},
		changeAddr: addrFor(0, "c"), blockHeight: 1000, rawTx: map[string]string{}}
	n, cc, idHex := watchTakerFixture(t, btc)

	for i := 0; i < vanishThreshold(t); i++ {
		n.watchCounterpartyDeposits()
	}

	pkts := cc.snapshot()
	if len(pkts) != 1 || pkts[0].Command != proto.XbcTransactionCancel {
		t.Fatalf("want exactly one Cancel packet, got %v", pkts)
	}
	var cb proto.CancelBody
	if err := cb.Unmarshal(pkts[0].Body); err != nil {
		t.Fatalf("Cancel body decode: %v", err)
	}
	if cb.Reason != uint32(crBadADepositTx) {
		t.Fatalf("cancel reason = %d, want crBadADepositTx (%d)", cb.Reason, crBadADepositTx)
	}
	if got := n.store.Get(idHex); got.Status != "rolled back" {
		t.Fatalf("order status = %q, want rolled back", got.Status)
	}
}

// TestDepositWatchQuietWhenUnspent pins the negative: an unspent counterparty
// deposit produces no packet and no state change.
func TestDepositWatchQuietWhenUnspent(t *testing.T) {
	btc := &fakeConnector{ticker: "BTC", funding: wallet.Utxo{TxID: strings.Repeat("aa", 32), Amount: 5e8},
		changeAddr: addrFor(0, "c"), blockHeight: 1000, rawTx: map[string]string{}}
	// The validated A-deposit exists on-chain.
	dep := &coins.Tx{Version: 1}
	dep.Inputs = append(dep.Inputs, coins.TxIn{Sequence: 0xffffffff})
	dep.Outputs = append(dep.Outputs, coins.TxOut{Value: 2.5e8, ScriptPubKey: []byte{0x51}})
	btc.setRawTx(strings.Repeat("cc", 32), hex.EncodeToString(dep.Serialize()))
	n, cc, idHex := watchTakerFixture(t, btc)

	n.watchCounterpartyDeposits()

	if pkts := cc.snapshot(); len(pkts) != 0 {
		t.Fatalf("unspent deposit must produce no packets, got %v", pkts)
	}
	if got := n.store.Get(idHex); got.Status != "created" {
		t.Fatalf("order status = %q, want unchanged created", got.Status)
	}
}

// TestDepositWatchSkipsTransientErrors pins the fail-open direction: a wallet
// that cannot answer the probe skips the round instead of cancelling a
// healthy swap.
func TestDepositWatchSkipsTransientErrors(t *testing.T) {
	n, cc, idHex := watchTakerFixture(t, errTxOutConn{})

	n.watchCounterpartyDeposits()

	if pkts := cc.snapshot(); len(pkts) != 0 {
		t.Fatalf("transient probe error must produce no packets, got %v", pkts)
	}
	if got := n.store.Get(idHex); got.Status != "created" {
		t.Fatalf("order status = %q, want unchanged created", got.Status)
	}
}

// TestDepositWatchApplySkipsClaimInFlight pins the late-landing apply: a
// probe enqueued before the claim build that lands after claimHex exists
// must not wire-cancel a claimable session.
func TestDepositWatchApplySkipsClaimInFlight(t *testing.T) {
	btc := &fakeConnector{ticker: "BTC", funding: wallet.Utxo{TxID: strings.Repeat("aa", 32), Amount: 5e8},
		changeAddr: addrFor(0, "c"), blockHeight: 1000, rawTx: map[string]string{}}
	n, cc, idHex := watchTakerFixture(t, btc)
	// Simulate the claim built while the probe was in flight: the
	// counterparty deposit reads missing because our own claim spent it.
	s := n.sessions[idHex]
	s.claimHex = "deadbeefclaim"
	s.claimTxID = strings.Repeat("dd", 32)
	s.claimCur = "BTC"

	n.postDepositWatchTask(idHex)

	if pkts := cc.snapshot(); len(pkts) != 0 {
		t.Fatalf("claim-in-flight session must produce no packets, got %v", pkts)
	}
	if got := n.store.Get(idHex); got.Status != "created" {
		t.Fatalf("order status = %q, want unchanged created", got.Status)
	}
}

// TestDepositWatchApplySkipsAfterBroadcast pins the post-broadcast landing:
// a probe that lands after the claim broadcast (CounterpartyRedeemed
// recorded) must not emit a spurious wire Cancel, even though the local
// cancel handler would ignore it.
func TestDepositWatchApplySkipsAfterBroadcast(t *testing.T) {
	btc := &fakeConnector{ticker: "BTC", funding: wallet.Utxo{TxID: strings.Repeat("aa", 32), Amount: 5e8},
		changeAddr: addrFor(0, "c"), blockHeight: 1000, rawTx: map[string]string{}}
	n, cc, idHex := watchTakerFixture(t, btc)
	// Simulate the claim broadcast racing the probe: redemption recorded on
	// the order while the GetTxOut probe was in flight.
	if !n.store.Update(idHex, func(o *Order) { o.CounterpartyRedeemed = true }) {
		t.Fatal("store update failed")
	}

	n.postDepositWatchTask(idHex)

	if pkts := cc.snapshot(); len(pkts) != 0 {
		t.Fatalf("post-broadcast session must produce no packets, got %v", pkts)
	}
	if got := n.store.Get(idHex); got.Status != "created" {
		t.Fatalf("order status = %q, want unchanged created", got.Status)
	}
}

// TestDepositWatchApplySkipsWhileAwait pins the build-in-flight landing: a
// probe that lands while a deposit/claim task holds await must not cancel.
func TestDepositWatchApplySkipsWhileAwait(t *testing.T) {
	btc := &fakeConnector{ticker: "BTC", funding: wallet.Utxo{TxID: strings.Repeat("aa", 32), Amount: 5e8},
		changeAddr: addrFor(0, "c"), blockHeight: 1000, rawTx: map[string]string{}}
	n, cc, idHex := watchTakerFixture(t, btc)
	n.sessions[idHex].holdAwait()

	n.postDepositWatchTask(idHex)

	if pkts := cc.snapshot(); len(pkts) != 0 {
		t.Fatalf("await-held session must produce no packets, got %v", pkts)
	}
	if got := n.store.Get(idHex); got.Status != "created" {
		t.Fatalf("order status = %q, want unchanged created", got.Status)
	}
}

// TestDepositWatchEnqueueSkipsClaimBuilt pins the enqueue half of the
// claim stand-down: a session with claim material never enqueues a probe,
// so no wasted gettxout fires while the claim awaits broadcast.
func TestDepositWatchEnqueueSkipsClaimBuilt(t *testing.T) {
	btc := &fakeConnector{ticker: "BTC", funding: wallet.Utxo{TxID: strings.Repeat("aa", 32), Amount: 5e8},
		changeAddr: addrFor(0, "c"), blockHeight: 1000, rawTx: map[string]string{}}
	n, cc, idHex := watchTakerFixture(t, btc)
	s := n.sessions[idHex]
	s.claimHex = "deadbeefclaim"
	s.claimTxID = strings.Repeat("dd", 32)
	s.claimCur = "BTC"

	n.watchCounterpartyDeposits()

	if pkts := cc.snapshot(); len(pkts) != 0 {
		t.Fatalf("claim-built session must enqueue no probe and produce no packets, got %v", pkts)
	}
	if got := n.store.Get(idHex); got.Status != "created" {
		t.Fatalf("order status = %q, want unchanged created", got.Status)
	}
}

// TestDepositWatchNoRecancelAfterRollback pins the success path: after a
// watch-driven cancel whose refund broadcast succeeds, the session is
// pruned (it stops being swept), so a second sweep emits no duplicate wire
// Cancel. It does not cover refund-pending/failure windows, where the
// session stays live by design (separate triage).
func TestDepositWatchNoRecancelAfterRollback(t *testing.T) {
	btc := &fakeConnector{ticker: "BTC", funding: wallet.Utxo{TxID: strings.Repeat("aa", 32), Amount: 5e8},
		changeAddr: addrFor(0, "c"), blockHeight: 1000, rawTx: map[string]string{}}
	n, cc, idHex := watchTakerFixture(t, btc)

	for i := 0; i < vanishThreshold(t); i++ {
		n.watchCounterpartyDeposits()
	}
	if got := n.store.Get(idHex); got.Status != "rolled back" {
		t.Fatalf("order status = %q, want rolled back after countdown", got.Status)
	}

	n.watchCounterpartyDeposits()

	if pkts := cc.snapshot(); len(pkts) != 1 {
		t.Fatalf("second sweep must not re-cancel, want exactly 1 Cancel total, got %v", pkts)
	}
}

// vanishThreshold returns the countdown length for the watch fixtures
// (taker watches the BTC deposit; alignConfs sets BTC BlockTime=60).
func vanishThreshold(t *testing.T) int {
	t.Helper()
	k := depositWatchVanishThreshold(60)
	if k < 2 {
		t.Fatalf("threshold = %d, want >= 2 for a meaningful countdown", k)
	}
	return k
}

// unvanishDeposit re-serves the fixture's counterparty deposit on-chain
// (the QuietWhenUnspent pattern); vanishDeposit drops it again. The fake
// has no unsetter, so the test performs the map surgery under the lock,
// exactly as setRawTx does for seeding.
func unvanishDeposit(btc *fakeConnector, txid string) {
	dep := &coins.Tx{Version: 1}
	dep.Inputs = append(dep.Inputs, coins.TxIn{Sequence: 0xffffffff})
	dep.Outputs = append(dep.Outputs, coins.TxOut{Value: 2.5e8, ScriptPubKey: []byte{0x51}})
	btc.setRawTx(txid, hex.EncodeToString(dep.Serialize()))
}

func vanishDeposit(btc *fakeConnector, txid string) {
	btc.mu.Lock()
	defer btc.mu.Unlock()
	delete(btc.rawTx, txid)
}

// TestDepositWatchFlapSurvivesTransientVanish pins the countdown's reason
// to exist: a single missing reading (reorg, propagation lag) must not
// cancel, and a reappeared deposit resets the countdown — cancelling
// again requires a full fresh run of consecutive missings.
func TestDepositWatchFlapSurvivesTransientVanish(t *testing.T) {
	btc := &fakeConnector{ticker: "BTC", funding: wallet.Utxo{TxID: strings.Repeat("aa", 32), Amount: 5e8},
		changeAddr: addrFor(0, "c"), blockHeight: 1000, rawTx: map[string]string{}}
	n, cc, idHex := watchTakerFixture(t, btc)
	depID := strings.Repeat("cc", 32)
	k := vanishThreshold(t)

	n.watchCounterpartyDeposits() // miss 1: silent
	if pkts := cc.snapshot(); len(pkts) != 0 {
		t.Fatalf("first missing must not cancel, got %v", pkts)
	}

	unvanishDeposit(btc, depID)
	n.watchCounterpartyDeposits() // reappeared: silent + reset
	if pkts := cc.snapshot(); len(pkts) != 0 {
		t.Fatalf("reappeared deposit must not cancel, got %v", pkts)
	}

	// Fresh vanish run: silence for k-1, cancel exactly on the kth. If the
	// reappearance had not reset the countdown, the cancel would land one
	// sweep early.
	vanishDeposit(btc, depID)
	for i := 1; i < k; i++ {
		n.watchCounterpartyDeposits()
		if pkts := cc.snapshot(); len(pkts) != 0 {
			t.Fatalf("missing %d/%d must not cancel yet, got %v", i, k, pkts)
		}
	}
	n.watchCounterpartyDeposits()
	pkts := cc.snapshot()
	if len(pkts) != 1 || pkts[0].Command != proto.XbcTransactionCancel {
		t.Fatalf("kth consecutive missing must cancel once, got %v", pkts)
	}
	var cb proto.CancelBody
	if err := cb.Unmarshal(pkts[0].Body); err != nil {
		t.Fatalf("Cancel body decode: %v", err)
	}
	if cb.Reason != uint32(crBadADepositTx) {
		t.Fatalf("cancel reason = %d, want crBadADepositTx (%d)", cb.Reason, crBadADepositTx)
	}
	if got := n.store.Get(idHex); got.Status != "rolled back" {
		t.Fatalf("order status = %q, want rolled back", got.Status)
	}
}

// TestDepositWatchCancelsAfterPersistentVanish pins the countdown's other
// half: a vanish that never resolves still cancels — patience is bounded.
func TestDepositWatchCancelsAfterPersistentVanish(t *testing.T) {
	btc := &fakeConnector{ticker: "BTC", funding: wallet.Utxo{TxID: strings.Repeat("aa", 32), Amount: 5e8},
		changeAddr: addrFor(0, "c"), blockHeight: 1000, rawTx: map[string]string{}}
	n, cc, idHex := watchTakerFixture(t, btc)
	k := vanishThreshold(t)

	for i := 1; i < k; i++ {
		n.watchCounterpartyDeposits()
		if pkts := cc.snapshot(); len(pkts) != 0 {
			t.Fatalf("missing %d/%d must not cancel yet, got %v", i, k, pkts)
		}
	}
	n.watchCounterpartyDeposits()
	pkts := cc.snapshot()
	if len(pkts) != 1 || pkts[0].Command != proto.XbcTransactionCancel {
		t.Fatalf("kth consecutive missing must cancel once, got %v", pkts)
	}
	if got := n.store.Get(idHex); got.Status != "rolled back" {
		t.Fatalf("order status = %q, want rolled back", got.Status)
	}
}

// TestDepositWatchVanishThreshold pins the chain-scaled countdown table:
// ~2 blocks of patience bounded to [3,12] sweeps; unknown speed defaults
// to the patient middle.
func TestDepositWatchVanishThreshold(t *testing.T) {
	for _, tc := range []struct {
		blockTime, want int
	}{
		{-1, 6}, {0, 6}, {15, 3}, {30, 3}, {60, 3}, {150, 6}, {300, 11}, {600, 12}, {7200, 12},
	} {
		if got := depositWatchVanishThreshold(tc.blockTime); got != tc.want {
			t.Errorf("depositWatchVanishThreshold(%d) = %d, want %d", tc.blockTime, got, tc.want)
		}
	}
}

// TestDepositWatchWatchdogWinsTiebreak pins exactly-one-Cancel when the
// stall watchdog fires first: its crTimeout cancel rolls back and prunes,
// so the deposit watch stays silent afterwards.
func TestDepositWatchWatchdogWinsTiebreak(t *testing.T) {
	btc := &fakeConnector{ticker: "BTC", funding: wallet.Utxo{TxID: strings.Repeat("aa", 32), Amount: 5e8},
		changeAddr: addrFor(0, "c"), blockHeight: 1000, rawTx: map[string]string{}}
	n, cc, idHex := watchTakerFixture(t, btc)
	// Silence past the stall threshold, derived from it so a threshold
	// bump cannot silently turn this into a no-op.
	n.sessions[idHex].lastProgress = uint64(NowMicro()) - uint64(sessionStallMicro) - uint64(60*1000000)

	n.watchStalledSessions()

	pkts := cc.snapshot()
	if len(pkts) != 1 || pkts[0].Command != proto.XbcTransactionCancel {
		t.Fatalf("watchdog must cancel once, got %v", pkts)
	}
	var cb proto.CancelBody
	if err := cb.Unmarshal(pkts[0].Body); err != nil {
		t.Fatalf("Cancel body decode: %v", err)
	}
	if cb.Reason != uint32(crTimeout) {
		t.Fatalf("cancel reason = %d, want crTimeout (%d)", cb.Reason, crTimeout)
	}

	n.watchCounterpartyDeposits()
	if pkts := cc.snapshot(); len(pkts) != 1 {
		t.Fatalf("deposit watch must stay silent after watchdog cancel, want 1 Cancel total, got %v", pkts)
	}
}

// TestDepositWatchStandsDownUntilValidated pins the S5 lesson (BLOCK/PIVX
// order 9698af09, 2026-09-14): a counterparty deposit that never validated in
// our build (theirP2SHNative == 0) reading "unknown" from a chain-blind
// backend is the 0-conf propagation race, NOT a proven vanish — the watch
// must stand down and leave the session to the build/retry path (which never
// cancels on blindness). Only a VALIDATED deposit that later vanishes
// (double-spend/reorg) cancels — pinned by TestDepositWatchCancelsOnSpentDeposit.
func TestDepositWatchStandsDownUntilValidated(t *testing.T) {
	btc := &fakeConnector{ticker: "BTC", funding: wallet.Utxo{TxID: strings.Repeat("aa", 32), Amount: 5e8},
		changeAddr: addrFor(0, "c"), blockHeight: 1000, rawTx: map[string]string{}}
	n, cc, idHex := watchTakerFixture(t, btc)
	// Undo the fixture's validation: the build has never succeeded for this
	// session (the S5 race: the deposit is younger than our backend's view).
	if s := n.sessions[idHex]; s != nil {
		s.theirP2SHNative = 0
	}

	n.watchCounterpartyDeposits()

	if pkts := cc.snapshot(); len(pkts) != 0 {
		t.Fatalf("unvalidated deposit must not be watched to cancellation, got %v", pkts)
	}
	if got := n.store.Get(idHex); got == nil || got.Status != "created" {
		t.Fatalf("order must stay live and unchanged, got %+v", got)
	}
}

// stageOwnDeposit classifies the fixture taker's own (LTC) deposit for the
// counterparty-vanish tests: known requires a verbose entry (the fake
// asserts depth on serve); spent additionally requires gettxout-absence;
// unspent additionally serves the outpoint via rawTx; blind stages neither.
func stageOwnDeposit(ltc *fakeConnector, txid string, known, unspent bool) {
	if known {
		if ltc.verboseTx == nil {
			ltc.verboseTx = map[string]wallet.VerboseTx{}
		}
		ltc.verboseTx[txid] = wallet.VerboseTx{Confirmations: 5}
	}
	if unspent {
		unvanishDeposit(ltc, txid)
	}
}

// TestCounterpartyVanishWithOwnSpentArmsHunt pins Commit 3's core: a taker
// whose counterparty deposit is durably missing while its own deposit is
// proven spent hunts the secret instead of cancelling — cancelling would
// broadcast a refund that can never confirm. The first k-1 sweeps must
// neither cancel nor hunt (the own-probe runs only on the decisive round);
// the kth arms the hunt with zero packets. The load contract (no mempool
// or block reads on this path) is pinned by call counters: any such call,
// even a swallowed one, breaks the zero asserts at the end.
func TestCounterpartyVanishWithOwnSpentArmsHunt(t *testing.T) {
	btc := &fakeConnector{ticker: "BTC", funding: wallet.Utxo{TxID: strings.Repeat("aa", 32), Amount: 5e8},
		changeAddr: addrFor(0, "c"), blockHeight: 1000, rawTx: map[string]string{}}
	n, cc, idHex := watchTakerFixture(t, btc)
	ltc := n.cfg().Connectors["LTC"].(*fakeConnector)
	ownDep := strings.Repeat("dd", 32)
	n.sessions[idHex].ourDepositTxID = ownDep
	stageOwnDeposit(ltc, ownDep, true, false)
	k := vanishThreshold(t)

	for i := 1; i < k; i++ {
		n.watchCounterpartyDeposits()
		if pkts := cc.snapshot(); len(pkts) != 0 {
			t.Fatalf("missing %d/%d must not cancel yet, got %v", i, k, pkts)
		}
		if n.sessions[idHex].secretHunt {
			t.Fatalf("hunt must arm only on the decisive round, armed at missing %d/%d", i, k)
		}
	}
	n.watchCounterpartyDeposits()
	if pkts := cc.snapshot(); len(pkts) != 0 {
		t.Fatalf("own-spent vanish must hunt, not cancel, got %v", pkts)
	}
	s := n.sessions[idHex]
	if !s.secretHunt {
		t.Fatal("taker with spent own deposit must be hunting")
	}
	if got := n.store.Get(idHex); got.Status != "created" {
		t.Fatalf("order status = %q, want unchanged created", got.Status)
	}
	for _, c := range []*fakeConnector{btc, ltc} {
		if c.mempoolCalls != 0 || c.blockTxsCalls != 0 {
			t.Fatalf("%s: mempool/block calls = %d/%d, want 0/0", c.ticker, c.mempoolCalls, c.blockTxsCalls)
		}
	}
}

// TestHuntedSessionSkipsCounterpartyWatch pins the companion stand-down: a
// hunting session stays silent under persistent counterparty missing —
// cancelling it would kill the hunt with an unconfirmable refund.
func TestHuntedSessionSkipsCounterpartyWatch(t *testing.T) {
	btc := &fakeConnector{ticker: "BTC", funding: wallet.Utxo{TxID: strings.Repeat("aa", 32), Amount: 5e8},
		changeAddr: addrFor(0, "c"), blockHeight: 1000, rawTx: map[string]string{}}
	n, cc, idHex := watchTakerFixture(t, btc)
	n.sessions[idHex].secretHunt = true

	for i := 0; i < vanishThreshold(t); i++ {
		n.watchCounterpartyDeposits()
	}
	if pkts := cc.snapshot(); len(pkts) != 0 {
		t.Fatalf("hunted session must stay silent, got %v", pkts)
	}
	if !n.sessions[idHex].secretHunt {
		t.Fatal("hunt flag must survive the watch")
	}
	if got := n.store.Get(idHex); got.Status != "created" {
		t.Fatalf("order status = %q, want unchanged created", got.Status)
	}
}

// TestHuntedSessionApplySkipsLateProbe pins the apply-time hunt stand-down
// for the mid-flight landing: a probe enqueued pre-hunt that applies after
// the hunt armed must not count or cancel. Driven via postDepositWatchTask
// directly (bypassing the enqueue stand-down) with the own deposit staged
// blind, so the decisive round's own-probe cannot prove spent (which would
// take the hunt branch and mask the new check) — only the apply-time hunt
// check can silence it.
func TestHuntedSessionApplySkipsLateProbe(t *testing.T) {
	btc := &fakeConnector{ticker: "BTC", funding: wallet.Utxo{TxID: strings.Repeat("aa", 32), Amount: 5e8},
		changeAddr: addrFor(0, "c"), blockHeight: 1000, rawTx: map[string]string{}}
	n, cc, idHex := watchTakerFixture(t, btc)
	s := n.sessions[idHex]
	s.secretHunt = true
	s.ourDepositTxID = strings.Repeat("dd", 32)

	for i := 0; i < vanishThreshold(t); i++ {
		n.postDepositWatchTask(idHex)
	}
	if pkts := cc.snapshot(); len(pkts) != 0 {
		t.Fatalf("late probe on hunted session must stay silent, got %v", pkts)
	}
	if !s.secretHunt {
		t.Fatal("hunt flag must survive the late probe")
	}
	if got := n.store.Get(idHex); got.Status != "created" {
		t.Fatalf("order status = %q, want unchanged created", got.Status)
	}
}

// TestCounterpartyVanishMakerOwnSpentStaysLoud pins the maker branch: a
// spent maker deposit without local finish contradicts the protocol (the
// taker can only spend after our claim, which finishes us), so the watch
// cancels exactly as before and never hunts. Built on the taker fixture
// with the role flipped: only the role-gated branch is under test, so the
// mismatched currencies are documented, not hidden.
func TestCounterpartyVanishMakerOwnSpentStaysLoud(t *testing.T) {
	btc := &fakeConnector{ticker: "BTC", funding: wallet.Utxo{TxID: strings.Repeat("aa", 32), Amount: 5e8},
		changeAddr: addrFor(0, "c"), blockHeight: 1000, rawTx: map[string]string{}}
	n, cc, idHex := watchTakerFixture(t, btc)
	s := n.sessions[idHex]
	s.isMaker = true
	s.state = csCreatedA
	ltc := n.cfg().Connectors["LTC"].(*fakeConnector)
	ownDep := strings.Repeat("dd", 32)
	s.ourDepositTxID = ownDep
	stageOwnDeposit(ltc, ownDep, true, false)
	k := vanishThreshold(t)

	for i := 1; i < k; i++ {
		n.watchCounterpartyDeposits()
		if pkts := cc.snapshot(); len(pkts) != 0 {
			t.Fatalf("missing %d/%d must not cancel yet, got %v", i, k, pkts)
		}
	}
	n.watchCounterpartyDeposits()
	pkts := cc.snapshot()
	if len(pkts) != 1 || pkts[0].Command != proto.XbcTransactionCancel {
		t.Fatalf("maker own-spent vanish must cancel once, got %v", pkts)
	}
	var cb proto.CancelBody
	if err := cb.Unmarshal(pkts[0].Body); err != nil {
		t.Fatalf("Cancel body decode: %v", err)
	}
	if cb.Reason != uint32(crBadBDepositTx) {
		t.Fatalf("cancel reason = %d, want crBadBDepositTx (%d)", cb.Reason, crBadBDepositTx)
	}
	// The cancel prunes the session after the refund broadcast; the
	// retained pointer still proves no hunt was armed along the way.
	if s.secretHunt {
		t.Fatal("maker must never hunt")
	}
	if got := n.store.Get(idHex); got.Status != "rolled back" {
		t.Fatalf("order status = %q, want rolled back", got.Status)
	}
}

// TestCounterpartyVanishOwnUnspentCancels pins the genuine-vanish path: an
// own deposit still unspent means nobody claimed, so the durable
// counterparty vanish cancels exactly as before, with no hunt.
func TestCounterpartyVanishOwnUnspentCancels(t *testing.T) {
	btc := &fakeConnector{ticker: "BTC", funding: wallet.Utxo{TxID: strings.Repeat("aa", 32), Amount: 5e8},
		changeAddr: addrFor(0, "c"), blockHeight: 1000, rawTx: map[string]string{}}
	n, cc, idHex := watchTakerFixture(t, btc)
	ltc := n.cfg().Connectors["LTC"].(*fakeConnector)
	ownDep := strings.Repeat("dd", 32)
	s := n.sessions[idHex]
	s.ourDepositTxID = ownDep
	stageOwnDeposit(ltc, ownDep, true, true)
	k := vanishThreshold(t)

	for i := 1; i < k; i++ {
		n.watchCounterpartyDeposits()
		if pkts := cc.snapshot(); len(pkts) != 0 {
			t.Fatalf("missing %d/%d must not cancel yet, got %v", i, k, pkts)
		}
	}
	n.watchCounterpartyDeposits()
	pkts := cc.snapshot()
	if len(pkts) != 1 || pkts[0].Command != proto.XbcTransactionCancel {
		t.Fatalf("genuine vanish must cancel once, got %v", pkts)
	}
	// The cancel prunes the session after the refund broadcast; the
	// retained pointer still proves no hunt was armed along the way.
	if s.secretHunt {
		t.Fatal("genuine vanish must not hunt")
	}
	if got := n.store.Get(idHex); got.Status != "rolled back" {
		t.Fatalf("order status = %q, want rolled back", got.Status)
	}
}

// TestCounterpartyVanishOwnBlindCancels pins the no-proof direction: a
// blind own-deposit probe proves nothing, so the durable counterparty
// vanish cancels exactly as before, with no hunt.
func TestCounterpartyVanishOwnBlindCancels(t *testing.T) {
	btc := &fakeConnector{ticker: "BTC", funding: wallet.Utxo{TxID: strings.Repeat("aa", 32), Amount: 5e8},
		changeAddr: addrFor(0, "c"), blockHeight: 1000, rawTx: map[string]string{}}
	n, cc, idHex := watchTakerFixture(t, btc)
	s := n.sessions[idHex]
	s.ourDepositTxID = strings.Repeat("dd", 32)
	k := vanishThreshold(t)

	for i := 1; i < k; i++ {
		n.watchCounterpartyDeposits()
		if pkts := cc.snapshot(); len(pkts) != 0 {
			t.Fatalf("missing %d/%d must not cancel yet, got %v", i, k, pkts)
		}
	}
	n.watchCounterpartyDeposits()
	pkts := cc.snapshot()
	if len(pkts) != 1 || pkts[0].Command != proto.XbcTransactionCancel {
		t.Fatalf("blind own-probe vanish must cancel once, got %v", pkts)
	}
	// The cancel prunes the session after the refund broadcast; the
	// retained pointer still proves no hunt was armed along the way.
	if s.secretHunt {
		t.Fatal("blind own-probe must not hunt")
	}
	if got := n.store.Get(idHex); got.Status != "rolled back" {
		t.Fatalf("order status = %q, want rolled back", got.Status)
	}
}

// TestDepositWatchNoRecancelWhenSessionSurvivesRollback stages the
// started-mode async window the success path normally closes by pruning:
// cancel #1 sent, refund still in flight, session live, status already
// "rolled back". A second countdown must not re-cancel.
func TestDepositWatchNoRecancelWhenSessionSurvivesRollback(t *testing.T) {
	btc := &fakeConnector{ticker: "BTC", funding: wallet.Utxo{TxID: strings.Repeat("aa", 32), Amount: 5e8},
		changeAddr: addrFor(0, "c"), blockHeight: 1000, rawTx: map[string]string{}}
	n, cc, idHex := watchTakerFixture(t, btc)
	s := n.sessions[idHex]
	k := vanishThreshold(t)

	for i := 0; i < k; i++ {
		n.watchCounterpartyDeposits()
	}
	if pkts := cc.snapshot(); len(pkts) != 1 {
		t.Fatalf("first countdown must cancel once, got %v", pkts)
	}
	if got := n.store.Get(idHex); got.Status != "rolled back" {
		t.Fatalf("order status = %q, want rolled back", got.Status)
	}
	// The success path prunes the session; re-insert the retained object
	// to model a refund still in flight when the next sweep fires.
	n.sessions[idHex] = s
	for i := 0; i < k; i++ {
		n.watchCounterpartyDeposits()
		if pkts := cc.snapshot(); len(pkts) != 1 {
			t.Fatalf("surviving session must not re-cancel, want 1 total, got %v", pkts)
		}
	}
}

// TestDepositWatchNoRecancelAfterRollbackFailed pins the failed-refund
// window: cancel #1 sent, refund broadcast failed ("rollback failed",
// backoff owned by the sweep), session live. The next countdown must not
// re-cancel — retry belongs to scanRefunds, not to a second Cancel.
func TestDepositWatchNoRecancelAfterRollbackFailed(t *testing.T) {
	btc := &fakeConnector{ticker: "BTC", funding: wallet.Utxo{TxID: strings.Repeat("aa", 32), Amount: 5e8},
		changeAddr: addrFor(0, "c"), blockHeight: 1000, rawTx: map[string]string{}}
	n, cc, idHex := watchTakerFixture(t, btc)
	n.cfg().Connectors["LTC"].(*fakeConnector).sendErr = errors.New("simulated rpc failure")
	k := vanishThreshold(t)

	for i := 0; i < k; i++ {
		n.watchCounterpartyDeposits()
	}
	if pkts := cc.snapshot(); len(pkts) != 1 {
		t.Fatalf("first countdown must cancel once, got %v", pkts)
	}
	got := n.store.Get(idHex)
	if got.Status != "rollback failed" {
		t.Fatalf("order status = %q, want rollback failed", got.Status)
	}
	if got.Reason != uint32(crBadADepositTx) {
		t.Fatalf("reason = %d, want crBadADepositTx (%d) preserved", got.Reason, crBadADepositTx)
	}
	if !n.refundBackoffActive(idHex) {
		t.Fatal("failed refund must own the retry via backoff")
	}
	if _, live := n.sessions[idHex]; !live {
		t.Fatal("failed refund must keep the session live")
	}
	for i := 0; i < k; i++ {
		n.watchCounterpartyDeposits()
		if pkts := cc.snapshot(); len(pkts) != 1 {
			t.Fatalf("failed-refund session must not re-cancel, want 1 total, got %v", pkts)
		}
	}
}

// TestDepositWatchSilentAfterUncancelledRefundFailure pins the absorbed
// first-notify: a refund that failed via scanRefunds (never cancelled,
// Reason 0) must not summon a first Cancel from the watch — the order
// stays in backoff-spaced retry, converging to C++ (no post-rollback
// cancel emission).
func TestDepositWatchSilentAfterUncancelledRefundFailure(t *testing.T) {
	btc := &fakeConnector{ticker: "BTC", funding: wallet.Utxo{TxID: strings.Repeat("aa", 32), Amount: 5e8},
		changeAddr: addrFor(0, "c"), blockHeight: 1000, rawTx: map[string]string{}}
	n, cc, idHex := watchTakerFixture(t, btc)
	ltc := n.cfg().Connectors["LTC"].(*fakeConnector)
	ltc.sendErr = errors.New("simulated rpc failure")

	n.scanRefunds()
	got := n.store.Get(idHex)
	if got.Status != "rollback failed" {
		t.Fatalf("order status = %q, want rollback failed", got.Status)
	}
	if got.Reason != 0 {
		t.Fatalf("reason = %d, want 0 (never cancelled)", got.Reason)
	}

	for i := 0; i < vanishThreshold(t); i++ {
		n.watchCounterpartyDeposits()
	}
	if pkts := cc.snapshot(); len(pkts) != 0 {
		t.Fatalf("uncancelled failed refund must stay silent, got %v", pkts)
	}
	if got := n.store.Get(idHex); got.Status != "rollback failed" {
		t.Fatalf("order status = %q, want unchanged rollback failed", got.Status)
	}
}

// TestWatchdogSilentAfterLiveWatchCancel pins the reverse tiebreak: after
// a watch cancel whose refund failed (live "rollback failed" session),
// an elapsed backoff window must not summon a watchdog re-cancel.
func TestWatchdogSilentAfterLiveWatchCancel(t *testing.T) {
	btc := &fakeConnector{ticker: "BTC", funding: wallet.Utxo{TxID: strings.Repeat("aa", 32), Amount: 5e8},
		changeAddr: addrFor(0, "c"), blockHeight: 1000, rawTx: map[string]string{}}
	n, cc, idHex := watchTakerFixture(t, btc)
	n.cfg().Connectors["LTC"].(*fakeConnector).sendErr = errors.New("simulated rpc failure")
	k := vanishThreshold(t)

	for i := 0; i < k; i++ {
		n.watchCounterpartyDeposits()
	}
	if pkts := cc.snapshot(); len(pkts) != 1 {
		t.Fatalf("first countdown must cancel once, got %v", pkts)
	}
	if got := n.store.Get(idHex); got.Status != "rollback failed" {
		t.Fatalf("order status = %q, want rollback failed", got.Status)
	}
	n.sessions[idHex].lastProgress = uint64(NowMicro()) - uint64(sessionStallMicro) - uint64(60*1000000)
	n.clearRefundBackoff(idHex) // emulate the elapsed backoff window
	n.watchStalledSessions()
	if pkts := cc.snapshot(); len(pkts) != 1 {
		t.Fatalf("watchdog must stay silent on failed refund, want 1 total, got %v", pkts)
	}
}
