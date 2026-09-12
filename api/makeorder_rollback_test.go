package api

import (
	"errors"
	"testing"

	"go-xbridge/p2p/servicenode"
	"go-xbridge/proto"
)

// failWriteConn wraps a captureXConn but makes every outbound SEND fail, so the
// make-order rollback-on-send-failure path can be exercised.
type failWriteConn struct {
	*captureXConn
	called bool
}

func (c *failWriteConn) WritePacket(p *proto.Packet, dest [20]byte) error {
	c.called = true
	return errors.New("injected send failure")
}

// TestMakeOrderStoreAddSendFailureRollback proves the make-order path rolls back
// the store entry + session (and the durable copy) when the outbound SEND
// fails, so no locally-live / hub-unknown orphan remains — which would let a
// counterparty take an order the maker's node never durably recorded (fund
// loss). It must also not leave the orphan on disk after a restart.
func TestMakeOrderStoreAddSendFailureRollback(t *testing.T) {
	reg := servicenode.NewRegistry()
	_, hubPub, _, _ := hubKey(t, 0x53)
	reg.AddPing(servicenode.ServiceNode{
		PubKey: hubPub, Tier: servicenode.TierSPV, Services: []string{"BTC", "SYS"}, XBridgeVersion: proto.ProtocolVersion,
	})
	n, cc := newHubNode(reg)
	n.config.DataDir = t.TempDir()
	// Force the outbound SEND to fail.
	fwc := &failWriteConn{captureXConn: cc}
	n.conn = fwc

	o, rerr := n.MakeOrder(MakeOrderParams{
		Maker: "BTC", MakerSize: "1.5", MakerAddress: btcAddr,
		Taker: "SYS", TakerSize: "0.3", TakerAddress: btcAddr2,
	})
	if rerr == nil {
		t.Fatal("MakeOrder with failing send should error")
	}
	// errUnknown (1002) is the make-order send-failure code; its fixed text is
	// "Internal Server Error" (the msg arg is ignored by xbridgeErrorText).
	if rerr.Code != errUnknown {
		t.Fatalf("MakeOrder error code = %d, want errUnknown (%d)", rerr.Code, errUnknown)
	}
	if !fwc.called {
		t.Fatal("WritePacket was never called — the rollback path was not reached")
	}
	if o != nil {
		t.Fatalf("expected nil order on failed send, got %+v", o)
	}
	if got := n.store.List(); len(got) != 0 {
		t.Fatalf("store has %d orders after failed send, want 0 (orphan leaked in memory)", len(got))
	}
	if got := n.store.History(); len(got) != 0 {
		t.Fatalf("store history has %d after failed send, want 0", len(got))
	}
	if len(n.sessions) != 0 {
		t.Fatalf("sessions map has %d entries after failed send, want 0 (orphan session leaked)", len(n.sessions))
	}

	// The durable copy must also lack the order: a restart must not resurrect an
	// orphan the hub never received.
	ps, _, err := loadSwaps(swapStatePath(n.config.DataDir))
	if err != nil {
		t.Fatalf("loadSwaps: %v", err)
	}
	if len(ps) != 0 {
		t.Fatalf("persisted %d swaps after failed send, want 0 (orphan leaked on disk)", len(ps))
	}
}
