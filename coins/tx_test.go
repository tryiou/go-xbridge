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

// TestTxWithTimeField verifies the per-coin serializeWithTimeField quirk
// (xbridge/xbitcointransaction.h:77-84): when WithTime is set, Serialize writes
// exactly 4 extra bytes (the nTime uint32) immediately after nVersion, and
// DeserializeWithTime(b, true) round-trips it. DeserializeWithTime(b, false)
// does NOT read the field, so the flag must be supplied by the caller — it
// cannot be detected on the wire. C4.
func TestTxWithTimeField(t *testing.T) {
	inner := BuildDepositUnlockScript(genPub, genPub, []byte("0123456789abcdef0123456789abcdef01234567"), 600)
	p2sh := BuildP2SHScript(KeyID(inner))
	tx := &Tx{
		Version: 2,
		Inputs: []TxIn{{
			PrevOut:   OutPoint{Index: 3},
			ScriptSig: []byte{0x01, 0x02, 0x03},
			Sequence:  0xffffffff,
		}},
		Outputs:  []TxOut{{Value: 1_000_000, ScriptPubKey: p2sh}},
		LockTime: 0,
	}

	rawNoTime := tx.Serialize()

	// With WithTime, serialization must carry exactly 4 extra bytes (nTime).
	tx.WithTime = true
	tx.TxTime = 0x12345678
	rawWithTime := tx.Serialize()
	if len(rawWithTime) != len(rawNoTime)+4 {
		t.Fatalf("with-time length %d, want %d (+4 nTime)", len(rawWithTime), len(rawNoTime)+4)
	}
	// nTime sits right after the 4-byte nVersion (LE uint32 of 0x12345678).
	if string(rawWithTime[4:8]) != string([]byte{0x78, 0x56, 0x34, 0x12}) {
		t.Errorf("nTime bytes = %x, want 78563412", rawWithTime[4:8])
	}

	// DeserializeWithTime(b, true) recovers the field.
	back, err := DeserializeWithTime(rawWithTime, true)
	if err != nil {
		t.Fatalf("DeserializeWithTime(true): %v", err)
	}
	if !back.WithTime {
		t.Error("WithTime not set after DeserializeWithTime(true)")
	}
	if back.TxTime != 0x12345678 {
		t.Errorf("TxTime = %#x, want 0x12345678", back.TxTime)
	}
	if string(back.Serialize()) != string(rawWithTime) {
		t.Error("re-serialize (with time) mismatch")
	}

	// DeserializeWithTime(b, false) ignores the time field entirely: a blob with
	// NO nTime, parsed with the flag off, round-trips as a normal tx and leaves
	// WithTime false. This is the contract the swap layer relies on for coins
	// that do NOT set serializeWithTimeField.
	plain, err := DeserializeWithTime(rawNoTime, false)
	if err != nil {
		t.Fatalf("DeserializeWithTime(false) on no-time blob: %v", err)
	}
	if plain.WithTime {
		t.Error("WithTime should be false after DeserializeWithTime(false) on no-time blob")
	}
	if plain.TxTime != 0 {
		t.Errorf("TxTime should be 0, got %#x", plain.TxTime)
	}
	if string(plain.Serialize()) != string(rawNoTime) {
		t.Error("re-serialize (no-time blob, flag off) mismatch")
	}

	// The wire format is ambiguous: a time-bearing blob parsed with the flag OFF
	// must NOT silently succeed with a wrong layout — it errors, proving the
	// caller is responsible for passing the correct hasTime. This is why the
	// swap layer threads the per-coin TxWithTimeField flag through.
	if _, err := DeserializeWithTime(rawWithTime, false); err == nil {
		t.Error("expected error deserializing time-bearing blob with hasTime=false")
	}
}

func TestKeyID(t *testing.T) {
	id := KeyID(genPub)
	if len(id) != 20 {
		t.Fatalf("KeyID len = %d, want 20", len(id))
	}
	// Independent oracle: HASH160 of the secp256k1 generator point, verified
	// with python hashlib (sha256+ripemd160) outside this repo. This is the
	// well-known identifier behind Bitcoin address 1BgGZ9tcN4rm9KBzDn7KprQzEPM8nQ265c.
	const want = "751e76e8199196d454941c45d1b3a323f1433bd6"
	if got := hex.EncodeToString(id[:]); got != want {
		t.Errorf("KeyID(genPub) = %s, want %s", got, want)
	}
}

func TestBuildDepositUnlockScript(t *testing.T) {
	myPub := mustDecode(t, "0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798")
	otherPub := mustDecode(t, "02f9308a019258c31049344f85f89d5229b531c845836f99b08601f113bce036f9")
	secret := mustDecode(t, "0123456789abcdef0123456789abcdef01234567")
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

// hash32 copies hex into a [32]byte (wire/internal byte order, verbatim).
func hash32(t *testing.T, s string) [32]byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 32 {
		t.Fatalf("bad 32-byte hex %q: %v", s, err)
	}
	var h [32]byte
	copy(h[:], b)
	return h
}

func mustDecode(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("bad hex %q: %v", s, err)
	}
	return b
}

// TestBIP143NativeP2WPKH checks HashForSigningSegwit against the canonical
// BIP143 native-P2WPKH vector (input 1). The final digest must equal the
// spec's published sigHash, which validates hashPrevouts/hashSequence/
// hashOutputs and the preimage layout end-to-end.
// https://github.com/bitcoin/bips/blob/master/bip-0143.mediawiki#native-p2wpkh
func TestBIP143NativeP2WPKH(t *testing.T) {
	tx := &Tx{
		Version: 1,
		Inputs: []TxIn{
			{
				PrevOut:  OutPoint{Hash: hash32(t, "fff7f7881a8099afa6940d42d1e7f6362bec38171ea3edf433541db4e4ad969f"), Index: 0},
				Sequence: 0xffffffee,
			},
			{
				PrevOut:  OutPoint{Hash: hash32(t, "ef51e1b804cc89d182d279655c3aa89e815b1b309fe287d9b2b55d57b90ec68a"), Index: 1},
				Sequence: 0xffffffff,
			},
		},
		Outputs: []TxOut{
			{Value: 112340000, ScriptPubKey: mustDecode(t, "76a9148280b37df378db99f66f85c95a783a76ac7a6d5988ac")},
			{Value: 223450000, ScriptPubKey: mustDecode(t, "76a9143bde42dbee7e4dbe6a21b2d50ce2f0167faa815988ac")},
		},
		LockTime: 17,
	}

	pub := mustDecode(t, "025476c2e83188368da1ff3e292e7acafcdb3566bb0ad253f62fc70f07aeee6357")
	// The P2WPKH scriptCode is the implied P2PKH script over HASH160(pubkey).
	keyHash := KeyID(pub)
	if got := hex.EncodeToString(keyHash[:]); got != "1d0f172a0ecb48aee1be1f2687d2963ae33f71a1" {
		t.Fatalf("KeyID(pub) = %s, want the BIP143 witness program hash", got)
	}
	scriptCode := P2WPKHScriptCode(keyHash[:])

	const amount = 600000000
	got, err := tx.HashForSigningSegwit(1, scriptCode, amount)
	if err != nil {
		t.Fatalf("HashForSigningSegwit: %v", err)
	}
	want := "c37af31116d1b27caf68aae9e3ac82f1477929014d5b917657d0eb49478cb670"
	if hex.EncodeToString(got[:]) != want {
		t.Errorf("BIP143 native P2WPKH sigHash\n got %s\nwant %s", hex.EncodeToString(got[:]), want)
	}

	// Sign with the vector's private key and verify the round-trip.
	priv := mustDecode(t, "619c335025c7f4012e556c2a58b2506e30b8511b53ade95ea316fd8c3286feb9")
	sig, err := SignTxInputSegwit(tx, 1, scriptCode, amount, priv)
	if err != nil {
		t.Fatalf("SignTxInputSegwit: %v", err)
	}
	ok, err := VerifyTxInputSegwit(tx, 1, scriptCode, pub, amount, sig)
	if err != nil || !ok {
		t.Fatalf("VerifyTxInputSegwit ok=%v err=%v", ok, err)
	}
	// Tampering the spent amount must break verification (BIP143 commits to it).
	if ok, _ := VerifyTxInputSegwit(tx, 1, scriptCode, pub, amount+1, sig); ok {
		t.Error("verification should fail when the committed amount changes")
	}
}

// TestBIP143NestedP2SHP2WPKH checks HashForSigningSegwit against the canonical
// BIP143 P2SH-P2WPKH vector. The scriptCode is again the implied P2PKH script
// over the witness key hash.
// https://github.com/bitcoin/bips/blob/master/bip-0143.mediawiki#p2sh-p2wpkh
func TestBIP143NestedP2SHP2WPKH(t *testing.T) {
	tx := &Tx{
		Version: 1,
		Inputs: []TxIn{{
			PrevOut:  OutPoint{Hash: hash32(t, "db6b1b20aa0fd7b23880be2ecbd4a98130974cf4748fb66092ac4d3ceb1a5477"), Index: 1},
			Sequence: 0xfffffffe,
		}},
		Outputs: []TxOut{
			{Value: 199996600, ScriptPubKey: mustDecode(t, "76a914a457b684d7f0d539a46a45bbc043f35b59d0d96388ac")},
			{Value: 800000000, ScriptPubKey: mustDecode(t, "76a914fd270b1ee6abcaea97fea7ad0402e8bd8ad6d77c88ac")},
		},
		LockTime: 1170,
	}
	// Witness key hash from the vector's redeemScript 0014{keyhash}.
	scriptCode := P2WPKHScriptCode(mustDecode(t, "79091972186c449eb1ded22b78e40d009bdf0089"))
	const amount = 1000000000
	got, err := tx.HashForSigningSegwit(0, scriptCode, amount)
	if err != nil {
		t.Fatalf("HashForSigningSegwit: %v", err)
	}
	want := "64f3b0f4dd2bb3aa1ce8566d220cc74dda9df97d8490cc81d89d735c92e59fb6"
	if hex.EncodeToString(got[:]) != want {
		t.Errorf("BIP143 P2SH-P2WPKH sigHash\n got %s\nwant %s", hex.EncodeToString(got[:]), want)
	}
}

// TestHashForSigningWithTimeField pins the WithTime digest: when WithTime is set,
// HashForSigning must write the 4-byte nTime immediately after nVersion,
// mirroring CTransactionSignatureSerializer::Serialize
// (xbitcointransaction.h:265-269). Golden digests are generated by a C++ oracle
// reproducing that serializer (g++ + OpenSSL), cross-checked against an
// independent Python reference; the no-time row also matches the untouched
// legacy SIGHASH_ALL path.
func TestHashForSigningWithTimeField(t *testing.T) {
	// txid "abcdef...89" in internal (little-endian) outpoint byte order.
	prev, err := hex.DecodeString("8967452301efcdab8967452301efcdab8967452301efcdab8967452301efcdab")
	if err != nil {
		t.Fatal(err)
	}
	var prevHash [32]byte
	copy(prevHash[:], prev)
	// inner = OP_HASH160 PUSHDATA(20 zero) OP_EQUAL; out = P2PKH to 20 zero.
	inner := []byte{0xa9, 0x14}
	inner = append(inner, make([]byte, 20)...)
	inner = append(inner, 0x87)
	out := []byte{0x76, 0xa9, 0x14}
	out = append(out, make([]byte, 20)...)
	out = append(out, 0x88, 0xac)

	tx := &Tx{
		Version: 1,
		Inputs: []TxIn{{
			PrevOut:  OutPoint{Hash: prevHash, Index: 0},
			Sequence: 0xfffffffe,
		}},
		Outputs: []TxOut{{
			Value:        1234567,
			ScriptPubKey: out,
		}},
		LockTime: 600,
	}

	t.Run("no-time", func(t *testing.T) {
		got := tx.HashForSigning(0, inner)
		want := "2120a1e31a1d5f69359a1a2849cf957827b67a5d4fe29048c6bbfa7cbe2308db"
		if h := hex.EncodeToString(got[:]); h != want {
			t.Errorf("legacy sighash (no nTime)\n got %s\nwant %s", h, want)
		}
	})

	t.Run("with-time", func(t *testing.T) {
		tx.WithTime = true
		tx.TxTime = 0x5f2a1b3c
		got := tx.HashForSigning(0, inner)
		want := "72ad1d63180769a9dcf27d052c1f2d382d719bf7a513180bdd19eb3e1721168c"
		if h := hex.EncodeToString(got[:]); h != want {
			t.Errorf("sighash with nTime=0x5f2a1b3c\n got %s\nwant %s", h, want)
		}
		// The with-time digest must differ from the no-time one.
		tx.WithTime = false
		if plain := tx.HashForSigning(0, inner); plain == got {
			t.Error("with-time sighash equals no-time sighash; nTime not committed")
		}
	})
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

// TestHashForSigningBIP143MatchesSegwit pins the parameterized BIP143 digest:
// passing SigHashAll must reproduce the old HashForSigningSegwit output on the
// canonical native-P2WPKH vector.
func TestHashForSigningBIP143MatchesSegwit(t *testing.T) {
	tx := &Tx{
		Version: 1,
		Inputs: []TxIn{
			{PrevOut: OutPoint{Hash: hash32(t, "fff7f7881a8099afa6940d42d1e7f6362bec38171ea3edf433541db4e4ad969f"), Index: 0}, Sequence: 0xffffffee},
			{PrevOut: OutPoint{Hash: hash32(t, "ef51e1b804cc89d182d279655c3aa89e815b1b309fe287d9b2b55d57b90ec68a"), Index: 1}, Sequence: 0xffffffff},
		},
		Outputs: []TxOut{
			{Value: 112340000, ScriptPubKey: mustDecode(t, "76a9148280b37df378db99f66f85c95a783a76ac7a6d5988ac")},
			{Value: 223450000, ScriptPubKey: mustDecode(t, "76a9143bde42dbee7e4dbe6a21b2d50ce2f0167faa815988ac")},
		},
		LockTime: 17,
	}
	scriptCode := P2WPKHScriptCode(mustDecode(t, "1d0f172a0ecb48aee1be1f2687d2963ae33f71a1"))
	const amount = 600000000

	got, err := tx.HashForSigningBIP143(1, scriptCode, amount, SigHashAll)
	if err != nil {
		t.Fatalf("HashForSigningBIP143: %v", err)
	}
	old, err := tx.HashForSigningSegwit(1, scriptCode, amount)
	if err != nil {
		t.Fatalf("HashForSigningSegwit: %v", err)
	}
	if got != old {
		t.Errorf("parameterized BIP143 (SigHashAll) != HashForSigningSegwit")
	}
	if h := hex.EncodeToString(got[:]); h != "c37af31116d1b27caf68aae9e3ac82f1477929014d5b917657d0eb49478cb670" {
		t.Errorf("BIP143 native P2WPKH sigHash\n got %s", h)
	}
}

// TestHashForSigningForkID pins the forkid digest: BCH/DEVAULT fork value 0
// commits hashType 0x41, BTG fork value 79 commits 0x4F41, and both must differ
// from the plain BIP143 SIGHASH_ALL digest (proving the fork value is committed).
func TestHashForSigningForkID(t *testing.T) {
	tx := &Tx{
		Version: 2,
		Inputs: []TxIn{{
			PrevOut:  OutPoint{Hash: hash32(t, "8967452301efcdab8967452301efcdab8967452301efcdab8967452301efcdab"), Index: 0},
			Sequence: 0xfffffffe,
		}},
		Outputs: []TxOut{{
			Value:        1234567,
			ScriptPubKey: BuildP2PKHScript(KeyID(genPub)),
		}},
		LockTime: 600,
	}
	// Arbitrary scriptCode bytes; content is irrelevant to digest construction.
	inner := []byte{0xa9, 0x14}
	inner = append(inner, make([]byte, 20)...)
	inner = append(inner, 0x87)
	const amount = 100000000

	plain, err := tx.HashForSigningBIP143(0, inner, amount, SigHashAll)
	if err != nil {
		t.Fatalf("HashForSigningBIP143: %v", err)
	}

	for _, tc := range []struct {
		forkValue uint32
		hashType  uint32
	}{
		{forkValue: 0, hashType: SigHashForkID | SigHashAll},          // BCH/DEVAULT -> 0x41
		{forkValue: 79, hashType: 79<<8 | SigHashForkID | SigHashAll}, // BTG -> 0x4F41
	} {
		got, err := tx.HashForSigningForkID(0, inner, amount, tc.forkValue)
		if err != nil {
			t.Fatalf("HashForSigningForkID(%d): %v", tc.forkValue, err)
		}
		direct, err := tx.HashForSigningBIP143(0, inner, amount, tc.hashType)
		if err != nil {
			t.Fatalf("HashForSigningBIP143(%#x): %v", tc.hashType, err)
		}
		if got != direct {
			t.Errorf("forkid(%d) digest != BIP143 hashType %#x", tc.forkValue, tc.hashType)
		}
		if got == plain {
			t.Errorf("forkid(%d) digest equals plain SIGHASH_ALL digest; fork value not committed", tc.forkValue)
		}
	}
}

// TestSignTxInputForkIDRoundTrip signs with the forkid digest and verifies: the
// trailing sighash byte is 0x41, verification round-trips, and tampering the
// committed amount breaks verification.
func TestSignTxInputForkIDRoundTrip(t *testing.T) {
	tx := &Tx{
		Version: 2,
		Inputs: []TxIn{{
			PrevOut:  OutPoint{Hash: hash32(t, "8967452301efcdab8967452301efcdab8967452301efcdab8967452301efcdab"), Index: 0},
			Sequence: 0xfffffffe,
		}},
		Outputs: []TxOut{{
			Value:        1234567,
			ScriptPubKey: BuildP2PKHScript(KeyID(genPub)),
		}},
		LockTime: 600,
	}
	inner := []byte{0xa9, 0x14}
	inner = append(inner, make([]byte, 20)...)
	inner = append(inner, 0x87)
	const amount = 100000000

	sig, err := SignTxInputForkID(tx, 0, inner, amount, 0, genPriv)
	if err != nil {
		t.Fatalf("SignTxInputForkID: %v", err)
	}
	if sig[len(sig)-1] != SigHashForkID|SigHashAll {
		t.Errorf("forkid signature trailing byte = %#x, want 0x41", sig[len(sig)-1])
	}
	ok, err := VerifyTxInputForkID(tx, 0, inner, genPub, amount, 0, sig)
	if err != nil || !ok {
		t.Fatalf("VerifyTxInputForkID ok=%v err=%v", ok, err)
	}
	if ok, err := VerifyTxInputForkID(tx, 0, inner, genPub, amount+1, 0, sig); ok || err != nil {
		t.Errorf("tampered amount: verification ok=%v err=%v, want false/nil", ok, err)
	}
	// The legacy verifier must reject the 0x41 sighash byte (wrong algorithm).
	if _, err := VerifyTxInput(tx, 0, inner, genPub, sig); err == nil {
		t.Error("legacy verifier accepted a forkid signature")
	}
}

// TestSignTxInputForCoinDispatch checks the per-coin dispatcher selects the
// forkid digest for forkid coins and the legacy digest otherwise.
func TestSignTxInputForCoinDispatch(t *testing.T) {
	tx := &Tx{
		Version: 2,
		Inputs: []TxIn{{
			PrevOut:  OutPoint{Hash: hash32(t, "8967452301efcdab8967452301efcdab8967452301efcdab8967452301efcdab"), Index: 0},
			Sequence: 0xfffffffe,
		}},
		Outputs: []TxOut{{
			Value:        1234567,
			ScriptPubKey: BuildP2PKHScript(KeyID(genPub)),
		}},
		LockTime: 600,
	}
	// Arbitrary scriptCode bytes; content is irrelevant to digest construction.
	inner := []byte{0xa9, 0x14}
	inner = append(inner, make([]byte, 20)...)
	inner = append(inner, 0x87)
	const amount = 100000000

	bch := Coin{Ticker: "BCH", signature: SigForkID, forkValue: 0xffdead}
	btg := Coin{Ticker: "BTG", signature: SigForkID, forkValue: 79}
	btc := Coin{Ticker: "BTC", signature: SigLegacy}

	sigBCH, err := SignTxInputForCoin(tx, 0, inner, amount, genPriv, bch)
	if err != nil {
		t.Fatalf("SignTxInputForCoin(BCH): %v", err)
	}
	if sigBCH[len(sigBCH)-1] != SigHashForkID|SigHashAll {
		t.Errorf("BCH signature byte = %#x, want 0x41", sigBCH[len(sigBCH)-1])
	}
	if ok, _ := VerifyTxInputForkID(tx, 0, inner, genPub, amount, 0xffdead, sigBCH); !ok {
		t.Error("BCH dispatch signature failed forkid-0xffdead verification")
	}

	sigBTG, err := SignTxInputForCoin(tx, 0, inner, amount, genPriv, btg)
	if err != nil {
		t.Fatalf("SignTxInputForCoin(BTG): %v", err)
	}
	if ok, _ := VerifyTxInputForkID(tx, 0, inner, genPub, amount, 79, sigBTG); !ok {
		t.Error("BTG dispatch signature failed forkid-79 verification")
	}
	// The same key under a different fork value must not verify.
	if ok, _ := VerifyTxInputForkID(tx, 0, inner, genPub, amount, 0, sigBTG); ok {
		t.Error("BTG signature verified under fork value 0")
	}

	sigBTC, err := SignTxInputForCoin(tx, 0, inner, amount, genPriv, btc)
	if err != nil {
		t.Fatalf("SignTxInputForCoin(BTC): %v", err)
	}
	if sigBTC[len(sigBTC)-1] != SigHashAll {
		t.Errorf("BTC signature byte = %#x, want 0x01", sigBTC[len(sigBTC)-1])
	}
	if ok, _ := VerifyTxInput(tx, 0, inner, genPub, sigBTC); !ok {
		t.Error("BTC dispatch signature failed legacy verification")
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
