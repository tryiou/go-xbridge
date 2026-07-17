package api

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log"
	"sort"
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
	// WalletVersion / WalletVersionStr are advertised in the getnetworkinfo
	// response. BLOCK-DX pings getnetworkinfo for its wallet-version gate; we
	// advertise Blocknet 4.4.1 (CLIENT_VERSION 4040100) by default.
	WalletVersion    int
	WalletVersionStr string
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

	// svcMu guards svcByPeer, the per-servicenode set of advertised token
	// services learned from XbcServicesPing packets. dxGetNetworkTokens unions
	// these to report the live network token set (C++ walletServices()).
	svcMu     sync.RWMutex
	svcByPeer map[string][]string
}

// NewNode dials the configured peer (if any) and starts ingesting broadcasts.
func NewNode(cfg *Config, store *Store) (*Node, error) {
	n := &Node{
		cfg:       cfg,
		store:     store,
		signer:    crypto.NewBtcSigner(),
		stop:      make(chan struct{}),
		sessions:  map[string]*SwapSession{},
		svcByPeer: map[string][]string{},
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

// connector returns the wallet connector for ticker, or an *rpcError when none
// is configured. This centralizes the nil/lookup check so swap handlers never
// index n.cfg.Connectors[t] unguarded — an absent connector must surface as an
// error, not as a nil-interface panic on the feed goroutine.
func (n *Node) connector(t string) (wallet.Connector, error) {
	if n.cfg == nil || n.cfg.Connectors == nil {
		return nil, fmt.Errorf("dx: no wallet configured")
	}
	conn, ok := n.cfg.Connectors[t]
	if !ok || conn == nil {
		return nil, fmt.Errorf("dx: no wallet configured for %s", t)
	}
	return conn, nil
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

// utxoChallenge builds the BIP137 message an order UTXO is signed over,
// matching C++ CXBridgeWalletConnector::signMessage: "<display-txid>:<vout>".
// VERIFY: confirm the exact challenge string against a live C++ hub before
// relying on cross-implementation acceptance of the proof.
func utxoChallenge(txid string, vout uint32) string {
	return fmt.Sprintf("%s:%d", txid, vout)
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
		sig, err := conn.SignMessage(u.Address, utxoChallenge(u.TxID, u.Vout))
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

		// --- servicenode service advertisement (network token discovery) ---
		case *proto.ServicesPingBody:
			n.recordServices(maker, b.Services)
		}
	}
}

// recordServices stores the token-service list a servicenode advertised via an
// XbcServicesPing packet. Thread-safe; called from the feed goroutine.
func (n *Node) recordServices(pubkey string, svcs []string) {
	n.svcMu.Lock()
	defer n.svcMu.Unlock()
	if n.svcByPeer == nil {
		n.svcByPeer = map[string][]string{}
	}
	n.svcByPeer[pubkey] = svcs
}

// NetworkTokens returns the union of tokens advertised by connected
// servicenodes (C++ walletServices()), always including the locally-known
// tokens from config. When no servicenodes are connected it reduces to the
// config list (static fallback).
func (n *Node) NetworkTokens() []string {
	n.svcMu.RLock()
	set := map[string]bool{}
	for _, svcs := range n.svcByPeer {
		for _, s := range svcs {
			if s != "" {
				set[s] = true
			}
		}
	}
	n.svcMu.RUnlock()
	for _, t := range n.cfg.NetworkTokens {
		set[t] = true
	}
	for _, t := range n.cfg.ExchangeWallets {
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
	// Defense in depth: a malformed/inbound packet must never crash the feed
	// goroutine (which would terminate the whole process). Recover from any
	// panic in the handler and log it; the swap is simply not progressed.
	defer func() {
		if r := recover(); r != nil {
			log.Printf("xbridge-go: recovered panic dispatching swap %s: %v", hexEncode(id[:]), r)
		}
	}()
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
	if len(n.cfg.PrivKey) != 32 {
		return makeError(errBadRequest, "dx", "no private key configured")
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
		if cc := n.cfg.Confs[p.Maker]; cc != nil && cc.DustAmount > 0 && minFrom < cc.DustAmount {
			return nil, makeError(errInvalidParameters, "dxMakePartialOrder", "The partial minimum_size is dust, i.e. it's too small.")
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

	// Attach BIP137 ownership proofs for the maker's spendable UTXOs so a C++
	// counterparty can verify we own the coins (best-effort: a wallet that
	// cannot sign leaves Utxos empty rather than failing the order broadcast).
	if c, e := n.connector(p.Maker); e == nil {
		if cc := n.cfg.Confs[p.Maker]; cc != nil {
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
	o.PartialRepost = p.Repost
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
		if a == 0 {
			return orderListResult{}, makeError(errInvalidParameters, "dxTakeOrder", "The amount cannot be less than or equal to 0: "+p.Amount)
		}
		if o.PartialAllowed {
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
		} else if a > 0 {
			return orderListResult{}, makeError(errInvalidPartialOrder, "dxTakeOrder", "")
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
	pkt := proto.NewPacket(proto.XbcTransactionAccepting, acc.Marshal())
	if err := n.signer.Sign(pkt, n.cfg.PrivKey); err != nil {
		return orderListResult{}, makeError(errUnknown, "dxTakeOrder", err.Error())
	}
	if p.DryRun {
		// C++ renders the dryrun result BEFORE the swap and does not broadcast.
		return o.toTakeDryrunResult(fromSize, toSize), nil
	}
	if err := n.conn.WritePacket(pkt); err != nil {
		return orderListResult{}, makeError(errUnknown, "dxTakeOrder", err.Error())
	}
	o.Updated = NowMicro()
	o.Status = "accepting"
	// Begin driving the client-side deposit handshake for this taken order.
	n.newTakerSession(o, p)
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
