package api

import "testing"

// TestArityBusinessMethods locks in the RPC-F52 arity gates for the old-style
// dx* methods: wrong param counts return the C++ business 1025 result-error
// with the exact makeError arg (HTTP 200).
func TestArityBusinessMethods(t *testing.T) {
	cases := []struct {
		method   string
		badCount int
		msg      string
	}{
		{"dxGetNewTokenAddress", 0, "(ticker)"},
		{"dxGetNewTokenAddress", 2, "(ticker)"},
		{"dxLoadXBridgeConf", 1, "This function does not accept any parameter."},
		{"dxGetLocalTokens", 1, "This function does not accept any parameter."},
		{"dxGetNetworkTokens", 1, "This function does not accept any parameters."},
		{"dxGetOrders", 1, "This function does not accept any parameters."},
		{"dxGetOrderFills", 1, "(maker) (taker) (combined, default=true)[optional]"},
		{"dxGetOrderFills", 4, "(maker) (taker) (combined, default=true)[optional]"},
		{"dxGetOrderHistory", 4, "(maker) (taker) (start time) (end time) (granularity) (order_ids, default=false)[optional] (with_inverse, default=false)[optional] (limit, default=2147483647)[optional]"},
		{"dxGetOrderHistory", 9, "(maker) (taker) (start time) (end time) (granularity) (order_ids, default=false)[optional] (with_inverse, default=false)[optional] (limit, default=2147483647)[optional]"},
		{"dxGetOrder", 0, "(id)"},
		{"dxGetOrder", 2, "(id)"},
		{"dxCancelOrder", 0, "(id)"},
		{"dxCancelOrder", 2, "(id)"},
		{"dxGetOrderBook", 2, "(detail, 1-4) (maker) (taker) (max_orders, default=50)[optional]"},
		{"dxGetOrderBook", 5, "(detail, 1-4) (maker) (taker) (max_orders, default=50)[optional]"},
		{"dxGetTokenBalances", 1, "This function does not accept any parameters."},
		{"dxGetLockedUtxos", 2, "Too many parameters."},
		{"dxFlushCancelledOrders", 2, "ageMillis must be an integer >= 0"},
		{"dxGetMyOrders", 1, "This function does not accept any parameters."},
	}
	for _, c := range cases {
		rerr := checkArity(c.method, c.badCount)
		if rerr == nil {
			t.Errorf("%s(%d) = nil, want business 1025", c.method, c.badCount)
			continue
		}
		if rerr.envelope {
			t.Errorf("%s(%d) = envelope error, want business result error", c.method, c.badCount)
		}
		if rerr.Code != errInvalidParameters {
			t.Errorf("%s(%d) code = %d, want 1025", c.method, c.badCount, rerr.Code)
		}
		want := "Invalid parameters: " + c.msg
		if rerr.Error != want {
			t.Errorf("%s(%d) error = %q, want %q", c.method, c.badCount, rerr.Error, want)
		}
		if rerr.Name != c.method {
			t.Errorf("%s(%d) name = %q, want %q", c.method, c.badCount, rerr.Name, c.method)
		}
	}
}

// TestArityThrowMethods locks in the throw-method arity gates: wrong counts
// surface as the envelope -1 error whose message is the byte-for-byte C++
// RPCHelpMan help text.
func TestArityThrowMethods(t *testing.T) {
	cases := []struct {
		method   string
		badCount int
		help     string
	}{
		{"dxMakeOrder", 6, helpDxMakeOrder},
		{"dxMakeOrder", 1, helpDxMakeOrder},
		{"dxMakePartialOrder", 5, helpDxMakePartialOrder}, // C++ gate is <6
		{"dxTakeOrder", 2, helpDxTakeOrder},
		{"dxTakeOrder", 6, helpDxTakeOrder},
		{"dxGetMyPartialOrderChain", 0, helpDxGetMyPartialOrderChain},
		{"dxGetMyPartialOrderChain", 2, helpDxGetMyPartialOrderChain},
		{"dxPartialOrderChainDetails", 0, helpDxPartialOrderChainDetails},
		{"dxPartialOrderChainDetails", 2, helpDxPartialOrderChainDetails},
		{"dxSplitAddress", 2, helpDxSplitAddress},
		{"dxSplitAddress", 7, helpDxSplitAddress},
		{"dxSplitInputs", 2, helpDxSplitInputs},
		{"dxSplitInputs", 8, helpDxSplitInputs},
		{"dxGetUtxos", 0, helpDxGetUtxos},
		{"dxGetUtxos", 3, helpDxGetUtxos},
		{"dxGetTradingData", 3, helpDxGetTradingData},
	}
	for _, c := range cases {
		rerr := checkArity(c.method, c.badCount)
		if rerr == nil {
			t.Errorf("%s(%d) = nil, want envelope -1", c.method, c.badCount)
			continue
		}
		if !rerr.envelope {
			t.Errorf("%s(%d) = business error, want envelope -1", c.method, c.badCount)
		}
		if rerr.Code != -1 {
			t.Errorf("%s(%d) code = %d, want -1", c.method, c.badCount, rerr.Code)
		}
		if rerr.Error != c.help {
			t.Errorf("%s(%d): envelope message != help constant", c.method, c.badCount)
		}
	}
}

// TestArityInRange verifies the valid counts pass (no gate).
func TestArityInRange(t *testing.T) {
	valid := [][2]string{
		{"dxGetNewTokenAddress", "1"}, {"dxGetOrderFills", "2"}, {"dxGetOrderFills", "3"},
		{"dxGetOrderHistory", "5"}, {"dxGetOrderHistory", "8"}, {"dxGetOrder", "1"},
		{"dxGetOrderBook", "3"}, {"dxGetOrderBook", "4"}, {"dxGetLockedUtxos", "0"},
		{"dxGetLockedUtxos", "1"}, {"dxFlushCancelledOrders", "1"}, {"dxCancelOrder", "1"},
		{"dxMakeOrder", "7"}, {"dxMakeOrder", "9"}, {"dxMakeOrder", "12"}, // extras ignored
		{"dxMakePartialOrder", "6"}, {"dxMakePartialOrder", "11"}, {"dxTakeOrder", "3"}, {"dxTakeOrder", "5"},
		{"dxGetMyPartialOrderChain", "1"}, {"dxPartialOrderChainDetails", "1"},
		{"dxSplitAddress", "3"}, {"dxSplitAddress", "6"}, {"dxSplitInputs", "3"}, {"dxSplitInputs", "7"},
		{"dxGetUtxos", "1"}, {"dxGetUtxos", "2"}, {"dxGetTradingData", "0"}, {"dxGetTradingData", "2"},
		{"dxGetMyOrders", "0"}, {"dxGetOrders", "0"}, {"dxGetTokenBalances", "0"}, {"dxLoadXBridgeConf", "0"},
	}
	for _, c := range valid {
		if rerr := checkArity(c[0], atoi(c[1])); rerr != nil {
			t.Errorf("%s(%s) = %v, want no gate", c[0], c[1], rerr)
		}
	}
	// A method with no registry entry is never gated.
	if rerr := checkArity("getnetworkinfo", 3); rerr != nil {
		t.Errorf("getnetworkinfo(3) = %v, want nil (Go shim owns its gate)", rerr)
	}
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}

// TestHelpTextConstants pins the transcription: each constant is non-empty,
// starts with the method name on the first line, and carries the C++ sections.
func TestHelpTextConstants(t *testing.T) {
	for method, help := range map[string]string{
		"dxMakeOrder": helpDxMakeOrder, "dxMakePartialOrder": helpDxMakePartialOrder,
		"dxTakeOrder": helpDxTakeOrder, "dxGetMyPartialOrderChain": helpDxGetMyPartialOrderChain,
		"dxPartialOrderChainDetails": helpDxPartialOrderChainDetails, "dxSplitAddress": helpDxSplitAddress,
		"dxSplitInputs": helpDxSplitInputs, "dxGetUtxos": helpDxGetUtxos,
		"dxGetTradingData": helpDxGetTradingData,
	} {
		if !hasPrefix(help, method+" ") {
			t.Errorf("%s help must start with the method name + oneline args", method)
		}
		if !contains(help, "\nExamples:\n> blocknet-cli "+method) {
			t.Errorf("%s help must carry the Examples section with blocknet-cli lines", method)
		}
	}
}

func hasPrefix(s, p string) bool { return len(s) >= len(p) && s[:len(p)] == p }
func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
