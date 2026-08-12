package coins

import "encoding/binary"

// Bitcoin script opcodes used by the P2PKH / P2SH / HTLC scripts XBridge builds.
// Only the subset the deposit/refund/payment layer needs is defined here.
const (
	Op0                   = 0x00 // OP_FALSE / empty push
	Op1                   = 0x51 // OP_TRUE
	OpIf                  = 0x63
	OpElse                = 0x67
	OpEndIf               = 0x68
	OpDrop                = 0x75
	OpDup                 = 0x76
	OpSize                = 0x82
	OpEqual               = 0x87
	OpEqualVerify         = 0x88
	OpHash160             = 0xa9
	OpCheckSig            = 0xac
	OpCheckSigVerify      = 0xad
	OpCheckLockTimeVerify = 0xb1
	OpReturn              = 0x6a
	OpPushData1           = 0x4c
	OpPushData2           = 0x4d
	OpPushData4           = 0x4e
)

// pushData appends Bitcoin's minimal push of b, mirroring CScript's
// operator<<(vector): length 0 → OP_0; 1..75 → single length byte; larger →
// OP_PUSHDATA1/2/4 with the length.
func pushData(b []byte) []byte {
	n := len(b)
	switch {
	case n == 0:
		return []byte{Op0}
	case n <= 75:
		out := make([]byte, 1+len(b))
		out[0] = byte(n)
		copy(out[1:], b)
		return out
	case n <= 0xff:
		return append([]byte{OpPushData1, byte(n)}, b...)
	case n <= 0xffff:
		l := make([]byte, 2)
		binary.LittleEndian.PutUint16(l, uint16(n))
		return append(append([]byte{OpPushData2}, l...), b...)
	default:
		l := make([]byte, 4)
		binary.LittleEndian.PutUint32(l, uint32(n))
		return append(append([]byte{OpPushData4}, l...), b...)
	}
}

// pushNum appends a minimal script-number encoding of n, mirroring CScript's
// operator<<(int64): -1 → OP_1NEGATE, 0 → OP_0, 1..16 → OP_1..OP_16, otherwise
// the minimal little-endian byte vector via pushData.
func pushNum(n int64) []byte {
	switch {
	case n == -1:
		return []byte{0x4f} // OP_1NEGATE
	case n == 0:
		return []byte{Op0}
	case n >= 1 && n <= 16:
		return []byte{byte(Op1 + (n - 1))}
	}
	// Minimal little-endian encoding with sign handling (BIP62/CScriptNum).
	neg := n < 0
	v := n
	if neg {
		v = -v
	}
	buf := []byte{}
	for v > 0 {
		buf = append(buf, byte(v&0xff))
		v >>= 8
	}
	// If the high bit of the last byte is set, append a 0x00 sign byte.
	if buf[len(buf)-1]&0x80 != 0 {
		if neg {
			buf = append(buf, 0x80)
		} else {
			buf = append(buf, 0x00)
		}
	} else if neg {
		buf[len(buf)-1] |= 0x80
	}
	return pushData(buf)
}

// BuildP2PKHScript returns OP_DUP OP_HASH160 <20-byte hash> OP_EQUALVERIFY OP_CHECKSIG.
func BuildP2PKHScript(pubKeyHash [20]byte) []byte {
	s := []byte{OpDup, OpHash160}
	s = append(s, pushData(pubKeyHash[:])...)
	s = append(s, OpEqualVerify, OpCheckSig)
	return s
}

// BuildP2SHScript returns OP_HASH160 <20-byte scriptHash> OP_EQUAL.
func BuildP2SHScript(scriptHash [20]byte) []byte {
	s := []byte{OpHash160}
	s = append(s, pushData(scriptHash[:])...)
	s = append(s, OpEqual)
	return s
}

// BuildOpReturnScript returns OP_RETURN <push(data)>, mirroring C++'s
// CScript() << OP_RETURN << ToByteVector(data) (bitcoinrpcconnector.cpp:202):
// the data is pushed with the minimal push opcode for its length. Used for the
// service-node fee tx's order-info data carrier.
func BuildOpReturnScript(data []byte) []byte {
	s := []byte{OpReturn}
	return append(s, pushData(data)...)
}
