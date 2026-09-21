package api

// Scripted-hub kill-matrix sequencer (Phase 2 test infrastructure).
//
// TestSwapHandshake already drives the full bilateral swap, but through
// direct OnXxx injection — bypassing processSwap's hub-signature verify,
// session hub-key pinning, await guards, and the engine/worker split. The
// hubSequencer below replays the identical handshake through the real
// dispatch path on started nodes (submit → processSwap → worker → resume →
// captureXConn), relaying each response's wire-decoded body into the next
// step's packet — the closest a harness gets to a live hub without one.
// Every step is a halt point: the matrix test stops after each phase and
// asserts the terminal/progress state, and the tick-driver test runs the
// real tickStages() table over halted swaps to prove recovery-before-
// termination instead of asserting piecemeal sweeps.
//
// No production code is exercised differently than production exercises it:
// packets are hub-signed, bodies are wire-decoded, broadcasts hit the shared
// fake connectors' chain view. All fixtures are in-memory — no live hub.

import (
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"go-xbridge/coins"
	"go-xbridge/config"
	"go-xbridge/crypto"
	"go-xbridge/proto"
	"go-xbridge/wallet"
)

// hubSequencer is a bilateral maker/taker pair driven by a scripted hub key.
// Both nodes share one connector per coin so the chain view is consistent
// (the taker sees the maker's payTx, as on a live network).
type hubSequencer struct {
	t         *testing.T
	makerNode *Node
	takerNode *Node
	makerCC   *captureXConn
	takerCC   *captureXConn
	hubPriv   []byte
	hub       [20]byte
	orderID   [32]byte
	tkPub     []byte
}

// newHubSequencer builds the pair with sessions created but the hub silent:
// no packet has been delivered yet. Nodes start their engines on call.
func newHubSequencer(t *testing.T) *hubSequencer {
	t.Helper()
	if err := coins.InitFromConf(map[string]*config.CoinConf{
		"BTC": {Ticker: "BTC", Title: "Bitcoin", Coin: 1e8, AddressPrefix: 0, ScriptPrefix: 5, CreateTxMethod: "BTC", BlockTime: 60},
		"LTC": {Ticker: "LTC", Title: "Litecoin", Coin: 1e8, AddressPrefix: 48, ScriptPrefix: 50, CreateTxMethod: "LTC", BlockTime: 60},
	}); err != nil {
		t.Fatal(err)
	}
	btcFundingPriv, btcFundingPub := newKey(t)
	btcFunding := wallet.Utxo{TxID: strings.Repeat("aa", 32), Vout: 0, Amount: 5e8,
		ScriptPubKey: hex.EncodeToString(coins.BuildP2PKHScript(coins.KeyID(btcFundingPub)))}
	btcConn := &fakeConnector{ticker: "BTC", funding: btcFunding, fundingPriv: btcFundingPriv,
		fundingPub: btcFundingPub, changeAddr: addrFor(0, "btc-change"), blockHeight: 1000,
		rawTx: map[string]string{}}
	ltcFundingPriv, ltcFundingPub := newKey(t)
	ltcFunding := wallet.Utxo{TxID: strings.Repeat("bb", 32), Vout: 0, Amount: 5e8,
		ScriptPubKey: hex.EncodeToString(coins.BuildP2PKHScript(coins.KeyID(ltcFundingPub)))}
	ltcConn := &fakeConnector{ticker: "LTC", funding: ltcFunding, fundingPriv: ltcFundingPriv,
		fundingPub: ltcFundingPub, changeAddr: addrFor(48, "ltc-change"), blockHeight: 1000,
		rawTx: map[string]string{}}
	conns := map[string]wallet.Connector{"BTC": btcConn, "LTC": ltcConn}
	confs := map[string]*config.CoinConf{
		"BTC": {Ticker: "BTC", Coin: 1e8, AddressPrefix: 0, CreateTxMethod: "BTC", BlockTime: 60},
		"LTC": {Ticker: "LTC", Coin: 1e8, AddressPrefix: 48, CreateTxMethod: "LTC", BlockTime: 60},
	}

	hubPriv := make([]byte, 32)
	hubPriv[31] = 7
	hubPub, err := crypto.CompressedPubKey(hubPriv)
	if err != nil {
		t.Fatal(err)
	}
	hub := coins.KeyID(hubPub[:])

	var orderID [32]byte
	oid := hash20("sequencer-order")
	copy(orderID[:], oid[:])

	mkMPriv, mkMPub := newKey(t)
	tkPriv, tkPub := newKey(t)
	mkAddr := addrFor(0, "maker-btc-dest")
	ltcAddr := addrFor(48, "taker-ltc-source")

	makerNode := newTestNode(t, confs, conns)
	takerNode := newTestNode(t, confs, conns)
	registerHub(t, makerNode, hubPriv)
	registerHub(t, takerNode, hubPriv)
	makerOrder := &Order{ID: orderID, FromCurrency: "BTC", ToCurrency: "LTC",
		FromAmount: 2.5e6, ToAmount: 2e6,
		SNodePubkey: hex.EncodeToString(hubPub[:]), HubAddress: coins.KeyID(hubPub[:])}
	takerOrder := &Order{ID: orderID, FromCurrency: "BTC", ToCurrency: "LTC",
		FromAmount: 2.5e6, ToAmount: 2e6,
		SNodePubkey: hex.EncodeToString(hubPub[:]), HubAddress: coins.KeyID(hubPub[:])}
	makerNode.newMakerSession(
		withUsedCoins(t, makerNode, makerOrder, []wallet.Utxo{btcFunding}),
		MakeOrderParams{MakerAddress: mkAddr, TakerAddress: ltcAddr},
		arr32(mkMPriv), toArr33(mkMPub))
	takerNode.newTakerSession(
		withUsedCoins(t, takerNode, takerOrder, []wallet.Utxo{ltcFunding}),
		TakeOrderParams{FromAddress: ltcAddr, ToAddress: mkAddr},
		arr32(tkPriv), toArr33(tkPub))
	makerNode.sessions[hexEncode(orderID[:])].hub = hub
	takerNode.sessions[hexEncode(orderID[:])].hub = hub

	// Sessions are created before the engines start so no off-engine map
	// write races a resume.
	makerCC := &captureXConn{}
	takerCC := &captureXConn{}
	makerNode.conn = makerCC
	takerNode.conn = takerCC
	makerNode.start()
	takerNode.start()
	t.Cleanup(func() { _ = makerNode.Close() })
	t.Cleanup(func() { _ = takerNode.Close() })

	return &hubSequencer{t: t, makerNode: makerNode, takerNode: takerNode,
		makerCC: makerCC, takerCC: takerCC, hubPriv: hubPriv, hub: hub,
		orderID: orderID, tkPub: tkPub}
}

// deliver injects one hub-signed packet through a node's real dispatch path.
// The same body instance drives the wire signature and the handler call, so
// what the handler sees is exactly what the hub sent.
func (q *hubSequencer) deliver(n *Node, cmd proto.XBridgeCommand, cmdName string, body interface{ Marshal() []byte }, fn func(*SwapSession) (proto.XBridgeCommand, responseBody, error)) {
	q.t.Helper()
	pkt := hubSignedPkt(q.t, q.hubPriv, cmd, body)
	n.submit(func() { n.processSwap(pkt, q.orderID, [20]byte{}, cmdName, fn) }, true)
}

// awaitResponse waits for the async worker resume to send the response.
func (q *hubSequencer) awaitResponse(cc *captureXConn, cmd proto.XBridgeCommand) *proto.Packet {
	q.t.Helper()
	return waitForPacket(q.t, cc, cmd, 5*time.Second)
}

// hold delivers Hold to both roles; both must answer HoldApply.
func (q *hubSequencer) hold() {
	q.t.Helper()
	body := &proto.HoldBody{HubAddress: q.hub, ID: q.orderID, FromAmount: 2e6, ToAmount: 2.5e6}
	q.deliver(q.makerNode, proto.XbcTransactionHold, "Hold", body,
		func(s *SwapSession) (proto.XBridgeCommand, responseBody, error) { return s.OnHold(body) })
	q.deliver(q.takerNode, proto.XbcTransactionHold, "Hold", body,
		func(s *SwapSession) (proto.XBridgeCommand, responseBody, error) { return s.OnHold(body) })
	q.awaitResponse(q.makerCC, proto.XbcTransactionHoldApply)
	q.awaitResponse(q.takerCC, proto.XbcTransactionHoldApply)
}

// init delivers Init to both roles; both must answer Initialized.
func (q *hubSequencer) init() {
	q.t.Helper()
	ltcHash := hash20("taker-ltc-source")
	btcHash := hash20("maker-btc-dest")
	mkBody := &proto.InitBody{ClientAddress: ltcHash, HubAddress: q.hub, ID: q.orderID,
		FromAddress: hash20("maker-btc-dest"), FromCurrency: "BTC", FromAmount: 2.5e6,
		ToAddress: hash20("taker-ltc-source"), ToCurrency: "LTC", ToAmount: 2e6}
	tkBody := &proto.InitBody{ClientAddress: btcHash, HubAddress: q.hub, ID: q.orderID,
		FromAddress: hash20("taker-ltc-source"), FromCurrency: "LTC", FromAmount: 2e6,
		ToAddress: hash20("maker-btc-dest"), ToCurrency: "BTC", ToAmount: 2.5e6}
	q.deliver(q.makerNode, proto.XbcTransactionInit, "Init", mkBody,
		func(s *SwapSession) (proto.XBridgeCommand, responseBody, error) { return s.OnInit(mkBody) })
	q.deliver(q.takerNode, proto.XbcTransactionInit, "Init", tkBody,
		func(s *SwapSession) (proto.XBridgeCommand, responseBody, error) { return s.OnInit(tkBody) })
	q.awaitResponse(q.makerCC, proto.XbcTransactionInitialized)
	q.awaitResponse(q.takerCC, proto.XbcTransactionInitialized)
}

// createARelay delivers CreateA to the maker and returns the wire-decoded
// CreatedA response the worker resume sends.
func (q *hubSequencer) createARelay() *proto.CreatedABody {
	q.t.Helper()
	body := &proto.CreateABody{HubAddress: q.hub, ID: q.orderID, BPubKey: toArr33(q.tkPub)}
	q.deliver(q.makerNode, proto.XbcTransactionCreateA, "CreateA", body,
		func(s *SwapSession) (proto.XBridgeCommand, responseBody, error) { return s.OnCreateA(body) })
	pkt := q.awaitResponse(q.makerCC, proto.XbcTransactionCreatedA)
	dec, err := proto.DecodeBody(pkt.Command, pkt.Body)
	if err != nil {
		q.t.Fatalf("decode CreatedA response: %v", err)
	}
	createdA, ok := dec.(*proto.CreatedABody)
	if !ok {
		q.t.Fatalf("CreatedA response = %T", dec)
	}
	if createdA.ADepositTxID == "" {
		q.t.Fatal("empty maker deposit txid")
	}
	return createdA
}

// createBRelay delivers CreateB to the taker and returns the wire-decoded
// CreatedB response.
func (q *hubSequencer) createBRelay(createdA *proto.CreatedABody) *proto.CreatedBBody {
	q.t.Helper()
	makerPub := readOnEngine(q.t, q.makerNode, func() [33]byte {
		return q.makerNode.sessions[hexEncode(q.orderID[:])].pubkey()
	})
	body := &proto.CreateBBody{HubAddress: q.hub, ID: q.orderID, APubKey: makerPub,
		ADepositTxID: createdA.ADepositTxID, HashedSecret: createdA.HashedSecret, ALockTime: createdA.ALockTime}
	q.deliver(q.takerNode, proto.XbcTransactionCreateB, "CreateB", body,
		func(s *SwapSession) (proto.XBridgeCommand, responseBody, error) { return s.OnCreateB(body) })
	pkt := q.awaitResponse(q.takerCC, proto.XbcTransactionCreatedB)
	dec, err := proto.DecodeBody(pkt.Command, pkt.Body)
	if err != nil {
		q.t.Fatalf("decode CreatedB response: %v", err)
	}
	createdB, ok := dec.(*proto.CreatedBBody)
	if !ok {
		q.t.Fatalf("CreatedB response = %T", dec)
	}
	if createdB.BDepositTxID == "" {
		q.t.Fatal("empty taker deposit txid")
	}
	return createdB
}

// confirmARelay delivers ConfirmA to the maker and returns the wire-decoded
// ConfirmedA response carrying the maker's payTx (secret now public).
func (q *hubSequencer) confirmARelay(createdB *proto.CreatedBBody) *proto.ConfirmedABody {
	q.t.Helper()
	body := &proto.ConfirmABody{HubAddress: q.hub, ID: q.orderID,
		BDepositTxID: createdB.BDepositTxID, BLockTime: createdB.BLockTime}
	q.deliver(q.makerNode, proto.XbcTransactionConfirmA, "ConfirmA", body,
		func(s *SwapSession) (proto.XBridgeCommand, responseBody, error) { return s.OnConfirmA(body) })
	pkt := q.awaitResponse(q.makerCC, proto.XbcTransactionConfirmedA)
	dec, err := proto.DecodeBody(pkt.Command, pkt.Body)
	if err != nil {
		q.t.Fatalf("decode ConfirmedA response: %v", err)
	}
	confirmedA, ok := dec.(*proto.ConfirmedABody)
	if !ok {
		q.t.Fatalf("ConfirmedA response = %T", dec)
	}
	if confirmedA.APayTxID == "" {
		q.t.Fatal("empty maker payTx id")
	}
	return confirmedA
}

// confirmBRelay delivers ConfirmB to the taker and returns the wire-decoded
// ConfirmedB response.
func (q *hubSequencer) confirmBRelay(confirmedA *proto.ConfirmedABody) *proto.ConfirmedBBody {
	q.t.Helper()
	body := &proto.ConfirmBBody{HubAddress: q.hub, ID: q.orderID, APayTxID: confirmedA.APayTxID}
	q.deliver(q.takerNode, proto.XbcTransactionConfirmB, "ConfirmB", body,
		func(s *SwapSession) (proto.XBridgeCommand, responseBody, error) { return s.OnConfirmB(body) })
	pkt := q.awaitResponse(q.takerCC, proto.XbcTransactionConfirmedB)
	dec, err := proto.DecodeBody(pkt.Command, pkt.Body)
	if err != nil {
		q.t.Fatalf("decode ConfirmedB response: %v", err)
	}
	confirmedB, ok := dec.(*proto.ConfirmedBBody)
	if !ok {
		q.t.Fatalf("ConfirmedB response = %T", dec)
	}
	if confirmedB.BPayTxID == "" {
		q.t.Fatal("empty taker payTx id")
	}
	return confirmedB
}

// finish delivers Finished to both roles. Unlike every other relay there is
// no response to await: OnFinished answers nothing on the wire. Ordering is
// still deterministic — delivering through submit(..., true) plus the
// caller's readOnEngine forms the barrier — but if OnFinished ever gains
// worker work, this step needs an explicit completion signal like the rest.
func (q *hubSequencer) finish() {
	q.t.Helper()
	body := &proto.FinishedBody{ID: q.orderID}
	q.deliver(q.makerNode, proto.XbcTransactionFinished, "Finished", body,
		func(s *SwapSession) (proto.XBridgeCommand, responseBody, error) { return s.OnFinished(body) })
	q.deliver(q.takerNode, proto.XbcTransactionFinished, "Finished", body,
		func(s *SwapSession) (proto.XBridgeCommand, responseBody, error) { return s.OnFinished(body) })
}

// makerState / takerState read engine-owned session state without racing a
// worker resume.
func (q *hubSequencer) makerState() clientState {
	q.t.Helper()
	return readOnEngine(q.t, q.makerNode, func() clientState {
		return q.makerNode.sessions[hexEncode(q.orderID[:])].state
	})
}

// takerState reads the taker session state on the engine.
func (q *hubSequencer) takerState() clientState {
	q.t.Helper()
	return readOnEngine(q.t, q.takerNode, func() clientState {
		return q.takerNode.sessions[hexEncode(q.orderID[:])].state
	})
}

// TestSequencerFullSwapThroughDispatch replays the entire bilateral swap
// through the real dispatch path: every packet is hub-signed, verified, and
// routed by processSwap, and every relay value is wire-decoded from the
// worker resume's response. Both sides must finish with history entries.
func TestSequencerFullSwapThroughDispatch(t *testing.T) {
	q := newHubSequencer(t)
	idHex := hexEncode(q.orderID[:])

	q.hold()
	if got := q.makerState(); got != csHoldApplied {
		t.Fatalf("maker state after Hold = %s, want csHoldApplied", got.String())
	}
	if got := q.takerState(); got != csHoldApplied {
		t.Fatalf("taker state after Hold = %s, want csHoldApplied", got.String())
	}

	q.init()
	if got := q.makerState(); got != csInitialized {
		t.Fatalf("maker state after Init = %s, want csInitialized", got.String())
	}
	if got := q.takerState(); got != csInitialized {
		t.Fatalf("taker state after Init = %s, want csInitialized", got.String())
	}

	createdA := q.createARelay()
	if got := q.makerState(); got != csCreatedA {
		t.Fatalf("maker state after CreateA = %s, want csCreatedA", got.String())
	}
	createdB := q.createBRelay(createdA)
	if got := q.takerState(); got != csCreatedB {
		t.Fatalf("taker state after CreateB = %s, want csCreatedB", got.String())
	}

	confirmedA := q.confirmARelay(createdB)
	if got := q.makerState(); got != csFinished {
		t.Fatalf("maker state after ConfirmA claim = %s, want csFinished", got.String())
	}
	q.confirmBRelay(confirmedA)
	if got := q.takerState(); got != csFinished {
		t.Fatalf("taker state after ConfirmB claim = %s, want csFinished", got.String())
	}
	// The taker must have recovered the maker's secret from the on-chain
	// payTx, not from any packet field.
	makerSecret := readOnEngine(t, q.makerNode, func() [33]byte {
		return q.makerNode.sessions[idHex].secret
	})
	takerSecret := readOnEngine(t, q.takerNode, func() [33]byte {
		return q.takerNode.sessions[idHex].secret
	})
	if takerSecret != makerSecret {
		t.Error("taker did not recover the maker's secret")
	}

	q.finish()
	for _, n := range []*Node{q.makerNode, q.takerNode} {
		if o := readOnEngine(t, n, func() *Order { return n.store.Get(idHex) }); o != nil {
			t.Fatalf("finished order still live: %+v", o)
		}
	}
}

// TestTickStagesRecoverBeforeTerminate drives the REAL engine-tick table
// (tickStages) instead of piecemeal sweeps, pinning its documented contract:
// recovery runs before termination, persist runs last. Case A: a deposit-out
// session past its locktime refunds, rolls back, and prunes through the full
// tick. Case B: a session with a due claim retry AND a stale watchdog clock
// is healed by the retry before the watchdog can cancel it — reverse the
// table order and the swap would cancel instead of completing.
func TestTickStagesRecoverBeforeTerminate(t *testing.T) {
	t.Run("past-locktime-refunds-through-tick", func(t *testing.T) {
		n, s, conn := setupSwapPair(t)
		idHex := hexEncode(s.id[:])
		if _, _, err := s.OnCreateA(&proto.CreateABody{HubAddress: s.hub, ID: s.id, BPubKey: to33(s.pubKey[:])}); err != nil {
			t.Fatalf("OnCreateA build: %v", err)
		}
		if s.refundHex == "" || s.ourLockTime == 0 {
			t.Fatal("no refund intent adopted")
		}
		conn.blockHeight = int64(s.ourLockTime) + 1
		for _, st := range n.tickStages() {
			st.run()
		}
		if !s.refundDone {
			t.Fatal("tick did not broadcast the past-locktime refund")
		}
		if o := n.store.Get(idHex); o == nil || o.Status != "rolled back" {
			t.Fatalf("order = %+v, want live 'rolled back'", o)
		}
		if _, ok := n.sessions[idHex]; ok {
			t.Fatal("refunded session was not pruned by the tick")
		}
	})

	t.Run("due-claim-retry-heals-before-watchdog-cancels", func(t *testing.T) {
		_, takerNode, _, takerSession, hub, orderID, makerPayTxID, tkLtcConn, mkLtcConn := blindTakerFixture(t)
		idHex := hexEncode(orderID[:])
		if _, _, err := takerSession.OnConfirmB(&proto.ConfirmBBody{HubAddress: hub, ID: orderID, APayTxID: makerPayTxID}); err == nil {
			t.Fatal("blind-backend ConfirmB unexpectedly succeeded")
		}
		// Backend catches up AND the hub has been silent past the stall
		// threshold: without recover-before-terminate ordering the tick
		// would cancel (crTimeout) instead of rebuilding the claim.
		hx, err := mkLtcConn.GetRawTransaction(makerPayTxID)
		if err != nil {
			t.Fatal("maker payTx missing from maker connector")
		}
		tkLtcConn.setRawTx(makerPayTxID, hx)
		takerSession.claimRetryAt = 1
		takerSession.lastProgress = uint64(NowMicro()) - sessionStallMicro - 1
		for _, st := range takerNode.tickStages() {
			st.run()
		}
		if takerSession.claimTxID == "" {
			t.Fatal("tick did not rebuild the due claim")
		}
		if got := takerSession.state; got != csFinished {
			t.Fatalf("taker state = %s, want csFinished (claimed, not canceled)", got.String())
		}
		if o := takerNode.store.Get(idHex); o == nil || o.Status == "canceled" {
			t.Fatalf("order = %+v, want live and not canceled", o)
		}
	})
}

// TestSequencerHaltMatrix stops the scripted hub after each handshake phase
// and proves the halted swap is never stranded: sessions stay live in their
// progress states, every built deposit keeps its pre-signed refund armed,
// and a full engine tick changes nothing (no prune of refund-owed sessions,
// no bogus rollback-failed while locktimes hold).
func TestSequencerHaltMatrix(t *testing.T) {
	halts := []struct {
		name  string
		drive func(q *hubSequencer)
		maker clientState
		taker clientState
		// armedMaker/armedTaker report whether that role's deposit is built
		// past this halt (single-sided arming included: after-createA the
		// maker alone must hold a pre-signed refund).
		armedMaker bool
		armedTaker bool
	}{
		{"after-hold", func(q *hubSequencer) { q.hold() }, csHoldApplied, csHoldApplied, false, false},
		{"after-init", func(q *hubSequencer) { q.hold(); q.init() }, csInitialized, csInitialized, false, false},
		{"after-createA", func(q *hubSequencer) { q.hold(); q.init(); q.createARelay() }, csCreatedA, csInitialized, true, false},
		{"after-createB", func(q *hubSequencer) {
			q.hold()
			q.init()
			q.createBRelay(q.createARelay())
		}, csCreatedA, csCreatedB, true, true},
	}
	for _, h := range halts {
		t.Run(h.name, func(t *testing.T) {
			q := newHubSequencer(t)
			idHex := hexEncode(q.orderID[:])
			h.drive(q)

			if got := q.makerState(); got != h.maker {
				t.Fatalf("maker state = %s, want %s", got.String(), h.maker.String())
			}
			if got := q.takerState(); got != h.taker {
				t.Fatalf("taker state = %s, want %s", got.String(), h.taker.String())
			}
			// Neither order may leave the live book at a halt.
			for _, n := range []*Node{q.makerNode, q.takerNode} {
				if o := readOnEngine(t, n, func() *Order { return n.store.Get(idHex) }); o == nil {
					t.Fatal("halted order left the live book")
				}
			}
			// Past the deposits, every built deposit keeps its pre-signed
			// refund armed (hex on the session AND the order record the
			// stored sweep keys on) — checked per role, so the maker-only
			// single-sided case past CreateA is covered too.
			for i, n := range []*Node{q.makerNode, q.takerNode} {
				wantArmed := h.armedMaker
				if i == 1 {
					wantArmed = h.armedTaker
				}
				if !wantArmed {
					continue
				}
				s := readOnEngine(t, n, func() *SwapSession { return n.sessions[idHex] })
				o := readOnEngine(t, n, func() *Order { return n.store.Get(idHex) })
				if s == nil || o == nil {
					t.Fatal("session or order missing past the deposits")
				}
				if s.refundHex == "" || o.RefundTx == "" || !o.DepositSent {
					t.Fatalf("refund not armed: session refundHex=%d order RefundTx=%d DepositSent=%v",
						len(s.refundHex), len(o.RefundTx), o.DepositSent)
				}
			}
			// A full engine tick over the halted pair must strand nothing:
			// locktimes are far in the future (heights ~1000, locks ~1120+),
			// so no broadcast fires, no session prunes, no status flips to
			// rollback-failed. The tick runs ON the engine (submit), never
			// beside it: these nodes are started, and engine-owned sweeps
			// must not run concurrently with the engine loop.
			for _, n := range []*Node{q.makerNode, q.takerNode} {
				n.submit(func() {
					for _, st := range n.tickStages() {
						st.run()
					}
				}, true)
				s := readOnEngine(t, n, func() *SwapSession { return n.sessions[idHex] })
				o := readOnEngine(t, n, func() *Order { return n.store.Get(idHex) })
				if s == nil || o == nil {
					t.Fatal("engine tick reaped a live halted swap")
				}
				if o.Status == "rollback failed" {
					t.Fatalf("tick marked a healthy halted swap rollback-failed (status=%q state=%s)",
						o.Status, s.state.String())
				}
			}
		})
	}
}
