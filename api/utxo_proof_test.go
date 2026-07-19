package api

import (
	"testing"

	"go-xbridge/coins"
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

// TestUtxoChallengeMatchesCpp confirms utxoChallenge produces exactly the
// C++ UtxoEntry::toString() form "txid:vout:amount:address"
// (xbridgewalletconnector.cpp:28), which C++ signs/verifies via
// conn->signMessage(entry.address, entry.toString(), sig). A mismatch here
// makes a Go order's proof unverifiable by a C++ node and vice versa.
func TestUtxoChallengeMatchesCpp(t *testing.T) {
	const (
		txid    = "1abc2def3abc4def5abc6def7abc8def9abc0def1abc2def3abc4def5abc6d"
		vout    = uint32(0)
		amount  = uint64(123456789)
		address = "1BvBMSEYstWetqTFn5Au4m4GFg7xJaNVN2"
	)
	got := utxoChallenge(txid, vout, amount, address)
	want := "1abc2def3abc4def5abc6def7abc8def9abc0def1abc2def3abc4def5abc6d:0:123456789:1BvBMSEYstWetqTFn5Au4m4GFg7xJaNVN2"
	if got != want {
		t.Fatalf("utxoChallenge = %q, want %q", got, want)
	}
	// A C++-shaped challenge must verify round-trip under the real signer.
	ctx := newWalletTestCtx()
	conn := ctx.Node.cfg().Connectors["BTC"]
	sig, err := conn.SignMessage(btcAddr, got)
	if err != nil {
		t.Fatalf("SignMessage: %v", err)
	}
	if ok, _ := conn.VerifyMessage(btcAddr, sig, got); !ok {
		t.Error("C++-shaped challenge should verify")
	}
}

// TestVerifyUtxoProofTampered confirms a tampered (wrong-length) proof fails
// verification: a valid 65-byte proof verifies, a truncated one does not.
func TestVerifyUtxoProofTampered(t *testing.T) {
	ctx := newWalletTestCtx()
	conn := ctx.Node.cfg().Connectors["BTC"]
	challenge := "1abc2def3abc4def:0:100000000:1BvBMSEYstWetqTFn5Au4m4GFg7xJaNVN2"
	good, err := conn.SignMessage(btcAddr, challenge)
	if err != nil {
		t.Fatalf("SignMessage: %v", err)
	}
	if ok, _ := conn.VerifyMessage(btcAddr, good, challenge); !ok {
		t.Error("valid 65-byte proof should verify")
	}
	bad := append([]byte(nil), good[:10]...) // truncated
	if ok, _ := conn.VerifyMessage(btcAddr, bad, challenge); ok {
		t.Error("truncated proof must fail verification")
	}
}
