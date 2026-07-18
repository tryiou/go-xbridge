package coins

import (
	"fmt"
	"strings"
)

// CashAddr encoding (Bitcoin Cash, BIP-CashAddr) is a bech32-family format with
// a different separator (':'), HRP expansion, checksum polynomial, and version
// byte. It lets go-xbridge decode/encode BCH addresses the way a C++ BCH
// connector expects. Implementation mirrors the CashAddr spec.

// cashaddrGenerator holds the five 40-bit generator constants Bitcoin ABC's
// cashaddr PolyMod XORs in, one per set bit of c0 (cashaddr.cpp::PolyMod). The
// previous port truncated these to their low byte, so its checksum validated
// only against itself and rejected every genuine BCH address. These are the
// authoritative values from Bitcoin ABC.
var cashaddrGenerator = []uint64{
	0x98f2bc8e61, // c0 & 0x01
	0x79b76d99e2, // c0 & 0x02
	0xf33e5fb3c4, // c0 & 0x04
	0xae2eabe2a8, // c0 & 0x08
	0x1e4f43e470, // c0 & 0x10
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

// cashaddrHRPExpand expands the HRP for checksumming. It mirrors Bitcoin ABC's
// cashaddr ExpandPrefix exactly: each char's low 5 bits (prefix[i] & 0x1f),
// followed by a single zero separator. (Do NOT split into high+low 5-bit pairs
// as bech32 does — CashAddr only takes the low 5 bits per char.)
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

// cashaddrSizeIndex maps a hash length in bytes to the 2-bit size field that
// Bitcoin Cash's cashaddrenc.cpp::PackAddrData stores in the low bits of the
// version byte. Mirrors the C++ switch (20→0, 24→1, 28→2, 32→3).
func cashaddrSizeIndex(n int) (int, error) {
	switch n {
	case 20:
		return 0, nil
	case 24:
		return 1, nil
	case 28:
		return 2, nil
	case 32:
		return 3, nil
	default:
		return 0, fmt.Errorf("coins: cashaddr unsupported hash length %d bytes", n)
	}
}

// cashaddrLenFromIndex is the inverse of cashaddrSizeIndex: the 2-bit size field
// back to a hash length in bytes.
func cashaddrLenFromIndex(i int) (int, bool) {
	switch i {
	case 0:
		return 20, true
	case 1:
		return 24, true
	case 2:
		return 28, true
	case 3:
		return 32, true
	default:
		return 0, false
	}
}

// cashaddrEncode encodes a 20/24/28/32-byte hash as a CashAddr string of the
// given prefix and type (hashType: 0=P2KH, 1=P2SH). The version byte packs the
// type into the top 3 bits and the hash size index (0/1/2/3) into the bottom 2
// bits, matching Bitcoin Cash CashAddr (Bitcoin ABC cashaddrenc.cpp::PackAddrData)
// — so 20-byte P2KH addresses begin with 'q' and P2SH with 'p' for the
// "bitcoincash" prefix. The previous Go port packed (len>>2) into the low bits,
// which is byte-incompatible with real BCH addresses and cannot decode them.
func cashaddrEncode(prefix string, hashType int, hash []byte) (string, error) {
	if hashType != 0 && hashType != 1 {
		return "", fmt.Errorf("coins: cashaddr invalid hash type %d", hashType)
	}
	sizeIdx, err := cashaddrSizeIndex(len(hash))
	if err != nil {
		return "", err
	}
	version := (hashType << 3) | sizeIdx
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
	version := decoded[0]
	hashType = version >> 3
	sizeIdx := version & 0x07
	hashLenBytes, ok := cashaddrLenFromIndex(sizeIdx)
	if !ok {
		return 0, nil, fmt.Errorf("coins: cashaddr invalid hash size index %d", sizeIdx)
	}
	if hashType != 0 && hashType != 1 {
		return 0, nil, fmt.Errorf("coins: cashaddr invalid hash type %d", hashType)
	}
	if len(decoded) != 1+hashLenBytes {
		return 0, nil, fmt.Errorf("coins: cashaddr unexpected payload length %d", len(decoded))
	}
	return hashType, intsToBytes(decoded[1 : 1+hashLenBytes]), nil
}
