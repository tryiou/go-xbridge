package wallet

import (
	"encoding/hex"
	"testing"

	"go-xbridge/coins"
	"go-xbridge/config"
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

// testHTLCFixture is the shared HTLC deposit/signer fixture for the
// LocalConnector signing tests: a 1-input/1-output deposit paying a P2SH HTLC
// redeem script, its prevout, and the local signer holding the maker key.
type testHTLCFixture struct {
	signer     *testHTLCSigner
	depositHex string
	prev       PrevTx
	myPub      []byte
	inner      []byte
}

func newTestHTLCFixture(t *testing.T) testHTLCFixture {
	t.Helper()
	// secp256k1 generator private key (32 bytes, last byte 0x01).
	myPriv := make([]byte, 32)
	myPriv[31] = 0x01
	myPub := mustHex(t, "0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798")
	otherPub := mustHex(t, "02f9308a019258c31049344f85f89d5229b531c845836f99b08601f113bce036f9")
	secretHash := mustHex(t, "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")

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

	return testHTLCFixture{
		signer:     &testHTLCSigner{myPriv: myPriv, myPub: myPub, otherPub: otherPub, inner: inner},
		depositHex: hex.EncodeToString(deposit.Serialize()),
		prev: PrevTx{
			TxID:         "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			Vout:         0,
			ScriptPubKey: hex.EncodeToString(p2sh),
			Amount:       1_000_000,
		},
		myPub: myPub,
		inner: inner,
	}
}

func TestLocalConnectorSignHTLC(t *testing.T) {
	f := newTestHTLCFixture(t)
	signer, depositHex, prev, myPub, inner := f.signer, f.depositHex, f.prev, f.myPub, f.inner
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

// TestLocalConnectorSignAfterRegistryFlip closure. LocalConnector
// must bind its coin's serializeWithTimeField flag at construction: a reload
// that flips the live registry mid-run must not re-interpret the txHex a
// signer is re-serializing. The connector is built while the registry is empty
// (captured flag false); after the registry flips BTC to TxWithTimeField=true,
// signing the same no-time-field deposit must still succeed and produce a
// byte-identical scriptSig (a live read would shift the layout and fail the
// parse).
func TestLocalConnectorSignAfterRegistryFlip(t *testing.T) {
	// Make the test self-contained: the connector must capture hasTimeField
	// from an empty registry (false), regardless of what earlier tests left in
	// the global registry. The defer below restores the same empty state.
	if err := coins.InitFromConf(map[string]*config.CoinConf{}); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = coins.InitFromConf(map[string]*config.CoinConf{}) }()
	f := newTestHTLCFixture(t)
	c := NewLocalConnector("BTC", f.signer, nil)

	sign := func() string {
		t.Helper()
		signedHex, complete, err := c.SignRawTransaction(f.depositHex, []PrevTx{f.prev})
		if err != nil {
			t.Fatalf("SignRawTransaction: %v", err)
		}
		if !complete {
			t.Fatal("expected complete=true")
		}
		signed, err := coins.Deserialize(mustHex(t, signedHex))
		if err != nil {
			t.Fatalf("deserialize signed: %v", err)
		}
		return hex.EncodeToString(signed.Inputs[0].ScriptSig)
	}
	golden := sign()

	if err := coins.InitFromConf(map[string]*config.CoinConf{
		"BTC": {Ticker: "BTC", Coin: 1e8, AddressPrefix: 0, CreateTxMethod: "BTC", BlockTime: 60, TxWithTimeField: true},
	}); err != nil {
		t.Fatal(err)
	}

	if got := sign(); got != golden {
		t.Fatalf("scriptSig changed after registry flip:\n got %s\nwant %s", got, golden)
	}
}

// TestLocalConnectorGetBlockTxsHasNoBlockSource pins the rescan degradation:
// LocalConnector must fail block-body reads (never an empty list, which the
// rescan would misread as a skippable empty block).
func TestLocalConnectorGetBlockTxsHasNoBlockSource(t *testing.T) {
	lc := &LocalConnector{}
	if _, err := lc.GetBlockTxs([32]byte{0x01}); err == nil {
		t.Fatal("GetBlock succeeded without a block source")
	}
}
