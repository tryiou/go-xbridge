// Package crypto provides XBridge packet signing/verification.
//
// The wire signature format matches Bitcoin Core's
// secp256k1_ecdsa_signature_serialize_compact: a 64-byte (r||s) compact ECDSA
// signature over SHA256(packet bytes with the signature region zeroed).
// See docs/protocol.md §Signing.
//
// Reference implementation (to be wired in with github.com/btcsuite/btcd/btcec/v2):
//
//	import (
//		"github.com/btcsuite/btcd/btcec/v2"
//		"github.com/btcsuite/btcd/btcec/v2/ecdsa"
//		"xbridge-go/proto"
//	)
//
//	func (s *btcSigner) Sign(p *proto.Packet, priv []byte) error {
//		key, _ := btcec.PrivKeyFromBytes(priv)
//		digest := p.Digest()
//		sig := ecdsa.Sign(key, digest[:])
//		compact := sig.SerializeCompact() // 64 bytes
//		copy(p.Signature[:], compact)
//		return nil
//	}
//
//	func (s *btcSigner) Verify(p *proto.Packet) (bool, error) {
//		pub, err := btcec.ParsePubKey(p.Pubkey[:])
//		if err != nil {
//			return false, err
//		}
//		sig, err := ecdsa.ParseSignature(p.Signature[:]) // 64-byte compact
//		if err != nil {
//			return false, err
//		}
//		return sig.Verify(p.Digest()[:], pub), nil
//	}
package crypto

import "xbridge-go/proto"

// Signer abstracts secp256k1 signing/verification for XBridge packets.
type Signer interface {
	// Sign signs p in place, setting p.Signature. priv is the 32-byte scalar.
	Sign(p *proto.Packet, priv []byte) error
	// Verify checks p's signature against p.Pubkey.
	Verify(p *proto.Packet) (bool, error)
}
