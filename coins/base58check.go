package coins

import (
	"crypto/sha256"
	"errors"
)

// base58CheckEncode prepends a version prefix and appends a 4-byte
// double-SHA256 checksum, then base58-encodes the result.
func base58CheckEncode(prefix byte, payload []byte) string {
	body := make([]byte, 1+len(payload))
	body[0] = prefix
	copy(body[1:], payload)
	cs := checksum(body)
	out := make([]byte, len(body)+4)
	copy(out, body)
	copy(out[len(body):], cs[:])
	return base58Encode(out)
}

// base58CheckDecode reverses base58CheckEncode, returning the version prefix and
// payload after verifying the checksum.
func base58CheckDecode(s string) (prefix byte, payload []byte, err error) {
	raw, err := base58Decode(s)
	if err != nil {
		return 0, nil, err
	}
	if len(raw) < 5 {
		return 0, nil, errors.New("coins: base58check string too short")
	}
	prefix = raw[0]
	payload = raw[1 : len(raw)-4]
	want := checksum(raw[:len(raw)-4])
	got := raw[len(raw)-4:]
	if !equal4(want[:], got) {
		return 0, nil, errors.New("coins: base58check checksum mismatch")
	}
	return prefix, payload, nil
}

func checksum(b []byte) [4]byte {
	h1 := sha256.Sum256(b)
	h2 := sha256.Sum256(h1[:])
	var cs [4]byte
	copy(cs[:], h2[:4])
	return cs
}

func equal4(a, b []byte) bool {
	if len(a) != 4 || len(b) != 4 {
		return false
	}
	return a[0] == b[0] && a[1] == b[1] && a[2] == b[2] && a[3] == b[3]
}
