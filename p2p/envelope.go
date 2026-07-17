package p2p

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"time"

	xlog "xbridge-go/log"
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
// (varint length + 20-byte broadcast address + 8-byte timestamp) ready to be
// used as the payload of an "xbridge" P2P message.
func encodeXBridgePayload(packet []byte) []byte {
	env := make([]byte, 0, xbridgeEnvelopeSize+len(packet))
	env = append(env, make([]byte, xbridgeAddrSize)...) // broadcast (zero dest addr)
	var ts [xbridgeTimestampSize]byte
	binary.LittleEndian.PutUint64(ts[:], uint64(time.Now().UnixMilli()))
	env = append(env, ts[:]...)
	env = append(env, packet...)
	out := writeVarInt(len(env))
	xlog.Debug("encode xbridge payload", "packetLen", len(packet), "envLen", len(out))
	return append(out, env...)
}

// DecodeXBridgePayload reverses encodeXBridgePayload, returning the raw
// XBridgePacket bytes (to be handed to proto.Unmarshal). It validates the
// varint length and that the envelope is at least 28 bytes.
func DecodeXBridgePayload(payload []byte) ([]byte, error) {
	n, off, err := readVarInt(payload, 0)
	if err != nil {
		xlog.Debug("decode xbridge payload failed", "err", err, "len", len(payload))
		return nil, err
	}
	if off+n != len(payload) {
		return nil, errors.New("p2p: xbridge envelope length mismatch")
	}
	if n < xbridgeEnvelopeSize {
		return nil, errors.New("p2p: xbridge envelope too small")
	}
	packet := payload[off+xbridgeEnvelopeSize : off+n]
	xlog.Debug("decode xbridge payload", "len", len(payload), "packetLen", len(packet), "hex", hex.EncodeToString(packet))
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

// readVarInt reads a Bitcoin CompactSize (varint) at *pos, advancing pos past
// it and returning the decoded value.
func readVarInt(b []byte, pos int) (int, int, error) {
	if pos >= len(b) {
		return 0, pos, errors.New("p2p: varint truncated")
	}
	first := b[pos]
	pos++
	switch {
	case first < 0xfd:
		return int(first), pos, nil
	case first == 0xfd:
		if pos+2 > len(b) {
			return 0, pos, errors.New("p2p: varint truncated")
		}
		v := int(binary.LittleEndian.Uint16(b[pos:]))
		return v, pos + 2, nil
	case first == 0xfe:
		if pos+4 > len(b) {
			return 0, pos, errors.New("p2p: varint truncated")
		}
		v := int(binary.LittleEndian.Uint32(b[pos:]))
		return v, pos + 4, nil
	default:
		if pos+8 > len(b) {
			return 0, pos, errors.New("p2p: varint truncated")
		}
		v := int(binary.LittleEndian.Uint64(b[pos:]))
		return v, pos + 8, nil
	}
}
