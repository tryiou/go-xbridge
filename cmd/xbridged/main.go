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
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"

	"xbridge-go/api"
	"xbridge-go/coins"
	"xbridge-go/config"
	"xbridge-go/wallet"
)

// mainnetMagic is Blocknet's mainnet network magic (src/chainparams.cpp).
var mainnetMagic = [4]byte{0xa1, 0xa0, 0xa2, 0xa3}

func defaultConfPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "xbridge.conf"
	}
	return filepath.Join(home, ".blocknet", "xbridge.conf")
}

func main() {
	rpcAddr := flag.String("rpcaddr", ":41414", "JSON-RPC listen address")
	nodeAddr := flag.String("node", "coreproxy.airdns.org:42111", "Blocknet service-node P2P address (host:port)")
	keyHex := flag.String("key", "", "hex-encoded 32-byte secp256k1 private key (enables dxMakeOrder/dxTakeOrder/dxCancelOrder)")
	confPath := flag.String("conf", defaultConfPath(), "path to xbridge.conf (read-only; never created)")
	magicHex := flag.String("magic", hex.EncodeToString(mainnetMagic[:]), "network magic (hex, 4 bytes)")
	flag.Parse()

	var magic [4]byte
	if b, err := hex.DecodeString(*magicHex); err != nil || len(b) != 4 {
		log.Fatalf("invalid -magic %q (must be 4 hex bytes)", *magicHex)
	} else {
		copy(magic[:], b)
	}

	var priv []byte
	if *keyHex != "" {
		k, err := hex.DecodeString(*keyHex)
		if err != nil || len(k) != 32 {
			log.Fatalf("invalid -key (must be 32 hex bytes): %v", err)
		}
		priv = k
	}

	// Read coin connectors from xbridge.conf (never creates it).
	conf, err := config.Load(*confPath)
	if err != nil {
		log.Fatalf("xbridge.conf: %v", err)
	}
	if err := coins.InitFromConf(conf.Coins); err != nil {
		log.Fatalf("coin config: %v", err)
	}

	connectors := map[string]wallet.Connector{}
	for ticker, cc := range conf.Coins {
		conn, err := wallet.NewConnectorFromConf(cc)
		if err != nil {
			log.Printf("warning: connector for %s not configured: %v", ticker, err)
			continue
		}
		connectors[ticker] = conn
	}

	networkTokens := make([]string, 0, len(conf.Coins))
	for t := range conf.Coins {
		networkTokens = append(networkTokens, t)
	}
	sort.Strings(networkTokens)

	store := api.NewStore()
	cfg := &api.Config{
		NodeAddr:       *nodeAddr,
		Magic:          magic,
		PrivKey:        priv,
		Confs:          conf.Coins,
		Connectors:     connectors,
		ExchangeWallets: conf.Main.ExchangeWallets,
		NetworkTokens:  networkTokens,
	}
	node, err := api.NewNode(cfg, store)
	if err != nil {
		log.Printf("warning: could not connect to service node %s: %v", *nodeAddr, err)
		log.Printf("continuing in read-only mode (order book will be empty)")
	}
	defer func() {
		if node != nil {
			_ = node.Close()
		}
	}()

	ctx := &api.HandlerCtx{Store: store, Node: node, Config: cfg}
	srv := api.NewServer(ctx)

	log.Printf("xbridged listening on %s (node=%s, key=%v, conf=%s, coins=%d)",
		*rpcAddr, *nodeAddr, priv != nil, *confPath, len(conf.Coins))
	if err := http.ListenAndServe(*rpcAddr, srv); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
