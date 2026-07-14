// Package swap is a Go port of the XBridge atomic-swap state machine
// (src/xbridge/xbridgetransaction.{h,cpp}). It models the two-party
// Transaction lifecycle: order join, the two-confirmation state progression,
// and terminal/cancellation paths. See docs/swap.md for the canonical spec.
package swap

import "fmt"

// State is a Transaction lifecycle state. Values mirror the C++ Transaction::State
// enum (src/xbridge/xbridgetransaction.h:36-50) so wire/log strings line up.
type State int

const (
	TrInvalid     State = 0
	TrNew         State = 1
	TrJoined      State = 2
	TrHold        State = 3
	TrInitialized State = 4
	TrCreated     State = 5
	TrSigned      State = 6
	TrCommited    State = 7
	TrFinished    State = 8
	TrCancelled   State = 9
	TrDropped     State = 10
)

// strStates mirrors C++ Transaction::strState (xbridgetransaction.cpp:198-201).
var strStates = [...]string{
	"trInvalid", "trNew", "trJoined", "trHold", "trInitialized",
	"trCreated", "trSigned", "trCommited", "trFinished", "trCancelled", "trDropped",
}

// String returns the canonical state name.
func (s State) String() string {
	if s < 0 || int(s) >= len(strStates) {
		return fmt.Sprintf("trUnknown(%d)", int(s))
	}
	return strStates[s]
}

// IsTerminal reports whether the transaction has reached an end state
// (cancelled, finished, or dropped) — C++ Transaction::isFinished().
func (s State) IsTerminal() bool {
	return s == TrFinished || s == TrCancelled || s == TrDropped
}

// IsValid reports whether the transaction is in any state other than trInvalid
// — C++ Transaction::isValid().
func (s State) IsValid() bool { return s != TrInvalid }

// Timing constants (seconds, blocks). From xbridgetransaction.h:54-67.
const (
	// LockTime is the base deposit lock time: 60s * 10min = 600s.
	LockTime = 60 * 10
	// PendingTTL is how long a trNew order stays broadcast before expiry: 6min.
	PendingTTL = 60 * 6
	// TTL is the general transaction TTL once past trNew: 1h.
	TTL = 60 * 60
	// DeadlineTTL is the order-deadline TTL from creation: 7d.
	DeadlineTTL = 60 * 60 * 24 * 7
	// BlocksTTL is the block-height TTL: 1440 blocks/day * 7d = 10080.
	BlocksTTL = 1440 * 7
)
