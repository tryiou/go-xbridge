package api

// Tests for the bounded crNotAccepted take auto-retry (api/take_retry.go).
// The live campaign (run13, 2026-09-23) showed hubs rejecting takes with
// reason 8 "out of bounds block height" when a fast-block coin's tip drifted
// 2+ blocks between the taker's blockContext fetch and the hub's packet
// processing — a race every C++ client eats because processTransactionReject
// never retries. These tests pin: retries fire only for crNotAccepted on
// locally-taken orders, re-broadcast a FRESH Accepting (full TakeOrder path),
// cap out at Config.TakeRetry, are dropped on deterministic rejects and on
// order cancellation, and are pruned when the order leaves the reject window.

import (
	"sync"
	"testing"
	"time"

	"go-xbridge/coins"
	"go-xbridge/config"
	"go-xbridge/crypto"
	"go-xbridge/proto"
	"go-xbridge/wallet"
)

// shortenTakeRetryBackoff replaces the 20s/40s retry backoffs with 10ms so
// the tests drive the goroutine scheduling without real waits.
func shortenTakeRetryBackoff(t *testing.T) {
	t.Helper()
	orig := takeRetryBackoff
	takeRetryBackoff = []time.Duration{10 * time.Millisecond, 10 * time.Millisecond}
	t.Cleanup(func() { takeRetryBackoff = orig })
}

// waitForTakeRetry polls cond until it holds or the deadline passes; the final
// evaluation happens unconditionally so the caller's assertion reports state.
func waitForTakeRetry(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	cond()
}

// retryOrderID is the seeded takeable order's id (shared by the harness and
// the reject sender so packets address the right order).
func retryOrderID() [32]byte {
	var oid [32]byte
	copy(oid[:], []byte("take-retry-order-0000000000000"))
	return oid
}

// retryHubPriv is the seeded hub's private key (the order's SNodePubkey, so
// handleRemoteReject's snode verification accepts rejects signed with it).
func retryHubPriv() []byte {
	hubPriv := make([]byte, 32)
	hubPriv[31] = 4
	return hubPriv
}

// newTakeRetryNode is the A3 harness plus retry wiring: BTC funding + BLOCK
// fee candidates, a registered hub, started engine, temp datadir.
func newTakeRetryNode(t *testing.T, takeRetry int) (*Node, *captureXConn) {
	t.Helper()
	if err := coins.InitFromConf(map[string]*config.CoinConf{
		"BTC":   {Ticker: "BTC", Coin: 1e8, AddressPrefix: 0, CreateTxMethod: "BTC", BlockTime: 60},
		"BLOCK": {Ticker: "BLOCK", Coin: 1e8, AddressPrefix: 0, CreateTxMethod: "BTC", BlockTime: 60, TxVersion: 1},
	}); err != nil {
		t.Fatal(err)
	}
	n := newTestNode(t, map[string]*config.CoinConf{
		"BTC":   {Ticker: "BTC", Coin: 1e8, AddressPrefix: 0, CreateTxMethod: "BTC", BlockTime: 60},
		"BLOCK": {Ticker: "BLOCK", Coin: 1e8, AddressPrefix: 0, CreateTxMethod: "BTC", BlockTime: 60, TxVersion: 1},
	}, map[string]wallet.Connector{
		"BTC": &stubConn{ticker: "BTC", addr: btcAddr, utxos: []wallet.Utxo{
			{TxID: "0000000000000000000000000000000000000000000000000000000000000001", Vout: 0,
				Amount: 300000000, Value: 3.0, ScriptPubKey: "76a914000000000000000000000000000000000000000088ac", Address: btcAddr},
		}},
		"BLOCK": &stubConn{ticker: "BLOCK", addr: btcAddr, matureUtxos: []wallet.Utxo{blkUtxo()}},
	})
	cc := &captureXConn{}
	n.conn = cc
	n.config.DataDir = t.TempDir()
	n.config.TakeRetry = takeRetry
	n.start()
	t.Cleanup(func() { _ = n.Close() })

	registerHub(t, n, retryHubPriv())
	hubPub, err := crypto.CompressedPubKey(retryHubPriv())
	if err != nil {
		t.Fatal(err)
	}
	oid := retryOrderID()
	o := &Order{
		ID: oid, FromCurrency: "BTC", ToCurrency: "BTC", FromAmount: 2.5e6, ToAmount: 2.5e6,
		OrigFromAmount: 2.5e6, OrigToAmount: 2.5e6,
		Status: "open", SNodePubkey: hexPub(t, retryHubPriv()), HubAddress: coins.KeyID(hubPub[:]),
	}
	n.store.Add(o)
	return n, cc
}

func takeRetryReject(t *testing.T, n *Node, reason TxCancelReason) {
	t.Helper()
	body := &proto.RejectBody{ID: retryOrderID(), Reason: uint32(reason)}
	pkt := signBodyPacket(t, proto.XbcTransactionReject, body.Marshal(), retryHubPriv())
	n.submit(func() { n.handleRemoteReject(pkt, body) }, true)
}

// takeRetryEntry reports the pending retry entry for the order, if any.
func takeRetryEntry(n *Node) *takeRetryState {
	oid := retryOrderID()
	n.takeRetriesMu.Lock()
	defer n.takeRetriesMu.Unlock()
	if st, ok := n.takeRetries[hexEncode(oid[:])]; ok {
		cp := *st
		return &cp
	}
	return nil
}

// seedTakeRetryEntry inserts a pending retry entry directly (what commitTake's
// registerTakeRetry does on the wire path), for tests that exercise the
// entry lifecycle without a full take.
func seedTakeRetryEntry(n *Node, idHex string) {
	n.takeRetriesMu.Lock()
	defer n.takeRetriesMu.Unlock()
	if n.takeRetries == nil {
		n.takeRetries = map[string]*takeRetryState{}
	}
	n.takeRetries[idHex] = &takeRetryState{}
}

// takeRetryEntryPresent reports whether a retry entry exists for idHex.
func takeRetryEntryPresent(n *Node, idHex string) bool {
	n.takeRetriesMu.Lock()
	defer n.takeRetriesMu.Unlock()
	_, ok := n.takeRetries[idHex]
	return ok
}

// TestTakeRetryFiresAndCaps drives the full lifecycle: a committed take is
// rejected crNotAccepted → a retry re-broadcasts (fresh fee/funding/block
// context) → a second reject schedules the last retry → the third reject
// exhausts the cap: the entry is dropped, the order stays open, and nothing
// further is broadcast.
func TestTakeRetryFiresAndCaps(t *testing.T) {
	shortenTakeRetryBackoff(t)
	retryOID := retryOrderID()
	n, cc := newTakeRetryNode(t, 2)

	res, rerr := n.TakeOrder(TakeOrderParams{
		ID:          orderIDString(retryOID),
		FromAddress: btcAddr,
		ToAddress:   btcAddr2,
	})
	if rerr != nil {
		t.Fatalf("initial take: %v", rerr)
	}
	if res.Status != "accepting" {
		t.Fatalf("take status = %q, want accepting", res.Status)
	}
	if st := takeRetryEntry(n); st == nil || st.attempts != 0 {
		t.Fatalf("retry entry after commit = %+v, want registered with attempts=0", st)
	}
	if got := len(cc.snapshot()); got != 1 {
		t.Fatalf("packets after initial take = %d, want 1", got)
	}

	// Reject #1: schedules retry attempt 1, which re-broadcasts a fresh take.
	takeRetryReject(t, n, crNotAccepted)
	waitForTakeRetry(t, 2*time.Second, func() bool { return len(cc.snapshot()) >= 2 })
	if got := len(cc.snapshot()); got != 2 {
		t.Fatalf("packets after first crNotAccepted retry = %d, want 2", got)
	}
	if st := takeRetryEntry(n); st == nil || st.attempts != 1 {
		t.Fatalf("retry entry after first retry = %+v, want attempts=1", st)
	}
	if o := n.store.Get(hexEncode(retryOID[:])); o == nil || o.Status != "accepting" {
		t.Fatalf("order after retry = %+v, want re-committed accepting take", o)
	}

	// Reject #2: schedules the last retry (attempt 2).
	takeRetryReject(t, n, crNotAccepted)
	waitForTakeRetry(t, 2*time.Second, func() bool { return len(cc.snapshot()) >= 3 })
	if got := len(cc.snapshot()); got != 3 {
		t.Fatalf("packets after second retry = %d, want 3", got)
	}
	if st := takeRetryEntry(n); st == nil || st.attempts != 2 {
		t.Fatalf("retry entry after second retry = %+v, want attempts=2", st)
	}

	// Reject #3: cap exhausted — entry dropped, order left open, no 4th take.
	takeRetryReject(t, n, crNotAccepted)
	if st := takeRetryEntry(n); st != nil {
		t.Fatalf("retry entry after cap exhausted = %+v, want dropped", st)
	}
	waitForTakeRetry(t, 2*time.Second, func() bool { return len(cc.snapshot()) >= 4 })
	if got := len(cc.snapshot()); got != 3 {
		t.Fatalf("packets after exhausted cap = %d, want 3 (no further retries)", got)
	}
}

// TestTakeRetryDisabledByDefault: TakeRetry=0 (the zero value) registers
// nothing and a crNotAccepted reject schedules no retry — exactly the C++
// behavior.
func TestTakeRetryDisabledByDefault(t *testing.T) {
	shortenTakeRetryBackoff(t)
	retryOID := retryOrderID()
	n, cc := newTakeRetryNode(t, 0)

	if _, rerr := n.TakeOrder(TakeOrderParams{
		ID: orderIDString(retryOID), FromAddress: btcAddr, ToAddress: btcAddr2,
	}); rerr != nil {
		t.Fatalf("initial take: %v", rerr)
	}
	takeRetryReject(t, n, crNotAccepted)
	waitForTakeRetry(t, 500*time.Millisecond, func() bool { return len(cc.snapshot()) >= 2 })
	if got := len(cc.snapshot()); got != 1 {
		t.Fatalf("packets after reject with retry disabled = %d, want 1", got)
	}
	if st := takeRetryEntry(n); st != nil {
		t.Fatalf("retry entry with retry disabled = %+v, want none", st)
	}
}

// TestTakeRetryDroppedOnDeterministicReject: a deterministic reject (bad
// taker utxo, reason 4) drops the pending retry entry — retrying a
// deterministic failure would repeat it.
func TestTakeRetryDroppedOnDeterministicReject(t *testing.T) {
	shortenTakeRetryBackoff(t)
	retryOID := retryOrderID()
	n, cc := newTakeRetryNode(t, 2)

	if _, rerr := n.TakeOrder(TakeOrderParams{
		ID: orderIDString(retryOID), FromAddress: btcAddr, ToAddress: btcAddr2,
	}); rerr != nil {
		t.Fatalf("initial take: %v", rerr)
	}
	takeRetryReject(t, n, crBadUtxo)
	if st := takeRetryEntry(n); st != nil {
		t.Fatalf("retry entry after deterministic reject = %+v, want dropped", st)
	}
	waitForTakeRetry(t, 2*time.Second, func() bool { return len(cc.snapshot()) >= 2 })
	if got := len(cc.snapshot()); got != 1 {
		t.Fatalf("packets after deterministic reject = %d, want 1 (no retry)", got)
	}
}

// TestTakeRetrySkippedWhenOrderCancelled: a remote cancel between the reject
// and the backoff timer drops the retry entry synchronously (handleRemoteCancel
// -> dropTakeRetry), so the timer later fires against a missing entry and
// broadcasts nothing.
func TestTakeRetrySkippedWhenOrderCancelled(t *testing.T) {
	shortenTakeRetryBackoff(t)
	retryOID := retryOrderID()
	n, cc := newTakeRetryNode(t, 2)

	if _, rerr := n.TakeOrder(TakeOrderParams{
		ID: orderIDString(retryOID), FromAddress: btcAddr, ToAddress: btcAddr2,
	}); rerr != nil {
		t.Fatalf("initial take: %v", rerr)
	}
	takeRetryReject(t, n, crNotAccepted)
	// Cancel (hub-signed) before the retry timer fires.
	body := &proto.CancelBody{ID: retryOrderID(), Reason: uint32(crTimeout)}
	cpkt := signBodyPacket(t, proto.XbcTransactionCancel, body.Marshal(), retryHubPriv())
	n.submit(func() { n.handleRemoteCancel(cpkt, body) }, true)
	if st := takeRetryEntry(n); st != nil {
		t.Fatalf("retry entry after remote cancel = %+v, want dropped", st)
	}
	waitForTakeRetry(t, 2*time.Second, func() bool { return len(cc.snapshot()) >= 2 })
	if got := len(cc.snapshot()); got != 1 {
		t.Fatalf("packets after cancelled order retry window = %d, want 1", got)
	}
}

// TestTakeRetryPrunedWhenOrderProgresses: an entry whose order advanced past
// the reject window (hold applied etc.) is pruned by the engine tick — a
// reject can no longer arrive for it.
func TestTakeRetryPrunedWhenOrderProgresses(t *testing.T) {
	shortenTakeRetryBackoff(t)
	retryOID := retryOrderID()
	n, _ := newTakeRetryNode(t, 2)

	if _, rerr := n.TakeOrder(TakeOrderParams{
		ID: orderIDString(retryOID), FromAddress: btcAddr, ToAddress: btcAddr2,
	}); rerr != nil {
		t.Fatalf("initial take: %v", rerr)
	}
	if st := takeRetryEntry(n); st == nil {
		t.Fatal("retry entry missing after commit")
	}
	// Simulate swap progression: the order leaves the accepting phase.
	n.store.Update(hexEncode(retryOID[:]), func(o *Order) { o.Status = "hold" })
	n.pruneTakeRetries()
	if st := takeRetryEntry(n); st != nil {
		t.Fatalf("retry entry after prune = %+v, want dropped", st)
	}
}

// TestTakeRetrySurvivesForgedCancel: a cancel that fails verification must NOT
// disarm the retry entry — drops happen only after the cancel is accepted.
// (A forged cancel for a known order ID must not kill the legitimate retry.)
func TestTakeRetrySurvivesForgedCancel(t *testing.T) {
	n := newCancelTestNode(nil)
	snodePriv := make([]byte, 32)
	snodePriv[0] = 0x51
	forgePriv := make([]byte, 32)
	forgePriv[0] = 0x52
	_, idHex := mustID(t)
	o := &Order{FromCurrency: "BTC", ToCurrency: "LTC", FromAmount: 1e6, ToAmount: 2e6,
		Status: "accepting", Role: 'B', Mine: true,
		SNodePubkey: hexPub(t, snodePriv)}
	o.ID = decodeID(t, idHex)
	n.store.Add(o)
	seedTakeRetryEntry(n, idHex)

	forged := signBodyPacket(t, proto.XbcTransactionCancel, (&proto.CancelBody{ID: o.ID, Reason: 1}).Marshal(), forgePriv)
	n.handleRemoteCancel(forged, &proto.CancelBody{ID: o.ID, Reason: 1})

	if !takeRetryEntryPresent(n, idHex) {
		t.Fatal("forged (unverified) cancel disarmed the retry entry: drops must happen only after acceptance")
	}
	if got := n.store.Get(idHex); got == nil || got.Status != "accepting" {
		t.Fatal("forged cancel must not touch the order")
	}
}

// TestLocalCancelDropsTakeRetry: our own dxCancelOrder disarms the retry entry
// once the cancel commits — the swap is over, no retry may fire afterwards.
func TestLocalCancelDropsTakeRetry(t *testing.T) {
	reg, _ := runningHub(t)
	n, _ := newHubNode(reg)
	o, rerr := n.MakeOrder(MakeOrderParams{
		Type: "partial", AutoSplit: true,
		Maker: "BTC", MakerSize: "2.5", MinSize: "1.0", MakerAddress: btcAddr,
		Taker: "SYS", TakerSize: "0.3", TakerAddress: btcAddr2,
	})
	if rerr != nil {
		t.Fatalf("MakeOrder: %v", rerr)
	}
	idHex := hexEncode(o.ID[:])
	seedTakeRetryEntry(n, idHex)

	if res, rerr := n.CancelOrder(CancelOrderParams{ID: idHex}); rerr != nil {
		t.Fatalf("CancelOrder: %v", rerr)
	} else if res.Status != "canceled" {
		t.Fatalf("status = %q, want canceled", res.Status)
	}
	if takeRetryEntryPresent(n, idHex) {
		t.Fatal("local cancel left the retry entry armed: a fired retry would re-take a canceled order")
	}
}

// TestTakeRetryBackoffEmptyListDoesNotPanic: an emptied backoff list (only a
// test override can do that) yields no delay instead of panicking on index -1.
func TestTakeRetryBackoffEmptyListDoesNotPanic(t *testing.T) {
	orig := takeRetryBackoff
	takeRetryBackoff = nil
	t.Cleanup(func() { takeRetryBackoff = orig })
	if got := takeRetryBackoffFor(1); got != 0 {
		t.Fatalf("backoff(1) with empty list = %v, want 0", got)
	}
	if got := takeRetryBackoffFor(99); got != 0 {
		t.Fatalf("backoff(99) with empty list = %v, want 0", got)
	}
}

// TestTakeRetryConfigReloadRace hammers entry operations against concurrent
// config swaps (what reloadConf does) under -race: every TakeRetry read must
// go through the config read lock, never a bare n.config read (engine and
// retry goroutines race hot-reload otherwise).
func TestTakeRetryConfigReloadRace(t *testing.T) {
	shortenTakeRetryBackoff(t)
	n, _ := newTakeRetryNode(t, 2)
	ids := []string{"race-a", "race-b", "race-c", "race-d"}
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				n.registerTakeRetry(id, TakeOrderParams{})
				n.scheduleTakeRetry(id)
				n.dropTakeRetry(id)
			}
		}(id)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			n.cfgMu.Lock()
			n.config = &Config{TakeRetry: 2}
			n.cfgMu.Unlock()
		}
	}()
	wg.Wait()
}

// ---------------------------------------------------------------------------
// The failure half of runTakeRetry (take_retry.go:187-228) and the branches no
// existing test reaches: backoff clamp, schedule-miss, not-open drop,
// both abandon arms, the pre-broadcast reschedule + cap chain, and the
// config-disabled drop.
// ---------------------------------------------------------------------------

// TestTakeRetryBackoffForClampsToLastEntry: an attempt past the end of the
// backoff list clamps to the last entry (take_retry.go:61) instead of
// indexing out of range.
func TestTakeRetryBackoffForClampsToLastEntry(t *testing.T) {
	orig := takeRetryBackoff
	takeRetryBackoff = []time.Duration{10 * time.Millisecond, 20 * time.Millisecond}
	t.Cleanup(func() { takeRetryBackoff = orig })
	if got := takeRetryBackoffFor(1); got != 10*time.Millisecond {
		t.Errorf("backoff(1) = %v, want 10ms", got)
	}
	if got := takeRetryBackoffFor(2); got != 20*time.Millisecond {
		t.Errorf("backoff(2) = %v, want 20ms", got)
	}
	if got := takeRetryBackoffFor(3); got != 20*time.Millisecond {
		t.Errorf("backoff(3) = %v, want 20ms (clamp to last entry)", got)
	}
	if got := takeRetryBackoffFor(99); got != 20*time.Millisecond {
		t.Errorf("backoff(99) = %v, want 20ms (clamp to last entry)", got)
	}
}

// TestScheduleTakeRetryUnknownOrderIsNoop: scheduling an id with no entry
// returns before arming a timer — no goroutine, no wire traffic, no entry
// created (take_retry.go:117-120).
func TestScheduleTakeRetryUnknownOrderIsNoop(t *testing.T) {
	shortenTakeRetryBackoff(t)
	n, cc := newTakeRetryNode(t, 2)
	n.scheduleTakeRetry("no-such-order-id")
	if takeRetryEntryPresent(n, "no-such-order-id") {
		t.Fatal("scheduling an unknown id must not create an entry")
	}
	// Long enough for any (buggy) 10ms timer to have fired by now.
	time.Sleep(50 * time.Millisecond)
	if got := len(cc.snapshot()); got != 0 {
		t.Errorf("packets = %d, want 0 (no timer may be armed)", got)
	}
}

// TestRunTakeRetrySkipsWhenOrderNotOpen covers both halves of the not-open
// branch (take_retry.go:169, :174-177): the order's status changed after the
// entry was armed, or the order is gone — either way the entry is dropped
// synchronously and TakeOrder never runs.
func TestRunTakeRetrySkipsWhenOrderNotOpen(t *testing.T) {
	shortenTakeRetryBackoff(t)
	n, cc := newTakeRetryNode(t, 2)
	oid := retryOrderID()
	idHex := hexEncode(oid[:])

	// (a) status no longer "open" (e.g. a remote cancel progressed it).
	seedTakeRetryEntry(n, idHex)
	if !n.store.Update(idHex, func(o *Order) { o.Status = "hold" }) {
		t.Fatal("seeded order not found")
	}
	n.runTakeRetry(idHex, TakeOrderParams{}, 1)
	if takeRetryEntry(n) != nil {
		t.Error("entry must be dropped when the order is no longer open")
	}
	if got := len(cc.snapshot()); got != 0 {
		t.Errorf("packets = %d, want 0 (TakeOrder must not run)", got)
	}
	if o := n.store.Get(idHex); o == nil {
		t.Error("order must remain in the store")
	} else if o.Status != "hold" {
		t.Errorf("order status = %q, want hold (untouched)", o.Status)
	}

	// (b) the order was removed entirely.
	seedTakeRetryEntry(n, idHex)
	n.store.Remove(idHex)
	n.runTakeRetry(idHex, TakeOrderParams{}, 1)
	if takeRetryEntry(n) != nil {
		t.Error("entry must be dropped when the order is gone")
	}
	if got := len(cc.snapshot()); got != 0 {
		t.Errorf("packets = %d, want 0 after order removal", got)
	}
}

// TestRunTakeRetryAbandonsOnMissingOrderID: runTakeRetry is keyed by idHex
// but TakeOrder looks up params.ID — a mismatch misses the lookup with 1021,
// which the abandon switch treats as "order no longer takeable": the entry is
// dropped and nothing is broadcast (take_retry.go:187-194).
func TestRunTakeRetryAbandonsOnMissingOrderID(t *testing.T) {
	shortenTakeRetryBackoff(t)
	n, cc := newTakeRetryNode(t, 2)
	oid := retryOrderID()
	idHex := hexEncode(oid[:])
	bad := TakeOrderParams{
		ID:          "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		FromAddress: btcAddr, ToAddress: btcAddr2,
	}

	// Pin the exact error the abandon switch will see.
	if _, rerr := n.TakeOrder(bad); rerr == nil {
		t.Fatal("TakeOrder with an unknown id must fail")
	} else {
		if rerr.Code != errTxNotFound {
			t.Fatalf("code = %d, want %d (errTxNotFound)", rerr.Code, errTxNotFound)
		}
		if rerr.Error != "Transaction  not found" {
			t.Errorf("error = %q, want %q", rerr.Error, "Transaction  not found")
		}
		if rerr.Name != "dxTakeOrder" {
			t.Errorf("name = %q, want dxTakeOrder", rerr.Name)
		}
	}

	seedTakeRetryEntry(n, idHex)
	n.runTakeRetry(idHex, bad, 1)
	if takeRetryEntry(n) != nil {
		t.Error("entry must be abandoned (dropped) on errTxNotFound")
	}
	if got := len(cc.snapshot()); got != 0 {
		t.Errorf("packets = %d, want 0", got)
	}
}

// TestRunTakeRetryAbandonsOnReservationBusy: the other abandon arm — the
// order is reserved for another taker, TakeOrder returns 1004 before any
// broadcast, and the retry gives up (take_retry.go:187-194).
func TestRunTakeRetryAbandonsOnReservationBusy(t *testing.T) {
	shortenTakeRetryBackoff(t)
	n, cc := newTakeRetryNode(t, 2)
	oid := retryOrderID()
	idHex := hexEncode(oid[:])
	params := TakeOrderParams{ID: orderIDString(oid), FromAddress: btcAddr, ToAddress: btcAddr2}

	// Hold the reservation as a concurrent taker would.
	if got := n.store.ReserveForTake(idHex, []string{"pre:0"}, nil, "BTC"); got != reserveOK {
		t.Fatalf("pre-reserve = %v, want reserveOK", got)
	}
	// Pin the error the abandon switch will see from the same call.
	if _, rerr := n.TakeOrder(params); rerr == nil {
		t.Fatal("TakeOrder against a reserved order must fail")
	} else if rerr.Code != errBadRequest {
		t.Fatalf("code = %d, want %d (errBadRequest)", rerr.Code, errBadRequest)
	}

	seedTakeRetryEntry(n, idHex)
	n.runTakeRetry(idHex, params, 1)
	if takeRetryEntry(n) != nil {
		t.Error("entry must be abandoned (dropped) on errBadRequest")
	}
	if got := len(cc.snapshot()); got != 0 {
		t.Errorf("packets = %d, want 0", got)
	}
}

// TestRunTakeRetryReschedulesAndCapsOnPreBroadcastFailure drives the whole
// failure half (take_retry.go:187 fallthrough, :197-228): a failure that never
// reaches the wire (same from/to address → 1025) reschedules on every attempt
// — attempts climb 0→1→2 under TakeRetry=2, the next failure sees the cap and
// drops the entry — and no packet is ever broadcast.
func TestRunTakeRetryReschedulesAndCapsOnPreBroadcastFailure(t *testing.T) {
	shortenTakeRetryBackoff(t)
	n, cc := newTakeRetryNode(t, 2)
	oid := retryOrderID()
	idHex := hexEncode(oid[:])
	bad := TakeOrderParams{ID: orderIDString(oid), FromAddress: btcAddr, ToAddress: btcAddr}

	// Pin the pre-broadcast failure the chain will keep hitting.
	if _, rerr := n.TakeOrder(bad); rerr == nil {
		t.Fatal("same-address take must fail")
	} else if rerr.Code != errInvalidParameters {
		t.Fatalf("code = %d, want %d (errInvalidParameters)", rerr.Code, errInvalidParameters)
	} else if want := "Invalid parameters: The from_address and to_address cannot be the same: " + btcAddr; rerr.Error != want {
		t.Errorf("error = %q, want %q", rerr.Error, want)
	}

	seedTakeRetryEntry(n, idHex)
	n.runTakeRetry(idHex, bad, 1) // synchronous attempt #1; attempts 0→1 arms a 10ms timer
	waitForTakeRetry(t, 2*time.Second, func() bool { return takeRetryEntry(n) == nil })
	if takeRetryEntry(n) != nil {
		t.Fatal("entry must be dropped once the cap is exhausted")
	}
	if got := len(cc.snapshot()); got != 0 {
		t.Errorf("packets = %d, want 0 — every failure was pre-broadcast", got)
	}
	if o := n.store.Get(idHex); o == nil || o.Status != "open" {
		t.Errorf("order must stay open, got %v", o)
	}
}

// TestRunTakeRetryDropsWhenConfigDisabledMidFlight: a config reload turning
// TakeRetry off after the entry is armed makes the failure path drop the entry
// at the max<=0 check instead of rescheduling (take_retry.go:197-210).
func TestRunTakeRetryDropsWhenConfigDisabledMidFlight(t *testing.T) {
	shortenTakeRetryBackoff(t)
	n, cc := newTakeRetryNode(t, 2)
	oid := retryOrderID()
	idHex := hexEncode(oid[:])
	bad := TakeOrderParams{ID: orderIDString(oid), FromAddress: btcAddr, ToAddress: btcAddr}

	seedTakeRetryEntry(n, idHex)
	n.cfgMu.Lock()
	n.config.TakeRetry = 0
	n.cfgMu.Unlock()

	n.runTakeRetry(idHex, bad, 1)
	if takeRetryEntry(n) != nil {
		t.Error("entry must be dropped when max <= 0")
	}
	if got := len(cc.snapshot()); got != 0 {
		t.Errorf("packets = %d, want 0", got)
	}
}
