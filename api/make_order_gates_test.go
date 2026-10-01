package api

import (
	"encoding/json"
	"testing"
)

// dxMakeOrderGateParams returns the 7 positional dxMakeOrder params with every
// gate satisfied; each test overrides exactly the slot under test so exactly
// one gate can fire.
func dxMakeOrderGateParams() []json.RawMessage {
	return []json.RawMessage{
		jstr("BTC"), jstr("1.5"), jstr(btcAddr),
		jstr("SYS"), jstr("0.3"), jstr(btcAddr2),
		jstr("exact"),
	}
}

// TestDxMakeOrderTypeGateExactOnly: a non-"exact" order_type is rejected with
// business 1025 before MakeOrder runs (handlers.go:346-348; C++ parity
// rpcxbridge.cpp:940-943 — the only supported type for dxMakeOrder).
func TestDxMakeOrderTypeGateExactOnly(t *testing.T) {
	ctx := newWalletTestCtx()
	params := dxMakeOrderGateParams()
	params[6] = jstr("partial")

	res, rerr := ctx.dxMakeOrder(params)
	if res != nil {
		t.Errorf("result = %v, want nil", res)
	}
	requireErrCode(t, rerr, errInvalidParameters, "type=partial")
	if rerr.envelope {
		t.Error("type gate must be a business error, not an envelope error")
	}
	if want := "Invalid parameters: Only the exact type is supported at this time."; rerr.Error != want {
		t.Errorf("message = %q, want %q", rerr.Error, want)
	}
	if rerr.Name != "dxMakeOrder" {
		t.Errorf("name = %q, want dxMakeOrder", rerr.Name)
	}
}

// TestDxMakeOrderBadUseAllFundsThrow: a present-but-non-boolean use_all_funds
// (param 7) throws the UniValue boolean error as an envelope -1, mirroring
// C++ request.params[7].get_bool() (rpcxbridge.cpp:986-988).
func TestDxMakeOrderBadUseAllFundsThrow(t *testing.T) {
	ctx := newWalletTestCtx()
	params := dxMakeOrderGateParams()
	params = append(params, json.RawMessage("123")) // params[7], not a bool

	_, rerr := ctx.dxMakeOrder(params)
	assertEnv(t, "dxMakeOrder use_all_funds", rerr, "JSON value is not a boolean as expected")
}

// TestDxMakeOrderMisspelledDryrun: 9 params where params[8] is a string other
// than "dryrun" is business 1025 echoing the raw value (handlers.go:339-341;
// C++ parity rpcxbridge.cpp:994-996).
func TestDxMakeOrderMisspelledDryrun(t *testing.T) {
	ctx := newWalletTestCtx()
	params := dxMakeOrderGateParams()
	params = append(params, json.RawMessage("true")) // use_all_funds
	params = append(params, jstr("live"))            // dryrun, misspelled

	res, rerr := ctx.dxMakeOrder(params)
	if res != nil {
		t.Errorf("result = %v, want nil", res)
	}
	requireErrCode(t, rerr, errInvalidParameters, "misspelled dryrun")
	if rerr.envelope {
		t.Error("dryrun value gate must be a business error, not an envelope error")
	}
	if want := "Invalid parameters: live"; rerr.Error != want {
		t.Errorf("message = %q, want %q", rerr.Error, want)
	}
	if rerr.Name != "dxMakeOrder" {
		t.Errorf("name = %q, want dxMakeOrder", rerr.Name)
	}
}

// TestDxMakeOrderDryrunTypeThrow: 9 params where params[8] is not a string
// throws the UniValue string error as an envelope -1 (spStr handlers.go:335-338;
// C++ json_spirit get_str).
func TestDxMakeOrderDryrunTypeThrow(t *testing.T) {
	ctx := newWalletTestCtx()
	params := dxMakeOrderGateParams()
	params = append(params, json.RawMessage("true")) // use_all_funds
	params = append(params, json.RawMessage("123"))  // dryrun, not a string

	_, rerr := ctx.dxMakeOrder(params)
	assertEnv(t, "dxMakeOrder dryrun type", rerr, "get_value< string > called on integer Value")
}

// TestDxMakeOrderGateOrderDivergesFromCpp_TypeAfterOptionals pins the CURRENT
// Go gate order as a documented divergence: with type AND use_all_funds both
// invalid, Go reads use_all_funds first (handlers.go:326) and returns the
// envelope -1 boolean error, while C++ checks order_type first
// (rpcxbridge.cpp:940 before :988) and would return business 1025
// "Only the exact type is supported at this time.". Single-violation cases are
// parity-identical; this test keeps the divergence visible so any future
// alignment shows up as a red here.
func TestDxMakeOrderGateOrderDivergesFromCpp_TypeAfterOptionals(t *testing.T) {
	ctx := newWalletTestCtx()
	params := dxMakeOrderGateParams()
	params[6] = jstr("partial")                     // violates the type gate
	params = append(params, json.RawMessage("123")) // violates use_all_funds

	_, rerr := ctx.dxMakeOrder(params)
	assertEnv(t, "dxMakeOrder gate order (Go checks use_all_funds first)", rerr,
		"JSON value is not a boolean as expected")
}
