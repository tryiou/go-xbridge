package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	xlog "go-xbridge/log"
	"go-xbridge/proto"
	"go-xbridge/wallet"
)

// Byte arrays and []proto.UtxoEntry round-trip losslessly through the default JSON codec,
// so no bespoke serializer; changing their encoding would break existing files.
type persistedSwap struct {
	ID               [32]byte          `json:"id"`
	IsMaker          bool              `json:"isMaker"`
	Type             OrderType         `json:"type"`
	From             [20]byte          `json:"from"`
	To               [20]byte          `json:"to"`
	FromCurrency     string            `json:"fromCurrency"`
	ToCurrency       string            `json:"toCurrency"`
	FromAmount       uint64            `json:"fromAmount"`
	ToAmount         uint64            `json:"toAmount"`
	OrigFromAmount   uint64            `json:"origFromAmount"`
	OrigToAmount     uint64            `json:"origToAmount"`
	MinFromAmount    uint64            `json:"minFromAmount"`
	PartialAllowed   bool              `json:"partialAllowed"`
	PartialRepost    bool              `json:"partialRepost"`
	ParentID         [32]byte          `json:"parentID"`
	Created          uint64            `json:"created"`
	Updated          uint64            `json:"updated"`
	BlockHash        [32]byte          `json:"blockHash"`
	BlockNumber      uint32            `json:"blockNumber"`
	MakerPubkey      string            `json:"makerPubkey"`
	MakerAddress     string            `json:"makerAddress"`
	TakerAddress     string            `json:"takerAddress"`
	BlockID          string            `json:"blockID"`
	RefundTx         string            `json:"refundTx"`
	BinTxId          string            `json:"binTxId"`
	OBinTxId         string            `json:"oBinTxId"`
	OBinTxVout       uint32            `json:"oBinTxVout"`
	OBinTxP2SHAmount uint64            `json:"oBinTxP2SHAmount"`
	OOverpayment     uint64            `json:"oOverpayment"`
	Status           string            `json:"status"`
	Utxos            []proto.UtxoEntry `json:"utxos"`
	UtxoCurrency     string            `json:"utxoCurrency"`

	// The funding selection must survive restart: without it a pre-deposit
	// crash cannot rebuild the deposit and the swap stalls.
	UsedCoins []wallet.Utxo `json:"usedCoins"`

	SNodePubkey          string   `json:"sNodePubkey"`
	HubAddress           [20]byte `json:"hubAddress"`
	OtherPubkey          string   `json:"otherPubkey"`
	MakerKey             string   `json:"makerKey"`
	Reason               uint32   `json:"reason"`
	Role                 byte     `json:"role"`
	DepositSent          bool     `json:"depositSent"`
	CounterpartyRedeemed bool     `json:"counterpartyRedeemed"`
	OrigFromCurrency     string   `json:"origFromCurrency"`
	OrigToCurrency       string   `json:"origToCurrency"`

	SrcCur        string `json:"srcCur"`
	DstCur        string `json:"dstCur"`
	SrcAmt        uint64 `json:"srcAmt"`
	DstAmt        uint64 `json:"dstAmt"`
	OurSourceAddr string `json:"ourSourceAddr"`
	OurDestAddr   string `json:"ourDestAddr"`

	TheirPub   [33]byte `json:"theirPub"`
	PrivKey    [32]byte `json:"privKey"`
	PubKey     [33]byte `json:"pubKey"`
	Secret     [33]byte `json:"secret"`
	SecretHash [20]byte `json:"secretHash"`

	OurLockTime    uint32 `json:"ourLockTime"`
	OurDepositTxID string `json:"ourDepositTxID"`
	RefundHex      string `json:"refundHex"`
	RefundDone     bool   `json:"refundDone"`
	// SecretHunt re-arms taker-side secret recovery after a restart: a hunting
	// session must keep hunting, never fail its first post-restart refund.
	SecretHunt bool `json:"secretHunt,omitempty"`
	// HuntSince is wall micros from the hunt start (NowMicro); 0 = not clocked yet.
	// ScanCursor is the rescan's next chain height, 0 = unseeded.
	HuntSince  uint64 `json:"huntSince,omitempty"`
	ScanCursor uint32 `json:"scanCursor,omitempty"`
	// DepositHex is the signed deposit hex that lets the tick repost a failed
	// broadcast; pre-upgrade records lack it and simply never repost.
	DepositHex string `json:"depositHex,omitempty"`

	TheirDepositTxID string   `json:"theirDepositTxID"`
	TheirLockTime    uint32   `json:"theirLockTime"`
	TheirSecretHash  [20]byte `json:"theirSecretHash"`
	TheirPayTxID     string   `json:"theirPayTxID,omitempty"`
	// ClaimRetryAt, DepositRetryAt and NotReadySince are wall micros (NowMicro()); 0 = none.
	ClaimRetryAt   uint64 `json:"claimRetryAt,omitempty"`
	ClaimRetries   uint32 `json:"claimRetries,omitempty"`
	DepositRetryAt uint64 `json:"depositRetryAt,omitempty"`
	DepositRetries uint32 `json:"depositRetries,omitempty"`
	NotReadySince  uint64 `json:"notReadySince,omitempty"`
	// NotReadySeen is the admission's visibility verdict; absent on pre-upgrade
	// records, so a restarted wait resumes unseen and slows past the taper age.
	NotReadySeen bool `json:"notReadySeen,omitempty"`
	// The out-params must ride the session record: the claim's order copy
	// updates only at phase-2 broadcast, so a claim rebuilt from it is stale.
	TheirDepositVout uint32 `json:"theirDepositVout,omitempty"`
	TheirP2SHNative  uint64 `json:"theirP2SHNative,omitempty"`
	TheirOverpayment uint64 `json:"theirOverpayment,omitempty"`

	ClaimHex  string `json:"claimHex,omitempty"`
	ClaimTxID string `json:"claimTxID,omitempty"`
	ClaimCur  string `json:"claimCur,omitempty"`

	Hub    [20]byte    `json:"hub"`
	HubKey [33]byte    `json:"hubKey"`
	State  clientState `json:"state"`

	// HoldApplySentAt is the last HoldApply send time; restore re-stamps a zero
	// value so a pre-upgrade parked session resumes resending instead of skipping.
	HoldApplySentAt uint64 `json:"holdApplySentAt,omitempty"`

	Historical bool `json:"historical,omitempty"`
}

type persistedBroadcast struct {
	OrderID string        `json:"orderID"`
	Kind    broadcastKind `json:"kind"`
	Coin    string        `json:"coin"`
	TxID    string        `json:"txid"`
	Hex     string        `json:"hex"`
	Seq     uint64        `json:"seq,omitempty"`
	// Confs is the last observed confirmation depth; 0 = unknown (not the required depth).
	Confs          int    `json:"confs,omitempty"`
	FirstSeenMicro uint64 `json:"firstSeenMicro,omitempty"`
	Attempts       int    `json:"attempts,omitempty"`
}

// swapFile is the on-disk envelope, checksummed across swaps plus both broadcast sections:
// C++ pins a fixed RecordChecksum in SerializeFileDB, we hash the actual data — downgrades fail closed, back up first.
type swapFile struct {
	Sum        string               `json:"sum"`
	Swaps      []persistedSwap      `json:"swaps"`
	Broadcasts []persistedBroadcast `json:"broadcasts,omitempty"`
	Settled    []persistedBroadcast `json:"settled,omitempty"`
}

func swapStatePath(dir string) string {
	return filepath.Join(dir, "xbridged-swaps.json")
}

// snapshotSwaps must run on the engine goroutine: it reads n.sessions, which the engine alone owns (no lock).
func snapshotSwaps(n *Node) []persistedSwap {
	ps := make([]persistedSwap, 0, len(n.sessions))
	for _, o := range n.store.List() {
		if !o.Mine {
			continue
		}
		idHex := hexEncode(o.ID[:])
		if s := n.sessions[idHex]; s != nil && !n.sessionIsTerminal(idHex, s) {
			ps = append(ps, persistFromSession(s, o))
		} else {
			ps = append(ps, persistFromOrder(o))
		}
	}
	for _, e := range n.store.History() {
		if e.Order == nil || !e.Order.Mine {
			continue
		}
		ps = append(ps, persistFromHistoryEntry(e))
	}
	return ps
}

// snapshotBroadcasts always returns nil settled: the watch table holds active watches
// only; settled records exist only in legacy files and are migrated at load (migrateSettledArchive).
func snapshotBroadcasts(n *Node) (active, settled []persistedBroadcast) {
	n.trackedMu.Lock()
	defer n.trackedMu.Unlock()
	for _, tb := range n.tracked {
		pb := persistedBroadcast{
			OrderID: tb.OrderID, Kind: tb.Kind, Coin: tb.Coin,
			TxID: tb.TxID, Hex: tb.Hex, Seq: tb.Seq, Confs: tb.Confs,
			FirstSeenMicro: tb.FirstSeenMicro, Attempts: tb.Attempts,
		}
		active = append(active, pb)
	}
	return active, nil
}

// A var (not a func) so tests can instrument the marshal boundary.
var marshalSwapFile = func(swaps []persistedSwap, broadcasts, settled []persistedBroadcast) ([]byte, error) {
	if broadcasts == nil {
		broadcasts = []persistedBroadcast{}
	}
	blob, err := json.Marshal(swapEnvelope{Swaps: swaps, Broadcasts: broadcasts, Settled: settled})
	if err != nil {
		return nil, fmt.Errorf("api: marshal swaps: %w", err)
	}
	sum := sha256.Sum256(blob)
	env := swapFile{Sum: hex.EncodeToString(sum[:]), Swaps: swaps, Broadcasts: broadcasts, Settled: settled}
	data, err := json.Marshal(env)
	if err != nil {
		return nil, fmt.Errorf("api: marshal swap file: %w", err)
	}
	return data, nil
}

// swapEnvelope is the checksummed body; nil Broadcasts normalize to [] so an
// absent section hashes like an empty one, and Settled is omitted when empty.
type swapEnvelope struct {
	Swaps      []persistedSwap      `json:"swaps"`
	Broadcasts []persistedBroadcast `json:"broadcasts"`
	Settled    []persistedBroadcast `json:"settled,omitempty"`
}

// writeSwaps writes via temp + fsync + atomic rename, mirroring C++ SerializeFileDB's
// atomic file write; it stays a var so tests can instrument the disk-write boundary.
var writeSwaps = func(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("api: mkdir swap dir: %w", err)
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("api: open swap tmp: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("api: write swap tmp: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("api: fsync swap tmp: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("api: close swap tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("api: rename swap file: %w", err)
	}
	return nil
}

// loadSwaps treats a missing file as first boot (nils, not an error);
// a present file must parse and pass checksum, else it errors so the caller decides.
func loadSwaps(path string) ([]persistedSwap, []persistedBroadcast, []persistedBroadcast, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil, nil
		}
		return nil, nil, nil, fmt.Errorf("api: read swap file: %w", err)
	}
	return parseSwapFile(data, path)
}

// parseSwapFile verifies the section set its writer hashed: files predating
// the broadcast sections carry a swaps-only checksum and verify through that path.
func parseSwapFile(data []byte, path string) ([]persistedSwap, []persistedBroadcast, []persistedBroadcast, error) {
	var env swapFile
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, nil, nil, fmt.Errorf("api: parse swap file %s: %w", path, err)
	}
	bc := env.Broadcasts
	if bc == nil {
		bc = []persistedBroadcast{}
	}
	if blob, err := json.Marshal(swapEnvelope{Swaps: env.Swaps, Broadcasts: bc, Settled: env.Settled}); err == nil {
		sum := sha256.Sum256(blob)
		if hex.EncodeToString(sum[:]) == env.Sum {
			return env.Swaps, bc, env.Settled, nil
		}
	}
	if len(env.Broadcasts) == 0 && len(env.Settled) == 0 {
		if blob, err := json.Marshal(env.Swaps); err == nil {
			sum := sha256.Sum256(blob)
			if hex.EncodeToString(sum[:]) == env.Sum {
				return env.Swaps, nil, nil, nil
			}
		}
	}
	return nil, nil, nil, fmt.Errorf("api: swap file %s checksum mismatch", path)
}

var errSwapEnvelopeUnusable = fmt.Errorf("api: swap file envelope unusable")

// loadSwapsLenient salvages individually-valid records after the strict load
// failed; zero-ID records drop — they restore as a live order and re-persist forever.
func loadSwapsLenient(data []byte) (good []persistedSwap, dropped int, err error) {
	var raw struct {
		Swaps []json.RawMessage `json:"swaps"`
	}
	if uerr := json.Unmarshal(data, &raw); uerr != nil || raw.Swaps == nil {
		if uerr != nil {
			err = fmt.Errorf("%w: %v", errSwapEnvelopeUnusable, uerr)
		} else {
			err = errSwapEnvelopeUnusable
		}
		return nil, 0, err
	}
	for _, r := range raw.Swaps {
		var ps persistedSwap
		if uerr := json.Unmarshal(r, &ps); uerr != nil {
			dropped++
			xlog.Debug("swap salvage: dropping unparseable record", "err", uerr)
			continue
		}
		if ps.ID == ([32]byte{}) {
			dropped++
			xlog.Debug("swap salvage: dropping zero-ID record")
			continue
		}
		good = append(good, ps)
	}
	return good, dropped, nil
}

// quarantineSwapFile renames a corrupt file aside as evidence: the daemon
// never deletes it, so stale .bad files accumulate for operator cleanup.
func quarantineSwapFile(path string) (string, error) {
	base := fmt.Sprintf("%s.bad.%d", path, time.Now().UnixNano())
	qpath := base
	for i := 0; i < 1000; i++ {
		if _, err := os.Stat(qpath); os.IsNotExist(err) {
			if rerr := os.Rename(path, qpath); rerr != nil {
				return "", fmt.Errorf("api: quarantine swap file: %w", rerr)
			}
			return qpath, nil
		} else if err != nil {
			return "", fmt.Errorf("api: quarantine swap file: %w", err)
		}
		qpath = fmt.Sprintf("%s.%d", base, i+1)
	}
	return "", fmt.Errorf("api: quarantine swap file: no free evidence name for %s", path)
}

type persistJob struct {
	path    string
	swaps   []persistedSwap
	bc      []persistedBroadcast
	settled []persistedBroadcast
}

// persistNow runs on the engine goroutine (snapshotSwaps reads the engine-owned session map), never off-engine
// on a started node; it gates the swap/make-order send, so a crash after the send never leaves state the local copy lacks.
func (n *Node) persistNow() error {
	cfg := n.cfg()
	if cfg == nil || cfg.DataDir == "" {
		return nil
	}
	path := swapStatePath(cfg.DataDir)
	// Clear the queued snapshot first or persistLoop can flush a stale one over
	// this write; locks take persistMu before persistWriteMu, as writeLatestPersist does.
	n.persistMu.Lock()
	n.persistLatest = nil
	n.persistMu.Unlock()
	n.persistWriteMu.Lock()
	defer n.persistWriteMu.Unlock()
	swaps := snapshotSwaps(n)
	n.noteSnapshotCount(len(swaps))
	bc, settled := snapshotBroadcasts(n)
	data, err := marshalSwapFile(swaps, bc, settled)
	if err != nil {
		xlog.Error("swap persist failed", "dir", cfg.DataDir, "err", err)
		n.persistFailures.Add(1)
		return err
	}
	if err := writeSwapsWithRetry(path, data); err != nil {
		xlog.Error("swap persist failed after retries", "dir", cfg.DataDir, "err", err)
		n.persistFailures.Add(1)
		return err
	}
	return nil
}

// noteSnapshotCount runs on the engine goroutine (plain atomics suffice); counting at snapshot
// time makes a shrink warning mean silent history loss, never a failed disk write (persist failure).
func (n *Node) noteSnapshotCount(m int) {
	if !n.swapCountSeen.CompareAndSwap(false, true) {
		prev := n.lastSwapCount.Swap(int64(m))
		switch {
		case prev == int64(m):
			// Steady state: the overwhelmingly common case stays silent.
		case int64(m) < prev:
			xlog.Warn("swap snapshot record count shrank", "from", prev, "to", m,
				"note", "legitimate only via dxFlushCancelledOrders, a rolled-back dxMakeOrder or dxTakeOrder, or a reject restoring the order; any other decrease is silent history loss")
		default:
			xlog.Info("swap snapshot record count grew", "from", prev, "to", m)
		}
		return
	}
	n.lastSwapCount.Store(int64(m))
}

// persist flattens on the engine goroutine on a started node and hands marshal + fsync to the background
// persistLoop, so a slow disk never stalls packet processing; with no loop running (tests/inline) they run synchronously.
func (n *Node) persist() {
	cfg := n.cfg()
	if cfg == nil || cfg.DataDir == "" {
		return
	}
	path := swapStatePath(cfg.DataDir)
	swaps := snapshotSwaps(n)
	n.noteSnapshotCount(len(swaps))
	bc, settled := snapshotBroadcasts(n)
	if !n.persistUp.Load() {
		data, err := marshalSwapFile(swaps, bc, settled)
		if err != nil {
			xlog.Error("swap persist failed", "dir", cfg.DataDir, "err", err)
			n.persistFailures.Add(1)
			return
		}
		if err := writeSwaps(path, data); err != nil {
			xlog.Error("swap persist failed", "dir", cfg.DataDir, "err", err)
			n.persistFailures.Add(1)
		}
		return
	}
	n.publishLatest(&persistJob{path: path, swaps: swaps, bc: bc, settled: settled})
}

func (n *Node) publishLatest(job *persistJob) {
	n.persistMu.Lock()
	n.persistLatest = job
	n.persistMu.Unlock()
	select {
	case n.persistSignal <- struct{}{}:
	default:
	}
}

func orderFields(o *Order) persistedSwap {
	return persistedSwap{
		ID:               o.ID,
		Type:             o.Type,
		From:             o.From,
		To:               o.To,
		FromCurrency:     o.FromCurrency,
		ToCurrency:       o.ToCurrency,
		FromAmount:       o.FromAmount,
		ToAmount:         o.ToAmount,
		OrigFromAmount:   o.OrigFromAmount,
		OrigToAmount:     o.OrigToAmount,
		MinFromAmount:    o.MinFromAmount,
		PartialAllowed:   o.PartialAllowed,
		PartialRepost:    o.PartialRepost,
		ParentID:         o.ParentID,
		Created:          o.Created,
		Updated:          o.Updated,
		BlockHash:        o.BlockHash,
		BlockNumber:      o.BlockNumber,
		MakerPubkey:      o.MakerPubkey,
		MakerAddress:     o.MakerAddress,
		TakerAddress:     o.TakerAddress,
		BlockID:          o.BlockID,
		RefundTx:         o.RefundTx,
		BinTxId:          o.BinTxId,
		OBinTxId:         o.OBinTxId,
		OBinTxVout:       o.OBinTxVout,
		OBinTxP2SHAmount: o.OBinTxP2SHAmount,
		OOverpayment:     o.OOverpayment,
		Status:           o.Status,
		Utxos:            o.Utxos,
		UtxoCurrency:     o.UtxoCurrency,
		UsedCoins:        o.UsedCoins,

		SNodePubkey:          o.SNodePubkey,
		HubAddress:           o.HubAddress,
		OtherPubkey:          o.OtherPubkey,
		MakerKey:             o.MakerKey,
		Reason:               o.Reason,
		Role:                 o.Role,
		DepositSent:          o.DepositSent,
		CounterpartyRedeemed: o.CounterpartyRedeemed,
		OrigFromCurrency:     o.OrigFromCurrency,
		OrigToCurrency:       o.OrigToCurrency,
	}
}

func persistFromOrder(o *Order) persistedSwap {
	return orderFields(o)
}

func persistFromHistoryEntry(e historyEntry) persistedSwap {
	ps := orderFields(e.Order)
	ps.Status = e.Status
	ps.Reason = e.Reason
	ps.Updated = e.Updated
	ps.Historical = true
	return ps
}

func persistFromSession(s *SwapSession, o *Order) persistedSwap {
	ps := orderFields(o)
	ps.IsMaker = s.isMaker
	ps.SrcCur = s.srcCur
	ps.DstCur = s.dstCur
	ps.SrcAmt = s.srcAmt
	ps.DstAmt = s.dstAmt
	ps.OurSourceAddr = s.ourSourceAddr
	ps.OurDestAddr = s.ourDestAddr
	ps.TheirPub = s.theirPub
	ps.PrivKey = s.privKey
	ps.PubKey = s.pubKey
	ps.Secret = s.secret
	ps.SecretHash = s.secretHash
	ps.OurLockTime = s.ourLockTime
	ps.OurDepositTxID = s.ourDepositTxID
	ps.RefundHex = s.refundHex
	ps.RefundDone = s.refundDone
	ps.SecretHunt = s.secretHunt
	ps.HuntSince = s.huntSince
	ps.ScanCursor = s.scanCursor
	ps.DepositHex = s.depositHex
	ps.TheirDepositTxID = s.theirDepositTxID
	ps.TheirLockTime = s.theirLockTime
	ps.TheirSecretHash = s.theirSecretHash
	ps.TheirPayTxID = s.theirPayTxID
	ps.ClaimRetryAt = s.claimRetryAt
	ps.ClaimRetries = s.claimRetries
	ps.DepositRetryAt = s.depositRetryAt
	ps.DepositRetries = s.depositRetries
	ps.NotReadySince = s.notReadySince
	ps.NotReadySeen = s.notReadySeen
	ps.TheirDepositVout = s.theirDepositVout
	ps.TheirP2SHNative = s.theirP2SHNative
	ps.TheirOverpayment = s.theirOverpayment
	ps.ClaimHex = s.claimHex
	ps.ClaimTxID = s.claimTxID
	ps.ClaimCur = s.claimCur
	ps.Hub = s.hub
	ps.HubKey = s.hubKey
	ps.State = s.state
	ps.HoldApplySentAt = s.holdApplySentAt
	return ps
}

// restoreSwap runs single-threaded from NewNode's restore block, before the engine starts,
// so its unsynchronized writes to n.sessions are safe (no concurrent readers yet).
func (n *Node) restoreSwap(ps persistedSwap) {
	o := buildOrder(ps)

	// Key this override on RefundTx, never State: an order-only record stores
	// csIdle, which would file a refund-owed swap into history.
	refundPending := ps.Status == "canceled" && ps.DepositSent && ps.RefundTx != "" && !ps.RefundDone
	// A "finished" record with deposit out, a pre-signed refund and no claim
	// evidence never claimed: the deposit is still locked and owes a recovery sweep, not history.
	finishedRefundPending := ps.Status == "finished" && ps.DepositSent && ps.RefundTx != "" && !ps.RefundDone && ps.ClaimTxID == "" && !ps.CounterpartyRedeemed
	if (ps.Historical || ps.State == csFinished || (isOrderTerminal(ps.Status) && (ps.RefundDone || ps.RefundHex == "" || ps.State < csCreatedA))) && !refundPending && !finishedRefundPending {
		n.store.AddToHistory(o, ps.Status, uint64(ps.Reason), ps.Updated)
		return
	}
	// This never-claimed finish is restored to the deposit-broadcast status so the
	// book, the sweeps, and the watchdog see a pre-claim deposit-out order.
	if finishedRefundPending {
		o.Status = "created"
		o.Updated = uint64(NowMicro())
	}
	n.store.Add(o)

	if !hasSessionData(ps) {
		return
	}

	// Never restore csFinished verbatim: sessionIsTerminal would prune the
	// session on the next tick and re-strand the refund — restore it by role.
	restoredState := ps.State
	if finishedRefundPending {
		if ps.IsMaker {
			restoredState = csCreatedA
		} else {
			restoredState = csCreatedB
		}
	}

	s := &SwapSession{
		n:                n,
		isMaker:          ps.IsMaker,
		id:               ps.ID,
		srcCur:           ps.SrcCur,
		dstCur:           ps.DstCur,
		srcAmt:           ps.SrcAmt,
		dstAmt:           ps.DstAmt,
		ourSourceAddr:    ps.OurSourceAddr,
		ourDestAddr:      ps.OurDestAddr,
		theirPub:         ps.TheirPub,
		privKey:          ps.PrivKey,
		pubKey:           ps.PubKey,
		secret:           ps.Secret,
		secretHash:       ps.SecretHash,
		ourLockTime:      ps.OurLockTime,
		ourDepositTxID:   ps.OurDepositTxID,
		refundHex:        ps.RefundHex,
		refundDone:       ps.RefundDone,
		secretHunt:       ps.SecretHunt,
		huntSince:        ps.HuntSince,
		scanCursor:       ps.ScanCursor,
		depositHex:       ps.DepositHex,
		theirDepositTxID: ps.TheirDepositTxID,
		theirLockTime:    ps.TheirLockTime,
		theirSecretHash:  ps.TheirSecretHash,
		theirPayTxID:     ps.TheirPayTxID,
		claimRetryAt:     ps.ClaimRetryAt,
		claimRetries:     ps.ClaimRetries,
		depositRetryAt:   ps.DepositRetryAt,
		depositRetries:   ps.DepositRetries,
		notReadySince:    ps.NotReadySince,
		notReadySeen:     ps.NotReadySeen,
		theirDepositVout: ps.TheirDepositVout,
		theirP2SHNative:  ps.TheirP2SHNative,
		theirOverpayment: ps.TheirOverpayment,
		claimHex:         ps.ClaimHex,
		claimTxID:        ps.ClaimTxID,
		claimCur:         ps.ClaimCur,
		hub:              ps.Hub,
		hubKey:           ps.HubKey,
		state:            restoredState,
		holdApplySentAt:  ps.HoldApplySentAt,
		// The watchdog clock restarts at restore: downtime is not hub silence.
		lastProgress: uint64(NowMicro()),
	}
	if s.state == csHoldApplied && s.holdApplySentAt == 0 {
		s.holdApplySentAt = uint64(NowMicro())
	}
	if s.secretHunt && s.huntSince == 0 {
		s.huntSince = uint64(NowMicro())
	}
	// A restored claim already broadcast has no retry trigger left, so arm one
	// immediate pass: the tick adopts the confirmed claim and drives the finish.
	if s.state == csConfirmedA || s.state == csConfirmedB {
		if s.claimTxID != "" && s.claimHex != "" && s.claimRetryAt == 0 {
			s.claimRetryAt = uint64(NowMicro())
		}
	}
	n.sessions[hexEncode(ps.ID[:])] = s
}

func hasSessionData(ps persistedSwap) bool {
	return ps.State > csIdle ||
		ps.PrivKey != ([32]byte{}) ||
		ps.PubKey != ([33]byte{}) ||
		ps.RefundHex != "" ||
		ps.OurDepositTxID != "" ||
		ps.SecretHash != ([20]byte{})
}

func buildOrder(ps persistedSwap) *Order {
	return &Order{
		ID:               ps.ID,
		Type:             ps.Type,
		From:             ps.From,
		To:               ps.To,
		FromCurrency:     ps.FromCurrency,
		ToCurrency:       ps.ToCurrency,
		FromAmount:       ps.FromAmount,
		ToAmount:         ps.ToAmount,
		OrigFromAmount:   ps.OrigFromAmount,
		OrigToAmount:     ps.OrigToAmount,
		MinFromAmount:    ps.MinFromAmount,
		PartialAllowed:   ps.PartialAllowed,
		PartialRepost:    ps.PartialRepost,
		ParentID:         ps.ParentID,
		Created:          ps.Created,
		Updated:          ps.Updated,
		BlockHash:        ps.BlockHash,
		BlockNumber:      ps.BlockNumber,
		MakerPubkey:      ps.MakerPubkey,
		MakerAddress:     ps.MakerAddress,
		TakerAddress:     ps.TakerAddress,
		BlockID:          ps.BlockID,
		RefundTx:         ps.RefundTx,
		BinTxId:          ps.BinTxId,
		OBinTxId:         ps.OBinTxId,
		OBinTxVout:       ps.OBinTxVout,
		OBinTxP2SHAmount: ps.OBinTxP2SHAmount,
		OOverpayment:     ps.OOverpayment,
		Status:           ps.Status,
		Utxos:            ps.Utxos,
		UtxoCurrency:     ps.UtxoCurrency,
		UsedCoins:        ps.UsedCoins,
		Mine:             true,

		SNodePubkey:          ps.SNodePubkey,
		HubAddress:           ps.HubAddress,
		OtherPubkey:          ps.OtherPubkey,
		MakerKey:             ps.MakerKey,
		Reason:               ps.Reason,
		Role:                 ps.Role,
		DepositSent:          ps.DepositSent,
		CounterpartyRedeemed: ps.CounterpartyRedeemed,
		OrigFromCurrency:     ps.OrigFromCurrency,
		OrigToCurrency:       ps.OrigToCurrency,
	}
}
