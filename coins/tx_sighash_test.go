package coins

import "testing"

// keyID20 returns the 20-byte HASH160 of genPub as a byte slice, for building
// a P2WPKH scriptCode in the bounds tests.
func keyID20(t *testing.T) []byte {
	t.Helper()
	h := KeyID(genPub)
	return h[:]
}

// TestVerifyTxInputRejectsWrongSighash confirms VerifyTxInput rejects a
// signature whose trailing sighash byte is not SIGHASH_ALL. XBridge only ever
// uses SIGHASH_ALL; accepting SINGLE/NONE/ANYONECANPAY would let a
// counterparty's malleated signature validate against a different commitment.
func TestVerifyTxInputRejectsWrongSighash(t *testing.T) {
	inner := BuildDepositUnlockScript(genPub, genPub, make([]byte, 20), 600)
	sig, err := SignTxInput(testTx(), 0, inner, genPriv)
	if err != nil {
		t.Fatalf("SignTxInput: %v", err)
	}
	// Flip the trailing sighash type to SIGHASH_SINGLE (0x03).
	bad := append([]byte(nil), sig...)
	bad[len(bad)-1] = 0x03
	if ok, err := VerifyTxInput(testTx(), 0, inner, genPub, bad); ok || err == nil {
		t.Fatalf("expected rejection of non-ALL sighash (ok=%v err=%v)", ok, err)
	}
	// The unmodified SIGHASH_ALL signature must still verify.
	if ok, err := VerifyTxInput(testTx(), 0, inner, genPub, sig); !ok || err != nil {
		t.Fatalf("valid SIGHASH_ALL sig should verify (ok=%v err=%v)", ok, err)
	}
}

// TestHashForSigningSegwitIndexBounds confirms an out-of-range input index is a
// returned error, not a slice-bounds panic.
func TestHashForSigningSegwitIndexBounds(t *testing.T) {
	tx := &Tx{
		Version: 1,
		Inputs:  []TxIn{{PrevOut: OutPoint{Index: 0}, Sequence: 0xffffffff}},
		Outputs: []TxOut{{Value: 1, ScriptPubKey: BuildP2PKHScript(KeyID(genPub))}},
	}
	scriptCode := P2WPKHScriptCode(keyID20(t))
	if _, err := tx.HashForSigningSegwit(1, scriptCode, 1000); err == nil {
		t.Fatal("expected error for input index past end")
	}
	if _, err := tx.HashForSigningSegwit(-1, scriptCode, 1000); err == nil {
		t.Fatal("expected error for negative input index")
	}
	if _, err := tx.HashForSigningSegwit(0, scriptCode, 1000); err != nil {
		t.Fatalf("unexpected error for valid index: %v", err)
	}
}

// TestSignTxInputSegwitIndexBounds confirms the signing wrapper propagates the
// index-range error instead of panicking.
func TestSignTxInputSegwitIndexBounds(t *testing.T) {
	tx := &Tx{
		Version: 1,
		Inputs:  []TxIn{{PrevOut: OutPoint{Index: 0}, Sequence: 0xffffffff}},
		Outputs: []TxOut{{Value: 1, ScriptPubKey: BuildP2PKHScript(KeyID(genPub))}},
	}
	scriptCode := P2WPKHScriptCode(keyID20(t))
	if _, err := SignTxInputSegwit(tx, 5, scriptCode, 1000, genPriv); err == nil {
		t.Fatal("expected error signing out-of-range input")
	}
}
