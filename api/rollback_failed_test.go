package api

import (
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"go-xbridge/coins"
	"go-xbridge/config"
	"go-xbridge/crypto"
	"go-xbridge/wallet"
)

// rollbackFailedTestNode builds an inline (engine-not-started) node with a
// single LTC connector backed by conn, so postRefundTask runs synchronously on
// the caller and the apply's store mutations are directly observable.
func rollbackFailedTestNode(t *testing.T, conn wallet.Connector) *Node {
	t.Helper()
	if err := coins.InitFromConf(map[string]*config.CoinConf{
		"LTC": {Ticker: "LTC", Coin: 1e8, AddressPrefix: 48, CreateTxMethod: "LTC", BlockTime: 60},
	}); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{
		Confs: map[string]*config.CoinConf{
			"LTC": {Ticker: "LTC", Coin: 1e8, AddressPrefix: 48, CreateTxMethod: "LTC", BlockTime: 60},
		},
		Connectors: map[string]wallet.Connector{"LTC": conn},
	}
	node := &Node{config: cfg, store: NewStore(), signer: crypto.NewBtcSigner(), stop: make(chan struct{})}
	node.sessions = map[string]*SwapSession{}
	return node
}

// refundHexFixture is a minimal serialized tx that fakeConnector accepts for a
// successful broadcast.
func refundHexFixture() string {
	rtx := &coins.Tx{Version: 1}
	rtx.Inputs = []coins.TxIn{{PrevOut: coins.OutPoint{Hash: mustHash(strings.Repeat("ab", 32)), Index: 0}, Sequence: 0xfffffffe}}
	rtx.Outputs = []coins.TxOut{{Value: 1, ScriptPubKey: []byte{0x51}}}
	return hex.EncodeToString(rtx.Serialize())
}

// TestRollbackFailedStatusOnRefundBroadcastFailure locks in STATE-F74: a
// refund-broadcast failure writes the trRollbackFailed descriptor state
// ("rollback failed", C++ xbridgesession.cpp:3908) on the rollback path, and a
// later successful retry restores "rolled back" (C++ trRollbackFailed ->
// trRollback, :3912-3913).
func TestRollbackFailedStatusOnRefundBroadcastFailure(t *testing.T) {
	ltc := &fakeConnector{ticker: "LTC", blockHeight: 1000, rawTx: map[string]string{}}
	n := rollbackFailedTestNode(t, ltc)

	var id [32]byte
	copy(id[:], []byte("rollback-failed-order-0000"))
	idHex := hexEncode(id[:])
	o := &Order{ID: id, FromCurrency: "LTC", ToCurrency: "LTC", Status: "rolled back"}
	n.store.Add(o)
	n.sessions[idHex] = &SwapSession{n: n, id: id, isMaker: false, srcCur: "LTC", dstCur: "LTC",
		refundHex: refundHexFixture(), state: csCreatedA}

	// Broadcast failure -> the rollback path carries "rollback failed".
	ltc.sendErr = errors.New("simulated rpc failure")
	if !n.postRefundTask(idHex, "LTC", refundHexFixture(), 0, false, nil) {
		t.Fatal("postRefundTask dropped the task")
	}
	if got := n.store.Get(idHex); got == nil || got.Status != "rollback failed" {
		t.Fatalf("status after failed refund = %v, want rollback failed", got)
	}
	// The sweep must still be able to retry: the session survives (not terminal).
	if s := n.sessions[idHex]; s == nil || s.refundDone {
		t.Fatal("session must stay live (refundDone false) so the sweep can retry")
	}

	// Retry success -> status flips back to "rolled back", session pruned.
	ltc.sendErr = nil
	if !n.postRefundTask(idHex, "LTC", refundHexFixture(), 0, false, nil) {
		t.Fatal("postRefundTask dropped the retry")
	}
	if got := n.store.Get(idHex); got == nil || got.Status != "rolled back" {
		t.Fatalf("status after successful retry = %v, want rolled back", got)
	}
	if s := n.sessions[idHex]; s != nil {
		t.Fatal("session must be pruned after a successful refund (moveTransactionToHistory)")
	}
}

// TestRefundFailureStateGate proves the gate mirrors C++ redeemOrderDeposit
// (xbridgesession.cpp:3852-3908): a failed refund broadcast writes
// "rollback failed" only when the order's session has broadcast a deposit
// (csCreatedA+ — the state >= trCreated analog, and the exact predicate
// scanRefunds uses). The gate keys off the SESSION state, not the store order
// ordinal: the taker's order stays "accepting" (ordinal 3) for its whole swap,
// so an ordinal gate would never fire for taker refunds (C++ writes
// trRollbackFailed for takers too). A pre-deposit session (state < csCreatedA)
// is left untouched.
func TestRefundFailureStateGate(t *testing.T) {
	ltc := &fakeConnector{ticker: "LTC", blockHeight: 1000, rawTx: map[string]string{}}
	n := rollbackFailedTestNode(t, ltc)

	// Pre-deposit: session not yet at csCreatedA -> no write.
	var pre [32]byte
	copy(pre[:], []byte("refund-fail-predeposit-000"))
	preKey := hexEncode(pre[:])
	n.store.Add(&Order{ID: pre, FromCurrency: "LTC", ToCurrency: "LTC", Status: "accepting"})
	n.sessions[preKey] = &SwapSession{n: n, id: pre, isMaker: true, srcCur: "LTC", dstCur: "LTC",
		refundHex: refundHexFixture(), state: csInitialized}

	// Mid-swap maker: store order "created" (its real status), deposit sent.
	var maker [32]byte
	copy(maker[:], []byte("refund-fail-maker-0000000"))
	makerKey := hexEncode(maker[:])
	n.store.Add(&Order{ID: maker, FromCurrency: "LTC", ToCurrency: "LTC", Status: "created"})
	n.sessions[makerKey] = &SwapSession{n: n, id: maker, isMaker: true, srcCur: "LTC", dstCur: "LTC",
		refundHex: refundHexFixture(), state: csCreatedA}

	// Taker: store order stays "accepting", session at csCreatedB.
	var taker [32]byte
	copy(taker[:], []byte("refund-fail-taker-0000000"))
	takerKey := hexEncode(taker[:])
	n.store.Add(&Order{ID: taker, FromCurrency: "LTC", ToCurrency: "LTC", Status: "accepting"})
	n.sessions[takerKey] = &SwapSession{n: n, id: taker, isMaker: false, srcCur: "LTC", dstCur: "LTC",
		refundHex: refundHexFixture(), state: csCreatedB}

	ltc.sendErr = errors.New("simulated rpc failure")
	for _, k := range []string{preKey, makerKey, takerKey} {
		if !n.postRefundTask(k, "LTC", refundHexFixture(), 0, false, nil) {
			t.Fatal("postRefundTask dropped the task")
		}
	}
	if got := n.store.Get(preKey); got == nil || got.Status != "accepting" {
		t.Fatalf("pre-deposit order status after failure = %v, want accepting (unchanged)", got)
	}
	if got := n.store.Get(makerKey); got == nil || got.Status != "rollback failed" {
		t.Fatalf("mid-swap maker status after failure = %v, want rollback failed", got)
	}
	if got := n.store.Get(takerKey); got == nil || got.Status != "rollback failed" {
		t.Fatalf("taker status after failure = %v, want rollback failed (session gate, not ordinal)", got)
	}

	// Retry success restores "rolled back" for the taker.
	ltc.sendErr = nil
	if !n.postRefundTask(takerKey, "LTC", refundHexFixture(), 0, false, nil) {
		t.Fatal("postRefundTask dropped the retry")
	}
	if got := n.store.Get(takerKey); got == nil || got.Status != "rolled back" {
		t.Fatalf("taker status after success = %v, want rolled back", got)
	}

	// A user-canceled order (dxCancelOrder sets "canceled", then broadcasts the
	// refund) must NOT flip to "rollback failed" on a failed broadcast — C++
	// never produces trRollbackFailed for trCancelled.
	var canc [32]byte
	copy(canc[:], []byte("refund-fail-canceled-00000"))
	cancKey := hexEncode(canc[:])
	n.store.Add(&Order{ID: canc, FromCurrency: "LTC", ToCurrency: "LTC", Status: "canceled"})
	n.sessions[cancKey] = &SwapSession{n: n, id: canc, isMaker: false, srcCur: "LTC", dstCur: "LTC",
		refundHex: refundHexFixture(), state: csCreatedA}
	ltc.sendErr = errors.New("simulated rpc failure")
	if !n.postRefundTask(cancKey, "LTC", refundHexFixture(), 0, false, nil) {
		t.Fatal("postRefundTask dropped the task")
	}
	if got := n.store.Get(cancKey); got == nil || got.Status != "canceled" {
		t.Fatalf("canceled order status after failed refund = %v, want canceled (unchanged)", got)
	}

	// Session-less fallback (stored-order escape hatch): rollbackGate keys on
	// RefundTx, not the order status — a session-less taker order stored
	// "accepting" with RefundTx set still fires, while an order with no
	// deposit (RefundTx "") is left alone.
	var noDep [32]byte
	copy(noDep[:], []byte("refund-fail-nodeposit-000"))
	noDepKey := hexEncode(noDep[:])
	n.store.Add(&Order{ID: noDep, FromCurrency: "LTC", ToCurrency: "LTC", Status: "created"})
	ltc.sendErr = errors.New("simulated rpc failure")
	if !n.postRefundTask(noDepKey, "LTC", refundHexFixture(), 0, false, nil) {
		t.Fatal("postRefundTask dropped the task")
	}
	if got := n.store.Get(noDepKey); got == nil || got.Status != "created" {
		t.Fatalf("session-less order without RefundTx after failure = %v, want created (unchanged)", got)
	}

	var sessless [32]byte
	copy(sessless[:], []byte("refund-fail-sessless-000"))
	sesslessKey := hexEncode(sessless[:])
	n.store.Add(&Order{ID: sessless, FromCurrency: "LTC", ToCurrency: "LTC", Status: "accepting",
		RefundTx: refundHexFixture()})
	ltc.sendErr = errors.New("simulated rpc failure")
	if !n.postRefundTask(sesslessKey, "LTC", refundHexFixture(), 0, false, nil) {
		t.Fatal("postRefundTask dropped the task")
	}
	if got := n.store.Get(sesslessKey); got == nil || got.Status != "rollback failed" {
		t.Fatalf("session-less taker (RefundTx set) after failure = %v, want rollback failed", got)
	}
}
