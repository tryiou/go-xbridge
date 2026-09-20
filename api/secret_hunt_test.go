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
	ltcConn := &stubConnForHunt{ticker: "LTC"}
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
		return v, nil
	}
	return wallet.VerboseTx{}, &wallet.RPCError{Code: -5, Message: "No such transaction"}
}
func (s *stubConnForHunt) GetRawMempool() ([]string, error) {
	return nil, errors.New("stubConnForHunt: unsupported")
}

// TestHuntFlagsFileRoundTrip proves the hunt survives the disk format, not
// just the in-memory restoreSwap literal: persist → loadSwaps → restoreSwap
// must carry SecretHunt and HuntSince, and the restored session must keep
// hunting (sweep stood down, watch eligible).
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
		secretHunt: true, huntSince: 123456789}
	n.persist()

	ps, _, _, err := loadSwaps(swapStatePath(dir))
	if err != nil {
		t.Fatalf("loadSwaps: %v", err)
	}
	if len(ps) != 1 {
		t.Fatalf("loaded %d swaps, want 1", len(ps))
	}
	if !ps[0].SecretHunt || ps[0].HuntSince != 123456789 {
		t.Fatalf("hunt flags did not round-trip: %+v", ps[0])
	}

	n2 := newPersistNode(t, dir)
	n2.restoreSwap(ps[0])
	s := n2.sessions[idHex]
	if s == nil {
		t.Fatal("hunt record did not restore a live session")
	}
	if !s.secretHunt || s.huntSince != 123456789 {
		t.Fatalf("restored hunt = %v/%d, want true/123456789", s.secretHunt, s.huntSince)
	}
}
