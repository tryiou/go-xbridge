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
//
// The values are the config-package mirrors of the C++ constexprs
// (xbridgewallet.h:96-102), the single source of truth shared with the
// admission gates (config.Admit).
const (
	makerLockTimeSec      = config.XMakerLocktimeTargetSeconds
	takerLockTimeSec      = config.XTakerLocktimeTargetSeconds
	xMinLockTimeBlocks    = config.XMinLockTimeBlocks
	xSlowTakerLockTimeSec = config.XSlowTakerLocktimeTargetSeconds
	xSlowBlockTimeSec     = config.XSlowBlockTimeSeconds

	// refundCheckInterval is how often the background watcher scans live sessions
	// for refunds whose deposit lockTime has passed. Overridable in tests.
	refundCheckInterval = 60 * time.Second

	// expirySweepInterval mirrors C++ TIMER_INTERVAL (xbridgeapp.cpp:90): the
	// order-book expiry sweep (checkAndEraseExpiredTransactions) runs every 15 s,
	// and saveOrders fires every 4th tick (~60 s, xbridgeapp.cpp:3744). Overridable
	// in tests.
	expirySweepInterval = 15 * time.Second
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
	// depositHex is the signed deposit raw hex, adopted at build success
	// alongside ourDepositTxID. A tick-driven repost re-sends these IDENTICAL
	// bytes when the first broadcast fails (swap_retry.go) — never a rebuild,
	// so a repost can never double-deposit.
	depositHex string

	// Built ELSE-branch claim (maker: redeem of B; taker: redeem of A),
	// persisted before broadcast so a crash between claim-build and broadcast
	// no longer loses payHex (C++ keeps the built payTx in the in-memory xtx
	// until send). Hub redelivery rebuilds the claim from the trigger
	// pointers, as does the tick-driven claim retry (swap_retry.go); the
	// built hex itself is the manual-recovery record (broadcastable as-is
	// with sendrawtransaction alongside the txlog entry).
	claimHex  string // signed claim raw hex, empty until built
	claimTxID string // locally-derived claim txid (authoritative)
	claimCur  string // chain the claim spends on

	theirDepositTxID string // counterparty's deposit txid (from ConfirmA/CreateB)
	theirLockTime    uint32
	theirSecretHash  [20]byte

	// theirPayTxID is the counterparty's claim payTx that reveals the secret
	// (taker side: the maker's APayTxID from ConfirmB, stored at stage 1 even
	// when the claim build fails). A hub retransmit is ephemeral; this pointer
	// is what lets the tick retry rebuild the claim without the hub.
	theirPayTxID string

	// claimRetryAt is the wall-microsecond timestamp after which the engine
	// tick re-posts a failed ELSE-branch claim build (0 = none scheduled).
	// A failed build is transient until proven otherwise (backend lag,
	// mempool blindness): the trigger pointers above are sufficient to
	// rebuild, so dropping the retry would strand a claimable HTLC whenever
	// the hub stays quiet (live-proven e6730fe2). claimRetries counts
	// consecutive failures and drives the backoff; a successful build clears
	// both. Retries refresh lastProgress — the session is actively
	// recovering, not silent — while hub/user cancel still terminates it.
	claimRetryAt uint64
	claimRetries uint32

	// depositRetryAt/depositRetries schedule the tick-driven rebuild of a
	// failed HTLC deposit build (swap_retry.go): same contract as the claim
	// slot, one stage earlier. A failed deposit build is transient until
	// proven otherwise (backend lag, mempool blindness); selfCancel aborts
	// never land here.
	depositRetryAt uint64
	depositRetries uint32

	// Validated counterparty deposit: the C++
	// checkDepositTransaction out-params recorded when we accept the
	// counterparty's deposit (CreateB for the taker's A-check, ConfirmA for the
	// maker's B-check). P2SHNative is the exact matched output value in the
	// coin's native base (the claim spend); Overpayment is XBridge 1e6 base.
	theirDepositVout uint32
	theirP2SHNative  uint64
	theirOverpayment uint64

	// hub is the service-node address, pinned at session creation (maker:
	// chosen at MakeOrder; taker: order's HubAddress).
	hub [20]byte
	// hubKey is the trusted hub service-node pubkey (C++ xtx->sPubKey), pinned
	// at creation for BOTH roles (maker: the SN chosen at make; taker: order's
	// SNodePubkey); every hub handshake packet is re-verified against it.
	hubKey [33]byte
	state  clientState

	// await is true while a three-phase handshake task (deposit/claim build + broadcast) for this
	// session is in flight: set by the staged handler's stage 1, cleared by its
	// resume on success AND error. The hub sends the next packet only after our
	// response, so any packet arriving during await is a retransmit and is
	// dropped by processSwap. Engine-owned; never read off the engine goroutine.
	await bool
	// awaitSince stamps when await was set (wall micros, 0 when unset). A
	// worker result clears both together; a result that never arrives leaves
	// a stuck guard the tick timeout (clearStuckAwait) releases. Memory-only
	// like await itself: a restart starts unstamped, and hub redelivery plus
	// the retry sweeps re-drive whatever was in flight.
	awaitSince uint64

	// lastProgress is the wall-microsecond timestamp of the last observable
	// forward motion: session creation, an accepted hub packet, or a broadcast
	// we issued. The hub-silence watchdog (Phase 1 R3) cancels sessions silent
	// past the stall threshold instead of stalling until locktime.
	// Engine-owned like await.
	lastProgress uint64

	// holdApplySentAt is the wall-microsecond timestamp of the last
	// HoldApply send (first send via processSwap, later ones via the
	// resender). The resender (api/hold_resend.go) only fires while the
	// session parks in csHoldApplied; resends deliberately do NOT touch
	// lastProgress, so the silence watchdog still cancels a mute hub
	// instead of resending forever. Zero until the first send.
	// Engine-owned like await.
	holdApplySentAt uint64
}

// depositOutcome is the worker-produced result of a deposit BUILD (CreateA/CreateB).
// Nothing is broadcast yet: the engine durably persists the intent
// (refundHex + txid + lockTime) via persistNow BEFORE posting the broadcast
// task, so a crash can never strand an on-chain deposit with no refund.
// depositHex is the signed deposit raw hex for the transcript; the same hex
// is handed to the broadcast task; txid is the locally-derived
// txid (authoritative, C++ binTxId).
type depositOutcome struct {
	txid      string
	lockTime  uint32
	refundHex string
	// depositHex is the signed deposit raw hex: the transcript records it and
	// the broadcast task sends it (single field, single owner).
	depositHex string
	// conn is the snapshot wallet connector the deposit was built against.
	// The broadcast task must use THIS connector, never a post-reload one:
	// a dxLoadXBridgeConf landing mid-task must not swap which wallet a
	// deposit broadcasts through (the snapshot exists for exactly this).
	conn wallet.Connector
}

// confirmOutcome is the worker-produced result of a claim BUILD (ConfirmA/B):
// the signed claim hex plus the validated counterparty deposit (ConfirmA) or
// the recovered secret (ConfirmB). Nothing is broadcast yet: the engine
// durably persists the intent via persistNow BEFORE posting the broadcast
// task. payTxID is the locally-derived claim id (authoritative; the wallet's
// reported id is only ever logged). cur is the chain the claim spends on.
type confirmOutcome struct {
	secret  [33]byte
	payTxID string
	payHex  string
	cur     string
	check   wallet.DepositCheck
	// conn is the snapshot wallet connector the claim was built against
	// (same mid-task-reload rule as depositOutcome.conn).
	conn wallet.Connector
}

// selfCancelErr marks a worker failure that must broadcast a Cancel packet with
// the given TxCancelReason and roll back locally — C++ sendCancelTransaction
// (crBadADepositTx/crBadBDepositTx/crBadALockTime/crBadBLockTime) + the
// immediately-following processTransactionCancel. The resume (engine side)
// recognizes it via errors.As and calls SwapSession.sendSelfCancel.
type selfCancelErr struct {
	reason TxCancelReason
}

func (e *selfCancelErr) Error() string {
	return fmt.Sprintf("api: self-cancel (TxCancelReason %d: %s)", e.reason, TxCancelReasonText(uint32(e.reason)))
}

// sendSelfCancel broadcasts a signed xbcTransactionCancel for OUR OWN rejection
// of the counterparty's deposit/locktime and rolls back locally, mirroring C++
// sendCancelTransaction + processTransactionCancel + sendPacketBroadcast
// (xbridgesession.cpp:3525-3576). Must run on the engine goroutine (it mutates
// the store/order): the callers are the three-phase resumes, which execute on the
// engine. The packet is signed with the session's per-trade M key; since
// Order.MakerKey is OUR M pubkey (order.go:96-99), handleRemoteCancel's
// iCanceled check accepts it and performs the state transition (cancel if no
// deposit sent, refund-broadcast rollback otherwise).
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
	// Local rollback first (C++ processTransactionCancel(reply)), then broadcast.
	s.n.handleRemoteCancel(pkt, body)
	// Durable-write the rolled-back state BEFORE broadcasting the cancel, so a
	// crash after the send normally cannot leave a locally-cancelled swap on
	// disk as still-live (which a counterparty could act on — fund loss). On a
	// persist failure we still broadcast the cancel (preventing counterparty
	// fund-lock) and surface the error; the rare stale-disk case is then
	// reconciled by the next persist.
	if err := s.n.persistNow(); err != nil {
		xlog.Error("selfCancel: persist failed", "order", orderID, "err", err)
	}
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
// deposit plus the validated counterparty A-deposit (the resume
// records DepositVout/P2SHAmount/Excess on the session for the redeem).
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
// theirs (C++ checkDepositTransaction call sites
// xbridgesession.cpp:2495/2957). expectedAmount is XBridge 1e6 base. Tri-state:
// ErrDepositNotReady and ErrNoChainSource are returned as errors (the caller
// sends NO response — C++ processLater / the connector cannot judge); a
// returned DepositCheck with IsGood=false is a definitively bad deposit (the
// caller must wire-Cancel, crBadADepositTx/crBadBDepositTx).
func (c *swapCtx) checkCounterpartyDeposit(hash [20]byte, expectedAmount uint64) (wallet.DepositCheck, error) {
	conn := c.connectors[c.dstCur]
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
// stage-1 enqueue time. The three-phase handshake workers run the wallet-I/O
// builders against THIS value and never touch a live session, which the engine
// owns exclusively — a worker reading a session field would race the engine's
// resume writes.
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

	// connectors / confs are shallow snapshots of the live config taken at
	// enqueue time; coinsMap is a value copy of the coin registry's entries
	// for this session's two currencies, taken under one atomic load.
	// A dxLoadXBridgeConf or the 30s wallet sweep swaps n.config
	// with NEW connector/confs objects and re-publishes the coin registry; the
	// worker task must build against the set that was current when the swap
	// started — the C++ session holds the connector pointer it captured, so a
	// mid-task reload cannot swap which wallet a deposit/claim is built
	// against, nor the coin parameters (decimals/prefix/codec) the transaction
	// is built with. Interfaces, config pointers, and Coin values are
	// immutable, so holding them is race-free.
	connectors map[string]wallet.Connector
	confs      map[string]*config.CoinConf
	coinsMap   map[string]coins.Coin

	// ourDepositP2SH is the exact value of OUR deposit's P2SH output as built by
	// BuildDepositTx (Amount+fee2, output 0). buildRefundTx commits it to the
	// forkid digest, so the committed value is structural rather
	// than recomputed — immune to a conf hot-reload landing between buildDeposit
	// and buildRefundTx.
	ourDepositP2SH uint64

	// funding is the deposit's exact funding set — the make/take-time selection
	// recorded as Order.UsedCoins (C++ xtx->usedCoins). buildDeposit
	// spends exactly this, never a fresh ListUnspent.
	funding []wallet.Utxo
}

// snapshot copies the session's fields a worker task needs, plus shallow
// snapshots of the live connector/confs sets. It runs on the engine
// goroutine (stage 1), so reading live session state and the cfg-guarded config
// here is safe.
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

// orderFunding returns the order's recorded funding set (Order.UsedCoins) for
// the deposit to spend — C++ xtx->usedCoins, populated at make/take time.
// store.Get returns a deep copy, so the worker can hold this
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
		lastProgress:  uint64(NowMicro()),
	}
	// The maker's trusted hub key is the servicenode chosen at make
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
		lastProgress:  uint64(NowMicro()),
	}
	// The taker's trusted hub key is the servicenode that broadcast
	// the order (C++ xtx->sPubKey = the SN whose header signed the order). It is
	// pinned HERE, at session creation, so every hub handshake packet
	// (Hold/Init/CreateA/B/ConfirmA/B/Finished) is re-verified against it — a
	// forged Finished can never disable the refund watcher. An order without a
	// known SNodePubkey cannot be authenticated and stays unpinned, causing
	// processSwap to drop all hub packets for it.
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
	orderID := hexEncode(s.id[:])
	// Re-verify the hub-relayed give/take amounts against the order
	// (C++ processTransactionHold, xbridgesession.cpp:1404-1471). Any mismatch
	// is dropped with NO reply (C++ return true) — the hub retransmits.
	if err := s.verifyHold(b); err != nil {
		xlog.Warn("hold rejected", "order", orderID, "err", err)
		return 0, nil, nil
	}
	// Source resolution is shared with the resender (api/hold_resend.go) so
	// first send and resends emit identical bytes for a session.
	src, err := holdApplySource(s)
	if err != nil {
		return 0, nil, err
	}
	// C++ maker resizes the order to the taker's partial amounts at Hold
	// (xbridgesession.cpp:1525-1528 `fromAmount=damount;toAmount=samount`) so
	// the maker deposit locks the PARTIAL amount. verifyHold already gated the
	// bounds/drift above, so adopting here is safe and required for partial
	// takes; full takes are unaffected (amounts equal).
	if s.isMaker {
		s.srcAmt = b.ToAmount
		s.dstAmt = b.FromAmount
		// Mirror the descr reassignment onto the stored order record (C++
		// fromAmount/toAmount; Orig* already preserves the maker's original
		// pair). Downstream stages (deposit sizing, chain details) read the
		// resized pair exactly like the C++ session does. The store may be
		// absent in wiring tests (like setOrderStatus below, which nil-guards).
		if s.n != nil && s.n.store != nil {
			s.n.store.Update(orderID, func(o *Order) {
				o.FromAmount = b.ToAmount
				o.ToAmount = b.FromAmount
			})
			// Make the resize crash-durable: without a persist, a crash
			// before the next persistLoop tick would restore pre-resize
			// amounts and the rebuilt deposit would lock the full amount.
			// Async (engine-safe); a persist failure only delays durability
			// to the next tick, it never blocks the handshake.
			s.n.persist()
		}
	}
	s.state = csHoldApplied
	// C++ maker/taker order both advance to trHold here (xbridgesession.cpp:1529).
	s.setOrderStatus("hold")
	xlog.Info("hold applied", "order", orderID, "state", s.state.String())
	return proto.XbcTransactionHoldApply, &proto.HoldApplyBody{
		HubAddress: s.hub, ClientAddress: src, ID: s.id,
	}, nil
}

// verifyHold ports the C++ processTransactionHold amount/price re-verification
// (xbridgesession.cpp:1404-1471). The Hold body carries the TAKER's give
// (FromAmount) and take (ToAmount) — for the maker that is (dstAmt, srcAmt),
// for the taker (srcAmt, dstAmt). Returns nil on success; the caller drops the
// packet without a reply on failure.
func (s *SwapSession) verifyHold(b *proto.HoldBody) error {
	if s.isMaker {
		// role 'A' (C++ :1425-1461): the taker cannot take more than the maker
		// holds, cannot offer more than the maker wants, and cannot take below a
		// partial order's minimum.
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
	// role 'B' (C++ :1404-1424): the taker's give/take must match exactly.
	if b.FromAmount != s.srcAmt {
		return fmt.Errorf("taker from amount from snode should match expected amount")
	}
	if b.ToAmount != s.dstAmt {
		return fmt.Errorf("taker to amount from snode should match expected amount")
	}
	// Drift against the order's original (maker-facing) pair; for a taker the
	// orig pair is (dstAmt, srcAmt) when no store order is present.
	origFrom, origTo := s.dstAmt, s.srcAmt
	if o := s.order(); o != nil && o.OrigFromAmount > 0 {
		origFrom, origTo = o.OrigFromAmount, o.OrigToAmount
	}
	if !swap.PartialOrderDriftCheck(origFrom, origTo, s.srcAmt, s.dstAmt) {
		return fmt.Errorf("taker price doesn't match maker expected price")
	}
	return nil
}

// OnInit (hub→each) → Initialized (9): echo back our destination address.
func (s *SwapSession) OnInit(b *proto.InitBody) (proto.XBridgeCommand, responseBody, error) {
	orderID := hexEncode(s.id[:])
	// C++ processTransactionInit drops once already initialized
	// (xbridgesession.cpp:1725-1732).
	if s.state >= csInitialized {
		xlog.Info("Init ignored: swap already initialized", "order", orderID, "state", s.state.String())
		return 0, nil, nil
	}
	// Full order-detail re-verification with the INTENDED OR
	// semantics (reject on ANY single-field mismatch). C++ :1750-1756 uses a
	// buggy && (only rejects when EVERY field differs); the intended — and
	// secure — behavior is to reject on any mismatch. Documented divergence.
	if err := s.verifyInit(b); err != nil {
		xlog.Warn("init rejected", "order", orderID, "err", err)
		return 0, nil, nil
	}
	s.state = csInitialized
	// C++ maker/taker order both advance to trInitialized here (xbridgesession.cpp:1762).
	s.setOrderStatus("initialized")
	xlog.Info("initialized", "order", orderID, "state", s.state.String())
	return proto.XbcTransactionInitialized, &proto.InitializedBody{
		HubAddress: s.hub, ClientAddress: b.ClientAddress, ID: s.id,
	}, nil
}

// verifyInit re-verifies the Init packet's order details against the session
// (intended OR semantics — see OnInit).
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

// order returns the session's store order, or nil when the node has no store
// (raw test nodes) or the order is absent.
func (s *SwapSession) order() *Order {
	if s.n == nil || s.n.store == nil {
		return nil
	}
	return s.n.store.Get(hexEncode(s.id[:]))
}

// setOrderStatus advances the order's client-descriptor Status to mirror the
// C++ TransactionDescr::State progression the swap is driving
// (xbridgesession.cpp:1529 trHold, :1762 trInitialized, :2174 trCreated). The
// authoritative swap progress lives in s.state; this keeps the order's Status
// field (what dxGetOrders/BLOCKDX surface) in lock-step so an in-swap order is
// reported as hold/initialized/created rather than as a still-open order. This
// is the inverse of the old "frozen Status" divergence, where a maker order
// stayed at the initial status for the whole swap and the session state was the
// only thing distinguishing in-swap from unmatched.
func (s *SwapSession) setOrderStatus(status string) {
	if s.n == nil || s.n.store == nil {
		return
	}
	idHex := hexEncode(s.id[:])
	s.n.store.Update(idHex, func(o *Order) {
		o.Status = status
	})
}

// decodeAddrHash decodes addrStr on cur into its 20-byte HASH160 (zero on an
// unknown coin / undecodable address).
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

// OnCreateA (hub→maker) → CreatedA (11): build + broadcast our deposit A.
//
// Three-phase: stage 1 (engine) validates and records the counterparty key;
// the worker builds the deposit (all wallet I/O, no broadcast); the phase-1
// resume persists the intent and posts the broadcast; the phase-2 resume
// applies the confirmed broadcast, sends the CreatedA response, and persists.
func (s *SwapSession) OnCreateA(b *proto.CreateABody) (proto.XBridgeCommand, responseBody, error) {
	orderID := hexEncode(s.id[:])
	if !s.isMaker {
		return 0, nil, fmt.Errorf("api: CreateA received by taker session %s", orderID)
	}
	// C++ processTransactionCreateA drops a CreateA once the transaction
	// already reached trCreated (xbridgesession.cpp:1947). A retransmit arriving
	// AFTER our deposit completed (await is cleared) must not re-broadcast a
	// second deposit.
	if s.state >= csCreatedA {
		xlog.Info("CreateA ignored: swap already past deposit", "order", orderID, "state", s.state.String())
		return 0, nil, nil
	}
	// A retransmit arriving WHILE the deposit worker is in flight (await set)
	// must not re-post a second deposit task — that would double-broadcast our
	// deposit. The in-flight worker will complete and send CreatedA.
	if s.await {
		xlog.Info("CreateA ignored: deposit worker in flight", "order", orderID)
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
	task := makerDepositBuildTask(s, c)
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
	s.holdAwait()
	s.n.postSwapTask(orderID, task)
	return 0, nil, nil // deferred; applyCreatedA sends on worker completion
}

// applyCreatedA is the engine-side resume for a CreateA deposit BUILD task
// (phase 1): it adopts the built intent (txid/lockTime/refundHex) to the
// session and order, durably persists it via persistNow, and posts the
// broadcast task — it never broadcasts itself. The hub response leaves in
// phase 2 (applyCreatedABroadcast), after the broadcast confirms. await stays
// set across the broadcast and is cleared by phase 2 (or here on build/persist
// failure), so a retransmit can neither double-build nor double-broadcast. A
// persist failure withholds the broadcast (fail closed); the hub retransmits
// and the deposit is rebuilt.
func (s *SwapSession) applyCreatedA(v any, terr error) responseBody {
	orderID := hexEncode(s.id[:])
	if terr != nil {
		s.releaseAwait()
		xlog.Error("CreateA deposit task failed", "order", orderID, "err", terr)
		// Transient until proven otherwise (wallet offline, fee estimation):
		// schedule a tick-driven rebuild instead of stranding pre-deposit
		// when the hub stays quiet. Nothing was broadcast (ourDepositTxID is
		// only set on success), so a retry can never double-broadcast.
		if s.ourDepositTxID == "" {
			scheduleDepositRetry(s, NowMicro(), "maker", terr)
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
	// Transcript of the built (not yet broadcast) deposit + refund, so even a
	// crash before the broadcast leaves the full manual-recovery record.
	txLogDepositBuilt(s.id, "A", s.srcCur, s.srcAmt, s.dstCur, s.dstAmt, out.lockTime, out.depositHex, out.refundHex)
	// Nil in started mode (phase 2 sends the response); the phase-2 body in
	// inline mode (synchronous broadcast).
	var body responseBody
	s.n.postBroadcastTask(orderID, out.conn, s.srcCur, out.depositHex, broadcastDeposit, func(sentID string, berr error) {
		body = s.applyCreatedABroadcast(out, sentID, berr)
	})
	return body
}

// applyCreatedABroadcast is the engine-side resume for a CreateA deposit
// BROADCAST task (phase 2): it records the confirmed broadcast, advances the
// order to created, writes the transcript entry, and sends the CreatedA
// response (started mode) or returns it (inline mode). A broadcast failure
// leaves the session at its pre-deposit state with the intent durable —
// resumption comes from hub redelivery (rebuild) or cancel (clean: no deposit
// exists, and enqueueRefund refuses unbroadcast intents).
func (s *SwapSession) applyCreatedABroadcast(out depositOutcome, sentID string, terr error) responseBody {
	orderID := hexEncode(s.id[:])
	s.releaseAwait()
	if terr != nil {
		xlog.Error("CreateA deposit broadcast failed", "order", orderID, "err", terr)
		// The intent (hex + txid) is durable: schedule a tick-driven repost
		// of the IDENTICAL bytes instead of stranding pre-created.
		if s.ourDepositTxID != "" && s.depositHex != "" {
			scheduleDepositRetry(s, NowMicro(), "maker-broadcast", terr)
		}
		return nil
	}
	clearDepositRetry(s)
	// The wallet's reported id is logged (C++ :2187-2191) but never adopted:
	// C++ keeps binTxId (the locally-derived txid) as authoritative.
	if sentID != "" && sentID != out.txid {
		xlog.Warn("CreateA: wallet reported a different sent txid", "order", orderID, "local", out.txid, "sent", sentID)
	}
	s.n.store.Update(orderID, func(o *Order) {
		o.DepositSent = true
	})
	s.state = csCreatedA
	// C++ maker order advances to trCreated here (xbridgesession.cpp:2174).
	s.setOrderStatus("created")
	xlog.Info("deposit A broadcast", "order", orderID, "txid", out.txid,
		"lockTime", out.lockTime, "secretHash", hexEncode(s.secretHash[:]))
	xlog.Debug("deposit A refund pre-signed", "order", orderID)
	// Swap transcript (dedicated log-tx file): only now that the broadcast is
	// confirmed — the transcript must never claim an unconfirmed broadcast.
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

// OnCreateB (hub→taker) → CreatedB (13): learn maker's deposit + build ours.
//
// Three-phase: stage 1 (engine) records the counterparty deposit; the worker
// drift-checks its lockTime (GetBlockCount) then builds ours (no broadcast);
// the phase-1 resume persists the intent and posts the broadcast; the phase-2
// resume applies the confirmed broadcast, sends the CreatedB response, and
// persists.
func (s *SwapSession) OnCreateB(b *proto.CreateBBody) (proto.XBridgeCommand, responseBody, error) {
	orderID := hexEncode(s.id[:])
	if s.isMaker {
		return 0, nil, fmt.Errorf("api: CreateB received by maker session %s", orderID)
	}
	// C++ processTransactionCreateB drops a CreateB once the transaction
	// already reached trCreated (xbridgesession.cpp:2424). A post-completion
	// retransmit must not re-broadcast a second deposit.
	if s.state >= csCreatedB {
		xlog.Info("CreateB ignored: swap already past deposit", "order", orderID, "state", s.state.String())
		return 0, nil, nil
	}
	// A retransmit arriving WHILE the deposit worker is in flight (await set)
	// must not re-post a second deposit task — that would double-broadcast our
	// deposit. The in-flight worker will complete and send CreatedB.
	if s.await {
		xlog.Info("CreateB ignored: deposit worker in flight", "order", orderID)
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
	task := takerDepositBuildTask(s, c)
	if !s.n.engineRunning.Load() {
		v, terr := safeTaskRun(task)
		if terr != nil {
			// Mirror the started-mode resume (selfCancelErr must still fire).
			s.applyCreatedB(nil, terr)
			return 0, nil, terr
		}
		return proto.XbcTransactionCreatedB, s.applyCreatedB(v, nil), nil
	}
	s.holdAwait()
	s.n.postSwapTask(orderID, task)
	return 0, nil, nil // deferred; applyCreatedB sends on worker completion
}

// applyCreatedB is the engine-side resume for a CreateB deposit BUILD task
// (phase 1): it adopts the built intent plus the validated counterparty
// out-params, durably persists via persistNow, and posts the broadcast task —
// it never broadcasts itself (see applyCreatedA for the phase contract). A
// persist failure withholds the broadcast (fail closed); the hub retransmits
// and the deposit is rebuilt.
func (s *SwapSession) applyCreatedB(v any, terr error) responseBody {
	orderID := hexEncode(s.id[:])
	if terr != nil {
		s.releaseAwait()
		if s.failSelfCancel(terr) {
			return nil
		}
		xlog.Error("CreateB deposit task failed", "order", orderID, "err", terr)
		// Transient until proven otherwise (backend lag, mempool blindness):
		// schedule a tick-driven rebuild instead of stranding at initialized
		// when the hub stays quiet (live-proven d4df334e). Nothing was
		// broadcast (ourDepositTxID is only set on success), so a retry can
		// never double-broadcast.
		if s.ourDepositTxID == "" {
			scheduleDepositRetry(s, NowMicro(), "taker", terr)
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
	// Durable intent BEFORE broadcast: from here on, a crash recovers the
	// pre-signed refund from disk (and the sweep owns it post-restart).
	if err := s.n.persistNow(); err != nil {
		s.releaseAwait()
		xlog.Error("CreateB intent persist failed, deposit withheld", "order", orderID, "err", err)
		return nil
	}
	// Transcript of the built (not yet broadcast) deposit + refund.
	txLogDepositBuilt(s.id, "B", s.srcCur, s.srcAmt, s.dstCur, s.dstAmt, out.out.lockTime, out.out.depositHex, out.out.refundHex)
	// Nil in started mode (phase 2 sends the response); the phase-2 body in
	// inline mode (synchronous broadcast).
	var body responseBody
	s.n.postBroadcastTask(orderID, out.out.conn, s.srcCur, out.out.depositHex, broadcastDeposit, func(sentID string, berr error) {
		body = s.applyCreatedBBroadcast(out, sentID, berr)
	})
	return body
}

// applyCreatedBBroadcast is the engine-side resume for a CreateB deposit
// BROADCAST task (phase 2): it records the confirmed broadcast, advances the
// order to created, writes the transcript entry, and sends the CreatedB
// response (started mode) or returns it (inline mode). A broadcast failure
// leaves the session at its pre-deposit state with the intent durable —
// resumption comes from hub redelivery (rebuild) or cancel (clean: no deposit
// exists, and enqueueRefund refuses unbroadcast intents).
func (s *SwapSession) applyCreatedBBroadcast(out createdBOutcome, sentID string, terr error) responseBody {
	orderID := hexEncode(s.id[:])
	s.releaseAwait()
	if terr != nil {
		xlog.Error("CreateB deposit broadcast failed", "order", orderID, "err", terr)
		// The intent (hex + txid) is durable: schedule a tick-driven repost
		// of the IDENTICAL bytes instead of stranding pre-created.
		if s.ourDepositTxID != "" && s.depositHex != "" {
			scheduleDepositRetry(s, NowMicro(), "taker-broadcast", terr)
		}
		return nil
	}
	clearDepositRetry(s)
	// The wallet's reported id is logged (C++ :2710-2714) but never adopted:
	// the locally-derived txid stays authoritative.
	if sentID != "" && sentID != out.out.txid {
		xlog.Warn("CreateB: wallet reported a different sent txid", "order", orderID, "local", out.out.txid, "sent", sentID)
	}
	s.n.store.Update(orderID, func(o *Order) {
		o.DepositSent = true
	})
	s.state = csCreatedB
	// C++ taker order advances to trCreated here (xbridgesession.cpp:2702).
	s.setOrderStatus("created")
	xlog.Info("deposit B broadcast", "order", orderID, "txid", out.out.txid,
		"lockTime", out.out.lockTime, "makerDeposit", s.theirDepositTxID)
	xlog.Debug("deposit B refund pre-signed", "order", orderID)
	// Swap transcript (dedicated log-tx file): only now that the broadcast is
	// confirmed — the transcript must never claim an unconfirmed broadcast.
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

// OnConfirmA (hub→maker's dest) → ConfirmedA (19): redeem taker's deposit B by
// revealing our secret on-chain, then broadcast A's payTx.
//
// Three-phase: stage 1 (engine) records the taker deposit; the worker
// drift-checks its lockTime and builds the claim (no broadcast); the phase-1
// resume persists the intent and posts the broadcast; the phase-2 resume
// applies the confirmed claim, sends the ConfirmedA response, and persists.
func (s *SwapSession) OnConfirmA(b *proto.ConfirmABody) (proto.XBridgeCommand, responseBody, error) {
	orderID := hexEncode(s.id[:])
	if !s.isMaker {
		return 0, nil, fmt.Errorf("api: ConfirmA received by taker session %s", orderID)
	}
	// C++ processTransactionConfirmA drops a ConfirmA once the transaction
	// already reached trCommited (xbridgesession.cpp:2897). A retransmit AFTER
	// we redeemed must not re-broadcast a second claim payTx.
	if s.state >= csConfirmedA {
		xlog.Info("ConfirmA ignored: swap already past claim", "order", orderID, "state", s.state.String())
		return 0, nil, nil
	}
	// A retransmit arriving WHILE the claim worker is in flight (await set)
	// must not re-post a second redeem task — that would double-broadcast the
	// claim payTx. The in-flight worker will complete and send ConfirmedA.
	if s.await {
		xlog.Info("ConfirmA ignored: claim worker in flight", "order", orderID)
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
	task := makerClaimBuildTask(s, c)
	if !s.n.engineRunning.Load() {
		v, terr := safeTaskRun(task)
		if terr != nil {
			// Mirror the started-mode resume (selfCancelErr must still fire).
			s.applyConfirmedA(nil, terr)
			return 0, nil, terr
		}
		return proto.XbcTransactionConfirmedA, s.applyConfirmedA(v, nil), nil
	}
	s.holdAwait()
	s.n.postSwapTask(orderID, task)
	return 0, nil, nil // deferred; applyConfirmedA sends on worker completion
}

// applyConfirmedA is the engine-side resume for a ConfirmA claim BUILD task
// (phase 1): it adopts the validated counterparty out-params, durably persists
// the claim intent via persistNow, and posts the broadcast task — it never
// broadcasts itself (see applyCreatedA for the phase contract). A persist
// failure withholds the broadcast (fail closed); the hub retransmits and the
// claim is rebuilt, as does the tick-driven claim retry.
func (s *SwapSession) applyConfirmedA(v any, terr error) responseBody {
	orderID := hexEncode(s.id[:])
	if terr != nil {
		s.releaseAwait()
		if s.failSelfCancel(terr) {
			return nil
		}
		xlog.Error("ConfirmA claim task failed", "order", orderID, "err", terr)
		// Transient until proven otherwise (backend lag, mempool blindness):
		// schedule a tick-driven rebuild instead of stranding at createdA
		// when the hub stays quiet. A built claim (claimTxID set) never
		// reaches here — only the no-broadcast case retries.
		if s.claimTxID == "" {
			scheduleClaimRetry(s, NowMicro(), "maker", terr)
		}
		return nil
	}
	out := v.(confirmOutcome)
	clearClaimRetry(s)
	s.theirDepositVout = out.check.DepositVout
	s.theirP2SHNative = out.check.P2SHNative
	s.theirOverpayment = out.check.Excess
	// Durable intent BEFORE broadcast: from here on, a crash restores the
	// session identity, the counterparty deposit pointers
	// (theirDepositTxID/LockTime/SecretHash plus the validated
	// vout/P2SHNative), and the built claim itself (claimHex/TxID/Cur) from
	// disk as the manual-recovery record. The automatic path still rebuilds
	// on hub redelivery; persistNow only guarantees nothing is lost.
	s.claimHex, s.claimTxID, s.claimCur = out.payHex, out.payTxID, out.cur
	if err := s.n.persistNow(); err != nil {
		s.releaseAwait()
		xlog.Error("ConfirmA intent persist failed, claim withheld", "order", orderID, "err", err)
		return nil
	}
	// Transcript of the built (not yet broadcast) claim.
	txLogClaimBuilt(s.id, "A", out.cur, out.payHex)
	// Nil in started mode (phase 2 sends the response); the phase-2 body in
	// inline mode (synchronous broadcast).
	var body responseBody
	s.n.postBroadcastTask(orderID, out.conn, out.cur, out.payHex, broadcastClaim, func(sentID string, berr error) {
		body = s.applyConfirmedABroadcast(out, sentID, berr)
	})
	return body
}

// applyConfirmedABroadcast is the engine-side resume for a ConfirmA claim
// BROADCAST task (phase 2): it records the confirmed claim, writes the
// transcript entry, and sends the ConfirmedA response (started mode) or
// returns it (inline mode). A broadcast failure leaves the session at its
// pre-claim state with the intent durable — resumption comes from hub
// redelivery (rebuild), the claim-retry sweep, or cancel (the counterparty deposit is untouched).
func (s *SwapSession) applyConfirmedABroadcast(out confirmOutcome, sentID string, terr error) responseBody {
	orderID := hexEncode(s.id[:])
	s.releaseAwait()
	if terr != nil {
		xlog.Error("ConfirmA claim broadcast failed", "order", orderID, "err", terr)
		// The intent (hex + txid) is durable: schedule a tick-driven repost
		// of the IDENTICAL bytes instead of stranding pre-confirmed.
		if s.claimTxID != "" && s.claimHex != "" {
			scheduleClaimRetry(s, NowMicro(), "maker-broadcast", terr)
		}
		return nil
	}
	clearClaimRetry(s)
	// The wallet's reported id is logged but never adopted: the locally
	// derived payTx id stays authoritative.
	if sentID != "" && sentID != out.payTxID {
		xlog.Warn("ConfirmA: wallet reported a different sent txid", "order", orderID, "local", out.payTxID, "sent", sentID)
	}
	payTxID := out.payTxID
	s.state = csConfirmedA
	xlog.Info("ConfirmA: payTx broadcast", "order", orderID, "payTxID", payTxID)
	// Swap transcript (dedicated log-tx file): only now that the broadcast is
	// confirmed — the transcript must never claim an unconfirmed broadcast.
	txLogClaim(s.id, "A", out.cur, payTxID, out.payHex)
	// Counterparty-deposit redeemed (C++ hasRedeemedCounterpartyDeposit()); the
	// validated deposit out-params feed the order record.
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

// OnConfirmB (hub→taker's dest) → ConfirmedB (21): recover the secret from A's
// payTx, then redeem maker's deposit A.
//
// Three-phase: stage 1 (engine) is pure; the worker fetches the maker's payTx,
// recovers the secret, and builds the claim (no broadcast); the phase-1 resume
// persists the intent (including the secret) and posts the broadcast; the
// phase-2 resume applies the confirmed claim, sends the ConfirmedB response,
// and persists.
func (s *SwapSession) OnConfirmB(b *proto.ConfirmBBody) (proto.XBridgeCommand, responseBody, error) {
	orderID := hexEncode(s.id[:])
	if s.isMaker {
		return 0, nil, fmt.Errorf("api: ConfirmB received by maker session %s", orderID)
	}
	// C++ processTransactionConfirmB drops a ConfirmB once the transaction
	// already reached trCommited (xbridgesession.cpp:3152). A retransmit AFTER
	// we redeemed must not re-broadcast a second claim payTx.
	if s.state >= csConfirmedB {
		xlog.Info("ConfirmB ignored: swap already past claim", "order", orderID, "state", s.state.String())
		return 0, nil, nil
	}
	// A retransmit arriving WHILE the claim worker is in flight (await set)
	// must not re-post a second redeem task — that would double-broadcast the
	// claim payTx. The in-flight worker will complete and send ConfirmedB.
	if s.await {
		xlog.Info("ConfirmB ignored: claim worker in flight", "order", orderID)
		return 0, nil, nil
	}
	// Record the maker's payTx id on the session BEFORE the build: a failed
	// build must still know its trigger so the tick retry can rebuild without
	// a hub retransmit (the packet is ephemeral). Durable via the async
	// persist; a crash before the flush falls back to hub redelivery.
	s.theirPayTxID = b.APayTxID
	s.n.persist()
	c := s.snapshot()
	task := takerClaimBuildTask(s, c, b.APayTxID)
	if !s.n.engineRunning.Load() {
		v, terr := safeTaskRun(task)
		if terr != nil {
			// Mirror the started-mode resume (selfCancelErr must still fire).
			s.applyConfirmedB(nil, terr)
			return 0, nil, terr
		}
		return proto.XbcTransactionConfirmedB, s.applyConfirmedB(v, nil), nil
	}
	s.holdAwait()
	s.n.postSwapTask(orderID, task)
	return 0, nil, nil // deferred; applyConfirmedB sends on worker completion
}

// applyConfirmedB is the engine-side resume for a ConfirmB claim BUILD task
// (phase 1): it adopts the worker-recovered secret, durably persists the
// claim intent via persistNow, and posts the broadcast task — it never
// broadcasts itself (see applyCreatedA for the phase contract). A persist
// failure withholds the broadcast (fail closed); the hub retransmits and the
// claim is rebuilt (the secret is re-recoverable from the maker's payTx),
// as does the tick-driven claim retry.
func (s *SwapSession) applyConfirmedB(v any, terr error) responseBody {
	orderID := hexEncode(s.id[:])
	if terr != nil {
		s.releaseAwait()
		xlog.Error("ConfirmB claim task failed", "order", orderID, "err", terr)
		// Transient until proven otherwise (backend lag, mempool blindness):
		// schedule a tick-driven rebuild instead of stranding at createdB
		// when the hub stays quiet (live-proven e6730fe2). A built claim
		// (claimTxID set) never reaches here — only the no-broadcast case
		// retries, so a retry can never double-broadcast.
		if s.claimTxID == "" {
			scheduleClaimRetry(s, NowMicro(), "taker", terr)
		}
		return nil
	}
	out := v.(confirmOutcome)
	clearClaimRetry(s)
	s.secret = out.secret
	// Durable intent BEFORE broadcast: from here on, a crash restores the
	// recovered secret, the counterparty deposit pointers, and the built
	// claim itself (claimHex/TxID/Cur) from disk as the manual-recovery
	// record (see applyConfirmedA).
	s.claimHex, s.claimTxID, s.claimCur = out.payHex, out.payTxID, out.cur
	if err := s.n.persistNow(); err != nil {
		s.releaseAwait()
		xlog.Error("ConfirmB intent persist failed, claim withheld", "order", orderID, "err", err)
		return nil
	}
	// Transcript of the built (not yet broadcast) claim.
	txLogClaimBuilt(s.id, "B", out.cur, out.payHex)
	// Nil in started mode (phase 2 sends the response); the phase-2 body in
	// inline mode (synchronous broadcast).
	var body responseBody
	s.n.postBroadcastTask(orderID, out.conn, out.cur, out.payHex, broadcastClaim, func(sentID string, berr error) {
		body = s.applyConfirmedBBroadcast(out, sentID, berr)
	})
	return body
}

// applyConfirmedBBroadcast is the engine-side resume for a ConfirmB claim
// BROADCAST task (phase 2): it records the confirmed claim, writes the
// transcript entry, and sends the ConfirmedB response (started mode) or
// returns it (inline mode). A broadcast failure leaves the session at its
// pre-claim state with the intent (and secret) durable — resumption comes
// from hub redelivery (rebuild) or cancel.
func (s *SwapSession) applyConfirmedBBroadcast(out confirmOutcome, sentID string, terr error) responseBody {
	orderID := hexEncode(s.id[:])
	s.releaseAwait()
	if terr != nil {
		xlog.Error("ConfirmB claim broadcast failed", "order", orderID, "err", terr)
		// The intent (hex + txid) is durable: schedule a tick-driven repost
		// of the IDENTICAL bytes instead of stranding pre-confirmed.
		if s.claimTxID != "" && s.claimHex != "" {
			scheduleClaimRetry(s, NowMicro(), "taker-broadcast", terr)
		}
		return nil
	}
	clearClaimRetry(s)
	// The wallet's reported id is logged but never adopted: the locally
	// derived payTx id stays authoritative.
	if sentID != "" && sentID != out.payTxID {
		xlog.Warn("ConfirmB: wallet reported a different sent txid", "order", orderID, "local", out.payTxID, "sent", sentID)
	}
	s.state = csConfirmedB
	xlog.Info("ConfirmB: payTx broadcast", "order", orderID, "payTxID", out.payTxID)
	// Swap transcript (dedicated log-tx file): only now that the broadcast is
	// confirmed — the transcript must never claim an unconfirmed broadcast.
	txLogClaim(s.id, "B", out.cur, out.payTxID, out.payHex)
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

// runRefundTask executes a refund broadcast for conn/refundHex. When checkLock
// is set it only broadcasts once the chain is at/above lockTime (the sweep
// path); otherwise it broadcasts immediately (the cancel/rollback/escape-hatch
// paths, matching C++ force-refund semantics). Returns ("", nil) when the
// refund is not yet due — the sweep retries on the next tick. Runs on a worker
// goroutine; self-contained (all inputs captured by value, including the
// connector captured at enqueue time). cur names the coin for error
// context only.
func runRefundTask(conn wallet.Connector, cur, refundHex string, lockTime uint32, checkLock bool) (string, error) {
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
// queue drops the task (the broadcast is idempotent and a later retry — the
// sweep for live sessions, a manual call for stored orders — re-enqueues it),
// clears the guard so that retry can re-enqueue it, and still invokes done with
// an error so a caller awaiting the outcome (tryStoredRefund's chained
// candidates, BroadcastRefund) never blocks forever on the drop.
func (n *Node) postRefundTask(orderID, cur, refundHex string, lockTime uint32, checkLock bool, done func(txid string, err error)) bool {
	// Capture the connector at enqueue time (engine side) so a mid-task reload
	// cannot swap which wallet broadcasts the refund.
	conn := n.cfg().Connectors[cur]
	task := workTask{
		orderID: orderID,
		run: func() (any, error) {
			return runRefundTask(conn, cur, refundHex, lockTime, checkLock)
		},
		apply: func(v any, err error) {
			delete(n.pendingRefunds, orderID)
			txid, _ := v.(string)
			if done != nil {
				done(txid, err)
			}
			if err != nil {
				xlog.Warn("refund broadcast failed", "order", orderID, "err", err)
				// C++ redeemOrderDeposit (xbridgesession.cpp:3852-3908): a failed
				// refund broadcast sets trRollbackFailed for any order at state >=
				// trCreated, then the watcher retries later (:3875-3888). The same
				// function serves both the cancel rollback path (:3415) and the
				// fund-safety deposit-spend watch (xbridgeapp.cpp:3441). Go's
				// scanRefunds is its analog and only sweeps sessions that have
				// broadcast a deposit (csCreatedA+).
				if n.rollbackGate(orderID) {
					n.store.Update(orderID, func(o *Order) {
						// Never clobber a user-canceled/terminal order (C++
						// never produces trRollbackFailed for trCancelled), and
						// stay idempotent across repeated failed retries.
						if !isOrderTerminal(o.Status) && o.Status != "rollback failed" {
							o.Status = "rollback failed"
							o.Updated = NowMicro()
						}
					})
					n.persist()
				}
				// Sweep-driven and cancel-path fire-and-forget attempts
				// (done == nil) back off instead of retrying every sweep;
				// manual BroadcastRefund (done != nil) always bypasses the
				// gate, so explicit operator intent is unaffected.
				if done == nil {
					n.recordRefundFailure(orderID)
				}
				return
			}
			if txid == "" {
				return // not yet refundable; the next sweep retries
			}
			xlog.Info("refund broadcast", "order", orderID, "txid", txid)
			n.clearRefundBackoff(orderID)
			// Track for confirmation watch (Phase 0): wallet acceptance is
			// not chain inclusion. Txid derived locally, never adopted.
			if localID, lerr := txIDFromHex(refundHex); lerr == nil {
				n.recordBroadcast(orderID, broadcastRefund, cur, localID, refundHex)
			} else {
				xlog.Warn("reconcile: refund untrackable, bad hex", "order", orderID, "cur", cur)
			}
			// Swap transcript (dedicated log-tx file): tie the pre-signed refund
			// to its on-chain txid for manual verification.
			if s := n.sessions[orderID]; s != nil {
				s.refundDone = true
				txLogRefund(s.id, cur, lockTime, txid)
			} else if o := n.store.Get(orderID); o != nil {
				txLogRefund(o.ID, cur, lockTime, txid)
			}
			// C++ redeemOrderDeposit success sets trRollback (:3911): a deposit
			// redeemed via the refund is rolled back, whether the prior attempt
			// wrote "rollback failed" or the order was still mid-swap.
			if n.rollbackGate(orderID) {
				n.store.Update(orderID, func(o *Order) {
					if !isOrderTerminal(o.Status) && o.Status != "rolled back" {
						o.Status = "rolled back"
						o.Updated = NowMicro()
					}
				})
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
		// The task never ran, so done must still fire: a caller awaiting its
		// outcome (tryStoredRefund's chained candidates, BroadcastRefund) would
		// otherwise block forever. A later retry re-enqueues.
		if done != nil {
			done("", errors.New("api: refund task dropped, engine busy"))
		}
		return false
	}
}

// orderDepositSent reports whether the order's deposit broadcast confirmed
// (phase 2 recorded DepositSent). Since intent-before-broadcast, a persisted
// refund intent alone no longer proves an on-chain deposit — this flag is the
// proof. Engine- or test-side; store.Get is lock-guarded.
func (n *Node) orderDepositSent(orderID string) bool {
	o := n.store.Get(orderID)
	return o != nil && o.DepositSent
}

// rollbackGate reports whether C++ redeemOrderDeposit's state gate
// (state >= trCreated, xbridgesession.cpp:3852-3854) holds for orderID. The
// session state is authoritative — csCreatedA is the deposit-broadcast state,
// and scanRefunds only sweeps sessions past it (swap.go:1205); the store order
// is the fallback for the stored-order escape hatch, where no live session
// exists, keyed on RefundTx (set exactly when the deposit was built, C++
// trCreated — covering the taker too, whose stored status now advances hold/initialized/created like the maker's, so the fallback keys on RefundTx rather than a status string).
// Engine-owned (reads n.sessions); call from the engine or inline tests.
func (n *Node) rollbackGate(orderID string) bool {
	if s := n.sessions[orderID]; s != nil {
		// The broadcast-confirmed state advances past csCreatedA in phase 2;
		// a crash-recovered session keeps its pre-created state but carries
		// a verified DepositSent — both prove an on-chain deposit.
		return s.state >= csCreatedA || n.orderDepositSent(orderID)
	}
	if o := n.store.Get(orderID); o != nil {
		// Session-less fallback: the stored refund alone no longer proves an
		// on-chain deposit (phase 1 persists it pre-broadcast) — require the
		// verified DepositSent too.
		return o.RefundTx != "" && o.DepositSent
	}
	return false
}

// postSwapTask posts a three-phase handshake task (deposit build / claim) to the
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

// postBroadcastTask posts a raw-hex chain broadcast (deposit / claim) to the
// worker pool after the engine has durably persisted the intent, so a crash
// between broadcast and resume can always recover (the refund was persisted
// first). conn is the snapshot wallet connector the hex was built against —
// captured by the worker from its build-time snapshot, never re-resolved
// post-reload — so a dxLoadXBridgeConf landing mid-task cannot swap which
// wallet broadcasts (the snapshot exists for exactly this). apply runs on the
// engine with the wallet-reported sent txid. A full task queue drops the
// broadcast and clears the session's await guard so the hub's retransmit
// re-stages it (fund-safe: the intent is durable, nothing was broadcast).
// Inline mode (tests) runs synchronously.
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
				// Track the broadcast for confirmation watch (Phase 0):
				// wallet acceptance is not chain inclusion (live-proven S2
				// hole). The txid is derived locally, never adopted.
				if localID, lerr := txIDFromHex(hex); lerr == nil {
					n.recordBroadcast(orderID, kind, cur, localID, hex)
				} else {
					xlog.Warn("reconcile: broadcast untrackable, bad hex", "order", orderID, "cur", cur)
				}
				// Our broadcast went out: watchdog progress (engine-side).
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
			s.await = false
		}
		xlog.Warn("broadcast task dropped, engine busy", "order", orderID)
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
		if s.refundDone || s.refundHex == "" || s.state == csFinished {
			continue
		}
		if n.refundBackoffActive(id) {
			continue
		}
		// No on-chain deposit, no refund: an intent persisted pre-broadcast
		// (or a state still pre-created) means the deposit may never have
		// broadcast — refunding it would be a harmless wallet reject, but the
		// correct paths are hub redelivery (rebuild) or cancel. A
		// crash-recovered session verified on-chain carries DepositSent and
		// IS swept. The store lookup runs only for pre-created states.
		if s.state < csCreatedA && !n.orderDepositSent(id) {
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

// watchCounterpartyDeposits polls the validated counterparty deposit of every
// live pre-claim session and cancels when it disappears (spent or reorged).
// It is the thin-client counterpart of C++ watchForSpentDeposit plus the
// fund-safety deposit-spend sweep (xbridgeapp.cpp:3441): without a wallet-side
// mempool watcher this port cannot extract the secret from an unknown
// spending transaction, so a vanished deposit fails over to the safe path —
// wire-Cancel with the role's deposit reason (crBadA/BDepositTx, the same
// reasons the CreateB/ConfirmA validation uses) and rollback to our own
// pre-signed refund — instead of stalling until our own locktime or
// broadcasting a claim that can never confirm. A wallet error is transient
// (skip the round); only a definitive gettxout-missing cancels. Runs on the
// engine goroutine; each eligible session posts a worker task guarded by
// pendingWatch so no session is ever double-polled. Inline mode (tests)
// probes synchronously.
func (n *Node) watchCounterpartyDeposits() {
	for id, s := range n.sessions {
		if s.theirDepositTxID == "" || s.await {
			continue
		}
		// Watch only the pre-claim window: once we claimed, the deposit is
		// legitimately spent by our own payTx (CounterpartyRedeemed / the
		// confirmed state records it).
		if s.isMaker {
			if s.state < csCreatedA || s.state >= csConfirmedA {
				continue
			}
		} else if s.state < csCreatedB || s.state >= csConfirmedB {
			continue
		}
		if n.store != nil {
			if o := n.store.Get(id); o != nil && (isOrderTerminal(o.Status) || o.CounterpartyRedeemed) {
				continue
			}
		}
		if n.engineRunning.Load() {
			// Started mode: guard against double-enqueue. Inline mode
			// (tests) runs synchronously, so the guard is redundant there.
			if n.pendingWatch[id] {
				continue
			}
			n.pendingWatch[id] = true
		}
		n.postDepositWatchTask(id)
	}
}

// runDepositWatchTask probes whether the watched counterparty deposit output
// is still unspent. It returns (true, nil) when unspent, (false, nil) when
// definitively spent or missing (reorged/double-spent), and an error when the
// wallet cannot answer (transient: skip the round, never cancel on it).
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

// postDepositWatchTask posts one counterparty-deposit probe to the worker pool
// (or runs it synchronously in inline mode). The apply runs on the engine: a
// vanished deposit self-cancels with the role's deposit reason; any other
// outcome leaves the session untouched for the next sweep. A full task queue
// drops the probe (the next sweep re-enqueues; nothing was broadcast).
func (n *Node) postDepositWatchTask(orderID string) bool {
	s := n.sessions[orderID]
	if s == nil {
		return false
	}
	conn := n.cfg().Connectors[s.dstCur]
	// Capture plain values for the worker: the session is engine-owned and
	// must never be read off the engine goroutine (race detector).
	dstCur, depTxID, depVout := s.dstCur, s.theirDepositTxID, s.theirDepositVout
	task := workTask{
		orderID: orderID,
		run: func() (any, error) {
			return runDepositWatchTask(conn, dstCur, depTxID, depVout)
		},
		apply: func(v any, err error) {
			delete(n.pendingWatch, orderID)
			if err != nil {
				xlog.Debug("deposit watch probe unavailable, skipping round", "order", orderID, "err", err)
				return
			}
			if unspent, _ := v.(bool); unspent {
				return
			}
			// Re-resolve the session: it may have finished, been
			// cancelled, or been pruned while the probe was in flight.
			s := n.sessions[orderID]
			if s == nil {
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
	// The sweep covers broadcast-confirmed states (csCreatedA+) and
	// crash-recovered sessions verified on-chain (DepositSent) alike.
	if s.refundHex != "" && !s.refundDone && (s.state >= csCreatedA || n.orderDepositSent(id)) {
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
			n.clearRefundBackoff(id)
			delete(n.sessions, id)
			xlog.Info("swap session pruned", "order", id, "state", s.state.String())
		}
	}
}

// enqueueRefund schedules a fund-recovery refund broadcast for orderID: the
// live session's pre-signed refund when present, else the order's stored refund
// hex (the escape-hatch fallback, trying each deposit chain). Fire-and-forget
// from the engine (done == nil); the optional done callback receives the
// outcome when it lands. Inline mode (tests) runs synchronously and invokes
// done before returning.
func (n *Node) enqueueRefund(orderID string, done func(txid string, err error)) {
	// A persisted intent (refundHex present) is not proof of an on-chain
	// deposit: since intent-before-broadcast, the refund is durable before the
	// deposit broadcasts. Only attempt the refund when the deposit was
	// actually sent — either the state machine advanced past the broadcast
	// (phase 2) or the order carries a verified DepositSent (crash recovery).
	// (C++ redeemOrderDeposit is a no-op below trCreated.)
	if s := n.sessions[orderID]; s != nil && s.refundHex != "" {
		if s.state < csCreatedA && !n.orderDepositSent(orderID) {
			if done != nil {
				done("", fmt.Errorf("api: no deposit broadcast for order %s", orderID))
			}
			return
		}
		// Take the pendingRefunds guard so a concurrent sweep
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
	if o == nil || o.RefundTx == "" || !o.DepositSent {
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

// computeLockTimeFor returns the absolute block height to embed in the deposit
// HTLC for a given coin (mirrors C++ lockTime(): currentBlock + target/blockTime).
// It is also used to compute our expectation of the counterparty's lockTime so we
// can validate it via acceptableLockTimeDrift. Runs on a worker in the three-phase
// handshake, so it reads config/connectors only — never live session state.
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
// wallet connector, signs the funding inputs via the wallet, and pre-builds
// the CLTV refund — but does NOT broadcast (C++ builds deposit → refund →
// broadcast; the broadcast is a separate engine-gated step so the refund is
// durable first). It returns the signed hex + locally-derived txid + lockTime
// and the pre-signed refund hex as a depositOutcome. Runs on a worker in the
// three-phase handshake; it reads only this snapshot and lock-guarded config,
// and performs no session/order mutation — those happen in the engine-side
// resume.
func (c *swapCtx) buildDeposit(isMaker bool) (depositOutcome, error) {
	cur := c.srcCur
	amt := c.srcAmt
	conn := c.connectors[cur]
	cc := c.conf(cur)
	if conn == nil {
		return depositOutcome{}, fmt.Errorf("api: no connector for %s", cur)
	}
	coin, ok := c.coin(cur)
	if !ok {
		return depositOutcome{}, fmt.Errorf("api: unknown coin %s", cur)
	}
	// Spend the recorded make/take-time selection (Order.UsedCoins,
	// C++ xtx->usedCoins), never a fresh ListUnspent.
	funding := c.funding
	if len(funding) == 0 {
		return depositOutcome{}, fmt.Errorf("api: no funding UTXOs for %s (order has no used coins)", cur)
	}
	// Fail closed on an unavailable lockTime (C++ cancels when
	// lockTime==0||opponentLockTime==0, xbridgesession.cpp:2037-2043): a
	// zero-lockTime HTLC is immediately refundable and the counterparty's
	// drift check would reject it, burning fees for a doomed swap. The
	// opponent-zero half is covered at the validation points: a zero
	// counterparty lockTime fails acceptableLockTimeDrift (theirLT==0 makes
	// diff*blockTime exceed any drift) into selfCancel crBadALockTime /
	// crBadBLockTime at OnCreateB / OnConfirmA.
	lockTime := c.computeLockTime(isMaker)
	if lockTime == 0 {
		return depositOutcome{}, fmt.Errorf("api: locktime unavailable for %s (getblockcount failed)", cur)
	}
	// Change returns to the largest funding UTXO's address (C++
	// largestUtxo.address, xbridgesession.cpp:2102): a known-good,
	// wallet-watched address. A fresh GetNewAddress is only the fallback when
	// the funding address cannot be decoded (e.g. test fixtures without one).
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
	// The deposit network fee uses minTxFee1(nIn, 3) — C++ computes
	// fee1 over the deposit with three outputs (p2sh + change + dust safety),
	// xbridgesession.cpp:1994 (maker) / :2526 (taker).
	fee := estimateFee(cc, len(funding), 3)
	// fee2 is the p2sh redeem margin C++ locks into the HTLC output on top of
	// the order amount (minTxFee2(1,1), xbridgesession.cpp:2094/:2615); it is
	// collected when the deposit is claimed or refunded.
	fee2 := estimateFee(cc, 1, 1)
	xlog.Debug("buildDeposit: plan", "order", c.orderID, "isMaker", isMaker,
		"cur", cur, "amountXB", amt, "lockTime", lockTime, "txVersion", c.txVersion(cur),
		"utxos", len(funding), "fee", fee, "fee2", fee2)

	hash := c.secretHash
	if !isMaker {
		hash = c.theirSecretHash
	}
	// The on-chain deposit locks NATIVE base units.
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
	tx, err := spec.BuildDepositTx(coin, funding, change, fee, fee2, depositDustLimit(cc, conn))
	if err != nil {
		return depositOutcome{}, err
	}
	// The P2SH HTLC is always output 0; record its exact value so buildRefundTx
	// can commit it to the forkid digest without recomputing fee2.
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
	// Derive the deposit txid LOCALLY (C++ binTxId comes from
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

	// No broadcast here: the engine persists this intent (refundHex + txid +
	// lockTime) via persistNow BEFORE the broadcast task runs, so a crash
	// between build and broadcast loses nothing.
	return depositOutcome{txid: localTxID, lockTime: lockTime, refundHex: refundHex, depositHex: signed, conn: conn}, nil
}

// buildRefundTx pre-signs the IF-branch (CLTV) refund that returns the deposit to
// our source address, spendable only after the deposit's lockTime. Runs on a
// worker in the three-phase handshake (reads only this snapshot + config).
func (c *swapCtx) buildRefundTx(spec *swap.DepositSpec, cur string) (string, error) {
	coin, ok := c.coin(cur)
	if !ok {
		return "", fmt.Errorf("api: unknown coin %s", cur)
	}
	dest, err := c.destScript(cur, c.ourSourceAddr)
	if err != nil {
		return "", err
	}
	// For forkid coins the digest commits the spent output's exact
	// value. The refund spends our deposit's P2SH output (outAmount+fee2,
	// xbridgesession.cpp:2135), whose value was recorded at build time as
	// c.ourDepositP2SH (never recomputed, so a conf hot-reload can't skew it).
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
	// The refund pays the FULL nominal amount — C++ refund output is
	// outAmount (xbridgesession.cpp:2149), spending the deposit's outAmount+fee2
	// output, so fee2 is the refund's implicit miner fee. Was spec.Amount - fee
	// (a second fee2 deduction).
	tx.Outputs = append(tx.Outputs, coins.TxOut{Value: spec.Amount, ScriptPubKey: dest})

	inner := spec.RedeemScript()
	sig, err := coins.SignTxInputForCoin(tx, 0, inner, depositP2SH, c.privKey[:], coin)
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
// the taker redeems the maker's BTC deposit. Runs on a worker in the three-phase
// handshake (reads only this snapshot + config); ConfirmB sets c.secret to the
// recovered preimage before calling.
// confirmDepositKnownByRawTx degrades the claim's unspent pre-check for
// backends whose gettxout is blind outside their wallet (code -5): the
// deposit counts as known when the verbose raw tx carries the exact validated
// vout (script hex + native value) at the required confirmation depth.
// Anything less — unknown tx, shallow confs, missing vout, value or script
// mismatch — returns false (keep waiting). Spent status is opaque here by
// construction; callers must only invoke it after a -5, never on a spent
// proof (ok=false, nil error), which always waits.
func confirmDepositKnownByRawTx(conn wallet.Connector, txid string, vout uint32, scriptHex string, value uint64, minConf int) bool {
	vtx, verr := conn.GetRawTransactionVerbose(txid)
	if verr != nil {
		return false
	}
	if vtx.Confirmations < minConf {
		return false
	}
	out, ok := vtx.Outputs[vout]
	if !ok || out.Value != value || out.ScriptHex != scriptHex {
		return false
	}
	return true
}

func (c *swapCtx) redeemCounterparty(isMaker bool) (payHex, depositCur string, err error) {
	depositCoin, ok := c.coin(c.dstCur)
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
	// Spend the VALIDATED counterparty deposit (C++ oBinTxVout /
	// oBinTxP2SHAmount, recorded by the deposit check) instead of the hardcoded
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
	// Re-check the counterparty deposit is still unspent immediately before
	// building the claim. C++ watches the deposit spend continuously
	// (watchForSpentDeposit / the fund-safety sweep, xbridgeapp.cpp:3441); a
	// point-in-time validation alone would let a reorged or double-spent
	// deposit produce a claim that can never confirm. A missing output fails
	// the task (hub redelivery re-validates from scratch) instead of
	// broadcasting a doomed spend.
	//
	// Facade-blind gettxout: non-Core backends answer code -5 for any
	// non-wallet tx, so a confirmed counterparty deposit can never read
	// unspent there (live-proven: the claim stalled forever on a 15-deep
	// deposit). On -5 ONLY, degrade to confirmDepositKnownByRawTx: the exact
	// validated vout (script + value) at the validation confirmation depth.
	// Spent status stays opaque in that path, but a claim broadcast on a
	// spent output cannot confirm — proceeding is fund-safe, stalling strands
	// funds. A spent proof (ok=false, nil error) always waits, on any backend.
	if conn, ok := c.connectors[depositCur]; ok && conn != nil {
		if _, unspent, gerr := conn.GetTxOut(c.theirDepositTxID, c.theirDepositVout); gerr != nil || !unspent {
			degraded := false
			if code, isRPC := wallet.RPCErrorCode(gerr); isRPC && code == -5 {
				degraded = confirmDepositKnownByRawTx(conn, c.theirDepositTxID, c.theirDepositVout,
					hex.EncodeToString(theirSpec.P2SHScript()), p2shNative, c.minConf(c.conf(depositCur)))
			}
			if !degraded {
				if gerr != nil {
					xlog.Debug("redeemCounterparty: deposit unspent check unavailable (transient)", "order", c.orderID, "err", gerr)
				} else {
					xlog.Debug("redeemCounterparty: counterparty deposit spent or missing (reorg?)", "order", c.orderID, "deposit", c.theirDepositTxID, "vout", c.theirDepositVout)
				}
				return "", "", fmt.Errorf("api: counterparty deposit %s:%d not unspent, awaiting redelivery", c.theirDepositTxID, c.theirDepositVout)
			}
			xlog.Info("redeemCounterparty: gettxout-blind backend, proceeding on verbose raw-tx match", "order", c.orderID, "deposit", c.theirDepositTxID, "vout", c.theirDepositVout)
		}
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
	// For forkid coins the digest commits the exact spent value —
	// the validated deposit P2SH amount (C++ oBinTxP2SHAmount,
	// xbridgesession.cpp:3967) — not the nominal order amount.
	sig, err := coins.SignTxInputForCoin(tx, 0, inner, p2shNative, c.privKey[:], depositCoin)
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

// conf returns the per-coin conf from this task's snapshot, so a
// mid-task reload cannot change the minConf/txVersion/blockTime a deposit or
// claim is built with.
func (c *swapCtx) conf(cur string) *config.CoinConf {
	if c.confs != nil {
		return c.confs[cur]
	}
	return nil
}

// coin returns the coin value for cur from this task's snapshot, so
// a mid-task reload cannot change the decimals/prefix/codec a deposit, refund,
// or claim is built with — the C++ session holds the coin parameters it
// captured at task start. The snapshot covers the session's own two currencies
// (srcCur/dstCur); anything else reports not-found, mirroring the live
// registry's unknown-coin semantics.
func (c *swapCtx) coin(cur string) (coins.Coin, bool) {
	if c.coinsMap == nil {
		return coins.Coin{}, false
	}
	coin, ok := c.coinsMap[cur]
	return coin, ok
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

// depositDustLimit returns the minimum non-dust deposit-change value for the
// coin (C++ isDustAmount, xbridgewalletconnectorbtc.cpp:1900-1906): the live
// relay fee when the wallet reports one, else the conf MinimumAmount, else
// C++'s 5460 constant — the same effectiveDust chain the order paths use. A
// relay error yields 0 relay fee (fail open to the conf fallbacks, never to a
// zero dust limit that would re-admit dust change).
func depositDustLimit(cc *config.CoinConf, conn wallet.Connector) uint64 {
	var relayFee float64
	if conn != nil {
		relayFee, _ = conn.GetRelayFee()
	}
	return effectiveDust(cc, relayFee)
}

// ownDepositVout is the output index of our own deposit's P2SH HTLC output.
// BuildDepositTx always emits it as output 0 (change, if any, follows).
const ownDepositVout = 0

// secretFromPayTx extracts the 33-byte HTLC secret preimage from a serialized
// payTx, verifying it against the expected secretHash hx (the deposit's
// HashedSecret). C++ does the same in getSecretFromPaymentTransaction
// (xbridgewalletconnectorbtc.cpp:2241-2276), called at xbridgesession.cpp:3935
// with the taker's OWN deposit (binTxId, binTxVout=0 — createDepositTransaction
// stamps txVout=0, connectorbtc.cpp:2413): it first requires the vin to spend
// that outpoint (:2249-2254) and only then adopts a push whose getKeyId(push)
// equals hx. depTxID/depVout are that expected outpoint (display-order txid,
// usually our own deposit at vout 0); inputs not spending it are skipped, so
// a decoy payTx carrying the (already public) secret without spending the
// deposit can never satisfy extraction.
func secretFromPayTx(payHex string, hx [20]byte, hasTime bool, depTxID string, depVout uint32) ([33]byte, bool) {
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
// length. It is used to materialize a session's trusted hub key.
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

// txIDFromBytes returns the display-order txid of serialized tx bytes.
func txIDFromBytes(raw []byte) string {
	h1 := sha256.Sum256(raw)
	h2 := sha256.Sum256(h1[:])
	out := make([]byte, 32)
	for i := 0; i < 32; i++ {
		out[i] = h2[31-i]
	}
	return hex.EncodeToString(out)
}

// txIDFromHex returns the display-order txid of a serialized tx (used to label
// the locally-built refund/claim txs).
func txIDFromHex(rawHex string) (string, error) {
	raw, err := hex.DecodeString(rawHex)
	if err != nil {
		return "", err
	}
	return txIDFromBytes(raw), nil
}
