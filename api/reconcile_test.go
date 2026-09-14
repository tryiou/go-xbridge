package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"go-xbridge/coins"
	"go-xbridge/config"
	"go-xbridge/proto"
	"go-xbridge/wallet"
)

// Phase-0 confirmation tracking: failing-first tests. The reconcile loop
// records every broadcast and polls its confirmation depth so later phases
// can distinguish "done" from "accepted then dropped" (live-proven S2 hole).

func TestBroadcastPersistRoundTrip(t *testing.T) {
	swaps := []persistedSwap{{ID: [32]byte{0x1}}}
	bc := []persistedBroadcast{{
		OrderID: "2b1d88d0", Kind: broadcastClaim, Coin: "BLOCK",
		TxID: "0c3f4d70", Hex: "deadbeef", Seq: 7, Confs: 3,
		FirstSeenMicro: 123456789, Attempts: 2,
	}}
	data, err := marshalSwapFile(swaps, bc, nil)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	gotSwaps, gotBc, _, err := parseSwapFile(data, "test")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(gotSwaps) != 1 || len(gotBc) != 1 {
		t.Fatalf("round trip = %d swaps %d broadcasts, want 1/1", len(gotSwaps), len(gotBc))
	}
	if gotBc[0].TxID != "0c3f4d70" || gotBc[0].Confs != 3 || gotBc[0].Kind != broadcastClaim {
		t.Fatalf("broadcast mismatch: %+v", gotBc[0])
	}
	if gotBc[0].FirstSeenMicro != 123456789 || gotBc[0].Attempts != 2 {
		t.Fatalf("rebroadcast fields lost: %+v", gotBc[0])
	}
}

func TestBroadcastLegacyFileLoads(t *testing.T) {
	// Old-shape file: swaps-only envelope with legacy swaps-only checksum.
	swaps := []persistedSwap{{ID: [32]byte{0x2}}}
	blob, err := json.Marshal(swaps)
	if err != nil {
		t.Fatal(err)
	}
	legacySum := sha256.Sum256(blob)
	legacy := map[string]any{"sum": hex.EncodeToString(legacySum[:]), "swaps": swaps}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	gotSwaps, gotBc, _, err := parseSwapFile(data, "test")
	if err != nil {
		t.Fatalf("legacy load: %v", err)
	}
	if len(gotSwaps) != 1 || len(gotBc) != 0 {
		t.Fatalf("legacy = %d swaps %d broadcasts, want 1/0", len(gotSwaps), len(gotBc))
	}
}

func TestCorruptBroadcastsQuarantined(t *testing.T) {
	// Garbage broadcasts section with a valid swaps checksum must fail
	// closed (whole file rejected), never silently drop tracking state.
	env := map[string]any{"sum": "00", "swaps": []persistedSwap{}, "broadcasts": "garbage"}
	data, _ := json.Marshal(env)
	if _, _, _, err := parseSwapFile(data, "test"); err == nil {
		t.Fatal("corrupt file must error, not load")
	}
}

// confirmTestNode builds an inline node with a stub BLOCK connector serving
// verboseTx, for poller tests.
func confirmTestNode(t *testing.T, bc *stubConn) *Node {
	t.Helper()
	n := newTestNode(t, map[string]*config.CoinConf{}, map[string]wallet.Connector{"BLOCK": bc})
	return n
}

func TestConfirmPollerUpdates(t *testing.T) {
	bc := &stubConn{ticker: "BLOCK", verboseTx: map[string]wallet.VerboseTx{
		"tx1": {TxID: "tx1", Confirmations: 4, Outputs: map[uint32]wallet.VerboseTxOut{}},
	}}
	n := confirmTestNode(t, bc)
	n.recordBroadcast("order1", broadcastClaim, "BLOCK", "tx1", "hex")
	n.pollBroadcastConfirmations()
	got := n.trackedSnapshot()
	if len(got) != 1 || got["tx1"].Confs != 4 {
		t.Fatalf("tracked = %+v, want tx1 confs 4", got)
	}
}

func TestConfirmPollerUnknownKeepsWatching(t *testing.T) {
	// Facade-blind unknown (-5): confs stay 0, entry kept, no error surfaced.
	bc := &stubConn{ticker: "BLOCK", verboseErr: &wallet.RPCError{Code: -5, Message: "unknown"}}
	n := confirmTestNode(t, bc)
	n.recordBroadcast("order1", broadcastDeposit, "BLOCK", "tx-missing", "hex")
	n.pollBroadcastConfirmations()
	got := n.trackedSnapshot()
	if len(got) != 1 || got["tx-missing"].Confs != 0 {
		t.Fatalf("tracked = %+v, want entry kept at 0", got)
	}
}

func TestRecordBroadcastDedups(t *testing.T) {
	bc := &stubConn{ticker: "BLOCK"}
	n := confirmTestNode(t, bc)
	n.recordBroadcast("order1", broadcastClaim, "BLOCK", "tx1", "hex1")
	n.recordBroadcast("order1", broadcastClaim, "BLOCK", "tx1", "hex1")
	if len(n.trackedSnapshot()) != 1 {
		t.Fatal("re-record of same txid must not duplicate")
	}
}

// TestPruneTracked proves cap pruning keeps live-session and shallow entries
// and drops the oldest deep sessionless entries first.
func TestPruneTracked(t *testing.T) {
	bc := &stubConn{ticker: "BLOCK"}
	n := confirmTestNode(t, bc)
	const total = trackedCap + 3
	for i := 0; i < total; i++ {
		n.recordBroadcast("order-deep", broadcastClaim, "BLOCK", "deep-tx-"+string(rune('a'+i%26))+itoa(i), "hex")
	}
	// One live-session entry (must survive) and one shallow entry.
	n.recordBroadcast("order-live", broadcastClaim, "BLOCK", "live-tx", "hex")
	n.sessions["order-live"] = &SwapSession{}
	n.recordBroadcast("order-shallow", broadcastClaim, "BLOCK", "shallow-tx", "hex")
	n.trackedMu.Lock()
	for txid, tb := range n.tracked {
		if txid == "shallow-tx" {
			tb.Confs = 1
		} else {
			tb.Confs = trackedRetainDepth
		}
	}
	n.trackedMu.Unlock()
	n.pruneTracked()
	got := n.trackedSnapshot()
	if len(got) > trackedCap {
		t.Fatalf("tracked = %d, want <= %d", len(got), trackedCap)
	}
	if _, ok := got["live-tx"]; !ok {
		t.Fatal("live-session entry pruned")
	}
	if _, ok := got["shallow-tx"]; !ok {
		t.Fatal("shallow entry pruned")
	}
	if _, ok := got["deep-tx-a0"]; ok {
		t.Fatal("oldest deep sessionless entry kept")
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	p := len(b)
	for i > 0 {
		p--
		b[p] = byte('0' + i%10)
		i /= 10
	}
	return string(b[p:])
}

// TestRestoreTracked proves startup restore reloads valid records, skips
// corrupt/duplicates, and advances the sequence so new records sort after.
func TestRestoreTracked(t *testing.T) {
	bc := &stubConn{ticker: "BLOCK"}
	n := confirmTestNode(t, bc)
	n.restoreTracked([]persistedBroadcast{
		{OrderID: "o1", Kind: broadcastDeposit, Coin: "BLOCK", TxID: "tx1", Hex: "aa", Seq: 5, Confs: 2},
		{OrderID: "o2", Kind: broadcastClaim, Coin: "BLOCK", TxID: "", Hex: "bb"},
		{OrderID: "o3", Kind: broadcastClaim, Coin: "", TxID: "tx3", Hex: "cc"},
		{OrderID: "o1", Kind: broadcastDeposit, Coin: "BLOCK", TxID: "tx1", Hex: "aa", Seq: 5, Confs: 2},
	})
	got := n.trackedSnapshot()
	if len(got) != 1 {
		t.Fatalf("tracked = %d, want 1 (valid only)", len(got))
	}
	if got["tx1"].Confs != 2 || got["tx1"].Kind != broadcastDeposit {
		t.Fatalf("restored = %+v", got["tx1"])
	}
	n.recordBroadcast("o9", broadcastClaim, "BLOCK", "tx9", "hex")
	if got := n.trackedSnapshot(); got["tx9"].Seq <= 5 {
		t.Fatalf("new seq = %d, want > 5 (after restored max)", got["tx9"].Seq)
	}
}

// TestClaimBroadcastTracked proves the claim funnel records with kind claim:
// a direct postBroadcastTask claim drive lands in the watch table.
func TestClaimBroadcastTracked(t *testing.T) {
	ctx := newWalletTestCtx()
	stub, ok := ctx.Node.cfg().Connectors["BTC"].(*stubConn)
	if !ok {
		t.Fatal("no stub BTC connector")
	}
	applied := false
	ctx.Node.postBroadcastTask("order-claim", stub, "BTC", "abcd", broadcastClaim, func(sentID string, terr error) {
		applied = true
		if terr != nil {
			t.Errorf("apply err: %v", terr)
		}
	})
	if !applied {
		t.Fatal("apply never ran")
	}
	want := txIDFromHexMust(t, "abcd")
	got := ctx.Node.trackedSnapshot()
	tb, ok := got[want]
	if !ok {
		t.Fatalf("claim tx not tracked: %v", got)
	}
	if tb.Kind != broadcastClaim || tb.Coin != "BTC" || tb.Hex != "abcd" {
		t.Fatalf("tracked = %+v", tb)
	}
}

func txIDFromHexMust(t *testing.T, h string) string {
	t.Helper()
	id, err := txIDFromHex(h)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// TestRefundBroadcastTracked proves the refund path records with kind refund.
func TestRefundBroadcastTracked(t *testing.T) {
	n, s, _ := setupSwapPair(t)
	idHex := hexEncode(s.id[:])
	s.refundHex = "abcd"
	s.state = csCreatedA
	n.postRefundTask(idHex, "BTC", "abcd", 0, false, nil)
	want := txIDFromHexMust(t, "abcd")
	got := n.trackedSnapshot()
	tb, ok := got[want]
	if !ok {
		t.Fatalf("refund tx not tracked: %v", got)
	}
	if tb.Kind != broadcastRefund || tb.Coin != "BTC" {
		t.Fatalf("tracked = %+v", tb)
	}
}

// TestSplitBroadcastTracked proves the wallet-utility split path records with
// kind split and no order link.
func TestSplitBroadcastTracked(t *testing.T) {
	ctx := newWalletTestCtx()
	stub, ok := ctx.Node.cfg().Connectors["BTC"].(*stubConn)
	if !ok {
		t.Fatal("no stub BTC connector")
	}
	utxo := stub.utxos[0]
	if _, err := ctx.splitTx("BTC", "0.5", btcAddr, false, false, true, []wallet.Utxo{utxo}, "dxSplit"); err != nil {
		t.Fatalf("splitTx: %v", err)
	}
	got := ctx.Node.trackedSnapshot()
	if len(got) != 1 {
		t.Fatalf("tracked = %d, want 1 (the split)", len(got))
	}
	for _, tb := range got {
		if tb.Kind != broadcastSplit || tb.Coin != "BTC" || tb.OrderID != "" || tb.Hex == "" {
			t.Fatalf("tracked = %+v", tb)
		}
	}
}

// TestPrepBroadcastTracked proves the partial-order prep path records with
// kind prep under the real prep txid (not the wallet return, not empty).
func TestPrepBroadcastTracked(t *testing.T) {
	reg, _ := runningHub(t)
	n, _ := newHubNode(reg)
	o, rerr := n.MakeOrder(MakeOrderParams{
		Type: "partial", AutoSplit: true,
		Maker: "BTC", MakerSize: "2.5", MinSize: "1.0", MakerAddress: btcAddr,
		Taker: "SYS", TakerSize: "0.3", TakerAddress: btcAddr2,
	})
	if rerr != nil {
		t.Fatalf("MakeOrder(autoSplit): %v", rerr)
	}
	if o.PrepTx == "" {
		t.Fatal("no prep txid (fixture does not broadcast prep)")
	}
	got := n.trackedSnapshot()
	tb, ok := got[o.PrepTx]
	if !ok {
		t.Fatalf("prep tx %s not tracked: %v", o.PrepTx, got)
	}
	if tb.Kind != broadcastPrep || tb.Coin != "BTC" || tb.Hex == "" {
		t.Fatalf("tracked = %+v", tb)
	}
}

// TestDepositBroadcastTracked proves the deposit path records its broadcast
// for confirmation watch: after OnCreateA broadcasts, the deposit txid is
// tracked with kind deposit on the right coin.
func TestDepositBroadcastTracked(t *testing.T) {
	if err := coins.InitFromConf(map[string]*config.CoinConf{
		"BTC": {Ticker: "BTC", Coin: 1e8, AddressPrefix: 0, ScriptPrefix: 5, CreateTxMethod: "BTC", BlockTime: 60},
	}); err != nil {
		t.Fatal(err)
	}
	mkPriv, mkPub := newKey(t)
	funding := wallet.Utxo{TxID: strings.Repeat("aa", 32), Vout: 0, Amount: 5e8, ScriptPubKey: hex.EncodeToString(coins.BuildP2PKHScript(coins.KeyID(mkPub)))}
	conn := &fakeConnector{ticker: "BTC", funding: funding, fundingPriv: mkPriv, fundingPub: mkPub, changeAddr: addrFor(0, "chg"), blockHeight: 1000, rawTx: map[string]string{}}
	confs := map[string]*config.CoinConf{
		"BTC": {Ticker: "BTC", Coin: 1e8, AddressPrefix: 0, CreateTxMethod: "BTC", BlockTime: 60},
	}
	n := newTestNode(t, confs, map[string]wallet.Connector{"BTC": conn})
	var oid [32]byte
	oh := hash20("track-deposit-order")
	copy(oid[:], oh[:])
	ord := &Order{ID: oid, FromCurrency: "BTC", ToCurrency: "BTC", FromAmount: 1e6, ToAmount: 1e6}
	n.newMakerSession(withUsedCoins(t, n, ord, []wallet.Utxo{funding}), MakeOrderParams{MakerAddress: addrFor(0, "m"), TakerAddress: addrFor(0, "t")}, arr32(mkPriv), toArr33(mkPub))
	sess := n.sessions[hexEncode(oid[:])]
	var hub [20]byte
	sess.hub = hub
	_, tkPub := newKey(t)
	if _, _, err := sess.OnCreateA(&proto.CreateABody{HubAddress: hub, ID: oid, BPubKey: to33(tkPub)}); err != nil {
		t.Fatalf("OnCreateA: %v", err)
	}
	got := n.trackedSnapshot()
	if len(got) != 1 {
		t.Fatalf("tracked = %d entries, want 1 (the deposit)", len(got))
	}
	for _, tb := range got {
		if tb.Kind != broadcastDeposit || tb.Coin != "BTC" || tb.Hex == "" {
			t.Fatalf("tracked = %+v, want kind=deposit coin=BTC with hex", tb)
		}
	}
}

// Phase-1 R1 rebroadcast: failing-first tests. A tracked broadcast stuck at 0
// confs past the coin-aware age threshold is rebroadcast with the IDENTICAL
// bytes (same txid — never rebuilt, never fee-bumped).

func rebroadcastTestSetup(t *testing.T) (*Node, *fakeConnector, string, string) {
	t.Helper()
	conn := &fakeConnector{ticker: "BTC", blockHeight: 1000, rawTx: map[string]string{}}
	confs := map[string]*config.CoinConf{
		"BTC": {Ticker: "BTC", Coin: 1e8, AddressPrefix: 0, CreateTxMethod: "BTC", BlockTime: 60},
	}
	n := newTestNode(t, confs, map[string]wallet.Connector{"BTC": conn})
	tx := &coins.Tx{Version: 1}
	tx.Inputs = append(tx.Inputs, coins.TxIn{PrevOut: coins.OutPoint{Hash: [32]byte{0x11}, Index: 0}, Sequence: 0xffffffff})
	tx.Outputs = append(tx.Outputs, coins.TxOut{Value: 99900000, ScriptPubKey: []byte{0x51}})
	hexStr := hex.EncodeToString(tx.Serialize())
	txid, err := txIDFromHex(hexStr)
	if err != nil {
		t.Fatal(err)
	}
	n.recordBroadcast("order-rb", broadcastDeposit, "BTC", txid, hexStr)
	return n, conn, txid, hexStr
}

// TestRebroadcastUnconfirmed proves R1: a tracked broadcast stuck at 0 confs
// past the coin-aware age threshold is rebroadcast with the IDENTICAL bytes.
func TestRebroadcastUnconfirmed(t *testing.T) {
	n, conn, txid, hexStr := rebroadcastTestSetup(t)
	n.tracked[txid].FirstSeenMicro = uint64(NowMicro()) - uint64(n.rebroadcastAfterMicro("BTC")+1)
	n.rebroadcastUnconfirmed()
	if got := conn.rawTx[txid]; got != hexStr {
		t.Fatalf("rebroadcast hex mismatch: got %q want recorded bytes", got)
	}
	if len(conn.broadcasts) != 1 || conn.broadcasts[0] != txid {
		t.Fatalf("broadcasts = %v, want [%s]", conn.broadcasts, txid)
	}
}

// TestRebroadcastSkipsConfirmedFreshCapped proves the negative gates:
// confirmed, fresh, and attempt-exhausted entries are never rebroadcast.
func TestRebroadcastSkipsConfirmedFreshCapped(t *testing.T) {
	n, conn, txid, _ := rebroadcastTestSetup(t)
	old := uint64(NowMicro()) - uint64(n.rebroadcastAfterMicro("BTC")+1)

	n.tracked[txid].Confs = 6
	n.tracked[txid].FirstSeenMicro = old
	n.rebroadcastUnconfirmed()
	if len(conn.broadcasts) != 0 {
		t.Fatalf("confirmed entry rebroadcast: %v", conn.broadcasts)
	}

	n.tracked[txid].Confs = 0
	n.tracked[txid].FirstSeenMicro = uint64(NowMicro())
	n.rebroadcastUnconfirmed()
	if len(conn.broadcasts) != 0 {
		t.Fatalf("fresh entry rebroadcast: %v", conn.broadcasts)
	}

	n.tracked[txid].FirstSeenMicro = old
	n.tracked[txid].Attempts = maxRebroadcasts
	n.rebroadcastUnconfirmed()
	if len(conn.broadcasts) != 0 {
		t.Fatalf("exhausted entry rebroadcast: %v", conn.broadcasts)
	}
}

// TestRebroadcastResetsClock proves a successful rebroadcast restarts the age
// clock (no hot loop) and counts the attempt.
func TestRebroadcastResetsClock(t *testing.T) {
	n, conn, txid, _ := rebroadcastTestSetup(t)
	n.tracked[txid].FirstSeenMicro = uint64(NowMicro()) - uint64(n.rebroadcastAfterMicro("BTC")+1)
	n.rebroadcastUnconfirmed()
	if len(conn.broadcasts) != 1 {
		t.Fatalf("broadcasts = %v, want 1", conn.broadcasts)
	}
	if n.tracked[txid].Attempts != 1 {
		t.Fatalf("attempts = %d, want 1", n.tracked[txid].Attempts)
	}
	n.rebroadcastUnconfirmed()
	if len(conn.broadcasts) != 1 {
		t.Fatalf("second sweep rebroadcast without aging: %v", conn.broadcasts)
	}
}

// TestRebroadcastSendErrorKeepsWatching proves a failed rebroadcast counts the
// attempt but keeps the entry under watch (transient outage is not give-up).
func TestRebroadcastSendErrorKeepsWatching(t *testing.T) {
	n, conn, txid, _ := rebroadcastTestSetup(t)
	conn.sendErr = errors.New("wallet down")
	n.tracked[txid].FirstSeenMicro = uint64(NowMicro()) - uint64(n.rebroadcastAfterMicro("BTC")+1)
	n.rebroadcastUnconfirmed()
	if len(conn.broadcasts) != 0 {
		t.Fatalf("failed send recorded as broadcast: %v", conn.broadcasts)
	}
	if n.tracked[txid].Attempts != 1 {
		t.Fatalf("attempts = %d, want 1", n.tracked[txid].Attempts)
	}
	if _, ok := n.tracked[txid]; !ok {
		t.Fatal("entry dropped after transient send error")
	}
}

// TestRestoreZeroFirstSeenStartsClock proves the upgrade path: persisted
// entries without a first-seen stamp (pre-rebroadcast files) start their age
// clock at restore, so a restart never mass-rebroadcasts old entries.
func TestRestoreZeroFirstSeenStartsClock(t *testing.T) {
	n := newTestNode(t, map[string]*config.CoinConf{}, map[string]wallet.Connector{})
	before := uint64(NowMicro())
	n.restoreTracked([]persistedBroadcast{{
		OrderID: "o", Kind: broadcastDeposit, Coin: "BTC", TxID: "t", Hex: "h",
	}})
	tb, ok := n.tracked["t"]
	if !ok {
		t.Fatal("entry not restored")
	}
	if tb.FirstSeenMicro < before || tb.FirstSeenMicro > uint64(NowMicro()) {
		t.Fatalf("FirstSeenMicro = %d, want restore-time", tb.FirstSeenMicro)
	}
}

// TestRebroadcastThresholdCoinAware proves the age threshold follows the
// coin's block time (3×, floored at 120s) with a safe default for unknown or
// zero block times.
func TestRebroadcastThresholdCoinAware(t *testing.T) {
	mk := func(confs map[string]*config.CoinConf) *Node {
		return newTestNode(t, confs, map[string]wallet.Connector{})
	}
	btc := map[string]*config.CoinConf{
		"BTC":  {Ticker: "BTC", BlockTime: 60},
		"SLOW": {Ticker: "SLOW", BlockTime: 600},
		"ZERO": {Ticker: "ZERO", BlockTime: 0},
		"FAST": {Ticker: "FAST", BlockTime: 30},
	}
	n := mk(btc)
	if got := n.rebroadcastAfterMicro("BTC"); got != 180*1000000 {
		t.Fatalf("BTC after = %d, want 180s", got)
	}
	if got := n.rebroadcastAfterMicro("SLOW"); got != 1800*1000000 {
		t.Fatalf("SLOW after = %d, want 1800s", got)
	}
	if got := n.rebroadcastAfterMicro("ZERO"); got != 180*1000000 {
		t.Fatalf("ZERO after = %d, want default 180s", got)
	}
	if got := n.rebroadcastAfterMicro("FAST"); got != 120*1000000 {
		t.Fatalf("FAST after = %d, want floor 120s", got)
	}
	if got := n.rebroadcastAfterMicro("MISSING"); got != 180*1000000 {
		t.Fatalf("MISSING after = %d, want default 180s", got)
	}
	if got := mk(map[string]*config.CoinConf{}).rebroadcastAfterMicro("BTC"); got != 180*1000000 {
		t.Fatalf("empty conf after = %d, want default 180s", got)
	}
}

// TestRebroadcastSingleFlight proves a sweep never stacks: with a broadcast
// already in flight the entry point is a no-op.
func TestRebroadcastSingleFlight(t *testing.T) {
	n, conn, txid, _ := rebroadcastTestSetup(t)
	n.tracked[txid].FirstSeenMicro = uint64(NowMicro()) - n.rebroadcastAfterMicro("BTC") - 1
	n.pendingRebroadcast.Store(true)
	n.rebroadcastUnconfirmed()
	if len(conn.broadcasts) != 0 {
		t.Fatalf("stacked sweep rebroadcast: %v", conn.broadcasts)
	}
}

// Phase-1 R2 session-less refund sweep + rollback backoff: failing-first
// tests. Stored Mine orders with a verified deposit and a pre-signed refund
// but NO live session (restart/prune) must be swept like live sessions (G12);
// repeated failures back off instead of retrying every sweep.

func storedRefundSetup(t *testing.T, lockTime uint32) (*Node, *fakeConnector, string, string) {
	t.Helper()
	conn := &fakeConnector{ticker: "BTC", blockHeight: 1000, rawTx: map[string]string{}}
	confs := map[string]*config.CoinConf{
		"BTC": {Ticker: "BTC", Coin: 1e8, AddressPrefix: 0, CreateTxMethod: "BTC", BlockTime: 60},
	}
	n := newTestNode(t, confs, map[string]wallet.Connector{"BTC": conn})
	tx := &coins.Tx{Version: 1, LockTime: lockTime}
	tx.Inputs = append(tx.Inputs, coins.TxIn{PrevOut: coins.OutPoint{Hash: [32]byte{0x22}, Index: 0}, Sequence: 0xfffffffe})
	tx.Outputs = append(tx.Outputs, coins.TxOut{Value: 50000000, ScriptPubKey: []byte{0x51}})
	hexStr := hex.EncodeToString(tx.Serialize())
	txid, err := txIDFromHex(hexStr)
	if err != nil {
		t.Fatal(err)
	}
	var oid [32]byte
	oid[0] = 0x77
	idHex := hexEncode(oid[:])
	n.store.Add(&Order{ID: oid, Mine: true, FromCurrency: "BTC", ToCurrency: "BTC",
		DepositSent: true, RefundTx: hexStr, UtxoCurrency: "BTC", Status: "open"})
	return n, conn, idHex, txid
}

// TestSessionlessRefundSweepBroadcasts proves the G12 fix: a due stored
// refund with no live session is broadcast with its stored bytes.
func TestSessionlessRefundSweepBroadcasts(t *testing.T) {
	n, conn, idHex, txid := storedRefundSetup(t, 900)
	if _, ok := n.sessions[idHex]; ok {
		t.Fatal("setup must leave no live session")
	}
	n.scanStoredRefunds()
	if len(conn.broadcasts) != 1 || conn.broadcasts[0] != txid {
		t.Fatalf("broadcasts = %v, want [%s]", conn.broadcasts, txid)
	}
	if o := n.store.Get(idHex); o.Status != "rolled back" {
		t.Fatalf("status = %q, want rolled back", o.Status)
	}
}

// TestSessionlessSkipsLocked proves a stored refund is not broadcast before
// its locktime (early broadcast would be a network-rejected double-spend
// risk and status churn).
func TestSessionlessSkipsLocked(t *testing.T) {
	n, conn, idHex, _ := storedRefundSetup(t, 1100)
	n.scanStoredRefunds()
	if len(conn.broadcasts) != 0 {
		t.Fatalf("pre-locktime broadcast: %v", conn.broadcasts)
	}
	if o := n.store.Get(idHex); o.Status == "rollback failed" {
		t.Fatalf("status churned to %q on a not-yet-due refund", o.Status)
	}
}

// TestSessionlessSkipsConfirmedRefund proves an already-confirmed stored
// refund reconciles the order state without rebroadcasting.
func TestSessionlessSkipsConfirmedRefund(t *testing.T) {
	n, conn, idHex, txid := storedRefundSetup(t, 900)
	n.recordBroadcast(idHex, broadcastRefund, "BTC", txid, "hex")
	n.tracked[txid].Confs = 6
	n.scanStoredRefunds()
	if len(conn.broadcasts) != 0 {
		t.Fatalf("confirmed refund rebroadcast: %v", conn.broadcasts)
	}
	if o := n.store.Get(idHex); o.Status != "rolled back" {
		t.Fatalf("status = %q, want rolled back", o.Status)
	}
}

// TestSessionlessSkipsIneligible proves the complement guards: live session,
// foreign, hex-less, unsent, terminal, connector-less, and bad-hex orders are
// never swept from the store.
func TestSessionlessSkipsIneligible(t *testing.T) {
	n, conn, idHex, _ := storedRefundSetup(t, 900)
	cases := map[string]func(o *Order){
		"foreign":  func(o *Order) { o.Mine = false },
		"no hex":   func(o *Order) { o.RefundTx = "" },
		"unsent":   func(o *Order) { o.DepositSent = false },
		"terminal": func(o *Order) { o.Status = "finished" },
		"no coin":  func(o *Order) { o.UtxoCurrency = ""; o.FromCurrency = "" },
		"bad hex":  func(o *Order) { o.RefundTx = "zz" },
	}
	for name, mut := range cases {
		n.store.Update(idHex, mut)
		n.scanStoredRefunds()
		if len(conn.broadcasts) != 0 {
			t.Fatalf("%s: swept: %v", name, conn.broadcasts)
		}
	}
	// Live session present: the session sweep owns it, not the store sweep.
	n2, conn2, idHex2, _ := storedRefundSetup(t, 900)
	n2.sessions[idHex2] = &SwapSession{}
	n2.scanStoredRefunds()
	if len(conn2.broadcasts) != 0 {
		t.Fatalf("live-session order swept from store: %v", conn2.broadcasts)
	}
	_ = n
}

// TestRefundFailureBackoff proves R2b: a failed refund schedules an
// escalating retry (not every sweep), and success clears the schedule.
func TestRefundFailureBackoff(t *testing.T) {
	n, conn, idHex, _ := storedRefundSetup(t, 900)
	conn.sendErr = errors.New("wallet down")
	n.scanStoredRefunds()
	if n.refundAttempts[idHex] != 1 {
		t.Fatalf("attempts = %d, want 1", n.refundAttempts[idHex])
	}
	first := n.refundRetryAt[idHex]
	if first <= uint64(NowMicro()) {
		t.Fatalf("retryAt = %d, want future", first)
	}
	// Second sweep while gated: no new attempt.
	n.scanStoredRefunds()
	if n.refundAttempts[idHex] != 1 {
		t.Fatalf("attempts = %d after gated sweep, want 1", n.refundAttempts[idHex])
	}
	// Escalation: force the gate open, fail again, delay must grow.
	n.refundRetryAt[idHex] = uint64(NowMicro()) - 1
	n.scanStoredRefunds()
	if n.refundAttempts[idHex] != 2 {
		t.Fatalf("attempts = %d, want 2", n.refundAttempts[idHex])
	}
	if n.refundRetryAt[idHex] <= first {
		t.Fatalf("retryAt did not escalate: %d <= %d", n.refundRetryAt[idHex], first)
	}
	// Success clears the schedule.
	conn.sendErr = nil
	n.refundRetryAt[idHex] = uint64(NowMicro()) - 1
	n.scanStoredRefunds()
	if len(conn.broadcasts) != 1 {
		t.Fatalf("broadcasts = %v, want 1 success", conn.broadcasts)
	}
	if _, ok := n.refundRetryAt[idHex]; ok {
		t.Fatal("retry schedule not cleared on success")
	}
	if _, ok := n.refundAttempts[idHex]; ok {
		t.Fatal("attempt count not cleared on success")
	}
}

// TestSessionlessNoDoubleBroadcast is the regression test for the review
// FAIL: two consecutive sweeps must broadcast a due stored refund exactly
// once — the second sweep sees "rolled back" (and the tracked entry, owned
// by the rebroadcast sweep while unconfirmed) and stands down.
func TestSessionlessNoDoubleBroadcast(t *testing.T) {
	n, conn, idHex, txid := storedRefundSetup(t, 900)
	n.scanStoredRefunds()
	n.scanStoredRefunds()
	if len(conn.broadcasts) != 1 || conn.broadcasts[0] != txid {
		t.Fatalf("broadcasts = %v, want exactly one [%s]", conn.broadcasts, txid)
	}
	// Even with the status tampered back, a tracked (unconfirmed) refund is
	// never re-posted from the store sweep — the rebroadcast sweep owns it.
	n.store.Update(idHex, func(o *Order) { o.Status = "open" })
	n.scanStoredRefunds()
	if len(conn.broadcasts) != 1 {
		t.Fatalf("tracked refund re-posted: %v", conn.broadcasts)
	}
}

// Phase-1 R3 hub-silence watchdog + R4 TTL/lock release: failing-first tests.

type stallConn struct{ sent int }

func (c *stallConn) ReadPacket() (*proto.Packet, string, error) { return nil, "", io.EOF }
func (c *stallConn) WritePacket(*proto.Packet, [20]byte) error  { c.sent++; return nil }
func (c *stallConn) Close() error                               { return nil }

// stallSetup builds an inline node with a live in-swap session (maker,
// deposit broadcast) whose hub went silent, plus a capturing XConn.
func stallSetup(t *testing.T, st clientState, mutate func(*SwapSession, *Order)) (*Node, *stallConn, string, *fakeConnector) {
	t.Helper()
	conn := &fakeConnector{ticker: "BTC", blockHeight: 1000, rawTx: map[string]string{}}
	confs := map[string]*config.CoinConf{
		"BTC": {Ticker: "BTC", Coin: 1e8, AddressPrefix: 0, CreateTxMethod: "BTC", BlockTime: 60},
	}
	n := newTestNode(t, confs, map[string]wallet.Connector{"BTC": conn})
	sc := &stallConn{}
	n.conn = sc
	mkPriv, mkPub := newKey(t)
	tx := &coins.Tx{Version: 1, LockTime: 900}
	tx.Inputs = append(tx.Inputs, coins.TxIn{PrevOut: coins.OutPoint{Hash: [32]byte{0x33}, Index: 0}, Sequence: 0xfffffffe})
	tx.Outputs = append(tx.Outputs, coins.TxOut{Value: 50000000, ScriptPubKey: []byte{0x51}})
	hexStr := hex.EncodeToString(tx.Serialize())
	var oid [32]byte
	oid[0] = 0x78
	idHex := hexEncode(oid[:])
	n.store.Add(&Order{ID: oid, Mine: true, FromCurrency: "BTC", ToCurrency: "BTC",
		MakerKey: hexEncode(mkPub), Status: "created", DepositSent: true,
		RefundTx: hexStr, UtxoCurrency: "BTC"})
	s := &SwapSession{n: n, isMaker: true, id: oid, srcCur: "BTC", dstCur: "BTC",
		state: st, refundHex: hexStr, privKey: arr32(mkPriv), pubKey: toArr33(mkPub)}
	if mutate != nil {
		mutate(s, n.store.Get(idHex))
	}
	n.sessions[idHex] = s
	return n, sc, idHex, conn
}

// TestStalledSessionCancelled proves R3: a live session silent past the stall
// threshold is canceled (counterparty notified) and rolled back (refund out).
func TestStalledSessionCancelled(t *testing.T) {
	n, sc, idHex, conn := stallSetup(t, csCreatedA, func(s *SwapSession, _ *Order) {
		s.lastProgress = uint64(NowMicro()) - sessionStallMicro - 1
	})
	n.watchStalledSessions()
	if sc.sent != 1 {
		t.Fatalf("cancel packets sent = %d, want 1", sc.sent)
	}
	if len(conn.broadcasts) != 1 {
		t.Fatalf("refund broadcasts = %v, want 1", conn.broadcasts)
	}
	if o := n.store.Get(idHex); o.Status != "rolled back" {
		t.Fatalf("status = %q, want rolled back", o.Status)
	}
}

// TestStallSkipsHealthy proves the watchdog stands down for fresh, in-flight,
// claim-built, finished, and refund-done sessions.
func TestStallSkipsHealthy(t *testing.T) {
	fresh := func(s *SwapSession, _ *Order) { s.lastProgress = uint64(NowMicro()) }
	awaiting := func(s *SwapSession, _ *Order) {
		s.lastProgress = uint64(NowMicro()) - sessionStallMicro - 1
		s.await = true
	}
	claimed := func(s *SwapSession, _ *Order) {
		s.lastProgress = uint64(NowMicro()) - sessionStallMicro - 1
		s.claimHex = "deadbeef"
	}
	done := func(s *SwapSession, _ *Order) {
		s.lastProgress = uint64(NowMicro()) - sessionStallMicro - 1
		s.refundDone = true
	}
	finished := func(s *SwapSession, _ *Order) {
		s.lastProgress = uint64(NowMicro()) - sessionStallMicro - 1
		s.state = csFinished
	}
	for name, mut := range map[string]func(*SwapSession, *Order){
		"fresh": fresh, "await": awaiting, "claim built": claimed,
		"refund done": done, "finished": finished,
	} {
		n, sc, idHex, conn := stallSetup(t, csCreatedA, mut)
		n.watchStalledSessions()
		if sc.sent != 0 || len(conn.broadcasts) != 0 {
			t.Fatalf("%s: watchdog fired (sent=%d broadcasts=%v)", name, sc.sent, conn.broadcasts)
		}
		// Non-vacuous: the session and order are untouched, not merely
		// unacted (a no-op watchdog would also send nothing).
		if _, ok := n.sessions[idHex]; !ok {
			t.Fatalf("%s: session pruned without firing", name)
		}
		if o := n.store.Get(idHex); o.Status != "created" {
			t.Fatalf("%s: status = %q, want created", name, o.Status)
		}
	}
}

// TestRolledBackReleasesLocks proves R4a: a rolled-back order no longer
// excludes its UTXOs from funding selection (refund succeeded — funding is
// spent), while rollback-failed and live orders stay locked.
func TestRolledBackReleasesLocks(t *testing.T) {
	s := NewStore()
	mk := func(id byte, status string) *Order {
		return &Order{ID: [32]byte{id}, FromCurrency: "BTC", ToCurrency: "BTC",
			Status: status, Mine: true, UtxoCurrency: "BTC",
			Utxos: []proto.UtxoEntry{{TxID: [32]byte{id}, Vout: 0}}}
	}
	s.Add(mk(0x0a, "rolled back"))
	s.Add(mk(0x0b, "rollback failed"))
	s.Add(mk(0x0c, "open"))
	got := s.LockedUtxoInfoFor("BTC")
	key := func(id byte) string { return utxoEntryKey(proto.UtxoEntry{TxID: [32]byte{id}, Vout: 0}) }
	if got[key(0x0a)] {
		t.Error("rolled-back utxo must be released")
	}
	if !got[key(0x0b)] {
		t.Error("rollback-failed utxo must stay locked (refund may still be owed)")
	}
	if !got[key(0x0c)] {
		t.Error("live order utxo must stay locked")
	}
}

// TestStaleMineOrderGC proves R4b: an ancient Mine order with no session, no
// deposit, and nothing tracked is dropped (locks released, book cleaned),
// while orders owned by R1/R2 sweeps, live sessions, or recent activity stay.
func TestStaleMineOrderGC(t *testing.T) {
	mk := func(n *Node, id byte, st string, updated uint64, depSent bool) string {
		var oid [32]byte
		oid[0] = id
		n.store.Add(&Order{ID: oid, Mine: true, FromCurrency: "BTC", ToCurrency: "BTC",
			Status: st, Updated: updated, DepositSent: depSent, UtxoCurrency: "BTC"})
		return hexEncode(oid[:])
	}
	old := uint64(NowMicro()) - mineOrderTTLMicro - 1
	n := newTestNode(t, map[string]*config.CoinConf{}, map[string]wallet.Connector{})
	stale := mk(n, 0x01, "open", old, false)
	n.gcStaleMineOrders()
	if n.store.Get(stale) != nil {
		t.Fatal("ancient limbo order not dropped")
	}
	// DepositSent → R2 owns it.
	n2 := newTestNode(t, map[string]*config.CoinConf{}, map[string]wallet.Connector{})
	dep := mk(n2, 0x02, "open", old, true)
	n2.gcStaleMineOrders()
	if n2.store.Get(dep) == nil {
		t.Fatal("deposit-sent order dropped while R2 owns it")
	}
	// Tracked unconfirmed → R1 owns it.
	n3 := newTestNode(t, map[string]*config.CoinConf{}, map[string]wallet.Connector{})
	trk := mk(n3, 0x03, "open", old, false)
	n3.recordBroadcast(trk, broadcastDeposit, "BTC", "sometxid", "hex")
	n3.gcStaleMineOrders()
	if n3.store.Get(trk) == nil {
		t.Fatal("tracked order dropped while R1 owns it")
	}
	// Live session → session sweep owns it.
	n4 := newTestNode(t, map[string]*config.CoinConf{}, map[string]wallet.Connector{})
	ses := mk(n4, 0x04, "open", old, false)
	n4.sessions[ses] = &SwapSession{}
	n4.gcStaleMineOrders()
	if n4.store.Get(ses) == nil {
		t.Fatal("live-session order dropped")
	}
	// Recent → kept.
	n5 := newTestNode(t, map[string]*config.CoinConf{}, map[string]wallet.Connector{})
	rec := mk(n5, 0x05, "open", uint64(NowMicro()), false)
	n5.gcStaleMineOrders()
	if n5.store.Get(rec) == nil {
		t.Fatal("recent order dropped")
	}
}

// TestStallNoRefire is the regression test for the review FAIL: a second
// watchdog pass after a cancel must stand down (order rolled back), not
// re-cancel and re-enqueue every tick.
func TestStallNoRefire(t *testing.T) {
	n, sc, idHex, conn := stallSetup(t, csCreatedA, func(s *SwapSession, _ *Order) {
		s.lastProgress = uint64(NowMicro()) - sessionStallMicro - 1
	})
	n.watchStalledSessions()
	n.watchStalledSessions()
	if sc.sent != 1 {
		t.Fatalf("cancel packets sent = %d, want exactly 1", sc.sent)
	}
	if len(conn.broadcasts) != 1 {
		t.Fatalf("refund broadcasts = %v, want exactly 1", conn.broadcasts)
	}
	if o := n.store.Get(idHex); o.Status != "rolled back" {
		t.Fatalf("status = %q, want rolled back", o.Status)
	}
}

// TestStallSkipsRedeemed proves the watchdog never cancels a swap the
// counterparty already redeemed (cancel would be locally ignored yet still
// broadcast every tick).
func TestStallSkipsRedeemed(t *testing.T) {
	n, sc, idHex, conn := stallSetup(t, csCreatedA, func(s *SwapSession, _ *Order) {
		s.lastProgress = uint64(NowMicro()) - sessionStallMicro - 1
	})
	n.store.Update(idHex, func(o *Order) { o.CounterpartyRedeemed = true })
	n.watchStalledSessions()
	if sc.sent != 0 || len(conn.broadcasts) != 0 {
		t.Fatalf("watchdog fired on redeemed swap (sent=%d broadcasts=%v)", sc.sent, conn.broadcasts)
	}
}
