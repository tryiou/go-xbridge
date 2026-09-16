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
	// RPC bind defaults to loopback only, mirroring blocknetd's
	// httpserver.cpp:308 loopback default. An explicit -rpcbind (host:port) is
	// required to expose the JSON-RPC surface beyond localhost.
	rpcBind := flag.String("rpcbind", "127.0.0.1:41414", "JSON-RPC listen address (host:port); defaults to loopback, set explicitly to bind elsewhere")
	// RPC auth is enforced whenever ANY credential is set —
	// -rpcuser/-rpcpassword (both required) or -rpcauth user:salt$hash entries
	// (comma-separated, HMAC-SHA256 parity with blocknetd).
	rpcUser := flag.String("rpcuser", "", "RPC Basic auth username (requires -rpcpassword)")
	rpcPass := flag.String("rpcpassword", "", "RPC Basic auth password (requires -rpcuser)")
	rpcAuth := flag.String("rpcauth", "", "RPC multi-user auth entries, comma-separated, format user:salt$hash")
	network := flag.String("network", "mainnet", "Blocknet network to discover on and select the P2P magic from: mainnet|testnet|regtest (-magic overrides the magic)")
	nodeAddr := flag.String("node", "", "explicit Blocknet service-node P2P address (host:port); empty enables network discovery")
	addNode := flag.String("addnode", "", "comma-separated explicit peer addresses (host:port) to add to discovered peers")
	confPath := flag.String("conf", defaultConfPath(), "path to xbridge.conf (read-only; never created)")
	magicHex := flag.String("magic", "", "network magic (hex, 4 bytes); derived from -network if empty")
	walletVersion := flag.Int("walletversion", 4040100, "Blocknet CLIENT_VERSION advertised in getnetworkinfo (default Blocknet 4.4.1)")
	walletVersionStr := flag.String("walletversionstr", "/Blocknet:4.4.1/", "Blocknet subversion advertised in getnetworkinfo (default Blocknet 4.4.1)")
	logLevel := flag.String("loglevel", "info", "log verbosity: debug|info|warn|error")
	datadir := flag.String("datadir", "", "directory for xbridged local swap state (incl. per-trade keys); empty uses the OS config dir (~/.config/xbridged, ~/Library/Application Support/xbridged, %AppData%\\xbridged)")
	logFile := flag.String("logfile", "", "log file path; empty defaults to <datadir>/xbridged.log (file logging is always on)")
	rpcServerTimeout := flag.Int("rpcservertimeout", 30, "timeout in seconds for HTTP RPC requests (C++ DEFAULT_HTTP_SERVER_TIMEOUT parity)")
	// -dxnowallets mirrors C++ gArgs.GetBoolArg("-dxnowallets",
	// settings().showAllOrders()) (xbridgeapp.cpp:372): show all orders across
	// the network regardless of local wallets, overriding Main.ShowAllOrders.
	dxnowallets := flag.Bool("dxnowallets", false, "show all orders across the network for non-local wallets (C++ -dxnowallets; overrides Main.ShowAllOrders)")
	// -enableexchange mirrors C++'s flag of the same name (init.cpp:569). It
	// gates Exchange::isEnabled on a service node (settings.cpp:45); xbridged
	// is not a service node and exchange mode is inherent to it, so the flag is
	// accepted for blocknetd CLI parity and is a no-op.
	enableExchange := flag.Bool("enableexchange", false, "accepted for blocknetd CLI parity; exchange mode is inherent for xbridged (no-op)")
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
	// <datadir>/xbridged.log unless -logfile overrides. File logging is
	// always on: an empty -logfile selects the default file.
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

	// Dedicated per-day swap transcript (<datadir>/log-tx/xbridgep2p_*.log,
	// Core log-tx analog): every broadcast deposit/refund/claim lands here
	// with the order id, locktime, and full raw hex for manual recovery. It is
	// always on — unlike the general log, which never carries trade hex.
	if err := xlog.SetTxLogDir(filepath.Join(dataDir, "log-tx")); err != nil {
		fatalf("txlog dir: %v", err)
	}

	// Dedicated packet log (<datadir>/log-p2p/xbridgep2p.log): every xbridge
	// packet send/receive trace lands here instead of the general log. Same
	// size-based rotation as the general log file.
	var pktRw io.Closer
	if pktR, err := xlog.SetP2PLogFile(filepath.Join(dataDir, "log-p2p", "xbridgep2p.log"), 10<<20, 2); err != nil {
		fatalf("p2p log file: %v", err)
	} else {
		pktRw = pktR
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
		case "regtest":
			magic = p2p.RegtestMagic
		default:
			magic = p2p.MainnetMagic
		}
	}

	// Read coin connectors from xbridge.conf (never creates it). Static
	// admission runs first: a coin failing the gates — or a stray
	// section with COIN==0 — is skipped, never fatal, exactly as C++ skips
	// wallets that fail updateActiveWallets (xbridgeapp.cpp:1002-1040).
	conf, err := config.Load(*confPath)
	if err != nil {
		fatalf("xbridge.conf", "err", err)
	}
	admitted := config.Admitted(conf.Coins)
	if err := coins.InitFromConf(admitted); err != nil {
		fatalf("coin config", "err", err)
	}

	// Connect exactly the [Main].ExchangeWallets currencies, applying
	// admission + the live reachability probe. The Activator is passed to the
	// Node so failed-startup wallets stay "bad" for the retry window across the
	// 30s sweep.
	activator := wallet.NewActivator()
	connectors, drops := activator.Activate(admitted, conf.Main.ExchangeWallets, true)
	for _, d := range drops {
		xlog.Warn("wallet not activated", "coin", d.Ticker, "reason", d.Reason)
	}

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
		Confs:            admitted,
		Connectors:       connectors,
		ExchangeWallets:  conf.Main.ExchangeWallets,
		WalletVersion:    *walletVersion,
		WalletVersionStr: *walletVersionStr,
		DataDir:          dataDir,
		ConfPath:         *confPath,
		// ShowAllOrders follows the conf unless -dxnowallets overrides it
		// (C++ showAllOrders() / -dxnowallets, xbridgeapp.cpp:372). Pre-fix
		// the daemon ignored Main.ShowAllOrders entirely until a reload.
		ShowAllOrders:      conf.Main.ShowAllOrders || *dxnowallets,
		ForceShowAllOrders: *dxnowallets,
		// Reachability probe on (faithful updateActiveWallets); disabled in
		// tests only. The startup Activator is shared so failed wallets stay
		// "bad" for the 300s retry window across the 30s sweep.
		CheckReachability: true,
		Activator:         activator,
	}
	node, err := api.NewNode(cfg, store)
	if err != nil {
		xlog.Warn("could not connect to explicit service node; continuing in read-only mode (order book will be empty)",
			"node", *nodeAddr, "err", err)
	}

	ctx := &api.HandlerCtx{Store: store, Node: node}
	srv := api.NewServer(ctx)
	// Auth is enforced whenever ANY credential is configured
	// (-rpcuser+-rpcpassword or -rpcauth). With none, the loopback-default bind
	// is open (documented divergence; docs/api.md §Calling convention).
	var rpcauthList []string
	for _, e := range strings.Split(*rpcAuth, ",") {
		if e = strings.TrimSpace(e); e != "" {
			rpcauthList = append(rpcauthList, e)
		}
	}
	authConfigured := (*rpcUser != "" && *rpcPass != "") || len(rpcauthList) > 0
	if authConfigured {
		if *rpcUser != "" && *rpcPass != "" {
			srv.SetAuth(*rpcUser, *rpcPass)
		}
		if len(rpcauthList) > 0 {
			srv.SetRpcAuth(rpcauthList)
		}
	} else if !isLoopbackAddr(*rpcBind) {
		// Unauthenticated RPC bound beyond loopback is not safe to expose
		// (mirrors blocknetd's warning for RPC without auth).
		xlog.Warn("RPC without authentication is not safe to expose",
			"rpcbind", *rpcBind, "hint", "set -rpcuser/-rpcpassword or -rpcauth")
	}

	if *nodeAddr != "" {
		xlog.Info("xbridged listening", "addr", *rpcBind, "mode", "explicit",
			"node", *nodeAddr, "network", *network, "conf", *confPath, "coins", len(conf.Coins),
			"showallorders", cfg.ShowAllOrders, "enableexchange", *enableExchange)
	} else {
		xlog.Info("xbridged listening", "addr", *rpcBind, "mode", "discovery",
			"network", *network, "conf", *confPath, "coins", len(conf.Coins),
			"showallorders", cfg.ShowAllOrders, "enableexchange", *enableExchange)
	}
	// httpSrv is kept by name so shutdown can drain in-flight RPC handlers
	// (srv.Shutdown) instead of exiting under them. Timeouts: the
	// read/write timeout mirrors C++ -rpcservertimeout (evhttp_set_timeout,
	// httpserver.cpp:393, DEFAULT_HTTP_SERVER_TIMEOUT=30); the header and idle
	// timeouts are Go-side hardening (no C++ counterpart).
	rpcTimeout := time.Duration(*rpcServerTimeout) * time.Second
	httpSrv := &http.Server{
		Addr:              *rpcBind,
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       rpcTimeout,
		WriteTimeout:      rpcTimeout,
		IdleTimeout:       60 * time.Second,
	}
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
	if pktRw != nil {
		if err := pktRw.Close(); err != nil {
			xlog.Error("p2p log close", "err", err)
		}
	}
	xlog.CloseTxLog()
	stopSig()
	os.Exit(0)
}
