package coins

// Coin describes a UTXO-based blockchain traded over XBridge. The address
// parameters are the version bytes / HRP the connected wallet uses; they drive
// address decoding/encoding in address.go. Values for the coins below are taken
// from each chain's mainnet chainparams. Entries marked [VERIFY] should be
// double-checked against the upstream source before being relied on.
type Coin struct {
	Ticker    string // wire ticker, e.g. "BTC"
	Name      string
	Decimals  int    // base-unit precision (8 = satoshi)
	P2PKH     byte   // base58check version byte for P2PKH addresses
	P2SH      byte   // base58check version byte for P2SH addresses
	Bech32HRP string // HRP for native segwit addresses ("", if none)
	SegWit    bool   // whether native segwit (bech32/bech32m) addresses exist
}

// Known mainnet coins. XBridge's wallet connectors cover more chains; this is
// the initial, foundation set needed by coins/.
var Coins = map[string]Coin{
	"BTC":   {Ticker: "BTC", Name: "Bitcoin", Decimals: 8, P2PKH: 0x00, P2SH: 0x05, Bech32HRP: "bc", SegWit: true},
	"LTC":   {Ticker: "LTC", Name: "Litecoin", Decimals: 8, P2PKH: 0x30, P2SH: 0x32, Bech32HRP: "ltc", SegWit: true},
	"DOGE":  {Ticker: "DOGE", Name: "Dogecoin", Decimals: 8, P2PKH: 0x1e, P2SH: 0x16, Bech32HRP: "", SegWit: false},
	"DGB":   {Ticker: "DGB", Name: "Digibyte", Decimals: 8, P2PKH: 0x1e, P2SH: 0x3f, Bech32HRP: "dgb", SegWit: true}, // [VERIFY]
	"BLOCK": {Ticker: "BLOCK", Name: "Blocknet", Decimals: 8, P2PKH: 0x1a, P2SH: 0x1c, Bech32HRP: "", SegWit: false},
}

// Get returns the Coin for a ticker (case-insensitive), or false.
func Get(ticker string) (Coin, bool) {
	c, ok := Coins[normalizeTicker(ticker)]
	return c, ok
}

// MustGet returns the Coin for a ticker or panics. Use only with compile-time
// known tickers.
func MustGet(ticker string) Coin {
	c, ok := Get(ticker)
	if !ok {
		panic("coins: unknown coin " + ticker)
	}
	return c
}

func normalizeTicker(t string) string {
	out := make([]byte, 0, len(t))
	for i := 0; i < len(t); i++ {
		c := t[i]
		if c >= 'a' && c <= 'z' {
			c -= 'a' - 'A'
		}
		out = append(out, c)
	}
	return string(out)
}
