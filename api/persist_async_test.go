package api

import (
	"encoding/json"
	"sync"
	"testing"
	"time"
)

// persistNode builds a started engine node wired for background-persist tests:
// a temp DataDir, an empty store + session map, and a parked reader (the
// blockConn from engine_test.go) so the engine loop runs without a real peer.
func persistNode(t *testing.T) *Node {
	t.Helper()
	n := newEngineNode()
	n.config = &Config{DataDir: t.TempDir(), PersistSecrets: true}
	n.conn = &blockConn{stop: n.stop}
	n.start()
	t.Cleanup(func() { _ = n.Close() })
	return n
}

// addLocalOrder books a local (Mine) order so a persist snapshot has something
// to serialize.
func addLocalOrder(n *Node, seed byte) {
	o := &Order{ID: [32]byte{seed}, FromCurrency: "BTC", ToCurrency: "LTC", FromAmount: 1e8, ToAmount: 2e8, Mine: true}
	n.store.Add(o)
}

// TestPersistDoesNotBlockEngine — a slow disk (parked
// writeSwaps) must not stall the engine: persist() snapshots + publishes
// non-blocking, the persistLoop does the fsync, and the engine keeps processing
// awaited commands.
func TestPersistDoesNotBlockEngine(t *testing.T) {
	n := persistNode(t)

	diskParked := make(chan struct{})
	release := make(chan struct{})
	var parkOnce, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	orig := writeSwaps
	writeSwaps = func(path string, data []byte) error {
		parkOnce.Do(func() { close(diskParked) })
		<-release
		return nil
	}
	// Cleanup order matters: this runs BEFORE persistNode's Close cleanup (LIFO),
	// so the parked writeSwaps is released before Close joins the persistLoop.
	t.Cleanup(func() { writeSwaps = orig; unblock() })

	// Engine-side persist whose disk write parks on the gate.
	addLocalOrder(n, 1)
	n.submit(func() { n.persist() }, true)
	<-diskParked // the fsync is now in flight

	// The engine must still process an awaited command promptly.
	done := make(chan struct{})
	n.submit(func() { close(done) }, true)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("engine blocked on a background persist")
	}
	unblock()
}

// TestPersistCoalescesBurst — a burst of engine persists must collapse into the
// newest snapshot: with the first disk write parked, two more publishes leave
// one pending signal + one latest slot, so exactly two writes happen (A then
// C), B is superseded, and the file reflects C (all three orders).
func TestPersistCoalescesBurst(t *testing.T) {
	n := persistNode(t)

	var mu sync.Mutex
	writes := 0
	var lastData []byte
	firstWrite := make(chan struct{})
	release := make(chan struct{})
	var firstOnce, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	orig := writeSwaps
	writeSwaps = func(path string, data []byte) error {
		mu.Lock()
		writes++
		lastData = data
		mu.Unlock()
		firstOnce.Do(func() { close(firstWrite) })
		<-release // park every write so the burst coalesces deterministically
		return nil
	}
	// Cleanup order matters: this runs BEFORE persistNode's Close cleanup (LIFO),
	// so the parked writeSwaps is released before Close joins the persistLoop.
	t.Cleanup(func() { writeSwaps = orig; unblock() })

	// A: first disk write parks on the gate. persist() runs ON the engine (the
	// snapshot reads the engine-owned sessions map, so it must never be called
	// from a test goroutine on a started node).
	persist := func() { n.submit(func() { n.persist() }, true) }
	addLocalOrder(n, 1)
	persist()
	<-firstWrite

	// B and C publish while A is parked: C supersedes B in the latest slot.
	addLocalOrder(n, 2)
	persist()
	addLocalOrder(n, 3)
	persist()

	unblock()
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		w := writes
		mu.Unlock()
		if w >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("write count = %d, want 2 (A + C; B coalesced)", w)
		}
		time.Sleep(2 * time.Millisecond)
	}
	mu.Lock()
	w := writes
	mu.Unlock()
	if w != 2 {
		t.Fatalf("write count = %d, want exactly 2 (A + C)", w)
	}

	// The newest snapshot wins: the final write carries all three orders (the
	// captured blob, since the override parks instead of touching disk).
	mu.Lock()
	final := lastData
	mu.Unlock()
	var env swapFile
	if err := json.Unmarshal(final, &env); err != nil {
		t.Fatalf("decode final write: %v", err)
	}
	if len(env.Swaps) != 3 {
		t.Fatalf("final write carries %d swaps, want 3 (newest snapshot wins)", len(env.Swaps))
	}
}

// TestPersistFlushedOnClose — Close must flush the newest pending snapshot
// before returning, so the last engine state is durable after a graceful stop
// (even a persist published by a command that raced the shutdown drain).
func TestPersistFlushedOnClose(t *testing.T) {
	n := persistNode(t)
	persist := func() { n.submit(func() { n.persist() }, true) }
	addLocalOrder(n, 1)
	persist() // async
	addLocalOrder(n, 2)
	persist() // async, supersedes the first

	_ = n.Close() // joins all goroutines, then flushes the latest slot

	ps, err := loadSwaps(swapStatePath(n.config.DataDir))
	if err != nil {
		t.Fatalf("loadSwaps: %v", err)
	}
	if len(ps) != 2 {
		t.Fatalf("persisted %d swaps, want 2 (final flush on Close)", len(ps))
	}
}
