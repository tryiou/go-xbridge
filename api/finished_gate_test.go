package api

import (
	"testing"

	"go-xbridge/proto"
	"go-xbridge/wallet"
)

// H2 (early-Finished strand) proof-first tests. A hub Finished arriving while
// our deposit is still out and neither side has claimed must NOT move the
// order to history (invisible to both refund sweeps) nor prune the session
// that owns the pre-signed refund. It must defer: the session stays live and
// the refund sweep still owns recovery. Legitimate finishes (post-claim,
// counterparty-redeemed, or nothing at risk) still terminate.
//
// C++ terminates unconditionally here
// (processTransactionFinished, xbridgesession.cpp:3851-3904); the deferral is
// a deliberate client-local divergence (no wire change) that closes the
// strand without altering any honest-hub outcome.

// seedDepositOutOrder marks o's deposit as broadcast with a pre-signed refund,
// the post-phase-2 state (applyCreatedA/B set both: swap.go).
func seedDepositOutOrder(ctx *HandlerCtx, o *Order) string {
	idHex := hexEncode(o.ID[:])
	ctx.Store.Update(idHex, func(u *Order) {
		u.DepositSent = true
		u.RefundTx = "deadbeefrefund"
	})
	return idHex
}

// TestEarlyFinishedAtCreatedADefers proves the maker case: Finished at
// csCreatedA with the deposit out and no claim on either side leaves the
// order live and the session unpruned.
func TestEarlyFinishedAtCreatedADefers(t *testing.T) {
	ctx := newWalletTestCtx()
	o := seedOrder(ctx)
	idHex := seedDepositOutOrder(ctx, o)

	s := &SwapSession{
		n: ctx.Node, id: o.ID, isMaker: true,
		state: csCreatedA, srcCur: "BTC",
		ourDepositTxID: "aadeposit", ourLockTime: 90,
		refundHex: "deadbeefrefund",
	}
	ctx.Node.sessions = map[string]*SwapSession{idHex: s}
	if _, _, err := s.OnFinished(&proto.FinishedBody{ID: o.ID}); err != nil {
		t.Fatalf("OnFinished: %v", err)
	}

	if ctx.Store.Get(idHex) == nil {
		t.Fatal("early Finished moved a deposit-out order to history — " +
			"the refund sweeps can never recover it")
	}
	if _, ok := ctx.Node.sessions[idHex]; !ok {
		t.Fatal("early Finished pruned the session owning the pre-signed refund")
	}
	if s.state != csCreatedA {
		t.Fatalf("session state = %s, want csCreatedA (untouched)", s.state.String())
	}
}

// TestEarlyFinishedAtCreatedBTakerDefers proves the taker case.
func TestEarlyFinishedAtCreatedBTakerDefers(t *testing.T) {
	ctx := newWalletTestCtx()
	o := seedOrder(ctx)
	idHex := seedDepositOutOrder(ctx, o)

	s := &SwapSession{
		n: ctx.Node, id: o.ID, isMaker: false,
		state: csCreatedB, srcCur: "BTC",
		ourDepositTxID: "bbdeposit", ourLockTime: 90,
		refundHex: "deadbeefrefund",
	}
	ctx.Node.sessions = map[string]*SwapSession{idHex: s}
	if _, _, err := s.OnFinished(&proto.FinishedBody{ID: o.ID}); err != nil {
		t.Fatalf("OnFinished: %v", err)
	}

	if ctx.Store.Get(idHex) == nil {
		t.Fatal("early Finished moved a deposit-out taker order to history")
	}
	if _, ok := ctx.Node.sessions[idHex]; !ok {
		t.Fatal("early Finished pruned the taker session owning the refund")
	}
}

// TestDeferredFinishedStillRefundsAtLocktime proves the deferral is not a
// stall: after the early Finished, the live session is still swept and the
// pre-signed refund broadcasts once the CLTV releases.
func TestDeferredFinishedStillRefundsAtLocktime(t *testing.T) {
	ctx := newWalletTestCtx()
	o := seedOrder(ctx)
	idHex := seedDepositOutOrder(ctx, o)

	s := &SwapSession{
		n: ctx.Node, id: o.ID, isMaker: true,
		state: csCreatedA, srcCur: "BTC",
		ourDepositTxID: "aadeposit", ourLockTime: 90,
		refundHex: "deadbeefrefund",
	}
	ctx.Node.sessions = map[string]*SwapSession{idHex: s}
	if _, _, err := s.OnFinished(&proto.FinishedBody{ID: o.ID}); err != nil {
		t.Fatalf("OnFinished: %v", err)
	}

	// stubConn GetBlockCount is fixed at 100; lockTime 90 is released.
	ctx.Node.scanRefunds()
	if !s.refundDone {
		t.Fatal("deferred session was not swept: refund never broadcast")
	}
	if got := ctx.Store.Get(idHex); got == nil || got.Status != "rolled back" {
		t.Fatalf("order status = %+v, want live 'rolled back'", got)
	}
}

// TestFinishedAfterLocalClaimTerminates pins the no-regression side: a
// Finished arriving after our own claim broadcast (csFinished, claim tracked,
// counterparty redeemed) still moves to history and prunes.
func TestFinishedAfterLocalClaimTerminates(t *testing.T) {
	ctx := newWalletTestCtx()
	o := seedOrder(ctx)
	idHex := hexEncode(o.ID[:])
	ctx.Store.Update(idHex, func(u *Order) {
		u.DepositSent = true
		u.RefundTx = "deadbeefrefund"
		u.CounterpartyRedeemed = true
	})

	s := &SwapSession{
		n: ctx.Node, id: o.ID, isMaker: true,
		state: csFinished, srcCur: "BTC",
		ourDepositTxID: "aadeposit", ourLockTime: 90,
		refundHex: "deadbeefrefund",
		claimHex:  "claimhex", claimTxID: "claimtxid", claimCur: "BTC",
	}
	ctx.Node.sessions = map[string]*SwapSession{idHex: s}
	if _, _, err := s.OnFinished(&proto.FinishedBody{ID: o.ID}); err != nil {
		t.Fatalf("OnFinished: %v", err)
	}

	if ctx.Store.Get(idHex) != nil {
		t.Fatal("post-claim Finished must leave the live book")
	}
	if len(ctx.Store.History()) != 1 {
		t.Fatalf("history = %+v, want one 'finished' entry", ctx.Store.History())
	}
	if _, ok := ctx.Node.sessions[idHex]; ok {
		t.Fatal("post-claim Finished must prune the session")
	}
}

// TestFinishedWithCounterpartyRedeemedTerminates pins the redeem clause: even
// at a pre-finish state, a redeemed counterparty deposit means our claim is
// done and termination is safe.
func TestFinishedWithCounterpartyRedeemedTerminates(t *testing.T) {
	ctx := newWalletTestCtx()
	o := seedOrder(ctx)
	idHex := seedDepositOutOrder(ctx, o)
	ctx.Store.Update(idHex, func(u *Order) { u.CounterpartyRedeemed = true })

	s := &SwapSession{
		n: ctx.Node, id: o.ID, isMaker: false,
		state: csCreatedB, srcCur: "BTC",
		ourDepositTxID: "bbdeposit", ourLockTime: 90,
		refundHex: "deadbeefrefund",
	}
	ctx.Node.sessions = map[string]*SwapSession{idHex: s}
	if _, _, err := s.OnFinished(&proto.FinishedBody{ID: o.ID}); err != nil {
		t.Fatalf("OnFinished: %v", err)
	}

	if ctx.Store.Get(idHex) != nil {
		t.Fatal("redeemed Finished must leave the live book")
	}
	if _, ok := ctx.Node.sessions[idHex]; ok {
		t.Fatal("redeemed Finished must prune the session")
	}
}

// TestFinishedOnDepositlessOrderTerminates pins C++ parity for the
// nothing-at-risk path: no deposit out, no claim — finishing is harmless
// bookkeeping, not a strand.
func TestFinishedOnDepositlessOrderTerminates(t *testing.T) {
	ctx := newWalletTestCtx()
	o := seedOrder(ctx)
	idHex := hexEncode(o.ID[:])

	s := &SwapSession{n: ctx.Node, id: o.ID, state: csHoldApplied}
	ctx.Node.sessions = map[string]*SwapSession{idHex: s}
	if _, _, err := s.OnFinished(&proto.FinishedBody{ID: o.ID}); err != nil {
		t.Fatalf("OnFinished: %v", err)
	}

	if ctx.Store.Get(idHex) != nil {
		t.Fatal("depositless Finished must leave the live book")
	}
	if _, ok := ctx.Node.sessions[idHex]; ok {
		t.Fatal("depositless Finished must prune the session")
	}
}

// TestRestoreFinishedOrderOnlyRescueStaysSweepable proves the sessionless
// half of the restart rescue: a "finished" record with the deposit out and a
// pre-signed refund but no session data (pruned before persist, the
// order-only shape) still comes back live with a sweepable status. The
// sessionless stored sweep owns it from there (it skips only
// finished/dropped/invalid/rolled-back); no session is rebuilt because there
// are no keys to rebuild it from.
func TestRestoreFinishedOrderOnlyRescueStaysSweepable(t *testing.T) {
	n := newTestNode(t, alignConfs(), map[string]wallet.Connector{})
	var id [32]byte
	oid := hash20("finished-order-only-rescue")
	copy(id[:], oid[:])
	ps := persistedSwap{
		ID:           id,
		FromCurrency: "LTC",
		ToCurrency:   "BTC",
		Status:       "finished",
		State:        csIdle, // order-only persist: no session survived
		Role:         'B',
		DepositSent:  true,
		RefundTx:     "deadbeefrefund",
		RefundDone:   false,
		UtxoCurrency: "LTC",
	}
	n.restoreSwap(ps)
	idHex := hexEncode(id[:])
	o := n.store.Get(idHex)
	if o == nil {
		t.Fatal("finished order-only record was filed into history — " +
			"scanStoredRefunds can never see it")
	}
	if o.Status != "created" {
		t.Fatalf("rescued order status = %q, want created (sweepable)", o.Status)
	}
	if s := n.sessions[idHex]; s != nil {
		t.Fatal("order-only rescue must not fabricate a session without keys")
	}
}

// TestRestoreGenuineFinishStaysHistory pins the rescue predicate's negative
// side: a "finished" record WITH claim evidence (local claim tracked and
// counterparty redeemed) is genuinely done and must route to history, not
// back to live. Companion to TestRestoreHistoricalFinishedStaysHistory,
// which covers the minimal record.
func TestRestoreGenuineFinishStaysHistory(t *testing.T) {
	n := newTestNode(t, alignConfs(), map[string]wallet.Connector{})
	var id [32]byte
	oid := hash20("genuine-finish-stays-history")
	copy(id[:], oid[:])
	ps := persistedSwap{
		ID:                   id,
		FromCurrency:         "LTC",
		ToCurrency:           "BTC",
		Status:               "finished",
		State:                csFinished,
		Role:                 'A',
		IsMaker:              true,
		DepositSent:          true,
		RefundTx:             "deadbeefrefund",
		RefundDone:           false,
		CounterpartyRedeemed: true,
		PrivKey:              [32]byte{0x11},
		SrcCur:               "LTC",
		OurLockTime:          900,
		OurDepositTxID:       "aadeposit",
		RefundHex:            "deadbeefrefund",
		ClaimTxID:            "claimtxid",
		ClaimHex:             "claimhex",
		ClaimCur:             "BTC",
		Historical:           true,
	}
	n.restoreSwap(ps)
	if o := n.store.Get(hexEncode(id[:])); o != nil {
		t.Fatal("genuinely finished record was resurrected live — " +
			"completed swaps must stay in history")
	}
	if len(n.sessions) != 0 {
		t.Fatal("genuinely finished record must not rebuild a session")
	}
}

// records stranded by the pre-gate behavior: a "finished" record with the
// deposit out, a pre-signed refund, and no claim evidence on either side must
// come back live (session state demoted to the deposit-broadcast state) so
// the sweep can still recover it. A genuinely finished record (claim tracked)
// still routes to history.
func TestRestoreFinishedWithDepositOwedRestoresLive(t *testing.T) {
	n := newTestNode(t, alignConfs(), map[string]wallet.Connector{})
	var id [32]byte
	oid := hash20("finished-deposit-owed")
	copy(id[:], oid[:])
	ps := persistedSwap{
		ID:                   id,
		FromCurrency:         "LTC",
		ToCurrency:           "BTC",
		Status:               "finished",
		State:                csFinished,
		Role:                 'A',
		IsMaker:              true,
		DepositSent:          true,
		RefundTx:             "deadbeefrefund",
		RefundDone:           false,
		CounterpartyRedeemed: false,
		PrivKey:              [32]byte{0x11},
		SrcCur:               "LTC",
		OurLockTime:          900,
		OurDepositTxID:       "aadeposit",
		RefundHex:            "deadbeefrefund",
	}
	n.restoreSwap(ps)
	idHex := hexEncode(id[:])
	o := n.store.Get(idHex)
	if o == nil {
		t.Fatal("finished-with-deposit-owed record was filed into history — " +
			"the sweep can never recover the deposit")
	}
	s := n.sessions[idHex]
	if s == nil {
		t.Fatal("no live session rebuilt for the owed refund")
	}
	if s.state != csCreatedA {
		t.Fatalf("restored session state = %s, want csCreatedA", s.state.String())
	}
}
