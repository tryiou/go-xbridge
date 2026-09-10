package api

import (
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"go-xbridge/coins"
	"go-xbridge/config"
	"go-xbridge/crypto"
	"go-xbridge/proto"
	"go-xbridge/wallet"
)

// setupTwoCoinNode registers BTC/LTC from conf and returns a started node with
// a gated BTC connector (SendRawTransaction parks until open()) and a free LTC
// connector with a funding UTXO so a CreateA deposit can be built and broadcast.
func setupTwoCoinNode(t *testing.T) (*Node, *captureXConn, *gatedConnector, *fakeConnector) {
	t.Helper()
	if err := coins.InitFromConf(map[string]*config.CoinConf{
		"BTC": {Ticker: "BTC", Coin: 1e8, AddressPrefix: 0, CreateTxMethod: "BTC", BlockTime: 60},
		"LTC": {Ticker: "LTC", Coin: 1e8, AddressPrefix: 48, CreateTxMethod: "LTC", BlockTime: 60},
	}); err != nil {
		t.Fatal(err)
	}
	btcPriv, btcPub := newKey(t)
	btcFunding := wallet.Utxo{TxID: strings.Repeat("aa", 32), Vout: 0, Amount: 5e8,
		ScriptPubKey: hex.EncodeToString(coins.BuildP2PKHScript(coins.KeyID(btcPub)))}
	btc := &fakeConnector{ticker: "BTC", funding: btcFunding, fundingPriv: btcPriv, fundingPub: btcPub,
		changeAddr: addrFor(0, "btc-change"), blockHeight: 1000, rawTx: map[string]string{}}
	gated := newGatedConnector(btc)
	ltcPriv, ltcPub := newKey(t)
	ltcFunding := wallet.Utxo{TxID: strings.Repeat("bb", 32), Vout: 0, Amount: 5e8,
		ScriptPubKey: hex.EncodeToString(coins.BuildP2PKHScript(coins.KeyID(ltcPub)))}
	ltc := &fakeConnector{ticker: "LTC", funding: ltcFunding, fundingPriv: ltcPriv, fundingPub: ltcPub,
		changeAddr: addrFor(48, "ltc-change"), blockHeight: 1000, rawTx: map[string]string{}}
	confs := map[string]*config.CoinConf{
		"BTC": {Ticker: "BTC", Coin: 1e8, AddressPrefix: 0, CreateTxMethod: "BTC", BlockTime: 60},
		"LTC": {Ticker: "LTC", Coin: 1e8, AddressPrefix: 48, CreateTxMethod: "LTC", BlockTime: 60},
	}
	n, cc := newStartedNode(t, confs, map[string]wallet.Connector{"BTC": gated, "LTC": ltc})
	return n, cc, gated, ltc
}

// TestConcurrentRefundSweepAndDepositTask. The refund sweep
// (scanRefunds, engine-owned) reads session A's await/refundHex/state while A's
// deposit resume writes them, and posts session B's due pre-signed refund to a
// worker. -race is the pass criteria; the broadcast-count invariants prove
// correctness, not just absence of a crash.
func TestConcurrentRefundSweepAndDepositTask(t *testing.T) {
	n, cc, gated, ltc := setupTwoCoinNode(t)
	defer gated.open() // runs before t.Cleanup's Close, so shutdown can always drain

	// Session A (maker, BTC): CreateA deposit parked on the gated connector.
	mPriv, mPub := newKey(t)
	var aID [32]byte
	copy(aID[:], []byte("concurrent-refund-a-order-000"))
	n.newMakerSession(withUsedCoins(t, n, &Order{ID: aID, FromCurrency: "BTC", ToCurrency: "BTC", FromAmount: 2.5e6, ToAmount: 2.5e6}, []wallet.Utxo{gated.funding}),
		MakeOrderParams{MakerAddress: addrFor(0, "maker-a"), TakerAddress: addrFor(0, "taker-a")}, arr32(mPriv), toArr33(mPub))
	n.submit(func() {
		if _, _, err := n.sessions[hexEncode(aID[:])].OnCreateA(&proto.CreateABody{ID: aID, BPubKey: to33(mPub)}); err != nil {
			t.Errorf("OnCreateA stage 1: %v", err)
		}
	}, true)
	deadline := time.Now().Add(5 * time.Second)
	for gated.callCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if gated.callCount() != 1 {
		t.Fatalf("deposit task never reached SendRawTransaction (calls=%d)", gated.callCount())
	}

	// Session B (maker, LTC): pre-signed refund already due (lockTime 1 < 1000).
	var bID [32]byte
	copy(bID[:], []byte("concurrent-refund-b-order-000"))
	rtx := &coins.Tx{Version: 1}
	rtx.Inputs = []coins.TxIn{{PrevOut: coins.OutPoint{Hash: mustHash(strings.Repeat("ab", 32)), Index: 0}, Sequence: 0xfffffffe}}
	rtx.Outputs = []coins.TxOut{{Value: 1, ScriptPubKey: []byte{0x51}}}
	n.submit(func() {
		n.sessions[hexEncode(bID[:])] = &SwapSession{
			n: n, id: bID, isMaker: false, srcCur: "LTC", dstCur: "BTC",
			refundHex: hex.EncodeToString(rtx.Serialize()), refundDone: false,
			ourLockTime: 1, state: csCreatedA,
		}
	}, true)

	// Hammer the sweep from 4 goroutines while A's deposit is still in flight.
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				n.submit(func() { n.scanRefunds() }, false)
			}
		}()
	}

	deadline = time.Now().Add(5 * time.Second)
	for len(ltc.broadcastSnapshot()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := ltc.broadcastSnapshot(); len(got) != 1 {
		t.Fatalf("refund broadcast %d times, want 1", len(got))
	}

	gated.open()
	pkt := waitForPacket(t, cc, proto.XbcTransactionCreatedA, 5*time.Second)
	body, err := proto.DecodeBody(pkt.Command, pkt.Body)
	if err != nil {
		t.Fatalf("decode CreatedA: %v", err)
	}
	createdA, ok := body.(*proto.CreatedABody)
	if !ok {
		t.Fatalf("response body = %T, want *proto.CreatedABody", body)
	}
	if createdA.ADepositTxID == "" {
		t.Fatal("empty ADepositTxID in response")
	}
	wg.Wait()

	if got := gated.broadcastSnapshot(); len(got) != 1 {
		t.Fatalf("deposit broadcast %d times, want 1", len(got))
	}
	if got := readOnEngine(t, n, func() string { return n.sessions[hexEncode(aID[:])].ourDepositTxID }); got != createdA.ADepositTxID {
		t.Errorf("A ourDepositTxID = %q, want %q", got, createdA.ADepositTxID)
	}
	if got := readOnEngine(t, n, func() bool { return n.sessions[hexEncode(aID[:])].await }); got {
		t.Error("A await still set after resume")
	}
	if got := readOnEngine(t, n, func() bool { return n.sessions[hexEncode(bID[:])] != nil }); got {
		t.Error("B session should have been pruned after its refund broadcast")
	}
	if got := ltc.broadcastSnapshot(); len(got) != 1 {
		t.Fatalf("refund broadcast %d times after more sweeps, want 1", len(got))
	}
}

// TestForceRefundTakesSweepGuard. A force-refund (enqueueRefund,
// e.g. from CancelOrder/BroadcastRefund) must take the pendingRefunds guard so
// the sweep (scanRefunds) cannot enqueue a second broadcast of the same refund
// hex while the force-refund is in flight. With the force task parked in the
// wallet, the sweep must skip the order; on the old code it enqueues a second
// broadcast (SendRawTransaction called twice for one refund).
func TestForceRefundTakesSweepGuard(t *testing.T) {
	n, _, gated, _ := setupTwoCoinNode(t)
	defer gated.open() // runs before t.Cleanup's Close, so shutdown can always drain

	var rID [32]byte
	copy(rID[:], []byte("force-refund-guard-order-000"))
	rtx := &coins.Tx{Version: 1}
	rtx.Inputs = []coins.TxIn{{PrevOut: coins.OutPoint{Hash: mustHash(strings.Repeat("ab", 32)), Index: 0}, Sequence: 0xfffffffe}}
	rtx.Outputs = []coins.TxOut{{Value: 1, ScriptPubKey: []byte{0x51}}}
	n.submit(func() {
		n.sessions[hexEncode(rID[:])] = &SwapSession{
			n: n, id: rID, isMaker: false, srcCur: "BTC", dstCur: "LTC",
			refundHex: hex.EncodeToString(rtx.Serialize()), refundDone: false,
			ourLockTime: 0, state: csCreatedB,
		}
	}, true)

	// Force-refund: the task parks in SendRawTransaction (gate held shut).
	n.submit(func() { n.enqueueRefund(hexEncode(rID[:]), nil) }, true)
	deadline := time.Now().Add(5 * time.Second)
	for gated.callCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if gated.callCount() != 1 {
		t.Fatalf("force-refund never reached SendRawTransaction (calls=%d)", gated.callCount())
	}

	// The sweep runs while the force-refund is still in flight. With the
	// pendingRefunds guard set, it must skip this order — exactly one broadcast
	// attempt total.
	n.submit(func() { n.scanRefunds() }, true)
	deadline = time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		if c := gated.callCount(); c != 1 {
			t.Fatalf("sweep double-enqueued the refund while force-refund in flight: %d SendRawTransaction calls, want 1", c)
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Release the gate: the force-refund lands, marks the session refundDone,
	// and the sweep (guard now cleared) still broadcasts nothing new.
	gated.open()
	deadline = time.Now().Add(5 * time.Second)
	for !readOnEngine(t, n, func() bool { return n.sessions[hexEncode(rID[:])] == nil }) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := gated.broadcastSnapshot(); len(got) != 1 {
		t.Fatalf("refund broadcast %d times, want 1", len(got))
	}
	if readOnEngine(t, n, func() bool { return n.sessions[hexEncode(rID[:])] != nil }) {
		t.Error("session should have been pruned after its refund broadcast")
	}
}

// TestEngineLivenessSlowWallet proves a parked wallet on one coin never stalls
// the engine or other sessions: session B's full handshake completes while A's
// deposit is still blocked in SendRawTransaction.
func TestEngineLivenessSlowWallet(t *testing.T) {
	n, cc, gated, ltc := setupTwoCoinNode(t)
	defer gated.open()

	mPriv, mPub := newKey(t)
	var aID [32]byte
	copy(aID[:], []byte("liveness-a-order-00000000000000"))
	n.newMakerSession(withUsedCoins(t, n, &Order{ID: aID, FromCurrency: "BTC", ToCurrency: "BTC", FromAmount: 2.5e6, ToAmount: 2.5e6}, []wallet.Utxo{gated.funding}),
		MakeOrderParams{MakerAddress: addrFor(0, "maker-a"), TakerAddress: addrFor(0, "taker-a")}, arr32(mPriv), toArr33(mPub))
	n.submit(func() {
		if _, _, err := n.sessions[hexEncode(aID[:])].OnCreateA(&proto.CreateABody{ID: aID, BPubKey: to33(mPub)}); err != nil {
			t.Errorf("A OnCreateA: %v", err)
		}
	}, true)
	deadline := time.Now().Add(5 * time.Second)
	for gated.callCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if gated.callCount() != 1 {
		t.Fatalf("A deposit never reached the wallet (calls=%d)", gated.callCount())
	}

	_, bPub := newKey(t)
	var bID [32]byte
	copy(bID[:], []byte("liveness-b-order-00000000000000"))
	n.newMakerSession(withUsedCoins(t, n, &Order{ID: bID, FromCurrency: "LTC", ToCurrency: "LTC", FromAmount: 2.5e6, ToAmount: 2.5e6}, []wallet.Utxo{ltc.funding}),
		MakeOrderParams{MakerAddress: addrFor(48, "maker-b"), TakerAddress: addrFor(48, "taker-b")}, arr32(mPriv), toArr33(bPub))
	n.submit(func() {
		if _, _, err := n.sessions[hexEncode(bID[:])].OnCreateA(&proto.CreateABody{ID: bID, BPubKey: to33(bPub)}); err != nil {
			t.Errorf("B OnCreateA: %v", err)
		}
	}, true)

	// The only CreatedA that can arrive is B's — A's is deferred behind the
	// parked wallet task (engineWorkers=4 leaves other workers free for B).
	waitForPacket(t, cc, proto.XbcTransactionCreatedA, 5*time.Second)
	if got := ltc.broadcastSnapshot(); len(got) != 1 {
		t.Fatalf("LTC deposit broadcast %d times, want 1", len(got))
	}
	if got := gated.callCount(); got != 1 {
		t.Fatalf("A wallet called %d times, want 1 (still parked)", got)
	}
	if got := readOnEngine(t, n, func() bool { return n.sessions[hexEncode(bID[:])].await }); got {
		t.Error("B await still set after its CreatedA")
	}
	if got := readOnEngine(t, n, func() bool { return n.sessions[hexEncode(aID[:])].await }); !got {
		t.Error("A await cleared while its deposit is still parked")
	}
}

// TestCloseDrainsInFlightTask locks in the shutdown-join order: Close must block
// until an in-flight wallet task completes (wg.Wait), then return cleanly.
// Non-vacuous: it asserts Close has NOT returned while the task is parked, then
// asserts it returns promptly once the gate opens.
func TestCloseDrainsInFlightTask(t *testing.T) {
	n, _, gated, _ := setupTwoCoinNode(t)
	defer gated.open() // on failure, lets t.Cleanup's Close drain instead of hanging

	mPriv, mPub := newKey(t)
	var aID [32]byte
	copy(aID[:], []byte("shutdown-a-order-0000000000000000"))
	n.newMakerSession(withUsedCoins(t, n, &Order{ID: aID, FromCurrency: "BTC", ToCurrency: "BTC", FromAmount: 2.5e6, ToAmount: 2.5e6}, []wallet.Utxo{gated.funding}),
		MakeOrderParams{MakerAddress: addrFor(0, "maker-a"), TakerAddress: addrFor(0, "taker-a")}, arr32(mPriv), toArr33(mPub))
	n.submit(func() {
		if _, _, err := n.sessions[hexEncode(aID[:])].OnCreateA(&proto.CreateABody{ID: aID, BPubKey: to33(mPub)}); err != nil {
			t.Errorf("OnCreateA: %v", err)
		}
	}, true)
	deadline := time.Now().Add(5 * time.Second)
	for gated.callCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if gated.callCount() != 1 {
		t.Fatalf("deposit task never reached the wallet (calls=%d)", gated.callCount())
	}

	done := make(chan error, 1)
	go func() { done <- n.Close() }()
	select {
	case err := <-done:
		t.Fatalf("Close returned while task in flight: %v", err)
	case <-time.After(300 * time.Millisecond):
	}

	gated.open()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return after the in-flight task drained")
	}
}

// TestConcurrentTakeOrderSingleSession — no-fork proof. Eight HTTP-style
// goroutines take the same open order concurrently; the engine serializes the
// taker-session registration so exactly one session survives, the book stays
// consistent, and no Accepting packet is lost.
func TestConcurrentTakeOrderSingleSession(t *testing.T) {
	const takes = 8
	if err := coins.InitFromConf(map[string]*config.CoinConf{
		"BTC":   {Ticker: "BTC", Coin: 1e8, AddressPrefix: 0, CreateTxMethod: "BTC", BlockTime: 60},
		"BLOCK": {Ticker: "BLOCK", Coin: 1e8, AddressPrefix: 0, CreateTxMethod: "BTC", BlockTime: 60, TxVersion: 1},
	}); err != nil {
		t.Fatal(err)
	}
	// The taker's funding wallet must clear the take pre-checks: eight 300 BTC
	// utxos cover the 100-BTC order (selectUtxos gt path) one per concurrent
	// take, and the BLOCK connector holds eight distinct 1.0 BLOCK p2pkh utxos
	// so each take can reserve its own service-node fee funder (take #1 locks
	// its selections; a shared set starves takes 2..8).
	var funders []wallet.Utxo
	for i := 0; i < takes; i++ {
		funders = append(funders, wallet.Utxo{
			TxID: fmt.Sprintf("%064d", i+1), Vout: 0,
			Amount: 30000000000, Value: 300.0,
			ScriptPubKey: "76a914000000000000000000000000000000000000000088ac",
			Address:      addrFor(0, fmt.Sprintf("take-funding-%d", i)),
		})
	}
	conn := &fakeConnector{
		ticker: "BTC", blockHeight: 1000, rawTx: map[string]string{},
		funders: funders,
	}
	blkUtxos := make([]wallet.Utxo, 0, takes)
	for i := 0; i < takes; i++ {
		u := blkUtxo()
		u.TxID = fmt.Sprintf("%064d", 0x100+i)
		blkUtxos = append(blkUtxos, u)
	}
	n, cc := newStartedNode(t, map[string]*config.CoinConf{
		"BTC":   {Ticker: "BTC", Coin: 1e8, AddressPrefix: 0, CreateTxMethod: "BTC", BlockTime: 60},
		"BLOCK": {Ticker: "BLOCK", Coin: 1e8, AddressPrefix: 0, CreateTxMethod: "BTC", BlockTime: 60, TxVersion: 1},
	}, map[string]wallet.Connector{
		"BTC":   conn,
		"BLOCK": &stubConn{ticker: "BLOCK", addr: btcAddr, utxos: blkUtxos},
	})

	hubPriv := make([]byte, 32)
	hubPriv[31] = 2
	hubPub, err := crypto.CompressedPubKey(hubPriv)
	if err != nil {
		t.Fatal(err)
	}
	registerHub(t, n, hubPriv)

	var oid [32]byte
	copy(oid[:], []byte("concurrent-take-order-0000000000"))
	o := &Order{
		ID: oid, FromCurrency: "BTC", ToCurrency: "BTC", FromAmount: 2.5e6, ToAmount: 2.5e6,
		Status: "open", SNodePubkey: hex.EncodeToString(hubPub[:]), HubAddress: coins.KeyID(hubPub[:]),
	}
	n.store.Add(o)

	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	loses := 0
	for i := 0; i < takes; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, rerr := n.TakeOrder(TakeOrderParams{
				ID:          orderIDString(oid),
				FromAddress: addrFor(0, fmt.Sprintf("take-from-%d", i)),
				ToAddress:   addrFor(0, fmt.Sprintf("take-to-%d", i)),
			})
			mu.Lock()
			defer mu.Unlock()
			if rerr == nil {
				wins++
				return
			}
			// A losing raft always fails on the atomic reservation, never on a
			// partial-wallet error: the selection windows are over the full pool
			// and the reservation is the one exclusion point that serializes
			// concurrent takes. A same-order loser is refused by the in-flight
			// gate with BAD_REQUEST ("not accepting, order already accepted", C++
			// xbridgeapp.cpp:2122-2125); a distinct-order loser still collides on
			// a shared key with INSUFFICIENT_FUNDS ("cannot reuse utxo inputs").
			// A stale locked-set snapshot (node.go:1428) can also route a
			// same-order loser to the collision path, so accept either code.
			// A loser snapshotting after the winner's commit sees Mine and gets
			// 1025 "own order" — matching C++ (accept stamps from/to at
			// xbridgeapp.cpp:2376-2380, so isLocal hits first at
			// rpcxbridge.cpp:1211). Accept only this exact text.
			if rerr.Code != errBadRequest && rerr.Code != errInsufficientFunds &&
				(rerr.Code != errInvalidParameters || !strings.Contains(rerr.Error, "Unable to accept your own order")) {
				t.Errorf("TakeOrder %d: unexpected error %v", i, rerr)
			} else {
				loses++
			}
		}(i)
	}
	wg.Wait()

	if wins < 1 {
		t.Fatalf("no take succeeded (wins=%d)", wins)
	}
	if wins+loses != takes {
		t.Fatalf("outcomes = %d, want %d takes accounted for", wins+loses, takes)
	}
	// The engine serializes the tails, so exactly one taker session survives
	// regardless of how many acceptings raced to the wire (no fork).
	if got := readOnEngine(t, n, func() int { return len(n.sessions) }); got != 1 {
		t.Fatalf("sessions = %d, want 1 (no fork)", got)
	}
	if got := n.store.Get(hexEncode(oid[:])); got == nil || got.Status != "accepting" {
		t.Fatalf("order status = %v, want accepting", got)
	}
	// Every take that won broadcasts exactly one Accepting packet; a loser
	// fails at the reservation BEFORE any packet leaves.
	if got := len(cc.snapshot()); got != wins {
		t.Fatalf("Accepting packets written = %d, want %d (one per winning take)", got, wins)
	}
}

// TestConcurrentTakeOrderDistinctOrdersExclusiveReservation.
// Eight concurrent takes of eight DISTINCT orders draw from one SHARED scarce
// pool (3 funding utxos + 3 BLOCK fee utxos). The atomic reservation
// (ReserveForTake) must guarantee no two orders ever claim the same key, so
// the surviving acceptings' inputs are pairwise disjoint, at most 3 takes can
// win, and every loser fails with INSUFFICIENT_FUNDS before broadcasting.
func TestConcurrentTakeOrderDistinctOrdersExclusiveReservation(t *testing.T) {
	const (
		takes = 8 // concurrent takes
		pool  = 3 // shared scarce utxos per currency
	)
	if err := coins.InitFromConf(map[string]*config.CoinConf{
		"BTC":   {Ticker: "BTC", Coin: 1e8, AddressPrefix: 0, CreateTxMethod: "BTC", BlockTime: 60},
		"BLOCK": {Ticker: "BLOCK", Coin: 1e8, AddressPrefix: 0, CreateTxMethod: "BTC", BlockTime: 60, TxVersion: 1},
	}); err != nil {
		t.Fatal(err)
	}
	var funders []wallet.Utxo
	for i := 0; i < pool; i++ {
		funders = append(funders, wallet.Utxo{
			TxID: fmt.Sprintf("%064d", i+1), Vout: 0,
			Amount: 30000000000, Value: 300.0,
			ScriptPubKey: "76a914000000000000000000000000000000000000000088ac",
			Address:      addrFor(0, fmt.Sprintf("shared-funding-%d", i)),
		})
	}
	conn := &fakeConnector{
		ticker: "BTC", blockHeight: 1000, rawTx: map[string]string{},
		funders: funders,
	}
	blkUtxos := make([]wallet.Utxo, 0, pool)
	for i := 0; i < pool; i++ {
		u := blkUtxo()
		u.TxID = fmt.Sprintf("%064d", 0x100+i)
		blkUtxos = append(blkUtxos, u)
	}
	n, cc := newStartedNode(t, map[string]*config.CoinConf{
		"BTC":   {Ticker: "BTC", Coin: 1e8, AddressPrefix: 0, CreateTxMethod: "BTC", BlockTime: 60},
		"BLOCK": {Ticker: "BLOCK", Coin: 1e8, AddressPrefix: 0, CreateTxMethod: "BTC", BlockTime: 60, TxVersion: 1},
	}, map[string]wallet.Connector{
		"BTC":   conn,
		"BLOCK": &stubConn{ticker: "BLOCK", addr: btcAddr, utxos: blkUtxos},
	})

	hubPriv := make([]byte, 32)
	hubPriv[31] = 2
	registerHub(t, n, hubPriv)
	hubPub, err := crypto.CompressedPubKey(hubPriv)
	if err != nil {
		t.Fatal(err)
	}

	var oids [takes][32]byte
	for i := 0; i < takes; i++ {
		var oid [32]byte
		copy(oid[:], []byte(fmt.Sprintf("exclusive-order-%02d-0000000000", i)))
		oids[i] = oid
		o := &Order{
			ID: oid, FromCurrency: "BTC", ToCurrency: "BTC", FromAmount: 2.5e6, ToAmount: 2.5e6,
			Status: "open", SNodePubkey: hex.EncodeToString(hubPub[:]), HubAddress: coins.KeyID(hubPub[:]),
		}
		n.store.Add(o)
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := map[int]bool{}
	loses := map[int]bool{}
	for i := 0; i < takes; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, rerr := n.TakeOrder(TakeOrderParams{
				ID:          orderIDString(oids[i]),
				FromAddress: addrFor(0, fmt.Sprintf("take-from-%d", i)),
				ToAddress:   addrFor(0, fmt.Sprintf("take-to-%d", i)),
			})
			mu.Lock()
			defer mu.Unlock()
			if rerr == nil {
				wins[i] = true
			} else if rerr.Code == errInsufficientFunds {
				loses[i] = true
			} else {
				t.Errorf("TakeOrder %d: unexpected error %v", i, rerr)
			}
		}(i)
	}
	wg.Wait()

	if len(wins) < 1 {
		t.Fatal("no take succeeded")
	}
	if len(wins) > pool {
		t.Fatalf("wins = %d, want at most %d with a shared pool of %d", len(wins), pool, pool)
	}
	if len(loses) != takes-len(wins) {
		t.Fatalf("losers = %d, want %d", len(loses), takes-len(wins))
	}
	if len(cc.snapshot()) != len(wins) {
		t.Fatalf("Accepting packets = %d, want %d (losers must not broadcast)", len(cc.snapshot()), len(wins))
	}

	// The core guarantee: across ALL surviving acceptings, no "txid:vout" key
	// is shared by two different orders. Winners carry exactly one funding and
	// one fee utxo, so a collision-free union of size 2*|wins| proves the
	// exclusion worked (Fixing the TOCTOU, each survivor claimed unique inputs).
	seen := map[string]string{}
	for _, o := range n.store.List() {
		if o.Status != "accepting" {
			continue
		}
		if len(o.Utxos) != 1 || len(o.FeeUtxos) != 1 {
			t.Fatalf("accepted order %s has %d utxos / %d fee utxos, want 1/1",
				orderIDString(o.ID), len(o.Utxos), len(o.FeeUtxos))
		}
		owner := orderIDString(o.ID)
		for _, u := range o.Utxos {
			k := utxoEntryKey(u)
			if prev, ok := seen[k]; ok && prev != owner {
				t.Fatalf("funding key %s claimed by both %s and %s (double-reserve)", k, prev, owner)
			}
			seen[k] = owner
		}
		for _, u := range o.FeeUtxos {
			k := u.TxID + ":" + fmt.Sprint(u.Vout)
			if prev, ok := seen[k]; ok && prev != owner {
				t.Fatalf("fee key %s claimed by both %s and %s (double-reserve)", k, prev, owner)
			}
			seen[k] = owner
		}
	}
	if got := len(seen); got != 2*len(wins) {
		t.Fatalf("reserved key count = %d, want %d (each winner 1 funding + 1 fee)", got, 2*len(wins))
	}
}

// TestConcurrentCancelOrderSingleSession — the cancel counterpart of the take
// no-fork proof. Eight HTTP-style goroutines cancel the same order with a live
// session; the engine serializes the tails so the book ends consistent and no
// session fork/leak occurs. NOTE: CancelOrder takes the RAW store key (the RPC
// handler normalizes via orderIDKey before calling it), unlike TakeOrder which
// normalizes internally.
func TestConcurrentCancelOrderSingleSession(t *testing.T) {
	if err := coins.InitFromConf(map[string]*config.CoinConf{
		"BTC": {Ticker: "BTC", Coin: 1e8, AddressPrefix: 0, CreateTxMethod: "BTC", BlockTime: 60},
	}); err != nil {
		t.Fatal(err)
	}
	conn := &fakeConnector{ticker: "BTC", blockHeight: 1000}
	n, cc := newStartedNode(t, map[string]*config.CoinConf{
		"BTC": {Ticker: "BTC", Coin: 1e8, AddressPrefix: 0, CreateTxMethod: "BTC", BlockTime: 60},
	}, map[string]wallet.Connector{"BTC": conn})

	mPriv, mPub := newKey(t)
	var oid [32]byte
	copy(oid[:], []byte("concurrent-cancel-order-00000000"))
	o := &Order{
		ID: oid, FromCurrency: "BTC", ToCurrency: "BTC", FromAmount: 2.5e6, ToAmount: 2.5e6,
		Status: "open", Mine: true, // a local maker order is cancellable (C++ isLocal)
	}
	n.store.Add(o)
	n.newMakerSession(o, MakeOrderParams{MakerAddress: addrFor(0, "maker-c"), TakerAddress: addrFor(0, "taker-c")}, arr32(mPriv), toArr33(mPub))

	const cancels = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok, dup := 0, 0
	for i := 0; i < cancels; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, rerr := n.CancelOrder(CancelOrderParams{ID: hexEncode(oid[:])})
			mu.Lock()
			defer mu.Unlock()
			if rerr == nil {
				ok++
				return
			}
			// C++ cancelXBridgeTransaction rejects a re-cancel with
			// INVALID_STATE (xbridgeapp.cpp:2481-2486): the engine serializes
			// the cancels, so the losers see the order already canceled.
			if rerr.Code == errInvalidState {
				dup++
				return
			}
			t.Errorf("CancelOrder: %v", rerr)
		}()
	}
	wg.Wait()
	if ok != 1 || dup != cancels-1 {
		t.Fatalf("cancel outcomes: %d ok / %d already-canceled, want 1 / %d", ok, dup, cancels-1)
	}

	// Engine-serialized tails: exactly one session survives (no fork/leak).
	if got := readOnEngine(t, n, func() int { return len(n.sessions) }); got != 1 {
		t.Fatalf("sessions = %d, want 1 (no fork)", got)
	}
	if got := n.store.Get(hexEncode(oid[:])); got == nil || got.Status != "canceled" {
		t.Fatalf("order status = %v, want canceled", got)
	}
	// The serialized engine writes exactly one cancel packet — the single
	// cancel that passed the state gate (C++ sends one per successful
	// cancelXBridgeTransaction).
	if got := len(cc.snapshot()); got != 1 {
		t.Fatalf("cancel packets written = %d, want 1", got)
	}
}

// TestCloseDrainsInFlightRefundTask locks in the shutdown-join order for a
// refund sweep task parked in the wallet: Close must block (wg.Wait) until the
// refund broadcast completes, then return cleanly. The parked task here is a
// refund-sweep task, not a deposit task.
func TestCloseDrainsInFlightRefundTask(t *testing.T) {
	n, _, gated, _ := setupTwoCoinNode(t)
	defer gated.open() // on failure, lets t.Cleanup's Close drain instead of hanging

	// A session with a due pre-signed refund (lockTime 1 < height 1000) on the
	// gated coin, so the sweep posts a refund task that parks in SendRawTransaction.
	var rID [32]byte
	copy(rID[:], []byte("shutdown-refund-order-0000000000"))
	rtx := &coins.Tx{Version: 1}
	rtx.Inputs = []coins.TxIn{{PrevOut: coins.OutPoint{Hash: mustHash(strings.Repeat("cd", 32)), Index: 0}, Sequence: 0xfffffffe}}
	rtx.Outputs = []coins.TxOut{{Value: 1, ScriptPubKey: []byte{0x51}}}
	n.submit(func() {
		n.sessions[hexEncode(rID[:])] = &SwapSession{
			n: n, id: rID, isMaker: false, srcCur: "BTC", dstCur: "LTC",
			refundHex: hex.EncodeToString(rtx.Serialize()), refundDone: false,
			ourLockTime: 1, state: csCreatedA,
		}
	}, true)

	// The sweep posts the refund task; wait until it is parked in the wallet.
	n.submit(func() { n.scanRefunds() }, false)
	deadline := time.Now().Add(5 * time.Second)
	for gated.callCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if gated.callCount() != 1 {
		t.Fatalf("refund task never reached SendRawTransaction (calls=%d)", gated.callCount())
	}

	done := make(chan error, 1)
	go func() { done <- n.Close() }()
	select {
	case err := <-done:
		t.Fatalf("Close returned while refund task in flight: %v", err)
	case <-time.After(300 * time.Millisecond):
	}

	gated.open()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return after the refund task drained")
	}
}

// TestEngineLivenessDispatchSwapSlowWallet drives session B's CreateA through
// the FULL dispatch path (hub-signed packet → processSwap re-verify → stage1 →
// worker → resume → sign+send) while session A's wallet is parked, proving a
// slow wallet cannot stall packet dispatch for unrelated sessions.
func TestEngineLivenessDispatchSwapSlowWallet(t *testing.T) {
	n, cc, gated, ltc := setupTwoCoinNode(t)
	defer gated.open()

	hubPriv := make([]byte, 32)
	hubPriv[31] = 2
	hubPub, err := crypto.CompressedPubKey(hubPriv)
	if err != nil {
		t.Fatal(err)
	}
	registerHub(t, n, hubPriv)

	// Session A (maker, BTC): deposit parked on the gated connector.
	mPriv, mPub := newKey(t)
	var aID [32]byte
	copy(aID[:], []byte("liveness-dispatch-a-order-00000"))
	aOrder := &Order{
		ID: aID, FromCurrency: "BTC", ToCurrency: "BTC", FromAmount: 2.5e6, ToAmount: 2.5e6,
		SNodePubkey: hex.EncodeToString(hubPub[:]), HubAddress: coins.KeyID(hubPub[:]),
	}
	n.newMakerSession(withUsedCoins(t, n, aOrder, []wallet.Utxo{gated.funding}), MakeOrderParams{MakerAddress: addrFor(0, "maker-a"), TakerAddress: addrFor(0, "taker-a")}, arr32(mPriv), toArr33(mPub))
	n.submit(func() {
		if _, _, err := n.sessions[hexEncode(aID[:])].OnCreateA(&proto.CreateABody{ID: aID, BPubKey: to33(mPub)}); err != nil {
			t.Errorf("A OnCreateA: %v", err)
		}
	}, true)
	deadline := time.Now().Add(5 * time.Second)
	for gated.callCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if gated.callCount() != 1 {
		t.Fatalf("A deposit never reached the wallet (calls=%d)", gated.callCount())
	}

	// Session B (maker, LTC): CreateA delivered as a live hub-signed packet via
	// processSwap (hub-key re-verify + registry check), marshaled onto the
	// engine goroutine like an inbound packet would be.
	_, bPub := newKey(t)
	var bID [32]byte
	copy(bID[:], []byte("liveness-dispatch-b-order-00000"))
	bOrder := &Order{
		ID: bID, FromCurrency: "LTC", ToCurrency: "LTC", FromAmount: 2.5e6, ToAmount: 2.5e6,
		SNodePubkey: hex.EncodeToString(hubPub[:]), HubAddress: coins.KeyID(hubPub[:]),
	}
	n.newMakerSession(withUsedCoins(t, n, bOrder, []wallet.Utxo{ltc.funding}), MakeOrderParams{MakerAddress: addrFor(48, "maker-b"), TakerAddress: addrFor(48, "taker-b")}, arr32(mPriv), toArr33(bPub))
	createA := hubSignedPkt(t, hubPriv, proto.XbcTransactionCreateA, &proto.CreateABody{
		HubAddress: coins.KeyID(hubPub[:]), ID: bID, BPubKey: to33(bPub),
	})
	n.submit(func() {
		n.processSwap(createA, bID, [20]byte{}, "CreateA", func(s *SwapSession) (proto.XBridgeCommand, responseBody, error) {
			return s.OnCreateA(&proto.CreateABody{HubAddress: coins.KeyID(hubPub[:]), ID: bID, BPubKey: to33(bPub)})
		})
	}, false)

	// B's CreatedA arrives (A's is deferred behind the parked wallet).
	waitForPacket(t, cc, proto.XbcTransactionCreatedA, 5*time.Second)
	if got := ltc.broadcastSnapshot(); len(got) != 1 {
		t.Fatalf("LTC deposit broadcast %d times, want 1", len(got))
	}
	if got := gated.callCount(); got != 1 {
		t.Fatalf("A wallet called %d times, want 1 (still parked)", got)
	}
	if got := readOnEngine(t, n, func() bool { return n.sessions[hexEncode(bID[:])].await }); got {
		t.Error("B await still set after its CreatedA")
	}
	if got := readOnEngine(t, n, func() bool { return n.sessions[hexEncode(aID[:])].await }); !got {
		t.Error("A await cleared while its deposit is still parked")
	}
}

// TestRefundTaskDropInvokesDone proves a full worker queue cannot strand a
// refund caller: when postRefundTask's drop branch fires (all workers busy and
// the task buffer full), it must still invoke the done callback so
// BroadcastRefund returns an error instead of blocking forever on the out
// channel. The stored-order path (tryStoredRefund chaining candidates through
// the callback) hangs pre-fix, because the drop branch never called done.
func TestRefundTaskDropInvokesDone(t *testing.T) {
	n, _, gated, _ := setupTwoCoinNode(t)
	defer gated.open() // runs before t.Cleanup's Close, so shutdown can always drain

	// Park every worker in SendRawTransaction (gate held shut), then fill the
	// 16-slot task buffer with due refunds so the next postRefundTask must drop.
	for i := 0; i < engineWorkers+cap(n.tasks); i++ {
		var rID [32]byte
		copy(rID[:], fmt.Sprintf("refund-drop-%03d-00000000000000", i))
		rtx := &coins.Tx{Version: 1}
		rtx.Inputs = []coins.TxIn{{PrevOut: coins.OutPoint{Hash: mustHash(strings.Repeat("ef", 32)), Index: 0}, Sequence: 0xfffffffe}}
		rtx.Outputs = []coins.TxOut{{Value: 1, ScriptPubKey: []byte{0x51}}}
		n.submit(func() {
			n.sessions[hexEncode(rID[:])] = &SwapSession{
				n: n, id: rID, isMaker: false, srcCur: "BTC", dstCur: "LTC",
				refundHex: hex.EncodeToString(rtx.Serialize()), refundDone: false,
				ourLockTime: 1, state: csCreatedA,
			}
		}, true)
	}

	// The sweep enqueues a refund task for each session; exactly engineWorkers
	// tasks park in SendRawTransaction and the rest fill the buffer.
	n.submit(func() { n.scanRefunds() }, false)
	// Generous wall-clock bound: the property is parking, not speed.
	deadline := time.Now().Add(30 * time.Second)
	for gated.callCount() < engineWorkers && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if gated.callCount() < engineWorkers {
		t.Fatalf("expected %d parked refund tasks, got %d", engineWorkers, gated.callCount())
	}

	// A stored order with no live session: BroadcastRefund routes through
	// tryStoredRefund, whose chained postRefundTask hits the full queue and
	// drops. The done callback must still fire so the caller unblocks.
	var sID [32]byte
	copy(sID[:], []byte("refund-drop-stored-order-0000"))
	storedID := hexEncode(sID[:])
	rtx := &coins.Tx{Version: 1}
	rtx.Inputs = []coins.TxIn{{PrevOut: coins.OutPoint{Hash: mustHash(strings.Repeat("f0", 32)), Index: 0}, Sequence: 0xfffffffe}}
	rtx.Outputs = []coins.TxOut{{Value: 1, ScriptPubKey: []byte{0x51}}}
	n.store.Add(&Order{ID: sID, FromCurrency: "BTC", ToCurrency: "LTC", RefundTx: hex.EncodeToString(rtx.Serialize())})

	done := make(chan error, 1)
	go func() { _, err := n.BroadcastRefund(storedID); done <- err }()
	select {
	case err := <-done:
		// The chain exhausts (both candidates dropped), so the caller sees the
		// tryStoredRefund exhaustion error, not a success.
		if err == nil || !strings.Contains(err.Error(), "stored refund") {
			t.Fatalf("BroadcastRefund err = %v, want stored-refund exhaustion error", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("BroadcastRefund blocked forever on a dropped refund task")
	}
}

// TestEngineAppliesResultBeforeNextPacket proves the engine drains a ready
// worker result before judging an inbound packet for the same session: with a
// session in the await state and both a completed-deposit result and a next-step
// CreateA packet queued, the result apply must clear await first so the packet
// is processed (deposit task posted → SendRawTransaction). If the packet is
// judged first, processSwap drops it as a retransmit and no deposit is built.
func TestEngineAppliesResultBeforeNextPacket(t *testing.T) {
	n, _, gated, _ := setupTwoCoinNode(t)
	defer gated.open() // runs before t.Cleanup's Close, so shutdown can always drain

	// A maker session pinned to a registered hub so the CreateA packet passes
	// verifyHubPacket, with funding so processing the packet posts a deposit.
	mPriv, mPub := newKey(t)
	_, tkPub := newKey(t)
	hubPriv := make([]byte, 32)
	hubPriv[31] = 2
	hubPub, err := crypto.CompressedPubKey(hubPriv)
	if err != nil {
		t.Fatal(err)
	}
	registerHub(t, n, hubPriv)

	var orderID [32]byte
	copy(orderID[:], []byte("order-id-order-id-order-id-0"))
	mkAddr := addrFor(0, "maker-btc-dest")
	o := &Order{
		ID: orderID, FromCurrency: "BTC", ToCurrency: "BTC", FromAmount: 2.5e6, ToAmount: 2.5e6,
		SNodePubkey: hex.EncodeToString(hubPub[:]), HubAddress: coins.KeyID(hubPub[:]),
	}
	n.newMakerSession(withUsedCoins(t, n, o, []wallet.Utxo{gated.funding}), MakeOrderParams{MakerAddress: mkAddr, TakerAddress: mkAddr}, arr32(mPriv), toArr33(mPub))
	n.submit(func() {
		s := n.sessions[hexEncode(orderID[:])]
		s.await = true // a deposit/claim task is "in flight"
	}, true)

	// Park the engine so both the result and the packet are queued before it
	// returns to its select: with the flat select the packet could be picked
	// while await is still set and dropped; with the priority select the result
	// is always drained first.
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unpark := func() { releaseOnce.Do(func() { close(release) }) }
	defer unpark() // safety net: a failing test must not hang Close on the parked engine
	n.submit(func() { close(started); <-release }, false)
	<-started

	// The completed-deposit result (clears await) and the next-step CreateA.
	n.results <- workResult{
		task: workTask{
			orderID: hexEncode(orderID[:]),
			apply: func(v any, terr error) {
				s := n.sessions[hexEncode(orderID[:])]
				if s != nil {
					s.await = false
				}
			},
		},
	}
	n.packets <- inboundPacket{
		pkt: hubSignedPkt(t, hubPriv, proto.XbcTransactionCreateA, &proto.CreateABody{
			HubAddress: coins.KeyID(hubPub[:]), ID: orderID, BPubKey: to33(tkPub),
		}),
		snode: hex.EncodeToString(hubPub[:]),
	}
	unpark() // release the parked engine; the defer is a no-op safety net now

	// The packet must be processed (deposit task reaches the wallet), proving the
	// result apply ran before the packet was judged.
	deadline := time.Now().Add(5 * time.Second)
	for gated.callCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if c := gated.callCount(); c == 0 {
		t.Fatalf("next-step packet dropped as a retransmit: result not applied before the packet (calls=%d)", c)
	}
}
