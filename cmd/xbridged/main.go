// Command xbridged is the xbridge-go JSON-RPC server — a drop-in replacement
// for blocknetd's dx* RPC surface. Point a dapp's RPC URL at it to reach the
// XBridge API over the live Blocknet service-node P2P network.
//
// Coin connectors are read from xbridge.conf (the same file core-wallet XBridge
// uses) — xbridge-go never creates or modifies it. Every coin, including BLOCK
// and BTC, is defined entirely by its [TICKER] section there.
package main

import (
	"encoding/hex"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"xbridge-go/api"
	"xbridge-go/coins"
	"xbridge-go/config"
	xlog "xbridge-go/log"
	"xbridge-go/p2p"
	"xbridge-go/wallet"
)

// fatalf logs an error at ERROR level and exits non-zero (slog has no Fatal).
func fatalf(format string, args ...any) {
	xlog.Error(fmt.Sprintf(format, args...))
	os.Exit(1)
}

func defaultConfPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "xbridge.conf"
	}
	return filepath.Join(home, ".blocknet", "xbridge.conf")
}

func main() {
	rpcAddr := flag.String("rpcaddr", ":41414", "JSON-RPC listen address")
	network := flag.String("network", "mainnet", "Blocknet network to discover on: mainnet|testnet|staging (ignored if -node is set)")
	nodeAddr := flag.String("node", "", "explicit Blocknet service-node P2P address (host:port); empty enables network discovery")
	addNode := flag.String("addnode", "", "comma-separated explicit peer addresses (host:port) to add to discovered peers")
	keyHex := flag.String("key", "", "hex-encoded 32-byte secp256k1 private key (enables dxMakeOrder/dxTakeOrder/dxCancelOrder)")
	confPath := flag.String("conf", defaultConfPath(), "path to xbridge.conf (read-only; never created)")
	magicHex := flag.String("magic", "", "network magic (hex, 4 bytes); derived from -network if empty")
	walletVersion := flag.Int("walletversion", 4040100, "Blocknet CLIENT_VERSION advertised in getnetworkinfo (default Blocknet 4.4.1)")
	walletVersionStr := flag.String("walletversionstr", "/blocknet:4.4.1/", "Blocknet subversion advertised in getnetworkinfo (default Blocknet 4.4.1)")
	logLevel := flag.String("loglevel", "info", "log verbosity: debug|info|warn|error")
	flag.Parse()

	if lvl, err := xlog.ParseLevel(*logLevel); err != nil {
		fatalf("%v", err)
	} else {
		xlog.SetLevel(lvl)
	}

	var magic [4]byte
	if *magicHex != "" {
		if b, err := hex.DecodeString(*magicHex); err != nil || len(b) != 4 {
			fatalf("invalid -magic %q (must be 4 hex bytes)", *magicHex)
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

	var priv []byte
	if *keyHex != "" {
		k, err := hex.DecodeString(*keyHex)
		if err != nil || len(k) != 32 {
			fatalf("invalid -key (must be 32 hex bytes): %v", err)
		}
		priv = k
	}

	// Read coin connectors from xbridge.conf (never creates it).
	conf, err := config.Load(*confPath)
	if err != nil {
		fatalf("xbridge.conf: %v", err)
	}
	if err := coins.InitFromConf(conf.Coins); err != nil {
		fatalf("coin config: %v", err)
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
		PrivKey:          priv,
		Confs:            conf.Coins,
		Connectors:       connectors,
		ExchangeWallets:  conf.Main.ExchangeWallets,
		NetworkTokens:    networkTokens,
		WalletVersion:    *walletVersion,
		WalletVersionStr: *walletVersionStr,
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

	ctx := &api.HandlerCtx{Store: store, Node: node, Config: cfg}
	srv := api.NewServer(ctx)

	if *nodeAddr != "" {
		xlog.Info("xbridged listening", "addr", *rpcAddr, "mode", "explicit",
			"node", *nodeAddr, "network", *network, "key", priv != nil, "conf", *confPath, "coins", len(conf.Coins))
	} else {
		xlog.Info("xbridged listening", "addr", *rpcAddr, "mode", "discovery",
			"network", *network, "key", priv != nil, "conf", *confPath, "coins", len(conf.Coins))
	}
	if err := http.ListenAndServe(*rpcAddr, srv); err != nil {
		fatalf("http server: %v", err)
	}
}
