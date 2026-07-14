package p2p

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
)

const cmdSize = 12

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
	if uint32(len(data)) < uint32(4+cmdSize+8)+m.Length {
		return nil, errors.New("p2p: payload shorter than declared length")
	}
	m.Payload = make([]byte, m.Length)
	copy(m.Payload, data[4+cmdSize+8:4+cmdSize+8+m.Length])
	if Checksum(m.Payload) != m.Checksum {
		return nil, errors.New("p2p: checksum mismatch")
	}
	return m, nil
}
