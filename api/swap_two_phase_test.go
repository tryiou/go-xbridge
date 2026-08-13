package api

import (
	"encoding/hex"
	"strings"
	"sync"
	"testing"
	"time"

	"go-xbridge/coins"
	"go-xbridge/config"
	"go-xbridge/crypto"
	"go-xbridge/p2p/servicenode"
	"go-xbridge/proto"
	"go-xbridge/wallet"
)

// waitForPacket polls the capture conn until a packet with the given command is
// written (or the timeout expires). The response is written by the engine-side
// resume after a two-phase task completes, so a started-mode test asserts the
// full stage1 → worker → resume → send pipeline through this.
func waitForPacket(t *testing.T, cc *captureXConn, cmd proto.XBridgeCommand, timeout time.Duration) *proto.Packet {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, p := range cc.snapshot() {
			if p.Command == cmd {
				return p
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no %s packet written within %v (wrote %d)", cmd, timeout, len(cc.snapshot()))
	return nil
}

// readOnEngine reads engine-owned session/state through the engine goroutine,
// so a started-mode test never races the engine's resume writes. The channel
// close in submit provides the happens-before for `out`.
func readOnEngine[T any](t *testing.T, n *Node, f func() T) T {
	t.Helper()
	var out T
	n.submit(func() { out = f() }, true)
	return out
}

// newStartedNode builds a node with the engine + worker pool running, wired to
// a captureXConn, so tests can drive two-phase handlers through submit and
// observe the outbound responses the resumes send.
func newStartedNode(t *testing.T, confs map[string]*config.CoinConf, conns map[string]wallet.Connector) (*Node, *captureXConn) {
	t.Helper()
	n := newTestNode(t, confs, conns)
	cc := &captureXConn{}
	n.conn = cc
	n.start()
	t.Cleanup(func() { _ = n.Close() })
	return n, cc
}

// registerHub pins a servicenode so a session's trusted hub key passes
// verifyHubPacket in started mode (STRICT hubRegistered, mirroring C++ getSn).
func registerHub(t *testing.T, n *Node, hubPriv []byte) {
	t.Helper()
	hubPub, err := crypto.CompressedPubKey(hubPriv)
	if err != nil {
		t.Fatal(err)
	}
	reg := servicenode.NewRegistry()
	reg.AddPing(servicenode.ServiceNode{
		PubKey: hubPub, Tier: servicenode.TierSPV, Services: []string{"BTC"}, XBridgeVersion: proto.ProtocolVersion,
	})
	n.snReg = reg
}

// hubSignedPkt signs a body for the given command with the hub's key, the
// format a live hub packet takes on the wire.
func hubSignedPkt(t *testing.T, hubPriv []byte, cmd proto.XBridgeCommand, body interface{ Marshal() []byte }) *proto.Packet {
	t.Helper()
	pkt := proto.NewPacket(cmd, body.Marshal())
	if err := crypto.NewBtcSigner().Sign(pkt, hubPriv); err != nil {
		t.Fatal(err)
	}
	return pkt
}

// TestTwoPhaseCreateAWorkerPool drives a full maker CreateA through the started
// engine: stage 1 runs on the engine, the deposit build runs on a worker, and
// the engine-side resume sends the CreatedA response. It asserts the deposit
// was actually broadcast from the worker, the session adopted the outcome, and
// the await guard was cleared.
func TestTwoPhaseCreateAWorkerPool(t *testing.T) {
	if err := coins.InitFromConf(map[string]*config.CoinConf{
		"BTC": {Ticker: "BTC", Title: "Bitcoin", Coin: 1e8, AddressPrefix: 0, ScriptPrefix: 5, CreateTxMethod: "BTC", BlockTime: 60},
	}); err != nil {
		t.Fatal(err)
	}

	mPriv, mPub := newKey(t)
	_, tkPub := newKey(t)
	btcFundingPriv, btcFundingPub := newKey(t)
	btcFunding := wallet.Utxo{
		TxID:         strings.Repeat("aa", 32),
		Vout:         0,
		Amount:       5e8,
		ScriptPubKey: hex.EncodeToString(coins.BuildP2PKHScript(coins.KeyID(btcFundingPub))),
	}
	btcConn := &fakeConnector{
		ticker: "BTC", funding: btcFunding, fundingPriv: btcFundingPriv, fundingPub: btcFundingPub,
		changeAddr: addrFor(0, "btc-change"), blockHeight: 1000, rawTx: map[string]string{},
	}
	confs := map[string]*config.CoinConf{"BTC": {Ticker: "BTC", Coin: 1e8, AddressPrefix: 0, CreateTxMethod: "BTC", BlockTime: 60}}
	conns := map[string]wallet.Connector{"BTC": btcConn}

	n, cc := newStartedNode(t, confs, conns)

	var orderID [32]byte
	oid := hash20("order-id")
	copy(orderID[:], oid[:])
	mkAddr := addrFor(0, "maker-btc-dest")
	n.newMakerSession(withUsedCoins(t, n, &Order{ID: orderID, FromCurrency: "BTC", ToCurrency: "BTC", FromAmount: 2.5e6, ToAmount: 2.5e6}, []wallet.Utxo{btcConn.funding}), MakeOrderParams{MakerAddress: mkAddr, TakerAddress: mkAddr}, arr32(mPriv), toArr33(mPub))
	s := n.sessions[hexEncode(orderID[:])]

	// Stage 1 on the engine; the worker builds+broadcasts the deposit async.
	n.submit(func() {
		if _, _, err := s.OnCreateA(&proto.CreateABody{HubAddress: [20]byte{}, ID: orderID, BPubKey: to33(tkPub)}); err != nil {
			t.Errorf("OnCreateA stage 1: %v", err)
		}
	}, true)

	pkt := waitForPacket(t, cc, proto.XbcTransactionCreatedA, 5*time.Second)
	body, err := proto.DecodeBody(pkt.Command, pkt.Body)
	if err != nil {
		t.Fatalf("decode CreatedA response: %v", err)
	}
	createdA, ok := body.(*proto.CreatedABody)
	if !ok {
		t.Fatalf("response body = %T, want *proto.CreatedABody", body)
	}
	if createdA.ADepositTxID == "" {
		t.Fatal("empty ADepositTxID in response")
	}
	if createdA.RefTx == "" {
		t.Fatal("empty RefTx (refund) in response")
	}

	// The deposit must have been broadcast once, by the worker.
	bcasts := btcConn.broadcastSnapshot()
	if len(bcasts) != 1 {
		t.Fatalf("deposit broadcast %d times, want 1", len(bcasts))
	}
	if bcasts[0] != createdA.ADepositTxID {
		t.Errorf("broadcast txid %s != response %s", bcasts[0], createdA.ADepositTxID)
	}

	// Resume applied the outcome and cleared the await guard on the engine.
	if got := readOnEngine(t, n, func() string { return s.ourDepositTxID }); got != createdA.ADepositTxID {
		t.Errorf("session ourDepositTxID = %q, want %q", got, createdA.ADepositTxID)
	}
	if got := readOnEngine(t, n, func() uint32 { return s.ourLockTime }); got != createdA.ALockTime {
		t.Errorf("session ourLockTime = %d, want %d", got, createdA.ALockTime)
	}
	if got := readOnEngine(t, n, func() bool { return s.await }); got {
		t.Error("session await still set after resume")
	}
}

// TestTwoPhaseConfirmBSecretRecovery proves the wallet I/O that recovers the
// HTLC preimage from the maker's payTx runs on the worker: the taker session
// learns the secret only via the resume, and its claim is broadcast from the
// worker. The maker's payTx is seeded in the shared LTC connector (the coin the
// maker redeemed); the taker's claim then spends the maker's BTC deposit.
func TestTwoPhaseConfirmBSecretRecovery(t *testing.T) {
	if err := coins.InitFromConf(map[string]*config.CoinConf{
		"BTC": {Ticker: "BTC", Title: "Bitcoin", Coin: 1e8, AddressPrefix: 0, ScriptPrefix: 5, CreateTxMethod: "BTC", BlockTime: 60},
		"LTC": {Ticker: "LTC", Title: "Litecoin", Coin: 1e8, AddressPrefix: 48, ScriptPrefix: 50, CreateTxMethod: "LTC", BlockTime: 60},
	}); err != nil {
		t.Fatal(err)
	}

	btcFundingPriv, btcFundingPub := newKey(t)
	btcFunding := wallet.Utxo{
		TxID:         strings.Repeat("aa", 32),
		Vout:         0,
		Amount:       5e8,
		ScriptPubKey: hex.EncodeToString(coins.BuildP2PKHScript(coins.KeyID(btcFundingPub))),
	}
	btcConn := &fakeConnector{
		ticker: "BTC", funding: btcFunding, fundingPriv: btcFundingPriv, fundingPub: btcFundingPub,
		changeAddr: addrFor(0, "btc-change"), blockHeight: 1000, rawTx: map[string]string{},
	}
	ltcConn := &fakeConnector{
		ticker: "LTC", funding: wallet.Utxo{}, fundingPriv: nil, fundingPub: nil,
		changeAddr: addrFor(48, "ltc-change"), blockHeight: 1000, rawTx: map[string]string{},
	}
	confs := map[string]*config.CoinConf{
		"BTC": {Ticker: "BTC", Coin: 1e8, AddressPrefix: 0, CreateTxMethod: "BTC", BlockTime: 60},
		"LTC": {Ticker: "LTC", Coin: 1e8, AddressPrefix: 48, CreateTxMethod: "LTC", BlockTime: 60},
	}
	conns := map[string]wallet.Connector{"BTC": btcConn, "LTC": ltcConn}

	// Taker keys (the taker's session owns them; the exact maker key values are
	// irrelevant to this test — secretFromPayTx only matches the secret push).
	tkPriv, tkPub := newKey(t)
	mkPub := make([]byte, 33)
	mkPub[0] = 0x03
	copy(mkPub[1:], tkPub[1:])

	// The maker's secret; its hash is what the taker's deposit commits to.
	var secret [33]byte
	for i := range secret {
		secret[i] = byte(i + 1)
	}
	secretHash := coins.KeyID(secret[:])

	// The maker's payTx, as ConfirmA would have produced it: spends the taker's
	// LTC deposit and reveals the secret in its scriptSig.
	makerPayTxID := strings.Repeat("dd", 32)
	inner := coins.BuildDepositUnlockScript(mkPub, mkPub, secretHash[:], 1030)
	ptx := &coins.Tx{Version: 1}
	ptx.Inputs = append(ptx.Inputs, coins.TxIn{
		PrevOut:  coins.OutPoint{Hash: mustHash(strings.Repeat("ee", 32)), Index: 0},
		Sequence: 0xffffffff,
		ScriptSig: coins.BuildPaymentScriptSig(
			secret[:], make([]byte, 71), mkPub, inner),
	})
	ptx.Outputs = append(ptx.Outputs, coins.TxOut{Value: 1, ScriptPubKey: []byte{0x51}})
	ltcConn.setRawTx(makerPayTxID, hex.EncodeToString(ptx.Serialize()))

	n, cc := newStartedNode(t, confs, conns)

	var orderID [32]byte
	oid := hash20("order-id")
	copy(orderID[:], oid[:])
	// Taker: srcCur = the coin it deposits (LTC, where the maker's payTx lives),
	// dstCur = the maker's coin (BTC, whose deposit it claims).
	tkAddr := addrFor(48, "taker-ltc-source")
	btcDest := addrFor(0, "maker-btc-dest")
	n.newTakerSession(withUsedCoins(t, n, &Order{ID: orderID, FromCurrency: "BTC", ToCurrency: "LTC", FromAmount: 2.5e6, ToAmount: 2e6}, []wallet.Utxo{ltcConn.funding}), TakeOrderParams{FromAddress: tkAddr, ToAddress: btcDest}, arr32(tkPriv), to33(tkPub))
	s := n.sessions[hexEncode(orderID[:])]
	// Seed the counterparty deposit knowledge ConfirmB needs (normally learned
	// via CreateB); the secret itself is deliberately NOT set. The validated
	// deposit out-params (CRYPTO-F90) feed the claim: vout 0, 2.5 BTC.
	s.theirSecretHash = secretHash
	s.theirDepositTxID = strings.Repeat("cc", 32) // the maker's BTC deposit
	s.theirLockTime = 1030
	s.theirPub = to33(mkPub)
	s.theirDepositVout = 0
	s.theirP2SHNative = 2.5e8

	n.submit(func() {
		if _, _, err := s.OnConfirmB(&proto.ConfirmBBody{HubAddress: [20]byte{}, ID: orderID, APayTxID: makerPayTxID}); err != nil {
			t.Errorf("OnConfirmB stage 1: %v", err)
		}
	}, true)

	pkt := waitForPacket(t, cc, proto.XbcTransactionConfirmedB, 5*time.Second)
	body, err := proto.DecodeBody(pkt.Command, pkt.Body)
	if err != nil {
		t.Fatalf("decode ConfirmedB response: %v", err)
	}
	confirmedB, ok := body.(*proto.ConfirmedBBody)
	if !ok {
		t.Fatalf("response body = %T, want *proto.ConfirmedBBody", body)
	}
	if confirmedB.BPayTxID == "" {
		t.Fatal("empty BPayTxID in response")
	}

	// The taker's claim must be broadcast on the maker's coin (BTC) and spend
	// the maker's deposit.
	bcasts := btcConn.broadcastSnapshot()
	if len(bcasts) != 1 {
		t.Fatalf("claim broadcast %d times, want 1", len(bcasts))
	}
	if bcasts[0] != confirmedB.BPayTxID {
		t.Errorf("claim txid %s != response %s", bcasts[0], confirmedB.BPayTxID)
	}

	// The resume adopted the worker-recovered secret into the session.
	if got := readOnEngine(t, n, func() [33]byte { return s.secret }); got != secret {
		t.Error("session secret not recovered by the worker")
	}
	if got := readOnEngine(t, n, func() bool { return s.await }); got {
		t.Error("session await still set after resume")
	}
}

// TestCreateAStateGuardDropsPostCompletionRetransmitE2E proves the STATE-F77 state
// guard end-to-end: after the deposit task COMPLETES (await cleared, state ==
// csCreatedA), a duplicate CreateA from the hub must be dropped by the handler
// — not processSwap's in-flight guard — so exactly one deposit is ever
// broadcast and exactly one CreatedA response is sent. Mirrors C++
// processTransactionCreateA's `state >= trCreated` guard (xbridgesession.cpp:1947).
func TestCreateAStateGuardDropsPostCompletionRetransmitE2E(t *testing.T) {
	if err := coins.InitFromConf(map[string]*config.CoinConf{
		"BTC": {Ticker: "BTC", Title: "Bitcoin", Coin: 1e8, AddressPrefix: 0, ScriptPrefix: 5, CreateTxMethod: "BTC", BlockTime: 60},
	}); err != nil {
		t.Fatal(err)
	}

	mPriv, mPub := newKey(t)
	_, tkPub := newKey(t)
	btcFundingPriv, btcFundingPub := newKey(t)
	btcFunding := wallet.Utxo{
		TxID:         strings.Repeat("aa", 32),
		Vout:         0,
		Amount:       5e8,
		ScriptPubKey: hex.EncodeToString(coins.BuildP2PKHScript(coins.KeyID(btcFundingPub))),
	}
	btcConn := &fakeConnector{
		ticker: "BTC", funding: btcFunding, fundingPriv: btcFundingPriv, fundingPub: btcFundingPub,
		changeAddr: addrFor(0, "btc-change"), blockHeight: 1000, rawTx: map[string]string{},
	}
	confs := map[string]*config.CoinConf{"BTC": {Ticker: "BTC", Coin: 1e8, AddressPrefix: 0, CreateTxMethod: "BTC", BlockTime: 60}}
	conns := map[string]wallet.Connector{"BTC": btcConn}

	hubPriv := make([]byte, 32)
	hubPriv[31] = 2
	hubPub, err := crypto.CompressedPubKey(hubPriv)
	if err != nil {
		t.Fatal(err)
	}

	n, cc := newStartedNode(t, confs, conns)
	registerHub(t, n, hubPriv)

	var orderID [32]byte
	copy(orderID[:], []byte("order-id-order-id-order-id-0"))
	mkAddr := addrFor(0, "maker-btc-dest")
	o := &Order{
		ID: orderID, FromCurrency: "BTC", ToCurrency: "BTC", FromAmount: 2.5e6, ToAmount: 2.5e6,
		SNodePubkey: hex.EncodeToString(hubPub[:]), HubAddress: coins.KeyID(hubPub[:]),
	}
	n.newMakerSession(withUsedCoins(t, n, o, []wallet.Utxo{btcConn.funding}), MakeOrderParams{MakerAddress: mkAddr, TakerAddress: mkAddr}, arr32(mPriv), toArr33(mPub))
	s := n.sessions[hexEncode(orderID[:])]

	dispatch := func() {
		n.submit(func() {
			n.processSwap(createAPkt(t, hubPriv, orderID, tkPub), orderID, [20]byte{}, "CreateA", func(sess *SwapSession) (proto.XBridgeCommand, responseBody, error) {
				return sess.OnCreateA(&proto.CreateABody{HubAddress: coins.KeyID(hubPub[:]), ID: orderID, BPubKey: to33(tkPub)})
			})
		}, true)
	}

	// First delivery completes: deposit broadcast by the worker, CreatedA sent.
	dispatch()
	pkt := waitForPacket(t, cc, proto.XbcTransactionCreatedA, 5*time.Second)
	if _, err := proto.DecodeBody(pkt.Command, pkt.Body); err != nil {
		t.Fatalf("decode CreatedA response: %v", err)
	}
	if got := readOnEngine(t, n, func() clientState { return s.state }); got != csCreatedA {
		t.Fatalf("session state = %v after first CreateA, want csCreatedA", got)
	}
	if got := btcConn.broadcastSnapshot(); len(got) != 1 {
		t.Fatalf("deposit broadcast %d times after first CreateA, want 1", len(got))
	}

	// Post-completion retransmit: await is cleared, so only the handler's state
	// guard can drop it. No second deposit, no second CreatedA.
	dispatch()
	if got := btcConn.broadcastSnapshot(); len(got) != 1 {
		t.Fatalf("retransmit re-broadcast the deposit: %d broadcasts, want 1", len(got))
	}
	var createdA int
	for _, p := range cc.snapshot() {
		if p.Command == proto.XbcTransactionCreatedA {
			createdA++
		}
	}
	if createdA != 1 {
		t.Fatalf("%d CreatedA responses sent, want 1", createdA)
	}
	if got := readOnEngine(t, n, func() clientState { return s.state }); got != csCreatedA {
		t.Fatalf("session state = %v after retransmit, want csCreatedA", got)
	}
	if got := readOnEngine(t, n, func() bool { return s.await }); got {
		t.Error("session await set after retransmit (should stay clear)")
	}
}

// createAPkt returns a hub-signed CreateA packet for the given order, the wire
// shape a retransmit takes.
func createAPkt(t *testing.T, hubPriv []byte, orderID [32]byte, tkPub []byte) *proto.Packet {
	t.Helper()
	hubPub, err := crypto.CompressedPubKey(hubPriv)
	if err != nil {
		t.Fatal(err)
	}
	return hubSignedPkt(t, hubPriv, proto.XbcTransactionCreateA, &proto.CreateABody{HubAddress: coins.KeyID(hubPub[:]), ID: orderID, BPubKey: to33(tkPub)})
}

// mustHash decodes a 32-byte hex string into a [32]byte, panicking on bad hex.
func mustHash(s string) [32]byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	var h [32]byte
	copy(h[:], b)
	return h
}

// gatedConnector wraps a fakeConnector and blocks every SendRawTransaction until
// release is called, so a started-mode test can hold a deposit task in flight
// (await set) while it probes the engine's retransmit handling.
type gatedConnector struct {
	*fakeConnector
	release chan struct{}
	once    sync.Once
	mu      sync.Mutex
	calls   int
}

func newGatedConnector(c *fakeConnector) *gatedConnector {
	return &gatedConnector{fakeConnector: c, release: make(chan struct{})}
}

func (g *gatedConnector) SendRawTransaction(txHex string) (string, error) {
	g.mu.Lock()
	g.calls++
	g.mu.Unlock()
	<-g.release
	return g.fakeConnector.SendRawTransaction(txHex)
}

func (g *gatedConnector) callCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls
}

func (g *gatedConnector) open() {
	g.once.Do(func() { close(g.release) })
}

// TestTwoPhaseAwaitDropsRetransmit proves the retransmit guard: while a deposit
// task is in flight (await set), a duplicate CreateA from the hub must be
// dropped by processSwap before it re-runs stage 1 — so exactly one deposit is
// ever broadcast.
func TestTwoPhaseAwaitDropsRetransmit(t *testing.T) {
	if err := coins.InitFromConf(map[string]*config.CoinConf{
		"BTC": {Ticker: "BTC", Title: "Bitcoin", Coin: 1e8, AddressPrefix: 0, ScriptPrefix: 5, CreateTxMethod: "BTC", BlockTime: 60},
	}); err != nil {
		t.Fatal(err)
	}

	mPriv, mPub := newKey(t)
	_, tkPub := newKey(t)
	btcFundingPriv, btcFundingPub := newKey(t)
	btcFunding := wallet.Utxo{
		TxID:         strings.Repeat("aa", 32),
		Vout:         0,
		Amount:       5e8,
		ScriptPubKey: hex.EncodeToString(coins.BuildP2PKHScript(coins.KeyID(btcFundingPub))),
	}
	btcConn := &fakeConnector{
		ticker: "BTC", funding: btcFunding, fundingPriv: btcFundingPriv, fundingPub: btcFundingPub,
		changeAddr: addrFor(0, "btc-change"), blockHeight: 1000, rawTx: map[string]string{},
	}
	gated := newGatedConnector(btcConn)
	confs := map[string]*config.CoinConf{"BTC": {Ticker: "BTC", Coin: 1e8, AddressPrefix: 0, CreateTxMethod: "BTC", BlockTime: 60}}
	conns := map[string]wallet.Connector{"BTC": gated}

	// Hub pinned on the session so the retransmit packet passes verifyHubPacket.
	hubPriv := make([]byte, 32)
	hubPriv[31] = 2
	hubPub, err := crypto.CompressedPubKey(hubPriv)
	if err != nil {
		t.Fatal(err)
	}

	n, cc := newStartedNode(t, confs, conns)
	registerHub(t, n, hubPriv)

	var orderID [32]byte
	copy(orderID[:], []byte("order-id-order-id-order-id-0"))
	mkAddr := addrFor(0, "maker-btc-dest")
	o := &Order{
		ID: orderID, FromCurrency: "BTC", ToCurrency: "BTC", FromAmount: 2.5e6, ToAmount: 2.5e6,
		SNodePubkey: hex.EncodeToString(hubPub[:]), HubAddress: coins.KeyID(hubPub[:]),
	}
	n.newMakerSession(withUsedCoins(t, n, o, []wallet.Utxo{btcConn.funding}), MakeOrderParams{MakerAddress: mkAddr, TakerAddress: mkAddr}, arr32(mPriv), toArr33(mPub))
	s := n.sessions[hexEncode(orderID[:])]

	// The live hub packet: CreateA signed by the pinned hub.
	createA := hubSignedPkt(t, hubPriv, proto.XbcTransactionCreateA, &proto.CreateABody{HubAddress: coins.KeyID(hubPub[:]), ID: orderID, BPubKey: to33(tkPub)})

	// First delivery: stage 1 runs on the engine, posts the deposit task, sets
	// await; the worker blocks in SendRawTransaction (gate held shut).
	n.submit(func() {
		n.processSwap(createA, orderID, [20]byte{}, "CreateA", func(sess *SwapSession) (proto.XBridgeCommand, responseBody, error) {
			return sess.OnCreateA(&proto.CreateABody{HubAddress: coins.KeyID(hubPub[:]), ID: orderID, BPubKey: to33(tkPub)})
		})
	}, true)

	// Wait until the worker is actually holding the deposit broadcast.
	deadline := time.Now().Add(5 * time.Second)
	for gated.callCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if gated.callCount() != 1 {
		t.Fatalf("deposit task never reached SendRawTransaction (calls=%d)", gated.callCount())
	}

	// Retransmit: the hub resent CreateA while await is set. processSwap must
	// drop it before re-running stage 1 (which would broadcast a second deposit).
	n.submit(func() {
		n.processSwap(createA, orderID, [20]byte{}, "CreateA", func(sess *SwapSession) (proto.XBridgeCommand, responseBody, error) {
			return sess.OnCreateA(&proto.CreateABody{HubAddress: coins.KeyID(hubPub[:]), ID: orderID, BPubKey: to33(tkPub)})
		})
	}, true)
	if gated.callCount() != 1 {
		t.Fatalf("retransmit re-ran stage 1: %d SendRawTransaction calls, want 1", gated.callCount())
	}

	// Release the gate: the first task completes and the resume sends CreatedA.
	gated.open()
	pkt := waitForPacket(t, cc, proto.XbcTransactionCreatedA, 5*time.Second)
	if _, err := proto.DecodeBody(pkt.Command, pkt.Body); err != nil {
		t.Fatalf("decode CreatedA response: %v", err)
	}

	// Exactly one deposit broadcast total, and the guard was cleared.
	if got := btcConn.broadcastSnapshot(); len(got) != 1 {
		t.Fatalf("deposit broadcast %d times after retransmit, want 1", len(got))
	}
	if got := readOnEngine(t, n, func() bool { return s.await }); got {
		t.Error("session await still set after resume")
	}
}
