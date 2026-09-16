package api

// Broadcast-repost tests: a BUILT intent whose broadcast fails must be
// re-posted (identical bytes) by the tick sweep instead of stranding.
//
// This is the Phase-2 half the build retries (swap_retry.go) do not cover:
// the build succeeded (txid + hex persisted) but SendRawTransaction failed,
// and neither hub redelivery (which rebuilds, risking a second deposit) nor
// any sweep re-sends the exact bytes. All fixtures are in-memory —
// fakeConnector.sendErr injects the broadcast failure, no live hub.

import (
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"go-xbridge/coins"
	"go-xbridge/config"
	"go-xbridge/proto"
	"go-xbridge/wallet"
)

// errFakeBroadcastDown injects a wallet-accepted-nothing broadcast failure.
var errFakeBroadcastDown = errors.New("fake: broadcast backend down")

// TestMakerDepositBroadcastFailureReposts pins the deposit Phase-2 hole: the
// build succeeds (intent durable) but the broadcast fails. The session must
// schedule a retry holding the exact hex, and the sweep must re-post those
// identical bytes — never rebuild (which would double-deposit).
func TestMakerDepositBroadcastFailureReposts(t *testing.T) {
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
		rawTx: map[string]string{}, confirmations: map[string]int{},
		sendErr: errFakeBroadcastDown}
	confs := map[string]*config.CoinConf{
		"BTC": {Ticker: "BTC", Coin: 1e8, AddressPrefix: 0, CreateTxMethod: "BTC", BlockTime: 60},
		"LTC": {Ticker: "LTC", Coin: 1e8, AddressPrefix: 48, CreateTxMethod: "LTC", BlockTime: 60},
	}
	conns := map[string]wallet.Connector{"BTC": btcConn}
	makerNode := newTestNode(t, confs, conns)
	mkMPriv, mkMPub := newKey(t)
	_, tkMPub := newKey(t)
	var orderID [32]byte
	oid := hash20("maker-broadcast-retry-order")
	copy(orderID[:], oid[:])
	mkOrder := &Order{ID: orderID, FromCurrency: "BTC", ToCurrency: "LTC", FromAmount: 2.5e6, ToAmount: 2e6}
	makerNode.newMakerSession(withUsedCoins(t, makerNode, mkOrder, []wallet.Utxo{btcFunding}),
		MakeOrderParams{MakerAddress: addrFor(0, "m"), TakerAddress: addrFor(48, "t")}, arr32(mkMPriv), toArr33(mkMPub))
	s := makerNode.sessions[hexEncode(orderID[:])]
	var hub [20]byte
	hb := hash20("hub")
	copy(hub[:], hb[:])
	s.hub = hub
	// Inline mode returns no error here (the BUILD succeeded); the
	// broadcast failure surfaces as: intent stored, state held pre-created,
	// retry scheduled.
	if _, _, err := s.OnCreateA(&proto.CreateABody{HubAddress: hub, ID: orderID, BPubKey: to33(tkMPub)}); err != nil {
		t.Fatalf("CreateA build unexpectedly failed: %v", err)
	}
	if s.state == csCreatedA {
		t.Fatal("state advanced despite failed broadcast")
	}
	if s.ourDepositTxID == "" {
		t.Fatal("ourDepositTxID not adopted despite successful build")
	}
	if s.depositHex == "" {
		t.Fatal("built deposit hex not stored for repost")
	}
	if s.depositRetryAt == 0 {
		t.Fatal("depositRetryAt not scheduled after broadcast failure")
	}
	sentHex := s.depositHex
	nBroadcasts := len(btcConn.broadcasts)
	// Backend recovers; the tick sweep re-posts the IDENTICAL bytes.
	btcConn.sendErr = nil
	s.depositRetryAt = 1
	makerNode.retryFailedDepositBuilds(NowMicro())
	if len(btcConn.broadcasts) != nBroadcasts+1 {
		t.Fatal("sweep did not re-post the deposit broadcast")
	}
	if btcConn.broadcasts[len(btcConn.broadcasts)-1] != s.ourDepositTxID {
		t.Fatal("repost broadcast a different txid (must be identical bytes)")
	}
	if s.state != csCreatedA {
		t.Fatalf("state = %s, want createdA", s.state.String())
	}
	if s.depositRetryAt != 0 {
		t.Fatal("retry timestamp not cleared after successful repost")
	}
	_ = sentHex
}

// TestTakerClaimBroadcastFailureReposts pins the claim Phase-2 hole: the
// claim builds (secret recovered, hex persisted) but the broadcast fails.
// The sweep must re-post the identical claim bytes once the backend
// recovers — never rebuild (which is harmless but wasteful) and never drop.
func TestTakerClaimBroadcastFailureReposts(t *testing.T) {
	makerNode, takerNode, makerSession, takerSession, hub, orderID, makerPayTxID, tkLtcConn, mkLtcConn := blindTakerFixture(t)
	_ = makerNode
	_ = makerSession
	// Backend catches up so the BUILD succeeds; only the broadcast fails.
	hx, ok := mkLtcConn.rawTx[makerPayTxID]
	if !ok {
		t.Fatal("maker payTx missing from maker connector")
	}
	tkLtcConn.rawTx[makerPayTxID] = hx
	// Fail the claim broadcast: the taker's claim spends BTC (dstCur).
	tkBtcConn, ok := takerNode.config.Connectors["BTC"].(*fakeConnector)
	if !ok {
		t.Fatal("taker BTC connector not a fake")
	}
	tkBtcConn.sendErr = errFakeBroadcastDown
	// Inline mode returns no error (the BUILD succeeded); the broadcast
	// failure surfaces as: intent stored, state held pre-confirmed, retry
	// scheduled.
	if _, _, err := takerSession.OnConfirmB(&proto.ConfirmBBody{HubAddress: hub, ID: orderID, APayTxID: makerPayTxID}); err != nil {
		t.Fatalf("ConfirmB build unexpectedly failed: %v", err)
	}
	if takerSession.state != csCreatedB {
		t.Fatalf("state = %s, want createdB (failed broadcast must not advance)", takerSession.state.String())
	}
	if takerSession.claimTxID == "" {
		t.Fatal("claimTxID not adopted despite successful build")
	}
	if takerSession.claimHex == "" {
		t.Fatal("built claim hex not stored for repost")
	}
	if takerSession.claimRetryAt == 0 {
		t.Fatal("claimRetryAt not scheduled after broadcast failure")
	}
	// Backend recovers; the tick sweep re-posts the IDENTICAL bytes.
	tkBtcConn.sendErr = nil
	nBroadcasts := len(tkBtcConn.broadcasts)
	takerSession.claimRetryAt = 1
	takerNode.retryFailedClaimBuilds(NowMicro())
	if len(tkBtcConn.broadcasts) != nBroadcasts+1 {
		t.Fatal("sweep did not re-post the claim broadcast")
	}
	if tkBtcConn.broadcasts[len(tkBtcConn.broadcasts)-1] != takerSession.claimTxID {
		t.Fatal("repost broadcast a different txid (must be identical bytes)")
	}
	// C++ trader parity (xbridgesession.cpp:3185): the successful redeem
	// broadcast is the finish — terminal at the repost, not at Finished.
	if takerSession.state != csFinished {
		t.Fatalf("state = %s, want finished after successful repost", takerSession.state.String())
	}
	if takerSession.claimRetryAt != 0 {
		t.Fatal("retry timestamp not cleared after successful repost")
	}
}

// TestBroadcastAdoptsConfirmedIntent pins the idempotence guard: if the
// "failed" broadcast actually landed on-chain (wallet accepted, response
// lost), the sweep must adopt the confirmation — never rebroadcast.
func TestBroadcastAdoptsConfirmedIntent(t *testing.T) {
	_, takerNode, _, takerSession, hub, orderID, makerPayTxID, tkLtcConn, mkLtcConn := blindTakerFixture(t)
	hx, ok := mkLtcConn.rawTx[makerPayTxID]
	if !ok {
		t.Fatal("maker payTx missing from maker connector")
	}
	tkLtcConn.rawTx[makerPayTxID] = hx
	tkBtcConn, ok := takerNode.config.Connectors["BTC"].(*fakeConnector)
	if !ok {
		t.Fatal("taker BTC connector not a fake")
	}
	tkBtcConn.sendErr = errFakeBroadcastDown
	if _, _, err := takerSession.OnConfirmB(&proto.ConfirmBBody{HubAddress: hub, ID: orderID, APayTxID: makerPayTxID}); err != nil {
		t.Fatalf("ConfirmB build unexpectedly failed: %v", err)
	}
	claimID := takerSession.claimTxID
	if tkBtcConn.verboseTx == nil {
		tkBtcConn.verboseTx = map[string]wallet.VerboseTx{}
	}
	// Chain says confirmed (backend caught up past us); backend recovers too.
	tkBtcConn.sendErr = nil
	tkBtcConn.verboseTx[claimID] = wallet.VerboseTx{TxID: claimID, Confirmations: 2}
	nBroadcasts := len(tkBtcConn.broadcasts)
	takerSession.claimRetryAt = 1
	takerNode.retryFailedClaimBuilds(NowMicro())
	if len(tkBtcConn.broadcasts) != nBroadcasts {
		t.Fatal("sweep rebroadcast a confirmed claim (must adopt, not resend)")
	}
	// C++ trader parity (xbridgesession.cpp:3185): adoption replays the
	// successful-broadcast resume, which is the finish.
	if takerSession.state != csFinished {
		t.Fatalf("state = %s, want finished after confirmation adoption", takerSession.state.String())
	}
}
