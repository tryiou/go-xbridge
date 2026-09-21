package coins

import (
	"bytes"
	"testing"

	"go-xbridge/config"
)

// TestRegistryHas pins the Has/Get membership contract: a registered ticker
// (case-insensitive) reports true, unknown tickers false. Previously only Get
// was covered (TestCoinRegistry); Has sat at 0%. The registry is re-seeded
// with BTC intact so later files keep their fixture.
func TestRegistryHas(t *testing.T) {
	if err := InitFromConf(map[string]*config.CoinConf{
		"BTC": {Ticker: "BTC", CreateTxMethod: "BTC", AddressPrefix: 0, ScriptPrefix: 5, Coin: 100000000},
	}); err != nil {
		t.Fatalf("InitFromConf: %v", err)
	}
	for _, tc := range []struct {
		ticker string
		want   bool
	}{
		{"BTC", true}, {"btc", true}, {"Btc", true},
		{"NOTACOIN", false}, {"", false},
	} {
		if got := Has(tc.ticker); got != tc.want {
			t.Errorf("Has(%q) = %v, want %v", tc.ticker, got, tc.want)
		}
		// Has must agree with Get on membership.
		_, ok := Get(tc.ticker)
		if ok != tc.want {
			t.Errorf("Get(%q) membership = %v, want %v (disagrees with Has)", tc.ticker, ok, tc.want)
		}
	}
}

// TestBuildOpReturnScript pins the OP_RETURN envelope: first byte OP_RETURN
// (0x6a), then Bitcoin's minimal push of data (empty → OP_0, ≤75 → length
// byte, 76..255 → OP_PUSHDATA1, larger → OP_PUSHDATA2/4). Previously 0%
// covered. Each case round-trips through an independent re-parse of the push.
func TestBuildOpReturnScript(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		// wantPush is the expected push-prefix after OP_RETURN.
		wantPush []byte
	}{
		{"empty", nil, []byte{Op0}},
		{"one-byte", []byte{0xab}, []byte{0x01, 0xab}},
		{"short", bytes.Repeat([]byte{0xcd}, 75), append([]byte{75}, bytes.Repeat([]byte{0xcd}, 75)...)},
		{"pushdata1", bytes.Repeat([]byte{0xef}, 76), append([]byte{OpPushData1, 76}, bytes.Repeat([]byte{0xef}, 76)...)},
		{"pushdata2", bytes.Repeat([]byte{0x11}, 300), append([]byte{OpPushData2, 44, 1}, bytes.Repeat([]byte{0x11}, 300)...)},
	}
	for _, tc := range cases {
		got := BuildOpReturnScript(tc.data)
		want := append([]byte{OpReturn}, tc.wantPush...)
		if !bytes.Equal(got, want) {
			t.Errorf("%s: script = %x, want %x", tc.name, got, want)
			continue
		}
		// Independent re-parse: strip OP_RETURN, decode the push, compare payload.
		back, rest := parsePush(t, got[1:])
		if len(rest) != 0 {
			t.Errorf("%s: trailing bytes after push: %x", tc.name, rest)
		}
		if !bytes.Equal(back, tc.data) {
			t.Errorf("%s: re-parsed payload = %x, want %x", tc.name, back, tc.data)
		}
	}
}

// parsePush decodes one minimal data push (the inverse of pushData), without
// calling it — an independent reader for the round-trip half of the test.
func parsePush(t *testing.T, s []byte) ([]byte, []byte) {
	t.Helper()
	if len(s) == 0 {
		t.Fatal("empty push")
	}
	switch op := s[0]; {
	case op == Op0:
		return nil, s[1:]
	case op <= 75:
		n := int(op)
		if len(s) < 1+n {
			t.Fatalf("short push: need %d, have %d", n, len(s)-1)
		}
		return s[1 : 1+n], s[1+n:]
	case op == OpPushData1:
		if len(s) < 2 {
			t.Fatal("truncated OP_PUSHDATA1 length")
		}
		n := int(s[1])
		if len(s) < 2+n {
			t.Fatalf("short PUSHDATA1 push: need %d, have %d", n, len(s)-2)
		}
		return s[2 : 2+n], s[2+n:]
	case op == OpPushData2:
		if len(s) < 3 {
			t.Fatal("truncated OP_PUSHDATA2 length")
		}
		n := int(s[1]) | int(s[2])<<8
		if len(s) < 3+n {
			t.Fatalf("short PUSHDATA2 push: need %d, have %d", n, len(s)-3)
		}
		return s[3 : 3+n], s[3+n:]
	default:
		t.Fatalf("unexpected push opcode %#x", op)
		return nil, nil
	}
}

// TestOpReturnGoldens pins two exact byte vectors: the empty commitment and
// a 4-byte marker, so a push-encoding regression shows as a byte diff.
func TestOpReturnGoldens(t *testing.T) {
	if got := hexOf(BuildOpReturnScript(nil)); got != "6a00" {
		t.Errorf("empty OP_RETURN = %s, want 6a00", got)
	}
	if got := hexOf(BuildOpReturnScript([]byte{0xde, 0xad, 0xbe, 0xef})); got != "6a04deadbeef" {
		t.Errorf("marker OP_RETURN = %s, want 6a04deadbeef", got)
	}
}

func hexOf(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, 2*len(b))
	for _, c := range b {
		out = append(out, digits[c>>4], digits[c&0xf])
	}
	return string(out)
}
