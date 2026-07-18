// Command xbridged is the go-xbridge JSON-RPC server — a drop-in replacement
// for blocknetd's dx* RPC surface. Point a dapp's RPC URL at it to reach the
// XBridge API over the live Blocknet service-node P2P network.
//
// Coin connectors are read from xbridge.conf (the same file core-wallet XBridge
// uses) — go-xbridge never creates or modifies it. Every coin, including BLOCK
// and BTC, is defined entirely by its [TICKER] section there.
package main

import (
	"encoding/hex"
	"flag"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go-xbridge/api"
	"go-xbridge/coins"
	"go-xbridge/config"
	xlog "go-xbridge/log"
	"go-xbridge/p2p"
	"go-xbridge/wallet"
)

// fatalf logs an error at ERROR level with the given structured fields and
// exits non-zero (slog has no Fatal). It mirrors the xlog.Error(msg, key,
// value, ...) convention used across the daemon.
func fatalf(msg string, args ...any) {
	xlog.Error(msg, args...)
	os.Exit(1)
}

func defaultConfPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "xbridge.conf"
	}
	return filepath.Join(home, ".blocknet", "xbridge.conf")
}

// resolveDataDir returns dir if non-empty, otherwise the OS config dir joined
// with "xbridged" (cross-platform: ~/.config/xbridged on Linux,
// ~/Library/Application Support/xbridged on macOS,
// %AppData%\xbridged on Windows). It falls back to ".xbridged" in the
// working directory if UserConfigDir is unavailable.
func resolveDataDir(dir string) string {
	if dir != "" {
		return dir
	}
	base, err := os.UserConfigDir()
	if err != nil || base == "" {
		return ".xbridged"
	}
	return filepath.Join(base, "xbridged")
}

func main() {
	rpcAddr := flag.String("rpcaddr", ":41414", "JSON-RPC listen address")
	network := flag.String("network", "mainnet", "Blocknet network to discover on: mainnet|testnet|staging (ignored if -node is set)")
	nodeAddr := flag.String("node", "", "explicit Blocknet service-node P2P address (host:port); empty enables network discovery")
	addNode := flag.String("addnode", "", "comma-separated explicit peer addresses (host:port) to add to discovered peers")
	confPath := flag.String("conf", defaultConfPath(), "path to xbridge.conf (read-only; never created)")
	magicHex := flag.String("magic", "", "network magic (hex, 4 bytes); derived from -network if empty")
	walletVersion := flag.Int("walletversion", 4040100, "Blocknet CLIENT_VERSION advertised in getnetworkinfo (default Blocknet 4.4.1)")
	walletVersionStr := flag.String("walletversionstr", "/blocknet:4.4.1/", "Blocknet subversion advertised in getnetworkinfo (default Blocknet 4.4.1)")
	logLevel := flag.String("loglevel", "info", "log verbosity: debug|info|warn|error")
	datadir := flag.String("datadir", "", "directory for xbridged local swap state (incl. per-trade keys); empty uses the OS config dir (~/.config/xbridged, ~/Library/Application Support/xbridged, %AppData%\\xbridged)")
	logFile := flag.String("logfile", "", "log file path; empty defaults to <datadir>/xbridged.log; set to \"\" to disable file logging")
	flag.Parse()

	if lvl, err := xlog.ParseLevel(*logLevel); err != nil {
		fatalf("%v", err)
	} else {
		xlog.SetLevel(lvl)
	}

	// Ensure the data directory exists before logging to it (it is otherwise
	// only created lazily on the first swap-state save).
	dataDir := resolveDataDir(*datadir)
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		fatalf("cannot create datadir %q: %v", dataDir, err)
	}

	// Set up file logging (stderr remains active). Default to
	// <datadir>/xbridged.log unless -logfile overrides. An empty -logfile
	// disables the file entirely.
	if *logFile != "" {
		rw, err := xlog.SetFileLogger(*logFile, 10<<20, 2)
		if err != nil {
			fatalf("log file: %v", err)
		}
		defer rw.Close()
	} else {
		rw, err := xlog.SetFileLogger(filepath.Join(dataDir, "xbridged.log"), 10<<20, 2)
		if err != nil {
			fatalf("log file: %v", err)
		}
		defer rw.Close()
	}

	var magic [4]byte
	if *magicHex != "" {
		if b, err := hex.DecodeString(*magicHex); err != nil || len(b) != 4 {
			fatalf("invalid -magic", "value", *magicHex, "want", "4 hex bytes")
		} else {
			copy(magic[:], b)
		}
	} else {
		switch *network {
		case "testnet":
			magic = p2p.TestnetMagic
		case "staging":
			magic = p2p.StagingMagic
		default:
			magic = p2p.MainnetMagic
		}
	}

	// Read coin connectors from xbridge.conf (never creates it).
	conf, err := config.Load(*confPath)
	if err != nil {
		fatalf("xbridge.conf", "err", err)
	}
	if err := coins.InitFromConf(conf.Coins); err != nil {
		fatalf("coin config", "err", err)
	}

	connectors := map[string]wallet.Connector{}
	for ticker, cc := range conf.Coins {
		conn, err := wallet.NewConnectorFromConf(cc)
		if err != nil {
			xlog.Warn("connector not configured", "coin", ticker, "err", err)
			continue
		}
		connectors[ticker] = conn
	}

	networkTokens := make([]string, 0, len(conf.Coins))
	for t := range conf.Coins {
		networkTokens = append(networkTokens, t)
	}
	sort.Strings(networkTokens)

	// Explicit peers (-addnode) augment the discovered pool.
	var addNodes []string
	if *addNode != "" {
		for _, a := range strings.Split(*addNode, ",") {
			if a = strings.TrimSpace(a); a != "" {
				addNodes = append(addNodes, a)
			}
		}
	}

	store := api.NewStore()
	cfg := &api.Config{
		NodeAddr:         *nodeAddr,
		Network:          *network,
		AddNodes:         addNodes,
		Magic:            magic,
		Confs:            conf.Coins,
		Connectors:       connectors,
		ExchangeWallets:  conf.Main.ExchangeWallets,
		NetworkTokens:    networkTokens,
		WalletVersion:    *walletVersion,
		WalletVersionStr: *walletVersionStr,
		DataDir:          dataDir,
		ConfPath:         *confPath,
	}
	node, err := api.NewNode(cfg, store)
	if err != nil {
		xlog.Warn("could not connect to explicit service node; continuing in read-only mode (order book will be empty)",
			"node", *nodeAddr, "err", err)
	}
	defer func() {
		if node != nil {
			_ = node.Close()
		}
	}()

	ctx := &api.HandlerCtx{Store: store, Node: node}
	srv := api.NewServer(ctx)

	if *nodeAddr != "" {
		xlog.Info("xbridged listening", "addr", *rpcAddr, "mode", "explicit",
			"node", *nodeAddr, "network", *network, "conf", *confPath, "coins", len(conf.Coins))
	} else {
		xlog.Info("xbridged listening", "addr", *rpcAddr, "mode", "discovery",
			"network", *network, "conf", *confPath, "coins", len(conf.Coins))
	}
	if err := http.ListenAndServe(*rpcAddr, srv); err != nil {
		fatalf("http server", "err", err)
	}
}
