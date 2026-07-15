package swap

import (
	"testing"
	"time"
)

// joinedForSession builds a maker/taker pair and joins them, returning the
// maker-side joined Transaction plus fixed (deterministic) 33-byte pubkeys.
func joinedForSession() (*Transaction, [33]byte, [33]byte) {
	mk := NewTransaction([32]byte{9}, "BTC", "LTC", 100, 200,
		Member{Source: Addr{0x01}, Dest: Addr{0x02}}, false, 0, time.Unix(1000, 0))
	tk := NewTransaction([32]byte{8}, "LTC", "BTC", 200, 100,
		Member{Source: Addr{0x03}, Dest: Addr{0x04}}, false, 0, time.Unix(1000, 0))
	if !mk.TryJoin(tk) {
		panic("join failed in test fixture")
	}
	var lp, op [33]byte
	lp[0], op[0] = 0x02, 0x03
	return mk, lp, op
}

// TestSessionDepositGatesProgression checks that the two deposits CONFIRMING
// (not merely being created) is what advances the gate from trJoined -> trHold,
// mirroring C++'s increaseStateCounter(trJoined, fromSource) for each member.
func TestSessionDepositGatesProgression(t *testing.T) {
	t2, lp, op := joinedForSession()
	s := NewSession(t2, RoleMaker, lp, op)
	if s.T.State != TrJoined {
		t.Fatalf("state = %s, want trJoined", s.T.State)
	}

	// Create + adopt deposits; gate must stay trJoined until both confirm.
	if _, err := s.CreateLocalDeposit(600); err != nil {
		t.Fatalf("CreateLocalDeposit: %v", err)
	}
	var sh [20]byte
	sh[0] = 0xaa
	s.AdoptCounterparty(sh, 600)
	if s.T.State != TrJoined {
		t.Fatalf("state = %s, want trJoined before confirmation", s.T.State)
	}

	// Local confirms only -> still trJoined (needs the counterparty too).
	s.ConfirmLocalDeposit()
	if s.T.State != TrJoined {
		t.Fatalf("state = %s, want trJoined after local-only confirm", s.T.State)
	}

	// Counterparty confirms -> gate advances to trHold.
	s.ConfirmOtherDeposit()
	if s.T.State != TrHold {
		t.Fatalf("state = %s, want trHold after both confirm", s.T.State)
	}
}

// TestSessionLocalDepositAmount checks the local node locks the right side's
// amount/currency for the maker role (SourceAmount/SourceCurrency).
func TestSessionLocalDepositAmount(t *testing.T) {
	t2, lp, op := joinedForSession()
	s := NewSession(t2, RoleMaker, lp, op)
	spec, err := s.CreateLocalDeposit(600)
	if err != nil {
		t.Fatalf("CreateLocalDeposit: %v", err)
	}
	if spec.Amount != 100 || spec.Currency != "BTC" {
		t.Fatalf("maker deposit = %d %s, want 100 BTC", spec.Amount, spec.Currency)
	}
	if s.Other == nil {
		// Adopt the counterparty to validate its mirrored amount.
		var sh [20]byte
		s.AdoptCounterparty(sh, 600)
	}
	if s.Other.Amount != 200 || s.Other.Currency != "LTC" {
		t.Fatalf("taker deposit = %d %s, want 200 LTC", s.Other.Amount, s.Other.Currency)
	}
}
