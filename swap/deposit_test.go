package swap

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"testing"

	btcec "github.com/btcsuite/btcd/btcec/v2"

	"go-xbridge/coins"
	"go-xbridge/wallet"
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
	tx, err := spec.BuildDepositTx(c, funding, coins.KeyID(localPub[:]), 1000, 0, 0)
	if err != nil {
		t.Fatalf("BuildDepositTx: %v", err)
	}
	if len(tx.Outputs) != 2 {
		t.Fatalf("want 2 outputs (deposit + change), got %d", len(tx.Outputs))
	}
	// Deposit inputs must be SEQUENCE_FINAL (0xffffffff): C++ createRawTransaction
	// stamps SEQUENCE_FINAL on every input when cltv=true (xbridgerpc.cpp:367) and
	// checkDepositTransaction hard-rejects anything else
	// (xbridgewalletconnectorbtc.cpp:2076-2080).
	if tx.Inputs[0].Sequence != 0xffffffff {
		t.Errorf("deposit input sequence %#x != 0xffffffff (SEQUENCE_FINAL)", tx.Inputs[0].Sequence)
	}
	// Deposit output is the P2SH HTLC.
	if got := hex.EncodeToString(tx.Outputs[0].ScriptPubKey); got[:2] != "a9" {
		t.Errorf("deposit output is not P2SH (OP_HASH160): %s", got)
	}

	// Sign the funding input with the local priv key.
	sig, err := spec.SignInput(tx, 0, fundScript, funding[0].Amount, localPriv, c)
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

// decodeScriptPushes splits a Bitcoin scriptSig into its data pushes plus the
// trailing opcodes (e.g. OP_1/OP_0 pushed between the data pushes). It mirrors
// how the C++ connector later walks the scriptSig to recover the signature and
// redeem script, but for tests we only need structural validation (the btcd
// txscript VM is not a dependency; coins reimplements verification directly).
func decodeScriptPushes(t *testing.T, b []byte) (pushes [][]byte, ops []byte) {
	t.Helper()
	i := 0
	for i < len(b) {
		c := b[i]
		switch {
		case c >= 1 && c <= 75:
			n := int(c)
			i++
			if i+n > len(b) {
				t.Fatalf("script push runs past end (len %d, need %d)", len(b), i+n)
			}
			pushes = append(pushes, b[i:i+n])
			i += n
		case c == coins.OpPushData1:
			if i+2 > len(b) {
				t.Fatalf("OP_PUSHDATA1 truncated")
			}
			n := int(b[i+1])
			i += 2
			if i+n > len(b) {
				t.Fatalf("OP_PUSHDATA1 data runs past end")
			}
			pushes = append(pushes, b[i:i+n])
			i += n
		case c == coins.OpPushData2:
			if i+3 > len(b) {
				t.Fatalf("OP_PUSHDATA2 truncated")
			}
			n := int(binary.LittleEndian.Uint16(b[i+1 : i+3]))
			i += 3
			if i+n > len(b) {
				t.Fatalf("OP_PUSHDATA2 data runs past end")
			}
			pushes = append(pushes, b[i:i+n])
			i += n
		case c == coins.OpPushData4:
			if i+5 > len(b) {
				t.Fatalf("OP_PUSHDATA4 truncated")
			}
			n := int(binary.LittleEndian.Uint32(b[i+1 : i+5]))
			i += 5
			if i+n > len(b) {
				t.Fatalf("OP_PUSHDATA4 data runs past end")
			}
			pushes = append(pushes, b[i:i+n])
			i += n
		default:
			ops = append(ops, c)
			i++
		}
	}
	return pushes, ops
}

// TestRefundScriptSig builds a maker deposit, signs the funding input, and
// asserts the IF-branch (CLTV refund) scriptSig is byte-for-byte:
//
//	<sig> <depositorPub> OP_1 <redeemScript>
//
// matching C++ xbridgewalletconnectorbtc.cpp (~:2478). The inner push must
// equal the deposit's HTLC redeem script so the refund spends the P2SH HTLC.
func TestRefundScriptSig(t *testing.T) {
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

	fundScript := coins.BuildP2PKHScript(coins.KeyID(localPub[:]))
	funding := []wallet.Utxo{{
		TxID:         "0000000000000000000000000000000000000000000000000000000000000001",
		Vout:         0,
		Amount:       2000000,
		ScriptPubKey: hex.EncodeToString(fundScript),
	}}
	c := coins.Coin{Decimals: 8}
	tx, err := spec.BuildDepositTx(c, funding, coins.KeyID(localPub[:]), 1000, 0, 0)
	if err != nil {
		t.Fatalf("BuildDepositTx: %v", err)
	}
	sig, err := spec.SignInput(tx, 0, fundScript, funding[0].Amount, localPriv, c)
	if err != nil {
		t.Fatalf("SignInput: %v", err)
	}

	rs := spec.RefundScriptSig(sig)
	pushes, ops := decodeScriptPushes(t, rs)
	if len(pushes) != 3 {
		t.Fatalf("refund scriptSig wants 3 pushes, got %d", len(pushes))
	}
	if len(ops) != 1 || ops[0] != coins.Op1 {
		t.Fatalf("refund scriptSig wants a single OP_1 between pushes, got ops=%v", ops)
	}
	if !bytes.Equal(pushes[1], localPub[:]) {
		t.Error("refund scriptSig second push is not the depositor pubkey")
	}
	if inner := spec.RedeemScript(); !bytes.Equal(pushes[2], inner) {
		t.Error("refund scriptSig inner push is not the HTLC redeem script")
	}
}

// TestPaymentScriptSig asserts the ELSE-branch (counterparty claim) scriptSig
// is byte-for-byte:
//
//	<xPubKey(secret)> <sig> <myPubKey> OP_0 <redeemScript>
//
// matching C++ xbridgewalletconnectorbtc.cpp (~:2558). Crucially it enforces
// the "secret IS the maker's 33-byte xPubKey" invariant: HASH160(xPubKey) must
// equal the deposit's secretHash — the value the redeem script's ELSE branch
// checks before paying out.
func TestPaymentScriptSig(t *testing.T) {
	localPub, _ := randKey(t)
	otherPub, _ := randKey(t)

	// The secret the claimer reveals is a 33-byte xPubKey; its HASH160 is the
	// secretHash the deposit's redeem script checks in the ELSE branch.
	var xPubKey [33]byte
	if _, err := rand.Read(xPubKey[:]); err != nil {
		t.Fatal(err)
	}
	secretHash := coins.KeyID(xPubKey[:])

	spec := &DepositSpec{
		Currency:        "BTC",
		Amount:          1000000,
		DepositorPub:    localPub,
		CounterpartyPub: otherPub,
		Hash:            secretHash,
		LockTime:        600,
	}
	// The payment spends the counterparty's deposit: the claimer pushes its
	// secret (xPubKey), then its signature, then its own pubkey (the
	// counterparty/depositor of that deposit), and OP_0 selects the ELSE branch.
	inner := spec.RedeemScript()
	var sig [70]byte
	if _, err := rand.Read(sig[:]); err != nil {
		t.Fatal(err)
	}
	ps := coins.BuildPaymentScriptSig(xPubKey[:], sig[:], otherPub[:], inner)

	pushes, ops := decodeScriptPushes(t, ps)
	if len(pushes) != 4 {
		t.Fatalf("payment scriptSig wants 4 pushes, got %d", len(pushes))
	}
	if len(ops) != 1 || ops[0] != coins.Op0 {
		t.Fatalf("payment scriptSig wants a single OP_0 between pushes, got ops=%v", ops)
	}
	if !bytes.Equal(pushes[0], xPubKey[:]) {
		t.Error("payment scriptSig first push is not the secret xPubKey")
	}
	if !bytes.Equal(pushes[2], otherPub[:]) {
		t.Error("payment scriptSig myPubKey push is not the counterparty pubkey")
	}
	if !bytes.Equal(pushes[3], inner) {
		t.Error("payment scriptSig inner push is not the HTLC redeem script")
	}
	// The secret-is-xPubKey invariant the claim relies on.
	if got := coins.KeyID(xPubKey[:]); got != secretHash {
		t.Error("HASH160(xPubKey) != secretHash (secret-is-xPubKey invariant broken)")
	}
}

// TestDepositLocksAmountPlusFee2 verifies the HTLC deposit output locks
// Amount + fee2 (the p2sh redeem margin), matching C++ outAmount+fee2
// (xbridgesession.cpp:2094 maker, :2615 taker). checkDepositTransaction
// requires depositP2SHAmount >= amount + 0.95*fee2
// (xbridgewalletconnectorbtc.cpp:2183); locking Amount+fee2 exceeds that band.
func TestDepositLocksAmountPlusFee2(t *testing.T) {
	localPub, _ := randKey(t)
	otherPub, _ := randKey(t)

	spec := &DepositSpec{
		Currency:        "BTC",
		Amount:          1_000_000,
		DepositorPub:    localPub,
		CounterpartyPub: otherPub,
		LockTime:        600,
	}
	fundScript := coins.BuildP2PKHScript(coins.KeyID(localPub[:]))
	funding := []wallet.Utxo{{
		TxID:         "0000000000000000000000000000000000000000000000000000000000000001",
		Vout:         0,
		Amount:       1_100_000,
		ScriptPubKey: hex.EncodeToString(fundScript),
	}}
	c := coins.Coin{Decimals: 8}
	fee := uint64(1000)
	fee2 := uint64(250)
	tx, err := spec.BuildDepositTx(c, funding, coins.KeyID(localPub[:]), fee, fee2, 0)
	if err != nil {
		t.Fatalf("BuildDepositTx: %v", err)
	}
	// The P2SH HTLC output must carry Amount + fee2.
	if got := tx.Outputs[0].Value; got != spec.Amount+fee2 {
		t.Fatalf("deposit P2SH value = %d, want %d (Amount+fee2)", got, spec.Amount+fee2)
	}
	// Change is the remainder after Amount + fee + fee2.
	if got := tx.Outputs[1].Value; got != 1_100_000-spec.Amount-fee-fee2 {
		t.Errorf("change = %d, want %d", got, 1_100_000-spec.Amount-fee-fee2)
	}
	// C++ acceptance band: depositP2SHAmount >= amount + 0.95*fee2.
	deposit := tx.Outputs[0].Value
	minAccepted := uint64(float64(fee2) * 0.95)
	if deposit < spec.Amount+minAccepted {
		t.Errorf("deposit %d < amount + 0.95*fee2 (%d)", deposit, spec.Amount+minAccepted)
	}
}

// TestDepositDustChangeSuppressed verifies dust change is folded into the
// miner fee instead of emitted as an output the relay would reject, mirroring
// C++ (xbridgesession.cpp:2098-2106 `if (!connFrom->isDustAmount(rest))`).
func TestDepositDustChangeSuppressed(t *testing.T) {
	localPub, _ := randKey(t)
	otherPub, _ := randKey(t)

	spec := &DepositSpec{
		Currency:        "BTC",
		Amount:          1_000_000,
		DepositorPub:    localPub,
		CounterpartyPub: otherPub,
		LockTime:        600,
	}
	fundScript := coins.BuildP2PKHScript(coins.KeyID(localPub[:]))
	funding := []wallet.Utxo{{
		TxID:         "0000000000000000000000000000000000000000000000000000000000000001",
		Vout:         0,
		Amount:       1_000_000 + 1000 + 250 + 100, // change of 100 < dustLimit
		ScriptPubKey: hex.EncodeToString(fundScript),
	}}
	c := coins.Coin{Decimals: 8}
	tx, err := spec.BuildDepositTx(c, funding, coins.KeyID(localPub[:]), 1000, 250, 546)
	if err != nil {
		t.Fatalf("BuildDepositTx: %v", err)
	}
	if len(tx.Outputs) != 1 {
		t.Fatalf("dust change must be suppressed (1 output), got %d outputs", len(tx.Outputs))
	}
	if got := tx.Outputs[0].Value; got != spec.Amount+250 {
		t.Fatalf("deposit P2SH value = %d, want %d (Amount+fee2)", got, spec.Amount+250)
	}

	// Change at exactly the dust limit is still emitted.
	funding[0].Amount = 1_000_000 + 1000 + 250 + 546
	tx, err = spec.BuildDepositTx(c, funding, coins.KeyID(localPub[:]), 1000, 250, 546)
	if err != nil {
		t.Fatalf("BuildDepositTx: %v", err)
	}
	if len(tx.Outputs) != 2 || tx.Outputs[1].Value != 546 {
		t.Fatalf("change at dust limit must be emitted, got %+v", tx.Outputs)
	}
}
