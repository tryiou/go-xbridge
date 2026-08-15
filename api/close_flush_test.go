package api

import (
	"sync"
	"testing"
	"time"

	xlog "go-xbridge/log"
)

// TestNodeCloseFlushesDedupeSweepers — CONC-F93 proof at the api boundary.
// Node.Close must call xlog.FlushAll, so a library-level Close (not just the
// daemon's main.go) stops every Dedupe sweeper and emits any pending summary
// instead of leaking the goroutine.
func TestNodeCloseFlushesDedupeSweepers(t *testing.T) {
	var mu sync.Mutex
	emitted := 0
	summarize := func(key string, total int, elapsed time.Duration) {
		mu.Lock()
		emitted++
		mu.Unlock()
	}
	d := xlog.NewDedupe(time.Hour, summarize)
	defer d.Flush()
	for i := 0; i < 5; i++ {
		d.Event("k") // 4 suppressed; the pending summary is held until flush
	}

	n := newEngineNode()
	_ = n.Close() // must invoke xlog.FlushAll

	mu.Lock()
	got := emitted
	mu.Unlock()
	if got != 1 {
		t.Fatalf("Close flushed %d summaries, want 1 (pending summary emitted)", got)
	}
}
