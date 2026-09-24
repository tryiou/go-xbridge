package api

import (
	"crypto/sha256"
	"encoding/binary"
	"strings"
	"testing"

	"go-xbridge/p2p/servicenode"
	"go-xbridge/proto"
	"go-xbridge/version"
	"go-xbridge/wallet"
)

// runningHub returns a registry with one running hub advertising BTC+SYS (the
// protocol version match Pick requires, servicenode.go:459).
func runningHub(t *testing.T) (*servicenode.Registry, [33]byte) {
	t.Helper()
	_, hubPub, _, _ := hubKey(t, 0x51)
	reg := servicenode.NewRegistry()
	reg.AddPing(servicenode.ServiceNode{
		PubKey: hubPub, Tier: servicenode.TierSPV, Services: []string{"BTC", "SYS"}, XBridgeVersion: version.XBridgeProtocolVersion,
	})
	return reg, hubPub
}

// TestMakeOrderDeterministicID locks the make-order id to the deterministic
// function: with a fixed stub signature and a zero block hash (no BLOCK
// connector), o.ID must equal sha256dOrderID over the maker/taker identity,
// amounts, o.Created, and o.Utxos[0].Signature — never a rand.Read id.
func TestMakeOrderDeterministicID(t *testing.T) {
	n, cc := newHubNode(servicenode.NewRegistry())
	fromID, _ := decodeAddr("dxMakeOrder", "BTC", btcAddr)
	toID, _ := decodeAddr("dxMakeOrder", "SYS", btcAddr2)

	o, rerr := n.MakeOrder(MakeOrderParams{
		Maker: "BTC", MakerSize: "1.5", MakerAddress: btcAddr,
		Taker: "SYS", TakerSize: "0.3", TakerAddress: btcAddr2,
		DryRun: true,
	})
	if rerr != nil {
		t.Fatalf("MakeOrder(dry-run): %v", rerr)
	}
	wantID := sha256dOrderID(fromID, "BTC", 1500000, toID, "SYS", 300000,
		o.Created, o.BlockHash, o.Utxos[0].Signature[:])
	if o.ID != wantID {
		t.Fatalf("order id = %x, want deterministic %x", o.ID, wantID)
	}
	// Independent wiring check: rebuild the id preimage with stdlib only
	// (CompactSize varstr per the Bitcoin consensus encoding, little-endian
	// u64s, double-SHA256) from the order's OBSERVED fields. TestSha256dOrderID
	// already pins this layout against a python-hashlib golden, so a match
	// here proves MakeOrder hashes the fields it stores — the same-function
	// recompute above alone could not catch a wiring swap (e.g. hashing the
	// pre-take amounts while storing post-take ones).
	var pre []byte
	pre = appendSpecVarStr(pre, fromID[:])
	pre = appendSpecVarStr(pre, []byte("BTC"))
	pre = binary.LittleEndian.AppendUint64(pre, 1500000)
	pre = appendSpecVarStr(pre, toID[:])
	pre = appendSpecVarStr(pre, []byte("SYS"))
	pre = binary.LittleEndian.AppendUint64(pre, 300000)
	pre = binary.LittleEndian.AppendUint64(pre, o.Created)
	pre = append(pre, o.BlockHash[:]...)
	pre = appendSpecVarStr(pre, o.Utxos[0].Signature[:])
	h1 := sha256.Sum256(pre)
	if want := sha256.Sum256(h1[:]); o.ID != want {
		t.Fatalf("order id = %x, want spec-preimage hash %x", o.ID, want)
	}
	if o.Status != "open" {
		t.Fatalf("status = %q, want open (trPending)", o.Status)
	}
	if len(o.Utxos) != 1 || o.Utxos[0].RawAddress != fromID {
		t.Fatalf("utxos = %+v, want the single funded utxo at the maker address", o.Utxos)
	}
	if len(cc.snapshot()) != 0 {
		t.Fatalf("dry-run wrote %d packets, want 0", len(cc.snapshot()))
	}
	if n.store.Get(hexEncode(o.ID[:])) != nil {
		t.Fatal("dry-run must not add the order to the store")
	}
}

// appendSpecVarStr appends a CompactSize-prefixed byte string per the Bitcoin
// consensus encoding (independent spec, not via p2p.MarshalVarStr): single
// byte for len < 0xfd, 0xfd+u16le below 0x10000, else 0xfe+u32le. The layout
// it produces is cross-checked by TestSha256dOrderID's external golden.
func appendSpecVarStr(b, s []byte) []byte {
	n := len(s)
	switch {
	case n < 0xfd:
		b = append(b, byte(n))
	case n < 0x10000:
		b = append(b, 0xfd, byte(n), byte(n>>8))
	default:
		b = append(b, 0xfe, byte(n), byte(n>>8), byte(n>>16), byte(n>>24))
	}
	return append(b, s...)
}

// TestMakeOrderAutoSplitPrepTx drives a non-dry autoSplit partial make (2.5 BTC
// from a single 3.0 BTC utxo, min 1.0). The prep tx is built, signed, hashed
// (real txid, not the wallet's return) and broadcast, then the utxo set is
// rebuilt from the prep outputs and the id re-hashed. The order stays pending
// ("open", PrepTx set): no SEND, but the maker session IS registered — C++
// generates the descriptor key before the broadcast gate (:1997), so the
// pending order carries its signing key from creation and stays cancelable.
func TestMakeOrderAutoSplitPrepTx(t *testing.T) {
	reg, _ := runningHub(t)
	n, cc := newHubNode(reg)
	fromID, _ := decodeAddr("dxMakeOrder", "BTC", btcAddr)
	toID, _ := decodeAddr("dxMakeOrder", "SYS", btcAddr2)

	o, rerr := n.MakeOrder(MakeOrderParams{
		Type: "partial", AutoSplit: true,
		Maker: "BTC", MakerSize: "2.5", MinSize: "1.0", MakerAddress: btcAddr,
		Taker: "SYS", TakerSize: "0.3", TakerAddress: btcAddr2,
	})
	if rerr != nil {
		t.Fatalf("MakeOrder(autoSplit): %v", rerr)
	}
	if o.Status != "open" {
		t.Fatalf("status = %q, want open (pending)", o.Status)
	}
	if o.PrepTx == "" || len(o.PrepTx) != 64 {
		t.Fatalf("prep txid = %q, want a 64-hex real tx hash", o.PrepTx)
	}
	if o.PrepTx == "txid123" {
		t.Fatal("prep txid must be the real tx hash, not the wallet's SendRawTransaction return")
	}
	// The prep outputs' utxos carry the real prep txid (C++ orderPrepTx :1844),
	// in internal byte order on the wire.
	if got, err := reverseTxidHex(o.PrepTx); err != nil {
		t.Fatalf("prep txid not hex: %v", err)
	} else if got != o.Utxos[0].TxID {
		t.Fatalf("prep txid %x != Utxos[0].TxID %x", got, o.Utxos[0].TxID)
	}
	// Rebuilt used set: 2 splits + the remainder vout (change stays behind).
	if len(o.Utxos) != 3 {
		t.Fatalf("len(Utxos) = %d, want 3", len(o.Utxos))
	}
	for i, want := range []uint32{0, 1, 2} {
		if o.Utxos[i].Vout != want {
			t.Fatalf("Utxos[%d].Vout = %d, want %d", i, o.Utxos[i].Vout, want)
		}
	}
	if o.Utxos[0].RawAddress != fromID {
		t.Fatalf("prep output address = %x, want maker %x", o.Utxos[0].RawAddress, fromID)
	}
	// The pending id is re-hashed over the FINAL proof signature (:1936-1946).
	wantID := sha256dOrderID(fromID, "BTC", 2500000, toID, "SYS", 300000,
		o.Created, o.BlockHash, o.Utxos[0].Signature[:])
	if o.ID != wantID {
		t.Fatalf("order id = %x, want re-hashed %x", o.ID, wantID)
	}
	if len(cc.snapshot()) != 0 {
		t.Fatalf("pending autoSplit order wrote %d packets, want 0 (no SEND)", len(cc.snapshot()))
	}
	if s := n.sessions[hexEncode(o.ID[:])]; s == nil {
		t.Fatal("pending order must register a maker session (cancel signs with its key)")
	} else if hexEncode(s.pubKey[:]) != o.MakerKey {
		t.Fatal("pending session key must match the order MakerKey")
	}
	if n.store.Get(hexEncode(o.ID[:])) == nil {
		t.Fatal("pending order must be stored locally")
	}
	// Sanity anchor: every rebuilt proof uses the same stub signature, so the
	// id-rehash input is fully determined by the selection.
	if o.Utxos[0].Signature != o.Utxos[1].Signature {
		t.Fatal("rebuilt proofs should share the stub signature")
	}
}

// TestMakeOrderExactMatchPartial drives a non-dry partial make whose selection
// exactly matches the ideal utxo set (two 1.000009 utxos for 2.0/1.0). No prep
// tx is needed: the order lists immediately as "created" with a single SEND
// carrying both ideal utxo proofs (C++ partialExactUtxoMatch).
func TestMakeOrderExactMatchPartial(t *testing.T) {
	reg, _ := runningHub(t)
	utxos := []wallet.Utxo{
		{TxID: strings.Repeat("aa", 32), Vout: 0, Amount: 100000900, Value: 1.000009, ScriptPubKey: "76a914000000000000000000000000000000000000000088ac", Address: btcAddr},
		{TxID: strings.Repeat("bb", 32), Vout: 1, Amount: 100000900, Value: 1.000009, ScriptPubKey: "76a914000000000000000000000000000000000000000088ac", Address: btcAddr},
	}
	n, cc := newHubNodeUtxos(reg, utxos)
	fromID, _ := decodeAddr("dxMakeOrder", "BTC", btcAddr)
	toID, _ := decodeAddr("dxMakeOrder", "SYS", btcAddr2)

	o, rerr := n.MakeOrder(MakeOrderParams{
		Type: "partial", AutoSplit: true,
		Maker: "BTC", MakerSize: "2.0", MinSize: "1.0", MakerAddress: btcAddr,
		Taker: "SYS", TakerSize: "0.3", TakerAddress: btcAddr2,
	})
	if rerr != nil {
		t.Fatalf("MakeOrder(exact partial): %v", rerr)
	}
	if o.Status != "open" {
		t.Fatalf("status = %q, want open (exact match, trPending)", o.Status)
	}
	if o.PrepTx != "" {
		t.Fatalf("prep txid = %q, want empty for an exact match", o.PrepTx)
	}
	pkts := cc.snapshot()
	if len(pkts) != 1 {
		t.Fatalf("wrote %d packets, want 1 SEND", len(pkts))
	}
	if pkts[0].Command != proto.XbcTransaction {
		t.Fatalf("command = %d, want XbcTransaction", pkts[0].Command)
	}
	dec, err := proto.DecodeBody(proto.XbcTransaction, pkts[0].Body)
	if err != nil {
		t.Fatalf("decode SEND body: %v", err)
	}
	body, ok := dec.(*proto.OrderBody)
	if !ok {
		t.Fatalf("body = %T, want *proto.OrderBody", dec)
	}
	if len(body.Utxos) != 2 {
		t.Fatalf("SEND carries %d utxos, want the 2 ideal utxos", len(body.Utxos))
	}
	wantTx, _ := reverseTxidHex(strings.Repeat("aa", 32))
	if body.Utxos[0].TxID != wantTx || body.Utxos[0].Vout != 0 {
		t.Fatalf("SEND Utxos[0] = %+v, want the first ideal utxo", body.Utxos[0])
	}
	if body.Utxos[1].Vout != 1 {
		t.Fatalf("SEND Utxos[1].Vout = %d, want 1", body.Utxos[1].Vout)
	}
	wantID := sha256dOrderID(fromID, "BTC", 2000000, toID, "SYS", 300000,
		o.Created, o.BlockHash, o.Utxos[0].Signature[:])
	if o.ID != wantID {
		t.Fatalf("order id = %x, want %x", o.ID, wantID)
	}
	if s := n.sessions[hexEncode(o.ID[:])]; s == nil {
		t.Fatal("exact-match order must start a maker session")
	}
}

// TestMakeOrderInsufficientFunds verifies selection is now mandatory: an
// unfunded maker wallet fails the order with errInsufficientFunds (1019), even
// with an eligible hub (C++ :1645/:1669).
func TestMakeOrderInsufficientFunds(t *testing.T) {
	reg, _ := runningHub(t)
	n, cc := newHubNodeUtxos(reg, nil)
	_, rerr := n.MakeOrder(MakeOrderParams{
		Maker: "BTC", MakerSize: "1.5", MakerAddress: btcAddr,
		Taker: "SYS", TakerSize: "0.3", TakerAddress: btcAddr2,
	})
	if rerr == nil || rerr.Code != errInsufficientFunds {
		t.Fatalf("MakeOrder(unfunded) = %v, want errInsufficientFunds", rerr)
	}
	if len(cc.snapshot()) != 0 {
		t.Fatalf("unfunded order wrote %d packets, want 0", len(cc.snapshot()))
	}
}
