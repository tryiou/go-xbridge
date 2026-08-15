package api

import (
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"

	"go-xbridge/crypto"
	"go-xbridge/p2p"
	"go-xbridge/proto"
)

// readRawMsg reads one P2P frame (header + payload) from nc.
func readRawMsg(nc net.Conn) *p2p.Message {
	hdr := make([]byte, 4+12+4+4)
	if _, err := io.ReadFull(nc, hdr); err != nil {
		return nil
	}
	length := binary.LittleEndian.Uint32(hdr[4+12 : 4+12+4])
	payload := make([]byte, length)
	if _, err := io.ReadFull(nc, payload); err != nil {
		return nil
	}
	msg, err := p2p.UnmarshalMessage(append(hdr, payload...))
	if err != nil {
		return nil
	}
	return msg
}

// writeRawMsg writes one P2P frame to nc.
func writeRawMsg(nc net.Conn, m p2p.Message) {
	_, _ = nc.Write(m.Marshal())
}

// wrapXBridgePkt replicates p2p.encodeXBridgePayload (unexported) for the test:
// varint(28+pktLen) || 20-byte dest || 8-byte ts || pkt.
func wrapXBridgePkt(pkt []byte) []byte {
	inner := make([]byte, 0, 28+len(pkt))
	inner = append(inner, make([]byte, 20)...)
	var ts [8]byte
	inner = append(inner, ts[:]...)
	inner = append(inner, pkt...)
	var out []byte
	if n := len(inner); n < 0xfd {
		out = append(out, byte(n))
	} else {
		out = append(out, 0xfd, byte(n), byte(n>>8))
	}
	return append(out, inner...)
}

// undersizedEnvelope is an xbridge message payload DecodeXBridgePayload
// rejects: a varint declaring 0 bytes, below the 28-byte envelope minimum — the
// analog of C++'s `raw.size() < (20 + sizeof(time_t))`
// (net_processing.cpp:2874-2878).
var undersizedEnvelope = []byte{0x00}

// TestReaderLoopHubBan locks in the hub ban for the direct hub connection: ten
// undersized xbridge envelopes accumulate +10 each (C++ Misbehaving +10,
// net_processing.cpp:2874-2878) and the reader loop drops the connection at the
// 100-point ban threshold.
func TestReaderLoopHubBan(t *testing.T) {
	client, server := net.Pipe()

	// Server side: complete the version/verack handshake, then flood 10
	// undersized xbridge envelopes, then block so the test can observe the
	// client dropping the connection after the ban.
	go func() {
		readRawMsg(server) // client version
		v := p2p.NewVersion(nil).Marshal()
		writeRawMsg(server, p2p.Message{Magic: p2p.MainnetMagic, Command: "version", Payload: v, Checksum: p2p.Checksum(v)})
		readRawMsg(server) // client verack
		writeRawMsg(server, p2p.Message{Magic: p2p.MainnetMagic, Command: "verack", Checksum: p2p.Checksum(nil)})
		for i := 0; i < 10; i++ {
			writeRawMsg(server, p2p.Message{Magic: p2p.MainnetMagic, Command: p2p.XBridgeNetCommand,
				Payload: undersizedEnvelope, Checksum: p2p.Checksum(undersizedEnvelope)})
		}
		readRawMsg(server) // blocks until the client (banned) closes
	}()

	conn, err := p2p.NewConn(client, p2p.MainnetMagic)
	if err != nil {
		t.Fatalf("NewConn: %v", err)
	}
	n := &Node{conn: conn, signer: crypto.NewBtcSigner(), stop: make(chan struct{})}
	n.wg.Add(1)
	done := make(chan struct{})
	go func() {
		n.readerLoop()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("reader loop did not drop the banned hub connection")
	}
	if _, _, err := conn.ReadPacket(); err == nil {
		t.Fatal("hub connection must be closed after the ban")
	}
}

// TestReaderLoopHubSubThresholdNoBan proves a single undersized envelope scores
// +10 but is tolerated below the threshold — the hub stays connected. A
// sized-but-undecodable BODY is dropped without a penalty (C++ DoS 0,
// xbridgesession.cpp:312).
func TestReaderLoopHubSubThresholdNoBan(t *testing.T) {
	client, server := net.Pipe()
	go func() {
		readRawMsg(server)
		v := p2p.NewVersion(nil).Marshal()
		writeRawMsg(server, p2p.Message{Magic: p2p.MainnetMagic, Command: "version", Payload: v, Checksum: p2p.Checksum(v)})
		readRawMsg(server)
		writeRawMsg(server, p2p.Message{Magic: p2p.MainnetMagic, Command: "verack", Checksum: p2p.Checksum(nil)})
		writeRawMsg(server, p2p.Message{Magic: p2p.MainnetMagic, Command: p2p.XBridgeNetCommand,
			Payload: undersizedEnvelope, Checksum: p2p.Checksum(undersizedEnvelope)})
		// A valid envelope with a garbage CancelBody (needs 36 bytes) is
		// dropped unscored — the DoS-0 case.
		pkt := proto.NewPacket(proto.XbcTransactionCancel, []byte{0xde, 0xad, 0xbe, 0xef})
		payload := wrapXBridgePkt(pkt.Marshal())
		writeRawMsg(server, p2p.Message{Magic: p2p.MainnetMagic, Command: p2p.XBridgeNetCommand,
			Payload: payload, Checksum: p2p.Checksum(payload)})
		readRawMsg(server) // blocks until the client closes (shutdown)
	}()

	conn, err := p2p.NewConn(client, p2p.MainnetMagic)
	if err != nil {
		t.Fatalf("NewConn: %v", err)
	}
	n := &Node{conn: conn, signer: crypto.NewBtcSigner(), stop: make(chan struct{})}
	n.wg.Add(1)
	go func() {
		n.readerLoop()
	}()
	t.Cleanup(func() { _ = n.Close() })
	time.Sleep(300 * time.Millisecond)
	// Not banned: the conn is still usable.
	if err := conn.WritePacket(proto.NewPacket(proto.XbcTransactionCancel, make([]byte, 36)), [20]byte{}); err != nil {
		t.Fatalf("hub conn closed below the threshold: %v", err)
	}
}
