// Package swap provides the shared XBridge swap-support types used by
// production code: the HTLC deposit layer (DepositSpec, deposit.go), the
// partial-order price-drift check (PartialOrderDriftCheck, price.go), the
// TransactionDescr::State ordinal map surfaced by the dx* RPCs, and the TTL
// constants consumed by the store expiry logic.
//
// The C++ two-party Transaction state machine (xbridgetransaction.{h,cpp}) is
// the HUB's machine: this thin client never runs it. The live swap driver is
// api.SwapSession/clientState (api/swap.go); that package owns the actual
// handshake progression. The descriptor enum here is what actually reaches the
// wire and the dx* RPCs, so it stays the single source of truth for those
// ordinals (api/response.go derives its map from it).
//
// Ownership contract: this package is PURE — it holds no mutexes and no shared
// mutable state. Values must never be shared across goroutines; the api package
// owns them exclusively on its engine goroutine and passes immutable snapshots
// (swapCtx) to worker goroutines.
package swap

import "fmt"

// ---------------------------------------------------------------------------
// TransactionDescr::State — the client/wire descriptor status enum.
//
// The transaction DESCRIPTOR that is serialized into XBridge packets and
// surfaced by the dx* RPCs uses a richer 16-value enum
// (xbridgetransactiondescr.h:43-61), which includes the rollback states
// (rolled back=10, rollback failed=11) that the C++ exchange machine lacks.
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
