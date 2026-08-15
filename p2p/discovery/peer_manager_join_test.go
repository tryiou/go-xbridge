package discovery

import (
	"context"
	"testing"
	"time"

	"go-xbridge/p2p"
)

// TestPeerManagerCloseAbortsInflightDialAndJoins — CONC-F93 proof. Close must
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
