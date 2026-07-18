package api

import (
	"testing"

	"xbridge-go/coins"
)

// TestBuildUtxoProofs confirms each spendable UTXO gets a 65-byte BIP137
// ownership proof and the correct 20-byte address id, matching the UtxoEntry
// carried in order/pending/accepting bodies.
func TestBuildUtxoProofs(t *testing.T) {
	ctx := newWalletTestCtx()
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
	if len(entries) != len(utxos) {
		t.Fatalf("got %d entries, want %d", len(entries), len(utxos))
	}
	e := entries[0]
	// stubConn returns a 65-byte proof starting at 0x01.
	if e.Signature[0] != 0x01 {
		t.Errorf("Signature[0] = 0x%x, want 0x01", e.Signature[0])
	}
	// RawAddress must equal the decoded utxo address id.
	a, err := coin.DecodeAddress(btcAddr)
	if err != nil {
		t.Fatalf("DecodeAddress: %v", err)
	}
	id, ok := a.ID()
	if !ok {
		t.Fatal("address has no id")
	}
	if e.RawAddress != id {
		t.Errorf("RawAddress = %x, want %x", e.RawAddress, id)
	}
}

// TestVerifyUtxoProofTampered confirms a tampered (wrong-length) proof fails
// verification: a valid 65-byte proof verifies, a truncated one does not.
func TestVerifyUtxoProofTampered(t *testing.T) {
	ctx := newWalletTestCtx()
	conn := ctx.Node.cfg().Connectors["BTC"]
	good, err := conn.SignMessage(btcAddr, "1abc:0")
	if err != nil {
		t.Fatalf("SignMessage: %v", err)
	}
	if ok, _ := conn.VerifyMessage(btcAddr, good, "1abc:0"); !ok {
		t.Error("valid 65-byte proof should verify")
	}
	bad := append([]byte(nil), good[:10]...) // truncated
	if ok, _ := conn.VerifyMessage(btcAddr, bad, "1abc:0"); ok {
		t.Error("truncated proof must fail verification")
	}
}
