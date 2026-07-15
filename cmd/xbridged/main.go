// Command xbridged is the xbridge-go JSON-RPC server — a drop-in replacement
// for blocknetd's dx* RPC surface. Point a dapp's RPC URL at it to reach the
// XBridge API over the live Blocknet service-node P2P network.
package main

import (
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"

	"xbridge-go/api"
)

// mainnetMagic is Blocknet's mainnet network magic (src/chainparams.cpp).
var mainnetMagic = [4]byte{0xa1, 0xa0, 0xa2, 0xa3}

func main() {
	rpcAddr := flag.String("rpcaddr", ":41414", "JSON-RPC listen address")
	nodeAddr := flag.String("node", "coreproxy.airdns.org:42111", "Blocknet service-node P2P address (host:port)")
	keyHex := flag.String("key", "", "hex-encoded 32-byte secp256k1 private key (enables dxMakeOrder/dxTakeOrder/dxCancelOrder)")
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

	store := api.NewStore()
	cfg := &api.Config{
		NodeAddr: *nodeAddr,
		Magic:    magic,
		PrivKey:  priv,
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

	log.Printf("xbridged listening on %s (node=%s, key=%v)", *rpcAddr, *nodeAddr, priv != nil)
	if err := http.ListenAndServe(*rpcAddr, srv); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
