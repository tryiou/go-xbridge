package coins

import (
	"bytes"
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
	defer func() { _ = os.Remove(f.Name()) }()
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
	_ = f.Close()
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

// TestBase58DecodeLengthCap locks in the hardening bound on base58Decode input:
// the big-int decode is O(n²), so an oversized wire-controlled string must be
// rejected up front rather than amplified. maxBase58Len (128) is well above any
// real base58check address (~46 chars) while capping the quadratic cost.
func TestBase58DecodeLengthCap(t *testing.T) {
	// 129 leading '1's — one past maxBase58Len. Even though every char is a
	// valid base58 digit and '1's are the cheapest case, the length cap must
	// fire before any decode work.
	if _, err := base58Decode(strings.Repeat("1", maxBase58Len+1)); err == nil {
		t.Fatalf("base58Decode(%d chars) succeeded, want length error", maxBase58Len+1)
	}
	// Exactly at the cap is still accepted (the boundary is inclusive).
	if _, err := base58Decode(strings.Repeat("1", maxBase58Len)); err != nil {
		t.Fatalf("base58Decode(%d chars) errored: %v", maxBase58Len, err)
	}
	// A real P2PKH address decodes fine (well under the cap).
	if _, err := base58Decode("1BvBMSEYstWetqTFn5Au4m4GFg7xJaNVN2"); err != nil {
		t.Fatalf("base58Decode(real address) errored: %v", err)
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

// TestCoinSignatureDescriptor pins the per-coin descriptors derived from
// CreateTxMethod: the BCH-family coins sign with the forkid
// digest (fork value 0), BTG with fork value 79, plain BTC-family coins legacy.
// The tables mirror the C++ connector classes, not hardcoded coin values.
func TestCoinSignatureDescriptor(t *testing.T) {
	cases := []struct {
		method     string
		family     FamilyKind
		kind       SignatureKind
		forkValue  uint32
		cashPrefix string
		bech32HRP  string
		segwit     bool
	}{
		{"BTC", FamilyUTXOBTC, SigLegacy, 0, "", "bc", true},
		{"BLOCK", FamilyUTXOBTC, SigLegacy, 0, "", "", false},
		{"DOGE", FamilyUTXOBTC, SigLegacy, 0, "", "", false},
		// BCH commits fork value 0xffdead (live mainnet replay protection,
		// bch.cpp:203-209,497-499); DEVAULT disables it (devault.cpp:171).
		{"BCH", FamilyUTXOBCH, SigForkID, 0xffdead, "bitcoincash", "", false},
		{"DEVAULT", FamilyUTXOBCH, SigForkID, 0, "devault", "", false},
		{"BTG", FamilyUTXOBTC, SigForkID, 79, "", "btg", true},
		{"PART", FamilyUTXOBTC, SigLegacy, 0, "", "", false},
	}
	for _, c := range cases {
		coin, err := FromConf(&config.CoinConf{
			Ticker:         "T",
			Title:          c.method,
			CreateTxMethod: c.method,
			AddressPrefix:  0,
			ScriptPrefix:   5,
			Coin:           100000000,
		})
		if err != nil {
			t.Fatalf("FromConf(%s): %v", c.method, err)
		}
		if coin.Family() != c.family {
			t.Errorf("%s: family = %q, want %q", c.method, coin.Family(), c.family)
		}
		if coin.SignatureKind() != c.kind {
			t.Errorf("%s: signature kind = %v, want %v", c.method, coin.SignatureKind(), c.kind)
		}
		if coin.ForkValue() != c.forkValue {
			t.Errorf("%s: fork value = %d, want %d", c.method, coin.ForkValue(), c.forkValue)
		}
		if coin.CashAddrPrefix != c.cashPrefix {
			t.Errorf("%s: cashaddr prefix = %q, want %q", c.method, coin.CashAddrPrefix, c.cashPrefix)
		}
		if coin.Bech32HRP != c.bech32HRP {
			t.Errorf("%s: bech32 HRP = %q, want %q", c.method, coin.Bech32HRP, c.bech32HRP)
		}
		if coin.SegWit != c.segwit {
			t.Errorf("%s: segwit = %v, want %v", c.method, coin.SegWit, c.segwit)
		}
	}
}

// twenty returns a fixed 20-byte identifier.
func twenty() []byte {
	return []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a,
		0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10, 0x11, 0x12, 0x13, 0x14}
}

// TestBTGAddressRoundTrip verifies the BTG connector is a forkid
// coin with native segwit (bech32 HRP "btg"): base58check P2PKH/P2SH (version
// bytes 38/23, as in the manifest bitcoingold conf) and bech32 segwit
// addresses all round-trip through encode/decode.
func TestBTGAddressRoundTrip(t *testing.T) {
	btg, err := FromConf(&config.CoinConf{
		Ticker: "BTG", Title: "BitcoinGold", CreateTxMethod: "BTG",
		AddressPrefix: 38, ScriptPrefix: 23, Coin: 100000000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if btg.SignatureKind() != SigForkID || btg.ForkValue() != 79 {
		t.Fatalf("BTG signing descriptor = kind %v fork %d, want SigForkID/79", btg.SignatureKind(), btg.ForkValue())
	}
	if !btg.SegWit || btg.Bech32HRP != "btg" {
		t.Fatalf("BTG segwit = %v HRP %q, want true/btg", btg.SegWit, btg.Bech32HRP)
	}

	p2pkh := Address{Coin: btg, Kind: P2PKH, Prefix: 38, Hash: twenty()}.String()
	a, err := btg.DecodeAddress(p2pkh)
	if err != nil || a.Kind != P2PKH || !bytes.Equal(a.Hash, twenty()) {
		t.Fatalf("BTG P2PKH round-trip failed: err=%v kind=%v addr=%q", err, a.Kind, p2pkh)
	}

	p2sh := Address{Coin: btg, Kind: P2SH, Prefix: 23, Hash: twenty()}.String()
	a, err = btg.DecodeAddress(p2sh)
	if err != nil || a.Kind != P2SH || !bytes.Equal(a.Hash, twenty()) {
		t.Fatalf("BTG P2SH round-trip failed: err=%v kind=%v addr=%q", err, a.Kind, p2sh)
	}

	// The version-byte literals above deliberately mirror the manifest
	// bitcoingold conf (AddressPrefix=38, ScriptPrefix=23); keep them literal so
	// a P2PKH<->P2SH swap in FromConf cannot be masked by derived literals.
	wit := Address{Coin: btg, Kind: P2WPKH, Hash: twenty(), WitnessVersion: 0}.String()
	if !strings.HasPrefix(wit, "btg1") {
		t.Fatalf("BTG segwit address %q not bech32 with btg HRP", wit)
	}
	a, err = btg.DecodeAddress(wit)
	if err != nil || a.Kind != P2WPKH || !bytes.Equal(a.Hash, twenty()) {
		t.Fatalf("BTG bech32 round-trip failed: err=%v kind=%v addr=%q", err, a.Kind, wit)
	}
}

// TestDevaultAddressCashaddr verifies the DEVAULT connector is
// classified as the BCH family with cashaddr HRP "devault" (devault.cpp:274-280)
// and fork value 0 (replay protection disabled, devault.cpp:171): a cashaddr
// address round-trips and legacy base58check input is rejected (the documented
// cashaddr-only hardening — the BCH-family decoder is cashaddr-only).
func TestDevaultAddressCashaddr(t *testing.T) {
	dev, err := FromConf(&config.CoinConf{
		Ticker: "DVT", Title: "DeVault", CreateTxMethod: "DEVAULT",
		AddressPrefix: 0, ScriptPrefix: 5, Coin: 100000000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if dev.Family() != FamilyUTXOBCH {
		t.Fatalf("DEVAULT family = %q, want %q", dev.Family(), FamilyUTXOBCH)
	}
	if dev.SignatureKind() != SigForkID || dev.ForkValue() != 0 {
		t.Fatalf("DEVAULT signing descriptor = kind %v fork %d, want SigForkID/0", dev.SignatureKind(), dev.ForkValue())
	}

	addr := Address{Coin: dev, Kind: P2PKH, Hash: twenty()}.String()
	if !strings.HasPrefix(addr, "devault:") {
		t.Fatalf("DEVAULT address %q not cashaddr with devault HRP", addr)
	}
	a, err := dev.DecodeAddress(addr)
	if err != nil || a.Kind != P2PKH || !bytes.Equal(a.Hash, twenty()) {
		t.Fatalf("DEVAULT cashaddr round-trip failed: err=%v kind=%v addr=%q", err, a.Kind, addr)
	}
	// Legacy base58check input is rejected — the BCH-family decoder is
	// cashaddr-only (documented hardening).
	if _, err := dev.DecodeAddress(base58CheckEncode(0, twenty())); err == nil {
		t.Error("DEVAULT accepted a legacy base58check address (cashaddr-only hardening)")
	}
}
