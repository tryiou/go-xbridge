package coins

import (
	"encoding/hex"
	"testing"
)

// genPriv is the private key 1; genPub is its compressed secp256k1 pubkey
// (the curve generator point). A canonical, well-known keypair.
var (
	genPriv = func() []byte {
		b := make([]byte, 32)
		b[31] = 1
		return b
	}()
	genPub, _ = hex.DecodeString("0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798")
)

func TestTxRoundTrip(t *testing.T) {
	inner := BuildDepositUnlockScript(genPub, genPub, []byte("0123456789abcdef0123456789abcdef01234567"), 600)
	p2sh := BuildP2SHScript(KeyID(inner))

	tx := &Tx{
		Version: 2,
		Inputs: []TxIn{{
			PrevOut:   OutPoint{Index: 3},
			ScriptSig: []byte{0x01, 0x02, 0x03},
			Sequence:  0xffffffff,
		}, {
			PrevOut:  OutPoint{Index: 1},
			Sequence: 0,
		}},
		Outputs: []TxOut{{
			Value:        1_000_000,
			ScriptPubKey: p2sh,
		}},
		LockTime: 0,
	}
	raw := tx.Serialize()
	back, err := Deserialize(raw)
	if err != nil {
		t.Fatalf("Deserialize: %v", err)
	}
	if back.Version != tx.Version {
		t.Errorf("version %d != %d", back.Version, tx.Version)
	}
	if len(back.Inputs) != len(tx.Inputs) {
		t.Fatalf("inputs %d != %d", len(back.Inputs), len(tx.Inputs))
	}
	if back.Inputs[0].PrevOut.Index != 3 || back.Inputs[0].Sequence != 0xffffffff {
		t.Errorf("input 0 mismatch: %+v", back.Inputs[0])
	}
	if string(back.Inputs[0].ScriptSig) != string(tx.Inputs[0].ScriptSig) {
		t.Errorf("scriptsig mismatch")
	}
	if len(back.Outputs) != 1 || back.Outputs[0].Value != 1_000_000 {
		t.Errorf("output mismatch: %+v", back.Outputs)
	}
	if string(back.Outputs[0].ScriptPubKey) != string(p2sh) {
		t.Errorf("scriptpubkey mismatch")
	}
	if string(back.Serialize()) != string(raw) {
		t.Error("re-serialize mismatch")
	}
}

func TestKeyID(t *testing.T) {
	id := KeyID(genPub)
	if len(id) != 20 {
		t.Fatalf("KeyID len = %d, want 20", len(id))
	}
	// Recompute independently.
	exp := KeyID(genPub)
	if id != exp {
		t.Error("KeyID not deterministic")
	}
}

func TestBuildDepositUnlockScript(t *testing.T) {
	myPub, _ := hex.DecodeString("0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798")
	otherPub, _ := hex.DecodeString("02f9308a019258c31049344f85f619bcf79a3b296b825f9cc0d5c7d3a0b5c8e8e")
	secret, _ := hex.DecodeString("0123456789abcdef0123456789abcdef01234567")
	lockTime := uint32(600)

	s := BuildDepositUnlockScript(myPub, otherPub, secret, lockTime)

	if s[0] != OpIf {
		t.Errorf("script[0] = %#x, want OP_IF", s[0])
	}
	if s[len(s)-1] != OpEndIf {
		t.Errorf("last = %#x, want OP_ENDIF", s[len(s)-1])
	}
	// The IF branch must CHECKLOCKTIMEVERIFY and pay myKeyID; the ELSE branch
	// must CHECKSIGVERIFY + check a 33-byte secret preimage against secretHash.
	myID := KeyID(myPub)
	otherID := KeyID(otherPub)
	checks := [][]byte{
		{OpIf},
		{OpCheckLockTimeVerify, OpDrop},
		{OpDup, OpHash160},
		pushData(myID[:]),
		{OpEqualVerify, OpCheckSig},
		{OpElse},
		{OpDup, OpHash160},
		pushData(otherID[:]),
		{OpEqualVerify, OpCheckSigVerify},
		{OpSize},
		pushNum(33),
		{OpEqualVerify, OpHash160},
		pushData(secret),
		{OpEqual},
		{OpEndIf},
	}
	pos := 0
	for _, c := range checks {
		idx := indexOf(s, c, pos)
		if idx < 0 {
			t.Fatalf("sub-sequence %x not found after offset %d in script", c, pos)
		}
		pos = idx + len(c)
	}
	// The lockTime (600 = 0x0258) must appear as a 2-byte minimal push.
	if idx := indexOf(s, []byte{0x02, 0x58, 0x02}, 0); idx < 0 {
		t.Errorf("lockTime 600 not encoded as minimal push in script")
	}
}

func TestBuildRefundAndPaymentScriptSig(t *testing.T) {
	inner, _ := hex.DecodeString("aabbcc")
	sig := []byte{0x30, 0x01, 0x02} // pretend DER
	myPub := genPub
	xPub, _ := hex.DecodeString("03b0b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f8091a")

	refund := BuildRefundScriptSig(sig, myPub, inner)
	// <sig> <myPubKey> OP_1 <inner>
	if refund[0] != byte(len(sig)) {
		t.Errorf("refund: sig length prefix wrong")
	}
	if !hasSuffix(inner, refund) {
		t.Errorf("refund: inner script not at tail")
	}
	if refund[len(refund)-1-len(inner)-1] != Op1 {
		t.Errorf("refund: OP_1 not before inner script")
	}

	pay := BuildPaymentScriptSig(xPub, sig, myPub, inner)
	// <xPubKey> <sig> <myPubKey> OP_0 <inner>
	if !hasSuffix(inner, pay) {
		t.Errorf("payment: inner script not at tail")
	}
	// OP_0 sits right before the inner push.
	innerPushStart := len(pay) - (1 + len(inner)) // OP_PUSHDATA(1) + inner
	if pay[innerPushStart-1] != Op0 {
		t.Errorf("payment: OP_0 not before inner script (got %#x)", pay[innerPushStart-1])
	}
}

func TestTxSerializeAndSign(t *testing.T) {
	var prev [32]byte
	copy(prev[:], []byte("0123456789abcdef0123456789abcdef"))
	inner := BuildDepositUnlockScript(genPub, genPub, make([]byte, 20), 600)
	refundSig, _ := SignTxInput(testTx(), 0, inner, genPriv)
	scriptSig := BuildRefundScriptSig(refundSig, genPub, inner)

	tx := &Tx{
		Version: 1,
		Inputs: []TxIn{{
			PrevOut:   OutPoint{Hash: prev, Index: 0},
			ScriptSig: scriptSig,
			Sequence:  0xfffffffe, // SEQUENCE_FINAL-1 (lockTime active)
		}},
		Outputs: []TxOut{{
			Value:        1000000,
			ScriptPubKey: BuildP2PKHScript(KeyID(genPub)),
		}},
		LockTime: 600,
	}

	raw := tx.Serialize()
	// version 1 LE prefix
	if raw[0] != 0x01 || raw[1] != 0x00 || raw[2] != 0x00 || raw[3] != 0x00 {
		t.Errorf("serialized version prefix wrong: %x", raw[:4])
	}
	// locktime 600 LE suffix
	if raw[len(raw)-4] != 0x58 || raw[len(raw)-3] != 0x02 {
		t.Errorf("serialized locktime suffix wrong: %x", raw[len(raw)-4:])
	}

	// Signing must verify.
	ok, err := VerifyTxInput(tx, 0, inner, genPub, refundSig)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !ok {
		t.Error("signature did not verify")
	}
	// Tampering with the sighash must fail verification.
	tx.Outputs[0].Value = 999999
	if ok, _ := VerifyTxInput(tx, 0, inner, genPub, refundSig); ok {
		t.Error("verification should fail after output amount tampered")
	}
}

func testTx() *Tx {
	var prev [32]byte
	copy(prev[:], []byte("0123456789abcdef0123456789abcdef"))
	return &Tx{
		Version: 1,
		Inputs: []TxIn{{
			PrevOut:   OutPoint{Hash: prev, Index: 0},
			ScriptSig: []byte{},
			Sequence:  0xfffffffe,
		}},
		Outputs: []TxOut{{
			Value:        1000000,
			ScriptPubKey: BuildP2PKHScript(KeyID(genPub)),
		}},
		LockTime: 600,
	}
}

// indexOf returns the first index of sub within s at or after from, or -1.
func indexOf(s, sub []byte, from int) int {
	if from < 0 || from > len(s) {
		return -1
	}
	for i := from; i+len(sub) <= len(s); i++ {
		if equalBytes(s[i:i+len(sub)], sub) {
			return i
		}
	}
	return -1
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func hasSuffix(suffix, s []byte) bool {
	return len(suffix) <= len(s) && equalBytes(s[len(s)-len(suffix):], suffix)
}
