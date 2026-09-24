package p2p

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"go-xbridge/proto"
	"go-xbridge/version"
)

// versionPayloadForTest marshals a minimal version payload with the given
// protocol version, using the real encoder.
func versionPayloadForTest(ver int32) []byte {
	return (&VersionMessage{
		Version:   ver,
		Timestamp: 1,
		Nonce:     1,
		UserAgent: version.UserAgent,
	}).Marshal()
}

// writeFrame writes one P2P frame to nc (net.Pipe Write blocks until the other
// side reads, so callers must sequence frames against the client's reads).
func writeFrame(nc net.Conn, m Message) { _, _ = nc.Write(m.Marshal()) }

// readFrame reads one P2P frame from nc.
func readFrame(nc net.Conn) (*Message, error) {
	hdr := make([]byte, 4+cmdSize+8)
	if _, err := io.ReadFull(nc, hdr); err != nil {
		return nil, err
	}
	length := binary.LittleEndian.Uint32(hdr[4+cmdSize : 4+cmdSize+4])
	payload := make([]byte, length)
	if _, err := io.ReadFull(nc, payload); err != nil {
		return nil, err
	}
	return UnmarshalMessage(append(hdr, payload...))
}

// TestConnWrongMagicRejected asserts a frame with the wrong network magic is
// fatal to the connection (C++ disconnects on an invalid message start,
// net_processing.cpp:3117-3121).
func TestConnWrongMagicRejected(t *testing.T) {
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	go func() {
		if _, err := readFrame(server); err != nil { // our version
			return
		}
		writeFrame(server, Message{
			Magic:    TestnetMagic, // wrong magic, valid payload checksum
			Command:  "version",
			Payload:  versionPayloadForTest(version.BitcoinProtocolVersion),
			Checksum: Checksum(versionPayloadForTest(version.BitcoinProtocolVersion)),
		})
	}()
	if _, err := NewConn(client, MainnetMagic); err == nil {
		t.Fatal("expected handshake error for wrong-magic frame")
	}
}

// TestConnChecksumFrameDropped asserts a bad-checksum frame is logged-and-dropped
// rather than killing the connection: the following good frame still completes
// the handshake (C++ net_processing.cpp:3138-3145).
func TestConnChecksumFrameDropped(t *testing.T) {
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	go func() {
		if _, err := readFrame(server); err != nil { // our version
			return
		}
		writeFrame(server, Message{
			Magic:    MainnetMagic,
			Command:  "version",
			Payload:  versionPayloadForTest(version.BitcoinProtocolVersion),
			Checksum: [4]byte{0xde, 0xad, 0xbe, 0xef}, // bad checksum
		})
		writeFrame(server, Message{
			Magic:    MainnetMagic,
			Command:  "version",
			Payload:  versionPayloadForTest(version.BitcoinProtocolVersion),
			Checksum: Checksum(versionPayloadForTest(version.BitcoinProtocolVersion)),
		})
		if _, err := readFrame(server); err != nil { // our verack
			return
		}
		writeFrame(server, Message{Magic: MainnetMagic, Command: "verack", Checksum: Checksum(nil)})
	}()
	c, err := NewConn(client, MainnetMagic)
	if err != nil {
		t.Fatalf("handshake should survive a bad-checksum frame: %v", err)
	}
	_ = c.Close()
}

// TestConnOversizedLengthRejected asserts a declared payload larger than the
// C++ wire cap (MAX_PROTOCOL_MESSAGE_LENGTH, net.h:55) is fatal (disconnect).
func TestConnOversizedLengthRejected(t *testing.T) {
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	go func() {
		if _, err := readFrame(server); err != nil { // our version
			return
		}
		hdr := make([]byte, 4+cmdSize+8)
		binary.LittleEndian.PutUint32(hdr[4+cmdSize:4+cmdSize+4], MaxPayloadSize+1)
		_, _ = server.Write(hdr) // header alone is rejected before the payload read
	}()
	if _, err := NewConn(client, MainnetMagic); err == nil {
		t.Fatal("expected error for oversized declared length")
	}
}

// TestConnHandshakeOK anchors the happy path so the added validation (magic,
// checksum-drop, cap) is proven not to break a conforming peer exchange.
func TestConnHandshakeOK(t *testing.T) {
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	go func() {
		if _, err := readFrame(server); err != nil { // our version
			return
		}
		writeFrame(server, Message{
			Magic:    MainnetMagic,
			Command:  "version",
			Payload:  versionPayloadForTest(version.BitcoinProtocolVersion),
			Checksum: Checksum(versionPayloadForTest(version.BitcoinProtocolVersion)),
		})
		if _, err := readFrame(server); err != nil { // our verack
			return
		}
		writeFrame(server, Message{Magic: MainnetMagic, Command: "verack", Checksum: Checksum(nil)})
	}()
	c, err := NewConn(client, MainnetMagic)
	if err != nil {
		t.Fatalf("handshake failed: %v", err)
	}
	if c.PeerVersion() == nil || c.PeerVersion().Version != version.BitcoinProtocolVersion {
		t.Fatalf("peerVersion = %+v, want version %d", c.PeerVersion(), version.BitcoinProtocolVersion)
	}
	_ = c.Close()
}

// TestConnVersionBelowMinimum asserts a peer advertising a version below
// MIN_PEER_PROTO_VERSION is disconnected (C++ net_processing.cpp:1617-1626).
func TestConnVersionBelowMinimum(t *testing.T) {
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	go func() {
		if _, err := readFrame(server); err != nil { // our version
			return
		}
		writeFrame(server, Message{
			Magic:    MainnetMagic,
			Command:  "version",
			Payload:  versionPayloadForTest(version.MinPeerProtoVersion - 1),
			Checksum: Checksum(versionPayloadForTest(version.MinPeerProtoVersion - 1)),
		})
	}()
	if _, err := NewConn(client, MainnetMagic); err == nil {
		t.Fatal("expected handshake error for obsolete peer version")
	}
}

// TestConnDuplicateVersion asserts a second version message is fatal
// (C++ disconnects on a duplicate version, net_processing.cpp:1574-1582).
func TestConnDuplicateVersion(t *testing.T) {
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	go func() {
		if _, err := readFrame(server); err != nil { // our version
			return
		}
		writeFrame(server, Message{
			Magic:    MainnetMagic,
			Command:  "version",
			Payload:  versionPayloadForTest(version.BitcoinProtocolVersion),
			Checksum: Checksum(versionPayloadForTest(version.BitcoinProtocolVersion)),
		})
		if _, err := readFrame(server); err != nil { // our verack (after version 1)
			return
		}
		writeFrame(server, Message{
			Magic:    MainnetMagic,
			Command:  "version",
			Payload:  versionPayloadForTest(version.BitcoinProtocolVersion),
			Checksum: Checksum(versionPayloadForTest(version.BitcoinProtocolVersion)),
		})
	}()
	if _, err := NewConn(client, MainnetMagic); err == nil {
		t.Fatal("expected handshake error for duplicate version message")
	}
}

// TestHandshakeTimeout asserts the deadline matches the C++
// DEFAULT_PEER_CONNECT_TIMEOUT (net.h:83).
func TestHandshakeTimeout(t *testing.T) {
	if handshakeTimeout != 60*time.Second {
		t.Fatalf("handshakeTimeout = %v, want 60s (C++ DEFAULT_PEER_CONNECT_TIMEOUT)", handshakeTimeout)
	}
}

// TestHandshakeStallTimesOut locks the handshake READ bound: a peer that sends
// its version message but then never sends verack must fail the handshake
// within handshakeTimeout — NOT be left open for idleReadTimeout. This guards
// the readMessage deadline re-arm (which is gated on handshaken) against
// overriding handshake()'s own shorter read deadline mid-exchange.
func TestHandshakeStallTimesOut(t *testing.T) {
	// Shorten the handshake window so the test does not wait 60 seconds.
	oldH := handshakeTimeout
	handshakeTimeout = 100 * time.Millisecond
	t.Cleanup(func() { handshakeTimeout = oldH })
	// The idle window must be far larger than the handshake window (100×),
	// proving the stall is bounded by the handshake deadline, not the idle
	// deadline — while still small enough that a gating regression fails fast
	// in CI rather than hanging for the full idle window.
	oldI := idleReadTimeout
	idleReadTimeout = 10 * time.Second
	t.Cleanup(func() { idleReadTimeout = oldI })

	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	defer func() { _ = server.Close() }()
	go func() {
		if _, err := readFrame(server); err != nil { // our version
			return
		}
		v := versionPayloadForTest(version.BitcoinProtocolVersion)
		writeFrame(server, Message{Magic: MainnetMagic, Command: "version", Payload: v, Checksum: Checksum(v)})
		// Read our verack so the client's write succeeds, then never send our
		// own verack: the client blocks reading it until the handshake deadline
		// fires. (Without this read, the client would fail on its verack WRITE
		// deadline instead, not exercising the read bound this test guards.)
		_, _ = readFrame(server) // our verack
	}()

	start := time.Now()
	_, err := NewConn(client, MainnetMagic)
	if err == nil {
		t.Fatal("expected handshake to fail for a peer that never sends verack")
	}
	if elapsed := time.Since(start); elapsed >= idleReadTimeout {
		t.Fatalf("handshake took %v — the stall was bounded by idleReadTimeout, not handshakeTimeout", elapsed)
	}
}

// TestConnReadPacketVersionGate asserts ReadPacket rejects an inbound packet
// whose header version differs from XBRIDGE_PROTOCOL_VERSION, mirroring the
// C++ gate (xbridgesession.cpp:343-368, xbridgeapp.cpp:648,737). The wrong-
// version packet is dropped and the connection stays usable for the next
// (conforming) packet.
func TestConnReadPacketVersionGate(t *testing.T) {
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	go func() {
		if _, err := readFrame(server); err != nil { // our version
			return
		}
		v := versionPayloadForTest(version.BitcoinProtocolVersion)
		writeFrame(server, Message{Magic: MainnetMagic, Command: "version", Payload: v, Checksum: Checksum(v)})
		if _, err := readFrame(server); err != nil { // our verack
			return
		}
		writeFrame(server, Message{Magic: MainnetMagic, Command: "verack", Checksum: Checksum(nil)})
		// A wrong-version packet (54) first: the gate must drop it.
		bad := &proto.Packet{
			Version: 54,
			Command: proto.XbcTransaction,
		}
		badPayload := encodeXBridgePayload(bad.Marshal(), [20]byte{})
		writeFrame(server, Message{Magic: MainnetMagic, Command: XBridgeNetCommand, Payload: badPayload, Checksum: Checksum(badPayload)})
		// Then a conforming packet that must still be readable.
		good := proto.NewPacket(proto.XbcTransaction, nil)
		goodPayload := encodeXBridgePayload(good.Marshal(), [20]byte{})
		writeFrame(server, Message{Magic: MainnetMagic, Command: XBridgeNetCommand, Payload: goodPayload, Checksum: Checksum(goodPayload)})
	}()

	conn, err := NewConn(client, MainnetMagic)
	if err != nil {
		t.Fatalf("handshake failed: %v", err)
	}
	defer func() { _ = conn.Close() }()

	if _, _, err := conn.ReadPacket(); err == nil {
		t.Fatal("expected wrong-version packet to be dropped")
	}
	pkt, _, err := conn.ReadPacket()
	if err != nil {
		t.Fatalf("conforming packet after the dropped one: %v", err)
	}
	if pkt.Version != version.XBridgeProtocolVersion {
		t.Fatalf("packet version = %d, want %d", pkt.Version, version.XBridgeProtocolVersion)
	}
}

// TestConnIdleReadTimeout verifies the post-handshake slowloris guard: a peer
// that completes the handshake and then goes silent must surface a read
// timeout (C++ disconnects on TIMEOUT_INTERVAL idle, net.h:45 / net.cpp:1068).
// The deadline is re-armed per frame, so this is an IDLE timeout, not a
// total-connection bound.
func TestConnIdleReadTimeout(t *testing.T) {
	// Shorten the idle window so the test does not wait 20 minutes.
	old := idleReadTimeout
	idleReadTimeout = 100 * time.Millisecond
	t.Cleanup(func() { idleReadTimeout = old })

	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	go func() {
		if _, err := readFrame(server); err != nil { // our version
			return
		}
		v := versionPayloadForTest(version.BitcoinProtocolVersion)
		writeFrame(server, Message{Magic: MainnetMagic, Command: "version", Payload: v, Checksum: Checksum(v)})
		if _, err := readFrame(server); err != nil { // our verack
			return
		}
		writeFrame(server, Message{Magic: MainnetMagic, Command: "verack", Checksum: Checksum(nil)})
		// Handshake complete; say nothing more. ReadPacket must time out.
	}()

	conn, err := NewConn(client, MainnetMagic)
	if err != nil {
		t.Fatalf("handshake failed: %v", err)
	}
	defer func() { _ = conn.Close() }()

	_, _, err = conn.ReadPacket()
	if err == nil {
		t.Fatal("expected idle read timeout after silent handshake")
	}
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("ReadPacket err = %v, want a timeout", err)
	}
}
