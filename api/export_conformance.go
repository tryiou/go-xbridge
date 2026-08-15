//go:build conformance

package api

import (
	"go-xbridge/coins"
)

// Conformance-test export shim.
//
// The external conformance suite (module go-xbridge/conformance, run with
// `go test -tags conformance`) exercises go-xbridge's wire/crypto/state code
// through the exported packages directly, but its fx* fixture hooks need a
// handful of go-xbridge/api functions that are intentionally unexported
// (they are internal helpers, not RPC surface). This file re-exports exactly
// those functions under the `conformance` build tag so the suite can wire its
// fixtures without widening the package's public API for normal builds.
//
// Build-tag discipline: this file is compiled ONLY with `-tags conformance`.
// Normal `go build ./...` / `go test ./...` never see these symbols.

// XbridgeErrorText re-exports xbridgeErrorText (mirror of C++
// util/xbridgeerror.cpp::xbridgeErrorText).
func XbridgeErrorText(code int, arg string) string { return xbridgeErrorText(code, arg) }

// FormatXAmount re-exports formatXAmount (xBridgeStringValueFromAmount).
func FormatXAmount(amt uint64) string { return formatXAmount(amt) }

// FormatBalanceNative re-exports formatBalanceNative (xBridgeStringValueFromPrice
// over a native base-unit balance) adapted to the suite's (decimals, native)
// signature.
func FormatBalanceNative(decimals, native uint64) string {
	return formatBalanceNative(coins.Coin{Decimals: int(decimals)}, native)
}

// FormatXPrice re-exports formatXPrice over a plain ratio (denominator /
// numerator). The suite's signature mirrors the C++ price formula call sites
// (xutil.cpp:293-312); Go renders the plain ratio, which is itself the
// documented DIVERGENT behavior for dxGetOrderBook (see the conformance
// suite's TestFormatXPriceVectors row dxGetOrderBook/price-formula).
func FormatXPrice(numerator, denominator uint64) string {
	return formatXPrice(float64(denominator) / float64(numerator))
}

// ISO8601 re-exports iso8601 (xutil::iso8601).
func ISO8601(us uint64) string { return iso8601(us) }

// ParseXAmount re-exports parseXAmount (xBridgeAmountFromString).
func ParseXAmount(s string) (uint64, error) { return parseXAmount(s) }

// LocktimeConstants re-exports the unexported locktime computation constants
// (api/swap.go, api/locktime.go) by the C++ constant names the suite asserts.
func LocktimeConstants() map[string]int64 {
	return map[string]int64{
		"XMIN_LOCKTIME_BLOCKS":                xMinLockTimeBlocks,
		"XMAX_LOCKTIME_DRIFT_BLOCKS":          xMaxLockTimeDriftBlocks,
		"XMAKER_LOCKTIME_TARGET_SECONDS":      makerLockTimeSec,
		"XTAKER_LOCKTIME_TARGET_SECONDS":      takerLockTimeSec,
		"XSLOW_TAKER_LOCKTIME_TARGET_SECONDS": xSlowTakerLockTimeSec,
		"XSLOW_BLOCKTIME_SECONDS":             xSlowBlockTimeSec,
		"XLOCKTIME_DRIFT_SECONDS":             xLockTimeDriftSeconds,
		"LOCKTIME_THRESHOLD":                  lockTimeThreshold,
	}
}
