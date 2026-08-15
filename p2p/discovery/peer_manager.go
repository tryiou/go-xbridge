package discovery

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	xlog "go-xbridge/log"
	"go-xbridge/p2p"
	"go-xbridge/p2p/servicenode"
	"go-xbridge/proto"
)

// Options configures a PeerManager.
type Options struct {
	// ExplicitAddrs are peer addresses (host:port, may be hostnames) to always
	// attempt, in addition to discovered peers. These correspond to -addnode.
	ExplicitAddrs []string
	// SeedOverride replaces the network's built-in seeds (rarely used).
	SeedOverride []string
	// TargetPeers is how many healthy outbound peers to maintain. Defaults to 8.
	TargetPeers int
	// DialCooldown is how long a failed/used address is skipped before being
	// re-candidated. Defaults to 2 minutes.
	DialCooldown time.Duration
	// BanThreshold is the misbehaviour score at which a peer is disconnected
	// and excluded from re-candidating (C++ -banscore, default 100). A peer
	// scores +10 per malformed/undersized XBRIDGE envelope and +20 per rejected
	// addr payload (net_processing.cpp:2877, :1825-1830). Defaults to 100.
	BanThreshold int
	// BanDuration is how long a banned peer is excluded. Defaults to 24 hours
	// (C++ default -bantime).
	BanDuration time.Duration
	// Dialer optionally overrides the connect function (used by tests). The
	// default is p2p.DialContext, which aborts an in-flight dial when its
	// context is cancelled (Close), so PeerManager.Close stays bounded.
	Dialer func(ctx context.Context, addr string, magic [4]byte, timeout time.Duration) (*p2p.Conn, error)
}

// PeerManager maintains N outbound Blocknet peers and exposes a single
// ReadPacket/WritePacket surface (so it is a drop-in for *p2p.Conn behind the
// api.XConn interface). XBridge packets from any peer are funnelled into a
// consumer channel; WritePacket broadcasts to all live peers.
type PeerManager struct {
	magic   [4]byte
	network string
	opts    Options
	target  int
	dial    func(ctx context.Context, addr string, magic [4]byte, timeout time.Duration) (*p2p.Conn, error)

	addrMan   *AddrMan
	xbridgeCh chan peerPacket
	done      chan struct{}
	once      sync.Once
	// ctx is the manager's own lifecycle context, cancelled by Close so an
	// in-flight dial aborts immediately (dial is ctx-aware via DialContext).
	ctx    context.Context
	cancel context.CancelFunc
	// wg tracks the maintain, connectOne, and per-peer readLoop goroutines so
	// Close joins them all before returning (C++ joins every xbridge thread on
	// shutdown, xbridgeapp.cpp:530-544).
	wg sync.WaitGroup

	// seeds is the resolved bootstrap address list for the network, populated
	// once on first use so we do not re-run a blocking DNS lookup every tick.
	seeds    []string
	seedOnce sync.Once

	mu            sync.Mutex
	peers         map[string]*p2p.Conn // addr -> conn (nil while connecting)
	rr            int                  // round-robin cursor over candidates
	explicit      []string
	explicitSet   map[string]bool
	explicitTried map[string]time.Time

	// snReg is the servicenode registry, populated from SNREGISTER / SNPING /
	// SNLISTPING P2P messages (mirrors a core XBridge wallet learning the
	// network token set). Exposed via ServiceNodes() for dxGetNetworkTokens.
	// The registry also keeps the raw accepted ping payloads (Registry.raw) so
	// an inbound SNLIST is answered byte-identically, matching C++ which
	// re-serializes from its registry (net_processing.cpp:2992-3001).
	snReg *servicenode.Registry

	// dialCooldown is how long a failed/used address is skipped before being
	// re-candidated, so unreachable peers are not dialed every maintain tick.
	dialCooldown time.Duration

	// banThreshold / banDuration are the misbehaviour gate (C++ -banscore /
	// -bantime). misbehave is the per-peer score map; banned holds the ban
	// expiry for disqualified peers so nextCandidate never re-dials them.
	banThreshold int
	banDuration  time.Duration
	misbehavior  map[string]int
	banned       map[string]time.Time
}

// peerPacket couples an XBridge packet with the TCP peer address it arrived
// from, so consumers can correlate received packets with connect/disconnect logs.
type peerPacket struct {
	peer string
	pkt  *proto.Packet
}

// New constructs a PeerManager. It does not connect until Start is called.
func New(magic [4]byte, network string, opts Options) *PeerManager {
	target := opts.TargetPeers
	if target <= 0 {
		target = 8
	}
	dial := opts.Dialer
	if dial == nil {
		dial = p2p.DialContext
	}
	explicit := opts.ExplicitAddrs
	if len(opts.SeedOverride) > 0 {
		// SeedOverride acts as the explicit set when provided.
		explicit = append(explicit, opts.SeedOverride...)
	}
	explicitSet := make(map[string]bool, len(explicit))
	for _, a := range explicit {
		explicitSet[a] = true
	}
	dialCooldown := opts.DialCooldown
	if dialCooldown <= 0 {
		dialCooldown = 2 * time.Minute
	}
	banThreshold := opts.BanThreshold
	if banThreshold <= 0 {
		banThreshold = 100
	}
	banDuration := opts.BanDuration
	if banDuration <= 0 {
		banDuration = 24 * time.Hour
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &PeerManager{
		magic:         magic,
		network:       network,
		opts:          opts,
		target:        target,
		dial:          dial,
		ctx:           ctx,
		cancel:        cancel,
		explicit:      explicit,
		explicitSet:   explicitSet,
		explicitTried: make(map[string]time.Time),
		addrMan:       NewAddrMan(),
		xbridgeCh:     make(chan peerPacket, 256),
		done:          make(chan struct{}),
		peers:         make(map[string]*p2p.Conn),
		snReg:         servicenode.NewRegistry(),
		dialCooldown:  dialCooldown,
		banThreshold:  banThreshold,
		banDuration:   banDuration,
		misbehavior:   make(map[string]int),
		banned:        make(map[string]time.Time),
	}
}

// Start seeds the address manager and begins maintaining the peer pool. It
// returns immediately; maintenance runs in a background goroutine until Close
// (or the context is cancelled).
func (m *PeerManager) Start(ctx context.Context) {
	// Seed the AddrMan with the network's bootstrap addresses so we always have
	// a candidate set even before any peer advertises addresses.
	m.seedAddrMan()
	m.connectUpTo(ctx, m.target)
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.maintain(ctx)
	}()
}

// bootstrapAddrs is the seed source; overridable in tests to keep them
// hermetic (mirrors the addrman `now` clock hook).
var bootstrapAddrs = p2p.BootstrapAddrs

// bootstrapSeeds returns the network's resolved bootstrap addresses, resolving
// them (from bootstrapAddrs) only once. The result is cached so the per-tick
// seedAddrMan refresh and candidates() never re-trigger a DNS lookup.
func (m *PeerManager) bootstrapSeeds() []string {
	m.seedOnce.Do(func() {
		m.seeds = bootstrapAddrs(m.network)
	})
	return m.seeds
}

// seedAddrMan registers the network's bootstrap seeds with the address manager.
// Re-adding a known seed refreshes its seen time (so Prune never evicts the
// static seed set) while preserving its dial-cooldown state.
func (m *PeerManager) seedAddrMan() {
	for _, a := range m.bootstrapSeeds() {
		if host, portStr, err := net.SplitHostPort(a); err == nil {
			if ip := net.ParseIP(host); ip != nil {
				if port, perr := parsePort(portStr); perr == nil {
					m.addrMan.Add(ip, port, 0)
				}
			}
		}
	}
}

// maintain keeps the pool at the target size on a ticker.
func (m *PeerManager) maintain(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.done:
			return
		case <-ticker.C:
			m.connectUpTo(ctx, m.target)
			m.addrMan.Prune(30 * time.Minute)
			// Prune drops discovered peers whose seen time went stale. Re-seed
			// afterwards so the static bootstrap set is never pruned away (which
			// would also wipe its dial-cooldown state).
			m.seedAddrMan()
			// Drop expired ban entries so a long-running daemon's maps stay
			// bounded (reconnecting peers are also cleared in connectOne).
			m.pruneBans()
		}
	}
}

// pruneBans removes ban entries whose window has expired.
func (m *PeerManager) pruneBans() {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := now()
	for addr, exp := range m.banned {
		if !n.Before(exp) {
			delete(m.banned, addr)
		}
	}
}

// connectUpTo launches outbound connections until `want` healthy peers are
// live or we run out of candidates.
func (m *PeerManager) connectUpTo(ctx context.Context, want int) {
	for m.liveCount() < want {
		select {
		case <-ctx.Done():
			return
		case <-m.done:
			return
		default:
		}
		cand := m.nextCandidate()
		if cand == "" {
			// No more candidates right now; wait for the next tick or a peer
			// to advertise more addresses.
			return
		}
		m.mu.Lock()
		m.peers[cand] = nil // reserve; set to the real conn on success
		m.mu.Unlock()
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			m.connectOne(cand)
		}()
	}
}

// nextCandidate returns the next candidate address we are not already connected
// to (or trying), advancing a round-robin cursor. Returns "" if none remain.
func (m *PeerManager) nextCandidate() string {
	cands := m.candidates()
	if len(cands) == 0 {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := 0; i < len(cands); i++ {
		idx := (m.rr + i) % len(cands)
		c := cands[idx]
		if _, ok := m.peers[c]; ok {
			continue
		}
		if m.bannedLocked(c) {
			continue
		}
		if m.explicitSet[c] {
			// Explicit addrs are not tracked by the addrman (they may be
			// hostnames), so their cooldown lives here.
			if t, tried := m.explicitTried[c]; tried && now().Sub(t) < m.dialCooldown {
				continue
			}
		} else if !m.addrMan.NeedsTry(c, m.dialCooldown) {
			continue
		}
		m.rr = (idx + 1) % len(cands)
		return c
	}
	return ""
}

// candidates returns the prioritized candidate list: explicit addrs, then
// discovered peers, then network bootstrap seeds.
func (m *PeerManager) candidates() []string {
	var c []string
	c = append(c, m.explicit...)
	for _, e := range m.addrMan.Random(m.addrMan.Count()) {
		c = append(c, net.JoinHostPort(e.IP.String(), itoa(e.Port)))
	}
	c = append(c, m.bootstrapSeeds()...)
	return c
}

// connectOne dials a single candidate, registers the live conn, and starts its
// read loop. The dial uses the manager's own context (m.ctx), so Close cancels
// an in-flight dial immediately; on failure it frees the reservation so the
// address can be retried.
func (m *PeerManager) connectOne(addr string) {
	m.mu.Lock()
	if m.explicitSet[addr] {
		m.explicitTried[addr] = now()
	}
	m.mu.Unlock()
	m.addrMan.MarkTried(addr)
	// Dial with the manager's own context so Close cancels an in-flight dial
	// immediately instead of waiting out the timeout.
	conn, err := m.dial(m.ctx, addr, m.magic, 30*time.Second)
	if err != nil {
		m.mu.Lock()
		delete(m.peers, addr)
		m.mu.Unlock()
		return
	}
	m.mu.Lock()
	m.peers[addr] = conn
	// C++ nMisbehavior is per-connection and resets on reconnect; only the
	// BanMan list persists, for the ban window. Clearing both here means a
	// peer that reconnects after its 24 h ban starts clean (and prunes the
	// stale entries so the maps stay bounded).
	delete(m.misbehavior, addr)
	delete(m.banned, addr)
	m.mu.Unlock()
	xlog.Info("peer connected", "peer", addr)

	// Start reading from the peer first so its addr/XBridge traffic is consumed
	// even if the getaddr write below is slow to be read by the peer.
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.readLoop(addr, conn)
	}()

	// Ask the peer for its known addresses (enables further discovery).
	if err := conn.SendCommand(p2p.CmdGetAddr, nil); err != nil {
		// Non-fatal; we can still relay XBridge from this peer.
		xlog.Debug("getaddr failed", "peer", addr, "err", err)
	}
}

// readLoop processes messages from one peer until it errors or we're closed.
func (m *PeerManager) readLoop(addr string, conn *p2p.Conn) {
	defer func() {
		_ = conn.Close()
		m.mu.Lock()
		delete(m.peers, addr)
		// nMisbehavior is per-connection: a disconnect (even a clean one) drops
		// the tally, so a reconnecting peer starts clean and the map stays
		// bounded.
		delete(m.misbehavior, addr)
		m.mu.Unlock()
		xlog.Info("peer disconnected", "peer", addr)
	}()
	for {
		select {
		case <-m.done:
			return
		default:
		}
		msg, err := conn.ReadMessage()
		if err != nil {
			if err != io.EOF {
				xlog.Debug("peer read loop ended", "peer", addr, "err", err)
			}
			return
		}
		switch msg.Command {
		case p2p.XBridgeNetCommand:
			pktBytes, derr := p2p.DecodeXBridgePayload(msg.Payload)
			if derr != nil {
				// Undersized/malformed xbridge envelope: C++ Misbehaves +10
				// (net_processing.cpp:2874-2878, raw.size() < 28).
				xlog.Debug("peer xbridge payload decode failed", "peer", addr, "err", derr)
				m.misbehave(addr, 10)
				continue
			}
			pkt, perr := proto.Unmarshal(pktBytes)
			if perr != nil {
				// Sized-but-undecodable body: C++ drops without a penalty (the
				// session DoS is 0, xbridgesession.cpp:312).
				xlog.Debug("peer xbridge packet unmarshal failed", "peer", addr, "err", perr)
				continue
			}
			select {
			case m.xbridgeCh <- peerPacket{peer: addr, pkt: pkt}:
			case <-m.done:
				return
			}
		case p2p.CmdAddr:
			entries, aerr := p2p.ParseAddr(msg.Payload)
			if aerr != nil {
				// Includes the >1000-record cap, which C++ answers with
				// Misbehaving +20 (net_processing.cpp:1825-1830).
				xlog.Warn("p2p: addr payload rejected", "peer", addr, "err", aerr)
				m.misbehave(addr, 20)
				break
			}
			m.addrMan.AddSlice(entries)
		case p2p.CmdGetAddr:
			// C++ ignores "getaddr" from outbound connections
			// (net_processing.cpp:2665-2668), and go-xbridge only makes
			// outbound connections, so the request is never answered.
			xlog.Debug("p2p: ignoring getaddr (outbound-only client)", "peer", addr)
		case p2p.CmdPing:
			_ = conn.SendCommand(p2p.CmdPong, msg.Payload)
		case servicenode.CmdSNRegister:
			sn, derr := servicenode.ParseServiceNode(msg.Payload)
			if derr != nil {
				xlog.Warn("servicenode: SNREGISTER parse failed", "peer", addr, "err", derr)
				break
			}
			m.snReg.AddRegistration(sn)
		case servicenode.CmdSNPing, servicenode.CmdSNListPing:
			sn, derr := servicenode.ParseServiceNodePing(msg.Payload)
			if derr != nil {
				xlog.Warn("servicenode: SNPING/SNLISTPING parse failed", "peer", addr, "cmd", msg.Command, "err", derr)
				break
			}
			// Mirror the strict-newer accept gate so SNLIST is answered with
			// exactly the pings the registry keeps (servicenodemgr.h:843-852).
			// The raw payload travels into the registry so the SNLIST echo is
			// byte-identical (C++ re-serializes from its registry,
			// net_processing.cpp:2992-3001) — no parallel map here.
			m.snReg.AddPing(sn, msg.Payload)
		case servicenode.CmdSNList:
			// C++ answers SNLIST with one SNLISTPING per known ping
			// (net_processing.cpp:2992-3001); the registry echoes the stored
			// raw accepted pings (byte-identical to C++'s re-serialization),
			// sorted by pubkey for deterministic ordering across relays.
			_, pings := m.snReg.AcceptedRawPings()
			for _, raw := range pings {
				if err := conn.SendCommand(servicenode.CmdSNListPing, raw); err != nil {
					xlog.Debug("servicenode: SNLISTPING send failed", "peer", addr, "err", err)
					break
				}
			}
		default:
			// version/verack (handshake) and addrv2 are intentionally ignored.
		}
	}
}

// ReadPacket blocks until an XBridge packet is available from any peer, or
// returns io.EOF once Close has been called. The returned peer address is the
// TCP endpoint the packet arrived from.
func (m *PeerManager) ReadPacket() (pkt *proto.Packet, peer string, err error) {
	select {
	case pp, ok := <-m.xbridgeCh:
		if !ok {
			return nil, "", io.EOF
		}
		return pp.pkt, pp.peer, nil
	case <-m.done:
		return nil, "", io.EOF
	}
}

// WritePacket sends an XBridge packet to all live peers. The destination is
// carried in the envelope: a zero dest broadcasts, a non-zero dest reaches only
// the addressed node via C++ onMessageReceived (relaying is done by the P2P
// network, mirroring App::Impl::onSend's ForEachNode fan-out, xbridgeapp.cpp:606).
// It returns an error if there are no connected peers, or the last send error seen.
func (m *PeerManager) WritePacket(p *proto.Packet, dest [20]byte) error {
	m.mu.Lock()
	conns := make([]*p2p.Conn, 0, len(m.peers))
	for _, c := range m.peers {
		if c != nil {
			conns = append(conns, c)
		}
	}
	m.mu.Unlock()
	if len(conns) == 0 {
		return fmt.Errorf("discovery: no connected peers to write to")
	}
	var lastErr error
	for _, c := range conns {
		if err := c.WritePacket(p, dest); err != nil {
			lastErr = err
		}
	}
	return lastErr
}

// Peers returns the addresses of currently-connected (handshaked) peers.
func (m *PeerManager) Peers() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.peers))
	for a, c := range m.peers {
		if c != nil {
			out = append(out, a)
		}
	}
	return out
}

// AddrCount returns the number of discovered peer addresses known to the
// address manager (useful for diagnostics and tests).
func (m *PeerManager) AddrCount() int {
	return m.addrMan.Count()
}

// ServiceNodes returns the live servicenode registry, populated from SNREGISTER /
// SNPING / SNLISTPING messages. The api.Node uses it to derive the network
// token set for dxGetNetworkTokens (mirroring C++ walletServices()).
func (m *PeerManager) ServiceNodes() *servicenode.Registry {
	return m.snReg
}

// misbehave adds score to a peer's misbehaviour tally and, at the ban
// threshold (C++ -banscore), disconnects it and excludes it from
// re-candidating for the ban window. Mirrors C++ Misbehaving
// (net_processing.cpp:2877, :1825-1830); unlike C++, the disconnect is
// immediate (a thin client has no ban-score checkpoints).
func (m *PeerManager) misbehave(addr string, score int) {
	m.mu.Lock()
	m.misbehavior[addr] += score
	total := m.misbehavior[addr]
	banned := total >= m.banThreshold
	if banned {
		m.banned[addr] = now().Add(m.banDuration)
	}
	conn := m.peers[addr]
	m.mu.Unlock()
	xlog.Warn("peer misbehaving", "peer", addr, "score", score, "total", total, "banned", banned)
	if banned && conn != nil {
		// Closing unblocks readLoop; its defer removes the peer from the pool.
		_ = conn.Close()
	}
}

// bannedLocked reports whether addr is inside its ban window. Caller must hold
// m.mu (used by nextCandidate inside its critical section).
func (m *PeerManager) bannedLocked(addr string) bool {
	exp, ok := m.banned[addr]
	return ok && now().Before(exp)
}

// isBanned reports whether addr is inside its ban window.
func (m *PeerManager) isBanned(addr string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.bannedLocked(addr)
}

// liveCount returns the number of fully-connected peers.
func (m *PeerManager) liveCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, c := range m.peers {
		if c != nil {
			n++
		}
	}
	return n
}

// Close stops maintenance, cancels in-flight dials, closes all peer
// connections, and joins every goroutine (maintain, connectOne, per-peer
// readLoop) before returning, mirroring C++'s join_all on shutdown
// (xbridgeapp.cpp:530-544). The dial phase is cancelled immediately via the
// manager context, and p2p.DialContext aborts an in-flight version handshake
// on that cancellation (NewConnCtx), so even a peer that accepts TCP but
// stalls the version exchange cannot hold the join past the cancellation
// (holds for the default Dialer). Safe to call multiple times. Start must not
// be called concurrently with Close.
func (m *PeerManager) Close() error {
	m.once.Do(func() {
		m.cancel()
		close(m.done)
		m.mu.Lock()
		for _, c := range m.peers {
			if c != nil {
				_ = c.Close()
			}
		}
		m.mu.Unlock()
		// Note: xbridgeCh is intentionally left open — a blocked send in
		// readLoop selects against m.done and returns rather than sending to a
		// closed channel, so we avoid a send-on-closed-channel panic.
		m.wg.Wait()
	})
	return nil
}

// parsePort is a helper for BootstrapAddrs seeds of the form host:port.
func parsePort(s string) (uint16, error) {
	var p uint16
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("discovery: bad port %q", s)
		}
		p = p*10 + uint16(r-'0')
	}
	return p, nil
}
