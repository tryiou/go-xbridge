package coins

import (
	"fmt"
	"sync/atomic"

	"go-xbridge/config"
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

// SignatureKind identifies how local HTLC signatures (refund/claim) are
// produced for a coin. Mirrors the C++ connector's per-connector signing
// override (xbridgewalletconnector{btc,bch,btg}.cpp createRefundTransaction):
//   - SigLegacy: legacy SIGHASH_ALL (0x01) digest — BtcWalletConnector family.
//   - SigForkID: BIP143 digest with a fork value committed in the sighash type
//     (SIGHASH_FORKID, 0x40) — BCH/DEVAULT (fork value 0), BTG (79).
type SignatureKind int

const (
	SigLegacy SignatureKind = iota
	SigForkID
)

// Coin describes a UTXO-based blockchain traded over XBridge. The address
// parameters are the version bytes / HRP the connected wallet uses; they drive
// address decoding/encoding in address.go. NO coin values are hardcoded: every
// Coin is built at startup from its [TICKER] section in xbridge.conf via
// InitFromConf. The original core-wallet XBridge supplies these same values
// through xbridge.conf, so go-xbridge simply mirrors that (read-only).
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

	// signature is the local-signing algorithm (see SignatureKind), and
	// forkValue the BIP135 fork value committed in the digest for SigForkID
	// coins. Both derive from CreateTxMethod, mirroring the C++ connector
	// constants (no coin is hardcoded in the registry itself).
	signature SignatureKind
	forkValue uint32

	// TxWithTimeField mirrors <COIN>.TxWithTimeField in xbridge.conf
	// (xbridgeapp.cpp:993). When true, deposit/refund/claim transactions for this
	// coin carry the extra 4-byte nTime field after nVersion on the wire (the
	// XBridge serializeWithTimeField quirk). Carried on Coin so the deposit/
	// refund builders can stamp it without a conf round-trip.
	TxWithTimeField bool
}

// Family returns the chain family.
func (c Coin) Family() FamilyKind { return c.family }

// SignatureKind returns the local-signing algorithm for this coin.
func (c Coin) SignatureKind() SignatureKind { return c.signature }

// ForkValue returns the BIP135 fork value committed in the sighash type when
// SignatureKind() == SigForkID (0 for BCH/DEVAULT — hashType 0x41; 79 for BTG —
// hashType 0x4F41). Meaningless for SigLegacy coins.
func (c Coin) ForkValue() uint32 { return c.forkValue }

// registry is the runtime coin set, published atomically by InitFromConf.
// Readers (Get/Has) dereference the pointer with no lock, so a
// dxLoadXBridgeConf hot-reload can never race a concurrent lookup: a reader
// sees either one consistent snapshot or the next, never a half-populated map
// (which would abort the process with "concurrent map read and map write").
var registry atomic.Pointer[map[string]Coin]

func init() {
	m := map[string]Coin{}
	registry.Store(&m)
}

// InitFromConf populates the registry from parsed xbridge.conf sections. It
// builds a fresh immutable map and publishes it atomically, so the registry
// always reflects exactly the coins named in conf (nothing more, nothing less).
// On error the previous registry is left untouched (last-good on failure).
func InitFromConf(confs map[string]*config.CoinConf) error {
	next := map[string]Coin{}
	for ticker, c := range confs {
		coin, err := FromConf(c)
		if err != nil {
			return err
		}
		next[ticker] = coin
	}
	registry.Store(&next)
	return nil
}

// FromConf builds a Coin from a single [TICKER] conf section.
func FromConf(c *config.CoinConf) (Coin, error) {
	if c.Coin == 0 {
		return Coin{}, fmt.Errorf("coins: %s: COIN not set in xbridge.conf", c.Ticker)
	}
	// CashAddrPrefix: the conf value wins; when empty it falls back to the
	// per-connector class constant, mirroring the C++ BCH/DEVAULT connectors
	// (bch.cpp:306-308 sets "bitcoincash" only if empty; devault.cpp:278-280
	// overrides a "bitcoincash" value to "devault"). Pre-fix Go ignored the
	// conf value entirely and always used the method-derived constant.
	cashAddr := c.CashAddrPrefix
	if cashAddr == "" {
		cashAddr = cashAddrPrefixFromMethod(c.CreateTxMethod)
	} else if c.CreateTxMethod == "DEVAULT" && cashAddr == "bitcoincash" {
		cashAddr = "devault"
	}
	return Coin{
		Ticker:          c.Ticker,
		Name:            c.Title,
		Decimals:        decimalsFromCoin(c.Coin),
		P2PKH:           byte(c.AddressPrefix),
		P2SH:            byte(c.ScriptPrefix),
		Bech32HRP:       bech32HRPFromMethod(c.CreateTxMethod),
		SegWit:          segWitFromMethod(c.CreateTxMethod),
		family:          familyFromMethod(c.CreateTxMethod),
		CashAddrPrefix:  cashAddr,
		signature:       signatureKindFromMethod(c.CreateTxMethod),
		forkValue:       forkValueFromMethod(c.CreateTxMethod),
		TxWithTimeField: c.TxWithTimeField,
	}, nil
}

// familyFromMethod maps a CreateTxMethod to its chain family. Unmapped methods
// default to the BTC family (the generic base58check + bech32 codec), matching
// C++'s default connector behavior.
func familyFromMethod(m string) FamilyKind {
	switch normalizeTicker(m) {
	case "BCH", "DEVAULT":
		return FamilyUTXOBCH
	default:
		return FamilyUTXOBTC
	}
}

// cashAddrPrefixFromMethod returns the CashAddr HRP for BCH-family coins.
// Mirrors the C++ connectors' per-class prefix constants (bch.cpp:307
// "bitcoincash", devault.cpp:278-279 overriding to "devault"); the method string
// comes from conf so nothing is hardcoded in the registry itself.
func cashAddrPrefixFromMethod(m string) string {
	switch normalizeTicker(m) {
	case "BCH":
		return "bitcoincash"
	case "DEVAULT":
		return "devault"
	default:
		return ""
	}
}

// signatureKindFromMethod maps a CreateTxMethod to its local-signing algorithm.
// Mirrors the C++ connector classes: only the BCH-family (BCH/DEVAULT) and BTG
// override createRefundTransaction/createPaymentTransaction with a forkid
// sighash; the BTC-family base uses the legacy digest. Unmapped methods default
// to legacy.
func signatureKindFromMethod(m string) SignatureKind {
	switch normalizeTicker(m) {
	case "BCH", "DEVAULT", "BTG":
		return SigForkID
	default:
		return SigLegacy
	}
}

// forkValueFromMethod returns the BIP135 fork value committed in the sighash for
// forkid coins. The values are the per-connector constants C++ bakes into its
// connector classes (no coin is hardcoded in the registry itself):
//   - DEVAULT: 0 — devault.cpp:171 explicitly disables
//     SCRIPT_ENABLE_REPLAY_PROTECTION ("not supported at this time"), so the
//     plain withForkId() hashType 0x41 stands.
//   - BTG: 79 — btg.cpp:69 FORKID_IN_USE, digest hashType (79<<8)|0x41 = 0x4F41.
//   - BCH: 0xffdead — bch.cpp:203-209,497-499: when the chain's median time is
//     >= 1605441600 (BCH's permanent 2020-11-15 replay-protection upgrade) the
//     connector rewrites the fork value to 0xff0000|(fork^0xdead) = 0xffdead,
//     so the digest commits hashType 0xffdead41. Live BCH mainnet has been past
//     that threshold continuously, so a thin client always signs with 0xffdead;
//     the pre-upgrade (fork value 0) case no longer exists on a live chain. The
//     fork value only affects the digest — the DER signature byte stays 0x41
//     (SIGHASH_ALL|SIGHASH_FORKID).
func forkValueFromMethod(m string) uint32 {
	switch normalizeTicker(m) {
	case "BCH":
		return 0xffdead
	case "BTG":
		return 79
	default:
		return 0
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
	case "BTC", "LTC", "DGB", "BTG":
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
	case "BTG":
		return "btg"
	default:
		return ""
	}
}

// Get returns the Coin for a ticker (case-insensitive), or false.
func Get(ticker string) (Coin, bool) {
	c, ok := (*registry.Load())[normalizeTicker(ticker)]
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
	_, ok := (*registry.Load())[normalizeTicker(ticker)]
	return ok
}

// Snapshot returns a consistent copy of the whole registry under a single
// atomic load, so a caller that needs several coins at once sees a mutually
// consistent set. Use this rather than repeated Get calls:
// two Gets straddling a dxLoadXBridgeConf Store could observe a mixed set
// (e.g. one currency from the old registry and one from the new), whereas one
// Snapshot never can. The returned Coin values are immutable copies.
func Snapshot() map[string]Coin {
	m := *registry.Load()
	out := make(map[string]Coin, len(m))
	for t, c := range m {
		out[t] = c
	}
	return out
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
