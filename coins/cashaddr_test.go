package coins

import (
	"encoding/hex"
	"strings"
	"testing"

	"xbridge-go/config"
)

// TestCashAddrRoundTripP2KH encodes a 20-byte hash as a P2KH CashAddr and
// decodes it back, asserting the type and hash survive and the address uses the
// BCH "q" P2KH prefix.
func TestCashAddrRoundTripP2KH(t *testing.T) {
	hash, _ := hex.DecodeString("F5BF48B397DAE70BE82B3CCA2CEE1A261CE18FE9")
	addr, err := cashaddrEncode("bitcoincash", 0, hash)
	if err != nil {
		t.Fatalf("cashaddrEncode: %v", err)
	}
	if !strings.HasPrefix(addr, "bitcoincash:q") {
		t.Fatalf("P2KH cashaddr should start with 'bitcoincash:q', got %q", addr)
	}
	typ, got, err := cashaddrDecode(addr, "bitcoincash")
	if err != nil {
		t.Fatalf("cashaddrDecode: %v", err)
	}
	if typ != 0 {
		t.Fatalf("type = %d, want 0 (P2KH)", typ)
	}
	if hex.EncodeToString(got) != hex.EncodeToString(hash) {
		t.Fatalf("hash = %x, want %x", got, hash)
	}
}

// TestCashAddrRoundTripP2SH encodes the same hash as a P2SH CashAddr; it must
// differ from the P2KH form (distinct version byte) and decode back to type 1.
func TestCashAddrRoundTripP2SH(t *testing.T) {
	hash, _ := hex.DecodeString("F5BF48B397DAE70BE82B3CCA2CEE1A261CE18FE9")
	p2kh, err := cashaddrEncode("bitcoincash", 0, hash)
	if err != nil {
		t.Fatal(err)
	}
	p2sh, err := cashaddrEncode("bitcoincash", 1, hash)
	if err != nil {
		t.Fatal(err)
	}
	if p2kh == p2sh {
		t.Fatalf("P2KH and P2SH cashaddrs must differ (got %q for both)", p2kh)
	}
	if !strings.HasPrefix(p2sh, "bitcoincash:p") {
		t.Fatalf("P2SH cashaddr should start with 'bitcoincash:p', got %q", p2sh)
	}
	typ, got, err := cashaddrDecode(p2sh, "bitcoincash")
	if err != nil {
		t.Fatalf("cashaddrDecode: %v", err)
	}
	if typ != 1 {
		t.Fatalf("type = %d, want 1 (P2SH)", typ)
	}
	if hex.EncodeToString(got) != hex.EncodeToString(hash) {
		t.Fatalf("hash = %x, want %x", got, hash)
	}
}

// TestCashAddrWrongPrefix is rejected.
func TestCashAddrWrongPrefix(t *testing.T) {
	hash, _ := hex.DecodeString("F5BF48B397DAE70BE82B3CCA2CEE1A261CE18FE9")
	addr, _ := cashaddrEncode("bitcoincash", 0, hash)
	if _, _, err := cashaddrDecode(addr, "bchtest"); err == nil {
		t.Fatal("expected prefix mismatch error")
	}
}

// TestCashAddrChecksumMismatch is rejected (flip the last symbol).
func TestCashAddrChecksumMismatch(t *testing.T) {
	hash, _ := hex.DecodeString("F5BF48B397DAE70BE82B3CCA2CEE1A261CE18FE9")
	addr, _ := cashaddrEncode("bitcoincash", 0, hash)
	bad := addr[:len(addr)-1] + "x"
	if _, _, err := cashaddrDecode(bad, "bitcoincash"); err == nil {
		t.Fatal("expected checksum mismatch error")
	}
}

// TestCashAddrMixedCase is rejected (CashAddr is lowercase-only).
func TestCashAddrMixedCase(t *testing.T) {
	hash, _ := hex.DecodeString("F5BF48B397DAE70BE82B3CCA2CEE1A261CE18FE9")
	addr, _ := cashaddrEncode("bitcoincash", 0, hash)
	mixed := strings.ToUpper(addr[:1]) + addr[1:]
	if _, _, err := cashaddrDecode(mixed, "bitcoincash"); err == nil {
		t.Fatal("expected mixed-case error")
	}
}

// TestBCHCoinCashAddr wires the CashAddr codec through the Coin API: a BCH coin
// round-trips a hash through DecodeAddress/Encode (String), and the P2KH address
// yields the correct 20-byte id.
func TestBCHCoinCashAddr(t *testing.T) {
	c, err := FromConf(&config.CoinConf{
		Ticker: "BCH", Title: "Bitcoin Cash", CreateTxMethod: "BCH",
		AddressPrefix: 0, ScriptPrefix: 5, Coin: 100000000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.Family() != FamilyUTXOBCH {
		t.Fatalf("family = %q, want %q", c.Family(), FamilyUTXOBCH)
	}
	if c.CashAddrPrefix != "bitcoincash" {
		t.Fatalf("cashaddr prefix = %q, want bitcoincash", c.CashAddrPrefix)
	}
	hash, _ := hex.DecodeString("F5BF48B397DAE70BE82B3CCA2CEE1A261CE18FE9")
	addr, err := cashaddrEncode("bitcoincash", 0, hash)
	if err != nil {
		t.Fatal(err)
	}
	a, err := c.DecodeAddress(addr)
	if err != nil {
		t.Fatalf("DecodeAddress: %v", err)
	}
	if a.Kind != P2PKH {
		t.Fatalf("kind = %v, want P2PKH", a.Kind)
	}
	if a.String() != addr {
		t.Fatalf("String() = %q, want %q", a.String(), addr)
	}
	id, ok := a.ID()
	if !ok {
		t.Fatal("BCH P2KH address has no 20-byte id")
	}
	if hex.EncodeToString(id[:]) != hex.EncodeToString(hash) {
		t.Fatalf("id = %x, want %x", id, hash)
	}
}

// TestDGBCoinConf confirms Digibyte chainparams: it is a BTC-family UTXO chain
// with native segwit (HRP "dgb"), matching C++'s DGB connector. Resolves the
// [VERIFY] flag on DGB chainparams in the audit — these values mirror
// xbridgewalletconnectordgb.cpp.
func TestDGBCoinConf(t *testing.T) {
	c, err := FromConf(&config.CoinConf{
		Ticker: "DGB", Title: "Digibyte", CreateTxMethod: "DGB",
		AddressPrefix: 30, ScriptPrefix: 63, Coin: 100000000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.Family() != FamilyUTXOBTC {
		t.Fatalf("family = %q, want %q", c.Family(), FamilyUTXOBTC)
	}
	if !c.SegWit {
		t.Fatal("DGB should report segwit support")
	}
	if c.Bech32HRP != "dgb" {
		t.Fatalf("bech32 HRP = %q, want dgb", c.Bech32HRP)
	}
}
