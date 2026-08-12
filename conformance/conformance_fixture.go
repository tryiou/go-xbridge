//go:build conformance

package conformance_test

import (
	"go-xbridge/api"
)

// Fixture wiring for the conformance suite's fx* hooks.
//
// The suite lives in an external module (go-xbridge/conformance) so the
// exported packages (proto, p2p, crypto, swap, servicenode) are exercised
// directly. The fx* hooks that need go-xbridge/api helpers are wired here to
// the build-tagged re-exports in go-xbridge/api/export_conformance.go (which
// exists only under `-tags conformance`).
//
// fxErrorName / fxResponseKeys are NOT wired: they require a live api.Handler
// with a fixture store + connectors to drive business-error paths and render
// response objects. Until such a fixture harness exists, the tests that use
// them (TestRPCMethodNameField, TestRPCResponseShape) t.Skip — the reference
// vectors they encode remain complete in this file.

func init() {
	fxXbridgeErrorText = api.XbridgeErrorText
	fxFormatXAmount = api.FormatXAmount
	fxFormatBalanceNative = api.FormatBalanceNative
	fxFormatXPrice = api.FormatXPrice
	fxISO8601 = api.ISO8601
	fxParseXAmount = api.ParseXAmount
	fxLocktimeConstants = api.LocktimeConstants
}
