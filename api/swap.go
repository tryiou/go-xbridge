package api

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"go-xbridge/coins"
	"go-xbridge/config"
	"go-xbridge/crypto"
	xlog "go-xbridge/log"
	"go-xbridge/proto"
	"go-xbridge/swap"
	"go-xbridge/wallet"
)

const (
	makerLockTimeSec      = config.XMakerLocktimeTargetSeconds
	takerLockTimeSec      = config.XTakerLocktimeTargetSeconds
	xMinLockTimeBlocks    = config.XMinLockTimeBlocks
	xSlowTakerLockTimeSec = config.XSlowTakerLocktimeTargetSeconds
	xSlowBlockTimeSec     = config.XSlowBlockTimeSeconds

	refundCheckInterval = 60 * time.Second

	expirySweepInterval = 15 * time.Second
)

type clientState int

const (
	csIdle clientState = iota
	csMaker
	csTaker
	csHoldApplied
	csInitialized
	csCreatedA
	csCreatedB
	csConfirmedA
	csConfirmedB
	csFinished
)

// String renders the client state for logs — a local view only; the hub's Transaction state is authoritative.
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

// SwapSession is one order's client-side swap: the hub relays packets and owns the protocol state, while this type answers them and performs the on-chain deposit, claim, and refund work.
type SwapSession struct {
	n       *Node
	isMaker bool
	id      [32]byte

	srcCur, dstCur string
	srcAmt, dstAmt uint64

	ourSourceAddr, ourDestAddr string

	theirPub   [33]byte
	privKey    [32]byte
	pubKey     [33]byte
	secret     [33]byte
	secretHash [20]byte

	ourLockTime    uint32
	ourDepositTxID string
	refundHex      string
	refundDone     bool
	// secretHunt arms after our own deposit is proven spent (the counterparty
	// claimed): the pre-signed refund can never confirm, so every refund path stands down.
	secretHunt bool
	huntSince  uint64
	// scanCursor is the next chain height the rescan reads (0 = unseeded); it
	// persists with the session so each block is read at most once per hunt.
	scanCursor uint32
	// depositHex is the signed deposit raw hex: a failed broadcast reposts these
	// identical bytes, never a rebuild, so a repost cannot double-deposit.
	depositHex string

	claimHex  string
	claimTxID string
	claimCur  string

	theirDepositTxID string
	theirLockTime    uint32
	theirSecretHash  [20]byte

	// theirPayTxID is the counterparty claim payTx that revealed the secret,
	// stored on the hub packet so the tick retry can rebuild without a retransmit.
	theirPayTxID string

	claimRetryAt   uint64
	claimRetries   uint32
	depositRetryAt uint64
	depositRetries uint32
	notReadySince  uint64
	notReadySeen   bool

	// Validated counterparty deposit out-params: P2SHNative is the exact
	// native-base output value the claim spends; Overpayment is XBridge 1e6 base.
	theirDepositVout uint32
	theirP2SHNative  uint64
	theirOverpayment uint64

	hub    [20]byte
	hubKey [33]byte
	state  clientState

	await        bool
	awaitSince   uint64
	vanishMisses int

	// lastProgress stamps the last observable forward motion (creation, an
	// accepted hub packet, a broadcast); the silence watchdog cancels on it.
	lastProgress uint64

	// holdApplySentAt stamps the last HoldApply send; resends never touch
	// lastProgress, so a mute hub still trips the silence watchdog.
	holdApplySentAt uint64
}

type depositOutcome struct {
	txid       string
	lockTime   uint32
	refundHex  string
	depositHex string
	// conn is the connector the deposit was built against; the broadcast task
	// must use this snapshot, never a post-reload one.
	conn wallet.Connector
}

type confirmOutcome struct {
	secret  [33]byte
	payTxID string
	payHex  string
	cur     string
	check   wallet.DepositCheck
	conn    wallet.Connector
}

// selfCancelErr marks a worker failure the resume must turn into a self-cancel:
// the engine catches it via errors.As and calls sendSelfCancel.
type selfCancelErr struct {
	reason TxCancelReason
}

func (e *selfCancelErr) Error() string {
	return fmt.Sprintf("api: self-cancel (TxCancelReason %d: %s)", e.reason, TxCancelReasonText(uint32(e.reason)))
}

// sendSelfCancel rejects the counterparty's deposit or locktime: local rollback
// first, persist, then broadcast the signed Cancel. Engine goroutine only.
func (s *SwapSession) sendSelfCancel(reason TxCancelReason) {
	orderID := hexEncode(s.id[:])
	if s.n == nil || s.n.conn == nil {
		xlog.Error("selfCancel: no network connector", "order", orderID, "reason", reason, "reasonText", TxCancelReasonText(uint32(reason)))
		return
	}
	body := &proto.CancelBody{ID: s.id, Reason: uint32(reason)}
	pkt := proto.NewPacket(proto.XbcTransactionCancel, body.Marshal())
	if err := s.n.signer.Sign(pkt, s.privKey[:]); err != nil {
		xlog.Error("selfCancel: sign failed", "order", orderID, "err", err)
		return
	}
	xlog.Warn("selfCancel: counterparty deposit rejected", "order", orderID, "reason", reason, "reasonText", TxCancelReasonText(uint32(reason)))
	// Round-trip: this own packet verifies against o.MakerKey (= our M key), so
	// iCanceled accepts it — no bad-sig rejection, no rebroadcast of our own Cancel.
	s.n.handleRemoteCancel(pkt, body)
	// Persist the rollback BEFORE broadcasting: a crash after the send must not
	// leave a locally-cancelled swap on disk as still-live (a counterparty could act on it).
	if err := s.n.persistNow(); err != nil {
		xlog.Error("selfCancel: persist failed", "order", orderID, "err", err)
	}
	if err := s.n.conn.WritePacket(pkt, [20]byte{}); err != nil {
		xlog.Error("selfCancel: broadcast failed", "order", orderID, "err", err)
	}
}

func (s *SwapSession) failSelfCancel(terr error) bool {
	var sce *selfCancelErr
	if !errors.As(terr, &sce) {
		return false
	}
	s.sendSelfCancel(sce.reason)
	return true
}

type createdBOutcome struct {
	out   depositOutcome
	check wallet.DepositCheck
}

func (c *swapCtx) counterpartyDepositScriptHex(hash [20]byte) string {
	spec := swap.DepositSpec{
		Currency:        c.dstCur,
		DepositorPub:    c.theirPub,
		CounterpartyPub: c.pubKey,
		Hash:            hash,
		LockTime:        c.theirLockTime,
	}
	return hex.EncodeToString(spec.P2SHScript())
}

// checkCounterpartyDeposit returns the wallet's tri-state verdict on the counterparty deposit: an error means no reply is sent (the hub retransmits); IsGood=false is a definitively bad deposit that must wire-Cancel.
func (c *swapCtx) checkCounterpartyDeposit(hash [20]byte, expectedAmount uint64) (wallet.DepositCheck, error) {
	conn := c.connectors[c.dstCur]
	if conn == nil {
		return wallet.DepositCheck{}, fmt.Errorf("api: no connector for %s", c.dstCur)
	}
	return conn.CheckDepositTransaction(c.theirDepositTxID, c.counterpartyDepositScriptHex(hash), expectedAmount, c.minConf(c.conf(c.dstCur)))
}

func counterpartyCoin(s *SwapSession) coins.Coin {
	c, _ := coins.Get(s.dstCur)
	return c
}

// swapCtx is an immutable session snapshot taken at stage-1 enqueue time;
// workers read only it, never the engine-owned live session (a worker read would race the engine's resume writes).
type swapCtx struct {
	n             *Node
	id            [32]byte
	orderID       string
	isMaker       bool
	srcCur        string
	dstCur        string
	srcAmt        uint64
	dstAmt        uint64
	ourSourceAddr string
	ourDestAddr   string

	theirPub         [33]byte
	privKey          [32]byte
	pubKey           [33]byte
	secret           [33]byte
	secretHash       [20]byte
	theirDepositTxID string
	theirLockTime    uint32
	theirSecretHash  [20]byte
	theirDepositVout uint32
	theirP2SHNative  uint64
	theirOverpayment uint64
	ourDepositTxID   string
	ourLockTime      uint32
	refundHex        string

	// connectors/confs/coinsMap snapshot the live config at enqueue time: a
	// mid-task reload must never change the wallet or coin params a build uses.
	connectors map[string]wallet.Connector
	confs      map[string]*config.CoinConf
	coinsMap   map[string]coins.Coin

	// ourDepositP2SH is our deposit's exact built P2SH output value;
	// buildRefundTx commits it to the forkid digest instead of recomputing fee2 after a conf reload.
	ourDepositP2SH uint64

	funding []wallet.Utxo
}

func (s *SwapSession) snapshot() swapCtx {
	var connectors map[string]wallet.Connector
	var confs map[string]*config.CoinConf
	if cfg := s.n.cfg(); cfg != nil {
		connectors = make(map[string]wallet.Connector, len(cfg.Connectors))
		for t, c := range cfg.Connectors {
			connectors[t] = c
		}
		confs = make(map[string]*config.CoinConf, len(cfg.Confs))
		for t, c := range cfg.Confs {
			confs[t] = c
		}
	}
	coinsMap := make(map[string]coins.Coin, 2)
	snap := coins.Snapshot()
	if src, ok := snap[s.srcCur]; ok {
		coinsMap[s.srcCur] = src
	}
	if dst, ok := snap[s.dstCur]; ok {
		coinsMap[s.dstCur] = dst
	}
	return swapCtx{
		n:                s.n,
		id:               s.id,
		orderID:          hexEncode(s.id[:]),
		isMaker:          s.isMaker,
		srcCur:           s.srcCur,
		dstCur:           s.dstCur,
		srcAmt:           s.srcAmt,
		dstAmt:           s.dstAmt,
		ourSourceAddr:    s.ourSourceAddr,
		ourDestAddr:      s.ourDestAddr,
		theirPub:         s.theirPub,
		privKey:          s.privKey,
		pubKey:           s.pubKey,
		secret:           s.secret,
		secretHash:       s.secretHash,
		theirDepositTxID: s.theirDepositTxID,
		theirLockTime:    s.theirLockTime,
		theirSecretHash:  s.theirSecretHash,
		theirDepositVout: s.theirDepositVout,
		theirP2SHNative:  s.theirP2SHNative,
		theirOverpayment: s.theirOverpayment,
		ourDepositTxID:   s.ourDepositTxID,
		ourLockTime:      s.ourLockTime,
		refundHex:        s.refundHex,
		connectors:       connectors,
		confs:            confs,
		coinsMap:         coinsMap,
		funding:          s.n.orderFunding(s.id),
	}
}

func (n *Node) orderFunding(id [32]byte) []wallet.Utxo {
	o := n.store.Get(hexEncode(id[:]))
	if o == nil {
		return nil
	}
	return o.UsedCoins
}

func (n *Node) newMakerSession(o *Order, p MakeOrderParams, priv [32]byte, pub [33]byte) {
	xPub, err := crypto.NewPrivateKey()
	if err != nil {
		xlog.Error("swap: failed to generate secret", "order", hexEncode(o.ID[:]), "err", err)
		return
	}
	xpk, err := crypto.CompressedPubKey(xPub)
	if err != nil {
		xlog.Error("swap: failed to derive public key", "order", hexEncode(o.ID[:]), "err", err)
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
		privKey:       priv,
		pubKey:        pub,
		secret:        xpk,
		secretHash:    coins.KeyID(xpk[:]),
		state:         csMaker,
		lastProgress:  uint64(NowMicro()),
	}
	// The session's trusted hub key is pinned at session creation, never learned from network packets.
	s.hub = o.HubAddress
	s.hubKey = decodePub33(o.SNodePubkey)
	n.sessions[hexEncode(o.ID[:])] = s
	xlog.Info("swap session created", "order", hexEncode(o.ID[:]), "role", "maker",
		"srcCur", o.FromCurrency, "srcAmt", o.FromAmount, "dstCur", o.ToCurrency, "dstAmt", o.ToAmount,
		"hub", o.SNodePubkey)
}

func (n *Node) newTakerSession(o *Order, p TakeOrderParams, priv [32]byte, pub [33]byte) {
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
		privKey:       priv,
		pubKey:        pub,
		state:         csTaker,
		lastProgress:  uint64(NowMicro()),
	}
	s.hubKey = decodePub33(o.SNodePubkey)
	s.hub = o.HubAddress
	n.sessions[hexEncode(o.ID[:])] = s
	xlog.Info("swap session created", "order", hexEncode(o.ID[:]), "role", "taker",
		"srcCur", o.ToCurrency, "srcAmt", o.ToAmount, "dstCur", o.FromCurrency, "dstAmt", o.FromAmount,
		"hubKeyPinned", s.hubKey != [33]byte{}, "hub", o.SNodePubkey)
}

// OnHold handles the hub's Hold and replies HoldApply echoing our source address.
func (s *SwapSession) OnHold(b *proto.HoldBody) (proto.XBridgeCommand, responseBody, error) {
	orderID := hexEncode(s.id[:])
	// The hub retransmits Hold for the session's whole lifetime: a redelivered
	// Hold must never rewind a session that already advanced past it.
	if s.state >= csHoldApplied {
		xlog.Info("Hold ignored: swap already past hold", "order", orderID, "state", s.state.String())
		return 0, nil, nil
	}
	if err := s.verifyHold(b); err != nil {
		xlog.Warn("hold rejected", "order", orderID, "err", err)
		return 0, nil, nil
	}
	src, err := holdApplySource(s)
	if err != nil {
		return 0, nil, err
	}
	// The maker adopts the taker's partial amounts here (verifyHold already
	// gated bounds and drift), so its deposit locks the taken amount.
	if s.isMaker {
		s.srcAmt = b.ToAmount
		s.dstAmt = b.FromAmount
		if s.n != nil && s.n.store != nil {
			s.n.store.Update(orderID, func(o *Order) {
				o.FromAmount = b.ToAmount
				o.ToAmount = b.FromAmount
			})
			// Persist the resize: a crash before the next tick would restore
			// pre-resize amounts and the rebuilt deposit would lock the full amount.
			s.n.persist()
		}
	}
	s.state = csHoldApplied
	s.setOrderStatus("hold")
	xlog.Info("hold applied", "order", orderID, "state", s.state.String())
	return proto.XbcTransactionHoldApply, &proto.HoldApplyBody{
		HubAddress: s.hub, ClientAddress: src, ID: s.id,
	}, nil
}

// verifyHold re-checks the hub-relayed amounts: From/To are the taker's give
// and take — (dstAmt, srcAmt) for a maker, the session's own pair for a taker.
func (s *SwapSession) verifyHold(b *proto.HoldBody) error {
	if s.isMaker {
		if b.ToAmount > s.srcAmt {
			return fmt.Errorf("taker requesting an amount that is too large")
		}
		if b.FromAmount > s.dstAmt {
			return fmt.Errorf("taker sending an amount that is too large")
		}
		if o := s.order(); o != nil && o.PartialAllowed && b.ToAmount < o.MinFromAmount {
			return fmt.Errorf("taker requesting an amount that is too small")
		}
		if !swap.PartialOrderDriftCheck(s.srcAmt, s.dstAmt, b.FromAmount, b.ToAmount) {
			return fmt.Errorf("taker price doesn't match maker expected price")
		}
		return nil
	}
	if b.FromAmount != s.srcAmt {
		return fmt.Errorf("taker from amount from snode should match expected amount")
	}
	if b.ToAmount != s.dstAmt {
		return fmt.Errorf("taker to amount from snode should match expected amount")
	}
	// Drift is checked against the order's original maker-facing pair; with no
	// store order a taker's original pair is (dstAmt, srcAmt).
	origFrom, origTo := s.dstAmt, s.srcAmt
	if o := s.order(); o != nil && o.OrigFromAmount > 0 {
		origFrom, origTo = o.OrigFromAmount, o.OrigToAmount
	}
	if !swap.PartialOrderDriftCheck(origFrom, origTo, s.srcAmt, s.dstAmt) {
		return fmt.Errorf("taker price doesn't match maker expected price")
	}
	return nil
}

// OnInit handles the hub's Init and replies Initialized echoing our destination address.
func (s *SwapSession) OnInit(b *proto.InitBody) (proto.XBridgeCommand, responseBody, error) {
	orderID := hexEncode(s.id[:])
	if s.state >= csInitialized {
		xlog.Info("Init ignored: swap already initialized", "order", orderID, "state", s.state.String())
		return 0, nil, nil
	}
	if err := s.verifyInit(b); err != nil {
		xlog.Warn("init rejected", "order", orderID, "err", err)
		return 0, nil, nil
	}
	s.state = csInitialized
	s.setOrderStatus("initialized")
	xlog.Info("initialized", "order", orderID, "state", s.state.String())
	return proto.XbcTransactionInitialized, &proto.InitializedBody{
		HubAddress: s.hub, ClientAddress: b.ClientAddress, ID: s.id,
	}, nil
}

func (s *SwapSession) verifyInit(b *proto.InitBody) error {
	if b.ID != s.id {
		return fmt.Errorf("order id mismatch")
	}
	if b.FromAddress != decodeAddrHash(s.srcCur, s.ourSourceAddr) {
		return fmt.Errorf("from address mismatch")
	}
	if b.FromCurrency != s.srcCur {
		return fmt.Errorf("from currency mismatch")
	}
	if b.FromAmount != s.srcAmt {
		return fmt.Errorf("from amount mismatch")
	}
	if b.ToAddress != decodeAddrHash(s.dstCur, s.ourDestAddr) {
		return fmt.Errorf("to address mismatch")
	}
	if b.ToCurrency != s.dstCur {
		return fmt.Errorf("to currency mismatch")
	}
	if b.ToAmount != s.dstAmt {
		return fmt.Errorf("to amount mismatch")
	}
	return nil
}

func (s *SwapSession) order() *Order {
	if s.n == nil || s.n.store == nil {
		return nil
	}
	return s.n.store.Get(hexEncode(s.id[:]))
}

// setOrderStatus mirrors the session's handshake progress onto the order's
// Status field (what dxGetOrders surfaces); s.state stays authoritative.
func (s *SwapSession) setOrderStatus(status string) {
	if s.n == nil || s.n.store == nil {
		return
	}
	idHex := hexEncode(s.id[:])
	s.n.store.Update(idHex, func(o *Order) {
		o.Status = status
		// Refresh the timestamp so dxGetOrder's updated_at tracks session progress.
		o.Updated = NowMicro()
	})
}

func decodeAddrHash(cur, addrStr string) [20]byte {
	c, ok := coins.Get(cur)
	if !ok {
		return [20]byte{}
	}
	a, err := c.DecodeAddress(addrStr)
	if err != nil {
		return [20]byte{}
	}
	var h [20]byte
	copy(h[:], a.Hash)
	return h
}

// OnCreateA builds and broadcasts the maker's deposit A, then replies CreatedA.
func (s *SwapSession) OnCreateA(b *proto.CreateABody) (proto.XBridgeCommand, responseBody, error) {
	orderID := hexEncode(s.id[:])
	if !s.isMaker {
		xlog.Info("CreateA received for wrong role, expected maker", "order", orderID, "state", s.state.String())
		return 0, nil, nil
	}
	if s.state >= csCreatedA {
		xlog.Info("CreateA ignored: swap already past deposit", "order", orderID, "state", s.state.String())
		return 0, nil, nil
	}
	if s.await {
		xlog.Info("CreateA ignored: deposit worker in flight", "order", orderID)
		return 0, nil, nil
	}
	if b.BPubKey == [33]byte{} {
		return 0, nil, fmt.Errorf("api: CreateA missing B pubkey")
	}
	s.theirPub = b.BPubKey
	xlog.Info("CreateA: building deposit A", "order", orderID, "counterparty", hexEncode(b.BPubKey[:]))
	s.n.store.Update(orderID, func(o *Order) {
		o.OtherPubkey = hexEncode(b.BPubKey[:])
	})
	c := s.snapshot()
	task := makerDepositBuildTask(s, c)
	if !s.n.engineRunning.Load() {
		v, terr := safeTaskRun(task)
		if terr != nil {
			// Run the apply even on error: it owns the failure handling and the
			// retry scheduling, so returning early would skip them.
			s.applyCreatedA(nil, terr)
			return 0, nil, terr
		}
		return proto.XbcTransactionCreatedA, s.applyCreatedA(v, nil), nil
	}
	s.holdAwait()
	s.n.postSwapTask(orderID, task)
	return 0, nil, nil
}

func (n *Node) sessionAlive(s *SwapSession) bool {
	if n == nil || s == nil {
		return false
	}
	cur, ok := n.sessions[hexEncode(s.id[:])]
	return ok && cur == s
}

// dropGhostResult reports whether the worker result belongs to a session pruned
// before it landed: drop it, but the caller must still record any broadcast (chain truth outlives the session).
func (s *SwapSession) dropGhostResult(phase string) bool {
	if s.n.sessionAlive(s) {
		return false
	}
	xlog.Warn("ghost result dropped: session pruned before worker result", "order", hexEncode(s.id[:]), "phase", phase)
	return true
}

func (s *SwapSession) applyCreatedA(v any, terr error) responseBody {
	orderID := hexEncode(s.id[:])
	if s.dropGhostResult("build") {
		return nil
	}
	if terr != nil {
		s.releaseAwait()
		if errors.Is(terr, wallet.ErrDepositNotReady) {
			// Unreachable today: maker buildDeposit spends recorded funding and never
			// checks counterparty visibility; kept uniform with the other build sites.
			xlog.Info("CreateA build not ready, fast re-poll", "order", orderID, "err", terr)
		} else {
			xlog.Error("CreateA deposit task failed", "order", orderID, "err", terr)
		}
		if s.ourDepositTxID == "" {
			now := NowMicro()
			if !s.scheduleNotReady(now, "maker", false, terr) {
				scheduleDepositRetry(s, now, "maker", terr)
			}
		}
		return nil
	}
	out := v.(depositOutcome)
	clearDepositRetry(s)
	s.ourDepositTxID = out.txid
	s.ourLockTime = out.lockTime
	s.refundHex = out.refundHex
	s.depositHex = out.depositHex
	s.n.store.Update(orderID, func(o *Order) {
		o.BinTxId = out.txid
		o.DepositSent = false
	})
	s.n.store.Update(orderID, func(o *Order) {
		o.RefundTx = out.refundHex
	})
	// Durable intent BEFORE broadcast: from here on, a crash recovers the
	// pre-signed refund from disk (and the sweep owns it post-restart).
	if err := s.n.persistNow(); err != nil {
		s.releaseAwait()
		xlog.Error("CreateA intent persist failed, deposit withheld", "order", orderID, "err", err)
		return nil
	}
	txLogDepositBuilt(s.id, "A", s.srcCur, s.srcAmt, s.dstCur, s.dstAmt, out.lockTime, out.depositHex, out.refundHex)
	var body responseBody
	s.n.postBroadcastTask(orderID, out.conn, s.srcCur, out.depositHex, broadcastDeposit, func(sentID string, berr error) {
		body = s.applyCreatedABroadcast(out, sentID, berr)
	})
	return body
}

func (s *SwapSession) applyCreatedABroadcast(out depositOutcome, sentID string, terr error) responseBody {
	orderID := hexEncode(s.id[:])
	if s.dropGhostResult("broadcast") {
		if s.n != nil {
			s.n.recordBroadcast(orderID, broadcastDeposit, s.srcCur, out.txid, out.depositHex)
		}
		return nil
	}
	s.releaseAwait()
	if terr != nil {
		xlog.Error("CreateA deposit broadcast failed", "order", orderID, "err", terr)
		if s.ourDepositTxID != "" && s.depositHex != "" {
			scheduleDepositRetry(s, NowMicro(), "maker-broadcast", terr)
		}
		return nil
	}
	clearDepositRetry(s)
	if sentID != "" && sentID != out.txid {
		xlog.Warn("CreateA: wallet reported a different sent txid", "order", orderID, "local", out.txid, "sent", sentID)
	}
	s.n.store.Update(orderID, func(o *Order) {
		o.DepositSent = true
	})
	s.state = csCreatedA
	s.setOrderStatus("created")
	xlog.Info("deposit A broadcast", "order", orderID, "txid", out.txid,
		"lockTime", out.lockTime, "secretHash", hexEncode(s.secretHash[:]))
	xlog.Debug("deposit A refund pre-signed", "order", orderID)
	// Transcript only after the confirmed broadcast: it must never claim an unconfirmed one.
	txLogDeposit(s.id, "A", s.srcCur, s.srcAmt, s.dstCur, s.dstAmt, out.lockTime, out.depositHex, out.refundHex)
	body := responseBody(&proto.CreatedABody{
		HubAddress: s.hub, ID: s.id,
		ADepositTxID: out.txid, HashedSecret: s.secretHash, ALockTime: out.lockTime,
		RefTx: out.refundHex,
	})
	if s.n.engineRunning.Load() {
		s.n.persist()
		if err := s.n.send(s.hub, proto.XbcTransactionCreatedA, body, s.privKey[:]); err != nil {
			xlog.Error("CreatedA response send failed", "order", orderID, "err", err)
		}
		return nil
	}
	return body
}

// OnCreateB learns the maker's deposit, builds and broadcasts the taker's deposit B, then replies CreatedB.
func (s *SwapSession) OnCreateB(b *proto.CreateBBody) (proto.XBridgeCommand, responseBody, error) {
	orderID := hexEncode(s.id[:])
	if s.state >= csCreatedB {
		xlog.Info("CreateB ignored: swap already past deposit", "order", orderID, "state", s.state.String())
		return 0, nil, nil
	}
	if s.await {
		xlog.Info("CreateB ignored: deposit worker in flight", "order", orderID)
		return 0, nil, nil
	}
	if s.isMaker {
		xlog.Info("CreateB received for wrong role, expected taker", "order", orderID, "state", s.state.String())
		return 0, nil, nil
	}
	if b.ADepositTxID == "" {
		xlog.Warn("CreateB missing maker deposit txid, canceling", "order", orderID)
		s.sendSelfCancel(crBadADepositTx)
		return 0, nil, nil
	}
	if s.theirPub != [33]byte{} {
		xlog.Info("CreateB ignored: counterparty key already adopted", "order", orderID)
		return 0, nil, nil
	}
	if b.APubKey == [33]byte{} {
		return 0, nil, fmt.Errorf("api: CreateB missing A pubkey")
	}
	s.theirPub = b.APubKey
	s.theirDepositTxID = b.ADepositTxID
	s.n.store.Update(orderID, func(o *Order) {
		o.OBinTxId = b.ADepositTxID
		o.OtherPubkey = hexEncode(b.APubKey[:])
	})
	s.theirSecretHash = b.HashedSecret
	s.theirLockTime = b.ALockTime
	xlog.Info("CreateB: learned maker deposit", "order", orderID,
		"makerDeposit", b.ADepositTxID, "makerLockTime", b.ALockTime, "secretHash", hexEncode(b.HashedSecret[:]))
	c := s.snapshot()
	task := takerDepositBuildTask(s, c)
	if !s.n.engineRunning.Load() {
		v, terr := safeTaskRun(task)
		if terr != nil {
			s.applyCreatedB(nil, terr)
			return 0, nil, terr
		}
		return proto.XbcTransactionCreatedB, s.applyCreatedB(v, nil), nil
	}
	s.holdAwait()
	s.n.postSwapTask(orderID, task)
	return 0, nil, nil
}

func (s *SwapSession) applyCreatedB(v any, terr error) responseBody {
	orderID := hexEncode(s.id[:])
	if s.dropGhostResult("build") {
		return nil
	}
	if terr != nil {
		s.releaseAwait()
		if s.failSelfCancel(terr) {
			return nil
		}
		if errors.Is(terr, wallet.ErrDepositNotReady) {
			xlog.Info("CreateB build not ready, fast re-poll", "order", orderID, "err", terr)
		} else {
			xlog.Error("CreateB deposit task failed", "order", orderID, "err", terr)
		}
		if s.ourDepositTxID == "" {
			now := NowMicro()
			if !s.scheduleNotReady(now, "taker", false, terr) {
				scheduleDepositRetry(s, now, "taker", terr)
			}
		}
		return nil
	}
	out := v.(createdBOutcome)
	clearDepositRetry(s)
	s.ourDepositTxID = out.out.txid
	s.ourLockTime = out.out.lockTime
	s.refundHex = out.out.refundHex
	s.depositHex = out.out.depositHex
	s.theirDepositVout = out.check.DepositVout
	s.theirP2SHNative = out.check.P2SHNative
	s.theirOverpayment = out.check.Excess
	s.n.store.Update(orderID, func(o *Order) {
		o.BinTxId = out.out.txid
		o.DepositSent = false
	})
	s.n.store.Update(orderID, func(o *Order) {
		o.RefundTx = out.out.refundHex
	})
	s.n.store.Update(orderID, func(o *Order) {
		o.OBinTxVout = out.check.DepositVout
		o.OBinTxP2SHAmount = toXBridgeAmt(counterpartyCoin(s), out.check.P2SHNative)
		o.OOverpayment = out.check.Excess
	})
	if err := s.n.persistNow(); err != nil {
		s.releaseAwait()
		xlog.Error("CreateB intent persist failed, deposit withheld", "order", orderID, "err", err)
		return nil
	}
	txLogDepositBuilt(s.id, "B", s.srcCur, s.srcAmt, s.dstCur, s.dstAmt, out.out.lockTime, out.out.depositHex, out.out.refundHex)
	var body responseBody
	s.n.postBroadcastTask(orderID, out.out.conn, s.srcCur, out.out.depositHex, broadcastDeposit, func(sentID string, berr error) {
		body = s.applyCreatedBBroadcast(out, sentID, berr)
	})
	return body
}

func (s *SwapSession) applyCreatedBBroadcast(out createdBOutcome, sentID string, terr error) responseBody {
	orderID := hexEncode(s.id[:])
	if s.dropGhostResult("broadcast") {
		if s.n != nil {
			s.n.recordBroadcast(orderID, broadcastDeposit, s.srcCur, out.out.txid, out.out.depositHex)
		}
		return nil
	}
	s.releaseAwait()
	if terr != nil {
		xlog.Error("CreateB deposit broadcast failed", "order", orderID, "err", terr)
		if s.ourDepositTxID != "" && s.depositHex != "" {
			scheduleDepositRetry(s, NowMicro(), "taker-broadcast", terr)
		}
		return nil
	}
	clearDepositRetry(s)
	if sentID != "" && sentID != out.out.txid {
		xlog.Warn("CreateB: wallet reported a different sent txid", "order", orderID, "local", out.out.txid, "sent", sentID)
	}
	s.n.store.Update(orderID, func(o *Order) {
		o.DepositSent = true
	})
	s.state = csCreatedB
	s.setOrderStatus("created")
	xlog.Info("deposit B broadcast", "order", orderID, "txid", out.out.txid,
		"lockTime", out.out.lockTime, "makerDeposit", s.theirDepositTxID)
	xlog.Debug("deposit B refund pre-signed", "order", orderID)
	txLogDeposit(s.id, "B", s.srcCur, s.srcAmt, s.dstCur, s.dstAmt, out.out.lockTime, out.out.depositHex, out.out.refundHex)
	body := responseBody(&proto.CreatedBBody{
		HubAddress: s.hub, ID: s.id,
		BDepositTxID: out.out.txid, BLockTime: out.out.lockTime,
		RefTx: out.out.refundHex,
	})
	if s.n.engineRunning.Load() {
		s.n.persist()
		if err := s.n.send(s.hub, proto.XbcTransactionCreatedB, body, s.privKey[:]); err != nil {
			xlog.Error("CreatedB response send failed", "order", orderID, "err", err)
		}
		return nil
	}
	return body
}

// OnConfirmA redeems the taker's deposit by revealing our secret on-chain, then broadcasts A's payTx.
func (s *SwapSession) OnConfirmA(b *proto.ConfirmABody) (proto.XBridgeCommand, responseBody, error) {
	orderID := hexEncode(s.id[:])
	if s.state >= csConfirmedA {
		xlog.Info("ConfirmA ignored: swap already past claim", "order", orderID, "state", s.state.String())
		return 0, nil, nil
	}
	if !s.isMaker {
		xlog.Info("ConfirmA received for wrong role, expected maker", "order", orderID, "state", s.state.String())
		return 0, nil, nil
	}
	if s.await {
		xlog.Info("ConfirmA ignored: claim worker in flight", "order", orderID)
		return 0, nil, nil
	}
	s.theirDepositTxID = b.BDepositTxID
	s.theirLockTime = b.BLockTime
	s.n.store.Update(orderID, func(o *Order) {
		o.OBinTxId = b.BDepositTxID
	})
	c := s.snapshot()
	task := makerClaimBuildTask(s, c)
	if !s.n.engineRunning.Load() {
		v, terr := safeTaskRun(task)
		if terr != nil {
			s.applyConfirmedA(nil, terr)
			return 0, nil, terr
		}
		return proto.XbcTransactionConfirmedA, s.applyConfirmedA(v, nil), nil
	}
	s.holdAwait()
	s.n.postSwapTask(orderID, task)
	return 0, nil, nil
}

func (s *SwapSession) applyConfirmedA(v any, terr error) responseBody {
	orderID := hexEncode(s.id[:])
	if s.dropGhostResult("build") {
		return nil
	}
	if terr != nil {
		s.releaseAwait()
		if s.failSelfCancel(terr) {
			return nil
		}
		if errors.Is(terr, wallet.ErrDepositNotReady) {
			xlog.Info("ConfirmA build not ready, fast re-poll", "order", orderID, "err", terr)
		} else {
			xlog.Error("ConfirmA claim task failed", "order", orderID, "err", terr)
		}
		if s.claimTxID == "" {
			now := NowMicro()
			if !s.scheduleNotReady(now, "maker", true, terr) {
				scheduleClaimRetry(s, now, "maker", terr)
			}
		}
		return nil
	}
	out := v.(confirmOutcome)
	clearClaimRetry(s)
	s.theirDepositVout = out.check.DepositVout
	s.theirP2SHNative = out.check.P2SHNative
	s.theirOverpayment = out.check.Excess
	s.claimHex, s.claimTxID, s.claimCur = out.payHex, out.payTxID, out.cur
	if err := s.n.persistNow(); err != nil {
		s.releaseAwait()
		xlog.Error("ConfirmA intent persist failed, claim withheld", "order", orderID, "err", err)
		return nil
	}
	txLogClaimBuilt(s.id, "A", out.cur, out.payHex)
	var body responseBody
	s.n.postBroadcastTask(orderID, out.conn, out.cur, out.payHex, broadcastClaim, func(sentID string, berr error) {
		body = s.applyConfirmedABroadcast(out, sentID, berr)
	})
	return body
}

func (s *SwapSession) applyConfirmedABroadcast(out confirmOutcome, sentID string, terr error) responseBody {
	orderID := hexEncode(s.id[:])
	if s.dropGhostResult("broadcast") {
		if s.n != nil {
			s.n.recordBroadcast(orderID, broadcastClaim, out.cur, out.payTxID, out.payHex)
		}
		return nil
	}
	s.releaseAwait()
	if terr != nil {
		xlog.Error("ConfirmA claim broadcast failed", "order", orderID, "err", terr)
		if s.claimTxID != "" && s.claimHex != "" {
			scheduleClaimRetry(s, NowMicro(), "maker-broadcast", terr)
		}
		return nil
	}
	clearClaimRetry(s)
	if sentID != "" && sentID != out.payTxID {
		xlog.Warn("ConfirmA: wallet reported a different sent txid", "order", orderID, "local", out.payTxID, "sent", sentID)
	}
	payTxID := out.payTxID
	s.state = csFinished
	s.setOrderStatus("finished")
	xlog.Info("ConfirmA: payTx broadcast", "order", orderID, "payTxID", payTxID)
	txLogClaim(s.id, "A", out.cur, payTxID, out.payHex)
	s.n.store.Update(orderID, func(o *Order) {
		o.CounterpartyRedeemed = true
		o.OBinTxVout = out.check.DepositVout
		o.OBinTxP2SHAmount = toXBridgeAmt(counterpartyCoin(s), out.check.P2SHNative)
		o.OOverpayment = out.check.Excess
	})
	body := responseBody(&proto.ConfirmedABody{
		HubAddress: s.hub, ID: s.id, APayTxID: payTxID,
	})
	if s.n.engineRunning.Load() {
		s.n.persist()
		if err := s.n.send(s.hub, proto.XbcTransactionConfirmedA, body, s.privKey[:]); err != nil {
			xlog.Error("ConfirmedA response send failed", "order", orderID, "err", err)
		}
		return nil
	}
	return body
}

// OnConfirmB recovers the secret from A's payTx, then redeems the maker's deposit and replies ConfirmedB.
func (s *SwapSession) OnConfirmB(b *proto.ConfirmBBody) (proto.XBridgeCommand, responseBody, error) {
	orderID := hexEncode(s.id[:])
	if s.state >= csConfirmedB {
		xlog.Info("ConfirmB ignored: swap already past claim", "order", orderID, "state", s.state.String())
		return 0, nil, nil
	}
	if s.await {
		xlog.Info("ConfirmB ignored: claim worker in flight", "order", orderID)
		return 0, nil, nil
	}
	s.theirPayTxID = b.APayTxID
	s.n.persist()
	c := s.snapshot()
	task := takerClaimBuildTask(s, c, b.APayTxID)
	if !s.n.engineRunning.Load() {
		v, terr := safeTaskRun(task)
		if terr != nil {
			s.applyConfirmedB(nil, terr)
			return 0, nil, terr
		}
		return proto.XbcTransactionConfirmedB, s.applyConfirmedB(v, nil), nil
	}
	s.holdAwait()
	s.n.postSwapTask(orderID, task)
	return 0, nil, nil
}

func (s *SwapSession) applyConfirmedB(v any, terr error) responseBody {
	orderID := hexEncode(s.id[:])
	if s.dropGhostResult("build") {
		return nil
	}
	if terr != nil {
		s.releaseAwait()
		if errors.Is(terr, wallet.ErrDepositNotReady) {
			xlog.Info("ConfirmB build not ready, fast re-poll", "order", orderID, "err", terr)
		} else {
			xlog.Error("ConfirmB claim task failed", "order", orderID, "err", terr)
		}
		if s.claimTxID == "" {
			now := NowMicro()
			if !s.scheduleNotReady(now, "taker", true, terr) {
				scheduleClaimRetry(s, now, "taker", terr)
			}
		}
		return nil
	}
	out := v.(confirmOutcome)
	clearClaimRetry(s)
	s.secret = out.secret
	s.claimHex, s.claimTxID, s.claimCur = out.payHex, out.payTxID, out.cur
	if err := s.n.persistNow(); err != nil {
		s.releaseAwait()
		xlog.Error("ConfirmB intent persist failed, claim withheld", "order", orderID, "err", err)
		return nil
	}
	txLogClaimBuilt(s.id, "B", out.cur, out.payHex)
	var body responseBody
	s.n.postBroadcastTask(orderID, out.conn, out.cur, out.payHex, broadcastClaim, func(sentID string, berr error) {
		body = s.applyConfirmedBBroadcast(out, sentID, berr)
	})
	return body
}

func (s *SwapSession) applyConfirmedBBroadcast(out confirmOutcome, sentID string, terr error) responseBody {
	orderID := hexEncode(s.id[:])
	if s.dropGhostResult("broadcast") {
		if s.n != nil {
			s.n.recordBroadcast(orderID, broadcastClaim, out.cur, out.payTxID, out.payHex)
		}
		return nil
	}
	s.releaseAwait()
	if terr != nil {
		xlog.Error("ConfirmB claim broadcast failed", "order", orderID, "err", terr)
		if s.claimTxID != "" && s.claimHex != "" {
			scheduleClaimRetry(s, NowMicro(), "taker-broadcast", terr)
		}
		return nil
	}
	clearClaimRetry(s)
	if sentID != "" && sentID != out.payTxID {
		xlog.Warn("ConfirmB: wallet reported a different sent txid", "order", orderID, "local", out.payTxID, "sent", sentID)
	}
	s.state = csFinished
	s.setOrderStatus("finished")
	xlog.Info("ConfirmB: payTx broadcast", "order", orderID, "payTxID", out.payTxID)
	txLogClaim(s.id, "B", out.cur, out.payTxID, out.payHex)
	s.n.store.Update(orderID, func(o *Order) {
		o.CounterpartyRedeemed = true
	})
	body := responseBody(&proto.ConfirmedBBody{
		HubAddress: s.hub, ID: s.id, BPayTxID: out.payTxID,
	})
	if s.n.engineRunning.Load() {
		s.n.persist()
		if err := s.n.send(s.hub, proto.XbcTransactionConfirmedB, body, s.privKey[:]); err != nil {
			xlog.Error("ConfirmedB response send failed", "order", orderID, "err", err)
		}
		return nil
	}
	return body
}

// finishedMayTerminate reports whether a hub Finished may prune the session:
// terminating while our own deposit is still out strands its pre-signed refund.
func (s *SwapSession) finishedMayTerminate(orderID string) bool {
	// Role-insensitive: the live path jumps to csFinished at the claim broadcast;
	// csConfirmedA/B comes only from restore, where the claim is already on-chain.
	if s.state >= csConfirmedA {
		return true
	}
	if o := s.n.store.Get(orderID); o != nil && o.CounterpartyRedeemed {
		return true
	}
	return s.state < csCreatedA && !s.n.orderDepositSent(orderID)
}

// OnFinished marks the order terminal and moves it to history; a Finished arriving while our deposit is still out and no claim exists is deferred instead.
func (s *SwapSession) OnFinished(b *proto.FinishedBody) (proto.XBridgeCommand, responseBody, error) {
	orderID := hexEncode(s.id[:])
	if !s.finishedMayTerminate(orderID) {
		// Bounded: every hub packet re-stamps lastProgress, so a live hub is
		// bounded by the locktime sweep; 30m of silence trips the stall watchdog.
		if earlyFinishedDedup.Event(orderID) {
			xlog.Warn("deferring early Finished: deposit out, no claim yet; session stays live for the refund sweep",
				"order", orderID, "state", s.state.String())
		}
		return 0, nil, nil
	}
	s.n.store.MoveToHistory(orderID, "finished", 0, NowMicro())
	s.state = csFinished
	xlog.Info("swap finished", "order", orderID, "state", s.state.String())
	s.n.persist()
	s.n.pruneSessions()
	return 0, nil, nil
}

// probeOwnDeposit classifies our own deposit outpoint: an inconclusive reading
// returns (false, false), so blindness never counts as spent — nor as proven live.
func probeOwnDeposit(conn wallet.Connector, depTxID string, depVout uint32) (known, spent bool) {
	if conn == nil || depTxID == "" {
		return false, false
	}
	vtx, verr := conn.GetRawTransactionVerbose(depTxID)
	if verr != nil || !vtx.HasConfirmations || vtx.Confirmations < 0 {
		return false, false
	}
	_, ok, gerr := conn.GetTxOut(depTxID, depVout)
	if gerr != nil {
		return false, false
	}
	return true, !ok
}

// refundOutcome is a refund broadcast result: depositSpent reports our own
// deposit already spent — the counterparty claimed, so no refund is possible.
type refundOutcome struct {
	txid         string
	depositSpent bool
	// sendErr carries the broadcast failure the spent verdict overrode: the
	// spent verdict must not hide the wallet's rejection from the caller or the log.
	sendErr error
}

// errRefundDepositSpent reports a refund abandoned because the counterparty
// already spent our deposit; surfaced to manual callers through done.
var errRefundDepositSpent = errors.New("api: own deposit already spent, hunting secret instead of refunding")

func runRefundTask(conn wallet.Connector, cur, refundHex string, lockTime uint32, checkLock bool, depTxID string, depVout uint32, proveSpend bool) (refundOutcome, error) {
	if conn == nil {
		return refundOutcome{}, fmt.Errorf("api: no connector for %s", cur)
	}
	if checkLock {
		h, err := conn.GetBlockCount()
		if err != nil || h < 1 {
			return refundOutcome{}, fmt.Errorf("api: getblockcount %s: %v", cur, err)
		}
		if uint32(h) < lockTime {
			return refundOutcome{}, nil
		}
	}
	txid, err := conn.SendRawTransaction(refundHex)
	if err == nil {
		return refundOutcome{txid: txid}, nil
	}
	// A broadcast failure may be a re-send of an already-broadcast refund:
	// a known txid proves node visibility (mempool or block); else the send error stands.
	if localID, lerr := txIDFromHex(refundHex); lerr == nil {
		if _, gerr := conn.GetRawTransaction(localID); gerr == nil {
			xlog.Info("refund already on chain", "coin", cur, "txid", localID)
			return refundOutcome{txid: localID}, nil
		}
	}
	if proveSpend {
		if known, spent := probeOwnDeposit(conn, depTxID, depVout); known && spent {
			return refundOutcome{depositSpent: true, sendErr: err}, nil
		}
	}
	return refundOutcome{}, err
}

// postRefundTask runs a refund broadcast inline or on the worker pool; even a
// dropped task invokes done with an error so awaiting callers never block.
func (n *Node) postRefundTask(orderID, cur, refundHex string, lockTime uint32, checkLock bool, done func(txid string, err error)) bool {
	conn := n.cfg().Connectors[cur]
	depTxID, depVout, proveSpend := "", uint32(0), false
	if s := n.sessions[orderID]; s != nil && s.ourDepositTxID != "" {
		depTxID, depVout = s.ourDepositTxID, uint32(ownDepositVout)
		proveSpend = n.rollbackGate(orderID)
	} else if o := n.store.Get(orderID); o != nil && o.BinTxId != "" {
		depTxID, depVout = o.BinTxId, uint32(ownDepositVout)
		proveSpend = n.rollbackGate(orderID)
	}
	task := workTask{
		orderID: orderID,
		run: func() (any, error) {
			return runRefundTask(conn, cur, refundHex, lockTime, checkLock, depTxID, depVout, proveSpend)
		},
		apply: func(v any, err error) {
			delete(n.pendingRefunds, orderID)
			out, _ := v.(refundOutcome)
			if err == nil && out.depositSpent {
				s := n.sessions[orderID]
				if s != nil && !s.isMaker {
					n.enterSecretHunt(s, orderID, cur)
					if done != nil {
						done("", errRefundDepositSpent)
					}
					return
				}
				if s != nil {
					xlog.Error("maker deposit spent without local finish: protocol contradiction",
						"order", orderID, "coin", cur, "deposit", s.ourDepositTxID)
				} else {
					if huntWarnDedup.Event(orderID) {
						xlog.Warn("own deposit spent with no live session: manual recovery required",
							"order", orderID, "coin", cur)
					}
					if done == nil {
						n.recordRefundFailure(orderID)
					} else {
						done("", errRefundDepositSpent)
					}
					return
				}
				err = out.sendErr
				if err == nil {
					err = errRefundDepositSpent
				}
			}
			txid := out.txid
			if done != nil {
				done(txid, err)
			}
			if err != nil {
				xlog.Warn("refund broadcast failed", "order", orderID, "err", err)
				if n.rollbackGate(orderID) {
					n.store.Update(orderID, func(o *Order) {
						// Never clobber a terminal order (a user cancel is not a
						// failure) and stay idempotent across repeated retries.
						if !isOrderTerminal(o.Status) && o.Status != "rollback failed" {
							o.Status = "rollback failed"
							o.Updated = NowMicro()
						}
					})
					n.persist()
				}
				// Only fire-and-forget attempts (done == nil) back off; a manual
				// BroadcastRefund always bypasses the backoff.
				if done == nil {
					n.recordRefundFailure(orderID)
				}
				return
			}
			if txid == "" {
				xlog.Debug("refund deferred: deposit locktime not reached", "order", orderID)
				return
			}
			xlog.Info("refund broadcast", "order", orderID, "txid", txid)
			n.clearRefundBackoff(orderID)
			if localID, lerr := txIDFromHex(refundHex); lerr == nil {
				n.recordBroadcast(orderID, broadcastRefund, cur, localID, refundHex)
			} else {
				xlog.Warn("reconcile: refund untrackable, bad hex", "order", orderID, "cur", cur)
			}
			if s := n.sessions[orderID]; s != nil {
				s.refundDone = true
				txLogRefund(s.id, cur, lockTime, txid)
			} else if o := n.store.Get(orderID); o != nil {
				txLogRefund(o.ID, cur, lockTime, txid)
			}
			if n.rollbackGate(orderID) {
				n.store.Update(orderID, func(o *Order) {
					// A refund redeems the deposit from any prior state, a user
					// cancel included; only truly dead records keep their status.
					if !isOrderTerminal(o.Status) || o.Status == "canceled" {
						if o.Status != "rolled back" {
							o.Status = "rolled back"
							o.Updated = NowMicro()
						}
					}
				})
			}
			n.persist()
			n.pruneSessions()
		},
	}
	if !n.engineRunning.Load() {
		v, err := safeTaskRun(task)
		task.apply(v, err)
		return true
	}
	select {
	case n.tasks <- task:
		return true
	default:
		delete(n.pendingRefunds, orderID)
		xlog.Warn("refund task dropped, engine busy", "order", orderID)
		if done != nil {
			done("", errors.New("api: refund task dropped, engine busy"))
		}
		return false
	}
}

// orderDepositSent reports the order's confirmed deposit broadcast: since
// intent-before-broadcast, a persisted refund intent alone proves nothing on-chain.
func (n *Node) orderDepositSent(orderID string) bool {
	o := n.store.Get(orderID)
	return o != nil && o.DepositSent
}

// rollbackGate reports whether an on-chain deposit exists to redeem: the live
// session state first, else the stored order (RefundTx present AND DepositSent).
func (n *Node) rollbackGate(orderID string) bool {
	if s := n.sessions[orderID]; s != nil {
		return s.state >= csCreatedA || n.orderDepositSent(orderID)
	}
	if o := n.store.Get(orderID); o != nil {
		return o.RefundTx != "" && o.DepositSent
	}
	return false
}

// postSwapTask runs a handshake build/claim task inline or on the worker pool;
// a dropped task clears the await guard so the hub's retransmit re-stages it.
func (n *Node) postSwapTask(orderID string, task workTask) bool {
	if !n.engineRunning.Load() {
		v, err := safeTaskRun(task)
		task.apply(v, err)
		return true
	}
	select {
	case n.tasks <- task:
		return true
	default:
		if s := n.sessions[orderID]; s != nil {
			s.releaseAwait()
		}
		xlog.Warn("swap task dropped, engine busy", "order", orderID)
		return false
	}
}

// postBroadcastTask broadcasts raw hex only after the engine durably persisted
// the intent, so a crash between broadcast and resume always recovers.
func (n *Node) postBroadcastTask(orderID string, conn wallet.Connector, cur, hex string, kind broadcastKind, apply func(sentID string, terr error)) bool {
	task := workTask{
		orderID: orderID,
		run: func() (any, error) {
			if conn == nil {
				return "", fmt.Errorf("api: no connector for %s", cur)
			}
			return conn.SendRawTransaction(hex)
		},
		apply: func(v any, terr error) {
			sentID, _ := v.(string)
			if terr == nil {
				if localID, lerr := txIDFromHex(hex); lerr == nil {
					n.recordBroadcast(orderID, kind, cur, localID, hex)
				} else {
					xlog.Warn("reconcile: broadcast untrackable, bad hex", "order", orderID, "cur", cur)
				}
				if s := n.sessions[orderID]; s != nil {
					s.lastProgress = uint64(NowMicro())
				}
			}
			apply(sentID, terr)
		},
	}
	if !n.engineRunning.Load() {
		v, err := safeTaskRun(task)
		task.apply(v, err)
		return true
	}
	select {
	case n.tasks <- task:
		return true
	default:
		if s := n.sessions[orderID]; s != nil {
			s.releaseAwait()
		}
		xlog.Warn("broadcast task dropped, engine busy", "order", orderID)
		return false
	}
}

func (n *Node) enterSecretHunt(s *SwapSession, orderID, cur string) {
	if !s.secretHunt {
		s.secretHunt = true
		s.huntSince = uint64(NowMicro())
		n.clearRefundBackoff(orderID)
		n.persist()
		xlog.Warn("own deposit already spent by counterparty claim: refund impossible, hunting secret on the deposit watch",
			"order", orderID, "coin", cur, "deposit", s.ourDepositTxID)
	}
}

// scanRefunds sweeps live sessions and broadcasts any pre-signed refund whose
// deposit lockTime has passed; pendingRefunds prevents a double-enqueue.
func (n *Node) scanRefunds() {
	for id, s := range n.sessions {
		if s.refundDone || s.refundHex == "" || s.state == csFinished {
			continue
		}
		if s.secretHunt {
			if huntWarnDedup.Event(id) {
				elapsed := "unknown age"
				if s.huntSince != 0 && uint64(NowMicro()) >= s.huntSince {
					elapsed = (time.Duration(uint64(NowMicro())-s.huntSince) * time.Microsecond).Round(time.Second).String()
				}
				xlog.Warn("secret hunt ongoing: refund stood down, deposit watch owns recovery",
					"order", id, "coin", s.srcCur, "deposit", s.ourDepositTxID, "huntingFor", elapsed)
			}
			continue
		}
		// Past our own claim broadcast the refund must not run: our claim revealed
		// the secret, so the counterparty can spend our deposit with no CLTV wait.
		if s.isMaker && s.state >= csConfirmedA {
			continue
		}
		if !s.isMaker && s.state >= csConfirmedB {
			continue
		}
		if n.refundBackoffActive(id) {
			continue
		}
		if s.state < csCreatedA && !n.orderDepositSent(id) {
			continue
		}
		if n.engineRunning.Load() {
			if n.pendingRefunds[id] {
				continue
			}
			n.pendingRefunds[id] = true
		}
		n.postRefundTask(id, s.srcCur, s.refundHex, s.ourLockTime, true, nil)
	}
}

// watchCounterpartyDeposits cancels a live pre-claim session only after its
// counterparty deposit disappears durably: a single missing reading may be a reorg or propagation lag, so a countdown absorbs it.
func (n *Node) watchCounterpartyDeposits() {
	for id, s := range n.sessions {
		// A round this sweep does not act must not bank patience credit: every
		// guard skip resets the vanish countdown, while an in-flight probe preserves it.
		if s.theirDepositTxID == "" || s.await {
			s.vanishMisses = 0
			continue
		}
		// A built claim owns the counterparty deposit now: its disappearance is
		// our own claim's spend, never a vanish.
		if s.claimHex != "" || s.claimTxID != "" {
			s.vanishMisses = 0
			continue
		}
		if s.secretHunt {
			s.vanishMisses = 0
			continue
		}
		// Only the pre-claim window is watched: past it the counterparty
		// deposit is legitimately spent by our own claim.
		if s.isMaker {
			if s.state < csCreatedA || s.state >= csConfirmedA {
				s.vanishMisses = 0
				continue
			}
		} else if s.state < csCreatedB || s.state >= csConfirmedB {
			s.vanishMisses = 0
			continue
		}
		// Cancelled, already-claimed, or already-failed orders must not summon a
		// second Cancel: the counterparty was notified or never needed it.
		if n.store != nil {
			if o := n.store.Get(id); o != nil && (isOrderTerminal(o.Status) || o.CounterpartyRedeemed ||
				o.Status == "rolled back" || o.Status == "rollback failed") {
				s.vanishMisses = 0
				continue
			}
		}
		// Stand down until the deposit validated once: theirP2SHNative is 0
		// only while validation has not run, never because the deposit is gone.
		if s.theirP2SHNative == 0 {
			s.vanishMisses = 0
			continue
		}
		if n.engineRunning.Load() {
			if n.pendingWatch[id] {
				continue
			}
			n.pendingWatch[id] = true
		}
		n.postDepositWatchTask(id)
	}
}

// depositWatchVanishThreshold scales to the watched chain: two blocks of
// patience absorb a shallow reorg; the unknown-block-time default errs toward patience.
func depositWatchVanishThreshold(blockTimeSec int) int {
	if blockTimeSec <= 0 {
		return 6
	}
	n := (2*blockTimeSec+59)/60 + 1 // ceil(2 blocks in ticks) + 1 spare
	if n < 3 {
		return 3
	}
	if n > 12 {
		return 12
	}
	return n
}

type depositWatchOutcome struct {
	unspent  bool
	ownKnown bool
	ownSpent bool
}

// runDepositWatchTask probes the counterparty deposit: an error means the
// wallet could not answer — a skipped round, never a cancel.
func runDepositWatchTask(conn wallet.Connector, cur, txid string, vout uint32) (bool, error) {
	if conn == nil {
		return false, fmt.Errorf("api: no connector for %s", cur)
	}
	_, ok, err := conn.GetTxOut(txid, vout)
	if err != nil {
		return false, err
	}
	return ok, nil
}

func (n *Node) postDepositWatchTask(orderID string) bool {
	s := n.sessions[orderID]
	if s == nil {
		return false
	}
	conn := n.cfg().Connectors[s.dstCur]
	// The worker must never read the live session: it is engine-owned (race detector).
	dstCur, depTxID, depVout := s.dstCur, s.theirDepositTxID, s.theirDepositVout
	blockTime := 0
	if cc := n.cfg().Confs[s.dstCur]; cc != nil && cc.BlockTime > 0 {
		blockTime = cc.BlockTime
	}
	threshold := depositWatchVanishThreshold(blockTime)
	// The own-deposit probe waits for the decisive round: only a proven
	// broadcast (rollbackGate) makes its verdict mean anything.
	proveSpend := s.ourDepositTxID != "" && n.rollbackGate(orderID)
	decisiveOwnProbe := proveSpend && s.vanishMisses+1 >= threshold
	ownConn := n.cfg().Connectors[s.srcCur]
	ourDepTxID := s.ourDepositTxID
	task := workTask{
		orderID: orderID,
		run: func() (any, error) {
			unspent, err := runDepositWatchTask(conn, dstCur, depTxID, depVout)
			if err != nil || unspent || !decisiveOwnProbe {
				if err != nil {
					return depositWatchOutcome{}, err
				}
				return depositWatchOutcome{unspent: unspent}, nil
			}
			known, spent := probeOwnDeposit(ownConn, ourDepTxID, ownDepositVout)
			return depositWatchOutcome{ownKnown: known, ownSpent: spent}, nil
		},
		apply: func(v any, err error) {
			delete(n.pendingWatch, orderID)
			s := n.sessions[orderID]
			if err != nil {
				// A blind backend is no evidence either way: it must not count
				// as a vanish, and patience must not run out on it.
				if s != nil {
					s.vanishMisses = 0
				}
				xlog.Debug("deposit watch probe unavailable, skipping round", "order", orderID, "err", err)
				return
			}
			out, _ := v.(depositWatchOutcome)
			if out.unspent {
				if s != nil {
					s.vanishMisses = 0
				}
				return
			}
			if s == nil {
				return
			}
			// A hunting session is owned by the own-deposit watch: cancelling
			// would broadcast a refund that can never confirm.
			if s.secretHunt {
				s.vanishMisses = 0
				return
			}
			// Re-check the enqueue-time guards: a claim or cancel may have raced
			// the probe, and cancelling then would kill a live claim or spam a Cancel.
			if s.await || s.claimHex != "" || s.claimTxID != "" {
				s.vanishMisses = 0
				return
			}
			if s.isMaker {
				if s.state < csCreatedA || s.state >= csConfirmedA {
					s.vanishMisses = 0
					return
				}
			} else if s.state < csCreatedB || s.state >= csConfirmedB {
				s.vanishMisses = 0
				return
			}
			if n.store != nil {
				if o := n.store.Get(orderID); o != nil && (isOrderTerminal(o.Status) || o.CounterpartyRedeemed ||
					o.Status == "rolled back" || o.Status == "rollback failed") {
					s.vanishMisses = 0
					return
				}
			}
			s.vanishMisses++
			if s.vanishMisses < threshold {
				xlog.Debug("deposit watch: vanish unconfirmed, awaiting durable absence",
					"order", orderID, "missing", s.vanishMisses, "threshold", threshold)
				return
			}
			s.vanishMisses = 0
			// Reset before acting: neither branch clears the counter, and the
			// countdown must not bank credit across a hunt arming or a cancel.
			if out.ownKnown && out.ownSpent && !s.isMaker {
				n.enterSecretHunt(s, orderID, s.srcCur)
				return
			}
			xlog.Warn("deposit watch: counterparty deposit gone, cancelling", "order", orderID,
				"deposit", s.theirDepositTxID, "vout", s.theirDepositVout)
			reason := crBadADepositTx // taker watches the maker A-deposit
			if s.isMaker {            // maker watches the taker B-deposit
				reason = crBadBDepositTx
			}
			s.sendSelfCancel(reason)
		},
	}
	if !n.engineRunning.Load() {
		v, err := safeTaskRun(task)
		task.apply(v, err)
		return true
	}
	select {
	case n.tasks <- task:
		return true
	default:
		delete(n.pendingWatch, orderID)
		xlog.Warn("deposit watch task dropped, engine busy", "order", orderID)
		return false
	}
}

func (n *Node) sessionIsTerminal(id string, s *SwapSession) bool {
	if s.state == csFinished {
		return true
	}
	if s.refundHex != "" && !s.refundDone && (s.state >= csCreatedA || n.orderDepositSent(id)) {
		return false
	}
	// Never prune a task in flight: it would orphan the broadcast and strand
	// the deposit whose pre-signed refund is about to be recorded.
	if s.await {
		return false
	}
	o := n.store.Get(id)
	return o == nil || isOrderTerminal(o.Status) || s.refundDone
}

func (n *Node) pruneSessions() {
	for id, s := range n.sessions {
		if n.sessionIsTerminal(id, s) {
			delete(n.pendingRefunds, id)
			n.clearRefundBackoff(id)
			n.clearRescanFailure(id)
			// Every watch guard, not just its own: a pruned session's in-flight
			// task cannot squat the id, and no sweep loops over the session to
			// clear it later. An in-flight task's apply handles the gone
			// session itself (dropGhostResult / sessionAlive).
			delete(n.pendingWatch, id)
			delete(n.pendingOwnWatch, id)
			delete(n.ownWatchSeen, id)
			delete(n.stallWarned, id)
			delete(n.sessions, id)
			xlog.Info("swap session pruned", "order", id, "state", s.state.String())
		}
	}
}

// enqueueRefund force-broadcasts an order's refund — the live session's
// pre-signed hex when present, else the stored hex — reporting through done when set.
func (n *Node) enqueueRefund(orderID string, done func(txid string, err error)) {
	if s := n.sessions[orderID]; s != nil && s.refundHex != "" {
		if s.state < csCreatedA && !n.orderDepositSent(orderID) {
			if done != nil {
				done("", fmt.Errorf("api: no deposit broadcast for order %s", orderID))
			}
			return
		}
		// Take the pendingRefunds guard so the sweep cannot double-enqueue this
		// hex; postRefundTask clears it on success and error, so a failure retries.
		if n.engineRunning.Load() {
			n.pendingRefunds[orderID] = true
		}
		// Locktime parity with the background sweep (scanRefunds): a
		// pre-locktime force-refund defers ("", nil) instead of broadcasting
		// a CLTV-locked spend the network must reject.
		n.postRefundTask(orderID, s.srcCur, s.refundHex, s.ourLockTime, true, done)
		return
	}
	o := n.store.Get(orderID)
	if o == nil || o.RefundTx == "" || !o.DepositSent {
		if done != nil {
			done("", fmt.Errorf("api: no refund available for order %s", orderID))
		}
		return
	}
	// Decode once for the refund's lockTime (the same bytes the stored sweep
	// reads in scanStoredRefunds); an undecodable hex is reported, never
	// broadcast blind against a candidate chain.
	raw, derr := hex.DecodeString(o.RefundTx)
	if derr != nil {
		xlog.Warn("stored refund bad hex, skipping force-refund", "order", orderID)
		if done != nil {
			done("", fmt.Errorf("api: stored refund for %s undecodable: %w", orderID, derr))
		}
		return
	}
	rtx, derr := coins.Deserialize(raw)
	if derr != nil {
		xlog.Warn("stored refund undeserializable, skipping force-refund", "order", orderID)
		if done != nil {
			done("", fmt.Errorf("api: stored refund for %s undeserializable: %w", orderID, derr))
		}
		return
	}
	// Own chain first (utxoCurrency tag, else the legacy Role rule): a
	// deferral on the right chain must stop here, never chain onto others.
	seen := map[string]bool{}
	cands := make([]string, 0, 3)
	for _, cur := range append([]string{utxoCurrency(o)}, o.FromCurrency, o.ToCurrency) {
		if cur != "" && !seen[cur] && n.cfg().Connectors[cur] != nil {
			seen[cur] = true
			cands = append(cands, cur)
		}
	}
	n.tryStoredRefund(orderID, o.RefundTx, rtx.LockTime, cands, done)
}

// tryStoredRefund broadcasts the stored refund against each candidate currency
// in order, stopping at the first success or deferral; the callbacks chain so
// no currency is broadcast twice. A locktime deferral (nil error, empty txid)
// and a spent-deposit verdict both stop the chain: the former must not touch
// other chains, the latter is chain-independent.
func (n *Node) tryStoredRefund(orderID, refundHex string, lockTime uint32, cands []string, done func(txid string, err error)) {
	if len(cands) == 0 {
		if done != nil {
			done("", fmt.Errorf("api: could not broadcast stored refund for %s", orderID))
		}
		return
	}
	cur := cands[0]
	n.postRefundTask(orderID, cur, refundHex, lockTime, true, func(txid string, err error) {
		// Deferral (err == nil, txid == "") and success both stop here:
		// chaining onto the next currency would broadcast the same bytes
		// against the wrong chain. Only a genuine error chains.
		if err == nil {
			if done != nil {
				done(txid, nil)
			}
			return
		}
		// A spent verdict is chain-independent: our deposit is gone on every
		// chain, so stop chaining and surface the typed cause.
		if errors.Is(err, errRefundDepositSpent) {
			if done == nil {
				n.recordRefundFailure(orderID)
			} else {
				done("", err)
			}
			return
		}
		n.tryStoredRefund(orderID, refundHex, lockTime, cands[1:], done)
	})
}

// BroadcastRefund force-broadcasts an order's pre-signed CLTV refund without waiting for the background sweep, returning the accepted txid.
// Pre-locktime it defers like the sweep: ("", nil) means not yet due (retry
// past the deposit lockTime), never a failure and never a broadcast.
func (n *Node) BroadcastRefund(orderID string) (string, error) {
	if !n.engineRunning.Load() {
		var txid string
		var rerr error
		n.enqueueRefund(orderID, func(t string, e error) { txid, rerr = t, e })
		return txid, rerr
	}
	type outcome struct {
		txid string
		err  error
	}
	out := make(chan outcome, 1)
	n.submit(func() {
		n.enqueueRefund(orderID, func(t string, e error) { out <- outcome{t, e} })
	}, false)
	select {
	case o := <-out:
		return o.txid, o.err
	case <-n.stop:
		return "", fmt.Errorf("api: node closed during refund broadcast for %s", orderID)
	}
}

func (c swapCtx) computeLockTimeFor(cur string, isMaker bool) uint32 {
	conn := c.connectors[cur]
	cc := c.conf(cur)
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

func (c *swapCtx) computeLockTime(isMaker bool) uint32 {
	return c.computeLockTimeFor(c.srcCur, isMaker)
}

// buildDeposit builds and signs the HTLC deposit and pre-signs its CLTV refund
// without broadcasting; the engine persists that intent before any broadcast.
func (c *swapCtx) buildDeposit(isMaker bool) (depositOutcome, error) {
	cur := c.srcCur
	amt := c.srcAmt
	conn := c.connectors[cur]
	cc := c.conf(cur)
	if conn == nil {
		return depositOutcome{}, fmt.Errorf("api: no connector for %s", cur)
	}
	if cc == nil {
		// Every connector was built from a conf in the same snapshot, so a
		// missing one is unsafe: the deposit's dust gate would silently vanish.
		return depositOutcome{}, fmt.Errorf("api: no conf for %s", cur)
	}
	coin, ok := c.coin(cur)
	if !ok {
		return depositOutcome{}, fmt.Errorf("api: unknown coin %s", cur)
	}
	// Spend the funding UTXOs recorded on the order, never a fresh ListUnspent.
	funding := c.funding
	if len(funding) == 0 {
		return depositOutcome{}, fmt.Errorf("api: no funding UTXOs for %s (order has no used coins)", cur)
	}
	// A zero lockTime makes the HTLC immediately refundable, so fail closed
	// instead of building a deposit the counterparty's drift check must reject.
	lockTime := c.computeLockTime(isMaker)
	if lockTime == 0 {
		return depositOutcome{}, fmt.Errorf("api: locktime unavailable for %s (getblockcount failed)", cur)
	}
	// Change returns to the largest funding UTXO's address (a wallet-watched
	// address); GetNewAddress is only the fallback when that one cannot be decoded.
	change := [20]byte{}
	largest := funding[0]
	for _, u := range funding[1:] {
		if u.Amount > largest.Amount {
			largest = u
		}
	}
	if a, derr := coin.DecodeAddress(largest.Address); derr == nil {
		copy(change[:], a.Hash)
	} else {
		changeStr, err := conn.GetNewAddress()
		if err != nil {
			return depositOutcome{}, err
		}
		if a, derr := coin.DecodeAddress(changeStr); derr != nil {
			return depositOutcome{}, derr
		} else {
			copy(change[:], a.Hash)
		}
	}
	fee := estimateFee(cc, len(funding), 3)
	fee2 := estimateFee(cc, 1, 1)
	xlog.Debug("buildDeposit: plan", "order", c.orderID, "isMaker", isMaker,
		"cur", cur, "amountXB", amt, "lockTime", lockTime, "txVersion", c.txVersion(cur),
		"utxos", len(funding), "fee", fee, "fee2", fee2)

	hash := c.secretHash
	if !isMaker {
		hash = c.theirSecretHash
	}
	// The deposit locks native base units: convert the XBridge 1e6 amount at
	// this boundary so the scales never mix (a no-op for COIN=1e6 coins).
	nativeAmt := fromXBridgeAmt(coin, amt)
	spec := &swap.DepositSpec{
		Currency:        cur,
		Amount:          nativeAmt,
		DepositorPub:    c.pubKey,
		CounterpartyPub: c.theirPub,
		Hash:            hash,
		LockTime:        lockTime,
		TxVersion:       c.txVersion(cur),
	}
	tx, err := spec.BuildDepositTx(coin, funding, change, fee, fee2, depositDustLimit(cc))
	if err != nil {
		return depositOutcome{}, err
	}
	if len(tx.Outputs) == 0 {
		return depositOutcome{}, fmt.Errorf("api: deposit tx has no outputs for %s", cur)
	}
	c.ourDepositP2SH = tx.Outputs[0].Value
	xlog.Debug("buildDeposit: unsigned tx built", "order", c.orderID, "txVersion", tx.Version, "outputs", len(tx.Outputs))
	prevTxs := make([]wallet.PrevTx, 0, len(funding))
	for _, u := range funding {
		prevTxs = append(prevTxs, wallet.PrevTx{TxID: u.TxID, Vout: u.Vout, ScriptPubKey: u.ScriptPubKey, Amount: u.Amount})
	}
	unsigned := hex.EncodeToString(tx.Serialize())
	signed, complete, serr := conn.SignRawTransaction(unsigned, prevTxs)
	if serr != nil {
		return depositOutcome{}, serr
	}
	if !complete {
		return depositOutcome{}, fmt.Errorf("api: deposit signing incomplete for %s", cur)
	}
	// The txid is derived locally here and the CLTV refund is pre-built before
	// any broadcast: a refund-build failure must never strand a live deposit.
	localTxID, err := txIDFromHex(signed)
	if err != nil {
		return depositOutcome{}, err
	}
	c.ourDepositTxID = localTxID
	c.ourLockTime = lockTime
	refundHex, err := c.buildRefundTx(spec, cur)
	if err != nil {
		return depositOutcome{}, err
	}
	c.refundHex = refundHex

	return depositOutcome{txid: localTxID, lockTime: lockTime, refundHex: refundHex, depositHex: signed, conn: conn}, nil
}

func (c *swapCtx) buildRefundTx(spec *swap.DepositSpec, cur string) (string, error) {
	coin, ok := c.coin(cur)
	if !ok {
		return "", fmt.Errorf("api: unknown coin %s", cur)
	}
	dest, err := c.destScript(cur, c.ourSourceAddr)
	if err != nil {
		return "", err
	}
	depositP2SH := c.ourDepositP2SH
	if depositP2SH == 0 {
		return "", fmt.Errorf("api: no recorded deposit P2SH value for %s", cur)
	}
	h, err := reverseTxidHex(c.ourDepositTxID)
	if err != nil {
		return "", err
	}
	tx := &coins.Tx{Version: int32(c.txVersion(cur)), LockTime: spec.LockTime}
	// Per-coin serializeWithTimeField: stamp nTime after nVersion so the refund
	// matches the counterparty's XBridge connector wire layout.
	if cc := c.conf(cur); cc != nil && cc.TxWithTimeField {
		tx.WithTime = true
		tx.TxTime = uint32(time.Now().Unix())
	}
	xlog.Debug("buildRefundTx: plan", "order", c.orderID, "cur", cur,
		"deposit", c.ourDepositTxID, "lockTime", spec.LockTime, "amount", spec.Amount, "depositP2SH", depositP2SH, "txVersion", c.txVersion(cur))
	tx.Inputs = append(tx.Inputs, coins.TxIn{
		PrevOut:  coins.OutPoint{Hash: h, Index: 0},
		Sequence: 0xfffffffe, // enable CLTV
	})
	// The refund pays the full nominal amount: the deposit output carries
	// amount+fee2, so fee2 is this transaction's miner fee.
	tx.Outputs = append(tx.Outputs, coins.TxOut{Value: spec.Amount, ScriptPubKey: dest})

	inner := spec.RedeemScript()
	sig, err := coins.SignTxInputForCoin(tx, 0, inner, depositP2SH, c.privKey[:], coin)
	if err != nil {
		return "", err
	}
	tx.Inputs[0].ScriptSig = coins.BuildRefundScriptSig(sig, spec.DepositorPub[:], inner)
	return hex.EncodeToString(tx.Serialize()), nil
}

// confirmDepositKnownByRawTx is the claim pre-check fallback for backends whose
// gettxout is blind outside their own wallet (code -5): known only when the raw tx carries the exact validated vout at depth.
func confirmDepositKnownByRawTx(conn wallet.Connector, txid string, vout uint32, scriptHex string, value uint64, minConf int) bool {
	vtx, verr := conn.GetRawTransactionVerbose(txid)
	if verr != nil {
		return false
	}
	// At minConf > 0 the depth must be asserted; at 0-conf a served exact vout
	// is the mempool-presence proof, while a negative depth still rejects.
	if minConf > 0 {
		if !vtx.HasConfirmations {
			return false
		}
		if vtx.Confirmations < minConf {
			return false
		}
	} else if vtx.HasConfirmations && vtx.Confirmations < 0 {
		return false
	}
	out, ok := vtx.Outputs[vout]
	if !ok || out.Value != value || out.ScriptHex != scriptHex {
		return false
	}
	return true
}

// redeemCounterparty claims the deposit we receive: the maker reveals the secret
// taking the taker's deposit, the taker uses it taking the maker's.
func (c *swapCtx) redeemCounterparty(isMaker bool) (payHex, depositCur string, err error) {
	depositCoin, ok := c.coin(c.dstCur)
	if !ok {
		return "", "", fmt.Errorf("api: unknown coin %s", c.dstCur)
	}
	theirSpec := swap.DepositSpec{
		Currency:        c.dstCur,
		Amount:          fromXBridgeAmt(depositCoin, c.dstAmt),
		DepositorPub:    c.theirPub,
		CounterpartyPub: c.pubKey,
		LockTime:        c.theirLockTime,
	}
	if c.isMaker {
		theirSpec.Hash = c.secretHash
	} else {
		theirSpec.Hash = c.theirSecretHash
	}
	depositCur = c.dstCur

	dest, err := c.destScript(depositCur, c.ourDestAddr)
	if err != nil {
		return "", "", err
	}
	fee := estimateFee(c.conf(depositCur), 1, 1)
	// Spend the validated counterparty deposit's exact value minus fee2, not
	// the nominal amount: P2SHNative is exact, so the excess is what the redeemer keeps.
	p2shNative := c.theirP2SHNative
	if p2shNative == 0 {
		return "", "", fmt.Errorf("api: counterparty deposit not validated (no p2sh amount)")
	}
	if fee >= p2shNative {
		return "", "", fmt.Errorf("api: counterparty deposit amount too small for claim fee")
	}
	// Re-check the deposit is unspent immediately before building the claim:
	// a point-in-time validation would admit a reorged or double-spent deposit.
	if conn, ok := c.connectors[depositCur]; ok && conn != nil {
		_, unspent, gerr := conn.GetTxOut(c.theirDepositTxID, c.theirDepositVout)
		if gerr != nil {
			// A fetch error after visibility was proven is backend flakiness:
			// fast-poll it instead of burning the failure backoff.
			degraded := false
			if code, isRPC := wallet.RPCErrorCode(gerr); isRPC && code == -5 {
				degraded = confirmDepositKnownByRawTx(conn, c.theirDepositTxID, c.theirDepositVout,
					hex.EncodeToString(theirSpec.P2SHScript()), p2shNative, c.minConf(c.conf(depositCur)))
			}
			if !degraded {
				xlog.Debug("redeemCounterparty: deposit unspent check unavailable (transient)", "order", c.orderID, "err", gerr)
				// Dual-%w keeps the RPC cause introspectable while
				// scheduleNotReady still finds the not-ready sentinel.
				return "", "", fmt.Errorf("api: counterparty deposit %s:%d unspent check failed: %w: %w",
					c.theirDepositTxID, c.theirDepositVout, gerr, wallet.ErrDepositNotReady)
			}
			xlog.Info("redeemCounterparty: gettxout-blind backend, proceeding on verbose raw-tx match", "order", c.orderID, "deposit", c.theirDepositTxID, "vout", c.theirDepositVout)
		} else if !unspent {
			// Spent proof (ok=false, nil) is reorg or double-spend, never index
			// lag: it stays on the failure backoff, not the fast re-poll.
			xlog.Debug("redeemCounterparty: counterparty deposit spent or missing (reorg?)", "order", c.orderID, "deposit", c.theirDepositTxID, "vout", c.theirDepositVout)
			return "", "", fmt.Errorf("api: counterparty deposit %s:%d not unspent, awaiting redelivery", c.theirDepositTxID, c.theirDepositVout)
		}
	}
	h, err := reverseTxidHex(c.theirDepositTxID)
	if err != nil {
		return "", "", err
	}
	tx := &coins.Tx{Version: int32(c.txVersion(depositCur)), LockTime: 0}
	if cc := c.conf(depositCur); cc != nil && cc.TxWithTimeField {
		tx.WithTime = true
		tx.TxTime = uint32(time.Now().Unix())
	}
	xlog.Debug("redeemCounterparty: plan", "order", c.orderID, "isMaker", isMaker,
		"depositCur", depositCur, "deposit", c.theirDepositTxID, "vout", c.theirDepositVout,
		"nominal", theirSpec.Amount, "p2sh", p2shNative, "fee", fee, "txVersion", c.txVersion(depositCur))
	tx.Inputs = append(tx.Inputs, coins.TxIn{
		PrevOut:  coins.OutPoint{Hash: h, Index: c.theirDepositVout},
		Sequence: 0xffffffff,
	})
	// C++ redeem rule (redeemOrderCounterpartyDeposit / xBridgeExcessSats):
	// pay the full order amount whenever the deposit does not strictly cover
	// amount + fee2; only a strict excess is added on top. In-band deposits
	// (legacy truncated escrows inside the 5% acceptance band) therefore pay
	// the redeemer whole instead of short-changing by the fee gap.
	payout := theirSpec.Amount
	if p2shNative > theirSpec.Amount+fee {
		payout = p2shNative - fee
	}
	tx.Outputs = append(tx.Outputs, coins.TxOut{Value: payout, ScriptPubKey: dest})

	inner := theirSpec.RedeemScript()
	sig, err := coins.SignTxInputForCoin(tx, 0, inner, p2shNative, c.privKey[:], depositCoin)
	if err != nil {
		return "", "", err
	}
	tx.Inputs[0].ScriptSig = coins.BuildPaymentScriptSig(c.secret[:], sig, c.pubKey[:], inner)
	return hex.EncodeToString(tx.Serialize()), depositCur, nil
}

func (c *swapCtx) destScript(cur, addrStr string) ([]byte, error) {
	coin, ok := c.coin(cur)
	if !ok {
		return nil, fmt.Errorf("api: unknown coin %s", cur)
	}
	a, err := coin.DecodeAddress(addrStr)
	if err != nil {
		return nil, err
	}
	var h [20]byte
	copy(h[:], a.Hash)
	return coins.BuildP2PKHScript(h), nil
}

func (c *swapCtx) connector(t string) (wallet.Connector, error) {
	conn := c.connectors[t]
	if conn == nil {
		return nil, fmt.Errorf("dx: no wallet configured for %s", t)
	}
	return conn, nil
}

func (c *swapCtx) conf(cur string) *config.CoinConf {
	if c.confs != nil {
		return c.confs[cur]
	}
	return nil
}

func (c *swapCtx) coin(cur string) (coins.Coin, bool) {
	if c.coinsMap == nil {
		return coins.Coin{}, false
	}
	coin, ok := c.coinsMap[cur]
	return coin, ok
}

func (c *swapCtx) txVersion(cur string) int {
	cc := c.conf(cur)
	if cc == nil || cc.TxVersion <= 0 {
		return 1
	}
	return cc.TxVersion
}

func (c *swapCtx) minConf(cc *config.CoinConf) int {
	if cc == nil {
		return 0
	}
	return cc.Confirmations
}

func depositDustLimit(cc *config.CoinConf) uint64 {
	return effectiveDust(cc)
}

// ownDepositVout is the output index of our own deposit's P2SH HTLC output.
// BuildDepositTx always emits it as output 0 (change, if any, follows).
const ownDepositVout = 0

// secretFromPayTx extracts the preimage only from an input spending the expected
// outpoint whose push hashes to hx, so a decoy payTx carrying the public secret never qualifies.
func secretFromPayTx(payHex string, hx [20]byte, hasTime bool, depTxID string, depVout uint32) ([33]byte, bool) {
	raw, err := hex.DecodeString(payHex)
	if err != nil {
		xlog.Debug("secretFromPayTx: bad hex", "err", err)
		return [33]byte{}, false
	}
	tx, err := coins.DeserializeWithTime(raw, hasTime)
	if err != nil {
		// tx is nil on this path: logging its fields would dereference nil.
		xlog.Debug("secretFromPayTx: cannot deserialize", "err", err)
		return [33]byte{}, false
	}
	if len(tx.Inputs) == 0 {
		xlog.Debug("secretFromPayTx: no inputs")
		return [33]byte{}, false
	}
	var depHash [32]byte
	if b, derr := hex.DecodeString(depTxID); derr != nil || len(b) != 32 {
		xlog.Debug("secretFromPayTx: bad deposit txid", "txid", depTxID)
		return [33]byte{}, false
	} else {
		for i := 0; i < 32; i++ {
			depHash[i] = b[31-i] // display order -> internal little-endian
		}
	}
	for _, in := range tx.Inputs {
		if in.PrevOut.Hash != depHash || in.PrevOut.Index != depVout {
			continue
		}
		if secret, ok := secretFromScriptSig(in.ScriptSig, hx); ok {
			return secret, true
		}
	}
	xlog.Debug("secretFromPayTx", "ok", false)
	return [33]byte{}, false
}

// secretFromScriptSig returns the 33-byte push whose HASH160 equals hx — never
// the first 33-byte element — so a malleated scriptSig cannot yield the wrong secret.
func secretFromScriptSig(script []byte, hx [20]byte) ([33]byte, bool) {
	i := 0
	var secret [33]byte
	for i < len(script) {
		op := script[i]
		i++
		var n int
		switch {
		case op <= 0x4b:
			n = int(op)
		case op == 0x4c:
			if i >= len(script) {
				return secret, false
			}
			n = int(script[i])
			i++
		case op == 0x4d:
			if i+2 > len(script) {
				return secret, false
			}
			n = int(script[i]) | int(script[i+1])<<8
			i += 2
		default:
			continue
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

func decodePub33(s string) [33]byte {
	var out [33]byte
	if s == "" {
		return out
	}
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 33 {
		return [33]byte{}
	}
	copy(out[:], b)
	return out
}

func txIDFromBytes(raw []byte) string {
	h1 := sha256.Sum256(raw)
	h2 := sha256.Sum256(h1[:])
	out := make([]byte, 32)
	for i := 0; i < 32; i++ {
		out[i] = h2[31-i]
	}
	return hex.EncodeToString(out)
}

func txIDFromHex(rawHex string) (string, error) {
	raw, err := hex.DecodeString(rawHex)
	if err != nil {
		return "", err
	}
	return txIDFromBytes(raw), nil
}
