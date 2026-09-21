package api

import (
	"encoding/hex"
	"strings"
	"testing"

	"go-xbridge/coins"
	"go-xbridge/config"
	"go-xbridge/proto"
	"go-xbridge/wallet"
)

// guardSession returns a session pre-positioned at the given client state so a
// guard test can drive a POST-COMPLETION retransmit through a handler without
// any coin/node setup: the state guards fire before any wallet or store
// work, so no connector, registry, or store is needed.
func guardSession(isMaker bool, state clientState) *SwapSession {
	var id [32]byte
	copy(id[:], "order-id-order-id-order-id-0")
	return &SwapSession{isMaker: isMaker, id: id, state: state}
}

// assertRetransmitDropped checks the handler's retransmit contract: it returns
// no response command/body, no error, and leaves the session state and the
// seeded counterparty fields untouched (a retransmit must not re-run stage 1).
func assertRetransmitDropped(t *testing.T, s *SwapSession, cmd proto.XBridgeCommand, body responseBody, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("retransmit errored: %v", err)
	}
	if cmd != 0 || body != nil {
		t.Fatalf("retransmit produced a response (%d, %v), want (0, nil)", cmd, body)
	}
}

// TestCreateAStateGuardDropsPostCompletionRetransmit mirrors C++
// processTransactionCreateA's `state >= trCreated` guard
// (xbridgesession.cpp:1947): a CreateA arriving after the maker's deposit was
// already created must be dropped without re-broadcasting a second deposit or
// overwriting the recorded counterparty key.
func TestCreateAStateGuardDropsPostCompletionRetransmit(t *testing.T) {
	s := guardSession(true, csCreatedA)
	s.theirPub = [33]byte{7}

	cmd, body, err := s.OnCreateA(&proto.CreateABody{ID: s.id, BPubKey: [33]byte{9}})
	assertRetransmitDropped(t, s, cmd, body, err)
	if s.state != csCreatedA {
		t.Errorf("state regressed to %v", s.state)
	}
	if s.theirPub != [33]byte{7} {
		t.Errorf("counterparty pubkey overwritten by retransmit: %x", s.theirPub)
	}
}

// TestCreateBStateGuardDropsPostCompletionRetransmit mirrors
// processTransactionCreateB's `state >= trCreated` guard (xbridgesession.cpp:2424).
func TestCreateBStateGuardDropsPostCompletionRetransmit(t *testing.T) {
	s := guardSession(false, csCreatedB)
	s.theirPub = [33]byte{7}
	s.theirDepositTxID = "cc"

	cmd, body, err := s.OnCreateB(&proto.CreateBBody{ID: s.id, APubKey: [33]byte{9}, ADepositTxID: "dd"})
	assertRetransmitDropped(t, s, cmd, body, err)
	if s.state != csCreatedB {
		t.Errorf("state regressed to %v", s.state)
	}
	if s.theirPub != [33]byte{7} {
		t.Errorf("counterparty pubkey overwritten by retransmit: %x", s.theirPub)
	}
	if s.theirDepositTxID != "cc" {
		t.Errorf("counterparty deposit overwritten by retransmit: %q", s.theirDepositTxID)
	}
}

// TestConfirmAStateGuardDropsPostCompletionRetransmit mirrors
// processTransactionConfirmA's `state >= trCommited` guard
// (xbridgesession.cpp:2897): a ConfirmA arriving after the maker already
// redeemed must not re-broadcast a second claim payTx.
func TestConfirmAStateGuardDropsPostCompletionRetransmit(t *testing.T) {
	s := guardSession(true, csConfirmedA)
	s.theirDepositTxID = "cc"

	cmd, body, err := s.OnConfirmA(&proto.ConfirmABody{ID: s.id, BDepositTxID: "dd"})
	assertRetransmitDropped(t, s, cmd, body, err)
	if s.state != csConfirmedA {
		t.Errorf("state regressed to %v", s.state)
	}
	if s.theirDepositTxID != "cc" {
		t.Errorf("counterparty deposit overwritten by retransmit: %q", s.theirDepositTxID)
	}
}

// TestConfirmBStateGuardDropsPostCompletionRetransmit mirrors
// processTransactionConfirmB's `state >= trCommited` guard
// (xbridgesession.cpp:3152): a ConfirmB arriving after the taker already
// redeemed must not re-broadcast a second claim payTx.
func TestConfirmBStateGuardDropsPostCompletionRetransmit(t *testing.T) {
	s := guardSession(false, csConfirmedB)

	cmd, body, err := s.OnConfirmB(&proto.ConfirmBBody{ID: s.id, APayTxID: "dd"})
	assertRetransmitDropped(t, s, cmd, body, err)
	if s.state != csConfirmedB {
		t.Errorf("state regressed to %v", s.state)
	}
}

// TestStuckAwaitClearsAndRedrives pins the recovery: a session with a
// long-lost worker result (await set, stamp 10 min old, retry pointers
// complete) gets its guard released and its deposit rebuild re-driven on the
// same tick pass.
func TestStuckAwaitClearsAndRedrives(t *testing.T) {
	if err := coins.InitFromConf(map[string]*config.CoinConf{
		"BTC": {Ticker: "BTC", Title: "Bitcoin", Coin: 1e8, AddressPrefix: 0, ScriptPrefix: 5, CreateTxMethod: "BTC", BlockTime: 60},
		"LTC": {Ticker: "LTC", Title: "Litecoin", Coin: 1e8, AddressPrefix: 48, ScriptPrefix: 50, CreateTxMethod: "LTC", BlockTime: 60},
	}); err != nil {
		t.Fatal(err)
	}
	// Taker side blind to the maker deposit: any rebuild fails transiently
	// and reschedules (backend lag), proving the re-drive ran.
	btcFundingPriv, btcFundingPub := newKey(t)
	btcFunding := wallet.Utxo{TxID: strings.Repeat("aa", 32), Vout: 0, Amount: 5e8,
		ScriptPubKey: hex.EncodeToString(coins.BuildP2PKHScript(coins.KeyID(btcFundingPub)))}
	btcConn := &fakeConnector{ticker: "BTC", funding: btcFunding, fundingPriv: btcFundingPriv,
		fundingPub: btcFundingPub, changeAddr: addrFor(0, "btc-change"), blockHeight: 1000,
		rawTx: map[string]string{}, confirmations: map[string]int{}}
	ltcFundingPriv, ltcFundingPub := newKey(t)
	ltcFunding := wallet.Utxo{TxID: strings.Repeat("bb", 32), Vout: 0, Amount: 5e8,
		ScriptPubKey: hex.EncodeToString(coins.BuildP2PKHScript(coins.KeyID(ltcFundingPub)))}
	ltcConn := &fakeConnector{ticker: "LTC", funding: ltcFunding, fundingPriv: ltcFundingPriv,
		fundingPub: ltcFundingPub, changeAddr: addrFor(48, "ltc-change"), blockHeight: 1000,
		rawTx: map[string]string{}, confirmations: map[string]int{}}
	confs := map[string]*config.CoinConf{
		"BTC": {Ticker: "BTC", Coin: 1e8, AddressPrefix: 0, CreateTxMethod: "BTC", BlockTime: 60},
		"LTC": {Ticker: "LTC", Coin: 1e8, AddressPrefix: 48, CreateTxMethod: "LTC", BlockTime: 60},
	}
	n := newTestNode(t, confs, map[string]wallet.Connector{"BTC": btcConn, "LTC": ltcConn})
	cc := &captureXConn{}
	n.conn = cc

	tkPriv, tkPub := newKey(t)
	_, mkPub := newKey(t)
	var orderID [32]byte
	oid := hash20("stuck-await-order")
	copy(orderID[:], oid[:])
	takerOrder := &Order{ID: orderID, FromCurrency: "BTC", ToCurrency: "LTC",
		FromAmount: 2.5e6, ToAmount: 2e6, Status: "accepting"}
	n.newTakerSession(withUsedCoins(t, n, takerOrder, []wallet.Utxo{ltcFunding}),
		TakeOrderParams{FromAddress: addrFor(48, "taker-ltc"), ToAddress: addrFor(0, "taker-btc")},
		arr32(tkPriv), to33(tkPub))
	s := n.sessions[hexEncode(orderID[:])]
	// Complete pre-deposit pointers, retry due, but the guard claims a worker
	// is still in flight from 10 minutes ago (result lost).
	s.theirPub = to33(mkPub)
	s.theirDepositTxID = strings.Repeat("dd", 32)
	s.theirSecretHash = hash20("secret-hash")
	s.theirLockTime = 1115
	s.state = csInitialized
	s.depositRetryAt = 1
	s.lastProgress = NowMicro()
	s.await = true
	s.awaitSince = NowMicro() - 10*60*1000000

	for _, st := range n.tickStages() {
		st.run()
	}
	if s.await {
		t.Fatal("stuck guard not released: session still claims a worker in flight")
	}
	// The re-drive must have actually run (rescheduled into the future),
	// not merely been unblocked: a released-but-undriven guard would leave
	// the old due timestamp behind.
	if s.depositRetryAt <= NowMicro() {
		t.Fatal("guard released but re-drive did not run (retry still due in the past)")
	}
	for _, p := range cc.snapshot() {
		if p.Command == proto.XbcTransactionCancel {
			t.Fatal("unexpected Cancel sent for a recovering session")
		}
	}
}

// TestFreshAwaitUntouched pins the safety side: a recently-set guard (worker
// plausibly still running) must survive the tick untouched.
func TestFreshAwaitUntouched(t *testing.T) {
	n := newTestNode(t, map[string]*config.CoinConf{}, map[string]wallet.Connector{})
	var id [32]byte
	fh := hash20("fresh-await-order")
	copy(id[:], fh[:])
	mPriv, mPub := newKey(t)
	o := &Order{ID: id, FromCurrency: "BTC", ToCurrency: "LTC", FromAmount: 1e6, ToAmount: 1e6}
	n.newMakerSession(o, MakeOrderParams{MakerAddress: "m", TakerAddress: "t"}, arr32(mPriv), toArr33(mPub))
	s := n.sessions[hexEncode(id[:])]
	s.holdAwait()
	for _, st := range n.tickStages() {
		st.run()
	}
	if !s.await {
		t.Fatal("fresh guard cleared: worker still plausibly in flight")
	}
}
