package log

import (
	"sync"
	"time"
)

// Dedupe suppresses repeated log lines that carry the same key, emitting the
// first occurrence and then staying quiet until the key's quiet window elapses,
// at which point it logs a single summary line and resets.
//
// It is meant for high-frequency, low-value events that still represent a real
// condition (e.g. per-peer dial failures, repeated cancels for orders we do not
// track): the first sighting is always reported, repeats are collapsed so they
// never bury the log, and a periodic summary keeps the underlying issue visible.
//
// Unlike a single-slot deduplicator, Dedupe keeps an independent bucket per key,
// so interleaved distinct keys no longer clobber each other. A key relayed many
// times by many peers collapses into one first-sighting plus periodic
// "count=N over=DURATION" summaries whose elapsed reflects true time since the
// key was first seen (never 0s).
//
// Dedupe is safe for concurrent use. A nil *Dedupe is a no-op, so callers
// may keep a package-level pointer without guarding every call.
type Dedupe struct {
	mu        sync.Mutex
	m         map[string]*bucket
	quietFor  time.Duration
	summarize func(key string, total int, elapsed time.Duration)

	// stopCh is the live stop channel for the current sweep goroutine (nil when
	// no sweep is running). It is recreated on each (re)start so a Flush never
	// permanently disables later Events — dedup keeps summarizing after a
	// shutdown-triggered flush.
	stopCh chan struct{}
	// sweepOnce guards lazy start of the sweep goroutine.
	sweepOnce sync.Once
	// sweepStarted reports whether the sweep goroutine is live (under mu).
	sweepStarted bool
}

// bucket tracks one key's recent activity.
type bucket struct {
	first   time.Time // first sighting in the current active window
	last    time.Time // most recent event (drives quiet reset + idle eviction)
	count   int       // events since the last emitted summary
	lastSum time.Time // when we last emitted a summary for this key
}

// registry holds every live *Dedupe so FlushAll can flush them on shutdown.
var (
	regMu    sync.Mutex
	registry = map[*Dedupe]struct{}{}
)

// pendingSummary captures the arguments of a summarize call so it can be
// invoked after the lock is released (the callback may re-enter Dedupe).
type pendingSummary struct {
	key     string
	count   int
	elapsed time.Duration
}

// NewDedupe returns a *Dedupe that suppresses repeats of the same key for
// quietFor, then reports a summary via summarize (may be nil). The collapsed
// key is passed to summarize so callers can identify what was deduplicated.
func NewDedupe(quietFor time.Duration, summarize func(key string, total int, elapsed time.Duration)) *Dedupe {
	d := &Dedupe{
		m:         make(map[string]*bucket),
		quietFor:  quietFor,
		summarize: summarize,
	}
	regMu.Lock()
	registry[d] = struct{}{}
	regMu.Unlock()
	return d
}

// Event reports that key occurred. It returns true when this is the first
// sighting of key (or the first after its quiet window elapsed) and the caller
// should log it normally; false means the event was suppressed.
func (d *Dedupe) Event(key string) (first bool) {
	if d == nil {
		return true
	}
	d.mu.Lock()

	now := time.Now()
	b := d.m[key]

	// A summary for the previous bucket, when the old window is being closed. It
	// is invoked after the lock is released to avoid a deadlock if summarize
	// re-enters Dedupe.
	var pending *pendingSummary
	if b == nil || now.Sub(b.first) >= d.quietFor {
		// New key, or the previous window went quiet: flush the old bucket's
		// pending summary before opening a fresh one.
		if b != nil && b.count > 1 && d.summarize != nil {
			pending = &pendingSummary{key: key, count: b.count, elapsed: now.Sub(b.first)}
		}
		d.m[key] = &bucket{
			first:   now,
			last:    now,
			count:   1,
			lastSum: now,
		}
		d.startSweepLocked()
		d.mu.Unlock()
		if pending != nil {
			d.summarize(pending.key, pending.count, pending.elapsed)
		}
		return true
	}

	b.count++
	b.last = now
	d.mu.Unlock()
	return false
}

// startSweepLocked lazily starts the sweep goroutine. Caller must hold d.mu.
func (d *Dedupe) startSweepLocked() {
	if d.sweepStarted {
		return
	}
	d.sweepStarted = true
	interval := d.quietFor / 2
	if interval <= 0 {
		interval = time.Second
	}
	d.stopCh = make(chan struct{})
	go d.sweep(d.stopCh, interval)
}

// sweep periodically emits summaries for buckets that have accumulated repeats
// and evicts idle one-shot buckets. It returns when stopCh is closed.
func (d *Dedupe) sweep(stopCh chan struct{}, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-stopCh:
			return
		case <-ticker.C:
			d.tick()
		}
	}
}

// tick performs a single sweep pass. Exported for deterministic tests.
func (d *Dedupe) tick() {
	if d == nil {
		return
	}
	d.mu.Lock()
	if !d.sweepStarted {
		d.mu.Unlock()
		return
	}
	now := time.Now()
	idleCutoff := d.quietFor * 2
	var pending []*pendingSummary
	for key, b := range d.m {
		if now.Sub(b.last) > idleCutoff {
			// Idle bucket: emit any pending summary (nothing hidden), then evict
			// so one-shot keys do not grow the map without bound.
			if b.count > 1 && d.summarize != nil {
				pending = append(pending, &pendingSummary{key: key, count: b.count, elapsed: now.Sub(b.first)})
			}
			delete(d.m, key)
			continue
		}
		if b.count > 1 && d.summarize != nil && now.Sub(b.lastSum) >= d.quietFor/2 {
			pending = append(pending, &pendingSummary{key: key, count: b.count, elapsed: now.Sub(b.first)})
			b.count = 0
			b.lastSum = now
		}
	}
	d.mu.Unlock()
	for _, p := range pending {
		d.summarize(p.key, p.count, p.elapsed)
	}
}

// Flush forces any pending summary to be emitted (e.g. on shutdown) and stops
// the sweep goroutine. It is idempotent (safe to call twice, and safe against
// concurrent Event calls): a later Event restarts the sweeper on its own, so
// dedup is not permanently muted by a shutdown-triggered flush.
func (d *Dedupe) Flush() {
	if d == nil {
		return
	}
	regMu.Lock()
	delete(registry, d)
	regMu.Unlock()
	d.mu.Lock()
	if d.stopCh != nil {
		close(d.stopCh)
		d.stopCh = nil
	}
	d.sweepStarted = false
	now := time.Now()
	var pending []*pendingSummary
	for key, b := range d.m {
		if b.count > 1 && d.summarize != nil {
			pending = append(pending, &pendingSummary{key: key, count: b.count, elapsed: now.Sub(b.first)})
		}
		delete(d.m, key)
	}
	d.mu.Unlock()
	for _, p := range pending {
		d.summarize(p.key, p.count, p.elapsed)
	}
}

// FlushAll flushes every live Dedupe (used on daemon shutdown so pending
// summaries are not lost).
func FlushAll() {
	regMu.Lock()
	all := make([]*Dedupe, 0, len(registry))
	for d := range registry {
		all = append(all, d)
	}
	regMu.Unlock()
	for _, d := range all {
		d.Flush()
	}
}
