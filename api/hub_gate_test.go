package api

import (
	"bytes"
	"strings"
	"testing"

	"go-xbridge/coins"
	"go-xbridge/config"
	"go-xbridge/crypto"
	"go-xbridge/p2p/servicenode"
	"go-xbridge/proto"
	"go-xbridge/wallet"
)

// newHubNode builds a Node wired for hub-gate tests: two wallet connectors,
// a BLOCK connector (fee cs are paid on the Blocknet chain), a captureXConn
// (so requireWrite passes and outbound packets are recorded), an empty store +
// session map, and the given service-node registry. The BTC connector is funded
// with a single 3.0 BTC utxo so selection passes for the standard 1.5 BTC exact
// make (C++ selectUtxos gt-single path) and the BLOCK connector holds a 1.0
// BLOCK p2pkh utxo for the take's service-node fee tx.
func newHubNode(reg *servicenode.Registry) (*Node, *captureXConn) {
	return newHubNodeUtxos(reg, []wallet.Utxo{
		{TxID: "0000000000000000000000000000000000000000000000000000000000000001", Vout: 0,
			Amount: 300000000, Value: 3.0, ScriptPubKey: "76a914000000000000000000000000000000000000000088ac", Address: btcAddr},
	})
}

// blkUtxo returns the default funding BLOCK p2pkh utxo used by the take fee
// path: a 1.0 BLOCK output (native 1e8) spending to btcAddr (BLOCK conf uses
// the same base58 prefix so the legacy change script decodes).
func blkUtxo() wallet.Utxo {
	return wallet.Utxo{
		TxID:         "0000000000000000000000000000000000000000000000000000000000000002",
		Vout:         0,
		Amount:       100000000,
		Value:        1.0,
		ScriptPubKey: "76a914000000000000000000000000000000000000000088ac",
		Address:      btcAddr,
	}
}

// newHubNodeUtxos is newHubNode with an explicit BTC utxo set (used by the
// Stage 3c make-order KATs: custom partial-exact wallets, unfunded wallets).
func newHubNodeUtxos(reg *servicenode.Registry, btc []wallet.Utxo) (*Node, *captureXConn) {
	return buildHubNode(reg, btc, []wallet.Utxo{blkUtxo()})
}

// newHubNodeWithoutBlock is newHubNode minus any BLOCK connector, used to prove
// the take's fee prep refuses INSUFFICIENT_FUNDS when no Blocknet wallet is
// configured (C++ acceptXBridgeTransaction :2236). buildHubNode only installs a
// BLOCK entry when a utxo set is provided, so nil here means the connector does
// not exist at all.
func newHubNodeWithoutBlock(reg *servicenode.Registry) (*Node, *captureXConn) {
	return buildHubNode(reg, []wallet.Utxo{
		{TxID: "0000000000000000000000000000000000000000000000000000000000000001", Vout: 0,
			Amount: 300000000, Value: 3.0, ScriptPubKey: "76a914000000000000000000000000000000000000000088ac", Address: btcAddr},
	}, nil)
}

// newHubNodeUnfundedBlock wires a BLOCK connector that reports no spendable
// utxos, proving the take's fee prep refuses INSUFFICIENT_FUNDS (C++ :2240).
func newHubNodeUnfundedBlock(reg *servicenode.Registry) (*Node, *captureXConn) {
	return buildHubNode(reg, []wallet.Utxo{
		{TxID: "0000000000000000000000000000000000000000000000000000000000000001", Vout: 0,
			Amount: 300000000, Value: 3.0, ScriptPubKey: "76a914000000000000000000000000000000000000000088ac", Address: btcAddr},
	}, []wallet.Utxo{})
}

func buildHubNode(reg *servicenode.Registry, btc []wallet.Utxo, block []wallet.Utxo) (*Node, *captureXConn) {
	coinConfs := map[string]*config.CoinConf{
		"BTC": {Ticker: "BTC", CreateTxMethod: "BTC", AddressPrefix: 0, ScriptPrefix: 5, Coin: 100000000},
		"SYS": {Ticker: "SYS", CreateTxMethod: "SYS", AddressPrefix: 0, ScriptPrefix: 5, Coin: 100000000},
	}
	if block != nil {
		coinConfs["BLOCK"] = &config.CoinConf{Ticker: "BLOCK", CreateTxMethod: "BTC", AddressPrefix: 0, ScriptPrefix: 5, Coin: 100000000, TxVersion: 1}
	}
	if err := coins.InitFromConf(coinConfs); err != nil {
		panic(err)
	}
	cc := &captureXConn{}
	cfg := &Config{
		Confs: map[string]*config.CoinConf{
			"BTC": {Ticker: "BTC", CreateTxMethod: "BTC", AddressPrefix: 0, ScriptPrefix: 5, Coin: 100000000},
			"SYS": {Ticker: "SYS", CreateTxMethod: "SYS", AddressPrefix: 0, ScriptPrefix: 5, Coin: 100000000},
		},
		Connectors: map[string]wallet.Connector{
			"BTC": &stubConn{ticker: "BTC", addr: btcAddr, utxos: btc},
			"SYS": &stubConn{ticker: "SYS", addr: btcAddr},
		},
	}
	if block != nil {
		cfg.Confs["BLOCK"] = &config.CoinConf{Ticker: "BLOCK", CreateTxMethod: "BTC", AddressPrefix: 0, ScriptPrefix: 5, Coin: 100000000, TxVersion: 1}
		cfg.Connectors["BLOCK"] = &stubConn{ticker: "BLOCK", addr: btcAddr, utxos: block}
	}
	n := &Node{
		config:   cfg,
		signer:   crypto.NewBtcSigner(),
		stop:     make(chan struct{}),
		store:    NewStore(),
		sessions: map[string]*SwapSession{},
		conn:     cc,
		snReg:    reg,
	}
	return n, cc
}

func hubKey(t *testing.T, seed byte) ([32]byte, [33]byte, string, [20]byte) {
	t.Helper()
	priv := make([]byte, 32)
	priv[31] = seed
	pub := mustPub(t, priv)
	return arr32(priv), pub, hexPub(t, priv), coins.KeyID(pub[:])
}

// TestMakeOrderNoRegistry verifies dxMakeOrder fails with NO_SERVICE_NODE (1032)
// when no service-node registry is configured (C++ makeTransaction:1515).
// C++ emits makeError(statusCode, __FUNCTION__) with NO argument, so the message
// is the bare "Could not find a service node with required services: " (the
// maker/taker pair is NOT appended).
func TestMakeOrderNoRegistry(t *testing.T) {
	n, _ := newHubNode(nil)
	o, rerr := n.MakeOrder(MakeOrderParams{
		Maker: "BTC", MakerSize: "1.5", MakerAddress: btcAddr,
		Taker: "SYS", TakerSize: "0.3", TakerAddress: btcAddr2,
	})
	if rerr == nil || rerr.Code != errNoServiceNode {
		t.Fatalf("MakeOrder(nil registry) = %v, want errNoServiceNode", rerr)
	}
	if rerr.Error != "Could not find a service node with required services: " {
		t.Errorf("MakeOrder(nil registry) message = %q, want bare text with trailing space", rerr.Error)
	}
	if o != nil {
		t.Fatalf("expected nil order, got %+v", o)
	}
}

// TestMakeOrderEmptyRegistry verifies dxMakeOrder fails with NO_SERVICE_NODE
// when the registry holds no eligible hub.
func TestMakeOrderEmptyRegistry(t *testing.T) {
	n, cc := newHubNode(servicenode.NewRegistry())
	_, rerr := n.MakeOrder(MakeOrderParams{
		Maker: "BTC", MakerSize: "1.5", MakerAddress: btcAddr,
		Taker: "SYS", TakerSize: "0.3", TakerAddress: btcAddr2,
	})
	if rerr == nil || rerr.Code != errNoServiceNode {
		t.Fatalf("MakeOrder(empty registry) = %v, want errNoServiceNode", rerr)
	}
	if len(cc.snapshot()) != 0 {
		t.Fatalf("no SEND should be written when no hub is available")
	}
}

// TestMakeOrderMissingMakerConnectorNoSession verifies dxMakeOrder surfaces
// NO_SESSION (1018) when the maker's wallet connector is absent, rather than
// panicking on a nil-interface ListUnspent. The maker connector handle is
// fetched once at the connector gate and reused for the funding enumeration, so
// a dxLoadXBridgeConf reload between the two must not leave the funding path
// with a nil connector.
func TestMakeOrderMissingMakerConnectorNoSession(t *testing.T) {
	reg := servicenode.NewRegistry()
	_, hubPub, _, _ := hubKey(t, 0x53)
	reg.AddPing(servicenode.ServiceNode{
		PubKey: hubPub, Tier: servicenode.TierSPV, Services: []string{"BTC", "SYS"}, XBridgeVersion: proto.ProtocolVersion,
	})
	n, _ := newHubNode(reg)
	// Drop the maker wallet: the connector gate must error, not deref a nil
	// interface in the ListUnspent that follows it.
	delete(n.config.Connectors, "BTC")

	o, rerr := n.MakeOrder(MakeOrderParams{
		Maker: "BTC", MakerSize: "1.5", MakerAddress: btcAddr,
		Taker: "SYS", TakerSize: "0.3", TakerAddress: btcAddr2,
	})
	if rerr == nil {
		t.Fatal("MakeOrder(missing maker connector) = nil error, want NO_SESSION")
	}
	if rerr.Code != errNoSession {
		t.Fatalf("MakeOrder(missing maker connector) code = %d, want %d", rerr.Code, errNoSession)
	}
	if want := "No session for currency Unable to connect to wallet: BTC"; rerr.Error != want {
		t.Errorf("MakeOrder(missing maker connector) message = %q, want %q", rerr.Error, want)
	}
	if o != nil {
		t.Fatalf("expected nil order, got %+v", o)
	}
}

// TestMakeOrderMissingTakerConnectorNoSession mirrors the maker side for the
// taker currency: the taker connector gate is a presence check (no funding
// follows), and it must also surface NO_SESSION rather than proceed.
func TestMakeOrderMissingTakerConnectorNoSession(t *testing.T) {
	reg := servicenode.NewRegistry()
	_, hubPub, _, _ := hubKey(t, 0x54)
	reg.AddPing(servicenode.ServiceNode{
		PubKey: hubPub, Tier: servicenode.TierSPV, Services: []string{"BTC", "SYS"}, XBridgeVersion: proto.ProtocolVersion,
	})
	n, _ := newHubNode(reg)
	delete(n.config.Connectors, "SYS")

	o, rerr := n.MakeOrder(MakeOrderParams{
		Maker: "BTC", MakerSize: "1.5", MakerAddress: btcAddr,
		Taker: "SYS", TakerSize: "0.3", TakerAddress: btcAddr2,
	})
	if rerr == nil || rerr.Code != errNoSession {
		t.Fatalf("MakeOrder(missing taker connector) = %v, want NO_SESSION", rerr)
	}
	if want := "No session for currency Unable to connect to wallet: SYS"; rerr.Error != want {
		t.Errorf("MakeOrder(missing taker connector) message = %q, want %q", rerr.Error, want)
	}
	if o != nil {
		t.Fatalf("expected nil order, got %+v", o)
	}
}

// TestMakeOrderUnknownTickerNoSession verifies an unknown maker currency
// fails with NO_SESSION (1018) "Unable to connect to wallet", exactly as
// live Core — the connector gate runs before address decoding, so it never
// surfaces as 1025 "unsupported currency".
func TestMakeOrderUnknownTickerNoSession(t *testing.T) {
	n, _ := newHubNode(servicenode.NewRegistry())
	o, rerr := n.MakeOrder(MakeOrderParams{
		Maker: "XXX", MakerSize: "0.5", MakerAddress: btcAddr,
		Taker: "SYS", TakerSize: "0.5", TakerAddress: btcAddr2,
		DryRun: true,
	})
	if rerr == nil || rerr.Code != errNoSession {
		t.Fatalf("MakeOrder(unknown maker) = [%v, %v], want NO_SESSION", o, rerr)
	}
	if want := "No session for currency Unable to connect to wallet: XXX"; rerr.Error != want {
		t.Errorf("MakeOrder(unknown maker) message = %q, want %q", rerr.Error, want)
	}
	if o != nil {
		t.Fatalf("expected nil order, got %+v", o)
	}
}

// TestMakeOrderBadSizesThrow locks in the throw for dxMakeOrder maker/taker
// sizes (rpcxbridge.cpp:929/933: lexical_cast<double> after the precision
// gate). Dry-run still parses, so no broadcast is needed.
func TestMakeOrderBadSizesThrow(t *testing.T) {
	n, _ := newHubNode(servicenode.NewRegistry())
	mk := func(maker, taker string) *rpcError {
		_, rerr := n.MakeOrder(MakeOrderParams{
			Maker: "BTC", MakerSize: maker, MakerAddress: btcAddr,
			Taker: "SYS", TakerSize: taker, TakerAddress: btcAddr2,
			DryRun: true,
		})
		return rerr
	}
	assertLexicalThrow(t, mk("abc", "0.3"))
	assertLexicalThrow(t, mk("1.5", "abc"))
}

// TestMakePartialBadMinSizeThrow locks in the throw for dxMakePartialOrder
// minimum_size (rpcxbridge.cpp:3042).
func TestMakePartialBadMinSizeThrow(t *testing.T) {
	n, _ := newHubNode(servicenode.NewRegistry())
	_, rerr := n.MakeOrder(MakeOrderParams{
		Maker: "BTC", MakerSize: "1.5", MakerAddress: btcAddr,
		Taker: "SYS", TakerSize: "0.3", TakerAddress: btcAddr2,
		Type: "partial", MinSize: "abc", DryRun: true,
	})
	assertLexicalThrow(t, rerr)
}

// TestTakeOrderBadAmountThrow locks in the throw for the TakeOrder amount
// re-check (rpcxbridge.cpp:1157), reached by direct callers past the handler.
func TestTakeOrderBadAmountThrow(t *testing.T) {
	n, _ := newHubNode(servicenode.NewRegistry())
	id := [32]byte{0x23}
	n.store.Add(&Order{
		ID: id, Type: OrderTypeBroadcast, FromCurrency: "BTC", FromAmount: 1500000,
		ToCurrency: "BTC", ToAmount: 300000, Status: "open", Mine: false,
	})
	_, rerr := n.TakeOrder(TakeOrderParams{ID: dispID(id), FromAddress: btcAddr, ToAddress: btcAddr2, Amount: "abc", DryRun: true})
	assertLexicalThrow(t, rerr)
}

// TestMakeOrderDryRunSkipsHubGate verifies the hub gate does not block Go's
// dry-run preview: with an empty registry and no hub, a dry-run make still
// succeeds (validation/render only; nothing broadcast, no session created) and
// writes no packet. Real (non-dry) makes keep the NO_SERVICE_NODE gate.
func TestMakeOrderDryRunSkipsHubGate(t *testing.T) {
	n, cc := newHubNode(servicenode.NewRegistry())
	o, rerr := n.MakeOrder(MakeOrderParams{
		Maker: "BTC", MakerSize: "1.5", MakerAddress: btcAddr,
		Taker: "SYS", TakerSize: "0.3", TakerAddress: btcAddr2,
		DryRun: true,
	})
	if rerr != nil {
		t.Fatalf("MakeOrder(dry-run, empty registry) = %v, want success", rerr)
	}
	if o == nil {
		t.Fatal("expected a dry-run order")
	}
	if len(cc.snapshot()) != 0 {
		t.Fatalf("dry-run wrote %d packets, want 0", len(cc.snapshot()))
	}
	if n.store.Get(hexEncode(o.ID[:])) != nil {
		t.Fatal("dry-run must not add the order to the store")
	}
}

// TestMakeOrderExactMinFromAmountZero locks in C++ wire parity for exact
// orders: C++ sendXBridgeTransaction passes partialMinimum=0
// (xbridgeapp.cpp:1479) so minFromAmount is 0 on the wire and renders
// partial_minimum "0.000000" (rpcxbridge.cpp:808). The port defaulted it to
// the maker size.
func TestMakeOrderExactMinFromAmountZero(t *testing.T) {
	n, _ := newHubNode(servicenode.NewRegistry())
	o, rerr := n.MakeOrder(MakeOrderParams{
		Maker: "BTC", MakerSize: "1.5", MakerAddress: btcAddr,
		Taker: "SYS", TakerSize: "0.3", TakerAddress: btcAddr2,
		DryRun: true,
	})
	if rerr != nil {
		t.Fatalf("MakeOrder(dry-run) = %v, want success", rerr)
	}
	if o.MinFromAmount != 0 {
		t.Errorf("exact order MinFromAmount = %d, want 0 (C++ partialMinimum=0)", o.MinFromAmount)
	}
}

// TestMakeOrderReturnsStoreCopy proves dxMakeOrder returns a snapshot COPY of
// the stored order, never the store's live record. On the old code the
// returned *Order WAS the live record: the HTTP handler rendered
// makeOrderResponse() on it while the engine could concurrently write it (a
// relayed self-echo bumps Updated via store.Touch; a remote cancel writes
// Status) — a real data race. Part 1 mutates the returned order and asserts the
// book is unaffected (deterministically fails on the old aliasing code); part 2
// runs engine-style store.Touch writes concurrently with response renders, which
// must touch disjoint memory (green under -race only on the fixed code).
func TestMakeOrderReturnsStoreCopy(t *testing.T) {
	hubPriv, hubPub, _, _ := hubKey(t, 0x60)
	reg := servicenode.NewRegistry()
	reg.AddPing(servicenode.ServiceNode{
		PubKey: hubPub, Tier: servicenode.TierSPV, Services: []string{"BTC", "SYS"}, XBridgeVersion: proto.ProtocolVersion,
	})
	n, _ := newHubNode(reg)

	o, rerr := n.MakeOrder(MakeOrderParams{
		Maker: "BTC", MakerSize: "1.5", MakerAddress: btcAddr,
		Taker: "SYS", TakerSize: "0.3", TakerAddress: btcAddr2,
	})
	if rerr != nil {
		t.Fatalf("MakeOrder: %v", rerr)
	}
	key := hexEncode(o.ID[:])
	if n.store.Get(key) == nil {
		t.Fatal("order was not added to the store")
	}
	liveUpdated := n.store.Get(key).Updated

	// 1. Aliasing: mutating the returned order must not corrupt the book.
	o.Updated = liveUpdated + 1
	o.Status = "canceled"
	got := n.store.Get(key)
	if got.Updated != liveUpdated {
		t.Fatalf("returned order aliases the live record: mutating it changed store Updated (%d -> %d)", liveUpdated, got.Updated)
	}
	if got.Status != "open" {
		t.Fatalf("returned order aliases the live record: mutating it changed store Status to %q", got.Status)
	}

	// 2. Render-vs-engine-write race: an engine-side store.Touch writes the
	// LIVE record's Updated while the handler renders the returned order. With
	// the fix these touch disjoint memory (-race stays green); on the old code
	// the returned pointer IS the live record and this is a flagged data race.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 5000; i++ {
			n.store.Touch(key)
		}
	}()
	for i := 0; i < 2000; i++ {
		_ = o.makeOrderResponse()
		_ = iso8601(o.Updated)
	}
	<-done
	_ = hubPriv
}

// TestTakeOrderDryRunSkipsHubGate mirrors the make side: an unpinned order can
// still be dry-run taken (preview only) — no hub required, no Accepting written.
func TestTakeOrderDryRunSkipsHubGate(t *testing.T) {
	n, cc := newHubNode(servicenode.NewRegistry())
	id := [32]byte{0x23}
	n.store.Add(&Order{
		ID: id, Type: OrderTypeBroadcast, FromCurrency: "BTC", FromAmount: 1500000,
		ToCurrency: "BTC", ToAmount: 300000, Status: "open", Mine: false,
	})
	res, rerr := n.TakeOrder(TakeOrderParams{ID: dispID(id), FromAddress: btcAddr, ToAddress: btcAddr2, DryRun: true})
	if rerr != nil {
		t.Fatalf("TakeOrder(dry-run, unpinned) = %v, want success", rerr)
	}
	if res.Status != "filled" {
		t.Fatalf("dry-run result = %+v, want a filled preview", res)
	}
	if len(cc.snapshot()) != 0 {
		t.Fatalf("dry-run take wrote %d packets, want 0", len(cc.snapshot()))
	}
}

// TestMakeOrderPicksHub verifies dxMakeOrder selects a running, version-matching
// servicenode advertising both currencies: the SEND is addressed to that hub and
// the order carries the hub anchor (SNodePubkey/HubAddress, C++ OrderDescr).
func TestMakeOrderPicksHub(t *testing.T) {
	hubPriv, hubPub, hubPubHex, hubAddr := hubKey(t, 0x51)
	reg := servicenode.NewRegistry()
	reg.AddPing(servicenode.ServiceNode{
		PubKey: hubPub, Tier: servicenode.TierSPV, Services: []string{"BTC", "SYS"}, XBridgeVersion: proto.ProtocolVersion,
	})
	n, cc := newHubNode(reg)

	o, rerr := n.MakeOrder(MakeOrderParams{
		Maker: "BTC", MakerSize: "1.5", MakerAddress: btcAddr,
		Taker: "SYS", TakerSize: "0.3", TakerAddress: btcAddr2,
	})
	if rerr != nil {
		t.Fatalf("MakeOrder: %v", rerr)
	}

	pkts := cc.snapshot()
	if len(pkts) != 1 {
		t.Fatalf("wrote %d packets, want 1 SEND", len(pkts))
	}
	if pkts[0].Command != proto.XbcTransaction {
		t.Fatalf("command = %d, want XbcTransaction", pkts[0].Command)
	}
	dests := cc.snapshotDests()
	if len(dests) != 1 || dests[0] != hubAddr {
		t.Fatalf("SEND destination = %x, want chosen hub %x", dests[0], hubAddr)
	}
	if o.SNodePubkey != hubPubHex || o.HubAddress != hubAddr {
		t.Fatalf("order hub anchor = %q/%x, want %q/%x", o.SNodePubkey, o.HubAddress, hubPubHex, hubAddr)
	}
	if o.Mine != true || o.Role != 'A' {
		t.Fatalf("order should be a local maker (Mine=%v Role=%c)", o.Mine, o.Role)
	}
	// The maker session must be pinned to the chosen hub.
	s := n.sessions[hexEncode(o.ID[:])]
	if s == nil || s.hubKey != hubPub || s.hub != hubAddr {
		t.Fatalf("maker session not pinned to chosen hub: %+v", s)
	}
	_ = hubPriv
}

// TestTakeOrderUnpinnedRefused verifies dxTakeOrder refuses an order with no
// trusted, registered hub with NO_SERVICE_NODE (C++ acceptXBridgeTransaction:
// 2168-2197). The empty registry means no key is a known servicenode.
func TestTakeOrderUnpinnedRefused(t *testing.T) {
	n, cc := newHubNode(servicenode.NewRegistry())
	id := [32]byte{0x21}
	n.store.Add(&Order{
		ID: id, Type: OrderTypeBroadcast, FromCurrency: "BTC", FromAmount: 1500000,
		ToCurrency: "BTC", ToAmount: 300000, Status: "open", Mine: false,
	})
	_, rerr := n.TakeOrder(TakeOrderParams{ID: dispID(id), FromAddress: btcAddr, ToAddress: btcAddr2})
	if rerr == nil || rerr.Code != errNoServiceNode {
		t.Fatalf("TakeOrder(unpinned) = %v, want errNoServiceNode", rerr)
	}
	if len(cc.snapshot()) != 0 {
		t.Fatalf("no Accepting should be written for an unpinned order")
	}
}

// TestTakeOrderPinnedAccepting verifies a taken order with a valid hub anchor
// produces an Accepting body led by the order's HubAddress and addressed to the
// pinned hub, once the hub is a registered servicenode (STRICT hubRegistered,
// C++ getSn — membership only).
func TestTakeOrderPinnedAccepting(t *testing.T) {
	_, hubPub, hubPubHex, hubAddr := hubKey(t, 0x52)
	reg := servicenode.NewRegistry()
	reg.AddPing(servicenode.ServiceNode{
		PubKey: hubPub, Tier: servicenode.TierSPV, Services: []string{"BTC", "SYS"}, XBridgeVersion: proto.ProtocolVersion,
		PaymentAddress: hubAddr,
	})
	n, cc := newHubNode(reg)
	id := [32]byte{0x22}
	n.store.Add(&Order{
		ID: id, Type: OrderTypeBroadcast, FromCurrency: "BTC", FromAmount: 1500000,
		ToCurrency: "BTC", ToAmount: 300000, Status: "open", Mine: false,
		SNodePubkey: hubPubHex, HubAddress: hubAddr,
	})
	res, rerr := n.TakeOrder(TakeOrderParams{ID: dispID(id), FromAddress: btcAddr, ToAddress: btcAddr2})
	if rerr != nil {
		t.Fatalf("TakeOrder(pinned): %v", rerr)
	}
	if res.ID != dispID(id) {
		t.Fatalf("result id = %q, want %q", res.ID, dispID(id))
	}

	pkts := cc.snapshot()
	if len(pkts) != 1 {
		t.Fatalf("wrote %d packets, want 1 Accepting", len(pkts))
	}
	if pkts[0].Command != proto.XbcTransactionAccepting {
		t.Fatalf("command = %d, want XbcTransactionAccepting", pkts[0].Command)
	}
	dests := cc.snapshotDests()
	if len(dests) != 1 || dests[0] != hubAddr {
		t.Fatalf("Accepting destination = %x, want hub %x", dests[0], hubAddr)
	}
	acc, err := proto.DecodeBody(proto.XbcTransactionAccepting, pkts[0].Body)
	if err != nil {
		t.Fatalf("decode Accepting body: %v", err)
	}
	ab, ok := acc.(*proto.AcceptingBody)
	if !ok {
		t.Fatalf("body = %T, want *proto.AcceptingBody", acc)
	}
	if ab.HubAddress != hubAddr {
		t.Fatalf("Accepting body hubAddress = %x, want %x", ab.HubAddress, hubAddr)
	}
	// The Accepting must carry a real service-node fee tx AND the taker's
	// signed funding utxo entries — an empty pair is dropped by the hub
	// (xbridgesession.cpp:848-916, crBadFeeTx). The body must be >= 188 bytes.
	if len(ab.ServiceNodeFeeTx) == 0 {
		t.Fatal("Accepting body has an empty ServiceNodeFeeTx")
	}
	if len(ab.Utxos) == 0 {
		t.Fatal("Accepting body has no funding utxo entries")
	}
	if len(pkts[0].Body) < 188 {
		t.Fatalf("Accepting packet body = %d bytes, want >= 188 (hub drop gate)", len(pkts[0].Body))
	}
	// The fee tx's 0.015 BLOCK output must pay the hub's registry payment
	// address (snode.getPaymentAddress(), xbridgeapp.cpp:2196), not the body's
	// hubAddress alias or a zero address.
	if got := hasFeeOutput(ab.ServiceNodeFeeTx, hubAddr); !got {
		t.Fatalf("Accepting fee tx does not pay the registry payment address %x", hubAddr)
	}
	// The taker session must be pinned to the order's hub.
	s := n.sessions[hexEncode(id[:])]
	if s == nil || s.hubKey != hubPub || s.hub != hubAddr {
		t.Fatalf("taker session not pinned to order hub: %+v", s)
	}
}

// hasFeeOutput decodes the fee tx bytes and reports whether some output is a
// 25-byte P2PKH paying at least serviceNodeFee*COIN to the given 20-byte key
// (mirrors the hub receiver's check, xbridgesession.cpp:895-911).
func hasFeeOutput(feeTx []byte, dest [20]byte) bool {
	tx, err := coins.Deserialize(feeTx)
	if err != nil {
		return false
	}
	want := uint64(serviceNodeFeeReal * 100000000)
	for _, out := range tx.Outputs {
		if out.Value < want {
			continue
		}
		if len(out.ScriptPubKey) != 25 || out.ScriptPubKey[0] != 0x76 || out.ScriptPubKey[1] != 0xa9 || out.ScriptPubKey[2] != 0x14 {
			continue
		}
		if bytes.Equal(out.ScriptPubKey[3:23], dest[:]) {
			return true
		}
	}
	return false
}

// TestStaleHubKnownTakenNotPicked locks the C++ split between the take and
// make hub gates. hubRegistered mirrors sn::ServiceNodeMgr::getSn
// (servicenodemgr.h:418-432): a plain snodes map lookup with no running
// filter. An order anchored to a hub that is known (SNREGISTER) but has never
// pinged can still be taken (acceptXBridgeTransaction, xbridgeapp.cpp:2179,
// only rejects when getSn is null). Make hub selection is different: Pick
// keeps the running() filter (findNodeWithService, xbridgeapp.cpp:2910), so
// the same stale-but-known node is never selected for a new make.
func TestStaleHubKnownTakenNotPicked(t *testing.T) {
	priv, hubPub, hubPubHex, hubAddr := hubKey(t, 0x55)
	reg := servicenode.NewRegistry()
	hubReg, err := servicenode.SignRegistration(servicenode.ServiceNode{
		PubKey:         hubPub,
		Tier:           servicenode.TierSPV,
		PaymentAddress: hubAddr,
		Collateral:     []servicenode.CollateralUTXO{{TxID: [32]byte{0x01}, Vout: 0}},
		BestBlock:      1000,
	}, priv[:])
	if err != nil {
		t.Fatalf("SignRegistration: %v", err)
	}
	reg.AddRegistration(hubReg)
	n, cc := newHubNode(reg)

	// Make still requires a RUNNING hub: a registered-but-never-pinged node is
	// known, yet Pick filters it out and dxMakeOrder fails NO_SERVICE_NODE.
	if _, rerr := n.MakeOrder(MakeOrderParams{
		Maker: "BTC", MakerSize: "1.5", MakerAddress: btcAddr,
		Taker: "SYS", TakerSize: "0.3", TakerAddress: btcAddr2,
	}); rerr == nil || rerr.Code != errNoServiceNode {
		t.Fatalf("MakeOrder(stale-but-known hub) = %v, want errNoServiceNode", rerr)
	}

	// Take only requires MEMBERSHIP (getSn): the stale-but-known hub is a valid
	// take anchor and the Accepting is addressed to it.
	id := [32]byte{0x45}
	n.store.Add(&Order{
		ID: id, Type: OrderTypeBroadcast, FromCurrency: "BTC", FromAmount: 1500000,
		ToCurrency: "BTC", ToAmount: 300000, Status: "open", Mine: false,
		SNodePubkey: hubPubHex, HubAddress: hubAddr,
	})
	res, rerr := n.TakeOrder(TakeOrderParams{ID: dispID(id), FromAddress: btcAddr, ToAddress: btcAddr2})
	if rerr != nil {
		t.Fatalf("TakeOrder(stale-but-known hub) = %v, want success", rerr)
	}
	if res.ID != dispID(id) {
		t.Fatalf("result id = %q, want %q", res.ID, dispID(id))
	}
	pkts := cc.snapshot()
	if len(pkts) != 1 || pkts[0].Command != proto.XbcTransactionAccepting {
		t.Fatalf("wrote %d packets, want 1 Accepting", len(pkts))
	}
	dests := cc.snapshotDests()
	if len(dests) != 1 || dests[0] != hubAddr {
		t.Fatalf("Accepting destination = %x, want stale-but-known hub %x", dests[0], hubAddr)
	}
	if s := n.sessions[hexEncode(id[:])]; s == nil || s.hubKey != hubPub || s.hub != hubAddr {
		t.Fatalf("taker session not pinned to stale-but-known hub: %+v", s)
	}
}

// TestIngestPending verifies the xbcPendingTransaction ingest path: the wire's
// 20-byte hub field is the broadcaster's per-session id (m_myid), NOT GetID of
// the signing key (xbridgesession.cpp:182-183,804-811), so an order whose
// HubAddress != GetID(signer) is still ingested with the header key stored as
// SNodePubkey. A known order is never re-created (a local/taken order's state
// survives a relayed copy); a canceled order may be re-accepted via
// rebroadcast.
func TestIngestPending(t *testing.T) {
	_, _, hubPubHex, hubAddr := hubKey(t, 0x53)
	forgerPriv := make([]byte, 32)
	forgerPriv[31] = 0x54
	ordID := [32]byte{0x31}

	newBody := func() *proto.PendingTransactionBody {
		return &proto.PendingTransactionBody{
			ID: ordID, FromCurrency: "BTC", FromAmount: 1500000,
			ToCurrency: "BTC", ToAmount: 300000,
			HubAddress: hubAddr, Created: NowMicro(),
			PartialAllowed: false, MinFromAmount: 0,
		}
	}

	// 1. A broadcast whose hub field != GetID(signing key) is NORMAL (the field
	//    is the broadcaster's session id): it is ingested with the header key
	//    as SNodePubkey and the hub field stored verbatim.
	n1 := &Node{store: NewStore()}
	n1.ingestPending(newBody(), hexPub(t, forgerPriv))
	if g1 := n1.store.Get(hexEncode(ordID[:])); g1 == nil {
		t.Fatal("broadcast with non-GetID hub field was not ingested")
	} else if g1.SNodePubkey != hexPub(t, forgerPriv) || g1.HubAddress != hubAddr {
		t.Fatalf("ingested order lost snode/hub fields: %+v", g1)
	}

	// 2. A relayed copy of a known order must NOT replace it (C++ only
	//    refreshes the timestamp). Simulate a taken order and re-ingest.
	n2 := &Node{store: NewStore()}
	n2.ingestPending(newBody(), hubPubHex)
	ex := n2.store.Get(hexEncode(ordID[:]))
	if ex == nil {
		t.Fatal("valid broadcast was not ingested")
	} else if ex.SNodePubkey != hubPubHex || ex.HubAddress != hubAddr {
		t.Fatalf("ingested order lost snode/hub fields: %+v", ex)
	}
	n2.store.Update(hexEncode(ordID[:]), func(o *Order) {
		o.Role = 'B'
		o.Status = "accepting"
	})
	n2.ingestPending(newBody(), hubPubHex)
	after := n2.store.Get(hexEncode(ordID[:]))
	if after == nil {
		t.Fatal("known order was lost after re-ingest")
	}
	if after.Role != 'B' || after.Status != "accepting" {
		t.Fatalf("relayed copy clobbered the taken order: %+v", after)
	}

	// 3. A canceled order must NOT be re-accepted via rebroadcast (mirrors C++
	//    appendTransaction's history guard: known orders are never replaced). The
	//    rebroadcast is silently dropped.
	n2.store.Update(hexEncode(ordID[:]), func(o *Order) {
		o.Status = "canceled"
	})
	n2.ingestPending(newBody(), hubPubHex)
	after = n2.store.Get(hexEncode(ordID[:]))
	if after == nil {
		t.Fatal("canceled order should remain live after rejected rebroadcast")
	}
	if after.Status != "canceled" {
		t.Fatalf("canceled order was re-accepted as %q, want canceled", after.Status)
	}
}

// pinnedHub registers a running SPV servicenode (with a payment address) for
// the take-path tests and returns its pubkey hex and address.
func pinnedHub(t *testing.T, seed byte) (_ *servicenode.Registry, pubHex string, hubAddr [20]byte) {
	t.Helper()
	_, hubPub, hubPubHex, hubAddr := hubKey(t, seed)
	reg := servicenode.NewRegistry()
	reg.AddPing(servicenode.ServiceNode{
		PubKey: hubPub, Tier: servicenode.TierSPV, Services: []string{"BTC", "SYS"}, XBridgeVersion: proto.ProtocolVersion,
		PaymentAddress: hubAddr,
	})
	return reg, hubPubHex, hubAddr
}

// TestTakeOrderNoBlockConnector makes the availableBalance pre-check fail
// INSUFFICIENT_FUNDS_DX when no BLOCK connector is configured: C++
// acceptXBridgeTransaction:2161 computes availableBalance() from the daemon's
// loaded wallets (GetWallets(), i.e. the BLOCK wallet) and with no BLOCK wallet
// the balance is 0, below the service-node fee. Assert nothing is broadcast.
func TestTakeOrderNoBlockConnector(t *testing.T) {
	reg, pubHex, hubAddr := pinnedHub(t, 0x61)
	n, cc := newHubNodeWithoutBlock(reg)
	id := [32]byte{0x71}
	n.store.Add(&Order{
		ID: id, Type: OrderTypeBroadcast, FromCurrency: "BTC", FromAmount: 1500000,
		ToCurrency: "BTC", ToAmount: 300000, Status: "open", Mine: false,
		SNodePubkey: pubHex, HubAddress: hubAddr,
	})
	_, rerr := n.TakeOrder(TakeOrderParams{ID: dispID(id), FromAddress: btcAddr, ToAddress: btcAddr2})
	if rerr == nil || rerr.Code != errInsufficientFundsDX {
		t.Fatalf("TakeOrder(no BLOCK connector) = %v, want errInsufficientFundsDX", rerr)
	}
	if len(cc.snapshot()) != 0 {
		t.Fatalf("no Accepting should be written when the fee cannot be prepared")
	}
}

// TestTakeOrderUnfundedBLOCKFee makes the fee-prep step fail INSUFFICIENT_FUNDS
// when the BLOCK wallet holds no spendable p2pkh funder (selectFeeUtxos finds
// nothing, C++ :2240).
func TestTakeOrderUnfundedBLOCKFee(t *testing.T) {
	reg, pubHex, hubAddr := pinnedHub(t, 0x62)
	n, cc := newHubNodeUnfundedBlock(reg)
	id := [32]byte{0x72}
	n.store.Add(&Order{
		ID: id, Type: OrderTypeBroadcast, FromCurrency: "BTC", FromAmount: 1500000,
		ToCurrency: "BTC", ToAmount: 300000, Status: "open", Mine: false,
		SNodePubkey: pubHex, HubAddress: hubAddr,
	})
	_, rerr := n.TakeOrder(TakeOrderParams{ID: dispID(id), FromAddress: btcAddr, ToAddress: btcAddr2})
	if rerr == nil || rerr.Code != errInsufficientFunds {
		t.Fatalf("TakeOrder(unfunded BLOCK) = %v, want errInsufficientFunds", rerr)
	}
	if len(cc.snapshot()) != 0 {
		t.Fatalf("no Accepting should be written for an unfunded fee")
	}
}

// zeroBalanceConn is a stubConn whose wallet reports no balance, modeling a
// configured-but-empty BLOCK wallet for the availableBalance gate.
type zeroBalanceConn struct{ *stubConn }

func (z *zeroBalanceConn) GetBalance() (uint64, error) { return 0, nil }

// newHubNodeEmptyBlockWallet is buildHubNode with a BLOCK connector that
// reports zero balance, so the availableBalance pre-check (C++ :2161) fails
// INSUFFICIENT_FUNDS_DX before any fee work — unlike newHubNodeUnfundedBlock,
// whose stub models a funded wallet with no spendable p2pkh fee utxos
// (fee-prep INSUFFICIENT_FUNDS, C++ :2240).
func newHubNodeEmptyBlockWallet(reg *servicenode.Registry) (*Node, *captureXConn) {
	n, cc := buildHubNode(reg, []wallet.Utxo{
		{TxID: "0000000000000000000000000000000000000000000000000000000000000001", Vout: 0,
			Amount: 300000000, Value: 3.0, ScriptPubKey: "76a914000000000000000000000000000000000000000088ac", Address: btcAddr},
	}, []wallet.Utxo{blkUtxo()})
	n.config.Connectors["BLOCK"] = &zeroBalanceConn{&stubConn{ticker: "BLOCK", addr: btcAddr, utxos: []wallet.Utxo{blkUtxo()}}}
	return n, cc
}

// TestTakeOrderEmptyBlockWallet proves the availableBalance gate fails a
// take with INSUFFICIENT_FUNDS_DX when the BLOCK wallet is configured but its
// balance is below the service-node fee (C++ acceptXBridgeTransaction:2161)
// and nothing is broadcast.
func TestTakeOrderEmptyBlockWallet(t *testing.T) {
	reg, pubHex, hubAddr := pinnedHub(t, 0x69)
	n, cc := newHubNodeEmptyBlockWallet(reg)
	id := [32]byte{0x79}
	n.store.Add(&Order{
		ID: id, Type: OrderTypeBroadcast, FromCurrency: "BTC", FromAmount: 1500000,
		ToCurrency: "BTC", ToAmount: 300000, Status: "open", Mine: false,
		SNodePubkey: pubHex, HubAddress: hubAddr,
	})
	_, rerr := n.TakeOrder(TakeOrderParams{ID: dispID(id), FromAddress: btcAddr, ToAddress: btcAddr2})
	if rerr == nil || rerr.Code != errInsufficientFundsDX {
		t.Fatalf("TakeOrder(empty BLOCK wallet) = %v, want errInsufficientFundsDX", rerr)
	}
	if len(cc.snapshot()) != 0 {
		t.Fatalf("no Accepting should be written when the BLOCK balance is below the service-node fee")
	}
}

// TestTakeOrderUnfundedFromCurrency makes the take fail INSUFFICIENT_FUNDS when
// the from-currency wallet cannot cover the take amount. The new checkAcceptParams
// pre-check (C++ rpcxbridge.cpp:1204 -> checkAmount xbridgeapp.cpp:2561-2580)
// fires here with a zero balance; it runs BEFORE the funding selectUtxos step.
func TestTakeOrderUnfundedFromCurrency(t *testing.T) {
	reg, pubHex, hubAddr := pinnedHub(t, 0x63)
	n, cc := newHubNodeUtxos(reg, nil) // BTC wallet has no utxos
	id := [32]byte{0x73}
	n.store.Add(&Order{
		ID: id, Type: OrderTypeBroadcast, FromCurrency: "BTC", FromAmount: 1500000,
		ToCurrency: "BTC", ToAmount: 300000, Status: "open", Mine: false,
		SNodePubkey: pubHex, HubAddress: hubAddr,
	})
	_, rerr := n.TakeOrder(TakeOrderParams{ID: dispID(id), FromAddress: btcAddr, ToAddress: btcAddr2})
	if rerr == nil || rerr.Code != errInsufficientFunds {
		t.Fatalf("TakeOrder(unfunded from-currency) = %v, want errInsufficientFunds", rerr)
	}
	if len(cc.snapshot()) != 0 {
		t.Fatalf("no Accepting should be written for unfunded takers")
	}
}

// TestTakeOrderDust refuses a take whose taker amount falls below the dust
// threshold with DUST (C++ acceptXBridgeTransaction :2147-2157).
func TestTakeOrderDust(t *testing.T) {
	reg, pubHex, hubAddr := pinnedHub(t, 0x64)
	n, cc := newHubNode(reg)
	id := [32]byte{0x74}
	n.store.Add(&Order{
		ID: id, Type: OrderTypeBroadcast, FromCurrency: "BTC", FromAmount: 1500000,
		ToCurrency: "BTC", ToAmount: 10, Status: "open", Mine: false,
		SNodePubkey: pubHex, HubAddress: hubAddr,
	})
	_, rerr := n.TakeOrder(TakeOrderParams{ID: dispID(id), FromAddress: btcAddr, ToAddress: btcAddr2})
	if rerr == nil || rerr.Code != errDust {
		t.Fatalf("TakeOrder(dust) = %v, want errDust", rerr)
	}
	if len(cc.snapshot()) != 0 {
		t.Fatalf("no Accepting should be written for a dust take")
	}
}

// TestTakeOrderDryRunSkipsDust proves C++ renders the dryrun preview BEFORE the
// accept path's dust checks (rpcxbridge.cpp:1227 vs xbridgeapp.cpp:2147-2157),
// so a dry-run take of an under-dust order succeeds with a filled preview even
// though a real take of the same order is refused DUST.
func TestTakeOrderDryRunSkipsDust(t *testing.T) {
	reg, pubHex, hubAddr := pinnedHub(t, 0x65)
	n, cc := newHubNode(reg)
	id := [32]byte{0x75}
	n.store.Add(&Order{
		ID: id, Type: OrderTypeBroadcast, FromCurrency: "BTC", FromAmount: 1500000,
		ToCurrency: "BTC", ToAmount: 10, Status: "open", Mine: false,
		SNodePubkey: pubHex, HubAddress: hubAddr,
	})
	res, rerr := n.TakeOrder(TakeOrderParams{ID: dispID(id), FromAddress: btcAddr, ToAddress: btcAddr2, DryRun: true})
	if rerr != nil {
		t.Fatalf("TakeOrder(dry-run dust) = %v, want a filled preview", rerr)
	}
	if res.Status != "filled" {
		t.Fatalf("dry-run dust result = %+v, want a filled preview", res)
	}
	if len(cc.snapshot()) != 0 {
		t.Fatalf("dry-run take wrote %d packets, want 0", len(cc.snapshot()))
	}
}

// TestTakeOrderDryRunUnderFunded proves checkAcceptParams (C++ rpcxbridge.cpp:
// 1204) runs BEFORE the dryrun branch: an empty from-currency wallet fails a
// dry-run take with INSUFFICIENT_FUNDS, not a preview.
func TestTakeOrderDryRunUnderFunded(t *testing.T) {
	reg, pubHex, hubAddr := pinnedHub(t, 0x66)
	n, _ := newHubNodeUtxos(reg, nil) // BTC wallet has no utxos
	id := [32]byte{0x76}
	n.store.Add(&Order{
		ID: id, Type: OrderTypeBroadcast, FromCurrency: "BTC", FromAmount: 1500000,
		ToCurrency: "BTC", ToAmount: 300000, Status: "open", Mine: false,
		SNodePubkey: pubHex, HubAddress: hubAddr,
	})
	_, rerr := n.TakeOrder(TakeOrderParams{ID: dispID(id), FromAddress: btcAddr, ToAddress: btcAddr2, DryRun: true})
	if rerr == nil || rerr.Code != errInsufficientFunds {
		t.Fatalf("TakeOrder(dry-run, under-funded) = %v, want errInsufficientFunds", rerr)
	}
}

// TestTakeOrderDryRunNoSession proves checkAcceptParams reports NO_SESSION for a
// currency without a live connector before the dryrun branch, matching the C++
// checkAmount connector guard (xbridgeapp.cpp:2567-2571).
func TestTakeOrderDryRunNoSession(t *testing.T) {
	reg, _, _ := pinnedHub(t, 0x67)
	n, _ := newHubNode(reg) // no LTC connector
	id := [32]byte{0x77}
	n.store.Add(&Order{
		ID: id, Type: OrderTypeBroadcast, FromCurrency: "BTC", FromAmount: 1500000,
		ToCurrency: "LTC", ToAmount: 300000, Status: "open", Mine: false,
	})
	_, rerr := n.TakeOrder(TakeOrderParams{ID: dispID(id), FromAddress: btcAddr, ToAddress: btcAddr2, DryRun: true})
	if rerr == nil || rerr.Code != errNoSession {
		t.Fatalf("TakeOrder(dry-run, no session) = %v, want errNoSession", rerr)
	}
}

// TestTakeOrderBadAddressAfterFundsGate proves address validation is deferred
// until AFTER checkAcceptParams (C++ rpcxbridge.cpp:1219-1225 isValidAddress
// runs after the :1204 balance gate): with an under-funded wallet and invalid
// addresses, the take fails INSUFFICIENT_FUNDS, never INVALID_ADDRESS.
func TestTakeOrderBadAddressAfterFundsGate(t *testing.T) {
	reg, pubHex, hubAddr := pinnedHub(t, 0x68)
	n, _ := newHubNodeUtxos(reg, nil) // BTC wallet has no utxos
	id := [32]byte{0x78}
	n.store.Add(&Order{
		ID: id, Type: OrderTypeBroadcast, FromCurrency: "BTC", FromAmount: 1500000,
		ToCurrency: "BTC", ToAmount: 300000, Status: "open", Mine: false,
		SNodePubkey: pubHex, HubAddress: hubAddr,
	})
	_, rerr := n.TakeOrder(TakeOrderParams{ID: dispID(id), FromAddress: "not-an-address-1", ToAddress: "not-an-address-2"})
	if rerr == nil || rerr.Code != errInsufficientFunds {
		t.Fatalf("TakeOrder(bad address, under-funded) = %v, want errInsufficientFunds (address gate must run later)", rerr)
	}
}

// TestMakePartialDustNativeScale locks in the partial minimum_size being
// compared in the coin's NATIVE base units (partialMinimum * COIN < dustAmount,
// xbridgewalletconnectorbtc.cpp:1900-1904), NOT in XBridge 1e6 base against a
// native dust value. buildHubNode confs set no conf dust (MinimumAmount), so
// effectiveDust = 0.546 * relayFee * COIN; the stub wallet reports relayFee
// 0.0001, giving 0.546 * 0.0001 * 1e8 = 5460 native (numerically equal to
// cppDustFallback).
// Native = minFrom * 1e8/1e6 = minFrom * 100: 0.000055 -> 5500 native (NOT
// dust), 0.000054 -> 5400 native (dust). The old 1e6-vs-native comparison
// rejected everything below 5460 XBridge base (i.e. 0.00546 coins), wrongly
// rejecting 0.000055.
func TestMakePartialDustNativeScale(t *testing.T) {
	n, _ := newHubNode(nil) // dryrun skips the hub gate, so no registry needed
	mk := func(min string) *rpcError {
		_, rerr := n.MakeOrder(MakeOrderParams{
			Maker: "BTC", MakerSize: "1.5", MakerAddress: btcAddr,
			Taker: "SYS", TakerSize: "0.3", TakerAddress: btcAddr2,
			Type: "partial", MinSize: min, DryRun: true,
		})
		return rerr
	}
	if rerr := mk("0.000055"); rerr != nil {
		t.Errorf("MakeOrder(min 0.000055) = %v, want NO dust error (native 5500 >= 5460)", rerr)
	}
	if rerr := mk("0.000054"); rerr == nil || rerr.Code != errInvalidParameters || !strings.Contains(rerr.Error, "dust") {
		t.Errorf("MakeOrder(min 0.000054) = %v, want 1025 dust error (native 5400 < 5460)", rerr)
	}
}

// TestTakeOrderBadAddressMessage locks in the address errors: C++ checks
// the TO address first (against the order's from-currency connector) and each
// message uses the arg ": <cur> address is bad. Are you using the correct
// address?" with name dxTakeOrder (rpcxbridge.cpp:1219-1225) — not the
// "dxMakeOrder"-leaked generic form. Distinct BTC/SYS legs prove the currency
// mapping (to -> order's from, from -> order's to).
func TestTakeOrderBadAddressMessage(t *testing.T) {
	reg, pubHex, hubAddr := pinnedHub(t, 0x69)
	n, _ := newHubNode(reg)
	// Fund SYS so checkAcceptParams (the taker's sending currency) passes and
	// the later address gate is reached.
	n.config.Connectors["SYS"] = &stubConn{ticker: "SYS", addr: btcAddr, utxos: []wallet.Utxo{{
		TxID: "0000000000000000000000000000000000000000000000000000000000000009", Vout: 0,
		Amount: 100000000, Value: 1.0, ScriptPubKey: "76a914000000000000000000000000000000000000000088ac", Address: btcAddr,
	}}}
	id := [32]byte{0x79}
	n.store.Add(&Order{
		ID: id, Type: OrderTypeBroadcast, FromCurrency: "BTC", FromAmount: 1500000,
		ToCurrency: "SYS", ToAmount: 300000, Status: "open", Mine: false,
		SNodePubkey: pubHex, HubAddress: hubAddr,
	})
	// TO address (taker's receiving leg = order's BTC) is checked first; the
	// message carries the order's FROM currency.
	if _, rerr := n.TakeOrder(TakeOrderParams{ID: dispID(id), FromAddress: btcAddr, ToAddress: "not-an-address"}); rerr == nil || rerr.Code != errInvalidAddress {
		t.Fatalf("TakeOrder(bad to address) = %v, want INVALID_ADDRESS", rerr)
	} else if rerr.Name != "dxTakeOrder" || rerr.Error != "Bad address : BTC address is bad. Are you using the correct address?" {
		t.Errorf("TakeOrder(bad to address) = %+v, want name dxTakeOrder + BTC message", rerr)
	}
	// FROM address (taker's sending leg = order's SYS) -> order's TO currency.
	if _, rerr := n.TakeOrder(TakeOrderParams{ID: dispID(id), FromAddress: "not-an-address", ToAddress: btcAddr}); rerr == nil || rerr.Code != errInvalidAddress || rerr.Error != "Bad address : SYS address is bad. Are you using the correct address?" {
		t.Errorf("TakeOrder(bad from address) = %v, want SYS message", rerr)
	}
}

// TestTakeOrderOwnOrder locks in C++ isLocal() -> 1025 "Unable to accept your
// own order." (rpcxbridge.cpp:1210-1212): Mine is the Go isLocal
// proxy, so a local maker order cannot be taken.
func TestTakeOrderOwnOrder(t *testing.T) {
	reg, pubHex, hubAddr := pinnedHub(t, 0x6a)
	n, _ := newHubNode(reg)
	id := [32]byte{0x7a}
	n.store.Add(&Order{
		ID: id, Type: OrderTypeBroadcast, FromCurrency: "BTC", FromAmount: 1500000,
		ToCurrency: "BTC", ToAmount: 300000, Status: "open", Mine: true,
		SNodePubkey: pubHex, HubAddress: hubAddr,
	})
	_, rerr := n.TakeOrder(TakeOrderParams{ID: dispID(id), FromAddress: btcAddr, ToAddress: btcAddr2})
	if rerr == nil || rerr.Code != errInvalidParameters || rerr.Error != "Invalid parameters: Unable to accept your own order." {
		t.Fatalf("TakeOrder(own order) = %v, want 1025 'Unable to accept your own order.'", rerr)
	}
}

// TestTakeOrderNotFoundBare locks in C++ dxTakeOrder's not-found message:
// makeError(TRANSACTION_NOT_FOUND, __FUNCTION__) with NO argument — the
// double-space "Transaction  not found" (rpcxbridge.cpp:1176-1179).
func TestTakeOrderNotFoundBare(t *testing.T) {
	n, _ := newHubNode(nil)
	var id [32]byte
	id[0] = 0x7b
	_, rerr := n.TakeOrder(TakeOrderParams{ID: dispID(id), FromAddress: btcAddr, ToAddress: btcAddr2})
	if rerr == nil || rerr.Code != errTxNotFound || rerr.Error != "Transaction  not found" {
		t.Fatalf("TakeOrder(unknown) = %v, want 1021 double-space message", rerr)
	}
}
