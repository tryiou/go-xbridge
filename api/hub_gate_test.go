package api

import (
	"testing"

	"go-xbridge/coins"
	"go-xbridge/config"
	"go-xbridge/crypto"
	"go-xbridge/p2p/servicenode"
	"go-xbridge/proto"
	"go-xbridge/wallet"
)

// newHubNode builds a Node wired for hub-gate tests: two wallet connectors,
// a captureXConn (so requireWrite passes and outbound packets are recorded),
// an empty store + session map, and the given service-node registry. The BTC
// connector is funded with a single 3.0 BTC utxo so selection passes for the
// standard 1.5 BTC exact make (C++ selectUtxos gt-single path).
func newHubNode(reg *servicenode.Registry) (*Node, *captureXConn) {
	return newHubNodeUtxos(reg, []wallet.Utxo{
		{TxID: "0000000000000000000000000000000000000000000000000000000000000001", Vout: 0,
			Amount: 300000000, Value: 3.0, ScriptPubKey: "76a914000000000000000000000000000000000000000088ac", Address: btcAddr},
	})
}

// newHubNodeUtxos is newHubNode with an explicit BTC utxo set (used by the
// Stage 3c make-order KATs: custom partial-exact wallets, unfunded wallets).
func newHubNodeUtxos(reg *servicenode.Registry, btc []wallet.Utxo) (*Node, *captureXConn) {
	coins.InitFromConf(map[string]*config.CoinConf{
		"BTC": {Ticker: "BTC", CreateTxMethod: "BTC", AddressPrefix: 0, ScriptPrefix: 5, Coin: 100000000},
		"SYS": {Ticker: "SYS", CreateTxMethod: "SYS", AddressPrefix: 0, ScriptPrefix: 5, Coin: 100000000},
	})
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
func TestMakeOrderNoRegistry(t *testing.T) {
	n, _ := newHubNode(nil)
	o, rerr := n.MakeOrder(MakeOrderParams{
		Maker: "BTC", MakerSize: "1.5", MakerAddress: btcAddr,
		Taker: "SYS", TakerSize: "0.3", TakerAddress: btcAddr2,
	})
	if rerr == nil || rerr.Code != errNoServiceNode {
		t.Fatalf("MakeOrder(nil registry) = %v, want errNoServiceNode", rerr)
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
	// The taker session must be pinned to the order's hub.
	s := n.sessions[hexEncode(id[:])]
	if s == nil || s.hubKey != hubPub || s.hub != hubAddr {
		t.Fatalf("taker session not pinned to order hub: %+v", s)
	}
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
	_, hubPub, hubPubHex, hubAddr := hubKey(t, 0x55)
	reg := servicenode.NewRegistry()
	reg.AddRegistration(servicenode.ServiceNode{
		PubKey: hubPub, Tier: servicenode.TierSPV, Services: []string{"BTC", "SYS"}, XBridgeVersion: proto.ProtocolVersion,
	})
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
	ex.Role = 'B'
	ex.Status = "accepting"
	n2.ingestPending(newBody(), hubPubHex)
	after := n2.store.Get(hexEncode(ordID[:]))
	if after != ex {
		t.Fatal("relayed copy replaced the known order")
	}
	if after.Role != 'B' || after.Status != "accepting" {
		t.Fatalf("relayed copy clobbered the taken order: %+v", after)
	}

	// 3. A canceled order may be re-accepted via rebroadcast (re-created as a
	//    fresh pending entry).
	ex.Status = "canceled"
	n2.ingestPending(newBody(), hubPubHex)
	after = n2.store.Get(hexEncode(ordID[:]))
	if after == ex {
		t.Fatal("canceled order was not re-created by the rebroadcast")
	}
	if after.Status != "open" {
		t.Fatalf("re-accepted order status = %q, want open", after.Status)
	}
}
