// Package crypto provides XBridge packet signing/verification.
//
// The wire signature format matches Bitcoin Core's
// secp256k1_ecdsa_signature_serialize_compact: a 64-byte (r||s) compact ECDSA
// signature over SHA256(packet bytes with the signature region zeroed).
// See docs/protocol.md §Signing.
package crypto

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"

	btcec "github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	secp256k1 "github.com/decred/dcrd/dcrec/secp256k1/v4"
	secp_ecdsa "github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
	xlog "go-xbridge/log"
	"go-xbridge/proto"
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
		xlog.Debug("crypto: verify failed parsing pubkey", "err", err)
		return false, err
	}
	sig, err := compactParse(p.Signature[:])
	if err != nil {
		xlog.Debug("crypto: verify failed parsing signature", "err", err)
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
		xlog.Debug("crypto: verify failed parsing pubkey", "pubkey", pubkeyHex, "err", err)
		return false, err
	}
	sig, err := compactParse(p.Signature[:])
	if err != nil {
		xlog.Debug("crypto: verify failed parsing signature", "pubkey", pubkeyHex, "err", err)
		return false, err
	}
	d := p.Digest()
	return sig.Verify(d[:], pub), nil
}

// NewPrivateKey returns a fresh 32-byte secp256k1 scalar. XBridge uses
// this for the per-order HTLC secret keypair (xPubKey/xPrivKey), generated
// at order-creation time (C++ xbridgeapp.cpp:2001). It uses the full 256-bit
// range with retry until the scalar is in [1, N-1] (N = curve group order),
// mirroring C++ m_cp.makeNewKey (CRYPTO-F82): the pre-fix code cleared the top
// bit, discarding one bit of entropy. Keys are local (no interop impact), but
// the generated space should match C++.
func NewPrivateKey() ([]byte, error) {
	order := secp256k1.Params().N
	for i := 0; i < 64; i++ {
		var b [32]byte
		if _, err := rand.Read(b[:]); err != nil {
			return nil, err
		}
		// N < 2^256, so only a scalar >= N or 0 needs a redraw (probability
		// ~2^-128 per draw; the loop effectively never iterates).
		if new(big.Int).SetBytes(b[:]).Cmp(order) >= 0 {
			continue
		}
		if b == [32]byte{} {
			continue
		}
		return b[:], nil
	}
	return nil, errors.New("crypto: failed to generate a valid private key")
}

// FullyValidPubKey reports whether pk is a 33-byte compressed secp256k1
// public key with a valid curve point, mirroring CPubKey::IsFullyValid
// (pubkey.cpp:157-196): length check plus point-on-curve. The trivial
// 02/03-prefix check is not enough — an invalid point must be rejected like
// C++ (servicenode.h:405,791).
func FullyValidPubKey(pk [33]byte) bool {
	_, err := secp256k1.ParsePubKey(pk[:])
	return err == nil
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

// DoubleSHA256 returns the Bitcoin double-SHA256 (SHA256d) of b. C++ service
// node sigHashes are built with CHashWriter (servicenode.h:748-752), which is
// SHA256d over the serialized fields.
func DoubleSHA256(b []byte) [32]byte {
	h := sha256.Sum256(b)
	return sha256.Sum256(h[:])
}

// SignCompact returns the 65-byte recoverable compact ECDSA signature over
// hash (header + r + s), matching C++ CKey::SignCompact. Service node pings
// are signed this way (ServiceNodePing::sign, servicenode.h:769-771).
func SignCompact(priv []byte, hash []byte) ([]byte, error) {
	if len(priv) != 32 {
		return nil, errors.New("crypto: private key must be 32 bytes")
	}
	key := secp256k1.PrivKeyFromBytes(priv)
	return secp_ecdsa.SignCompact(key, hash, true), nil
}

// RecoverCompact recovers the pubkey that signed hash from a 65-byte compact
// signature, serialized per the header's compression bit, matching C++
// CPubKey::RecoverCompact (pubkey.cpp:199-205). An uncompressed header yields
// a 65-byte key, which can never match the 33-byte compressed snodePubKey, so
// such pings are rejected by the caller (servicenode.h:814).
func RecoverCompact(sig, hash []byte) ([]byte, error) {
	pub, wasCompressed, err := secp_ecdsa.RecoverCompact(sig, hash)
	if err != nil {
		return nil, err
	}
	if !wasCompressed {
		return pub.SerializeUncompressed(), nil
	}
	return pub.SerializeCompressed(), nil
}
