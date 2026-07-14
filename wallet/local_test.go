package wallet

import (
	"encoding/hex"
	"testing"

	"xbridge-go/coins"
)

// testHTLCSigner signs the refund (IF) branch of an XBridge HTLC deposit,
// assembling the refund scriptSig with coins.BuildRefundScriptSig. It holds the
// maker's key and the inner redeem script.
type testHTLCSigner struct {
	myPriv, myPub, otherPub, inner []byte
}

func (s *testHTLCSigner) SignInput(tx *coins.Tx, idx int, prev PrevTx) ([]byte, error) {
	sig, err := coins.SignTxInput(tx, idx, s.inner, s.myPriv)
	if err != nil {
		return nil, err
	}
	return coins.BuildRefundScriptSig(sig, s.myPub, s.inner), nil
}

func TestLocalConnectorSignHTLC(t *testing.T) {
	// secp256k1 generator private key (32 bytes, last byte 0x01).
	myPriv := make([]byte, 32)
	myPriv[31] = 0x01
	myPub, _ := hex.DecodeString("0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798")
	otherPub, _ := hex.DecodeString("02f9308a019258c31049344f85f619bcf79a3b296b825f9cc0d5c7d3a0b5c8e8e")
	secretHash, _ := hex.DecodeString("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")

	// Build the HTLC inner redeem script and its P2SH scriptPubKey.
	inner := coins.BuildDepositUnlockScript(myPub, otherPub, secretHash, 600)
	p2sh := coins.BuildP2SHScript(coins.KeyID(inner))

	// Deposit tx: spends some prevout, pays `inner` via P2SH.
	deposit := &coins.Tx{
		Version: 1,
		Inputs: []coins.TxIn{{
			PrevOut:  coins.OutPoint{Index: 0},
			Sequence: 0,
		}},
		Outputs: []coins.TxOut{{
			Value:        1_000_000,
			ScriptPubKey: p2sh,
		}},
	}
	depositHex := hex.EncodeToString(deposit.Serialize())

	prev := PrevTx{
		TxID:         "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Vout:         0,
		ScriptPubKey: hex.EncodeToString(p2sh),
		Amount:       1_000_000,
	}

	signer := &testHTLCSigner{myPriv: myPriv, myPub: myPub, otherPub: otherPub, inner: inner}
	c := NewLocalConnector("BTC", signer, nil)

	signedHex, complete, err := c.SignRawTransaction(depositHex, []PrevTx{prev})
	if err != nil {
		t.Fatalf("SignRawTransaction: %v", err)
	}
	if !complete {
		t.Fatal("expected complete=true")
	}

	// The signed tx's scriptSig must be exactly the refund scriptSig.
	signed, err := coins.Deserialize(mustHex(t, signedHex))
	if err != nil {
		t.Fatalf("deserialize signed: %v", err)
	}
	want := coins.BuildRefundScriptSig(mustSign(t, signed, 0), myPub, inner)
	if string(signed.Inputs[0].ScriptSig) != string(want) {
		t.Fatalf("scriptSig mismatch:\n got %x\nwant %x", signed.Inputs[0].ScriptSig, want)
	}

	// Verify the embedded signature against the inner script + maker pubkey.
	sigWithSighash := mustSign(t, signed, 0)
	ok, err := coins.VerifyTxInput(signed, 0, inner, myPub, sigWithSighash)
	if err != nil {
		t.Fatalf("VerifyTxInput: %v", err)
	}
	if !ok {
		t.Fatal("signature did not verify")
	}

	// Tamper: change the output value; verification must fail.
	bad := *signed
	bad.Outputs[0].Value = 999_999
	if ok, _ := coins.VerifyTxInput(&bad, 0, inner, myPub, sigWithSighash); ok {
		t.Fatal("tampered tx verified (should not)")
	}
}

// mustHex decodes hex or fails the test.
func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("hex: %v", err)
	}
	return b
}

// mustSign returns the first pushed element (the signature + SIGHASH) of the
// input's scriptSig, which for the refund branch is "<sig> <pub> OP_1 <inner>".
func mustSign(t *testing.T, tx *coins.Tx, idx int) []byte {
	t.Helper()
	// Recover the signature by stripping the known suffix.
	// scriptSig = pushData(sig) pushData(myPub) OP_1 pushData(inner)
	// We extract the first push directly from the raw bytes.
	ss := tx.Inputs[idx].ScriptSig
	// length of first push is ss[0] (<=75 here).
	if len(ss) < 1 {
		t.Fatal("empty scriptsig")
	}
	n := int(ss[0])
	if n < 1 || n+1 > len(ss) {
		t.Fatalf("bad push length %d", n)
	}
	return ss[1 : 1+n]
}
