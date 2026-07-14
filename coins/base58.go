package coins

import (
	"errors"
	"math/big"
)

// b58Alphabet is the Bitcoin base58 alphabet (no 0/O/I/l).
const b58Alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

var b58Idx [256]int8

func init() {
	for i := range b58Idx {
		b58Idx[i] = -1
	}
	for i := 0; i < len(b58Alphabet); i++ {
		b58Idx[b58Alphabet[i]] = int8(i)
	}
}

// base58Decode decodes a base58 string to bytes (the big-integer method).
func base58Decode(s string) ([]byte, error) {
	base := big.NewInt(58)
	x := big.NewInt(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if b58Idx[c] < 0 {
			return nil, errors.New("coins: invalid base58 character")
		}
		x.Mul(x, base)
		x.Add(x, big.NewInt(int64(b58Idx[c])))
	}
	// Count leading '1's as zero bytes.
	var leading int
	for leading = 0; leading < len(s) && s[leading] == '1'; leading++ {
	}
	buf := x.Bytes()
	out := make([]byte, leading+len(buf))
	copy(out[leading:], buf)
	return out, nil
}

// base58Encode encodes bytes to a base58 string.
func base58Encode(b []byte) string {
	x := new(big.Int).SetBytes(b)
	base := big.NewInt(58)
	zero := big.NewInt(0)
	mod := new(big.Int)
	var out []byte
	for x.Cmp(zero) > 0 {
		x.DivMod(x, base, mod)
		out = append(out, b58Alphabet[mod.Int64()])
	}
	// Preserve leading zero bytes as '1'.
	for _, c := range b {
		if c != 0 {
			break
		}
		out = append(out, '1')
	}
	// Reverse.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return string(out)
}
