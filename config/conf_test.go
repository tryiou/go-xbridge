package config

import (
	"os"
	"path/filepath"
	"testing"
)

const sampleConf = `
[Main]
ExchangeWallets=BTC,DOGE,BLOCK
ShowAllOrders=1
FullLog=0

[BTC]
Title=Bitcoin
Address=
Ip=127.0.0.1
Port=8332
Username=bitcoinrpc
Password=secret
CreateTxMethod=BTC
AddressPrefix=0
ScriptPrefix=5
SecretPrefix=128
COIN=100000000
TxVersion=1
DustAmount=546
MinTxFee=1000
BlockTime=600
FeePerByte=2
Confirmations=2
JSONVersion=1.0
ContentType=application/json
CashAddrPrefix=

[BCH]
Title=Bitcoin Cash
Address=
Ip=127.0.0.1
Port=8332
Username=bchrpc
Password=bchsecret
CreateTxMethod=BCH
AddressPrefix=0
ScriptPrefix=5
SecretPrefix=128
COIN=100000000
TxVersion=2
DustAmount=546
MinTxFee=1000
BlockTime=600
FeePerByte=2
Confirmations=2
JSONVersion=1.0
ContentType=application/json
CashAddrPrefix=bitcoincash

[BLOCK]
Title=Blocknet
Ip=127.0.0.1
Port=41414
Username=blockrpc
Password=blocksecret
CreateTxMethod=BLOCK
AddressPrefix=26
ScriptPrefix=28
SecretPrefix=154
COIN=100000000
TxVersion=1
DustAmount=1000
BlockTime=60
FeePerByte=20
Confirmations=10
OmitJSONVersion=1
`

func writeConf(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "xbridge.conf")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write conf: %v", err)
	}
	return p
}

func TestLoadSample(t *testing.T) {
	p := writeConf(t, sampleConf)
	conf, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if len(conf.Main.ExchangeWallets) != 3 ||
		conf.Main.ExchangeWallets[0] != "BTC" ||
		conf.Main.ExchangeWallets[1] != "DOGE" ||
		conf.Main.ExchangeWallets[2] != "BLOCK" {
		t.Errorf("ExchangeWallets = %v", conf.Main.ExchangeWallets)
	}
	if !conf.Main.ShowAllOrders {
		t.Error("ShowAllOrders should be true")
	}

	btc, ok := conf.Coins["BTC"]
	if !ok {
		t.Fatal("BTC section missing")
	}
	if btc.Port != 8332 || btc.Coin != 100000000 || btc.CreateTxMethod != "BTC" {
		t.Errorf("BTC conf wrong: %+v", btc)
	}

	// BLOCK is configured exactly like any other coin (no special-casing).
	block, ok := conf.Coins["BLOCK"]
	if !ok {
		t.Fatal("BLOCK section missing")
	}
	if block.Port != 41414 || block.CreateTxMethod != "BLOCK" || block.Ip != "127.0.0.1" {
		t.Errorf("BLOCK conf wrong: %+v", block)
	}
	if !block.OmitJSONVersion {
		t.Error("BLOCK OmitJSONVersion should be true")
	}

	// BTC sets JSONVersion=1.0 explicitly; OmitJSONVersion defaults to false.
	if btc.JSONVersion != "1.0" || btc.OmitJSONVersion {
		t.Errorf("BTC jsonrpc conf wrong: version=%q omit=%v", btc.JSONVersion, btc.OmitJSONVersion)
	}

	// Address / CashAddrPrefix mirror the C++ reader (xbridgeapp.cpp:977/997).
	// BTC leaves them empty (non-BCH coin); BCH sets CashAddrPrefix.
	if btc.Address != "" {
		t.Errorf("BTC Address = %q, want empty", btc.Address)
	}
	if btc.CashAddrPrefix != "" {
		t.Errorf("BTC CashAddrPrefix = %q, want empty", btc.CashAddrPrefix)
	}
	bch, ok := conf.Coins["BCH"]
	if !ok {
		t.Fatal("BCH section missing")
	}
	if bch.CashAddrPrefix != "bitcoincash" {
		t.Errorf("BCH CashAddrPrefix = %q, want bitcoincash", bch.CashAddrPrefix)
	}
}

func TestLoadMissing(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "does-not-exist.conf")); err == nil {
		t.Fatal("expected error for missing conf")
	}
}

// TestLoadSkipsRpcSection locks the [Rpc] handling: [Rpc] is a dead section in C++
// (util/settings.h:49-65) and must not be parsed as a coin (which would abort
// startup with "COIN not set"). The section must simply not appear in Coins.
func TestLoadSkipsRpcSection(t *testing.T) {
	conf, err := Load(writeConf(t, `
[Main]
ExchangeWallets=BTC

[Rpc]
Enable=1
Port=8332

[BTC]
Title=Bitcoin
Ip=127.0.0.1
Port=8332
COIN=100000000
BlockTime=600
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := conf.Coins["Rpc"]; ok {
		t.Error("[Rpc] was parsed as a coin section")
	}
	if _, ok := conf.Coins["BTC"]; !ok {
		t.Error("BTC section missing")
	}
}

// TestExchangeWalletsCppSemantics locks the ExchangeWallets parsing: it splits on
// ",", ";" or ":" and each symbol is uppercased/validated by ccy::Symbol
// (length 1..8, no trimming — util/settings.cpp:143-166, currency.h:45-57).
func TestExchangeWalletsCppSemantics(t *testing.T) {
	conf, err := Load(writeConf(t, `
[Main]
ExchangeWallets=BTC;ltc:DGB,,XRB123456
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []string{"BTC", "LTC", "DGB"}
	if len(conf.Main.ExchangeWallets) != len(want) {
		t.Fatalf("ExchangeWallets = %v, want %v", conf.Main.ExchangeWallets, want)
	}
	for i := range want {
		if conf.Main.ExchangeWallets[i] != want[i] {
			t.Errorf("ExchangeWallets[%d] = %q, want %q", i, conf.Main.ExchangeWallets[i], want[i])
		}
	}

	// Whitespace inside the list is preserved (C++ does not trim within the
	// value): " LTC" is a valid 4-byte symbol uppercased to " LTC" — and a
	// distinct section name from [LTC]. (The value itself is trimmed at the
	// edges by the INI parser, as boost's parser does.)
	conf, err = Load(writeConf(t, `
[Main]
ExchangeWallets=BTC, LTC
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(conf.Main.ExchangeWallets) != 2 || conf.Main.ExchangeWallets[0] != "BTC" || conf.Main.ExchangeWallets[1] != " LTC" {
		t.Errorf("ExchangeWallets = %v, want [BTC \" LTC\"]", conf.Main.ExchangeWallets)
	}

	// Exactly-8-byte symbols are accepted; a trailing separator and a
	// whitespace-only token (a valid 1-byte symbol, uppercased to a space) are
	// kept, matching C++'s raw-bytes validate.
	conf, err = Load(writeConf(t, `
[Main]
ExchangeWallets=BTCABC12, ,LTC,
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(conf.Main.ExchangeWallets) != 3 ||
		conf.Main.ExchangeWallets[0] != "BTCABC12" ||
		conf.Main.ExchangeWallets[1] != " " ||
		conf.Main.ExchangeWallets[2] != "LTC" {
		t.Errorf("ExchangeWallets = %v, want [BTCABC12 \" \" LTC]", conf.Main.ExchangeWallets)
	}

	// Empty list.
	conf, err = Load(writeConf(t, "[Main]\nExchangeWallets=\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(conf.Main.ExchangeWallets) != 0 {
		t.Errorf("ExchangeWallets = %v, want empty", conf.Main.ExchangeWallets)
	}
}

// TestCaseSensitiveKeys locks the case-sensitivity: key and section lookups are exact-case
// (boost property_tree). A miscased key reads as absent; "[main]" is a coin
// section, not the [Main] block.
func TestCaseSensitiveKeys(t *testing.T) {
	conf, err := Load(writeConf(t, `
[main]
ExchangeWallets=BTC

[BTC]
coin=100000000
BlockTime=600
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(conf.Main.ExchangeWallets) != 0 {
		t.Errorf("[main] should not be the Main section: %v", conf.Main.ExchangeWallets)
	}
	btc, ok := conf.Coins["BTC"]
	if !ok {
		t.Fatal("BTC section missing")
	}
	if btc.Coin != 0 {
		t.Errorf("lowercase coin= read as Coin %d, want 0 (absent key)", btc.Coin)
	}
	if btc.BlockTime != 600 {
		t.Errorf("BlockTime = %d, want 600 (correct-case key still read)", btc.BlockTime)
	}
}

// TestTitleDefaultsEmpty locks the title default: C++ Settings::get returns "" for a
// missing key (settings.h:75-84), so Title defaults to "" — not the section
// name (the pre-fix Go default).
func TestTitleDefaultsEmpty(t *testing.T) {
	conf, err := Load(writeConf(t, `
[BTC]
Title=Bitcoin

[LTC]
Ip=127.0.0.1
Port=9332
COIN=100000000
BlockTime=150
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	btc, btcOK := conf.Coins["BTC"]
	ltc, ltcOK := conf.Coins["LTC"]
	if !btcOK || !ltcOK {
		t.Fatalf("missing sections: BTC=%v LTC=%v", btcOK, ltcOK)
	}
	if btc.Title != "Bitcoin" {
		t.Errorf("BTC Title = %q, want Bitcoin", btc.Title)
	}
	if ltc.Title != "" {
		t.Errorf("LTC Title = %q, want \"\" (missing key)", ltc.Title)
	}
}
