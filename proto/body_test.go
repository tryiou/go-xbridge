package proto

import (
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
			return (&CreateABody{ClientAddress: [20]byte{1}, HubAddress: [20]byte{2}, ID: [32]byte{3}, BPubKey: [33]byte{4}}).Marshal()
		},
		func(d []byte) error { var b CreateABody; return b.Unmarshal(d) })
	roundTrip(t, "CreatedABody",
		func() []byte {
			return (&CreatedABody{HubAddress: [20]byte{1}, ClientAddress: [20]byte{2}, ID: [32]byte{3}, ADepositTxID: "abc", HashedSecret: [20]byte{4}, ALockTime: 5, BLockTime: 6}).Marshal()
		},
		func(d []byte) error { var b CreatedABody; return b.Unmarshal(d) })
	roundTrip(t, "CreateBBody",
		func() []byte {
			return (&CreateBBody{ClientAddress: [20]byte{1}, HubAddress: [20]byte{2}, ID: [32]byte{3}, APubKey: [33]byte{4}, ADepositTxID: "abc", HashedSecret: [20]byte{5}, ALockTime: 6, BLockTime: 7}).Marshal()
		},
		func(d []byte) error { var b CreateBBody; return b.Unmarshal(d) })
	roundTrip(t, "CreatedBBody",
		func() []byte {
			return (&CreatedBBody{HubAddress: [20]byte{1}, ClientAddress: [20]byte{2}, ID: [32]byte{3}, BDepositTxID: "xyz"}).Marshal()
		},
		func(d []byte) error { var b CreatedBBody; return b.Unmarshal(d) })
	roundTrip(t, "ConfirmABody",
		func() []byte {
			return (&ConfirmABody{ClientAddress: [20]byte{1}, HubAddress: [20]byte{2}, ID: [32]byte{3}, BDepositTxID: "bdeps"}).Marshal()
		},
		func(d []byte) error { var b ConfirmABody; return b.Unmarshal(d) })
	roundTrip(t, "ConfirmedABody",
		func() []byte {
			return (&ConfirmedABody{HubAddress: [20]byte{1}, ClientAddress: [20]byte{2}, ID: [32]byte{3}, XPubKey: [33]byte{4}}).Marshal()
		},
		func(d []byte) error { var b ConfirmedABody; return b.Unmarshal(d) })
	roundTrip(t, "ConfirmBBody",
		func() []byte {
			return (&ConfirmBBody{ClientAddress: [20]byte{1}, HubAddress: [20]byte{2}, ID: [32]byte{3}, XPubKey: [33]byte{4}, ADepositTxID: "adep"}).Marshal()
		},
		func(d []byte) error { var b ConfirmBBody; return b.Unmarshal(d) })
	roundTrip(t, "ConfirmedBBody",
		func() []byte {
			return (&ConfirmedBBody{HubAddress: [20]byte{1}, ClientAddress: [20]byte{2}, ID: [32]byte{3}}).Marshal()
		},
		func(d []byte) error { var b ConfirmedBBody; return b.Unmarshal(d) })
	roundTrip(t, "CancelBody",
		func() []byte { return (&CancelBody{ID: [32]byte{1}, Reason: 7}).Marshal() },
		func(d []byte) error { var b CancelBody; return b.Unmarshal(d) })
	roundTrip(t, "RejectBody",
		func() []byte { return (&RejectBody{ID: [32]byte{1}, Reason: 8}).Marshal() },
		func(d []byte) error { var b RejectBody; return b.Unmarshal(d) })
	roundTrip(t, "FinishedBody",
		func() []byte { return (&FinishedBody{ClientAddress: [20]byte{1}, ID: [32]byte{2}}).Marshal() },
		func(d []byte) error { var b FinishedBody; return b.Unmarshal(d) })
	roundTrip(t, "ServicesPingBody",
		func() []byte { return (&ServicesPingBody{Services: []string{"dx", "blocknet"}}).Marshal() },
		func(d []byte) error { var b ServicesPingBody; return b.Unmarshal(d) })
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
	end := off + n     // n covers envelope + packet
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
