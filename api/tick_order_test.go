package api

// Tick-ordering pin: the 60 s engine tick MUST run recovery sweeps before
// termination sweeps. Both sides act on the same sessions and the only thing
// stopping them from fighting is `await` + call sequence: a retry sweep that
// runs first sets `await`/refreshes progress so the watchdog later in the
// SAME tick stands down. Reverse the order and a session with a due retry
// gets canceled (crTimeout + wire-Cancel) instead of healed.
//
// This test pins the contract two ways: the stage table order itself
// (reordering engine.go breaks it loudly instead of shipping a silent race),
// and the behavioral outcome (retry wins, no Cancel sent).

import (
	"encoding/hex"
	"strings"
	"testing"

	"go-xbridge/coins"
	"go-xbridge/config"
	"go-xbridge/proto"
	"go-xbridge/wallet"
)

// TestTickStageOrderRecoveryBeforeTermination pins the exact tick sequence:
// every recovery sweep precedes every termination sweep, and the persist
// flush stays last (crash-resumability: flush after all mutations).
func TestTickStageOrderRecoveryBeforeTermination(t *testing.T) {
	n := newTestNode(t, map[string]*config.CoinConf{}, map[string]wallet.Connector{})
	var names []string
	for _, st := range n.tickStages() {
		names = append(names, st.name)
	}
	want := []string{
		"clearStuckAwait",
		"scanRefunds", "scanStoredRefunds",
		"resendHoldApplies", "retryFailedClaimBuilds", "retryFailedDepositBuilds",
		"recordSilentHubs", "watchStalledSessions", "gcStaleMineOrders",
		"watchCounterpartyDeposits", "pollBroadcastConfirmations", "rebroadcastUnconfirmed",
		"pruneTracked", "pruneSessions", "persist",
	}
	if len(names) != len(want) {
		t.Fatalf("tick stages = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("tick stages = %v, want %v", names, want)
		}
	}
	recovery := []string{"resendHoldApplies", "retryFailedClaimBuilds", "retryFailedDepositBuilds"}
	termination := []string{"watchStalledSessions", "watchCounterpartyDeposits", "gcStaleMineOrders", "pruneSessions"}
	pos := map[string]int{}
	for i, name := range names {
		pos[name] = i
	}
	for _, rec := range recovery {
		for _, term := range termination {
			if pos[rec] >= pos[term] {
				t.Fatalf("recovery stage %q (index %d) must precede termination stage %q (index %d)",
					rec, pos[rec], term, pos[term])
			}
		}
	}
}

// TestTickRetryBeatsWatchdog pins the behavioral outcome: a session that is
// BOTH retry-due and watchdog-eligible must be rebuilt, never canceled. The
// stages run in tick-table order; if the watchdog ran first it would
// wire-Cancel a recoverable swap.
func TestTickRetryBeatsWatchdog(t *testing.T) {
	if err := coins.InitFromConf(map[string]*config.CoinConf{
		"BTC": {Ticker: "BTC", Title: "Bitcoin", Coin: 1e8, AddressPrefix: 0, ScriptPrefix: 5, CreateTxMethod: "BTC", BlockTime: 60},
		"LTC": {Ticker: "LTC", Title: "Litecoin", Coin: 1e8, AddressPrefix: 48, ScriptPrefix: 50, CreateTxMethod: "LTC", BlockTime: 60},
	}); err != nil {
		t.Fatal(err)
	}
	// Taker deposits LTC; its BTC connector is BLIND to the maker's deposit,
	// so every build fails transiently and reschedules (backend lag).
	btcFundingPriv, btcFundingPub := newKey(t)
	btcFunding := wallet.Utxo{TxID: strings.Repeat("aa", 32), Vout: 0, Amount: 5e8,
		ScriptPubKey: hex.EncodeToString(coins.BuildP2PKHScript(coins.KeyID(btcFundingPub)))}
	btcConn := &fakeConnector{ticker: "BTC", funding: btcFunding, fundingPriv: btcFundingPriv,
		fundingPub: btcFundingPub, changeAddr: addrFor(0, "btc-change"), blockHeight: 1000,
		rawTx: map[string]string{}, confirmations: map[string]int{}}
	ltcFundingPriv, ltcFundingPub := newKey(t)
	ltcFunding := wallet.Utxo{TxID: strings.Repeat("bb", 32), Vout: 0, Amount: 5e8,
		ScriptPubKey: hex.EncodeToString(coins.BuildP2PKHScript(coins.KeyID(ltcFundingPub)))}
	ltcConn := &fakeConnector{ticker: "LTC", funding: ltcFunding, fundingPriv: ltcFundingPriv,
		fundingPub: ltcFundingPub, changeAddr: addrFor(48, "ltc-change"), blockHeight: 1000,
		rawTx: map[string]string{}, confirmations: map[string]int{}}
	confs := map[string]*config.CoinConf{
		"BTC": {Ticker: "BTC", Coin: 1e8, AddressPrefix: 0, CreateTxMethod: "BTC", BlockTime: 60},
		"LTC": {Ticker: "LTC", Coin: 1e8, AddressPrefix: 48, CreateTxMethod: "LTC", BlockTime: 60},
	}
	n := newTestNode(t, confs, map[string]wallet.Connector{"BTC": btcConn, "LTC": ltcConn})
	cc := &captureXConn{}
	n.conn = cc

	tkPriv, tkPub := newKey(t)
	_, mkPub := newKey(t)
	var orderID [32]byte
	oid := hash20("retry-beats-watchdog-order")
	copy(orderID[:], oid[:])
	takerOrder := &Order{ID: orderID, FromCurrency: "BTC", ToCurrency: "LTC",
		FromAmount: 2.5e6, ToAmount: 2e6, Status: "accepting"}
	n.newTakerSession(withUsedCoins(t, n, takerOrder, []wallet.Utxo{ltcFunding}),
		TakeOrderParams{FromAddress: addrFor(48, "taker-ltc"), ToAddress: addrFor(0, "taker-btc")},
		arr32(tkPriv), to33(tkPub))
	s := n.sessions[hexEncode(orderID[:])]
	// Complete pre-deposit pointers (as a CreateB would leave them), retry
	// due, but silent past the 30-minute watchdog threshold: BOTH loops are
	// eligible on the next tick.
	s.theirPub = to33(mkPub)
	s.theirDepositTxID = strings.Repeat("dd", 32)
	s.theirSecretHash = hash20("secret-hash")
	s.theirLockTime = 1115 // inside the drift window (expectation 1000+120)
	s.state = csInitialized
	s.depositRetryAt = 1
	s.lastProgress = uint64(int64(NowMicro()) - 31*60*1000000)

	// Run the tick stages in table order (inline/engine-stopped = synchronous).
	for _, st := range n.tickStages() {
		st.run()
	}
	// The retry must have run (rescheduled after the transient build
	// failure) and the watchdog must NOT have canceled.
	if s.depositRetryAt == 0 {
		t.Fatal("retry sweep did not run: no retry scheduled")
	}
	for _, p := range cc.snapshot() {
		if p.Command == proto.XbcTransactionCancel {
			t.Fatal("watchdog canceled a session with a due retry (retry must run first)")
		}
	}
	if o := n.store.Get(hexEncode(orderID[:])); o == nil {
		t.Fatal("order removed from live store (watchdog canceled it)")
	} else if isOrderTerminal(o.Status) {
		t.Fatalf("order terminal (%q): watchdog canceled it", o.Status)
	}
}
