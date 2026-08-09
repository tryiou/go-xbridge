// Package swap is a Go port of the XBridge atomic-swap state machine
// (src/xbridge/xbridgetransaction.{h,cpp}). It models the two-party
// Transaction lifecycle: order join, the two-confirmation state progression,
// and terminal/cancellation paths. See docs/architecture.md for the canonical
// spec.
//
// Ownership contract: this package is PURE — it holds no mutexes and no shared
// mutable state. Session/Transaction values must never be shared across
// goroutines; the api package owns them exclusively on its engine goroutine and
// passes immutable snapshots (swapCtx) to worker goroutines, which never touch a
// live Session/Transaction.
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

// ---------------------------------------------------------------------------
// TransactionDescr::State — the client/wire descriptor status enum.
//
// The exchange-side machine above uses the 11-value Transaction::State
// (xbridgetransaction.h). The transaction DESCRIPTOR that is serialized into
// XBridge packets and surfaced by the dx* RPCs uses a richer 16-value enum
// (xbridgetransactiondescr.h:43-61), which includes the rollback states
// (rolled back=10, rollback failed=11) that the exchange machine lacks. The
// previous port omitted this enum entirely, so a transaction in a rollback
// state was unknowable to the swap layer and the dxCancelOrder "cannot cancel
// once state >= trCreated" guard could not reason about it.
//
// This DescrState is the single source of truth for those 16 ordinals and their
// canonical names (xbridgetransactiondescr.h:665-688 strState); api/response.go
// derives its ordinal map from it so the two layers cannot drift.
// ---------------------------------------------------------------------------

// DescrState is xbridge::TransactionDescr::State (xbridgetransactiondescr.h).
type DescrState int

const (
	DescrExpired        DescrState = -1
	DescrNew            DescrState = 0
	DescrOffline        DescrState = 1
	DescrOpen           DescrState = 2 // C++ trPending
	DescrAccepting      DescrState = 3
	DescrHold           DescrState = 4
	DescrInitialized    DescrState = 5
	DescrCreated        DescrState = 6
	DescrSigned         DescrState = 7
	DescrCommited       DescrState = 8
	DescrFinished       DescrState = 9
	DescrRollback       DescrState = 10
	DescrRollbackFailed DescrState = 11
	DescrDropped        DescrState = 12
	DescrCancelled      DescrState = 13
	DescrInvalid        DescrState = 14
)

// descrStateNames maps the canonical TransactionDescr::strState name to its
// ordinal, mirroring xbridgetransactiondescr.h:665-688.
var descrStateNames = map[string]DescrState{
	"expired":         DescrExpired,
	"new":             DescrNew,
	"offline":         DescrOffline,
	"open":            DescrOpen,
	"accepting":       DescrAccepting,
	"hold":            DescrHold,
	"initialized":     DescrInitialized,
	"created":         DescrCreated,
	"signed":          DescrSigned,
	"commited":        DescrCommited,
	"finished":        DescrFinished,
	"rolled back":     DescrRollback,
	"rollback failed": DescrRollbackFailed,
	"dropped":         DescrDropped,
	"canceled":        DescrCancelled,
	"invalid":         DescrInvalid,
}

// String returns the canonical TransactionDescr::State name (strState).
func (s DescrState) String() string {
	switch s {
	case DescrExpired:
		return "expired"
	case DescrNew:
		return "new"
	case DescrOffline:
		return "offline"
	case DescrOpen:
		return "open"
	case DescrAccepting:
		return "accepting"
	case DescrHold:
		return "hold"
	case DescrInitialized:
		return "initialized"
	case DescrCreated:
		return "created"
	case DescrSigned:
		return "signed"
	case DescrCommited:
		return "commited"
	case DescrFinished:
		return "finished"
	case DescrRollback:
		return "rolled back"
	case DescrRollbackFailed:
		return "rollback failed"
	case DescrDropped:
		return "dropped"
	case DescrCancelled:
		return "canceled"
	case DescrInvalid:
		return "invalid"
	default:
		return fmt.Sprintf("descrState(%d)", int(s))
	}
}

// DescrStateFromName returns the DescrState for a canonical status name.
func DescrStateFromName(name string) (DescrState, bool) {
	s, ok := descrStateNames[name]
	return s, ok
}

// DescrStateOrdinal returns the TransactionDescr::State ordinal for a canonical
// status name, or 0 (trNew) if the name is unknown. Matches the C++ enum
// ordering used by dxCancelOrder's "cannot cancel once state >= trCreated" guard.
func DescrStateOrdinal(name string) int {
	if s, ok := descrStateNames[name]; ok {
		return int(s)
	}
	return 0
}

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
