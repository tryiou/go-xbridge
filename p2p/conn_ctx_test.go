package p2p

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"
)

// TestConnHandshakeAbortsOnCtxCancel closure. NewConnCtx must abort
// an in-flight version/verack handshake the moment its context is cancelled
// (instead of waiting out handshakeTimeout), so a PeerManager Close is not
// wedged by a peer that accepts a connection but never speaks. The peer here
// never sends a version; the client handshake blocks (net.Pipe is synchronous
// until the far end reads/writes), and closing the pipe on cancel unblocks it.
func TestConnHandshakeAbortsOnCtxCancel(t *testing.T) {
	server, client := net.Pipe()
	defer func() { _ = server.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := NewConnCtx(ctx, client, TestnetMagic)
		done <- err
	}()

	time.Sleep(50 * time.Millisecond) // let the handshake start
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("NewConnCtx err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("NewConnCtx did not abort the handshake on cancel")
	}
}

// TestConnCtxUncancelledHandshakesOK anchors the wrapper's happy path: with an
// uncancelled context NewConnCtx must behave exactly like NewConn, so the
// canceller goroutine never mis-errors a healthy handshake.
func TestConnCtxUncancelledHandshakesOK(t *testing.T) {
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
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
	c, err := NewConnCtx(context.Background(), client, MainnetMagic)
	if err != nil {
		t.Fatalf("NewConnCtx: %v", err)
	}
	if c.PeerVersion() == nil || c.PeerVersion().Version != BitcoinProtocolVersion {
		t.Fatalf("peerVersion = %+v, want version %d", c.PeerVersion(), BitcoinProtocolVersion)
	}
	_ = c.Close()
}

// TestDialContextAbortsHandshakeStall closure at the dial boundary.
// A real listener accepts the TCP connection but never sends a version; a
// cancelled context must abort the in-flight handshake promptly (not after
// handshakeTimeout), which is what keeps a discovery PeerManager Close bounded.
func TestDialContextAbortsHandshakeStall(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	hold := make(chan struct{})
	var releaseHold sync.Once
	t.Cleanup(func() { releaseHold.Do(func() { close(hold) }) })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		<-hold // accept and stall: never send a version
	}()

	ctx, cancel := context.WithCancel(context.Background())
	start := time.Now()
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	_, err = DialContext(ctx, ln.Addr().String(), TestnetMagic, 30*time.Second)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("DialContext err = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("DialContext took %v to abort a stalled handshake", elapsed)
	}
	releaseHold.Do(func() { close(hold) })
}
