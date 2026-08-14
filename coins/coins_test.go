package coins

import (
	"encoding/hex"
	"os"
	"strings"
	"testing"

	"go-xbridge/config"
)

// TestMain seeds the coin registry from a fixture xbridge.conf so the existing
// codec tests (which call MustGet) run against conf-derived coins rather than a
// hardcoded map.
func TestMain(m *testing.M) {
	f, err := os.CreateTemp("", "xbridge-*.conf")
	if err != nil {
		panic(err)
	}
	defer os.Remove(f.Name())
	body := `[Main]
ExchangeWallets=BTC,BLOCK,DOGE

[BTC]
Title=Bitcoin
CreateTxMethod=BTC
AddressPrefix=0
ScriptPrefix=5
SecretPrefix=128
COIN=100000000
TxVersion=1

[BLOCK]
Title=Blocknet
CreateTxMethod=BLOCK
AddressPrefix=26
ScriptPrefix=28
SecretPrefix=154
COIN=100000000

[DOGE]
Title=Dogecoin
CreateTxMethod=DOGE
AddressPrefix=30
ScriptPrefix=22
SecretPrefix=158
COIN=100000000
`
	if _, err := f.WriteString(body); err != nil {
		panic(err)
	}
	f.Close()
	conf, err := config.Load(f.Name())
	if err != nil {
		panic(err)
	}
	if err := InitFromConf(conf.Coins); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

func TestBase58RoundTrip(t *testing.T) {
	samples := []string{
		"", "00", "00010203", "ff", "deadbeef",
		"0000000000000000000000000000000000000000000000000000000000000000",
	}
	for _, s := range samples {
		raw, _ := hex.DecodeString(s)
		enc := base58Encode(raw)
		dec, err := base58Decode(enc)
		if err != nil {
			t.Fatalf("decode %q: %v", enc, err)
		}
		if hex.EncodeToString(dec) != s {
			t.Errorf("round-trip mismatch: %q -> %q -> %q", s, enc, hex.EncodeToString(dec))
		}
	}
}

func TestBase58CheckRejectsBadChecksum(t *testing.T) {
	// A valid-looking BTC P2PKH address with its final char changed.
	if _, _, err := base58CheckDecode("1BvBMSEYstWetqTFn5Au4m4GFg7xJaNVN3"); err == nil {
		t.Error("expected checksum error for tampered address")
	}
}

func TestBech32Vector(t *testing.T) {
	// BIP173 test vector: BC1QW508D6QEJXTDG4Y5R3ZARVARY0C5XW7KV8F3T4
	addr := "BC1QW508D6QEJXTDG4Y5R3ZARVARY0C5XW7KV8F3T4"
	hrp, ver, prog, err := bech32Decode(addr)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if hrp != "bc" || ver != 0 {
		t.Fatalf("hrp=%q ver=%d, want bc/0", hrp, ver)
	}
	if got := hex.EncodeToString(prog); got != "751e76e8199196d454941c45d1b3a323f1433bd6" {
		t.Fatalf("program = %s", got)
	}
	// Re-encode and compare (case-insensitive).
	re, err := bech32Encode(hrp, ver, prog)
	if err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	if !strings.EqualFold(re, addr) {
		t.Errorf("re-encode = %s, want %s", re, addr)
	}
}

func TestParseAmount(t *testing.T) {
	c := MustGet("BTC")
	cases := []struct {
		in   string
		want uint64
	}{
		{"1", 100000000},
		{"1.5", 150000000},
		{"0.00000001", 1},
		{"21", 2100000000},
		{"0", 0},
	}
	for _, tc := range cases {
		got, err := ParseAmount(c, tc.in)
		if err != nil {
			t.Fatalf("ParseAmount(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("ParseAmount(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
	if _, err := ParseAmount(c, "1.123456789"); err == nil {
		t.Error("expected error for too many decimals")
	}
	if _, err := ParseAmount(c, "999999999999999999999"); err == nil {
		t.Error("expected overflow error")
	}
}

func TestFormatAmount(t *testing.T) {
	c := MustGet("BTC")
	cases := []struct {
		in   uint64
		want string
	}{
		{100000000, "1"},
		{150000000, "1.5"},
		{1, "0.00000001"},
		{0, "0"},
		{2100000000000000, "21000000"},
	}
	for _, tc := range cases {
		if got := FormatAmount(c, tc.in); got != tc.want {
			t.Errorf("FormatAmount(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
	// Round-trip Parse->Format->Parse stability.
	for _, v := range []uint64{1, 12345678, 2100000000000000, 99999999} {
		s := FormatAmount(c, v)
		got, err := ParseAmount(c, s)
		if err != nil {
			t.Fatalf("re-parse %q: %v", s, err)
		}
		if got != v {
			t.Errorf("round-trip %d -> %q -> %d", v, s, got)
		}
	}
}

func TestDecodeAddressLegacy(t *testing.T) {
	btc := MustGet("BTC")
	// Satoshi genesis address (well-known P2PKH vector).
	a, err := btc.DecodeAddress("1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa")
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if a.Kind != P2PKH {
		t.Fatalf("kind = %v, want P2PKH", a.Kind)
	}
	if hex.EncodeToString(a.Hash) != "62e907b15cbf27d5425399ebf6f0fb50ebb88f18" {
		t.Fatalf("hash160 = %s", hex.EncodeToString(a.Hash))
	}
	if a.String() != "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa" {
		t.Errorf("re-encode = %s", a.String())
	}

	// Round-trip for BLOCK (legacy only) and DOGE.
	for _, tk := range []string{"BLOCK", "DOGE"} {
		c := MustGet(tk)
		orig := base58CheckEncode(c.P2PKH, []byte{0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff, 0x00, 0x11, 0x22, 0x33, 0x44})
		addr, err := c.DecodeAddress(orig)
		if err != nil {
			t.Fatalf("%s decode: %v", tk, err)
		}
		if addr.String() != orig {
			t.Errorf("%s round-trip: %s != %s", tk, addr.String(), orig)
		}
	}

	// Wrong version byte for the coin must error.
	btcWrong := base58CheckEncode(0x1a /* BLOCK P2PKH */, []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20})
	if _, err := btc.DecodeAddress(btcWrong); err == nil {
		t.Error("expected version-byte mismatch error")
	}
}

func TestDecodeAddressSegwit(t *testing.T) {
	btc := MustGet("BTC")
	a, err := btc.DecodeAddress("BC1QW508D6QEJXTDG4Y5R3ZARVARY0C5XW7KV8F3T4")
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if a.Kind != P2WPKH || hex.EncodeToString(a.Hash) != "751e76e8199196d454941c45d1b3a323f1433bd6" {
		t.Fatalf("bad decode: %+v", a)
	}
	if a.String() != "bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4" {
		t.Errorf("re-encode = %s", a.String())
	}
	if _, ok := a.ID(); !ok {
		t.Error("P2WPKH should yield a 20-byte ID")
	}
}

func TestCoinRegistry(t *testing.T) {
	if _, ok := Get("btc"); !ok {
		t.Error("case-insensitive lookup of BTC failed")
	}
	if _, ok := Get("NOTACOIN"); ok {
		t.Error("unknown coin should not be found")
	}
}

// TestFormatAmountFixed locks the C++ xBridgeStringValueFromPrice(amount, COIN)
// contract (xutil.cpp:216-221): base units rendered with exactly the coin's
// Decimals fractional digits, no trailing-zero trimming.
func TestFormatAmountFixed(t *testing.T) {
	btc := Coin{Decimals: 8}
	six := Coin{Decimals: 6}
	cases := []struct {
		c    Coin
		v    uint64
		want string
	}{
		{btc, 100000000, "1.00000000"},
		{btc, 150000000, "1.50000000"},
		{btc, 1, "0.00000001"},
		{btc, 0, "0.00000000"},
		{six, 100000000, "100.000000"},
		{six, 1000000, "1.000000"},
		{six, 0, "0.000000"},
	}
	for _, c := range cases {
		if got := FormatAmountFixed(c.c, c.v); got != c.want {
			t.Errorf("FormatAmountFixed(%v) = %q, want %q", c.v, got, c.want)
		}
	}
	// The trimmed formatter must be unaffected.
	if got := FormatAmount(btc, 100000000); got != "1" {
		t.Errorf("FormatAmount(1 BTC) = %q, want \"1\"", got)
	}
}
