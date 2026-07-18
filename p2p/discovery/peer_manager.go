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
	// Dialer optionally overrides the connect function (used by tests). The
	// default is p2p.Dial.
	Dialer func(addr string, magic [4]byte, timeout time.Duration) (*p2p.Conn, error)
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
	dial    func(addr string, magic [4]byte, timeout time.Duration) (*p2p.Conn, error)

	addrMan   *AddrMan
	xbridgeCh chan peerPacket
	done      chan struct{}
	once      sync.Once

	mu       sync.Mutex
	peers    map[string]*p2p.Conn // addr -> conn (nil while connecting)
	rr       int                  // round-robin cursor over candidates
	explicit []string

	// snReg is the servicenode registry, populated from SNREGISTER / SNPING /
	// SNLISTPING P2P messages (mirrors a core XBridge wallet learning the
	// network token set). Exposed via ServiceNodes() for dxGetNetworkTokens.
	snReg *servicenode.Registry
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
		dial = p2p.Dial
	}
	explicit := opts.ExplicitAddrs
	if len(opts.SeedOverride) > 0 {
		// SeedOverride acts as the explicit set when provided.
		explicit = append(explicit, opts.SeedOverride...)
	}
	return &PeerManager{
		magic:     magic,
		network:   network,
		opts:      opts,
		target:    target,
		dial:      dial,
		explicit:  explicit,
		addrMan:   NewAddrMan(),
		xbridgeCh: make(chan peerPacket, 256),
		done:      make(chan struct{}),
		peers:     make(map[string]*p2p.Conn),
		snReg:     servicenode.NewRegistry(),
	}
}

// Start seeds the address manager and begins maintaining the peer pool. It
// returns immediately; maintenance runs in a background goroutine until Close
// (or the context is cancelled).
func (m *PeerManager) Start(ctx context.Context) {
	// Seed the AddrMan with the network's bootstrap addresses so we always have
	// a candidate set even before any peer advertises addresses.
	for _, a := range p2p.BootstrapAddrs(m.network) {
		if host, portStr, err := net.SplitHostPort(a); err == nil {
			if ip := net.ParseIP(host); ip != nil {
				if port, perr := parsePort(portStr); perr == nil {
					m.addrMan.Add(ip, port, 0)
				}
			}
		}
	}
	m.connectUpTo(ctx, m.target)
	go m.maintain(ctx)
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
		go m.connectOne(ctx, cand)
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
		if _, ok := m.peers[c]; !ok {
			m.rr = (idx + 1) % len(cands)
			return c
		}
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
	c = append(c, p2p.BootstrapAddrs(m.network)...)
	return c
}

// connectOne dials a single candidate, registers the live conn, and starts its
// read loop. On failure it frees the reservation so the address can be retried.
func (m *PeerManager) connectOne(ctx context.Context, addr string) {
	conn, err := m.dial(addr, m.magic, 30*time.Second)
	if err != nil {
		m.mu.Lock()
		delete(m.peers, addr)
		m.mu.Unlock()
		return
	}
	m.mu.Lock()
	m.peers[addr] = conn
	m.mu.Unlock()
	xlog.Info("peer connected", "peer", addr)

	// Start reading from the peer first so its addr/XBridge traffic is consumed
	// even if the getaddr write below is slow to be read by the peer.
	go m.readLoop(addr, conn)

	// Ask the peer for its known addresses (enables further discovery).
	if err := conn.SendCommand(p2p.CmdGetAddr, nil); err != nil {
		// Non-fatal; we can still relay XBridge from this peer.
		xlog.Debug("getaddr failed", "peer", addr, "err", err)
	}
}

// readLoop processes messages from one peer until it errors or we're closed.
func (m *PeerManager) readLoop(addr string, conn *p2p.Conn) {
	defer func() {
		conn.Close()
		m.mu.Lock()
		delete(m.peers, addr)
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
				continue
			}
			pkt, perr := proto.Unmarshal(pktBytes)
			if perr != nil {
				continue
			}
			select {
			case m.xbridgeCh <- peerPacket{peer: addr, pkt: pkt}:
			case <-m.done:
				return
			}
		case p2p.CmdAddr:
			if entries, aerr := p2p.ParseAddr(msg.Payload); aerr == nil {
				m.addrMan.AddSlice(entries)
			}
		case p2p.CmdGetAddr:
			sample := m.addrMan.Random(64)
			_ = conn.SendCommand(p2p.CmdAddr, p2p.MarshalAddr(sample))
		case p2p.CmdPing:
			_ = conn.SendCommand(p2p.CmdPong, msg.Payload)
		case servicenode.CmdSNRegister:
			sn, derr := servicenode.ParseServiceNode(msg.Payload)
			if derr != nil {
				xlog.Warn("servicenode: SNREGISTER parse error", "peer", addr, "err", derr)
				break
			}
			m.snReg.AddRegistration(sn)
		case servicenode.CmdSNPing, servicenode.CmdSNListPing:
			sn, derr := servicenode.ParseServiceNodePing(msg.Payload)
			if derr != nil {
				xlog.Warn("servicenode: SNPING/SNLISTPING parse error", "peer", addr, "cmd", msg.Command, "err", derr)
				break
			}
			m.snReg.AddPing(sn)
		case servicenode.CmdSNList:
			xlog.Debug("servicenode: ignoring SNLIST (stock client does not answer)", "peer", addr)
			// A stock XBridge client does not answer SNLIST (only XRouter
			// does); ignore it. We learn the SN set from relayed pings.
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

// WritePacket broadcasts an XBridge packet to all live peers. It returns an
// error if there are no connected peers, or the last send error seen.
func (m *PeerManager) WritePacket(p *proto.Packet) error {
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
		if err := c.WritePacket(p); err != nil {
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

// Close stops maintenance, closes all peer connections, and tears down the
// manager. It is safe to call multiple times.
func (m *PeerManager) Close() error {
	m.once.Do(func() {
		close(m.done)
		m.mu.Lock()
		for _, c := range m.peers {
			if c != nil {
				c.Close()
			}
		}
		m.mu.Unlock()
		// Note: xbridgeCh is intentionally left open — a blocked send in
		// readLoop selects against m.done and returns rather than sending to a
		// closed channel, so we avoid a send-on-closed-channel panic.
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
