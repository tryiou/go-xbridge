package api

import (
	"errors"
	"testing"

	"go-xbridge/config"
	"go-xbridge/proto"
	"go-xbridge/wallet"
)

// H1 Phase 1 (secret-hunt) proof-first tests. When our own deposit outpoint
// is already spent — the counterparty claimed, so the secret is public
// on-chain — a failed refund broadcast must arm a secret hunt, not mark
// "rollback failed": the refund is impossible (input gone), the mempool
// watch owns recovery, and the session must survive. Backend blindness (the
// deposit tx unknown to the wallet) must NOT hunt — it keeps the current
// retry behavior.
//
// Discriminator (mirrors the C++ spend-vs-unknown distinction the thin
// client can still observe): deposit tx KNOWN with confirmations>=0 plus
// gettxout null == spent; deposit tx UNKNOWN == backend cannot see, retry.

// huntFixture builds a taker session at csCreatedB with the deposit proven
// out (DepositSent + order RefundTx) and a failing refund broadcast. The
// caller decides what the stub wallet knows about the deposit tx.
func huntFixture(t *testing.T, depTx string, depKnown bool) (*HandlerCtx, *SwapSession, *stubConn, string) {
	t.Helper()
	ctx := newWalletTestCtx()
	o := seedOrder(ctx)
	idHex := hexEncode(o.ID[:])
	ctx.Store.Update(idHex, func(u *Order) {
		u.DepositSent = true
		u.RefundTx = "deadbeefrefund"
		u.BinTxId = depTx
	})
	conn := ctx.Node.config.Connectors["BTC"].(*stubConn)
	conn.sendErr = errors.New("broadcast refused: input already spent")
	if depKnown {
		conn.verboseTx = map[string]wallet.VerboseTx{
			depTx: {TxID: depTx, Confirmations: 5},
		}
	}
	s := &SwapSession{
		n: ctx.Node, id: o.ID, isMaker: false,
		state: csCreatedB, srcCur: "BTC",
		ourDepositTxID: depTx, ourLockTime: 90,
		refundHex:       "deadbeefrefund",
		theirSecretHash: hash20("secret-hash"),
	}
	ctx.Node.sessions = map[string]*SwapSession{idHex: s}
	return ctx, s, conn, idHex
}

// TestRefundFailureOnSpentDepositArmsHunt proves the core H1 Phase 1 claim:
// refund fails, deposit provably spent → hunt armed, no "rollback failed",
// session and order survive with status untouched.
func TestRefundFailureOnSpentDepositArmsHunt(t *testing.T) {
	ctx, s, _, idHex := huntFixture(t, "aadeposit", true)

	// stubConn GetBlockCount is fixed at 100; lockTime 90 is released.
	ctx.Node.scanRefunds()

	if !s.secretHunt {
		t.Fatal("spent own deposit did not arm the secret hunt")
	}
	if got := ctx.Store.Get(idHex); got == nil || got.Status == "rollback failed" {
		t.Fatalf("order = %+v, want live and never failure-marked", got)
	}
	if _, ok := ctx.Node.sessions[idHex]; !ok {
		t.Fatal("hunt session was pruned")
	}
}

// TestRefundFailureOnBlindBackendKeepsRetry proves the discriminator's safe
// side: deposit tx unknown to the wallet → current behavior preserved
// ("rollback failed" + backoff retry), never a hunt.
func TestRefundFailureOnBlindBackendKeepsRetry(t *testing.T) {
	ctx, s, _, idHex := huntFixture(t, "bbdeposit", false)

	ctx.Node.scanRefunds()

	if s.secretHunt {
		t.Fatal("backend blindness misclassified as a spent deposit")
	}
	if got := ctx.Store.Get(idHex); got == nil || got.Status != "rollback failed" {
		t.Fatalf("order = %+v, want live 'rollback failed'", got)
	}
}

// TestHuntSessionSkipsSweepAndWatchdog proves a hunted session is left alone
// to recover: the refund sweep never re-attempts the impossible broadcast,
// and the stall watchdog never cancels it into a force-refund.
func TestHuntSessionSkipsSweepAndWatchdog(t *testing.T) {
	ctx, s, _, idHex := huntFixture(t, "ccdeposit", true)

	ctx.Node.scanRefunds()
	if !s.secretHunt {
		t.Fatal("setup: hunt not armed")
	}

	// Second sweep: no re-attempt (refundDone stays false AND the status is
	// untouched — any attempt would fail through to "rollback failed").
	ctx.Node.scanRefunds()
	if s.refundDone {
		t.Fatal("sweep broadcast an impossible refund on a hunt session")
	}
	if got := ctx.Store.Get(idHex); got == nil || got.Status == "rollback failed" {
		t.Fatalf("order = %+v, want untouched by the second sweep", got)
	}

	// Stale watchdog clock past the 30-minute cancel threshold: the hunt
	// session must not cancel (cancellation force-broadcasts the same
	// impossible refund and would destroy the hunt).
	s.lastProgress = uint64(NowMicro()) - sessionStallMicro - 1
	ctx.Node.watchStalledSessions()
	if _, ok := ctx.Node.sessions[idHex]; !ok {
		t.Fatal("watchdog reaped a hunting session")
	}
	if got := ctx.Store.Get(idHex); got == nil || got.Status == "canceled" {
		t.Fatalf("order = %+v, want never canceled while hunting", got)
	}
}

// TestRestoreRearmsSecretHunt proves the hunt survives a restart: the flag
// rides the session record and the restored session keeps hunting instead
// of failing its first post-restart refund.
func TestRestoreRearmsSecretHunt(t *testing.T) {
	n := newTestNode(t, alignConfs(), map[string]wallet.Connector{})
	var id [32]byte
	oid := hash20("hunt-restores-live")
	copy(id[:], oid[:])
	ps := persistedSwap{
		ID:             id,
		FromCurrency:   "LTC",
		ToCurrency:     "BTC",
		Status:         "created",
		State:          csCreatedB,
		Role:           'B',
		IsMaker:        false,
		DepositSent:    true,
		RefundTx:       "deadbeefrefund",
		RefundDone:     false,
		PrivKey:        [32]byte{0x11},
		SrcCur:         "LTC",
		OurLockTime:    900,
		OurDepositTxID: "dddeposit",
		RefundHex:      "deadbeefrefund",
		SecretHunt:     true,
	}
	n.restoreSwap(ps)
	idHex := hexEncode(id[:])
	s := n.sessions[idHex]
	if s == nil {
		t.Fatal("hunt record did not restore a live session")
	}
	if !s.secretHunt {
		t.Fatal("restored session lost the hunt flag")
	}
}

// TestHuntedSessionRecoversViaMempoolWatch proves a hunted session still
// recovers through the deposit watch: with the maker's payTx visible in the
// mempool, the watch re-drives ConfirmB and the taker claims to csFinished.
// The hunt flag must never gate recovery — it only stands down the refund.
func TestHuntedSessionRecoversViaMempoolWatch(t *testing.T) {
	_, takerNode, _, takerSession, _, orderID, makerPayTxID, tkLtcConn, mkLtcConn := blindTakerFixture(t)
	idHex := hexEncode(orderID[:])
	takerSession.secretHunt = true

	hx, err := mkLtcConn.GetRawTransaction(makerPayTxID)
	if err != nil {
		t.Fatal("maker payTx missing from maker connector")
	}
	tkLtcConn.setRawTx(makerPayTxID, hx)
	tkLtcConn.mempoolTxids = []string{makerPayTxID}

	takerNode.watchOwnDepositSpends()

	if takerSession.secret == [33]byte{} {
		t.Fatal("hunted session did not recover the secret from the mempool spend")
	}
	if takerSession.state != csFinished {
		t.Fatalf("hunted session state = %s, want csFinished", takerSession.state.String())
	}
	if o := takerNode.store.Get(idHex); o == nil || !o.CounterpartyRedeemed {
		t.Fatalf("order = %+v, want counterparty redeemed", o)
	}
	// Terminal prune carries the flag away with the session: no residue.
	takerNode.pruneSessions()
	if _, ok := takerNode.sessions[idHex]; ok {
		t.Fatal("recovered session was not pruned")
	}
}

// stubFundedTxID is the newWalletTestCtx stub's funded UTXO id
// (wallet_methods_test.go): the only outpoint its GetTxOut reports present.
// Tests needing an affirmative-unspent signal point their deposit here and
// say so; if the stub's funding ever changes, these tests fail loudly at
// the classification they pin instead of silently passing.
const stubFundedTxID = "0000000000000000000000000000000000000000000000000000000000000000"

// TestHuntRevalidationClearsOnUnspent proves the hunt is bounded: when the
// deposit outpoint reads unspent again (deep reorg corrected a false
// positive), the flag clears and the refund path resumes.
func TestHuntRevalidationClearsOnUnspent(t *testing.T) {
	ctx := newWalletTestCtx()
	o := seedOrder(ctx)
	idHex := hexEncode(o.ID[:])
	ctx.Store.Update(idHex, func(u *Order) {
		u.DepositSent = true
		u.RefundTx = "deadbeefrefund"
	})
	conn := ctx.Node.config.Connectors["BTC"].(*stubConn)
	// Deposit tx known AND outpoint present: affirmative unspent (see
	// stubFundedTxID above for the coupling).
	depTx := stubFundedTxID
	conn.verboseTx = map[string]wallet.VerboseTx{
		depTx: {TxID: depTx, Confirmations: 3},
	}
	s := &SwapSession{
		n: ctx.Node, id: o.ID, isMaker: false,
		state: csCreatedB, srcCur: "BTC",
		ourDepositTxID: depTx, ourLockTime: 90,
		refundHex:       "deadbeefrefund",
		theirSecretHash: hash20("secret-hash"),
		secretHunt:      true,
		huntSince:       uint64(NowMicro()) - 3600*1000000,
	}
	ctx.Node.sessions = map[string]*SwapSession{idHex: s}
	_ = conn

	ctx.Node.watchOwnDepositSpends()

	if s.secretHunt {
		t.Fatal("hunt did not clear after the deposit read unspent again")
	}
	// Refund path resumes: past locktime the next sweep broadcasts.
	ctx.Node.scanRefunds()
	if !s.refundDone {
		t.Fatal("cleared hunt did not resume the refund path")
	}
}

// TestHuntRevalidationHoldsOnBlindness proves the conservative direction:
// when the backend cannot see the deposit tx at all, the hunt persists
// (never flaps back into doomed refund retries on missing evidence).
func TestHuntRevalidationHoldsOnBlindness(t *testing.T) {
	ctx := newWalletTestCtx()
	o := seedOrder(ctx)
	idHex := hexEncode(o.ID[:])
	s := &SwapSession{
		n: ctx.Node, id: o.ID, isMaker: false,
		state: csCreatedB, srcCur: "BTC",
		ourDepositTxID: "eedeposit", ourLockTime: 90,
		refundHex:       "deadbeefrefund",
		theirSecretHash: hash20("secret-hash"),
		secretHunt:      true,
		huntSince:       uint64(NowMicro()) - 3600*1000000,
	}
	ctx.Node.sessions = map[string]*SwapSession{idHex: s}

	ctx.Node.watchOwnDepositSpends()

	if !s.secretHunt {
		t.Fatal("blind backend cleared the hunt on missing evidence")
	}
}

// TestManualRefundOnHuntedSessionReportsSpent proves the done!=nil branch:
// a manual BroadcastRefund on a spent deposit reports the typed cause
// instead of a wallet reject or a false success.
func TestManualRefundOnHuntedSessionReportsSpent(t *testing.T) {
	ctx, s, _, idHex := huntFixture(t, "ffdeposit", true)

	var doneErr error
	done := func(_ string, err error) { doneErr = err }
	ctx.Node.postRefundTask(idHex, "BTC", s.refundHex, s.ourLockTime, true, done)

	if !errors.Is(doneErr, errRefundDepositSpent) {
		t.Fatalf("manual refund err = %v, want errRefundDepositSpent", doneErr)
	}
	if !s.secretHunt {
		t.Fatal("manual spent-refund did not arm the hunt")
	}
}

// TestRefundOnChainBeatsSpentClassification pins the precedence: when the
// refund itself is already confirmed on-chain AND the deposit reads spent,
// the known success wins — the refund won the race, no hunt.
func TestRefundOnChainBeatsSpentClassification(t *testing.T) {
	n, s, conn := setupSwapPair(t)
	idHex := hexEncode(s.id[:])
	if _, _, err := s.OnCreateA(&proto.CreateABody{HubAddress: s.hub, ID: s.id, BPubKey: to33(s.pubKey[:])}); err != nil {
		t.Fatalf("OnCreateA build: %v", err)
	}
	refundID, err := txIDFromHex(s.refundHex)
	if err != nil {
		t.Fatalf("refund hex: %v", err)
	}
	// Refund confirmed on-chain (visible to the wallet) while the deposit
	// independently reads spent: the refund won, classify success.
	conn.rawTx[refundID] = s.refundHex
	conn.verboseTx = map[string]wallet.VerboseTx{
		s.ourDepositTxID: {TxID: s.ourDepositTxID, Confirmations: 5},
	}
	conn.blockHeight = int64(s.ourLockTime) + 1

	n.scanRefunds()

	if s.secretHunt {
		t.Fatal("confirmed refund misclassified as a spent deposit")
	}
	if !s.refundDone {
		t.Fatal("confirmed refund not reconciled")
	}
	if o := n.store.Get(idHex); o == nil || o.Status != "rolled back" {
		t.Fatalf("order = %+v, want live 'rolled back'", o)
	}
}

// TestMakerSpentDepositNeverHunts proves the arming gate: a spent maker
// deposit without local finish contradicts the protocol (the taker can only
// spend after the maker claimed, which finishes the maker) — so makers keep
// the loud legacy path (rollback failed + retry) and never hunt, a state
// with no owned recovery.
func TestMakerSpentDepositNeverHunts(t *testing.T) {
	ctx := newWalletTestCtx()
	o := seedOrder(ctx)
	idHex := hexEncode(o.ID[:])
	ctx.Store.Update(idHex, func(u *Order) {
		u.DepositSent = true
		u.RefundTx = "deadbeefrefund"
		u.BinTxId = "mmdeposit"
	})
	conn := ctx.Node.config.Connectors["BTC"].(*stubConn)
	conn.sendErr = errors.New("broadcast refused: input already spent")
	conn.verboseTx = map[string]wallet.VerboseTx{
		"mmdeposit": {TxID: "mmdeposit", Confirmations: 5},
	}
	s := &SwapSession{
		n: ctx.Node, id: o.ID, isMaker: true,
		state: csCreatedA, srcCur: "BTC",
		ourDepositTxID: "mmdeposit", ourLockTime: 90,
		refundHex:       "deadbeefrefund",
		theirSecretHash: hash20("secret-hash"),
	}
	ctx.Node.sessions = map[string]*SwapSession{idHex: s}

	ctx.Node.scanRefunds()

	if s.secretHunt {
		t.Fatal("maker session hunted a protocol-contradictory spent deposit")
	}
	if got := ctx.Store.Get(idHex); got == nil || got.Status != "rollback failed" {
		t.Fatalf("order = %+v, want live 'rollback failed' (loud legacy path)", got)
	}
}

// TestHuntedPreCreatedTakerRecoversViaWatch proves the crash-recovered shape:
// a hunted taker parked below csCreatedB with the deposit proven out
// (DepositSent) still recovers through the deposit watch — the csCreatedB
// state gate is only a proxy for "deposit out," which the probe already
// proved, so it must not exclude hunting sessions.
func TestHuntedPreCreatedTakerRecoversViaWatch(t *testing.T) {
	_, takerNode, _, takerSession, _, orderID, makerPayTxID, tkLtcConn, mkLtcConn := blindTakerFixture(t)
	idHex := hexEncode(orderID[:])
	// Simulate the crash-recovered pre-created session: deposit proven
	// broadcast, state never advanced, hunt armed by a failed refund.
	takerSession.state = csInitialized
	takerSession.secretHunt = true
	takerSession.huntSince = uint64(NowMicro())
	hx, err := mkLtcConn.GetRawTransaction(makerPayTxID)
	if err != nil {
		t.Fatal("maker payTx missing from maker connector")
	}
	tkLtcConn.setRawTx(makerPayTxID, hx)
	tkLtcConn.mempoolTxids = []string{makerPayTxID}

	takerNode.watchOwnDepositSpends()

	if takerSession.secret == [33]byte{} {
		t.Fatal("pre-created hunted session did not recover from the mempool spend")
	}
	if takerSession.state != csFinished {
		t.Fatalf("state = %s, want csFinished", takerSession.state.String())
	}
	if o := takerNode.store.Get(idHex); o == nil || !o.CounterpartyRedeemed {
		t.Fatalf("order = %+v, want counterparty redeemed", o)
	}
}

// TestFailedRedriveKeepsHuntAndSchedulesRetry pins the residue semantics: a
// hunt is spent-deposit mode, not secret-seeking — so a ConfirmB re-drive
// that finds the spend but fails the claim broadcast must KEEP the hunt
// (the refund is still impossible) and schedule the claim retry, never
// strand silently and never resume doomed refunds.
func TestFailedRedriveKeepsHuntAndSchedulesRetry(t *testing.T) {
	_, takerNode, _, takerSession, _, _, makerPayTxID, tkLtcConn, mkLtcConn := blindTakerFixture(t)
	takerSession.secretHunt = true
	takerSession.huntSince = uint64(NowMicro())
	hx, err := mkLtcConn.GetRawTransaction(makerPayTxID)
	if err != nil {
		t.Fatal("maker payTx missing from maker connector")
	}
	tkLtcConn.setRawTx(makerPayTxID, hx)
	tkLtcConn.mempoolTxids = []string{makerPayTxID}
	// The claim builds (secret adopted) but its broadcast dies. The claim
	// spends the maker's deposit on the taker's DESTINATION chain (BTC),
	// not the own-deposit chain — fail that connector.
	tkBtcConn, ok := takerNode.config.Connectors["BTC"].(*fakeConnector)
	if !ok {
		t.Fatal("taker BTC connector not a fake")
	}
	tkBtcConn.sendErr = errors.New("claim broadcast down")

	takerNode.watchOwnDepositSpends()

	if !takerSession.secretHunt {
		t.Fatal("failed re-drive cleared the hunt while the refund is still impossible")
	}
	if takerSession.claimRetryAt == 0 {
		t.Fatal("failed re-drive did not schedule the claim retry")
	}
	if takerSession.secret == [33]byte{} {
		t.Fatal("secret was not adopted from the found spend")
	}
	// And the refund stays stood down: no fallback to doomed broadcasts.
	takerNode.scanRefunds()
	if takerSession.refundDone {
		t.Fatal("sweep broadcast a refund on a hunted session with a pending claim")
	}
}

// TestZeroConfSpentHunt proves: a 0-conf (mempool) deposit tx known to the
// wallet with its outpoint already spent classifies spent. Core gettxout
// reflects the mempool, so this is the live 0-conf edge the probe relies on.
func TestZeroConfSpentHunt(t *testing.T) {
	ctx, s, _, idHex := huntFixture(t, "00deposit", true)
	conn := ctx.Node.config.Connectors["BTC"].(*stubConn)
	conn.verboseTx = map[string]wallet.VerboseTx{
		"00deposit": {TxID: "00deposit", Confirmations: 0},
	}

	ctx.Node.scanRefunds()

	if !s.secretHunt {
		t.Fatal("0-conf spent deposit did not arm the hunt")
	}
	if got := ctx.Store.Get(idHex); got == nil || got.Status == "rollback failed" {
		t.Fatalf("order = %+v, want live and never failure-marked", got)
	}
}

// TestZeroConfUnspentKeepsRetry: 0-conf deposit tx known AND outpoint present
// (funded stub utxo) classifies NOT spent — the refund failed for another
// reason and the legacy retry path owns it.
func TestZeroConfUnspentKeepsRetry(t *testing.T) {
	ctx := newWalletTestCtx()
	o := seedOrder(ctx)
	idHex := hexEncode(o.ID[:])
	ctx.Store.Update(idHex, func(u *Order) {
		u.DepositSent = true
		u.RefundTx = "deadbeefrefund"
	})
	depTx := stubFundedTxID
	conn := ctx.Node.config.Connectors["BTC"].(*stubConn)
	conn.sendErr = errors.New("broadcast refused: mempool conflict")
	conn.verboseTx = map[string]wallet.VerboseTx{
		depTx: {TxID: depTx, Confirmations: 0},
	}
	s := &SwapSession{
		n: ctx.Node, id: o.ID, isMaker: false,
		state: csCreatedB, srcCur: "BTC",
		ourDepositTxID: depTx, ourLockTime: 90,
		refundHex:       "deadbeefrefund",
		theirSecretHash: hash20("secret-hash"),
	}
	ctx.Node.sessions = map[string]*SwapSession{idHex: s}

	ctx.Node.scanRefunds()

	if s.secretHunt {
		t.Fatal("unspent 0-conf deposit misclassified as spent")
	}
	if got := ctx.Store.Get(idHex); got == nil || got.Status != "rollback failed" {
		t.Fatalf("order = %+v, want live 'rollback failed'", got)
	}
}

// TestGetTxOutErrorKeepsRetry: a gettxout backend failure with the deposit tx
// known classifies UNKNOWN (never spent) — errors fail safe toward retry.
func TestGetTxOutErrorKeepsRetry(t *testing.T) {
	ctx, s, _, idHex := huntFixture(t, "eedeposit", true)
	conn := ctx.Node.config.Connectors["BTC"].(*stubConn)
	conn.txOutErr = errors.New("wallet backend down")

	ctx.Node.scanRefunds()

	if s.secretHunt {
		t.Fatal("gettxout error misclassified as a spent deposit")
	}
	if got := ctx.Store.Get(idHex); got == nil || got.Status != "rollback failed" {
		t.Fatalf("order = %+v, want live 'rollback failed'", got)
	}
}

// TestStoredSpentShortCircuitsCandidates proves the sessionless multi-chain
// escape hatch stops on a spent verdict: the verdict is chain-independent
// (our deposit is gone — no candidate currency can refund it), so chaining
// on would waste broadcasts and bury the typed cause under a generic error.
func TestStoredSpentShortCircuitsCandidates(t *testing.T) {
	confs := map[string]*config.CoinConf{
		"LTC": {Ticker: "LTC", Coin: 1e8, AddressPrefix: 48, CreateTxMethod: "LTC", BlockTime: 60},
		"BTC": {Ticker: "BTC", Coin: 1e8, AddressPrefix: 0, CreateTxMethod: "BTC", BlockTime: 60},
	}
	ltcConn := &stubConnForHunt{ticker: "LTC", assertDepth: true}
	btcConn := &stubConnForHunt{ticker: "BTC"}
	n := newTestNode(t, confs, map[string]wallet.Connector{"LTC": ltcConn, "BTC": btcConn})
	var id [32]byte
	oid := hash20("stored-spent-short-circuit")
	copy(id[:], oid[:])
	idHex := hexEncode(id[:])
	n.store.Add(&Order{ID: id, FromCurrency: "LTC", ToCurrency: "BTC", Mine: true,
		BinTxId: "sesdeposit", RefundTx: "deadbeefrefund", DepositSent: true, Status: "open"})
	// First candidate fails spent; second is healthy and WOULD succeed if
	// tried (proving the short-circuit, not a double failure).
	ltcConn.sendErr = errors.New("broadcast refused: input already spent")
	ltcConn.verboseTx = map[string]wallet.VerboseTx{
		"sesdeposit": {TxID: "sesdeposit", Confirmations: 5},
	}

	var doneErr error
	var doneTx string
	n.tryStoredRefund(idHex, "deadbeefrefund", []string{"LTC", "BTC"}, func(txid string, err error) {
		doneTx, doneErr = txid, err
	})

	if !errors.Is(doneErr, errRefundDepositSpent) {
		t.Fatalf("done err = %v (txid %q), want errRefundDepositSpent — chaining buried it", doneErr, doneTx)
	}
	if btcConn.sends != 0 {
		t.Fatalf("second candidate attempted %d broadcast(s) after a spent verdict", btcConn.sends)
	}
}

// stubConnForHunt is a send-counting stub for the chaining test: stubConn
// records nothing on broadcast, so the short-circuit (no second attempt)
// would be unobservable through it.
type stubConnForHunt struct {
	ticker    string
	sendErr   error
	verboseTx map[string]wallet.VerboseTx
	sends     int
	// assertDepth makes served verbose entries assert their depth (like the
	// production RPC mapping on a present confirmations field, and like the
	// other fakes' serve paths). Unset, entries serve verbatim — the
	// missing-field shape for presence-discipline tests.
	assertDepth bool
}

func (s *stubConnForHunt) Ticker() string { return s.ticker }
func (s *stubConnForHunt) GetBalance() (uint64, error) {
	return 0, errors.New("stubConnForHunt: no balance source")
}
func (s *stubConnForHunt) GetNewAddress() (string, error) {
	return "", errors.New("stubConnForHunt: no address pool")
}
func (s *stubConnForHunt) ListUnspent(minConf int) ([]wallet.Utxo, error) {
	return nil, errors.New("stubConnForHunt: no UTXO source")
}
func (s *stubConnForHunt) SignRawTransaction(txHex string, prevTxs []wallet.PrevTx) (string, bool, error) {
	return "", false, errors.New("stubConnForHunt: cannot sign")
}
func (s *stubConnForHunt) SendRawTransaction(txHex string) (string, error) {
	s.sends++
	if s.sendErr != nil {
		return "", s.sendErr
	}
	return "txid123", nil
}
func (s *stubConnForHunt) GetRelayFee() (float64, error) { return 0.0001, nil }
func (s *stubConnForHunt) GetBlockCount() (int64, error) { return 1000, nil }
func (s *stubConnForHunt) GetBlockHash(height int64) ([32]byte, error) {
	return [32]byte{}, nil
}
func (s *stubConnForHunt) GetBlockTxs(blockHash [32]byte) ([]wallet.BlockTx, error) {
	return nil, errors.New("stubConnForHunt: unsupported")
}
func (s *stubConnForHunt) GetRawTransaction(txid string) (string, error) {
	return "", errors.New("stubConnForHunt: unknown transaction")
}
func (s *stubConnForHunt) CheckDepositTransaction(depositTxID, expectedScriptHex string, expectedAmount uint64, requiredConfirmations int) (wallet.DepositCheck, error) {
	return wallet.DepositCheck{}, errors.New("stubConnForHunt: no chain source")
}
func (s *stubConnForHunt) SignMessage(address, message string) ([]byte, error) {
	return nil, errors.New("stubConnForHunt: cannot sign")
}
func (s *stubConnForHunt) VerifyMessage(address string, sig []byte, message string) (bool, error) {
	return false, errors.New("stubConnForHunt: cannot verify")
}
func (s *stubConnForHunt) GetTxOut(txid string, vout uint32) (wallet.Utxo, bool, error) {
	return wallet.Utxo{}, false, nil
}
func (s *stubConnForHunt) GetRawTransactionVerbose(txid string) (wallet.VerboseTx, error) {
	if v, ok := s.verboseTx[txid]; ok {
		// Canned entries assert depth only when asked: the RPC mapping
		// sets HasConfirmations solely on a present confirmations field,
		// and presence tests need the unasserted shape.
		if s.assertDepth {
			v.HasConfirmations = true
		}
		return v, nil
	}
	return wallet.VerboseTx{}, &wallet.RPCError{Code: -5, Message: "No such transaction"}
}
func (s *stubConnForHunt) GetRawMempool() ([]string, error) {
	return nil, errors.New("stubConnForHunt: unsupported")
}

// TestHuntFlagsFileRoundTrip proves the hunt survives the disk format, not
// just the in-memory restoreSwap literal: persist → loadSwaps → restoreSwap
// must carry SecretHunt, HuntSince, and ScanCursor, and the restored session
// must keep hunting (sweep stood down, watch eligible, rescan resumed).
func TestHuntFlagsFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	n := newPersistNode(t, dir)
	var id [32]byte
	copy(id[:], []byte("hunt-round-trip-order-id00000")) // 32 bytes
	idHex := hexEncode(id[:])
	o := &Order{ID: id, FromCurrency: "LTC", ToCurrency: "BTC", FromAmount: 1e6,
		ToAmount: 2e6, Mine: true, Status: "created", DepositSent: true,
		RefundTx: refundHexFixture(), BinTxId: "dddeposit", UtxoCurrency: "LTC"}
	n.store.Add(o)
	n.sessions[idHex] = &SwapSession{n: n, id: id, isMaker: false, state: csCreatedB,
		srcCur: "LTC", ourDepositTxID: "dddeposit", ourLockTime: 900,
		refundHex: "deadbeefrefund", theirSecretHash: hash20("secret-hash"),
		secretHunt: true, huntSince: 123456789, scanCursor: 998877}
	n.persist()

	ps, _, _, err := loadSwaps(swapStatePath(dir))
	if err != nil {
		t.Fatalf("loadSwaps: %v", err)
	}
	if len(ps) != 1 {
		t.Fatalf("loaded %d swaps, want 1", len(ps))
	}
	if !ps[0].SecretHunt || ps[0].HuntSince != 123456789 || ps[0].ScanCursor != 998877 {
		t.Fatalf("hunt flags did not round-trip: %+v", ps[0])
	}

	n2 := newPersistNode(t, dir)
	n2.restoreSwap(ps[0])
	s := n2.sessions[idHex]
	if s == nil {
		t.Fatal("hunt record did not restore a live session")
	}
	if !s.secretHunt || s.huntSince != 123456789 || s.scanCursor != 998877 {
		t.Fatalf("restored hunt = %v/%d/%d, want true/123456789/998877", s.secretHunt, s.huntSince, s.scanCursor)
	}
}

// rescanPage seeds one canned block page (height -> txs) on a fake connector,
// returning the internal hash wired for that height.
func rescanPage(t *testing.T, conn *fakeConnector, height int64, txs []wallet.BlockTx) [32]byte {
	t.Helper()
	var h [32]byte
	h[0] = byte(height)
	h[1] = byte(height >> 8)
	if conn.blockHashes == nil {
		conn.blockHashes = map[int64][32]byte{}
	}
	if conn.blocks == nil {
		conn.blocks = map[[32]byte][]wallet.BlockTx{}
	}
	conn.blockHashes[height] = h
	conn.blocks[h] = txs
	return h
}

// TestRescanFindsConfirmedSpender proves the confirmed leg: with an empty
// mempool (the mempool leg cannot fire), a hunted taker recovers the secret
// from a block page carrying the vin-matched spender and finishes.
func TestRescanFindsConfirmedSpender(t *testing.T) {
	_, takerNode, _, takerSession, _, orderID, makerPayTxID, tkLtcConn, mkLtcConn := blindTakerFixture(t)
	idHex := hexEncode(orderID[:])
	takerSession.secretHunt = true
	takerSession.huntSince = uint64(NowMicro())
	hx, err := mkLtcConn.GetRawTransaction(makerPayTxID)
	if err != nil {
		t.Fatal("maker payTx missing from maker connector")
	}
	tkLtcConn.setRawTx(makerPayTxID, hx)
	tkLtcConn.mempoolTxids = nil // confirmed case: mempool is empty
	// Pages: 1000 empty, 1001 carries the spender, tip at 1001.
	rescanPage(t, tkLtcConn, 1000, []wallet.BlockTx{{TxID: "unrelated", Vin: []wallet.BlockVin{{TxID: "other", Vout: 1}}}})
	rescanPage(t, tkLtcConn, 1001, []wallet.BlockTx{{TxID: makerPayTxID, Vin: []wallet.BlockVin{{TxID: takerSession.ourDepositTxID, Vout: 0}}}})
	tkLtcConn.blockHeight = 1001
	takerSession.scanCursor = 1000

	takerNode.watchOwnDepositSpends()

	if takerSession.secret == [33]byte{} {
		t.Fatal("rescan did not recover the secret from the confirmed spend")
	}
	if takerSession.state != csFinished {
		t.Fatalf("state = %s, want csFinished", takerSession.state.String())
	}
	if o := takerNode.store.Get(idHex); o == nil || !o.CounterpartyRedeemed {
		t.Fatalf("order = %+v, want counterparty redeemed", o)
	}
	if takerSession.scanCursor != 1002 {
		t.Fatalf("cursor = %d, want 1002 (past both scanned pages)", takerSession.scanCursor)
	}
}

// TestRescanCursorAdvancesAndPersists proves each block is read once ever:
// empty pages advance the cursor past them, and the advance persists with
// the session for the next tick.
func TestRescanCursorAdvancesAndPersists(t *testing.T) {
	_, takerNode, _, takerSession, _, orderID, _, tkLtcConn, _ := blindTakerFixture(t)
	idHex := hexEncode(orderID[:])
	takerSession.secretHunt = true
	takerSession.scanCursor = 990
	for h := int64(990); h <= 992; h++ {
		rescanPage(t, tkLtcConn, h, []wallet.BlockTx{{TxID: "unrelated", Vin: []wallet.BlockVin{{TxID: "other", Vout: 0}}}})
	}
	tkLtcConn.blockHeight = 992
	tkLtcConn.mempoolTxids = nil

	takerNode.watchOwnDepositSpends()

	if takerSession.scanCursor != 993 {
		t.Fatalf("cursor = %d, want 993 (past all scanned pages)", takerSession.scanCursor)
	}
	if _, ok := takerNode.sessions[idHex]; !ok {
		t.Fatal("scanning session was pruned")
	}
	if takerSession.secretHunt != true || takerSession.secret != [33]byte{} {
		t.Fatal("empty scan disturbed the hunt")
	}
}

// TestRescanSeedsFromConfirmationDepth proves legacy cursor seeding: a
// cursorless session starts at tip minus the deposit's confirmation depth
// (≈ broadcast height), not at genesis and not at tip.
func TestRescanSeedsFromConfirmationDepth(t *testing.T) {
	_, takerNode, _, takerSession, _, _, _, tkLtcConn, _ := blindTakerFixture(t)
	takerSession.secretHunt = true
	// Deposit confirmed 5 deep at tip 1000 → first scanned page is 995.
	tkLtcConn.verboseTx = map[string]wallet.VerboseTx{
		takerSession.ourDepositTxID: {TxID: takerSession.ourDepositTxID, Confirmations: 5},
	}
	rescanPage(t, tkLtcConn, 995, []wallet.BlockTx{{TxID: "unrelated", Vin: []wallet.BlockVin{{TxID: "other", Vout: 0}}}})
	tkLtcConn.blockHeight = 1000
	tkLtcConn.mempoolTxids = nil

	takerNode.watchOwnDepositSpends()

	if takerSession.scanCursor != 996 {
		t.Fatalf("cursor = %d, want 996 (seeded 995 + one page)", takerSession.scanCursor)
	}
}

// TestRescanSeedsWindowOnBlindDeposit proves the unknown-deposit fallback:
// with no verbose view, the cursor seeds one window back — bounded,
// budgeted, documented — instead of genesis or tip.
func TestRescanSeedsWindowOnBlindDeposit(t *testing.T) {
	_, takerNode, _, takerSession, _, _, _, tkLtcConn, _ := blindTakerFixture(t)
	takerSession.secretHunt = true
	// No verboseTx entry: the backend cannot see the deposit tx.
	rescanPage(t, tkLtcConn, 856, []wallet.BlockTx{{TxID: "unrelated", Vin: []wallet.BlockVin{{TxID: "other", Vout: 0}}}})
	tkLtcConn.blockHeight = 1000
	tkLtcConn.mempoolTxids = nil

	takerNode.watchOwnDepositSpends()

	if takerSession.scanCursor != 857 {
		t.Fatalf("cursor = %d, want 857 (tip 1000 - window 144 + one page)", takerSession.scanCursor)
	}
}

// TestRescanPrunedHoldsCursor proves pruned-history degradation: a page
// failure holds the cursor (never marks skips), keeps the hunt, and stays
// silent at WARN level (Debug only — the hourly hunt WARN owns visibility).
func TestRescanPrunedHoldsCursor(t *testing.T) {
	_, takerNode, _, takerSession, _, _, _, tkLtcConn, _ := blindTakerFixture(t)
	takerSession.secretHunt = true
	takerSession.scanCursor = 990
	tkLtcConn.blockErr = errors.New("pruned history: block not available")
	tkLtcConn.blockHeight = 995
	tkLtcConn.mempoolTxids = nil

	takerNode.watchOwnDepositSpends()

	if takerSession.scanCursor != 990 {
		t.Fatalf("cursor moved to %d on page failure — skipped blocks", takerSession.scanCursor)
	}
	if !takerSession.secretHunt {
		t.Fatal("page failure dropped the hunt")
	}
}

// TestRescanRespectsPageBudget proves the rate limit: with ten empty pages
// ahead, one round advances exactly rescanPageBudget pages — bursts are
// structurally impossible, catch-up stretches instead.
func TestRescanRespectsPageBudget(t *testing.T) {
	_, takerNode, _, takerSession, _, _, _, tkLtcConn, _ := blindTakerFixture(t)
	takerSession.secretHunt = true
	takerSession.scanCursor = 900
	for h := int64(900); h < 910; h++ {
		rescanPage(t, tkLtcConn, h, []wallet.BlockTx{{TxID: "unrelated", Vin: []wallet.BlockVin{{TxID: "other", Vout: 0}}}})
	}
	tkLtcConn.blockHeight = 909
	tkLtcConn.mempoolTxids = nil

	takerNode.watchOwnDepositSpends()

	if takerSession.scanCursor != 900+rescanPageBudget {
		t.Fatalf("cursor = %d, want %d (exactly one budget)", takerSession.scanCursor, 900+rescanPageBudget)
	}
}

// TestRescanSkipsOwnRefundTx proves the exclusion: our own refund spends our
// deposit by construction but carries no secret — adopting it would poison
// theirPayTxID and park the claim retry on a secretless tx. The page still
// counts as scanned (fully decidable), so the cursor advances past it.
func TestRescanSkipsOwnRefundTx(t *testing.T) {
	_, takerNode, _, takerSession, _, orderID, _, tkLtcConn, _ := blindTakerFixture(t)
	idHex := hexEncode(orderID[:])
	takerSession.secretHunt = true
	takerSession.huntSince = uint64(NowMicro())
	takerSession.scanCursor = 1000
	refundTxID, err := txIDFromHex(takerSession.refundHex)
	if err != nil || refundTxID == "" {
		t.Fatalf("no refund txid to exclude: %v", err)
	}
	rescanPage(t, tkLtcConn, 1000, []wallet.BlockTx{{
		TxID: refundTxID,
		Vin:  []wallet.BlockVin{{TxID: takerSession.ourDepositTxID, Vout: 0}},
	}})
	tkLtcConn.blockHeight = 1000
	tkLtcConn.mempoolTxids = nil

	takerNode.watchOwnDepositSpends()

	if takerSession.theirPayTxID != "" {
		t.Fatalf("own refund adopted as counterparty payTx: %q", takerSession.theirPayTxID)
	}
	if takerSession.secret != [33]byte{} {
		t.Fatal("secret adopted from a secretless refund")
	}
	if takerSession.scanCursor != 1001 {
		t.Fatalf("cursor = %d, want 1001 (decidable page counts as scanned)", takerSession.scanCursor)
	}
	if !takerSession.secretHunt {
		t.Fatal("excluded refund dropped the hunt")
	}
	if _, ok := takerNode.sessions[idHex]; !ok {
		t.Fatal("session pruned on an excluded refund")
	}
}

// TestRescanHoldsOnUnvalidatableMatch proves fail-closed validation: a
// vin-matched candidate whose bytes cannot be fetched holds the cursor —
// never declared, never skipped past.
func TestRescanHoldsOnUnvalidatableMatch(t *testing.T) {
	_, takerNode, _, takerSession, _, _, _, tkLtcConn, _ := blindTakerFixture(t)
	takerSession.secretHunt = true
	takerSession.scanCursor = 1000
	// Vin matches our deposit but the tx bytes are unavailable: undecidable.
	rescanPage(t, tkLtcConn, 1000, []wallet.BlockTx{{
		TxID: "unfetchable-spender",
		Vin:  []wallet.BlockVin{{TxID: takerSession.ourDepositTxID, Vout: 0}},
	}})
	tkLtcConn.blockHeight = 1000
	tkLtcConn.mempoolTxids = nil

	takerNode.watchOwnDepositSpends()

	if takerSession.scanCursor != 1000 {
		t.Fatalf("cursor = %d, want 1000 (held on undecidable page)", takerSession.scanCursor)
	}
	if takerSession.theirPayTxID != "" {
		t.Fatal("unfetchable candidate adopted as payTx")
	}
	if !takerSession.secretHunt {
		t.Fatal("held page dropped the hunt")
	}
}

// TestRescanBackoffSpacesPageFailures proves perpetual failure degrades to
// backoff, not bursts: after a failed page the leg stands down for the
// backoff window while the mempool leg keeps running.
func TestRescanBackoffSpacesPageFailures(t *testing.T) {
	_, takerNode, _, takerSession, _, orderID, _, tkLtcConn, _ := blindTakerFixture(t)
	idHex := hexEncode(orderID[:])
	takerSession.secretHunt = true
	takerSession.scanCursor = 990
	tkLtcConn.blockErr = errors.New("pruned history: block not available")
	tkLtcConn.blockHeight = 995
	tkLtcConn.mempoolTxids = nil

	takerNode.watchOwnDepositSpends()
	if !takerNode.rescanBackoffActive(idHex) {
		t.Fatal("page failure did not arm the rescan backoff")
	}

	// Second round inside the window: the leg must not fire again (no new
	// RPC storm on a permanently missing page).
	tkLtcConn.blockErr = errors.New("must not be called under backoff")
	takerNode.watchOwnDepositSpends()
	if takerSession.scanCursor != 990 {
		t.Fatalf("cursor moved to %d under backoff", takerSession.scanCursor)
	}
}

// TestRescanPageBudgetFor pins the dynamic bound: slow chains keep the
// default, fast chains scale up (capped) instead of falling behind.
func TestRescanPageBudgetFor(t *testing.T) {
	if got := rescanPageBudgetFor(0); got != 3 {
		t.Fatalf("budget(0) = %d, want 3 (default)", got)
	}
	if got := rescanPageBudgetFor(600); got != 3 {
		t.Fatalf("budget(600) = %d, want 3", got)
	}
	if got := rescanPageBudgetFor(60); got != 3 {
		t.Fatalf("budget(60) = %d, want 3", got)
	}
	if got := rescanPageBudgetFor(10); got != 12 {
		t.Fatalf("budget(10) = %d, want 12 (cap)", got)
	}
	if got := rescanPageBudgetFor(30); got != 3 {
		t.Fatalf("budget(30) = %d, want 3 (base covers 2/min growth)", got)
	}
	if got := rescanPageBudgetFor(20); got != 7 {
		t.Fatalf("budget(20) = %d, want 7 (boundary inclusive: exactly double growth still gains)", got)
	}
	if got := rescanPageBudgetFor(15); got != 9 {
		t.Fatalf("budget(15) = %d, want 9", got)
	}
}

// TestOutsideWindowSpendParksVisible documents the bounded visible park: a
// blind backend with a spend older than the seed window cannot find it —
// the session stays hunted and alive (hourly WARN owns visibility) instead
// of failing, cancelling, or stranding silently.
func TestOutsideWindowSpendParksVisible(t *testing.T) {
	_, takerNode, _, takerSession, _, orderID, _, tkLtcConn, _ := blindTakerFixture(t)
	idHex := hexEncode(orderID[:])
	takerSession.secretHunt = true
	takerSession.huntSince = uint64(NowMicro())
	// Blind backend (no verbose view) at tip 1000: seed lands at 856, three
	// empty pages scan, nothing found — the older spend stays out of reach.
	for h := int64(856); h <= 858; h++ {
		rescanPage(t, tkLtcConn, h, []wallet.BlockTx{{TxID: "unrelated", Vin: []wallet.BlockVin{{TxID: "other", Vout: 0}}}})
	}
	tkLtcConn.blockHeight = 1000
	tkLtcConn.mempoolTxids = nil

	takerNode.watchOwnDepositSpends()

	if !takerSession.secretHunt {
		t.Fatal("visible park dropped the hunt")
	}
	if takerSession.scanCursor != 859 {
		t.Fatalf("cursor = %d, want 859 (window scanned, nothing found)", takerSession.scanCursor)
	}
	if _, ok := takerNode.sessions[idHex]; !ok {
		t.Fatal("parked session was pruned")
	}
	if o := takerNode.store.Get(idHex); o == nil {
		t.Fatal("parked order left the live book")
	}
}

// TestRescanNegativeConfFallsBackToWindow proves conflicted deposits seed
// the window, not the depth math: negative confirmations are reorg evidence,
// and tip-minus-negative would seed ABOVE the tip.
func TestRescanNegativeConfFallsBackToWindow(t *testing.T) {
	_, takerNode, _, takerSession, _, _, _, tkLtcConn, _ := blindTakerFixture(t)
	takerSession.secretHunt = true
	tkLtcConn.verboseTx = map[string]wallet.VerboseTx{
		takerSession.ourDepositTxID: {TxID: takerSession.ourDepositTxID, Confirmations: -1, HasConfirmations: true},
	}
	rescanPage(t, tkLtcConn, 856, []wallet.BlockTx{{TxID: "unrelated", Vin: []wallet.BlockVin{{TxID: "other", Vout: 0}}}})
	tkLtcConn.blockHeight = 1000
	tkLtcConn.mempoolTxids = nil

	takerNode.watchOwnDepositSpends()

	if takerSession.scanCursor != 857 {
		t.Fatalf("cursor = %d, want 857 (window fallback, not tip+1)", takerSession.scanCursor)
	}
}

// TestRescanSeedsZeroOnYoungChain proves zero-backfill semantics: below the
// window the cursor seeds at genesis and scans up — correct on young chains,
// never a negative height.
func TestRescanSeedsZeroOnYoungChain(t *testing.T) {
	_, takerNode, _, takerSession, _, _, _, tkLtcConn, _ := blindTakerFixture(t)
	takerSession.secretHunt = true
	for h := int64(0); h <= 2; h++ {
		rescanPage(t, tkLtcConn, h, []wallet.BlockTx{{TxID: "unrelated", Vin: []wallet.BlockVin{{TxID: "other", Vout: 0}}}})
	}
	tkLtcConn.blockHeight = 100
	tkLtcConn.mempoolTxids = nil

	takerNode.watchOwnDepositSpends()

	if takerSession.scanCursor != 3 {
		t.Fatalf("cursor = %d, want 3 (genesis-seeded catch-up)", takerSession.scanCursor)
	}
}

// TestRescanPartialAdvancePersists proves a mid-budget failure keeps partial
// progress: the fully-scanned page persists, the failed page holds.
func TestRescanPartialAdvancePersists(t *testing.T) {
	_, takerNode, _, takerSession, _, _, _, tkLtcConn, _ := blindTakerFixture(t)
	takerSession.secretHunt = true
	takerSession.scanCursor = 990
	rescanPage(t, tkLtcConn, 990, []wallet.BlockTx{{TxID: "unrelated", Vin: []wallet.BlockVin{{TxID: "other", Vout: 0}}}})
	// 991 has a hash but no body: GetBlockTxs fails like a pruned page.
	var h991 [32]byte
	h991[0] = 0xdf
	if tkLtcConn.blockHashes == nil {
		tkLtcConn.blockHashes = map[int64][32]byte{}
	}
	tkLtcConn.blockHashes[991] = h991
	tkLtcConn.blockHeight = 991
	tkLtcConn.mempoolTxids = nil

	takerNode.watchOwnDepositSpends()

	if takerSession.scanCursor != 991 {
		t.Fatalf("cursor = %d, want 991 (past the good page, held at the bad one)", takerSession.scanCursor)
	}
}

// TestRescanMultiHuntBudgetsIndependent proves per-session bounds compose:
// two hunts on one node each advance within budget — no shared counter, no
// starvation, no multiplication beyond one bound each.
func TestRescanMultiHuntBudgetsIndependent(t *testing.T) {
	_, n1, _, s1, _, _, _, c1, _ := blindTakerFixture(t)
	_, n2, _, s2, _, _, _, c2, _ := blindTakerFixture(t)
	for _, tc := range []struct {
		n *Node
		s *SwapSession
		c *fakeConnector
	}{
		{n1, s1, c1},
		{n2, s2, c2},
	} {
		tc.s.secretHunt = true
		tc.s.scanCursor = 900
		for h := int64(900); h < 910; h++ {
			rescanPage(t, tc.c, h, []wallet.BlockTx{{TxID: "unrelated", Vin: []wallet.BlockVin{{TxID: "other", Vout: 0}}}})
		}
		tc.c.blockHeight = 909
		tc.c.mempoolTxids = nil
		tc.n.watchOwnDepositSpends()
		if tc.s.scanCursor != 903 {
			t.Fatalf("cursor = %d, want 903 (one budget each)", tc.s.scanCursor)
		}
	}
}

// TestConfirmPollerMissingFieldKeepsDepth proves the presence discipline in
// the confirmation poller: a verbose response without an asserted depth
// keeps the last depth instead of recording zero.
func TestConfirmPollerMissingFieldKeepsDepth(t *testing.T) {
	bc := &stubConnForHunt{ticker: "BLOCK", verboseTx: map[string]wallet.VerboseTx{
		"tx1": {TxID: "tx1", Confirmations: 4, Outputs: map[uint32]wallet.VerboseTxOut{}},
	}}
	n := newTestNode(t, map[string]*config.CoinConf{}, map[string]wallet.Connector{"BLOCK": bc})
	n.recordBroadcast("order1", broadcastClaim, "BLOCK", "tx1", "hex")
	n.pollBroadcastConfirmations()
	got := n.trackedSnapshot()
	if len(got) != 1 || got["tx1"].Confs != 0 {
		t.Fatalf("tracked = %+v, want entry kept at last depth 0", got)
	}
}

// TestConfirmDepositWaitsOnMissingField proves the validation gate requires
// asserted depth: exact script/value match with no confirmations field
// waits instead of proceeding — the field's absence proves nothing.
func TestConfirmDepositWaitsOnMissingField(t *testing.T) {
	conn := &stubConnForHunt{ticker: "BTC", verboseTx: map[string]wallet.VerboseTx{
		"deptx": {TxID: "deptx", Outputs: map[uint32]wallet.VerboseTxOut{
			0: {Value: 100000, ScriptHex: "deadbeef"},
		}},
	}}
	if confirmDepositKnownByRawTx(conn, "deptx", 0, "deadbeef", 100000, 0) {
		t.Fatal("deposit validation proceeded on unasserted depth")
	}
}
