package p2p

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"time"

	xlog "go-xbridge/log"
	"go-xbridge/proto"
)

// dialDedup collapses repeated dial failures for the same address so a swarm
// of unreachable peers does not bury the log. The first failure per address is
// logged normally; repeats are suppressed and flushed as a periodic summary so
// the underlying connectivity problem stays visible without the per-attempt spam.
var dialDedup = xlog.NewDedupe(60*time.Second, func(addr string, total int, elapsed time.Duration) {
	xlog.Debug("dial failures suppressed", "addr", addr, "count", total, "over", elapsed.Round(time.Second).String())
})

// handshakeTimeout bounds the version/verack exchange so a misbehaving peer
// cannot hang Dial indefinitely.
const handshakeTimeout = 30 * time.Second

// Conn is a thin XBridge peer connection over the Bitcoin P2P transport.
// It performs the version/verack handshake and streams decoded XBridge packets.
type Conn struct {
	netConn     net.Conn
	magic       [4]byte
	reader      *bufio.Reader
	peerVersion *VersionMessage
	// OnNonXBridge, if set, is invoked for every raw P2P message whose
	// command is not the XBridge envelope command ("xbridge"). It lets callers
	// observe raw servicenode messages (snr/snp/snlp) that would otherwise
	// be skipped by ReadPacket. Optional; nil means skip as before.
	OnNonXBridge func(cmd string, payload []byte)
}

// Dial connects to a Blocknet peer and completes the handshake.
func Dial(addr string, magic [4]byte, timeout time.Duration) (*Conn, error) {
	nc, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		// Dedupe.Event is internally synchronized and atomically reports the
		// first occurrence, so concurrent Dial failures for the same address
		// produce exactly one diagnostic log line.
		if first := dialDedup.Event(addr); first {
			xlog.Debug("dial failed", "addr", addr, "err", err)
		}
		return nil, err
	}
	return NewConn(nc, magic)
}

// NewConn completes the handshake over an already-established transport (e.g. a
// TCP connection, or a stream tunneled through an HTTP CONNECT proxy). The
// caller is responsible for dialing; NewConn only performs version/verack and
// takes ownership of nc (closing it on handshake failure).
func NewConn(nc net.Conn, magic [4]byte) (*Conn, error) {
	c := &Conn{netConn: nc, magic: magic, reader: bufio.NewReader(nc)}
	if err := c.handshake(); err != nil {
		xlog.Debug("handshake failed", "addr", nc.RemoteAddr().String(), "err", err)
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
			if v, err := UnmarshalVersion(msg.Payload); err == nil {
				c.peerVersion = v
			}
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
// non-XBridge P2P messages (ping/pong, addr, etc.). The `xbridge` payload is
// unwrapped from its transport envelope (varint length + 20-byte dest addr + 8-byte
// timestamp) before being parsed as a proto.Packet. The returned peer is the
// TCP remote address of this connection.
func (c *Conn) ReadPacket() (*proto.Packet, string, error) {
	for {
		msg, err := c.readMessage()
		if err != nil {
			return nil, "", err
		}
		if msg.Command != XBridgeNetCommand {
			if c.OnNonXBridge != nil {
				c.OnNonXBridge(msg.Command, msg.Payload)
			}
			continue
		}
		pktBytes, err := DecodeXBridgePayload(msg.Payload)
		if err != nil {
			return nil, "", err
		}
		pkt, err := proto.Unmarshal(pktBytes)
		if err != nil {
			return nil, "", err
		}
		return pkt, c.netConn.RemoteAddr().String(), nil
	}
}

// WritePacket sends an XBridge packet as a `xbridge` P2P message, wrapping it
// in the transport envelope (varint length + 20-byte destination addr + 8-byte
// timestamp) expected by service nodes. A zero dest broadcasts; a non-zero dest
// addresses the packet to a specific node's keyId (C++ App::Impl::onSend,
// xbridgeapp.cpp:595, and the onMessageReceived / onBroadcastReceived split in
// net_processing.cpp:2896-2899).
func (c *Conn) WritePacket(p *proto.Packet, dest [20]byte) error {
	payload := encodeXBridgePayload(p.Marshal(), dest)
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

// PeerVersion returns the peer's advertised version message, captured during
// the handshake, or nil if it was not seen/parsed.
func (c *Conn) PeerVersion() *VersionMessage { return c.peerVersion }

// NetConn returns the underlying TCP connection (e.g. to set a read deadline).
func (c *Conn) NetConn() net.Conn { return c.netConn }

// ReadMessage reads the next raw P2P message (any command), skipping nothing.
// Useful for diagnostics (e.g. discovering the XBridge command name) and for
// callers that need to see non-XBridge traffic.
func (c *Conn) ReadMessage() (*Message, error) { return c.readMessage() }

func (c *Conn) Close() error { return c.netConn.Close() }

// WriteMessage sends a raw, already-constructed P2P message (any command).
// Used by the discovery layer to exchange getaddr/addr/ping/pong directly.
func (c *Conn) WriteMessage(m *Message) error {
	_, err := c.netConn.Write(m.Marshal())
	return err
}

// SendCommand sends a P2P message with the given command name and payload.
// The caller is responsible for the payload being correctly framed for that
// command (e.g. via p2p.MarshalAddr for an "addr" message).
func (c *Conn) SendCommand(cmd string, payload []byte) error {
	msg := &Message{
		Magic:    c.magic,
		Command:  cmd,
		Payload:  payload,
		Checksum: Checksum(payload),
	}
	return c.WriteMessage(msg)
}
