package discovery

import (
	"context"
	"encoding/binary"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	xlog "go-xbridge/log"
	"go-xbridge/p2p"
	"go-xbridge/proto"
	"go-xbridge/version"
)

// installDecodeSplitLogs wires the production file split (general + packet
// files) into temp dirs for decode-routing assertions. Restores the logger
// and level on cleanup. Mirrors api/installSplitLogs through the real Set
// calls.
func installDecodeSplitLogs(t *testing.T) (genPath, pktPath string) {
	t.Helper()
	dir := t.TempDir()
	genPath = filepath.Join(dir, "xbridged.log")
	pktPath = filepath.Join(dir, "log-p2p", "xbridgep2p.log")

	genRw, err := xlog.SetFileLogger(genPath, 1<<20, 2)
	if err != nil {
		t.Fatalf("SetFileLogger: %v", err)
	}
	pktRw, err := xlog.SetP2PLogFile(pktPath, 1<<20, 2)
	if err != nil {
		t.Fatalf("SetFileLogger: %v", err)
	}
	oldLevel := xlog.Level()
	xlog.SetLevel(slog.LevelDebug)
	t.Cleanup(func() {
		xlog.SetLevel(oldLevel)
		_ = genRw.Close()
		_ = pktRw.Close()
		xlog.SetLogger(nil)
	})
	return genPath, pktPath
}

func readDecodeLog(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile %s: %v", path, err)
	}
	return string(data)
}

// waitForDecodeLog polls path until it contains substr or the timeout fires.
func waitForDecodeLog(t *testing.T, path, substr string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		body := readDecodeLog(t, path)
		if strings.Contains(body, substr) {
			return body
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %q in %s; body: %q", substr, path, body)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// wrongVersionFrame builds a well-formed xbridge transport frame whose inner
// packet header carries a version one below the effective one: it sails
// through the envelope decoder and dies exactly at the proto version gate.
// No literals: both sides derive from the single source.
func wrongVersionFrame() []byte {
	pkt := make([]byte, proto.HeaderSize+10)
	binary.LittleEndian.PutUint32(pkt[0:4], version.XBridgeProtocolVersion-1)
	return wrapXBridge(pkt)
}

// decodeFailureDialer completes the version/verack handshake, then serves n
// copies of a wrong-version xbridge frame.
func decodeFailureDialer(n int) func(context.Context, string, [4]byte, time.Duration) (*p2p.Conn, error) {
	return func(ctx context.Context, addr string, m [4]byte, timeout time.Duration) (*p2p.Conn, error) {
		client, server := net.Pipe()
		go func() {
			readMsg(tDummy{}, server)
			writeMsg(server, p2p.Message{Magic: m, Command: "version", Payload: peerVersionPayload(), Checksum: p2p.Checksum(peerVersionPayload())})
			readMsg(tDummy{}, server)
			writeMsg(server, p2p.Message{Magic: m, Command: "verack", Checksum: p2p.Checksum(nil)})
			for i := 0; i < n; i++ {
				bad := wrongVersionFrame()
				writeMsg(server, p2p.Message{Magic: m, Command: p2p.XBridgeNetCommand, Payload: bad, Checksum: p2p.Checksum(bad)})
			}
		}()
		return p2p.NewConn(client, m)
	}
}

// TestReadLoopDecodeFailuresRoutedToPacketFile pins the minority-fork diet:
// repeated version-gated drops must never reach the general log. The first
// sighting lands in the packet file; repeats collapse (no per-packet lines).
func TestReadLoopDecodeFailuresRoutedToPacketFile(t *testing.T) {
	genPath, pktPath := installDecodeSplitLogs(t)

	pm := New(p2p.MainnetMagic, "regtest", Options{
		TargetPeers:   1,
		ExplicitAddrs: []string{"fake:1"},
		Dialer:        decodeFailureDialer(3),
	})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pm.Start(ctx)
	defer func() { _ = pm.Close() }()

	waitForDecodeLog(t, pktPath, "unmarshal failed", 5*time.Second)
	time.Sleep(300 * time.Millisecond) // settle: repeats must stay collapsed

	if gen := readDecodeLog(t, genPath); strings.Contains(gen, "unmarshal failed") {
		t.Errorf("general log must not contain decode failures: %q", gen)
	}
	if got := strings.Count(readDecodeLog(t, pktPath), "unmarshal failed"); got != 1 {
		t.Errorf("packet log has %d decode-failure lines, want 1 (first-sighting only)", got)
	}
}

// corruptEnvelopeDialer completes the version/verack handshake, then serves
// n malformed xbridge frames (a 1-byte payload fails the envelope length
// check): each must score +10 misbehaviour, C++ net_processing.cpp:2874-2878.
func corruptEnvelopeDialer(n int) func(context.Context, string, [4]byte, time.Duration) (*p2p.Conn, error) {
	return func(ctx context.Context, addr string, m [4]byte, timeout time.Duration) (*p2p.Conn, error) {
		client, server := net.Pipe()
		go func() {
			readMsg(tDummy{}, server)
			writeMsg(server, p2p.Message{Magic: m, Command: "version", Payload: peerVersionPayload(), Checksum: p2p.Checksum(peerVersionPayload())})
			readMsg(tDummy{}, server)
			writeMsg(server, p2p.Message{Magic: m, Command: "verack", Checksum: p2p.Checksum(nil)})
			for i := 0; i < n; i++ {
				bad := []byte{0x01}
				writeMsg(server, p2p.Message{Magic: m, Command: p2p.XBridgeNetCommand, Payload: bad, Checksum: p2p.Checksum(bad)})
			}
		}()
		return p2p.NewConn(client, m)
	}
}

// TestCorruptEnvelopeRoutedAndScored pins the other decode path: a malformed
// envelope is a corrupt frame, not version drift — its first-sighting still
// leaves the general log for the packet file, repeats still collapse, but
// the +10 misbehaviour penalty still fires per frame (scoring is not gated
// by the log diet).
func TestCorruptEnvelopeRoutedAndScored(t *testing.T) {
	genPath, pktPath := installDecodeSplitLogs(t)

	pm := New(p2p.MainnetMagic, "regtest", Options{
		TargetPeers:   1,
		ExplicitAddrs: []string{"fake:1"},
		Dialer:        corruptEnvelopeDialer(3),
	})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pm.Start(ctx)
	defer func() { _ = pm.Close() }()

	waitForDecodeLog(t, pktPath, "payload decode failed", 5*time.Second)
	time.Sleep(300 * time.Millisecond) // settle: repeats must stay collapsed

	if gen := readDecodeLog(t, genPath); strings.Contains(gen, "payload decode failed") {
		t.Errorf("general log must not contain decode failures: %q", gen)
	}
	if got := strings.Count(readDecodeLog(t, pktPath), "payload decode failed"); got != 1 {
		t.Errorf("packet log has %d decode-failure lines, want 1 (first-sighting only)", got)
	}
	pm.mu.Lock()
	total := 0
	for _, score := range pm.misbehavior {
		total += score
	}
	banned := len(pm.banned)
	pm.mu.Unlock()
	if total != 30 {
		t.Errorf("misbehaviour total = %d, want 30 (3 frames x +10, scoring unconditional)", total)
	}
	if banned != 0 {
		t.Errorf("banned peers = %d, want 0 (30 < default 100 threshold)", banned)
	}
}

// TestVersionMismatchScoresNoMisbehavior guards the other half of the gate
// contract: dropping a well-formed but wrong-version packet carries no
// misbehaviour penalty (C++ state.DoS() is a TODO), so a minority fork can
// never ban its way across the network.
func TestVersionMismatchScoresNoMisbehavior(t *testing.T) {
	genPath, pktPath := installDecodeSplitLogs(t)

	pm := New(p2p.MainnetMagic, "regtest", Options{
		TargetPeers:   1,
		ExplicitAddrs: []string{"fake:1"},
		Dialer:        decodeFailureDialer(5),
	})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pm.Start(ctx)
	defer func() { _ = pm.Close() }()

	// Prove the frames actually flowed through the read loop (either
	// destination: routing is the other test's job) before asserting.
	deadline := time.Now().Add(5 * time.Second)
	for {
		gen, pkt := readDecodeLog(t, genPath), readDecodeLog(t, pktPath)
		if strings.Contains(gen, "unmarshal failed") || strings.Contains(pkt, "unmarshal failed") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timeout waiting for decode failures to flow")
		}
		time.Sleep(10 * time.Millisecond)
	}

	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		pm.mu.Lock()
		scored := len(pm.misbehavior) != 0 || len(pm.banned) != 0
		pm.mu.Unlock()
		if scored {
			t.Fatalf("version-gated drops must not score misbehaviour or bans")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
