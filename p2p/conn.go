package p2p

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"time"

	"xbridge-go/proto"
)

// handshakeTimeout bounds the version/verack exchange so a misbehaving peer
// cannot hang Dial indefinitely.
const handshakeTimeout = 30 * time.Second

// Conn is a thin XBridge peer connection over the Bitcoin P2P transport.
// It performs the version/verack handshake and streams decoded XBridge packets.
type Conn struct {
	netConn net.Conn
	magic   [4]byte
	reader  *bufio.Reader
}

// Dial connects to a Blocknet peer and completes the handshake.
func Dial(addr string, magic [4]byte, timeout time.Duration) (*Conn, error) {
	nc, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return nil, err
	}
	c := &Conn{netConn: nc, magic: magic, reader: bufio.NewReader(nc)}
	if err := c.handshake(); err != nil {
		nc.Close()
		return nil, err
	}
	return c, nil
}

// handshake performs the Bitcoin P2P version/verack exchange:
//
//	us  -- version -->  peer
//	us  <-- version --  peer
//	us  -- verack  -->  peer   (sent once we see the peer's version)
//	us  <-- verack  --  peer
//
// Unrelated messages (ping, pong, addr, …) observed during the exchange are
// ignored. A deadline bounds the whole exchange (see handshakeTimeout).
func (c *Conn) handshake() error {
	_ = c.netConn.SetDeadline(time.Now().Add(handshakeTimeout))
	defer c.netConn.SetDeadline(time.Time{})

	if err := c.writeVersion(); err != nil {
		return err
	}
	seenVersion, seenVerack := false, false
	for !(seenVersion && seenVerack) {
		msg, err := c.readMessage()
		if err != nil {
			return err
		}
		switch msg.Command {
		case "version":
			seenVersion = true
			if err := c.writeVerack(); err != nil {
				return err
			}
		case "verack":
			seenVerack = true
		default:
			// Ignore unrelated messages during the handshake.
		}
	}
	return nil
}

func (c *Conn) writeVersion() error {
	payload := NewVersion(c.netConn.RemoteAddr()).Marshal()
	msg := &Message{
		Magic:    c.magic,
		Command:  "version",
		Payload:  payload,
		Checksum: Checksum(payload),
	}
	_, err := c.netConn.Write(msg.Marshal())
	return err
}

func (c *Conn) writeVerack() error {
	msg := &Message{
		Magic:    c.magic,
		Command:  "verack",
		Checksum: Checksum(nil),
	}
	_, err := c.netConn.Write(msg.Marshal())
	return err
}

// ReadPacket reads the next XBridge packet from the stream, skipping any
// non-XBridge P2P messages (ping/pong, addr, etc.).
func (c *Conn) ReadPacket() (*proto.Packet, error) {
	for {
		msg, err := c.readMessage()
		if err != nil {
			return nil, err
		}
		if msg.Command != XBridgeNetCommand {
			continue
		}
		return proto.Unmarshal(msg.Payload)
	}
}

// WritePacket sends an XBridge packet as a `xbridge` P2P message.
func (c *Conn) WritePacket(p *proto.Packet) error {
	payload := p.Marshal()
	msg := &Message{
		Magic:    c.magic,
		Command:  XBridgeNetCommand,
		Payload:  payload,
		Checksum: Checksum(payload),
	}
	_, err := c.netConn.Write(msg.Marshal())
	return err
}

func (c *Conn) readMessage() (*Message, error) {
	hdr := make([]byte, 4+cmdSize+8)
	if _, err := io.ReadFull(c.reader, hdr); err != nil {
		return nil, err
	}
	length := binary.LittleEndian.Uint32(hdr[4+cmdSize : 4+cmdSize+4])
	if length > 64*1024*1024 {
		return nil, errors.New("p2p: implausible message length")
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(c.reader, payload); err != nil {
		return nil, err
	}
	return UnmarshalMessage(append(hdr, payload...))
}

func (c *Conn) Close() error { return c.netConn.Close() }
