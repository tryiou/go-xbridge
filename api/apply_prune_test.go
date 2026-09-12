package api

// Apply-after-prune tests: a worker result that lands AFTER its session was
// pruned must not resurrect ghost state. The engine is single-threaded but
// worker results queue in the buffered results channel while the tick runs
// prune in between — the apply then runs on a detached object. Broadcast
// facts (txid + hex) are chain truth and must still be tracked; everything
// else (store flips, hub responses, session state) must not happen.

import (
	"encoding/hex"
	"strings"
	"testing"

	"go-xbridge/coins"
	"go-xbridge/config"
	"go-xbridge/proto"
	"go-xbridge/wallet"
)

// ghostMakerFixture drives a maker through a FAILED deposit broadcast:
// intent adopted (txid + hex), state held pre-created, retry scheduled.
func ghostMakerFixture(t *testing.T) (*Node, *SwapSession, *fakeConnector, [32]byte, [20]byte) {
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
		rawTx: map[string]string{}, confirmations: map[string]int{},
		sendErr: errFakeBroadcastDown}
	confs := map[string]*config.CoinConf{
		"BTC": {Ticker: "BTC", Coin: 1e8, AddressPrefix: 0, CreateTxMethod: "BTC", BlockTime: 60},
		"LTC": {Ticker: "LTC", Coin: 1e8, AddressPrefix: 48, CreateTxMethod: "LTC", BlockTime: 60},
	}
	n := newTestNode(t, confs, map[string]wallet.Connector{"BTC": btcConn})
	n.conn = &captureXConn{}
	mkMPriv, mkMPub := newKey(t)
	_, tkMPub := newKey(t)
	var orderID [32]byte
	oid := hash20("ghost-apply-order")
	copy(orderID[:], oid[:])
	mkOrder := &Order{ID: orderID, FromCurrency: "BTC", ToCurrency: "LTC",
		FromAmount: 2.5e6, ToAmount: 2e6, Status: "open"}
	n.newMakerSession(withUsedCoins(t, n, mkOrder, []wallet.Utxo{btcFunding}),
		MakeOrderParams{MakerAddress: addrFor(0, "m"), TakerAddress: addrFor(48, "t")},
		arr32(mkMPriv), toArr33(mkMPub))
	s := n.sessions[hexEncode(orderID[:])]
	var hub [20]byte
	hb := hash20("hub")
	copy(hub[:], hb[:])
	s.hub = hub
	if _, _, err := s.OnCreateA(&proto.CreateABody{HubAddress: hub, ID: orderID, BPubKey: to33(tkMPub)}); err != nil {
		t.Fatalf("CreateA build unexpectedly failed: %v", err)
	}
	if s.ourDepositTxID == "" || s.depositHex == "" {
		t.Fatal("deposit intent not adopted despite successful build")
	}
	return n, s, btcConn, orderID, hub
}

// TestGhostDepositBroadcastAdoptsTrackingOnly pins the late-result contract:
// the pruned session's broadcast success must still TRACK the chain fact
// (txid + hex, so confirmation-watch owns it) while changing nothing else —
// no session resurrection, no store flips, no hub response.
func TestGhostDepositBroadcastAdoptsTrackingOnly(t *testing.T) {
	n, s, btcConn, orderID, _ := ghostMakerFixture(t)
	idHex := hexEncode(orderID[:])
	out := depositOutcome{txid: s.ourDepositTxID, lockTime: s.ourLockTime,
		refundHex: s.refundHex, depositHex: s.depositHex, conn: btcConn}
	// Prune wins the race: session gone while the broadcast result is queued.
	delete(n.sessions, idHex)
	nBroadcasts := len(btcConn.broadcasts)
	body := s.applyCreatedABroadcast(out, s.ourDepositTxID, nil)
	if body != nil {
		t.Fatal("ghost apply returned a hub response (must drop)")
	}
	if _, alive := n.sessions[idHex]; alive {
		t.Fatal("ghost apply resurrected the pruned session")
	}
	if o := n.store.Get(idHex); o == nil {
		t.Fatal("live order record lost")
	} else if o.DepositSent {
		t.Fatal("ghost apply flipped DepositSent on a pruned session")
	}
	if len(btcConn.broadcasts) != nBroadcasts {
		t.Fatal("ghost apply rebroadcast (must only track)")
	}
	n.trackedMu.Lock()
	_, tracked := n.tracked[s.ourDepositTxID]
	n.trackedMu.Unlock()
	if !tracked {
		t.Fatal("broadcast fact not tracked: confirmation-watch blind to it")
	}
}

// TestGhostBuildResultDropped pins the build-phase half: a build result for
// a pruned session adopts nothing and broadcasts nothing.
func TestGhostBuildResultDropped(t *testing.T) {
	n, s, btcConn, orderID, hub := ghostMakerFixture(t)
	idHex := hexEncode(orderID[:])
	_ = hub
	out := depositOutcome{txid: s.ourDepositTxID, lockTime: s.ourLockTime,
		refundHex: s.refundHex, depositHex: s.depositHex, conn: btcConn}
	delete(n.sessions, idHex)
	nBroadcasts := len(btcConn.broadcasts)
	// Backend healthy again: without the guard this stale build result would
	// adopt intent AND broadcast. It must do neither.
	btcConn.sendErr = nil
	body := s.applyCreatedA(out, nil)
	if body != nil {
		t.Fatal("ghost apply returned a hub response (must drop)")
	}
	if _, alive := n.sessions[idHex]; alive {
		t.Fatal("ghost apply resurrected the pruned session")
	}
	if o := n.store.Get(idHex); o == nil {
		t.Fatal("live order record lost")
	} else if o.DepositSent {
		t.Fatal("ghost apply flipped DepositSent on a pruned session")
	}
	if len(btcConn.broadcasts) != nBroadcasts {
		t.Fatal("ghost apply broadcast (must drop)")
	}
}
