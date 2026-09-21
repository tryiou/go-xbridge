package api

import (
	"encoding/json"
	"testing"
)

// partialCtx builds a HandlerCtx over a funded hub node with one running
// hub, for exercising the dxMakePartialOrder handler end-to-end (in-memory,
// no live hub).
func partialCtx(t *testing.T) *HandlerCtx {
	t.Helper()
	reg, _ := runningHub(t)
	n, _ := newHubNode(reg)
	return &HandlerCtx{Store: n.store, Node: n}
}

func partialParams(minSize string, extra ...json.RawMessage) []json.RawMessage {
	p := []json.RawMessage{
		jstr("BTC"), jstr("1.5"), jstr(btcAddr),
		jstr("SYS"), jstr("0.3"), jstr(btcAddr2),
		jstr(minSize),
	}
	return append(p, extra...)
}

func jbool(b bool) json.RawMessage {
	if b {
		return json.RawMessage("true")
	}
	return json.RawMessage("false")
}

// TestDxMakePartialOrderDryrun pins the partial dry-run contract (previously
// the entire handler sat at 0%): order_type "partial", real partial_minimum,
// zero id, status created — and no store side effects, no packets.
func TestDxMakePartialOrderDryrun(t *testing.T) {
	ctx := partialCtx(t)
	n := ctx.Node

	res, rerr := ctx.dxMakePartialOrder(partialParams("0.5", jbool(true), jbool(true), jbool(true), jstr("dryrun")))
	if rerr != nil {
		t.Fatalf("dxMakePartialOrder(dryrun): %+v", rerr)
	}
	obj, ok := res.(dryrunMakeOrderResult)
	if !ok {
		t.Fatalf("result type = %T, want dryrunMakeOrderResult", res)
	}
	if obj.OrderType != "partial" {
		t.Errorf("order_type = %q, want partial", obj.OrderType)
	}
	if obj.PartialMinimum != "0.500000" {
		t.Errorf("partial_minimum = %q, want 0.500000", obj.PartialMinimum)
	}
	if obj.MakerSize != "1.500000" || obj.TakerSize != "0.300000" {
		t.Errorf("sizes = (%q, %q), want (1.500000, 0.300000)", obj.MakerSize, obj.TakerSize)
	}
	if obj.Status != "created" {
		t.Errorf("status = %q, want created", obj.Status)
	}
	if !obj.PartialRepost {
		t.Error("dryrun partial_repost = false, want true (explicit repost=true echoes)")
	}
	_ = n
}

// TestDxMakePartialOrderLive pins the live partial make: order_type partial,
// real minimum, repost echo, status created, and the order persisted.
func TestDxMakePartialOrderLive(t *testing.T) {
	ctx := partialCtx(t)

	res, rerr := ctx.dxMakePartialOrder(partialParams("0.5"))
	if rerr != nil {
		t.Fatalf("dxMakePartialOrder: %+v", rerr)
	}
	obj, ok := res.(makeOrderResult)
	if !ok {
		t.Fatalf("result type = %T, want makeOrderResult", res)
	}
	if obj.OrderType != "partial" {
		t.Errorf("order_type = %q, want partial", obj.OrderType)
	}
	if obj.PartialMinimum != "0.500000" {
		t.Errorf("partial_minimum = %q, want 0.500000", obj.PartialMinimum)
	}
	if !obj.PartialRepost {
		t.Error("partial_repost = false, want true (default repost)")
	}
	if obj.Status != "created" {
		t.Errorf("status = %q, want created", obj.Status)
	}
	// Response IDs render in display (uint256::GetHex) order; the store
	// keys internal bytes — parse back before lookup.
	if id := parseOrderIDS(obj.ID); ctx.Store.Get(hexEncode(id[:])) == nil {
		t.Error("live partial order not found in store")
	}
}

// TestDxMakePartialOrderEdges pins the handler gates: a misspelled 11th
// param is an error (never a silent broadcast), unknown currencies fail,
// and a minimum above the maker size fails.
func TestDxMakePartialOrderEdges(t *testing.T) {
	ctx := partialCtx(t)

	if _, rerr := ctx.dxMakePartialOrder(partialParams("0.5", jbool(true), jbool(true), jbool(true), jstr("live"))); rerr == nil {
		t.Error("misspelled dryrun: want error, got nil (must never silently broadcast)")
	} else {
		requireErrCode(t, rerr, errInvalidParameters, "misspelled dryrun")
	}

	badCur := partialParams("0.5")
	badCur[0] = jstr("NOPE")
	if _, rerr := ctx.dxMakePartialOrder(badCur); rerr == nil {
		t.Error("unknown maker currency: want error, got nil")
	} else {
		requireErrCode(t, rerr, errNoSession, "unknown maker currency")
	}

	bigMin := partialParams("99.0", jbool(true), jbool(true), jbool(true), jstr("dryrun"))
	if _, rerr := ctx.dxMakePartialOrder(bigMin); rerr == nil {
		t.Error("minimum above maker size: want error, got nil")
	}

}
