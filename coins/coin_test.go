package coins

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"go-xbridge/config"
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

// TestFromConfCashAddrPrefix locks CFG-F91: the conf `CashAddrPrefix` value
// wins over the method-derived class constant, with C++'s fallbacks (bch.cpp:
// 306-308, devault.cpp:278-280) when empty / "bitcoincash" on DEVAULT.
func TestFromConfCashAddrPrefix(t *testing.T) {
	bch := &config.CoinConf{Ticker: "BCH", CreateTxMethod: "BCH", Coin: 100000000, CashAddrPrefix: "mybch"}
	c, err := FromConf(bch)
	if err != nil {
		t.Fatalf("FromConf: %v", err)
	}
	if c.CashAddrPrefix != "mybch" {
		t.Errorf("CashAddrPrefix = %q, want conf value mybch (conf wins)", c.CashAddrPrefix)
	}

	// Empty conf value: the method-derived class constant kicks in.
	bch.CashAddrPrefix = ""
	c, _ = FromConf(bch)
	if c.CashAddrPrefix != "bitcoincash" {
		t.Errorf("CashAddrPrefix = %q, want bitcoincash fallback", c.CashAddrPrefix)
	}

	// DEVAULT with a "bitcoincash" conf value is overridden to "devault".
	dv := &config.CoinConf{Ticker: "DVT", CreateTxMethod: "DEVAULT", Coin: 100000000, CashAddrPrefix: "bitcoincash"}
	c, _ = FromConf(dv)
	if c.CashAddrPrefix != "devault" {
		t.Errorf("DEVAULT CashAddrPrefix = %q, want devault", c.CashAddrPrefix)
	}
	dv.CashAddrPrefix = "mydvt"
	c, _ = FromConf(dv)
	if c.CashAddrPrefix != "mydvt" {
		t.Errorf("DEVAULT CashAddrPrefix = %q, want conf value mydvt", c.CashAddrPrefix)
	}
}

// TestConcurrentInitFromConfGet proves the registry's atomic publication: a
// hot-reload (InitFromConf) can run on one goroutine while readers call Get on
// others, and no reader ever observes a torn or partial snapshot (the -race
// detector would flag a plain-map implementation).
func TestConcurrentInitFromConfGet(t *testing.T) {
	// Restore the TestMain-seeded registry when the test finishes so sibling
	// codec tests (which call MustGet for BLOCK/DOGE) still see their coins.
	// Captured BEFORE the first InitFromConf below.
	saved := registry.Load()
	defer registry.Store(saved)

	base := map[string]*config.CoinConf{
		"BTC": {Ticker: "BTC", Coin: 1e8, AddressPrefix: 0, ScriptPrefix: 5, CreateTxMethod: "BTC"},
	}
	if err := InitFromConf(base); err != nil {
		t.Fatal(err)
	}

	var stop atomic.Bool
	var wg sync.WaitGroup
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				if c, ok := Get("BTC"); !ok || c.Ticker != "BTC" || c.P2PKH != 0 {
					t.Errorf("reader saw missing/partial BTC coin: ok=%v ticker=%q p2pkh=%d", ok, c.Ticker, c.P2PKH)
				}
				if c, ok := Get("LTC"); ok && (c.Ticker != "LTC" || c.P2PKH != 48) {
					t.Error("reader saw a partial LTC coin")
				}
			}
		}()
	}
	for i := 0; i < 200; i++ {
		if i%2 == 0 {
			if err := InitFromConf(base); err != nil {
				t.Fatal(err)
			}
			continue
		}
		ltc := map[string]*config.CoinConf{}
		for k, v := range base {
			ltc[k] = v
		}
		ltc["LTC"] = &config.CoinConf{Ticker: "LTC", Coin: 1e8, AddressPrefix: 48, ScriptPrefix: 50, CreateTxMethod: "LTC"}
		if err := InitFromConf(ltc); err != nil {
			t.Fatal(err)
		}
	}
	stop.Store(true)
	wg.Wait()

	if c, ok := Get("BTC"); !ok || c.Ticker != "BTC" {
		t.Fatalf("BTC lost from registry after reloads: ok=%v", ok)
	}
}
