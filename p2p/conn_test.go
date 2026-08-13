package p2p

import (
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"
)

// versionPayloadForTest marshals a minimal version payload with the given
// protocol version, using the real encoder.
func versionPayloadForTest(ver int32) []byte {
	return (&VersionMessage{
		Version:   ver,
		Timestamp: 1,
		Nonce:     1,
		UserAgent: UserAgent,
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
	defer client.Close()
	go func() {
		if _, err := readFrame(server); err != nil { // our version
			return
		}
		writeFrame(server, Message{
			Magic:    TestnetMagic, // wrong magic, valid payload checksum
			Command:  "version",
			Payload:  versionPayloadForTest(70713),
			Checksum: Checksum(versionPayloadForTest(70713)),
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
	defer client.Close()
	go func() {
		if _, err := readFrame(server); err != nil { // our version
			return
		}
		writeFrame(server, Message{
			Magic:    MainnetMagic,
			Command:  "version",
			Payload:  versionPayloadForTest(70713),
			Checksum: [4]byte{0xde, 0xad, 0xbe, 0xef}, // bad checksum
		})
		writeFrame(server, Message{
			Magic:    MainnetMagic,
			Command:  "version",
			Payload:  versionPayloadForTest(70713),
			Checksum: Checksum(versionPayloadForTest(70713)),
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
	defer client.Close()
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
	defer client.Close()
	go func() {
		if _, err := readFrame(server); err != nil { // our version
			return
		}
		writeFrame(server, Message{
			Magic:    MainnetMagic,
			Command:  "version",
			Payload:  versionPayloadForTest(BitcoinProtocolVersion),
			Checksum: Checksum(versionPayloadForTest(BitcoinProtocolVersion)),
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
	if c.PeerVersion() == nil || c.PeerVersion().Version != BitcoinProtocolVersion {
		t.Fatalf("peerVersion = %+v, want version %d", c.PeerVersion(), BitcoinProtocolVersion)
	}
	_ = c.Close()
}

// TestConnVersionBelowMinimum asserts a peer advertising a version below
// MIN_PEER_PROTO_VERSION is disconnected (C++ net_processing.cpp:1617-1626).
func TestConnVersionBelowMinimum(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	go func() {
		if _, err := readFrame(server); err != nil { // our version
			return
		}
		writeFrame(server, Message{
			Magic:    MainnetMagic,
			Command:  "version",
			Payload:  versionPayloadForTest(MinPeerProtoVersion - 1),
			Checksum: Checksum(versionPayloadForTest(MinPeerProtoVersion - 1)),
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
	defer client.Close()
	go func() {
		if _, err := readFrame(server); err != nil { // our version
			return
		}
		writeFrame(server, Message{
			Magic:    MainnetMagic,
			Command:  "version",
			Payload:  versionPayloadForTest(BitcoinProtocolVersion),
			Checksum: Checksum(versionPayloadForTest(BitcoinProtocolVersion)),
		})
		if _, err := readFrame(server); err != nil { // our verack (after version 1)
			return
		}
		writeFrame(server, Message{
			Magic:    MainnetMagic,
			Command:  "version",
			Payload:  versionPayloadForTest(BitcoinProtocolVersion),
			Checksum: Checksum(versionPayloadForTest(BitcoinProtocolVersion)),
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
