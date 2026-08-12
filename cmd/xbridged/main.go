// Command xbridged is the go-xbridge JSON-RPC server — a drop-in replacement
// for blocknetd's dx* RPC surface. Point a dapp's RPC URL at it to reach the
// XBridge API over the live Blocknet service-node P2P network.
//
// Coin connectors are read from xbridge.conf (the same file core-wallet XBridge
// uses) — go-xbridge never creates or modifies it. Every coin, including BLOCK
// and BTC, is defined entirely by its [TICKER] section there.
package main

import (
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"go-xbridge/api"
	"go-xbridge/coins"
	"go-xbridge/config"
	xlog "go-xbridge/log"
	"go-xbridge/p2p"
	"go-xbridge/wallet"
)

// fatalf logs an error at ERROR level with the given structured fields and
// exits non-zero (slog has no Fatal). It mirrors the xlog.Error(msg, key,
// value, ...) convention used across the daemon. FlushAll runs first so
// pending dedup summaries are not lost on the fatal path (os.Exit skips
// defers).
func fatalf(msg string, args ...any) {
	xlog.Error(msg, args...)
	xlog.FlushAll()
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

// isLoopbackAddr reports whether an addr host:port string binds to a loopback
// interface (localhost, 127.0.0.0/8, or ::1). An empty host (":41414") binds
// all interfaces and is treated as non-loopback.
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

func main() {
	// RPC bind defaults to loopback only (SEC-F01), mirroring blocknetd's
	// httpserver.cpp:308 loopback default. An explicit -rpcbind (host:port) is
	// required to expose the JSON-RPC surface beyond localhost.
	rpcBind := flag.String("rpcbind", "127.0.0.1:41414", "JSON-RPC listen address (host:port); defaults to loopback, set explicitly to bind elsewhere")
	// RPC auth (SEC-F01): enforced only when BOTH -rpcuser and -rpcpassword are set
	// (no cookie fallback). Unauthenticated RPC is safe only because the
	// default bind is loopback; a non-loopback bind with no auth logs a warning.
	rpcUser := flag.String("rpcuser", "", "RPC Basic auth username (requires -rpcpassword)")
	rpcPass := flag.String("rpcpassword", "", "RPC Basic auth password (requires -rpcuser)")
	network := flag.String("network", "mainnet", "Blocknet network to discover on and select the P2P magic from: mainnet|testnet|staging (-magic overrides the magic)")
	nodeAddr := flag.String("node", "", "explicit Blocknet service-node P2P address (host:port); empty enables network discovery")
	addNode := flag.String("addnode", "", "comma-separated explicit peer addresses (host:port) to add to discovered peers")
	confPath := flag.String("conf", defaultConfPath(), "path to xbridge.conf (read-only; never created)")
	magicHex := flag.String("magic", "", "network magic (hex, 4 bytes); derived from -network if empty")
	walletVersion := flag.Int("walletversion", 4040100, "Blocknet CLIENT_VERSION advertised in getnetworkinfo (default Blocknet 4.4.1)")
	walletVersionStr := flag.String("walletversionstr", "/blocknet:4.4.1/", "Blocknet subversion advertised in getnetworkinfo (default Blocknet 4.4.1)")
	logLevel := flag.String("loglevel", "info", "log verbosity: debug|info|warn|error")
	datadir := flag.String("datadir", "", "directory for xbridged local swap state (incl. per-trade keys); empty uses the OS config dir (~/.config/xbridged, ~/Library/Application Support/xbridged, %AppData%\\xbridged)")
	logFile := flag.String("logfile", "", "log file path; empty defaults to <datadir>/xbridged.log; set to \"\" to disable file logging")
	persistSecrets := flag.Bool("persistsecrets", true, "persist per-trade M keypair/secret/pre-signed refund in the swap-state file (default true; C++ orders.dat parity); false keeps the file secret-free, but a restarted mid-flight swap cannot auto-refund or re-sign cancels")
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
	var rw io.Closer
	if *logFile != "" {
		r, err := xlog.SetFileLogger(*logFile, 10<<20, 2)
		if err != nil {
			fatalf("log file: %v", err)
		}
		rw = r
	} else {
		r, err := xlog.SetFileLogger(filepath.Join(dataDir, "xbridged.log"), 10<<20, 2)
		if err != nil {
			fatalf("log file: %v", err)
		}
		rw = r
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
		PersistSecrets:   *persistSecrets,
	}
	node, err := api.NewNode(cfg, store)
	if err != nil {
		xlog.Warn("could not connect to explicit service node; continuing in read-only mode (order book will be empty)",
			"node", *nodeAddr, "err", err)
	}

	ctx := &api.HandlerCtx{Store: store, Node: node}
	srv := api.NewServer(ctx)
	// SEC-F01: RPC auth is enforced only when both -rpcuser and -rpcpassword are
	// provided (no cookie fallback). Either alone is ignored.
	if *rpcUser != "" && *rpcPass != "" {
		srv.SetAuth(*rpcUser, *rpcPass)
	} else if !isLoopbackAddr(*rpcBind) {
		// Unauthenticated RPC bound beyond loopback is not safe to expose
		// (mirrors blocknetd's warning for RPC without auth).
		xlog.Warn("RPC without authentication is not safe to expose",
			"rpcbind", *rpcBind, "hint", "set -rpcuser and -rpcpassword")
	}

	if *nodeAddr != "" {
		xlog.Info("xbridged listening", "addr", *rpcBind, "mode", "explicit",
			"node", *nodeAddr, "network", *network, "conf", *confPath, "coins", len(conf.Coins))
	} else {
		xlog.Info("xbridged listening", "addr", *rpcBind, "mode", "discovery",
			"network", *network, "conf", *confPath, "coins", len(conf.Coins))
	}
	// httpSrv is kept by name so shutdown can drain in-flight RPC handlers
	// (srv.Shutdown) instead of exiting under them.
	httpSrv := &http.Server{Addr: *rpcBind, Handler: srv}
	// Finalization on SIGINT/SIGTERM. os.Exit skips defers, so shutdown steps
	// must run explicitly here: stop the P2P node (peer conns / read-loops),
	// flush pending dedup summaries, then close the log file.
	sigCtx, stopSig := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSig()

	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fatalf("http server", "err", err)
		}
	}()

	<-sigCtx.Done()
	// Drain in-flight RPC handlers before tearing down the node and logging: a
	// handler mid-swap must not race node.Close() (peer conn teardown) or the
	// log-file close that follows.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		xlog.Warn("http server shutdown", "err", err)
	}
	cancel()
	if node != nil {
		if err := node.Close(); err != nil {
			xlog.Warn("node close", "err", err)
		}
	}
	xlog.FlushAll()
	if rw != nil {
		if err := rw.Close(); err != nil {
			xlog.Error("log close", "err", err)
		}
	}
	stopSig()
	os.Exit(0)
}
