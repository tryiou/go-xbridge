package api

// Claim-rebuild tests: a failed ELSE-branch claim build must schedule a
// tick-driven retry instead of stranding the session.
//
// Live-proven hole (BLOCK/LTC order e6730fe2): the taker's one-shot ConfirmB
// claim build fired while the EXR-routed BLOCK backend still could not serve
// the maker's fresh payTx (-5 blind), failed once, and never retried. The hub
// never redelivered ConfirmB, so B sat at `created` with a fully claimable
// HTLC while A finished — manual recovery was the only way out. From now on
// the failed build records its trigger (the counterparty payTx id) and a
// retry timestamp, and the engine tick re-posts the identical build until it
// succeeds or the session ends. All fixtures are in-memory — no live hub.

import (
	"encoding/hex"
	"strings"
	"testing"

	"go-xbridge/coins"
	"go-xbridge/config"
	"go-xbridge/proto"
	"go-xbridge/wallet"
)

// blindTakerFixture drives maker+ taker through ConfirmA (maker reveals its
// secret on-chain), except the taker's LTC connector is a SEPARATE instance
// that never saw the maker's payTx broadcast — the backend-lag blindness that
// stranded e6730fe2. Returns both nodes/sessions, the hub, and the maker payTx.
func blindTakerFixture(t *testing.T) (*Node, *Node, *SwapSession, *SwapSession, [20]byte, [32]byte, string, *fakeConnector, *fakeConnector) {
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
	btcConn := &fakeConnector{ticker: "BTC", funding: btcFunding, fundingPriv: btcFundingPriv,
		fundingPub: btcFundingPub, changeAddr: addrFor(0, "btc-change"), blockHeight: 1000,
		rawTx: map[string]string{}}
	ltcFundingPriv, ltcFundingPub := newKey(t)
	ltcFunding := wallet.Utxo{TxID: strings.Repeat("bb", 32), Vout: 0, Amount: 5e8,
		ScriptPubKey: hex.EncodeToString(coins.BuildP2PKHScript(coins.KeyID(ltcFundingPub)))}
	// Maker-side LTC connector: sees every broadcast (shared out of the
	// maker's node only for its own builds).
	mkLtcConn := &fakeConnector{ticker: "LTC", funding: ltcFunding, fundingPriv: ltcFundingPriv,
		fundingPub: ltcFundingPub, changeAddr: addrFor(48, "ltc-change"), blockHeight: 1000,
		rawTx: map[string]string{}, confirmations: map[string]int{}}
	// Taker-side LTC connector: same funding keys, but a BLIND mempool — the
	// maker's payTx never lands here, exactly like the lagging EXR backend.
	tkLtcConn := &fakeConnector{ticker: "LTC", funding: ltcFunding, fundingPriv: ltcFundingPriv,
		fundingPub: ltcFundingPub, changeAddr: addrFor(48, "ltc-change"), blockHeight: 1000,
		rawTx: map[string]string{}, confirmations: map[string]int{}}
	mkConfs := map[string]*config.CoinConf{
		"BTC": {Ticker: "BTC", Coin: 1e8, AddressPrefix: 0, CreateTxMethod: "BTC", BlockTime: 60},
		"LTC": {Ticker: "LTC", Coin: 1e8, AddressPrefix: 48, CreateTxMethod: "LTC", BlockTime: 60},
	}
	makerNode := newTestNode(t, mkConfs, map[string]wallet.Connector{"BTC": btcConn, "LTC": mkLtcConn})
	takerNode := newTestNode(t, mkConfs, map[string]wallet.Connector{"BTC": btcConn, "LTC": tkLtcConn})

	tkPriv, tkPub := newKey(t)
	mkMPriv, mkMPub := newKey(t)
	var orderID [32]byte
	oid := hash20("claim-retry-order")
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
	btcHash := hash20("maker-btc-dest")
	ltcHash := hash20("taker-ltc-source")
	if _, _, err := makerSession.OnInit(&proto.InitBody{ClientAddress: ltcHash, HubAddress: hub, ID: orderID,
		FromAddress: hash20("maker-btc-dest"), FromCurrency: "BTC", FromAmount: 2.5e6,
		ToAddress: hash20("taker-ltc-source"), ToCurrency: "LTC", ToAmount: 2e6}); err != nil {
		t.Fatalf("maker OnInit: %v", err)
	}
	if _, _, err := takerSession.OnInit(&proto.InitBody{ClientAddress: btcHash, HubAddress: hub, ID: orderID,
		FromAddress: hash20("taker-ltc-source"), FromCurrency: "LTC", FromAmount: 2e6,
		ToAddress: hash20("maker-btc-dest"), ToCurrency: "BTC", ToAmount: 2.5e6}); err != nil {
		t.Fatalf("taker OnInit: %v", err)
	}
	_, bodyA, err := makerSession.OnCreateA(&proto.CreateABody{HubAddress: hub, ID: orderID, BPubKey: to33(tkPub)})
	if err != nil {
		t.Fatalf("maker OnCreateA: %v", err)
	}
	createdA := bodyA.(*proto.CreatedABody)
	_, bodyB, err := takerSession.OnCreateB(&proto.CreateBBody{HubAddress: hub, ID: orderID,
		APubKey: makerSession.pubkey(), ADepositTxID: createdA.ADepositTxID,
		HashedSecret: createdA.HashedSecret, ALockTime: createdA.ALockTime})
	if err != nil {
		t.Fatalf("taker OnCreateB: %v", err)
	}
	createdB := bodyB.(*proto.CreatedBBody)
	// Same chain, separate mempools: the maker learns the taker's deposit
	// broadcast (chain view syncs), but the taker never learns the maker's
	// payTx (backend lag — the e6730fe2 blindness).
	for id, hx := range tkLtcConn.rawTx {
		mkLtcConn.rawTx[id] = hx
		mkLtcConn.confirmations[id] = 1
	}
	_, bodyCA, err := makerSession.OnConfirmA(&proto.ConfirmABody{HubAddress: hub, ID: orderID,
		BDepositTxID: createdB.BDepositTxID, BLockTime: createdB.BLockTime})
	if err != nil {
		t.Fatalf("maker OnConfirmA: %v", err)
	}
	makerPayTxID := bodyCA.(*proto.ConfirmedABody).APayTxID
	if makerPayTxID == "" {
		t.Fatal("empty maker payTx id")
	}
	return makerNode, takerNode, makerSession, takerSession, hub, orderID, makerPayTxID, tkLtcConn, mkLtcConn
}

// TestTakerClaimRetryScheduledOnBuildFailure pins the e6730fe2 lesson: when
// the ConfirmB claim build fails (backend blind to the maker's payTx), the
// session must record the trigger payTx id and a retry timestamp instead of
// stranding at created with no recovery path.
func TestTakerClaimRetryScheduledOnBuildFailure(t *testing.T) {
	_, takerNode, _, takerSession, hub, orderID, makerPayTxID, _, _ := blindTakerFixture(t)
	_ = takerNode
	if _, _, err := takerSession.OnConfirmB(&proto.ConfirmBBody{HubAddress: hub, ID: orderID, APayTxID: makerPayTxID}); err == nil {
		t.Fatal("blind-backend ConfirmB unexpectedly succeeded")
	}
	if takerSession.claimTxID != "" {
		t.Fatal("claimTxID set despite failed build")
	}
	if takerSession.theirPayTxID != makerPayTxID {
		t.Fatalf("theirPayTxID = %q, want maker payTx %q", takerSession.theirPayTxID, makerPayTxID)
	}
	if takerSession.claimRetryAt == 0 {
		t.Fatal("claimRetryAt not scheduled after failed claim build")
	}
	if takerSession.claimRetryAt <= NowMicro() {
		t.Fatal("claimRetryAt not in the future (no backoff)")
	}
}

// TestTakerClaimRetryRedrivesAndClears pins the recovery: once the backend
// catches up (payTx visible) and the retry time passes, the engine tick
// re-posts the identical build, the claim broadcasts, and the retry clears.
func TestTakerClaimRetryRedrivesAndClears(t *testing.T) {
	_, takerNode, _, takerSession, hub, orderID, makerPayTxID, tkLtcConn, mkLtcConn := blindTakerFixture(t)
	if _, _, err := takerSession.OnConfirmB(&proto.ConfirmBBody{HubAddress: hub, ID: orderID, APayTxID: makerPayTxID}); err == nil {
		t.Fatal("blind-backend ConfirmB unexpectedly succeeded")
	}
	// Backend catches up: the maker's payTx becomes visible. The maker's LTC
	// connector recorded every broadcast it made; copy the payTx across.
	hx, ok := mkLtcConn.rawTx[makerPayTxID]
	if !ok {
		t.Fatal("maker payTx missing from maker connector")
	}
	tkLtcConn.rawTx[makerPayTxID] = hx
	// Force the retry due and run the tick sweep.
	takerSession.claimRetryAt = 1
	takerNode.retryFailedClaimBuilds(NowMicro())
	if takerSession.claimTxID == "" {
		t.Fatal("retry did not build the claim")
	}
	if takerSession.claimRetryAt != 0 {
		t.Fatal("retry timestamp not cleared after successful rebuild")
	}
	// C++ trader parity (xbridgesession.cpp:3185): the successful claim
	// broadcast is the finish.
	if takerSession.state != csFinished {
		t.Fatalf("state = %s, want finished", takerSession.state.String())
	}
}

// TestMakerClaimRetryScheduledOnBuildFailure pins the maker-side half: a
// ConfirmA build that fails for a TRANSIENT reason (the taker's deposit is
// unknown to our backend yet — ErrDepositNotReady, never a self-cancel
// rejection) schedules a retry instead of stranding at createdA.
func TestMakerClaimRetryScheduledOnBuildFailure(t *testing.T) {
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
		rawTx: map[string]string{}}
	// Maker-side LTC connector: healthy but BLIND to the taker's deposit
	// (never broadcast through it) — the check waits transiently instead of
	// rejecting.
	ltcConn := &fakeConnector{ticker: "LTC", funding: btcFunding, fundingPriv: btcFundingPriv,
		fundingPub: btcFundingPub, changeAddr: addrFor(48, "ltc-change"), blockHeight: 1000,
		rawTx: map[string]string{}}
	confs := map[string]*config.CoinConf{
		"BTC": {Ticker: "BTC", Coin: 1e8, AddressPrefix: 0, CreateTxMethod: "BTC", BlockTime: 60},
		"LTC": {Ticker: "LTC", Coin: 1e8, AddressPrefix: 48, CreateTxMethod: "LTC", BlockTime: 60},
	}
	makerNode := newTestNode(t, confs, map[string]wallet.Connector{"BTC": btcConn, "LTC": ltcConn})
	mkMPriv, mkMPub := newKey(t)
	_, tkMPub := newKey(t)
	var orderID [32]byte
	moid := hash20("maker-retry-order")
	copy(orderID[:], moid[:])
	mkOrder := &Order{ID: orderID, FromCurrency: "BTC", ToCurrency: "LTC", FromAmount: 2.5e6, ToAmount: 2e6}
	makerNode.newMakerSession(withUsedCoins(t, makerNode, mkOrder, []wallet.Utxo{btcFunding}),
		MakeOrderParams{MakerAddress: addrFor(0, "m"), TakerAddress: addrFor(48, "t")}, arr32(mkMPriv), toArr33(mkMPub))
	s := makerNode.sessions[hexEncode(orderID[:])]
	var hub [20]byte
	hb := hash20("hub")
	copy(hub[:], hb[:])
	s.hub = hub
	// Maker deposits first (state createdA), then learns of a taker deposit
	// its backend has never seen. BLockTime mirrors the fixture chain height
	// (blockHeight 1000 → lock ~1030) so the drift gate passes and the
	// failure lands in the transient deposit check, not self-cancel.
	if _, _, err := s.OnCreateA(&proto.CreateABody{HubAddress: hub, ID: orderID, BPubKey: to33(tkMPub)}); err != nil {
		t.Fatalf("maker OnCreateA: %v", err)
	}
	if _, _, err := s.OnConfirmA(&proto.ConfirmABody{HubAddress: hub, ID: orderID,
		BDepositTxID: strings.Repeat("dd", 32), BLockTime: 1030}); err == nil {
		t.Fatal("blind-deposit ConfirmA unexpectedly succeeded")
	}
	if s.claimTxID != "" {
		t.Fatal("claimTxID set despite failed build")
	}
	if s.claimRetryAt == 0 {
		t.Fatal("claimRetryAt not scheduled after failed maker claim build")
	}
}

// TestMakerClaimLocktimeZeroRetriesNotCanceled pins the S5 lesson (BLOCK/PIVX
// order 9698af09, 2026-09-14): a locktime EXPECTATION that cannot be computed
// (wallet outage: nil connector, failed/empty GetBlockCount) is a TRANSIENT
// condition on OUR side, never a counterparty fault — the build must schedule
// the standard claim retry instead of wire-cancelling with crBadBLockTime.
// C++ cancels instantly on any non-VERIFY_ERROR redeem failure
// (xbridgesession.cpp:2985-2995) and killed a healthy funded swap that way;
// the drift check itself still rejects genuinely-bad locktimes.
func TestMakerClaimLocktimeZeroRetriesNotCanceled(t *testing.T) {
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
		rawTx: map[string]string{}}
	// Maker-side LTC connector: healthy wallet, but its chain height read
	// returns 0 (outage) — computeLockTimeFor therefore returns 0.
	ltcConn := &fakeConnector{ticker: "LTC", funding: btcFunding, fundingPriv: btcFundingPriv,
		fundingPub: btcFundingPub, changeAddr: addrFor(48, "ltc-change"), blockHeight: 0,
		rawTx: map[string]string{}}
	confs := map[string]*config.CoinConf{
		"BTC": {Ticker: "BTC", Coin: 1e8, AddressPrefix: 0, CreateTxMethod: "BTC", BlockTime: 60},
		"LTC": {Ticker: "LTC", Coin: 1e8, AddressPrefix: 48, CreateTxMethod: "LTC", BlockTime: 60},
	}
	makerNode := newTestNode(t, confs, map[string]wallet.Connector{"BTC": btcConn, "LTC": ltcConn})
	mkMPriv, mkMPub := newKey(t)
	_, tkMPub := newKey(t)
	var orderID [32]byte
	moid := hash20("maker-lt0-order")
	copy(orderID[:], moid[:])
	mkOrder := &Order{ID: orderID, FromCurrency: "BTC", ToCurrency: "LTC", FromAmount: 2.5e6, ToAmount: 2e6}
	makerNode.newMakerSession(withUsedCoins(t, makerNode, mkOrder, []wallet.Utxo{btcFunding}),
		MakeOrderParams{MakerAddress: addrFor(0, "m"), TakerAddress: addrFor(48, "t")}, arr32(mkMPriv), toArr33(mkMPub))
	s := makerNode.sessions[hexEncode(orderID[:])]
	var hub [20]byte
	hb := hash20("hub")
	copy(hub[:], hb[:])
	s.hub = hub
	if _, _, err := s.OnCreateA(&proto.CreateABody{HubAddress: hub, ID: orderID, BPubKey: to33(tkMPub)}); err != nil {
		t.Fatalf("maker OnCreateA: %v", err)
	}
	// BLockTime 1030 is what a healthy height-1000 chain would produce; the
	// expectation cannot be computed (height 0) — transient, not a drift fault.
	_, _, _ = s.OnConfirmA(&proto.ConfirmABody{HubAddress: hub, ID: orderID,
		BDepositTxID: strings.Repeat("dd", 32), BLockTime: 1030})
	if s.claimRetryAt == 0 {
		t.Fatal("locktime-zero build must schedule a claim retry, not strand")
	}
	if got := makerNode.sessions[hexEncode(orderID[:])]; got == nil || got.state == csIdle {
		t.Fatalf("healthy swap self-cancelled on a wallet outage: session=%v", got)
	}
	if o := makerNode.store.Get(hexEncode(orderID[:])); o == nil || isOrderTerminal(o.Status) {
		t.Fatalf("order must stay live, got %+v", o)
	}
}
