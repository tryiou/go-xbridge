package api

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"xbridge-go/coins"
	"xbridge-go/config"
	"xbridge-go/crypto"
	xlog "xbridge-go/log"
	"xbridge-go/proto"
	"xbridge-go/swap"
	"xbridge-go/wallet"
)

// Locktime targets mirror xbridgewallet.h: the maker (role A) locks for 2h, the
// taker (role B) for 30m. C++ computes the absolute block height as
// currentBlock + target/blockTime; we do the same via the connector's
// getblockcount. XLOCKTIME_DRIFT (15m) is the tolerance applied to the taker's
// CreateB locktime — checked but not strictly enforced here.
const (
	makerLockTimeSec = 7200
	takerLockTimeSec = 1800

	// C++ xbridgewallet.h constexprs (not conf-driven; faithfully mirrored):
	xMinLockTimeBlocks    = 6    // XMIN_LOCKTIME_BLOCKS
	xSlowTakerLockTimeSec = 3600 // XSLOW_TAKER_LOCKTIME_TARGET_SECONDS
	xSlowBlockTimeSec     = 600  // XSLOW_BLOCKTIME_SECONDS

	// refundCheckInterval is how often the background watcher scans live sessions
	// for refunds whose deposit lockTime has passed. Overridable in tests.
	refundCheckInterval = 60 * time.Second
)

// clientState tracks the local client's progress through the hub-driven swap.
// The hub owns the authoritative Transaction state; this is just our side's view
// of which handshake step we have completed.
type clientState int

const (
	csIdle clientState = iota
	csMaker
	csTaker
	csHoldApplied
	csInitialized
	csCreatedA // maker broadcast its deposit
	csCreatedB // taker broadcast its deposit
	csConfirmedA
	csConfirmedB
	csFinished
)

// String renders the client state for logs (the hub owns the authoritative
// Transaction state; this is only our local view).
func (c clientState) String() string {
	switch c {
	case csIdle:
		return "idle"
	case csMaker:
		return "maker"
	case csTaker:
		return "taker"
	case csHoldApplied:
		return "holdApplied"
	case csInitialized:
		return "initialized"
	case csCreatedA:
		return "createdA"
	case csCreatedB:
		return "createdB"
	case csConfirmedA:
		return "confirmedA"
	case csConfirmedB:
		return "confirmedB"
	case csFinished:
		return "finished"
	default:
		return fmt.Sprintf("clientState(%d)", int(c))
	}
}

// SwapSession is the XBridge CLIENT side of one order's swap. XBridge is a
// three-party protocol: the service-node HUB holds the authoritative Transaction
// state machine and relays packets; the maker (role A) and taker (role B) never
// talk directly. Our thin client responds to hub-originated packets and performs
// the on-chain work the C++ client would: building/broadcasting the HTLC deposit,
// redeeming the counterparty's deposit by revealing (maker) or recovering (taker)
// the secret, and pre-building the CLTV refund for the cancel/expiry path.
type SwapSession struct {
	n       *Node
	isMaker bool
	id      [32]byte

	srcCur, dstCur string // currencies we give / receive
	srcAmt, dstAmt uint64 // amounts we lock / expect

	ourSourceAddr, ourDestAddr string // our addresses for srcCur / dstCur

	theirPub   [33]byte // counterparty's deposit pubkey (from CreateA/B)
	secret     [33]byte // maker: xPubKey preimage; taker: recovered from A's payTx
	secretHash [20]byte // HASH160(secret); both deposits share it

	ourLockTime    uint32
	ourDepositTxID string
	refundHex      string // pre-signed IF-branch refund, for cancel/expiry
	refundDone     bool   // guard so the watcher broadcasts the refund at most once

	theirDepositTxID string // counterparty's deposit txid (from ConfirmA/CreateB)
	theirLockTime    uint32
	theirSecretHash  [20]byte

	hub   [20]byte // service-node address, learned from inbound packets
	state clientState
}

// newMakerSession registers the client-side maker for a freshly created order and
// generates the 33-byte HTLC secret (xPubKey); HASH160(xPubKey) is the secretHash
// carried in the deposit. Both deposits use the same secretHash.
func (n *Node) newMakerSession(o *Order, p MakeOrderParams) {
	if len(n.cfg.PrivKey) != 32 {
		return
	}
	priv, err := crypto.NewPrivateKey()
	if err != nil {
		return
	}
	xPub, err := crypto.CompressedPubKey(priv)
	if err != nil {
		return
	}
	s := &SwapSession{
		n:             n,
		isMaker:       true,
		id:            o.ID,
		srcCur:        o.FromCurrency,
		dstCur:        o.ToCurrency,
		srcAmt:        o.FromAmount,
		dstAmt:        o.ToAmount,
		ourSourceAddr: p.MakerAddress,
		ourDestAddr:   p.TakerAddress,
		secret:        xPub,
		secretHash:    coins.KeyID(xPub[:]),
		state:         csMaker,
	}
	n.sessMu.Lock()
	n.sessions[hexEncode(o.ID[:])] = s
	n.sessMu.Unlock()
	xlog.Info("swap session created", "order", hexEncode(o.ID[:]), "role", "maker",
		"srcCur", o.FromCurrency, "srcAmt", o.FromAmount, "dstCur", o.ToCurrency, "dstAmt", o.ToAmount)
}

// newTakerSession registers the client-side taker for a taken order. The secret
// is unknown until CreateB teaches us the secretHash; the actual preimage is
// recovered from the maker's payTx at ConfirmB.
func (n *Node) newTakerSession(o *Order, p TakeOrderParams) {
	if len(n.cfg.PrivKey) != 32 {
		return
	}
	s := &SwapSession{
		n:             n,
		isMaker:       false,
		id:            o.ID,
		srcCur:        o.ToCurrency,
		dstCur:        o.FromCurrency,
		srcAmt:        o.ToAmount,
		dstAmt:        o.FromAmount,
		ourSourceAddr: p.FromAddress,
		ourDestAddr:   p.ToAddress,
		state:         csTaker,
	}
	n.sessMu.Lock()
	n.sessions[hexEncode(o.ID[:])] = s
	n.sessMu.Unlock()
	xlog.Info("swap session created", "order", hexEncode(o.ID[:]), "role", "taker",
		"srcCur", o.ToCurrency, "srcAmt", o.ToAmount, "dstCur", o.FromCurrency, "dstAmt", o.FromAmount)
}

// ---------------------------------------------------------------------------
// Handshake handlers — each returns the response body to broadcast (or nil) plus
// an error. Side effects (building/broadcasting deposits and their claim/refund
// spends) happen inside; the caller signs + broadcasts the response.
// ---------------------------------------------------------------------------

// OnHold (hub→both) → HoldApply (7): echo our source address as the client's own.
func (s *SwapSession) OnHold(b *proto.HoldBody) (proto.XBridgeCommand, responseBody, error) {
	c, ok := coins.Get(s.srcCur)
	if !ok {
		return 0, nil, fmt.Errorf("api: unknown coin %s", s.srcCur)
	}
	a, err := c.DecodeAddress(s.ourSourceAddr)
	if err != nil {
		return 0, nil, err
	}
	src := [20]byte{}
	copy(src[:], a.Hash)
	s.state = csHoldApplied
	xlog.Info("hold applied", "order", hexEncode(s.id[:]), "state", s.state.String())
	return proto.XbcTransactionHoldApply, &proto.HoldApplyBody{
		HubAddress: s.hub, ClientAddress: src, ID: s.id,
	}, nil
}

// OnInit (hub→each) → Initialized (9): echo back our destination address.
func (s *SwapSession) OnInit(b *proto.InitBody) (proto.XBridgeCommand, responseBody, error) {
	s.state = csInitialized
	xlog.Info("initialized", "order", hexEncode(s.id[:]), "state", s.state.String())
	return proto.XbcTransactionInitialized, &proto.InitializedBody{
		HubAddress: s.hub, ClientAddress: b.ClientAddress, ID: s.id,
	}, nil
}

// OnCreateA (hub→maker) → CreatedA (11): build + broadcast our deposit A.
func (s *SwapSession) OnCreateA(b *proto.CreateABody) (proto.XBridgeCommand, responseBody, error) {
	if !s.isMaker {
		return 0, nil, fmt.Errorf("api: CreateA received by taker session %s", hexEncode(s.id[:]))
	}
	if b.BPubKey == [33]byte{} {
		return 0, nil, fmt.Errorf("api: CreateA missing B pubkey")
	}
	s.theirPub = b.BPubKey
	xlog.Info("CreateA: building deposit A", "order", hexEncode(s.id[:]), "counterparty", hexEncode(b.BPubKey[:]))
	txid, refundHex, err := s.buildDeposit(true)
	if err != nil {
		return 0, nil, err
	}
	s.state = csCreatedA
	xlog.Info("deposit A broadcast", "order", hexEncode(s.id[:]), "txid", txid,
		"lockTime", s.ourLockTime, "secretHash", hexEncode(s.secretHash[:]))
	xlog.Debug("deposit A refund pre-signed", "order", hexEncode(s.id[:]), "refundHex", refundHex)
	return proto.XbcTransactionCreatedA, &proto.CreatedABody{
		HubAddress: s.hub, ID: s.id,
		ADepositTxID: txid, HashedSecret: s.secretHash, ALockTime: s.ourLockTime,
		RefTx: refundHex,
	}, nil
}

// OnCreateB (hub→taker) → CreatedB (13): learn maker's deposit + build ours.
func (s *SwapSession) OnCreateB(b *proto.CreateBBody) (proto.XBridgeCommand, responseBody, error) {
	if s.isMaker {
		return 0, nil, fmt.Errorf("api: CreateB received by maker session %s", hexEncode(s.id[:]))
	}
	if b.APubKey == [33]byte{} {
		return 0, nil, fmt.Errorf("api: CreateB missing A pubkey")
	}
	s.theirPub = b.APubKey
	s.theirDepositTxID = b.ADepositTxID
	// Record the counterparty (maker) deposit txid on the order so
	// dxPartialOrderChainDetails can emit p2sh_deposits_counterparty.
	if o := s.n.store.Get(hexEncode(s.id[:])); o != nil {
		o.OBinTxId = b.ADepositTxID
	}
	s.theirSecretHash = b.HashedSecret
	s.theirLockTime = b.ALockTime
	xlog.Info("CreateB: learned maker deposit", "order", hexEncode(s.id[:]),
		"makerDeposit", b.ADepositTxID, "makerLockTime", b.ALockTime, "secretHash", hexEncode(b.HashedSecret[:]))
	txid, refundHex, err := s.buildDeposit(false)
	if err != nil {
		return 0, nil, err
	}
	s.state = csCreatedB
	xlog.Info("deposit B broadcast", "order", hexEncode(s.id[:]), "txid", txid,
		"lockTime", s.ourLockTime, "makerDeposit", b.ADepositTxID)
	xlog.Debug("deposit B refund pre-signed", "order", hexEncode(s.id[:]), "refundHex", refundHex)
	return proto.XbcTransactionCreatedB, &proto.CreatedBBody{
		HubAddress: s.hub, ID: s.id,
		BDepositTxID: txid, BLockTime: s.ourLockTime,
		RefTx: refundHex,
	}, nil
}

// OnConfirmA (hub→maker's dest) → ConfirmedA (19): redeem taker's deposit B by
// revealing our secret on-chain, then broadcast A's payTx.
func (s *SwapSession) OnConfirmA(b *proto.ConfirmABody) (proto.XBridgeCommand, responseBody, error) {
	if !s.isMaker {
		return 0, nil, fmt.Errorf("api: ConfirmA received by taker session %s", hexEncode(s.id[:]))
	}
	s.theirDepositTxID = b.BDepositTxID
	// Record the counterparty (taker) deposit txid on the order so
	// dxPartialOrderChainDetails can emit p2sh_deposits_counterparty.
	if o := s.n.store.Get(hexEncode(s.id[:])); o != nil {
		o.OBinTxId = b.BDepositTxID
	}
	s.theirLockTime = b.BLockTime

	xlog.Info("ConfirmA: redeeming taker deposit", "order", hexEncode(s.id[:]), "takerDeposit", b.BDepositTxID)
	payHex, cur, err := s.redeemCounterparty(true)
	if err != nil {
		return 0, nil, err
	}
	xlog.Debug("ConfirmA: claim tx built", "order", hexEncode(s.id[:]), "cur", cur, "payHex", payHex)
	conn, e := s.n.connector(cur)
	if e != nil {
		return 0, nil, e
	}
	payTxID, err := conn.SendRawTransaction(payHex)
	if err != nil {
		return 0, nil, fmt.Errorf("api: broadcast payTx: %w", err)
	}
	s.state = csConfirmedA
	xlog.Info("ConfirmA: payTx broadcast", "order", hexEncode(s.id[:]), "payTxID", payTxID)
	return proto.XbcTransactionConfirmedA, &proto.ConfirmedABody{
		HubAddress: s.hub, ID: s.id, APayTxID: payTxID,
	}, nil
}

// OnConfirmB (hub→taker's dest) → ConfirmedB (21): recover the secret from A's
// payTx, then redeem maker's deposit A.
func (s *SwapSession) OnConfirmB(b *proto.ConfirmBBody) (proto.XBridgeCommand, responseBody, error) {
	if s.isMaker {
		return 0, nil, fmt.Errorf("api: ConfirmB received by maker session %s", hexEncode(s.id[:]))
	}
	// Recover the 33-byte secret preimage from the maker's payTx.
	conn := s.n.cfg.Connectors[s.srcCur]
	if conn == nil {
		return 0, nil, fmt.Errorf("api: no connector for %s", s.srcCur)
	}
	payHex, err := conn.GetRawTransaction(b.APayTxID)
	if err != nil {
		return 0, nil, fmt.Errorf("api: getrawtransaction %s: %w", b.APayTxID, err)
	}
	// The maker's payTx was serialized by the maker's XBridge connector; if that
	// coin sets serializeWithTimeField we must parse the nTime field accordingly.
	hasTime := false
	if cc := s.conf(s.srcCur); cc != nil {
		hasTime = cc.TxWithTimeField
	}
	secret, ok := secretFromPayTx(payHex, s.theirSecretHash, hasTime)
	if !ok {
		return 0, nil, fmt.Errorf("api: could not recover secret from payTx %s", b.APayTxID)
	}
	s.secret = secret
	xlog.Info("ConfirmB: secret recovered from maker payTx", "order", hexEncode(s.id[:]),
		"makerPayTx", b.APayTxID, "secretHash", hexEncode(s.theirSecretHash[:]))

	payHex2, cur, err := s.redeemCounterparty(false)
	if err != nil {
		return 0, nil, err
	}
	xlog.Debug("ConfirmB: claim tx built", "order", hexEncode(s.id[:]), "cur", cur, "payHex", payHex2)
	conn, e := s.n.connector(cur)
	if e != nil {
		return 0, nil, e
	}
	payTxID, err := conn.SendRawTransaction(payHex2)
	if err != nil {
		return 0, nil, fmt.Errorf("api: broadcast payTx: %w", err)
	}
	s.state = csConfirmedB
	xlog.Info("ConfirmB: payTx broadcast", "order", hexEncode(s.id[:]), "payTxID", payTxID)
	return proto.XbcTransactionConfirmedB, &proto.ConfirmedBBody{
		HubAddress: s.hub, ID: s.id, BPayTxID: payTxID,
	}, nil
}

// OnFinished (hub→both): the swap is complete on the hub; record the fill.
func (s *SwapSession) OnFinished(b *proto.FinishedBody) (proto.XBridgeCommand, responseBody, error) {
	if o := s.n.store.Get(hexEncode(s.id[:])); o != nil {
		o.Status = "completed"
		o.Updated = NowMicro()
	}
	s.state = csFinished
	xlog.Info("swap finished", "order", hexEncode(s.id[:]), "state", s.state.String())
	return 0, nil, nil
}

// broadcastRefund sends this session's pre-signed CLTV refund to its source
// chain, returning the refund txid. The refund spends the deposit back to our
// source address and is only valid after the deposit's lockTime, so calling it
// is always fund-safe: it can never claim the counterparty's funds or double-
// spend a legitimately claimed deposit. Caller must not hold n.sessMu (this does
// wallet I/O, not session-state access).
func (n *Node) broadcastRefund(s *SwapSession) (string, error) {
	cur := s.srcCur
	conn := n.cfg.Connectors[cur]
	if conn == nil {
		return "", fmt.Errorf("api: no connector for %s", cur)
	}
	if s.refundHex == "" {
		return "", fmt.Errorf("api: no refund available for order %s", hexEncode(s.id[:]))
	}
	txid, err := conn.SendRawTransaction(s.refundHex)
	if err != nil {
		xlog.Error("refund broadcast failed", "order", hexEncode(s.id[:]), "err", err)
		return "", err
	}
	xlog.Info("refund broadcast", "order", hexEncode(s.id[:]), "txid", txid)
	return txid, nil
}

// BroadcastRefund is the manual escape hatch: it force-broadcasts the pre-signed
// CLTV refund for an order (e.g. a swap has stalled and the deposit's lockTime
// has passed), returning the deposit to the source address without waiting for
// the background watcher. If a live session exists it uses that; otherwise it
// falls back to the order's stored refund hex (trying both the maker and taker
// deposit chains).
func (n *Node) BroadcastRefund(orderID string) (string, error) {
	n.sessMu.Lock()
	s := n.sessions[orderID]
	n.sessMu.Unlock()
	if s != nil && s.refundHex != "" {
		txid, err := n.broadcastRefund(s)
		if err == nil {
			s.refundDone = true
		}
		return txid, err
	}
	if o := n.store.Get(orderID); o != nil && o.RefundTx != "" {
		for _, cur := range []string{o.FromCurrency, o.ToCurrency} {
			conn := n.cfg.Connectors[cur]
			if conn == nil {
				continue
			}
			txid, err := conn.SendRawTransaction(o.RefundTx)
			if err != nil {
				xlog.Warn("escape-hatch refund broadcast failed", "order", orderID, "cur", cur, "err", err)
				continue
			}
			xlog.Info("escape-hatch refund broadcast", "order", orderID, "txid", txid)
			return txid, nil
		}
		return "", fmt.Errorf("api: could not broadcast stored refund for %s", orderID)
	}
	return "", fmt.Errorf("api: no refund available for order %s", orderID)
}

// checkRefunds scans all live sessions and auto-broadcasts any pre-signed
// refund whose deposit lockTime has passed (the fund-safety safety net). It is
// invoked by the background refundWatcher and can be called directly (e.g. in
// tests) to drive the check on demand. Caller must not hold n.sessMu.
func (n *Node) checkRefunds() {
	n.sessMu.Lock()
	defer n.sessMu.Unlock()
	for id, s := range n.sessions {
		if s.refundDone || s.refundHex == "" || s.state == csFinished || s.state < csCreatedA {
			continue
		}
		conn := n.cfg.Connectors[s.srcCur]
		if conn == nil {
			continue
		}
		h, err := conn.GetBlockCount()
		if err != nil || h < 1 {
			continue
		}
		if uint32(h) >= s.ourLockTime {
			txid, berr := n.broadcastRefund(s)
			if berr != nil {
				xlog.Error("refund auto-broadcast failed", "order", id, "err", berr)
				continue
			}
			s.refundDone = true
			xlog.Info("refund auto-broadcast on lockTime expiry", "order", id, "txid", txid)
		}
	}
}

// ---------------------------------------------------------------------------
// Deposit / claim / refund construction
// ---------------------------------------------------------------------------

// computeLockTime returns the absolute block height to embed in the deposit's
// HTLC (mirrors C++: currentBlock + target/blockTime).
func (s *SwapSession) computeLockTime(isMaker bool) uint32 {
	cur := s.srcCur
	conn := s.n.cfg.Connectors[cur]
	cc := s.conf(cur)
	if conn == nil {
		return 0
	}
	n, err := conn.GetBlockCount()
	if err != nil || n < 1 {
		return 0
	}
	bt := 60
	if cc != nil && cc.BlockTime > 0 {
		bt = cc.BlockTime
	}
	target := makerLockTimeSec
	if !isMaker {
		target = takerLockTimeSec
		// C++ xbridgewalletconnectorbtc.cpp lockTime(role 'B'): on slow chains
		// (blockTime >= XSLOW_BLOCKTIME_SECONDS) use the longer taker target.
		if bt >= xSlowBlockTimeSec {
			target = xSlowTakerLockTimeSec
		}
	}
	blocks := target / bt
	if blocks < xMinLockTimeBlocks { // XMIN_LOCKTIME_BLOCKS clamp (C++)
		blocks = xMinLockTimeBlocks
	}
	return uint32(n) + uint32(blocks)
}

// buildDeposit builds the local participant's HTLC deposit, funds it from the
// wallet connector, signs the funding inputs via the wallet, broadcasts it, and
// pre-builds the CLTV refund. It returns the broadcast deposit txid and the
// pre-signed refund hex.
func (s *SwapSession) buildDeposit(isMaker bool) (txid, refundHex string, err error) {
	cur := s.srcCur
	amt := s.srcAmt
	conn := s.n.cfg.Connectors[cur]
	cc := s.conf(cur)
	if conn == nil {
		return "", "", fmt.Errorf("api: no connector for %s", cur)
	}
	c, ok := coins.Get(cur)
	if !ok {
		return "", "", fmt.Errorf("api: unknown coin %s", cur)
	}
	funding, err := conn.ListUnspent(s.minConf(cc))
	if err != nil {
		return "", "", err
	}
	if len(funding) == 0 {
		return "", "", fmt.Errorf("api: no funding UTXOs for %s", cur)
	}
	changeStr, err := conn.GetNewAddress()
	if err != nil {
		return "", "", err
	}
	var change [20]byte
	if a, derr := c.DecodeAddress(changeStr); derr != nil {
		return "", "", derr
	} else {
		copy(change[:], a.Hash)
	}
	fee := estimateFee(cc, len(funding), 2)
	lockTime := s.computeLockTime(isMaker)
	xlog.Debug("buildDeposit: plan", "order", hexEncode(s.id[:]), "isMaker", isMaker,
		"cur", cur, "amount", amt, "lockTime", lockTime, "txVersion", s.txVersion(cur),
		"utxos", len(funding), "fee", fee)

	hash := s.secretHash
	if !isMaker {
		hash = s.theirSecretHash
	}
	spec := &swap.DepositSpec{
		Currency:        cur,
		Amount:          amt,
		DepositorPub:    s.n.pubkey,
		CounterpartyPub: s.theirPub,
		Hash:            hash,
		LockTime:        lockTime,
		TxVersion:       s.txVersion(cur),
	}
	tx, err := spec.BuildDepositTx(c, funding, change, fee)
	if err != nil {
		return "", "", err
	}
	xlog.Debug("buildDeposit: unsigned tx built", "order", hexEncode(s.id[:]), "txVersion", tx.Version, "outputs", len(tx.Outputs))
	prevTxs := make([]wallet.PrevTx, 0, len(funding))
	for _, u := range funding {
		prevTxs = append(prevTxs, wallet.PrevTx{TxID: u.TxID, Vout: u.Vout, ScriptPubKey: u.ScriptPubKey, Amount: u.Amount})
	}
	unsigned := hex.EncodeToString(tx.Serialize())
	signed, complete, serr := conn.SignRawTransaction(unsigned, prevTxs)
	if serr != nil {
		return "", "", serr
	}
	if !complete {
		return "", "", fmt.Errorf("api: deposit signing incomplete for %s", cur)
	}
	txid, err = conn.SendRawTransaction(signed)
	if err != nil {
		return "", "", err
	}
	s.ourDepositTxID = txid
	s.ourLockTime = lockTime
	// Record our own deposit txid on the order so dxPartialOrderChainDetails can
	// emit p2sh_deposits.
	if o := s.n.store.Get(hexEncode(s.id[:])); o != nil {
		o.BinTxId = txid
	}

	refundHex, err = s.buildRefundTx(spec, cur)
	if err != nil {
		return "", "", err
	}
	s.refundHex = refundHex
	// Tie the pre-signed refund to the order so dxCancelOrder can surface
	// `refund_tx` for an order whose deposit has been broadcast (C++ returns the
	// empty string for orders cancelled before any deposit — which is still the
	// case here, since buildDeposit only runs once a swap reaches the deposit step).
	if o := s.n.store.Get(hexEncode(s.id[:])); o != nil {
		o.RefundTx = refundHex
	}
	return txid, refundHex, nil
}

// buildRefundTx pre-signs the IF-branch (CLTV) refund that returns the deposit to
// our source address, spendable only after the deposit's lockTime.
func (s *SwapSession) buildRefundTx(spec *swap.DepositSpec, cur string) (string, error) {
	dest, err := s.destScript(cur, s.ourSourceAddr)
	if err != nil {
		return "", err
	}
	fee := estimateFee(s.conf(cur), 1, 1)
	if fee >= spec.Amount {
		return "", fmt.Errorf("api: deposit amount too small for refund fee")
	}
	h, err := reverseTxidHex(s.ourDepositTxID)
	if err != nil {
		return "", err
	}
	tx := &coins.Tx{Version: int32(s.txVersion(cur)), LockTime: spec.LockTime}
	// Per-coin serializeWithTimeField: stamp nTime after nVersion so the refund
	// matches the counterparty's XBridge connector wire layout.
	if cc := s.conf(cur); cc != nil && cc.TxWithTimeField {
		tx.WithTime = true
		tx.TxTime = uint32(time.Now().Unix())
	}
	xlog.Debug("buildRefundTx: plan", "order", hexEncode(s.id[:]), "cur", cur,
		"deposit", s.ourDepositTxID, "lockTime", spec.LockTime, "amount", spec.Amount, "fee", fee, "txVersion", s.txVersion(cur))
	tx.Inputs = append(tx.Inputs, coins.TxIn{
		PrevOut:  coins.OutPoint{Hash: h, Index: 0},
		Sequence: 0xfffffffe, // enable CLTV
	})
	tx.Outputs = append(tx.Outputs, coins.TxOut{Value: spec.Amount - fee, ScriptPubKey: dest})

	inner := spec.RedeemScript()
	sig, err := coins.SignTxInput(tx, 0, inner, s.n.cfg.PrivKey)
	if err != nil {
		return "", err
	}
	tx.Inputs[0].ScriptSig = coins.BuildRefundScriptSig(sig, spec.DepositorPub[:], inner)
	return hex.EncodeToString(tx.Serialize()), nil
}

// redeemCounterparty builds and returns the ELSE-branch payment that claims the
// counterparty's deposit, revealing (maker) or using (taker) the secret. We always
// redeem the counterparty's deposit, which is the currency we receive (dstCur)
// and the amount we receive (dstAmt): the maker redeems the taker's LTC deposit,
// the taker redeems the maker's BTC deposit.
func (s *SwapSession) redeemCounterparty(isMaker bool) (payHex, depositCur string, err error) {
	theirSpec := swap.DepositSpec{
		Currency:        s.dstCur,
		Amount:          s.dstAmt,
		DepositorPub:    s.theirPub,
		CounterpartyPub: s.n.pubkey,
		LockTime:        s.theirLockTime,
	}
	if s.isMaker {
		theirSpec.Hash = s.secretHash
	} else {
		theirSpec.Hash = s.theirSecretHash
	}
	depositCur = s.dstCur

	dest, err := s.destScript(depositCur, s.ourDestAddr)
	if err != nil {
		return "", "", err
	}
	fee := estimateFee(s.conf(depositCur), 1, 1)
	if fee >= theirSpec.Amount {
		return "", "", fmt.Errorf("api: counterparty deposit amount too small for claim fee")
	}
	h, err := reverseTxidHex(s.theirDepositTxID)
	if err != nil {
		return "", "", err
	}
	tx := &coins.Tx{Version: int32(s.txVersion(depositCur)), LockTime: 0} // ELSE branch, no CLTV
	// Per-coin serializeWithTimeField: the claim (ELSE-branch) spend must carry
	// the nTime field for coins whose connector sets it, matching the
	// counterparty's XBridge serialization.
	if cc := s.conf(depositCur); cc != nil && cc.TxWithTimeField {
		tx.WithTime = true
		tx.TxTime = uint32(time.Now().Unix())
	}
	xlog.Debug("redeemCounterparty: plan", "order", hexEncode(s.id[:]), "isMaker", isMaker,
		"depositCur", depositCur, "deposit", s.theirDepositTxID, "amount", theirSpec.Amount, "fee", fee, "txVersion", s.txVersion(depositCur))
	tx.Inputs = append(tx.Inputs, coins.TxIn{
		PrevOut:  coins.OutPoint{Hash: h, Index: 0},
		Sequence: 0xffffffff,
	})
	tx.Outputs = append(tx.Outputs, coins.TxOut{Value: theirSpec.Amount - fee, ScriptPubKey: dest})

	inner := theirSpec.RedeemScript()
	sig, err := coins.SignTxInput(tx, 0, inner, s.n.cfg.PrivKey)
	if err != nil {
		return "", "", err
	}
	// The ELSE branch requires <secret> <sig> <myPubKey> OP_0 <inner>, where
	// myPubKey is the deposit's CounterpartyPub (== our trader key).
	tx.Inputs[0].ScriptSig = coins.BuildPaymentScriptSig(s.secret[:], sig, s.n.pubkey[:], inner)
	return hex.EncodeToString(tx.Serialize()), depositCur, nil
}

// destScript returns the P2PKH output script for addr on cur.
func (s *SwapSession) destScript(cur, addrStr string) ([]byte, error) {
	c, ok := coins.Get(cur)
	if !ok {
		return nil, fmt.Errorf("api: unknown coin %s", cur)
	}
	a, err := c.DecodeAddress(addrStr)
	if err != nil {
		return nil, err
	}
	var h [20]byte
	copy(h[:], a.Hash)
	return coins.BuildP2PKHScript(h), nil
}

func (s *SwapSession) conf(cur string) *config.CoinConf {
	if s.n.cfg.Confs != nil {
		return s.n.cfg.Confs[cur]
	}
	return nil
}

// txVersion returns the per-coin transaction version to stamp on the deposit,
// refund, and claim txs. C++ reads <COIN>.TxVersion from xbridge.conf (default
// 1); we mirror that, falling back to 1 when unset. Never hardcode this.
func (s *SwapSession) txVersion(cur string) int {
	cc := s.conf(cur)
	if cc == nil || cc.TxVersion <= 0 {
		return 1
	}
	return cc.TxVersion
}

// minConf returns the connector's minimum confirmations for spendable UTXOs,
// defaulting to 0 when no conf is configured.
func (s *SwapSession) minConf(cc *config.CoinConf) int {
	if cc == nil {
		return 0
	}
	return cc.Confirmations
}

// secretFromPayTx extracts the 33-byte HTLC secret preimage from a serialized
// payTx, verifying it against the expected secretHash hx (the deposit's
// HashedSecret). C++ does the same in getSecretFromPaymentTransaction, which only
// adopts a push whose getKeyId(push) equals hx.
func secretFromPayTx(payHex string, hx [20]byte, hasTime bool) ([33]byte, bool) {
	raw, err := hex.DecodeString(payHex)
	if err != nil {
		xlog.Debug("secretFromPayTx: bad hex", "err", err)
		return [33]byte{}, false
	}
	tx, err := coins.DeserializeWithTime(raw, hasTime)
	if err != nil || len(tx.Inputs) == 0 {
		xlog.Debug("secretFromPayTx: cannot deserialize", "err", err, "inputs", len(tx.Inputs))
		return [33]byte{}, false
	}
	secret, ok := secretFromScriptSig(tx.Inputs[0].ScriptSig, hx)
	xlog.Debug("secretFromPayTx", "ok", ok)
	return secret, ok
}

// secretFromScriptSig parses a payment scriptSig (<secret 33> <sig> <myPubKey>
// OP_0 <inner>) and returns the 33-byte push whose HASH160 (coins.KeyID) equals
// the expected secretHash hx. This mirrors C++ getSecretFromPaymentTransaction,
// which only adopts a push when getKeyId(chk) == hx — verifying the preimage
// actually unlocks the deposit rather than blindly taking the first 33-byte
// element. A malleated/non-conforming scriptSig (e.g. myPubKey pushed ahead of
// the real secret) yields ok == false instead of the wrong element.
func secretFromScriptSig(script []byte, hx [20]byte) ([33]byte, bool) {
	i := 0
	var secret [33]byte
	for i < len(script) {
		op := script[i]
		i++
		var n int
		switch {
		case op <= 0x4b: // direct push OP_1..OP_75
			n = int(op)
		case op == 0x4c: // OP_PUSHDATA1
			if i >= len(script) {
				return secret, false
			}
			n = int(script[i])
			i++
		case op == 0x4d: // OP_PUSHDATA2
			if i+2 > len(script) {
				return secret, false
			}
			n = int(script[i]) | int(script[i+1])<<8
			i += 2
		default:
			continue // not a data push; skip
		}
		if i+n > len(script) {
			return secret, false
		}
		push := script[i : i+n]
		if n == 33 && coins.KeyID(push) == hx {
			copy(secret[:], push)
			return secret, true
		}
		i += n
	}
	return secret, false
}

// txIDFromHex returns the display-order txid of a serialized tx (used to label
// the locally-built refund/claim txs).
func txIDFromHex(rawHex string) (string, error) {
	raw, err := hex.DecodeString(rawHex)
	if err != nil {
		return "", err
	}
	h1 := sha256.Sum256(raw)
	h2 := sha256.Sum256(h1[:])
	out := make([]byte, 32)
	for i := 0; i < 32; i++ {
		out[i] = h2[31-i]
	}
	return hex.EncodeToString(out), nil
}
