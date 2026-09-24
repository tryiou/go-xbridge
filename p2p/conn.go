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

// maxSendBufferSize caps the per-connection outbound queue. C++ PushMessage
// grows vSendMsg without bound and pauses the peer once nSendSize exceeds
// nSendBufferMaxSize (net.cpp:2721-2722), eventually disconnecting a peer that
// cannot drain. A thin client has no socket-handler timeout machinery, so the
// cap disconnect is immediate. Frames are never dropped while the writer is
// alive: XBridge handshake frames are not safely retransmittable by the caller,
// so dropping a CreatedA response would make the hub retransmit CreateA and
// double-broadcast a deposit. (Frames queued after a write error are discarded
// with the teardown, exactly as C++ discards vSendMsg on disconnect.)
var maxSendBufferSize = 1 << 20

// writeTimeout bounds a single net.Conn.Write on the writer goroutine, so a
// peer that stops reading cannot wedge the writer forever. On expiry the write
// fails and the connection is torn down (the reader loop observes the close).
var writeTimeout = 30 * time.Second

// dialDedup collapses repeated dial failures for the same address so a swarm
// of unreachable peers does not bury the log. The first failure per address is
// logged normally; repeats are suppressed and flushed as a periodic summary so
// the underlying connectivity problem stays visible without the per-attempt spam.
var dialDedup = xlog.NewDedupe(60*time.Second, func(addr string, total int, elapsed time.Duration) {
	xlog.Debug("dial failures suppressed", "addr", addr, "count", total, "over", elapsed.Round(time.Second).String())
})

// handshakeTimeout bounds the version/verack exchange so a misbehaving peer
// cannot hang Dial indefinitely. It mirrors C++ DEFAULT_PEER_CONNECT_TIMEOUT =
// 60 s (net.h:83), the deadline C++ gives a peer to send its first message.
var handshakeTimeout = 60 * time.Second

// idleReadTimeout bounds a single frame read AFTER the handshake: a peer that
// completes the version/verack exchange and then stops sending would otherwise
// pin the reader goroutine forever (slowloris). C++ disconnects a peer that
// goes silent for TIMEOUT_INTERVAL = 20 min (net.h:45, the nLastRecv check at
// net.cpp:1068); the deadline here is re-armed per frame, so it is an IDLE
// timeout (20 min since the last complete frame), not a total-connection bound.
// A var (not a const) so tests can shorten it.
var idleReadTimeout = 20 * time.Minute

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

	// writeMu serializes handshake-phase writes. Post-handshake frames go
	// through the outbound queue, whose mutex provides the same serialization
	// (the queue is drained by a single writer goroutine), so concurrent
	// producers (the api engine, discovery read-loops) keep deterministic order
	// without a socket-level lock.
	writeMu sync.Mutex

	// handshaken gates the buffered write path: the version/verack exchange
	// writes synchronously (the conn is not yet shared); every later frame is
	// enqueued for the writer goroutine so a slow peer can never block the
	// caller (C++ PushMessage never blocks, net.cpp:2705-2730).
	handshaken atomic.Bool

	// sendMu guards the outbound queue. The queue is a growing slice (C++
	// vSendMsg) capped by maxSendBufferSize; overflow disconnects the peer
	// instead of dropping a frame. wake signals the writer; done is closed by
	// Close to stop it.
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

	// lastErrMu guards lastErr, the most recent writer-side socket error so a
	// later send observes a broken peer (C++ surfaces send failures on the
	// socket-handler thread, never on PushMessage).
	lastErrMu sync.Mutex
	lastErr   error
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

// DialContext is the context-aware dial used by the discovery PeerManager: the
// dial aborts the moment ctx is cancelled (Close), so an in-flight connect is
// interrupted immediately instead of running out its timeout. The version
// handshake is bounded by handshakeTimeout inside NewConn and aborts on ctx
// cancellation via NewConnCtx, so a peer that stalls the handshake cannot hold
// a manager Close past the cancellation.
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

// NewConn completes the handshake over an already-established transport (e.g. a
// TCP connection, or a stream tunneled through an HTTP CONNECT proxy). The
// caller is responsible for dialing; NewConn only performs version/verack and
// takes ownership of nc (closing it on handshake failure).
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

// NewConnCtx is like NewConn, but aborts the version/verack handshake the
// moment ctx is cancelled: it closes nc, unblocking the handshake's blocking
// read/write on the underlying net.Conn, and returns ctx.Err(). DialContext
// uses it so a discovery PeerManager shutdown interrupts an in-flight handshake
// instead of waiting out handshakeTimeout; C++ joins its connect threads on
// shutdown, so a peer that accepts TCP but never speaks cannot wedge the join.
// The canceller goroutine is joined before returning, so nothing leaks, and a
// handshake that completes concurrently with a cancel is torn down rather than
// handed to a caller that is closing the peer.
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
		// The close that unblocked the handshake usually surfaces as a raw net
		// error; when the context is the cause, report the cancellation the
		// caller can act on (the manager is closing the peer either way).
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
				// C++ disconnects on a duplicate version message
				// (net_processing.cpp:1574-1582).
				return errors.New("p2p: duplicate version message")
			}
			seenVersion = true
			v, err := UnmarshalVersion(msg.Payload)
			if err != nil {
				return err
			}
			if v.Version < version.MinPeerProtoVersion {
				// C++ disconnects peers below MIN_PEER_PROTO_VERSION
				// (net_processing.cpp:1617-1626).
				return fmt.Errorf("p2p: peer version %d below minimum %d", v.Version, version.MinPeerProtoVersion)
			}
			c.peerVersion = v
			if err := c.writeVerack(); err != nil {
				return err
			}
		case "verack":
			seenVerack = true
		default:
			// Ignore unrelated messages during the handshake.
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

// write serializes buf to the wire. During the handshake it writes
// synchronously (the conn is not yet shared); afterwards it appends to the
// outbound queue and returns immediately — the writer goroutine performs the
// actual socket write, so a slow peer can never stall the caller (C++
// PushMessage never blocks, net.cpp:2705-2730).
func (c *Conn) write(buf []byte) error {
	if !c.handshaken.Load() {
		return c.writeSync(buf)
	}
	return c.enqueue(buf)
}

// writeSync performs a direct, blocking net.Conn.Write under writeMu. Used only
// for the handshake phase, before the buffered writer exists and the conn is
// shared.
func (c *Conn) writeSync(buf []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err := c.netConn.Write(buf)
	return err
}

// enqueue appends buf to the outbound queue and wakes the writer. It never
// blocks on the socket and never drops a frame. When the queue exceeds the
// maxSendBufferSize cap the peer is disconnected (C++ pauses the peer at the
// cap, net.cpp:2721-2722, and disconnects a peer that cannot drain). If the
// connection is already dead, the last recorded write error is returned so the
// caller observes the break on the next send.
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

// startWriter launches the outbound writer goroutine: it drains the queue and
// performs the socket writes, keeping callers of write() off the wire. It
// exits when Close closes done (which also unblocks a pending write via the
// underlying net.Conn close) or when a socket write fails, so a conn that is
// closed — by the peer or by us — never leaks the goroutine.
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
					// Tear the connection down in BOTH directions (C++ sets
					// fDisconnect on a send failure): closing netConn unblocks
					// the reader loop, so the caller observes the break and can
					// reconnect/ban instead of lingering on a half-dead conn
					// whose every send fails forever.
					_ = c.teardown()
					return
				}
			}
		}
	}()
}

// writeWithDeadline writes one frame, bounding the socket write so a peer that
// stops reading cannot wedge the writer forever.
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

// pendingErr returns the last writer-side socket error, if any.
func (c *Conn) pendingErr() error {
	c.lastErrMu.Lock()
	defer c.lastErrMu.Unlock()
	return c.lastErr
}

// recordErr stores the writer-side socket error so later sends observe it.
func (c *Conn) recordErr(err error) {
	c.lastErrMu.Lock()
	c.lastErr = err
	c.lastErrMu.Unlock()
}

// ErrMalformedXBridge marks an xbridge transport envelope that failed decode
// (undersized / length mismatch) — the case C++ answers with Misbehaving +10
// (net_processing.cpp:2874-2878, raw.size() < 28). ReadPacket returns it so the
// caller can distinguish a malformed envelope from an ordinary I/O error.
var ErrMalformedXBridge = errors.New("p2p: malformed xbridge envelope")

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
			return nil, "", fmt.Errorf("%w: %v", ErrMalformedXBridge, err)
		}
		pkt, err := proto.Unmarshal(pktBytes)
		if err != nil {
			// Sized-but-undecodable body: C++ drops it with DoS 0
			// (xbridgesession.cpp:312) — not a misbehaviour offense.
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
	return c.write(msg.Marshal())
}

func (c *Conn) readMessage() (*Message, error) {
	for {
		// Post-handshake only: re-arm the idle read deadline per frame, so a
		// peer that stops sending times out after idleReadTimeout (C++
		// TIMEOUT_INTERVAL, net.h:45 / net.cpp:1068). During the handshake this
		// must NOT touch the deadline — handshake() sets its own 60 s bound
		// (handshakeTimeout), and overriding it here would widen the handshake
		// slowloris window 20x. handshaken is set at the end of handshake();
		// reads are single-goroutine, so the load is race-free.
		if c.handshaken.Load() {
			_ = c.netConn.SetReadDeadline(time.Now().Add(idleReadTimeout))
		}
		hdr := make([]byte, 4+cmdSize+8)
		if _, err := io.ReadFull(c.reader, hdr); err != nil {
			return nil, err
		}
		length := binary.LittleEndian.Uint32(hdr[4+cmdSize : 4+cmdSize+4])
		if length > MaxPayloadSize {
			// C++ disconnects a peer declaring more than
			// MAX_PROTOCOL_MESSAGE_LENGTH (net.cpp:583-585).
			return nil, errors.New("p2p: implausible message length")
		}
		payload := make([]byte, length)
		if _, err := io.ReadFull(c.reader, payload); err != nil {
			return nil, err
		}
		msg, err := UnmarshalMessage(append(hdr, payload...))
		if err != nil {
			if errors.Is(err, ErrChecksum) {
				// C++ logs and drops a bad-checksum frame without disconnecting
				// (net_processing.cpp:3138-3145); keep the connection and read
				// the next frame.
				xlog.Debug("p2p: dropping frame with bad checksum")
				continue
			}
			return nil, err
		}
		if msg.Magic != c.magic {
			// C++ disconnects on an invalid message start
			// (net_processing.cpp:3117-3121).
			return nil, errors.New("p2p: unexpected network magic")
		}
		// Clear the frame deadline once a complete message arrived, so a
		// subsequent frame re-arms it fresh. Only meaningful post-handshake;
		// during the handshake the caller owns the deadline.
		if c.handshaken.Load() {
			_ = c.netConn.SetReadDeadline(time.Time{})
		}
		return msg, nil
	}
}

// PeerVersion returns the peer's advertised version message, captured during
// the handshake, or nil if it was not seen/parsed.
func (c *Conn) PeerVersion() *VersionMessage { return c.peerVersion }

// NetConn returns the underlying TCP connection. The p2p layer owns the socket
// deadlines: handshake() sets the 60 s exchange deadline, and readMessage
// re-arms a per-frame read deadline afterward (idleReadTimeout, mirroring C++
// TIMEOUT_INTERVAL, net.h:45). Caller-set deadlines on the returned conn are
// overwritten/cleared and must not be relied on — use NetConn for socket-level
// inspection (e.g. RemoteAddr), not for read deadlines.
func (c *Conn) NetConn() net.Conn { return c.netConn }

// ReadMessage reads the next raw P2P message (any command), skipping nothing.
// Useful for diagnostics (e.g. discovering the XBridge command name) and for
// callers that need to see non-XBridge traffic.
func (c *Conn) ReadMessage() (*Message, error) { return c.readMessage() }

// teardown closes the underlying connection and signals the writer without
// waiting for the writer goroutine (which may itself be the caller). It is
// safe to call concurrently from the writer and from Close: doneOnce guards
// the single close of done, and net.Conn.Close is safe to call multiple times.
func (c *Conn) teardown() error {
	c.doneOnce.Do(func() { close(c.done) })
	return c.netConn.Close()
}

// Close tears the connection down. It signals the writer (closing the
// underlying net.Conn also unblocks any pending write), then waits for the
// writer goroutine to exit, so no outbound goroutine leaks. Safe to call
// multiple times and from any goroutine.
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

// WriteMessage sends a raw, already-constructed P2P message (any command).
// Used by the discovery layer to exchange getaddr/addr/ping/pong directly.
func (c *Conn) WriteMessage(m *Message) error {
	return c.write(m.Marshal())
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
