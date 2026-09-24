package p2p

import (
	"encoding/binary"
	"errors"
	"time"

	xlog "go-xbridge/log"
)

// XBridge transport envelope (src/xbridge/xbridgeapp.cpp App::Impl::onSend and
// src/net_processing.cpp XBRIDGE handling): the Bitcoin P2P "xbridge" message
// payload is NOT the serialized XBridgePacket directly. It is wrapped as:
//
//	varint(28 + packetLen)
//	[ 20 bytes ] destination address (uint160); 20 zero bytes == broadcast
//	[  8 bytes ] uint64 LE timestamp
//	[  packet  ] the XBridgePacket (129-byte header + body)
//
// The outer Bitcoin P2P frame (magic|command|length|checksum) already wraps
// this whole payload; see message.go.
const (
	xbridgeAddrSize      = 20
	xbridgeTimestampSize = 8
	xbridgeEnvelopeSize  = xbridgeAddrSize + xbridgeTimestampSize // 28
)

// encodeXBridgePayload wraps raw XBridgePacket bytes in the transport envelope
// (varint length + 20-byte destination address + 8-byte timestamp) ready to be
// used as the payload of an "xbridge" P2P message. A zero dest address is the
// broadcast form; a non-zero dest addresses the packet to a specific node's
// keyId (C++ App::Impl::onSend, xbridgeapp.cpp:595-616).
func encodeXBridgePayload(packet []byte, dest [20]byte) []byte {
	env := make([]byte, 0, xbridgeEnvelopeSize+len(packet))
	env = append(env, dest[:]...)
	var ts [xbridgeTimestampSize]byte
	// C++ stamps this 8-byte field in MICROSECONDS (timeToInt =
	// total_microseconds(), xutil.cpp:280) and includes it in the SHA256-signed
	// body (Hash(msg.begin(), msg.end()), xbridgeapp.cpp:576). A millisecond
	// value here is a 1000x wire divergence that fails every counterparty
	// signature check — must stay UnixMicro().
	binary.LittleEndian.PutUint64(ts[:], uint64(time.Now().UnixMicro()))
	env = append(env, ts[:]...)
	env = append(env, packet...)
	out := writeVarInt(len(env))
	xlog.With("p2p", true).Debug("encode xbridge payload", "packetLen", len(packet), "envLen", len(out))
	return append(out, env...)
}

// DecodeXBridgePayload reverses encodeXBridgePayload, returning the raw
// XBridgePacket bytes (to be handed to proto.Unmarshal). It validates the
// varint length and that the envelope is at least 28 bytes.
func DecodeXBridgePayload(payload []byte) ([]byte, error) {
	n, off, err := readVarInt(payload, 0)
	if err != nil {
		return nil, err
	}
	if off+n != len(payload) {
		return nil, errors.New("p2p: xbridge envelope length mismatch")
	}
	if n < xbridgeEnvelopeSize {
		return nil, errors.New("p2p: xbridge envelope too small")
	}
	packet := payload[off+xbridgeEnvelopeSize : off+n]
	return packet, nil
}

// writeVarInt encodes a Bitcoin CompactSize (varint) integer.
func writeVarInt(n int) []byte {
	switch {
	case n < 0xfd:
		return []byte{byte(n)}
	case n <= 0xffff:
		b := make([]byte, 3)
		b[0] = 0xfd
		binary.LittleEndian.PutUint16(b[1:], uint16(n))
		return b
	case n <= 0xffffffff:
		b := make([]byte, 5)
		b[0] = 0xfe
		binary.LittleEndian.PutUint32(b[1:], uint32(n))
		return b
	default:
		b := make([]byte, 9)
		b[0] = 0xff
		binary.LittleEndian.PutUint64(b[1:], uint64(n))
		return b
	}
}

// ReadVarInt is the exported entry point for parsing a Bitcoin CompactSize
// (varint) integer. It is used by sibling packages (e.g. servicenode) that
// parse raw P2P payloads sharing Bitcoin's varint encoding.
func ReadVarInt(b []byte, pos int) (int, int, error) { return readVarInt(b, pos) }

// maxCompactSize mirrors C++ MAX_SIZE (src/serialize.h:27): ReadCompactSize
// throws on any decoded value above it (0x02000000 = 32 MiB).
const maxCompactSize = 0x02000000

// readVarInt reads a Bitcoin CompactSize (varint) at *pos, advancing pos past
// it and returning the decoded value. It rejects non-canonical encodings
// (an extended form used where the single-byte/16-bit form would do) and
// values above MAX_SIZE, matching C++ ReadCompactSize (serialize.h:289-305).
func readVarInt(b []byte, pos int) (int, int, error) {
	if pos >= len(b) {
		return 0, pos, errors.New("p2p: varint truncated")
	}
	first := b[pos]
	pos++
	var v uint64
	switch {
	case first < 0xfd:
		v = uint64(first)
	case first == 0xfd:
		if pos+2 > len(b) {
			return 0, pos, errors.New("p2p: varint truncated")
		}
		v = uint64(binary.LittleEndian.Uint16(b[pos:]))
		pos += 2
		if v < 0xfd {
			return 0, pos, errors.New("p2p: non-canonical varint")
		}
	case first == 0xfe:
		if pos+4 > len(b) {
			return 0, pos, errors.New("p2p: varint truncated")
		}
		v = uint64(binary.LittleEndian.Uint32(b[pos:]))
		pos += 4
		if v < 0x10000 {
			return 0, pos, errors.New("p2p: non-canonical varint")
		}
	default:
		if pos+8 > len(b) {
			return 0, pos, errors.New("p2p: varint truncated")
		}
		v = binary.LittleEndian.Uint64(b[pos:])
		pos += 8
		if v < 0x100000000 {
			return 0, pos, errors.New("p2p: non-canonical varint")
		}
	}
	if v > maxCompactSize {
		return 0, pos, errors.New("p2p: varint too large")
	}
	return int(v), pos, nil
}
