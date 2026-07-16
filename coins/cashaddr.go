package coins

import (
	"fmt"
	"strings"
)

// CashAddr encoding (Bitcoin Cash, BIP-CashAddr) is a bech32-family format with
// a different separator (':'), HRP expansion, checksum polynomial, and version
// byte. It lets xbridge-go decode/encode BCH addresses the way a C++ BCH
// connector expects. Implementation mirrors the CashAddr spec.

// cashaddrGenerator is the CashAddr checksum polynomial (distinct from bech32's).
var cashaddrGenerator = []uint64{
	0x98, 0x79, 0x05, 0x97, 0x2d, 0x25, 0x66, 0x0d, 0xed, 0x27, 0x31, 0x31,
	0x69, 0x22, 0x55, 0x19, 0x89, 0x8e, 0x80, 0x1e, 0x6b, 0xcb, 0x3d, 0x40,
	0x88, 0x2a, 0x2c, 0x0c, 0xb9, 0x48, 0xb4, 0xca, 0x4b, 0x4f, 0x6f, 0x3d,
	0xb2, 0xe8, 0x8a, 0x96,
}

// cashaddrPolymod computes the CashAddr checksum over the data (5-bit groups).
func cashaddrPolymod(values []int) uint64 {
	c := uint64(1)
	for _, d := range values {
		c0 := c >> 35
		c = ((c & 0x07ffffffff) << 5) ^ uint64(d)
		for i := 0; i < 5; i++ {
			if (c0>>uint(i))&1 == 1 {
				c ^= cashaddrGenerator[i]
			}
		}
	}
	return c
}

// cashaddrHRPExpand expands the HRP for checksumming: each char's low 5 bits,
// then a single zero separator. (Differs from bech32's two-part expansion.)
func cashaddrHRPExpand(hrp string) []int {
	out := make([]int, 0, len(hrp)+1)
	for i := 0; i < len(hrp); i++ {
		out = append(out, int(hrp[i]&0x1f))
	}
	out = append(out, 0)
	return out
}

// cashaddrCreateChecksum returns the 8 5-bit checksum symbols for prefix+payload.
// Mirrors bech32's construction: the checksum is the top 8 symbols of
// (polymod(prefix-exp + payload + 8 zero symbols) XOR 1), which guarantees
// cashaddrVerifyChecksum accepts the resulting address.
func cashaddrCreateChecksum(prefix string, payload []int) []int {
	data := append(cashaddrHRPExpand(prefix), payload...)
	data = append(data, 0, 0, 0, 0, 0, 0, 0, 0)
	poly := cashaddrPolymod(data) ^ 1
	cs := make([]int, 8)
	for i := 0; i < 8; i++ {
		cs[i] = int((poly >> uint(5*(7-i))) & 31)
	}
	return cs
}

// cashaddrVerifyChecksum reports whether prefix+full (incl. checksum) is valid.
func cashaddrVerifyChecksum(prefix string, full []int) bool {
	data := append(cashaddrHRPExpand(prefix), full...)
	return cashaddrPolymod(data) == 1
}

// cashaddrEncode encodes a 20-byte hash as a CashAddr string of the given prefix
// and type (hashType: 0=P2KH, 1=P2SH). The version byte packs the type into the
// top 3 bits and the hash length (in bytes >> 2) into the bottom 5 bits, matching
// the Bitcoin Cash CashAddr spec (so P2KH addresses begin with 'q' and P2SH with
// 'p' for the "bitcoincash" prefix).
func cashaddrEncode(prefix string, hashType int, hash []byte) (string, error) {
	if len(hash) != 20 {
		return "", fmt.Errorf("coins: cashaddr requires a 20-byte hash")
	}
	if hashType != 0 && hashType != 1 {
		return "", fmt.Errorf("coins: cashaddr invalid hash type %d", hashType)
	}
	version := (hashType << 3) | (len(hash) >> 2)
	payload, err := convertBits(append([]int{version}, bytesToInts(hash)...), 8, 5, true)
	if err != nil {
		return "", err
	}
	cs := cashaddrCreateChecksum(prefix, payload)
	full := append(payload, cs...)
	var sb strings.Builder
	sb.WriteString(prefix)
	sb.WriteByte(':')
	for _, v := range full {
		sb.WriteByte(bech32Charset[v])
	}
	return sb.String(), nil
}

// cashaddrDecode decodes a CashAddr string into its hash type (0=P2KH, 1=P2SH)
// and 20-byte hash, validating the prefix matches and the checksum is correct.
func cashaddrDecode(addr, wantPrefix string) (hashType int, hash []byte, err error) {
	if len(addr) < 8 || len(addr) > 110 {
		return 0, nil, fmt.Errorf("coins: invalid cashaddr length")
	}
	lower := strings.ToLower(addr)
	upper := strings.ToUpper(addr)
	if lower != addr && upper != addr {
		return 0, nil, fmt.Errorf("coins: cashaddr mixed case")
	}
	pos := strings.Index(lower, ":")
	if pos < 1 {
		return 0, nil, fmt.Errorf("coins: cashaddr missing separator")
	}
	prefix := lower[:pos]
	if wantPrefix != "" && prefix != wantPrefix {
		return 0, nil, fmt.Errorf("coins: cashaddr prefix %q does not match %q", prefix, wantPrefix)
	}
	data := make([]int, 0, len(lower)-pos-1)
	for i := pos + 1; i < len(lower); i++ {
		idx := strings.IndexByte(bech32Charset, lower[i])
		if idx < 0 {
			return 0, nil, fmt.Errorf("coins: cashaddr invalid symbol %q", lower[i])
		}
		data = append(data, idx)
	}
	if !cashaddrVerifyChecksum(prefix, data) {
		return 0, nil, fmt.Errorf("coins: cashaddr checksum mismatch")
	}
	data = data[:len(data)-8] // drop checksum
	decoded, err := convertBits(data, 5, 8, false)
	if err != nil {
		return 0, nil, err
	}
	if len(decoded) != 21 {
		return 0, nil, fmt.Errorf("coins: cashaddr unexpected payload length")
	}
	version := decoded[0]
	hashType = version >> 3
	hashLenBytes := (version & 0x07) << 2
	if hashType != 0 && hashType != 1 {
		return 0, nil, fmt.Errorf("coins: cashaddr invalid hash type %d", hashType)
	}
	if hashLenBytes != 20 {
		return 0, nil, fmt.Errorf("coins: cashaddr unsupported hash length %d bytes", hashLenBytes)
	}
	return hashType, intsToBytes(decoded[1:]), nil
}
