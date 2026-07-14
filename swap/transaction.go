package swap

import "time"

// Addr is a 20-byte (uint160) address as carried on the XBridge wire
// (proto "20-byte address" field). It is the public identity a participant
// confirms state transitions with.
type Addr [20]byte

// Member is one side of a swap (role A = maker, B = taker). Source is the
// address the participant sends FROM; Dest is the address they receive TO.
// Mirrors XBridgeTransactionMember (src/xbridge/xbridgetransactionmember.h).
type Member struct {
	Source Addr
	Dest   Addr
}

// Transaction is a two-party atomic-swap order, ported from C++
// xbridge::Transaction. A is the maker (order creator); B is the taker, filled
// in by TryJoin. Source/Dest currency+amount describe the maker's view: the
// maker gives SourceCurrency:SourceAmount and receives DestCurrency:DestAmount.
type Transaction struct {
	ID        [32]byte
	CreatedAt int64 // unix seconds
	LastAt    int64 // unix seconds, updated on each state change

	// Maker view of the order.
	SourceCurrency string
	DestCurrency   string
	SourceAmount   uint64
	DestAmount     uint64

	PartialAllowed bool
	MinFromAmount  uint64

	A Member
	B Member

	State State

	// changedA/changedB are the per-phase confirmation flags. C++ reuses a
	// single pair across every phase (reset after each transition), so we do
	// the same.
	changedA bool
	changedB bool
}

// NewTransaction builds a trNew maker order. now is the creation time; pass
// time.Now() in production (kept injectable for tests).
func NewTransaction(id [32]byte, srcCur, dstCur string, srcAmt, dstAmt uint64,
	a Member, partial bool, minFrom uint64, now time.Time) *Transaction {
	t := now.Unix()
	return &Transaction{
		ID:             id,
		CreatedAt:      t,
		LastAt:         t,
		SourceCurrency: srcCur,
		DestCurrency:   dstCur,
		SourceAmount:   srcAmt,
		DestAmount:     dstAmt,
		PartialAllowed: partial,
		MinFromAmount:  minFrom,
		A:              a,
		State:          TrNew,
	}
}

// tryJoinMatches reports whether a taker order (o) is compatible with this maker
// order for joining. Extracted from Transaction::tryJoin
// (xbridgetransaction.cpp:495-540).
func (t *Transaction) tryJoinMatches(o *Transaction) bool {
	if t.State != TrNew || o.State != TrNew {
		return false
	}
	if t.SourceCurrency != o.DestCurrency || t.DestCurrency != o.SourceCurrency {
		return false
	}
	if t.PartialAllowed != o.PartialAllowed {
		return false
	}
	if !t.PartialAllowed {
		if t.SourceAmount != o.DestAmount || t.DestAmount != o.SourceAmount {
			return false
		}
		return true
	}
	// Partial: taker wants no more than the maker offers, and gives no less
	// than the maker asks; the taker's receive amount must clear the maker's
	// minimum. (C++ also applies xBridgePartialOrderDriftCheck for price
	// tolerance; we require an exact price match for determinism — see
	// docs/swap.md §Join.)
	if t.SourceAmount < o.DestAmount || t.DestAmount < o.SourceAmount {
		return false
	}
	if o.DestAmount < t.MinFromAmount {
		return false
	}
	return true
}

// TryJoin joins a taker order (o) to this maker order. On success B becomes the
// taker's member (C++: m_b = other->m_a) and the state advances to trJoined.
// Returns false if the orders are incompatible. Ports Transaction::tryJoin.
func (t *Transaction) TryJoin(o *Transaction) bool {
	if !t.tryJoinMatches(o) {
		return false
	}
	t.B = o.A
	t.State = TrJoined
	t.touch()
	return true
}

// IncreaseStateCounter advances the state machine when a confirmation arrives
// from participant `from`. Each of the four progression phases requires BOTH
// members to confirm (A then B, in either order) before advancing:
//
//	trJoined      → (both via Source)      → trHold
//	trHold        → (both via Dest)        → trInitialized
//	trInitialized → (both via Source)      → trCreated
//	trCreated     → (both via Dest)        → trFinished
//
// If state != current state, or the phase is unhandled, it returns trInvalid.
// Ports Transaction::increaseStateCounter (xbridgetransaction.cpp:113-187).
func (t *Transaction) IncreaseStateCounter(state State, from Addr) State {
	if state != t.State {
		return TrInvalid
	}
	switch state {
	case TrJoined:
		t.mark(from, t.A.Source, t.B.Source)
		if t.changedA && t.changedB {
			t.State = TrHold
			t.reset()
		}
	case TrHold:
		t.mark(from, t.A.Dest, t.B.Dest)
		if t.changedA && t.changedB {
			t.State = TrInitialized
			t.reset()
		}
	case TrInitialized:
		t.mark(from, t.A.Source, t.B.Source)
		if t.changedA && t.changedB {
			t.State = TrCreated
			t.reset()
		}
	case TrCreated:
		t.mark(from, t.A.Dest, t.B.Dest)
		if t.changedA && t.changedB {
			t.State = TrFinished
			t.reset()
		}
	default:
		return TrInvalid
	}
	return t.State
}

// mark sets the changedA/changedB flags if `from` matches a participant's
// expected address for the current phase.
func (t *Transaction) mark(from, a, b Addr) {
	if from == a {
		t.changedA = true
	} else if from == b {
		t.changedB = true
	}
}

func (t *Transaction) reset() { t.changedA, t.changedB = false, false }

// touch updates LastAt to now.
func (t *Transaction) touch() { t.LastAt = time.Now().Unix() }

// Cancel moves the transaction to trCancelled (C++ Transaction::cancel).
func (t *Transaction) Cancel() {
	t.State = TrCancelled
	t.touch()
}

// Drop moves the transaction to trDropped (C++ Transaction::drop).
func (t *Transaction) Drop() {
	t.State = TrDropped
	t.touch()
}

// Finish moves the transaction to trFinished (C++ Transaction::finish).
func (t *Transaction) Finish() {
	t.State = TrFinished
	t.touch()
}

// IsFinished reports a terminal state (C++ Transaction::isFinished).
func (t *Transaction) IsFinished() bool { return t.State.IsTerminal() }

// IsValid reports a non-invalid state (C++ Transaction::isValid).
func (t *Transaction) IsValid() bool { return t.State.IsValid() }

// IsExpired reports whether the transaction has exceeded its TTL for its
// current state (C++ Transaction::isExpired). now is the current time.
func (t *Transaction) IsExpired(now time.Time) bool {
	n := now.Unix()
	ageCreated := n - t.CreatedAt
	ageLast := n - t.LastAt
	if t.State == TrNew {
		return ageCreated > DeadlineTTL || ageLast > PendingTTL
	}
	return t.State > TrNew && ageLast > TTL
}
