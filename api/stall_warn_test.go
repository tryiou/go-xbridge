package api

import "testing"

// Tests for the early stall warning (watchStalledSessions' WARN phase): a
// live session silent past sessionStallWarnMicro must be reported once per
// silent period — with no side effects (no cancel, no refund) — and the
// reported state must reset when progress resumes. The 30-minute cancel
// behavior is covered by TestStalledSessionCancelled.

// TestStallEarlyWarnFiresOncePerSilence proves the warn phase records the
// silence without acting on it, never re-warns for the same silence, and
// re-arms for a new silence.
func TestStallEarlyWarnFiresOncePerSilence(t *testing.T) {
	n, sc, idHex, conn := stallSetup(t, csCreatedA, func(s *SwapSession, _ *Order) {
		s.lastProgress = uint64(NowMicro()) - sessionStallWarnMicro - 1
	})
	progress := n.sessions[idHex].lastProgress

	// First sweep: warn recorded, nothing sent.
	n.watchStalledSessions()
	if got := n.stallWarned[idHex]; got != progress {
		t.Fatalf("stallWarned = %d, want %d (warn not recorded)", got, progress)
	}
	if sc.sent != 0 || len(conn.broadcasts) != 0 {
		t.Fatalf("early warn must not act: sent=%d broadcasts=%v", sc.sent, conn.broadcasts)
	}
	if o := n.store.Get(idHex); o.Status != "created" {
		t.Fatalf("status = %q, want created (warn must not mutate)", o.Status)
	}

	// Second sweep on the same silence: no re-warn, still no action.
	n.watchStalledSessions()
	if got := n.stallWarned[idHex]; got != progress {
		t.Fatalf("stallWarned changed on re-sweep: %d, want %d", got, progress)
	}

	// A new silence (progress advanced, then stalled again): re-armed.
	progress2 := progress - 60*1000000
	n.sessions[idHex].lastProgress = progress2
	n.watchStalledSessions()
	if got := n.stallWarned[idHex]; got != progress2 {
		t.Fatalf("stallWarned = %d, want %d (new silence not re-warned)", got, progress2)
	}
	if sc.sent != 0 {
		t.Fatalf("sent = %d, want 0 (still below cancel threshold)", sc.sent)
	}
}

// TestStallEarlyWarnClearedOnProgress proves a resumed session drops its warn
// entry, so a later stall warns again instead of being permanently muted.
func TestStallEarlyWarnClearedOnProgress(t *testing.T) {
	n, _, idHex, _ := stallSetup(t, csCreatedA, func(s *SwapSession, _ *Order) {
		s.lastProgress = uint64(NowMicro()) - sessionStallWarnMicro - 1
	})
	n.watchStalledSessions()
	if _, ok := n.stallWarned[idHex]; !ok {
		t.Fatal("warn entry missing after silent sweep")
	}
	n.sessions[idHex].lastProgress = uint64(NowMicro()) // progress resumed
	n.watchStalledSessions()
	if _, ok := n.stallWarned[idHex]; ok {
		t.Fatal("warn entry must be dropped once progress resumes")
	}
}

// TestStallEarlyWarnSkipsFresh proves sessions below the warn threshold never
// enter the warn map (the map only tracks reported silences).
func TestStallEarlyWarnSkipsFresh(t *testing.T) {
	n, _, idHex, _ := stallSetup(t, csCreatedA, func(s *SwapSession, _ *Order) {
		s.lastProgress = uint64(NowMicro())
	})
	n.watchStalledSessions()
	if len(n.stallWarned) != 0 {
		t.Fatalf("stallWarned = %v, want empty for a fresh session", n.stallWarned)
	}
	if _, ok := n.sessions[idHex]; !ok {
		t.Fatal("session must survive a fresh sweep")
	}
}

// TestStallWarnEntryPrunedWithSession proves the warn entry does not outlive
// its session: a session that terminates (or is pruned) while still silent
// must drop its stallWarned entry with it — watchStalledSessions iterates
// n.sessions, so an orphaned entry is unreachable and would accumulate over
// long uptime.
func TestStallWarnEntryPrunedWithSession(t *testing.T) {
	n, _, idHex, _ := stallSetup(t, csCreatedA, func(s *SwapSession, _ *Order) {
		s.lastProgress = uint64(NowMicro()) - sessionStallWarnMicro - 1
	})
	n.watchStalledSessions()
	if _, ok := n.stallWarned[idHex]; !ok {
		t.Fatal("warn entry missing after silent sweep")
	}
	n.sessions[idHex].state = csFinished // terminal without progress resuming
	n.pruneSessions()
	if _, ok := n.sessions[idHex]; ok {
		t.Fatal("terminal session must be pruned")
	}
	if _, ok := n.stallWarned[idHex]; ok {
		t.Fatal("stallWarned entry must be dropped with its pruned session")
	}
}
