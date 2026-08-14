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

// TestPruneExpiredCreatedTrNew verifies the trNew ("created") expiry rules:
// created-age > DeadlineTTL, last-activity age > TTL, or block-height elapsed >
// BlocksTTL. The inactivity TTL is 1 h, not the 6-min pendingTTL: the C++ APP
// side (which governs a trader's own orders, xbridgeapp.cpp:3604-3634) only
// transitions trNew to trOffline at pendingTTL and erases it after an hour of
// no activity — the 6-min/block predicates come from the exchange-book sweep
// (xbridgetransaction.cpp:268-311, xbridgeexchange.cpp:712-747). All
// comparisons are strict >.
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
// activity age > TTL or created-age > DeadlineTTL (C++ isExpired for
// state > trNew plus the descriptor deadline erase xbridgeapp.cpp:3630-3634).
// Block-height expiry NEVER applies past trNew (C++ :295-296 short-circuit),
// even when BlockNumber is very old and the tip is far ahead.
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
		{"TTL boundary exact not expired", 0, swap.TTL, tip - 5, false},
		{"TTL +1 expired", 0, swap.TTL + 1, tip - 5, true},
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

	expired := expiryTestOrder(1, "open", nowSec, 0, swap.TTL+1)
	ctx.Store.Add(expired)
	key := hexEncode(expired.ID[:])

	// In-swap MAKER: the store order stays "created" for the whole swap and the
	// handshake never advances its Status, so the session state is what tells
	// the sweep an in-swap order from an unmatched one (C++ advances the
	// descriptor to trHold, xbridgesession.cpp:1529). A maker session past its
	// initial state must protect the order; an unmatched maker session (initial
	// state) must not.
	inSwap := expiryTestOrder(2, "created", nowSec, 0, swap.TTL+1)
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
	unmatched := expiryTestOrder(3, "created", nowSec, 0, swap.TTL+1)
	unmatchedKey := hexEncode(unmatched.ID[:])
	ctx.Store.Add(unmatched)
	ctx.Node.sessions[unmatchedKey] = &SwapSession{n: ctx.Node, id: unmatched.ID, isMaker: true, state: csMaker}
	ctx.Node.pruneExpired()
	if ctx.Store.Get(unmatchedKey) != nil {
		t.Fatal("unmatched maker order must be pruned (session still at csMaker)")
	}
}

// TestPruneExpiredInSwapGuard proves the inSwap guard at the store level: an id
// in the inSwap set is never swept, regardless of status/age.
func TestPruneExpiredInSwapGuard(t *testing.T) {
	s := NewStore()
	now := time.Now()
	nowSec := uint64(now.Unix())
	o := expiryTestOrder(1, "created", nowSec, swap.DeadlineTTL+100, swap.TTL+100)
	s.Add(o)
	key := hexEncode(o.ID[:])
	if got := s.PruneExpired(now, 1_000_000, map[string]bool{key: true}); len(got) != 0 {
		t.Fatalf("PruneExpired with inSwap guard = %v, want untouched", got)
	}
}

// TestPruneExpiredNoHistoryEntry locks in C++ parity: eraseExpiredTransactions
// erases from m_pendingTransactions without moveTransactionToHistory, so a
// pruned order leaves the live book AND does not appear in Store.history.
func TestPruneExpiredNoHistoryEntry(t *testing.T) {
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
	if len(s.History()) != 0 {
		t.Fatalf("history = %v, want empty (C++ eraseExpiredTransactions never moves to history)", s.History())
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
