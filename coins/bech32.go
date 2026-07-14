package coins

import (
	"errors"
	"strings"
)

// bech32 constants. The separator is '1'. bech32 (BIP173) uses the constant 1;
// bech32m (BIP350) uses 0x2bc. Segwit v0 uses bech32, v1+ uses bech32m.
const (
	bech32Const  = 1
	bech32mConst = 0x2bc
)

var bech32Charset = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"

var bech32CharsetIdx = func() [256]int8 {
	t := [256]int8{}
	for i := range t {
		t[i] = -1
	}
	for i := 0; i < len(bech32Charset); i++ {
		t[bech32Charset[i]] = int8(i)
	}
	return t
}()

// bech32Polymod computes the checksum over the data (in 5-bit groups).
func bech32Polymod(values []int) int {
	gen := []int{0x3b6a57b2, 0x26508e6d, 0x1ea119fa, 0x3d4233dd, 0x2a1462b3}
	chk := 1
	for _, v := range values {
		b := chk >> 25
		chk = (chk&0x1ffffff)<<5 ^ v
		for i := 0; i < 5; i++ {
			if (b>>uint(i))&1 == 1 {
				chk ^= gen[i]
			}
		}
	}
	return chk
}

// bech32HRPExpand expands the human-readable part for checksumming.
func bech32HRPExpand(hrp string) []int {
	out := []int{}
	for i := 0; i < len(hrp); i++ {
		out = append(out, int(hrp[i]>>5))
	}
	out = append(out, 0)
	for i := 0; i < len(hrp); i++ {
		out = append(out, int(hrp[i]&0x1f))
	}
	return out
}

// bech32CreateChecksum returns the 6 5-bit checksum symbols for the given
// constant (bech32 vs bech32m).
func bech32CreateChecksum(hrp string, data []int, c int) []int {
	values := append(bech32HRPExpand(hrp), data...)
	values = append(values, 0, 0, 0, 0, 0, 0)
	polymod := bech32Polymod(values) ^ c
	out := make([]int, 6)
	for i := 0; i < 6; i++ {
		out[i] = (polymod >> uint(5*(5-i))) & 31
	}
	return out
}

// bech32VerifyChecksum returns true if the data+checksum is valid for const c.
func bech32VerifyChecksum(hrp string, data []int, c int) bool {
	values := append(bech32HRPExpand(hrp), data...)
	return bech32Polymod(values) == c
}

// convertBits regroups bits between two widths, optionally padding. Used to
// convert between 5-bit bech32 symbols and 8-bit program bytes.
func convertBits(data []int, fromBits, toBits uint, pad bool) ([]int, error) {
	acc := 0
	bits := uint(0)
	var out []int
	maxv := (1 << toBits) - 1
	for _, v := range data {
		if v < 0 || (v>>fromBits) != 0 {
			return nil, errors.New("coins: invalid bech32 data range")
		}
		acc = (acc << fromBits) | v
		bits += fromBits
		for bits >= toBits {
			bits -= toBits
			out = append(out, (acc>>bits)&maxv)
		}
	}
	if pad {
		if bits > 0 {
			out = append(out, (acc<<(toBits-bits))&maxv)
		}
	} else if bits >= fromBits || ((acc<<(toBits-bits))&maxv) != 0 {
		return nil, errors.New("coins: invalid bech32 padding")
	}
	return out, nil
}

// bech32Encode builds a segwit address. witnessVersion is 0..16; program is the
// witness program bytes. bech32m is used for witnessVersion >= 1.
func bech32Encode(hrp string, witnessVersion int, program []byte) (string, error) {
	if witnessVersion < 0 || witnessVersion > 16 {
		return "", errors.New("coins: invalid witness version")
	}
	if len(program) < 2 || len(program) > 40 {
		return "", errors.New("coins: invalid witness program length")
	}
	wit := []int{witnessVersion}
	data := wit // witness version is already a single 5-bit group
	prog, err := convertBits(bytesToInts(program), 8, 5, true)
	if err != nil {
		return "", err
	}
	data = append(data, prog...)
	cc := bech32Const
	if witnessVersion >= 1 {
		cc = bech32mConst
	}
	cs := bech32CreateChecksum(hrp, data, cc)
	full := append(data, cs...)
	var sb strings.Builder
	sb.WriteString(hrp)
	sb.WriteByte('1')
	for _, v := range full {
		sb.WriteByte(bech32Charset[v])
	}
	return sb.String(), nil
}

// bech32Decode parses a segwit address, returning the HRP, witness version, and
// program bytes. It accepts both bech32 and bech32m (the constant is detected
// via checksum verification).
func bech32Decode(addr string) (hrp string, witnessVersion int, program []byte, err error) {
	if len(addr) < 8 || len(addr) > 90 {
		return "", 0, nil, errors.New("coins: invalid bech32 length")
	}
	lower := strings.ToLower(addr)
	upper := strings.ToUpper(addr)
	if lower != addr && upper != addr {
		return "", 0, nil, errors.New("coins: bech32 mixed case")
	}
	s := lower
	pos := strings.LastIndex(s, "1")
	if pos < 1 || pos+7 > len(s) {
		return "", 0, nil, errors.New("coins: missing bech32 separator")
	}
	hrp = s[:pos]
	data := make([]int, 0, len(s)-pos-1)
	for i := pos + 1; i < len(s); i++ {
		c := s[i]
		if bech32CharsetIdx[c] < 0 {
			return "", 0, nil, errors.New("coins: invalid bech32 symbol")
		}
		data = append(data, int(bech32CharsetIdx[c]))
	}
	if !bech32VerifyChecksum(hrp, data, bech32Const) && !bech32VerifyChecksum(hrp, data, bech32mConst) {
		return "", 0, nil, errors.New("coins: bech32 checksum mismatch")
	}
	// Drop the 6 checksum symbols.
	data = data[:len(data)-6]
	prog5, err := convertBits(data[1:], 5, 8, false)
	if err != nil {
		return "", 0, nil, err
	}
	witnessVersion = data[0]
	if witnessVersion < 0 || witnessVersion > 16 {
		return "", 0, nil, errors.New("coins: invalid witness version")
	}
	program = intsToBytes(prog5)
	return hrp, witnessVersion, program, nil
}

func bytesToInts(b []byte) []int {
	out := make([]int, len(b))
	for i, v := range b {
		out[i] = int(v)
	}
	return out
}

func intsToBytes(in []int) []byte {
	out := make([]byte, len(in))
	for i, v := range in {
		out[i] = byte(v)
	}
	return out
}
