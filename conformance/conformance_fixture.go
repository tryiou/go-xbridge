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
// fxErrorName / fxResponseKeys are wired to a live fixture HandlerCtx in
// package api: directly-constructed Nodes (no engine/dial) with seeded stores
// and stub connectors, so the suite drives the REAL dispatch/handler paths.
// The write-command response rows (dxMakeOrder / dxMakePartialOrder /
// dxTakeOrder / dxCancelOrder) need a full hub + stub XConn + funded
// connectors to reach a SUCCESS shape; the suite does not drive those from the
// fixture (their refKeys are documented known-gap skips — the shapes are
// already covered by the api KAT tests), so the fixture covers the read-only
// methods.

func init() {
	fxXbridgeErrorText = api.XbridgeErrorText
	fxFormatXAmount = api.FormatXAmount
	fxFormatBalanceNative = api.FormatBalanceNative
	fxFormatXPrice = api.FormatXPrice
	fxISO8601 = api.ISO8601
	fxParseXAmount = api.ParseXAmount
	fxLocktimeConstants = api.LocktimeConstants
	fxOrderBookResultJSON = api.OrderBookResultJSON
	fxErrorName = api.ConformanceErrorName
	fxResponseKeys = api.ConformanceResponseKeys
}
