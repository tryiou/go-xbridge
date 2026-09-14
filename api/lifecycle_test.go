package api

import (
	"encoding/json"
	"testing"

	"go-xbridge/proto"
)

// TestOnFinishedMovesOrderToHistory proves the C++-matching lifecycle: a
// finished swap's order leaves the live book (Store.Get -> nil) and enters
// Store.history as a full snapshot (C++ moveTransactionToHistory), while both
// maker and taker sessions reach csFinished.
func TestOnFinishedMovesOrderToHistory(t *testing.T) {
	ctx := newWalletTestCtx()
	o := seedOrder(ctx)
	idHex := hexEncode(o.ID[:])

	s := &SwapSession{n: ctx.Node, id: o.ID, state: csConfirmedB}
	ctx.Node.sessions = map[string]*SwapSession{idHex: s}
	if _, _, err := s.OnFinished(&proto.FinishedBody{ID: o.ID}); err != nil {
		t.Fatalf("OnFinished: %v", err)
	}

	if ctx.Store.Get(idHex) != nil {
		t.Fatal("finished order must leave the live book")
	}
	h := ctx.Store.History()
	if len(h) != 1 || h[0].ID != idHex || h[0].Status != "finished" {
		t.Fatalf("history = %+v, want one 'finished' entry", h)
	}
	if h[0].Order == nil {
		t.Fatal("history entry must carry the order snapshot")
	}
	if h[0].Order.Status != "finished" || !h[0].Order.Mine {
		t.Fatalf("snapshot = %+v, want status 'finished' Mine order", h[0].Order)
	}
	if s.state != csFinished {
		t.Fatalf("session state = %s, want csFinished", s.state.String())
	}
	if _, ok := ctx.Node.sessions[idHex]; ok {
		t.Fatal("finished session must be pruned")
	}
}

// TestOnFinishedDoubleFinishIsIdempotent covers the hub->both Finished path:
// the hub delivers two Finished packets for the same order id, so the second
// OnFinished must not create a duplicate history entry. Idempotency comes from
// MoveToHistory's missing-order no-op (the first call already removed the live
// order); the duplicate-append guard itself is covered by
// TestMoveToHistoryDuplicateGuarded.
func TestOnFinishedDoubleFinishIsIdempotent(t *testing.T) {
	ctx := newWalletTestCtx()
	o := seedOrder(ctx)
	idHex := hexEncode(o.ID[:])
	s := &SwapSession{n: ctx.Node, id: o.ID, state: csConfirmedB}
	ctx.Node.sessions = map[string]*SwapSession{idHex: s}
	for i := 0; i < 2; i++ {
		if _, _, err := s.OnFinished(&proto.FinishedBody{ID: o.ID}); err != nil {
			t.Fatalf("OnFinished #%d: %v", i, err)
		}
	}
	if len(ctx.Store.History()) != 1 {
		t.Fatalf("history = %+v, want exactly 1 entry", ctx.Store.History())
	}
}

// TestMoveToHistoryMissingOrderNoop proves a MoveToHistory for an id with no
// live order (e.g. a session that never had its order registered, as in the
// forged-Finished regression test) is a harmless no-op.
func TestMoveToHistoryMissingOrderNoop(t *testing.T) {
	ctx := newWalletTestCtx()
	var id [32]byte
	copy(id[:], []byte("missing-order-id-00000000000"))
	ctx.Store.MoveToHistory(hexEncode(id[:]), "finished", 0, NowMicro())
	if len(ctx.Store.History()) != 0 {
		t.Fatalf("history = %+v, want 0 entries", ctx.Store.History())
	}
}

// TestMoveToHistoryDuplicateGuarded proves AddToHistory / MoveToHistory never
// record the same id twice (C++ duplicate guard in moveTransactionToHistory).
func TestMoveToHistoryDuplicateGuarded(t *testing.T) {
	ctx := newWalletTestCtx()
	o := seedOrder(ctx)
	idHex := hexEncode(o.ID[:])
	ctx.Store.AddToHistory(o, "canceled", 1, 10)
	ctx.Store.AddToHistory(o, "canceled", 1, 20)
	if len(ctx.Store.History()) != 1 {
		t.Fatalf("history = %+v, want 1 entry", ctx.Store.History())
	}
	if got := ctx.Store.HistoryOrder(idHex); got == nil || got.Status != "canceled" {
		t.Fatalf("HistoryOrder = %+v, want canceled snapshot", got)
	}
}

// TestSessionIsTerminal covers the prune predicate: live open sessions and
// refund-pending sessions are kept; finished, moved-to-history, terminal-order,
// and refund-done sessions are pruned.
func TestSessionIsTerminal(t *testing.T) {
	ctx := newWalletTestCtx()
	o := seedOrder(ctx) // Mine, Status "open"
	idHex := hexEncode(o.ID[:])

	liveOpen := &SwapSession{n: ctx.Node, id: o.ID, state: csConfirmedB}
	if ctx.Node.sessionIsTerminal(idHex, liveOpen) {
		t.Error("live open session must not be terminal")
	}

	finished := &SwapSession{n: ctx.Node, id: o.ID, state: csFinished}
	if !ctx.Node.sessionIsTerminal(idHex, finished) {
		t.Error("csFinished session must be terminal")
	}

	// Order moved to history -> session has nothing left to do.
	ctx2 := newWalletTestCtx()
	moved := &SwapSession{n: ctx2.Node, id: o.ID, state: csCreatedA}
	ctx2.Node.sessions = map[string]*SwapSession{idHex: moved}
	ctx2.Store.Add(o)
	ctx2.Store.MoveToHistory(idHex, "canceled", 1, NowMicro())
	if !ctx2.Node.sessionIsTerminal(idHex, moved) {
		t.Error("session whose order moved to history must be terminal")
	}

	// Refund still owed: keep even though the order is terminal.
	ctx3 := newWalletTestCtx()
	o3 := seedOrder(ctx3)
	id3 := hexEncode(o3.ID[:])
	ctx3.Store.Update(id3, func(o *Order) { o.Status = "canceled" })
	refundPending := &SwapSession{n: ctx3.Node, id: o3.ID, state: csCreatedA, refundHex: "deadbeef"}
	if ctx3.Node.sessionIsTerminal(id3, refundPending) {
		t.Error("refund-pending session must be kept")
	}
	refundPending.refundDone = true
	if !ctx3.Node.sessionIsTerminal(id3, refundPending) {
		t.Error("refund-done session must be terminal")
	}

	// Rolled-back order stays live but the session prunes once refund is done.
	ctx4 := newWalletTestCtx()
	o4 := seedOrder(ctx4)
	id4 := hexEncode(o4.ID[:])
	ctx4.Store.Update(id4, func(o *Order) { o.Status = "rolled back" })
	rolledBack := &SwapSession{n: ctx4.Node, id: o4.ID, state: csCreatedA, refundHex: "deadbeef", refundDone: true}
	if !ctx4.Node.sessionIsTerminal(id4, rolledBack) {
		t.Error("rolled-back refund-done session must be terminal")
	}
}

// TestPruneSessionsRemovesTerminal verifies pruneSessions drops terminal
// sessions and clears their in-flight refund guard.
func TestPruneSessionsRemovesTerminal(t *testing.T) {
	ctx := newWalletTestCtx()
	o := seedOrder(ctx)
	idHex := hexEncode(o.ID[:])

	live := &SwapSession{n: ctx.Node, id: o.ID, state: csConfirmedB}
	done := &SwapSession{n: ctx.Node, id: o.ID, state: csFinished}
	ctx.Node.sessions = map[string]*SwapSession{idHex: live, "done": done}
	ctx.Node.pendingRefunds = map[string]bool{"done": true}
	ctx.Node.pruneSessions()

	if _, ok := ctx.Node.sessions[idHex]; !ok {
		t.Fatal("live session must survive pruning")
	}
	if _, ok := ctx.Node.sessions["done"]; ok {
		t.Fatal("finished session must be pruned")
	}
	if _, ok := ctx.Node.pendingRefunds["done"]; ok {
		t.Fatal("pruned session's refund guard must be cleared")
	}
}

// TestPersistFinishedRoutesToHistory proves the restart round-trip: a finished
// swap is persisted as a history record and restoreSwap routes it back into
// Store.history (C++ loadOrders -> m_historicTransactions), never the live set.
func TestPersistFinishedRoutesToHistory(t *testing.T) {
	dir := t.TempDir()
	n := newPersistNode(t, dir)

	o := &Order{
		ID:           arr32([]byte("persist-finished-order-id00")),
		FromCurrency: "BTC",
		ToCurrency:   "LTC",
		FromAmount:   1e8,
		ToAmount:     2e8,
		Mine:         true,
		Status:       "finished",
	}
	n.store.AddToHistory(o, "finished", 0, NowMicro())
	n.persist()

	ps, _, _, err := loadSwaps(swapStatePath(dir))
	if err != nil {
		t.Fatalf("loadSwaps: %v", err)
	}
	if len(ps) != 1 || !ps[0].Historical {
		t.Fatalf("loaded = %+v, want 1 historical record", ps)
	}

	n2 := newPersistNode(t, dir)
	for _, p := range ps {
		n2.restoreSwap(p)
	}
	idHex := hexEncode(o.ID[:])
	if n2.store.Get(idHex) != nil {
		t.Fatal("historical record must not re-enter the live book")
	}
	if got := n2.store.HistoryOrder(idHex); got == nil || got.Status != "finished" {
		t.Fatalf("HistoryOrder = %+v, want finished snapshot restored", got)
	}
}

// TestPersistOrderOnlyRestoresLive proves a live order whose session was pruned
// (e.g. rolled-back after refundDone) is persisted order-only and restored live
// without a session.
func TestPersistOrderOnlyRestoresLive(t *testing.T) {
	dir := t.TempDir()
	n := newPersistNode(t, dir)

	o := &Order{
		ID:           arr32([]byte("persist-order-only-order-000")),
		FromCurrency: "BTC",
		ToCurrency:   "LTC",
		FromAmount:   1e8,
		ToAmount:     2e8,
		Mine:         true,
		Status:       "rolled back",
	}
	n.store.Add(o)
	n.persist()

	ps, _, _, err := loadSwaps(swapStatePath(dir))
	if err != nil {
		t.Fatalf("loadSwaps: %v", err)
	}
	if len(ps) != 1 || ps[0].Historical || hasSessionData(ps[0]) {
		t.Fatalf("loaded = %+v, want 1 order-only non-historical record", ps)
	}

	n2 := newPersistNode(t, dir)
	for _, p := range ps {
		n2.restoreSwap(p)
	}
	idHex := hexEncode(o.ID[:])
	if got := n2.store.Get(idHex); got == nil || got.Status != "rolled back" {
		t.Fatalf("restored order = %+v, want live rolled-back order", got)
	}
	if len(n2.sessions) != 0 {
		t.Fatalf("sessions = %d, want 0 (no session data)", len(n2.sessions))
	}
}

// TestDxGetMyOrdersMergesHistory proves finished/cancelled local orders remain
// visible via dxGetMyOrders after they left the live book (C++ dxGetMyOrders
// merges history), and remote history entries stay excluded.
func TestDxGetMyOrdersMergesHistory(t *testing.T) {
	ctx := newWalletTestCtx()
	live := seedOrder(ctx) // Mine, open

	finished := &Order{ID: [32]byte{0x02}, FromCurrency: "BTC", FromAmount: 100,
		ToCurrency: "BTC", ToAmount: 50, Status: "finished", Mine: true, Created: 5, Updated: 5}
	remote := &Order{ID: [32]byte{0x03}, FromCurrency: "BTC", FromAmount: 200,
		ToCurrency: "BTC", ToAmount: 100, Status: "canceled", Mine: false, Created: 6, Updated: 6}
	ctx.Store.AddToHistory(finished, "finished", 0, 5)
	ctx.Store.AddToHistory(remote, "canceled", 1, 6)

	res, err := ctx.dxGetMyOrders(nil)
	if err != nil {
		t.Fatalf("dxGetMyOrders: %v", err)
	}
	arr := res.([]orderDetailResult)
	if len(arr) != 2 {
		t.Fatalf("dxGetMyOrders = %+v, want live + finished history", arr)
	}
	ids := map[string]bool{}
	for _, r := range arr {
		ids[r.ID] = true
	}
	if !ids[dispID(live.ID)] || !ids[dispID(finished.ID)] {
		t.Fatalf("missing expected ids: %+v", arr)
	}
	if ids[dispID(remote.ID)] {
		t.Fatal("remote history entry must not appear in dxGetMyOrders")
	}
}

// TestDxGetOrderHistoryFallback proves dxGetOrder resolves an order that has
// left the live book via the history fallback (C++ App::transaction).
func TestDxGetOrderHistoryFallback(t *testing.T) {
	ctx := newWalletTestCtx()
	o := &Order{ID: [32]byte{0x02}, FromCurrency: "BTC", FromAmount: 100,
		ToCurrency: "BTC", ToAmount: 50, Status: "finished", Mine: true, Created: 5, Updated: 5}
	ctx.Store.AddToHistory(o, "finished", 0, 5)

	res, err := ctx.dxGetOrder([]json.RawMessage{jstr(dispID(o.ID))})
	if err != nil {
		t.Fatalf("dxGetOrder(history): %v", err)
	}
	if lr := res.(orderListResult); lr.Status != "finished" {
		t.Fatalf("dxGetOrder = %+v, want finished order from history", lr)
	}
}

// TestPartialOrderChainResolvesHistoryParent proves a finished parent order in
// history is still found by the partial chain walker (C++ getPartialOrderChain
// reads transactions() + history()).
func TestPartialOrderChainResolvesHistoryParent(t *testing.T) {
	ctx := newWalletTestCtx()
	parent := &Order{ID: [32]byte{0xaa}, FromCurrency: "BTC", FromAmount: 100,
		ToCurrency: "BTC", ToAmount: 50, Status: "finished", Mine: true, Created: 10, Updated: 10,
		PartialAllowed: true} // the chain root is a partial order (partial-chain filter)
	ctx.Store.AddToHistory(parent, "finished", 0, 10)
	child := seedOrder(ctx) // ID {0x01}
	child.ParentID = [32]byte{0xaa}
	child.Created, child.Updated = 20, 20 // created order: parent < child
	ctx.Store.Add(child)

	chain := ctx.partialOrderChain(hexEncode(child.ID[:]))
	if len(chain) != 2 {
		t.Fatalf("chain = %d orders, want 2 (history parent + live child)", len(chain))
	}
	if chain[0].ID != parent.ID || chain[1].ID != child.ID {
		t.Fatalf("chain order wrong: %x -> %x", chain[0].ID, chain[1].ID)
	}
}

// TestPartialOrderChainExcludesRemoteHistoryParent proves a non-local
// (remote) order in history is not resolved as a chain ancestor, matching
// C++ getPartialOrderChain, which walks isLocal() only (unlike the local
// finished-parent case above).
func TestPartialOrderChainExcludesRemoteHistoryParent(t *testing.T) {
	ctx := newWalletTestCtx()
	remote := &Order{ID: [32]byte{0xbb}, FromCurrency: "BTC", FromAmount: 100,
		ToCurrency: "BTC", ToAmount: 50, Status: "canceled", Mine: false, Created: 10, Updated: 10}
	ctx.Store.AddToHistory(remote, "canceled", 1, 10)
	child := seedOrder(ctx) // Mine, ID {0x01}
	child.ParentID = [32]byte{0xbb}
	ctx.Store.Add(child)

	chain := ctx.partialOrderChain(hexEncode(child.ID[:]))
	if len(chain) != 1 {
		t.Fatalf("chain = %d orders, want 1 (remote history parent excluded)", len(chain))
	}
	if chain[0].ID != child.ID {
		t.Fatalf("chain order wrong: %x", chain[0].ID)
	}
}

// TestPruneKeepsSessionWithInFlightDepositTask proves a session whose deposit
// task is still in flight (await true) survives a prune tick even after its
// order moved to history (e.g. a remote cancel landed while the wallet RPC was
// building the deposit). Pruning would orphan the on-chain broadcast and strand
// the deposit; once the apply lands, the refund guard keeps the session.
func TestPruneKeepsSessionWithInFlightDepositTask(t *testing.T) {
	ctx := newWalletTestCtx()
	o := seedOrder(ctx)
	idHex := hexEncode(o.ID[:])
	ctx.Store.MoveToHistory(idHex, "canceled", 1, NowMicro())
	s := &SwapSession{n: ctx.Node, id: o.ID, state: csMaker, await: true}
	ctx.Node.sessions = map[string]*SwapSession{idHex: s}
	ctx.Node.pruneSessions()
	if _, ok := ctx.Node.sessions[idHex]; !ok {
		t.Fatal("session with in-flight deposit task must survive a prune tick")
	}
}

// TestStoreHistoryBounded proves the history cap: exceeding maxStoreHistory
// trims the oldest entries.
func TestStoreHistoryBounded(t *testing.T) {
	ctx := newWalletTestCtx()
	for i := 0; i < maxStoreHistory+50; i++ {
		o := &Order{ID: [32]byte{byte(i % 256), byte(i / 256)}, Status: "canceled", Mine: true}
		ctx.Store.AddToHistory(o, "canceled", 1, uint64(i))
	}
	if got := len(ctx.Store.History()); got != maxStoreHistory {
		t.Fatalf("history len = %d, want %d", got, maxStoreHistory)
	}
}
