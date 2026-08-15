package log

import (
	"sync"
	"testing"
	"time"
)

// captureSummaries records summarize calls for assertions.
type captureSummaries struct {
	mu      sync.Mutex
	keys    []string
	counts  []int
	elapsed []time.Duration
}

func (c *captureSummaries) add(key string, total int, elapsed time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.keys = append(c.keys, key)
	c.counts = append(c.counts, total)
	c.elapsed = append(c.elapsed, elapsed)
}

func (c *captureSummaries) total() (int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.keys), len(c.elapsed)
}

func TestDedupe_MultiKey(t *testing.T) {
	cap := &captureSummaries{}
	d := NewDedupe(100*time.Millisecond, cap.add)

	const n = 10
	firstSightings := 0
	for i := 0; i < n; i++ {
		for k := 0; k < 3; k++ {
			if d.Event(string(rune('A' + k))) {
				firstSightings++
			}
		}
	}
	if firstSightings != 3 {
		t.Fatalf("first sightings = %d, want 3", firstSightings)
	}

	// Drain the buckets via explicit ticks.
	for i := 0; i < 4; i++ {
		d.tick()
		time.Sleep(120 * time.Millisecond)
	}
	d.Flush()

	summaries, _ := cap.total()
	if summaries != 3 {
		t.Fatalf("summaries = %d, want 3 (one per key)", summaries)
	}
	cap.mu.Lock()
	for i, e := range cap.elapsed {
		if e <= 0 {
			t.Fatalf("summary elapsed = %v, want > 0 (no over=0s)", e)
		}
		if cap.keys[i] == "" {
			t.Fatalf("summary key empty, want non-empty (key must be passed through)")
		}
	}
	cap.mu.Unlock()
}

func TestDedupe_SingleKeyRepeats(t *testing.T) {
	cap := &captureSummaries{}
	d := NewDedupe(50*time.Millisecond, cap.add)

	first := d.Event("k")
	if !first {
		t.Fatal("first event should return true")
	}
	for i := 0; i < 99; i++ {
		if d.Event("k") {
			t.Fatal("repeat should be suppressed")
		}
	}
	for i := 0; i < 4; i++ {
		d.tick()
		time.Sleep(60 * time.Millisecond)
	}
	d.Flush()

	summaries, _ := cap.total()
	// 1 first sighting + a few periodic summaries, far fewer than 100.
	if summaries == 0 || summaries >= 100 {
		t.Fatalf("summaries = %d, want 1..99", summaries)
	}
	cap.mu.Lock()
	for _, k := range cap.keys {
		if k != "k" {
			t.Fatalf("unexpected key %q, want k", k)
		}
	}
	cap.mu.Unlock()
}

func TestDedupe_QuietWindow(t *testing.T) {
	d := NewDedupe(50*time.Millisecond, nil)
	if !d.Event("k") {
		t.Fatal("first should be true")
	}
	if d.Event("k") {
		t.Fatal("repeat within window should be false")
	}
	time.Sleep(60 * time.Millisecond)
	if !d.Event("k") {
		t.Fatal("after quiet window should re-emit (true)")
	}
	d.Flush()
}

func TestDedupe_NilSafe(t *testing.T) {
	var d *Dedupe
	if !d.Event("x") {
		t.Fatal("nil Dedupe Event should return true")
	}
	d.Flush() // must not panic
}

func TestDedupe_FlushEmitsPending(t *testing.T) {
	cap := &captureSummaries{}
	d := NewDedupe(time.Hour, cap.add)
	if !d.Event("k") {
		t.Fatal("first should be true")
	}
	for i := 0; i < 5; i++ {
		d.Event("k")
	}
	d.Flush()
	summaries, _ := cap.total()
	if summaries != 1 {
		t.Fatalf("summaries = %d, want 1", summaries)
	}
	cap.mu.Lock()
	defer cap.mu.Unlock()
	if cap.keys[0] != "k" {
		t.Fatalf("key = %q, want k", cap.keys[0])
	}
	if cap.counts[0] != 6 {
		t.Fatalf("count = %d, want 6", cap.counts[0])
	}
}

func TestDedupe_IdleEviction(t *testing.T) {
	cap := &captureSummaries{}
	d := NewDedupe(50*time.Millisecond, cap.add)
	if !d.Event("k") {
		t.Fatal("first should be true")
	}
	for i := 0; i < 3; i++ {
		d.Event("k")
	}
	// Exceed the idle cutoff (2*quietFor) without ticking, then evict.
	time.Sleep(120 * time.Millisecond)
	d.tick()

	cap.mu.Lock()
	defer cap.mu.Unlock()
	if len(cap.keys) != 1 {
		t.Fatalf("idle-eviction summaries = %d, want 1 (emit-then-delete)", len(cap.keys))
	}
	if cap.keys[0] != "k" {
		t.Fatalf("key = %q, want k", cap.keys[0])
	}
	d.mu.Lock()
	n := len(d.m)
	d.mu.Unlock()
	if n != 0 {
		t.Fatalf("map size = %d, want 0 after eviction", n)
	}
	d.Flush()
}

func TestDedupe_SummarizeReentry(t *testing.T) {
	// The summarize callback re-enters the same Dedupe. A summary must never be
	// invoked while the mutex is held, or this would deadlock.
	var d *Dedupe
	d = NewDedupe(50*time.Millisecond, func(key string, total int, elapsed time.Duration) {
		d.Event("nested")
	})
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			for k := 0; k < 3; k++ {
				d.Event(string(rune('A' + k)))
			}
		}
		for i := 0; i < 4; i++ {
			d.tick()
			time.Sleep(60 * time.Millisecond)
		}
		d.Flush()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("dedupe deadlocked when summarize re-enters Event")
	}
}

func TestFlushAll(t *testing.T) {
	cap1 := &captureSummaries{}
	cap2 := &captureSummaries{}
	d1 := NewDedupe(time.Hour, cap1.add)
	d2 := NewDedupe(time.Hour, cap2.add)
	d1.Event("a")
	d2.Event("b")
	for i := 0; i < 3; i++ {
		d1.Event("a")
		d2.Event("b")
	}
	FlushAll()

	c1, _ := cap1.total()
	c2, _ := cap2.total()
	if c1 != 1 || c2 != 1 {
		t.Fatalf("FlushAll summaries = %d/%d, want 1/1", c1, c2)
	}
	cap1.mu.Lock()
	if cap1.keys[0] != "a" {
		t.Fatalf("cap1 key = %q, want a", cap1.keys[0])
	}
	cap1.mu.Unlock()
	cap2.mu.Lock()
	if cap2.keys[0] != "b" {
		t.Fatalf("cap2 key = %q, want b", cap2.keys[0])
	}
	cap2.mu.Unlock()
	regMu.Lock()
	n := len(registry)
	regMu.Unlock()
	if n != 0 {
		t.Fatalf("registry size = %d, want 0 after FlushAll", n)
	}
}

// TestDedupe_RestartReRegisters — a later Event after Flush restarts the sweep
// goroutine AND re-registers it, so a subsequent FlushAll still sees and stops
// it. Without the re-registration a restarted sweeper would be invisible to
// FlushAll and leak forever (the CONC-F93 gap the daemon's shutdown FlushAll
// used to miss).
func TestDedupe_RestartReRegisters(t *testing.T) {
	cap1 := &captureSummaries{}
	d := NewDedupe(time.Hour, cap1.add)

	d.Event("a")
	regMu.Lock()
	_, registered := registry[d]
	regMu.Unlock()
	if !registered {
		t.Fatal("first Event must register the sweeper")
	}

	d.Flush() // shutdown-style flush: deregisters and stops the sweeper
	regMu.Lock()
	_, registered = registry[d]
	regMu.Unlock()
	if registered {
		t.Fatal("Flush must deregister the sweeper")
	}

	d.Event("a") // reuse after flush: restarts the sweeper
	regMu.Lock()
	_, registered = registry[d]
	regMu.Unlock()
	if !registered {
		t.Fatal("restarted sweeper must re-register so FlushAll can stop it")
	}

	FlushAll() // must stop the restarted sweeper, or the goroutine leaks
	regMu.Lock()
	_, registered = registry[d]
	regMu.Unlock()
	if registered {
		t.Fatal("FlushAll must deregister the restarted sweeper")
	}
}
