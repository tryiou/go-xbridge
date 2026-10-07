package p2p

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	xlog "go-xbridge/log"
	"go-xbridge/proto"
	"go-xbridge/version"
)

// The per-connection send queue is capped; overflow disconnects the peer rather
// than dropping frames — some outbound frames have no resend path.
var maxSendBufferSize = 1 << 20

var writeTimeout = 30 * time.Second

var dialDedup = xlog.NewDedupe(60*time.Second, func(addr string, total int, elapsed time.Duration) {
	xlog.Debug("dial failures suppressed", "addr", addr, "count", total, "over", elapsed.Round(time.Second).String())
})

// handshakeTimeout and idleReadTimeout are vars (not consts) so tests can shorten them in place.
var handshakeTimeout = 60 * time.Second

var idleReadTimeout = 20 * time.Minute

// Conn is a thin XBridge peer connection over the Bitcoin P2P transport that performs the version/verack handshake and streams decoded XBridge packets.
type Conn struct {
	netConn     net.Conn
	magic       [4]byte
	reader      *bufio.Reader
	peerVersion *VersionMessage
	// OnNonXBridge, if set, receives every non-XBridge P2P message before ReadPacket skips it; nil disables it.
	OnNonXBridge func(cmd string, payload []byte)

	// writeMu serializes handshake-phase writes; after the handshake, the queue's single writer goroutine provides the same ordering, so no socket-level lock is needed.
	writeMu sync.Mutex

	handshaken atomic.Bool

	sendMu     sync.Mutex
	sendQ      [][]byte
	sendBytes  int
	overLimit  atomic.Bool
	wake       chan struct{}
	writerOnce sync.Once
	writerDone chan struct{}
	writerUp   atomic.Bool
	done       chan struct{}
	closeOnce  sync.Once
	doneOnce   sync.Once

	lastErrMu sync.Mutex
	lastErr   error
}

// Dial connects to a Blocknet peer and completes the handshake.
func Dial(addr string, magic [4]byte, timeout time.Duration) (*Conn, error) {
	nc, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		if first := dialDedup.Event(addr); first {
			xlog.Debug("dial failed", "addr", addr, "err", err)
		}
		return nil, err
	}
	return NewConn(nc, magic)
}

// DialContext is like Dial, but cancelling ctx aborts both the connection attempt and the in-flight handshake.
func DialContext(ctx context.Context, addr string, magic [4]byte, timeout time.Duration) (*Conn, error) {
	d := net.Dialer{Timeout: timeout}
	nc, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		if first := dialDedup.Event(addr); first {
			xlog.Debug("dial failed", "addr", addr, "err", err)
		}
		return nil, err
	}
	return NewConnCtx(ctx, nc, magic)
}

// NewConn completes the version/verack handshake over an already-established transport and takes ownership of nc, closing it if the handshake fails.
func NewConn(nc net.Conn, magic [4]byte) (*Conn, error) {
	c := &Conn{
		netConn:    nc,
		magic:      magic,
		reader:     bufio.NewReader(nc),
		wake:       make(chan struct{}, 1),
		writerDone: make(chan struct{}),
		done:       make(chan struct{}),
	}
	if err := c.handshake(); err != nil {
		xlog.Debug("handshake failed", "addr", nc.RemoteAddr().String(), "err", err)
		_ = nc.Close()
		return nil, err
	}
	return c, nil
}

// NewConnCtx is like NewConn but aborts the handshake when ctx is cancelled, closing nc and returning ctx.Err().
func NewConnCtx(ctx context.Context, nc net.Conn, magic [4]byte) (*Conn, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-ctx.Done():
			_ = nc.Close()
		case <-stop:
		}
	}()
	c, err := NewConn(nc, magic)
	close(stop)
	<-done
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	if ctx.Err() != nil {
		_ = c.Close()
		return nil, ctx.Err()
	}
	return c, nil
}

func (c *Conn) handshake() error {
	_ = c.netConn.SetDeadline(time.Now().Add(handshakeTimeout))
	defer func() { _ = c.netConn.SetDeadline(time.Time{}) }()

	if err := c.writeVersion(); err != nil {
		return err
	}
	seenVersion, seenVerack := false, false
	for !seenVersion || !seenVerack {
		msg, err := c.readMessage()
		if err != nil {
			return err
		}
		switch msg.Command {
		case "version":
			if seenVersion {
				return errors.New("p2p: duplicate version message")
			}
			seenVersion = true
			v, err := UnmarshalVersion(msg.Payload)
			if err != nil {
				return err
			}
			if v.Version < version.MinPeerProtoVersion {
				return fmt.Errorf("p2p: peer version %d below minimum %d", v.Version, version.MinPeerProtoVersion)
			}
			c.peerVersion = v
			if err := c.writeVerack(); err != nil {
				return err
			}
		case "verack":
			seenVerack = true
		default:
		}
	}
	c.handshaken.Store(true)
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
	return c.write(msg.Marshal())
}

func (c *Conn) writeVerack() error {
	msg := &Message{
		Magic:    c.magic,
		Command:  "verack",
		Checksum: Checksum(nil),
	}
	return c.write(msg.Marshal())
}

func (c *Conn) write(buf []byte) error {
	if !c.handshaken.Load() {
		return c.writeSync(buf)
	}
	return c.enqueue(buf)
}

func (c *Conn) writeSync(buf []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err := c.netConn.Write(buf)
	return err
}

func (c *Conn) enqueue(buf []byte) error {
	if e := c.pendingErr(); e != nil {
		return e
	}
	select {
	case <-c.done:
		return errors.New("p2p: connection closed")
	default:
	}
	if c.overLimit.Load() {
		return errors.New("p2p: send buffer over limit")
	}
	c.sendMu.Lock()
	c.sendQ = append(c.sendQ, buf)
	c.sendBytes += len(buf)
	over := c.sendBytes > maxSendBufferSize
	if over {
		c.overLimit.Store(true)
	}
	c.sendMu.Unlock()
	c.writerOnce.Do(c.startWriter)
	select {
	case c.wake <- struct{}{}:
	default:
	}
	if over {
		_ = c.Close()
		return errors.New("p2p: send buffer over limit; disconnected peer")
	}
	return nil
}

func (c *Conn) startWriter() {
	c.writerUp.Store(true)
	go func() {
		defer close(c.writerDone)
		for {
			select {
			case <-c.done:
				return
			case <-c.wake:
			}
			for {
				c.sendMu.Lock()
				if len(c.sendQ) == 0 {
					c.sendMu.Unlock()
					break
				}
				buf := c.sendQ[0]
				c.sendQ = c.sendQ[1:]
				c.sendBytes -= len(buf)
				c.sendMu.Unlock()
				if err := c.writeWithDeadline(buf); err != nil {
					c.recordErr(err)
					// A send failure must tear down both directions: closing netConn unblocks the reader loop, so the
					// caller observes the break instead of a half-dead connection whose sends all fail.
					_ = c.teardown()
					return
				}
			}
		}
	}()
}

func (c *Conn) writeWithDeadline(buf []byte) error {
	if err := c.netConn.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
		return err
	}
	_, err := c.netConn.Write(buf)
	if serr := c.netConn.SetWriteDeadline(time.Time{}); serr != nil && err == nil {
		err = serr
	}
	return err
}

func (c *Conn) pendingErr() error {
	c.lastErrMu.Lock()
	defer c.lastErrMu.Unlock()
	return c.lastErr
}

func (c *Conn) recordErr(err error) {
	c.lastErrMu.Lock()
	c.lastErr = err
	c.lastErrMu.Unlock()
}

// ErrMalformedXBridge marks an xbridge transport envelope that failed decode (bad varint, length mismatch, or undersized); ReadPacket wraps it so callers can tell it from an ordinary I/O error.
var ErrMalformedXBridge = errors.New("p2p: malformed xbridge envelope")

// ReadPacket reads the next XBridge packet from the stream, skipping non-XBridge P2P messages, and returns it with the peer's TCP address.
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
			return nil, "", fmt.Errorf("%w: %v", ErrMalformedXBridge, err)
		}
		pkt, err := proto.Unmarshal(pktBytes)
		if err != nil {
			return nil, "", err
		}
		return pkt, c.netConn.RemoteAddr().String(), nil
	}
}

// WritePacket sends an XBridge packet as a `xbridge` P2P message wrapped in the transport envelope; a zero dest broadcasts, a non-zero dest addresses one node by keyId.
func (c *Conn) WritePacket(p *proto.Packet, dest [20]byte) error {
	payload := encodeXBridgePayload(p.Marshal(), dest)
	msg := &Message{
		Magic:    c.magic,
		Command:  XBridgeNetCommand,
		Payload:  payload,
		Checksum: Checksum(payload),
	}
	return c.write(msg.Marshal())
}

func (c *Conn) readMessage() (*Message, error) {
	for {
		// Post-handshake only: re-arming the idle deadline during the handshake would replace handshake()'s
		// 60 s exchange bound with idleReadTimeout, widening the window 20x.
		if c.handshaken.Load() {
			_ = c.netConn.SetReadDeadline(time.Now().Add(idleReadTimeout))
		}
		hdr := make([]byte, 4+cmdSize+8)
		if _, err := io.ReadFull(c.reader, hdr); err != nil {
			return nil, err
		}
		length := binary.LittleEndian.Uint32(hdr[4+cmdSize : 4+cmdSize+4])
		if length > MaxPayloadSize {
			return nil, errors.New("p2p: implausible message length")
		}
		payload := make([]byte, length)
		if _, err := io.ReadFull(c.reader, payload); err != nil {
			return nil, err
		}
		msg, err := UnmarshalMessage(append(hdr, payload...))
		if err != nil {
			if errors.Is(err, ErrChecksum) {
				xlog.Debug("p2p: dropping frame with bad checksum")
				continue
			}
			return nil, err
		}
		if msg.Magic != c.magic {
			return nil, errors.New("p2p: unexpected network magic")
		}
		// Clearing the deadline after a complete frame so no stale read deadline carries over between frames;
		// the handshaken guard keeps it from wiping the exchange deadline handshake() set.
		if c.handshaken.Load() {
			_ = c.netConn.SetReadDeadline(time.Time{})
		}
		return msg, nil
	}
}

// PeerVersion returns the peer's version message captured during the handshake, or nil if none was seen.
func (c *Conn) PeerVersion() *VersionMessage { return c.peerVersion }

// NetConn returns the underlying TCP connection; the p2p layer owns its deadlines, so deadlines set by the caller are overwritten or cleared.
func (c *Conn) NetConn() net.Conn { return c.netConn }

// ReadMessage reads the next raw P2P message of any command; a frame with a
// bad checksum is dropped here (the reader waits for the next frame).
func (c *Conn) ReadMessage() (*Message, error) { return c.readMessage() }

// teardown closes the connection and signals the writer without waiting for that goroutine, which
// may be the caller; doneOnce makes it safe to call concurrently with Close.
func (c *Conn) teardown() error {
	c.doneOnce.Do(func() { close(c.done) })
	return c.netConn.Close()
}

// Close tears the connection down and waits for the writer goroutine to exit; it is safe to call multiple times from any goroutine.
func (c *Conn) Close() error {
	var err error
	c.closeOnce.Do(func() {
		err = c.teardown()
		if c.writerUp.Load() {
			<-c.writerDone
		}
	})
	return err
}

// WriteMessage sends a raw, already-constructed P2P message of any command.
func (c *Conn) WriteMessage(m *Message) error {
	return c.write(m.Marshal())
}

// SendCommand sends a P2P message with the given command and payload; the caller must frame the payload correctly for that command.
func (c *Conn) SendCommand(cmd string, payload []byte) error {
	msg := &Message{
		Magic:    c.magic,
		Command:  cmd,
		Payload:  payload,
		Checksum: Checksum(payload),
	}
	return c.WriteMessage(msg)
}
