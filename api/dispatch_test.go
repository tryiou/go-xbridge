package api

import (
	"encoding/json"
	"testing"
)

func raw(s string) json.RawMessage { return json.RawMessage(s) }

func assertEnv(t *testing.T, name string, err *rpcError, wantMsg string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: expected envelope error, got nil", name)
	}
	if !err.envelope {
		t.Errorf("%s: not an envelope error: %+v", name, err)
	}
	if err.Code != -1 {
		t.Errorf("%s: code = %d, want -1", name, err.Code)
	}
	if err.Error != wantMsg {
		t.Errorf("%s: message = %q, want %q", name, err.Error, wantMsg)
	}
}

// TestStrictParamEnvelopeErrors locks in the error channel: C++ throws
// a std::runtime_error (json_spirit / UniValue) for a present-but-wrong-type or
// null param, which surfaces as a JSON-RPC envelope error (code -1). The Go
// parsers must reproduce the exact C++ message, not a business 1025 result.
func TestStrictParamEnvelopeErrors(t *testing.T) {

	// json_spirit family: "get_value< <T> > called on <V> Value".
	_, _, err := spStr([]json.RawMessage{raw(`123`)}, 0)
	assertEnv(t, "spStr int", err, "get_value< string > called on integer Value")
	_, _, err = spStr([]json.RawMessage{raw(`null`)}, 0)
	assertEnv(t, "spStr null", err, "get_value< string > called on null Value")
	_, _, err = spStr([]json.RawMessage{raw(`true`)}, 0)
	assertEnv(t, "spStr bool", err, "get_value< string > called on boolean Value")
	_, _, err = spBool([]json.RawMessage{raw(`"true"`)}, 0)
	assertEnv(t, "spBool str", err, "get_value< boolean > called on string Value")
	_, _, err = spBool([]json.RawMessage{raw(`null`)}, 0)
	assertEnv(t, "spBool null", err, "get_value< boolean > called on null Value")
	_, _, err = spInt([]json.RawMessage{raw(`"3"`)}, 0)
	assertEnv(t, "spInt str", err, "get_value< integer > called on string Value")
	_, _, err = spInt([]json.RawMessage{raw(`3.5`)}, 0)
	assertEnv(t, "spInt real", err, "get_value< integer > called on real Value")
	_, _, err = spInt([]json.RawMessage{raw(`{}`)}, 0)
	assertEnv(t, "spInt obj", err, "get_value< integer > called on Object Value")
	_, _, err = spInt([]json.RawMessage{raw(`[]`)}, 0)
	assertEnv(t, "spInt array", err, "get_value< integer > called on Array Value")
	_, _, err = spInt64([]json.RawMessage{raw(`"x"`)}, 0)
	assertEnv(t, "spInt64 str", err, "get_value< integer > called on string Value")

	// Valid values parse cleanly.
	if s, ok, err := spStr([]json.RawMessage{raw(`"btc"`)}, 0); err != nil || !ok || s != "btc" {
		t.Errorf("spStr ok = %v, %v, present=%v", s, err, ok)
	}
	if b, ok, err := spBool([]json.RawMessage{raw(`true`)}, 0); err != nil || !ok || !b {
		t.Errorf("spBool ok = %v, %v, %v", b, err, ok)
	}
	if n, ok, err := spInt([]json.RawMessage{raw(`7`)}, 0); err != nil || !ok || n != 7 {
		t.Errorf("spInt ok = %v, %v, %v", n, err, ok)
	}

	// UniValue family: "JSON value is not a X as expected".
	_, err = uvStr([]json.RawMessage{raw(`123`)}, 0)
	assertEnv(t, "uvStr int", err, "JSON value is not a string as expected")
	_, err = uvStr([]json.RawMessage{raw(`null`)}, 0)
	assertEnv(t, "uvStr null", err, "JSON value is not a string as expected")
	_, err = uvBool([]json.RawMessage{raw(`"true"`)}, 0)
	assertEnv(t, "uvBool str", err, "JSON value is not a boolean as expected")
	_, err = uvBool([]json.RawMessage{raw(`null`)}, 0)
	assertEnv(t, "uvBool null", err, "JSON value is not a boolean as expected")
	_, err = uvArr([]json.RawMessage{raw(`{}`)}, 0)
	assertEnv(t, "uvArr obj", err, "JSON value is not an array as expected")

	// uvBoolOpt: null and absent keep the default; wrong type throws.
	if v, err := uvBoolOpt([]json.RawMessage{raw(`null`)}, 0, true); err != nil || !v {
		t.Errorf("uvBoolOpt null = %v, %v; want true, nil", v, err)
	}
	if v, err := uvBoolOpt(nil, 0, true); err != nil || !v {
		t.Errorf("uvBoolOpt absent = %v, %v; want true, nil", v, err)
	}
	_, err = uvBoolOpt([]json.RawMessage{raw(`"yes"`)}, 0, true)
	assertEnv(t, "uvBoolOpt str", err, "JSON value is not a boolean as expected")
}

// TestRpcTypeCheckErrors locks in the RPCTypeCheck family (dxGetMyPartialOrderChain,
// dxPartialOrderChainDetails, dxGetTradingData): wrong-typed params throw the
// -3 RPC_TYPE_ERROR envelope with "Expected type <t>, got <name>" (server.cpp:
// 98-103), distinct from the get_*() -1 throws.
func TestRpcTypeCheckErrors(t *testing.T) {
	assertRtc := func(t *testing.T, name string, err *rpcError, wantMsg string) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s: expected envelope error, got nil", name)
		}
		if !err.envelope {
			t.Errorf("%s: not an envelope error: %+v", name, err)
		}
		if err.Code != -3 {
			t.Errorf("%s: code = %d, want -3 (RPC_TYPE_ERROR)", name, err.Code)
		}
		if err.Error != wantMsg {
			t.Errorf("%s: message = %q, want %q", name, err.Error, wantMsg)
		}
	}

	_, err := rtcStr([]json.RawMessage{raw(`123`)}, 0)
	assertRtc(t, "rtcStr number", err, "Expected type string, got number")
	_, err = rtcStr([]json.RawMessage{raw(`null`)}, 0)
	assertRtc(t, "rtcStr null", err, "Expected type string, got null")
	_, err = rtcStr([]json.RawMessage{raw(`true`)}, 0)
	assertRtc(t, "rtcStr bool", err, "Expected type string, got bool")
	_, err = rtcNum([]json.RawMessage{raw(`"43200"`)}, 0)
	assertRtc(t, "rtcNum str", err, "Expected type number, got string")
	_, err = rtcNum([]json.RawMessage{raw(`true`)}, 0)
	assertRtc(t, "rtcNum bool", err, "Expected type number, got bool")
	_, err = rtcBool([]json.RawMessage{raw(`"true"`)}, 0)
	assertRtc(t, "rtcBool str", err, "Expected type bool, got string")
	_, err = rtcBool([]json.RawMessage{raw(`123`)}, 0)
	assertRtc(t, "rtcBool number", err, "Expected type bool, got number")

	// Valid inputs parse cleanly; absent is a no-op (arity gates enforce counts).
	if s, err := rtcStr([]json.RawMessage{raw(`"abc"`)}, 0); err != nil || s != "abc" {
		t.Errorf("rtcStr ok = %q, %v", s, err)
	}
	if n, err := rtcNum([]json.RawMessage{raw(`43200`)}, 0); err != nil || n != 43200 {
		t.Errorf("rtcNum ok = %d, %v", n, err)
	}
	// A real passes RPCTypeCheck (VNUM) but the json_spirit get_int() read
	// throws -1 (rpcxbridge.cpp:2853).
	_, err = rtcNum([]json.RawMessage{raw(`3.5`)}, 0)
	assertEnv(t, "rtcNum real", err, "get_value< integer > called on real Value")
	if _, err := rtcStr(nil, 0); err != nil {
		t.Errorf("rtcStr absent = %v, want nil", err)
	}
}

func TestJsonTypeOf(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{`{}`, "Object"},
		{`[]`, "Array"},
		{`"s"`, "string"},
		{`true`, "boolean"},
		{`null`, "null"},
		{`3`, "integer"},
		{`-7`, "integer"},
		{`3.5`, "real"},
		{`1e9`, "real"},
		{`1.0E2`, "real"},
	}
	for _, c := range cases {
		if got := jsonTypeOf(raw(c.in)); got != c.want {
			t.Errorf("jsonTypeOf(%s) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestNoSessionNameIsMethodName verifies NO_SESSION errors carry the
// handler's method name (C++ __FUNCTION__), never the hardcoded "dx".
func TestNoSessionNameIsMethodName(t *testing.T) {
	ctx := &HandlerCtx{Store: NewStore(), Node: &Node{}}
	check := func(method string, call func() (interface{}, *rpcError)) {
		t.Helper()
		_, err := call()
		if err == nil || err.Code != errNoSession {
			t.Fatalf("%s: expected NO_SESSION, got %v", method, err)
		}
		if err.Name != method {
			t.Errorf("%s: NO_SESSION name = %q, want %q", method, err.Name, method)
		}
	}
	check("dxGetOrder", func() (interface{}, *rpcError) {
		o := &Order{ID: [32]byte{2}, FromCurrency: "BTC", ToCurrency: "BTC", Status: "open"}
		ctx.Store.Add(o)
		return ctx.dxGetOrder([]json.RawMessage{jstr(dispID(o.ID))})
	})
	check("dxGetUtxos", func() (interface{}, *rpcError) {
		return ctx.dxGetUtxos([]json.RawMessage{jstr("BTC")})
	})
}
