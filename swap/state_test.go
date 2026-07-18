package swap

import "testing"

// TestDescrStateOrdinals verifies the 16-value TransactionDescr::State enum
// (xbridgetransactiondescr.h:43-61) round-trips: each canonical name maps to
// its ordinal via DescrStateFromName / DescrStateOrdinal, and DescrState.String
// (strState) returns the canonical name. This is C3 — the previous port omitted
// this enum, so rolled-back transactions were unknowable to the swap layer.
func TestDescrStateOrdinals(t *testing.T) {
	cases := []struct {
		name string
		st   DescrState
	}{
		{"expired", DescrExpired},
		{"new", DescrNew},
		{"offline", DescrOffline},
		{"open", DescrOpen},
		{"accepting", DescrAccepting},
		{"hold", DescrHold},
		{"initialized", DescrInitialized},
		{"created", DescrCreated},
		{"signed", DescrSigned},
		{"commited", DescrCommited},
		{"finished", DescrFinished},
		{"rolled back", DescrRollback},
		{"rollback failed", DescrRollbackFailed},
		{"dropped", DescrDropped},
		{"canceled", DescrCancelled},
		{"invalid", DescrInvalid},
	}
	for _, c := range cases {
		if got := c.st.String(); got != c.name {
			t.Errorf("DescrState(%d).String() = %q, want %q", int(c.st), got, c.name)
		}
		if s, ok := DescrStateFromName(c.name); !ok || s != c.st {
			t.Errorf("DescrStateFromName(%q) = (%d, %v), want (%d, true)", c.name, int(s), ok, int(c.st))
		}
		if o := DescrStateOrdinal(c.name); o != int(c.st) {
			t.Errorf("DescrStateOrdinal(%q) = %d, want %d", c.name, o, int(c.st))
		}
	}
}

// TestDescrStateRollbackRepresentable checks that the rollback states are
// representable and ordered strictly after trCreated (ordinal 6), so the
// dxCancelOrder "cannot cancel once state >= trCreated" guard (api/response.go
// stateOrdinal) can correctly reason about a rolled-back transaction.
func TestDescrStateRollbackRepresentable(t *testing.T) {
	if DescrRollback <= DescrCreated {
		t.Errorf("DescrRollback (%d) must be > DescrCreated (%d)", int(DescrRollback), int(DescrCreated))
	}
	if DescrRollbackFailed <= DescrCreated {
		t.Errorf("DescrRollbackFailed (%d) must be > DescrCreated (%d)", int(DescrRollbackFailed), int(DescrCreated))
	}
	if DescrExpired >= DescrCreated {
		t.Errorf("DescrExpired (%d) must be < DescrCreated (%d)", int(DescrExpired), int(DescrCreated))
	}
}
