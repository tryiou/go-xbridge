package api

import (
	"errors"
	"testing"

	"go-xbridge/wallet"
)

// TestRefundReconcileAlreadyOnChain proves the fund-recovery reconcile: a
// refund whose re-broadcast is rejected because the identical pre-signed
// transaction is ALREADY on chain (a prior run broadcast it; wallets answer
// e.g. -25 "transaction already in block chain") must reach the C++ success
// state trRollback ("rolled back", xbridgesession.cpp:3911-3913) instead of
// being wedged in "rollback failed" with an endless failing retry. The
// live incident this locks in: order refunded and confirmed on chain, while
// every sweep re-broadcast produced -25 and re-marked "rollback failed".
func TestRefundReconcileAlreadyOnChain(t *testing.T) {
	ltc := &fakeConnector{ticker: "LTC", blockHeight: 1000, rawTx: map[string]string{}}
	n := rollbackFailedTestNode(t, ltc)
	refundHex := refundHexFixture()
	txid, err := txIDFromHex(refundHex)
	if err != nil {
		t.Fatal(err)
	}
	// The wallet rejects the re-broadcast, but knows the refund on chain.
	ltc.sendErr = &wallet.RPCError{Code: -25,
		Message: "the transaction was rejected by network rules.\n\ntransaction already in block chain"}
	ltc.rawTx[txid] = refundHex

	var id [32]byte
	copy(id[:], []byte("refund-already-onchain-000"))
	idHex := hexEncode(id[:])
	n.store.Add(&Order{ID: id, FromCurrency: "LTC", ToCurrency: "LTC", Status: "rollback failed"})
	n.sessions[idHex] = &SwapSession{n: n, id: id, isMaker: false, srcCur: "LTC", dstCur: "LTC",
		refundHex: refundHex, state: csCreatedA}

	if !n.postRefundTask(idHex, "LTC", refundHex, 0, false, nil) {
		t.Fatal("postRefundTask dropped the task")
	}
	if got := n.store.Get(idHex); got == nil || got.Status != "rolled back" {
		t.Fatalf("status after already-on-chain refund = %v, want rolled back", got)
	}
	if s := n.sessions[idHex]; s != nil {
		t.Fatal("session must be pruned after the reconciled refund (moveTransactionToHistory)")
	}
	if n.refundBackoffActive(idHex) {
		t.Fatal("backoff scheduled for an already-on-chain refund (next sweep would spam the same -25)")
	}
}

// TestRefundFailureStillClassifiesWhenAbsent pins the C++ parity guard the
// reconcile must not break: when the broadcast fails AND the wallet does not
// know the refund transaction, the failure is real — trRollbackFailed
// ("rollback failed", xbridgesession.cpp:3908) with the sweep retry intact.
func TestRefundFailureStillClassifiesWhenAbsent(t *testing.T) {
	ltc := &fakeConnector{ticker: "LTC", blockHeight: 1000, rawTx: map[string]string{}}
	n := rollbackFailedTestNode(t, ltc)
	ltc.sendErr = errors.New("simulated rpc failure")

	refundHex := refundHexFixture()
	var id [32]byte
	copy(id[:], []byte("refund-absent-classified-0"))
	idHex := hexEncode(id[:])
	n.store.Add(&Order{ID: id, FromCurrency: "LTC", ToCurrency: "LTC", Status: "created"})
	n.sessions[idHex] = &SwapSession{n: n, id: id, isMaker: false, srcCur: "LTC", dstCur: "LTC",
		refundHex: refundHex, state: csCreatedA}

	if !n.postRefundTask(idHex, "LTC", refundHex, 0, false, nil) {
		t.Fatal("postRefundTask dropped the task")
	}
	if got := n.store.Get(idHex); got == nil || got.Status != "rollback failed" {
		t.Fatalf("status for an unknown-to-chain refund failure = %v, want rollback failed", got)
	}
	if !n.refundBackoffActive(idHex) {
		t.Fatal("backoff not scheduled for a real refund failure")
	}
}
