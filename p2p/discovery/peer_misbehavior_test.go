package discovery

import (
	"context"
	"net"
	"testing"
	"time"

	"go-xbridge/p2p"
)

// undersizedXBridgePayload is a payload that DecodeXBridgePayload rejects: a
// varint declaring 0 bytes, below the 28-byte envelope minimum — the analog of
// C++'s `raw.size() < (20 + sizeof(time_t))` (net_processing.cpp:2874-2878).
var undersizedXBridgePayload = []byte{0x00}

// pmWithFakePeer wires a PeerManager to one net.Pipe-backed fake peer whose
// goroutine sends `msgs` after the handshake. Returns the manager (started) and
// the fake peer's address.
func pmWithFakePeer(t *testing.T, opts Options, msgs func(server net.Conn, magic [4]byte)) (*PeerManager, string) {
	t.Helper()
	magic := p2p.MainnetMagic
	addr := "fake:1"
	if opts.BanThreshold == 0 {
		opts.BanThreshold = 100
	}
	pm := New(magic, "regtest", Options{
		TargetPeers:   1,
		ExplicitAddrs: []string{addr},
		BanThreshold:  opts.BanThreshold,
		BanDuration:   opts.BanDuration,
		Dialer: func(ctx context.Context, a string, m [4]byte, timeout time.Duration) (*p2p.Conn, error) {
			client, server := net.Pipe()
			go func() {
				readMsg(tDummy{}, server)
				writeMsg(server, p2p.Message{Magic: m, Command: "version", Payload: peerVersionPayload(), Checksum: p2p.Checksum(peerVersionPayload())})
				readMsg(tDummy{}, server)
				writeMsg(server, p2p.Message{Magic: m, Command: "verack", Checksum: p2p.Checksum(nil)})
				readMsg(tDummy{}, server) // manager's getaddr
				msgs(server, m)
			}()
			return p2p.NewConn(client, m)
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(func() { cancel(); pm.Close() })
	pm.Start(ctx)
	return pm, addr
}

// waitPeers polls until pm.Peers() has len(expect) entries.
func waitPeers(t *testing.T, pm *PeerManager, expect int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for len(pm.Peers()) != expect && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := len(pm.Peers()); got != expect {
		t.Fatalf("peers = %d, want %d", got, expect)
	}
}

// waitBanned polls until addr is banned AND its peer has left the pool (the
// connect→ban sequence can complete in well under a polling interval, so we
// cannot reliably observe the transient connected state, and there is a window
// between the ban being recorded and readLoop removing the peer).
func waitBanned(t *testing.T, pm *PeerManager, addr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		banned := pm.isBanned(addr)
		live := len(pm.Peers())
		if banned && live == 0 {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("peer %s: banned=%v peers=%d, want banned with no live peer", addr, pm.isBanned(addr), len(pm.Peers()))
}

// TestPeerManagerMisbehaveXBridgeBan locks in the hub ban for the discovery pool:
// ten undersized xbridge envelopes (+10 each, C++ net_processing.cpp:2874-2878)
// take a peer to the 100-point ban threshold — it is disconnected and excluded
// from re-candidating.
func TestPeerManagerMisbehaveXBridgeBan(t *testing.T) {
	pm, addr := pmWithFakePeer(t, Options{}, func(server net.Conn, m [4]byte) {
		for i := 0; i < 10; i++ {
			writeMsg(server, p2p.Message{Magic: m, Command: p2p.XBridgeNetCommand,
				Payload: undersizedXBridgePayload, Checksum: p2p.Checksum(undersizedXBridgePayload)})
		}
	})
	waitBanned(t, pm, addr)
}

// TestPeerManagerMisbehaveAddrBan locks in the addr penalty: five oversized
// addr payloads (+20 each, C++ net_processing.cpp:1825-1830) take the peer to
// the ban threshold.
func TestPeerManagerMisbehaveAddrBan(t *testing.T) {
	pm, addr := pmWithFakePeer(t, Options{}, func(server net.Conn, m [4]byte) {
		oversized := writeVarIntLocal(1001)
		for i := 0; i < 5; i++ {
			writeMsg(server, p2p.Message{Magic: m, Command: p2p.CmdAddr,
				Payload: oversized, Checksum: p2p.Checksum(oversized)})
		}
	})
	waitBanned(t, pm, addr)
}

// TestPeerManagerMisbehaveSubThresholdNoBan proves scores below the threshold
// do NOT disconnect the peer — a single undersized xbridge envelope is scored
// but tolerated, mirroring C++ Misbehaving accumulation.
func TestPeerManagerMisbehaveSubThresholdNoBan(t *testing.T) {
	pm, addr := pmWithFakePeer(t, Options{}, func(server net.Conn, m [4]byte) {
		writeMsg(server, p2p.Message{Magic: m, Command: p2p.XBridgeNetCommand,
			Payload: undersizedXBridgePayload, Checksum: p2p.Checksum(undersizedXBridgePayload)})
	})

	waitPeers(t, pm, 1)
	if pm.isBanned(addr) {
		t.Fatal("peer must not be banned below the threshold")
	}
}
