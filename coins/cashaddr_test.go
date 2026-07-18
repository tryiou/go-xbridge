package coins

import (
	"encoding/hex"
	"strings"
	"testing"

	"go-xbridge/config"
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

// TestCashAddrABCVectors verifies the official Bitcoin ABC cashaddr test vectors
// at the checksum/symbol level: each must pass cashaddrVerifyChecksum and
// re-encode byte-for-byte. This is the definitive C1 regression — the previous
// port truncated the checksum generator to its low byte and packed (len>>2)
// into the version byte, so it validated only against itself and rejected every
// genuine BCH address. These vectors are from
// bitcoin-abc/src/test/cashaddr_tests.cpp. They carry arbitrary (non-address)
// payloads, so they are checked at the symbol level rather than through the
// structural cashaddrDecode (which enforces version+standard-hash shape).
func TestCashAddrABCVectors(t *testing.T) {
	vectors := []string{
		"bitcoincash:qpzry9x8gf2tvdw0s3jn54khce6mua7lcw20ayyn",
		"bchtest:testnetaddress4d6njnut",
		"bchreg:555555555555555555555555555555555555555555555udxmlmrz",
	}
	for _, v := range vectors {
		pos := strings.Index(v, ":")
		prefix := v[:pos]
		syms := make([]int, 0)
		for i := pos + 1; i < len(v); i++ {
			syms = append(syms, strings.IndexByte(bech32Charset, v[i]))
		}
		if !cashaddrVerifyChecksum(prefix, syms) {
			t.Fatalf("cashaddrVerifyChecksum(%q) = false, want true", v)
		}
		// Re-encode: checksum from the payload (symbols minus the 8 checksum syms).
		payload := syms[:len(syms)-8]
		cs := cashaddrCreateChecksum(prefix, payload)
		full := append(append([]int{}, payload...), cs...)
		var sb strings.Builder
		sb.WriteString(prefix)
		sb.WriteByte(':')
		for _, s := range full {
			sb.WriteByte(bech32Charset[s])
		}
		if !strings.EqualFold(sb.String(), v) {
			t.Fatalf("round-trip %q -> re-encode %q", v, sb.String())
		}
	}
}

// TestCashAddrDecodeRealVector decodes a genuine mainnet BCH P2KH address and
// round-trips it. Confirms the version byte (C++ PackAddrData size index) and
// checksum now match Bitcoin ABC, so real BCH addresses are accepted.
func TestCashAddrDecodeRealVector(t *testing.T) {
	// Encoded with the corrected generator for the 20-byte P2KH hash
	// F5BF48B397DAE70BE82B3CCA2CEE1A261CE18FE9 (matches Bitcoin ABC output).
	const addr = "bitcoincash:qr6m7j9njldwwzlg9v7v5t8wrgnpecv0ayz9tlf4ch"
	const wantHash = "f5bf48b397dae70be82b3cca2cee1a261ce18fe9"
	typ, got, err := cashaddrDecode(addr, "bitcoincash")
	if err != nil {
		t.Fatalf("cashaddrDecode(%q): %v", addr, err)
	}
	if typ != 0 {
		t.Fatalf("type = %d, want 0 (P2KH)", typ)
	}
	if hex.EncodeToString(got) != wantHash {
		t.Fatalf("hash = %x, want %s", got, wantHash)
	}
	re, err := cashaddrEncode("bitcoincash", 0, got)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(re, addr) {
		t.Fatalf("re-encode = %q, want %q", re, addr)
	}
}

// TestCashAddrP2SHRealPrefix checks the P2SH form uses the "p" prefix and decodes
// back to type 1 with the same 20-byte hash.
func TestCashAddrP2SHRealPrefix(t *testing.T) {
	hash, _ := hex.DecodeString("F5BF48B397DAE70BE82B3CCA2CEE1A261CE18FE9")
	p2sh, err := cashaddrEncode("bitcoincash", 1, hash)
	if err != nil {
		t.Fatal(err)
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

// TestCashAddrMultiSize exercises the other three hash sizes Bitcoin ABC's
// PackAddrData supports (24/28/32 bytes), which the previous port rejected.
func TestCashAddrMultiSize(t *testing.T) {
	sizes := []int{24, 28, 32}
	for _, n := range sizes {
		h := make([]byte, n)
		for i := range h {
			h[i] = byte(i)
		}
		for _, typ := range []int{0, 1} {
			addr, err := cashaddrEncode("bitcoincash", typ, h)
			if err != nil {
				t.Fatalf("encode size %d type %d: %v", n, typ, err)
			}
			gotTyp, got, err := cashaddrDecode(addr, "bitcoincash")
			if err != nil {
				t.Fatalf("decode size %d type %d: %v", n, typ, err)
			}
			if gotTyp != typ {
				t.Fatalf("size %d: type = %d, want %d", n, gotTyp, typ)
			}
			if len(got) != n || hex.EncodeToString(got) != hex.EncodeToString(h) {
				t.Fatalf("size %d: hash = %x, want %x", n, got, h)
			}
		}
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
