package api

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
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

var earlyFinishedDedup = xlog.NewDedupe(60*time.Second, func(order string, total int, elapsed time.Duration) {
	xlog.Warn("early Finished deferred suppressed", "order", order[:16], "count", total, "over", elapsed.Round(time.Second).String())
})

var huntWarnDedup = xlog.NewDedupe(time.Hour, func(order string, total int, elapsed time.Duration) {
	xlog.Warn("secret hunt ongoing suppressed", "order", order[:16], "count", total, "over", elapsed.Round(time.Second).String())
})

func summarizeCancelLookup(order string, total int, elapsed time.Duration) {
	xlog.With("p2p", true).Debug("cancel: order lookup failures suppressed", "order", order[:16], "count", total, "over", elapsed.Round(time.Second).String())
}

var cancelDedup = xlog.NewDedupe(60*time.Second, summarizeCancelLookup)

func summarizeCancelBadSig(order string, total int, elapsed time.Duration) {
	xlog.With("p2p", true).Info("cancel: bad packet signature suppressed", "order", order[:16], "count", total, "over", elapsed.Round(time.Second).String())
}

var cancelBadSigDedup = xlog.NewDedupe(60*time.Second, summarizeCancelBadSig)

var unknownSwapDedup = xlog.NewDedupe(60*time.Second, func(order string, total int, elapsed time.Duration) {
	xlog.Debug("swap packet for unknown order suppressed", "order", order[:16], "count", total, "over", elapsed.Round(time.Second).String())
})

func summarizeCancelNoConnector(currency string, total int, elapsed time.Duration) {
	xlog.With("p2p", true).Warn("cancel: no connector suppressed", "currency", currency, "count", total, "over", elapsed.Round(time.Second).String())
}

var cancelNoConnectorDedup = xlog.NewDedupe(60*time.Second, summarizeCancelNoConnector)

func summarizeCancelAlreadyCanceled(order string, total int, elapsed time.Duration) {
	xlog.With("p2p", true).Info("cancel: already-canceled duplicates suppressed", "order", order, "count", total, "over", elapsed.Round(time.Second).String())
}

func summarizeHandshakeInflight(order string, total int, over time.Duration) {
	xlog.Debug("swap packet dropped: handshake retransmits suppressed", "order", order, "count", total, "over", over.Round(time.Second).String())
}

var handshakeInflightDedup = xlog.NewDedupe(60*time.Second, summarizeHandshakeInflight)

var cancelAlreadyCanceledDedup = xlog.NewDedupe(60*time.Second, summarizeCancelAlreadyCanceled)

func gossipLogger(o *Order) *slog.Logger {
	if o != nil && o.Mine {
		return xlog.L()
	}
	return xlog.With("p2p", true)
}

var walletSweepInterval = 30 * time.Second

// Config tunes a Node.
type Config struct {
	// NodeAddr pins the explicit service node to dial (host:port); empty selects discovery.
	NodeAddr string
	Magic    [4]byte
	// Confs holds the parsed [TICKER] sections from xbridge.conf.
	Confs map[string]*config.CoinConf
	// Connectors maps ticker to the wallet connector built from xbridge.conf.
	Connectors map[string]wallet.Connector
	// ExchangeWallets is the local-wallet list from [Main].ExchangeWallets, minus
	// any coin config.Load disabled for an unusable DustAmount (config.Conf.Wallets).
	ExchangeWallets []string
	// ShowAllOrders shows every order regardless of whether a wallet connector exists for its currencies.
	ShowAllOrders bool
	// ForceShowAllOrders is the -dxnowallets override, kept separate from ShowAllOrders so a
	// hot-reload can recompute ShowAllOrders without losing the daemon-level flag.
	ForceShowAllOrders bool
	// CheckReachability probes each connector on activation, reload, and sweep, dropping
	// wallets that fail the RPC probe.
	CheckReachability bool
	// Activator builds the active connector set; when nil, NewNode creates one.
	Activator *wallet.Activator
	// Network is the discovery network: "mainnet" (default), "testnet", or "regtest";
	// used only when NodeAddr is empty.
	Network string
	// AddNodes are extra peer addresses (host:port) to dial alongside discovered peers;
	// used only when NodeAddr is empty.
	AddNodes []string
	// WalletVersion / WalletVersionStr are advertised in getnetworkinfo, which BLOCK-DX
	// pings for its wallet-version gate.
	WalletVersion    int
	WalletVersionStr string
	// DataDir is where local swap state (incl. each trade's per-trade M keypair) persists;
	// empty resolves to <UserConfigDir>/xbridged, so persistence is on by default.
	DataDir string
	// ConfPath is the conf dxLoadXBridgeConf hot-reloads from; empty disables hot-reload
	// (the RPC returns a false result).
	ConfPath string
	// TakeRetry bounds the crNotAccepted auto-retry per locally-initiated take (0 disables).
	// A rejected take burns no service-node fee, so retries are fee-free up to the cap.
	TakeRetry int
}

// XConn is the packet connection a Node drives: *p2p.Conn or *discovery.PeerManager.
type XConn interface {
	ReadPacket() (pkt *proto.Packet, peer string, err error)
	// WritePacket writes a packet to dest; a zero dest broadcasts instead of addressing a keyId.
	WritePacket(*proto.Packet, [20]byte) error
	Close() error
}

// Node is the live XBridge client: it ingests order broadcasts, drives the dx* write paths, and runs the client side of the swap handshake.
type Node struct {
	config   *Config
	conn     XConn
	store    *Store
	signer   crypto.Signer
	stop     chan struct{}
	stopOnce sync.Once

	activator   *wallet.Activator
	activatorMu sync.Mutex

	// sessions is the set of in-flight swaps we are a party to (keyed by order-id
	// hex), written only by the engine goroutine (and by tests before it starts).
	sessions map[string]*SwapSession

	// silentHubs maps silently-failed hub keys to their exclusion expiry; silentHubsMu
	// guards it because the make path reads it off-engine.
	silentHubsMu sync.Mutex
	silentHubs   map[[33]byte]uint64

	// takeRetries is guarded by takeRetriesMu: the retry goroutine re-runs TakeOrder
	// off-engine, so the map is not engine-confined like sessions.
	takeRetriesMu sync.Mutex
	takeRetries   map[string]*takeRetryState

	startedAtMicro uint64

	// tracked* bookkeeping runs on worker/HTTP goroutines, not the engine, so
	// trackedMu guards it (unlike sessions); the pending* flags are single-flight guards.
	trackedMu          sync.Mutex
	tracked            map[string]*trackedBroadcast
	trackedSeq         atomic.Uint64
	pendingConfirm     atomic.Bool
	pendingRebroadcast atomic.Bool

	engineRunning atomic.Bool

	packets chan inboundPacket
	cmds    chan engineCmd
	tasks   chan workTask
	results chan workResult

	packetsDropped atomic.Uint64

	wg sync.WaitGroup

	// persistMu guards persistLatest; persistWriteMu serializes the disk write. The two
	// are never held together (persistMu is released first), so there is no lock-order cycle.
	persistMu       sync.Mutex
	persistLatest   *persistJob
	persistWriteMu  sync.Mutex
	persistSignal   chan struct{}
	persistUp       atomic.Bool
	lastSwapCount   atomic.Int64
	swapCountSeen   atomic.Bool
	persistFailures atomic.Int64

	pendingRefunds map[string]bool

	refundAttempts map[string]int
	refundRetryAt  map[string]uint64

	rescanAttempts map[string]int
	rescanRetryAt  map[string]uint64

	stallWarned map[string]uint64

	pendingWatch map[string]bool

	pendingOwnWatch map[string]bool
	ownWatchSeen    map[string]map[string]struct{}

	blockMu     sync.RWMutex
	block       [32]byte
	blockHeight uint32
	blockAt     time.Time

	cfgMu sync.RWMutex

	snReg *servicenode.Registry

	// exchangeStarted is false in production — this thin client never runs the hub
	// role; tests set it via SetExchangeStarted.
	exchangeStarted atomic.Bool
}

// NewNode dials the configured peer (if any) and starts ingesting broadcasts.
func NewNode(cfg *Config, store *Store) (*Node, error) {
	n := &Node{
		config:         cfg,
		store:          store,
		signer:         crypto.NewBtcSigner(),
		stop:           make(chan struct{}),
		sessions:       map[string]*SwapSession{},
		snReg:          servicenode.NewRegistry(),
		activator:      cfg.Activator,
		startedAtMicro: uint64(NowMicro()),
	}
	if n.activator == nil {
		n.activator = wallet.NewActivator()
	}

	// Restore before the dial: a dial failure returns early (read-only mode), and the
	// node must still come up with its persisted swaps.
	if cfg.DataDir != "" {
		n.restoreLocalSwaps(cfg.DataDir)
	}

	if cfg.NodeAddr != "" {
		conn, err := p2p.Dial(cfg.NodeAddr, cfg.Magic, 30*time.Second)
		if err != nil {
			return n, fmt.Errorf("api: dial service node: %w", err)
		}
		// OnNonXBridge routes the snr/snp/snlp messages into the registry; ReadPacket
		// would otherwise drop them.
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
					n.snReg.AddPing(sn, payload)
				} else {
					xlog.Warn("servicenode: SNPING/SNLISTPING parse failed", "err", derr)
				}
			}
		}
		n.conn = conn
		xlog.Info("connected to explicit service node", "addr", cfg.NodeAddr)
	} else {
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

func (n *Node) restoreLocalSwaps(dataDir string) {
	path := swapStatePath(dataDir)
	ps, bc, settled, strictErr := loadSwaps(path)
	if strictErr == nil {
		for _, p := range ps {
			n.restoreSwap(p)
		}
		if len(ps) > 0 {
			xlog.Info("restored local swaps from disk", "count", len(ps), "dir", dataDir)
		}
		n.restoreTracked(bc)
		if len(settled) > 0 {
			n.migrateSettledArchive(settled)
		}
		n.reconcileUnconfirmedDeposits()
		return
	}
	data, rerr := os.ReadFile(path)
	if rerr != nil {
		xlog.Error("could not load persisted swaps; starting fresh", "dir", dataDir, "err", rerr)
		return
	}
	good, dropped, lerr := loadSwapsLenient(data)
	qpath, qerr := quarantineSwapFile(path)
	if qerr != nil {
		// Salvage wins over evidence: a failed quarantine must not discard fund-recovery
		// records already parsed.
		xlog.Error("could not quarantine corrupt swap file; restoring salvaged records without evidence backup",
			"dir", dataDir, "path", path, "salvaged", len(good), "err", qerr)
		for _, p := range good {
			n.restoreSwap(p)
		}
		n.reconcileUnconfirmedDeposits()
		return
	}
	if lerr != nil {
		xlog.Error("corrupt swap file quarantined, nothing salvageable; starting fresh",
			"dir", dataDir, "quarantine", qpath, "err", strictErr)
		return
	}
	for _, p := range good {
		n.restoreSwap(p)
	}
	if len(good) == 0 && dropped == 0 {
		xlog.Warn("empty corrupt swap file quarantined",
			"dir", dataDir, "quarantine", qpath, "err", strictErr)
	} else {
		xlog.Error("corrupt swap file quarantined, valid records salvaged",
			"dir", dataDir, "quarantine", qpath,
			"salvaged", len(good), "dropped", dropped, "err", strictErr)
	}
	if len(good) > 0 {
		xlog.Info("restored local swaps from disk", "count", len(good), "dir", dataDir)
	}
	n.reconcileUnconfirmedDeposits()
}

func (n *Node) reconcileUnconfirmedDeposits() {
	for id, s := range n.sessions {
		if s.refundHex == "" || s.refundDone || s.state >= csCreatedA {
			continue
		}
		if n.orderDepositSent(id) || s.ourDepositTxID == "" {
			continue
		}
		conn := n.cfg().Connectors[s.srcCur]
		if conn == nil {
			xlog.Warn("reconcile: no connector, deposit state unknown", "order", id, "cur", s.srcCur, "txid", s.ourDepositTxID)
			continue
		}
		hex, err := conn.GetRawTransaction(s.ourDepositTxID)
		if err != nil || hex == "" {
			xlog.Info("reconcile: deposit not on chain, awaiting redelivery or cancel", "order", id, "txid", s.ourDepositTxID)
			continue
		}
		n.store.Update(id, func(o *Order) {
			o.DepositSent = true
		})
		xlog.Warn("reconcile: deposit found on chain without confirmation record, marked sent", "order", id, "txid", s.ourDepositTxID)
	}
}

// cfg returns the live configuration under the read lock; config reads must go
// through it so a hot-reload pointer swap cannot race them.
func (n *Node) cfg() *Config {
	n.cfgMu.RLock()
	defer n.cfgMu.RUnlock()
	return n.config
}

func (n *Node) reloadConf() error {
	path := n.cfg().ConfPath
	if path == "" {
		return fmt.Errorf("dxLoadXBridgeConf: no conf path configured (daemon started without -conf)")
	}
	conf, err := config.Load(path)
	if err != nil {
		return fmt.Errorf("dxLoadXBridgeConf: load %s: %w", path, err)
	}
	// Same rule as startup: a coin whose DustAmount cannot be used is disabled
	// for this snapshot and reported, never fatal. A reload that repairs the file
	// brings the coin back; one that breaks it drops only that coin.
	ReportDisabled(conf)
	admitted := config.Admitted(conf.Coins)
	if err := coins.InitFromConf(admitted); err != nil {
		return fmt.Errorf("dxLoadXBridgeConf: coin registry: %w", err)
	}
	a := n.walletActivator()
	a.ClearBad()
	connectors, drops := a.Activate(admitted, conf.Wallets(), n.cfg().CheckReachability)
	for _, d := range drops {
		xlog.Warn("wallet not activated after reload", "coin", d.Ticker, "reason", d.Reason)
	}

	fresh := &Config{
		NodeAddr:           n.cfg().NodeAddr,
		Magic:              n.cfg().Magic,
		Confs:              admitted,
		Connectors:         connectors,
		ExchangeWallets:    conf.Wallets(),
		ForceShowAllOrders: n.cfg().ForceShowAllOrders,
		ShowAllOrders:      conf.Main.ShowAllOrders || n.cfg().ForceShowAllOrders,
		// TakeRetry is daemon-level (not conf): carried over, else a reload would
		// silently disable retries.
		TakeRetry:         n.cfg().TakeRetry,
		CheckReachability: n.cfg().CheckReachability,
		Network:           n.cfg().Network,
		AddNodes:          n.cfg().AddNodes,
		WalletVersion:     n.cfg().WalletVersion,
		WalletVersionStr:  n.cfg().WalletVersionStr,
		DataDir:           n.cfg().DataDir,
		ConfPath:          path,
		Activator:         a, // the Activator actually used for this reload
	}
	n.cfgMu.Lock()
	n.config = fresh
	n.cfgMu.Unlock()
	if !fresh.ShowAllOrders {
		n.store.PruneUnconnected(connectorTickerSet(connectors))
	}
	xlog.Info("reloaded xbridge.conf", "path", path, "coins", len(admitted),
		"connectors", len(connectors), "drops", len(drops))
	return nil
}

func connectorTickerSet(conns map[string]wallet.Connector) map[string]bool {
	out := make(map[string]bool, len(conns))
	for t := range conns {
		out[t] = true
	}
	return out
}

func (n *Node) walletActivator() *wallet.Activator {
	n.activatorMu.Lock()
	defer n.activatorMu.Unlock()
	if n.activator == nil {
		n.activator = wallet.NewActivator()
	}
	return n.activator
}

// sweepConnectors re-probes reachability from the in-memory settings (the conf file
// is re-read only by reloadConf); order pruning happens only on reload.
func (n *Node) sweepConnectors() {
	c := n.cfg()
	if c == nil || c.Connectors == nil {
		return
	}
	conns, drops := n.walletActivator().Activate(c.Confs, c.ExchangeWallets, c.CheckReachability)
	for _, d := range drops {
		// Debug, not Warn: an unreachable wallet re-drops on every 30 s sweep.
		xlog.Debug("wallet not reachable", "coin", d.Ticker, "reason", d.Reason)
	}
	n.cfgMu.Lock()
	defer n.cfgMu.Unlock()
	// A reload that completed during the probe window replaced this snapshot, so its
	// connectors win; bail instead of clobbering.
	if n.config != c {
		return
	}
	if sameConnectorTickers(c.Connectors, conns) {
		return
	}
	fresh := *c // shallow copy; only Connectors differs
	fresh.Connectors = conns
	n.config = &fresh
	tickers := make([]string, 0, len(conns))
	for t := range conns {
		tickers = append(tickers, t)
	}
	sort.Strings(tickers)
	var added, removed []string
	for t := range conns {
		if _, ok := c.Connectors[t]; !ok {
			added = append(added, t)
		}
	}
	for t := range c.Connectors {
		if _, ok := conns[t]; !ok {
			removed = append(removed, t)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	xlog.Info("wallet sweep updated connectors", "connectors", len(conns), "tickers", strings.Join(tickers, ","), "added", strings.Join(added, ","), "removed", strings.Join(removed, ","))
}

func sameConnectorTickers(a, b map[string]wallet.Connector) bool {
	if len(a) != len(b) {
		return false
	}
	for t := range a {
		if _, ok := b[t]; !ok {
			return false
		}
	}
	return true
}

func (n *Node) sweepLoop() {
	defer n.wg.Done()
	t := time.NewTicker(walletSweepInterval)
	defer t.Stop()
	for {
		select {
		case <-n.stop:
			return
		case <-t.C:
			n.sweepConnectors()
		}
	}
}

// blockConnector returns the BLOCK connector, whose chain backs the anti-replay stamps on outgoing orders.
func (n *Node) blockConnector() wallet.Connector {
	if n.cfg() == nil || n.cfg().Connectors == nil {
		return nil
	}
	return n.cfg().Connectors["BLOCK"]
}

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

// confFor returns the coin's conf from one config snapshot. A configured
// connector always has a conf, so an absent one means the config was replaced
// between two reads; refuse rather than treat the coin as having no dust gate.
func (n *Node) confFor(t string) (*config.CoinConf, error) {
	c := n.cfg()
	if c == nil || c.Confs == nil {
		return nil, fmt.Errorf("dx: no wallet configured")
	}
	cc := c.Confs[t]
	if cc == nil {
		return nil, fmt.Errorf("dx: no wallet configured for %s", t)
	}
	return cc, nil
}

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

// availableBalance reads the BLOCK wallet alone: the fee pre-check is against the
// Blocknet wallet, never a sum over every coin connector.
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

// wholeCoinOstream must keep its exact formatting: the counterparty formats the same
// value independently, so any change here breaks signature verification.
func wholeCoinOstream(v float64) string {
	return strconv.FormatFloat(v, 'g', 6, 64)
}

func utxoChallenge(txid string, vout uint32, amount float64, address string) string {
	return txid + ":" + strconv.FormatUint(uint64(vout), 10) + ":" + wholeCoinOstream(amount) + ":" + address
}

var errBadSigLen = errors.New("api: signature must be 65 bytes")

var errBadAddr = errors.New("api: utxo address is invalid")

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
	if count-1 > math.MaxUint32 {
		return
	}
	n.blockMu.Lock()
	n.block = h
	n.blockHeight = uint32(count - 1)
	n.blockAt = time.Now()
	n.blockMu.Unlock()
}

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

// currentBlockHeight may do wallet RPC on a stale cache — handler/worker goroutines
// only, never the engine.
func (n *Node) currentBlockHeight() uint32 {
	n.blockMu.RLock()
	fresh := n.blockAt.After(time.Now().Add(-60*time.Second)) && n.block != [32]byte{}
	height := n.blockHeight
	n.blockMu.RUnlock()
	if fresh {
		return height
	}
	n.refreshBlock()
	n.blockMu.RLock()
	height = n.blockHeight
	n.blockMu.RUnlock()
	return height
}

// cachedBlockHeight is the engine-safe variant (no wallet RPC): a stale cache returns
// 0, so the expiry sweep defers its height predicate instead of pruning on stale data.
func (n *Node) cachedBlockHeight() uint32 {
	n.blockMu.RLock()
	fresh := n.blockAt.After(time.Now().Add(-60*time.Second)) && n.block != [32]byte{}
	h := n.blockHeight
	n.blockMu.RUnlock()
	if !fresh {
		return 0
	}
	return h
}

func (n *Node) pruneExpired() {
	// A taker is only in-swap once its order left "open": a hub-rejected taker sits at
	// "open" with a live session, and protecting it would keep it in the book forever.
	inSwap := map[string]bool{}
	for id, s := range n.sessions {
		if s.state <= csMaker {
			continue
		}
		if !s.isMaker {
			if o := n.store.Get(id); o == nil || o.Status == "open" {
				continue
			}
		}
		inSwap[id] = true
	}
	removed := n.store.PruneExpired(time.Now(), n.cachedBlockHeight(), inSwap)
	for _, id := range removed {
		xlog.Info("order expired, pruned", "order", id)
	}
	if len(removed) > 0 {
		xlog.Info("expiry sweep", "removed", len(removed))
	}
}

// rebroadcastInterval re-posts open maker orders before PendingTTL expires them, so
// an un-taken order stays in the book instead of silently timing out.
const rebroadcastInterval = 240 * time.Second

func (n *Node) rebroadcastOpenOrders() {
	now := NowMicro()
	gap := uint64(rebroadcastInterval / time.Microsecond)
	for id, s := range n.sessions {
		if !s.isMaker || s.state != csMaker {
			continue
		}
		o := n.store.Get(id)
		if o == nil || !o.Mine || o.PrepTx != "" || o.Status != "open" {
			continue
		}
		if now-o.Updated < gap {
			continue
		}
		if !n.orderUtxosStillValid(o) {
			continue
		}
		hubAddr := o.HubAddress
		if hubAddr == [20]byte{} {
			hubAddr = s.hub
		}
		pkt, err := n.buildOrderPacket(o, s.privKey)
		if err != nil {
			xlog.Error("rebroadcast: build failed", "order", id, "err", err)
			continue
		}
		if err := n.conn.WritePacket(pkt, hubAddr); err != nil {
			xlog.Error("rebroadcast: send failed", "order", id, "err", err)
			if newAddr, ok := n.rotateHub(id, s, o); ok {
				if rerr := n.conn.WritePacket(pkt, newAddr); rerr != nil {
					xlog.Error("rebroadcast: retry send failed", "order", id, "err", rerr)
					continue
				}
			} else {
				continue
			}
		}
		n.store.Update(id, func(ord *Order) { ord.Updated = now })
		xlog.Debug("order rebroadcast", "order", id)
	}
}

// rotateHub re-pins the hub only before any handshake packet: the anchor has
// authenticated nothing yet; the order id's preimage excludes the hub anyway.
func (n *Node) rotateHub(id string, s *SwapSession, o *Order) ([20]byte, bool) {
	if n.snReg == nil || !s.isMaker || s.state != csMaker {
		return [20]byte{}, false
	}
	var newKey [33]byte
	var ok bool
	if s.hubKey != ([33]byte{}) {
		newKey, ok = n.snReg.Pick([]string{o.FromCurrency, o.ToCurrency}, s.hubKey)
	} else {
		newKey, ok = n.snReg.Pick([]string{o.FromCurrency, o.ToCurrency})
	}
	if !ok {
		xlog.Warn("rebroadcast: no alternative hub, keeping anchor", "order", id)
		return [20]byte{}, false
	}
	newAddr := coins.KeyID(newKey[:])
	newPubHex := hexEncode(newKey[:])
	s.hub, s.hubKey = newAddr, newKey
	n.store.Update(id, func(ord *Order) {
		ord.HubAddress = newAddr
		ord.SNodePubkey = newPubHex
	})
	n.persist()
	xlog.Info("rebroadcast: migrated to new hub", "order", id, "hub", newPubHex)
	return newAddr, true
}

// orderUtxosStillValid cancels only on a definitively-spent funding output; a wallet
// error leaves the live order alone instead of cancelling it.
func (n *Node) orderUtxosStillValid(o *Order) bool {
	if len(o.UsedCoins) == 0 {
		return true
	}
	cfg := n.cfg()
	if cfg == nil {
		return true
	}
	conn := cfg.Connectors[o.FromCurrency]
	if conn == nil {
		return true
	}
	for _, u := range o.UsedCoins {
		_, ok, err := conn.GetTxOut(u.TxID, u.Vout)
		if err != nil {
			xlog.Warn("rebroadcast: utxo check unavailable, skipping round", "order", hexEncode(o.ID[:]), "err", err)
			return true
		}
		if !ok {
			xlog.Info("rebroadcast: funding spent, cancelling", "order", hexEncode(o.ID[:]), "utxo", u.TxID)
			if serr := n.sendCancelTransaction(hexEncode(o.ID[:]), uint32(crBadAUtxo)); serr != nil {
				xlog.Error("rebroadcast: cancel send failed", "order", hexEncode(o.ID[:]), "err", serr)
			}
			now := NowMicro()
			n.store.Update(hexEncode(o.ID[:]), func(stored *Order) {
				stored.Status = "canceled"
				stored.Updated = now
			})
			n.persist()
			return false
		}
	}
	return true
}

// buildOrderPacket rebuilds the same xbcPendingTransaction MakeOrder sent (same
// OrderBody, re-signed with the per-trade M key) so the hub relays it as the same order.
func (n *Node) buildOrderPacket(o *Order, priv [32]byte) (*proto.Packet, error) {
	body := &proto.OrderBody{
		ID:             o.ID,
		From:           o.From,
		FromCurrency:   o.FromCurrency,
		FromAmount:     o.FromAmount,
		To:             o.To,
		ToCurrency:     o.ToCurrency,
		ToAmount:       o.ToAmount,
		Created:        o.Created,
		BlockHash:      o.BlockHash,
		PartialAllowed: o.PartialAllowed,
		MinFromAmount:  o.MinFromAmount,
		Utxos:          o.Utxos,
	}
	pkt := proto.NewPacket(proto.XbcTransaction, body.Marshal())
	if err := n.signer.Sign(pkt, priv[:]); err != nil {
		return nil, err
	}
	return pkt, nil
}

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

func (n *Node) handlePacket(in inboundPacket) {
	body, err := proto.DecodeBody(in.pkt.Command, in.pkt.Body)
	if err != nil {
		// The reader already decoded this body; a failure here means it changed in
		// transit between reader and engine, so drop it defensively.
		xlog.Warn("engine packet body decode skipped", "command", in.pkt.Command.String(), "err", err)
		return
	}
	switch b := body.(type) {
	case *proto.PendingTransactionBody:
		n.ingestPending(b, in.snode)

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

func (n *Node) ingestPending(b *proto.PendingTransactionBody, snode string) {
	o := normalizeFromPendingBody(b, snode)
	o.BlockNumber = n.cachedBlockHeight()
	if n.store.Touch(hexEncode(o.ID[:])) {
		return
	}
	// A known canceled or historic order must not be re-accepted from a network
	// rebroadcast, so a failed Touch with a known id stops here.
	if n.store.HasOrder(hexEncode(o.ID[:])) {
		return
	}
	n.verifyAndBook(o)
}

func (n *Node) verifyAndBook(o *Order) {
	idHex := hexEncode(o.ID[:])
	if len(o.Utxos) == 0 || n.cfg() == nil {
		n.store.Add(o)
		return
	}
	cfg := n.cfg()
	conn := cfg.Connectors[o.FromCurrency]
	coin, hasCoin := coins.Get(o.FromCurrency)
	if conn == nil || !hasCoin {
		n.store.Add(o)
		return
	}
	utxos := make([]proto.UtxoEntry, len(o.Utxos))
	copy(utxos, o.Utxos)
	task := workTask{
		orderID: idHex,
		run: func() (any, error) {
			return verifyOrderUtxos(conn, coin, utxos, o.FromAmount)
		},
		apply: func(v any, err error) {
			ok, _ := v.(bool)
			if err != nil || !ok {
				xlog.Warn("inbound order not booked: maker utxo proofs failed", "order", idHex, "err", err)
				return
			}
			// Re-check under the store: a relayed copy may have been booked (or
			// the order canceled) while the proof was being verified.
			if n.store.HasOrder(idHex) {
				return
			}
			n.store.Add(o)
		},
	}
	if !n.engineRunning.Load() {
		v, err := safeTaskRun(task)
		task.apply(v, err)
		return
	}
	select {
	case n.tasks <- task:
	default:
		// Engine busy: drop the ingest; the order is re-broadcast periodically
		// and re-ingested then.
		xlog.Warn("ingest verification dropped, engine busy", "order", idHex)
	}
}

func verifyOrderUtxos(conn wallet.Connector, c coins.Coin, utxos []proto.UtxoEntry, required uint64) (bool, error) {
	var sum float64
	valid := 0
	for _, e := range utxos {
		var rev [32]byte
		for i := 0; i < 32; i++ {
			rev[i] = e.TxID[31-i]
		}
		txid := hexEncode(rev[:])
		u, ok, err := conn.GetTxOut(txid, e.Vout)
		if err != nil {
			xlog.Warn("order utxo proof skipped: gettxout failed", "txid", txid, "err", err)
			continue
		}
		if !ok {
			continue
		}
		a := coins.Address{Coin: c, Kind: coins.P2PKH, Prefix: c.P2PKH, Hash: e.RawAddress[:]}
		address := a.String()
		if address == "" {
			continue
		}
		ok, err = conn.VerifyMessage(address, e.Signature[:], utxoChallenge(txid, e.Vout, u.Value, address))
		if err != nil {
			xlog.Warn("order utxo proof skipped: verifymessage failed", "txid", txid, "err", err)
			continue
		}
		if !ok {
			continue
		}
		sum += u.Value
		valid++
	}
	return valid > 0 && sum*float64(coinScale) >= float64(required), nil
}

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
		"packets_dropped", n.packetsDropped.Load(),
		"persist_failures", n.persistFailures.Load(),
		"tokens", strings.Join(n.NetworkTokens(), ","))
}

// NetworkTokens returns the P2P network's advertised tokens, with no config fallback.
func (n *Node) NetworkTokens() []string {
	if n.snReg == nil {
		return []string{}
	}
	ws := n.snReg.WalletServices()
	if len(ws) == 0 {
		return []string{}
	}
	return ws
}

type responseBody interface {
	Marshal() []byte
}

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
	// The reply must echo the inbound packet's hub route, not the make-time
	// KeyID: the hub drops packets whose body[0:20] is not its live session id.
	if hub != ([20]byte{}) {
		s.hub = hub
	}
	xlog.With("p2p", true, "order", orderID).Info("swap packet received", "command", cmdName, "state", s.state.String())
	// Stamp progress only past the hub-signature gate, so a forged packet
	// cannot keep a dead session alive by resetting the silence watchdog.
	s.lastProgress = uint64(NowMicro())
	if s.await {
		if first := handshakeInflightDedup.Event(orderID); first {
			swlog.Debug("swap packet dropped: handshake task in flight", "command", cmdName)
		}
		return
	}
	// A panic here would kill the shared feed goroutine and the whole
	// process, so recover and let this one swap go unprogressed.
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
		xlog.With("p2p", true, "order", orderID).Info("swap response sent", "command", cmd.String())
		if cmd == proto.XbcTransactionHoldApply {
			s.holdApplySentAt = NowMicro()
		}
	}
}

func (n *Node) verifyHubPacket(pkt *proto.Packet, s *SwapSession) bool {
	if s.hubKey == [33]byte{} {
		return false
	}
	if ok, _ := n.signer.VerifyAgainst(pkt, hexEncode(s.hubKey[:])); !ok {
		return false
	}
	if s.state >= csHoldApplied {
		return true
	}
	return n.hubRegistered(s.hubKey[:])
}

// hubRegistered reports whether the pubkey is a known servicenode:
// membership only (C++ getSn), whereas make's hub Pick also requires running.
func (n *Node) hubRegistered(pk []byte) bool {
	reg := n.snReg
	if reg == nil {
		return false
	}
	var key [33]byte
	copy(key[:], pk)
	return reg.Known(key)
}

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
	// Persist before the response leaves: a crash in between would let the
	// hub know swap state the durable copy lacks, stranding a restarted swap.
	if err := n.persistNow(); err != nil {
		return err
	}
	if err := n.conn.WritePacket(pkt, dest); err != nil {
		return err
	}
	if xlog.Level() <= slog.LevelDebug {
		// slog evaluates disabled-level args, so packetHex must stay inside
		// the guard or every send would marshal and hex-encode its packet.
		xlog.With("p2p", true).Debug("swap packet sent", "command", cmd.String(), "hex", packetHex(pkt))
	}
	return nil
}

// MakeOrderParams are the parsed dxMakeOrder / dxMakePartialOrder arguments.
type MakeOrderParams struct {
	Maker        string
	MakerSize    string
	MakerAddress string
	Taker        string
	TakerSize    string
	TakerAddress string
	Type         string
	MinSize      string
	UseAllFunds  bool
	AutoSplit    bool
	Repost       bool
	DryRun       bool
}

func decodeAddr(method, currency, addrStr string) ([20]byte, *rpcError) {
	c, ok := coins.Get(currency)
	if !ok {
		return [20]byte{}, makeError(errInvalidParameters, method, "unsupported currency: "+currency)
	}
	addr, err := c.DecodeAddress(addrStr)
	if err != nil {
		return [20]byte{}, makeError(errInvalidAddress, method, addrStr)
	}
	id, ok := addr.ID()
	if !ok {
		return [20]byte{}, makeError(errInvalidAddress, method, addrStr)
	}
	return id, nil
}

func takeBadAddressErr(cur string, e *rpcError) *rpcError {
	if e.Code == errInvalidAddress {
		return makeError(errInvalidAddress, "dxTakeOrder", ": "+cur+" address is bad. Are you using the correct address?")
	}
	return e
}

func (n *Node) requireWrite(name string) *rpcError {
	if n.conn == nil {
		return makeError(errNoServiceNode, name, "")
	}
	return nil
}

// MakeOrder builds, signs and (unless dry-run) broadcasts an xbcTransaction packet, returning dxMakeOrder's response shape.
func (n *Node) MakeOrder(p MakeOrderParams) (*Order, *rpcError) {
	if e := n.requireWrite("dxMakeOrder"); e != nil {
		return nil, e
	}
	if !xBridgeValidCoin(p.MakerSize) {
		return nil, makeError(errInvalidParameters, "dxMakeOrder", "The maker_size is too precise. The maximum precision supported is 6 digits.")
	}
	if !xBridgeValidCoin(p.TakerSize) {
		return nil, makeError(errInvalidParameters, "dxMakeOrder", "The taker_size is too precise. The maximum precision supported is 6 digits.")
	}
	fromAmt, aerr := lexicalAmount(p.MakerSize)
	if aerr != nil {
		return nil, aerr
	}
	toAmt, aerr := lexicalAmount(p.TakerSize)
	if aerr != nil {
		return nil, aerr
	}
	// Fetch the maker connector once and reuse it below: dxLoadXBridgeConf
	// swaps the config wholesale, so enumeration stays on the validated handle.
	makerConn, cerr := n.connector(p.Maker)
	if cerr != nil {
		return nil, makeError(errNoSession, "dxMakeOrder", "Unable to connect to wallet: "+p.Maker)
	}
	if _, e := n.connector(p.Taker); e != nil {
		return nil, makeError(errNoSession, "dxMakeOrder", "Unable to connect to wallet: "+p.Taker)
	}
	// A connector always has a conf in the same snapshot. Refuse if it does not,
	// so an absent conf can never present itself as a zero dust threshold.
	cc, cerr := n.confFor(p.Maker)
	if cerr != nil {
		return nil, makeError(errNoSession, "dxMakeOrder", "Unable to connect to wallet: "+p.Maker)
	}
	fromID, e := decodeAddr("dxMakeOrder", p.Maker, p.MakerAddress)
	if e != nil {
		return nil, e
	}
	toID, e := decodeAddr("dxMakeOrder", p.Taker, p.TakerAddress)
	if e != nil {
		return nil, e
	}
	if p.MakerAddress == p.TakerAddress {
		return nil, makeError(errInvalidParameters, "dxMakeOrder", "The maker_address and taker_address cannot be the same: "+p.MakerAddress)
	}
	if fromAmt > maxXSize || toAmt > maxXSize {
		return nil, makeError(errInvalidParameters, "dxMakeOrder", "The maximum supported size is 100000000")
	}
	if fromAmt == 0 || toAmt == 0 {
		return nil, makeError(errInvalidParameters, "dxMakeOrder", "The minimum supported size is "+formatXPrice(1.0/float64(coinScale)))
	}
	var hubKey [33]byte
	var hubAddr [20]byte
	if !p.DryRun {
		reg := n.snReg
		if reg == nil {
			xlog.Warn("dxMakeOrder refused: no service-node registry (empty snReg)", "maker", p.Maker, "taker", p.Taker)
			return nil, makeError(errNoServiceNode, "dxMakeOrder", "")
		}
		var ok bool
		hubKey, ok = n.pickHub([]string{p.Maker, p.Taker}, NowMicro())
		if !ok {
			xlog.Warn("dxMakeOrder refused: no running hub advertising both currencies", "maker", p.Maker, "taker", p.Taker)
			return nil, makeError(errNoServiceNode, "dxMakeOrder", "")
		}
		hubAddr = coins.KeyID(hubKey[:])
	}

	partial := false
	minFrom := uint64(0)
	nativeCoin := cc.Coin
	if p.Type == "partial" {
		partial = true
		if p.MinSize == "" {
			return nil, makeError(errInvalidParameters, "dxMakePartialOrder", "minimum_size is required for partial orders")
		}
		m, merr := lexicalAmount(p.MinSize)
		if merr != nil {
			return nil, merr
		}
		minFrom = m
		if minFrom > fromAmt {
			return nil, makeError(errInvalidParameters, "dxMakePartialOrder", "The minimum_size can't be more than maker_size")
		}
		if float64(minFrom)/float64(coinScale)*float64(nativeCoin) < float64(effectiveDust(cc)) {
			return nil, makeError(errInvalidParameters, "dxMakePartialOrder", "The partial minimum_size is dust, i.e. it's too small.")
		}
	}

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
		// Per-utxo fee provisions round UP: split sizes must cover the
		// exact-sats fees the per-fill session will later need.
		partialFee1 := ceilFeeXB(cc, estimateFee(cc, 1, 3))
		partialFee2 := ceilFeeXB(cc, estimateFee(cc, 1, 1))
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
		partialRemainderIsDust = isDustNative(xBridgeValueFromAmount(partialRemainderVoutTotal+partialPerUtxoFees), cc, nativeCoin)
		partialVoutsTotal = partialFees + partialSplitVoutsTotal
		if partialRemainderRequired && !partialRemainderIsDust {
			partialVoutsTotal += partialRemainderVoutTotal
		}
		partialOrderVouts = partialUtxosRequiredForMinimum
		if partialRemainderRequired && !partialRemainderIsDust {
			partialOrderVouts++
		}
	}

	conn := makerConn
	minConf := cc.Confirmations
	locked := n.store.LockedUtxoInfoFor(p.Maker)
	outputs, listErr := conn.ListUnspent(minConf)
	if listErr != nil {
		return nil, makeError(errInsufficientFunds, "dxMakeOrder", listErr.Error())
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
	funding := outputsForUse

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

	ts := NowMicro()
	bh := n.currentBlockHash()
	id := sha256dOrderID(fromID, p.Maker, fromAmt, toID, p.Taker, toAmt, ts, bh, proofs[0].Signature[:])

	// The per-trade M key is generated here so the packet-signing key and the
	// HTLC DepositorPub are the same key by construction (session pubKey = mPub).
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

	pending := false
	var prepTxID string
	if partial {
		if exactMatch {
			if len(outputsForUse) > maxPartialOrderUtxos {
				return nil, makeError(errInvalidAmount, "dxMakePartialOrder", "failed to create order, the maximum number of utxos on the order was exceeded")
			}
		} else if p.AutoSplit {
			existing := outputsForUse[:0]
			var vins []wallet.Utxo
			var vinsTotal uint64
			remaining := partialUtxosRequiredForMinimum
			voutsTotal := int64(partialVoutsTotal)
			for _, vin := range outputsForUse {
				if camount(vin) == minFrom+partialPerUtxoFees && remaining > 0 {
					existing = append(existing, vin)
					remaining--
					voutsTotal -= int64(minFrom + partialPerUtxoFees)
					continue
				}
				vinsTotal += vin.Amount
				vins = append(vins, vin)
			}
			// Exact integer native outputs (== C++ prep tx, xbridgeapp.cpp
			// :1801-1807: DescrToSats of the XB split sizes, no doubles).
			var vouts []prepVout
			for i := 0; i < remaining; i++ {
				vouts = append(vouts, prepVout{p.MakerAddress, fromXBridgeAmt(coin, minFrom+partialPerUtxoFees)})
			}
			if partialRemainderRequired && !partialRemainderIsDust {
				vouts = append(vouts, prepVout{p.MakerAddress, fromXBridgeAmt(coin, partialRemainderVoutTotal+partialPerUtxoFees)})
			}
			prepFee := estimateFee(cc, len(vins), len(vouts)+1)
			var voutsTotalNative uint64
			for _, vo := range vouts {
				voutsTotalNative += vo.amount
			}
			// C++ rejects non-positive change (xbridgeapp.cpp :1812); the
			// dust comparison below stays double-based exactly like C++
			// (:1821) — a threshold policy, not construction.
			if vinsTotal <= voutsTotalNative+prepFee {
				return nil, makeError(errInvalidAmount, "dxMakePartialOrder", "failed to create order, insufficient funds on partial order")
			}
			if changeNative := vinsTotal - voutsTotalNative - prepFee; !isDustNative(float64(changeNative)/float64(nativeCoin), cc, nativeCoin) {
				vouts = append(vouts, prepVout{p.MakerAddress, changeNative})
			}
			signedHex, txid, rerr := buildPrepTx(conn, cc, coin, vins, vouts)
			if rerr != nil {
				return nil, rerr
			}
			if !p.DryRun {
				if _, err := conn.SendRawTransaction(signedHex); err != nil {
					return nil, makeError(errUnknown, "dxMakePartialOrder", err.Error())
				}
				// The id is re-derived by the re-sign below, so this watch entry is recorded without an order id.
				n.recordBroadcast("", broadcastPrep, p.Maker, txid, signedHex)
			}
			prepTxID = txid
			used := existing
			partialNew := int64(0)
			// Rebuilt-utxo accounting stays in XB via camount(): the Value
			// double is the nearest double to the exact native amount, so
			// camount's +1e-8 nudge recovers the integer XB exactly (double
			// error ~6e-11 XB against a 0.01-XB margin) — no drift versus the
			// XB-space voutsTotal cap.
			for i, vo := range vouts {
				if voutsTotal-partialNew <= 0 {
					break
				}
				used = append(used, wallet.Utxo{
					TxID: prepTxID, Vout: uint32(i), Address: p.MakerAddress,
					Value: float64(vo.amount) / float64(nativeCoin), Amount: vo.amount,
				})
				partialNew += int64(camount(used[len(used)-1]))
			}
			if len(used) > maxPartialOrderUtxos {
				return nil, makeError(errInvalidAmount, "dxMakePartialOrder", "failed to create order, the maximum number of utxos on the order was exceeded")
			}
			if len(used) == 0 {
				return nil, makeError(errInvalidPartialOrder, "dxMakePartialOrder", "failed to create order, cannot lock partial order utxos")
			}
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
			funding = used
			pending = true
		}
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
	o.BlockNumber = n.currentBlockHeight()
	o.MakerAddress = p.MakerAddress
	o.TakerAddress = p.TakerAddress
	o.BlockID = orderIDString(body.BlockHash)
	o.PartialRepost = p.Repost
	o.Mine = true
	o.UtxoCurrency = p.Maker
	o.Role = 'A'
	o.MakerKey = hexEncode(mPub[:])
	o.OrigFromCurrency = p.Maker
	o.OrigToCurrency = p.Taker
	o.UsedCoins = funding
	if partial {
		if pending {
			o.Status = "open"
			o.PrepTx = prepTxID
		} else {
			o.Status = "open"
		}
	} else {
		o.Status = "open"
	}
	if !p.DryRun {
		o.SNodePubkey = hexEncode(hubKey[:])
		o.HubAddress = hubAddr
		// Respond from a store snapshot copy, never the live record: the engine may
		// concurrently Touch/Status-write it, which would race the handler's render.
		var rerr *rpcError
		var stored *Order
		n.submit(func() {
			n.store.Add(o)
			if pending {
				// A pending order is never broadcast, but its maker session is registered from
				// creation, so it keeps its signing key and stays cancelable with no swap activity.
				n.newMakerSession(o, p, mPrivArr, mPub)
				n.persist()
				stored = n.store.Get(hexEncode(o.ID[:]))
				return
			}
			// Persist BEFORE sending the make packet: a crash after the send must never
			// leave the hub holding an order local state has no record of (stranded take).
			n.newMakerSession(o, p, mPrivArr, mPub)
			if err := n.persistNow(); err != nil {
				delete(n.sessions, hexEncode(o.ID[:]))
				n.store.Remove(hexEncode(o.ID[:]))
				rerr = makeError(errUnknown, "dxMakeOrder", err.Error())
				return
			}
			if err := n.conn.WritePacket(pkt, hubAddr); err != nil {
				delete(n.sessions, hexEncode(o.ID[:]))
				n.store.Remove(hexEncode(o.ID[:]))
				_ = n.persistNow()
				rerr = makeError(errUnknown, "dxMakeOrder", err.Error())
				return
			}
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

func (n *Node) checkAcceptParams(currency string, fromSize uint64, fromAddress string) *rpcError {
	conn, e := n.connector(currency)
	if e != nil {
		return makeError(errNoSession, "dxTakeOrder", currency)
	}
	outputs, err := conn.ListUnspent(1)
	if err != nil {
		return makeError(errInsufficientFunds, "dxTakeOrder", fromAddress)
	}
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

// TakeOrder broadcasts an xbcTransactionAccepting packet for an order and returns the dxTakeOrder response shape.
func (n *Node) TakeOrder(p TakeOrderParams) (orderListResult, *rpcError) {
	if e := n.requireWrite("dxTakeOrder"); e != nil {
		return orderListResult{}, e
	}
	if p.FromAddress == p.ToAddress {
		return orderListResult{}, makeError(errInvalidParameters, "dxTakeOrder", "The from_address and to_address cannot be the same: "+p.FromAddress)
	}
	key := orderIDKey(p.ID)

	// An explicit amount <= 0 must error 1025 with the raw string even when the id is
	// unknown, so amount validation runs before the order lookup (C++ dxTakeOrder).
	var takeAmount uint64
	if p.Amount != "" {
		a, err := lexicalAmount(p.Amount)
		if err != nil {
			return orderListResult{}, err
		}
		if a == 0 {
			return orderListResult{}, makeError(errInvalidParameters, "dxTakeOrder", "The amount cannot be less than or equal to 0: "+p.Amount)
		}
		takeAmount = a
	}

	o := n.store.Get(key)
	if o == nil {
		// C++ passes no argument here, so the rendered message keeps its double space.
		return orderListResult{}, makeError(errTxNotFound, "dxTakeOrder", "")
	}

	// Pre-swap frame: the taker sends o.ToAmount and receives o.FromAmount.
	fromSize := o.ToAmount
	toSize := o.FromAmount
	if p.Amount != "" {
		a := takeAmount
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

	// This balance gate runs before the self-trade, connector, and address checks,
	// so when several checks fail the balance error wins.
	if e := n.checkAcceptParams(o.ToCurrency, fromSize, p.FromAddress); e != nil {
		return orderListResult{}, e
	}

	if o.Mine {
		return orderListResult{}, makeError(errInvalidParameters, "dxTakeOrder", "Unable to accept your own order.")
	}

	toConn, cerr := n.connector(o.ToCurrency)
	if cerr != nil {
		return orderListResult{}, makeError(errNoSession, "dxTakeOrder", "Unable to connect to wallet: "+o.ToCurrency)
	}
	if _, e := n.connector(o.FromCurrency); e != nil {
		return orderListResult{}, makeError(errNoSession, "dxTakeOrder", "Unable to connect to wallet: "+o.FromCurrency)
	}

	// The taker's to-address sits on the order's from-currency chain (pre-swap frame), so
	// it decodes against o.FromCurrency first; the from-address (o.ToCurrency) follows.
	toID, e := decodeAddr("dxTakeOrder", o.FromCurrency, p.ToAddress)
	if e != nil {
		return orderListResult{}, takeBadAddressErr(o.FromCurrency, e)
	}
	fromID, e := decodeAddr("dxTakeOrder", o.ToCurrency, p.FromAddress)
	if e != nil {
		return orderListResult{}, takeBadAddressErr(o.ToCurrency, e)
	}

	if p.DryRun {
		xlog.Warn("TakeOrder dry run — take not broadcast", "order", p.ID, "fromCur", o.ToCurrency, "toCur", o.FromCurrency)
		return o.toTakeDryrunResult(fromSize, toSize), nil
	}

	ccTo, cerr := n.confFor(o.ToCurrency)
	if cerr != nil {
		return orderListResult{}, makeError(errNoSession, "dxTakeOrder", o.ToCurrency)
	}
	ccFrom, cerr := n.confFor(o.FromCurrency)
	if cerr != nil {
		return orderListResult{}, makeError(errNoSession, "dxTakeOrder", o.FromCurrency)
	}
	nativeTo := ccTo.Coin
	nativeFrom := ccFrom.Coin
	if isDustNative(xBridgeValueFromAmount(fromSize), ccTo, nativeTo) {
		return orderListResult{}, makeError(errDust, "dxTakeOrder", "")
	}
	if isDustNative(xBridgeValueFromAmount(toSize), ccFrom, nativeFrom) {
		return orderListResult{}, makeError(errDust, "dxTakeOrder", "")
	}

	if funds, ferr := n.availableBalance(); ferr != nil {
		return orderListResult{}, makeError(errNoSession, "dxTakeOrder", ferr.Error())
	} else if funds < xBridgeIntFromReal(serviceNodeFeeReal) {
		return orderListResult{}, makeError(errInsufficientFundsDX, "dxTakeOrder", "")
	}

	hubKey := decodePub33(o.SNodePubkey)
	if !n.hubRegistered(hubKey[:]) {
		return orderListResult{}, makeError(errNoServiceNode, "dxTakeOrder", "")
	}
	feeDest, ok := n.snReg.PaymentAddress(hubKey)
	if !ok {
		return orderListResult{}, makeError(errNoServiceNode, "dxTakeOrder", "")
	}
	lockedKeys := n.store.LockedUtxoInfoFor(o.ToCurrency)
	// The fee filter must use the ALL-token locked set, not this per-token snapshot: it has to
	// match ReserveForTake's check or a fee input locked as another order's funding dies with 1019.
	feeLocked, _ := n.store.LockedUtxoInfo()

	// Take funding is selected FIRST from the full spendable set; the
	// service-node fee is built afterward from the remainder (see
	// selectTakeFunding). Fee-first starves funding when one utxo dominates
	// the wallet: the fee selector's single-gt rule eats it and funding
	// selection fails on what is left.
	//
	// This repeats the balance gate's scan (checkAcceptParams): same wallet, same
	// minconf, so the gate's verdict and this selection draw from one spendable set.
	conn := toConn
	outputs, err := conn.ListUnspent(1)
	if err != nil {
		return orderListResult{}, makeError(errInsufficientFunds, "dxTakeOrder", err.Error())
	}
	filtered := make([]wallet.Utxo, 0, len(outputs))
	for _, u := range outputs {
		// Funding must be 25-byte P2PKH: the deposit path cannot spend a P2SH,
		// multisig, or OP_RETURN output, so those must never fund a taker entry.
		if !isP2PKH25(u.ScriptPubKey) {
			continue
		}
		k := u.TxID + ":" + strconv.FormatUint(uint64(u.Vout), 10)
		if lockedKeys[k] {
			continue
		}
		filtered = append(filtered, u)
	}

	blk := n.blockConnector()
	blkCoin, _ := coins.Get("BLOCK")
	blkConf := n.cfg().Confs["BLOCK"]
	feeUtxoAvail, err := blk.ListUnspentWithZeroConf()
	if err != nil {
		return orderListResult{}, makeError(errInsufficientFunds, "dxTakeOrder", "")
	}
	feeUtxos := make([]wallet.Utxo, 0, len(feeUtxoAvail))
	for _, u := range feeUtxoAvail {
		// Fee inputs need at least 1 conf (C++ unspentP2PKH): a 0-conf fee parent the
		// hub's mempool lacks fails hub broadcast with crBadFeeTx.
		if u.Confirmations < 1 {
			continue
		}
		if !isP2PKH25(u.ScriptPubKey) {
			continue
		}
		k := u.TxID + ":" + strconv.FormatUint(uint64(u.Vout), 10)
		if lockedKeys[k] || feeLocked[k] {
			continue
		}
		feeUtxos = append(feeUtxos, u)
	}
	usedCoins, feeAvailRest, ok := selectTakeFunding(p.FromAddress, filtered, feeUtxos, ccTo, fromSize)
	if !ok {
		return orderListResult{}, makeError(errInsufficientFunds, "dxTakeOrder", "insufficient funds")
	}
	info, ierr := feeOrderInfo(o.ID, o.ToCurrency, fromSize, o.FromCurrency, toSize)
	if ierr != nil {
		if errors.Is(ierr, errOrderInfoOverflow) {
			return orderListResult{}, makeError(errInvalidOnchainHist, "dxTakeOrder", "")
		}
		return orderListResult{}, makeError(errInsufficientFunds, "dxTakeOrder", "")
	}
	rawFeeHex, feeInputs, rerr := buildServiceNodeFeeTx(blk, blkCoin, blkConf, feeDest, info, feeAvailRest)
	if rerr != nil {
		return orderListResult{}, rerr
	}
	feeBytes, ferr := hex.DecodeString(rawFeeHex)
	if ferr != nil {
		return orderListResult{}, makeError(errInsufficientFunds, "dxTakeOrder", ferr.Error())
	}
	// The fee is spent even when the swap later fails, so log its sum and destination
	// at prep time: a take that burns the fee with no log evidence is unauditable.
	feeSum := uint64(0)
	for _, u := range feeInputs {
		feeSum += u.Amount
	}
	xlog.Info("take: service-node fee prepared", "order", p.ID, "fee",
		formatXAmount(feeSum), "dest", feeDest, "inputs", len(feeInputs))

	feeKey := make(map[string]bool, len(feeInputs))
	for _, u := range feeInputs {
		feeKey[u.TxID+":"+strconv.FormatUint(uint64(u.Vout), 10)] = true
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
	fromH, fromHash, cerr := n.blockContext(o.ToCurrency)
	if cerr != nil {
		return orderListResult{}, makeError(errNoSession, "dxTakeOrder", cerr.Error())
	}
	toH, toHash, cerr := n.blockContext(o.FromCurrency)
	if cerr != nil {
		return orderListResult{}, makeError(errNoSession, "dxTakeOrder", cerr.Error())
	}
	// The hub re-validates these heights and truncated hashes against its own wallets in
	// processTransactionAccepting (heights within 2 blocks), so log exactly what we report.
	xlog.Info("take: accepting prepared", "order", p.ID,
		"fromCur", o.ToCurrency, "fromHeight", fromH, "fromHash", hex.EncodeToString(fromHash[:]),
		"toCur", o.FromCurrency, "toHeight", toH, "toHash", hex.EncodeToString(toHash[:]),
		"hub", o.SNodePubkey)

	// Claim the fee and funding inputs under the store lock BEFORE any Accepting packet
	// can leave, so two in-flight takes can never select the same utxo.
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
	// The take commits as one awaited engine unit (re-check, commit, persist, send), so
	// a cancel or expiry can never land between the re-check and the wire write.
	var result *Order
	var terr *rpcError
	n.submit(func() {
		result, terr = n.commitTake(key, p, pkt, tPrivArr, tPub, proofs, usedCoins, feeInputs)
	}, true)
	if terr != nil {
		return orderListResult{}, terr
	}
	if result == nil {
		// commitTake releases the reserve only when its re-check finds the order gone;
		// if submit bailed on n.stop the reserve is still held, so release here — never leak it.
		n.store.ReleaseReserve(key)
		return orderListResult{}, makeError(errTxNotFound, "dxTakeOrder", p.ID)
	}
	return result.toTakeResult(fromSize, toSize), nil
}

func (n *Node) commitTake(key string, p TakeOrderParams, pkt *proto.Packet, tPrivArr [32]byte, tPub [33]byte, proofs []proto.UtxoEntry, usedCoins, feeInputs []wallet.Utxo) (*Order, *rpcError) {
	if n.store.Get(key) == nil {
		n.store.ReleaseReserve(key)
		return nil, nil
	}
	now := NowMicro()
	makerKey := hexEncode(tPub[:])
	// Commit from the live book, not the HTTP snapshot (it predates this re-check);
	// the Accepting body is snapshot-built but holds only immutable identity fields.
	var prevOrder *Order
	n.store.Update(key, func(stored *Order) {
		// Full pre-take deep copy under the store lock: a failed persist/send
		// restores it wholesale, so a revert never depends on today's field set.
		prevOrder = stored.Copy()
		stored.Updated = now
		stored.Status = "accepting"
		stored.Role = 'B'
		stored.Mine = true
		stored.MakerKey = makerKey
		// Currencies stamp unconditionally: tickers are invariant across partial fills.
		stored.OrigFromCurrency = prevOrder.FromCurrency
		stored.OrigToCurrency = prevOrder.ToCurrency
		// Backfill the orig* amounts only when zero (= unset): the reject restore
		// rebuilds FromAmount/ToAmount from them, so a re-take must never clobber them.
		if stored.OrigFromAmount == 0 {
			stored.OrigFromAmount = prevOrder.FromAmount
		}
		if stored.OrigToAmount == 0 {
			stored.OrigToAmount = prevOrder.ToAmount
		}
		// The role-named address fields take the taker's own from/to on purpose:
		// toDetailResult renders them by role, so this frame reads taker-relative.
		stored.MakerAddress = p.FromAddress
		stored.TakerAddress = p.ToAddress
		stored.Utxos = proofs
		stored.UsedCoins = usedCoins
		stored.FeeUtxos = feeInputs
		stored.UtxoCurrency = prevOrder.ToCurrency
	})
	// Release only after the commit: the keys move from the reservation onto the
	// order's own Utxos/FeeUtxos, so the exclusion never opens a gap.
	n.store.ReleaseReserve(key)
	revertTake := func() {
		// Restore the exact pre-take record: a failed take must leave no phantom
		// local swap on disk, and prevOrder was captured under store.mu.
		if prevOrder != nil {
			n.store.Update(key, func(s *Order) { *s = *prevOrder.Copy() })
		}
		delete(n.sessions, key)
		if err := n.persistNow(); err != nil {
			xlog.Error("swap revert persist failed; on-disk state may diverge from memory", "order", key, "err", err)
		}
	}
	// Drive the session pin and the Accepting destination from the live committed
	// record: the HTTP snapshot predates the engine re-check.
	live := n.store.Get(key)
	if live == nil {
		// Unreachable: the take just committed this record on the engine
		// goroutine and no other engine closure can interleave. Fail closed.
		revertTake()
		return nil, makeError(errUnknown, "dxTakeOrder", "order vanished after commit")
	}
	n.newTakerSession(live, p, tPrivArr, tPub)
	// Durable write before send (like MakeOrder): a crash between the two must never
	// leave the hub holding a take our local state lacks — that strands the taker's deposit.
	if err := n.persistNow(); err != nil {
		revertTake()
		return nil, makeError(errUnknown, "dxTakeOrder", err.Error())
	}
	if err := n.conn.WritePacket(pkt, live.HubAddress); err != nil {
		revertTake()
		return nil, makeError(errUnknown, "dxTakeOrder", err.Error())
	}
	xlog.Info("take broadcast", "order", key, "hub", live.SNodePubkey)
	n.registerTakeRetry(key, p)
	return n.store.Get(key), nil
}

// CancelOrderParams are the parsed dxCancelOrder arguments.
type CancelOrderParams struct {
	ID string
}

// CancelOrder broadcasts an xbcTransactionCancel packet for an order and returns the dxCancelOrder response shape.
func (n *Node) CancelOrder(p CancelOrderParams) (*Order, *rpcError) {
	if e := n.requireWrite("dxCancelOrder"); e != nil {
		return nil, e
	}
	o := n.store.Get(p.ID)
	if o == nil {
		return nil, makeError(errTxNotFound, "dxCancelOrder", "")
	}
	if !o.Mine {
		return nil, makeError(errTxNotFound, "dxCancelOrder", "")
	}
	cfg := n.cfg()
	if cfg == nil || cfg.Connectors[o.FromCurrency] == nil {
		return nil, makeError(errNoSession, "dxCancelOrder", "")
	}
	reason := uint32(crRpcRequest)
	var rerr *rpcError
	n.submit(func() {
		// Re-check on the engine: the order may have vanished since the HTTP snapshot.
		cur := n.store.Get(p.ID)
		if cur == nil {
			rerr = makeError(errTxNotFound, "dxCancelOrder", "")
			return
		}
		// Race guard: status may have advanced past "created" between the RPC gate
		// and this engine closure; past its window the cancel fails as invalid-state.
		if stateOrdinal(cur.Status) > 6 {
			rerr = makeError(errInvalidState, "dxCancelOrder", "")
			return
		}
		if e := n.sendCancelTransaction(p.ID, reason); e != nil {
			rerr = e
			return
		}
		now := NowMicro()
		// Targeted field write: copying in the HTTP snapshot would clobber concurrent updates.
		n.store.Update(p.ID, func(stored *Order) {
			stored.Status = "canceled"
			stored.Updated = now
		})
		n.dropTakeRetry(p.ID)
		o.Status = "canceled"
		o.Updated = now
		n.enqueueRefund(p.ID, nil)
		n.persist()
	}, true)
	if rerr != nil {
		return nil, rerr
	}
	return o, nil
}

// SetExchangeStarted toggles the exchange/hub role flag; production never sets it — only tests exercise the cancel handler's exchange branch.
func (n *Node) SetExchangeStarted(v bool) { n.exchangeStarted.Store(v) }

// ExchangeStarted reports whether the exchange/hub role flag is set.
func (n *Node) ExchangeStarted() bool { return n.exchangeStarted.Load() }

func (n *Node) sessionFor(idHex string) *SwapSession {
	return n.sessions[idHex]
}

// sendCancelTransaction signs and broadcasts the cancel packet only; local state
// changes (status, history, refund) belong to the caller.
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
	xlog.Info("cancel: cancel packet sent", "order", idHex, "reason", reason, "reasonText", TxCancelReasonText(reason))
	return nil
}

// markStale backdates Updated by 241 s so the next rebroadcast cycle re-offers the
// order immediately — that gate only re-posts orders at least 240 s old.
func (n *Node) markStale(o *Order) {
	o.Updated = NowMicro() - 241_000_000
}

// Reject calls these like the C++ unlock helpers, but Go's take locks release in
// clearUsedCoins, so both stay no-ops.
func (n *Node) onUnlockCoins(o *Order)    {}
func (n *Node) onUnlockFeeUtxos(o *Order) {}

func (n *Node) handleRemoteCancel(pkt *proto.Packet, b *proto.CancelBody) {
	idHex := hexEncode(b.ID[:])
	o := n.store.Get(idHex)
	if o == nil {
		if cancelDedup.Event(idHex) {
			gossipLogger(nil).Debug("cancel: order lookup failed", "order", idHex)
		}
		return
	}
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
		// Disarm the retry only after verification: a forged cancel must never
		// disarm the entry.
		n.dropTakeRetry(idHex)
		xlog.Info("cancel: counterparty requested cancel", "order", idHex)
		return
	}
	if o = n.store.Get(idHex); o == nil {
		xlog.Info("cancel: order not found on recheck", "order", idHex)
		return
	}
	iCanceled := false
	if o.MakerKey != "" {
		if ok, _ := n.signer.VerifyAgainst(pkt, o.MakerKey); ok {
			iCanceled = true
		}
	}
	snOK, _ := n.signer.VerifyAgainst(pkt, o.SNodePubkey)
	othOK, _ := n.signer.VerifyAgainst(pkt, o.OtherPubkey)
	if !snOK && !othOK && !iCanceled {
		// Own orders bypass the dedup: forged-cancel attempts against our orders
		// must stay visible in the main log.
		if o.Mine {
			xlog.Info("cancel: bad packet signature for cancelation request on order, not canceling", "order", idHex)
			return
		}
		if cancelBadSigDedup.Event(idHex) {
			gossipLogger(o).Info("cancel: bad packet signature for cancelation request on order, not canceling", "order", idHex)
		}
		return
	}
	if _, e := n.connector(o.FromCurrency); e != nil {
		if o.Mine {
			xlog.Warn("cancel: no connector for currency, not canceling", "order", idHex, "currency", o.FromCurrency)
			return
		}
		if cancelNoConnectorDedup.Event(o.FromCurrency) {
			gossipLogger(o).Warn("cancel: no connector for currency, not canceling", "order", idHex, "currency", o.FromCurrency)
		}
		return
	}

	if o.Mine && stateOrdinal(o.Status) <= 2 && !iCanceled {
		n.store.Update(idHex, n.markStale)
		n.dropTakeRetry(idHex)
		xlog.Info("cancel: cancel received, rebroadcasting order on another service node", "order", idHex)
		return
	} else if stateOrdinal(o.Status) < 6 { // pre-deposit: no refund needed, retire to history
		n.store.MoveToHistoryU32(idHex, "canceled", b.Reason, NowMicro())
		n.dropTakeRetry(idHex)
		gossipLogger(o).Info("cancel: counterparty cancel request", "order", idHex, "reason", b.Reason, "reasonText", TxCancelReasonText(b.Reason))
		return
	} else if o.Status == "canceled" {
		// Cancel packets repeat (hub redelivery), so dedup logs only the first
		// sighting; repeats collapse into the periodic summary.
		if cancelAlreadyCanceledDedup.Event(idHex) {
			gossipLogger(o).Info("cancel: already canceled", "order", idHex)
		}
		return
	} else if !o.DepositSent { // deposit never sent: nothing on-chain to refund, cancel in place
		n.store.Update(idHex, func(o *Order) {
			o.Status = "canceled"
			o.Reason = b.Reason
			o.Updated = NowMicro()
		})
		n.dropTakeRetry(idHex)
		gossipLogger(o).Info("cancel: counterparty cancel request", "order", idHex)
		n.persist()
		return
	} else if o.CounterpartyRedeemed {
		xlog.Info("cancel: counterparty already redeemed, ignore cancel", "order", idHex)
		return
	}

	if o.RefundTx == "" {
		n.store.Update(idHex, func(o *Order) {
			o.Status = "canceled"
			o.Reason = b.Reason
			o.Updated = NowMicro()
		})
		n.dropTakeRetry(idHex)
		xlog.Info("cancel: could not find a refund transaction for order", "order", idHex)
		n.persist()
		return
	}

	n.store.RemovePendingPackets(idHex)
	// "canceled" (refund pending), never "rolled back" (refund done): the
	// broadcast below may defer to the locktime sweep, and every recovery
	// path keys on "canceled" — the success flip in postRefundTask, the
	// stored sweep's skip-list, and restoreSwap's refund-pending rule.
	// Writing "rolled back" pre-broadcast strands sessionless records (the
	// stored sweep skips "rolled back") and poisons nothing on redelivery
	// (already-canceled dedups before this branch).
	n.store.Update(idHex, func(o *Order) {
		o.Status = "canceled"
		o.Reason = b.Reason
		o.Updated = NowMicro()
	})
	n.dropTakeRetry(idHex)
	if o.RefundTx != "" {
		n.enqueueRefund(idHex, nil)
	}
	xlog.Info("cancel: rollback initiated", "order", idHex, "reason", b.Reason, "reasonText", TxCancelReasonText(b.Reason))
	n.persist()
}

func (n *Node) handleRemoteReject(pkt *proto.Packet, b *proto.RejectBody) {
	idHex := hexEncode(b.ID[:])
	o := n.store.Get(idHex)
	if o == nil {
		return
	}
	if o.Role != 'B' || stateOrdinal(o.Status) > 3 {
		return
	}
	if ok, _ := n.signer.VerifyAgainst(pkt, o.SNodePubkey); !ok {
		return
	}
	n.store.Update(idHex, func(o *Order) {
		o.Reason = b.Reason
	})
	xlog.Info("reject: order rejected by servicenode", "order", idHex, "reason", b.Reason, "reasonText", TxCancelReasonText(b.Reason))

	n.store.Update(idHex, func(o *Order) {
		o.Reason = b.Reason
		o.Status = "open"
		o.clearUsedCoins()
	})
	n.onUnlockCoins(o)
	n.onUnlockFeeUtxos(o)
	n.store.RemovePendingPackets(idHex)
	xlog.Info("reject: order restored to pending", "order", idHex)
	n.persist()
	if b.Reason == uint32(crNotAccepted) {
		n.scheduleTakeRetry(idHex)
	} else {
		n.dropTakeRetry(idHex)
	}
}
