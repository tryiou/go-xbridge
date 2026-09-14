package api

import (
	"testing"
	"time"

	"go-xbridge/swap"
)

// expiryTestOrder builds an order with a controllable Created/Updated/status.
// nowSec is the sweep epoch (seconds); createdAgo/updatedAgo are ages in
// seconds (0 = now).
func expiryTestOrder(seed byte, status string, nowSec uint64, createdAgo, updatedAgo uint64) *Order {
	o := testStoreOrder(seed)
	o.Status = status
	o.Created = (nowSec - createdAgo) * 1000000
	o.Updated = (nowSec - updatedAgo) * 1000000
	return o
}

// TestPruneExpiredCreatedTrNew exercises the store-level age rules for a
// "created" order. In go-xbridge "created" is the in-swap trCreated state
// (xbridgesession.cpp:2174); the node-level sweep keeps live in-swap orders out
// of the prune set via the inSwap session guard (api/node.go pruneExpired), so
// this branch only ever fires for an orphaned "created" order. The bounds
// mirror the C++ APP-side ownership sweep (xbridgeapp.cpp:3604-3634): created
// is treated with the trNew-style bounds — created-age > DeadlineTTL,
// last-activity age > TTL (1 h, not the 6-min pendingTTL), or block-height
// elapsed > BlocksTTL. All comparisons are strict >.
func TestPruneExpiredCreatedTrNew(t *testing.T) {
	now := time.Now()
	nowSec := uint64(now.Unix())
	const tip = uint32(100_000)

	cases := []struct {
		name     string
		created  uint64
		updated  uint64
		blockNum uint32
		cur      uint32
		want     bool
	}{
		{"fresh", 0, 0, tip - 5, tip, false},
		{"deadline boundary exact not expired", swap.DeadlineTTL, 0, tip - 5, tip, false},
		{"deadline +1 expired", swap.DeadlineTTL + 1, 0, tip - 5, tip, true},
		{"inactivity TTL boundary exact not expired", 0, swap.TTL, tip - 5, tip, false},
		{"inactivity TTL +1 expired", 0, swap.TTL + 1, tip - 5, tip, true},
		{"pendingTTL inactivity NOT expired (app-side)", 0, swap.PendingTTL + 1, tip - 5, tip, false},
		{"block boundary exact not expired", 0, 0, tip - swap.BlocksTTL, tip, false},
		{"block +1 expired", 0, 0, tip - swap.BlocksTTL - 1, tip, true},
		{"blockNumber equals tip not expired", 0, 0, tip, tip, false},
		{"blockNumber 0 skips block expiry", 0, 0, 0, tip, false},
		{"currentBlock 0 skips block expiry", 0, 0, 1, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := NewStore()
			o := expiryTestOrder(1, "created", nowSec, tc.created, tc.updated)
			o.BlockNumber = tc.blockNum
			s.Add(o)
			got := s.PruneExpired(now, tc.cur, nil)
			if (len(got) == 1) != tc.want {
				t.Fatalf("PruneExpired = %v, want expired=%v", got, tc.want)
			}
		})
	}
}

// TestPruneExpiredOpenTrPending verifies the trPending ("open") rules: last-
// activity age > PendingTTL or created-age > DeadlineTTL. C++ flips trPending
// to trExpired once inactive for pendingTTL (6 min) (xbridgeapp.cpp:3611-3616)
// and the order book only surfaces trPending (rpcxbridge.cpp:1591), so an
// inactive order leaves the book at the 6 min mark; the deadline erase is
// xbridgeapp.cpp:3630-3634. Block-height expiry NEVER applies past trNew
// (C++ :295-296 short-circuit), even when BlockNumber is very old and the tip
// is far ahead.
func TestPruneExpiredOpenTrPending(t *testing.T) {
	now := time.Now()
	nowSec := uint64(now.Unix())
	const tip = uint32(100_000)

	cases := []struct {
		name     string
		created  uint64
		updated  uint64
		blockNum uint32
		want     bool
	}{
		{"fresh", 0, 0, tip - 5, false},
		{"pendingTTL boundary exact not expired", 0, swap.PendingTTL, tip - 5, false},
		{"pendingTTL +1 expired", 0, swap.PendingTTL + 1, tip - 5, true},
		{"deadline +1 expired", swap.DeadlineTTL + 1, 0, tip - 5, true},
		{"block never applies past trNew", 0, 0, 1, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := NewStore()
			o := expiryTestOrder(1, "open", nowSec, tc.created, tc.updated)
			o.BlockNumber = tc.blockNum
			s.Add(o)
			got := s.PruneExpired(now, tip, nil)
			if (len(got) == 1) != tc.want {
				t.Fatalf("PruneExpired = %v, want expired=%v", got, tc.want)
			}
		})
	}
}

// TestPruneExpiredOpenInactiveWindow is the order-book divergence regression:
// an open order idle for ~20 min — past core's 6 min pendingTTL cutoff but
// inside go's old 1 h TTL window — must be pruned, matching the C++ app-side
// trPending → trExpired flip (xbridgeapp.cpp:3611-3616).
func TestPruneExpiredOpenInactiveWindow(t *testing.T) {
	s := NewStore()
	now := time.Now()
	nowSec := uint64(now.Unix())
	o := expiryTestOrder(1, "open", nowSec, 0, 20*60)
	s.Add(o)
	key := hexEncode(o.ID[:])
	if got := s.PruneExpired(now, 0, nil); len(got) != 1 || got[0] != key {
		t.Fatalf("PruneExpired = %v, want the ~20 min idle order pruned", got)
	}
	if s.Get(key) != nil {
		t.Fatal("idle open order must leave the live book")
	}
}

// TestPruneExpiredSkipsNonOpenBook proves the sweep only ever touches the open
// book ("created"/"open"). In-swap orders ("accepting" and beyond) and terminal
// orders are never pruned, even when very old — they leave the book via their
// own lifecycle paths (C++ eraseExpiredTransactions only sweeps
// m_pendingTransactions).
func TestPruneExpiredSkipsNonOpenBook(t *testing.T) {
	now := time.Now()
	nowSec := uint64(now.Unix())
	for _, status := range []string{
		"accepting", "hold", "initialized", "signed", "commited",
		"finished", "canceled", "rolled back", "rolled back failed", "dropped", "invalid",
	} {
		s := NewStore()
		o := expiryTestOrder(1, status, nowSec, swap.DeadlineTTL+100, swap.TTL+100)
		s.Add(o)
		if got := s.PruneExpired(now, 1_000_000, nil); len(got) != 0 {
			t.Fatalf("status %s pruned = %v, want untouched", status, got)
		}
	}
}

// TestPruneExpiredSkipsPendingPartial proves C++ isOrderPending protection
// (xbridgeapp.cpp:3606-3607): a partial order waiting on its prep-tx split
// (PrepTx set) is never swept, even when its activity age is past the TTLs.
func TestPruneExpiredSkipsPendingPartial(t *testing.T) {
	now := time.Now()
	nowSec := uint64(now.Unix())
	for _, status := range []string{"created", "open"} {
		s := NewStore()
		o := expiryTestOrder(1, status, nowSec, swap.DeadlineTTL+100, swap.TTL+100)
		o.PrepTx = "deadbeef"
		s.Add(o)
		if got := s.PruneExpired(now, 1_000_000, nil); len(got) != 0 {
			t.Fatalf("status %s with PrepTx pruned = %v, want untouched (C++ isOrderPending)", status, got)
		}
	}
}

// TestNodePruneExpired wires the node-level sweep (the engine's 15 s ticker
// calls pruneExpired): an expired open order leaves the live book, an in-swap
// order survives, and the sweep needs no BLOCK connector (cached height 0 →
// time predicates only).
func TestNodePruneExpired(t *testing.T) {
	ctx := newWalletTestCtx()
	now := time.Now()
	nowSec := uint64(now.Unix())

	expired := expiryTestOrder(1, "open", nowSec, 0, swap.PendingTTL+1)
	ctx.Store.Add(expired)
	key := hexEncode(expired.ID[:])

	// In-swap MAKER: the order now starts at "open" (trPending) and advances to
	// hold/initialized/created as the swap drives it (api/swap.go setOrderStatus,
	// mirroring C++ xbridgesession.cpp:1529/1762/2174). The inSwap guard below
	// keys off the session state (past csMaker), which agrees with the order's
	// Status: any session past the initial csMaker is in-swap or finished and
	// must survive; an unmatched maker (still at csMaker) is sweepable.
	inSwap := expiryTestOrder(2, "open", nowSec, 0, swap.TTL+1)
	inSwapKey := hexEncode(inSwap.ID[:])
	ctx.Store.Add(inSwap)
	ctx.Node.sessions = map[string]*SwapSession{
		inSwapKey: {n: ctx.Node, id: inSwap.ID, isMaker: true, state: csHoldApplied},
	}

	ctx.Node.pruneExpired()

	if ctx.Store.Get(key) != nil {
		t.Fatal("expired open order must be pruned by node.pruneExpired")
	}
	if ctx.Store.Get(inSwapKey) == nil {
		t.Fatal("in-swap maker order must survive node.pruneExpired (session past csMaker)")
	}

	// Unmatched maker (session still at the initial csMaker state): sweepable.
	unmatched := expiryTestOrder(3, "open", nowSec, 0, swap.TTL+1)
	unmatchedKey := hexEncode(unmatched.ID[:])
	ctx.Store.Add(unmatched)
	ctx.Node.sessions[unmatchedKey] = &SwapSession{n: ctx.Node, id: unmatched.ID, isMaker: true, state: csMaker}
	ctx.Node.pruneExpired()
	if ctx.Store.Get(unmatchedKey) != nil {
		t.Fatal("unmatched maker order must be pruned (session still at csMaker)")
	}
}

// TestNodePruneExpiredTakerInSwap verifies the taker half of the inSwap guard:
// a taker whose session has advanced past csMaker AND whose order has progressed
// past "open" (e.g. "created" at its deposit broadcast, swap.go applyCreatedB)
// must survive the sweep, while a hub-Rejected taker whose order is restored to
// "open" (session still past csTaker) must be pruned — otherwise the rejected
// order would leak into the book forever.
func TestNodePruneExpiredTakerInSwap(t *testing.T) {
	ctx := newWalletTestCtx()
	now := time.Now()
	nowSec := uint64(now.Unix())

	// In-swap TAKER: order advanced to "created", session past csMaker. The
	// stale Updated (TTL+1) would expire it on the activity predicate alone, so
	// survival proves the guard covers takers too.
	inSwap := expiryTestOrder(4, "created", nowSec, 0, swap.TTL+1)
	inSwapKey := hexEncode(inSwap.ID[:])
	ctx.Store.Add(inSwap)
	ctx.Node.sessions = map[string]*SwapSession{
		inSwapKey: {n: ctx.Node, id: inSwap.ID, isMaker: false, state: csCreatedB},
	}
	ctx.Node.pruneExpired()
	if ctx.Store.Get(inSwapKey) == nil {
		t.Fatal("in-swap taker order must survive node.pruneExpired (session past csMaker, status created)")
	}

	// Rejected TAKER: order restored to "open" (session still past csMaker) must
	// NOT be protected — it must be pruned, not leaked into the book.
	rejected := expiryTestOrder(5, "open", nowSec, 0, swap.TTL+1)
	rejectedKey := hexEncode(rejected.ID[:])
	ctx.Store.Add(rejected)
	ctx.Node.sessions[rejectedKey] = &SwapSession{n: ctx.Node, id: rejected.ID, isMaker: false, state: csCreatedB}
	ctx.Node.pruneExpired()
	if ctx.Store.Get(rejectedKey) != nil {
		t.Fatal("rejected taker order (status open) must be pruned, not leaked by the guard")
	}
}

// TestPruneExpiredInSwapGuard proves the inSwap guard at the store level: an id
// in the inSwap set is never swept, regardless of status/age.
func TestPruneExpiredInSwapGuard(t *testing.T) {
	s := NewStore()
	now := time.Now()
	nowSec := uint64(now.Unix())
	o := expiryTestOrder(1, "open", nowSec, swap.DeadlineTTL+100, swap.TTL+100)
	s.Add(o)
	key := hexEncode(o.ID[:])
	if got := s.PruneExpired(now, 1_000_000, map[string]bool{key: true}); len(got) != 0 {
		t.Fatalf("PruneExpired with inSwap guard = %v, want untouched", got)
	}
}

// TestPruneExpiredWritesHistory locks in the durability rule: every expiry
// records a terminal history entry (status "expired", reason crTimeout) so a
// pruned order stays answerable (HasOrder/HistoryOrder) and survives a
// restart — a finished or stranded order must never silently vanish. This
// deliberately diverges from C++ eraseExpiredTransactions, which erases
// without moveTransactionToHistory; the port keeps the audit trail like its
// finished/cancelled records.
func TestPruneExpiredWritesHistory(t *testing.T) {
	s := NewStore()
	now := time.Now()
	nowSec := uint64(now.Unix())
	o := expiryTestOrder(1, "open", nowSec, 0, swap.TTL+1)
	s.Add(o)
	key := hexEncode(o.ID[:])
	if got := s.PruneExpired(now, 0, nil); len(got) != 1 {
		t.Fatalf("PruneExpired = %v, want 1 removed", got)
	}
	if s.Get(key) != nil {
		t.Fatal("pruned order must leave the live book")
	}
	hist := s.History()
	if len(hist) != 1 {
		t.Fatalf("history = %v, want exactly one expired record", hist)
	}
	if hist[0].ID != key || hist[0].Status != "expired" || hist[0].Reason != uint32(crTimeout) {
		t.Fatalf("history entry = %+v, want id %s status expired reason crTimeout", hist[0], key)
	}
	if hist[0].Order == nil {
		t.Fatal("history entry lost the order snapshot")
	}
	if !s.HasOrder(key) {
		t.Fatal("expired order must remain answerable via history (anti-re-entry guard)")
	}
}

// TestPruneExpiredClockSkew covers a timestamp ahead of now: the age clamps to
// 0, so the order is never "expired" (C++ would compute a negative duration,
// which also fails the strict > check).
func TestPruneExpiredClockSkew(t *testing.T) {
	s := NewStore()
	now := time.Now()
	nowSec := uint64(now.Unix())
	o := expiryTestOrder(1, "open", nowSec, 0, 0)
	o.Updated = (nowSec + 3600) * 1000000 // one hour in the future
	s.Add(o)
	if got := s.PruneExpired(now, 0, nil); len(got) != 0 {
		t.Fatalf("PruneExpired = %v, want untouched for clock skew", got)
	}
}

// TestPruneExpiredReturnsRemovedIDs checks the returned id list reflects the
// removed set (used for the sweep log).
func TestPruneExpiredReturnsRemovedIDs(t *testing.T) {
	s := NewStore()
	now := time.Now()
	nowSec := uint64(now.Unix())
	expired := expiryTestOrder(1, "open", nowSec, 0, swap.TTL+1)
	kept := expiryTestOrder(2, "open", nowSec, 0, 0)
	s.Add(expired)
	s.Add(kept)
	got := s.PruneExpired(now, 0, nil)
	if len(got) != 1 || got[0] != hexEncode(expired.ID[:]) {
		t.Fatalf("PruneExpired = %v, want only the expired order's id", got)
	}
	if s.Get(hexEncode(kept.ID[:])) == nil {
		t.Fatal("fresh order must survive the sweep")
	}
}

// TestPruneExpiredReleasesLockedUtxoContribution proves removing an expired
// order releases its reserved utxo contribution: LockedUtxoInfo derives the
// locked set from live (non-terminal) orders, so a pruned order's inputs no
// longer appear as locked.
func TestPruneExpiredReleasesLockedUtxoContribution(t *testing.T) {
	s := NewStore()
	now := time.Now()
	nowSec := uint64(now.Unix())
	o := expiryTestOrder(1, "open", nowSec, 0, swap.TTL+1)
	s.Add(o)
	keys, _ := s.LockedUtxoInfo()
	if len(keys) == 0 {
		t.Fatal("open order's utxos must be locked before the sweep")
	}
	s.PruneExpired(now, 0, nil)
	keys, _ = s.LockedUtxoInfo()
	if len(keys) != 0 {
		t.Fatalf("locked utxos = %v, want empty after prune (C++ unlockUtxos, xbridgeexchange.cpp:739)", keys)
	}
}
