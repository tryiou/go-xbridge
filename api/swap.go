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

// TxCancelReason values used by this branch's wire-Cancel paths. Only the B3
// subset is defined here; the full enum (crUnknown..crBadFeeTx,
// xbridgepacket.h:21-48) is STATE-F73's (B8) concern.
const (
	crBadADepositTx uint32 = 14
	crBadBDepositTx uint32 = 15
	crBadALockTime  uint32 = 18
	crBadBLockTime  uint32 = 19
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
	privKey    [32]byte // per-trade M keypair (C++ mPrivKey): signs packets + HTLC
	pubKey     [33]byte // per-trade M pubkey (C++ mPubKey): packet header + HTLC DepositorPub
	secret     [33]byte // maker: xPubKey preimage; taker: recovered from A's payTx
	secretHash [20]byte // HASH160(secret); both deposits share it

	ourLockTime    uint32
	ourDepositTxID string
	refundHex      string // pre-signed IF-branch refund, for cancel/expiry
	refundDone     bool   // guard so the watcher broadcasts the refund at most once

	theirDepositTxID string // counterparty's deposit txid (from ConfirmA/CreateB)
	theirLockTime    uint32
	theirSecretHash  [20]byte

	// Validated counterparty deposit (CRYPTO-F90): the C++
	// checkDepositTransaction out-params recorded when we accept the
	// counterparty's deposit (CreateB for the taker's A-check, ConfirmA for the
	// maker's B-check). P2SHNative is the exact matched output value in the
	// coin's native base (the claim spend); Overpayment is XBridge 1e6 base.
	theirDepositVout uint32
	theirP2SHNative  uint64
	theirOverpayment uint64

	hub    [20]byte // service-node address, pinned at session creation (maker: chosen at MakeOrder; taker: order's HubAddress)
	hubKey [33]byte // trusted hub service-node pubkey (C++ xtx->sPubKey): pinned at creation for BOTH roles (maker: the SN chosen at make; taker: order's SNodePubkey); every hub handshake packet is re-verified against it (STATE-F78)
	state  clientState

	// await is true while a two-phase handshake task (deposit/claim) for this
	// session is in flight: set by the staged handler's stage 1, cleared by its
	// resume on success AND error. The hub sends the next packet only after our
	// response, so any packet arriving during await is a retransmit and is
	// dropped by processSwap. Engine-owned; never read off the engine goroutine.
	await bool
}

// depositOutcome is the worker-produced result of a deposit build+broadcast
// (CreateA/CreateB).
type depositOutcome struct {
	txid      string
	lockTime  uint32
	refundHex string
}

// confirmOutcome is the worker-produced result of a claim (ConfirmA/B). secret
// is the recovered HTLC preimage (ConfirmB only; zero for ConfirmA). check
// carries the validated counterparty deposit (ConfirmA's F85 check; zero for
// ConfirmB, whose check ran at CreateB).
type confirmOutcome struct {
	secret  [33]byte
	payTxID string
	check   wallet.DepositCheck
}

// selfCancelErr marks a worker failure that must broadcast a Cancel packet with
// the given TxCancelReason and roll back locally — C++ sendCancelTransaction
// (crBadADepositTx/crBadBDepositTx/crBadALockTime/crBadBLockTime) + the
// immediately-following processTransactionCancel. The resume (engine side)
// recognizes it via errors.As and calls SwapSession.sendSelfCancel.
type selfCancelErr struct {
	reason uint32
}

func (e *selfCancelErr) Error() string {
	return fmt.Sprintf("api: self-cancel (TxCancelReason %d)", e.reason)
}

// sendSelfCancel broadcasts a signed xbcTransactionCancel for OUR OWN rejection
// of the counterparty's deposit/locktime and rolls back locally, mirroring C++
// sendCancelTransaction + processTransactionCancel + sendPacketBroadcast
// (xbridgesession.cpp:3525-3576). Must run on the engine goroutine (it mutates
// the store/order): the callers are the two-phase resumes, which execute on the
// engine. The packet is signed with the session's per-trade M key; since
// Order.MakerKey is OUR M pubkey (order.go:96-99), handleRemoteCancel's
// iCanceled check accepts it and performs the state transition (cancel if no
// deposit sent, refund-broadcast rollback otherwise).
func (s *SwapSession) sendSelfCancel(reason uint32) {
	orderID := hexEncode(s.id[:])
	if s.n == nil || s.n.conn == nil {
		xlog.Error("selfCancel: no network connector", "order", orderID, "reason", reason)
		return
	}
	body := &proto.CancelBody{ID: s.id, Reason: reason}
	pkt := proto.NewPacket(proto.XbcTransactionCancel, body.Marshal())
	if err := s.n.signer.Sign(pkt, s.privKey[:]); err != nil {
		xlog.Error("selfCancel: sign failed", "order", orderID, "err", err)
		return
	}
	xlog.Warn("selfCancel: counterparty deposit rejected", "order", orderID, "reason", reason)
	// Local rollback first (C++ processTransactionCancel(reply)), then broadcast.
	s.n.handleRemoteCancel(pkt, body)
	if err := s.n.conn.WritePacket(pkt, [20]byte{}); err != nil {
		xlog.Error("selfCancel: broadcast failed", "order", orderID, "err", err)
	}
}

// failSelfCancel recognizes a selfCancelErr returned by a worker task and
// performs the wire-Cancel + local rollback. It returns true when handled.
// Engine-side (called from the resumes).
func (s *SwapSession) failSelfCancel(terr error) bool {
	var sce *selfCancelErr
	if !errors.As(terr, &sce) {
		return false
	}
	s.sendSelfCancel(sce.reason)
	return true
}

// createdBOutcome is the worker result of the taker's CreateB task: the built
// deposit plus the validated counterparty A-deposit (CRYPTO-F90 — the resume
// records DepositVout/P2SHAmount/Excess on the session for the F90 redeem).
type createdBOutcome struct {
	out   depositOutcome
	check wallet.DepositCheck
}

// counterpartyDepositScriptHex returns the P2SH output script the counterparty's
// deposit must carry: OP_HASH160 <HASH160(unlockScript)> OP_EQUAL, where the
// unlock script is createDepositUnlockScript(DepositorPub=theirPub,
// CounterpartyPub=ourKey, hash, opponentLockTime) (xbridgesession.cpp:2484,
// 2946). For the taker checking A, hash = the maker's HashedSecret
// (theirSecretHash); for the maker checking B, hash = OUR secretHash (both
// deposits share it).
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

// checkCounterpartyDeposit validates the counterparty's deposit against the
// expected p2sh script and amount before we commit our own deposit or redeem
// theirs (CRYPTO-F85 — C++ checkDepositTransaction call sites
// xbridgesession.cpp:2495/2957). expectedAmount is XBridge 1e6 base. Tri-state:
// ErrDepositNotReady and ErrNoChainSource are returned as errors (the caller
// sends NO response — C++ processLater / the connector cannot judge); a
// returned DepositCheck with IsGood=false is a definitively bad deposit (the
// caller must wire-Cancel, crBadADepositTx/crBadBDepositTx).
func (c *swapCtx) checkCounterpartyDeposit(hash [20]byte, expectedAmount uint64) (wallet.DepositCheck, error) {
	conn := c.n.cfg().Connectors[c.dstCur]
	if conn == nil {
		return wallet.DepositCheck{}, fmt.Errorf("api: no connector for %s", c.dstCur)
	}
	return conn.CheckDepositTransaction(c.theirDepositTxID, c.counterpartyDepositScriptHex(hash), expectedAmount, c.minConf(c.conf(c.dstCur)))
}

// counterpartyCoin returns the coin of the counterparty's deposit currency
// (dstCur). The order's OBinTxP2SHAmount record is XBridge base (C++
// oBinTxP2SHAmount = whole × COIN), derived from the exact native value.
func counterpartyCoin(s *SwapSession) coins.Coin {
	c, _ := coins.Get(s.dstCur)
	return c
}

// swapCtx is an immutable engine-side snapshot of a SwapSession, taken at
// stage-1 enqueue time. The two-phase handshake workers run the wallet-I/O
// builders against THIS value (plus the node's lock-guarded config via c.n) and
// never touch a live session, which the engine owns exclusively — a worker
// reading a session field would race the engine's resume writes.
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

	// funding is the deposit's exact funding set — the make/take-time selection
	// recorded as Order.UsedCoins (CRYPTO-F87, C++ xtx->usedCoins). buildDeposit
	// spends exactly this, never a fresh ListUnspent.
	funding []wallet.Utxo
}

// snapshot copies the session's fields a worker task needs. It runs on the
// engine goroutine (stage 1), so reading live session state here is safe.
func (s *SwapSession) snapshot() swapCtx {
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
		funding:          s.n.orderFunding(s.id),
	}
}

// orderFunding returns the order's recorded funding set (Order.UsedCoins) for
// the deposit to spend — C++ xtx->usedCoins, populated at make/take time
// (CRYPTO-F87). store.Get returns a deep copy, so the worker can hold this
// without racing the engine.
func (n *Node) orderFunding(id [32]byte) []wallet.Utxo {
	o := n.store.Get(hexEncode(id[:]))
	if o == nil {
		return nil
	}
	return o.UsedCoins
}

// newMakerSession registers the client-side maker for a freshly created order and
// generates the 33-byte HTLC secret (xPubKey); HASH160(xPubKey) is the secretHash
// carried in the deposit. Both deposits use the same secretHash.
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
	}
	// STATE-F78: the maker's trusted hub key is the servicenode chosen at make
	// time (C++ xtx->sPubKey = findNodeWithService result). It is pinned HERE,
	// at session creation — never learned from network packets — so every hub
	// handshake packet (Hold/Init/CreateA/B/ConfirmA/B/Finished) is re-verified
	// against it; a forged Finished can never disable the refund watcher.
	s.hub = o.HubAddress
	s.hubKey = decodePub33(o.SNodePubkey)
	n.sessions[hexEncode(o.ID[:])] = s
	xlog.Info("swap session created", "order", hexEncode(o.ID[:]), "role", "maker",
		"srcCur", o.FromCurrency, "srcAmt", o.FromAmount, "dstCur", o.ToCurrency, "dstAmt", o.ToAmount)
}

// newTakerSession registers the client-side taker for a taken order. The secret
// is unknown until CreateB teaches us the secretHash; the actual preimage is
// recovered from the maker's payTx at ConfirmB.
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
	}
	// STATE-F78: the taker's trusted hub key is the servicenode that broadcast
	// the order (C++ xtx->sPubKey = the SN whose header signed the order). It is
	// pinned HERE, at session creation, so every hub handshake packet
	// (Hold/Init/CreateA/B/ConfirmA/B/Finished) is re-verified against it — a
	// forged Finished can never disable the refund watcher. An order without a
	// known SNodePubkey cannot be authenticated and stays unpinned, causing
	// dispatchSwap to drop all hub packets for it.
	s.hubKey = decodePub33(o.SNodePubkey)
	s.hub = o.HubAddress
	n.sessions[hexEncode(o.ID[:])] = s
	xlog.Info("swap session created", "order", hexEncode(o.ID[:]), "role", "taker",
		"srcCur", o.ToCurrency, "srcAmt", o.ToAmount, "dstCur", o.FromCurrency, "dstAmt", o.FromAmount,
		"hubKeyPinned", s.hubKey != [33]byte{})
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
//
// Two-phase: stage 1 (engine) validates and records the counterparty key; the
// worker builds+broadcasts the deposit (all wallet I/O); the resume applies the
// outcome to the session/order, sends the CreatedA response, and persists.
func (s *SwapSession) OnCreateA(b *proto.CreateABody) (proto.XBridgeCommand, responseBody, error) {
	orderID := hexEncode(s.id[:])
	if !s.isMaker {
		return 0, nil, fmt.Errorf("api: CreateA received by taker session %s", orderID)
	}
	// STATE-F77: C++ processTransactionCreateA drops a CreateA once the transaction
	// already reached trCreated (xbridgesession.cpp:1947). A retransmit arriving
	// AFTER our deposit completed (await is cleared) must not re-broadcast a
	// second deposit.
	if s.state >= csCreatedA {
		xlog.Info("CreateA ignored: swap already past deposit", "order", orderID, "state", s.state.String())
		return 0, nil, nil
	}
	if b.BPubKey == [33]byte{} {
		return 0, nil, fmt.Errorf("api: CreateA missing B pubkey")
	}
	s.theirPub = b.BPubKey
	xlog.Info("CreateA: building deposit A", "order", orderID, "counterparty", hexEncode(b.BPubKey[:]))
	// Record the counterparty (taker) M pubkey on the order (C++ oPubKey).
	s.n.store.Update(orderID, func(o *Order) {
		o.OtherPubkey = hexEncode(b.BPubKey[:])
	})
	c := s.snapshot()
	task := workTask{
		orderID: orderID,
		run: func() (any, error) {
			return c.buildDeposit(true)
		},
		apply: func(v any, terr error) { s.applyCreatedA(v, terr) },
	}
	if !s.n.engineRunning.Load() {
		v, terr := safeTaskRun(task)
		if terr != nil {
			// Mirror the started-mode resume: the apply always runs, so a
			// selfCancelErr (bad counterparty deposit/locktime) still broadcasts
			// the Cancel and rolls back.
			s.applyCreatedA(nil, terr)
			return 0, nil, terr
		}
		return proto.XbcTransactionCreatedA, s.applyCreatedA(v, nil), nil
	}
	s.await = true
	s.n.postSwapTask(orderID, task)
	return 0, nil, nil // deferred; applyCreatedA sends on worker completion
}

// applyCreatedA is the engine-side resume for a CreateA deposit task: it applies
// the outcome to the session and order, persists, and sends the CreatedA
// response (started mode) or returns the response body (inline mode). It always
// clears the session's await guard, so an errored task leaves the swap resumable
// (the hub resends).
func (s *SwapSession) applyCreatedA(v any, terr error) responseBody {
	orderID := hexEncode(s.id[:])
	s.await = false
	if terr != nil {
		xlog.Error("CreateA deposit task failed", "order", orderID, "err", terr)
		return nil
	}
	out := v.(depositOutcome)
	s.ourDepositTxID = out.txid
	s.ourLockTime = out.lockTime
	s.refundHex = out.refundHex
	s.n.store.Update(orderID, func(o *Order) {
		o.BinTxId = out.txid
		o.DepositSent = true
	})
	s.n.store.Update(orderID, func(o *Order) {
		o.RefundTx = out.refundHex
	})
	s.state = csCreatedA
	xlog.Info("deposit A broadcast", "order", orderID, "txid", out.txid,
		"lockTime", out.lockTime, "secretHash", hexEncode(s.secretHash[:]))
	xlog.Debug("deposit A refund pre-signed", "order", orderID)
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

// OnCreateB (hub→taker) → CreatedB (13): learn maker's deposit + build ours.
//
// Two-phase: stage 1 (engine) records the counterparty deposit; the worker
// drift-checks its lockTime (GetBlockCount) then builds+broadcasts ours; the
// resume applies the outcome, sends the CreatedB response, and persists.
func (s *SwapSession) OnCreateB(b *proto.CreateBBody) (proto.XBridgeCommand, responseBody, error) {
	orderID := hexEncode(s.id[:])
	if s.isMaker {
		return 0, nil, fmt.Errorf("api: CreateB received by maker session %s", orderID)
	}
	// STATE-F77: C++ processTransactionCreateB drops a CreateB once the transaction
	// already reached trCreated (xbridgesession.cpp:2424). A post-completion
	// retransmit must not re-broadcast a second deposit.
	if s.state >= csCreatedB {
		xlog.Info("CreateB ignored: swap already past deposit", "order", orderID, "state", s.state.String())
		return 0, nil, nil
	}
	if b.APubKey == [33]byte{} {
		return 0, nil, fmt.Errorf("api: CreateB missing A pubkey")
	}
	s.theirPub = b.APubKey
	s.theirDepositTxID = b.ADepositTxID
	// Record the counterparty (maker) deposit txid on the order so
	// dxPartialOrderChainDetails can emit p2sh_deposits_counterparty.
	s.n.store.Update(orderID, func(o *Order) {
		o.OBinTxId = b.ADepositTxID
		// Record the counterparty (maker) M pubkey on the order (C++ oPubKey).
		o.OtherPubkey = hexEncode(b.APubKey[:])
	})
	s.theirSecretHash = b.HashedSecret
	s.theirLockTime = b.ALockTime
	xlog.Info("CreateB: learned maker deposit", "order", orderID,
		"makerDeposit", b.ADepositTxID, "makerLockTime", b.ALockTime, "secretHash", hexEncode(b.HashedSecret[:]))
	c := s.snapshot()
	task := workTask{
		orderID: orderID,
		run: func() (any, error) {
			// Validate the counterparty (maker) A-deposit lockTime BEFORE we
			// broadcast our own deposit (mirrors C++ xbridgesession.cpp:2464
			// bad-locktime cancel). Compare the received value against our own
			// expectation for that same deposit role on the maker's coin
			// (dstCur) — NOT against our B lockTime, which legitimately differs
			// by ~90 blocks.
			expLT := c.computeLockTimeFor(c.dstCur, true)
			if !acceptableLockTimeDrift(expLT, b.ALockTime, c.blockTimeFor(c.dstCur)) {
				// C++ :2464-2472 — bad counterparty locktime → wire-Cancel.
				return nil, &selfCancelErr{reason: crBadALockTime}
			}
			// CRYPTO-F85: validate the maker's A deposit BEFORE committing ours
			// (C++ :2495). Wait → no response (hub retransmits); bad → Cancel.
			dcheck, err := c.checkCounterpartyDeposit(c.theirSecretHash, c.dstAmt)
			if err != nil {
				return nil, err
			}
			if !dcheck.IsGood {
				return nil, &selfCancelErr{reason: crBadADepositTx}
			}
			c.theirDepositVout = dcheck.DepositVout
			c.theirP2SHNative = dcheck.P2SHNative
			c.theirOverpayment = dcheck.Excess
			out, err := c.buildDeposit(false)
			if err != nil {
				return nil, err
			}
			return createdBOutcome{out: out, check: dcheck}, nil
		},
		apply: func(v any, terr error) { s.applyCreatedB(v, terr) },
	}
	if !s.n.engineRunning.Load() {
		v, terr := safeTaskRun(task)
		if terr != nil {
			// Mirror the started-mode resume (selfCancelErr must still fire).
			s.applyCreatedB(nil, terr)
			return 0, nil, terr
		}
		return proto.XbcTransactionCreatedB, s.applyCreatedB(v, nil), nil
	}
	s.await = true
	s.n.postSwapTask(orderID, task)
	return 0, nil, nil // deferred; applyCreatedB sends on worker completion
}

// applyCreatedB is the engine-side resume for a CreateB deposit task (see
// applyCreatedA for the resume contract).
func (s *SwapSession) applyCreatedB(v any, terr error) responseBody {
	orderID := hexEncode(s.id[:])
	s.await = false
	if terr != nil {
		if s.failSelfCancel(terr) {
			return nil
		}
		xlog.Error("CreateB deposit task failed", "order", orderID, "err", terr)
		return nil
	}
	out := v.(createdBOutcome)
	s.ourDepositTxID = out.out.txid
	s.ourLockTime = out.out.lockTime
	s.refundHex = out.out.refundHex
	s.theirDepositVout = out.check.DepositVout
	s.theirP2SHNative = out.check.P2SHNative
	s.theirOverpayment = out.check.Excess
	s.n.store.Update(orderID, func(o *Order) {
		o.BinTxId = out.out.txid
		o.DepositSent = true
	})
	s.n.store.Update(orderID, func(o *Order) {
		o.RefundTx = out.out.refundHex
	})
	s.n.store.Update(orderID, func(o *Order) {
		o.OBinTxVout = out.check.DepositVout
		o.OBinTxP2SHAmount = toXBridgeAmt(counterpartyCoin(s), out.check.P2SHNative)
		o.OOverpayment = out.check.Excess
	})
	s.state = csCreatedB
	xlog.Info("deposit B broadcast", "order", orderID, "txid", out.out.txid,
		"lockTime", out.out.lockTime, "makerDeposit", s.theirDepositTxID)
	xlog.Debug("deposit B refund pre-signed", "order", orderID)
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

// OnConfirmA (hub→maker's dest) → ConfirmedA (19): redeem taker's deposit B by
// revealing our secret on-chain, then broadcast A's payTx.
//
// Two-phase: stage 1 (engine) records the taker deposit; the worker drift-checks
// its lockTime, builds the claim, and broadcasts the payTx; the resume applies
// the outcome, sends the ConfirmedA response, and persists.
func (s *SwapSession) OnConfirmA(b *proto.ConfirmABody) (proto.XBridgeCommand, responseBody, error) {
	orderID := hexEncode(s.id[:])
	if !s.isMaker {
		return 0, nil, fmt.Errorf("api: ConfirmA received by taker session %s", orderID)
	}
	// STATE-F77: C++ processTransactionConfirmA drops a ConfirmA once the transaction
	// already reached trCommited (xbridgesession.cpp:2897). A retransmit AFTER
	// we redeemed must not re-broadcast a second claim payTx.
	if s.state >= csConfirmedA {
		xlog.Info("ConfirmA ignored: swap already past claim", "order", orderID, "state", s.state.String())
		return 0, nil, nil
	}
	s.theirDepositTxID = b.BDepositTxID
	s.theirLockTime = b.BLockTime
	// Record the counterparty (taker) deposit txid on the order so
	// dxPartialOrderChainDetails can emit p2sh_deposits_counterparty.
	s.n.store.Update(orderID, func(o *Order) {
		o.OBinTxId = b.BDepositTxID
	})
	c := s.snapshot()
	task := workTask{
		orderID: orderID,
		run: func() (any, error) {
			// Validate the counterparty (taker) B-deposit lockTime (C++
			// acceptableLockTimeDrift). Compare against our own expectation for
			// that same deposit role on the taker's coin (dstCur). In Go's
			// hub-driven flow this is the earliest the maker learns it (after
			// our own deposit is already broadcast); if it fails we must not
			// proceed to redeem.
			expLT := c.computeLockTimeFor(c.dstCur, false)
			if !acceptableLockTimeDrift(expLT, b.BLockTime, c.blockTimeFor(c.dstCur)) {
				// C++ :2926-2935 — bad counterparty locktime → wire-Cancel.
				return nil, &selfCancelErr{reason: crBadBLockTime}
			}
			// CRYPTO-F85: validate the taker's B deposit before redeeming it
			// (C++ :2957). Wait → no response (hub retransmits); bad → Cancel.
			dcheck, err := c.checkCounterpartyDeposit(c.secretHash, c.dstAmt)
			if err != nil {
				return nil, err
			}
			if !dcheck.IsGood {
				return nil, &selfCancelErr{reason: crBadBDepositTx}
			}
			c.theirDepositVout = dcheck.DepositVout
			c.theirP2SHNative = dcheck.P2SHNative
			xlog.Info("ConfirmA: redeeming taker deposit", "order", orderID, "takerDeposit", b.BDepositTxID)
			payHex, cur, err := c.redeemCounterparty(true)
			if err != nil {
				return nil, err
			}
			xlog.Debug("ConfirmA: claim tx built", "order", orderID, "cur", cur)
			conn, e := s.n.connector(cur)
			if e != nil {
				return nil, e
			}
			payTxID, err := conn.SendRawTransaction(payHex)
			if err != nil {
				return nil, fmt.Errorf("api: broadcast payTx: %w", err)
			}
			return confirmOutcome{payTxID: payTxID, check: dcheck}, nil
		},
		apply: func(v any, terr error) { s.applyConfirmedA(v, terr) },
	}
	if !s.n.engineRunning.Load() {
		v, terr := safeTaskRun(task)
		if terr != nil {
			// Mirror the started-mode resume (selfCancelErr must still fire).
			s.applyConfirmedA(nil, terr)
			return 0, nil, terr
		}
		return proto.XbcTransactionConfirmedA, s.applyConfirmedA(v, nil), nil
	}
	s.await = true
	s.n.postSwapTask(orderID, task)
	return 0, nil, nil // deferred; applyConfirmedA sends on worker completion
}

// applyConfirmedA is the engine-side resume for a ConfirmA claim task (see
// applyCreatedA for the resume contract).
func (s *SwapSession) applyConfirmedA(v any, terr error) responseBody {
	orderID := hexEncode(s.id[:])
	s.await = false
	if terr != nil {
		if s.failSelfCancel(terr) {
			return nil
		}
		xlog.Error("ConfirmA claim task failed", "order", orderID, "err", terr)
		return nil
	}
	out := v.(confirmOutcome)
	s.theirDepositVout = out.check.DepositVout
	s.theirP2SHNative = out.check.P2SHNative
	s.theirOverpayment = out.check.Excess
	s.state = csConfirmedA
	xlog.Info("ConfirmA: payTx broadcast", "order", orderID, "payTxID", out.payTxID)
	// Counterparty-deposit redeemed (C++ hasRedeemedCounterpartyDeposit()); the
	// validated deposit out-params feed the F90 order record.
	s.n.store.Update(orderID, func(o *Order) {
		o.CounterpartyRedeemed = true
		o.OBinTxVout = out.check.DepositVout
		o.OBinTxP2SHAmount = toXBridgeAmt(counterpartyCoin(s), out.check.P2SHNative)
		o.OOverpayment = out.check.Excess
	})
	body := responseBody(&proto.ConfirmedABody{
		HubAddress: s.hub, ID: s.id, APayTxID: out.payTxID,
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

// OnConfirmB (hub→taker's dest) → ConfirmedB (21): recover the secret from A's
// payTx, then redeem maker's deposit A.
//
// Two-phase: stage 1 (engine) is pure; the worker fetches the maker's payTx,
// recovers the secret, builds the claim, and broadcasts the payTx; the resume
// applies the outcome (including the recovered secret), sends the ConfirmedB
// response, and persists.
func (s *SwapSession) OnConfirmB(b *proto.ConfirmBBody) (proto.XBridgeCommand, responseBody, error) {
	orderID := hexEncode(s.id[:])
	if s.isMaker {
		return 0, nil, fmt.Errorf("api: ConfirmB received by maker session %s", orderID)
	}
	// STATE-F77: C++ processTransactionConfirmB drops a ConfirmB once the transaction
	// already reached trCommited (xbridgesession.cpp:3152). A retransmit AFTER
	// we redeemed must not re-broadcast a second claim payTx.
	if s.state >= csConfirmedB {
		xlog.Info("ConfirmB ignored: swap already past claim", "order", orderID, "state", s.state.String())
		return 0, nil, nil
	}
	c := s.snapshot()
	task := workTask{
		orderID: orderID,
		run: func() (any, error) {
			// Recover the 33-byte secret preimage from the maker's payTx.
			conn := s.n.cfg().Connectors[c.srcCur]
			if conn == nil {
				return nil, fmt.Errorf("api: no connector for %s", c.srcCur)
			}
			payHex, err := conn.GetRawTransaction(b.APayTxID)
			if err != nil {
				return nil, fmt.Errorf("api: getrawtransaction %s: %w", b.APayTxID, err)
			}
			// The maker's payTx was serialized by the maker's XBridge connector;
			// if that coin sets serializeWithTimeField we must parse the nTime
			// field accordingly.
			hasTime := false
			if cc := c.conf(c.srcCur); cc != nil {
				hasTime = cc.TxWithTimeField
			}
			secret, ok := secretFromPayTx(payHex, c.theirSecretHash, hasTime)
			if !ok {
				return nil, fmt.Errorf("api: could not recover secret from payTx %s", b.APayTxID)
			}
			c.secret = secret // the claim's payment scriptSig pushes the preimage
			xlog.Info("ConfirmB: secret recovered from maker payTx", "order", orderID,
				"makerPayTx", b.APayTxID, "secretHash", hexEncode(c.theirSecretHash[:]))

			payHex2, cur, err := c.redeemCounterparty(false)
			if err != nil {
				return nil, err
			}
			xlog.Debug("ConfirmB: claim tx built", "order", orderID, "cur", cur)
			conn2, e := s.n.connector(cur)
			if e != nil {
				return nil, e
			}
			payTxID, err := conn2.SendRawTransaction(payHex2)
			if err != nil {
				return nil, fmt.Errorf("api: broadcast payTx: %w", err)
			}
			return confirmOutcome{secret: secret, payTxID: payTxID}, nil
		},
		apply: func(v any, terr error) { s.applyConfirmedB(v, terr) },
	}
	if !s.n.engineRunning.Load() {
		v, terr := safeTaskRun(task)
		if terr != nil {
			// Mirror the started-mode resume (selfCancelErr must still fire).
			s.applyConfirmedB(nil, terr)
			return 0, nil, terr
		}
		return proto.XbcTransactionConfirmedB, s.applyConfirmedB(v, nil), nil
	}
	s.await = true
	s.n.postSwapTask(orderID, task)
	return 0, nil, nil // deferred; applyConfirmedB sends on worker completion
}

// applyConfirmedB is the engine-side resume for a ConfirmB claim task (see
// applyCreatedA for the resume contract). It adopts the worker-recovered secret
// into the session.
func (s *SwapSession) applyConfirmedB(v any, terr error) responseBody {
	orderID := hexEncode(s.id[:])
	s.await = false
	if terr != nil {
		xlog.Error("ConfirmB claim task failed", "order", orderID, "err", terr)
		return nil
	}
	out := v.(confirmOutcome)
	s.secret = out.secret
	s.state = csConfirmedB
	xlog.Info("ConfirmB: payTx broadcast", "order", orderID, "payTxID", out.payTxID)
	// Counterparty-deposit redeemed (C++ hasRedeemedCounterpartyDeposit()).
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

// OnFinished (hub→both): the swap is complete on the hub; mark the order
// terminal (C++ trFinished) and move it out of the live book into history
// (C++ moveTransactionToHistory), which releases its reserved UTXOs and drops
// it from dxGetOrders, then prune the session.
func (s *SwapSession) OnFinished(b *proto.FinishedBody) (proto.XBridgeCommand, responseBody, error) {
	s.n.store.MoveToHistory(hexEncode(s.id[:]), "finished", 0, NowMicro())
	s.state = csFinished
	xlog.Info("swap finished", "order", hexEncode(s.id[:]), "state", s.state.String())
	s.n.persist()
	s.n.pruneSessions()
	return 0, nil, nil
}

// runRefundTask executes a refund broadcast for cur/refundHex. When checkLock
// is set it only broadcasts once the chain is at/above lockTime (the sweep
// path); otherwise it broadcasts immediately (the cancel/rollback/escape-hatch
// paths, matching C++ force-refund semantics). Returns ("", nil) when the
// refund is not yet due — the sweep retries on the next tick. Runs on a worker
// goroutine; self-contained (all inputs captured by value).
func (n *Node) runRefundTask(cur, refundHex string, lockTime uint32, checkLock bool) (string, error) {
	conn := n.cfg().Connectors[cur]
	if conn == nil {
		return "", fmt.Errorf("api: no connector for %s", cur)
	}
	if checkLock {
		h, err := conn.GetBlockCount()
		if err != nil || h < 1 {
			return "", fmt.Errorf("api: getblockcount %s: %v", cur, err)
		}
		if uint32(h) < lockTime {
			return "", nil // not yet refundable
		}
	}
	return conn.SendRawTransaction(refundHex)
}

// postRefundTask posts a refund broadcast to the worker pool (or runs it
// synchronously in inline mode, when the engine is not started). The apply runs
// on the engine: it clears the in-flight guard, invokes the optional done
// callback, marks the session refundDone on success, and persists. A full task
// queue drops the task (the broadcast is idempotent and the next sweep retries)
// and clears the guard so that sweep can re-enqueue it.
func (n *Node) postRefundTask(orderID, cur, refundHex string, lockTime uint32, checkLock bool, done func(txid string, err error)) bool {
	task := workTask{
		orderID: orderID,
		run: func() (any, error) {
			return n.runRefundTask(cur, refundHex, lockTime, checkLock)
		},
		apply: func(v any, err error) {
			delete(n.pendingRefunds, orderID)
			txid, _ := v.(string)
			if done != nil {
				done(txid, err)
			}
			if err != nil {
				xlog.Warn("refund broadcast failed", "order", orderID, "err", err)
				return
			}
			if txid == "" {
				return // not yet refundable; the next sweep retries
			}
			xlog.Info("refund broadcast", "order", orderID, "txid", txid)
			if s := n.sessions[orderID]; s != nil {
				s.refundDone = true
			}
			n.persist()
			// A refunded order is terminal: drop the session so it stops being
			// swept and stops being re-persisted (C++ moveTransactionToHistory).
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
		return false
	}
}

// postSwapTask posts a two-phase handshake task (deposit build / claim) to the
// worker pool (or runs it synchronously in inline mode, when the engine is not
// started). The apply runs on the engine: it applies the outcome to the session,
// sends the hub response, and persists. A full task queue drops the task and
// clears the session's await guard so the hub's retransmit re-stages it (fund-
// safe: nothing was broadcast).
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
			s.await = false
		}
		xlog.Warn("swap task dropped, engine busy", "order", orderID)
		return false
	}
}

// scanRefunds sweeps all live sessions and auto-broadcasts any pre-signed
// refund whose deposit lockTime has passed (the fund-safety safety net). Runs
// on the engine goroutine; each eligible session posts a worker task guarded by
// pendingRefunds so no order is ever double-enqueued. Inline mode (tests)
// broadcasts synchronously.
func (n *Node) scanRefunds() {
	for id, s := range n.sessions {
		if s.refundDone || s.refundHex == "" || s.state == csFinished || s.state < csCreatedA {
			continue
		}
		if n.engineRunning.Load() {
			// Started mode: guard against double-enqueue. Inline mode (tests)
			// runs synchronously, so the guard is redundant there.
			if n.pendingRefunds[id] {
				continue
			}
			n.pendingRefunds[id] = true
		}
		n.postRefundTask(id, s.srcCur, s.refundHex, s.ourLockTime, true, nil)
	}
}

// sessionIsTerminal reports whether a session can be dropped from n.sessions.
// It reached csFinished, or there is nothing left for it to do: its order left
// the live book (moved to history), its order is terminal, or its refund was
// already broadcast. The only reasons to keep a session are a pre-signed refund
// that may still need broadcasting (the scanRefunds sweep — a guard that is
// exactly the complement of the scanRefunds skip, so we never prune a session
// that may still broadcast a refund) or an in-flight deposit/claim task whose
// apply has not yet recorded the outcome.
func (n *Node) sessionIsTerminal(id string, s *SwapSession) bool {
	if s.state == csFinished {
		return true
	}
	// A refund may still be owed: never prune while the sweep could broadcast.
	if s.refundHex != "" && !s.refundDone && s.state >= csCreatedA {
		return false
	}
	// A deposit/claim task is in flight (await stays set until its apply lands
	// on the engine): pruning now would orphan the broadcast and strand a
	// deposit whose pre-signed refund is about to be recorded.
	if s.await {
		return false
	}
	o := n.store.Get(id)
	// Otherwise the session has no further work: the order left the live book
	// (moved to history), the order is terminal, or the refund is already sent.
	return o == nil || isOrderTerminal(o.Status) || s.refundDone
}

// pruneSessions removes terminal sessions from n.sessions, the lifecycle
// counterpart to C++ App::moveTransactionToHistory (terminal transactions
// leave the live map). Runs on the engine goroutine (ticker + resumes); inline
// tests call it single-threaded. Deleting a session also clears its in-flight
// refund guard so a stale sweep entry can never linger.
func (n *Node) pruneSessions() {
	for id, s := range n.sessions {
		if n.sessionIsTerminal(id, s) {
			delete(n.pendingRefunds, id)
			delete(n.sessions, id)
			xlog.Info("swap session pruned", "order", id, "state", s.state.String())
		}
	}
}

// checkRefunds is the public entry point for the refund sweep. The engine
// ticker drives scanRefunds directly; this wrapper exists for tests and
// external callers and routes through submit so production stays engine-owned.
func (n *Node) checkRefunds() {
	n.submit(n.scanRefunds, false)
}

// enqueueRefund schedules a fund-recovery refund broadcast for orderID: the
// live session's pre-signed refund when present, else the order's stored refund
// hex (the escape-hatch fallback, trying each deposit chain). Fire-and-forget
// from the engine (done == nil); the optional done callback receives the
// outcome when it lands. Inline mode (tests) runs synchronously and invokes
// done before returning.
func (n *Node) enqueueRefund(orderID string, done func(txid string, err error)) {
	if s := n.sessions[orderID]; s != nil && s.refundHex != "" {
		// CONC-F102: take the pendingRefunds guard so a concurrent sweep
		// (scanRefunds) cannot enqueue a second broadcast of the same refund
		// hex while this force-refund is in flight. postRefundTask's apply
		// clears the guard on success AND error, so the sweep safety net still
		// retries a failed force-refund once the deposit is due. Mirrors the
		// scanRefunds guard pattern (engine-owned in started mode; redundant in
		// inline mode, where scanRefunds skips the guard entirely).
		if n.engineRunning.Load() {
			n.pendingRefunds[orderID] = true
		}
		n.postRefundTask(orderID, s.srcCur, s.refundHex, 0, false, done)
		return
	}
	o := n.store.Get(orderID)
	if o == nil || o.RefundTx == "" {
		if done != nil {
			done("", fmt.Errorf("api: no refund available for order %s", orderID))
		}
		return
	}
	cands := make([]string, 0, 2)
	for _, cur := range []string{o.FromCurrency, o.ToCurrency} {
		if cur != "" && n.cfg().Connectors[cur] != nil {
			cands = append(cands, cur)
		}
	}
	n.tryStoredRefund(orderID, o.RefundTx, cands, done)
}

// tryStoredRefund broadcasts an order's stored refund hex against each
// candidate currency in order, stopping at the first success (C++ tries the
// maker then taker deposit chains). Chained via callbacks so no currency is
// ever double-broadcast.
func (n *Node) tryStoredRefund(orderID, refundHex string, cands []string, done func(txid string, err error)) {
	if len(cands) == 0 {
		if done != nil {
			done("", fmt.Errorf("api: could not broadcast stored refund for %s", orderID))
		}
		return
	}
	cur := cands[0]
	n.postRefundTask(orderID, cur, refundHex, 0, false, func(txid string, err error) {
		if err == nil && txid != "" {
			if done != nil {
				done(txid, nil)
			}
			return
		}
		n.tryStoredRefund(orderID, refundHex, cands[1:], done)
	})
}

// BroadcastRefund is the manual escape hatch: it force-broadcasts the pre-signed
// CLTV refund for an order (e.g. a swap has stalled and the deposit's lockTime
// has passed), returning the deposit to the source address without waiting for
// the background sweep. In started mode the broadcast runs on a worker and the
// caller awaits its outcome; inline mode (tests) runs synchronously.
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

// ---------------------------------------------------------------------------
// Deposit / claim / refund construction
// ---------------------------------------------------------------------------

// computeLockTime returns the absolute block height for OUR deposit on srcCur
// (mirrors C++: currentBlock + target/blockTime). See computeLockTimeFor for the
// general form used to validate a counterparty's lockTime on THEIR coin. It
// delegates to a swapCtx snapshot, keeping the logic in the worker-safe type.
func (s *SwapSession) computeLockTime(isMaker bool) uint32 {
	return s.snapshot().computeLockTimeFor(s.srcCur, isMaker)
}

// computeLockTimeFor returns the absolute block height to embed in the deposit
// HTLC for a given coin (mirrors C++ lockTime(): currentBlock + target/blockTime).
// It is also used to compute our expectation of the counterparty's lockTime so we
// can validate it via acceptableLockTimeDrift. Runs on a worker in the two-phase
// handshake, so it reads config/connectors only — never live session state.
func (c swapCtx) computeLockTimeFor(cur string, isMaker bool) uint32 {
	conn := c.n.cfg().Connectors[cur]
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

// computeLockTime returns the absolute block height for OUR deposit on srcCur
// (mirrors C++: currentBlock + target/blockTime).
func (c *swapCtx) computeLockTime(isMaker bool) uint32 {
	return c.computeLockTimeFor(c.srcCur, isMaker)
}

// buildDeposit builds the local participant's HTLC deposit, funds it from the
// wallet connector, signs the funding inputs via the wallet, pre-builds the CLTV
// refund, and only then broadcasts (CRYPTO-F86 — C++ builds deposit → refund →
// broadcast). It returns the broadcast deposit txid + lockTime and the
// pre-signed refund hex as a depositOutcome. Runs on a worker in the two-phase
// handshake; it reads only this snapshot and lock-guarded config, and performs
// no session/order mutation — those happen in the engine-side resume.
func (c *swapCtx) buildDeposit(isMaker bool) (depositOutcome, error) {
	cur := c.srcCur
	amt := c.srcAmt
	conn := c.n.cfg().Connectors[cur]
	cc := c.conf(cur)
	if conn == nil {
		return depositOutcome{}, fmt.Errorf("api: no connector for %s", cur)
	}
	coin, ok := coins.Get(cur)
	if !ok {
		return depositOutcome{}, fmt.Errorf("api: unknown coin %s", cur)
	}
	// CRYPTO-F87: spend the recorded make/take-time selection (Order.UsedCoins,
	// C++ xtx->usedCoins), never a fresh ListUnspent.
	funding := c.funding
	if len(funding) == 0 {
		return depositOutcome{}, fmt.Errorf("api: no funding UTXOs for %s (order has no used coins)", cur)
	}
	changeStr, err := conn.GetNewAddress()
	if err != nil {
		return depositOutcome{}, err
	}
	var change [20]byte
	if a, derr := coin.DecodeAddress(changeStr); derr != nil {
		return depositOutcome{}, derr
	} else {
		copy(change[:], a.Hash)
	}
	// CRYPTO-F78: the deposit network fee uses minTxFee1(nIn, 3) — C++ computes
	// fee1 over the deposit with three outputs (p2sh + change + dust safety),
	// xbridgesession.cpp:1994 (maker) / :2526 (taker).
	fee := estimateFee(cc, len(funding), 3)
	// fee2 is the p2sh redeem margin C++ locks into the HTLC output on top of
	// the order amount (minTxFee2(1,1), xbridgesession.cpp:2094/:2615); it is
	// collected when the deposit is claimed or refunded.
	fee2 := estimateFee(cc, 1, 1)
	lockTime := c.computeLockTime(isMaker)
	xlog.Debug("buildDeposit: plan", "order", c.orderID, "isMaker", isMaker,
		"cur", cur, "amountXB", amt, "lockTime", lockTime, "txVersion", c.txVersion(cur),
		"utxos", len(funding), "fee", fee, "fee2", fee2)

	hash := c.secretHash
	if !isMaker {
		hash = c.theirSecretHash
	}
	// W0 (CRYPTO-F90 prerequisite): the on-chain deposit locks NATIVE base units.
	// c.srcAmt is XBridge 1e6 base; convert at this boundary so the swap
	// package (BuildDepositTx output = Amount+fee2, change = total−Amount−fee−fee2)
	// never mixes scales. C++ converts outAmount = fromAmount/COIN(XBridge) to
	// whole coins and createDepositTransaction emits out.second*COIN(native)
	// (xbridgewalletconnectorbtc.cpp:2442-2450). For COIN=1e6 coins this is a
	// no-op; for BTC (1e8) it fixes the 100× under-lock.
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
	tx, err := spec.BuildDepositTx(coin, funding, change, fee, fee2)
	if err != nil {
		return depositOutcome{}, err
	}
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
	// CRYPTO-F86: derive the deposit txid LOCALLY (C++ binTxId comes from
	// createDepositTransaction, xbridgewalletconnectorbtc.cpp:2410) and
	// pre-build the CLTV refund BEFORE broadcasting. C++ builds deposit →
	// refund → then broadcasts (processTransactionCreateA/B); the old order
	// broadcast first, so a refund-build failure stranded a live deposit with
	// no escape hatch. The refund spends the locally-derived txid.
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

	sentID, err := conn.SendRawTransaction(signed)
	if err != nil {
		return depositOutcome{}, err
	}
	// The wallet's returned id is logged (C++ :2187-2191) but never adopted:
	// C++ keeps binTxId (the locally-derived txid) as authoritative.
	if sentID != "" && sentID != localTxID {
		xlog.Warn("buildDeposit: wallet reported a different sent txid", "order", c.orderID, "local", localTxID, "sent", sentID)
	}
	return depositOutcome{txid: localTxID, lockTime: lockTime, refundHex: refundHex}, nil
}

// buildRefundTx pre-signs the IF-branch (CLTV) refund that returns the deposit to
// our source address, spendable only after the deposit's lockTime. Runs on a
// worker in the two-phase handshake (reads only this snapshot + config).
func (c *swapCtx) buildRefundTx(spec *swap.DepositSpec, cur string) (string, error) {
	dest, err := c.destScript(cur, c.ourSourceAddr)
	if err != nil {
		return "", err
	}
	fee := estimateFee(c.conf(cur), 1, 1)
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
		"deposit", c.ourDepositTxID, "lockTime", spec.LockTime, "amount", spec.Amount, "fee", fee, "txVersion", c.txVersion(cur))
	tx.Inputs = append(tx.Inputs, coins.TxIn{
		PrevOut:  coins.OutPoint{Hash: h, Index: 0},
		Sequence: 0xfffffffe, // enable CLTV
	})
	// CRYPTO-F90: the refund pays the FULL nominal amount — C++ refund output is
	// outAmount (xbridgesession.cpp:2149), spending the deposit's outAmount+fee2
	// output, so fee2 is the refund's implicit miner fee. Was spec.Amount - fee
	// (a second fee2 deduction).
	tx.Outputs = append(tx.Outputs, coins.TxOut{Value: spec.Amount, ScriptPubKey: dest})

	inner := spec.RedeemScript()
	sig, err := coins.SignTxInput(tx, 0, inner, c.privKey[:])
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
// the taker redeems the maker's BTC deposit. Runs on a worker in the two-phase
// handshake (reads only this snapshot + config); ConfirmB sets c.secret to the
// recovered preimage before calling.
func (c *swapCtx) redeemCounterparty(isMaker bool) (payHex, depositCur string, err error) {
	depositCoin, ok := coins.Get(c.dstCur)
	if !ok {
		return "", "", fmt.Errorf("api: unknown coin %s", c.dstCur)
	}
	theirSpec := swap.DepositSpec{
		Currency:        c.dstCur,
		Amount:          fromXBridgeAmt(depositCoin, c.dstAmt), // W0: native base units
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
	// CRYPTO-F90: spend the VALIDATED counterparty deposit (C++ oBinTxVout /
	// oBinTxP2SHAmount, recorded by the F85 check) instead of the hardcoded
	// vout 0 / nominal amount. The claim pays the exact deposit value − fee2;
	// the excess over the nominal dstAmt (+fee2) is what the redeemer keeps
	// (C++ output = outAmount + oOverpayment = depositP2SH − fee2,
	// xbridgesession.cpp:3971). P2SHNative is the exact native value, so the
	// spend can never round-trip past the deposit.
	p2shNative := c.theirP2SHNative
	if p2shNative == 0 {
		return "", "", fmt.Errorf("api: counterparty deposit not validated (no p2sh amount)")
	}
	if fee >= p2shNative {
		return "", "", fmt.Errorf("api: counterparty deposit amount too small for claim fee")
	}
	h, err := reverseTxidHex(c.theirDepositTxID)
	if err != nil {
		return "", "", err
	}
	tx := &coins.Tx{Version: int32(c.txVersion(depositCur)), LockTime: 0} // ELSE branch, no CLTV
	// Per-coin serializeWithTimeField: the claim (ELSE-branch) spend must carry
	// the nTime field for coins whose connector sets it, matching the
	// counterparty's XBridge serialization.
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
	tx.Outputs = append(tx.Outputs, coins.TxOut{Value: p2shNative - fee, ScriptPubKey: dest})

	inner := theirSpec.RedeemScript()
	sig, err := coins.SignTxInput(tx, 0, inner, c.privKey[:])
	if err != nil {
		return "", "", err
	}
	// The ELSE branch requires <secret> <sig> <myPubKey> OP_0 <inner>, where
	// myPubKey is the deposit's CounterpartyPub (== our trader key).
	tx.Inputs[0].ScriptSig = coins.BuildPaymentScriptSig(c.secret[:], sig, c.pubKey[:], inner)
	return hex.EncodeToString(tx.Serialize()), depositCur, nil
}

// destScript returns the P2PKH output script for addr on cur.
func (c *swapCtx) destScript(cur, addrStr string) ([]byte, error) {
	coin, ok := coins.Get(cur)
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

func (c *swapCtx) conf(cur string) *config.CoinConf {
	if c.n.cfg().Confs != nil {
		return c.n.cfg().Confs[cur]
	}
	return nil
}

// txVersion returns the per-coin transaction version to stamp on the deposit,
// refund, and claim txs. C++ reads <COIN>.TxVersion from xbridge.conf (default
// 1); we mirror that, falling back to 1 when unset. Never hardcode this.
func (c *swapCtx) txVersion(cur string) int {
	cc := c.conf(cur)
	if cc == nil || cc.TxVersion <= 0 {
		return 1
	}
	return cc.TxVersion
}

// minConf returns the connector's minimum confirmations for spendable UTXOs,
// defaulting to 0 when no conf is configured.
func (c *swapCtx) minConf(cc *config.CoinConf) int {
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

// decodePub33 decodes a 33-byte compressed pubkey hex into a fixed array,
// returning the zero value when the string is empty, malformed, or the wrong
// length. It is used to materialize a session's trusted hub key (STATE-F78).
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
