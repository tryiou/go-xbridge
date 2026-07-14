package proto

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"time"
)

const (
	// ProtocolVersion is XBRIDGE_PROTOCOL_VERSION (src/xbridge/version.h).
	ProtocolVersion uint32 = 55

	// Header layout matches src/xbridge/xbridgepacket.h (class XBridgePacket).
	// 8 * uint32 header fields (little-endian) + 33-byte pubkey + 64-byte signature = 129 bytes.
	// NOTE: the C++ "crc" field (field32<5>, offset 20) overlaps the start of the
	// pubkey region and is unused (crc() always returns 0), so it is not written separately.
	HeaderSize   = 8*4 + PubkeySize + SigSize // 129
	PubkeySize   = 33
	SigSize      = 64
	PubkeyOffset = 20 // C++: pubkeyField() = &m_body[20]
	SigOffset    = 53 // C++: signatureField() = &m_body[53]
	BodyOffset   = HeaderSize

	// uint32 header field byte offsets (each field is 4 bytes, little-endian).
	offVersion   = 0
	offCommand   = 4
	offTimestamp = 8
	offOldSize   = 12
	offSize      = 16
	// offCrc = 20 overlaps PubkeyOffset and is unused.

	// headerDifference = headerSize - oldHeaderSize (8*4). Used by C++ to keep
	// the backward-compatible __oldSize field in sync with size.
	headerDifference = HeaderSize - 8*4
)

// Packet is one XBridge protocol message.
type Packet struct {
	Version   uint32
	Command   XBridgeCommand
	Timestamp uint32
	OldSize   uint32
	Size      uint32
	Pubkey    [PubkeySize]byte
	Signature [SigSize]byte
	Body      []byte
}

// NewPacket builds a packet for cmd with the given body. The packet timestamp
// is set to the current wall-clock time, matching C++ (time(0) in the ctor);
// the transport envelope adds its own 8-byte timestamp on send.
func NewPacket(cmd XBridgeCommand, body []byte) *Packet {
	return &Packet{
		Version:   ProtocolVersion,
		Command:   cmd,
		Timestamp: uint32(time.Now().Unix()),
		Size:      uint32(len(body)),
		OldSize:   uint32(len(body)) + headerDifference,
		Body:      body,
	}
}

// Marshal serializes the full packet (header + body) to wire bytes.
func (p *Packet) Marshal() []byte {
	buf := make([]byte, HeaderSize+len(p.Body))
	binary.LittleEndian.PutUint32(buf[offVersion:], p.Version)
	binary.LittleEndian.PutUint32(buf[offCommand:], uint32(p.Command))
	binary.LittleEndian.PutUint32(buf[offTimestamp:], p.Timestamp)
	binary.LittleEndian.PutUint32(buf[offOldSize:], p.OldSize)
	binary.LittleEndian.PutUint32(buf[offSize:], uint32(len(p.Body)))
	copy(buf[PubkeyOffset:PubkeyOffset+PubkeySize], p.Pubkey[:])
	copy(buf[SigOffset:SigOffset+SigSize], p.Signature[:])
	copy(buf[BodyOffset:], p.Body)
	return buf
}

// Digest returns SHA256 over the packet bytes with the signature region zeroed,
// matching C++ XBridgePacket::sign/verify (SHA256 of the entire m_body).
func (p *Packet) Digest() [32]byte {
	b := p.Marshal()
	zero := make([]byte, SigSize)
	copy(b[SigOffset:SigOffset+SigSize], zero)
	return sha256.Sum256(b)
}

// Unmarshal parses a full packet from wire bytes.
func Unmarshal(data []byte) (*Packet, error) {
	if len(data) < HeaderSize {
		return nil, errors.New("xbridge: data shorter than packet header")
	}
	p := &Packet{
		Version:   binary.LittleEndian.Uint32(data[offVersion:]),
		Command:   XBridgeCommand(binary.LittleEndian.Uint32(data[offCommand:])),
		Timestamp: binary.LittleEndian.Uint32(data[offTimestamp:]),
		OldSize:   binary.LittleEndian.Uint32(data[offOldSize:]),
		Size:      binary.LittleEndian.Uint32(data[offSize:]),
	}
	copy(p.Pubkey[:], data[PubkeyOffset:PubkeyOffset+PubkeySize])
	copy(p.Signature[:], data[SigOffset:SigOffset+SigSize])
	if uint32(len(data)) < HeaderSize+p.Size {
		return nil, errors.New("xbridge: declared body size exceeds data")
	}
	p.Body = make([]byte, p.Size)
	copy(p.Body, data[BodyOffset:BodyOffset+p.Size])
	return p, nil
}
