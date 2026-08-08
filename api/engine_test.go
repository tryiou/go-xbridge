package api

import (
	"io"
	"testing"
	"time"

	"go-xbridge/crypto"
	"go-xbridge/proto"
)

// blockConn is a test XConn whose ReadPacket blocks until the node's stop
// channel closes (so the reader loop parks instead of spinning on io.EOF) and
// whose WritePacket is a no-op. Used by engine tests that run the real engine.
type blockConn struct {
	stop chan struct{}
}

func (c *blockConn) ReadPacket() (*proto.Packet, string, error) {
	<-c.stop
	return nil, "", io.EOF
}
func (c *blockConn) WritePacket(p *proto.Packet, dest [20]byte) error { return nil }
func (c *blockConn) Close() error                                     { return nil }

// newEngineNode builds a minimal Node with the engine infrastructure zeroed;
// the caller decides whether to call start().
func newEngineNode() *Node {
	return &Node{
		signer:   crypto.NewBtcSigner(),
		stop:     make(chan struct{}),
		store:    NewStore(),
		sessions: map[string]*SwapSession{},
	}
}

// TestSubmitInlineFallback proves the single-threaded fallback the test suite
// relies on: a node whose engine was never started executes a submitted command
// synchronously on the caller goroutine, with no channels involved.
func TestSubmitInlineFallback(t *testing.T) {
	n := newEngineNode()
	calls := 0
	n.submit(func() {
		calls++
	}, true)
	if calls != 1 {
		t.Fatalf("inline submit ran %d times, want 1", calls)
	}
}

// TestEngineLoopAndShutdown runs the real engine against a parked reader: an
// awaited command must execute on the engine, two more must land in FIFO order,
// and Close must join every goroutine without deadlocking.
func TestEngineLoopAndShutdown(t *testing.T) {
	n := newEngineNode()
	n.conn = &blockConn{stop: n.stop}
	n.start()
	t.Cleanup(func() { _ = n.Close() })

	got := []int{}
	n.submit(func() {
		got = append(got, 1)
	}, true)
	if len(got) != 1 || got[0] != 1 {
		t.Fatalf("awaited command not executed, got %v", got)
	}

	n.submit(func() { got = append(got, 2) }, true)
	n.submit(func() { got = append(got, 3) }, true)
	if len(got) != 3 || got[0] != 1 || got[1] != 2 || got[2] != 3 {
		t.Fatalf("command order = %v, want [1 2 3]", got)
	}

	if err := n.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestCloseNoSubmitHang asserts that a submit after Close returns promptly (via
// the inline fallback / stop path) instead of hanging the caller.
func TestCloseNoSubmitHang(t *testing.T) {
	n := newEngineNode()
	n.conn = &blockConn{stop: n.stop}
	n.start()
	if err := n.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	done := make(chan struct{})
	go func() {
		n.submit(func() {}, true)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("submit hung after Close")
	}
}
