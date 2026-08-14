package api

import (
	"encoding/hex"
	"io"
	"sync"
	"testing"
	"time"

	"go-xbridge/coins"
	"go-xbridge/config"
	"go-xbridge/crypto"
	"go-xbridge/p2p/servicenode"
	"go-xbridge/proto"
	"go-xbridge/wallet"
)

// blockStub is a wallet.Connector that returns a fixed anti-replay block hash
// for height-1, used to exercise the order blockHash wiring (C1). It embeds
// stubConn to satisfy the rest of the Connector interface.
type blockStub struct {
	stubConn
	hash [32]byte
}

func (b *blockStub) GetBlockCount() (int64, error) { return 10, nil }
func (b *blockStub) GetBlockHash(height int64) ([32]byte, error) {
	return b.hash, nil
}

func newBlockNode(bs wallet.Connector) *Node {
	cfg := &Config{Connectors: map[string]wallet.Connector{"BLOCK": bs}}
	return &Node{config: cfg, signer: crypto.NewBtcSigner(), stop: make(chan struct{})}
}

func TestNodeRefreshBlock(t *testing.T) {
	var want [32]byte
	want[3] = 0x42
	want[31] = 0x99
	n := newBlockNode(&blockStub{hash: want})

	n.refreshBlock()
	if n.block != want {
		t.Fatalf("refreshBlock: got %x want %x", n.block, want)
	}
}

func TestNodeCurrentBlockHashFresh(t *testing.T) {
	var want [32]byte
	want[3] = 0x42
	n := newBlockNode(&blockStub{hash: want})

	// First call refreshes from the connector.
	got := n.currentBlockHash()
	if got != want {
		t.Fatalf("currentBlockHash (first): got %x want %x", got, want)
	}
	// Cached value (set within 60s) is returned unchanged.
	got = n.currentBlockHash()
	if got != want {
		t.Fatalf("currentBlockHash (cached): got %x want %x", got, want)
	}
}

func TestNodeCurrentBlockHashStale(t *testing.T) {
	var want [32]byte
	want[1] = 0x7f
	n := newBlockNode(&blockStub{hash: want})

	n.refreshBlock()
	// Force the cache stale; currentBlockHash must re-fetch from the connector.
	n.blockMu.Lock()
	n.blockAt = time.Now().Add(-10 * time.Minute)
	n.blockMu.Unlock()

	got := n.currentBlockHash()
	if got != want {
		t.Fatalf("currentBlockHash (stale): got %x want %x", got, want)
	}
}

func TestNodeRefreshBlockNoConnector(t *testing.T) {
	n := newBlockNode(nil) // no BLOCK connector
	n.refreshBlock()
	if n.block != [32]byte{} {
		t.Fatalf("expected zero block without connector, got %x", n.block)
	}
}

// captureXConn is a test XConn that records every packet written to it (and its
// envelope destination address). Its ReadPacket blocks forever (returns io.EOF)
// so a dispatch test cannot accidentally consume real input; we only care about
// the outbound direction.
type captureXConn struct {
	mu      sync.Mutex
	written []*proto.Packet
	dests   [][20]byte
}

func (c *captureXConn) ReadPacket() (*proto.Packet, string, error) {
	return nil, "", io.EOF
}
func (c *captureXConn) WritePacket(p *proto.Packet, dest [20]byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.written = append(c.written, p)
	c.dests = append(c.dests, dest)
	return nil
}
func (c *captureXConn) Close() error { return nil }
func (c *captureXConn) snapshot() []*proto.Packet {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]*proto.Packet, len(c.written))
	copy(out, c.written)
	return out
}
func (c *captureXConn) snapshotDests() [][20]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([][20]byte, len(c.dests))
	copy(out, c.dests)
	return out
}

// TestDispatchSwapSignsOutbound closes the untested outbound boundary: a hub
// packet dispatched through dispatchSwap must produce exactly one signed
// outbound packet, with the right command, whose signature verifies with our
// public key. This proves the trust boundary writes what the C++ hub expects.
func TestDispatchSwapSignsOutbound(t *testing.T) {
	coins.InitFromConf(map[string]*config.CoinConf{
		"BTC": {Ticker: "BTC", CreateTxMethod: "BTC", AddressPrefix: 0, ScriptPrefix: 5, Coin: 100000000},
	})

	// Per-trade M keypair (C++ mPubKey/mPrivKey).
	mPriv := make([]byte, 32)
	mPriv[31] = 1
	mPub, err := crypto.CompressedPubKey(mPriv)
	if err != nil {
		t.Fatal(err)
	}

	// The hub servicenode is chosen at make time and pinned on the session; the
	// order must carry the consistent hub anchor for the maker to pin it.
	hubPriv := make([]byte, 32)
	hubPriv[31] = 2
	hubPub, err := crypto.CompressedPubKey(hubPriv)
	if err != nil {
		t.Fatal(err)
	}

	var id [32]byte
	copy(id[:], []byte("order-id-order-id-order-id-0")) // 32 bytes exactly

	cfg := &Config{
		Connectors: map[string]wallet.Connector{"BTC": &stubConn{ticker: "BTC", addr: btcAddr}},
	}
	n := &Node{config: cfg, signer: crypto.NewBtcSigner(), stop: make(chan struct{}), sessions: map[string]*SwapSession{}}
	// STRICT hubRegistered (C++ getSn): the pinned hub must be a known
	// servicenode or the honest Hold would be dropped.
	reg := servicenode.NewRegistry()
	reg.AddPing(servicenode.ServiceNode{
		PubKey: hubPub, Tier: servicenode.TierSPV, Services: []string{"BTC"}, XBridgeVersion: proto.ProtocolVersion,
	})
	n.snReg = reg

	o := &Order{
		ID:           id,
		FromCurrency: "BTC",
		ToCurrency:   "LTC",
		FromAmount:   100000000,
		ToAmount:     200000000,
		SNodePubkey:  hexPub(t, hubPriv),
		HubAddress:   coins.KeyID(hubPub[:]),
	}
	n.newMakerSession(o, MakeOrderParams{MakerAddress: btcAddr, TakerAddress: btcAddr}, arr32(mPriv), mPub)

	cc := &captureXConn{}
	n.conn = cc

	// The inbound Hold must be signed by the session's pinned hub key (pinned at
	// newMakerSession from the order's SNodePubkey, STATE-F78).
	// OnHold drives the maker's response to a hub xbcTransactionHold. The body
	// carries the TAKER's give/take (order.to/order.from = 2e8/1e8); verifyHold
	// (STATE-F71) accepts it for this 1e8→2e8 maker order.
	hubPkt := proto.NewPacket(proto.XbcTransactionHold, (&proto.HoldBody{HubAddress: coins.KeyID(hubPub[:]), ID: id, FromAmount: 2e8, ToAmount: 1e8}).Marshal())
	if err := crypto.NewBtcSigner().Sign(hubPkt, hubPriv); err != nil {
		t.Fatal(err)
	}

	// OnHold drives the maker's response to a hub xbcTransactionHold.
	n.dispatchSwap(hubPkt, id, [20]byte{}, "Hold", func(s *SwapSession) (proto.XBridgeCommand, responseBody, error) {
		return s.OnHold(&proto.HoldBody{HubAddress: coins.KeyID(hubPub[:]), ID: id, FromAmount: 2e8, ToAmount: 1e8})
	})

	pkts := cc.snapshot()
	if len(pkts) != 1 {
		t.Fatalf("wrote %d packets, want exactly 1", len(pkts))
	}
	p := pkts[0]
	if p.Command != proto.XbcTransactionHoldApply {
		t.Fatalf("command = %d, want XbcTransactionHoldApply (%d)", p.Command, proto.XbcTransactionHoldApply)
	}
	ok, err := crypto.NewBtcSigner().Verify(p)
	if err != nil {
		t.Fatalf("Verify returned error: %v", err)
	}
	if !ok {
		t.Fatal("outbound packet signature did not verify")
	}
	// The reply must be addressed to the pinned hub, not broadcast.
	dests := cc.snapshotDests()
	if len(dests) != 1 || dests[0] != o.HubAddress {
		t.Fatalf("reply destination = %x, want pinned hub %x", dests[0], o.HubAddress)
	}
}

// TestDecodeAddr exercises the per-currency address decoder used by every
// swap handshake: a valid P2PKH and a valid P2WPKH both resolve to the right
// 20-byte id; unknown coins and malformed addresses surface as *rpcError (no
// silent default, no panic).
func TestDecodeAddr(t *testing.T) {
	coins.InitFromConf(map[string]*config.CoinConf{
		"BTC": {Ticker: "BTC", CreateTxMethod: "BTC", AddressPrefix: 0, ScriptPrefix: 5, Coin: 100000000},
	})

	// Valid P2PKH for version 0x00 — must decode to a non-zero 20-byte id.
	id, e := decodeAddr("dxMakeOrder", "BTC", btcAddr)
	if e != nil {
		t.Fatalf("P2PKH decode: %v", e)
	}
	if id == ([20]byte{}) {
		t.Fatal("P2PKH decoded to an empty id")
	}

	// Valid P2WPKH (BIP173 test vector BC1QW508D6QEJXTDG4Y5R3ZARVARY0C5XW7KV8F3T4);
	// its id equals the 20-byte witness program 751e76e8199196d454941c45d1b3a323f1433bd6.
	id2, e := decodeAddr("dxMakeOrder", "BTC", "BC1QW508D6QEJXTDG4Y5R3ZARVARY0C5XW7KV8F3T4")
	if e != nil {
		t.Fatalf("P2WPKH decode: %v", e)
	}
	var want [20]byte
	if _, err := hex.Decode(want[:], []byte("751e76e8199196d454941c45d1b3a323f1433bd6")); err != nil {
		t.Fatalf("test vector decode: %v", err)
	}
	if id2 != want {
		t.Fatalf("P2WPKH id = %x, want %x", id2, want)
	}

	// Malformed address -> rpcError.
	if _, e := decodeAddr("dxMakeOrder", "BTC", "not-an-address"); e == nil {
		t.Fatal("expected rpcError for malformed address")
	}
	// Unknown coin -> rpcError.
	if _, e := decodeAddr("dxMakeOrder", "NOPE", btcAddr); e == nil {
		t.Fatal("expected rpcError for unknown coin")
	}
}

// --- processTransactionCancel / processTransactionReject port tests ---

// signBodyPacket signs a body with priv and returns the packet whose Pubkey is
// the compressed pubkey of priv (matching the C++ wire contract).
func signBodyPacket(t *testing.T, cmd proto.XBridgeCommand, body []byte, priv []byte) *proto.Packet {
	t.Helper()
	pkt := proto.NewPacket(cmd, body)
	if err := crypto.NewBtcSigner().Sign(pkt, priv); err != nil {
		t.Fatalf("sign packet: %v", err)
	}
	return pkt
}

func newCancelTestNode(conn XConn) *Node {
	return &Node{
		config:   &Config{Connectors: map[string]wallet.Connector{"BTC": &stubConn{ticker: "BTC", addr: btcAddr}}},
		signer:   crypto.NewBtcSigner(),
		stop:     make(chan struct{}),
		store:    NewStore(),
		sessions: map[string]*SwapSession{},
		conn:     conn,
	}
}

func mustID(t *testing.T) (id [32]byte, idHex string) {
	t.Helper()
	copy(id[:], []byte("cancel-test-order-id-000000000")) // 32 bytes
	return id, hexEncode(id[:])
}

// TestRemoteCancelObservedOpen moves an observed (non-local) open order with a
// valid snode-signed cancel into history as "canceled".
func TestRemoteCancelObservedOpen(t *testing.T) {
	snodePriv := make([]byte, 32)
	snodePriv[0] = 0x11
	_, idHex := mustID(t)
	o := &Order{FromCurrency: "BTC", ToCurrency: "LTC", FromAmount: 1e6, ToAmount: 2e6,
		Status: "open", SNodePubkey: hexPub(t, snodePriv)}
	o.ID = decodeID(t, idHex)

	n := newCancelTestNode(nil)
	n.store.Add(o)

	pkt := signBodyPacket(t, proto.XbcTransactionCancel, (&proto.CancelBody{ID: o.ID, Reason: 1}).Marshal(), snodePriv)
	n.onRemoteCancel(pkt, &proto.CancelBody{ID: o.ID, Reason: 1})

	if n.store.Get(idHex) != nil {
		t.Fatal("observed open order should have been removed from live store")
	}
	h := n.store.History()
	if len(h) != 1 || h[0].Status != "canceled" {
		t.Fatalf("history = %+v, want one 'canceled' entry", h)
	}
}

// TestRemoteCancelLocalOpenRebroadcast: a local open order cancelled by the
// counterparty (not by us) must NOT be cancelled — it is marked stale for
// rebroadcast instead.
func TestRemoteCancelLocalOpenRebroadcast(t *testing.T) {
	snodePriv := make([]byte, 32)
	snodePriv[0] = 0x22
	_, idHex := mustID(t)
	o := &Order{FromCurrency: "BTC", ToCurrency: "LTC", FromAmount: 1e6, ToAmount: 2e6,
		Status: "open", Mine: true, SNodePubkey: hexPub(t, snodePriv)}
	o.ID = decodeID(t, idHex)

	n := newCancelTestNode(nil)
	n.store.Add(o)

	pkt := signBodyPacket(t, proto.XbcTransactionCancel, (&proto.CancelBody{ID: o.ID, Reason: 1}).Marshal(), snodePriv)
	n.onRemoteCancel(pkt, &proto.CancelBody{ID: o.ID, Reason: 1})

	got := n.store.Get(idHex)
	if got == nil {
		t.Fatal("local open order must stay in the live store (rebroadcast branch)")
	}
	if got.Status != "open" {
		t.Fatalf("local open order status = %q, want 'open'", got.Status)
	}
	if got.Updated > NowMicro()-240_000_000 {
		t.Fatal("local open order should have been marked stale (~241s in the past)")
	}
}

// TestRemoteCancelCreatedDepositSent rolls a created order with a sent deposit
// back to "rolled back".
func TestRemoteCancelCreatedDepositSent(t *testing.T) {
	snodePriv := make([]byte, 32)
	snodePriv[0] = 0x33
	_, idHex := mustID(t)
	o := &Order{FromCurrency: "BTC", ToCurrency: "LTC", FromAmount: 1e6, ToAmount: 2e6,
		Status: "created", DepositSent: true, RefundTx: "refundhex", SNodePubkey: hexPub(t, snodePriv)}
	o.ID = decodeID(t, idHex)

	n := newCancelTestNode(nil)
	n.store.Add(o)

	pkt := signBodyPacket(t, proto.XbcTransactionCancel, (&proto.CancelBody{ID: o.ID, Reason: 2}).Marshal(), snodePriv)
	n.onRemoteCancel(pkt, &proto.CancelBody{ID: o.ID, Reason: 2})

	got := n.store.Get(idHex)
	if got == nil {
		t.Fatal("created order must remain in the live store (rollback)")
	}
	if got.Status != "rolled back" {
		t.Fatalf("status = %q, want 'rolled back'", got.Status)
	}
	if got.Reason != 2 {
		t.Fatalf("reason = %d, want 2", got.Reason)
	}
}

// TestRemoteCancelAlreadyCanceled leaves an already-cancelled order unchanged.
func TestRemoteCancelAlreadyCanceled(t *testing.T) {
	snodePriv := make([]byte, 32)
	snodePriv[0] = 0x44
	_, idHex := mustID(t)
	o := &Order{FromCurrency: "BTC", ToCurrency: "LTC", FromAmount: 1e6, ToAmount: 2e6,
		Status: "canceled", SNodePubkey: hexPub(t, snodePriv)}
	o.ID = decodeID(t, idHex)

	n := newCancelTestNode(nil)
	n.store.Add(o)
	before := len(n.store.History())

	pkt := signBodyPacket(t, proto.XbcTransactionCancel, (&proto.CancelBody{ID: o.ID, Reason: 1}).Marshal(), snodePriv)
	n.onRemoteCancel(pkt, &proto.CancelBody{ID: o.ID, Reason: 1})

	if len(n.store.History()) != before {
		t.Fatal("already-cancelled order must not produce a new history entry")
	}
	if got := n.store.Get(idHex); got == nil || got.Status != "canceled" {
		t.Fatal("already-cancelled order must stay 'canceled'")
	}
}

// TestRemoteCancelCreatedNoDeposit cancels a created order whose deposit was
// never sent.
func TestRemoteCancelCreatedNoDeposit(t *testing.T) {
	snodePriv := make([]byte, 32)
	snodePriv[0] = 0x55
	_, idHex := mustID(t)
	o := &Order{FromCurrency: "BTC", ToCurrency: "LTC", FromAmount: 1e6, ToAmount: 2e6,
		Status: "created", DepositSent: false, SNodePubkey: hexPub(t, snodePriv)}
	o.ID = decodeID(t, idHex)

	n := newCancelTestNode(nil)
	n.store.Add(o)

	pkt := signBodyPacket(t, proto.XbcTransactionCancel, (&proto.CancelBody{ID: o.ID, Reason: 1}).Marshal(), snodePriv)
	n.onRemoteCancel(pkt, &proto.CancelBody{ID: o.ID, Reason: 1})

	got := n.store.Get(idHex)
	if got == nil || got.Status != "canceled" {
		t.Fatal("created+no-deposit order must become 'canceled' in the live store")
	}
}

// TestRemoteCancelCounterpartyRedeemed ignores the cancel.
func TestRemoteCancelCounterpartyRedeemed(t *testing.T) {
	snodePriv := make([]byte, 32)
	snodePriv[0] = 0x66
	_, idHex := mustID(t)
	o := &Order{FromCurrency: "BTC", ToCurrency: "LTC", FromAmount: 1e6, ToAmount: 2e6,
		Status: "created", DepositSent: true, CounterpartyRedeemed: true,
		SNodePubkey: hexPub(t, snodePriv)}
	o.ID = decodeID(t, idHex)

	n := newCancelTestNode(nil)
	n.store.Add(o)

	pkt := signBodyPacket(t, proto.XbcTransactionCancel, (&proto.CancelBody{ID: o.ID, Reason: 1}).Marshal(), snodePriv)
	n.onRemoteCancel(pkt, &proto.CancelBody{ID: o.ID, Reason: 1})

	if got := n.store.Get(idHex); got == nil || got.Status != "created" {
		t.Fatal("counterparty-redeemed order must ignore cancel (stay 'created')")
	}
}

// TestRemoteCancelCreatedNoRefund cancels when no refund tx is available.
func TestRemoteCancelCreatedNoRefund(t *testing.T) {
	snodePriv := make([]byte, 32)
	snodePriv[0] = 0x77
	_, idHex := mustID(t)
	o := &Order{FromCurrency: "BTC", ToCurrency: "LTC", FromAmount: 1e6, ToAmount: 2e6,
		Status: "created", DepositSent: true, RefundTx: "", SNodePubkey: hexPub(t, snodePriv)}
	o.ID = decodeID(t, idHex)

	n := newCancelTestNode(nil)
	n.store.Add(o)

	pkt := signBodyPacket(t, proto.XbcTransactionCancel, (&proto.CancelBody{ID: o.ID, Reason: 1}).Marshal(), snodePriv)
	n.onRemoteCancel(pkt, &proto.CancelBody{ID: o.ID, Reason: 1})

	if got := n.store.Get(idHex); got == nil || got.Status != "canceled" {
		t.Fatal("created+no-refund order must become 'canceled'")
	}
}

// TestRemoteCancelExchangeBranch verifies the Exchange::instance().isStarted()
// branch: a cancel signed by one of the session's member keys triggers
// sendCancelTransaction (a broadcast xbcTransactionCancel).
func TestRemoteCancelExchangeBranch(t *testing.T) {
	coins.InitFromConf(map[string]*config.CoinConf{
		"BTC": {Ticker: "BTC", CreateTxMethod: "BTC", AddressPrefix: 0, ScriptPrefix: 5, Coin: 100000000},
	})
	mPriv := make([]byte, 32)
	mPriv[0] = 0x88
	mPub, err := crypto.CompressedPubKey(mPriv)
	if err != nil {
		t.Fatal(err)
	}
	_, idHex := mustID(t)

	n := newCancelTestNode(&captureXConn{})
	n.SetExchangeStarted(true)
	o := &Order{FromCurrency: "BTC", ToCurrency: "LTC", FromAmount: 1e6, ToAmount: 2e6,
		Status: "open", SNodePubkey: "snodehex"}
	o.ID = decodeID(t, idHex)
	n.store.Add(o)
	n.newMakerSession(o, MakeOrderParams{MakerAddress: btcAddr, TakerAddress: btcAddr}, arr32(mPriv), mPub)

	pkt := signBodyPacket(t, proto.XbcTransactionCancel, (&proto.CancelBody{ID: o.ID, Reason: 3}).Marshal(), mPriv)
	n.onRemoteCancel(pkt, &proto.CancelBody{ID: o.ID, Reason: 3})

	pkts := n.conn.(*captureXConn).snapshot()
	if len(pkts) != 1 {
		t.Fatalf("exchange branch wrote %d packets, want 1", len(pkts))
	}
	if pkts[0].Command != proto.XbcTransactionCancel {
		t.Fatalf("command = %d, want XbcTransactionCancel", pkts[0].Command)
	}
}

// TestRemoteRejectRestoresToPending: a role-'B' accepting order rejected with a
// valid snode signature is restored to "open", role cleared, never cancelled.
func TestRemoteRejectRestoresToPending(t *testing.T) {
	snodePriv := make([]byte, 32)
	snodePriv[0] = 0x99
	_, idHex := mustID(t)
	o := &Order{FromCurrency: "LTC", ToCurrency: "BTC", FromAmount: 2e6, ToAmount: 1e6,
		OrigFromCurrency: "BTC", OrigToCurrency: "LTC", OrigFromAmount: 1e6, OrigToAmount: 2e6,
		Status: "accepting", Role: 'B', MakerKey: "ourMkey", OtherPubkey: "theirOkey",
		SNodePubkey: hexPub(t, snodePriv)}
	o.ID = decodeID(t, idHex)

	n := newCancelTestNode(nil)
	n.store.Add(o)

	pkt := signBodyPacket(t, proto.XbcTransactionReject, (&proto.RejectBody{ID: o.ID, Reason: 4}).Marshal(), snodePriv)
	n.onRemoteReject(pkt, &proto.RejectBody{ID: o.ID, Reason: 4})

	got := n.store.Get(idHex)
	if got == nil {
		t.Fatal("rejected order must remain in the live store")
	}
	if got.Status != "open" {
		t.Fatalf("status = %q, want 'open'", got.Status)
	}
	if got.Role != 0 {
		t.Fatalf("role = %d, want 0", got.Role)
	}
	if got.MakerKey != "" || got.OtherPubkey != "" {
		t.Fatal("MakerKey/OtherPubkey must be cleared on reject")
	}
	if got.Reason != 0 {
		t.Fatalf("reason = %d, want 0", got.Reason)
	}
	if got.FromCurrency != "BTC" || got.ToCurrency != "LTC" {
		t.Fatalf("currencies not restored to orig: %s/%s", got.FromCurrency, got.ToCurrency)
	}
}

// TestRemoteRejectIgnoredForMaker: a role-'A' order ignores the reject.
func TestRemoteRejectIgnoredForMaker(t *testing.T) {
	snodePriv := make([]byte, 32)
	snodePriv[0] = 0xaa
	_, idHex := mustID(t)
	o := &Order{FromCurrency: "BTC", ToCurrency: "LTC", FromAmount: 1e6, ToAmount: 2e6,
		Status: "accepting", Role: 'A', SNodePubkey: hexPub(t, snodePriv)}
	o.ID = decodeID(t, idHex)

	n := newCancelTestNode(nil)
	n.store.Add(o)

	pkt := signBodyPacket(t, proto.XbcTransactionReject, (&proto.RejectBody{ID: o.ID, Reason: 1}).Marshal(), snodePriv)
	n.onRemoteReject(pkt, &proto.RejectBody{ID: o.ID, Reason: 1})

	got := n.store.Get(idHex)
	if got == nil || got.Status != "accepting" || got.Role != 'A' {
		t.Fatal("role-'A' order must ignore reject")
	}
}

// TestRemoteCancelBadSignature is a negative test: a cancel whose packet key
// matches neither SNodePubkey, OtherPubkey, nor MakerKey must not change state.
func TestRemoteCancelBadSignature(t *testing.T) {
	goodSnode := make([]byte, 32)
	goodSnode[0] = 0xbb
	// attacker signs with a different key
	bad := make([]byte, 32)
	bad[0] = 0xcc
	_, idHex := mustID(t)
	o := &Order{FromCurrency: "BTC", ToCurrency: "LTC", FromAmount: 1e6, ToAmount: 2e6,
		Status: "open", SNodePubkey: hexPub(t, goodSnode), MakerKey: "makerkey", OtherPubkey: "otherkey"}
	o.ID = decodeID(t, idHex)

	n := newCancelTestNode(nil)
	n.store.Add(o)
	before := len(n.store.History())

	pkt := signBodyPacket(t, proto.XbcTransactionCancel, (&proto.CancelBody{ID: o.ID, Reason: 1}).Marshal(), bad)
	n.onRemoteCancel(pkt, &proto.CancelBody{ID: o.ID, Reason: 1})

	if len(n.store.History()) != before {
		t.Fatal("bad-signature cancel must not produce a history entry")
	}
	got := n.store.Get(idHex)
	if got == nil || got.Status != "open" {
		t.Fatal("bad-signature cancel must not change status")
	}
}

// decodeID parses a 64-hex-char id into a [32]byte.
func decodeID(t *testing.T, idHex string) [32]byte {
	t.Helper()
	var id [32]byte
	if _, err := hex.Decode(id[:], []byte(idHex)); err != nil {
		t.Fatalf("decode id: %v", err)
	}
	return id
}

func mustPub(t *testing.T, priv []byte) [33]byte {
	t.Helper()
	pub, err := crypto.CompressedPubKey(priv)
	if err != nil {
		t.Fatalf("compressed pubkey: %v", err)
	}
	return pub
}

// hexPub returns the hex encoding of the compressed pubkey for priv.
func hexPub(t *testing.T, priv []byte) string {
	t.Helper()
	pub := mustPub(t, priv)
	return hexEncode(pub[:])
}
