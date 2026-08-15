package discovery

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"go-xbridge/p2p"
)

// TestPeerManagerCloseAbortsInflightDialAndJoins. Close must
// cancel an in-flight dial (so it never waits out the dial timeout) and join
// every tracked goroutine before returning, mirroring C++ join_all on shutdown
// (xbridgeapp.cpp:530-544).
func TestPeerManagerCloseAbortsInflightDialAndJoins(t *testing.T) {
	dialStarted := make(chan struct{})
	dialFinished := make(chan struct{})
	pm := New(p2p.MainnetMagic, "regtest", Options{
		TargetPeers:   1,
		ExplicitAddrs: []string{"slow:1"},
		Dialer: func(ctx context.Context, addr string, magic [4]byte, timeout time.Duration) (*p2p.Conn, error) {
			close(dialStarted)
			defer close(dialFinished)
			<-ctx.Done() // the manager's lifecycle ctx; Close cancels it
			return nil, ctx.Err()
		},
	})
	pm.Start(context.Background())
	t.Cleanup(func() { _ = pm.Close() })

	select {
	case <-dialStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("dial never started")
	}

	start := time.Now()
	pm.Close()
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Close took %v — in-flight dial was not aborted", elapsed)
	}
	select {
	case <-dialFinished:
	case <-time.After(time.Second):
		t.Fatal("dial goroutine not joined by Close")
	}
}

// TestPeerManagerCloseFastWithStalledHandshake closure. A peer that
// accepts TCP but never sends a version must not hold Close up to
// handshakeTimeout: DialContext aborts the in-flight handshake the moment the
// manager context is cancelled (NewConnCtx), so connectOne returns promptly and
// the join completes fast.
func TestPeerManagerCloseFastWithStalledHandshake(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	hold := make(chan struct{})
	var releaseHold sync.Once
	t.Cleanup(func() { releaseHold.Do(func() { close(hold) }) })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		<-hold // accept and stall: never send a version
	}()

	dialStarted := make(chan struct{})
	pm := New(p2p.MainnetMagic, "regtest", Options{
		TargetPeers:   1,
		ExplicitAddrs: []string{ln.Addr().String()},
		Dialer: func(ctx context.Context, addr string, magic [4]byte, timeout time.Duration) (*p2p.Conn, error) {
			close(dialStarted)
			return p2p.DialContext(ctx, addr, magic, timeout)
		},
	})
	pm.Start(context.Background())
	t.Cleanup(func() { _ = pm.Close() })

	select {
	case <-dialStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("dial never started")
	}
	time.Sleep(100 * time.Millisecond) // let the handshake stall on the version exchange

	start := time.Now()
	pm.Close()
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Close took %v — stalled handshake was not aborted (want well under handshakeTimeout)", elapsed)
	}
	releaseHold.Do(func() { close(hold) })
}
