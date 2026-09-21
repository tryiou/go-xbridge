package api

// Rebroadcast triage: failed-hub rotation with anchor fallback, and
// spent-funding cancellation (C++ xbridgeapp checkAndRelayPendingOrders and
// orderUtxosAreStillValid). In-memory fixtures — no live hub, no network.
import (
	"testing"
	"time"

	"go-xbridge/coins"
	"go-xbridge/p2p/servicenode"
	"go-xbridge/proto"
	"go-xbridge/wallet"
)

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
	funding := wallet.Utxo{TxID: testTxID("aa"), Vout: 0, Amount: 5e8}
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

// TestRebroadcastCancelsSpentFunding pins C++ orderUtxosAreStillValid
// (xbridgeapp.cpp:3557-3569): a rebroadcast-due order whose funding is spent
// cancels with crBadAUtxo instead of re-posting a dead order.
func TestRebroadcastCancelsSpentFunding(t *testing.T) {
	alignInitCoins(t)

	btc := &fakeConnector{ticker: "BTC", funding: wallet.Utxo{TxID: testTxID("aa"), Amount: 5e8},
		changeAddr: addrFor(0, "c"), blockHeight: 1000, rawTx: map[string]string{}}
	n := newTestNode(t, alignConfs(), map[string]wallet.Connector{"BTC": btc})
	cc := &captureXConn{}
	n.conn = cc

	var id [32]byte
	oid := hash20("align-rebroadcast-spent")
	copy(id[:], oid[:])
	mPriv, mPub := newKey(t)
	spent := wallet.Utxo{TxID: testTxID("ff"), Vout: 0, Amount: 5e8} // unknown to the wallet: spent
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
