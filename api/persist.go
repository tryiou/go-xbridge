package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	xlog "go-xbridge/log"
	"go-xbridge/proto"
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

	TheirDepositTxID string   `json:"theirDepositTxID"`
	TheirLockTime    uint32   `json:"theirLockTime"`
	TheirSecretHash  [20]byte `json:"theirSecretHash"`

	Hub    [20]byte    `json:"hub"`
	HubKey [33]byte    `json:"hubKey"`
	State  clientState `json:"state"`

	// Historical marks a terminal record persisted from Store.history (C++
	// saveOrders writes m_historicTransactions alongside the live map).
	// restoreSwap routes these straight back into history, never the live set.
	Historical bool `json:"historical,omitempty"`
}

// swapFile is the on-disk envelope: a sha256 checksum of the swaps blob plus
// the blob, so loadSwaps can detect a truncated/corrupted file. C++ guards
// orders.dat with a fixed RecordChecksum in SerializeFileDB; we instead hash
// the actual data.
type swapFile struct {
	Sum   string          `json:"sum"`
	Swaps []persistedSwap `json:"swaps"`
}

// swapStatePath returns the local swap-state file path inside dir (the
// xbridged-swaps.json analogue of C++ orders.dat).
func swapStatePath(dir string) string {
	return filepath.Join(dir, "xbridged-swaps.json")
}

// snapshotSwaps marshals the swap-file envelope (live local swaps AND local
// history) into the blob writeSwaps durably stores. It mirrors C++ saveOrders,
// which serializes the live local transactions AND the local history: live
// orders are persisted with their session state (so an in-flight trade can be
// rebuilt post-restart, including its per-trade M keypair), while terminal
// orders that moved to Store.history are persisted as historical records so
// finished/cancelled trades remain visible after restart. The marshal (CPU,
// consistent snapshot) runs on the engine goroutine — it reads n.sessions,
// which the engine alone owns — while the disk write runs on the background
// persistLoop goroutine, so the engine never blocks on fsync.
func snapshotSwaps(n *Node) ([]byte, error) {
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

	blob, err := json.Marshal(ps)
	if err != nil {
		return nil, fmt.Errorf("api: marshal swaps: %w", err)
	}
	sum := sha256.Sum256(blob)
	env := swapFile{Sum: hex.EncodeToString(sum[:]), Swaps: ps}
	data, err := json.Marshal(env)
	if err != nil {
		return nil, fmt.Errorf("api: marshal swap file: %w", err)
	}
	return data, nil
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
		f.Close()
		return fmt.Errorf("api: write swap tmp: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
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
// (nil, nil)); a present but unparseable/corrupt file returns an error so the
// caller can decide whether to start fresh rather than silently lose state.
func loadSwaps(path string) ([]persistedSwap, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("api: read swap file: %w", err)
	}
	var env swapFile
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("api: parse swap file %s: %w", path, err)
	}
	// Re-marshal the in-file list and re-hash to verify integrity.
	blob, err := json.Marshal(env.Swaps)
	if err != nil {
		return nil, fmt.Errorf("api: re-marshal swaps: %w", err)
	}
	sum := sha256.Sum256(blob)
	if hex.EncodeToString(sum[:]) != env.Sum {
		return nil, fmt.Errorf("api: swap file %s checksum mismatch", path)
	}
	return env.Swaps, nil
}

// persistJob is one background swap-file write: the on-disk path plus the
// already-marshaled swapFile blob (the snapshot is built on the engine
// goroutine, where n.sessions is owned; only the disk I/O is backgrounded).
type persistJob struct {
	path string
	data []byte
}

// persist flushes local swap state to disk without blocking the engine on
// fsync. The snapshot (marshal of the swapFile env, reading the
// engine-owned n.sessions) is built on the engine goroutine; the actual
// temp-write/fsync/rename runs on the background persistLoop goroutine, so a
// slow disk never stalls packet processing — C++ saveOrders likewise runs on
// the timer/worker threads, never the message thread (xbridgeapp.cpp:3744).
// Coalescing: publishLatest overwrites the slot, so a burst of engine persists
// collapses into the newest snapshot. When the engine is not started (tests,
// inline mode) the write runs synchronously, preserving the single-threaded
// behaviour the test suite relies on.
func (n *Node) persist() {
	cfg := n.cfg()
	if cfg == nil || cfg.DataDir == "" {
		return
	}
	path := swapStatePath(cfg.DataDir)
	data, err := snapshotSwaps(n)
	if err != nil {
		xlog.Error("swap persist failed", "dir", cfg.DataDir, "err", err)
		return
	}
	if !n.persistUp.Load() {
		if err := writeSwaps(path, data); err != nil {
			xlog.Error("swap persist failed", "dir", cfg.DataDir, "err", err)
		}
		return
	}
	n.publishLatest(&persistJob{path: path, data: data})
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
	ps.TheirDepositTxID = s.theirDepositTxID
	ps.TheirLockTime = s.theirLockTime
	ps.TheirSecretHash = s.theirSecretHash
	ps.Hub = s.hub
	ps.HubKey = s.hubKey
	ps.State = s.state
	if !s.n.cfg().PersistSecrets {
		ps.PrivKey = [32]byte{}
		ps.Secret = [33]byte{}
		ps.RefundHex = ""
	}
	return ps
}

// restoreSwap rebuilds an Order (and, for live records, a SwapSession) from a
// persisted record and registers them on n (mirroring C++ loadOrders). Active
// swaps are re-driven by dispatchSwap when the hub's packets arrive
// post-restart; terminal records (finished, or terminal-status with no refund
// still owed) route into Store.history like C++ routes trFinished/trCancelled
// to m_historicTransactions. The caller is the NewNode restore block
// (single-threaded, before the engine starts).
func (n *Node) restoreSwap(ps persistedSwap) {
	o := buildOrder(ps)

	// Terminal records are restored to history, never re-registered as live
	// swaps: they would otherwise leak back into the live set on every restart.
	// Refund-pending cancelled swaps are kept live so the sweep can still
	// recover the deposit post-restart.
	if ps.Historical || ps.State == csFinished || (isOrderTerminal(ps.Status) && (ps.RefundDone || ps.RefundHex == "" || ps.State < csCreatedA)) {
		n.store.AddToHistory(o, ps.Status, uint64(ps.Reason), ps.Updated)
		return
	}
	n.store.Add(o)

	// Order-only records (persistFromOrder) carry no session: the terminal
	// session was pruned, so there is nothing to resume.
	if !hasSessionData(ps) {
		return
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
		theirDepositTxID: ps.TheirDepositTxID,
		theirLockTime:    ps.TheirLockTime,
		theirSecretHash:  ps.TheirSecretHash,
		hub:              ps.Hub,
		hubKey:           ps.HubKey,
		state:            ps.State,
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
