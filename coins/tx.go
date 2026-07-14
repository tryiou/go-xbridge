package coins

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"

	btcec "github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
)

// SigHashAll is the only sighash type XBridge uses for deposit/refund/payment.
const SigHashAll = 0x01

// OutPoint identifies a previous transaction output being spent.
type OutPoint struct {
	Hash  [32]byte
	Index uint32
}

// TxIn is a transaction input.
type TxIn struct {
	PrevOut   OutPoint
	ScriptSig []byte
	Sequence  uint32
}

// TxOut is a transaction output.
type TxOut struct {
	Value        uint64 // base units (satoshis etc.)
	ScriptPubKey []byte
}

// Tx is a Bitcoin-style transaction. SegWit selects the 0x0001 marker form on
// serialization; the deposit/refund layer here uses legacy (P2SH) scripts.
type Tx struct {
	Version  int32
	LockTime uint32
	Inputs   []TxIn
	Outputs  []TxOut
	SegWit   bool
}

// varInt encodes a Bitcoin variable-length integer.
func varInt(n int) []byte {
	switch {
	case n < 0xfd:
		return []byte{byte(n)}
	case n <= 0xffff:
		b := make([]byte, 3)
		b[0] = 0xfd
		binary.LittleEndian.PutUint16(b[1:], uint16(n))
		return b
	case n <= 0xffffffff:
		b := make([]byte, 5)
		b[0] = 0xfe
		binary.LittleEndian.PutUint32(b[1:], uint32(n))
		return b
	default:
		b := make([]byte, 9)
		b[0] = 0xff
		binary.LittleEndian.PutUint64(b[1:], uint64(n))
		return b
	}
}

// Serialize produces the wire bytes (classic, or segwit-marker form when
// SegWit is set). The scriptSig of each input is serialized verbatim.
func (t *Tx) Serialize() []byte {
	buf := make([]byte, 0, 256)
	var v [4]byte
	binary.LittleEndian.PutUint32(v[:], uint32(t.Version))
	buf = append(buf, v[:]...)
	if t.SegWit {
		buf = append(buf, 0x00, 0x01) // marker + flag
	}
	buf = append(buf, varInt(len(t.Inputs))...)
	for _, in := range t.Inputs {
		buf = append(buf, in.PrevOut.Hash[:]...)
		var idx [4]byte
		binary.LittleEndian.PutUint32(idx[:], in.PrevOut.Index)
		buf = append(buf, idx[:]...)
		buf = append(buf, varInt(len(in.ScriptSig))...)
		buf = append(buf, in.ScriptSig...)
		var seq [4]byte
		binary.LittleEndian.PutUint32(seq[:], in.Sequence)
		buf = append(buf, seq[:]...)
	}
	buf = append(buf, varInt(len(t.Outputs))...)
	for _, out := range t.Outputs {
		var val [8]byte
		binary.LittleEndian.PutUint64(val[:], out.Value)
		buf = append(buf, val[:]...)
		buf = append(buf, varInt(len(out.ScriptPubKey))...)
		buf = append(buf, out.ScriptPubKey...)
	}
	var lt [4]byte
	binary.LittleEndian.PutUint32(lt[:], t.LockTime)
	buf = append(buf, lt[:]...)
	return buf
}

// HashForSigning computes the legacy SIGHASH_ALL digest for input idx, with that
// input's scriptSig replaced by prevScript (the redeem/inner script) and all
// other inputs blanked. This matches C++ SignatureHash(inner, tx, idx,
// SIGHASH_ALL) for non-segwit transactions.
func (t *Tx) HashForSigning(idx int, prevScript []byte) [32]byte {
	buf := make([]byte, 0, 256)
	var v [4]byte
	binary.LittleEndian.PutUint32(v[:], uint32(t.Version))
	buf = append(buf, v[:]...)
	buf = append(buf, varInt(len(t.Inputs))...)
	for i, in := range t.Inputs {
		buf = append(buf, in.PrevOut.Hash[:]...)
		var ix [4]byte
		binary.LittleEndian.PutUint32(ix[:], in.PrevOut.Index)
		buf = append(buf, ix[:]...)
		script := []byte{}
		if i == idx {
			script = prevScript
		}
		buf = append(buf, varInt(len(script))...)
		buf = append(buf, script...)
		var seq [4]byte
		binary.LittleEndian.PutUint32(seq[:], in.Sequence)
		buf = append(buf, seq[:]...)
	}
	buf = append(buf, varInt(len(t.Outputs))...)
	for _, out := range t.Outputs {
		var val [8]byte
		binary.LittleEndian.PutUint64(val[:], out.Value)
		buf = append(buf, val[:]...)
		buf = append(buf, varInt(len(out.ScriptPubKey))...)
		buf = append(buf, out.ScriptPubKey...)
	}
	var lt [4]byte
	binary.LittleEndian.PutUint32(lt[:], t.LockTime)
	buf = append(buf, lt[:]...)
	// Sighash type (4-byte LE).
	var sh [4]byte
	binary.LittleEndian.PutUint32(sh[:], SigHashAll)
	buf = append(buf, sh[:]...)
	d1 := sha256.Sum256(buf)
	d2 := sha256.Sum256(d1[:])
	return d2
}

// SignTxInput signs input idx for prevScript with the 32-byte private scalar,
// returning the DER signature with the SIGHASH_ALL byte appended (the form that
// goes into a scriptSig). Mirrors C++ m_cp.sign + push_back(SIGHASH_ALL).
func SignTxInput(tx *Tx, idx int, prevScript, priv []byte) ([]byte, error) {
	if len(priv) != 32 {
		return nil, errors.New("coins: private key must be 32 bytes")
	}
	h := tx.HashForSigning(idx, prevScript)
	key, _ := btcec.PrivKeyFromBytes(priv)
	sig := ecdsa.Sign(key, h[:])
	der := sig.Serialize() // DER
	out := append(der, byte(SigHashAll))
	return out, nil
}

// VerifyTxInput checks a DER+SIGHASH signature (from SignTxInput) against the
// 33-byte compressed pubkey for input idx / prevScript.
func VerifyTxInput(tx *Tx, idx int, prevScript, pub, sigWithSighash []byte) (bool, error) {
	if len(sigWithSighash) < 1 {
		return false, errors.New("coins: empty signature")
	}
	h := tx.HashForSigning(idx, prevScript)
	der := sigWithSighash[:len(sigWithSighash)-1]
	sig, err := ecdsa.ParseSignature(der)
	if err != nil {
		return false, err
	}
	p, err := btcec.ParsePubKey(pub)
	if err != nil {
		return false, err
	}
	return sig.Verify(h[:], p), nil
}
