package api

// Alignment regression tests for the C++-parity audit commit.
//
// Each test pins one remediated deviation against the C++ reference
// (blocknet src/xbridge), failing before the fix and passing after. All use
// in-memory fixtures — no live hub, no network.

import (
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"go-xbridge/coins"
	"go-xbridge/config"
	"go-xbridge/crypto"
	"go-xbridge/p2p/servicenode"
	"go-xbridge/proto"
	"go-xbridge/wallet"
)

func alignConfs() map[string]*config.CoinConf {
	return map[string]*config.CoinConf{
		"BTC": {Ticker: "BTC", Title: "Bitcoin", Coin: 1e8, AddressPrefix: 0, ScriptPrefix: 5, CreateTxMethod: "BTC", BlockTime: 60, FeePerByte: 2},
		"LTC": {Ticker: "LTC", Title: "Litecoin", Coin: 1e8, AddressPrefix: 48, ScriptPrefix: 50, CreateTxMethod: "LTC", BlockTime: 60, FeePerByte: 2},
	}
}

func alignInitCoins(t *testing.T) {
	t.Helper()
	if err := coins.InitFromConf(alignConfs()); err != nil {
		t.Fatal(err)
	}
}

// TestHoldResizesMakerToPartialAmounts pins the C++ maker Hold reassignment
// (xbridgesession.cpp:1525-1528 `fromAmount=damount;toAmount=samount`): a
// partial take resizing the session AND the stored order so the maker deposit
// locks the partial amount. Full takes are unaffected.
func TestHoldResizesMakerToPartialAmounts(t *testing.T) {
	alignInitCoins(t)

	btc := &fakeConnector{ticker: "BTC", funding: wallet.Utxo{TxID: strings.Repeat("aa", 32)}, changeAddr: addrFor(0, "c"), blockHeight: 1000, rawTx: map[string]string{}}
	n := newTestNode(t, alignConfs(), map[string]wallet.Connector{"BTC": btc, "LTC": btc})

	var id [32]byte
	oid := hash20("align-partial")
	copy(id[:], oid[:])
	mPriv, mPub := newKey(t)
	o := &Order{ID: id, FromCurrency: "BTC", ToCurrency: "LTC",
		FromAmount: 10e6, ToAmount: 8e6, PartialAllowed: true, MinFromAmount: 1e6}
	n.newMakerSession(withUsedCoins(t, n, o, []wallet.Utxo{btc.funding}),
		MakeOrderParams{MakerAddress: addrFor(0, "m-src"), TakerAddress: addrFor(48, "m-dst")}, arr32(mPriv), toArr33(mPub))
	s := n.sessions[hexEncode(id[:])]

	var hub [20]byte
	// Partial take: taker gives 4e6 of 8e6, takes 5e6 of 10e6 (exact ratio).
	cmd, body, err := s.OnHold(&proto.HoldBody{HubAddress: hub, ID: id, FromAmount: 4e6, ToAmount: 5e6})
	if err != nil || cmd != proto.XbcTransactionHoldApply || body == nil {
		t.Fatalf("OnHold partial: cmd=%v body=%v err=%v", cmd, body, err)
	}
	if s.srcAmt != 5e6 || s.dstAmt != 4e6 {
		t.Fatalf("session amounts = (%d,%d), want resized (5e6,4e6)", s.srcAmt, s.dstAmt)
	}
	if got := n.store.Get(hexEncode(id[:])); got.FromAmount != 5e6 || got.ToAmount != 4e6 {
		t.Fatalf("stored order amounts = (%d,%d), want resized (5e6,4e6)", got.FromAmount, got.ToAmount)
	}

	// Taker side never resizes: exact-match session keeps its amounts.
	tPriv, tPub := newKey(t)
	to := &Order{ID: id, FromCurrency: "BTC", ToCurrency: "LTC", FromAmount: 10e6, ToAmount: 8e6}
	n.newTakerSession(withUsedCoins(t, n, to, []wallet.Utxo{btc.funding}),
		TakeOrderParams{FromAddress: addrFor(48, "t-src"), ToAddress: addrFor(0, "t-dst")}, arr32(tPriv), toArr33(tPub))
	ts := n.sessions[hexEncode(id[:])]
	beforeSrc, beforeDst := ts.srcAmt, ts.dstAmt
	if _, _, err := ts.OnHold(&proto.HoldBody{HubAddress: hub, ID: id, FromAmount: beforeSrc, ToAmount: beforeDst}); err != nil {
		t.Fatalf("taker OnHold: %v", err)
	}
	if ts.srcAmt != beforeSrc || ts.dstAmt != beforeDst {
		t.Fatalf("taker session resized to (%d,%d), want unchanged (%d,%d)", ts.srcAmt, ts.dstAmt, beforeSrc, beforeDst)
	}
}

// flakyWriteConn fails the first outbound SEND, then records and succeeds,
// exercising the rebroadcast hub-rotation path.
type flakyWriteConn struct {
	*captureXConn
	failed bool
}

func (c *flakyWriteConn) WritePacket(p *proto.Packet, dest [20]byte) error {
	if !c.failed {
		c.failed = true
		return errNotFound
	}
	return c.captureXConn.WritePacket(p, dest)
}

// rotationFixture builds an untaken maker order pinned to hubA with hubB as
// the failover candidate, and a conn that fails its first send.
func rotationFixture(t *testing.T) (*Node, *flakyWriteConn, string, [33]byte, [33]byte) {
	t.Helper()
	alignInitCoins(t)
	_, hubA := newKey(t)
	_, hubB := newKey(t)
	var hubArrA, hubArrB [33]byte
	copy(hubArrA[:], hubA)
	copy(hubArrB[:], hubB)
	reg := servicenode.NewRegistry()
	for _, hk := range [][33]byte{hubArrA, hubArrB} {
		reg.AddPing(servicenode.ServiceNode{
			PubKey: hk, Tier: servicenode.TierSPV,
			Services:       []string{"BTC", "LTC"},
			XBridgeVersion: proto.ProtocolVersion,
		})
	}
	funding := wallet.Utxo{TxID: strings.Repeat("aa", 32), Vout: 0, Amount: 5e8}
	btc := &fakeConnector{ticker: "BTC", funding: funding,
		changeAddr: addrFor(0, "c"), blockHeight: 1000, rawTx: map[string]string{}}
	n := newTestNode(t, alignConfs(), map[string]wallet.Connector{"BTC": btc})
	n.snReg = reg
	cc := &captureXConn{}
	fwc := &flakyWriteConn{captureXConn: cc}
	n.conn = fwc

	var id [32]byte
	oid := hash20("align-hub-rotation")
	copy(id[:], oid[:])
	idHex := hexEncode(id[:])
	mPriv, mPub := newKey(t)
	o := &Order{ID: id, FromCurrency: "BTC", ToCurrency: "LTC", FromAmount: 2.5e6,
		ToAmount: 2e6, Mine: true, Status: "open",
		SNodePubkey: hexEncode(hubA), HubAddress: coins.KeyID(hubA),
		Updated: NowMicro() - uint64(rebroadcastInterval/time.Microsecond) - 1000}
	n.newMakerSession(withUsedCoins(t, n, o, []wallet.Utxo{funding}),
		MakeOrderParams{MakerAddress: addrFor(0, "m-src"), TakerAddress: addrFor(48, "m-dst")}, arr32(mPriv), toArr33(mPub))
	return n, fwc, idHex, hubArrA, hubArrB
}

// TestRebroadcastRotatesHubOnSendFailure pins C++'s failed-hub rotation
// (checkAndRelayPendingOrders `notIn`, xbridgeapp.cpp:3274/3311): when the
// re-post send fails, the order migrates to a freshly-picked hub excluding
// the dead one and the retry goes there — the anchor never sticks to a hub
// that cannot be reached.
func TestRebroadcastRotatesHubOnSendFailure(t *testing.T) {
	n, fwc, idHex, hubA, hubB := rotationFixture(t)

	n.rebroadcastOpenOrders()

	var wantB [33]byte
	copy(wantB[:], hubB[:])
	dests := fwc.snapshotDests()
	if len(dests) != 1 || dests[0] != coins.KeyID(wantB[:]) {
		t.Fatalf("retry dest = %v, want hubB %x (failed hubA %x must be excluded)",
			dests, coins.KeyID(wantB[:]), coins.KeyID(hubA[:]))
	}
	o := n.store.Get(idHex)
	if o.HubAddress != coins.KeyID(wantB[:]) || o.SNodePubkey != hexEncode(wantB[:]) {
		t.Fatalf("order anchor not migrated to hubB: %+v", o)
	}
	if s := n.sessions[idHex]; s.hubKey != wantB || s.hub != coins.KeyID(wantB[:]) {
		t.Fatalf("session anchor not migrated to hubB")
	}
}

// TestRebroadcastKeepsAnchorWithoutAlternative pins the fallback: with no
// other hub available the anchor is kept (retry next round), never migrated
// to nothing.
func TestRebroadcastKeepsAnchorWithoutAlternative(t *testing.T) {
	n, fwc, idHex, hubA, _ := rotationFixture(t)
	// Registry with only the pinned hub: rotation must fail closed.
	reg := servicenode.NewRegistry()
	reg.AddPing(servicenode.ServiceNode{
		PubKey: hubA, Tier: servicenode.TierSPV,
		Services:       []string{"BTC", "LTC"},
		XBridgeVersion: proto.ProtocolVersion,
	})
	n.snReg = reg

	n.rebroadcastOpenOrders()

	if pkts := fwc.snapshot(); len(pkts) != 0 {
		t.Fatalf("no alternative hub: want no successful send, got %d", len(pkts))
	}
	o := n.store.Get(idHex)
	if o.HubAddress != coins.KeyID(hubA[:]) {
		t.Fatalf("anchor must be kept when rotation is impossible")
	}
}

type errBlockConn struct {
	wallet.Connector
}

func (e errBlockConn) GetBlockCount() (int64, error) { return 0, errNotFound }

// errTxOutConn fails GetTxOut, simulating a wallet that cannot answer the
// unspent probe (transient: the watch must skip the round, never cancel).
type errTxOutConn struct {
	wallet.Connector
}

func (e errTxOutConn) GetTxOut(string, uint32) (wallet.Utxo, bool, error) {
	return wallet.Utxo{}, false, errNotFound
}

// watchTakerFixture builds a taker session in the pre-claim window (own
// deposit broadcast, counterparty A-deposit validated) with a live order.
func watchTakerFixture(t *testing.T, btc wallet.Connector) (*Node, *captureXConn, string) {
	t.Helper()
	alignInitCoins(t)
	ltc := &fakeConnector{ticker: "LTC", funding: wallet.Utxo{TxID: strings.Repeat("bb", 32), Amount: 5e8},
		changeAddr: addrFor(48, "c"), blockHeight: 1000, rawTx: map[string]string{}}
	n := newTestNode(t, alignConfs(), map[string]wallet.Connector{"BTC": btc, "LTC": ltc})
	cc := &captureXConn{}
	n.conn = cc

	var id [32]byte
	oid := hash20("align-deposit-watch")
	copy(id[:], oid[:])
	idHex := hexEncode(id[:])
	tPriv, tPub := newKey(t)
	o := &Order{ID: id, FromCurrency: "BTC", ToCurrency: "LTC", FromAmount: 2.5e6,
		ToAmount: 2e6, Mine: true, Status: "created",
		RefundTx: "deadbeef", DepositSent: true}
	n.newTakerSession(withUsedCoins(t, n, o, []wallet.Utxo{ltc.funding}),
		TakeOrderParams{FromAddress: addrFor(48, "t-src"), ToAddress: addrFor(0, "t-dst")}, arr32(tPriv), toArr33(tPub))
	// Production TakeOrder records our per-trade M pubkey on the order; the
	// cancel path authenticates our own Cancel packet against it
	// (handleRemoteCancel iCanceled, C++ :3351-3357).
	o.MakerKey = hexEncode(tPub)
	s := n.sessions[idHex]
	s.state = csCreatedB
	s.theirDepositTxID = strings.Repeat("cc", 32)
	s.theirDepositVout = 0
	s.theirLockTime = 1030
	s.theirSecretHash = hash20("align-secret")
	s.refundHex = "deadbeef"
	return n, cc, idHex
}

// TestDepositWatchCancelsOnSpentDeposit pins the fund-safety watch (C++
// watchForSpentDeposit / xbridgeapp.cpp:3441): a validated counterparty
// deposit that vanishes is wire-cancelled with the taker's deposit reason
// (crBadADepositTx) and rolled back, instead of stalling or claiming into
// the void.
func TestDepositWatchCancelsOnSpentDeposit(t *testing.T) {
	btc := &fakeConnector{ticker: "BTC", funding: wallet.Utxo{TxID: strings.Repeat("aa", 32), Amount: 5e8},
		changeAddr: addrFor(0, "c"), blockHeight: 1000, rawTx: map[string]string{}}
	n, cc, idHex := watchTakerFixture(t, btc)

	n.watchCounterpartyDeposits()

	pkts := cc.snapshot()
	if len(pkts) != 1 || pkts[0].Command != proto.XbcTransactionCancel {
		t.Fatalf("want exactly one Cancel packet, got %v", pkts)
	}
	var cb proto.CancelBody
	if err := cb.Unmarshal(pkts[0].Body); err != nil {
		t.Fatalf("Cancel body decode: %v", err)
	}
	if cb.Reason != uint32(crBadADepositTx) {
		t.Fatalf("cancel reason = %d, want crBadADepositTx (%d)", cb.Reason, crBadADepositTx)
	}
	if got := n.store.Get(idHex); got.Status != "rolled back" {
		t.Fatalf("order status = %q, want rolled back", got.Status)
	}
}

// TestDepositWatchQuietWhenUnspent pins the negative: an unspent counterparty
// deposit produces no packet and no state change.
func TestDepositWatchQuietWhenUnspent(t *testing.T) {
	btc := &fakeConnector{ticker: "BTC", funding: wallet.Utxo{TxID: strings.Repeat("aa", 32), Amount: 5e8},
		changeAddr: addrFor(0, "c"), blockHeight: 1000, rawTx: map[string]string{}}
	// The validated A-deposit exists on-chain.
	dep := &coins.Tx{Version: 1}
	dep.Inputs = append(dep.Inputs, coins.TxIn{Sequence: 0xffffffff})
	dep.Outputs = append(dep.Outputs, coins.TxOut{Value: 2.5e8, ScriptPubKey: []byte{0x51}})
	btc.setRawTx(strings.Repeat("cc", 32), hex.EncodeToString(dep.Serialize()))
	n, cc, idHex := watchTakerFixture(t, btc)

	n.watchCounterpartyDeposits()

	if pkts := cc.snapshot(); len(pkts) != 0 {
		t.Fatalf("unspent deposit must produce no packets, got %v", pkts)
	}
	if got := n.store.Get(idHex); got.Status != "created" {
		t.Fatalf("order status = %q, want unchanged created", got.Status)
	}
}

// TestDepositWatchSkipsTransientErrors pins the fail-open direction: a wallet
// that cannot answer the probe skips the round instead of cancelling a
// healthy swap.
func TestDepositWatchSkipsTransientErrors(t *testing.T) {
	n, cc, idHex := watchTakerFixture(t, errTxOutConn{})

	n.watchCounterpartyDeposits()

	if pkts := cc.snapshot(); len(pkts) != 0 {
		t.Fatalf("transient probe error must produce no packets, got %v", pkts)
	}
	if got := n.store.Get(idHex); got.Status != "created" {
		t.Fatalf("order status = %q, want unchanged created", got.Status)
	}
}

// TestBuildDepositFailsClosedOnLocktimeZero pins the C++ lockTime==0 cancel
// guard (xbridgesession.cpp:2037-2043): no zero-lockTime (immediately
// refundable) HTLC may be built.
func TestBuildDepositFailsClosedOnLocktimeZero(t *testing.T) {
	alignInitCoins(t)

	btc := &fakeConnector{ticker: "BTC", funding: wallet.Utxo{TxID: strings.Repeat("aa", 32), Amount: 5e8}, changeAddr: addrFor(0, "c"), blockHeight: 1000, rawTx: map[string]string{}}
	n := newTestNode(t, alignConfs(), map[string]wallet.Connector{"BTC": btc})

	var id [32]byte
	oid := hash20("align-locktime-zero")
	copy(id[:], oid[:])
	mPriv, mPub := newKey(t)
	o := &Order{ID: id, FromCurrency: "BTC", ToCurrency: "LTC", FromAmount: 2.5e6, ToAmount: 2e6}
	n.newMakerSession(withUsedCoins(t, n, o, []wallet.Utxo{btc.funding}),
		MakeOrderParams{MakerAddress: addrFor(0, "m-src"), TakerAddress: addrFor(48, "m-dst")}, arr32(mPriv), toArr33(mPub))
	s := n.sessions[hexEncode(id[:])]

	c := s.snapshot()
	c.connectors["BTC"] = errBlockConn{btc}
	if _, err := c.buildDeposit(true); err == nil || !strings.Contains(err.Error(), "locktime") {
		t.Fatalf("buildDeposit with unavailable tip = %v, want locktime fail-closed error", err)
	}
}

// TestBuildDepositChangeToLargestFunding pins C++ largestUtxo.address change
// (xbridgesession.cpp:2102): change returns to the largest funding UTXO's
// address, a wallet-watched address, not a fresh one.
func TestBuildDepositChangeToLargestFunding(t *testing.T) {
	alignInitCoins(t)

	fPriv, fPub := newKey(t)
	fundScript := hex.EncodeToString(coins.BuildP2PKHScript(coins.KeyID(fPub)))
	big, small := wallet.Utxo{TxID: strings.Repeat("aa", 32), Vout: 0, Amount: 5e8,
		Address: addrFor(0, "big-funding"), ScriptPubKey: fundScript},
		wallet.Utxo{TxID: strings.Repeat("bb", 32), Vout: 1, Amount: 1e8,
			Address: addrFor(0, "small-funding"), ScriptPubKey: fundScript}
	btc := &fakeConnector{ticker: "BTC", funding: big, fundingPriv: fPriv, fundingPub: fPub,
		changeAddr: addrFor(0, "fresh-change"), blockHeight: 1000, rawTx: map[string]string{}}
	n := newTestNode(t, alignConfs(), map[string]wallet.Connector{"BTC": btc, "LTC": btc})

	var id [32]byte
	oid := hash20("align-change-addr")
	copy(id[:], oid[:])
	mPriv, mPub := newKey(t)
	o := &Order{ID: id, FromCurrency: "BTC", ToCurrency: "LTC", FromAmount: 1e6, ToAmount: 1e6}
	n.newMakerSession(withUsedCoins(t, n, o, []wallet.Utxo{small, big}),
		MakeOrderParams{MakerAddress: addrFor(0, "m-src"), TakerAddress: addrFor(48, "m-dst")}, arr32(mPriv), toArr33(mPub))
	s := n.sessions[hexEncode(id[:])]

	ctx := s.snapshot()
	out, err := ctx.buildDeposit(true)
	if err != nil {
		t.Fatalf("buildDeposit: %v", err)
	}
	raw, err := hex.DecodeString(out.depositHex)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := coins.Deserialize(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(tx.Outputs) != 2 {
		t.Fatalf("want deposit+change outputs, got %d", len(tx.Outputs))
	}
	bigHash := hash20("big-funding")
	want := coins.BuildP2PKHScript(bigHash)
	if hex.EncodeToString(tx.Outputs[1].ScriptPubKey) != hex.EncodeToString(want) {
		t.Fatalf("change script = %x, want P2PKH of largest funding address", tx.Outputs[1].ScriptPubKey)
	}
}

// TestVerifyHubPacketGraceAfterHold pins the mid-swap registry grace: once the
// handshake is underway the pinned hub key alone authenticates (C++ re-checks
// getSn only at intake, xbridgesession.cpp:1384), so a lapsed registration
// cannot abort an in-flight trade.
func TestVerifyHubPacketGraceAfterHold(t *testing.T) {
	alignInitCoins(t)
	n := newTestNode(t, alignConfs(), nil)

	hubPriv, hubPub := newKey(t)
	var hubKey [33]byte
	copy(hubKey[:], hubPub)
	s := &SwapSession{n: n, hubKey: hubKey, state: csHoldApplied}

	var hub [20]byte
	pkt := proto.NewPacket(proto.XbcTransactionHold,
		(&proto.HoldBody{HubAddress: hub, FromAmount: 1, ToAmount: 1}).Marshal())
	if err := n.signer.Sign(pkt, hubPriv); err != nil {
		t.Fatal(err)
	}
	if !n.verifyHubPacket(pkt, s) {
		t.Fatal("in-flight session with valid hub sig must verify despite empty registry")
	}
	s.state = csMaker
	if n.verifyHubPacket(pkt, s) {
		t.Fatal("pre-handshake session must require registry membership")
	}
	otherPriv, _ := newKey(t)
	bad := proto.NewPacket(proto.XbcTransactionHold,
		(&proto.HoldBody{HubAddress: hub, FromAmount: 1, ToAmount: 1}).Marshal())
	if err := n.signer.Sign(bad, otherPriv); err != nil {
		t.Fatal(err)
	}
	s.state = csHoldApplied
	if n.verifyHubPacket(bad, s) {
		t.Fatal("packet from a non-hub key must never verify")
	}
	if _, err := crypto.NewBtcSigner().Verify(pkt); err != nil {
		t.Fatalf("test packet itself must verify: %v", err)
	}
}

// TestRebroadcastCancelsSpentFunding pins C++ orderUtxosAreStillValid
// (xbridgeapp.cpp:3557-3569): a rebroadcast-due order whose funding is spent
// cancels with crBadAUtxo instead of re-posting a dead order.
func TestRebroadcastCancelsSpentFunding(t *testing.T) {
	alignInitCoins(t)

	btc := &fakeConnector{ticker: "BTC", funding: wallet.Utxo{TxID: strings.Repeat("aa", 32), Amount: 5e8},
		changeAddr: addrFor(0, "c"), blockHeight: 1000, rawTx: map[string]string{}}
	n := newTestNode(t, alignConfs(), map[string]wallet.Connector{"BTC": btc})
	cc := &captureXConn{}
	n.conn = cc

	var id [32]byte
	oid := hash20("align-rebroadcast-spent")
	copy(id[:], oid[:])
	mPriv, mPub := newKey(t)
	spent := wallet.Utxo{TxID: strings.Repeat("ff", 32), Vout: 0, Amount: 5e8} // unknown to the wallet: spent
	o := &Order{ID: id, FromCurrency: "BTC", ToCurrency: "LTC", FromAmount: 2.5e6,
		ToAmount: 2e6, Mine: true, Status: "open",
		Updated: NowMicro() - uint64(rebroadcastInterval/time.Microsecond) - 1000}
	n.newMakerSession(withUsedCoins(t, n, o, []wallet.Utxo{spent}),
		MakeOrderParams{MakerAddress: addrFor(0, "m-src"), TakerAddress: addrFor(48, "m-dst")}, arr32(mPriv), toArr33(mPub))

	n.rebroadcastOpenOrders()

	if got := n.store.Get(hexEncode(id[:])); got.Status != "canceled" {
		t.Fatalf("order status = %q, want canceled after funding spent", got.Status)
	}
	pkts := cc.snapshot()
	if len(pkts) != 1 || pkts[0].Command != proto.XbcTransactionCancel {
		t.Fatalf("want exactly one Cancel packet, got %v", pkts)
	}
	var cb proto.CancelBody
	if err := cb.Unmarshal(pkts[0].Body); err != nil {
		t.Fatalf("Cancel body decode: %v", err)
	}
	if cb.Reason != uint32(crBadAUtxo) {
		t.Fatalf("cancel reason = %d, want crBadAUtxo (%d)", cb.Reason, crBadAUtxo)
	}
}
