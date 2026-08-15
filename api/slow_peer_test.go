package api

import (
	"net"
	"testing"
	"time"

	"go-xbridge/p2p"
	"go-xbridge/proto"
)

// TestEngineWriteDoesNotBlockOnSlowPeer — CONC-F92 proof at the engine level.
// A hub peer that stops reading must not stall the engine's outbound path: the
// engine enqueues frames (the p2p writer goroutine does the blocking socket
// write) and keeps processing commands. Mirrors TestEngineLivenessSlowWallet
// for the socket-write axis.
func TestEngineWriteDoesNotBlockOnSlowPeer(t *testing.T) {
	client, server := net.Pipe()
	release := make(chan struct{})
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
		<-release // stuck peer: never reads, so the conn's writer blocks on its first frame
	}()
	conn, err := p2p.NewConn(client, p2p.MainnetMagic)
	if err != nil {
		t.Fatalf("NewConn: %v", err)
	}
	n := newEngineNode()
	n.conn = conn
	n.start()
	t.Cleanup(func() {
		close(release)
		_ = n.Close()
	})

	// Engine-side burst of outbound sends while the peer is stuck: every
	// WritePacket must return without blocking the engine goroutine.
	pkt := proto.NewPacket(proto.XbcTransactionCancel, make([]byte, 36))
	n.submit(func() {
		for i := 0; i < 100; i++ {
			if err := n.conn.WritePacket(pkt, [20]byte{}); err != nil {
				t.Errorf("engine WritePacket: %v", err)
				return
			}
		}
	}, false)

	// The engine must still be alive: an awaited command completes promptly.
	done := make(chan struct{})
	start := time.Now()
	n.submit(func() { close(done) }, true)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("engine blocked on a socket write to a slow peer")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("awaited command took %v — engine stalled on a slow peer", elapsed)
	}
}
