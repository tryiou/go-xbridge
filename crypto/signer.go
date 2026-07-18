// Package crypto provides XBridge packet signing/verification.
//
// The wire signature format matches Bitcoin Core's
// secp256k1_ecdsa_signature_serialize_compact: a 64-byte (r||s) compact ECDSA
// signature over SHA256(packet bytes with the signature region zeroed).
// See docs/protocol.md §Signing.
package crypto

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"

	btcec "github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	secp256k1 "github.com/decred/dcrd/dcrec/secp256k1/v4"
	secp_ecdsa "github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
	"xbridge-go/proto"
)

// Signer abstracts secp256k1 signing/verification for XBridge packets.
type Signer interface {
	// Sign signs p in place, setting p.Signature, which the wire verifier
	// checks against p.Pubkey. priv is the 32-byte scalar. Sign also derives
	// and sets p.Pubkey from priv, mirroring the C++ packet signer.
	Sign(p *proto.Packet, priv []byte) error
	// Verify checks p's signature against p.Pubkey.
	Verify(p *proto.Packet) (bool, error)
	// VerifyAgainst checks p's signature against an explicit 33-byte hex
	// pubkey (C++ packet->verify(pubkey) with a non-header key, e.g. the
	// order's mPubKey/oPubKey/sPubKey rather than pkt.Pubkey).
	VerifyAgainst(p *proto.Packet, pubkeyHex string) (bool, error)
}

// BtcSigner signs XBridge packets with a secp256k1 ECDSA key using
// btcd/btcec/v2, matching the C++ wire format.
type BtcSigner struct{}

// NewBtcSigner returns a Signer backed by btcec/v2.
func NewBtcSigner() *BtcSigner { return &BtcSigner{} }

// compactSerialize returns the 64-byte (r||s) compact encoding that the XBridge
// wire uses (C++ secp256k1_ecdsa_signature_serialize_compact). btcec/v2's
// Signature.Serialize() returns DER, so we marshal the scalar components
// ourselves: 32-byte big-endian R followed by 32-byte big-endian S.
func compactSerialize(sig *secp_ecdsa.Signature) [64]byte {
	var rBuf, sBuf [32]byte
	r := sig.R()
	s := sig.S()
	r.PutBytesUnchecked(rBuf[:])
	s.PutBytesUnchecked(sBuf[:])
	var out [64]byte
	copy(out[:32], rBuf[:])
	copy(out[32:], sBuf[:])
	return out
}

// compactParse rebuilds a Signature from the 64-byte (r||s) compact encoding.
func compactParse(b []byte) (*secp_ecdsa.Signature, error) {
	if len(b) != 64 {
		return nil, errors.New("crypto: compact signature must be 64 bytes")
	}
	var rMod, sMod secp256k1.ModNScalar
	if rMod.SetByteSlice(b[:32]) {
		return nil, errors.New("crypto: invalid R scalar")
	}
	if sMod.SetByteSlice(b[32:]) {
		return nil, errors.New("crypto: invalid S scalar")
	}
	return secp_ecdsa.NewSignature(&rMod, &sMod), nil
}

// Sign signs p in place. priv must be a 32-byte secp256k1 scalar. The packet's
// Pubkey is set to the compressed public key derived from priv.
func (BtcSigner) Sign(p *proto.Packet, priv []byte) error {
	if len(priv) != 32 {
		return errors.New("crypto: private key must be 32 bytes")
	}
	// btcec/v2 PrivKeyFromBytes returns (*PrivateKey, *PublicKey) with no error.
	key, _ := btcec.PrivKeyFromBytes(priv)
	copy(p.Pubkey[:], key.PubKey().SerializeCompressed())
	d := p.Digest()
	sig := ecdsa.Sign(key, d[:])
	compact := compactSerialize(sig)
	copy(p.Signature[:], compact[:])
	return nil
}

// Verify returns true iff p.Signature is a valid compact ECDSA signature over
// the packet digest, produced by the holder of p.Pubkey. (Verification accepts
// either low-S or high-S forms, matching the C++ secp256k1_ecdsa_verify check.)
func (BtcSigner) Verify(p *proto.Packet) (bool, error) {
	pub, err := btcec.ParsePubKey(p.Pubkey[:])
	if err != nil {
		return false, err
	}
	sig, err := compactParse(p.Signature[:])
	if err != nil {
		return false, err
	}
	d := p.Digest()
	return sig.Verify(d[:], pub), nil
}

// VerifyAgainst returns true iff p.Signature is a valid compact ECDSA signature
// over the packet digest, produced by the holder of the given 33-byte hex
// compressed pubkey. It mirrors C++ packet->verify(pubkey), which checks
// against an arbitrary key rather than the packet header's pubkey. A malformed
// or wrong-length hex yields (false, err); callers that wish to treat an
// absent key as "not verified" should ignore the error.
func (BtcSigner) VerifyAgainst(p *proto.Packet, pubkeyHex string) (bool, error) {
	raw, err := hex.DecodeString(pubkeyHex)
	if err != nil || len(raw) != 33 {
		return false, fmt.Errorf("crypto: bad pubkey hex %q", pubkeyHex)
	}
	pub, err := btcec.ParsePubKey(raw)
	if err != nil {
		return false, err
	}
	sig, err := compactParse(p.Signature[:])
	if err != nil {
		return false, err
	}
	d := p.Digest()
	return sig.Verify(d[:], pub), nil
}

// NewPrivateKey returns a fresh 32-byte secp256k1 scalar. XBridge uses
// this for the per-order HTLC secret keypair (xPubKey/xPrivKey), generated
// at order-creation time (C++ xbridgeapp.cpp:2001).
func NewPrivateKey() ([]byte, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, err
	}
	// Clear the high bits so the scalar is a valid secp256k1 private key
	// (< curve order); btcec rejects out-of-range scalars.
	b[0] &= 0x7f
	return b[:], nil
}

// CompressedPubKey derives the 33-byte compressed secp256k1 public key for a
// 32-byte private scalar, without signing anything.
func CompressedPubKey(priv []byte) ([33]byte, error) {
	if len(priv) != 32 {
		return [33]byte{}, errors.New("crypto: private key must be 32 bytes")
	}
	key, _ := btcec.PrivKeyFromBytes(priv)
	var p [33]byte
	copy(p[:], key.PubKey().SerializeCompressed())
	return p, nil
}
