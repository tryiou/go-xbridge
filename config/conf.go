// Package config reads Blocknet's xbridge.conf — the same INI file the original
// core-wallet XBridge reads. go-xbridge only READS this file (it never generates
// or mutates it); every coin connector, including BLOCK and BTC, is defined
// entirely by its [TICKER] section here. There is no hardcoded coin data anywhere
// in the library — the schema below is a faithful mirror of the **createConf()
// reader** (src/xbridge/xbridgeapp.cpp:976-997). Every field is a real
// `[TICKER]` conf key; nothing here is invented. Note: C++ does NOT read a
// `RelayFee` conf key — relay fee comes live from the wallet RPC
// `getmininginfo.relayfee` (xbridgewalletconnectorbtc.cpp:74-76) and is used
// only for dust fallback (`dustAmount = relayFee>0 ? 0.546*relayFee*COIN : 5460`,
// xbridgewalletconnectorbtc.cpp:1526). go-xbridge has no live relay-fee feed
// (thin client), so dust resolves from the conf `MinimumAmount` key — C++ maps
// that key onto the exchange wallets' dustAmount (xbridgeexchange.cpp:145) and
// never reads a `DustAmount` key — else the C++-defined constant 5460.
// `GetNewKeySupported`/`ImportWithNoScanSupported`/`DustAmount` are written by
// the createConf() template but are not read back by the reader.
package config

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Main holds the [Main] section of xbridge.conf.
type Main struct {
	// ExchangeWallets is the comma-separated list of tickers that have a local
	// wallet configured (the connectors go-xbridge drives).
	ExchangeWallets []string
	ShowAllOrders   bool
	FullLog         bool
}

// CoinConf holds one [TICKER] section of xbridge.conf. Field names and types
// mirror the original createConf() keys exactly; nothing is defaulted to a
// coin's "real" value — every value comes from the file.
type CoinConf struct {
	Ticker   string // section name, e.g. "BTC"
	Title    string
	Address  string // wallet/RPC bind address (C++ xbridgeapp.cpp:977 ".Address")
	Ip       string
	Port     int
	Username string
	Password string

	// CreateTxMethod selects the transaction-construction path (e.g. "BTC").
	// The original derives segwit/bech32 support from this inside the connector
	// class; go-xbridge reproduces that mapping (see coins package) so the
	// method string — supplied by conf — is the only input.
	CreateTxMethod string

	// AddressPrefix / ScriptPrefix / SecretPrefix are the base58check version
	// bytes (P2PKH / P2SH / WIF) as decimal integers in the conf.
	AddressPrefix int
	ScriptPrefix  int
	SecretPrefix  int

	// Coin is the base-unit multiplier (e.g. 100000000). Decimals are derived
	// from it (number of trailing zeros).
	Coin          uint64
	MinimumAmount uint64
	TxVersion     int
	// DustAmount is written by the createConf() template but never read back by
	// the C++ reader (xbridgeexchange.cpp:145 maps `MinimumAmount` onto the
	// exchange wallets' dustAmount instead). Parsed here for template fidelity;
	// go-xbridge's dust resolves from MinimumAmount (see the package comment).
	DustAmount    uint64
	MinTxFee      uint64
	BlockTime     int // seconds per block
	FeePerByte    uint64
	Confirmations int

	TxWithTimeField           bool
	LockCoinsSupported        bool
	GetNewKeySupported        bool
	ImportWithNoScanSupported bool

	CashAddrPrefix string // BCH cashaddr HRP (C++ xbridgeapp.cpp:997 ".CashAddrPrefix"); empty for non-BCH coins.

	// JSONVersion / ContentType are the RPC client version and content-type the
	// connected wallet expects (some wallets require a specific value). Both
	// default to empty, mirroring C++ xbridgeapp.cpp:995-996: an empty
	// JSONVersion omits the "jsonrpc" request field entirely
	// (XBridgeJSONRPCRequestObj) and an empty ContentType leaves the
	// Content-Type header unset (CallRPC), matching stock XBridge.
	JSONVersion string
	ContentType string
	// OmitJSONVersion, when true, drops the "jsonrpc" field from RPC requests
	// entirely. XLite-style wallets reject requests that carry it; Bitcoin Core
	// and blocknetd expect {"jsonrpc":"1.0",...}. Off by default.
	OmitJSONVersion bool
}

// Conf is the parsed xbridge.conf.
type Conf struct {
	Main  Main
	Coins map[string]*CoinConf // keyed by ticker (section name)
}

// Load reads and parses the xbridge.conf at path. It returns an error if the
// file is missing or malformed. It NEVER creates the file.
func Load(path string) (*Conf, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("config: cannot open %s: %w", path, err)
	}
	defer f.Close()

	parsed, err := parseINI(f)
	if err != nil {
		return nil, err
	}

	conf := &Conf{Coins: map[string]*CoinConf{}}
	for name, kv := range parsed {
		// boost property_tree is case-sensitive: only an exact "Main" section
		// is the [Main] block; "[main]" is a coin section like any other.
		if name == "Main" {
			conf.Main = parseMain(kv)
			continue
		}
		// C++ defines the Rpc.* keys (util/settings.h:49-65) but never reads
		// them — [Rpc] is a dead section. Never treat it as a coin: a stock
		// config carrying [Rpc] must not kill startup with "COIN not set".
		if name == "Rpc" {
			continue
		}
		cc := parseCoinConf(name, kv)
		conf.Coins[name] = cc
	}
	return conf, nil
}

// ---------------------------------------------------------------------------
// INI parsing
// ---------------------------------------------------------------------------

// parseINI reads a minimal INI file: "[Section]" headers, "key = value" lines,
// and "#"/";" comments. Leading/trailing whitespace is trimmed. Returns a map
// from section name to its key/value pairs.
func parseINI(f *os.File) (map[string]map[string]string, error) {
	out := map[string]map[string]string{}
	cur := ""
	sc := bufio.NewScanner(f)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		raw := strings.TrimSpace(sc.Text())
		if raw == "" || strings.HasPrefix(raw, "#") || strings.HasPrefix(raw, ";") {
			continue
		}
		if strings.HasPrefix(raw, "[") && strings.HasSuffix(raw, "]") {
			// The bracketed name is trimmed (boost's ini parser does the same,
			// ini_parser.hpp:110): "[ Main ]" is the "Main" section.
			cur = strings.TrimSpace(raw[1 : len(raw)-1])
			if _, ok := out[cur]; !ok {
				out[cur] = map[string]string{}
			}
			continue
		}
		if cur == "" {
			return nil, fmt.Errorf("config: line %d: key/value outside any [section]", lineNo)
		}
		eq := strings.IndexByte(raw, '=')
		if eq < 0 {
			return nil, fmt.Errorf("config: line %d: expected key=value", lineNo)
		}
		key := strings.TrimSpace(raw[:eq])
		val := strings.TrimSpace(raw[eq+1:])
		out[cur][key] = val
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("config: read: %w", err)
	}
	return out, nil
}

// section is a case-sensitive key/value view of one INI section. Boost
// property_tree is case-sensitive ("COIN" != "coin", CFG-F89), so lookups are
// exact — a miscased key reads as absent, exactly as it does in C++ (its
// consequence downstream — the coin failing to load — is the admission pass's
// concern, CFG-F85/F87, not the parser's).
type section map[string]string

func (s section) get(key string) (string, bool) {
	v, ok := s[key]
	return v, ok
}

func (s section) str(key, def string) string {
	if v, ok := s.get(key); ok && v != "" {
		return v
	}
	return def
}

func (s section) intp(key string, def int) int {
	v, ok := s.get(key)
	if !ok || v == "" {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return def
	}
	return n
}

func (s section) uintp(key string, def uint64) uint64 {
	v, ok := s.get(key)
	if !ok || v == "" {
		return def
	}
	n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64)
	if err != nil {
		return def
	}
	return n
}

func (s section) floatp(key string, def float64) float64 {
	v, ok := s.get(key)
	if !ok || v == "" {
		return def
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil {
		return def
	}
	return f
}

func (s section) boolp(key string) bool {
	v, ok := s.get(key)
	if !ok {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// parseMain parses the [Main] section. ExchangeWallets mirrors C++
// Settings::exchangeWallets (util/settings.cpp:143-166): the list splits on
// ",", ";" or ":" and each symbol is validated/uppercased by ccy::Symbol
// (currency.h:45-57) — length 1..8, no trimming (C++ does not trim).
func parseMain(kv map[string]string) Main {
	s := section(kv)
	ew := s.str("ExchangeWallets", "")
	wallets := []string{}
	if ew != "" {
		for _, raw := range strings.FieldsFunc(ew, isWalletSeparator) {
			if sym := validateSymbol(raw); sym != "" {
				wallets = append(wallets, sym)
			}
		}
	}
	return Main{
		ExchangeWallets: wallets,
		ShowAllOrders:   s.boolp("ShowAllOrders"),
		FullLog:         s.boolp("FullLog"),
	}
}

func isWalletSeparator(r rune) bool {
	return r == ',' || r == ';' || r == ':'
}

// validateSymbol mirrors ccy::Symbol::validate (src/xbridge/currency.h:45-57):
// uppercases the symbol and enforces a length of 1..8 bytes; anything else is
// invalid and returns "" (the C++ Symbol constructor throws on a bad length).
// There is no charset check and no trimming — C++ does not filter the raw
// bytes. The uppercase is byte-wise ASCII-only, matching C's ::toupper in the
// C locale (bytes >= 0x80 are left unchanged, so the length guarantee holds).
func validateSymbol(raw string) string {
	if len(raw) < 1 || len(raw) > 8 {
		return ""
	}
	out := []byte(raw)
	for i := range out {
		if out[i] >= 'a' && out[i] <= 'z' {
			out[i] -= 'a' - 'A'
		}
	}
	return string(out)
}

func parseCoinConf(name string, kv map[string]string) *CoinConf {
	s := section(kv)
	return &CoinConf{
		Ticker:                    name,
		Title:                     s.str("Title", ""), // C++ Settings::get defaults a missing key to "" (settings.h:75-84), not the section name
		Address:                   s.str("Address", ""),
		Ip:                        s.str("Ip", ""),
		Port:                      s.intp("Port", 0),
		Username:                  s.str("Username", ""),
		Password:                  s.str("Password", ""),
		CreateTxMethod:            s.str("CreateTxMethod", ""),
		AddressPrefix:             s.intp("AddressPrefix", 0),
		ScriptPrefix:              s.intp("ScriptPrefix", 0),
		SecretPrefix:              s.intp("SecretPrefix", 0),
		Coin:                      s.uintp("COIN", 0),
		MinimumAmount:             s.uintp("MinimumAmount", 0),
		TxVersion:                 s.intp("TxVersion", 1), // C++ xbridgeapp.cpp: s.get<uint32_t>(*i+".TxVersion", 1)
		DustAmount:                s.uintp("DustAmount", 0),
		MinTxFee:                  s.uintp("MinTxFee", 0),
		BlockTime:                 s.intp("BlockTime", 0),
		FeePerByte:                s.uintp("FeePerByte", 0),
		Confirmations:             s.intp("Confirmations", 0),
		TxWithTimeField:           s.boolp("TxWithTimeField"),
		LockCoinsSupported:        s.boolp("LockCoinsSupported"),
		GetNewKeySupported:        s.boolp("GetNewKeySupported"),
		ImportWithNoScanSupported: s.boolp("ImportWithNoScanSupported"),
		CashAddrPrefix:            s.str("CashAddrPrefix", ""),
		JSONVersion:               s.str("JSONVersion", ""),
		ContentType:               s.str("ContentType", ""),
		OmitJSONVersion:           s.boolp("OmitJSONVersion"),
	}
}
