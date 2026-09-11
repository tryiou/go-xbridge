package api

import (
	"testing"

	"go-xbridge/proto"
)

// TestCancelPendingOrder proves a pending autoSplit order (open, PrepTx set,
// never broadcast) is cancelable: C++ signs cancels from the descriptor key
// generated before the broadcast gate (xbridgeapp.cpp:1997), so the pending
// order carries its signing key from creation via its maker session.
func TestCancelPendingOrder(t *testing.T) {
	reg, _ := runningHub(t)
	n, cc := newHubNode(reg)

	o, rerr := n.MakeOrder(MakeOrderParams{
		Type: "partial", AutoSplit: true,
		Maker: "BTC", MakerSize: "2.5", MinSize: "1.0", MakerAddress: btcAddr,
		Taker: "SYS", TakerSize: "0.3", TakerAddress: btcAddr2,
	})
	if rerr != nil {
		t.Fatalf("MakeOrder(autoSplit): %v", rerr)
	}
	idHex := hexEncode(o.ID[:])

	res, rerr := n.CancelOrder(CancelOrderParams{ID: idHex})
	if rerr != nil {
		t.Fatalf("CancelOrder(pending): %+v", rerr)
	}
	if res.Status != "canceled" {
		t.Fatalf("status = %q, want canceled", res.Status)
	}

	pkts := cc.snapshot()
	if len(pkts) != 1 {
		t.Fatalf("broadcast packets = %d, want exactly one cancel", len(pkts))
	}
	if pkts[0].Command != proto.XbcTransactionCancel {
		t.Fatalf("command = %v, want XbcTransactionCancel", pkts[0].Command)
	}
	var cancel proto.CancelBody
	if err := cancel.Unmarshal(pkts[0].Body); err != nil {
		t.Fatalf("cancel body: %v", err)
	}
	if cancel.Reason != uint32(crRpcRequest) {
		t.Fatalf("cancel reason = %d, want %d (crRpcRequest)", cancel.Reason, crRpcRequest)
	}
	if got := hexEncode(pkts[0].Pubkey[:]); got != o.MakerKey {
		t.Fatal("cancel packet must carry the order M pubkey in its header")
	}
	if ok, _ := n.signer.Verify(pkts[0]); !ok {
		t.Fatal("cancel packet must verify against the order M key")
	}
}

// TestCancelPendingOrderAfterRestart proves the pre-deposit cancel window
// survives a restart: the pending session (with its key) round-trips the
// swap file, so a pending order made before a crash stays cancelable after.
func TestCancelPendingOrderAfterRestart(t *testing.T) {
	reg, _ := runningHub(t)
	n, _ := newHubNode(reg)
	dir := t.TempDir()
	n.config.DataDir = dir

	o, rerr := n.MakeOrder(MakeOrderParams{
		Type: "partial", AutoSplit: true,
		Maker: "BTC", MakerSize: "2.5", MinSize: "1.0", MakerAddress: btcAddr,
		Taker: "SYS", TakerSize: "0.3", TakerAddress: btcAddr2,
	})
	if rerr != nil {
		t.Fatalf("MakeOrder(autoSplit): %v", rerr)
	}
	idHex := hexEncode(o.ID[:])
	n.persist()

	n2, cc2 := newHubNode(reg)
	n2.config.DataDir = dir
	n2.restoreLocalSwaps(dir)

	if n2.sessions[idHex] == nil {
		t.Fatal("pending session must survive the restart")
	}
	res, rerr := n2.CancelOrder(CancelOrderParams{ID: idHex})
	if rerr != nil {
		t.Fatalf("CancelOrder(pending, post-restart): %+v", rerr)
	}
	if res.Status != "canceled" {
		t.Fatalf("status = %q, want canceled", res.Status)
	}
	pkts := cc2.snapshot()
	if len(pkts) != 1 || pkts[0].Command != proto.XbcTransactionCancel {
		t.Fatalf("want exactly one cancel packet, got %d", len(pkts))
	}
	if ok, _ := n2.signer.Verify(pkts[0]); !ok {
		t.Fatal("post-restart cancel packet must verify against the order M key")
	}
}
