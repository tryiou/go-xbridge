package coins

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"

	btcec "github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	xlog "go-xbridge/log"
)

// SigHashAll is the SIGHASH_ALL base type (the only base type XBridge uses for
// deposit/refund/payment).
const SigHashAll = 0x01

// SigHashForkID is the SIGHASH_FORKID flag (0x40). Forkid coins (BCH/DEVAULT,
// BTG) commit (forkValue<<8)|SIGHASH_FORKID|SIGHASH_ALL in the BIP143 digest
// and append SIGHASH_FORKID|SIGHASH_ALL (0x41) to the DER signature — the fork
// value affects only the digest (bch.cpp:391-406, btg.cpp:262-269).
const SigHashForkID = 0x40

// maxTxIns / maxTxOuts cap the number of inputs/outputs a transaction may
// declare. Peer- or wallet-supplied counts are untrusted; these (plus the
// remaining-bytes check below) prevent a crafted count from allocating a huge
// slice. The varint allows values up to ~4e9, which would OOM; real
// transactions carry far fewer.
const (
	maxTxIns  = 1 << 20
	maxTxOuts = 1 << 20
	// minTxInBytes / minTxOutBytes are the minimum on-wire sizes of one input /
	// output, used to bound the count by the bytes actually remaining.
	minTxInBytes  = 41 // PrevOut(32)+Index(4)+scriptlen varint(>=1)+Sequence(4)
	minTxOutBytes = 9  // Value(8)+scriptPubKey len varint(>=1)
)

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
	Witness   [][]byte // segwit witness stack (nil for legacy/P2SH inputs)
	// Amount is the value (base units) of the output this input spends. It is
	// NOT part of the wire encoding; BIP143 requires it in the segwit sighash
	// commitment, so callers signing a segwit input must populate it.
	Amount uint64
}

// TxOut is a transaction output.
type TxOut struct {
	Value        uint64 // base units (satoshis etc.)
	ScriptPubKey []byte
}

// Tx is a Bitcoin-style transaction. SegWit selects the 0x0001 marker form on
// serialization; the deposit/refund layer here uses legacy (P2SH) scripts.
//
// WithTime / TxTime carry the XBridge per-coin "serializeWithTimeField" quirk
// (xbridge/xbitcointransaction.h:39-84). When a coin's connector sets the flag
// (read from <COIN>.TxWithTimeField in xbridge.conf, xbridgeapp.cpp:993), the
// CTransaction is serialized with an extra 4-byte nTime (unsigned int) written
// immediately after nVersion and before the segwit marker / vin. Both peers
// agree on the flag out-of-band from conf, so the decoder is *told* whether the
// field is present (DeserializeWithTime) — it cannot be detected on the wire.
type Tx struct {
	Version  int32
	LockTime uint32
	Inputs   []TxIn
	Outputs  []TxOut
	SegWit   bool
	// WithTime, when true, makes Serialize write the 4-byte TxTime field. Set by
	// the deposit/refund/claim builders from the coin's TxWithTimeField flag.
	WithTime bool
	// TxTime is the nTime value (unix seconds) written when WithTime is set. Zero
	// is a valid value but the C++ constructors default it to time(nullptr) when
	// the flag is on (xbitcointransaction.h:92), so builders populate it.
	TxTime uint32
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
// SegWit is set). The scriptSig of each input is serialized verbatim. When
// WithTime is set (per-coin serializeWithTimeField), a 4-byte nTime is written
// immediately after nVersion and before the segwit marker / vin, mirroring
// XBridge CTransaction::SerializationOp (xbitcointransaction.h:77-84).
func (t *Tx) Serialize() []byte {
	buf := make([]byte, 0, 256)
	var v [4]byte
	binary.LittleEndian.PutUint32(v[:], uint32(t.Version))
	buf = append(buf, v[:]...)
	if t.WithTime {
		var tm [4]byte
		binary.LittleEndian.PutUint32(tm[:], t.TxTime)
		buf = append(buf, tm[:]...)
	}
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
	if t.SegWit {
		for _, in := range t.Inputs {
			buf = append(buf, varInt(len(in.Witness))...)
			for _, w := range in.Witness {
				buf = append(buf, varInt(len(w))...)
				buf = append(buf, w...)
			}
		}
	}
	return buf
}

// Deserialize parses wire bytes (classic, or segwit-marker form) into a Tx,
// assuming NO nTime field is present. Use DeserializeWithTime for coins whose
// connector sets serializeWithTimeField. It mirrors Serialize: legacy inputs
// carry only ScriptSig; segwit inputs additionally carry their Witness stack.
func Deserialize(b []byte) (*Tx, error) {
	t, err := deserializeTx(b, false)
	if err != nil {
		xlog.Debug("coins: tx deserialize failed", "err", err, "len", len(b))
	}
	return t, err
}

// DeserializeWithTime parses wire bytes into a Tx. When hasTime is true it reads
// the 4-byte nTime field (unsigned int) immediately after nVersion and records
// it on the returned Tx (WithTime=true, TxTime=read value) so a later
// Serialize reproduces the same layout — matching C++ CTransaction::SerializationOp.
// The flag is supplied by the caller because the wire format is ambiguous: both
// peers agree on serializeWithTimeField out-of-band from xbridge.conf.
func DeserializeWithTime(b []byte, hasTime bool) (*Tx, error) {
	t, err := deserializeTx(b, hasTime)
	if err != nil {
		xlog.Debug("coins: tx deserialize failed", "err", err, "len", len(b), "hasTime", hasTime)
	}
	return t, err
}

func deserializeTx(b []byte, hasTime bool) (*Tx, error) {
	t := &Tx{}
	pos := 0
	need := func(n int) ([]byte, error) {
		if pos+n > len(b) {
			return nil, errors.New("coins: tx truncated")
		}
		s := b[pos : pos+n]
		pos += n
		return s, nil
	}
	le32 := func(x []byte) uint32 { return binary.LittleEndian.Uint32(x) }

	ver, err := need(4)
	if err != nil {
		return nil, err
	}
	t.Version = int32(le32(ver))

	if hasTime {
		tm, err := need(4)
		if err != nil {
			return nil, err
		}
		t.TxTime = le32(tm)
		t.WithTime = true
	}

	if len(b) >= pos+2 && b[pos] == 0x00 && b[pos+1] == 0x01 {
		t.SegWit = true
		pos += 2
	}

	nIn, err := readVarInt(b, &pos)
	if err != nil {
		return nil, err
	}
	rem := len(b) - pos
	if rem < 0 {
		rem = 0
	}
	if nIn > maxTxIns || (nIn > 0 && uint64(nIn)*minTxInBytes > uint64(rem)) {
		return nil, errors.New("coins: too many inputs")
	}
	t.Inputs = make([]TxIn, nIn)
	for i := range t.Inputs {
		h, err := need(32)
		if err != nil {
			return nil, err
		}
		copy(t.Inputs[i].PrevOut.Hash[:], h)
		ix, err := need(4)
		if err != nil {
			return nil, err
		}
		t.Inputs[i].PrevOut.Index = le32(ix)
		slen, err := readVarInt(b, &pos)
		if err != nil {
			return nil, err
		}
		ss, err := need(slen)
		if err != nil {
			return nil, err
		}
		t.Inputs[i].ScriptSig = append([]byte(nil), ss...)
		seq, err := need(4)
		if err != nil {
			return nil, err
		}
		t.Inputs[i].Sequence = le32(seq)
	}

	nOut, err := readVarInt(b, &pos)
	if err != nil {
		return nil, err
	}
	rem = len(b) - pos
	if rem < 0 {
		rem = 0
	}
	if nOut > maxTxOuts || (nOut > 0 && uint64(nOut)*minTxOutBytes > uint64(rem)) {
		return nil, errors.New("coins: too many outputs")
	}
	t.Outputs = make([]TxOut, nOut)
	for i := range t.Outputs {
		val, err := need(8)
		if err != nil {
			return nil, err
		}
		t.Outputs[i].Value = binary.LittleEndian.Uint64(val)
		slen, err := readVarInt(b, &pos)
		if err != nil {
			return nil, err
		}
		spk, err := need(slen)
		if err != nil {
			return nil, err
		}
		t.Outputs[i].ScriptPubKey = append([]byte(nil), spk...)
	}

	lt, err := need(4)
	if err != nil {
		return nil, err
	}
	t.LockTime = le32(lt)

	if t.SegWit {
		for i := range t.Inputs {
			wc, err := readVarInt(b, &pos)
			if err != nil {
				return nil, err
			}
			t.Inputs[i].Witness = make([][]byte, wc)
			for w := 0; w < wc; w++ {
				wl, err := readVarInt(b, &pos)
				if err != nil {
					return nil, err
				}
				ws, err := need(wl)
				if err != nil {
					return nil, err
				}
				t.Inputs[i].Witness[w] = append([]byte(nil), ws...)
			}
		}
	}
	if pos != len(b) {
		return nil, errors.New("coins: trailing bytes in tx")
	}
	return t, nil
}

// readVarInt reads a Bitcoin variable-length integer at *pos, advancing pos.
func readVarInt(b []byte, pos *int) (int, error) {
	if *pos >= len(b) {
		return 0, errors.New("coins: tx truncated")
	}
	first := b[*pos]
	*pos++
	switch {
	case first < 0xfd:
		return int(first), nil
	case first == 0xfd:
		if *pos+2 > len(b) {
			return 0, errors.New("coins: tx truncated")
		}
		v := binary.LittleEndian.Uint16(b[*pos:])
		*pos += 2
		return int(v), nil
	case first == 0xfe:
		if *pos+4 > len(b) {
			return 0, errors.New("coins: tx truncated")
		}
		v := binary.LittleEndian.Uint32(b[*pos:])
		*pos += 4
		return int(v), nil
	default:
		if *pos+8 > len(b) {
			return 0, errors.New("coins: tx truncated")
		}
		v := binary.LittleEndian.Uint64(b[*pos:])
		*pos += 8
		return int(v), nil
	}
}

// HashForSigning computes the legacy SIGHASH_ALL digest for input idx, with that
// input's scriptSig replaced by prevScript (the redeem/inner script) and all
// other inputs blanked. This matches C++ SignatureHash(inner, tx, idx,
// SIGHASH_ALL) for non-segwit transactions. When t.WithTime is set, the 4-byte
// nTime is written immediately after nVersion, mirroring
// CTransactionSignatureSerializer::Serialize (xbitcointransaction.h:265-269),
// which serializes nTime when serializeWithTimeField is set.
func (t *Tx) HashForSigning(idx int, prevScript []byte) [32]byte {
	buf := make([]byte, 0, 256)
	var v [4]byte
	binary.LittleEndian.PutUint32(v[:], uint32(t.Version))
	buf = append(buf, v[:]...)
	if t.WithTime {
		var tm [4]byte
		binary.LittleEndian.PutUint32(tm[:], t.TxTime)
		buf = append(buf, tm[:]...)
	}
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

// HashForSigningBIP143 computes the BIP143 digest for input idx, committing the
// given hashType (the SIGHASH type, including any SIGHASH_FORKID flag and fork
// value) as the trailing 4-byte field. scriptCode is the script being executed
// (for P2WPKH this is the implied P2PKH script; for a P2WSH / nested-witness or
// forkid HTLC input it is the witness/redeem script). amount is the value (base
// units) of the output being spent. Reference:
// https://github.com/bitcoin/bips/blob/master/bip-0143.mediawiki
//
//	hashPrevouts = dSHA256(all input outpoints)
//	hashSequence = dSHA256(all input sequences)
//	hashOutputs  = dSHA256(all outputs)
//	preimage = version ‖ hashPrevouts ‖ hashSequence ‖ outpoint ‖
//	           scriptCode ‖ amount ‖ nSequence ‖ hashOutputs ‖ locktime ‖ hashType
//
// The forkid variants (BCH/BTG) use the same preimage with hashType carrying the
// fork value; the XBridge serializeWithTimeField nTime is NOT committed here,
// matching the C++ forkid SignatureHash (bch.cpp:192-270, btg.cpp:118-207).
func (t *Tx) HashForSigningBIP143(idx int, scriptCode []byte, amount uint64, hashType uint32) ([32]byte, error) {
	if idx < 0 || idx >= len(t.Inputs) {
		return [32]byte{}, errors.New("coins: input index out of range")
	}
	dsha := func(b []byte) [32]byte {
		h1 := sha256.Sum256(b)
		return sha256.Sum256(h1[:])
	}

	var prevoutsBuf bytes.Buffer
	var sequenceBuf bytes.Buffer
	for _, in := range t.Inputs {
		prevoutsBuf.Write(in.PrevOut.Hash[:])
		var ix [4]byte
		binary.LittleEndian.PutUint32(ix[:], in.PrevOut.Index)
		prevoutsBuf.Write(ix[:])
		var seq [4]byte
		binary.LittleEndian.PutUint32(seq[:], in.Sequence)
		sequenceBuf.Write(seq[:])
	}
	hashPrevouts := dsha(prevoutsBuf.Bytes())
	hashSequence := dsha(sequenceBuf.Bytes())

	var outputsBuf bytes.Buffer
	for _, out := range t.Outputs {
		var val [8]byte
		binary.LittleEndian.PutUint64(val[:], out.Value)
		outputsBuf.Write(val[:])
		outputsBuf.Write(varInt(len(out.ScriptPubKey)))
		outputsBuf.Write(out.ScriptPubKey)
	}
	hashOutputs := dsha(outputsBuf.Bytes())

	var buf bytes.Buffer
	var v [4]byte
	binary.LittleEndian.PutUint32(v[:], uint32(t.Version))
	buf.Write(v[:])
	buf.Write(hashPrevouts[:])
	buf.Write(hashSequence[:])
	// outpoint of the input being signed
	buf.Write(t.Inputs[idx].PrevOut.Hash[:])
	var ix [4]byte
	binary.LittleEndian.PutUint32(ix[:], t.Inputs[idx].PrevOut.Index)
	buf.Write(ix[:])
	// scriptCode (length-prefixed)
	buf.Write(varInt(len(scriptCode)))
	buf.Write(scriptCode)
	// amount of the output being spent
	var amt [8]byte
	binary.LittleEndian.PutUint64(amt[:], amount)
	buf.Write(amt[:])
	// nSequence of the input being signed
	var seq [4]byte
	binary.LittleEndian.PutUint32(seq[:], t.Inputs[idx].Sequence)
	buf.Write(seq[:])
	buf.Write(hashOutputs[:])
	var lt [4]byte
	binary.LittleEndian.PutUint32(lt[:], t.LockTime)
	buf.Write(lt[:])
	var sh [4]byte
	binary.LittleEndian.PutUint32(sh[:], hashType)
	buf.Write(sh[:])

	return dsha(buf.Bytes()), nil
}

// HashForSigningForkID computes the BCH-style (BIP135) forkid digest for input
// idx: the BIP143 preimage with the sighash type set to
// (forkValue<<8)|SIGHASH_FORKID|SIGHASH_ALL. forkValue 0 yields hashType 0x41
// (DEVAULT); 79 yields 0x4F41 (BTG); live BCH mainnet uses 0xffdead, committing
// 0xffdead41 (see forkValueFromMethod). Matches the C++ forkid SignatureHash
// (bch.cpp:192-270, btg.cpp:118-207), which always takes the BIP143 branch when
// SIGHASH_FORKID is set.
func (t *Tx) HashForSigningForkID(idx int, scriptCode []byte, amount uint64, forkValue uint32) ([32]byte, error) {
	hashType := (forkValue << 8) | SigHashForkID | SigHashAll
	return t.HashForSigningBIP143(idx, scriptCode, amount, hashType)
}

// HashForSigningSegwit computes the BIP143 SIGHASH_ALL digest for input idx.
// scriptCode is the script being executed (for P2WPKH this is the implied
// P2PKH script `OP_DUP OP_HASH160 <20> OP_EQUALVERIFY OP_CHECKSIG`; for a
// P2WSH / nested-witness HTLC input it is the witness/redeem script). amount is
// the value (base units) of the output being spent. Reference:
// https://github.com/bitcoin/bips/blob/master/bip-0143.mediawiki
func (t *Tx) HashForSigningSegwit(idx int, scriptCode []byte, amount uint64) ([32]byte, error) {
	return t.HashForSigningBIP143(idx, scriptCode, amount, SigHashAll)
}

// P2WPKHScriptCode returns the BIP143 scriptCode for spending a P2WPKH output
// committing to keyHash (the 20-byte HASH160 of the compressed pubkey): the
// implied P2PKH script `OP_DUP OP_HASH160 <20> OP_EQUALVERIFY OP_CHECKSIG`.
func P2WPKHScriptCode(keyHash []byte) []byte {
	s := make([]byte, 0, 25)
	s = append(s, 0x76, 0xa9, 0x14) // OP_DUP OP_HASH160 push(20)
	s = append(s, keyHash...)
	s = append(s, 0x88, 0xac) // OP_EQUALVERIFY OP_CHECKSIG
	return s
}

// SignTxInput signs input idx for prevScript with the 32-byte private scalar,
// returning the DER signature with the SIGHASH_ALL byte appended (the form that
// goes into a scriptSig). Mirrors C++ m_cp.sign + push_back(SIGHASH_ALL).
func SignTxInput(tx *Tx, idx int, prevScript, priv []byte) ([]byte, error) {
	if len(priv) != 32 {
		return nil, errors.New("coins: private key must be 32 bytes")
	}
	h := tx.HashForSigning(idx, prevScript)
	return signDigest(h, priv)
}

// SignTxInputSegwit signs input idx as a BIP143 (segwit) input: scriptCode is
// the witness script (or P2WPKH implied P2PKH script via P2WPKHScriptCode) and
// amount is the spent output's value. Returns the DER signature with the
// SIGHASH_ALL byte appended (the form that goes into the witness stack).
func SignTxInputSegwit(tx *Tx, idx int, scriptCode []byte, amount uint64, priv []byte) ([]byte, error) {
	if len(priv) != 32 {
		return nil, errors.New("coins: private key must be 32 bytes")
	}
	h, err := tx.HashForSigningSegwit(idx, scriptCode, amount)
	if err != nil {
		return nil, err
	}
	return signDigest(h, priv)
}

// SignTxInputForkID signs input idx with the forkid (BCH-style BIP135) digest:
// BIP143 over scriptCode/amount with hashType (forkValue<<8)|SIGHASH_FORKID|SIGHASH_ALL.
// Returns the DER signature with the 0x41 byte appended — the byte all forkid
// connectors push (bch.cpp:404, btg.cpp:269); the fork value only affects the
// digest. Mirrors C++ m_cp.sign + push_back(SIGHASH_ALL|SIGHASH_FORKID).
func SignTxInputForkID(tx *Tx, idx int, scriptCode []byte, amount uint64, forkValue uint32, priv []byte) ([]byte, error) {
	if len(priv) != 32 {
		return nil, errors.New("coins: private key must be 32 bytes")
	}
	h, err := tx.HashForSigningForkID(idx, scriptCode, amount, forkValue)
	if err != nil {
		return nil, err
	}
	return signDigestWithSighash(h, priv, SigHashForkID|SigHashAll)
}

// SignTxInputForCoin signs input idx for a coin's local-signing algorithm,
// dispatching on the coin's SignatureKind: the legacy SIGHASH_ALL digest for
// SigLegacy coins, the forkid BIP143 digest (committing the coin's ForkValue)
// for SigForkID coins. script is the prevout script being executed (the inner
// redeem script for HTLC spends, the P2PKH scriptPubKey for deposit funding
// inputs); amount is the spent output's value (forkid path only).
func SignTxInputForCoin(tx *Tx, idx int, script []byte, amount uint64, priv []byte, c Coin) ([]byte, error) {
	if c.SignatureKind() == SigForkID {
		return SignTxInputForkID(tx, idx, script, amount, c.ForkValue(), priv)
	}
	return SignTxInput(tx, idx, script, priv)
}

// signDigest signs a 32-byte digest and appends the SIGHASH_ALL byte.
func signDigest(h [32]byte, priv []byte) ([]byte, error) {
	return signDigestWithSighash(h, priv, SigHashAll)
}

// signDigestWithSighash signs a 32-byte digest and appends the given sighash
// byte (SigHashAll for legacy/BIP143, SigHashForkID|SigHashAll for forkid).
func signDigestWithSighash(h [32]byte, priv []byte, sighash byte) ([]byte, error) {
	key, _ := btcec.PrivKeyFromBytes(priv)
	sig := ecdsa.Sign(key, h[:])
	der := sig.Serialize()
	return append(der, sighash), nil
}

// VerifyTxInput checks a DER+SIGHASH signature (from SignTxInput) against the
// 33-byte compressed pubkey for input idx / prevScript.
func VerifyTxInput(tx *Tx, idx int, prevScript, pub, sigWithSighash []byte) (bool, error) {
	if len(sigWithSighash) < 1 {
		return false, errors.New("coins: empty signature")
	}
	if sigWithSighash[len(sigWithSighash)-1] != SigHashAll {
		return false, errors.New("coins: unexpected sighash type")
	}
	h := tx.HashForSigning(idx, prevScript)
	return verifyDigest(h, pub, sigWithSighash)
}

// VerifyTxInputSegwit checks a DER+SIGHASH signature (from SignTxInputSegwit)
// against the 33-byte compressed pubkey for input idx, using the BIP143 digest
// over scriptCode and the spent amount.
func VerifyTxInputSegwit(tx *Tx, idx int, scriptCode, pub []byte, amount uint64, sigWithSighash []byte) (bool, error) {
	if len(sigWithSighash) < 1 {
		return false, errors.New("coins: empty signature")
	}
	if sigWithSighash[len(sigWithSighash)-1] != SigHashAll {
		return false, errors.New("coins: unexpected sighash type")
	}
	h, err := tx.HashForSigningSegwit(idx, scriptCode, amount)
	if err != nil {
		return false, err
	}
	return verifyDigest(h, pub, sigWithSighash)
}

// VerifyTxInputForkID checks a DER+SIGHASH signature (from SignTxInputForkID)
// against the 33-byte compressed pubkey for input idx, using the forkid BIP143
// digest over scriptCode/amount and the coin's fork value. The signature's
// trailing byte must be 0x41 (SIGHASH_ALL|SIGHASH_FORKID).
func VerifyTxInputForkID(tx *Tx, idx int, scriptCode, pub []byte, amount uint64, forkValue uint32, sigWithSighash []byte) (bool, error) {
	if len(sigWithSighash) < 1 {
		return false, errors.New("coins: empty signature")
	}
	if sigWithSighash[len(sigWithSighash)-1] != SigHashForkID|SigHashAll {
		return false, errors.New("coins: unexpected sighash type")
	}
	h, err := tx.HashForSigningForkID(idx, scriptCode, amount, forkValue)
	if err != nil {
		return false, err
	}
	return verifyDigest(h, pub, sigWithSighash)
}

// verifyDigest parses a DER+sighash signature and verifies it against pub.
func verifyDigest(h [32]byte, pub, sigWithSighash []byte) (bool, error) {
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
