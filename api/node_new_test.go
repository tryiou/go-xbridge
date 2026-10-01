package api

import (
	"net"
	"testing"

	"go-xbridge/p2p"
	discovery "go-xbridge/p2p/discovery"
)

// TestNewNodeRegtestDiscoveryLifecycle drives the real NewNode constructor
// (node.go:445-513) — every other fixture builds a Node by struct literal and
// never starts the engine. Hermetic by construction: NodeAddr empty selects
// the discovery branch, "regtest" has no public seeds (p2p/seeds.go:75-76),
// AddNodes is nil, and with Connectors/Confs unset the sweep loop has nothing
// to touch — so the PeerManager starts with zero peers and never dials. The
// DataDir exercises restoreLocalSwaps' missing-file no-op path.
//
// Not parallel: the engine tick reads the swappable NowMicro/retryCheckInterval
// package vars.
func TestNewNodeRegtestDiscoveryLifecycle(t *testing.T) {
	cfg := &Config{Network: "regtest", DataDir: t.TempDir(), Magic: p2p.RegtestMagic}
	store := NewStore()
	n, err := NewNode(cfg, store)
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}
	// Registered immediately: NewNode starts the engine, persist, and worker
	// goroutines, so any failure below must still tear them down.
	t.Cleanup(func() { _ = n.Close() })

	if n.cfg() != cfg {
		t.Error("cfg() must return the exact config pointer passed to NewNode")
	}
	if n.store != store {
		t.Error("store identity must be preserved")
	}
	if n.snReg == nil {
		t.Error("snReg must be non-nil (the discovery manager's registry)")
	}
	if _, ok := n.conn.(*discovery.PeerManager); !ok {
		t.Errorf("conn = %T, want *discovery.PeerManager (NodeAddr empty)", n.conn)
	}
	if n.signer == nil {
		t.Error("signer must be non-nil")
	}
	if n.activator == nil {
		t.Error("activator must be defaulted when cfg.Activator is nil")
	}
	if n.startedAtMicro == 0 {
		t.Error("startedAtMicro must be stamped at construction")
	}
	if !n.engineRunning.Load() {
		t.Error("engine must be running after NewNode returns")
	}
	if !n.persistUp.Load() {
		t.Error("persist loop must be up after NewNode returns")
	}
	// Buffered channel caps fixed by start() (engine.go:158-167).
	for _, c := range []struct {
		name string
		got  int
		want int
	}{
		{"packets", cap(n.packets), 256},
		{"cmds", cap(n.cmds), 64},
		{"tasks", cap(n.tasks), 16},
		{"results", cap(n.results), engineWorkers},
		{"persistSignal", cap(n.persistSignal), 1},
	} {
		if c.got != c.want {
			t.Errorf("cap(%s) = %d, want %d", c.name, c.got, c.want)
		}
	}
	if len(n.sessions) != 0 {
		t.Errorf("sessions = %d entries, want 0 on a fresh node", len(n.sessions))
	}
	// Both maps are created lazily on first use — a fresh, idle node must not
	// have pre-created them (take_retry.go:86-87, reconcile.go:66-67).
	if n.takeRetries != nil {
		t.Error("takeRetries must stay nil until a take is registered")
	}
	if n.tracked != nil {
		t.Error("tracked must stay nil until a broadcast is recorded")
	}

	if err := n.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if n.engineRunning.Load() {
		t.Error("Close must stop the engine")
	}
	// The registered cleanup calls Close again: stopOnce + PeerManager.once
	// make the double teardown a no-op rather than a panic.
}

// TestNewNodeExplicitPeerDialFailure covers the explicit-peer error branch
// (node.go:470-473): when the only configured service node refuses the
// connection, NewNode returns both the partial node and the error, the engine
// never starts, and Close on the partial node is safe. The dial target is a
// just-closed loopback listener, so the refusal is immediate and local — the
// expected transport failure this test exists to pin.
func TestNewNodeExplicitPeerDialFailure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}

	cfg := &Config{NodeAddr: addr, DataDir: t.TempDir(), Magic: p2p.RegtestMagic}
	n, err := NewNode(cfg, NewStore())
	if err == nil {
		_ = n.Close()
		t.Fatal("NewNode: expected a dial error against the closed loopback port")
	}
	if n == nil {
		t.Fatal("NewNode must return the partial node alongside the dial error so the caller can Close it")
	}
	if n.engineRunning.Load() {
		t.Error("engine must not start when the explicit dial fails")
	}
	if n.conn != nil {
		t.Errorf("conn = %T, want nil after a failed dial", n.conn)
	}
	if err := n.Close(); err != nil {
		t.Errorf("Close on the partial node: %v", err)
	}
}
