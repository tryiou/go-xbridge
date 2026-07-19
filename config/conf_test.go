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
