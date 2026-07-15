package api

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"xbridge-go/coins"
	"xbridge-go/config"
	"xbridge-go/crypto"
	"xbridge-go/p2p"
	discovery "xbridge-go/p2p/discovery"
	"xbridge-go/proto"
	"xbridge-go/wallet"
)

// Config tunes a Node.
type Config struct {
	// NodeAddr is the Blocknet service-node P2P address (host:port). Empty
	// disables the live feed (read-only mode with an empty order book).
	NodeAddr string
	// Magic is the network magic (mainnet a1a0a2a3 by default).
	Magic [4]byte
	// PrivKey is the 32-byte secp256k1 scalar used to sign outbound packets.
	// Empty disables order creation (dxMakeOrder/dxTakeOrder/dxCancelOrder
	// return a no-session error, matching Blocknet when no wallet is loaded).
	PrivKey []byte
	// Confs holds the parsed [TICKER] sections from xbridge.conf.
	Confs map[string]*config.CoinConf
	// Connectors maps ticker -> the wallet connector xbridge-go drives for it
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
}

// XConn is the connection surface the Node needs. Both *p2p.Conn (a single
// explicit service node, used when Config.NodeAddr is set) and
// *discovery.PeerManager (an automatically-discovered pool, used when NodeAddr
// is empty) satisfy it, so PeerManager is a drop-in replacement.
type XConn interface {
	ReadPacket() (*proto.Packet, error)
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
	cfg    *Config
	conn   XConn
	store  *Store
	signer crypto.Signer
	pubkey [33]byte
	stop   chan struct{}

	// sessMu guards sessions, the set of in-flight swaps we are a party to
	// (keyed by order-id hex). The live hub drives each one through its packet
	// sequence; the local SwapSession responds and signs the on-chain ops.
	sessMu   sync.Mutex
	sessions map[string]*SwapSession

	// blockMu guards the cached anti-replay blockHash stamped on outgoing
	// orders. C++ uses chainActive.Tip()->pprev (BLOCK best block minus one);
	// we mirror that by querying the BLOCK connector's getblockcount/getblockhash.
	blockMu sync.RWMutex
	block   [32]byte
	blockAt time.Time
}

// NewNode dials the configured peer (if any) and starts ingesting broadcasts.
func NewNode(cfg *Config, store *Store) (*Node, error) {
	n := &Node{
		cfg:      cfg,
		store:    store,
		signer:   crypto.NewBtcSigner(),
		stop:     make(chan struct{}),
		sessions: map[string]*SwapSession{},
	}
	if len(cfg.PrivKey) == 32 {
		pk, err := crypto.CompressedPubKey(cfg.PrivKey)
		if err != nil {
			return nil, err
		}
		n.pubkey = pk
	}
	if cfg.NodeAddr != "" {
		// Explicit single-peer override: skip discovery, dial the given
		// service node directly (the legacy behaviour).
		conn, err := p2p.Dial(cfg.NodeAddr, cfg.Magic, 30*time.Second)
		if err != nil {
			return nil, fmt.Errorf("api: dial service node: %w", err)
		}
		n.conn = conn
		log.Printf("xbridge-go: connected to explicit service node %s", cfg.NodeAddr)
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
		n.conn = pm
		log.Printf("xbridge-go: network discovery started on %q (target %d peers)", network, 8)
	}
	go n.feed()
	go n.blockLoop()
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

// blockConnector returns the connector for the BLOCK chain, whose block height
// backs XBridge order anti-replay (C++ stamps chainActive.Tip()->pprev).
func (n *Node) blockConnector() wallet.Connector {
	if n.cfg == nil || n.cfg.Connectors == nil {
		return nil
	}
	return n.cfg.Connectors["BLOCK"]
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

// feed reads packets from the peer and stores orders.
func (n *Node) feed() {
	for {
		select {
		case <-n.stop:
			return
		default:
		}
		pkt, err := n.conn.ReadPacket()
		if err != nil {
			select {
			case <-n.stop:
				return
			case <-time.After(200 * time.Millisecond):
				continue
			}
		}
		body, err := proto.DecodeBody(pkt.Command, pkt.Body)
		if err != nil {
			continue
		}
		maker := hexEncode(pkt.Pubkey[:])
		switch b := body.(type) {
		case *proto.OrderBody:
			o := normalizeFromOrderBody(b, maker)
			n.store.Add(o)
		case *proto.PendingTransactionBody:
			o := normalizeFromPendingBody(b, maker)
			n.store.Add(o)

		// --- swap handshake (client side, hub-driven) ---
		case *proto.HoldBody:
			n.dispatchSwap(b.ID, b.HubAddress, func(s *SwapSession) (proto.XBridgeCommand, responseBody, error) {
				return s.OnHold(b)
			})
		case *proto.InitBody:
			n.dispatchSwap(b.ID, b.HubAddress, func(s *SwapSession) (proto.XBridgeCommand, responseBody, error) {
				return s.OnInit(b)
			})
		case *proto.CreateABody:
			n.dispatchSwap(b.ID, b.HubAddress, func(s *SwapSession) (proto.XBridgeCommand, responseBody, error) {
				return s.OnCreateA(b)
			})
		case *proto.CreateBBody:
			n.dispatchSwap(b.ID, b.HubAddress, func(s *SwapSession) (proto.XBridgeCommand, responseBody, error) {
				return s.OnCreateB(b)
			})
		case *proto.ConfirmABody:
			n.dispatchSwap(b.ID, b.HubAddress, func(s *SwapSession) (proto.XBridgeCommand, responseBody, error) {
				return s.OnConfirmA(b)
			})
		case *proto.ConfirmBBody:
			n.dispatchSwap(b.ID, b.HubAddress, func(s *SwapSession) (proto.XBridgeCommand, responseBody, error) {
				return s.OnConfirmB(b)
			})
		case *proto.FinishedBody:
			n.dispatchSwap(b.ID, [20]byte{}, func(s *SwapSession) (proto.XBridgeCommand, responseBody, error) {
				return s.OnFinished(b)
			})
		}
	}
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
func (n *Node) dispatchSwap(id [32]byte, hub [20]byte, fn func(*SwapSession) (proto.XBridgeCommand, responseBody, error)) {
	n.sessMu.Lock()
	s := n.sessions[hexEncode(id[:])]
	n.sessMu.Unlock()
	if s == nil {
		return
	}
	if hub != ([20]byte{}) {
		s.hub = hub
	}
	cmd, body, err := fn(s)
	if err != nil {
		return
	}
	if body == nil {
		return
	}
	_ = n.send(cmd, body)
}

// send signs and broadcasts a handshake response packet.
func (n *Node) send(cmd proto.XBridgeCommand, body responseBody) error {
	if n.conn == nil {
		return errors.New("api: not connected to a service node")
	}
	if len(n.cfg.PrivKey) != 32 {
		return errors.New("api: no private key configured")
	}
	pkt := proto.NewPacket(cmd, body.Marshal())
	if err := n.signer.Sign(pkt, n.cfg.PrivKey); err != nil {
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
		return makeError(errNoSession, "dxMakeOrder", "not connected to a service node")
	}
	if len(n.cfg.PrivKey) != 32 {
		return makeError(errBadRequest, "dxMakeOrder", "no private key configured")
	}
	return nil
}

// MakeOrder builds, signs and (unless dry-run) broadcasts an xbcTransaction
// packet, returning the order in dxMakeOrder's response shape.
func (n *Node) MakeOrder(p MakeOrderParams) (*Order, *rpcError) {
	if e := n.requireWrite(); e != nil {
		return nil, e
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

	partial := false
	minFrom := fromAmt
	if p.Type == "partial" {
		partial = true
		if p.MinSize != "" {
			if m, err := parseXAmount(p.MinSize); err == nil {
				minFrom = m
			}
		}
	}

	var id [32]byte
	if _, err := rand.Read(id[:]); err != nil {
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

	pkt := proto.NewPacket(proto.XbcTransaction, body.Marshal())
	if err := n.signer.Sign(pkt, n.cfg.PrivKey); err != nil {
		return nil, makeError(errUnknown, "dxMakeOrder", err.Error())
	}
	if !p.DryRun {
		if err := n.conn.WritePacket(pkt); err != nil {
			return nil, makeError(errUnknown, "dxMakeOrder", err.Error())
		}
	}

	o := normalizeFromOrderBody(body, hexEncode(n.pubkey[:]))
	o.MakerAddress = p.MakerAddress
	o.TakerAddress = p.TakerAddress
	o.BlockID = hexEncode(body.BlockHash[:])
	o.Mine = true
	if p.Type == "partial" {
		o.Status = "open"
	} else {
		o.Status = "created"
	}
	if !p.DryRun {
		n.store.Add(o)
		// Begin driving the client-side deposit handshake for this order.
		n.newMakerSession(o, p)
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
// returns the dxTakeOrder response shape. (Full deposit/refund handshake is the
// swap layer, wired in a later phase; this drives the accepting broadcast and
// the exact response object dapps consume.)
func (n *Node) TakeOrder(p TakeOrderParams) (*Order, *rpcError) {
	if e := n.requireWrite(); e != nil {
		return nil, e
	}
	o := n.store.Get(p.ID)
	if o == nil {
		return nil, makeError(errTxNotFound, "dxTakeOrder", p.ID)
	}
	fromID, e := decodeAddr(o.ToCurrency, p.FromAddress)
	if e != nil {
		return nil, e
	}
	toID, e := decodeAddr(o.FromCurrency, p.ToAddress)
	if e != nil {
		return nil, e
	}

	takeAmt := o.ToAmount
	if p.Amount != "" {
		if a, err := parseXAmount(p.Amount); err == nil {
			takeAmt = a
		}
	}

	acc := &proto.AcceptingBody{
		ID:              o.ID,
		From:            fromID,
		FromCurrency:    o.ToCurrency,
		FromAmount:      takeAmt,
		To:              toID,
		ToCurrency:      o.FromCurrency,
		ToAmount:        o.FromAmount,
		FromBlockHeight: 0,
		ToBlockHeight:   0,
	}
	pkt := proto.NewPacket(proto.XbcTransactionAccepting, acc.Marshal())
	if err := n.signer.Sign(pkt, n.cfg.PrivKey); err != nil {
		return nil, makeError(errUnknown, "dxTakeOrder", err.Error())
	}
	if !p.DryRun {
		if err := n.conn.WritePacket(pkt); err != nil {
			return nil, makeError(errUnknown, "dxTakeOrder", err.Error())
		}
	}
	o.Updated = NowMicro()
	o.Status = "accepting"
	// Begin driving the client-side deposit handshake for this taken order.
	n.newTakerSession(o, p)
	return o, nil
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
	body := (&proto.CancelBody{ID: o.ID, Reason: reason}).Marshal()
	pkt := proto.NewPacket(proto.XbcTransactionCancel, body)
	if err := n.signer.Sign(pkt, n.cfg.PrivKey); err != nil {
		return nil, makeError(errUnknown, "dxCancelOrder", err.Error())
	}
	if err := n.conn.WritePacket(pkt); err != nil {
		return nil, makeError(errUnknown, "dxCancelOrder", err.Error())
	}
	o.Status = "canceled"
	o.Updated = NowMicro()
	n.store.RecordCancelled(p.ID, o.Created)
	return o, nil
}
