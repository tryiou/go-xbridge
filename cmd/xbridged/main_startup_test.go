package main

import (
	"testing"
)

// TestStartupBannerArgs pins the in-file startup banner contract: binary
// identity plus the P2P-affecting knobs, in a fixed order, with values
// passed through untouched. The banner is the only in-file record of what binary is running (the pre-file-logger stderr line does
// not reach xbridged.log), so a missing or reordered field breaks
// post-mortem attribution.
func TestStartupBannerArgs(t *testing.T) {
	args := startupBannerArgs("0.0.2", "abc123", "2026-09-24", 56, "mainnet", "a1a0a2a3", 4, 2, "/data/x", "debug", true)

	wantKeys := []string{"version", "commit", "date", "xbridgeversion", "network", "magic", "addnodes", "takeretry", "datadir", "loglevel", "dxnowallets"}
	wantVals := []any{"0.0.2", "abc123", "2026-09-24", uint32(56), "mainnet", "a1a0a2a3", 4, 2, "/data/x", "debug", true}
	if len(args) != 2*len(wantKeys) {
		t.Fatalf("args len = %d, want %d (key/value pairs)", len(args), 2*len(wantKeys))
	}
	for i, k := range wantKeys {
		if args[2*i] != k {
			t.Errorf("args[%d] = %v, want key %q", 2*i, args[2*i], k)
		}
		if args[2*i+1] != wantVals[i] {
			t.Errorf("args[%d] = %v, want %v", 2*i+1, args[2*i+1], wantVals[i])
		}
	}
}
