package discovery

import (
	"context"
	"errors"
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
	// ExplicitAddrs are peer addresses (host:port) tried in addition to discovered peers; the -addnode set.
	ExplicitAddrs []string
	// SeedOverride adds extra explicit dial candidates; the built-in seeds are still tried.
	SeedOverride []string
	// TargetPeers is how many healthy outbound peers to maintain. Defaults to 8.
	TargetPeers int
	// DialCooldown is how long a failed or used address is skipped before being re-candidated (default 2 minutes).
	DialCooldown time.Duration
	// BanThreshold is the score at which a peer is disconnected and banned (default 100).
	BanThreshold int
	// BanDuration is how long a banned peer is excluded (default 24 hours).
	BanDuration time.Duration
	// Dialer optionally overrides the connect function (used by tests); the default is p2p.DialContext.
	Dialer func(ctx context.Context, addr string, magic [4]byte, timeout time.Duration) (*p2p.Conn, error)
}

// PeerManager maintains N outbound Blocknet peers behind a ReadPacket/WritePacket surface that serves as an api.XConn.
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
	// ctx is the manager's own lifecycle context, not Start's, so Close cancels in-flight dials.
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	seeds    []string
	seedOnce sync.Once

	mu            sync.Mutex
	peers         map[string]*p2p.Conn
	rr            int
	explicit      []string
	explicitSet   map[string]bool
	explicitTried map[string]time.Time

	snReg *servicenode.Registry

	// decodeDedup collapses repeated decode-failure log lines per peer. It gates
	// logging only: misbehaviour scoring in readLoop stays unconditional.
	decodeDedup *xlog.Dedupe

	dialCooldown time.Duration

	banThreshold int
	banDuration  time.Duration
	misbehavior  map[string]int
	banned       map[string]time.Time
}

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
		decodeDedup:   xlog.NewDedupe(60*time.Second, summarizeDecodeReject),
		dialCooldown:  dialCooldown,
		banThreshold:  banThreshold,
		banDuration:   banDuration,
		misbehavior:   make(map[string]int),
		banned:        make(map[string]time.Time),
	}
}

// Start seeds the address manager and runs maintenance in the background until Close or ctx cancellation.
func (m *PeerManager) Start(ctx context.Context) {
	m.seedAddrMan()
	m.connectUpTo(ctx, m.target)
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.maintain(ctx)
	}()
}

// bootstrapAddrs is the seed source, a package var so tests can pin it hermetically.
var bootstrapAddrs = p2p.BootstrapAddrs

// bootstrapSeeds resolves once so the per-tick refresh never re-runs the blocking DNS lookup.
func (m *PeerManager) bootstrapSeeds() []string {
	m.seedOnce.Do(func() {
		m.seeds = bootstrapAddrs(m.network)
	})
	return m.seeds
}

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
			// Re-seed after Prune: re-adding refreshes seen so future prunes keep the static set.
			m.seedAddrMan()
			m.pruneBans()
		}
	}
}

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
			// Explicit addrs may be hostnames the addrman never tracks, so their cooldown lives in explicitTried.
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

func (m *PeerManager) candidates() []string {
	var c []string
	c = append(c, m.explicit...)
	for _, e := range m.addrMan.Random(m.addrMan.Count()) {
		c = append(c, net.JoinHostPort(e.IP.String(), itoa(e.Port)))
	}
	c = append(c, m.bootstrapSeeds()...)
	return c
}

func (m *PeerManager) connectOne(addr string) {
	m.mu.Lock()
	if m.explicitSet[addr] {
		m.explicitTried[addr] = now()
	}
	m.mu.Unlock()
	m.addrMan.MarkTried(addr)
	conn, err := m.dial(m.ctx, addr, m.magic, 30*time.Second)
	if err != nil {
		m.mu.Lock()
		delete(m.peers, addr)
		m.mu.Unlock()
		return
	}
	m.mu.Lock()
	m.peers[addr] = conn
	delete(m.misbehavior, addr)
	delete(m.banned, addr)
	m.mu.Unlock()
	xlog.Info("peer connected", "peer", addr)

	// Start the read loop before the getaddr write so incoming traffic keeps flowing even if that send blocks.
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.readLoop(addr, conn)
	}()

	if err := conn.SendCommand(p2p.CmdGetAddr, nil); err != nil {
		xlog.Debug("getaddr failed", "peer", addr, "err", err)
	}
}

func (m *PeerManager) readLoop(addr string, conn *p2p.Conn) {
	defer func() {
		_ = conn.Close()
		m.mu.Lock()
		delete(m.peers, addr)
		// The misbehaviour tally is per-connection: any disconnect drops it, so a reconnecting peer starts clean.
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
				if first := m.decodeDedup.Event(addr + "|corrupt"); first {
					xlog.With("p2p", true).Debug("peer xbridge payload decode failed", "peer", addr, "err", derr)
				}
				m.misbehave(addr, 10)
				continue
			}
			pkt, perr := proto.Unmarshal(pktBytes)
			if perr != nil {
				// A sized-but-undecodable body carries no penalty, matching Session::processPacket's DoS(0) (xbridgesession.cpp).
				kind := "corrupt"
				if errors.Is(perr, proto.ErrUnsupportedVersion) {
					kind = "version"
				}
				if first := m.decodeDedup.Event(addr + "|" + kind); first {
					xlog.With("p2p", true).Debug("peer xbridge packet unmarshal failed", "peer", addr, "err", perr)
				}
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
				xlog.Warn("p2p: addr payload rejected", "peer", addr, "err", aerr)
				m.misbehave(addr, 20)
				break
			}
			m.addrMan.AddSlice(entries)
		case p2p.CmdGetAddr:
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
			// Store the raw payload too: the SNLIST echo must reproduce these bytes exactly, so nothing is re-encoded here.
			m.snReg.AddPing(sn, msg.Payload)
		case servicenode.CmdSNList:
			_, pings := m.snReg.AcceptedRawPings()
			for _, raw := range pings {
				if err := conn.SendCommand(servicenode.CmdSNListPing, raw); err != nil {
					xlog.Debug("servicenode: SNLISTPING send failed", "peer", addr, "err", err)
					break
				}
			}
		default:
			// Handshake traffic is consumed by p2p's dial-time handshake and addrv2 is never negotiated,
			// so strays that reach this default are deliberately ignored.
		}
	}
}

// ReadPacket blocks until a packet arrives from any peer; after Close it may still return buffered packets before io.EOF.
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

// WritePacket sends p to all live peers; it returns the last send error, or an error when no peers are connected.
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

// Peers returns the addresses of currently connected (post-handshake) peers.
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

// AddrCount returns the number of discovered peer addresses held by the address manager.
func (m *PeerManager) AddrCount() int {
	return m.addrMan.Count()
}

// ServiceNodes returns the servicenode registry populated from SNREGISTER, SNPING, and SNLISTPING messages.
func (m *PeerManager) ServiceNodes() *servicenode.Registry {
	return m.snReg
}

// summarizeDecodeReject keys each summary by "addr|kind" so a corrupt-frame trickle cannot hide inside a version-flood summary.
func summarizeDecodeReject(key string, total int, over time.Duration) {
	xlog.With("p2p", true).Debug("peer xbridge decode failures suppressed", "key", key, "count", total, "over", over.Round(time.Second).String())
}

// misbehave bans and disconnects immediately at the threshold, unlike the C++ node, which defers the ban to a later checkpoint.
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

func (m *PeerManager) bannedLocked(addr string) bool {
	exp, ok := m.banned[addr]
	return ok && now().Before(exp)
}

func (m *PeerManager) isBanned(addr string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.bannedLocked(addr)
}

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

// Close cancels in-flight dials and joins every goroutine before returning; Start must not be called concurrently with Close.
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
		// xbridgeCh is deliberately left open: readLoop's send selects against
		// m.done, so closing it here would race a send-on-closed-channel panic.
		m.wg.Wait()
		// Flush also stops the dedupe sweeper, so a closed manager holds no goroutine.
		m.decodeDedup.Flush()
	})
	return nil
}

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
