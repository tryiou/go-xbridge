package servicenode

import (
	"encoding/hex"
	"testing"
	"time"

	"go-xbridge/crypto"
	"go-xbridge/p2p"
	"go-xbridge/proto"
)

// mustKeypair returns a fresh secp256k1 keypair (compressed pubkey, privkey).
func mustKeypair(t *testing.T) ([33]byte, [32]byte) {
	t.Helper()
	priv, err := crypto.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	var privArr [32]byte
	copy(privArr[:], priv)
	pub, err := crypto.CompressedPubKey(privArr[:])
	if err != nil {
		t.Fatal(err)
	}
	return pub, privArr
}

// buildPingParts marshals a ServiceNodePing payload following the EXACT C++
// ServiceNodePing::SerializationOp order (servicenode.h:683-694): snodePubKey,
// bestBlock, bestBlockHash, pingTime, config, embedded ServiceNode (pubkey,
// tier, paymentAddress, collateral vec(0), bestBlock, bestBlockHash, signature
// vec(0)), ping signature. The ping is signed with priv over its sigHash
// (servicenode.h:748-752, sign :769-771) like a real SNPING. Outer and inner
// pubkeys may differ to exercise the isValid pubkey check (servicenode.h:791).
func buildPingParts(t *testing.T, outer, inner [33]byte, priv [32]byte, tier uint8, config string) []byte {
	t.Helper()
	var b []byte
	b = append(b, p2p.MarshalVarStr(string(outer[:]))...) // ping snodePubKey
	b = appendLE32(b, 1000)                               // bestBlock (uint32)
	b = append(b, make([]byte, 32)...)                    // bestBlockHash
	b = appendLE32(b, uint32(time.Now().Unix()))          // pingTime (uint32)
	b = append(b, p2p.MarshalVarStr(config)...)           // config (varstr)
	// embedded ServiceNode
	b = append(b, p2p.MarshalVarStr(string(inner[:]))...) // snodePubKey
	b = append(b, tier)                                   // tier uint8
	b = append(b, make([]byte, 20)...)                    // paymentAddress (CKeyID, 20 raw)
	b = appendVarInt0(b)                                  // collateral vec count = 0
	b = appendLE32(b, 1000)                               // bestBlock (int32)
	b = append(b, make([]byte, 32)...)                    // bestBlockHash
	b = appendVarInt0(b)                                  // signature vec len = 0
	// ping signature = SignCompact over double-SHA256(b) (sigHash)
	hash := crypto.DoubleSHA256(b)
	sig, err := crypto.SignCompact(priv[:], hash[:])
	if err != nil {
		t.Fatal(err)
	}
	b = append(b, p2p.MarshalVarStr(string(sig))...) // signature (varstr)
	return b
}

// buildPing signs a ping for a single keypair (outer == inner pubkey).
func buildPing(t *testing.T, pub [33]byte, priv [32]byte, tier uint8, config string) []byte {
	t.Helper()
	return buildPingParts(t, pub, pub, priv, tier, config)
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

// TestWalletServicesParity verifies a hand-crafted SNPING payload parses like
// C++ and WalletServices() yields the filtered union walletServices() would
// return (xbridgeapp.cpp:2758): SPV tokens only, ^[^:]+$, excluding xr/xrs.
func TestWalletServicesParity(t *testing.T) {
	// SPV node advertising BLOCK, BTC, xr (must be excluded), and an XRouter
	// service name containing ':' (must be excluded).
	pk1, priv1 := mustKeypair(t)
	cfg1 := `{"xbridgeversion":4140100,"xrouterversion":4140100,"xbridge":["BLOCK","BTC","xr","xrouter:btc_getblockcount"]}`
	ping1 := buildPing(t, pk1, priv1, TierSPV, cfg1)

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
	pk2, priv2 := mustKeypair(t)
	cfg2 := `{"xbridgeversion":4140100,"xrouterversion":4140100,"xbridge":["LTC","DOGE"]}`
	ping2 := buildPing(t, pk2, priv2, TierSPV, cfg2)
	sn2, err := ParseServiceNodePing(ping2)
	if err != nil {
		t.Fatalf("ParseServiceNodePing #2: %v", err)
	}

	// A non-SPV (OPEN) node must contribute NO wallet services.
	pk3, priv3 := mustKeypair(t)
	cfg3 := `{"xbridgeversion":4140100,"xrouterversion":4140100,"xbridge":["EVIL"]}`
	ping3 := buildPing(t, pk3, priv3, TierOpen, cfg3)
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
	// The OPEN-tier ping fails isValid (servicenode.h:787) and creates no node,
	// mirroring C++ processPing (servicenodemgr.h:186-187).
	if reg.Count() != 2 {
		t.Fatalf("registry count = %d, want 2 (non-SPV ping creates no node)", reg.Count())
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
	pk1, priv1 := mustKeypair(t)
	cfg1 := `{"xbridgeversion":4140100,"xrouterversion":4140100,"xbridge":["BLOCK"]}`
	sn1, err := ParseServiceNodePing(buildPing(t, pk1, priv1, TierSPV, cfg1))
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

// pickPubkey builds a distinct compressed pubkey per seed so Pick tests can
// distinguish candidates.
func pickPubkey(t *testing.T, seed byte) [33]byte {
	t.Helper()
	raw := make([]byte, 32)
	raw[31] = seed
	return mustPubkey(t, "02"+hex.EncodeToString(raw))
}

// TestPickEmpty verifies Pick on an empty registry returns false (no eligible
// hub -> C++ makeTransaction fails with NO_SERVICE_NODE).
func TestPickEmpty(t *testing.T) {
	reg := NewRegistry()
	if _, ok := reg.Pick([]string{"BTC"}); ok {
		t.Fatal("Pick on empty registry should return false")
	}
}

// TestPickVersionGate verifies a node whose advertised xbridge version does not
// equal XBRIDGE_PROTOCOL_VERSION is never selected (xbridgeapp.cpp:2905).
func TestPickVersionGate(t *testing.T) {
	reg := NewRegistry()
	reg.AddPing(ServiceNode{PubKey: pickPubkey(t, 1), Tier: TierSPV, Services: []string{"BTC"}, XBridgeVersion: proto.ProtocolVersion - 1})
	if _, ok := reg.Pick([]string{"BTC"}); ok {
		t.Fatal("version-mismatched node must not be picked")
	}
}

// TestPickRunningGate verifies a stale (not running) node is never selected.
func TestPickRunningGate(t *testing.T) {
	reg := NewRegistry()
	reg.AddPing(ServiceNode{PubKey: pickPubkey(t, 2), Tier: TierSPV, Services: []string{"BTC"}, XBridgeVersion: proto.ProtocolVersion})
	reg.now = func() time.Time { return time.Now().Add(6 * time.Minute) }
	if _, ok := reg.Pick([]string{"BTC"}); ok {
		t.Fatal("stale node must not be picked")
	}
}

// TestPickServicesGate verifies a node missing one of the requested currencies
// is never selected (containsAll of findShuffledNodesWithService).
func TestPickServicesGate(t *testing.T) {
	reg := NewRegistry()
	reg.AddPing(ServiceNode{PubKey: pickPubkey(t, 3), Tier: TierSPV, Services: []string{"BTC"}, XBridgeVersion: proto.ProtocolVersion})
	if _, ok := reg.Pick([]string{"BTC", "LTC"}); ok {
		t.Fatal("node missing LTC must not be picked for BTC/LTC")
	}
}

// TestPickEligible verifies a running, version-matching node advertising both
// currencies is selected, and the returned key is one of the eligible set.
func TestPickEligible(t *testing.T) {
	reg := NewRegistry()
	blocker := pickPubkey(t, 4)
	reg.AddPing(ServiceNode{PubKey: blocker, Tier: TierSPV, Services: []string{"BTC"}, XBridgeVersion: proto.ProtocolVersion})
	hub := pickPubkey(t, 5)
	reg.AddPing(ServiceNode{PubKey: hub, Tier: TierSPV, Services: []string{"BTC", "LTC"}, XBridgeVersion: proto.ProtocolVersion})

	pk, ok := reg.Pick([]string{"BTC", "LTC"})
	if !ok {
		t.Fatal("eligible hub should be picked")
	}
	if pk != hub {
		t.Fatalf("Pick returned %x, want the eligible node %x", pk, hub)
	}
}

// TestParseServiceNodePingRejectsFloatVersion verifies a float-formatted or
// out-of-int32-range xbridgeversion is REJECTED like C++ UniValue::get_int()
// (univalue_get.cpp:104-112): get_int() throws, parseConfig's catch
// (servicenode.h:614-615) leaves version 0 with services cleared — never
// truncated, never wrapped.
func TestParseServiceNodePingRejectsFloatVersion(t *testing.T) {
	pk, priv := mustKeypair(t)
	cases := []string{
		`{"xbridgeversion":55.0,"xrouterversion":55,"xbridge":["BTC"]}`,       // float form
		`{"xbridgeversion":5e1,"xrouterversion":55,"xbridge":["BTC"]}`,        // exponent form
		`{"xbridgeversion":3000000000,"xrouterversion":55,"xbridge":["BTC"]}`, // out of int32 range
	}
	for _, cfg := range cases {
		sn, err := ParseServiceNodePing(buildPing(t, pk, priv, TierSPV, cfg))
		if err != nil {
			t.Fatal(err)
		}
		if sn.XBridgeVersion != 0 {
			t.Fatalf("xbridgeversion = %d, want 0 (C++ get_int() rejects %s)", sn.XBridgeVersion, cfg)
		}
		if len(sn.Services) != 0 {
			t.Fatalf("services = %v, want empty (parseConfig aborted)", sn.Services)
		}
	}
}

// TestParseServiceNodePingRejectsQuotedVersion verifies a QUOTED numeric
// xbridgeversion/xrouterversion is rejected like C++: get_int() requires a bare
// number token (univalue_get.cpp:104-112), a quoted string (VSTR) throws, and
// parseConfig keeps version 0 (servicenode.h:614-615).
func TestParseServiceNodePingRejectsQuotedVersion(t *testing.T) {
	pk, priv := mustKeypair(t)
	cases := []struct {
		cfg string
		ver uint32
	}{
		{`{"xbridgeversion":"55","xrouterversion":55,"xbridge":["BTC"]}`, 0},  // quoted version -> 0
		{`{"xbridgeversion":55,"xrouterversion":"55","xbridge":["BTC"]}`, 55}, // quoted xrouterversion -> version retained
	}
	for _, tc := range cases {
		sn, err := ParseServiceNodePing(buildPing(t, pk, priv, TierSPV, tc.cfg))
		if err != nil {
			t.Fatal(err)
		}
		if sn.XBridgeVersion != tc.ver {
			t.Fatalf("xbridgeversion = %d, want %d (C++ get_int() rejects %s)", sn.XBridgeVersion, tc.ver, tc.cfg)
		}
		if len(sn.Services) != 0 {
			t.Fatalf("services = %v, want empty (parseConfig aborted)", sn.Services)
		}
	}
}

// TestParseServiceNodePingRejectsFloatXrouterVersion verifies a float
// xrouterversion is REJECTED like C++ (strict get_int() at servicenode.h:552):
// no services are collected, but the already-assigned xbridgeversion is
// retained (servicenode.h:546). 56 is used so the assertion is independent of
// proto.ProtocolVersion.
func TestParseServiceNodePingRejectsFloatXrouterVersion(t *testing.T) {
	pk, priv := mustKeypair(t)
	cfg := `{"xbridgeversion":56,"xrouterversion":55.0,"xbridge":["BTC"]}`
	sn, err := ParseServiceNodePing(buildPing(t, pk, priv, TierSPV, cfg))
	if err != nil {
		t.Fatal(err)
	}
	if sn.XBridgeVersion != 56 {
		t.Fatalf("xbridgeversion = %d, want 56 (retained before xrouterversion check)", sn.XBridgeVersion)
	}
	if len(sn.Services) != 0 {
		t.Fatalf("services = %v, want empty (parseConfig aborted)", sn.Services)
	}
}

// TestParseServiceNodePingRejectsBadSignature verifies a ping whose signature
// does not verify against its pubkey is rejected like C++ isValid
// (servicenode.h:810-815): a different signing key or a tampered byte.
func TestParseServiceNodePingRejectsBadSignature(t *testing.T) {
	cfg := `{"xbridgeversion":55,"xrouterversion":55,"xbridge":["BTC"]}`
	pubA, privA := mustKeypair(t)
	_, privB := mustKeypair(t)

	if _, err := ParseServiceNodePing(buildPing(t, pubA, privB, TierSPV, cfg)); err == nil {
		t.Fatal("ping signed by a different key must be rejected")
	}

	b := buildPing(t, pubA, privA, TierSPV, cfg)
	b[len(b)-1] ^= 0x01 // flip one byte of the trailing signature
	if _, err := ParseServiceNodePing(b); err == nil {
		t.Fatal("tampered ping signature must be rejected")
	}
}

// TestParseServiceNodePingRejectsMismatchedPubkeys verifies the ping is
// rejected when the outer snodePubKey differs from the embedded snode pubkey
// (isValid, servicenode.h:791).
func TestParseServiceNodePingRejectsMismatchedPubkeys(t *testing.T) {
	cfg := `{"xbridgeversion":55,"xrouterversion":55,"xbridge":["BTC"]}`
	pubA, _ := mustKeypair(t)
	pubB, privB := mustKeypair(t)
	b := buildPingParts(t, pubA, pubB, privB, TierSPV, cfg)
	if _, err := ParseServiceNodePing(b); err == nil {
		t.Fatal("outer/inner pubkey mismatch must be rejected")
	}
}

// TestParseServiceNodePingRejectsUncompressedHeader verifies a ping whose
// signature header claims an uncompressed pubkey is rejected, matching C++:
// RecoverCompact serializes the recovered key uncompressed (pubkey.cpp:199-205),
// so a 65-byte key never equals the 33-byte compressed snodePubKey
// (servicenode.h:814).
func TestParseServiceNodePingRejectsUncompressedHeader(t *testing.T) {
	cfg := `{"xbridgeversion":55,"xrouterversion":55,"xbridge":["BTC"]}`
	pub, priv := mustKeypair(t)
	b := buildPing(t, pub, priv, TierSPV, cfg)
	// The trailing signature varstr is 1-byte CompactSize (0x41) + 65 bytes, so
	// the header byte sits at len(b)-65. Compressed headers are 0x1f-0x22
	// (27 + recid + 4*compressed).
	hdr := b[len(b)-65]
	if hdr < 0x1f || hdr > 0x22 {
		t.Fatalf("unexpected signature header byte 0x%02x", hdr)
	}
	b[len(b)-65] = hdr - 4 // -> uncompressed 0x1b-0x1e, same recid bits
	if _, err := ParseServiceNodePing(b); err != errBadSignature {
		t.Fatalf("ParseServiceNodePing = %v, want errBadSignature", err)
	}
}

// TestAddPingNewerPingReplacesEntry verifies a STRICTLY NEWER valid ping
// REPLACES version/services wholesale, mirroring processPing (servicenodemgr.h:
// 189-192): addPing's strict-newer gate (:843-852) then addSn's wholesale
// replace (:861-871, setConfig, servicenode.h:671-675). Never merged.
func TestAddPingNewerPingReplacesEntry(t *testing.T) {
	reg := NewRegistry()
	key := pickPubkey(t, 0x61)
	base := uint32(time.Now().Unix()) - 200 // within the running() window
	reg.AddPing(ServiceNode{PubKey: key, Tier: TierSPV, Services: []string{"BTC"}, XBridgeVersion: proto.ProtocolVersion, PingTime: base})
	if _, ok := reg.Pick([]string{"BTC"}); !ok {
		t.Fatal("fresh node should be eligible")
	}
	if len(reg.WalletServices()) != 1 {
		t.Fatalf("WalletServices = %v, want [BTC]", reg.WalletServices())
	}

	reg.AddPing(ServiceNode{PubKey: key, Tier: TierSPV, Services: []string{"BTC"}, XBridgeVersion: 54, PingTime: base + 50})
	if _, ok := reg.Pick([]string{"BTC"}); ok {
		t.Fatal("newer ping with a different version must fail Pick's version gate (wholesale replace)")
	}
	if len(reg.WalletServices()) != 1 {
		t.Fatalf("WalletServices = %v, want [BTC] (no version gate on WalletServices)", reg.WalletServices())
	}

	reg.AddPing(ServiceNode{PubKey: key, Tier: TierSPV, Services: []string{"LTC"}, XBridgeVersion: proto.ProtocolVersion, PingTime: base + 100})
	if got := reg.WalletServices(); len(got) != 1 || got[0] != "LTC" {
		t.Fatalf("newer ping must replace services wholesale (no merge); got %v", got)
	}
}

// TestAddPingInvalidPingIgnored verifies a ping failing C++ isValid() is never
// stored (processPing, servicenodemgr.h:186-187): non-SPV tier
// (servicenode.h:787) and empty services (:794-795) cannot clear a healthy node.
func TestAddPingInvalidPingIgnored(t *testing.T) {
	reg := NewRegistry()
	key := pickPubkey(t, 0x64)
	base := uint32(time.Now().Unix()) - 200 // within the running() window
	reg.AddPing(ServiceNode{PubKey: key, Tier: TierSPV, Services: []string{"BTC"}, XBridgeVersion: proto.ProtocolVersion, PingTime: base})
	if _, ok := reg.Pick([]string{"BTC"}); !ok {
		t.Fatal("fresh node should be eligible")
	}

	reg.AddPing(ServiceNode{PubKey: key, Tier: TierSPV, PingTime: base + 50})                                                     // newer, but empty service list
	reg.AddPing(ServiceNode{PubKey: key, Services: []string{"BTC"}, XBridgeVersion: proto.ProtocolVersion, PingTime: base + 100}) // newer, but non-SPV tier
	if _, ok := reg.Pick([]string{"BTC"}); !ok {
		t.Fatal("invalid pings must not clear a healthy node (C++ isValid gate)")
	}
	if got := reg.WalletServices(); len(got) != 1 || got[0] != "BTC" {
		t.Fatalf("invalid pings must not clear services; got %v", got)
	}
}

// TestAddPingIgnoresStalePing verifies an OLDER ping never touches the stored
// node (addPing, servicenodemgr.h:843-852): a ping not strictly newer returns
// false in processPing (:189-190), so addSn never runs — a late-arriving stale
// ping must not reset a healthy node.
func TestAddPingIgnoresStalePing(t *testing.T) {
	reg := NewRegistry()
	key := pickPubkey(t, 0x63)
	base := uint32(time.Now().Unix()) - 200 // within the running() window
	reg.AddPing(ServiceNode{PubKey: key, Tier: TierSPV, Services: []string{"BTC"}, XBridgeVersion: proto.ProtocolVersion, PingTime: base})
	if _, ok := reg.Pick([]string{"BTC"}); !ok {
		t.Fatal("fresh node should be eligible")
	}

	reg.AddPing(ServiceNode{PubKey: key, Tier: TierSPV, Services: []string{"BTC"}, PingTime: base - 10}) // stale ping
	if _, ok := reg.Pick([]string{"BTC"}); !ok {
		t.Fatal("stale ping must not clear a healthy node (C++ addPing gate)")
	}
	if got := reg.WalletServices(); len(got) != 1 || got[0] != "BTC" {
		t.Fatalf("stale ping must not clear services; got %v", got)
	}
}

// TestAddRegistrationClearsNode verifies a registration arriving AFTER a ping
// resets the node: SNREGISTER carries no config on the wire (servicenode.h:355-
// 384), so addSn stores version 0/empty services — the Go registry must never
// merge the ping's earlier data. isValid has no service-list requirement
// (servicenode.h:398), only the SPV tier.
func TestAddRegistrationClearsNode(t *testing.T) {
	reg := NewRegistry()
	key := pickPubkey(t, 0x62)
	reg.AddPing(ServiceNode{PubKey: key, Tier: TierSPV, Services: []string{"BTC"}, XBridgeVersion: proto.ProtocolVersion})
	if _, ok := reg.Pick([]string{"BTC"}); !ok {
		t.Fatal("pinged node should be eligible before registration")
	}

	reg.AddRegistration(ServiceNode{PubKey: key, Tier: TierSPV}) // no wire config -> version 0, no services
	if _, ok := reg.Pick([]string{"BTC"}); ok {
		t.Fatal("registration-after-ping must reset the node (C++ addSn replace)")
	}
}

// TestPickEmptyNeed verifies Pick with an empty service request returns false,
// mirroring C++ findShuffledNodesWithService: searchCounter = 0 so the inner
// loop never reaches --searchCounter == 0 and no node is pushed
// (xbridgeapp.cpp:2924-2930).
func TestPickEmptyNeed(t *testing.T) {
	reg := NewRegistry()
	reg.AddPing(ServiceNode{PubKey: pickPubkey(t, 0x71), Tier: TierSPV, Services: []string{"BTC"}, XBridgeVersion: proto.ProtocolVersion})
	if _, ok := reg.Pick(nil); ok {
		t.Fatal("Pick(nil) must not return a hub (C++ never pushes for an empty request)")
	}
	if _, ok := reg.Pick([]string{}); ok {
		t.Fatal("Pick([]) must not return a hub (C++ never pushes for an empty request)")
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
