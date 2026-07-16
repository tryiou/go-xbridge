package coins

import (
	"fmt"

	"xbridge-go/config"
)

// FamilyKind identifies a chain's transaction/address family so connectors can
// branch on per-chain quirks. BTC/LTC/DGB share the base58check + bech32 family;
// Bitcoin Cash uses CashAddr; others may follow. Mirrors C++'s per-connector
// class selection (xbridgewalletconnector{btc,bch,...}.cpp).
type FamilyKind string

const (
	// FamilyUTXOBTC is the BTC-family (base58check P2PKH/P2SH + optional bech32).
	FamilyUTXOBTC FamilyKind = "utxo-btc"
	// FamilyUTXOBCH is Bitcoin Cash (CashAddr encoding, "bitcoincash:" prefix).
	FamilyUTXOBCH FamilyKind = "utxo-bch"
)

// Coin describes a UTXO-based blockchain traded over XBridge. The address
// parameters are the version bytes / HRP the connected wallet uses; they drive
// address decoding/encoding in address.go. NO coin values are hardcoded: every
// Coin is built at startup from its [TICKER] section in xbridge.conf via
// InitFromConf. The original core-wallet XBridge supplies these same values
// through xbridge.conf, so xbridge-go simply mirrors that (read-only).
type Coin struct {
	Ticker    string // wire ticker, e.g. "BTC"
	Name      string
	Decimals  int    // base-unit precision (8 = satoshi)
	P2PKH     byte   // base58check version byte for P2PKH addresses
	P2SH      byte   // base58check version byte for P2SH addresses
	Bech32HRP string // HRP for native segwit addresses ("", if none)
	SegWit    bool   // whether native segwit (bech32/bech32m) addresses exist

	// family is the chain family (see FamilyKind). It selects the address codec
	// and any per-chain RPC quirks. Derived from CreateTxMethod.
	family FamilyKind
	// CashAddrPrefix is the CashAddr HRP (e.g. "bitcoincash") used when family ==
	// FamilyUTXOBCH. Empty for other families.
	CashAddrPrefix string
}

// Family returns the chain family.
func (c Coin) Family() FamilyKind { return c.family }

// Coins is the runtime registry, populated entirely from xbridge.conf by
// InitFromConf. It starts empty — there is no baked-in set.
var Coins = map[string]Coin{}

// InitFromConf populates the registry from parsed xbridge.conf sections. It
// clears any previous contents first, so the registry always reflects exactly
// the coins named in conf (nothing more, nothing less).
func InitFromConf(confs map[string]*config.CoinConf) error {
	Coins = map[string]Coin{}
	for ticker, c := range confs {
		coin, err := FromConf(c)
		if err != nil {
			return err
		}
		Coins[ticker] = coin
	}
	return nil
}

// FromConf builds a Coin from a single [TICKER] conf section.
func FromConf(c *config.CoinConf) (Coin, error) {
	if c.Coin == 0 {
		return Coin{}, fmt.Errorf("coins: %s: COIN not set in xbridge.conf", c.Ticker)
	}
	return Coin{
		Ticker:         c.Ticker,
		Name:           c.Title,
		Decimals:       decimalsFromCoin(c.Coin),
		P2PKH:          byte(c.AddressPrefix),
		P2SH:           byte(c.ScriptPrefix),
		Bech32HRP:      bech32HRPFromMethod(c.CreateTxMethod),
		SegWit:         segWitFromMethod(c.CreateTxMethod),
		family:         familyFromMethod(c.CreateTxMethod),
		CashAddrPrefix: cashAddrPrefixFromMethod(c.CreateTxMethod),
	}, nil
}

// familyFromMethod maps a CreateTxMethod to its chain family. Unmapped methods
// default to the BTC family (the generic base58check + bech32 codec), matching
// C++'s default connector behavior.
func familyFromMethod(m string) FamilyKind {
	switch normalizeTicker(m) {
	case "BCH":
		return FamilyUTXOBCH
	default:
		return FamilyUTXOBTC
	}
}

// cashAddrPrefixFromMethod returns the CashAddr HRP for the BCH family. Empty
// for other families. Mirrors C++'s cashaddr prefix constants; the method string
// comes from conf so nothing is hardcoded in the registry itself.
func cashAddrPrefixFromMethod(m string) string {
	switch normalizeTicker(m) {
	case "BCH":
		return "bitcoincash"
	default:
		return ""
	}
}

// decimalsFromCoin returns the number of decimal places implied by the base-unit
// multiplier (e.g. 100000000 -> 8). COIN is always a power of ten in XBridge.
func decimalsFromCoin(coin uint64) int {
	d := 0
	for coin > 0 && coin%10 == 0 {
		d++
		coin /= 10
	}
	if d == 0 {
		// Fallback for a malformed COIN; most chains use 8.
		return 8
	}
	return d
}

// segWitFromMethod maps a CreateTxMethod to whether the chain supports native
// segwit. The original XBridge encodes this inside each wallet-connector class;
// the method string is supplied by xbridge.conf, so the table merely reproduces
// C++'s per-connector selection (no coin is hardcoded in the registry itself).
func segWitFromMethod(m string) bool {
	switch normalizeTicker(m) {
	case "BTC", "LTC", "DGB":
		return true
	default:
		return false
	}
}

// bech32HRPFromMethod maps a CreateTxMethod to its native-segwit HRP. Empty
// string means the chain has no native segwit addresses. Mirrors the per-chain
// HRP constant in C++'s connector classes; the method string comes from conf.
func bech32HRPFromMethod(m string) string {
	switch normalizeTicker(m) {
	case "BTC":
		return "bc"
	case "LTC":
		return "ltc"
	case "DGB":
		return "dgb"
	default:
		return ""
	}
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

// Has reports whether a ticker is registered.
func Has(ticker string) bool {
	_, ok := Coins[normalizeTicker(ticker)]
	return ok
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
