package servicenode

import (
	"fmt"
	"testing"

	"go-xbridge/version"
)

// TestPickAdmitsProtocolVersionOverride locks in the daemon-level
// -xbridgeversion path end to end at the registry gate: when the effective
// XBridge protocol version is overridden (SetXBridgeProtocolVersion), Pick
// admits pings advertising the overridden version and rejects pings
// advertising any other version (servicenode.go Pick gate: e.xbridgeVersion
// != version.XBridgeProtocolVersion). Tests restore the previous value; no
// t.Parallel anywhere in this repo — the global is safe to mutate under
// save/restore.
func TestPickAdmitsProtocolVersionOverride(t *testing.T) {
	orig := version.XBridgeProtocolVersion
	override := orig + 1
	version.SetXBridgeProtocolVersion(override)
	t.Cleanup(func() { version.SetXBridgeProtocolVersion(orig) })

	hub := pickPubkey(t, 0x33)
	other := pickPubkey(t, 0x34)
	reg := NewRegistry()
	reg.AddPing(ServiceNode{
		PubKey:         hub,
		Tier:           TierSPV,
		Services:       []string{"BTC", "LTC"},
		XBridgeVersion: override,
	})
	reg.AddPing(ServiceNode{
		PubKey:         other,
		Tier:           TierSPV,
		Services:       []string{"BTC", "LTC"},
		XBridgeVersion: orig,
	})

	got, ok := reg.Pick([]string{"BTC"})
	if !ok {
		t.Fatal("Pick must admit a hub advertising the overridden version")
	}
	if got != hub {
		t.Fatalf("Pick = %s, want the override-version hub", fmt.Sprintf("%x", got[:]))
	}

	// With the override-version hub excluded, the old-version hub must NOT
	// become eligible: the gate compares against the effective version.
	if _, ok := reg.Pick([]string{"BTC"}, hub); ok {
		t.Fatal("Pick must not admit a hub advertising a foreign version under the override")
	}
}
