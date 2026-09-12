package api

import (
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"go-xbridge/coins"
	"go-xbridge/config"
	"go-xbridge/crypto"
	"go-xbridge/proto"
	"go-xbridge/wallet"
)

// newStartedPersistNode builds a started node whose swap state is durably
// written to dir (so restartLocalSwaps can recover it) with secrets persisted.
// It wires a captureXConn and starts the engine + worker pool.
func newStartedPersistNode(t *testing.T, dir string, confs map[string]*config.CoinConf, conns map[string]wallet.Connector) (*Node, *captureXConn) {
	t.Helper()
	n := &Node{
		config:   &Config{DataDir: dir, Confs: confs, Connectors: conns},
		store:    NewStore(),
		signer:   crypto.NewBtcSigner(),
		sessions: map[string]*SwapSession{},
		stop:     make(chan struct{}),
	}
	cc := &captureXConn{}
	n.conn = cc
	n.start()
	t.Cleanup(func() { _ = n.Close() })
	return n, cc
}

// TestRestartRecoversCreatedA crashes a maker after it has sent CreatedA, then
// restarts from the same DataDir and asserts: (a) the live session — including
// its state and our deposit txid — is recovered from disk; (b) there is no
// orphan order on disk; (c) a hub retransmit after restart is dropped by the
// state guard, so no second deposit is broadcast and no second CreatedA sent.
// This proves the durable-before-send boundary (persistNow in send) plus
// restart recovery end-to-end. C++ App::saveOrders (xbridgeapp.cpp:3868-3898)
// has no before-send guarantee; this port does.
func TestRestartRecoversCreatedA(t *testing.T) {
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

	dir := t.TempDir()
	n, cc := newStartedPersistNode(t, dir, confs, conns)
	registerHub(t, n, hubPriv)

	var orderID [32]byte
	copy(orderID[:], []byte("order-id-order-id-order-id-0"))
	mkAddr := addrFor(0, "maker-btc-dest")
	o := &Order{
		ID: orderID, FromCurrency: "BTC", ToCurrency: "BTC", FromAmount: 2.5e6, ToAmount: 2.5e6,
		Mine:        true,
		SNodePubkey: hex.EncodeToString(hubPub[:]), HubAddress: coins.KeyID(hubPub[:]),
	}
	n.newMakerSession(withUsedCoins(t, n, o, []wallet.Utxo{btcConn.funding}), MakeOrderParams{MakerAddress: mkAddr, TakerAddress: mkAddr}, arr32(mPriv), toArr33(mPub))
	s := n.sessions[hexEncode(orderID[:])]

	// Drive to CreatedA and let the durable write land before the packet.
	dispatch := func(node *Node) {
		node.submit(func() {
			node.processSwap(createAPkt(t, hubPriv, orderID, tkPub), orderID, [20]byte{}, "CreateA", func(sess *SwapSession) (proto.XBridgeCommand, responseBody, error) {
				return sess.OnCreateA(&proto.CreateABody{HubAddress: coins.KeyID(hubPub[:]), ID: orderID, BPubKey: to33(tkPub)})
			})
		}, true)
	}
	dispatch(n)
	waitForPacket(t, cc, proto.XbcTransactionCreatedA, 5*time.Second)
	if got := readOnEngine(t, n, func() clientState { return s.state }); got != csCreatedA {
		t.Fatalf("state = %v after CreatedA, want csCreatedA", got)
	}
	if got := btcConn.broadcastSnapshot(); len(got) != 1 {
		t.Fatalf("deposit broadcast %d times, want 1", len(got))
	}

	// On disk: exactly one persisted swap, already at csCreatedA with its txid.
	ps, _, err := loadSwaps(swapStatePath(dir))
	if err != nil {
		t.Fatalf("loadSwaps: %v", err)
	}
	if len(ps) != 1 {
		t.Fatalf("persisted %d swaps, want 1", len(ps))
	}
	if ps[0].State != csCreatedA {
		t.Fatalf("persisted state = %v, want csCreatedA", ps[0].State)
	}
	if ps[0].OurDepositTxID == "" {
		t.Fatal("persisted OurDepositTxID empty after CreatedA")
	}

	// Crash.
	_ = n.Close()

	// Restart from the same DataDir (restore BEFORE start so the empty loop
	// cannot clobber the recovered file).
	n2 := &Node{
		config:   &Config{DataDir: dir, Confs: confs, Connectors: conns},
		store:    NewStore(),
		signer:   crypto.NewBtcSigner(),
		sessions: map[string]*SwapSession{},
		stop:     make(chan struct{}),
	}
	n2.restoreLocalSwaps(dir)
	cc2 := &captureXConn{}
	n2.conn = cc2
	n2.start()
	t.Cleanup(func() { _ = n2.Close() })
	registerHub(t, n2, hubPriv)

	s2 := n2.sessions[hexEncode(orderID[:])]
	if s2 == nil {
		t.Fatal("session not recovered from disk after restart")
	}
	if got := readOnEngine(t, n2, func() clientState { return s2.state }); got != csCreatedA {
		t.Fatalf("recovered state = %v, want csCreatedA", got)
	}
	if got := readOnEngine(t, n2, func() string { return s2.ourDepositTxID }); got != ps[0].OurDepositTxID {
		t.Fatalf("recovered ourDepositTxID = %q, want %q", got, ps[0].OurDepositTxID)
	}
	if len(n2.store.List()) != 1 || len(n2.sessions) != 1 {
		t.Fatalf("recovered store=%d sessions=%d, want 1/1 (no orphan)", len(n2.store.List()), len(n2.sessions))
	}

	// Hub retransmit after restart: state guard must drop it.
	dispatch(n2)
	if got := btcConn.broadcastSnapshot(); len(got) != 1 {
		t.Fatalf("retransmit re-broadcast the deposit: %d broadcasts, want 1", len(got))
	}
	var createdA int
	for _, p := range cc2.snapshot() {
		if p.Command == proto.XbcTransactionCreatedA {
			createdA++
		}
	}
	if createdA != 0 {
		t.Fatalf("%d CreatedA responses sent after restart retransmit, want 0", createdA)
	}
}

// TestRestartRecoversConfirmedB crashes a taker after it has claimed the maker's
// deposit (ConfirmedB), then restarts and asserts the csConfirmedB session is
// recovered and a retransmitted ConfirmB is dropped by the state guard — so no
// second claim is broadcast.
func TestRestartRecoversConfirmedB(t *testing.T) {
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

	tkPriv, tkPub := newKey(t)
	mkPub := make([]byte, 33)
	mkPub[0] = 0x03
	copy(mkPub[1:], tkPub[1:])

	var secret [33]byte
	for i := range secret {
		secret[i] = byte(i + 1)
	}
	secretHash := coins.KeyID(secret[:])

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

	dir := t.TempDir()
	n, cc := newStartedPersistNode(t, dir, confs, conns)

	var orderID [32]byte
	oid := hash20("order-id")
	copy(orderID[:], oid[:])
	tkAddr := addrFor(48, "taker-ltc-source")
	btcDest := addrFor(0, "maker-btc-dest")
	n.newTakerSession(withUsedCoins(t, n, &Order{ID: orderID, FromCurrency: "BTC", ToCurrency: "LTC", FromAmount: 2.5e6, ToAmount: 2e6, Mine: true}, []wallet.Utxo{ltcConn.funding}), TakeOrderParams{FromAddress: tkAddr, ToAddress: btcDest}, arr32(tkPriv), to33(tkPub))
	s := n.sessions[hexEncode(orderID[:])]
	s.theirSecretHash = secretHash
	s.theirDepositTxID = strings.Repeat("cc", 32)
	s.theirLockTime = 1030
	s.theirPub = to33(mkPub)
	s.theirDepositVout = 0
	s.theirP2SHNative = 2.5e8
	// The fixture payTx spends outpoint ee:0: that is the taker's own LTC
	// deposit (C++ binTxId/binTxVout), which the maker redeemed to reveal
	// the secret. Extraction binds to it (xbridgesession.cpp:3935).
	s.ourDepositTxID = strings.Repeat("ee", 32)
	// The validated maker BTC deposit must exist on-chain for the claim-path
	// unspent re-check (in production it was broadcast at CreateA and
	// validated at CreateB; this fixture seeds validation directly).
	ccTx := &coins.Tx{Version: 1}
	ccTx.Inputs = append(ccTx.Inputs, coins.TxIn{Sequence: 0xffffffff})
	ccTx.Outputs = append(ccTx.Outputs, coins.TxOut{Value: 2.5e8, ScriptPubKey: []byte{0x51}})
	btcConn.setRawTx(strings.Repeat("cc", 32), hex.EncodeToString(ccTx.Serialize()))

	drive := func(node *Node, sess *SwapSession) {
		node.submit(func() {
			if _, _, err := sess.OnConfirmB(&proto.ConfirmBBody{HubAddress: [20]byte{}, ID: orderID, APayTxID: makerPayTxID}); err != nil {
				t.Errorf("OnConfirmB: %v", err)
			}
		}, true)
	}
	drive(n, s)
	waitForPacket(t, cc, proto.XbcTransactionConfirmedB, 5*time.Second)
	if got := btcConn.broadcastSnapshot(); len(got) != 1 {
		t.Fatalf("claim broadcast %d times, want 1", len(got))
	}

	ps, _, err := loadSwaps(swapStatePath(dir))
	if err != nil {
		t.Fatalf("loadSwaps: %v", err)
	}
	if len(ps) != 1 || ps[0].State != csConfirmedB {
		t.Fatalf("persisted = %d swaps state %v, want 1/csConfirmedB", len(ps), ps[0].State)
	}

	_ = n.Close()

	n2 := &Node{
		config:   &Config{DataDir: dir, Confs: confs, Connectors: conns},
		store:    NewStore(),
		signer:   crypto.NewBtcSigner(),
		sessions: map[string]*SwapSession{},
		stop:     make(chan struct{}),
	}
	n2.restoreLocalSwaps(dir)
	cc2 := &captureXConn{}
	n2.conn = cc2
	n2.start()
	t.Cleanup(func() { _ = n2.Close() })

	s2 := n2.sessions[hexEncode(orderID[:])]
	if s2 == nil || readOnEngine(t, n2, func() clientState { return s2.state }) != csConfirmedB {
		t.Fatal("csConfirmedB session not recovered from disk after restart")
	}

	// Retransmit ConfirmB: state guard drops it — no second claim.
	drive(n2, s2)
	if got := btcConn.broadcastSnapshot(); len(got) != 1 {
		t.Fatalf("retransmit re-broadcast the claim: %d broadcasts, want 1", len(got))
	}
	var confirmedB int
	for _, p := range cc2.snapshot() {
		if p.Command == proto.XbcTransactionConfirmB {
			confirmedB++
		}
	}
	if confirmedB != 0 {
		t.Fatalf("%d ConfirmedB responses sent after restart retransmit, want 0", confirmedB)
	}
}

// TestRestartRecoversPreDeposit crashes a maker whose swap was persisted at the
// pre-deposit stage (the state a swap is in if it crashed before its deposit
// task ran), restarts, and re-drives OnCreateA. The recovered session is at the
// pre-deposit state, so the hub resend triggers exactly one deposit — proving a
// crash before the deposit is recovered to a resumable state and never
// double-broadcasts. (Node.Close waits on wg with no timeout, so the test never
// strands a worker: n1 is crashed with no in-flight task.)
func TestRestartRecoversPreDeposit(t *testing.T) {
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

	dir := t.TempDir()
	n, _ := newStartedPersistNode(t, dir, confs, conns)

	var orderID [32]byte
	copy(orderID[:], []byte("order-id-order-id-order-id-0"))
	mkAddr := addrFor(0, "maker-btc-dest")
	o := &Order{
		ID: orderID, FromCurrency: "BTC", ToCurrency: "BTC", FromAmount: 2.5e6, ToAmount: 2.5e6,
		Mine: true,
	}
	n.newMakerSession(withUsedCoins(t, n, o, []wallet.Utxo{btcConn.funding}), MakeOrderParams{MakerAddress: mkAddr, TakerAddress: mkAddr}, arr32(mPriv), toArr33(mPub))

	// Force a deterministic durable write of the pre-deposit state (no deposit
	// task runs in this test, so nothing else would trigger a persist).
	n.submit(func() { _ = n.persistNow() }, true)

	// The snapshot must be the pre-deposit stage: csMaker, no deposit txid.
	// (no deposit task was started, so the session is at csMaker with no txid).
	deadline := time.Now().Add(5 * time.Second)
	for {
		ps, _, perr := loadSwaps(swapStatePath(dir))
		if perr == nil && len(ps) == 1 && ps[0].State < csCreatedA && ps[0].OurDepositTxID == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pre-deposit snapshot not persisted: ps=%v err=%v", ps, perr)
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Crash with no in-flight worker, so Close returns promptly.
	_ = n.Close()

	// Restart with the same BTC fakeConnector so the replayed deposit can broadcast; cc2 captures only the hub-facing response.
	n2 := &Node{
		config:   &Config{DataDir: dir, Confs: confs, Connectors: map[string]wallet.Connector{"BTC": btcConn}},
		store:    NewStore(),
		signer:   crypto.NewBtcSigner(),
		sessions: map[string]*SwapSession{},
		stop:     make(chan struct{}),
	}
	n2.restoreLocalSwaps(dir)
	cc2 := &captureXConn{}
	n2.conn = cc2
	n2.start()
	t.Cleanup(func() { _ = n2.Close() })

	s2 := n2.sessions[hexEncode(orderID[:])]
	if s2 == nil {
		t.Fatal("session not recovered from disk after restart")
	}
	if got := readOnEngine(t, n2, func() clientState { return s2.state }); got >= csCreatedA {
		t.Fatalf("recovered state = %v, want pre-deposit (< csCreatedA)", got)
	}
	if s2.ourDepositTxID != "" {
		t.Fatal("recovered ourDepositTxID non-empty; expected pre-deposit")
	}

	// Hub resend after restart: builds + broadcasts exactly one deposit.
	n2.submit(func() {
		if _, _, err := s2.OnCreateA(&proto.CreateABody{HubAddress: [20]byte{}, ID: orderID, BPubKey: to33(tkPub)}); err != nil {
			t.Errorf("OnCreateA restart: %v", err)
		}
	}, true)
	waitForPacket(t, cc2, proto.XbcTransactionCreatedA, 5*time.Second)

	if got := btcConn.broadcastSnapshot(); len(got) != 1 {
		t.Fatalf("deposit broadcast %d times across crash+restart, want exactly 1", len(got))
	}
}
