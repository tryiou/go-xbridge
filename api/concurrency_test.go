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

// TestConcurrentRefundSweepAndDepositTask — F3 proof. The refund sweep
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
	n.newMakerSession(&Order{ID: aID, FromCurrency: "BTC", ToCurrency: "BTC", FromAmount: 1e8, ToAmount: 1e8},
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
				n.checkRefunds()
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

	if got := gated.fakeConnector.broadcastSnapshot(); len(got) != 1 {
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

// TestEngineLivenessSlowWallet proves a parked wallet on one coin never stalls
// the engine or other sessions: session B's full handshake completes while A's
// deposit is still blocked in SendRawTransaction.
func TestEngineLivenessSlowWallet(t *testing.T) {
	n, cc, gated, ltc := setupTwoCoinNode(t)
	defer gated.open()

	mPriv, mPub := newKey(t)
	var aID [32]byte
	copy(aID[:], []byte("liveness-a-order-00000000000000"))
	n.newMakerSession(&Order{ID: aID, FromCurrency: "BTC", ToCurrency: "BTC", FromAmount: 1e8, ToAmount: 1e8},
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
	n.newMakerSession(&Order{ID: bID, FromCurrency: "LTC", ToCurrency: "LTC", FromAmount: 1e8, ToAmount: 1e8},
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
	n.newMakerSession(&Order{ID: aID, FromCurrency: "BTC", ToCurrency: "BTC", FromAmount: 1e8, ToAmount: 1e8},
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
	if err := coins.InitFromConf(map[string]*config.CoinConf{
		"BTC": {Ticker: "BTC", Coin: 1e8, AddressPrefix: 0, CreateTxMethod: "BTC", BlockTime: 60},
	}); err != nil {
		t.Fatal(err)
	}
	conn := &fakeConnector{ticker: "BTC", blockHeight: 1000}
	n, cc := newStartedNode(t, map[string]*config.CoinConf{
		"BTC": {Ticker: "BTC", Coin: 1e8, AddressPrefix: 0, CreateTxMethod: "BTC", BlockTime: 60},
	}, map[string]wallet.Connector{"BTC": conn})

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
		ID: oid, FromCurrency: "BTC", ToCurrency: "BTC", FromAmount: 1e8, ToAmount: 1e8,
		Status: "open", SNodePubkey: hex.EncodeToString(hubPub[:]), HubAddress: coins.KeyID(hubPub[:]),
	}
	n.store.Add(o)

	const takes = 8
	var wg sync.WaitGroup
	for i := 0; i < takes; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, rerr := n.TakeOrder(TakeOrderParams{
				ID:          orderIDString(oid),
				FromAddress: addrFor(0, fmt.Sprintf("take-from-%d", i)),
				ToAddress:   addrFor(0, fmt.Sprintf("take-to-%d", i)),
			}); rerr != nil {
				t.Errorf("TakeOrder %d: %v", i, rerr)
			}
		}(i)
	}
	wg.Wait()

	if got := readOnEngine(t, n, func() int { return len(n.sessions) }); got != 1 {
		t.Fatalf("sessions = %d, want 1 (no fork)", got)
	}
	if got := n.store.Get(hexEncode(oid[:])); got == nil || got.Status != "accepting" {
		t.Fatalf("order status = %v, want accepting", got)
	}
	if got := len(cc.snapshot()); got != takes {
		t.Fatalf("Accepting packets written = %d, want %d", got, takes)
	}
}
