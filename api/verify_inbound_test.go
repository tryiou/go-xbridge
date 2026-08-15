package api

import (
	"testing"

	"go-xbridge/coins"
	"go-xbridge/proto"
)

// proofEntries builds valid UtxoEntry ownership proofs over the ctx BTC funding
// set (stubConn's 1-BTC output) via buildUtxoProofs.
func proofEntries(t *testing.T, ctx *HandlerCtx) []proto.UtxoEntry {
	t.Helper()
	conn := ctx.Node.cfg().Connectors["BTC"]
	coin, ok := coins.Get("BTC")
	if !ok {
		t.Fatal("BTC coin not registered")
	}
	utxos, err := conn.ListUnspent(0)
	if err != nil {
		t.Fatalf("ListUnspent: %v", err)
	}
	entries, err := buildUtxoProofs(conn, utxos, coin)
	if err != nil {
		t.Fatalf("buildUtxoProofs: %v", err)
	}
	return entries
}

// TestVerifyOrderUtxosValid locks in the happy path: an order whose maker
// UTXO proofs verify against the chain (getTxOut existence + BIP137 signature)
// and whose surviving entries cover the fromAmount is bookable (C++ snode
// processTransaction, xbridgesession.cpp:535-577).
func TestVerifyOrderUtxosValid(t *testing.T) {
	ctx := newWalletTestCtx()
	coin, _ := coins.Get("BTC")
	entries := proofEntries(t, ctx)
	ok, err := verifyOrderUtxos(ctx.Node.cfg().Connectors["BTC"], coin, entries, 1e6)
	if err != nil || !ok {
		t.Fatalf("verifyOrderUtxos = %v, %v; want true, nil (1 BTC covers 1e6 XBridge units)", ok, err)
	}
}

// TestVerifyOrderUtxosForgedProof proves a signature the wallet rejects
// invalidates the entry, so the order is not bookable.
func TestVerifyOrderUtxosForgedProof(t *testing.T) {
	ctx := newWalletTestCtx()
	coin, _ := coins.Get("BTC")
	entries := proofEntries(t, ctx)
	stub := ctx.Node.cfg().Connectors["BTC"].(*stubConn)
	stub.verifyFail = true
	ok, err := verifyOrderUtxos(stub, coin, entries, 1e6)
	if err != nil || ok {
		t.Fatalf("verifyOrderUtxos = %v, %v; want false (forged proof must invalidate)", ok, err)
	}
}

// TestVerifyOrderUtxosUnknownOutput proves a txid:vout that does not exist
// on-chain (getTxOut ok=false) invalidates the entry — a maker cannot prove
// ownership of an output the chain does not have.
func TestVerifyOrderUtxosUnknownOutput(t *testing.T) {
	ctx := newWalletTestCtx()
	coin, _ := coins.Get("BTC")
	entries := proofEntries(t, ctx)
	// Rewrite the txid to an output the wallet has never seen (stubConn's
	// GetTxOut reports ok=false for anything outside its utxos).
	entries[0].TxID = [32]byte{0xab, 0xcd}
	ok, err := verifyOrderUtxos(ctx.Node.cfg().Connectors["BTC"], coin, entries, 1e6)
	if err != nil || ok {
		t.Fatalf("verifyOrderUtxos = %v, %v; want false (unknown output)", ok, err)
	}
}

// TestVerifyOrderUtxosInsufficientSum proves the surviving entries must cover
// the maker's fromAmount (C++ xBridgeAmountFromReal(commonAmount) < samount,
// xbridgesession.cpp:571-577): a 1-BTC funding set cannot back a 2-BTC order.
func TestVerifyOrderUtxosInsufficientSum(t *testing.T) {
	ctx := newWalletTestCtx()
	coin, _ := coins.Get("BTC")
	entries := proofEntries(t, ctx)
	ok, err := verifyOrderUtxos(ctx.Node.cfg().Connectors["BTC"], coin, entries, 2e6)
	if err != nil || ok {
		t.Fatalf("verifyOrderUtxos = %v, %v; want false (1 BTC < 2 BTC)", ok, err)
	}
}

// TestVerifyOrderUtxosNoSurvivors proves no surviving entry rejects the order
// even when the required amount is zero (C++ utxoItems.empty() rejection,
// xbridgesession.cpp:566-570).
func TestVerifyOrderUtxosNoSurvivors(t *testing.T) {
	ctx := newWalletTestCtx()
	coin, _ := coins.Get("BTC")
	entries := proofEntries(t, ctx)
	entries[0].TxID = [32]byte{0xde, 0xad} // unknown output -> skipped
	ok, err := verifyOrderUtxos(ctx.Node.cfg().Connectors["BTC"], coin, entries, 0)
	if err != nil || ok {
		t.Fatalf("verifyOrderUtxos = %v, %v; want false (no surviving entries)", ok, err)
	}
}

// TestVerifyAndBookValid wires the verification into booking: an order whose
// maker proofs verify is added to the store.
func TestVerifyAndBookValid(t *testing.T) {
	ctx := newWalletTestCtx()
	o := &Order{
		ID: [32]byte{1}, FromCurrency: "BTC", ToCurrency: "LTC",
		FromAmount: 1e6, ToAmount: 2e6, Status: "open",
		Utxos: proofEntries(t, ctx),
	}
	idHex := hexEncode(o.ID[:])
	ctx.Node.verifyAndBook(o)
	if ctx.Store.Get(idHex) == nil {
		t.Fatal("verified order must be booked")
	}
}

// TestVerifyAndBookForged proves a forged-proof order is NOT booked (the
// core rule: an inbound order whose maker utxo proofs fail never enters the book).
func TestVerifyAndBookForged(t *testing.T) {
	ctx := newWalletTestCtx()
	o := &Order{
		ID: [32]byte{2}, FromCurrency: "BTC", ToCurrency: "LTC",
		FromAmount: 1e6, ToAmount: 2e6, Status: "open",
		Utxos: proofEntries(t, ctx),
	}
	idHex := hexEncode(o.ID[:])
	ctx.Node.cfg().Connectors["BTC"].(*stubConn).verifyFail = true
	ctx.Node.verifyAndBook(o)
	if ctx.Store.Get(idHex) != nil {
		t.Fatal("forged-proof order must NOT be booked")
	}
}

// TestVerifyAndBookNoConnector proves an order for a currency with no connector
// is booked without verification (there is nothing to verify against; the
// reload prune removes unconnected orders unless ShowAllOrders). The C++ trader behaves the
// same way for cmd-4 broadcasts, which carry no UTXO entries at all.
func TestVerifyAndBookNoConnector(t *testing.T) {
	ctx := newWalletTestCtx()
	o := &Order{
		ID: [32]byte{3}, FromCurrency: "DOGE", ToCurrency: "LTC",
		FromAmount: 1e6, ToAmount: 2e6, Status: "open",
		Utxos: proofEntries(t, ctx), // present but unverifiable (no DOGE connector)
	}
	idHex := hexEncode(o.ID[:])
	ctx.Node.verifyAndBook(o)
	if ctx.Store.Get(idHex) == nil {
		t.Fatal("order with no maker-currency connector must still be booked (unverified)")
	}
}
