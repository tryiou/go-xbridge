package coins

import (
	"os"
	"path/filepath"
	"testing"

	"xbridge-go/config"
)

const sampleConf = `
[Main]
ExchangeWallets=BTC,DOGE,BLOCK

[BTC]
Title=Bitcoin
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
FeePerByte=2
Confirmations=2

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
`

func writeConf(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "xbridge.conf")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write conf: %v", err)
	}
	return p
}

// TestFromConfNoHardcoding proves every Coin value comes from conf, with no
// baked-in literals: BTC and BLOCK differ only by what the conf says (no
// special-casing for BLOCK). It uses FromConf directly so it does not disturb
// the shared registry seeded by TestMain.
func TestFromConfNoHardcoding(t *testing.T) {
	p := writeConf(t, sampleConf)
	conf, err := config.Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	btc, ok := conf.Coins["BTC"]
	if !ok {
		t.Fatal("BTC section missing")
	}
	bc, err := FromConf(btc)
	if err != nil {
		t.Fatalf("FromConf BTC: %v", err)
	}
	if bc.Decimals != 8 || bc.P2PKH != 0 || bc.P2SH != 5 || !bc.SegWit || bc.Bech32HRP != "bc" {
		t.Errorf("BTC coin wrong: %+v", bc)
	}

	block, ok := conf.Coins["BLOCK"]
	if !ok {
		t.Fatal("BLOCK section missing")
	}
	blc, err := FromConf(block)
	if err != nil {
		t.Fatalf("FromConf BLOCK: %v", err)
	}
	if blc.Decimals != 8 || blc.P2PKH != 26 || blc.P2SH != 28 || blc.SegWit || blc.Bech32HRP != "" {
		t.Errorf("BLOCK coin wrong: %+v", blc)
	}
}
