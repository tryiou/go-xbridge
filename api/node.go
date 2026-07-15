package api

import (
	"crypto/rand"
	"fmt"
	"time"

	"xbridge-go/coins"
	"xbridge-go/config"
	"xbridge-go/crypto"
	"xbridge-go/p2p"
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
}

// Node is the live XBridge client: it maintains a P2P connection to a service
// node, ingests order broadcasts into the Store, and builds/signs/broadcasts
// the packets for dxMakeOrder / dxTakeOrder / dxCancelOrder.
type Node struct {
	cfg    *Config
	conn   *p2p.Conn
	store  *Store
	signer crypto.Signer
	pubkey [33]byte
	stop   chan struct{}
}

// NewNode dials the configured peer (if any) and starts ingesting broadcasts.
func NewNode(cfg *Config, store *Store) (*Node, error) {
	n := &Node{
		cfg:    cfg,
		store:  store,
		signer: crypto.NewBtcSigner(),
		stop:   make(chan struct{}),
	}
	if len(cfg.PrivKey) == 32 {
		pk, err := crypto.CompressedPubKey(cfg.PrivKey)
		if err != nil {
			return nil, err
		}
		n.pubkey = pk
	}
	if cfg.NodeAddr != "" {
		conn, err := p2p.Dial(cfg.NodeAddr, cfg.Magic, 30*time.Second)
		if err != nil {
			return nil, fmt.Errorf("api: dial service node: %w", err)
		}
		n.conn = conn
		go n.feed()
	}
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
		}
	}
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
		BlockHash:      [32]byte{},
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
