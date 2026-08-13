package p2p

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
)

const cmdSize = 12

// MaxPayloadSize bounds the declared payload length of a P2P message. Peer-
// supplied lengths are untrusted; this cap prevents a huge m.Length from
// allocating an enormous Payload (and avoids uint32 overflow in the length
// check). It matches C++ MAX_PROTOCOL_MESSAGE_LENGTH = 4,000,000 (src/net.h:55),
// which disconnects any peer declaring more (src/net.cpp:583-585).
const MaxPayloadSize = 4_000_000

// ErrChecksum is returned by UnmarshalMessage when a frame's checksum does not
// match its payload. It is non-fatal by design: C++ logs a bad-checksum frame
// and drops it without disconnecting (net_processing.cpp:3138-3145), so
// conn.readMessage continues to the next frame on this sentinel.
var ErrChecksum = errors.New("p2p: checksum mismatch")

// Message is a Bitcoin-style P2P message:
//
//	magic(4) || command(12, null-padded) || length(4, LE) || checksum(4) || payload
type Message struct {
	Magic    [4]byte
	Command  string
	Length   uint32
	Checksum [4]byte
	Payload  []byte
}

// Checksum returns the first 4 bytes of double-SHA256 of payload,
// matching Bitcoin's message checksum convention.
func Checksum(payload []byte) [4]byte {
	d1 := sha256.Sum256(payload)
	d2 := sha256.Sum256(d1[:])
	var c [4]byte
	copy(c[:], d2[:4])
	return c
}

func (m *Message) Marshal() []byte {
	buf := make([]byte, 0, 4+cmdSize+4+4+len(m.Payload))
	buf = append(buf, m.Magic[:]...)
	cmd := make([]byte, cmdSize)
	copy(cmd, []byte(m.Command))
	buf = append(buf, cmd...)
	var l [4]byte
	binary.LittleEndian.PutUint32(l[:], uint32(len(m.Payload)))
	buf = append(buf, l[:]...)
	buf = append(buf, m.Checksum[:]...)
	buf = append(buf, m.Payload...)
	return buf
}

// UnmarshalMessage parses a Bitcoin P2P message from the wire.
func UnmarshalMessage(data []byte) (*Message, error) {
	if len(data) < 4+cmdSize+4+4 {
		return nil, errors.New("p2p: message too short")
	}
	m := &Message{}
	copy(m.Magic[:], data[0:4])
	end := 4 + cmdSize
	i := 4
	for i < end && data[i] != 0 {
		i++
	}
	m.Command = string(data[4:i])
	m.Length = binary.LittleEndian.Uint32(data[4+cmdSize : 4+cmdSize+4])
	copy(m.Checksum[:], data[4+cmdSize+4:4+cmdSize+8])
	// m.Length is untrusted. Reject absurd sizes up front (cheap), then verify
	// it fits the buffer using uint64 math so a near-max uint32 cannot wrap the
	// comparison and slip a truncated/oversized payload through.
	if m.Length > MaxPayloadSize {
		return nil, errors.New("p2p: declared payload too large")
	}
	if uint64(len(data)) < uint64(4+cmdSize+8)+uint64(m.Length) {
		return nil, errors.New("p2p: payload shorter than declared length")
	}
	m.Payload = make([]byte, m.Length)
	copy(m.Payload, data[4+cmdSize+8:4+cmdSize+8+m.Length])
	if Checksum(m.Payload) != m.Checksum {
		return nil, ErrChecksum
	}
	return m, nil
}
