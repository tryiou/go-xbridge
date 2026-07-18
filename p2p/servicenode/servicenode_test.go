package servicenode

import (
	"encoding/hex"
	"testing"
	"time"

	"xbridge-go/p2p"
)

// buildPing marshals a ServiceNodePing payload following the EXACT C++
// ServiceNodePing::SerializationOp order (servicenode.h:683-694):
//
//	snodePubKey (varint len=33 || 33 bytes)
//	bestBlock    (uint32 LE)
//	bestBlockHash (32 bytes)
//	pingTime     (uint32 LE)
//	config       (varstr = varint len || JSON)
//	snode        (embedded ServiceNode: pubkey, tier uint8,
//	              paymentAddress 20 raw bytes, collateral vec(0),
//	              bestBlock int32, bestBlockHash 32, signature vec(0))
//	signature    (varstr = vec(0))
func buildPing(pubkey [33]byte, tier uint8, config string) []byte {
	var b []byte
	b = append(b, p2p.MarshalVarStr(string(pubkey[:]))...) // ping snodePubKey
	b = appendLE32(b, 1000)                                // bestBlock (uint32)
	b = append(b, make([]byte, 32)...)                     // bestBlockHash
	b = appendLE32(b, 1700000000)                          // pingTime (uint32)
	b = append(b, p2p.MarshalVarStr(config)...)            // config (varstr)
	// embedded ServiceNode
	b = append(b, p2p.MarshalVarStr(string(pubkey[:]))...) // snodePubKey
	b = append(b, tier)                                    // tier uint8
	b = append(b, make([]byte, 20)...)                     // paymentAddress (CKeyID, 20 raw)
	b = appendVarInt0(b)                                   // collateral vec count = 0
	b = appendLE32(b, 1000)                                // bestBlock (int32)
	b = append(b, make([]byte, 32)...)                     // bestBlockHash
	b = appendVarInt0(b)                                   // signature vec len = 0
	// ping signature (varstr, vec len = 0)
	b = appendVarInt0(b)
	return b
}

func appendLE32(b []byte, v uint32) []byte {
	return append(b, byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
}

func appendVarInt0(b []byte) []byte {
	return append(b, 0) // CompactSize 0
}

func mustPubkey(t *testing.T, h string) [33]byte {
	t.Helper()
	var pk [33]byte
	raw, err := hex.DecodeString(h)
	if err != nil || len(raw) != 33 {
		t.Fatalf("bad pubkey hex %q", h)
	}
	copy(pk[:], raw)
	return pk
}

// TestWalletServicesParity verifies that a hand-crafted SNPING payload parses
// exactly like C++ and that WalletServices() yields the filtered union a core
// XBridge wallet's dxGetNetworkTokens would return (walletServices(),
// xbridgeapp.cpp:2758): SPV xbridge tokens only, matching ^[^:]+$, excluding
// xr/xrs.
func TestWalletServicesParity(t *testing.T) {
	// SPV node advertising BLOCK, BTC, xr (must be excluded), and an XRouter
	// service name containing ':' (must be excluded).
	pk1 := mustPubkey(t, "02"+hex.EncodeToString(make([]byte, 32)))
	cfg1 := `{"xbridgeversion":4140100,"xrouterversion":4140100,"xbridge":["BLOCK","BTC","xr","xrouter:btc_getblockcount"]}`
	ping1 := buildPing(pk1, TierSPV, cfg1)

	sn1, err := ParseServiceNodePing(ping1)
	if err != nil {
		t.Fatalf("ParseServiceNodePing: %v", err)
	}
	if sn1.Tier != TierSPV {
		t.Fatalf("tier = %d, want SPV(1)", sn1.Tier)
	}
	// parseConfig returns the raw xbridge array (C++ stores raw services; the
	// ^[^:]+$ / xr / xrs filtering happens later in WalletServices()).
	if len(sn1.Services) != 4 {
		t.Fatalf("services = %v, want [BLOCK BTC xr xrouter:btc_getblockcount]", sn1.Services)
	}

	// Second SPV node advertising LTC and DOGE.
	pk2 := mustPubkey(t, "03"+hex.EncodeToString(make([]byte, 32)))
	cfg2 := `{"xbridgeversion":4140100,"xrouterversion":4140100,"xbridge":["LTC","DOGE"]}`
	ping2 := buildPing(pk2, TierSPV, cfg2)
	sn2, err := ParseServiceNodePing(ping2)
	if err != nil {
		t.Fatalf("ParseServiceNodePing #2: %v", err)
	}

	// A non-SPV (OPEN) node must contribute NO wallet services.
	pk3 := mustPubkey(t, "04"+hex.EncodeToString(make([]byte, 32)))
	cfg3 := `{"xbridgeversion":4140100,"xrouterversion":4140100,"xbridge":["EVIL"]}`
	ping3 := buildPing(pk3, TierOpen, cfg3)
	sn3, err := ParseServiceNodePing(ping3)
	if err != nil {
		t.Fatalf("ParseServiceNodePing #3: %v", err)
	}
	if len(sn3.Services) != 0 {
		t.Fatalf("non-SPV services = %v, want empty", sn3.Services)
	}

	reg := NewRegistry()
	reg.AddPing(sn1)
	reg.AddPing(sn2)
	reg.AddPing(sn3)
	if reg.Count() != 3 {
		t.Fatalf("registry count = %d, want 3", reg.Count())
	}

	got := reg.WalletServices()
	want := []string{"BLOCK", "BTC", "DOGE", "LTC"}
	if len(got) != len(want) {
		t.Fatalf("WalletServices = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("WalletServices[%d] = %q, want %q (got %v)", i, got[i], want[i], got)
		}
	}
}

// TestRunningGate verifies that only recently-pinged servicenodes (within the
// 5-minute window, ServiceNode::running(), servicenode.h:244) contribute to
// WalletServices().
func TestRunningGate(t *testing.T) {
	pk1 := mustPubkey(t, "02"+hex.EncodeToString(make([]byte, 32)))
	cfg1 := `{"xbridgeversion":4140100,"xrouterversion":4140100,"xbridge":["BLOCK"]}`
	sn1, err := ParseServiceNodePing(buildPing(pk1, TierSPV, cfg1))
	if err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry()
	reg.AddPing(sn1)
	if len(reg.WalletServices()) != 1 {
		t.Fatalf("expected BLOCK from running node")
	}

	// Simulate staleness by advancing the registry clock 6 minutes.
	reg.now = func() time.Time { return time.Now().Add(6 * time.Minute) }
	if len(reg.WalletServices()) != 0 {
		t.Fatalf("stale node should not contribute; got %v", reg.WalletServices())
	}
}

// TestParseServiceNode verifies SNREGISTER parsing (no bestBlock/pingTime prefix,
// int32 bestBlock, embedded config-less ServiceNode).
func TestParseServiceNode(t *testing.T) {
	pk := mustPubkey(t, "02"+hex.EncodeToString(make([]byte, 32)))
	var b []byte
	b = append(b, p2p.MarshalVarStr(string(pk[:]))...) // snodePubKey
	b = append(b, TierSPV)                             // tier uint8
	b = append(b, make([]byte, 20)...)                 // paymentAddress
	b = appendVarInt0(b)                               // collateral vec(0)
	b = appendLE32(b, 1000)                            // bestBlock (int32)
	b = append(b, make([]byte, 32)...)                 // bestBlockHash
	b = appendVarInt0(b)                               // signature vec(0)
	sn, err := ParseServiceNode(b)
	if err != nil {
		t.Fatalf("ParseServiceNode: %v", err)
	}
	if sn.Tier != TierSPV {
		t.Fatalf("tier = %d, want SPV", sn.Tier)
	}
}
