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

// handshake performs the Bitcoin P2P version/verack exchange.
//
// TODO: implement the `version` message payload. Blocknet nodes expect a valid
// Bitcoin version message (version, services, timestamp, addr_recv, addr_from,
// nonce, user-agent, start_height, relay). See docs/protocol.md §Handshake and
// verify against a live node. Until then this sends a minimal placeholder and
// waits for one inbound message; treat as NOT yet functional for real peering.
func (c *Conn) handshake() error {
	if err := c.writeVersion(); err != nil {
		return err
	}
	if _, err := c.readMessage(); err != nil {
		return err
	}
	return nil
}

func (c *Conn) writeVersion() error {
	msg := &Message{Magic: c.magic, Command: "version", Payload: []byte{}}
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
