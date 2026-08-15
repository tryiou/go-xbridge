package api

import (
	"net"
	"testing"
	"time"

	"go-xbridge/crypto"
	"go-xbridge/p2p"
	"go-xbridge/proto"
)

// TestReaderLoopDropsSignedWrongVersion locks in the inbound protocol-version
// gate end to end: a packet whose header version differs from
// XBRIDGE_PROTOCOL_VERSION is dropped even when its signature is VALID — the
// gate runs before signature verification, exactly like C++
// (Session::checkXBridgePacketVersion before packet->verify(),
// xbridgesession.cpp:343-368, xbridgeapp.cpp:648,737). The dropped packet must
// never reach the engine, and the connection must stay usable for the next
// conforming packet.
func TestReaderLoopDropsSignedWrongVersion(t *testing.T) {
	priv, err := crypto.NewPrivateKey()
	if err != nil {
		t.Fatalf("NewPrivateKey: %v", err)
	}
	signer := crypto.NewBtcSigner()
	// A decodable, validly-signed packet with a wrong protocol version: the
	// version gate (not the body decoder) is the only thing that can drop it.
	bad := proto.NewPacket(proto.XbcTransactionCancel, make([]byte, 36))
	bad.Version = 54
	if err := signer.Sign(bad, priv); err != nil {
		t.Fatalf("sign wrong-version packet: %v", err)
	}
	good := proto.NewPacket(proto.XbcTransactionCancel, make([]byte, 36))
	if err := signer.Sign(good, priv); err != nil {
		t.Fatalf("sign conforming packet: %v", err)
	}

	client, server := net.Pipe()
	go func() {
		if readRawMsg(server) == nil { // our version
			return
		}
		v := p2p.NewVersion(nil).Marshal()
		writeRawMsg(server, p2p.Message{Magic: p2p.MainnetMagic, Command: "version", Payload: v, Checksum: p2p.Checksum(v)})
		if readRawMsg(server) == nil { // our verack
			return
		}
		writeRawMsg(server, p2p.Message{Magic: p2p.MainnetMagic, Command: "verack", Checksum: p2p.Checksum(nil)})

		badPayload := wrapXBridgePkt(bad.Marshal())
		writeRawMsg(server, p2p.Message{Magic: p2p.MainnetMagic, Command: p2p.XBridgeNetCommand,
			Payload: badPayload, Checksum: p2p.Checksum(badPayload)})

		goodPayload := wrapXBridgePkt(good.Marshal())
		writeRawMsg(server, p2p.Message{Magic: p2p.MainnetMagic, Command: p2p.XBridgeNetCommand,
			Payload: goodPayload, Checksum: p2p.Checksum(goodPayload)})
	}()

	conn, err := p2p.NewConn(client, p2p.MainnetMagic)
	if err != nil {
		t.Fatalf("handshake failed: %v", err)
	}
	n := newEngineNode()
	n.conn = conn
	n.packets = make(chan inboundPacket, 256)
	n.wg.Add(1)
	go n.readerLoop()
	t.Cleanup(func() {
		// Close the conn first: it unblocks readerLoop's blocking ReadPacket,
		// so the reader goroutine can observe n.stop and return.
		_ = conn.Close()
		close(n.stop)
		n.wg.Wait()
	})

	// The wrong-version packet (sent first) must be dropped: the first packet
	// the engine sees is the conforming one.
	select {
	case in := <-n.packets:
		if in.pkt.Version != proto.ProtocolVersion {
			t.Fatalf("packet reached engine with version=%d cmd=%v; wrong-version packets must be dropped",
				in.pkt.Version, in.pkt.Command)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("conforming packet never reached the engine")
	}

	// No further packets: the wrong-version one was never delivered.
	select {
	case in := <-n.packets:
		t.Fatalf("unexpected extra packet reached the engine: version=%d cmd=%v", in.pkt.Version, in.pkt.Command)
	case <-time.After(300 * time.Millisecond):
	}
}
