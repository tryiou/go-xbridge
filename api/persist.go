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

// persistedSwap is the on-disk record of one local (Mine) swap, merging the
// durable fields of Order and SwapSession so a restart can fully rebuild the
// trade — including its per-trade M keypair. It mirrors C++ orders.dat (the
// set of local transactions saved by saveOrders / restored by loadOrders).
//
// Byte arrays ([32]/[33]/[20]byte) and []proto.UtxoEntry marshal via the
// default JSON codec (base64) and round-trip losslessly, so no bespoke
// serializer is needed.
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

	// UsedCoins is the recorded funding UTXO selection (C++ xtx->usedCoins,
	// xbridgetransactiondescr.h:136 READWRITE(usedCoins)). C++ persists it and
	// rebuilds the deposit from it after a restart; mirror that so a pre-deposit
	// crash can resume the maker/taker deposit instead of stalling.
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
	// SecretHunt re-arms taker-side secret recovery after a restart: a
	// hunting session must keep hunting (mempool watch), never fail its
	// first post-restart refund with "rollback failed". omitempty keeps
	// pre-hunt snapshots decodable; absent means not hunting.
	SecretHunt bool `json:"secretHunt,omitempty"`
	// HuntSince stamps when the hunt armed (wall micros), feeding the
	// hourly hunt WARN's elapsed time. omitempty like SecretHunt; restore
	// backfills a zero stamp with the restore time (downtime unknown).
	HuntSince uint64 `json:"huntSince,omitempty"`
	// ScanCursor is the confirmed-spend rescan's next height (0 = unseeded;
	// the first rescan round derives it). omitempty like the hunt flags.
	ScanCursor uint32 `json:"scanCursor,omitempty"`
	// DepositHex is the signed deposit raw hex for tick-driven repost of a
	// failed broadcast (swap_retry.go). omitempty: pre-upgrade records lack
	// it and simply never repost.
	DepositHex string `json:"depositHex,omitempty"`

	TheirDepositTxID string   `json:"theirDepositTxID"`
	TheirLockTime    uint32   `json:"theirLockTime"`
	TheirSecretHash  [20]byte `json:"theirSecretHash"`
	// TheirPayTxID is the counterparty's claim payTx id (taker side: the
	// maker's APayTxID from ConfirmB): the trigger the tick-driven claim
	// retry rebuilds from when the hub never redelivers (swap_retry.go).
	TheirPayTxID string `json:"theirPayTxID,omitempty"`
	// ClaimRetryAt/ClaimRetries schedule that retry (wall micros, 0 = none;
	// consecutive-failure count driving the backoff). omitempty keeps
	// pre-upgrade snapshots decodable; a zero value simply never fires.
	ClaimRetryAt uint64 `json:"claimRetryAt,omitempty"`
	ClaimRetries uint32 `json:"claimRetries,omitempty"`
	// DepositRetryAt/DepositRetries schedule the tick-driven rebuild of a
	// failed HTLC deposit build (swap_retry.go). Same omitempty upgrade rule
	// as the claim slot.
	DepositRetryAt uint64 `json:"depositRetryAt,omitempty"`
	DepositRetries uint32 `json:"depositRetries,omitempty"`
	// NotReadySince stamps the not-ready fast-lane wait start (wall micros,
	// swap_retry.go). Same omitempty upgrade rule as the retry slots.
	NotReadySince uint64 `json:"notReadySince,omitempty"`
	// NotReadySeen is the visibility verdict of the latest fast-lane
	// admission (true = watched tx observed). Same omitempty upgrade rule
	// (pre-upgrade snapshots predate the fields entirely): a restarted
	// unseen wait slows immediately past the taper age, which is the
	// correct posture for an already-degraded wait.
	NotReadySeen bool `json:"notReadySeen,omitempty"`
	// Validated counterparty-deposit out-params (C++ oBinTxVout /
	// oBinTxP2SHAmount / oOverpayment): needed to rebuild a claim after a
	// restart without re-running the deposit check. All three ride the
	// session record — the phase-1 claim intent persists BEFORE the order
	// record's OBinTx* copy is updated (phase-2 broadcast), so restoring
	// from the order copy would resurrect a stale (usually zero)
	// overpayment after a build/broadcast-window crash.
	TheirDepositVout uint32 `json:"theirDepositVout,omitempty"`
	TheirP2SHNative  uint64 `json:"theirP2SHNative,omitempty"`
	TheirOverpayment uint64 `json:"theirOverpayment,omitempty"`

	// Built-claim intent (C++ in-memory payTx equivalent): the signed claim
	// hex plus its locally-derived id and chain, persisted before broadcast.
	ClaimHex  string `json:"claimHex,omitempty"`
	ClaimTxID string `json:"claimTxID,omitempty"`
	ClaimCur  string `json:"claimCur,omitempty"`

	Hub    [20]byte    `json:"hub"`
	HubKey [33]byte    `json:"hubKey"`
	State  clientState `json:"state"`

	// HoldApplySentAt is the last HoldApply send time (api/hold_resend.go):
	// the resender and the silent-hub recorder key off it, so it rides the
	// session record like lastProgress-equivalent clocks. omitempty keeps
	// pre-upgrade snapshots decodable; restoreSwap re-stamps a zero value
	// for a still-parked session so old records resume resending instead
	// of skipping forever.
	HoldApplySentAt uint64 `json:"holdApplySentAt,omitempty"`

	// Historical marks a terminal record persisted from Store.history (C++
	// saveOrders writes m_historicTransactions alongside the live map).
	// restoreSwap routes these straight back into history, never the live set.
	Historical bool `json:"historical,omitempty"`
}

// persistedBroadcast is the durable form of one tracked broadcast
// (api/reconcile.go): the locally-derived txid plus the exact bytes, so a
// restart resumes confirmation watch (and later phases can rebroadcast the
// identical bytes). Confs is the last confirmed depth (0 = unknown).
type persistedBroadcast struct {
	OrderID        string        `json:"orderID"`
	Kind           broadcastKind `json:"kind"`
	Coin           string        `json:"coin"`
	TxID           string        `json:"txid"`
	Hex            string        `json:"hex"`
	Seq            uint64        `json:"seq,omitempty"`
	Confs          int           `json:"confs,omitempty"`
	FirstSeenMicro uint64        `json:"firstSeenMicro,omitempty"`
	Attempts       int           `json:"attempts,omitempty"`
}

// swapFile is the on-disk envelope: a sha256 checksum of the swaps blob plus
// the blob, so loadSwaps can detect a truncated/corrupted file. C++ guards
// orders.dat with a fixed RecordChecksum in SerializeFileDB; we instead hash
// the actual data.
//
// Broadcasts ride in the same envelope (omitted when empty). The "settled"
// section is legacy: older files may carry it, but new files always write it
// empty — finalized broadcasts are dropped from the watch table and never
// polled again. The checksum covers swaps AND both broadcast sections; files
// written before broadcasts existed verify through the legacy swaps-only
// fallback in parseSwapFile, and pre-settled files (broadcasts only) verify
// through the primary path unchanged (an empty omitted section hashes
// identically).
// Downgrade note: an older binary reading a file with any broadcast section
// fails closed (checksum mismatch) — upgrade is one-way for the state file,
// back it up first.
type swapFile struct {
	Sum        string               `json:"sum"`
	Swaps      []persistedSwap      `json:"swaps"`
	Broadcasts []persistedBroadcast `json:"broadcasts,omitempty"`
	Settled    []persistedBroadcast `json:"settled,omitempty"`
}

// swapStatePath returns the local swap-state file path inside dir (the
// xbridged-swaps.json analogue of C++ orders.dat).
func swapStatePath(dir string) string {
	return filepath.Join(dir, "xbridged-swaps.json")
}

// snapshotSwaps flattens the swap-file snapshot (live local swaps AND local
// history) into []persistedSwap. It mirrors C++ saveOrders, which serializes
// the live local transactions AND the local history: live orders are persisted
// with their session state (so an in-flight trade can be rebuilt post-restart,
// including its per-trade M keypair), while terminal orders that moved to
// Store.history are persisted as historical records so finished/cancelled
// trades remain visible after restart. The flatten runs on the engine
// goroutine — it reads n.sessions, which the engine alone owns — while the
// marshal to bytes runs on the background persistLoop goroutine, so the engine
// never does O(orders) JSON reflection work or blocks on fsync.
func snapshotSwaps(n *Node) []persistedSwap {
	ps := make([]persistedSwap, 0, len(n.sessions))
	for _, o := range n.store.List() {
		if !o.Mine {
			continue // only persist local orders, like C++ saveOrders' isLocal()
		}
		idHex := hexEncode(o.ID[:])
		if s := n.sessions[idHex]; s != nil && !n.sessionIsTerminal(idHex, s) {
			// Live in-flight swap: persist the session so a restart can resume.
			ps = append(ps, persistFromSession(s, o))
		} else {
			// Live order with no live session (terminal session pruned, or a
			// rolled-back order after refundDone): order-only record.
			ps = append(ps, persistFromOrder(o))
		}
	}
	for _, e := range n.store.History() {
		if e.Order == nil || !e.Order.Mine {
			continue // only persist local history, like C++ saveOrders
		}
		ps = append(ps, persistFromHistoryEntry(e))
	}
	return ps
}

// snapshotBroadcasts flattens the confirmation-watch table for the swap-file
// envelope. Only in-flight entries are persisted — finalized broadcasts are
// dropped from the watch table and never polled again, so the "settled"
// section is always empty on write (kept in the envelope only so older files
// still parse; their settled section is dropped on load). Mutex-guarded
// (record sites are not engine-confined); the marshal runs on the background
// persistLoop like swaps.
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

// runs on the background persistLoop goroutine, never the engine. A var so
// marshalSwapFile builds the swap-file envelope (sha256 checksum over swaps
// plus both broadcast sections) from flattened snapshots. Pure CPU over the
// slices, so it runs on the background persistLoop goroutine, never the
// engine. A var so tests can instrument (count/block) the marshal boundary
// without touching the engine-owned flatten.
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

// swapEnvelope is the checksummed body: swaps plus both broadcast watch
// sections, with nil normalized to [] so old-shape and empty-shape files hash
// deterministically.
type swapEnvelope struct {
	Swaps      []persistedSwap      `json:"swaps"`
	Broadcasts []persistedBroadcast `json:"broadcasts"`
	Settled    []persistedBroadcast `json:"settled,omitempty"`
}

// writeSwaps durably writes a marshaled swap-file blob via temp-write, fsync
// and atomic rename (C++ SerializeFileDB is atomic; this mirrors that). Runs on
// the background persistLoop goroutine, or synchronously in inline/test mode.
// A var so tests can instrument the disk-write boundary (count/block) without
// touching the engine snapshot path.
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

// loadSwaps reads the swap-state file. A missing file is not an error (returns
// (nil, nil, nil)); a present but unparseable/corrupt file returns an error so
// the caller can decide whether to start fresh rather than silently lose
// state.
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

// parseSwapFile verifies and splits a swap-file blob. The checksum covers
// swaps AND both broadcast sections (nil normalized to [] for determinism);
// files written before broadcasts existed verify through the legacy
// swaps-only checksum, and pre-settled files (broadcasts only) verify through
// the primary path unchanged — an empty omitted Settled section hashes
// identically to the old two-section envelope.
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

// errSwapEnvelopeUnusable marks a swap-state file whose envelope cannot even
// be listed (not JSON, or missing the record array): per-record salvage is
// impossible, so the caller quarantines the file and starts fresh.
var errSwapEnvelopeUnusable = fmt.Errorf("api: swap file envelope unusable")

// loadSwapsLenient salvages individually-valid records from a file the strict
// loadSwaps rejected (one bad record fails a typed slice, dooming every
// healthy swap). Records decode independently; the good ones return with the
// count of dropped ones. Checksum-free by necessity: the envelope checksum is
// whole-blob, so it cannot attest records once the blob fails — salvage is
// best-effort and the quarantined original stays the evidence. Zero-ID
// records drop: they restore as a live order under the 64-zeros key and would
// be re-persisted forever. Pure function over read bytes (no I/O). Fallback
// only — valid files keep the exact strict path including checksum.
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

// quarantineSwapFile preserves a corrupt swap-state file as
// "<path>.bad.<unixnano>": same bytes, same mode, atomic same-dir rename —
// no copy, no second format. A collision loop covers the same-nanosecond
// double-corrupt case. Stale files accumulate for operator cleanup; the
// daemon never deletes evidence itself.
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

// persistJob is one background swap-file write: the on-disk path plus the
// flattened snapshot (built on the engine goroutine, where n.sessions is
// owned). The marshal to bytes and the disk I/O both run on the background
// persistLoop goroutine, so the engine never does the O(orders) JSON reflection
// work nor blocks on fsync.
type persistJob struct {
	path    string
	swaps   []persistedSwap
	bc      []persistedBroadcast
	settled []persistedBroadcast
}

// persistNow durably writes the current swap snapshot to disk synchronously
// (marshal + fsync + atomic rename), blocking the caller. It is used at the
// critical boundary immediately before sending an outbound swap/make-order
// response, so a crash after the send can never leave the hub knowing state the
// local durable copy lacks (which would strand a restarted swap — fund loss).
// C++ App::saveOrders (xbridgeapp.cpp:3868-3898) writes on a timer with no
// before-send guarantee; this port makes the durable write precede the send.
// Runs on the engine goroutine (snapshotSwaps reads the engine-owned session
// map), never from a test/HTTP goroutine on a started node.
func (n *Node) persistNow() error {
	cfg := n.cfg()
	if cfg == nil || cfg.DataDir == "" {
		return nil
	}
	path := swapStatePath(cfg.DataDir)
	// Invalidate any queued async snapshot BEFORE writing, so the background
	// persistLoop cannot later flush a stale snapshot (taken before this one)
	// over the just-written state. Lock order matches writeLatestPersist
	// (persistMu then persistWriteMu) to avoid a deadlock.
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

// noteSnapshotCount tracks the swap-record count of each freshly built
// snapshot and logs every transition, so local swap history can never be
// silently erased: a past build wiped finished-swap history at runtime with
// no trace in the surviving logs precisely because nothing logged snapshot
// counts. A decrease is WARNed: the only legitimate shrink paths are
// dxFlushCancelledOrders (operator RPC, trCancelled records only) and the
// 1000-entry history cap; any other drop is finished-swap history loss. An
// increase is INFO (normal: a new order, a finished trade moving to
// history). The count is observed at snapshot time, before the marshal/write
// boundary — a failed disk write is a separate, already-counted failure
// (persistFailures), not a history loss. Both call sites (persist,
// persistNow) snapshot on the engine goroutine, so plain atomics suffice —
// no lock held.
func (n *Node) noteSnapshotCount(m int) {
	if !n.swapCountSeen.CompareAndSwap(false, true) {
		prev := n.lastSwapCount.Swap(int64(m))
		switch {
		case prev == int64(m):
			// Steady state: the overwhelmingly common case stays silent.
		case int64(m) < prev:
			xlog.Warn("swap snapshot record count shrank", "from", prev, "to", m,
				"note", "legitimate only via dxFlushCancelledOrders or the history cap; any other decrease is silent history loss")
		default:
			xlog.Info("swap snapshot record count grew", "from", prev, "to", m)
		}
		return
	}
	n.lastSwapCount.Store(int64(m))
}

// persist flushes local swap state to disk without blocking the engine on
// marshal or fsync. The snapshot flatten (reading the engine-owned n.sessions)
// runs on the engine goroutine; the marshal + checksum + temp-write/fsync/rename
// run on the background persistLoop goroutine, so a slow disk or a large book
// never stalls packet processing — C++ saveOrders likewise runs on the
// timer/worker threads, never the message thread (xbridgeapp.cpp:3744).
// Coalescing: publishLatest overwrites the slot, so a burst of engine persists
// collapses into the newest flattened snapshot. When the engine is not started
// (tests, inline mode) the marshal and the write run synchronously, preserving
// the single-threaded behaviour the test suite relies on.
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

// publishLatest records the newest swap-file write job and wakes the background
// persistLoop (non-blocking; a pending signal already covers it).
func (n *Node) publishLatest(job *persistJob) {
	n.persistMu.Lock()
	n.persistLatest = job
	n.persistMu.Unlock()
	select {
	case n.persistSignal <- struct{}{}:
	default:
	}
}

// orderFields flattens the durable Order fields shared by every persisted
// record (live sessions, order-only records, and history entries alike).
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

// persistFromOrder builds an order-only persisted record for a live order with
// no live session (its terminal session was pruned). The session fields are
// zeroed; restoreSwap re-adds the order without a session.
func persistFromOrder(o *Order) persistedSwap {
	return orderFields(o)
}

// persistFromHistoryEntry builds a historical persisted record from a
// Store.history entry (C++ saveOrders writes local historic transactions).
func persistFromHistoryEntry(e historyEntry) persistedSwap {
	ps := orderFields(e.Order)
	ps.Status = e.Status
	ps.Reason = e.Reason
	ps.Updated = e.Updated
	ps.Historical = true
	return ps
}

// persistFromSession flattens a live session + its order into a persistedSwap.
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

// restoreSwap rebuilds an Order (and, for live records, a SwapSession) from a
// persisted record and registers them on n (mirroring C++ loadOrders). Active
// swaps are re-driven by processSwap when the hub's packets arrive
// post-restart; terminal records (finished, or terminal-status with no refund
// still owed) route into Store.history like C++ routes trFinished/trCancelled
// to m_historicTransactions. The caller is the NewNode restore block
// (single-threaded, before the engine starts).
func (n *Node) restoreSwap(ps persistedSwap) {
	o := buildOrder(ps)

	// Terminal records are restored to history, never re-registered as live
	// swaps: they would otherwise leak back into the live set on every restart.
	// Refund-pending cancelled swaps are kept live so the sweep can still
	// recover the deposit post-restart. The refundPending override must NOT
	// key on ps.State: an order-only persist (session already pruned) stores
	// State=csIdle, which read as "State < csCreatedA" filed a canceled swap
	// with the deposit locked in the P2SH into history — invisible to
	// scanStoredRefunds forever (live 2026-09-15, order a4198f2d…). C++
	// redeems trCancelled transactions in its redeem scan, so canceled +
	// deposit-out + pre-signed refund always owes a recovery sweep. Key on
	// RefundTx — the order-record refund bytes (set exactly when the deposit
	// was built); the session-scoped RefundHex is empty on order-only records.
	refundPending := ps.Status == "canceled" && ps.DepositSent && ps.RefundTx != "" && !ps.RefundDone
	// A "finished" record with the deposit out, a pre-signed refund, and no
	// claim evidence on either side is a pre-gate early finish (OnFinished
	// terminated unconditionally before the finishedMayTerminate gate): the
	// deposit is still locked and still owes a recovery sweep. Key on the
	// order-record RefundTx — set exactly when the deposit was built
	// (applyCreatedA/B) — and require both no local claim (ClaimTxID) and
	// no counterparty redeem, so genuinely finished records (claim tracked,
	// counterparty redeemed) still route to history. Mirrors the
	// refundPending override above.
	finishedRefundPending := ps.Status == "finished" && ps.DepositSent && ps.RefundTx != "" && !ps.RefundDone && ps.ClaimTxID == "" && !ps.CounterpartyRedeemed
	if (ps.Historical || ps.State == csFinished || (isOrderTerminal(ps.Status) && (ps.RefundDone || ps.RefundHex == "" || ps.State < csCreatedA))) && !refundPending && !finishedRefundPending {
		n.store.AddToHistory(o, ps.Status, uint64(ps.Reason), ps.Updated)
		return
	}
	// A rescued early finish kept its pre-gate "finished" status on the
	// record, but the swap never completed: restore the deposit-broadcast
	// status so the live book, the sweeps, and the watchdog see a
	// pre-claim deposit-out order (which is what it is). The txlog and the
	// pre-signed refund hex remain the audit trail of what happened.
	if finishedRefundPending {
		o.Status = "created"
		o.Updated = uint64(NowMicro())
	}
	n.store.Add(o)

	// Order-only records (persistFromOrder) carry no session: the terminal
	// session was pruned, so there is nothing to resume.
	if !hasSessionData(ps) {
		return
	}

	// A rescued early finish recorded the pre-gate csFinished over the live
	// state; restoring it verbatim would make sessionIsTerminal prune the
	// session on the next tick and re-strand the refund. The deposit
	// broadcast is proven by DepositSent, so restore the deposit-broadcast
	// state by role — the exact state the session held when the early
	// Finished arrived.
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
		// Validated out-params ride the session record (adopted at
		// claim-build, before the order record is updated) so a restarted
		// claim rebuilds against the exact deposit output without
		// re-running the check; the order's OBinTx* copy stays the display
		// source of truth once broadcast.
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
		// Watchdog clock restarts at restore: pre-upgrade records predate
		// the stamp, and the downtime itself is not hub silence.
		lastProgress: uint64(NowMicro()),
	}
	// A pre-upgrade record (no HoldApplySentAt field) for a still-parked
	// session would otherwise skip the resender and the silence recorder
	// forever on its zero stamp. Re-stamp to restore time: the resender
	// waits one fresh interval (no post-restart burst) and the silence
	// clock restarts alongside the watchdog above.
	if s.state == csHoldApplied && s.holdApplySentAt == 0 {
		s.holdApplySentAt = uint64(NowMicro())
	}
	// A hunted record predating the hunt clock (zero stamp) restarts its
	// visibility window at restore: the downtime itself is unknown, so the
	// elapsed-hunting clock starts now rather than reporting a bogus age.
	if s.secretHunt && s.huntSince == 0 {
		s.huntSince = uint64(NowMicro())
	}
	// Restore reconciliation: a session persisted after its claim broadcast
	// (csConfirmedA/B, claimTxID set) whose claimRetryAt was consumed has no
	// retry trigger left. Schedule one immediate adoption pass — the tick's
	// adoption verifies the claim on-chain and drives the terminal finish
	// (C++ trFinished, xbridgesession.cpp:3002/:3185); a hub Finished packet
	// that may still arrive stays idempotent behind it.
	if s.state == csConfirmedA || s.state == csConfirmedB {
		if s.claimTxID != "" && s.claimHex != "" && s.claimRetryAt == 0 {
			s.claimRetryAt = uint64(NowMicro())
		}
	}
	n.sessions[hexEncode(ps.ID[:])] = s
}

// hasSessionData reports whether a persisted record carries a live session
// (persisted via persistFromSession) as opposed to an order-only record
// (persistFromOrder, whose session fields are all zero).
func hasSessionData(ps persistedSwap) bool {
	return ps.State > csIdle ||
		ps.PrivKey != ([32]byte{}) ||
		ps.PubKey != ([33]byte{}) ||
		ps.RefundHex != "" ||
		ps.OurDepositTxID != "" ||
		ps.SecretHash != ([20]byte{})
}

// buildOrder reconstructs the durable Order from a persisted record.
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
