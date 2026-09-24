package api

// Not-ready fast-lane tests: a build that fails because the counterparty
// deposit is not yet wallet-visible (ErrDepositNotReady, the -5 blindness
// behind every slow live swap) must re-poll on the fast 5 s ticker instead
// of burning the 60 s → 120 s failure backoff.
//
// Live-proven waste (BLOCK/PIVX 2026-09-23): deposit-B build fired 0.27 s
// after P2P-notify → -5 → 62 s backoff burn; claim build fired 0.3 s after
// deposit-B → -5 → 127 s burn. ~190 s of a 205 s swap. The fix separates
// "not yet visible" (re-poll soon, no backoff) from "genuinely failed"
// (backoff), bounded by a fast window after which the old backoff resumes.

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"

	"go-xbridge/coins"
	"go-xbridge/config"
	"go-xbridge/proto"
	"go-xbridge/wallet"
)

// notReadyMakerFixture mirrors TestMakerClaimRetryScheduledOnBuildFailure: a
// maker at createdA whose backend has never seen the taker's deposit.
func notReadyMakerFixture(t *testing.T) (*Node, *SwapSession, [20]byte, [32]byte) {
	t.Helper()
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
	moid := hash20("notready-maker-order")
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
	return makerNode, s, hub, orderID
}

// TestNotReadyClaimFastLane: a ConfirmA build failing on an invisible taker
// deposit must schedule a fast re-poll (one 5 s tick), NOT the 60 s backoff,
// and must not consume backoff budget.
func TestNotReadyClaimFastLane(t *testing.T) {
	_, s, hub, orderID := notReadyMakerFixture(t)
	before := NowMicro()
	if _, _, err := s.OnConfirmA(&proto.ConfirmABody{HubAddress: hub, ID: orderID,
		BDepositTxID: strings.Repeat("dd", 32), BLockTime: 1030}); err == nil {
		t.Fatal("blind-deposit ConfirmA unexpectedly succeeded")
	}
	if s.claimTxID != "" {
		t.Fatal("claimTxID set despite failed build")
	}
	if s.claimRetryAt == 0 {
		t.Fatal("claimRetryAt not scheduled after not-ready claim build")
	}
	if s.claimRetryAt <= before {
		t.Fatal("claimRetryAt not in the future")
	}
	if s.claimRetryAt > before+notReadyPollMicro+2*1000000 {
		t.Fatalf("claimRetryAt %d is a backoff delay, want fast re-poll (~5 s)", s.claimRetryAt-before)
	}
	if s.claimRetries != 0 {
		t.Fatalf("claimRetries = %d, want 0 (not-ready consumes no backoff budget)", s.claimRetries)
	}
	if s.notReadySince == 0 {
		t.Fatal("notReadySince not stamped on not-ready failure")
	}
}

// TestNotReadyDepositFastLane: a CreateB build failing on an invisible maker
// deposit must fast-poll, not back off.
func TestNotReadyDepositFastLane(t *testing.T) {
	makerNode, takerNode, makerSession, takerSession, hub, orderID, createdA, _, _ := blindDepositFixture(t)
	_ = makerNode
	_ = takerNode
	_ = makerSession
	before := NowMicro()
	// Unknown A-deposit (never broadcast): the check waits transiently.
	if _, _, err := takerSession.OnCreateB(&proto.CreateBBody{HubAddress: hub, ID: orderID,
		APubKey: makerSession.pubkey(), ADepositTxID: strings.Repeat("ee", 32),
		HashedSecret: createdA.HashedSecret, ALockTime: createdA.ALockTime}); err == nil {
		t.Fatal("blind-deposit CreateB unexpectedly succeeded")
	}
	if takerSession.ourDepositTxID != "" {
		t.Fatal("ourDepositTxID set despite failed build")
	}
	if takerSession.depositRetryAt == 0 {
		t.Fatal("depositRetryAt not scheduled after not-ready deposit build")
	}
	if takerSession.depositRetryAt > before+notReadyPollMicro+2*1000000 {
		t.Fatalf("depositRetryAt %d is a backoff delay, want fast re-poll (~5 s)", takerSession.depositRetryAt-before)
	}
	if takerSession.depositRetries != 0 {
		t.Fatalf("depositRetries = %d, want 0 (not-ready consumes no backoff budget)", takerSession.depositRetries)
	}
	if takerSession.notReadySince == 0 {
		t.Fatal("notReadySince not stamped on not-ready failure")
	}
}

// TestNotReadyWindowFallsBackToBackoff: a deposit that stays invisible past
// the fast window is treated as a genuine failure — backoff delay plus
// backoff budget consumed, with a loud log (no silent spin forever).
func TestNotReadyWindowFallsBackToBackoff(t *testing.T) {
	_, s, hub, orderID := notReadyMakerFixture(t)
	if _, _, err := s.OnConfirmA(&proto.ConfirmABody{HubAddress: hub, ID: orderID,
		BDepositTxID: strings.Repeat("dd", 32), BLockTime: 1030}); err == nil {
		t.Fatal("blind-deposit ConfirmA unexpectedly succeeded")
	}
	if s.notReadySince == 0 {
		t.Fatal("notReadySince not stamped on first not-ready failure")
	}
	// Age the wait past the window, then fail again: must back off.
	s.notReadySince = NowMicro() - notReadyFastWindowMicro - 1000000
	s.claimRetryAt = 0
	before := NowMicro()
	if _, _, err := s.OnConfirmA(&proto.ConfirmABody{HubAddress: hub, ID: orderID,
		BDepositTxID: strings.Repeat("dd", 32), BLockTime: 1030}); err == nil {
		t.Fatal("blind-deposit ConfirmA unexpectedly succeeded")
	}
	if s.claimRetries != 1 {
		t.Fatalf("claimRetries = %d, want 1 (window-exceeded consumes backoff)", s.claimRetries)
	}
	if s.claimRetryAt < before+claimRetryBaseMicro {
		t.Fatalf("claimRetryAt %d is not a backoff delay, want >= 60 s", s.claimRetryAt-before)
	}
	if s.notReadySince == 0 {
		t.Fatal("notReadySince cleared on window fallback: subsequent failures must keep backing off, not re-arm a fresh window")
	}
}

// TestGenuineFailureStillBacksOff pins the unchanged path: a non-sentinel
// build error schedules the classic backoff and consumes budget.
func TestGenuineFailureStillBacksOff(t *testing.T) {
	makerNode, s, _, _ := notReadyMakerFixture(t)
	_ = makerNode
	before := NowMicro()
	s.applyConfirmedA(nil, errors.New("boom"))
	if s.claimRetries != 1 {
		t.Fatalf("claimRetries = %d, want 1", s.claimRetries)
	}
	if s.claimRetryAt < before+claimRetryBaseMicro {
		t.Fatalf("claimRetryAt %d is not a backoff delay, want >= 60 s", s.claimRetryAt-before)
	}
	if s.notReadySince != 0 {
		t.Fatal("notReadySince stamped on a genuine (non-not-ready) failure")
	}
}

// TestNotReadyConfirmBFastLane: a taker claim build failing on a -5 payTx
// fetch (backend hasn't indexed the maker's payTx yet) must fast-poll.
func TestNotReadyConfirmBFastLane(t *testing.T) {
	_, takerNode, _, takerSession, hub, orderID, makerPayTxID, tkLtcConn, _ := blindTakerFixture(t)
	_ = takerNode
	// Production -5 shape for an unindexed payTx (fake's errNotFound is a
	// test-only plain error; the wire carries RPCError -5).
	tkLtcConn.payTxErr = &wallet.RPCError{Code: -5, Message: "No information available about transaction"}
	before := NowMicro()
	if _, _, err := takerSession.OnConfirmB(&proto.ConfirmBBody{HubAddress: hub, ID: orderID, APayTxID: makerPayTxID}); err == nil {
		t.Fatal("blind-payTx ConfirmB unexpectedly succeeded")
	}
	if takerSession.claimRetryAt == 0 {
		t.Fatal("claimRetryAt not scheduled after -5 payTx fetch")
	}
	if takerSession.claimRetryAt > before+notReadyPollMicro+2*1000000 {
		t.Fatalf("claimRetryAt %d is a backoff delay, want fast re-poll (~5 s)", takerSession.claimRetryAt-before)
	}
	if takerSession.claimRetries != 0 {
		t.Fatalf("claimRetries = %d, want 0 (not-ready consumes no backoff budget)", takerSession.claimRetries)
	}
}

// TestNotReadyFutureStampClamped: a future-dated wait stamp (wall-clock
// jump backward, or a stamp persisted by an ahead-clocked run) must not
// fast-poll indefinitely — it clamps to now and the window restarts.
func TestNotReadyFutureStampClamped(t *testing.T) {
	_, s, hub, orderID := notReadyMakerFixture(t)
	s.notReadySince = NowMicro() + 3600*1000000
	before := NowMicro()
	if _, _, err := s.OnConfirmA(&proto.ConfirmABody{HubAddress: hub, ID: orderID,
		BDepositTxID: strings.Repeat("dd", 32), BLockTime: 1030}); err == nil {
		t.Fatal("blind-deposit ConfirmA unexpectedly succeeded")
	}
	if s.notReadySince > before+2*1000000 {
		t.Fatalf("notReadySince %d not clamped to now (future stamp would skip window expiry)", s.notReadySince)
	}
	if s.claimRetryAt == 0 || s.claimRetryAt > before+notReadyPollMicro+2*1000000 {
		t.Fatal("future-stamped failure must still fast-poll")
	}
	if s.claimRetries != 0 {
		t.Fatalf("claimRetries = %d, want 0", s.claimRetries)
	}
}

// TestNotReadyClearedOnDepositSuccess mirrors the claim-path pin below: the
// deposit clearer resets the same wait triple.
func TestNotReadyClearedOnDepositSuccess(t *testing.T) {
	_, s, _, _ := notReadyMakerFixture(t)
	s.depositRetryAt = 123456789
	s.depositRetries = 2
	s.notReadySince = 555555555
	s.notReadySeen = true
	clearDepositRetry(s)
	if s.depositRetryAt != 0 || s.depositRetries != 0 || s.notReadySince != 0 || s.notReadySeen {
		t.Fatalf("deposit wait state not cleared: retryAt=%d retries=%d since=%d seen=%v",
			s.depositRetryAt, s.depositRetries, s.notReadySince, s.notReadySeen)
	}
}

// TestNotReadyClearedOnSuccess pins the claim-path clear contract together
// with the schedule (clearClaimRetry owns all three), so a later genuine
// failure starts a fresh window instead of inheriting a stale stamp.
func TestNotReadyClearedOnSuccess(t *testing.T) {
	_, s, hub, orderID := notReadyMakerFixture(t)
	if _, _, err := s.OnConfirmA(&proto.ConfirmABody{HubAddress: hub, ID: orderID,
		BDepositTxID: strings.Repeat("dd", 32), BLockTime: 1030}); err == nil {
		t.Fatal("blind-deposit ConfirmA unexpectedly succeeded")
	}
	if s.notReadySince == 0 {
		t.Fatal("notReadySince not stamped on not-ready failure")
	}
	clearClaimRetry(s)
	if s.notReadySince != 0 || s.claimRetryAt != 0 || s.claimRetries != 0 {
		t.Fatalf("wait state not cleared: notReadySince=%d claimRetryAt=%d claimRetries=%d",
			s.notReadySince, s.claimRetryAt, s.claimRetries)
	}
}

// TestDegradedZeroConfMempoolAccepted: a 0-conf policy coin accepts the
// verbose script/value match without a confirmations field (mempool
// presence IS the 0-conf proof — the wallet served the exact validated
// vout). Live cost of the strict gate: ~147 s per claim waiting for the
// first PIVX confirmation despite Confirmations=0.
func TestDegradedZeroConfMempoolAccepted(t *testing.T) {
	conn := &fakeConnector{ticker: "LTC", blockHeight: 1000,
		verboseBareConfs: true,
		verboseTx: map[string]wallet.VerboseTx{
			strings.Repeat("ab", 32): {Outputs: map[uint32]wallet.VerboseTxOut{
				0: {Value: 993900, ScriptHex: "deadbeef"},
			}},
		}}
	if !confirmDepositKnownByRawTx(conn, strings.Repeat("ab", 32), 0, "deadbeef", 993900, 0) {
		t.Fatal("0-conf degraded path must accept a verbose script/value match with no depth field")
	}
}

// TestDegradedConfPolicyStillGated: the same mempool-only evidence proves
// nothing for a 1-conf policy coin — the strict gate stands.
func TestDegradedConfPolicyStillGated(t *testing.T) {
	conn := &fakeConnector{ticker: "LTC", blockHeight: 1000,
		verboseBareConfs: true,
		verboseTx: map[string]wallet.VerboseTx{
			strings.Repeat("ab", 32): {Outputs: map[uint32]wallet.VerboseTxOut{
				0: {Value: 993900, ScriptHex: "deadbeef"},
			}},
		}}
	if confirmDepositKnownByRawTx(conn, strings.Repeat("ab", 32), 0, "deadbeef", 993900, 1) {
		t.Fatal("1-conf degraded path must reject depth-less verbose evidence")
	}
}

// visibleDepositMakerFixture drives maker + taker through CreateB with full
// chain visibility (the taker's B deposit is copied into the maker's view,
// like a caught-up backend), stopping before the maker's ConfirmA — the
// launchpad for redeem-stage tests. Returns maker node/session, hub,
// order id, and the taker's created-B body.
func visibleDepositMakerFixture(t *testing.T) (*Node, *SwapSession, [20]byte, [32]byte, *proto.CreatedBBody, *fakeConnector) {
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
	mkLtcConn := &fakeConnector{ticker: "LTC", funding: ltcFunding, fundingPriv: ltcFundingPriv,
		fundingPub: ltcFundingPub, changeAddr: addrFor(48, "ltc-change"), blockHeight: 1000,
		rawTx: map[string]string{}, confirmations: map[string]int{}}
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
	oid := hash20("redeem-split-order")
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
	// Backend catches up: the taker's deposit lands in the maker's view.
	for id, hx := range tkLtcConn.rawTx {
		mkLtcConn.rawTx[id] = hx
		mkLtcConn.confirmations[id] = 1
	}
	return makerNode, makerSession, hub, orderID, createdB, mkLtcConn
}

// TestRedeemTxOutErrorFastLanes: the deposit check passes (visibility proven)
// but the redeem-stage unspent re-check hits a backend error. That is backend
// flakiness, not a spent deposit — fast-poll, no backoff budget.
func TestRedeemTxOutErrorFastLanes(t *testing.T) {
	_, makerSession, hub, orderID, createdB, mkLtcConn := visibleDepositMakerFixture(t)
	mkLtcConn.txOutErr = errors.New("boom")
	before := NowMicro()
	if _, _, err := makerSession.OnConfirmA(&proto.ConfirmABody{HubAddress: hub, ID: orderID,
		BDepositTxID: createdB.BDepositTxID, BLockTime: createdB.BLockTime}); err == nil {
		t.Fatal("txout-error ConfirmA unexpectedly succeeded")
	}
	if makerSession.claimTxID != "" {
		t.Fatal("claimTxID set despite failed redeem")
	}
	if makerSession.claimRetryAt == 0 {
		t.Fatal("claimRetryAt not scheduled after txout-error redeem")
	}
	if makerSession.claimRetryAt > before+notReadyPollMicro+2*1000000 {
		t.Fatalf("claimRetryAt %d is a backoff delay, want fast re-poll (~5 s)", makerSession.claimRetryAt-before)
	}
	if makerSession.claimRetries != 0 {
		t.Fatalf("claimRetries = %d, want 0 (backend flakiness consumes no backoff)", makerSession.claimRetries)
	}
	if makerSession.notReadySince == 0 {
		t.Fatal("notReadySince not stamped on txout-error redeem")
	}
}

// TestRedeemSpentProofBacksOff pins the preserved slow path: a spent (or
// reorged) counterparty deposit is fund-safety-critical ambiguity, never
// index lag — the classic backoff owns it. Uses the exact production error
// string so the scheduling contract is pinned even though the fixture cannot
// easily stage a real double-spend.
func TestRedeemSpentProofBacksOff(t *testing.T) {
	makerNode, s, _, _ := notReadyMakerFixture(t)
	_ = makerNode
	before := NowMicro()
	s.applyConfirmedA(nil, errors.New("api: counterparty deposit dd:0 not unspent, awaiting redelivery"))
	if s.claimRetries != 1 {
		t.Fatalf("claimRetries = %d, want 1 (spent ambiguity backs off)", s.claimRetries)
	}
	if s.claimRetryAt < before+claimRetryBaseMicro {
		t.Fatalf("claimRetryAt %d is not a backoff delay, want >= 60 s", s.claimRetryAt-before)
	}
	if s.notReadySince != 0 {
		t.Fatal("notReadySince stamped on a spent-proof (non-not-ready) failure")
	}
}

// Taper tests: polls that cannot trigger progress (watched tx still
// invisible past 60 s — outage regime, never a healthy swap: 13/13 live
// swaps reach visibility in 10-40 s) slow from 5 s to 15 s. Anything seen —
// including every claim-side wait, which only runs after the deposit
// validated — keeps the 5 s cadence unconditionally, so swap speed is
// bit-identical on the healthy path.

// unseenNotReadyErr builds the wrapper the wallet returns when the watched
// bytes were never observed (CheckDepositTransaction "no tx found").
func unseenNotReadyErr() error {
	return &wallet.NotReadyError{Seen: false,
		Err: fmt.Errorf("%w: no tx found", wallet.ErrDepositNotReady)}
}

// seenNotReadyErr builds the wrapper shape for a watched-but-shallow tx
// (all other wallet not-ready returns stay bare, which defaults to seen).
func seenNotReadyErr() error {
	return &wallet.NotReadyError{Seen: true,
		Err: fmt.Errorf("%w: confirmations 0 of 1", wallet.ErrDepositNotReady)}
}

// TestNotReadyTaperUnseenOld: invisible past 60 s re-polls at 15 s, not 5 s.
func TestNotReadyTaperUnseenOld(t *testing.T) {
	_, s, _, _ := notReadyMakerFixture(t)
	now := NowMicro()
	s.notReadySince = now - 70*1000000
	if !s.scheduleNotReady(now, "maker", true, unseenNotReadyErr()) {
		t.Fatal("unseen wait inside the 10-min window must stay on the fast lane")
	}
	got := s.claimRetryAt - now
	if got < notReadyTaperPollMicro-2*1000000 || got > notReadyTaperPollMicro+2*1000000 {
		t.Fatalf("unseen 70 s wait re-polls in %d us, want tapered ~15 s", got)
	}
	if s.notReadySeen {
		t.Fatal("notReadySeen must be false after an unseen admission")
	}
}

// TestNotReadyTaperUnseenFresh: invisible inside 60 s keeps the 5 s cadence
// (every live healthy wait lives here).
func TestNotReadyTaperUnseenFresh(t *testing.T) {
	_, s, _, _ := notReadyMakerFixture(t)
	now := NowMicro()
	s.notReadySince = now - 30*1000000
	if !s.scheduleNotReady(now, "maker", true, unseenNotReadyErr()) {
		t.Fatal("fresh unseen wait must stay on the fast lane")
	}
	got := s.claimRetryAt - now
	if got < notReadyPollMicro-2*1000000 || got > notReadyPollMicro+2*1000000 {
		t.Fatalf("unseen 30 s wait re-polls in %d us, want fast ~5 s", got)
	}
}

// TestNotReadyTaperSeenAlwaysFast: a seen wait never tapers, however old
// (claim-side waits are always seen — the deposit validated before redeem).
func TestNotReadyTaperSeenAlwaysFast(t *testing.T) {
	_, s, _, _ := notReadyMakerFixture(t)
	now := NowMicro()
	s.notReadySince = now - 300*1000000
	if !s.scheduleNotReady(now, "maker", true, seenNotReadyErr()) {
		t.Fatal("seen wait inside the 10-min window must stay on the fast lane")
	}
	got := s.claimRetryAt - now
	if got < notReadyPollMicro-2*1000000 || got > notReadyPollMicro+2*1000000 {
		t.Fatalf("seen 300 s wait re-polls in %d us, want fast ~5 s", got)
	}
	if !s.notReadySeen {
		t.Fatal("notReadySeen must be true after a seen admission")
	}
}

// TestHonestSeverityNotReadyLogsInfo: routine not-ready build failures log
// at INFO (fast re-poll), never ERROR — an ERROR line must mean something
// genuinely failed. Drives all four build sites with an unseen not-ready
// error (direct apply calls: OnCreateA can never observe not-ready — the
// maker build spends recorded funding — so the site is driven directly).
func TestHonestSeverityNotReadyLogsInfo(t *testing.T) {
	genPath, _ := installSplitLogs(t)

	_, makerSession, _, _ := notReadyMakerFixture(t)
	makerSession.applyCreatedA(nil, unseenNotReadyErr())
	makerSession.applyConfirmedA(nil, unseenNotReadyErr())

	_, _, _, takerSession, _, _, _, _, _ := blindDepositFixture(t)
	takerSession.applyCreatedB(nil, unseenNotReadyErr())
	takerSession.applyConfirmedB(nil, unseenNotReadyErr())

	gen := readLogFile(t, genPath)
	if strings.Contains(gen, "task failed") {
		t.Errorf("routine not-ready failures must not log ERROR 'task failed':\n%s", gen)
	}
	if got := strings.Count(gen, "build not ready, fast re-poll"); got != 4 {
		t.Errorf("INFO 'build not ready, fast re-poll' lines = %d, want 4 (one per site)", got)
	}
}

// TestHonestSeverityGenuineLogsError: genuine (non-not-ready) build failures
// keep the ERROR line on all four build sites.
func TestHonestSeverityGenuineLogsError(t *testing.T) {
	genPath, _ := installSplitLogs(t)
	boom := errors.New("boom")

	_, makerSession, _, _ := notReadyMakerFixture(t)
	makerSession.applyCreatedA(nil, boom)
	makerSession.applyConfirmedA(nil, boom)

	_, _, _, takerSession, _, _, _, _, _ := blindDepositFixture(t)
	takerSession.applyCreatedB(nil, boom)
	takerSession.applyConfirmedB(nil, boom)

	gen := readLogFile(t, genPath)
	for _, want := range []string{"CreateA deposit task failed", "CreateB deposit task failed",
		"ConfirmA claim task failed", "ConfirmB claim task failed"} {
		if !strings.Contains(gen, want) {
			t.Errorf("genuine failure must keep ERROR line %q:\n%s", want, gen)
		}
	}
}

// TestNotReadySeenDefaultsTrue: a bare sentinel (all pre-taper sites and
// every claim-side return) classifies seen — the fail-safe direction.
func TestNotReadySeenDefaultsTrue(t *testing.T) {
	if !wallet.NotReadySeen(fmt.Errorf("wrap: %w", wallet.ErrDepositNotReady)) {
		t.Fatal("bare ErrDepositNotReady must classify seen")
	}
	if !wallet.NotReadySeen(errors.New("unrelated")) {
		t.Fatal("non-not-ready errors must classify seen")
	}
	if wallet.NotReadySeen(unseenNotReadyErr()) {
		t.Fatal("unseen wrapper must classify unseen")
	}
}
