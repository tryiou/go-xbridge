package api

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	xlog "go-xbridge/log"

	"go-xbridge/coins"
	"go-xbridge/config"
	"go-xbridge/crypto"
	"go-xbridge/p2p"
	discovery "go-xbridge/p2p/discovery"
	"go-xbridge/p2p/servicenode"
	"go-xbridge/proto"
	"go-xbridge/wallet"
)

// cancelDedup collapses repeated "cancel for an order we do not track"
// events. The same order cancel is rebroadcast by every peer that relays it, so
// without collapsing a single order would emit one line per peer. The first
// sighting per order is logged; repeats are suppressed and flushed as a
// periodic summary so the underlying condition (orders we never held being
// cancelled network-wide) stays visible without the per-peer spam.
var cancelDedup = xlog.NewDedupe(60*time.Second, func(order string, total int, elapsed time.Duration) {
	xlog.Debug("cancel: order lookup failures suppressed", "order", order[:16], "count", total, "over", elapsed.Round(time.Second).String())
})

// cancelBadSigDedup collapses repeated "bad packet signature" cancel rejections.
// The same malformed cancel is rebroadcast by every relaying servicenode, so a
// single bad order would emit one line per peer. The first sighting (per order)
// is logged; repeats are summarized periodically so the anomaly stays visible
// without spamming. Keyed on the order id, never on any specific currency.
var cancelBadSigDedup = xlog.NewDedupe(60*time.Second, func(order string, total int, elapsed time.Duration) {
	xlog.Info("cancel: bad packet signature suppressed", "order", order[:16], "count", total, "over", elapsed.Round(time.Second).String())
})

// unknownSwapDedup collapses repeated "swap packet for unknown order" DEBUG lines.
// A swap handshake for an order we are not a party to is relayed by every servicenode
// and rebroadcast over time, so a single unknown order would emit one line per copy.
// The first sighting (per order) is logged; repeats are summarized periodically so
// the condition stays visible without spamming. Keyed on the order id, never a coin.
var unknownSwapDedup = xlog.NewDedupe(60*time.Second, func(order string, total int, elapsed time.Duration) {
	xlog.Debug("swap packet for unknown order suppressed", "order", order[:16], "count", total, "over", elapsed.Round(time.Second).String())
})

// cancelNoConnectorDedup collapses repeated "no connector for currency" cancels.
// Every order whose from-currency has no configured connector emits one line; the
// currency (whatever it is, taken from the order at runtime) is the dedup key, so
// distinct missing currencies are reported separately while a single missing
// currency storm is summarized. No coin is hardcoded.
var cancelNoConnectorDedup = xlog.NewDedupe(60*time.Second, func(currency string, total int, elapsed time.Duration) {
	xlog.Warn("cancel: no connector suppressed", "currency", currency, "count", total, "over", elapsed.Round(time.Second).String())
})

// Config tunes a Node.
type Config struct {
	// NodeAddr is the Blocknet service-node P2P address (host:port). Empty
	// disables the live feed (read-only mode with an empty order book).
	NodeAddr string
	// Magic is the network magic (mainnet a1a0a2a3 by default).
	Magic [4]byte
	// Confs holds the parsed [TICKER] sections from xbridge.conf.
	Confs map[string]*config.CoinConf
	// Connectors maps ticker -> the wallet connector go-xbridge drives for it
	// (built from xbridge.conf). Wallet-backed dx* methods use this.
	Connectors map[string]wallet.Connector
	// ExchangeWallets is the local-wallet list from [Main].ExchangeWallets.
	ExchangeWallets []string
	// ShowAllOrders mirrors C++ settings().showAllOrders() / -dxnowallets:
	// when true, dxGetOrders shows every order regardless of whether a wallet
	// connector exists for its currencies (rpcxbridge.cpp:432-446).
	ShowAllOrders bool
	// NetworkTokens is the full set of coins known from xbridge.conf.
	NetworkTokens []string
	// Network is the Blocknet network to discover on: "mainnet" (default),
	// "testnet", or "staging". Used only when NodeAddr is empty (discovery).
	Network string
	// AddNodes are explicit peer addresses (host:port) to connect to in addition
	// to discovered peers (the -addnode flag). Used only when NodeAddr is empty.
	AddNodes []string
	// WalletVersion / WalletVersionStr are advertised in the getnetworkinfo
	// response. BLOCK-DX pings getnetworkinfo for its wallet-version gate; we
	// advertise Blocknet 4.4.1 (CLIENT_VERSION 4040100) by default.
	WalletVersion    int
	WalletVersionStr string
	// DataDir is the directory local swap state (incl. each trade's per-trade
	// M keypair) is persisted to, mirroring C++ orders.dat / loadOrders() /
	// saveOrders(). When empty (the default) it resolves to the OS config dir
	// (<UserConfigDir>/xbridged), so persistence is ON by default — matching
	// C++'s always-on orders.dat. Set via xbridged's -datadir flag.
	DataDir string
	// ConfPath is the path the daemon loaded xbridge.conf from. dxLoadXBridgeConf
	// hot-reloads from this path, mirroring C++'s reload-from-the-same-conf
	// behaviour. Empty disables hot-reload (the call returns an error).
	ConfPath string
	// PersistSecrets controls whether each trade's per-trade M keypair, HTLC
	// secret, and pre-signed refund are written to the swap-state file. On by
	// default, mirroring C++ saveOrders()/orders.dat (an in-flight swap can be
	// rebuilt and auto-refunded post-restart). Set -persistsecrets=false to
	// keep the swap-state file free of signing material (SEC-F04); a restarted
	// mid-flight swap then cannot auto-refund or re-sign a cancel.
	PersistSecrets bool
}

// XConn is the connection surface the Node needs. Both *p2p.Conn (a single
// explicit service node, used when Config.NodeAddr is set) and
// *discovery.PeerManager (an automatically-discovered pool, used when NodeAddr
// is empty) satisfy it, so PeerManager is a drop-in replacement.
type XConn interface {
	ReadPacket() (pkt *proto.Packet, peer string, err error)
	// WritePacket writes an XBridge packet. dest is the 20-byte envelope
	// destination (zero == broadcast; non-zero == addressed to a node's keyId,
	// C++ App::Impl::onSend / net_processing onMessageReceived).
	WritePacket(*proto.Packet, [20]byte) error
	Close() error
}

// Node is the live XBridge client: it maintains a P2P connection to a service
// node (or a discovered pool of them), ingests order broadcasts into the Store,
// and builds/signs/broadcasts the packets for dxMakeOrder / dxTakeOrder /
// dxCancelOrder. It also drives the client side of the three-party swap
// handshake (maker⇄hub⇄taker) when a local order is taken: each SwapSession
// responds to hub-originated packets and builds /broadcasts the HTLC deposits
// and their claim/refund spends.
type Node struct {
	config *Config
	conn   XConn
	store  *Store
	signer crypto.Signer
	stop   chan struct{}

	// sessions is the set of in-flight swaps we are a party to (keyed by
	// order-id hex). The live hub drives each one through its packet sequence;
	// the local SwapSession responds and signs the on-chain ops. Written only
	// by the engine goroutine (and inline by tests, which never start it).
	sessions map[string]*SwapSession

	// tickCount counts engine-ticker ticks so we persist every Nth tick.
	tickCount int

	// engineRunning is true once start() has launched the engine goroutine.
	// Node.submit runs commands inline on the caller when it is false (tests,
	// or a node that never started) — the single-threaded behaviour the test
	// suite relies on.
	engineRunning atomic.Bool

	// Engine channels: packets carries reader→engine traffic, cmds carries
	// handler→engine commands, tasks carries engine→worker wallet RPCs, and
	// results carries worker→engine outcomes. Buffers: packets drop-on-full
	// (a busy engine drops inbound broadcasts, like C++ under load), cmds
	// backpressure (never drop — a lost make/take/cancel is worse than a slow
	// handler), tasks drop-on-full (fund-safe: the refund guard is cleared so
	// the next sweep retries), results == engineWorkers (a result send can
	// never block once the engine is gone).
	packets chan inboundPacket
	cmds    chan engineCmd
	tasks   chan workTask
	results chan workResult

	// wg tracks the engine, reader, workers, blockLoop, and statusLoop so Close
	// can join them all before returning.
	wg sync.WaitGroup

	// pendingRefunds is the engine-owned guard against double-enqueueing a
	// refund broadcast for an order whose sweep task is already in flight.
	pendingRefunds map[string]bool

	// blockMu guards the cached anti-replay blockHash stamped on outgoing
	// orders. C++ uses chainActive.Tip()->pprev (BLOCK best block minus one);
	// we mirror that by querying the BLOCK connector's getblockcount/getblockhash.
	blockMu sync.RWMutex
	block   [32]byte
	blockAt time.Time

	// cfgMu guards the live configuration. dxLoadXBridgeConf hot-reloads it
	// (write lock) while handlers and the feed read it (read lock) via cfg().
	// This keeps a single mutable config slot so a reload never leaves a
	// handler reading a stale copy.
	cfgMu sync.RWMutex

	// snReg is the servicenode registry learned from SNREGISTER / SNPING /
	// SNLISTPING P2P messages (the same wire source a core XBridge wallet
	// uses). dxGetNetworkTokens unions snReg.WalletServices() (the live network
	// token set, C++ walletServices()) with the local config tokens.
	snReg *servicenode.Registry

	// exchangeStarted mirrors C++ Exchange::instance().isStarted(). It is true
	// only when this node runs the exchange/hub role. This thin client never
	// does, so it defaults false; the cancel handler's exchange branch is
	// therefore dead in production but ported verbatim for fidelity (and
	// exercisable in tests via SetExchangeStarted). Atomic so a test flip
	// cannot race the feed goroutine reading it in onRemoteCancel.
	exchangeStarted atomic.Bool
}

// NewNode dials the configured peer (if any) and starts ingesting broadcasts.
func NewNode(cfg *Config, store *Store) (*Node, error) {
	n := &Node{
		config:   cfg,
		store:    store,
		signer:   crypto.NewBtcSigner(),
		stop:     make(chan struct{}),
		sessions: map[string]*SwapSession{},
		snReg:    servicenode.NewRegistry(),
	}

	// Restore local swaps persisted to DataDir (mirrors C++ loadOrders). This
	// runs BEFORE the dial so a swap-loaded node survives even when the service
	// node is unreachable (read-only mode) — matching C++'s restart behaviour.
	if cfg.DataDir != "" {
		if ps, err := loadSwaps(swapStatePath(cfg.DataDir)); err != nil {
			xlog.Warn("could not load persisted swaps; starting fresh", "dir", cfg.DataDir, "err", err)
		} else if len(ps) > 0 {
			for _, p := range ps {
				n.restoreSwap(p)
			}
			xlog.Info("restored local swaps from disk", "count", len(ps), "dir", cfg.DataDir)
		}
	}

	if cfg.NodeAddr != "" {
		// Explicit single-peer override: skip discovery, dial the given
		// service node directly (the legacy behaviour).
		conn, err := p2p.Dial(cfg.NodeAddr, cfg.Magic, 30*time.Second)
		if err != nil {
			return n, fmt.Errorf("api: dial service node: %w", err)
		}
		// Route raw servicenode P2P messages (snr/snp/snlp) into the
		// registry; Conn.ReadPacket would otherwise skip them. This mirrors
		// how a core XBridge wallet learns the network token set.
		conn.OnNonXBridge = func(cmd string, payload []byte) {
			switch cmd {
			case servicenode.CmdSNRegister:
				if sn, derr := servicenode.ParseServiceNode(payload); derr == nil {
					n.snReg.AddRegistration(sn)
				} else {
					xlog.Warn("servicenode: SNREGISTER parse failed", "err", derr)
				}
			case servicenode.CmdSNPing, servicenode.CmdSNListPing:
				if sn, derr := servicenode.ParseServiceNodePing(payload); derr == nil {
					n.snReg.AddPing(sn)
				} else {
					xlog.Warn("servicenode: SNPING/SNLISTPING parse failed", "err", derr)
				}
			}
		}
		n.conn = conn
		xlog.Info("connected to explicit service node", "addr", cfg.NodeAddr)
	} else {
		// No explicit node: discover the Blocknet P2P network like a core
		// wallet would — pick seeds, connect, then gossip to learn peers.
		network := cfg.Network
		if network == "" {
			network = "mainnet"
		}
		pm := discovery.New(cfg.Magic, network, discovery.Options{
			ExplicitAddrs: cfg.AddNodes,
			TargetPeers:   8,
		})
		pm.Start(context.Background())
		n.snReg = pm.ServiceNodes()
		n.conn = pm
		xlog.Info("network discovery started", "network", network, "targetPeers", 8)
	}
	n.start()
	return n, nil
}

// cfg returns the live configuration under a read lock. All config reads must
// go through this accessor so dxLoadXBridgeConf's hot-reload (which swaps the
// pointer under the write lock) is safe against concurrent handler access.
func (n *Node) cfg() *Config {
	n.cfgMu.RLock()
	defer n.cfgMu.RUnlock()
	return n.config
}

// reloadConf hot-reloads xbridge.conf from ConfPath, mirroring C++'s
// dxLoadXBridgeConf (re-read the same conf the daemon started with and rebuild
// the coin registry + wallet connectors). The fresh *Config is swapped under
// the cfgMu write lock, so concurrent handlers keep seeing a consistent config
// and never a half-built one. On any load/parse failure the previous config is
// left untouched and the error is returned (the daemon keeps running, as C++
// does on a bad reload).
func (n *Node) reloadConf() error {
	path := n.cfg().ConfPath
	if path == "" {
		return fmt.Errorf("dxLoadXBridgeConf: no conf path configured (daemon started without -conf)")
	}
	conf, err := config.Load(path)
	if err != nil {
		return fmt.Errorf("dxLoadXBridgeConf: load %s: %w", path, err)
	}
	if err := coins.InitFromConf(conf.Coins); err != nil {
		return fmt.Errorf("dxLoadXBridgeConf: coin registry: %w", err)
	}
	connectors := map[string]wallet.Connector{}
	for ticker, cc := range conf.Coins {
		conn, cerr := wallet.NewConnectorFromConf(cc)
		if cerr != nil {
			xlog.Warn("connector not configured after reload", "coin", ticker, "err", cerr)
			continue
		}
		connectors[ticker] = conn
	}
	networkTokens := make([]string, 0, len(conf.Coins))
	for t := range conf.Coins {
		networkTokens = append(networkTokens, t)
	}
	sort.Strings(networkTokens)

	fresh := &Config{
		NodeAddr:         n.cfg().NodeAddr,
		Magic:            n.cfg().Magic,
		Confs:            conf.Coins,
		Connectors:       connectors,
		ExchangeWallets:  conf.Main.ExchangeWallets,
		ShowAllOrders:    conf.Main.ShowAllOrders,
		NetworkTokens:    networkTokens,
		Network:          n.cfg().Network,
		AddNodes:         n.cfg().AddNodes,
		WalletVersion:    n.cfg().WalletVersion,
		WalletVersionStr: n.cfg().WalletVersionStr,
		DataDir:          n.cfg().DataDir,
		ConfPath:         path,
	}
	n.cfgMu.Lock()
	n.config = fresh
	n.cfgMu.Unlock()
	xlog.Info("reloaded xbridge.conf", "path", path, "coins", len(conf.Coins))
	return nil
}

// blockConnector returns the connector for the BLOCK chain, whose block height
// backs XBridge order anti-replay (C++ stamps chainActive.Tip()->pprev).
func (n *Node) blockConnector() wallet.Connector {
	if n.cfg() == nil || n.cfg().Connectors == nil {
		return nil
	}
	return n.cfg().Connectors["BLOCK"]
}

// connector returns the wallet connector for ticker, or an *rpcError when none
// is configured. This centralizes the nil/lookup check so swap handlers never
// index n.cfg().Connectors[t] unguarded — an absent connector must surface as an
// error, not as a nil-interface panic on the feed goroutine.
func (n *Node) connector(t string) (wallet.Connector, error) {
	if n.cfg() == nil || n.cfg().Connectors == nil {
		return nil, fmt.Errorf("dx: no wallet configured")
	}
	conn, ok := n.cfg().Connectors[t]
	if !ok || conn == nil {
		return nil, fmt.Errorf("dx: no wallet configured for %s", t)
	}
	return conn, nil
}

// relayFeeFor returns the live relay fee (BTC per kB) for a coin's wallet, used
// by effectiveDust to compute C++'s dust threshold (xbridgewalletconnectorbtc.cpp:1526).
// A missing/invalid connector yields 0, letting effectiveDust fall back to the
// conf DustAmount or the C++ 5460 constant.
func (n *Node) relayFeeFor(ticker string) (float64, error) {
	conn, err := n.connector(ticker)
	if err != nil {
		return 0, err
	}
	return conn.GetRelayFee()
}

// blockContext returns the best block height and the first 8 ASCII characters
// of the display-hex block hash for ticker, mirroring C++
// xbridgeapp.cpp:2361-2374 and :2420-2424 (C++ memcpy's the first 8 chars of
// the getblockhash string onto the wire). GetBlockHash returns the internal
// little-endian hash, so the bytes are reversed to display order (uint256::GetHex)
// before the prefix is taken. Any failure is an error — the accept path reverts
// with NO_SESSION (xbridgeapp.cpp:2373); the anti-replay context is mandatory.
func (n *Node) blockContext(ticker string) (height uint32, hash [8]byte, err error) {
	conn, cerr := n.connector(ticker)
	if cerr != nil {
		return 0, [8]byte{}, cerr
	}
	h, herr := conn.GetBlockCount()
	if herr != nil {
		return 0, [8]byte{}, herr
	}
	if h < 1 {
		return 0, [8]byte{}, errors.New("api: block height below 1 for " + ticker)
	}
	if h <= int64(^uint32(0)) {
		height = uint32(h)
	}
	bh, berr := conn.GetBlockHash(h)
	if berr != nil {
		return height, [8]byte{}, berr
	}
	var rev [32]byte
	for i := 0; i < 32; i++ {
		rev[i] = bh[31-i]
	}
	hx := hexEncode(rev[:])
	copy(hash[:], hx[:8])
	return height, hash, nil
}

// availableBalance returns the Blocknet wallet's confirmed balance in native
// base units, mirroring C++ App::availableBalance (xbridgeapp.h:798-806).
// C++ sums CWallet::GetBalance() over GetWallets() — the wallets loaded in the
// running daemon, which for blocknetd is the BLOCK wallet only (wallet/wallet.h
// GetWallets = vpwallets registry). It is NOT the sum over every XBridge coin
// connector: the port must use the BLOCK connector alone. acceptXBridgeTransaction
// uses it for the INSUFFICIENT_FUNDS_DX pre-check (xbridgeapp.cpp:2159-2163);
// with no BLOCK wallet the balance is 0, which fails the take.
func (n *Node) availableBalance() (uint64, error) {
	if n.cfg() == nil || n.cfg().Connectors == nil {
		return 0, errors.New("dx: no wallet configured")
	}
	blk := n.blockConnector()
	if blk == nil {
		return 0, nil
	}
	return blk.GetBalance()
}

// wholeCoinOstream renders a whole-coin amount the way C++ does when streaming
// the UtxoEntry.amount double into a default-constructed std::ostringstream
// (defaultfloat, precision 6) in UtxoEntry::toString(). That is printf %g at 6
// significant digits: trailing zeros stripped, scientific notation when the
// decimal exponent is < -4 or >= 6. strconv.FormatFloat(v, 'g', 6, 64) is
// byte-identical to libstdc++ ostream output for these values.
func wholeCoinOstream(v float64) string {
	return strconv.FormatFloat(v, 'g', 6, 64)
}

// utxoChallenge builds the message an order UTXO is signed over, matching C++
// xbridge::wallet::UtxoEntry::toString() byte-for-byte:
// "txid:vout:amount:address" (xbridgewalletconnector.cpp:25-30), which C++
// signs/verifies via conn->signMessage(entry.address, entry.toString(), sig)
// and conn->verifyMessage(...) (xbridgeapp.cpp:1691/1896/2312,
// xbridgesession.cpp:550/1123). The txid is the raw display hex. The amount is
// the whole-coin double the wallet reported for the output (listunspent
// "value"/getTxOut "value"); both signer and verifier resolve it locally, so
// the strings must be identical for the same confirmed UTXO.
func utxoChallenge(txid string, vout uint32, amount float64, address string) string {
	return txid + ":" + strconv.FormatUint(uint64(vout), 10) + ":" + wholeCoinOstream(amount) + ":" + address
}

// errBadSigLen reports a signmessage proof that is not the 65 bytes XBridge
// requires (compact signature: 1 recovery byte + 64). The C++ writer rejects
// such proofs with INVALID_SIGNATURE (xbridgeapp.cpp:1705).
var errBadSigLen = errors.New("api: signature must be 65 bytes")

// errBadAddr reports a selected utxo whose address cannot be decoded to the
// 20-byte raw id XBridge requires. The C++ writer rejects such entries with
// INVALID_ADDRESS (xbridgeapp.cpp:1710-1713).
var errBadAddr = errors.New("api: utxo address is invalid")

// buildUtxoProofs attaches a BIP137 ownership proof to each spendable UTXO,
// producing the UtxoEntry list carried in order/pending/accepting bodies. Each
// proof is signmessage(address, UtxoEntry::toString()); the counterparty
// verifies it against the UTXO's address via VerifyMessage.
func buildUtxoProofs(conn wallet.Connector, utxos []wallet.Utxo, c coins.Coin) ([]proto.UtxoEntry, error) {
	out := make([]proto.UtxoEntry, 0, len(utxos))
	for _, u := range utxos {
		id, err := reverseTxidHex(u.TxID)
		if err != nil {
			return nil, err
		}
		a, err := c.DecodeAddress(u.Address)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", errBadAddr, err)
		}
		raw, ok := a.ID()
		if !ok {
			return nil, fmt.Errorf("%w: %s has no id", errBadAddr, u.Address)
		}
		sig, err := conn.SignMessage(u.Address, utxoChallenge(u.TxID, u.Vout, u.Value, u.Address))
		if err != nil {
			return nil, err
		}
		if len(sig) != 65 {
			return nil, errBadSigLen
		}
		var s [65]byte
		copy(s[:], sig)
		out = append(out, proto.UtxoEntry{TxID: id, Vout: u.Vout, RawAddress: raw, Signature: s})
	}
	return out, nil
}

// refreshBlock fetches the BLOCK best-block hash (tip-1, mirroring C++) and
// caches it for stamping on outgoing orders. No-op without a BLOCK connector.
func (n *Node) refreshBlock() {
	conn := n.blockConnector()
	if conn == nil {
		return
	}
	count, err := conn.GetBlockCount()
	if err != nil || count < 1 {
		return
	}
	h, err := conn.GetBlockHash(count - 1)
	if err != nil {
		return
	}
	n.blockMu.Lock()
	n.block = h
	n.blockAt = time.Now()
	n.blockMu.Unlock()
}

// currentBlockHash returns the cached anti-replay block hash, refreshing it if
// empty or older than a BLOCK block (~60s).
func (n *Node) currentBlockHash() [32]byte {
	n.blockMu.RLock()
	fresh := n.blockAt.After(time.Now().Add(-60*time.Second)) && n.block != [32]byte{}
	h := n.block
	n.blockMu.RUnlock()
	if fresh {
		return h
	}
	n.refreshBlock()
	n.blockMu.RLock()
	h = n.block
	n.blockMu.RUnlock()
	return h
}

// blockLoop keeps the cached block hash fresh.
func (n *Node) blockLoop() {
	defer n.wg.Done()
	n.refreshBlock()
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-n.stop:
			return
		case <-t.C:
			n.refreshBlock()
		}
	}
}

// handlePacket routes a decoded, signature-verified packet to its handler on
// the engine goroutine. It is the engine-side half of the former feed(): the
// reader loop does the socket read + decode/verify, and the engine applies the
// body type-switch here (onRemoteCancel also needs the raw pkt for
// VerifyAgainst).
func (n *Node) handlePacket(in inboundPacket) {
	body, err := proto.DecodeBody(in.pkt.Command, in.pkt.Body)
	if err != nil {
		// The reader already verified the body parses; a failure here means the
		// packet was mutated between reader and engine. Drop defensively.
		xlog.Warn("engine packet body decode skipped", "command", in.pkt.Command.String(), "err", err)
		return
	}
	switch b := body.(type) {
	case *proto.PendingTransactionBody:
		n.ingestPending(b, in.snode)

	// --- swap handshake (client side, hub-driven) ---
	case *proto.HoldBody:
		n.processSwap(in.pkt, b.ID, b.HubAddress, "Hold", func(s *SwapSession) (proto.XBridgeCommand, responseBody, error) {
			return s.OnHold(b)
		})
	case *proto.InitBody:
		n.processSwap(in.pkt, b.ID, b.HubAddress, "Init", func(s *SwapSession) (proto.XBridgeCommand, responseBody, error) {
			return s.OnInit(b)
		})
	case *proto.CreateABody:
		n.processSwap(in.pkt, b.ID, b.HubAddress, "CreateA", func(s *SwapSession) (proto.XBridgeCommand, responseBody, error) {
			return s.OnCreateA(b)
		})
	case *proto.CreateBBody:
		n.processSwap(in.pkt, b.ID, b.HubAddress, "CreateB", func(s *SwapSession) (proto.XBridgeCommand, responseBody, error) {
			return s.OnCreateB(b)
		})
	case *proto.ConfirmABody:
		n.processSwap(in.pkt, b.ID, b.HubAddress, "ConfirmA", func(s *SwapSession) (proto.XBridgeCommand, responseBody, error) {
			return s.OnConfirmA(b)
		})
	case *proto.ConfirmBBody:
		n.processSwap(in.pkt, b.ID, b.HubAddress, "ConfirmB", func(s *SwapSession) (proto.XBridgeCommand, responseBody, error) {
			return s.OnConfirmB(b)
		})
	case *proto.FinishedBody:
		n.processSwap(in.pkt, b.ID, [20]byte{}, "Finished", func(s *SwapSession) (proto.XBridgeCommand, responseBody, error) {
			return s.OnFinished(b)
		})
	case *proto.CancelBody:
		// Remote cancel (C++ processTransactionCancel). No session/hub
		// needed; handled directly against the store.
		n.handleRemoteCancel(in.pkt, b)
	case *proto.RejectBody:
		// Remote reject (C++ processTransactionReject).
		n.handleRemoteReject(in.pkt, b)
	}
}

// ingestPending adds a hub order broadcast (xbcPendingTransaction) to the store.
// Authenticity is provided by the feed's packet signature verification
// (n.signer.Verify, verified against the header pubkey at C++
// xbridgesession.cpp:736) — there is no GetID(hubAddress) invariant on the
// wire: the 20-byte hub field is the broadcaster's per-session id (m_myid,
// xbridgesession.cpp:182-183) used for routing, NOT GetID of the signing key,
// so it is stored verbatim (OrderDescr: sPubKey = header key,
// hubAddress = m_myId, xbridgesession.cpp:804-811). A relayed copy of a known
// order is never re-created: C++ processPendingTransaction only refreshes the
// timestamp of a known order (xbridgesession.cpp:753-788) — store.Add would
// REPLACE it and drop the local Role/Mine/MakerKey, so the existing record is
// preserved and bumped. A canceled order must NOT be re-accepted via a
// rebroadcast: C++ appendTransaction checks m_historicTransactions and returns
// early (xbridgeapp.cpp:1358), and for active canceled orders only calls
// updateTimestamp (never replacing state).
func (n *Node) ingestPending(b *proto.PendingTransactionBody, snode string) {
	o := normalizeFromPendingBody(b, snode)
	// Touch handles the "known, non-canceled" case (C++ processPendingTransaction).
	if n.store.Touch(hexEncode(o.ID[:])) {
		return
	}
	// Touch returned false: the order is either unknown, canceled-while-still-
	// live, or moved to history. Mirror appendTransaction: a known canceled/
	// historic order must NOT be re-accepted from a network rebroadcast.
	if n.store.HasOrder(hexEncode(o.ID[:])) {
		return
	}
	n.store.Add(o)
}

// logNetworkStatus emits a single aggregated snapshot of discovery health:
// live peers, discovered addresses, known service nodes, and the union of
// network tokens. It is the operator-visible counterpart to the per-packet
// Debug stream, surfaced at Info so it shows at the default log level.
func (n *Node) logNetworkStatus() {
	peers, addrs, snodes := 0, 0, 0
	if pm, ok := n.conn.(*discovery.PeerManager); ok {
		peers = len(pm.Peers())
		addrs = pm.AddrCount()
		if reg := pm.ServiceNodes(); reg != nil {
			snodes = reg.Count()
		}
	}
	xlog.Info("network status",
		"network", n.cfg().Network,
		"peers", peers,
		"addrs", addrs,
		"servicenodes", snodes,
		"tokens", strings.Join(n.NetworkTokens(), ","))
}

// NetworkTokens returns the union of tokens advertised by servicenodes on the
// P2P network (C++ walletServices(), xbridgeapp.cpp:2758) with the locally
// known tokens from xbridge.conf. The servicenode set is learned from
// SNREGISTER / SNPING / SNLISTPING messages via the registry; only SPV-tier
// xbridge tokens matching ^[^:]+$ (excluding xr/xrs) from servicenodes
// pinged within the 5-minute running window are included. When no servicenodes
// are seen it reduces to the config list (static fallback).
func (n *Node) NetworkTokens() []string {
	set := map[string]bool{}
	if reg := n.snReg; reg != nil {
		if ws := reg.WalletServices(); ws != nil {
			for _, t := range ws {
				set[t] = true
			}
		}
	}
	for _, t := range n.cfg().NetworkTokens {
		set[t] = true
	}
	for _, t := range n.cfg().ExchangeWallets {
		set[t] = true
	}
	if len(set) == 0 {
		return []string{}
	}
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// responseBody is implemented by every proto body the swap driver returns.
type responseBody interface {
	Marshal() []byte
}

// dispatchSwap routes a hub-originated handshake packet to the local session for
// the given order id and runs the session handler, submitting the work to the
// engine goroutine (which owns sessions). Packets for order ids we are not a
// party to are ignored. The `hub` parameter (the address the packet embeds) is
// intentionally ignored: the response destination is the hub pinned at session
// creation, never learned from the packet. When the engine is not started
// (tests) the work runs inline on the caller.
func (n *Node) dispatchSwap(pkt *proto.Packet, id [32]byte, hub [20]byte, cmdName string, fn func(*SwapSession) (proto.XBridgeCommand, responseBody, error)) {
	n.submit(func() { n.processSwap(pkt, id, hub, cmdName, fn) }, false)
}

// processSwap is the engine-side body of dispatchSwap: it looks up the session
// for the order id, re-verifies the packet against the session's trusted hub
// key, runs the session handler, and (if it produced a response) signs and
// sends it addressed to the session's pinned hub. Runs on the engine goroutine
// (or inline when the engine is not started).
//
// STATE-F78 (hub-key auth): every handshake packet is re-verified against the
// session's TRUSTED hub key before dispatch — mirroring C++
// xbridgesession.cpp:1364 packet->verify(xtx->sPubKey). The trusted key is
// pinned at session creation for BOTH roles (maker: the hub chosen at make
// time; taker: the order's SNodePubkey) — never learned from network packets.
// A packet that fails verification is dropped and never reaches the handler, so
// a forged Finished can never set csFinished and disable the refund watcher.
func (n *Node) processSwap(pkt *proto.Packet, id [32]byte, hub [20]byte, cmdName string, fn func(*SwapSession) (proto.XBridgeCommand, responseBody, error)) {
	s := n.sessions[hexEncode(id[:])]
	if s == nil {
		if unknownSwapDedup.Event(hexEncode(id[:])) {
			xlog.Debug("swap packet for unknown order", "order", hexEncode(id[:]))
		}
		return
	}
	orderID := hexEncode(id[:])
	swlog := xlog.With("order", orderID)
	if !n.verifyHubPacket(pkt, s) {
		xlog.Warn("swap packet dropped: hub signature not verified", "command", cmdName,
			"state", s.state.String(), "snode", hexEncode(pkt.Pubkey[:]))
		return
	}
	swlog.Info("swap packet received", "command", cmdName, "state", s.state.String())
	// Two-phase handshake: while a deposit/claim task for this session is in
	// flight (await set), the hub sends the next packet only after our response,
	// so any packet arriving now is a retransmit. Drop it rather than re-running
	// stage 1, which would broadcast a second deposit.
	if s.await {
		swlog.Debug("swap packet dropped: handshake task in flight", "command", cmdName)
		return
	}
	// Defense in depth: a malformed/inbound packet must never crash the feed
	// goroutine (which would terminate the whole process). Recover from any
	// panic in the handler and log it; the swap is simply not progressed.
	defer func() {
		if r := recover(); r != nil {
			swlog.Error("recovered panic dispatching swap", "panic", r)
		}
	}()
	cmd, body, err := fn(s)
	if err != nil {
		swlog.Error("swap handler error", "err", err)
		return
	}
	if body == nil {
		swlog.Debug("swap handler produced no response")
		return
	}
	if err := n.send(s.hub, cmd, body, s.privKey[:]); err != nil {
		swlog.Error("swap response send failed", "command", cmd.String(), "err", err)
	} else {
		swlog.Info("swap response sent", "command", cmd.String())
	}
}

// verifyHubPacket authenticates a hub-originated handshake packet against the
// session's trusted hub key (STATE-F78, C++ packet->verify(xtx->sPubKey)). The
// trusted key is pinned at session creation for BOTH roles — the maker's chosen
// servicenode, the taker's order SNodePubkey — never learned from network
// packets. A packet signed by any other key is dropped before it reaches the
// handler. The registry membership of the trusted hub is also re-checked,
// mirroring C++ getSn on every handshake packet (xbridgesession.cpp:1384).
func (n *Node) verifyHubPacket(pkt *proto.Packet, s *SwapSession) bool {
	if s.hubKey == [33]byte{} {
		return false // no trusted hub anchor -> cannot authenticate
	}
	if ok, _ := n.signer.VerifyAgainst(pkt, hexEncode(s.hubKey[:])); !ok {
		return false // packet not signed by the trusted hub key
	}
	return n.hubRegistered(s.hubKey[:])
}

// hubRegistered reports whether the pubkey is a known servicenode, mirroring
// C++ sn::ServiceNodeMgr::getSn (servicenodemgr.h:418-432): a plain snodes map
// lookup with no running filter — findSn (:878-892) is snodes.count(pubkey).
// It is STRICT: an empty registry refuses, exactly like C++ getSn returning
// null for an unknown key. It backs the take gate (xbridgeapp.cpp:2179) and
// the handshake packet gate (xbridgesession.cpp:1384), both of which only
// reject when the node is null. The make path is deliberately different: hub
// selection goes through Pick, which keeps C++'s running() filter
// (findNodeWithService, xbridgeapp.cpp:2910), so a stale-but-known node can
// be taken from but is never selected for a new make.
func (n *Node) hubRegistered(pk []byte) bool {
	reg := n.snReg
	if reg == nil {
		return false
	}
	var key [33]byte
	copy(key[:], pk)
	return reg.Known(key)
}

// send signs and broadcasts a handshake response packet with the swap's
// per-trade M keypair (C++ xtx->mPrivKey).
func (n *Node) send(dest [20]byte, cmd proto.XBridgeCommand, body responseBody, priv []byte) error {
	if n.conn == nil {
		return errors.New("api: not connected to a service node")
	}
	if len(priv) != 32 {
		return errors.New("api: no private key configured")
	}
	pkt := proto.NewPacket(cmd, body.Marshal())
	if err := n.signer.Sign(pkt, priv); err != nil {
		return err
	}
	return n.conn.WritePacket(pkt, dest)
}

// MakeOrderParams are the parsed dxMakeOrder / dxMakePartialOrder arguments.
type MakeOrderParams struct {
	Maker        string
	MakerSize    string
	MakerAddress string
	Taker        string
	TakerSize    string
	TakerAddress string
	Type         string // "exact" or "partial"
	MinSize      string // partial minimum (partial orders only)
	UseAllFunds  bool
	AutoSplit    bool // partial orders only; repost remainder splitting
	Repost       bool // partial orders only; repost remainder after a take
	DryRun       bool
}

func decodeAddr(currency, addrStr string) ([20]byte, *rpcError) {
	c, ok := coins.Get(currency)
	if !ok {
		return [20]byte{}, makeError(errInvalidParameters, "dxMakeOrder", "unsupported currency: "+currency)
	}
	addr, err := c.DecodeAddress(addrStr)
	if err != nil {
		return [20]byte{}, makeError(errInvalidAddress, "dxMakeOrder", addrStr)
	}
	id, ok := addr.ID()
	if !ok {
		return [20]byte{}, makeError(errInvalidAddress, "dxMakeOrder", addrStr)
	}
	return id, nil
}

func (n *Node) requireWrite() *rpcError {
	if n.conn == nil {
		return makeError(errNoServiceNode, "dx", "")
	}
	return nil
}

// MakeOrder builds, signs and (unless dry-run) broadcasts an xbcTransaction
// packet, returning the order in dxMakeOrder's response shape.
func (n *Node) MakeOrder(p MakeOrderParams) (*Order, *rpcError) {
	if e := n.requireWrite(); e != nil {
		return nil, e
	}
	// Reject amounts that are more precise than Blocknet allows (C++
	// xBridgeValidCoin): trailing zeros are ignored, so "25.000000" is ok but
	// "25.1234567" is not.
	if !xBridgeValidCoin(p.MakerSize) {
		return nil, makeError(errInvalidParameters, "dxMakeOrder", "The maker_size is too precise. The maximum precision supported is 6 digits.")
	}
	if !xBridgeValidCoin(p.TakerSize) {
		return nil, makeError(errInvalidParameters, "dxMakeOrder", "The taker_size is too precise. The maximum precision supported is 6 digits.")
	}
	fromAmt, err := parseXAmount(p.MakerSize)
	if err != nil {
		return nil, makeError(errInvalidParameters, "dxMakeOrder", "invalid maker_size")
	}
	toAmt, err := parseXAmount(p.TakerSize)
	if err != nil {
		return nil, makeError(errInvalidParameters, "dxMakeOrder", "invalid taker_size")
	}
	fromID, e := decodeAddr(p.Maker, p.MakerAddress)
	if e != nil {
		return nil, e
	}
	toID, e := decodeAddr(p.Taker, p.TakerAddress)
	if e != nil {
		return nil, e
	}
	// C++ dxMakeOrder: maker_address and taker_address must differ.
	if p.MakerAddress == p.TakerAddress {
		return nil, makeError(errInvalidParameters, "dxMakeOrder", "The maker_address and taker_address cannot be the same: "+p.MakerAddress)
	}
	// C++ upper/lower size limits (MAX_COIN = 100000000 whole coins; min size =
	// 1/COIN, rendered as xBridgeStringValueFromPrice(1.0/COIN) = 0.000001).
	if fromAmt > maxXSize || toAmt > maxXSize {
		return nil, makeError(errInvalidParameters, "dxMakeOrder", "The maximum supported size is 100000000")
	}
	if fromAmt == 0 || toAmt == 0 {
		return nil, makeError(errInvalidParameters, "dxMakeOrder", "The minimum supported size is "+formatXPrice(1.0/float64(coinScale)))
	}
	// C++ makeTransaction: findNodeWithService runs FIRST (xbridgeapp.cpp:1511),
	// before connector/dust checks; no eligible hub fails the order with
	// NO_SERVICE_NODE (:1515). Pick already filters to running, protocol-version
	// matching servicenodes that advertise both currencies (subsuming the
	// getSn re-check at :1518). The chosen hub is pinned for this order: its
	// pubkey/address travel in the SEND envelope and on the Order record.
	//
	// Skipped in dry-run: Go's dxMakeOrder dry-run is a validation/preview
	// extension (C++ has none) that broadcasts nothing and creates no session,
	// so there is no hub to select or protect.
	var hubKey [33]byte
	var hubAddr [20]byte
	if !p.DryRun {
		reg := n.snReg
		if reg == nil {
			// No registry at all: the operator must connect to a service node
			// (or run one) before any make can succeed (C++ :1515).
			xlog.Warn("dxMakeOrder refused: no service-node registry (empty snReg)", "maker", p.Maker, "taker", p.Taker)
			return nil, makeError(errNoServiceNode, "dxMakeOrder", p.Maker+"/"+p.Taker)
		}
		var ok bool
		hubKey, ok = reg.Pick([]string{p.Maker, p.Taker})
		if !ok {
			xlog.Warn("dxMakeOrder refused: no running hub advertising both currencies", "maker", p.Maker, "taker", p.Taker)
			return nil, makeError(errNoServiceNode, "dxMakeOrder", p.Maker+"/"+p.Taker)
		}
		hubAddr = coins.KeyID(hubKey[:])
	}

	// Per-currency connector (NO_SESSION) gate.
	if _, e := n.connector(p.Maker); e != nil {
		return nil, makeError(errNoSession, "dxMakeOrder", "Unable to connect to wallet: "+p.Maker)
	}
	if _, e := n.connector(p.Taker); e != nil {
		return nil, makeError(errNoSession, "dxMakeOrder", "Unable to connect to wallet: "+p.Taker)
	}

	partial := false
	minFrom := fromAmt
	cc := n.cfg().Confs[p.Maker]
	nativeCoin := uint64(coinScale)
	if cc != nil {
		nativeCoin = cc.Coin
	}
	var relayFee float64
	if p.Type == "partial" {
		partial = true
		// C++ reads minimum_size at params[6] (required for partials). A
		// non-parseable value errors, exceeding maker_size errors, and a
		// dust-level minimum errors.
		if p.MinSize == "" {
			return nil, makeError(errInvalidParameters, "dxMakePartialOrder", "minimum_size is required for partial orders")
		}
		m, err := parseXAmount(p.MinSize)
		if err != nil {
			return nil, makeError(errInvalidParameters, "dxMakePartialOrder", "invalid minimum_size")
		}
		minFrom = m
		if minFrom > fromAmt {
			return nil, makeError(errInvalidParameters, "dxMakePartialOrder", "The minimum_size can't be more than maker_size")
		}
		// C++ connFrom->isDustAmount(partialMinimum): base units < configured dust.
		relayFee, _ = n.relayFeeFor(p.Maker)
		if cc != nil && minFrom < effectiveDust(cc, relayFee) {
			return nil, makeError(errInvalidParameters, "dxMakePartialOrder", "The partial minimum_size is dust, i.e. it's too small.")
		}
	}

	// Partial-order prep plan (C++ xbridgeapp.cpp:1571-1604): how many split
	// vouts the autoSplit prep tx will create, the per-vout fee, and whether a
	// remainder vout is required. Computed only for partial orders; the autoSplit
	// branch below consumes them.
	partialUtxosRequiredForMinimum := 0
	partialRemainderRequired := false
	partialPerUtxoFees := uint64(0)
	partialVoutsTotal := uint64(0)
	partialRemainderVoutTotal := uint64(0)
	partialRemainderIsDust := false
	partialOrderVouts := 0
	if partial {
		partialUtxosRequiredForMinimum = int(fromAmt / minFrom)
		if partialUtxosRequiredForMinimum >= maxPartialOrderUtxos {
			partialUtxosRequiredForMinimum = maxPartialOrderUtxos - 1
			partialRemainderRequired = true
		} else if fromAmt%minFrom != 0 {
			partialRemainderRequired = true
		}
		partialFee1 := xBridgeIntFromReal(minTxFeeWhole(cc, 1, 3))
		partialFee2 := xBridgeIntFromReal(minTxFeeWhole(cc, 1, 1))
		partialPerUtxoFees = partialFee1 + partialFee2
		partialFees := uint64(partialUtxosRequiredForMinimum) * partialPerUtxoFees
		if partialRemainderRequired {
			partialFees += partialPerUtxoFees
		}
		partialSplitVoutsTotal := uint64(partialUtxosRequiredForMinimum) * minFrom
		if fromAmt < partialSplitVoutsTotal {
			return nil, makeError(errInsufficientFunds, "dxMakePartialOrder", "insufficient funds for partial order")
		}
		partialRemainderVoutTotal = fromAmt - partialSplitVoutsTotal
		partialRemainderIsDust = isDustNative(xBridgeValueFromAmount(partialRemainderVoutTotal+partialPerUtxoFees), cc, relayFee, nativeCoin)
		partialVoutsTotal = partialFees + partialSplitVoutsTotal
		if partialRemainderRequired && !partialRemainderIsDust {
			partialVoutsTotal += partialRemainderVoutTotal
		}
		partialOrderVouts = partialUtxosRequiredForMinimum
		if partialRemainderRequired && !partialRemainderIsDust {
			partialOrderVouts++
		}
	}

	// Fetch the maker's spendable utxos, excluding those locked by other orders
	// (C++ getAllLockedUtxos :1614 — per-token) and, when use_all_funds is
	// false, those not owned by the maker address (:1621-1627). getUnspent
	// failure fails the order.
	conn, _ := n.connector(p.Maker)
	minConf := 0
	if cc != nil {
		minConf = cc.Confirmations
	}
	locked := n.store.LockedUtxoInfoFor(p.Maker)
	outputs, err := conn.ListUnspent(minConf)
	if err != nil {
		return nil, makeError(errInsufficientFunds, "dxMakeOrder", err.Error())
	}
	filtered := outputs[:0]
	for _, u := range outputs {
		if locked[u.TxID+":"+strconv.FormatUint(uint64(u.Vout), 10)] {
			continue
		}
		if !p.UseAllFunds && u.Address != p.MakerAddress {
			continue
		}
		filtered = append(filtered, u)
	}

	// Select the funding utxos exactly as C++ does (:1636-1682).
	var outputsForUse []wallet.Utxo
	var utxoAmount, fees uint64
	exactMatch := false
	ok := false
	if partial {
		outputsForUse, utxoAmount, fees, exactMatch, ok = selectPartialUtxos(filtered, cc, fromAmt,
			uint64(partialUtxosRequiredForMinimum), partialPerUtxoFees, partialOrderVouts+1, minFrom, partialRemainderVoutTotal)
	} else {
		outputsForUse, utxoAmount, _, ok = selectUtxos(p.MakerAddress, filtered, cc, fromAmt)
	}
	_, _ = utxoAmount, fees
	if !ok {
		return nil, makeError(errInsufficientFunds, "dxMakeOrder", "insufficient funds")
	}

	// Sign the selected utxos; an un-signable wallet, a wrong-length signature,
	// or an undecodable address fails the order (C++ :1689-1715).
	coin, _ := coins.Get(p.Maker)
	proofs, err := buildUtxoProofs(conn, outputsForUse, coin)
	if err != nil {
		if errors.Is(err, errBadSigLen) {
			return nil, makeError(errInvalidSignature, "dxMakeOrder", "incorrect signature length, need 65 bytes")
		}
		if errors.Is(err, errBadAddr) {
			return nil, makeError(errInvalidAddress, "dxMakeOrder", err.Error())
		}
		return nil, makeError(errFundsNotSigned, "dxMakeOrder", err.Error())
	}

	// Capture the anti-replay context once and derive the deterministic order id
	// (C++ :1726-1763): double-SHA256 over the from/to identity, amounts,
	// timestamp, block hash, and the first selected utxo's signature.
	ts := NowMicro()
	bh := n.currentBlockHash()
	id := sha256dOrderID(fromID, p.Maker, fromAmt, toID, p.Taker, toAmt, ts, bh, proofs[0].Signature[:])

	// Generate the per-trade M keypair (C++ xtx->mPubKey/mPrivKey). It signs the
	// make packet and becomes the HTLC DepositorPub; generated here so the
	// wire signing pubkey == the HTLC pubkey by construction.
	mPriv, err := crypto.NewPrivateKey()
	if err != nil {
		return nil, makeError(errUnknown, "dxMakeOrder", err.Error())
	}
	var mPrivArr [32]byte
	copy(mPrivArr[:], mPriv)
	mPub, err := crypto.CompressedPubKey(mPriv)
	if err != nil {
		return nil, makeError(errUnknown, "dxMakeOrder", err.Error())
	}

	// autoSplit: a partial order whose selection is not an exact match of ideal
	// utxos builds a split (prep) transaction so a taker can take a minimum
	// slice, then re-signs and re-hashes the new utxo set (C++ :1783-1953). The
	// order stays pending ("open") until the prep tx confirms.
	pending := false
	var prepTxID string
	if partial {
		if exactMatch {
			if len(outputsForUse) > maxPartialOrderUtxos {
				return nil, makeError(errInvalidAmount, "dxMakePartialOrder", "failed to create order, the maximum number of utxos on the order was exceeded")
			}
		} else if p.AutoSplit {
			// a) Separate exact utxos; the rest become prep-tx inputs.
			existing := outputsForUse[:0]
			var vins []wallet.Utxo
			vinsTotal := 0.0
			remaining := partialUtxosRequiredForMinimum
			voutsTotal := int64(partialVoutsTotal)
			for _, vin := range outputsForUse {
				if camount(vin) == minFrom+partialPerUtxoFees && remaining > 0 {
					existing = append(existing, vin)
					remaining--
					voutsTotal -= int64(minFrom + partialPerUtxoFees)
					continue
				}
				vinsTotal += vin.Value
				vins = append(vins, vin)
			}
			// b) Plan the prep outputs: the remaining splits, the remainder vout
			//    (if not dust), and the change (C++ :1799-1817).
			var vouts []prepVout
			for i := 0; i < remaining; i++ {
				vouts = append(vouts, prepVout{p.MakerAddress, xBridgeValueFromAmount(minFrom + partialPerUtxoFees)})
			}
			if partialRemainderRequired && !partialRemainderIsDust {
				vouts = append(vouts, prepVout{p.MakerAddress, xBridgeValueFromAmount(partialRemainderVoutTotal + partialPerUtxoFees)})
			}
			changeAmount := vinsTotal - xBridgeValueFromAmount(uint64(voutsTotal)) - minTxFeeWhole(cc, len(vins), len(vouts)+1)
			if changeAmount < 2.220446049250313e-16 {
				return nil, makeError(errInvalidAmount, "dxMakePartialOrder", "failed to create order, insufficient funds on partial order")
			}
			if !isDustNative(changeAmount, cc, relayFee, nativeCoin) {
				vouts = append(vouts, prepVout{p.MakerAddress, changeAmount})
			}
			// c) Build, sign, and (unless dry-run) broadcast the prep tx; the
			//    prep txid is the real tx hash, not the wallet's return (:1820-1844).
			signedHex, txid, rerr := buildPrepTx(conn, cc, coin, vins, vouts)
			if rerr != nil {
				return nil, rerr
			}
			if !p.DryRun {
				if _, err := conn.SendRawTransaction(signedHex); err != nil {
					return nil, makeError(errUnknown, "dxMakePartialOrder", err.Error())
				}
			}
			prepTxID = txid
			// d) Rebuild the used-utxo set: the exact utxos plus enough prep
			//    outputs to cover the order (C++ :1846-1879).
			used := existing
			partialNew := int64(0)
			for i, vo := range vouts {
				if voutsTotal-partialNew <= 0 {
					break
				}
				used = append(used, wallet.Utxo{
					TxID: prepTxID, Vout: uint32(i), Address: p.MakerAddress,
					Value: vo.amount, Amount: uint64(vo.amount * float64(nativeCoin)),
				})
				partialNew += int64(camount(used[len(used)-1]))
			}
			if len(used) > maxPartialOrderUtxos {
				return nil, makeError(errInvalidAmount, "dxMakePartialOrder", "failed to create order, the maximum number of utxos on the order was exceeded")
			}
			if len(used) == 0 {
				return nil, makeError(errInvalidPartialOrder, "dxMakePartialOrder", "failed to create order, cannot lock partial order utxos")
			}
			// e) Re-sign the final set and re-hash the id (:1881-1946).
			finalProofs, err := buildUtxoProofs(conn, used, coin)
			if err != nil {
				if errors.Is(err, errBadSigLen) {
					return nil, makeError(errInvalidSignature, "dxMakePartialOrder", "incorrect signature length, need 65 bytes")
				}
				if errors.Is(err, errBadAddr) {
					return nil, makeError(errInvalidAddress, "dxMakePartialOrder", err.Error())
				}
				return nil, makeError(errFundsNotSigned, "dxMakePartialOrder", err.Error())
			}
			id = sha256dOrderID(fromID, p.Maker, fromAmt, toID, p.Taker, toAmt, ts, bh, finalProofs[0].Signature[:])
			proofs = finalProofs
			pending = true
		}
		// else: !autoSplit && !exactMatch — list immediately with the first id
		// (C++ :1954-1958, repostOrderChange=true).
	}

	body := &proto.OrderBody{
		ID:             id,
		From:           fromID,
		FromCurrency:   p.Maker,
		FromAmount:     fromAmt,
		To:             toID,
		ToCurrency:     p.Taker,
		ToAmount:       toAmt,
		Created:        ts,
		BlockHash:      bh,
		PartialAllowed: partial,
		MinFromAmount:  minFrom,
		Utxos:          proofs,
	}

	pkt := proto.NewPacket(proto.XbcTransaction, body.Marshal())
	if err := n.signer.Sign(pkt, mPriv); err != nil {
		return nil, makeError(errUnknown, "dxMakeOrder", err.Error())
	}
	if p.DryRun {
		xlog.Warn("MakeOrder dry run — order not broadcast", "maker", p.Maker, "taker", p.Taker, "makerSize", p.MakerSize, "takerSize", p.TakerSize)
	}

	o := normalizeFromOrderBody(body, hexEncode(mPub[:]))
	o.MakerAddress = p.MakerAddress
	o.TakerAddress = p.TakerAddress
	// C++ renders block_id via uint256::GetHex (reversed display order).
	o.BlockID = orderIDString(body.BlockHash)
	o.PartialRepost = p.Repost
	o.Mine = true
	o.UtxoCurrency = p.Maker
	// Local maker: set our per-trade M key and original currencies (C++
	// xbridgeapp.cpp:1751,2380). MakerKey is OUR mPubKey (not the snode
	// header); Orig* currencies are the maker-facing pair, restored on reject.
	o.Role = 'A'
	o.MakerKey = hexEncode(mPub[:])
	o.OrigFromCurrency = p.Maker
	o.OrigToCurrency = p.Taker
	if partial {
		if pending {
			// C++ setOrderPending(true): held until the prep tx confirms (:1949).
			// trPending renders as "open"; the prep txid rides on the order.
			o.Status = "open"
			o.PrepTx = prepTxID
		} else {
			// Exact-match and non-autoSplit partials list immediately.
			o.Status = "created"
		}
	} else {
		o.Status = "created"
	}
	if !p.DryRun {
		// C++ OrderDescr: sPubKey/hubAddress are the chosen servicenode, NOT
		// the maker's own key (xbridgeapp.cpp:1734-1735). The header-signed
		// pubkey is only used as the display MakerPubkey above. A dry-run order
		// keeps the normalizer's display defaults.
		o.SNodePubkey = hexEncode(hubKey[:])
		o.HubAddress = hubAddr
		// State mutation (store.Add, session registration, SEND, persist) runs
		// on the engine goroutine, which owns the book and session maps.
		//
		// CONC-F101: the response must render a store snapshot COPY, never the live
		// record. The engine may concurrently write the live order (a relayed
		// self-echo bumps Updated via store.Touch, or a remote cancel writes
		// Status), so returning the live pointer to the HTTP handler would race
		// its makeOrderResponse render. TakeOrder/CancelOrder already use this
		// pattern (n.store.Get inside the engine closure).
		var rerr *rpcError
		var stored *Order
		n.submit(func() {
			n.store.Add(o)
			if pending {
				// Pending autoSplit orders are held locally until the prep tx
				// confirms (C++ :2019 broadcast gate: only broadcast when
				// !isOrderPending() || partialExactUtxoMatch). No SEND, no session.
				n.persist()
				stored = n.store.Get(hexEncode(o.ID[:]))
				return
			}
			// SEND is addressed to the chosen hub's envelope address (C++
			// onSend(ptr->hubAddress, ...), xbridgeapp.cpp:2100). The hub relays
			// it to counterparties as an xbcPendingTransaction broadcast.
			if err := n.conn.WritePacket(pkt, hubAddr); err != nil {
				rerr = makeError(errUnknown, "dxMakeOrder", err.Error())
				return
			}
			// Begin driving the client-side deposit handshake for this order.
			n.newMakerSession(o, p, mPrivArr, mPub)
			// Persist the new local swap (incl. its per-trade M keypair) to disk.
			n.persist()
			stored = n.store.Get(hexEncode(o.ID[:]))
		}, true)
		if rerr != nil {
			return nil, rerr
		}
		return stored, nil
	}
	return o, nil
}

// TakeOrderParams are the parsed dxTakeOrder arguments.
type TakeOrderParams struct {
	ID          string
	FromAddress string
	ToAddress   string
	Amount      string // optional
	DryRun      bool
}

// checkAcceptParams mirrors C++ App::checkAcceptParams (xbridgeapp.cpp:
// 2539-2541) forwarded to checkAmount (:2561-2580): the given currency must
// have a live connector (else NO_SESSION) and its wallet must hold at least
// fromSize in spendable whole-coin utxos (else INSUFFICIENT_FUNDS). The balance
// is WalletConnector::getWalletBalance (xbridgewalletconnector.cpp:52-73): the
// sum of getUnspent whole-coin values at minconf 1, p2pkh only, excluding the
// locked utxo set; a wallet that fails to enumerate unspent reports -1 and so
// fails the check. dxTakeOrder calls it with the taker's sending currency
// (toCurrency) and fromSize (rpcxbridge.cpp:1204). The error message args
// mirror rpcxbridge.cpp:1262-1263 exactly: NO_SESSION carries the bare
// toCurrency ticker, INSUFFICIENT_FUNDS carries fromAddress.
func (n *Node) checkAcceptParams(currency string, fromSize uint64, fromAddress string) *rpcError {
	conn, e := n.connector(currency)
	if e != nil {
		return makeError(errNoSession, "dxTakeOrder", currency)
	}
	outputs, err := conn.ListUnspent(1)
	if err != nil {
		return makeError(errInsufficientFunds, "dxTakeOrder", fromAddress)
	}
	// Balance excludes only the coins locked on this currency (C++ checkAmount
	// getAllLockedUtxos(currency), xbridgeapp.cpp:2574) plus the global fee set.
	lockedKeys := n.store.LockedUtxoInfoFor(currency)
	var balance float64
	for _, u := range outputs {
		if !isP2PKH25(u.ScriptPubKey) {
			continue
		}
		if lockedKeys[u.TxID+":"+strconv.FormatUint(uint64(u.Vout), 10)] {
			continue
		}
		balance += u.Value
	}
	if balance < float64(fromSize)/float64(coinScale) {
		return makeError(errInsufficientFunds, "dxTakeOrder", fromAddress)
	}
	return nil
}

// TakeOrder broadcasts an xbcTransactionAccepting packet for the given order and
// returns the dxTakeOrder response shape. C++ swaps maker/taker before rendering
// the real take (so maker = order's toCurrency), keeps the PRE-swap frame for
// the dryrun result, and recomputes the swap sizes for partial takes via
// xBridgeSourceAmountFromPrice.
func (n *Node) TakeOrder(p TakeOrderParams) (orderListResult, *rpcError) {
	if e := n.requireWrite(); e != nil {
		return orderListResult{}, e
	}
	// C++ dxTakeOrder: from_address and to_address must differ.
	if p.FromAddress == p.ToAddress {
		return orderListResult{}, makeError(errInvalidParameters, "dxTakeOrder", "The from_address and to_address cannot be the same: "+p.FromAddress)
	}
	// C++ parses the id via uint256S (no format check here); an unparseable id
	// yields a null id whose lookup misses, so report not-found.
	key, kerr := orderIDKey(p.ID)
	if kerr != nil {
		return orderListResult{}, makeError(errTxNotFound, "dxTakeOrder", p.ID)
	}
	o := n.store.Get(key)
	if o == nil {
		return orderListResult{}, makeError(errTxNotFound, "dxTakeOrder", p.ID)
	}

	// C++ pre-swap orientation: fromSize = toAmount (taker sends), toSize =
	// fromAmount (taker receives).
	fromSize := o.ToAmount
	toSize := o.FromAmount
	if p.Amount != "" {
		a, err := parseXAmount(p.Amount)
		if err != nil {
			return orderListResult{}, makeError(errInvalidParameters, "dxTakeOrder", "invalid amount")
		}
		// C++ treats a take amount of 0 (and an omitted amount) as a FULL-ORDER
		// take: fromSize/toSize stay at the full order size. Only a positive amount
		// (on a partial order) engages the partial recompute via
		// xBridgeSourceAmountFromPrice.
		if a > 0 {
			if !o.PartialAllowed {
				return orderListResult{}, makeError(errInvalidPartialOrder, "dxTakeOrder", "")
			}
			if a < o.MinFromAmount {
				return orderListResult{}, makeError(errInvalidParameters, "dxTakeOrder", "The minimum amount for this order is: "+formatXAmount(o.MinFromAmount))
			}
			if a > o.FromAmount {
				return orderListResult{}, makeError(errInvalidParameters, "dxTakeOrder", "The maximum amount for this order is: "+formatXAmount(o.FromAmount))
			}
			if a < toSize {
				toSize = a
				fromSize = xBridgeSourceAmountFromPrice(toSize, o.ToAmount, o.FromAmount)
			}
		}
	}

	// C++ dxTakeOrder checkAcceptParams (rpcxbridge.cpp:1204): the taker's
	// sending currency (toCurrency, the swap frame has not swapped yet) must be
	// connected and must hold at least fromSize in spendable whole-coin utxos.
	// It forwards to checkAmount (xbridgeapp.cpp:2561-2580): NO_SESSION for a
	// missing connector, INSUFFICIENT_FUNDS when the balance is below
	// fromSize/COIN. The message args carry toCurrency/fromAddress
	// (rpcxbridge.cpp:1262-1263). This gate runs BEFORE the self-trade,
	// connector and address checks (matching C++ error precedence).
	if e := n.checkAcceptParams(o.ToCurrency, fromSize, p.FromAddress); e != nil {
		return orderListResult{}, e
	}

	// No self-trades.
	if o.Mine {
		return orderListResult{}, makeError(errInvalidParameters, "dxTakeOrder", "Unable to accept your own order.")
	}

	// C++ dxTakeOrder:1216-1217 requires both legs to have live connectors
	// before proceeding (the same NO_SESSION check repeats inside
	// acceptXBridgeTransaction at xbridgeapp.cpp:2137-2140).
	if _, e := n.connector(o.ToCurrency); e != nil {
		return orderListResult{}, makeError(errNoSession, "dxTakeOrder", "Unable to connect to wallet: "+o.ToCurrency)
	}
	if _, e := n.connector(o.FromCurrency); e != nil {
		return orderListResult{}, makeError(errNoSession, "dxTakeOrder", "Unable to connect to wallet: "+o.FromCurrency)
	}

	// C++ validates the taker/maker addresses only AFTER the amount,
	// checkAcceptParams, self-trade and connector gates (rpcxbridge.cpp:
	// 1219-1225 isValidAddress). An invalid address is INVALID_ADDRESS, but a
	// prior gate failure wins.
	fromID, e := decodeAddr(o.ToCurrency, p.FromAddress)
	if e != nil {
		return orderListResult{}, e
	}
	toID, e := decodeAddr(o.FromCurrency, p.ToAddress)
	if e != nil {
		return orderListResult{}, e
	}

	if p.DryRun {
		xlog.Warn("TakeOrder dry run — take not broadcast", "order", p.ID, "fromCur", o.ToCurrency, "toCur", o.FromCurrency)
		return o.toTakeDryrunResult(fromSize, toSize), nil
	}

	// C++ acceptXBridgeTransaction pre-checks (xbridgeapp.cpp:2133-2163): both
	// legs must have live connectors (NO_SESSION), neither amount may be dust
	// (DUST), and the Blocknet wallet balance must cover the service-node fee
	// (INSUFFICIENT_FUNDS_DX). The dust checks live in the ACCEPT path only —
	// the dryrun preview above returns before they ever run (rpcxbridge.cpp:
	// 1227 vs xbridgeapp.cpp:2147-2157).
	ccTo := n.cfg().Confs[o.ToCurrency]
	ccFrom := n.cfg().Confs[o.FromCurrency]
	nativeTo := uint64(coinScale)
	if ccTo != nil {
		nativeTo = ccTo.Coin
	}
	nativeFrom := uint64(coinScale)
	if ccFrom != nil {
		nativeFrom = ccFrom.Coin
	}
	relayTo, _ := n.relayFeeFor(o.ToCurrency)
	relayFrom, _ := n.relayFeeFor(o.FromCurrency)
	if isDustNative(xBridgeValueFromAmount(fromSize), ccTo, relayTo, nativeTo) {
		return orderListResult{}, makeError(errDust, "dxTakeOrder", "taker amount is dust")
	}
	if isDustNative(xBridgeValueFromAmount(toSize), ccFrom, relayFrom, nativeFrom) {
		return orderListResult{}, makeError(errDust, "dxTakeOrder", "maker amount is dust")
	}

	if funds, ferr := n.availableBalance(); ferr != nil {
		return orderListResult{}, makeError(errNoSession, "dxTakeOrder", ferr.Error())
	} else if funds < xBridgeIntFromReal(serviceNodeFeeReal) {
		return orderListResult{}, makeError(errInsufficientFundsDX, "dxTakeOrder", "not accepting order, insufficient funds")
	}

	// C++ acceptXBridgeTransaction (xbridgeapp.cpp:2165-2204): the order is
	// refused with NO_SERVICE_NODE when its sPubKey is not a valid 33-byte
	// servicenode key (:2168, decodePub33 yields a zero key) or when the key is
	// not a known servicenode in the local registry (:2179, getSn null —
	// membership only, no running filter). The session is pinned to that key
	// for the whole swap, so a forged order cannot steer the taker's funds to
	// a key we do not trust. The service-node fee DESTINATION is the registry
	// payment address (snode.getPaymentAddress(), :2196) — never the body's
	// hubAddress.
	hubKey := decodePub33(o.SNodePubkey)
	if !n.hubRegistered(hubKey[:]) {
		return orderListResult{}, makeError(errNoServiceNode, "dxTakeOrder", p.ID)
	}
	feeDest, ok := n.snReg.PaymentAddress(hubKey)
	if !ok {
		return orderListResult{}, makeError(errNoServiceNode, "dxTakeOrder", p.ID)
	}

	// BLOCK service-node fee prep (C++ :2236-2267). Only 25-byte p2pkh UTXOs at
	// minconf 1 fund the fee; locked UTXOs are excluded. Every fee-prep failure
	// maps to INSUFFICIENT_FUNDS (C++ :2240-2264). blk is guaranteed non-nil
	// here: a missing BLOCK connector was already rejected by the
	// availableBalance pre-check above (INSUFFICIENT_FUNDS_DX, balance 0 < fee),
	// and a connector whose GetBalance errors returns NO_SESSION — so the fee
	// prep can only run with a live BLOCK wallet.
	blk := n.blockConnector()
	blkCoin, _ := coins.Get("BLOCK")
	blkConf := n.cfg().Confs["BLOCK"]
	feeUtxoAvail, err := blk.ListUnspent(1)
	if err != nil {
		return orderListResult{}, makeError(errInsufficientFunds, "dxTakeOrder", err.Error())
	}
	// C++ fee-prep excludes getAllLockedUtxos(connFrom->currency) (toCurrency)
	// from the BLOCK fee outputs (:2247); in practice only the global fee set
	// collides, but stay faithful per-token. The same snapshot feeds the
	// funding exclusion below (:2270, same ticker).
	lockedKeys := n.store.LockedUtxoInfoFor(o.ToCurrency)
	feeUtxos := make([]wallet.Utxo, 0, len(feeUtxoAvail))
	for _, u := range feeUtxoAvail {
		if !isP2PKH25(u.ScriptPubKey) {
			continue
		}
		if lockedKeys[u.TxID+":"+strconv.FormatUint(uint64(u.Vout), 10)] {
			continue
		}
		feeUtxos = append(feeUtxos, u)
	}
	info, ierr := feeOrderInfo(o.ID, o.ToCurrency, fromSize, o.FromCurrency, toSize)
	if ierr != nil {
		if errors.Is(ierr, errOrderInfoOverflow) {
			return orderListResult{}, makeError(errInvalidOnchainHist, "dxTakeOrder", ierr.Error())
		}
		return orderListResult{}, makeError(errInsufficientFunds, "dxTakeOrder", ierr.Error())
	}
	rawFeeHex, feeInputs, rerr := buildServiceNodeFeeTx(blk, blkCoin, blkConf, feeDest, info, feeUtxos)
	if rerr != nil {
		return orderListResult{}, rerr
	}
	feeBytes, ferr := hex.DecodeString(rawFeeHex)
	if ferr != nil {
		return orderListResult{}, makeError(errInsufficientFunds, "dxTakeOrder", ferr.Error())
	}

	// Taker funding (C++ :2269-2358): selectUtxos from the to-currency wallet,
	// excluding both the per-token locked set (getAllLockedUtxos(o.ToCurrency),
	// re-fetched after lockFeeUtxos at :2270 — the lockedKeys snapshot above is
	// taken at the same point since nothing locks in between) and the
	// just-selected fee inputs (C++ excludes them via lockFeeUtxos, :2267).
	// Every failure maps to INSUFFICIENT_FUNDS; the proof-signing failures map
	// per C++ :2306-2316.
	// Unspent is enumerated at minconf 1, mirroring C++ getUnspent's rpc::
	// listUnspent with an EMPTY params array (xbridgewalletconnectorbtc.cpp:
	// 1604-1612), i.e. the wallet's default minconf of 1 — NOT the conf
	// Confirmations value. This is the SECOND enumeration of the taker wallet:
	// checkAcceptParams above already ran one via getWalletBalance→getUnspent
	// (xbridgeapp.cpp:2279 -> xbridgewalletconnector.cpp:55), exactly as C++.
	conn, _ := n.connector(o.ToCurrency)
	outputs, err := conn.ListUnspent(1)
	if err != nil {
		return orderListResult{}, makeError(errInsufficientFunds, "dxTakeOrder", err.Error())
	}
	feeKey := make(map[string]bool, len(feeInputs))
	for _, u := range feeInputs {
		feeKey[u.TxID+":"+strconv.FormatUint(uint64(u.Vout), 10)] = true
	}
	filtered := make([]wallet.Utxo, 0, len(outputs))
	for _, u := range outputs {
		// Funding inputs must be 25-byte P2PKH outputs (C++ getUnspent's
		// unspentP2PKH filter, xbridgewalletconnectorbtc.cpp:1605-1638). A
		// P2SH/multisig/OP_RETURN output is not spendable by the deposit path
		// and must never fund a taker entry.
		if !isP2PKH25(u.ScriptPubKey) {
			continue
		}
		k := u.TxID + ":" + strconv.FormatUint(uint64(u.Vout), 10)
		if lockedKeys[k] || feeKey[k] {
			continue
		}
		filtered = append(filtered, u)
	}
	usedCoins, _, _, ok := selectUtxos(p.FromAddress, filtered, ccTo, fromSize)
	if !ok {
		return orderListResult{}, makeError(errInsufficientFunds, "dxTakeOrder", "insufficient funds")
	}
	coin, _ := coins.Get(o.ToCurrency)
	proofs, err := buildUtxoProofs(conn, usedCoins, coin)
	if err != nil {
		if errors.Is(err, errBadSigLen) {
			return orderListResult{}, makeError(errInvalidSignature, "dxTakeOrder", "incorrect signature length, need 65 bytes")
		}
		if errors.Is(err, errBadAddr) {
			return orderListResult{}, makeError(errInvalidAddress, "dxTakeOrder", err.Error())
		}
		return orderListResult{}, makeError(errFundsNotSigned, "dxTakeOrder", err.Error())
	}

	// Block context from both connectors (C++ :2361-2374); any failure reverts
	// with NO_SESSION.
	fromH, fromHash, cerr := n.blockContext(o.ToCurrency)
	if cerr != nil {
		return orderListResult{}, makeError(errNoSession, "dxTakeOrder", cerr.Error())
	}
	toH, toHash, cerr := n.blockContext(o.FromCurrency)
	if cerr != nil {
		return orderListResult{}, makeError(errNoSession, "dxTakeOrder", cerr.Error())
	}

	// Atomic input reservation (C++ state gate + lockFeeUtxos + lockCoins
	// under m_utxosOrderLock, xbridgeapp.cpp:2122-2267). An order with an
	// in-flight take is refused FIRST with BAD_REQUEST ("not accepting, order
	// already accepted", C++ :2122-2125) — mirroring C++'s state-gate check
	// order, so a concurrent take of the same order can never overwrite the
	// first take's reserved keys. The fee inputs and the taker's funding set
	// are otherwise claimed in one step under the store lock, BEFORE any
	// Accepting packet leaves, so a concurrent take of a different order can
	// never double-select the same BLOCK fee utxo or funding utxo. A key
	// collision fails the take like C++'s "cannot reuse utxo inputs".
	// Claim the take's inputs atomically: the BLOCK fee keys as the global fee
	// set and the taker's funding keys (usedCoins) on o.ToCurrency — the same
	// split C++ lockFeeUtxos/lockCoins keep in m_feeUtxos vs m_utxosDict.
	feeList := make([]string, 0, len(feeKey))
	for k := range feeKey {
		feeList = append(feeList, k)
	}
	fundList := make([]string, 0, len(usedCoins))
	for _, u := range usedCoins {
		fundList = append(fundList, u.TxID+":"+strconv.FormatUint(uint64(u.Vout), 10))
	}
	switch n.store.ReserveForTake(key, feeList, fundList, o.ToCurrency) {
	case reserveOrderBusy:
		return orderListResult{}, makeError(errBadRequest, "dxTakeOrder", "not accepting, order already accepted")
	case reserveKeyCollision, reserveOrderGone:
		return orderListResult{}, makeError(errInsufficientFunds, "dxTakeOrder", "cannot reuse utxo inputs")
	}

	acc := &proto.AcceptingBody{
		ID:               o.ID,
		HubAddress:       o.HubAddress,
		ServiceNodeFeeTx: feeBytes,
		From:             fromID,
		FromCurrency:     o.ToCurrency,
		FromAmount:       fromSize,
		FromBlockHeight:  fromH,
		FromBlockHash:    fromHash,
		To:               toID,
		ToCurrency:       o.FromCurrency,
		ToAmount:         toSize,
		ToBlockHeight:    toH,
		ToBlockHash:      toHash,
		Utxos:            proofs,
	}
	// Generate the per-trade M keypair (C++ xtx->mPubKey/mPrivKey), generated
	// at accept time so the wire signing pubkey == the HTLC pubkey by construction.
	tPriv, err := crypto.NewPrivateKey()
	if err != nil {
		n.store.ReleaseReserve(key)
		return orderListResult{}, makeError(errUnknown, "dxTakeOrder", err.Error())
	}
	var tPrivArr [32]byte
	copy(tPrivArr[:], tPriv)
	tPub, err := crypto.CompressedPubKey(tPriv)
	if err != nil {
		n.store.ReleaseReserve(key)
		return orderListResult{}, makeError(errUnknown, "dxTakeOrder", err.Error())
	}
	pkt := proto.NewPacket(proto.XbcTransactionAccepting, acc.Marshal())
	if err := n.signer.Sign(pkt, tPriv); err != nil {
		n.store.ReleaseReserve(key)
		return orderListResult{}, makeError(errUnknown, "dxTakeOrder", err.Error())
	}
	if err := n.conn.WritePacket(pkt, o.HubAddress); err != nil {
		n.store.ReleaseReserve(key)
		return orderListResult{}, makeError(errUnknown, "dxTakeOrder", err.Error())
	}
	// State mutation (store update, session registration, persist) runs on the
	// engine goroutine, which owns the book and session maps. The take applies
	// a TARGETED store update (never *stored = *o) so a concurrent engine-side
	// field update (e.g. a remote cancel) is not clobbered.
	var result *Order
	n.submit(func() {
		// Authoritative re-check on the engine: the order may have been
		// cancelled/removed since the HTTP snapshot.
		if n.store.Get(key) == nil {
			n.store.ReleaseReserve(key)
			return
		}
		now := NowMicro()
		makerKey := hexEncode(tPub[:])
		// Local taker: set our per-trade M key and capture the original
		// (maker-facing) currencies BEFORE the take reorients the order
		// (C++ xbridgeapp.cpp:2380; the From/To swap happens in acc above).
		// On a reject these Orig* values restore the order to pending.
		o.Updated = now
		o.Status = "accepting"
		o.Role = 'B'
		o.MakerKey = makerKey
		o.OrigFromCurrency = o.FromCurrency
		o.OrigToCurrency = o.ToCurrency
		o.Utxos = proofs
		o.UsedCoins = usedCoins
		o.FeeUtxos = feeInputs
		o.UtxoCurrency = o.ToCurrency // taker funding coins live on the take's from-currency
		// Publish the take's mutations to the book under the store lock.
		n.store.Update(key, func(stored *Order) {
			stored.Updated = now
			stored.Status = "accepting"
			stored.Role = 'B'
			stored.MakerKey = makerKey
			stored.OrigFromCurrency = o.FromCurrency
			stored.OrigToCurrency = o.ToCurrency
			stored.Utxos = proofs
			stored.UsedCoins = usedCoins
			stored.FeeUtxos = feeInputs
			stored.UtxoCurrency = o.ToCurrency
		})
		// The committed Utxos/FeeUtxos now carry the reserved keys (LockedUtxoInfo
		// reads them off the order), so the in-flight reservation is exhausted.
		n.store.ReleaseReserve(key)
		// Begin driving the client-side deposit handshake for this taken order.
		n.newTakerSession(o, p, tPrivArr, tPub)
		// Persist the new local swap (incl. its per-trade M keypair) to disk.
		n.persist()
		result = n.store.Get(key)
	}, true)
	if result == nil {
		return orderListResult{}, makeError(errTxNotFound, "dxTakeOrder", p.ID)
	}
	return result.toTakeResult(fromSize, toSize), nil
}

// CancelOrderParams are the parsed dxCancelOrder arguments.
type CancelOrderParams struct {
	ID string
}

// CancelOrder broadcasts an xbcTransactionCancel packet for the given order and
// returns the dxCancelOrder response shape.
func (n *Node) CancelOrder(p CancelOrderParams) (*Order, *rpcError) {
	if e := n.requireWrite(); e != nil {
		return nil, e
	}
	o := n.store.Get(p.ID)
	if o == nil {
		return nil, makeError(errTxNotFound, "dxCancelOrder", p.ID)
	}
	var reason uint32 = 0
	var rerr *rpcError
	// State mutation (cancel packet, store update, refund, persist) runs on the
	// engine goroutine, which owns the session and book maps.
	n.submit(func() {
		// Authoritative re-check on the engine: the order may have been
		// cancelled/removed since the HTTP snapshot.
		if n.store.Get(p.ID) == nil {
			rerr = makeError(errTxNotFound, "dxCancelOrder", p.ID)
			return
		}
		// Cancel is signed with the trade's per-trade M keypair (C++ session
		// sendCancelTransaction uses ptr->mPrivKey). Use the live session if
		// we have one; otherwise there is no key to sign with.
		if e := n.sendCancelTransaction(p.ID, reason); e != nil {
			rerr = e
			return
		}
		now := NowMicro()
		// Publish the cancel to the book under the store lock (targeted, so a
		// concurrent engine-side field update is not clobbered).
		n.store.Update(p.ID, func(stored *Order) {
			stored.Status = "canceled"
			stored.Updated = now
		})
		o.Status = "canceled"
		o.Updated = now
		// C++ dxFlushCancelledOrders renders the flushed "id" as it.id.GetHex()
		// (rpcxbridge.cpp:1483), i.e. display order, so record the display id.
		n.store.RecordCancelled(orderIDString(o.ID), o.Created)
		// Best-effort fund recovery: if a deposit was already broadcast, return
		// it via the pre-signed CLTV refund rather than leaving it locked at
		// the hub. Fire-and-forget; outcomes are logged by the refund apply.
		n.enqueueRefund(p.ID, nil)
		// Persist the cancelled (and possibly refund-broadcast) state so it
		// survives a restart (matches C++ saveOrders). Placed last so the
		// refund guard is captured.
		n.persist()
	}, true)
	if rerr != nil {
		return nil, rerr
	}
	return o, nil
}

// ---------------------------------------------------------------------------
// Cancel/reject plumbing (ports of C++ Session::Impl::processTransactionCancel /
// processTransactionReject and their helpers).
// ---------------------------------------------------------------------------

// SetExchangeStarted toggles the exchange/hub role flag (C++ Exchange::instance()
// .isStarted()). It exists so tests can exercise the cancel handler's exchange
// branch; production never sets it (this is a thin client).
func (n *Node) SetExchangeStarted(v bool) { n.exchangeStarted.Store(v) }

// exchangeStarted mirrors C++ Exchange::instance().isStarted().
func (n *Node) ExchangeStarted() bool { return n.exchangeStarted.Load() }

// sessionFor returns the live swap session for idHex, or nil. It mirrors C++
// processTransactionCancel's pendingTransaction() then transaction() lookup: in
// Go a single sessions map holds both, so a nil result means "no valid
// transaction". Engine-internal (n.sessions is engine-owned).
func (n *Node) sessionFor(idHex string) *SwapSession {
	return n.sessions[idHex]
}

// sendCancelTransaction builds, signs (with the order's per-trade M keypair) and
// broadcasts an xbcTransactionCancel packet. It is the shared wire primitive
// behind both the local dxCancelOrder RPC and the remote-cancel handler (C++
// sendCancelTransaction). The caller is responsible for any local state changes
// (status, history, refund broadcast) — this only performs the packet I/O.
func (n *Node) sendCancelTransaction(idHex string, reason uint32) *rpcError {
	if n.conn == nil {
		return makeError(errNoServiceNode, "dxCancelOrder", "")
	}
	s := n.sessions[idHex]
	if s == nil {
		return makeError(errBadRequest, "dxCancelOrder", "no active session for order")
	}
	body := (&proto.CancelBody{ID: s.id, Reason: reason}).Marshal()
	pkt := proto.NewPacket(proto.XbcTransactionCancel, body)
	if err := n.signer.Sign(pkt, s.privKey[:]); err != nil {
		return makeError(errUnknown, "dxCancelOrder", err.Error())
	}
	if err := n.conn.WritePacket(pkt, [20]byte{}); err != nil {
		return makeError(errUnknown, "dxCancelOrder", err.Error())
	}
	return nil
}

// markStale sets o.Updated ~241 seconds in the past so the servicenode treats
// the order as stale and rebroadcasts it (C++ setUpdateTime(now - 241s),
// xbridgesession.cpp:3380-3381). Used by the local-rebroadcast cancel branch.
func (n *Node) markStale(o *Order) {
	o.Updated = NowMicro() - 241_000_000
}

// onUnlockCoins / onUnlockFeeUtxos are the thin-client equivalents of C++
// xapp.unlockCoins / unlockFeeUtxos. go-xbridge holds no locked-coin registry
// (the locks live in the connected wallet connector), so these are no-op stubs
// kept to mirror the reject call site verbatim.
func (n *Node) onUnlockCoins(o *Order)    {}
func (n *Node) onUnlockFeeUtxos(o *Order) {}

// onRemoteCancel is the public entry point for a hub-originated cancel; it
// submits the work to the engine goroutine (which owns the session and book
// maps). handlePacket calls handleRemoteCancel directly.
func (n *Node) onRemoteCancel(pkt *proto.Packet, b *proto.CancelBody) {
	n.submit(func() { n.handleRemoteCancel(pkt, b) }, false)
}

// handleRemoteCancel ports C++ Session::Impl::processTransactionCancel
// (xbridgesession.cpp:3288-3429) verbatim, including the Exchange branch and
// the state-machine switch. Runs on the engine goroutine.
func (n *Node) handleRemoteCancel(pkt *proto.Packet, b *proto.CancelBody) {
	idHex := hexEncode(b.ID[:])
	o := n.store.Get(idHex)
	if o == nil {
		if cancelDedup.Event(idHex) {
			xlog.Debug("cancel: order lookup failed", "order", idHex)
		}
		return
	}

	// --- Exchange branch (C++ :3316-3341) ---
	if n.exchangeStarted.Load() {
		s := n.sessionFor(idHex)
		if s == nil {
			xlog.Info("cancel: order not valid", "order", idHex)
			return
		}
		if ok, _ := n.signer.VerifyAgainst(pkt, hexEncode(s.pubKey[:])); !ok {
			if ok2, _ := n.signer.VerifyAgainst(pkt, hexEncode(s.theirPub[:])); !ok2 {
				xlog.Info("cancel: invalid packet signature", "order", idHex)
				return
			}
		}
		if err := n.sendCancelTransaction(idHex, b.Reason); err != nil {
			xlog.Error("cancel: send failed", "order", idHex, "err", err)
			return
		}
		xlog.Info("cancel: counterparty requested cancel", "order", idHex)
		return
	}

	// --- Non-exchange branch (C++ :3343-3428) ---
	if o = n.store.Get(idHex); o == nil {
		xlog.Info("cancel: order not found on recheck", "order", idHex)
		return
	}

	// Only Maker, Taker, or Servicenode can cancel (C++ :3351-3357).
	iCanceled := false
	if o.MakerKey != "" {
		if ok, _ := n.signer.VerifyAgainst(pkt, o.MakerKey); ok {
			iCanceled = true
		}
	}
	snOK, _ := n.signer.VerifyAgainst(pkt, o.SNodePubkey)
	othOK, _ := n.signer.VerifyAgainst(pkt, o.OtherPubkey)
	if !snOK && !othOK && !iCanceled {
		if cancelBadSigDedup.Event(idHex) {
			xlog.Info("cancel: bad packet signature for cancelation request on order, not canceling", "order", idHex)
		}
		return
	}

	// Connector gate (C++ :3359-3364). Thin client: if the from-currency has
	// no configured connector we cannot proceed, mirroring the C++ bail-out.
	if _, e := n.connector(o.FromCurrency); e != nil {
		if cancelNoConnectorDedup.Event(o.FromCurrency) {
			xlog.Warn("cancel: no connector for currency, not canceling", "order", idHex, "currency", o.FromCurrency)
		}
		return
	}

	// If local order is still open/pending and WE didn't initiate the cancel,
	// mark stale so it rebroadcasts on another servicenode (C++ :3379-3383).
	if o.Mine && stateOrdinal(o.Status) <= 2 && !iCanceled {
		n.store.Update(idHex, n.markStale)
		xlog.Info("cancel: cancel received, rebroadcasting order on another service node", "order", idHex)
		return
	} else if stateOrdinal(o.Status) < 6 { // no deposits yet (C++ :3384-3388)
		n.store.MoveToHistoryU32(idHex, "canceled", b.Reason, NowMicro())
		xlog.Info("cancel: counterparty cancel request", "order", idHex)
		return
	} else if o.Status == "canceled" { // already canceled (C++ :3389-3391)
		xlog.Info("cancel: already canceled", "order", idHex)
		return
	} else if !o.DepositSent { // cancel if deposit not sent (C++ :3392-3394)
		n.store.Update(idHex, func(o *Order) {
			o.Status = "canceled"
			o.Reason = b.Reason
			o.Updated = NowMicro()
		})
		xlog.Info("cancel: counterparty cancel request", "order", idHex)
		n.persist()
		return
	} else if o.CounterpartyRedeemed { // ignore if counterparty already redeemed (C++ :3395-3397)
		xlog.Info("cancel: counterparty already redeemed, ignore cancel", "order", idHex)
		return
	}

	// If no refund tx is defined, we cannot roll back (C++ :3400-3404).
	if o.RefundTx == "" {
		n.store.Update(idHex, func(o *Order) {
			o.Status = "canceled"
			o.Reason = b.Reason
			o.Updated = NowMicro()
		})
		xlog.Info("cancel: could not find a refund transaction for order", "order", idHex)
		n.persist()
		return
	}

	// Rollback path (C++ :3406-3428).
	n.store.RemovePendingPackets(idHex)
	n.store.Update(idHex, func(o *Order) {
		o.Status = "rolled back"
		o.Reason = b.Reason
		o.Updated = NowMicro()
	})
	if o.RefundTx != "" {
		n.enqueueRefund(idHex, nil)
		// C++ processLater; the background sweep retries on locktime.
	}
	xlog.Info("cancel: rollback initiated", "order", idHex)
	n.persist()
}

// onRemoteReject is the public entry point for a hub-originated reject; it
// submits the work to the engine goroutine. handlePacket calls
// handleRemoteReject directly.
func (n *Node) onRemoteReject(pkt *proto.Packet, b *proto.RejectBody) {
	n.submit(func() { n.handleRemoteReject(pkt, b) }, false)
}

// handleRemoteReject ports C++ Session::Impl::processTransactionReject
// (xbridgesession.cpp:3432-3485) verbatim. It restores the order to pending
// (trPending / "open") and NEVER cancels it. Runs on the engine goroutine.
func (n *Node) handleRemoteReject(pkt *proto.Packet, b *proto.RejectBody) {
	idHex := hexEncode(b.ID[:])
	o := n.store.Get(idHex)
	if o == nil {
		return
	}

	// Only the taker (role 'B') in the accepting phase may be rejected
	// (C++ :3452).
	if o.Role != 'B' || stateOrdinal(o.Status) > 3 {
		return
	}

	// Only the servicenode can reject an order (C++ :3456).
	if ok, _ := n.signer.VerifyAgainst(pkt, o.SNodePubkey); !ok {
		return
	}
	n.store.Update(idHex, func(o *Order) {
		o.Reason = b.Reason
	})
	xlog.Info("reject: order rejected by servicenode", "order", idHex)

	// Restore state on rejection (C++ :3463-3482). All field rewrites happen
	// under the store lock so a concurrent render never sees a torn order.
	n.store.Update(idHex, func(o *Order) {
		o.Reason = b.Reason
		o.Status = "open" // trPending
		o.clearUsedCoins()
	})
	n.onUnlockCoins(o)
	n.onUnlockFeeUtxos(o)
	n.store.RemovePendingPackets(idHex)
	xlog.Info("reject: order restored to pending", "order", idHex)
	n.persist()
}
