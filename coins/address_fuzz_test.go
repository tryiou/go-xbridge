package coins

import (
	"testing"

	"go-xbridge/config"
)

func init() {
	// Register a BTC coin so the fuzz target has a real address codec to
	// exercise. Tests that call InitFromConf with their own set may replace the
	// global registry; the target guards on a missing coin and returns safely.
	_ = InitFromConf(map[string]*config.CoinConf{
		"BTC": {Ticker: "BTC", CreateTxMethod: "BTC", AddressPrefix: 0, ScriptPrefix: 5, Coin: 100000000},
	})
}

// FuzzParseAddr drives every address decoder (base58check P2PKH/P2SH, bech32
// native segwit, CashAddr) with arbitrary input strings. The decoders sit on
// the untrusted inbound path and must never panic — invalid input returns an
// error and valid input round-trips.
func FuzzParseAddr(f *testing.F) {
	f.Add("1BvBMSEYstWetqTFn5Au4m4GFg7xJaNVN2")                     // P2PKH
	f.Add("BC1QW508D6QEJXTDG4Y5R3ZARVARY0C5XW7KV8F3T4")             // P2WPKH (BIP173)
	f.Add("bitcoincash:qpm2qsznhks23z7629mms6s4cwef9xce3zqfrnla3t") // cashaddr P2KH
	f.Fuzz(func(t *testing.T, s string) {
		c, ok := Get("BTC")
		if !ok {
			return
		}
		// Must never panic. Invalid input returns an error; valid input is
		// usable. We only assert safety here, not specific outputs.
		a, err := c.DecodeAddress(s)
		if err == nil {
			_ = a.String() // re-encode path must also be safe
		}
	})
}
