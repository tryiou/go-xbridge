package api

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
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
}

// XConn is the connection surface the Node needs. Both *p2p.Conn (a single
// explicit service node, used when Config.NodeAddr is set) and
// *discovery.PeerManager (an automatically-discovered pool, used when NodeAddr
// is empty) satisfy it, so PeerManager is a drop-in replacement.
type XConn interface {
	ReadPacket() (pkt *proto.Packet, peer string, err error)
	WritePacket(*proto.Packet) error
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

	// sessMu guards sessions, the set of in-flight swaps we are a party to
	// (keyed by order-id hex). The live hub drives each one through its packet
	// sequence; the local SwapSession responds and signs the on-chain ops.
	sessMu   sync.Mutex
	sessions map[string]*SwapSession

	// persistMu serializes persistence to disk (saveSwaps) against concurrent
	// persist() calls from the feed, MakeOrder/TakeOrder/CancelOrder, and the
	// refundWatcher. The actual read of sessions inside saveSwaps takes sessMu.
	persistMu sync.Mutex
	// tickCount counts refundWatcher ticks so we persist every Nth tick.
	tickCount int

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
	// exercisable in tests via SetExchangeStarted).
	exchangeStarted bool
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
			n.sessMu.Lock()
			for _, p := range ps {
				n.restoreSwap(p)
			}
			n.sessMu.Unlock()
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
	go n.feed()
	go n.blockLoop()
	go n.refundWatcher()
	return n, nil
}

// Close stops the feed and closes the connection.
func (n *Node) Close() error {
	select {
	case <-n.stop:
	default:
		close(n.stop)
	}
	if n.conn != nil {
		return n.conn.Close()
	}
	return nil
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

// blockContext returns the best block height and the first 8 bytes of the block
// hash for ticker, mirroring C++ xbridgeapp.cpp:2420 (the chain tip stamped on
// AcceptingBody). A missing/unreachable connector yields zeros — the order is
// still accepted; only the anti-replay context is absent.
func (n *Node) blockContext(ticker string) (height uint32, hash [8]byte) {
	conn, err := n.connector(ticker)
	if err != nil {
		return 0, [8]byte{}
	}
	h, err := conn.GetBlockCount()
	if err != nil || h < 1 {
		return 0, [8]byte{}
	}
	if h <= int64(^uint32(0)) {
		height = uint32(h)
	}
	bh, err := conn.GetBlockHash(h)
	if err != nil {
		return height, [8]byte{}
	}
	copy(hash[:], bh[:8])
	return height, hash
}

// utxoChallenge builds the message an order UTXO is signed over, matching
// C++ xbridge::wallet::UtxoEntry::toString(): "txid:vout:amount:address"
// (xbridgewalletconnector.cpp:28). C++ signs/verifies it via
// conn->signMessage(entry.address, entry.toString(), sig) and
// conn->verifyMessage(entry.address, entry.toString(), sig)
// (xbridgeapp.cpp:1691/1896/2312, xbridgesession.cpp:550/1123). The txid is the
// raw display hex and amount is in base units, exactly as C++ emits.
func utxoChallenge(txid string, vout uint32, amount uint64, address string) string {
	return fmt.Sprintf("%s:%d:%d:%s", txid, vout, amount, address)
}

// buildUtxoProofs attaches a BIP137 ownership proof to each spendable UTXO,
// producing the UtxoEntry list carried in order/pending/accepting bodies. Each
// proof is signmessage(address, "<txid>:<vout>"); the counterparty verifies it
// against the UTXO's address via VerifyMessage.
func buildUtxoProofs(conn wallet.Connector, utxos []wallet.Utxo, c coins.Coin) ([]proto.UtxoEntry, error) {
	out := make([]proto.UtxoEntry, 0, len(utxos))
	for _, u := range utxos {
		id, err := reverseTxidHex(u.TxID)
		if err != nil {
			return nil, err
		}
		a, err := c.DecodeAddress(u.Address)
		if err != nil {
			return nil, err
		}
		raw, ok := a.ID()
		if !ok {
			return nil, fmt.Errorf("api: utxo address %s has no id", u.Address)
		}
		sig, err := conn.SignMessage(u.Address, utxoChallenge(u.TxID, u.Vout, u.Amount, u.Address))
		if err != nil {
			return nil, err
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

// refundWatcher is the fund-safety safety net: it periodically scans live swap
// sessions and auto-broadcasts any pre-signed CLTV refund whose deposit lockTime
// has passed, so a stalled swap never leaves the local deposit permanently locked
// at the hub. Each refund is broadcast at most once (guarded by SwapSession.
// refundDone); the emergency escape hatch is Node.BroadcastRefund.
func (n *Node) refundWatcher() {
	t := time.NewTicker(refundCheckInterval)
	defer t.Stop()
	for {
		select {
		case <-n.stop:
			return
		case <-t.C:
			n.checkRefunds()
			// Mirror C++ saveOrders cadence: flush local swap state to disk
			// periodically so a crash loses at most a few minutes of progress.
			n.tickCount++
			if n.tickCount%4 == 0 {
				n.persist()
			}
		}
	}
}

// feed reads packets from the peer and stores orders.
func (n *Node) feed() {
	// Periodically emit an aggregated network-status snapshot so an operator
	// can see peer/SN health and the live token set at a glance (the
	// per-packet Debug stream is too noisy for that). Stops with the feed.
	go func() {
		tick := time.NewTicker(60 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-n.stop:
				return
			case <-tick.C:
				n.logNetworkStatus()
			}
		}
	}()
	var lastReadErr error
	for {
		select {
		case <-n.stop:
			return
		default:
		}
		pkt, peer, err := n.conn.ReadPacket()
		if err != nil {
			if !errors.Is(err, lastReadErr) {
				xlog.Debug("peer read failed", "peer", peer, "err", err)
				lastReadErr = err
			}
			select {
			case <-n.stop:
				return
			case <-time.After(200 * time.Millisecond):
				continue
			}
		}
		lastReadErr = nil
		body, err := proto.DecodeBody(pkt.Command, pkt.Body)
		if err != nil {
			xlog.Warn("packet body decode skipped", "command", pkt.Command.String(), "err", err)
			continue
		}
		// All traders verify the snode's packet signature against the pubkey
		// in the packet header (C++ xbridgesession.cpp:736, verbatim). A
		// bad signature means the packet was not signed by the claiming
		// servicenode, so it is dropped regardless of command.
		if ok, _ := n.signer.Verify(pkt); !ok {
			snode := hexEncode(pkt.Pubkey[:])
			xlog.Warn("bad snode packet signature", "command", pkt.Command.String(), "snode", snode, "peer", peer)
			continue
		}
		snode := hexEncode(pkt.Pubkey[:])
		xlog.Debug("packet received", "command", pkt.Command.String(), "snode", snode, "peer", peer)
		switch b := body.(type) {
		case *proto.OrderBody:
			o := normalizeFromOrderBody(b, snode)
			n.store.Add(o)
		case *proto.PendingTransactionBody:
			o := normalizeFromPendingBody(b, snode)
			n.store.Add(o)

		// --- swap handshake (client side, hub-driven) ---
		case *proto.HoldBody:
			n.dispatchSwap(b.ID, b.HubAddress, "Hold", func(s *SwapSession) (proto.XBridgeCommand, responseBody, error) {
				return s.OnHold(b)
			})
		case *proto.InitBody:
			n.dispatchSwap(b.ID, b.HubAddress, "Init", func(s *SwapSession) (proto.XBridgeCommand, responseBody, error) {
				return s.OnInit(b)
			})
		case *proto.CreateABody:
			n.dispatchSwap(b.ID, b.HubAddress, "CreateA", func(s *SwapSession) (proto.XBridgeCommand, responseBody, error) {
				return s.OnCreateA(b)
			})
		case *proto.CreateBBody:
			n.dispatchSwap(b.ID, b.HubAddress, "CreateB", func(s *SwapSession) (proto.XBridgeCommand, responseBody, error) {
				return s.OnCreateB(b)
			})
		case *proto.ConfirmABody:
			n.dispatchSwap(b.ID, b.HubAddress, "ConfirmA", func(s *SwapSession) (proto.XBridgeCommand, responseBody, error) {
				return s.OnConfirmA(b)
			})
		case *proto.ConfirmBBody:
			n.dispatchSwap(b.ID, b.HubAddress, "ConfirmB", func(s *SwapSession) (proto.XBridgeCommand, responseBody, error) {
				return s.OnConfirmB(b)
			})
		case *proto.FinishedBody:
			n.dispatchSwap(b.ID, [20]byte{}, "Finished", func(s *SwapSession) (proto.XBridgeCommand, responseBody, error) {
				return s.OnFinished(b)
			})
		case *proto.CancelBody:
			// Remote cancel (C++ processTransactionCancel). No session/hub
			// needed; handled directly against the store.
			n.onRemoteCancel(pkt, b)
		case *proto.RejectBody:
			// Remote reject (C++ processTransactionReject).
			n.onRemoteReject(pkt, b)
		}
	}
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
// the given order id, runs the session handler, and (if it produced a response)
// signs and broadcasts it. Packets for order ids we are not a party to are
// ignored. The optional hub address is recorded on the session so responses are
// addressed correctly.
func (n *Node) dispatchSwap(id [32]byte, hub [20]byte, cmdName string, fn func(*SwapSession) (proto.XBridgeCommand, responseBody, error)) {
	n.sessMu.Lock()
	s := n.sessions[hexEncode(id[:])]
	n.sessMu.Unlock()
	if s == nil {
		if unknownSwapDedup.Event(hexEncode(id[:])) {
			xlog.Debug("swap packet for unknown order", "order", hexEncode(id[:]))
		}
		return
	}
	if hub != ([20]byte{}) {
		s.hub = hub
	}
	orderID := hexEncode(id[:])
	swlog := xlog.With("order", orderID)
	swlog.Info("swap packet received", "command", cmdName, "state", s.state.String())
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
	if err := n.send(cmd, body, s.privKey[:]); err != nil {
		swlog.Error("swap response send failed", "command", cmd.String(), "err", err)
	} else {
		swlog.Info("swap response sent", "command", cmd.String())
	}
}

// send signs and broadcasts a handshake response packet with the swap's
// per-trade M keypair (C++ xtx->mPrivKey).
func (n *Node) send(cmd proto.XBridgeCommand, body responseBody, priv []byte) error {
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
	return n.conn.WritePacket(pkt)
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
	// Per-currency connector (NO_SESSION) gate.
	if _, e := n.connector(p.Maker); e != nil {
		return nil, makeError(errNoSession, "dxMakeOrder", "Unable to connect to wallet: "+p.Maker)
	}
	if _, e := n.connector(p.Taker); e != nil {
		return nil, makeError(errNoSession, "dxMakeOrder", "Unable to connect to wallet: "+p.Taker)
	}

	partial := false
	minFrom := fromAmt
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
		relayFee, _ := n.relayFeeFor(p.Maker)
		if cc := n.cfg().Confs[p.Maker]; cc != nil && minFrom < effectiveDust(cc, relayFee) {
			return nil, makeError(errInvalidParameters, "dxMakePartialOrder", "The partial minimum_size is dust, i.e. it's too small.")
		}
	}

	var id [32]byte
	if _, err := rand.Read(id[:]); err != nil {
		xlog.Error("MakeOrder: rng failure", "err", err)
		return nil, makeError(errUnknown, "dxMakeOrder", "failed to generate order id")
	}

	body := &proto.OrderBody{
		ID:             id,
		From:           fromID,
		FromCurrency:   p.Maker,
		FromAmount:     fromAmt,
		To:             toID,
		ToCurrency:     p.Taker,
		ToAmount:       toAmt,
		Created:        NowMicro(),
		BlockHash:      n.currentBlockHash(),
		PartialAllowed: partial,
		MinFromAmount:  minFrom,
	}

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

	// Attach BIP137 ownership proofs for the maker's spendable UTXOs so a C++
	// counterparty can verify we own the coins (best-effort: a wallet that
	// cannot sign leaves Utxos empty rather than failing the order broadcast).
	if c, e := n.connector(p.Maker); e == nil {
		if cc := n.cfg().Confs[p.Maker]; cc != nil {
			if utxos, e := c.ListUnspent(cc.Confirmations); e == nil && len(utxos) > 0 {
				if coin, ok := coins.Get(p.Maker); ok {
					if proofs, e := buildUtxoProofs(c, utxos, coin); e == nil {
						body.Utxos = proofs
					}
				}
			}
		}
	}

	pkt := proto.NewPacket(proto.XbcTransaction, body.Marshal())
	if err := n.signer.Sign(pkt, mPriv); err != nil {
		return nil, makeError(errUnknown, "dxMakeOrder", err.Error())
	}
	if p.DryRun {
		xlog.Warn("MakeOrder dry run — order not broadcast", "maker", p.Maker, "taker", p.Taker, "makerSize", p.MakerSize, "takerSize", p.TakerSize)
	}
	if !p.DryRun {
		if err := n.conn.WritePacket(pkt); err != nil {
			return nil, makeError(errUnknown, "dxMakeOrder", err.Error())
		}
	}

	o := normalizeFromOrderBody(body, hexEncode(mPub[:]))
	o.MakerAddress = p.MakerAddress
	o.TakerAddress = p.TakerAddress
	o.BlockID = hexEncode(body.BlockHash[:])
	o.PartialRepost = p.Repost
	o.Mine = true
	// Local maker: set our per-trade M key and original currencies (C++
	// xbridgeapp.cpp:1751,2380). MakerKey is OUR mPubKey (not the snode
	// header); Orig* currencies are the maker-facing pair, restored on reject.
	o.Role = 'A'
	o.MakerKey = hexEncode(mPub[:])
	o.OrigFromCurrency = p.Maker
	o.OrigToCurrency = p.Taker
	if p.Type == "partial" {
		o.Status = "open"
	} else {
		o.Status = "created"
	}
	if !p.DryRun {
		n.store.Add(o)
		// Begin driving the client-side deposit handshake for this order.
		n.newMakerSession(o, p, mPrivArr, mPub)
		// Persist the new local swap (incl. its per-trade M keypair) to disk.
		n.persist()
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
	o := n.store.Get(p.ID)
	if o == nil {
		return orderListResult{}, makeError(errTxNotFound, "dxTakeOrder", p.ID)
	}
	fromID, e := decodeAddr(o.ToCurrency, p.FromAddress)
	if e != nil {
		return orderListResult{}, e
	}
	toID, e := decodeAddr(o.FromCurrency, p.ToAddress)
	if e != nil {
		return orderListResult{}, e
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

	// No self-trades.
	if o.Mine {
		return orderListResult{}, makeError(errInvalidParameters, "dxTakeOrder", "Unable to accept your own order.")
	}

	fromH, fromHash := n.blockContext(o.ToCurrency)
	toH, toHash := n.blockContext(o.FromCurrency)
	acc := &proto.AcceptingBody{
		ID:              o.ID,
		From:            fromID,
		FromCurrency:    o.ToCurrency,
		FromAmount:      fromSize,
		FromBlockHeight: fromH,
		FromBlockHash:   fromHash,
		To:              toID,
		ToCurrency:      o.FromCurrency,
		ToAmount:        toSize,
		ToBlockHeight:   toH,
		ToBlockHash:     toHash,
	}
	// Generate the per-trade M keypair (C++ xtx->mPubKey/mPrivKey), generated
	// at accept time so the wire signing pubkey == the HTLC pubkey by construction.
	tPriv, err := crypto.NewPrivateKey()
	if err != nil {
		return orderListResult{}, makeError(errUnknown, "dxTakeOrder", err.Error())
	}
	var tPrivArr [32]byte
	copy(tPrivArr[:], tPriv)
	tPub, err := crypto.CompressedPubKey(tPriv)
	if err != nil {
		return orderListResult{}, makeError(errUnknown, "dxTakeOrder", err.Error())
	}
	pkt := proto.NewPacket(proto.XbcTransactionAccepting, acc.Marshal())
	if err := n.signer.Sign(pkt, tPriv); err != nil {
		return orderListResult{}, makeError(errUnknown, "dxTakeOrder", err.Error())
	}
	if p.DryRun {
		// C++ renders the dryrun result BEFORE the swap and does not broadcast.
		xlog.Warn("TakeOrder dry run — take not broadcast", "order", p.ID, "fromCur", o.ToCurrency, "toCur", o.FromCurrency)
		return o.toTakeDryrunResult(fromSize, toSize), nil
	}
	if err := n.conn.WritePacket(pkt); err != nil {
		return orderListResult{}, makeError(errUnknown, "dxTakeOrder", err.Error())
	}
	o.Updated = NowMicro()
	o.Status = "accepting"
	// Local taker: set our per-trade M key and capture the original
	// (maker-facing) currencies BEFORE the take reorients the order
	// (C++ xbridgeapp.cpp:2380; the From/To swap happens in acc above).
	// On a reject these Orig* values restore the order to pending.
	o.Role = 'B'
	o.MakerKey = hexEncode(tPub[:])
	o.OrigFromCurrency = o.FromCurrency
	o.OrigToCurrency = o.ToCurrency
	// Begin driving the client-side deposit handshake for this taken order.
	n.newTakerSession(o, p, tPrivArr, tPub)
	// Persist the new local swap (incl. its per-trade M keypair) to disk.
	n.persist()
	return o.toTakeResult(fromSize, toSize), nil
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
	// Cancel is signed with the trade's per-trade M keypair (C++ session
	// sendCancelTransaction uses ptr->mPrivKey). Use the live session if we
	// have one; otherwise there is no key to sign with.
	if err := n.sendCancelTransaction(p.ID, reason); err != nil {
		return nil, err
	}
	o.Status = "canceled"
	o.Updated = NowMicro()
	n.store.RecordCancelled(p.ID, o.Created)
	// Best-effort fund recovery: if a deposit was already broadcast, return it
	// via the pre-signed CLTV refund rather than leaving it locked at the hub.
	if o.RefundTx != "" {
		if _, rerr := n.BroadcastRefund(p.ID); rerr != nil {
			xlog.Warn("dxCancelOrder refund broadcast failed", "order", p.ID, "err", rerr)
		}
	}
	// Persist the cancelled (and possibly refund-broadcast) state so it survives
	// a restart (matches C++ saveOrders). Placed last so the refund guard is
	// captured.
	n.persist()
	return o, nil
}

// ---------------------------------------------------------------------------
// Cancel/reject plumbing (ports of C++ Session::Impl::processTransactionCancel /
// processTransactionReject and their helpers).
// ---------------------------------------------------------------------------

// SetExchangeStarted toggles the exchange/hub role flag (C++ Exchange::instance()
// .isStarted()). It exists so tests can exercise the cancel handler's exchange
// branch; production never sets it (this is a thin client).
func (n *Node) SetExchangeStarted(v bool) { n.exchangeStarted = v }

// exchangeStarted mirrors C++ Exchange::instance().isStarted().
func (n *Node) ExchangeStarted() bool { return n.exchangeStarted }

// sessionFor returns the live swap session for idHex, or nil. It mirrors C++
// processTransactionCancel's pendingTransaction() then transaction() lookup: in
// Go a single sessions map holds both, so a nil result means "no valid
// transaction".
func (n *Node) sessionFor(idHex string) *SwapSession {
	n.sessMu.Lock()
	defer n.sessMu.Unlock()
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
	n.sessMu.Lock()
	s := n.sessions[idHex]
	n.sessMu.Unlock()
	if s == nil {
		return makeError(errBadRequest, "dxCancelOrder", "no active session for order")
	}
	body := (&proto.CancelBody{ID: s.id, Reason: reason}).Marshal()
	pkt := proto.NewPacket(proto.XbcTransactionCancel, body)
	if err := n.signer.Sign(pkt, s.privKey[:]); err != nil {
		return makeError(errUnknown, "dxCancelOrder", err.Error())
	}
	if err := n.conn.WritePacket(pkt); err != nil {
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

// onRemoteCancel ports C++ Session::Impl::processTransactionCancel
// (xbridgesession.cpp:3288-3429) verbatim, including the Exchange branch and
// the state-machine switch.
func (n *Node) onRemoteCancel(pkt *proto.Packet, b *proto.CancelBody) {
	idHex := hexEncode(b.ID[:])
	o := n.store.Get(idHex)
	if o == nil {
		if cancelDedup.Event(idHex) {
			xlog.Debug("cancel: order lookup failed", "order", idHex)
		}
		return
	}

	// --- Exchange branch (C++ :3316-3341) ---
	if n.exchangeStarted {
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
		n.markStale(o)
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
		o.Status = "canceled"
		o.Reason = b.Reason
		o.Updated = NowMicro()
		xlog.Info("cancel: counterparty cancel request", "order", idHex)
		n.persist()
		return
	} else if o.CounterpartyRedeemed { // ignore if counterparty already redeemed (C++ :3395-3397)
		xlog.Info("cancel: counterparty already redeemed, ignore cancel", "order", idHex)
		return
	}

	// If no refund tx is defined, we cannot roll back (C++ :3400-3404).
	if o.RefundTx == "" {
		o.Status = "canceled"
		o.Reason = b.Reason
		o.Updated = NowMicro()
		xlog.Info("cancel: could not find a refund transaction for order", "order", idHex)
		n.persist()
		return
	}

	// Rollback path (C++ :3406-3428).
	n.store.RemovePendingPackets(idHex)
	o.Status = "rolled back"
	o.Reason = b.Reason
	o.Updated = NowMicro()
	if o.RefundTx != "" {
		if _, rerr := n.BroadcastRefund(idHex); rerr != nil {
			xlog.Warn("cancel: rollback refund broadcast failed", "order", idHex, "err", rerr)
			// C++ processLater; the background refundWatcher retries on locktime.
		}
	}
	xlog.Info("cancel: rollback initiated", "order", idHex)
	n.persist()
}

// onRemoteReject ports C++ Session::Impl::processTransactionReject
// (xbridgesession.cpp:3432-3485) verbatim. It restores the order to pending
// (trPending / "open") and NEVER cancels it.
func (n *Node) onRemoteReject(pkt *proto.Packet, b *proto.RejectBody) {
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

	o.Reason = b.Reason
	xlog.Info("reject: order rejected by servicenode", "order", idHex)

	// Restore state on rejection (C++ :3463-3482).
	o.Status = "open" // trPending
	n.onUnlockCoins(o)
	n.onUnlockFeeUtxos(o)
	o.clearUsedCoins()
	n.store.RemovePendingPackets(idHex)
	xlog.Info("reject: order restored to pending", "order", idHex)
	n.persist()
}
