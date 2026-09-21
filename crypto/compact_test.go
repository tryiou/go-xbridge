package crypto

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	//nolint:staticcheck // RIPEMD-160 is HASH160: the on-chain/wire identifier hash Bitcoin uses; the test reproduces the servicenode digest path, not a new application.
	"golang.org/x/crypto/ripemd160"
)

// TestDoubleSHA256 pins DoubleSHA256 against stdlib sha256 applied twice
// (independent of the helper): empty input and a fixed message. Previously
// only exercised indirectly via servicenode tests.
func TestDoubleSHA256(t *testing.T) {
	for _, in := range [][]byte{nil, {}, []byte("xbridge sighash"), bytes.Repeat([]byte{0xab}, 100)} {
		h1 := sha256.Sum256(in)
		want := sha256.Sum256(h1[:])
		if got := DoubleSHA256(in); got != want {
			t.Errorf("DoubleSHA256(%x) = %x, want %x", in, got, want)
		}
	}
}

// TestSignCompactRecoverRoundTrip pins the compact service-node signature
// path directly: SignCompact over a HASH160-style digest, then RecoverCompact
// must return the signer's compressed pubkey. It also pins the failure
// contract: short keys, short hashes, and corrupt signatures error (never a
// silent wrong key). Previously 0% covered in-package.
func TestSignCompactRecoverRoundTrip(t *testing.T) {
	priv := make([]byte, 32)
	priv[31] = 7
	pub, err := CompressedPubKey(priv)
	if err != nil {
		t.Fatalf("CompressedPubKey: %v", err)
	}
	if !FullyValidPubKey(pub) {
		t.Fatal("generated pubkey fails FullyValidPubKey")
	}

	// HASH160-style digest, as servicenode sigHashes are consumed.
	h1 := sha256.Sum256(pub[:])
	r := ripemd160.New()
	r.Write(h1[:])
	var digest [32]byte
	copy(digest[:], r.Sum(nil))

	sig, err := SignCompact(priv, digest[:])
	if err != nil {
		t.Fatalf("SignCompact: %v", err)
	}
	if len(sig) != 65 {
		t.Fatalf("compact sig len = %d, want 65", len(sig))
	}
	got, err := RecoverCompact(sig, digest[:])
	if err != nil {
		t.Fatalf("RecoverCompact: %v", err)
	}
	if !bytes.Equal(got, pub[:]) {
		t.Fatalf("recovered = %x, want %x", got, pub)
	}

	// A wrong digest must NOT recover our key (no silent misattribution).
	var other [32]byte
	other[0] = 0xff
	if got, err := RecoverCompact(sig, other[:]); err == nil && bytes.Equal(got, pub[:]) {
		t.Fatal("RecoverCompact(wrong digest) returned our key")
	}
	// A corrupted signature must never silently attribute to our key: it must
	// either fail to recover or recover a different key (ECDSA malleability
	// means some corruptions stay valid signatures — but never ours).
	bad := bytes.Clone(sig)
	bad[10] ^= 0xff
	if got, err := RecoverCompact(bad, digest[:]); err == nil && bytes.Equal(got, pub[:]) {
		t.Fatal("RecoverCompact(corrupt sig) returned our key")
	}
	if _, err := RecoverCompact(sig[:10], digest[:]); err == nil {
		t.Error("RecoverCompact(short sig): want error, got nil")
	}
	if _, err := SignCompact(priv[:31], digest[:]); err == nil {
		t.Error("SignCompact(short priv): want error, got nil")
	}
	if _, err := CompressedPubKey(priv[:31]); err == nil {
		t.Error("CompressedPubKey(short priv): want error, got nil")
	}
}

// TestFullyValidPubKey pins the curve-point check: the generator point and a
// fresh key pass; wrong length, bad prefix, and off-curve points fail.
// Previously 0% covered in-package.
func TestFullyValidPubKey(t *testing.T) {
	var gen [33]byte
	copy(gen[:], mustHexBytes(t, "0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798"))
	if !FullyValidPubKey(gen) {
		t.Error("generator point rejected")
	}
	var badLen [33]byte
	badLen[0] = 0x04 // uncompressed prefix on a 33-byte buffer: invalid
	if FullyValidPubKey(badLen) {
		t.Error("0x04 prefix accepted as compressed key")
	}
	var offCurve [33]byte
	offCurve[0] = 0x02
	offCurve[1] = 0xff // proven off-curve by independent check: x^3+7 mod p is
	// a quadratic non-residue (Legendre symbol -1, verified with python
	// big-ints outside this repo), so no y exists on secp256k1 for this x.
	if FullyValidPubKey(offCurve) {
		t.Error("proven off-curve point 0x02ff… accepted; curve validation regressed")
	}
}

func mustHexBytes(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
