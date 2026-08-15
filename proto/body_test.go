package proto

import (
	"bytes"
	"encoding/hex"
	"testing"
)

// roundTrip encodes b, decodes it back, re-encodes, and asserts the bytes are
// identical. This catches field ordering / length bugs even when we don't have
// ground-truth values for every field.
func roundTrip(t *testing.T, name string, marshal func() []byte, unmarshal func([]byte) error) {
	t.Helper()
	orig := marshal()
	decoded := make([]byte, len(orig))
	copy(decoded, orig)
	if err := unmarshal(decoded); err != nil {
		t.Fatalf("%s: unmarshal: %v", name, err)
	}
	again := marshal()
	if len(again) != len(orig) {
		t.Fatalf("%s: length changed after round-trip: %d -> %d", name, len(orig), len(again))
	}
	for i := range orig {
		if orig[i] != again[i] {
			t.Fatalf("%s: byte %d differs after round-trip: %02x vs %02x", name, i, orig[i], again[i])
		}
	}
}

func sampleUtxo() UtxoEntry {
	var u UtxoEntry
	copy(u.TxID[:], []byte("0123456789abcdef0123456789abcdef"))
	u.Vout = 7
	copy(u.RawAddress[:], []byte("addr20bytesXXXXXX"))
	copy(u.Signature[:], []byte("sig65bytes............................................................"))
	return u
}

func TestOrderBodyRoundTrip(t *testing.T) {
	b := &OrderBody{
		FromCurrency:   "BTC",
		FromAmount:     123456789,
		ToCurrency:     "LTC",
		ToAmount:       987654321,
		Created:        1700000000,
		PartialAllowed: true,
		MinFromAmount:  1000,
		Utxos:          []UtxoEntry{sampleUtxo()},
	}
	roundTrip(t, "OrderBody",
		func() []byte { return b.Marshal() },
		func(d []byte) error { return b.Unmarshal(d) },
	)

	// No utxos, partial disabled.
	b2 := &OrderBody{ID: [32]byte{1}, FromCurrency: "DOGE", FromAmount: 5, ToCurrency: "BTC", ToAmount: 6}
	roundTrip(t, "OrderBody(no-utxos)",
		func() []byte { return b2.Marshal() },
		func(d []byte) error { return b2.Unmarshal(d) },
	)
}

// TestUint256Verbatim is a C++-derived KAT confirming that 32-byte uint256
// fields (ID, BlockHash) are carried byte-for-byte in Bitcoin internal
// little-endian order with NO reversal. C++ appends them verbatim via
// blockHash.begin() for 32 bytes (xbridgeapp.cpp:2082), and Go's body_types.go
// does the same — so a value seeded with non-symmetric bytes must survive a
// round-trip unchanged.
func TestUint256Verbatim(t *testing.T) {
	want := [32]byte{}
	for i := range want {
		want[i] = byte(i) // 00 01 02 ... 1f — ordering would be obvious if reversed
	}
	b := &PendingTransactionBody{
		ID:             want,
		FromCurrency:   "BTC",
		FromAmount:     100,
		ToCurrency:     "DGB",
		ToAmount:       200,
		HubAddress:     [20]byte{3, 4, 5},
		Created:        42,
		PartialAllowed: true,
	}

	raw := b.Marshal()
	dec := &PendingTransactionBody{}
	if err := dec.Unmarshal(raw); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if dec.ID != want {
		t.Fatalf("ID not carried verbatim: got %x want %x", dec.ID, want)
	}
	// Also confirm the raw bytes appear in order on the wire (no byte-swap).
	if !bytes.Contains(raw, want[:]) {
		t.Fatal("uint256 bytes not present in wire order")
	}
}

func TestPendingTransactionBodyRoundTrip(t *testing.T) {
	// With optional trailing minFromAmount.
	b := &PendingTransactionBody{
		ID:             [32]byte{9},
		FromCurrency:   "BTC",
		FromAmount:     100,
		ToCurrency:     "DGB",
		ToAmount:       200,
		HubAddress:     [20]byte{3, 4, 5},
		Created:        42,
		PartialAllowed: true,
		MinFromAmount:  50,
	}
	roundTrip(t, "PendingTransactionBody(with-min)",
		func() []byte { return b.Marshal() },
		func(d []byte) error { return b.Unmarshal(d) },
	)

	// Without trailing minFromAmount (broadcast-writer form).
	b2 := &PendingTransactionBody{
		FromCurrency: "LTC", FromAmount: 1, ToCurrency: "DOGE", ToAmount: 2,
		PartialAllowed: false,
	}
	roundTrip(t, "PendingTransactionBody(no-min)",
		func() []byte { return b2.Marshal() },
		func(d []byte) error { return b2.Unmarshal(d) },
	)
}

func TestAcceptingBodyRoundTrip(t *testing.T) {
	b := &AcceptingBody{
		HubAddress:       [20]byte{1},
		ID:               [32]byte{2},
		ServiceNodeFeeTx: []byte("aabbcc"),
		From:             [20]byte{3},
		FromCurrency:     "BTC",
		FromAmount:       111,
		FromBlockHeight:  700000,
		FromBlockHash:    [8]byte{4, 5, 6, 7, 8, 9, 10, 11},
		To:               [20]byte{12},
		ToCurrency:       "LTC",
		ToAmount:         222,
		ToBlockHeight:    700001,
		ToBlockHash:      [8]byte{13, 14, 15, 16, 17, 18, 19, 20},
		Utxos:            []UtxoEntry{sampleUtxo()},
	}
	roundTrip(t, "AcceptingBody",
		func() []byte { return b.Marshal() },
		func(d []byte) error { return b.Unmarshal(d) },
	)
}

// TestAcceptingBodyGolden is a byte-exact C++-writer KAT for the
// xbcTransactionAccepting layout (sendAcceptingTransaction,
// xbridgeapp.cpp:2406-2469): hub(20) ‖ id(32) ‖ u32(feeLen) ‖ feeBytes ‖
// from(20) ‖ fc(varstr) ‖ fromAmount(u64LE) ‖ fromHeight(u32LE) ‖ fromHash(8) ‖
// to(20) ‖ tc(varstr) ‖ toAmount(u64LE) ‖ toHeight(u32LE) ‖ toHash(8) ‖
// varint(utxoCount) ‖ entries. The body uses no funding entries and a 3-byte
// fee to keep the golden readable; every fixed field is seeded with the same
// distinct bytes as TestAcceptingBodyRoundTrip.
func TestAcceptingBodyGolden(t *testing.T) {
	b := &AcceptingBody{
		HubAddress:       [20]byte{1},
		ID:               [32]byte{2},
		ServiceNodeFeeTx: []byte{0xaa, 0xbb, 0xcc},
		From:             [20]byte{3},
		FromCurrency:     "LTC",
		FromAmount:       111,
		FromBlockHeight:  700000,
		FromBlockHash:    [8]byte{4, 5, 6, 7, 8, 9, 10, 11},
		To:               [20]byte{12},
		ToCurrency:       "DOGE",
		ToAmount:         222,
		ToBlockHeight:    700001,
		ToBlockHash:      [8]byte{13, 14, 15, 16, 17, 18, 19, 20},
	}
	want := "0100000000000000000000000000000000000000" + // hub
		"0200000000000000000000000000000000000000000000000000000000000000" + // id
		"03000000" + // feeLen
		"aabbcc" + // fee bytes
		"0300000000000000000000000000000000000000" + // from
		"4c54430000000000" + // Currency("LTC"): 8-byte ASCII, null-padded
		"6f00000000000000" + // 111
		"60ae0a00" + // 700000 = 0x000AAE60
		"0405060708090a0b" + // fromHash
		"0c00000000000000000000000000000000000000" + // to (20 bytes)
		"444f474500000000" + // Currency("DOGE"): 8-byte ASCII, null-padded
		"de00000000000000" + // 222
		"61ae0a00" + // 700001
		"0d0e0f1011121314" + // toHash
		"00000000" // utxoCount (uint32) = 0
	got := b.Marshal()
	if hex.EncodeToString(got) != want {
		t.Fatalf("AcceptingBody.Marshal:\n got  %x\n want %s", got, want)
	}
}

// TestAcceptingBodySizeFloor locks the hub's drop gate: an Accepting packet is
// only accepted when the body is >= 188 bytes (xbridgesession.cpp:855). A real
// take — non-trivial fee tx plus at least one utxo entry (121 bytes each) — must
// clear it; the empty-fee/empty-utxos body did not.
func TestAcceptingBodySizeFloor(t *testing.T) {
	b := &AcceptingBody{
		HubAddress:       [20]byte{1},
		ID:               [32]byte{2},
		ServiceNodeFeeTx: bytes.Repeat([]byte{0xab}, 200), // ~real fee tx length
		From:             [20]byte{3},
		FromCurrency:     "BTC",
		FromAmount:       300000,
		FromBlockHeight:  700000,
		FromBlockHash:    [8]byte{4, 5, 6, 7, 8, 9, 10, 11},
		To:               [20]byte{12},
		ToCurrency:       "LTC",
		ToAmount:         1500000,
		ToBlockHeight:    700001,
		ToBlockHash:      [8]byte{13, 14, 15, 16, 17, 18, 19, 20},
		Utxos:            []UtxoEntry{sampleUtxo()},
	}
	if n := len(b.Marshal()); n < 188 {
		t.Fatalf("realistic Accepting body = %d bytes, want >= 188 (hub drop gate)", n)
	}
}

func TestSwapBodiesRoundTrip(t *testing.T) {
	roundTrip(t, "HoldBody",
		func() []byte {
			return (&HoldBody{HubAddress: [20]byte{1}, ID: [32]byte{2}, FromAmount: 1, ToAmount: 2}).Marshal()
		},
		func(d []byte) error { var b HoldBody; return b.Unmarshal(d) })
	roundTrip(t, "HoldApplyBody",
		func() []byte {
			return (&HoldApplyBody{HubAddress: [20]byte{1}, ClientAddress: [20]byte{2}, ID: [32]byte{3}}).Marshal()
		},
		func(d []byte) error { var b HoldApplyBody; return b.Unmarshal(d) })
	roundTrip(t, "InitBody",
		func() []byte {
			return (&InitBody{ClientAddress: [20]byte{1}, HubAddress: [20]byte{2}, ID: [32]byte{3}, FromAddress: [20]byte{4}, FromCurrency: "BTC", FromAmount: 5, ToAddress: [20]byte{6}, ToCurrency: "LTC", ToAmount: 7}).Marshal()
		},
		func(d []byte) error { var b InitBody; return b.Unmarshal(d) })
	roundTrip(t, "InitializedBody",
		func() []byte {
			return (&InitializedBody{HubAddress: [20]byte{1}, ClientAddress: [20]byte{2}, ID: [32]byte{3}}).Marshal()
		},
		func(d []byte) error { var b InitializedBody; return b.Unmarshal(d) })
	roundTrip(t, "CreateABody",
		func() []byte {
			return (&CreateABody{HubAddress: [20]byte{2}, ID: [32]byte{3}, BPubKey: [33]byte{4}}).Marshal()
		},
		func(d []byte) error { var b CreateABody; return b.Unmarshal(d) })
	roundTrip(t, "CreatedABody",
		func() []byte {
			return (&CreatedABody{HubAddress: [20]byte{1}, ID: [32]byte{3}, ADepositTxID: "abc", HashedSecret: [20]byte{4}, ALockTime: 5, RefTxID: "r1", RefTx: "deadbeef"}).Marshal()
		},
		func(d []byte) error { var b CreatedABody; return b.Unmarshal(d) })
	roundTrip(t, "CreateBBody",
		func() []byte {
			return (&CreateBBody{HubAddress: [20]byte{2}, ID: [32]byte{3}, APubKey: [33]byte{4}, ADepositTxID: "abc", HashedSecret: [20]byte{5}, ALockTime: 6}).Marshal()
		},
		func(d []byte) error { var b CreateBBody; return b.Unmarshal(d) })
	roundTrip(t, "CreatedBBody",
		func() []byte {
			return (&CreatedBBody{HubAddress: [20]byte{1}, ID: [32]byte{3}, BDepositTxID: "xyz", BLockTime: 9, RefTxID: "r2", RefTx: "cafe"}).Marshal()
		},
		func(d []byte) error { var b CreatedBBody; return b.Unmarshal(d) })
	roundTrip(t, "ConfirmABody",
		func() []byte {
			return (&ConfirmABody{HubAddress: [20]byte{2}, ID: [32]byte{3}, BDepositTxID: "bdeps", BLockTime: 8}).Marshal()
		},
		func(d []byte) error { var b ConfirmABody; return b.Unmarshal(d) })
	roundTrip(t, "ConfirmedABody",
		func() []byte {
			return (&ConfirmedABody{HubAddress: [20]byte{1}, ID: [32]byte{3}, APayTxID: "apay"}).Marshal()
		},
		func(d []byte) error { var b ConfirmedABody; return b.Unmarshal(d) })
	roundTrip(t, "ConfirmBBody",
		func() []byte {
			return (&ConfirmBBody{HubAddress: [20]byte{2}, ID: [32]byte{3}, APayTxID: "apay"}).Marshal()
		},
		func(d []byte) error { var b ConfirmBBody; return b.Unmarshal(d) })
	roundTrip(t, "ConfirmedBBody",
		func() []byte {
			return (&ConfirmedBBody{HubAddress: [20]byte{1}, ID: [32]byte{3}, BPayTxID: "bpay"}).Marshal()
		},
		func(d []byte) error { var b ConfirmedBBody; return b.Unmarshal(d) })
	roundTrip(t, "CancelBody",
		func() []byte { return (&CancelBody{ID: [32]byte{1}, Reason: 7}).Marshal() },
		func(d []byte) error { var b CancelBody; return b.Unmarshal(d) })
	roundTrip(t, "RejectBody",
		func() []byte { return (&RejectBody{ID: [32]byte{1}, Reason: 8}).Marshal() },
		func(d []byte) error { var b RejectBody; return b.Unmarshal(d) })
	roundTrip(t, "FinishedBody",
		func() []byte { return (&FinishedBody{ID: [32]byte{2}}).Marshal() },
		func(d []byte) error { var b FinishedBody; return b.Unmarshal(d) })
}

// TestDecodeBodyRejectsUnwriterCommands asserts commands with no C++ writer on
// either side (xbcXChatMessage 2, xbcServicesPing 50) are rejected by DecodeBody
// rather than mis-decoded by a speculative body type.
func TestDecodeBodyRejectsUnwriterCommands(t *testing.T) {
	for _, cmd := range []XBridgeCommand{XbcXChatMessage, XbcServicesPing} {
		if v, err := DecodeBody(cmd, []byte{0x01}); err == nil {
			t.Fatalf("DecodeBody(%s) = %v, nil; want unsupported-command error", cmd, v)
		}
	}
}

// liveOrderPacket is the same real xbridge P2P payload captured from a live
// Blocknet 4.4.1 node (coreproxy.airdns.org:42111) as in p2p/envelope_test.go,
// here used to validate the xbcTransaction body decode end-to-end against the
// real wire (command=3, size=279).
const liveOrderPacket = "fdb4016894ff47163a031d3ac8bfce10dfa3fbe290a48a4d27edd3985606003700000003000000faa8566a780100001701000002c6d68e9a98bf4bc54ee9fa11429bde598d7aae4cb2b94fe99b534addcd31ceb7d265f986f41bed10f44a5d1cabb55481dcfb23a69eebc44118ec12c2f51a855b0a05905ecb036dc013993e447a3572d634fbd13c8aaab697668647cccd5c391e0000000000000000000000001e380c064ef996a30c44913c779be71b7121e6e9e5c28f2a06af3add363e1e91d8cf2f4e12873469a75d71c74778f3935c2d53b2444f474500000000115e1700000000001a11b75482580340dc4cc6bd349553e7767e1f94424c4f434b00000021d77501000000001c7238c59856060030bdbd7fe31bba634ebe9a567d79de601047d7f2cfbf81b43760d7f507e8c58300000000000000000000010000001c569806f0bd7a450eb49fbf6dcf5a0f3c3182a74978701f0204d1e948da798e02000000d8cf2f4e12873469a75d71c74778f3935c2d53b22076244bfe101207df00bb14ea647ea319599b0690014e6eb909cb6203190a5e2e2d82e48083df1e395c95f989741227452ebdcb0f1641d8ce9594031a69e1472b"

// stripEnvelope mirrors p2p.DecodeXBridgePayload's envelope removal without
// importing p2p (which would create an import cycle).
func stripEnvelope(t *testing.T, rawHex string) []byte {
	t.Helper()
	raw, err := hex.DecodeString(rawHex)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	// CompactSize varint.
	var n, off int
	switch raw[0] {
	case 0xfd:
		n = int(raw[1]) | int(raw[2])<<8
		off = 3
	case 0xfe:
		n = int(raw[1]) | int(raw[2])<<8 | int(raw[3])<<16 | int(raw[4])<<24
		off = 5
	default:
		n = int(raw[0])
		off = 1
	}
	start := off + 28 // skip 28-byte transport envelope
	end := off + n    // n covers envelope + packet
	if end > len(raw) {
		t.Fatalf("envelope length %d exceeds payload %d", n, len(raw)-start)
	}
	return raw[start:end]
}

func TestLiveOrderBodyDecode(t *testing.T) {
	pktBytes := stripEnvelope(t, liveOrderPacket)
	p, err := Unmarshal(pktBytes)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if p.Command != XbcTransaction {
		t.Fatalf("command = %d, want %d", p.Command, XbcTransaction)
	}
	if p.Size != 279 {
		t.Fatalf("size = %d, want 279", p.Size)
	}

	body, err := DecodeBody(XbcTransaction, p.Body)
	if err != nil {
		t.Fatalf("DecodeBody: %v", err)
	}
	ob, ok := body.(*OrderBody)
	if !ok {
		t.Fatalf("DecodeBody returned %T, want *OrderBody", body)
	}

	// Real-wire sanity checks (layout correctness, not value correctness):
	// currencies must be readable 8-byte ASCII codes, amounts non-zero, the
	// single UTXO must carry a 20-byte raw address and a 65-byte signature.
	if ob.FromCurrency == "" || ob.ToCurrency == "" {
		t.Fatalf("empty currency: from=%q to=%q", ob.FromCurrency, ob.ToCurrency)
	}
	for _, c := range []string{ob.FromCurrency, ob.ToCurrency} {
		if len(c) == 0 || len(c) > 8 {
			t.Errorf("implausible currency code %q", c)
		}
	}
	if ob.FromAmount == 0 || ob.ToAmount == 0 {
		t.Errorf("zero amount: from=%d to=%d", ob.FromAmount, ob.ToAmount)
	}
	if len(ob.Utxos) != 1 {
		t.Fatalf("utxo count = %d, want 1", len(ob.Utxos))
	}
	u := ob.Utxos[0]
	if u.Vout == 0 && u.TxID == [32]byte{} {
		t.Errorf("utxo fields look uninitialized")
	}
	t.Logf("live order: %s→%s  amount %d→%d  partial=%v  minFrom=%d  utxos=%d",
		ob.FromCurrency, ob.ToCurrency, ob.FromAmount, ob.ToAmount,
		ob.PartialAllowed, ob.MinFromAmount, len(ob.Utxos))
}
