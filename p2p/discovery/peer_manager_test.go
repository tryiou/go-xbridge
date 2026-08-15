package discovery

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"go-xbridge/p2p"
	"go-xbridge/p2p/servicenode"
	"go-xbridge/proto"
)

// fakePeer is a minimal hand-rolled XBridge peer on one end of an in-memory
// net.Pipe: it completes the asymmetric version/verack handshake, advertises a
// single address, and sends one XBridge packet. (We drive the handshake
// manually rather than via a second p2p.Conn because net.Pipe is fully
// synchronous — two concurrent writers would deadlock.)
func fakePeer(nc net.Conn, magic [4]byte) {
	// 1) read our version
	readMsg(tDummy{}, nc)
	// 2) send our version
	writeMsg(nc, p2p.Message{
		Magic:    magic,
		Command:  "version",
		Payload:  peerVersionPayload(),
		Checksum: p2p.Checksum(peerVersionPayload()),
	})
	// 3) read our verack
	readMsg(tDummy{}, nc)
	// 4) send our verack
	writeMsg(nc, p2p.Message{Magic: magic, Command: "verack", Checksum: p2p.Checksum(nil)})

	// Consume the client's getaddr (it sends one right after the handshake).
	// net.Pipe is synchronous, so if we don't read it the client's write blocks.
	readMsg(tDummy{}, nc)

	// Advertise one address.
	entries := []p2p.AddrEntry{{Time: 1, Services: 1, IP: net.ParseIP("9.9.9.9"), Port: 41412}}
	writeMsg(nc, p2p.Message{
		Magic:    magic,
		Command:  p2p.CmdAddr,
		Payload:  p2p.MarshalAddr(entries),
		Checksum: p2p.Checksum(p2p.MarshalAddr(entries)),
	})

	// Send one XBridge packet (an xbcTransaction broadcast).
	pkt := proto.NewPacket(proto.XbcTransaction, []byte{0x01, 0x02, 0x03})
	env := wrapXBridge(pkt.Marshal())
	writeMsg(nc, p2p.Message{
		Magic:    magic,
		Command:  p2p.XBridgeNetCommand,
		Payload:  env,
		Checksum: p2p.Checksum(env),
	})
}

// tDummy is a no-op testing.T substitute so readMsg/writeMsg can log without a
// real *testing.T in scope. The fakePeer runs in a goroutine; failures are
// surfaced via the main test's assertions on the manager, not here.
type tDummy struct{}

func (tDummy) Errorf(string, ...interface{}) {}

func peerVersionPayload() []byte {
	// A minimal version message with fXRouter=true so the fake acts like a
	// service node. We reuse p2p's real marshal via a representative struct
	// isn't exposed; build bytes with relay=0, fxrouter=1 is unnecessary — the
	// handshake only needs *a* valid version the client can parse. Use NewVersion
	// through the p2p package is not available here; craft a small valid payload.
	v := &fakeVersion{}
	return v.bytes()
}

// fakeVersion builds a version payload whose only requirement is that
// p2p.UnmarshalVersion can parse it (the client ignores the fields for our
// test). We mirror the minimal field layout: version(4) + services(8) +
// timestamp(8) + addr_recv(26) + addr_from(26) + nonce(8) + ua(varstr) +
// start_height(4) + relay(1) + fxrouter(1).
type fakeVersion struct{}

func (fakeVersion) bytes() []byte {
	buf := new(bytes.Buffer)
	var f [8]byte
	binary.LittleEndian.PutUint32(f[:4], 70713)
	buf.Write(f[:4]) // version
	binary.LittleEndian.PutUint64(f[:], 0)
	buf.Write(f[:]) // services
	binary.LittleEndian.PutUint64(f[:], 1)
	buf.Write(f[:]) // timestamp
	buf.Write(make([]byte, 26))
	buf.Write(make([]byte, 26)) // addr_recv + addr_from
	binary.LittleEndian.PutUint64(f[:], 0)
	buf.Write(f[:])  // nonce
	buf.WriteByte(0) // ua length 0
	binary.LittleEndian.PutUint32(f[:4], 0)
	buf.Write(f[:4]) // start_height
	buf.WriteByte(0) // relay
	buf.WriteByte(1) // fxrouter (peer is a service node)
	return buf.Bytes()
}

// readMsg reads one P2P frame from nc and returns its Message.
func readMsg(_ tDummy, nc net.Conn) *p2p.Message {
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

// writeMsg writes one P2P frame to nc.
func writeMsg(nc net.Conn, m p2p.Message) {
	_, _ = nc.Write(m.Marshal())
}

// wrapXBridge replicates p2p.encodeXBridgePayload (unexported) for the test:
// varint(28+pktLen) || 20-byte dest || 8-byte ts || pkt.
func wrapXBridge(pkt []byte) []byte {
	inner := make([]byte, 0, 28+len(pkt))
	inner = append(inner, make([]byte, 20)...) // broadcast dest addr
	var ts [8]byte
	binary.LittleEndian.PutUint64(ts[:], 0)
	inner = append(inner, ts[:]...)
	inner = append(inner, pkt...)
	out := writeVarIntLocal(len(inner))
	return append(out, inner...)
}

func writeVarIntLocal(n int) []byte {
	if n < 0xfd {
		return []byte{byte(n)}
	}
	b := make([]byte, 3)
	b[0] = 0xfd
	binary.LittleEndian.PutUint16(b[1:], uint16(n))
	return b
}

// TestPeerManagerFakePeer connects a PeerManager to an in-memory fake peer and
// asserts that (a) the XBridge packet is delivered via ReadPacket and (b) the
// advertised address lands in the address manager. No real network is used.
func TestPeerManagerFakePeer(t *testing.T) {
	magic := p2p.MainnetMagic
	// Use "regtest" so BootstrapAddrs performs no real DNS lookups (regtest has
	// no configured seeds) — the only candidate is our injected explicit peer,
	// keeping the test hermetic. The magic still drives the handshake.
	pm := New(magic, "regtest", Options{
		TargetPeers:   1,
		ExplicitAddrs: []string{"fake:1"},
		Dialer: func(ctx context.Context, addr string, magic [4]byte, timeout time.Duration) (*p2p.Conn, error) {
			client, server := net.Pipe()
			go fakePeer(server, magic)
			return p2p.NewConn(client, magic)
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pm.Start(ctx)
	defer func() { _ = pm.Close() }()

	type res struct {
		pkt  *proto.Packet
		peer string
		err  error
	}
	ch := make(chan res, 1)
	go func() {
		p, peer, e := pm.ReadPacket()
		ch <- res{p, peer, e}
	}()

	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("ReadPacket: %v", r.err)
		}
		if r.pkt == nil || r.pkt.Command != proto.XbcTransaction {
			t.Fatalf("unexpected packet: %+v", r.pkt)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("timeout waiting for xbridge packet from fake peer")
	}

	// The advertised address should have been recorded.
	if pm.AddrCount() < 1 {
		t.Fatalf("addr manager count = %d, want >= 1", pm.AddrCount())
	}

	// And at least one healthy peer should be tracked.
	if len(pm.Peers()) < 1 {
		t.Fatalf("connected peers = %d, want >= 1", len(pm.Peers()))
	}
}

// TestPeerManagerExplicitCooldown verifies that an explicit (-addnode) address,
// which is not tracked by the addrman (it may be a hostname), still honors the
// dial cooldown: it must not be re-candidated within the window after a failed
// dial, and must become a candidate again once the window elapses.
func TestPeerManagerExplicitCooldown(t *testing.T) {
	orig := now
	defer func() { now = orig }()
	fake := time.Unix(1000, 0)
	now = func() time.Time { return fake }

	pm := New(p2p.MainnetMagic, "regtest", Options{
		TargetPeers:   1,
		ExplicitAddrs: []string{"hostname.example:41412"},
		DialCooldown:  2 * time.Minute,
		Dialer: func(ctx context.Context, addr string, magic [4]byte, timeout time.Duration) (*p2p.Conn, error) {
			return nil, errors.New("dial failed")
		},
	})

	const addr = "hostname.example:41412"
	if got := pm.nextCandidate(); got != addr {
		t.Fatalf("fresh explicit addr should be a candidate, got %q", got)
	}
	pm.connectOne(addr)

	// Within the cooldown window the explicit addr must not be re-candidated.
	if got := pm.nextCandidate(); got != "" {
		t.Fatalf("explicit addr re-candidated within cooldown, got %q", got)
	}

	// Past the cooldown it becomes a candidate again.
	fake = fake.Add(pm.dialCooldown + time.Second)
	if got := pm.nextCandidate(); got != addr {
		t.Fatalf("explicit addr past cooldown should be a candidate, got %q", got)
	}
}

// TestPeerManagerSeedSurvivesPrune verifies that per-tick re-seeding keeps the
// static bootstrap set resident in the addrman (its seen time refreshed), so
// Prune never evicts a seed and wipes its dial-cooldown bookkeeping — which
// would otherwise degrade unreachable seeds to a dial attempt every tick.
func TestPeerManagerSeedSurvivesPrune(t *testing.T) {
	origNow := now
	origBA := bootstrapAddrs
	defer func() {
		now = origNow
		bootstrapAddrs = origBA
	}()
	fake := time.Unix(1000, 0)
	now = func() time.Time { return fake }
	bootstrapAddrs = func(string) []string { return []string{"1.2.3.4:41412"} }

	pm := New(p2p.MainnetMagic, "mainnet", Options{
		TargetPeers:  1,
		DialCooldown: 2 * time.Minute,
	})
	pm.seedAddrMan()
	const seed = "1.2.3.4:41412"
	if !pm.addrMan.NeedsTry(seed, pm.dialCooldown) {
		t.Fatal("fresh seed should need a try")
	}
	pm.addrMan.MarkTried(seed)

	// Simulate 31 minutes of 15s maintain ticks (prune + re-seed each tick).
	for i := 0; i < 124; i++ {
		fake = fake.Add(15 * time.Second)
		pm.addrMan.Prune(30 * time.Minute)
		pm.seedAddrMan()
	}

	// The seed must still be resident (re-seeding keeps seen fresh so Prune
	// never drops it), unlike the pre-fix behavior where the first prune past
	// 30 minutes evicted it permanently.
	if pm.addrMan.Count() == 0 {
		t.Fatal("seed was pruned away despite per-tick re-seeding")
	}
	if !pm.addrMan.NeedsTry(seed, pm.dialCooldown) {
		t.Fatal("seed should need a try again after the cooldown elapsed")
	}
	pm.addrMan.MarkTried(seed)
	if pm.addrMan.NeedsTry(seed, pm.dialCooldown) {
		t.Fatal("just-tried seed must be cooled down even after 30+ min of ticks")
	}
}

// TestPeerManagerIgnoresGetaddr asserts an inbound getaddr is never answered:
// C++ ignores getaddr from outbound connections (net_processing.cpp:2665-2668)
// and go-xbridge only makes outbound connections, so the request must draw no
// response (the peer times out rather than reading an addr message).
func TestPeerManagerIgnoresGetaddr(t *testing.T) {
	magic := p2p.MainnetMagic
	done := make(chan struct{})
	pm := New(magic, "regtest", Options{
		TargetPeers:   1,
		ExplicitAddrs: []string{"fake:1"},
		Dialer: func(ctx context.Context, addr string, m [4]byte, timeout time.Duration) (*p2p.Conn, error) {
			client, server := net.Pipe()
			go func() {
				defer close(done)
				readMsg(tDummy{}, server) // our version
				writeMsg(server, p2p.Message{Magic: m, Command: "version", Payload: peerVersionPayload(), Checksum: p2p.Checksum(peerVersionPayload())})
				readMsg(tDummy{}, server) // our verack
				writeMsg(server, p2p.Message{Magic: m, Command: "verack", Checksum: p2p.Checksum(nil)})
				readMsg(tDummy{}, server) // manager's getaddr
				writeMsg(server, p2p.Message{Magic: m, Command: p2p.CmdGetAddr, Checksum: p2p.Checksum(nil)})
				_ = server.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
				if msg := readMsg(tDummy{}, server); msg != nil {
					t.Errorf("getaddr was answered with %q, want no response", msg.Command)
				}
			}()
			return p2p.NewConn(client, m)
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pm.Start(ctx)
	defer func() { _ = pm.Close() }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for getaddr-ignore assertion")
	}
}

// TestPeerManagerSNListEcho asserts an inbound SNLIST is answered with an
// SNLISTPING per stored accepted ping, echoing the raw accepted payloads
// (C++ net_processing.cpp:2992-3001).
func TestPeerManagerSNListEcho(t *testing.T) {
	magic := p2p.MainnetMagic
	var key [33]byte
	key[0], key[1] = 0x02, 0xaa
	payload := []byte{0x21, 0x02, 0xaa, 0xff} // arbitrary accepted ping bytes
	done := make(chan struct{})
	pm := New(magic, "regtest", Options{
		TargetPeers:   1,
		ExplicitAddrs: []string{"fake:1"},
		Dialer: func(ctx context.Context, addr string, m [4]byte, timeout time.Duration) (*p2p.Conn, error) {
			client, server := net.Pipe()
			go func() {
				defer close(done)
				readMsg(tDummy{}, server)
				writeMsg(server, p2p.Message{Magic: m, Command: "version", Payload: peerVersionPayload(), Checksum: p2p.Checksum(peerVersionPayload())})
				readMsg(tDummy{}, server)
				writeMsg(server, p2p.Message{Magic: m, Command: "verack", Checksum: p2p.Checksum(nil)})
				readMsg(tDummy{}, server) // manager's getaddr
				writeMsg(server, p2p.Message{Magic: m, Command: servicenode.CmdSNList, Checksum: p2p.Checksum(nil)})
				msg := readMsg(tDummy{}, server)
				if msg == nil || msg.Command != servicenode.CmdSNListPing {
					t.Errorf("expected SNLISTPING response, got %+v", msg)
					return
				}
				if !bytes.Equal(msg.Payload, payload) {
					t.Errorf("SNLISTPING payload = %x, want %x", msg.Payload, payload)
				}
				_ = server.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
				if extra := readMsg(tDummy{}, server); extra != nil {
					t.Errorf("unexpected extra response %q", extra.Command)
				}
			}()
			return p2p.NewConn(client, m)
		},
	})
	// Seed a stored accepted ping so the manager has something to echo. The
	// raw payload now lives in the registry (the parallel rawPings map is
	// gone); AddPing accepts an SPV-tier ping with a non-empty service list
	// whose pubkey must be a fully-valid compressed key, so key[0]=0x02 fits.
	if !pm.snReg.AddPing(servicenode.ServiceNode{
		PubKey:   key,
		Tier:     servicenode.TierSPV,
		Services: []string{"BTC"},
	}, payload) {
		t.Fatal("seed AddPing rejected")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pm.Start(ctx)
	defer func() { _ = pm.Close() }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for SNLIST echo assertion")
	}
}

// TestPeerManagerAddrCapDropped asserts an addr payload declaring more than
// 1000 records is dropped (not stored): a valid single-record addr is kept,
// the oversized one is rejected (C++ Misbehaving, net_processing.cpp:1825-1830).
func TestPeerManagerAddrCapDropped(t *testing.T) {
	magic := p2p.MainnetMagic
	pm := New(magic, "regtest", Options{
		TargetPeers:   1,
		ExplicitAddrs: []string{"fake:1"},
		Dialer: func(ctx context.Context, addr string, m [4]byte, timeout time.Duration) (*p2p.Conn, error) {
			client, server := net.Pipe()
			go func() {
				readMsg(tDummy{}, server)
				writeMsg(server, p2p.Message{Magic: m, Command: "version", Payload: peerVersionPayload(), Checksum: p2p.Checksum(peerVersionPayload())})
				readMsg(tDummy{}, server)
				writeMsg(server, p2p.Message{Magic: m, Command: "verack", Checksum: p2p.Checksum(nil)})
				readMsg(tDummy{}, server) // manager's getaddr
				entries := []p2p.AddrEntry{{Time: 1, Services: 1, IP: net.ParseIP("8.8.8.8"), Port: 41412}}
				addrPayload := p2p.MarshalAddr(entries)
				writeMsg(server, p2p.Message{Magic: m, Command: p2p.CmdAddr, Payload: addrPayload, Checksum: p2p.Checksum(addrPayload)})
				// Declared 1001 records, no bytes follow; rejected by the cap.
				oversized := writeVarIntLocal(1001)
				writeMsg(server, p2p.Message{Magic: m, Command: p2p.CmdAddr, Payload: oversized, Checksum: p2p.Checksum(oversized)})
			}()
			return p2p.NewConn(client, m)
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pm.Start(ctx)
	defer func() { _ = pm.Close() }()

	deadline := time.Now().Add(5 * time.Second)
	for pm.AddrCount() < 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if pm.AddrCount() != 1 {
		t.Fatalf("AddrCount = %d, want 1 (valid addr stored, oversized dropped)", pm.AddrCount())
	}
	time.Sleep(200 * time.Millisecond)
	if pm.AddrCount() != 1 {
		t.Fatalf("AddrCount grew to %d; oversized addr was not dropped", pm.AddrCount())
	}
}
