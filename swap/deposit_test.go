package swap

import (
	"crypto/rand"
	"encoding/hex"
	"testing"

	btcec "github.com/btcsuite/btcd/btcec/v2"

	"xbridge-go/coins"
	"xbridge-go/wallet"
)

// randKey returns a compressed 33-byte pubkey and its 32-byte priv scalar.
func randKey(t *testing.T) ([33]byte, []byte) {
	t.Helper()
	var priv [32]byte
	if _, err := rand.Read(priv[:]); err != nil {
		t.Fatal(err)
	}
	key, _ := btcec.PrivKeyFromBytes(priv[:])
	pub := key.PubKey().SerializeCompressed()
	var p [33]byte
	copy(p[:], pub)
	return p, priv[:]
}

// TestDepositBuildAndSign builds a maker deposit locking funds into the P2SH
// HTLC, signs the funding input with the depositor's key, and verifies the
// signature (and that a tampered output fails). Mirrors the HTLC sign/verify
// round-trip already covered for coins/htlc.go at the swap layer.
func TestDepositBuildAndSign(t *testing.T) {
	localPub, localPriv := randKey(t)
	otherPub, _ := randKey(t)

	spec := &DepositSpec{
		Currency:        "BTC",
		Amount:          1000000,
		DepositorPub:    localPub,
		CounterpartyPub: otherPub,
		LockTime:        600,
	}
	if _, err := rand.Read(spec.Secret[:]); err != nil {
		t.Fatal(err)
	}

	// Funding UTXO: a P2PKH output paying to localPub.
	fundScript := coins.BuildP2PKHScript(coins.KeyID(localPub[:]))
	funding := []wallet.Utxo{{
		TxID:         "0000000000000000000000000000000000000000000000000000000000000001",
		Vout:         0,
		Amount:       2000000,
		ScriptPubKey: hex.EncodeToString(fundScript),
	}}

	c := coins.Coin{Decimals: 8}
	tx, err := spec.BuildDepositTx(c, funding, coins.KeyID(localPub[:]), 1000)
	if err != nil {
		t.Fatalf("BuildDepositTx: %v", err)
	}
	if len(tx.Outputs) != 2 {
		t.Fatalf("want 2 outputs (deposit + change), got %d", len(tx.Outputs))
	}
	// Deposit output is the P2SH HTLC.
	if got := hex.EncodeToString(tx.Outputs[0].ScriptPubKey); got[:2] != "a9" {
		t.Errorf("deposit output is not P2SH (OP_HASH160): %s", got)
	}

	// Sign the funding input with the local priv key.
	sig, err := spec.SignInput(tx, 0, fundScript, localPriv)
	if err != nil {
		t.Fatalf("SignInput: %v", err)
	}
	ok, err := coins.VerifyTxInput(tx, 0, fundScript, localPub[:], sig)
	if err != nil {
		t.Fatalf("VerifyTxInput: %v", err)
	}
	if !ok {
		t.Fatal("deposit input signature did not verify")
	}

	// Tampering with the deposit amount must invalidate the signature.
	tx.Outputs[0].Value++
	if ok, _ := coins.VerifyTxInput(tx, 0, fundScript, localPub[:], sig); ok {
		t.Fatal("tampered deposit tx still verified")
	}

	// The refund scriptSig assembles with the signature.
	if rs := spec.RefundScriptSig(sig); len(rs) == 0 {
		t.Fatal("empty refund scriptSig")
	}
}

// TestDepositSecretHash checks HASH160(Secret) is non-empty and stable.
func TestDepositSecretHash(t *testing.T) {
	var secret [33]byte
	if _, err := rand.Read(secret[:]); err != nil {
		t.Fatal(err)
	}
	spec := &DepositSpec{Secret: secret}
	if h := spec.SecretHash(); h == ([20]byte{}) {
		t.Fatal("empty secret hash")
	}
}
