// Package config reads Blocknet's xbridge.conf — the same INI file the original
// core-wallet XBridge reads. xbridge-go only READS this file (it never generates
// or mutates it); every coin connector, including BLOCK and BTC, is defined
// entirely by its [TICKER] section here. There is no hardcoded coin data anywhere
// in the library — the schema below is a faithful mirror of
// src/xbridge/xbridgeapp.cpp createConf().
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
	// wallet configured (the connectors xbridge-go drives).
	ExchangeWallets []string
	ShowAllOrders   bool
	FullLog         bool
}

// CoinConf holds one [TICKER] section of xbridge.conf. Field names and types
// mirror the original createConf() keys exactly; nothing is defaulted to a
// coin's "real" value — every value comes from the file.
type CoinConf struct {
	Ticker  string // section name, e.g. "BTC"
	Title   string
	Ip      string
	Port    int
	Username string
	Password string

	// CreateTxMethod selects the transaction-construction path (e.g. "BTC").
	// The original derives segwit/bech32 support from this inside the connector
	// class; xbridge-go reproduces that mapping (see coins package) so the
	// method string — supplied by conf — is the only input.
	CreateTxMethod string

	// AddressPrefix / ScriptPrefix / SecretPrefix are the base58check version
	// bytes (P2PKH / P2SH / WIF) as decimal integers in the conf.
	AddressPrefix int
	ScriptPrefix  int
	SecretPrefix  int

	// Coin is the base-unit multiplier (e.g. 100000000). Decimals are derived
	// from it (number of trailing zeros).
	Coin uint64
	MinimumAmount uint64
	TxVersion     int
	DustAmount    uint64
	MinTxFee      uint64
	BlockTime     int // seconds per block
	FeePerByte    uint64
	Confirmations int

	TxWithTimeField          bool
	LockCoinsSupported       bool
	GetNewKeySupported       bool
	ImportWithNoScanSupported bool

	// JSONVersion / ContentType are the RPC client version and content-type the
	// connected wallet expects (some wallets require a specific value).
	JSONVersion string
	ContentType string
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
		if strings.EqualFold(name, "Main") {
			conf.Main = parseMain(kv)
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
			cur = raw[1 : len(raw)-1]
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

// section is a case-insensitive key/value view of one INI section.
type section map[string]string

func (s section) get(key string) (string, bool) {
	for k, v := range s {
		if strings.EqualFold(k, key) {
			return v, true
		}
	}
	return "", false
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

func parseMain(kv map[string]string) Main {
	s := section(kv)
	ew := s.str("ExchangeWallets", "")
	wallets := []string{}
	if ew != "" {
		for _, t := range strings.Split(ew, ",") {
			t = strings.TrimSpace(t)
			if t != "" {
				wallets = append(wallets, t)
			}
		}
	}
	return Main{
		ExchangeWallets: wallets,
		ShowAllOrders:   s.boolp("ShowAllOrders"),
		FullLog:         s.boolp("FullLog"),
	}
}

func parseCoinConf(name string, kv map[string]string) *CoinConf {
	s := section(kv)
	return &CoinConf{
		Ticker:                    name,
		Title:                     s.str("Title", name),
		Ip:                        s.str("Ip", ""),
		Port:                      s.intp("Port", 0),
		Username:                  s.str("Username", ""),
		Password:                  s.str("Password", ""),
		CreateTxMethod:            s.str("CreateTxMethod", ""),
		AddressPrefix:             s.intp("AddressPrefix", 0),
		ScriptPrefix:             s.intp("ScriptPrefix", 0),
		SecretPrefix:             s.intp("SecretPrefix", 0),
		Coin:                      s.uintp("COIN", 0),
		MinimumAmount:             s.uintp("MinimumAmount", 0),
		TxVersion:                 s.intp("TxVersion", 0),
		DustAmount:                s.uintp("DustAmount", 0),
		MinTxFee:                  s.uintp("MinTxFee", 0),
		BlockTime:                 s.intp("BlockTime", 0),
		FeePerByte:                s.uintp("FeePerByte", 0),
		Confirmations:             s.intp("Confirmations", 0),
		TxWithTimeField:           s.boolp("TxWithTimeField"),
		LockCoinsSupported:        s.boolp("LockCoinsSupported"),
		GetNewKeySupported:        s.boolp("GetNewKeySupported"),
		ImportWithNoScanSupported: s.boolp("ImportWithNoScanSupported"),
		JSONVersion:               s.str("JSONVersion", "1.0"),
		ContentType:               s.str("ContentType", "application/json"),
	}
}
