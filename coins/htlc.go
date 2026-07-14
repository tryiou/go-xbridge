package coins

import (
	"crypto/sha256"

	"golang.org/x/crypto/ripemd160"
)

// KeyID returns HASH160(pubKey) — the 20-byte identifier Bitcoin scripts use.
// Matches C++ getKeyId(pubKey). Works for compressed (33-byte) pubkeys.
func KeyID(pubKey []byte) [20]byte {
	h1 := sha256.Sum256(pubKey)
	h2 := ripemd160.New()
	h2.Write(h1[:])
	var out [20]byte
	copy(out[:], h2.Sum(nil))
	return out
}

// BuildDepositUnlockScript builds the XBridge HTLC redeem script — the C++
// createDepositUnlockScript inner script. The IF branch is the lockTime refund
// path paying myPubKey; the ELSE branch requires otherPubKey's signature AND
// the 33-byte secret preimage whose HASH160 equals secretHash (so revealing it
// lets the counterparty claim).
//
//	OP_IF
//	    <lockTime> OP_CHECKLOCKTIMEVERIFY OP_DROP
//	    OP_DUP OP_HASH160 <KeyID(myPubKey)> OP_EQUALVERIFY OP_CHECKSIG
//	OP_ELSE
//	    OP_DUP OP_HASH160 <KeyID(otherPubKey)> OP_EQUALVERIFY OP_CHECKSIGVERIFY
//	    OP_SIZE 33 OP_EQUALVERIFY OP_HASH160 <secretHash> OP_EQUAL
//	OP_ENDIF
func BuildDepositUnlockScript(myPubKey, otherPubKey, secretHash []byte, lockTime uint32) []byte {
	myID := KeyID(myPubKey)
	otherID := KeyID(otherPubKey)

	// IF branch.
	s := []byte{OpIf}
	s = append(s, pushNum(int64(lockTime))...)
	s = append(s, OpCheckLockTimeVerify, OpDrop)
	s = append(s, OpDup, OpHash160)
	s = append(s, pushData(myID[:])...)
	s = append(s, OpEqualVerify, OpCheckSig)

	// ELSE branch.
	s = append(s, OpElse)
	s = append(s, OpDup, OpHash160)
	s = append(s, pushData(otherID[:])...)
	s = append(s, OpEqualVerify, OpCheckSigVerify)
	s = append(s, OpSize)
	s = append(s, pushNum(33)...)
	s = append(s, OpEqualVerify, OpHash160)
	s = append(s, pushData(secretHash)...)
	s = append(s, OpEqual)

	s = append(s, OpEndIf)
	return s
}

// BuildRefundScriptSig assembles the scriptSig to spend the IF (refund) branch
// (C++ refund redeem): <sig> <myPubKey> OP_1 <innerScript>.
func BuildRefundScriptSig(sig, myPubKey, innerScript []byte) []byte {
	s := pushData(sig)
	s = append(s, pushData(myPubKey)...)
	s = append(s, Op1)
	s = append(s, pushData(innerScript)...)
	return s
}

// BuildPaymentScriptSig assembles the scriptSig to spend the ELSE (payment)
// branch (C++ payment redeem): <xPubKey> <sig> <myPubKey> OP_0 <innerScript>.
func BuildPaymentScriptSig(xPubKey, sig, myPubKey, innerScript []byte) []byte {
	s := pushData(xPubKey)
	s = append(s, pushData(sig)...)
	s = append(s, pushData(myPubKey)...)
	s = append(s, Op0)
	s = append(s, pushData(innerScript)...)
	return s
}
