package api

// Deposit-rebuild tests: a failed HTLC deposit build must schedule a
// tick-driven retry instead of stranding the session.
//
// Live-proven hole (BLOCK/DOGE order d4df334e): the taker's CreateB deposit
// build failed validation while the EXR-routed DOGE backend still could not
// serve the maker's fresh deposit (-5 blind). The hub's redelivery burst
// ended after seconds and nothing re-drove the build, so the taker sat at
// `initialized` with the maker parked at `created` — the same one-shot class
// as the claim hole closed in swap_retry.go, one stage earlier. From now on
// the failed build records a retry timestamp, and the engine tick re-posts
// the identical build until it succeeds or the session ends. All fixtures
// are in-memory — no live hub.

import (
	"encoding/hex"
	"strings"
	"testing"

	"go-xbridge/coins"
	"go-xbridge/config"
	"go-xbridge/proto"
	"go-xbridge/wallet"
)

// blindDepositFixture drives maker + taker through the maker's deposit
// broadcast, except the taker's BTC connector is a SEPARATE instance that
// never saw the maker's deposit — the backend-lag blindness that stranded
// d4df334e. Returns nodes, sessions, hub, order id, and both connectors.
func blindDepositFixture(t *testing.T) (*Node, *Node, *SwapSession, *SwapSession, [20]byte, [32]byte, *proto.CreatedABody, *fakeConnector, *fakeConnector) {
	t.Helper()
	if err := coins.InitFromConf(map[string]*config.CoinConf{
		"BTC": {Ticker: "BTC", Title: "Bitcoin", Coin: 1e8, AddressPrefix: 0, ScriptPrefix: 5, CreateTxMethod: "BTC", BlockTime: 60},
		"LTC": {Ticker: "LTC", Title: "Litecoin", Coin: 1e8, AddressPrefix: 48, ScriptPrefix: 50, CreateTxMethod: "LTC", BlockTime: 60},
	}); err != nil {
		t.Fatal(err)
	}
	btcFundingPriv, btcFundingPub := newKey(t)
	btcFunding := wallet.Utxo{TxID: strings.Repeat("aa", 32), Vout: 0, Amount: 5e8,
		ScriptPubKey: hex.EncodeToString(coins.BuildP2PKHScript(coins.KeyID(btcFundingPub)))}
	mkBtcConn := &fakeConnector{ticker: "BTC", funding: btcFunding, fundingPriv: btcFundingPriv,
		fundingPub: btcFundingPub, changeAddr: addrFor(0, "btc-change"), blockHeight: 1000,
		rawTx: map[string]string{}, confirmations: map[string]int{}}
	ltcFundingPriv, ltcFundingPub := newKey(t)
	ltcFunding := wallet.Utxo{TxID: strings.Repeat("bb", 32), Vout: 0, Amount: 5e8,
		ScriptPubKey: hex.EncodeToString(coins.BuildP2PKHScript(coins.KeyID(ltcFundingPub)))}
	ltcConn := &fakeConnector{ticker: "LTC", funding: ltcFunding, fundingPriv: ltcFundingPriv,
		fundingPub: ltcFundingPub, changeAddr: addrFor(48, "ltc-change"), blockHeight: 1000,
		rawTx: map[string]string{}, confirmations: map[string]int{}}
	// Taker-side BTC connector: same funding keys, BLIND mempool — the
	// maker's deposit never lands here.
	tkBtcConn := &fakeConnector{ticker: "BTC", funding: btcFunding, fundingPriv: btcFundingPriv,
		fundingPub: btcFundingPub, changeAddr: addrFor(0, "btc-change"), blockHeight: 1000,
		rawTx: map[string]string{}, confirmations: map[string]int{}}
	mkConfs := map[string]*config.CoinConf{
		"BTC": {Ticker: "BTC", Coin: 1e8, AddressPrefix: 0, CreateTxMethod: "BTC", BlockTime: 60},
		"LTC": {Ticker: "LTC", Coin: 1e8, AddressPrefix: 48, CreateTxMethod: "LTC", BlockTime: 60},
	}
	makerNode := newTestNode(t, mkConfs, map[string]wallet.Connector{"BTC": mkBtcConn, "LTC": ltcConn})
	takerNode := newTestNode(t, mkConfs, map[string]wallet.Connector{"BTC": tkBtcConn, "LTC": ltcConn})

	tkPriv, tkPub := newKey(t)
	mkMPriv, mkMPub := newKey(t)
	var orderID [32]byte
	oid := hash20("deposit-retry-order")
	copy(orderID[:], oid[:])

	mkAddr := addrFor(0, "maker-btc-dest")
	ltcAddr := addrFor(48, "taker-ltc-source")
	makerOrder := &Order{ID: orderID, FromCurrency: "BTC", ToCurrency: "LTC", FromAmount: 2.5e6, ToAmount: 2e6}
	takerOrder := &Order{ID: orderID, FromCurrency: "BTC", ToCurrency: "LTC", FromAmount: 2.5e6, ToAmount: 2e6}
	makerNode.newMakerSession(withUsedCoins(t, makerNode, makerOrder, []wallet.Utxo{btcFunding}),
		MakeOrderParams{MakerAddress: mkAddr, TakerAddress: ltcAddr}, arr32(mkMPriv), toArr33(mkMPub))
	takerNode.newTakerSession(withUsedCoins(t, takerNode, takerOrder, []wallet.Utxo{ltcFunding}),
		TakeOrderParams{FromAddress: ltcAddr, ToAddress: mkAddr}, arr32(tkPriv), to33(tkPub))

	var hub [20]byte
	hb := hash20("hub")
	copy(hub[:], hb[:])
	makerSession := makerNode.sessions[hexEncode(orderID[:])]
	takerSession := takerNode.sessions[hexEncode(orderID[:])]
	makerSession.hub = hub
	takerSession.hub = hub

	if _, _, err := makerSession.OnHold(&proto.HoldBody{HubAddress: hub, ID: orderID, FromAmount: 2e6, ToAmount: 2.5e6}); err != nil {
		t.Fatalf("maker OnHold: %v", err)
	}
	if _, _, err := takerSession.OnHold(&proto.HoldBody{HubAddress: hub, ID: orderID, FromAmount: 2e6, ToAmount: 2.5e6}); err != nil {
		t.Fatalf("taker OnHold: %v", err)
	}
	if _, _, err := makerSession.OnInit(&proto.InitBody{ClientAddress: hash20("taker-ltc-source"), HubAddress: hub, ID: orderID,
		FromAddress: hash20("maker-btc-dest"), FromCurrency: "BTC", FromAmount: 2.5e6,
		ToAddress: hash20("taker-ltc-source"), ToCurrency: "LTC", ToAmount: 2e6}); err != nil {
		t.Fatalf("maker OnInit: %v", err)
	}
	if _, _, err := takerSession.OnInit(&proto.InitBody{ClientAddress: hash20("maker-btc-dest"), HubAddress: hub, ID: orderID,
		FromAddress: hash20("taker-ltc-source"), FromCurrency: "LTC", FromAmount: 2e6,
		ToAddress: hash20("maker-btc-dest"), ToCurrency: "BTC", ToAmount: 2.5e6}); err != nil {
		t.Fatalf("taker OnInit: %v", err)
	}
	_, bodyA, err := makerSession.OnCreateA(&proto.CreateABody{HubAddress: hub, ID: orderID, BPubKey: to33(tkPub)})
	if err != nil {
		t.Fatalf("maker OnCreateA: %v", err)
	}
	createdA := bodyA.(*proto.CreatedABody)
	if createdA.ADepositTxID == "" {
		t.Fatal("empty maker deposit txid")
	}
	return makerNode, takerNode, makerSession, takerSession, hub, orderID, createdA, mkBtcConn, tkBtcConn
}

// TestTakerDepositRetryScheduledOnBuildFailure pins the d4df334e lesson:
// when the CreateB deposit build fails (backend blind to the maker's
// deposit), the session records a retry timestamp instead of stranding at
// initialized with no recovery path.
func TestTakerDepositRetryScheduledOnBuildFailure(t *testing.T) {
	_, _, makerSession, takerSession, hub, orderID, createdA, _, _ := blindDepositFixture(t)
	_, _, err := takerSession.OnCreateB(&proto.CreateBBody{HubAddress: hub, ID: orderID,
		APubKey: makerSession.pubkey(), ADepositTxID: createdA.ADepositTxID,
		HashedSecret: createdA.HashedSecret, ALockTime: createdA.ALockTime})
	if err == nil {
		t.Fatal("blind-backend CreateB unexpectedly succeeded")
	}
	if takerSession.ourDepositTxID != "" {
		t.Fatal("ourDepositTxID set despite failed build")
	}
	if takerSession.theirDepositTxID != createdA.ADepositTxID {
		t.Fatal("counterparty deposit pointer not recorded for retry")
	}
	if takerSession.depositRetryAt == 0 {
		t.Fatal("depositRetryAt not scheduled after failed deposit build")
	}
	if takerSession.depositRetryAt <= NowMicro() {
		t.Fatal("depositRetryAt not in the future (no backoff)")
	}
}

// TestTakerDepositRetryRedrivesAndClears pins the recovery: once the backend
// catches up (maker deposit visible) and the retry time passes, the engine
// tick re-posts the identical build, our deposit broadcasts, and the retry
// clears.
func TestTakerDepositRetryRedrivesAndClears(t *testing.T) {
	_, takerNode, makerSession, takerSession, hub, orderID, createdA, mkBtcConn, tkBtcConn := blindDepositFixture(t)
	_, _, err := takerSession.OnCreateB(&proto.CreateBBody{HubAddress: hub, ID: orderID,
		APubKey: makerSession.pubkey(), ADepositTxID: createdA.ADepositTxID,
		HashedSecret: createdA.HashedSecret, ALockTime: createdA.ALockTime})
	if err == nil {
		t.Fatal("blind-backend CreateB unexpectedly succeeded")
	}
	// Backend catches up: sync the maker's deposit broadcast across.
	for id, hx := range mkBtcConn.rawTx {
		tkBtcConn.rawTx[id] = hx
		tkBtcConn.confirmations[id] = 1
	}
	// Force the retry due and run the tick sweep.
	takerSession.depositRetryAt = 1
	takerNode.retryFailedDepositBuilds(NowMicro())
	if takerSession.ourDepositTxID == "" {
		t.Fatal("retry did not build our deposit")
	}
	if takerSession.depositRetryAt != 0 {
		t.Fatal("retry timestamp not cleared after successful rebuild")
	}
	if takerSession.state != csCreatedB {
		t.Fatalf("state = %s, want createdB", takerSession.state.String())
	}
}

// TestMakerDepositRetryRedrivesAndClears pins the maker-side half: a CreateA
// build that fails transiently (no connector for the funding chain yet)
// schedules a retry, and the tick sweep completes the deposit once the
// connector appears — no hub retransmit needed.
func TestMakerDepositRetryRedrivesAndClears(t *testing.T) {
	if err := coins.InitFromConf(map[string]*config.CoinConf{
		"BTC": {Ticker: "BTC", Title: "Bitcoin", Coin: 1e8, AddressPrefix: 0, ScriptPrefix: 5, CreateTxMethod: "BTC", BlockTime: 60},
		"LTC": {Ticker: "LTC", Title: "Litecoin", Coin: 1e8, AddressPrefix: 48, ScriptPrefix: 50, CreateTxMethod: "LTC", BlockTime: 60},
	}); err != nil {
		t.Fatal(err)
	}
	btcFundingPriv, btcFundingPub := newKey(t)
	btcFunding := wallet.Utxo{TxID: strings.Repeat("cc", 32), Vout: 0, Amount: 5e8,
		ScriptPubKey: hex.EncodeToString(coins.BuildP2PKHScript(coins.KeyID(btcFundingPub)))}
	btcConn := &fakeConnector{ticker: "BTC", funding: btcFunding, fundingPriv: btcFundingPriv,
		fundingPub: btcFundingPub, changeAddr: addrFor(0, "btc-change"), blockHeight: 1000,
		rawTx: map[string]string{}, confirmations: map[string]int{}}
	confs := map[string]*config.CoinConf{
		"BTC": {Ticker: "BTC", Coin: 1e8, AddressPrefix: 0, CreateTxMethod: "BTC", BlockTime: 60},
		"LTC": {Ticker: "LTC", Coin: 1e8, AddressPrefix: 48, CreateTxMethod: "LTC", BlockTime: 60},
	}
	conns := map[string]wallet.Connector{}
	makerNode := newTestNode(t, confs, conns)
	mkMPriv, mkMPub := newKey(t)
	_, tkMPub := newKey(t)
	var orderID [32]byte
	oid := hash20("maker-deposit-retry-order")
	copy(orderID[:], oid[:])
	mkOrder := &Order{ID: orderID, FromCurrency: "BTC", ToCurrency: "LTC", FromAmount: 2.5e6, ToAmount: 2e6}
	makerNode.newMakerSession(withUsedCoins(t, makerNode, mkOrder, []wallet.Utxo{btcFunding}),
		MakeOrderParams{MakerAddress: addrFor(0, "m"), TakerAddress: addrFor(48, "t")}, arr32(mkMPriv), toArr33(mkMPub))
	s := makerNode.sessions[hexEncode(orderID[:])]
	var hub [20]byte
	hb := hash20("hub")
	copy(hub[:], hb[:])
	s.hub = hub
	if _, _, err := s.OnCreateA(&proto.CreateABody{HubAddress: hub, ID: orderID, BPubKey: to33(tkMPub)}); err == nil {
		t.Fatal("connector-less CreateA unexpectedly succeeded")
	}
	if s.ourDepositTxID != "" {
		t.Fatal("ourDepositTxID set despite failed build")
	}
	if s.depositRetryAt == 0 {
		t.Fatal("depositRetryAt not scheduled after failed maker deposit build")
	}
	// Connector appears (wallet comes online); the tick sweep rebuilds.
	conns["BTC"] = btcConn
	s.depositRetryAt = 1
	makerNode.retryFailedDepositBuilds(NowMicro())
	if s.ourDepositTxID == "" {
		t.Fatal("retry did not build our deposit")
	}
	if s.depositRetryAt != 0 {
		t.Fatal("retry timestamp not cleared after successful rebuild")
	}
	if s.state != csCreatedA {
		t.Fatalf("state = %s, want createdA", s.state.String())
	}
}
