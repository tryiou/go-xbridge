package api

import (
	"strings"
	"testing"

	"go-xbridge/coins"
	"go-xbridge/config"
	"go-xbridge/proto"
	"go-xbridge/wallet"
)

// Regression tests for the hub-retransmit state machine (live S6 matrix #8,
// 2026-09-15, order a4198f2d…): the hub retransmits Hold for the session's
// whole lifetime, and C++ gates every handshake handler on the transaction's
// exact expected state ("wrong tx state, expecting … state",
// xbridgesession.cpp:1244-1253) so no packet can ever rewind a transaction.
// go-xbridge's OnHold had no progression guard: a redelivered Hold dragged a
// session that had already broadcast its deposit (csCreatedA) back to
// csHoldApplied. The next CreateA retransmit then re-ran the deposit build
// against the already-committed funding UTXO, and the wedged state starved the
// scheduled claim retry (retryFailedClaimBuilds matches the exact pre-claim
// state). These tests pin: late handshake packets are dropped, state never
// regresses.

func holdRegressionSetup(t *testing.T) (*SwapSession, *Node, [32]byte, [20]byte) {
	t.Helper()
	if err := coins.InitFromConf(map[string]*config.CoinConf{
		"BTC": {Ticker: "BTC", Coin: 1e8, AddressPrefix: 0, CreateTxMethod: "BTC", BlockTime: 60},
		"LTC": {Ticker: "LTC", Coin: 1e8, AddressPrefix: 48, CreateTxMethod: "BTC", BlockTime: 60},
	}); err != nil {
		t.Fatalf("InitFromConf: %v", err)
	}
	conn := &fakeConnector{ticker: "BTC", blockHeight: 1000, rawTx: map[string]string{}}
	confs := map[string]*config.CoinConf{
		"BTC": {Ticker: "BTC", Coin: 1e8, AddressPrefix: 0, CreateTxMethod: "BTC", BlockTime: 60},
	}
	n := newTestNode(t, confs, map[string]wallet.Connector{"BTC": conn})
	mPriv, mPub := newKey(t)
	var orderID [32]byte
	oid := hash20("hold-regression-order")
	copy(orderID[:], oid[:])
	o := &Order{ID: orderID, FromCurrency: "BTC", ToCurrency: "LTC", FromAmount: 2.5e6, ToAmount: 2e6}
	n.newMakerSession(withUsedCoins(t, n, o, nil),
		MakeOrderParams{MakerAddress: addrFor(0, "hold-reg-maker"), TakerAddress: addrFor(48, "hold-reg-taker")},
		arr32(mPriv), toArr33(mPub))
	s := n.sessions[hexEncode(orderID[:])]
	var hub [20]byte
	hb := hash20("hub")
	copy(hub[:], hb[:])
	s.hub = hub
	return s, n, orderID, hub
}

// driveToDepositStage walks the maker session through Hold and Init, then
// simulates applyCreatedA's post-broadcast state (deposit committed).
func driveToDepositStage(t *testing.T, s *SwapSession, orderID [32]byte, hub [20]byte) {
	t.Helper()
	if _, _, err := s.OnHold(&proto.HoldBody{HubAddress: hub, ID: orderID,
		FromAmount: 2e6, ToAmount: 2.5e6}); err != nil {
		t.Fatalf("first OnHold: %v", err)
	}
	ltcHash := hash20("hold-reg-taker")
	btcHash := hash20("hold-reg-maker")
	if _, _, err := s.OnInit(&proto.InitBody{ClientAddress: ltcHash, HubAddress: hub, ID: orderID,
		FromAddress: btcHash, FromCurrency: "BTC", FromAmount: 2.5e6,
		ToAddress: ltcHash, ToCurrency: "LTC", ToAmount: 2e6}); err != nil {
		t.Fatalf("OnInit: %v", err)
	}
	if s.state != csInitialized {
		t.Fatalf("precondition: state = %s, want initialized", s.state)
	}
	// Deposit broadcast (applyCreatedA phase 2): state + ourDepositTxID.
	s.state = csCreatedA
	s.ourDepositTxID = "deadbeefdeposit"
}

// TestHoldRetransmitDoesNotRegressDepositStage is THE #8 regression: a Hold
// retransmit on a session whose deposit already broadcast must be dropped —
// state stays csCreatedA, no HoldApply is emitted, no maker resize happens.
func TestHoldRetransmitDoesNotRegressDepositStage(t *testing.T) {
	s, n, orderID, hub := holdRegressionSetup(t)
	driveToDepositStage(t, s, orderID, hub)

	cmd, body, err := s.OnHold(&proto.HoldBody{HubAddress: hub, ID: orderID,
		FromAmount: 2e6, ToAmount: 2.5e6})
	if err != nil {
		t.Fatalf("late OnHold returned error: %v", err)
	}
	if cmd != 0 || body != nil {
		t.Fatalf("late OnHold must be dropped, got cmd=%v body=%v", cmd, body)
	}
	if s.state != csCreatedA {
		t.Fatalf("state regressed to %s, want createdA", s.state)
	}
	// The maker resize must not have run either (amounts untouched).
	if s.srcAmt != 2.5e6 || s.dstAmt != 2e6 {
		t.Fatalf("amounts mutated by late Hold: src=%d dst=%d", s.srcAmt, s.dstAmt)
	}
	if o := n.store.Get(hexEncode(orderID[:])); o != nil && (o.FromAmount != 2.5e6 || o.ToAmount != 2e6) {
		t.Fatalf("store amounts mutated by late Hold: %d/%d", o.FromAmount, o.ToAmount)
	}
}

// TestInitAndCreateARetransmitsStayMonotonic pins the whole #8 sequence with
// the fix in place: after a late Hold is dropped, later Init/CreateA
// retransmits cannot re-enter the deposit stage (state never rewinds, the
// deposit is never rebuilt).
func TestInitAndCreateARetransmitsStayMonotonic(t *testing.T) {
	s, _, orderID, hub := holdRegressionSetup(t)
	driveToDepositStage(t, s, orderID, hub)

	// Late Hold: dropped (previous test pins the drop; here just apply it).
	_, _, _ = s.OnHold(&proto.HoldBody{HubAddress: hub, ID: orderID, FromAmount: 2e6, ToAmount: 2.5e6})
	if s.state != csCreatedA {
		t.Fatalf("precondition: late Hold regressed state to %s", s.state)
	}

	// The Init retransmit that (pre-fix) re-initialized the regressed session:
	// ignored — state >= csInitialized.
	if _, _, err := s.OnInit(&proto.InitBody{ClientAddress: hash20("hold-reg-taker"), HubAddress: hub,
		ID: orderID, FromAddress: hash20("hold-reg-maker"), FromCurrency: "BTC",
		FromAmount: 2.5e6, ToAddress: hash20("hold-reg-taker"), ToCurrency: "LTC",
		ToAmount: 2e6}); err != nil {
		t.Fatalf("late OnInit: %v", err)
	}
	if s.state != csCreatedA {
		t.Fatalf("state changed by late Init: %s, want createdA", s.state)
	}

	// The CreateA retransmit that (pre-fix) re-ran the deposit build: ignored.
	var bad [33]byte
	copy(bad[:], []byte(strings.Repeat("k", 33)))
	if _, _, err := s.OnCreateA(&proto.CreateABody{HubAddress: hub, ID: orderID,
		BPubKey: bad}); err != nil {
		t.Fatalf("late OnCreateA: %v", err)
	}
	if s.state != csCreatedA {
		t.Fatalf("state changed by late CreateA: %s, want createdA", s.state)
	}
	if s.ourDepositTxID != "deadbeefdeposit" {
		t.Fatalf("deposit identity clobbered by retransmits: %q", s.ourDepositTxID)
	}
}
