package swap

import (
	"testing"
	"time"
)

var (
	aMakerSrc = Addr{0x01}
	aMakerDst = Addr{0x02}
	aTakerSrc = Addr{0x03}
	aTakerDst = Addr{0x04}
	aUnknown  = Addr{0xee}
)

func makerOrder() *Transaction {
	var id [32]byte
	copy(id[:], []byte("maker-order-id-00000000000000000000"))
	return NewTransaction(id, "BTC", "LTC", 100, 200,
		Member{Source: aMakerSrc, Dest: aMakerDst}, false, 0, time.Unix(1000, 0))
}

func takerOrder() *Transaction {
	var id [32]byte
	copy(id[:], []byte("taker-order-id-00000000000000000000"))
	// Complementary: gives LTC (maker's dest), wants BTC (maker's source).
	return NewTransaction(id, "LTC", "BTC", 200, 100,
		Member{Source: aTakerSrc, Dest: aTakerDst}, false, 0, time.Unix(1000, 0))
}

// TestTryJoin covers the complementary-match, currency-mismatch, amount-mismatch,
// and wrong-state cases of Transaction::tryJoin.
func TestTryJoin(t *testing.T) {
	mk := makerOrder()
	tk := takerOrder()

	if !mk.TryJoin(tk) {
		t.Fatal("complementary orders should join")
	}
	if mk.State != TrJoined {
		t.Errorf("state after join = %s, want trJoined", mk.State)
	}
	// B becomes the taker's member A.
	if mk.B.Source != aTakerSrc || mk.B.Dest != aTakerDst {
		t.Errorf("member B not taken from taker: %+v", mk.B)
	}

	// Currency mismatch.
	mk2 := makerOrder()
	badCur := takerOrder()
	badCur.SourceCurrency, badCur.DestCurrency = "DOGE", "BTC"
	if mk2.TryJoin(badCur) {
		t.Error("currency-mismatched orders should not join")
	}

	// Amount mismatch (non-partial requires exact amounts).
	mk3 := makerOrder()
	badAmt := takerOrder()
	badAmt.DestAmount = 99 // maker wants 100 BTC
	if mk3.TryJoin(badAmt) {
		t.Error("amount-mismatched orders should not join")
	}

	// Cannot join from a non-trNew order.
	mk4 := makerOrder()
	tk4 := takerOrder()
	tk4.State = TrJoined
	if mk4.TryJoin(tk4) {
		t.Error("joining a non-trNew order should fail")
	}
}

// TestTryJoinPartial covers the partial-order price-integrity / drift
// check (C++ xBridgePartialOrderDriftCheck, ported in swap/price.go).
// A taker may join when its amounts clear the maker's quoted price
// within a satoshi-level drift band, not only on an exact match —
// the behaviour corenet dapps depend on.
func TestTryJoinPartial(t *testing.T) {
	// Maker: 1 BTC (source) -> 200 LTC (dest), partial, min 10.
	mk := NewTransaction([32]byte{1}, "BTC", "LTC", 100, 200,
		Member{Source: aMakerSrc, Dest: aMakerDst}, true, 10, time.Unix(1000, 0))

	// Taker: gives LTC (maker dest), wants BTC (maker source).
	taker := func(src, dst uint64) *Transaction {
		id := [32]byte{2}
		return NewTransaction(id, "LTC", "BTC", src, dst,
			Member{Source: aTakerSrc, Dest: aTakerDst}, true, 0, time.Unix(1000, 0))
	}

	// Exact complementary amounts join.
	if !mk.TryJoin(taker(200, 100)) {
		t.Error("exact partial amounts should join")
	}

	// Drift-tolerant: taker's receive (100) is within the satoshi
	// band of the maker's quoted price, so it must join (this is
	// what the old exact-match check incorrectly rejected).
	mk2 := NewTransaction([32]byte{3}, "BTC", "LTC", 100, 200,
		Member{Source: aMakerSrc, Dest: aMakerDst}, true, 10, time.Unix(1000, 0))
	if !mk2.TryJoin(taker(199, 100)) {
		t.Error("within-drift partial (199 LTC for 100 BTC) should join")
	}

	// Below the maker's minimum partial size -> rejected.
	mk3 := NewTransaction([32]byte{4}, "BTC", "LTC", 100, 200,
		Member{Source: aMakerSrc, Dest: aMakerDst}, true, 50, time.Unix(1000, 0))
	if mk3.TryJoin(taker(49, 100)) {
		t.Error("taker below maker minPartial should not join")
	}

	// Out-of-drift: taker takes far too little / mismatched price -> rejected.
	mk4 := NewTransaction([32]byte{5}, "BTC", "LTC", 100, 200,
		Member{Source: aMakerSrc, Dest: aMakerDst}, true, 10, time.Unix(1000, 0))
	if mk4.TryJoin(taker(150, 100)) {
		t.Error("out-of-drift partial should not join")
	}

	// Taker wants more than the maker offers -> rejected (bounds).
	mk5 := NewTransaction([32]byte{6}, "BTC", "LTC", 100, 200,
		Member{Source: aMakerSrc, Dest: aMakerDst}, true, 10, time.Unix(1000, 0))
	if mk5.TryJoin(taker(200, 101)) {
		t.Error("taker wanting more than maker source should not join")
	}
}

// TestIncreaseStateCounterProgression walks both participants through every
// phase and asserts the state advances only after the second confirmation.
func TestIncreaseStateCounterProgression(t *testing.T) {
	mk := makerOrder()
	if !mk.TryJoin(takerOrder()) {
		t.Fatal("join failed")
	}

	// Phase trJoined -> trHold (both via Source).
	if s := mk.IncreaseStateCounter(TrJoined, aMakerSrc); s != TrJoined {
		t.Fatalf("after A's joined-confirm: %s, want trJoined", s)
	}
	if s := mk.IncreaseStateCounter(TrJoined, aTakerSrc); s != TrHold {
		t.Fatalf("after B's joined-confirm: %s, want trHold", s)
	}

	// Phase trHold -> trInitialized (both via Dest).
	if s := mk.IncreaseStateCounter(TrHold, aMakerDst); s != TrHold {
		t.Fatalf("after A's hold-confirm: %s, want trHold", s)
	}
	if s := mk.IncreaseStateCounter(TrHold, aTakerDst); s != TrInitialized {
		t.Fatalf("after B's hold-confirm: %s, want trInitialized", s)
	}

	// Phase trInitialized -> trCreated (both via Source).
	if s := mk.IncreaseStateCounter(TrInitialized, aMakerSrc); s != TrInitialized {
		t.Fatalf("after A's init-confirm: %s, want trInitialized", s)
	}
	if s := mk.IncreaseStateCounter(TrInitialized, aTakerSrc); s != TrCreated {
		t.Fatalf("after B's init-confirm: %s, want trCreated", s)
	}

	// Phase trCreated -> trFinished (both via Dest).
	if s := mk.IncreaseStateCounter(TrCreated, aMakerDst); s != TrCreated {
		t.Fatalf("after A's created-confirm: %s, want trCreated", s)
	}
	if s := mk.IncreaseStateCounter(TrCreated, aTakerDst); s != TrFinished {
		t.Fatalf("after B's created-confirm: %s, want trFinished", s)
	}
	if !mk.IsFinished() {
		t.Error("transaction should be finished")
	}
}

// TestIncreaseStateCounterNoopInvalid checks that an unrecognized confirmer is
// a no-op, and that confirming the wrong phase returns trInvalid.
func TestIncreaseStateCounterNoopInvalid(t *testing.T) {
	mk := makerOrder()
	if !mk.TryJoin(takerOrder()) {
		t.Fatal("join failed")
	}
	// Unknown confirmer: no-op, stays trJoined, returns trJoined.
	if s := mk.IncreaseStateCounter(TrJoined, aUnknown); s != TrJoined {
		t.Errorf("unknown confirmer returned %s, want trJoined", s)
	}
	if mk.State != TrJoined {
		t.Errorf("unknown confirmer must not advance state, got %s", mk.State)
	}
	// Confirming a phase we are not in returns trInvalid and leaves state.
	if s := mk.IncreaseStateCounter(TrHold, aMakerDst); s != TrInvalid {
		t.Errorf("wrong-phase confirm returned %s, want trInvalid", s)
	}
	if mk.State != TrJoined {
		t.Errorf("wrong-phase confirm must not change state, got %s", mk.State)
	}
}

// TestIsExpired checks the trNew (deadline/pending) and post-trNew (TTL) rules.
func TestIsExpired(t *testing.T) {
	now := time.Unix(1_000_000, 0)

	// Fresh trNew order (created just now) must not be expired.
	mk := NewTransaction([32]byte{}, "BTC", "LTC", 100, 200,
		Member{Source: aMakerSrc, Dest: aMakerDst}, false, 0, now)
	if mk.IsExpired(now) {
		t.Error("fresh trNew order must not be expired")
	}

	// trNew idle past pendingTTL -> expired.
	idle := NewTransaction([32]byte{}, "BTC", "LTC", 100, 200,
		Member{Source: aMakerSrc, Dest: aMakerDst}, false, 0, now)
	idle.LastAt = now.Add(-(PendingTTL + 10) * time.Second).Unix()
	if !idle.IsExpired(now) {
		t.Error("trNew idle past pendingTTL should be expired")
	}

	// trNew past creation deadline -> expired.
	stale := NewTransaction([32]byte{}, "BTC", "LTC", 100, 200,
		Member{Source: aMakerSrc, Dest: aMakerDst}, false, 0, now)
	stale.CreatedAt = now.Add(-(DeadlineTTL + 10) * time.Second).Unix()
	if !stale.IsExpired(now) {
		t.Error("trNew past deadlineTTL should be expired")
	}

	// Post-trNew, idle past TTL -> expired.
	joined := makerOrder()
	joined.TryJoin(takerOrder())
	joined.LastAt = now.Add(-(TTL + 10) * time.Second).Unix()
	if !joined.IsExpired(now) {
		t.Error("post-trNew idle past TTL should be expired")
	}
	joined.LastAt = now.Add(-(TTL - 10) * time.Second).Unix()
	if joined.IsExpired(now) {
		t.Error("post-trNew within TTL must not be expired")
	}
}

// TestIsExpiredByBlockNumber checks the block-height expiry variant: a trNew
// order expires once the chain advances more than BlocksTTL beyond the block it
// was created at, and stays valid within that window. Post-trNew delegates to
// the time-based TTL.
func TestIsExpiredByBlockNumber(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	const createdAtBlock = 1_000_000

	mk := NewTransaction([32]byte{}, "BTC", "LTC", 100, 200,
		Member{Source: aMakerSrc, Dest: aMakerDst}, false, 0, now)
	mk.BlockNumber = createdAtBlock

	// Within the block window: not expired.
	if mk.IsExpiredByBlockNumber(createdAtBlock) {
		t.Error("trNew within block window must not be expired (same block)")
	}
	if mk.IsExpiredByBlockNumber(createdAtBlock + BlocksTTL - 1) {
		t.Error("trNew just inside BlocksTTL must not be expired")
	}
	// One block past the window: expired.
	if !mk.IsExpiredByBlockNumber(createdAtBlock + BlocksTTL + 1) {
		t.Error("trNew past BlocksTTL must be expired")
	}

	// Post-trNew delegates to the time-based TTL (block height ignored), so an
	// order that is fresh by time is not expired by this check either.
	joined := makerOrder()
	joined.TryJoin(takerOrder())
	joined.BlockNumber = createdAtBlock
	joined.LastAt = time.Now().Unix()
	if joined.IsExpiredByBlockNumber(createdAtBlock + BlocksTTL + 1) {
		t.Error("post-trNew fresh by time must not be expired by block check")
	}
	// And an order idle past the time TTL is expired even with a low block.
	joined.LastAt = time.Now().Add(-(TTL + 10) * time.Second).Unix()
	if !joined.IsExpiredByBlockNumber(createdAtBlock) {
		t.Error("post-trNew idle past time TTL must be expired")
	}
}
