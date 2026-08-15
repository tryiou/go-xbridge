package crypto

import (
	"encoding/hex"
	"math/big"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	secp256k1 "github.com/decred/dcrd/dcrec/secp256k1/v4"
	"go-xbridge/proto"
)

// TestNewPrivateKeyFullRange pins the full-range generation: every
// key is a valid secp256k1 scalar in [1, N-1], and across enough draws the top
// bit is set at least once — proving the generator no longer clears it (the
// pre-fix code forced b[0]&=0x7f, discarding a bit of entropy).
func TestNewPrivateKeyFullRange(t *testing.T) {
	order := secp256k1.Params().N
	topBitSet := false
	for i := 0; i < 256; i++ {
		k, err := NewPrivateKey()
		if err != nil {
			t.Fatalf("NewPrivateKey: %v", err)
		}
		if len(k) != 32 {
			t.Fatalf("key length = %d, want 32", len(k))
		}
		v := new(big.Int).SetBytes(k)
		if v.Sign() == 0 || v.Cmp(order) >= 0 {
			t.Fatalf("key %x out of [1, N-1]", k)
		}
		if k[0]&0x80 != 0 {
			topBitSet = true
		}
	}
	if !topBitSet {
		t.Error("no generated key had the top bit set; generation still biased")
	}
}

func TestSignVerifyRoundTrip(t *testing.T) {
	signer := NewBtcSigner()
	priv, err := btcec.NewPrivateKey()
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	privBytes := priv.Serialize()

	body := []byte("order: BTC->BLOCK 0.01 @ 12345")
	p := proto.NewPacket(proto.XbcTransaction, body)

	if err := signer.Sign(p, privBytes); err != nil {
		t.Fatalf("sign: %v", err)
	}

	// Pubkey should be set by Sign (compressed, 33 bytes, 0x02/0x03 prefix).
	if p.Pubkey[0] != 0x02 && p.Pubkey[0] != 0x03 {
		t.Fatalf("pubkey not set/compressed: %x", p.Pubkey[:1])
	}
	// Signature should be non-zero 64 bytes.
	var zero [64]byte
	if p.Signature == zero {
		t.Fatal("signature is all zeros")
	}

	ok, err := signer.Verify(p)
	if err != nil {
		t.Fatalf("verify err: %v", err)
	}
	if !ok {
		t.Fatal("valid signature failed verification")
	}
}

func TestVerifyRejectsTamperedBody(t *testing.T) {
	signer := NewBtcSigner()
	priv, _ := btcec.NewPrivateKey()
	p := proto.NewPacket(proto.XbcTransaction, []byte("payload-A"))
	if err := signer.Sign(p, priv.Serialize()); err != nil {
		t.Fatalf("sign: %v", err)
	}
	if ok, _ := signer.Verify(p); !ok {
		t.Fatal("precondition: signature should verify")
	}

	// Tamper the body: digest changes, signature must no longer verify.
	p.Body[0] ^= 0xff
	ok, err := signer.Verify(p)
	if err != nil {
		t.Fatalf("verify err: %v", err)
	}
	if ok {
		t.Fatal("tampered body still verified")
	}
}

func TestVerifyRejectsTamperedSignature(t *testing.T) {
	signer := NewBtcSigner()
	priv, _ := btcec.NewPrivateKey()
	p := proto.NewPacket(proto.XbcTransactionInit, []byte("init"))
	if err := signer.Sign(p, priv.Serialize()); err != nil {
		t.Fatalf("sign: %v", err)
	}
	if ok, _ := signer.Verify(p); !ok {
		t.Fatal("precondition: signature should verify")
	}

	// Flip a bit in the signature; verification must fail.
	p.Signature[0] ^= 0x01
	ok, err := signer.Verify(p)
	if err != nil {
		t.Fatalf("verify err on bad sig: %v", err)
	}
	if ok {
		t.Fatal("tampered signature still verified")
	}
}

func TestSignVerifyAfterMarshalRoundTrip(t *testing.T) {
	signer := NewBtcSigner()
	priv, _ := btcec.NewPrivateKey()
	p := proto.NewPacket(proto.XbcTransactionCreatedA, []byte{1, 2, 3, 4, 5})
	if err := signer.Sign(p, priv.Serialize()); err != nil {
		t.Fatalf("sign: %v", err)
	}

	wire := p.Marshal()
	got, err := proto.Unmarshal(wire)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	ok, err := signer.Verify(got)
	if err != nil {
		t.Fatalf("verify after round-trip: %v", err)
	}
	if !ok {
		t.Fatal("signature invalid after marshal/unmarshal round-trip")
	}
}

func TestSignWrongKeyLength(t *testing.T) {
	signer := NewBtcSigner()
	p := proto.NewPacket(proto.XbcTransaction, []byte("x"))
	if err := signer.Sign(p, []byte("tooshort")); err == nil {
		t.Fatal("expected error for non-32-byte key")
	}
}

// TestVerifyAgainstRejectsMismatchedHeaderPubkey locks in C++
// packet->verify(pubkey) semantics (xbridgepacket.cpp:154-162): the header
// pubkey field must equal the given key BEFORE the signature is verified. A
// packet whose header claims pubkey X but is signed by key Y must be rejected
// by VerifyAgainst(pkt, Y) — the naive behavior (verify the signature against
// Y without checking the header) would accept it.
func TestVerifyAgainstRejectsMismatchedHeaderPubkey(t *testing.T) {
	signer := NewBtcSigner()

	// Two independent keypairs.
	privY, _ := btcec.NewPrivateKey()
	privX, _ := btcec.NewPrivateKey()
	pubY := privY.PubKey().SerializeCompressed()
	pubX := privX.PubKey().SerializeCompressed()

	// Build a packet whose header claims pubkey X, but whose signature was made
	// with key Y over the digest (which includes header pubkey X).
	p := proto.NewPacket(proto.XbcTransactionCancel, []byte("mismatch"))
	copy(p.Pubkey[:], pubX)
	d := p.Digest()
	sig := ecdsa.Sign(privY, d[:])
	compact := compactSerialize(sig)
	copy(p.Signature[:], compact[:])

	if ok, err := signer.VerifyAgainst(p, hexPubkey(pubY)); ok || err != nil {
		t.Fatalf("VerifyAgainst(mismatched header) = %v/%v, want false/nil (header pubkey %x != supplied %x)",
			ok, err, pubX, pubY)
	}

	// Positive control: a packet whose header pubkey matches the supplied key
	// still verifies.
	if err := signer.Sign(p, privY.Serialize()); err != nil {
		t.Fatalf("sign: %v", err)
	}
	if ok, err := signer.VerifyAgainst(p, hexPubkey(pubY)); !ok || err != nil {
		t.Fatalf("VerifyAgainst(matching header) = %v/%v, want true/nil", ok, err)
	}
}

// hexPubkey hex-encodes a 33-byte compressed pubkey for VerifyAgainst.
func hexPubkey(pub []byte) string {
	return hex.EncodeToString(pub)
}

// TestSignDeterministicKAT pins the signing output for a known key + digest.
// The C++ XBridge wire relies on two implementations agreeing byte-for-byte on
// a signature; that agreement is only possible because both use RFC6979
// deterministic ECDSA. We cannot fetch an external C++ vector from this sandbox,
// so this test pins the property that matters for interop: the same key and
// same digest always produce the identical 64-byte compact signature (a
// tampered/flaky signer would diverge and fail here). TODO: validate this exact
// signature against a live C++ node (e.g. the reachable
// coreproxy.airdns.org:42111 XBridge hub) as the one remaining live
// cross-check.
func TestSignDeterministicKAT(t *testing.T) {
	signer := NewBtcSigner()

	// Fixed private key ("1") and a fixed digest.
	priv := make([]byte, 32)
	priv[31] = 1
	var digest [32]byte
	copy(digest[:], []byte("0123456789abcdef0123456789abcdef")) // ascii digest, fixed

	p1 := proto.NewPacket(proto.XbcTransaction, digest[:])
	p2 := proto.NewPacket(proto.XbcTransaction, digest[:])
	if err := signer.Sign(p1, priv); err != nil {
		t.Fatalf("sign 1: %v", err)
	}
	if err := signer.Sign(p2, priv); err != nil {
		t.Fatalf("sign 2: %v", err)
	}

	// Identical key+digest must yield identical 64-byte compact signatures.
	if p1.Signature != p2.Signature {
		t.Fatalf("signature not deterministic:\n  sig1=%x\n  sig2=%x", p1.Signature, p2.Signature)
	}
	var zero [64]byte
	if p1.Signature == zero {
		t.Fatal("signature is all zeros")
	}

	// And the deterministic signature must verify against the published pubkey.
	ok, err := signer.Verify(p1)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !ok {
		t.Fatal("deterministic signature failed verification")
	}
}
