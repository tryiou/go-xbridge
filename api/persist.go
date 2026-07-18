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
	ID             [32]byte          `json:"id"`
	IsMaker        bool              `json:"isMaker"`
	Type           OrderType         `json:"type"`
	From           [20]byte          `json:"from"`
	To             [20]byte          `json:"to"`
	FromCurrency   string            `json:"fromCurrency"`
	ToCurrency     string            `json:"toCurrency"`
	FromAmount     uint64            `json:"fromAmount"`
	ToAmount       uint64            `json:"toAmount"`
	OrigFromAmount uint64            `json:"origFromAmount"`
	OrigToAmount   uint64            `json:"origToAmount"`
	MinFromAmount  uint64            `json:"minFromAmount"`
	PartialAllowed bool              `json:"partialAllowed"`
	PartialRepost  bool              `json:"partialRepost"`
	ParentID       [32]byte          `json:"parentID"`
	Created        uint64            `json:"created"`
	Updated        uint64            `json:"updated"`
	BlockHash      [32]byte          `json:"blockHash"`
	MakerPubkey    string            `json:"makerPubkey"`
	MakerAddress   string            `json:"makerAddress"`
	TakerAddress   string            `json:"takerAddress"`
	BlockID        string            `json:"blockID"`
	RefundTx       string            `json:"refundTx"`
	BinTxId        string            `json:"binTxId"`
	OBinTxId       string            `json:"oBinTxId"`
	Status         string            `json:"status"`
	Utxos          []proto.UtxoEntry `json:"utxos"`

	SNodePubkey          string `json:"sNodePubkey"`
	OtherPubkey          string `json:"otherPubkey"`
	MakerKey             string `json:"makerKey"`
	Reason               uint32 `json:"reason"`
	Role                 byte   `json:"role"`
	DepositSent          bool   `json:"depositSent"`
	CounterpartyRedeemed bool   `json:"counterpartyRedeemed"`
	OrigFromCurrency     string `json:"origFromCurrency"`
	OrigToCurrency       string `json:"origToCurrency"`

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

	Hub   [20]byte    `json:"hub"`
	State clientState `json:"state"`
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

// saveSwaps writes all local (Mine) swaps atomically. It snapshots the
// sessions under sessMu, filters to those whose order is Mine (mirroring C++
// saveOrders' isLocal() gate), then marshals + sha256s the blob and performs
// an atomic temp-write / fsync / rename (C++ SerializeFileDB is atomic; this
// mirrors that). The caller must hold persistMu.
func saveSwaps(path string, n *Node) error {
	n.sessMu.Lock()
	ps := make([]persistedSwap, 0, len(n.sessions))
	for id, s := range n.sessions {
		o := n.store.Get(id)
		if o == nil || !o.Mine {
			continue // only persist local swaps, like C++ saveOrders
		}
		ps = append(ps, persistFromSession(s, o))
	}
	n.sessMu.Unlock()

	blob, err := json.Marshal(ps)
	if err != nil {
		return fmt.Errorf("api: marshal swaps: %w", err)
	}
	sum := sha256.Sum256(blob)
	env := swapFile{Sum: hex.EncodeToString(sum[:]), Swaps: ps}
	data, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("api: marshal swap file: %w", err)
	}

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

// persist flushes local swap state to disk (no-op when DataDir is unset, the
// previous behaviour). Safe to call from any goroutine; serializes against
// concurrent persist() calls via persistMu.
func (n *Node) persist() {
	if n.cfg() == nil || n.cfg().DataDir == "" {
		return
	}
	n.persistMu.Lock()
	defer n.persistMu.Unlock()
	if err := saveSwaps(swapStatePath(n.cfg().DataDir), n); err != nil {
		xlog.Warn("swap persist failed", "dir", n.cfg().DataDir, "err", err)
	}
}

// persistFromSession flattens a live session + its order into a persistedSwap.
func persistFromSession(s *SwapSession, o *Order) persistedSwap {
	return persistedSwap{
		ID:             o.ID,
		IsMaker:        s.isMaker,
		Type:           o.Type,
		From:           o.From,
		To:             o.To,
		FromCurrency:   o.FromCurrency,
		ToCurrency:     o.ToCurrency,
		FromAmount:     o.FromAmount,
		ToAmount:       o.ToAmount,
		OrigFromAmount: o.OrigFromAmount,
		OrigToAmount:   o.OrigToAmount,
		MinFromAmount:  o.MinFromAmount,
		PartialAllowed: o.PartialAllowed,
		PartialRepost:  o.PartialRepost,
		ParentID:       o.ParentID,
		Created:        o.Created,
		Updated:        o.Updated,
		BlockHash:      o.BlockHash,
		MakerPubkey:    o.MakerPubkey,
		MakerAddress:   o.MakerAddress,
		TakerAddress:   o.TakerAddress,
		BlockID:        o.BlockID,
		RefundTx:       o.RefundTx,
		BinTxId:        o.BinTxId,
		OBinTxId:       o.OBinTxId,
		Status:         o.Status,
		Utxos:          o.Utxos,

		SNodePubkey:          o.SNodePubkey,
		OtherPubkey:          o.OtherPubkey,
		MakerKey:             o.MakerKey,
		Reason:               o.Reason,
		Role:                 o.Role,
		DepositSent:          o.DepositSent,
		CounterpartyRedeemed: o.CounterpartyRedeemed,
		OrigFromCurrency:     o.OrigFromCurrency,
		OrigToCurrency:       o.OrigToCurrency,
		SrcCur:               s.srcCur,
		DstCur:               s.dstCur,
		SrcAmt:               s.srcAmt,
		DstAmt:               s.dstAmt,
		OurSourceAddr:        s.ourSourceAddr,
		OurDestAddr:          s.ourDestAddr,
		TheirPub:             s.theirPub,
		PrivKey:              s.privKey,
		PubKey:               s.pubKey,
		Secret:               s.secret,
		SecretHash:           s.secretHash,
		OurLockTime:          s.ourLockTime,
		OurDepositTxID:       s.ourDepositTxID,
		RefundHex:            s.refundHex,
		RefundDone:           s.refundDone,
		TheirDepositTxID:     s.theirDepositTxID,
		TheirLockTime:        s.theirLockTime,
		TheirSecretHash:      s.theirSecretHash,
		Hub:                  s.hub,
		State:                s.state,
	}
}

// restoreSwap rebuilds an Order + SwapSession from a persisted record and
// registers them on n (mirroring C++ loadOrders). Active swaps are re-driven by
// dispatchSwap when the hub's packets arrive post-restart; cancelled/finished
// ones stay inert but remain restorable. Caller should hold n.sessMu.
func (n *Node) restoreSwap(ps persistedSwap) {
	o := &Order{
		ID:             ps.ID,
		Type:           ps.Type,
		From:           ps.From,
		To:             ps.To,
		FromCurrency:   ps.FromCurrency,
		ToCurrency:     ps.ToCurrency,
		FromAmount:     ps.FromAmount,
		ToAmount:       ps.ToAmount,
		OrigFromAmount: ps.OrigFromAmount,
		OrigToAmount:   ps.OrigToAmount,
		MinFromAmount:  ps.MinFromAmount,
		PartialAllowed: ps.PartialAllowed,
		PartialRepost:  ps.PartialRepost,
		ParentID:       ps.ParentID,
		Created:        ps.Created,
		Updated:        ps.Updated,
		BlockHash:      ps.BlockHash,
		MakerPubkey:    ps.MakerPubkey,
		MakerAddress:   ps.MakerAddress,
		TakerAddress:   ps.TakerAddress,
		BlockID:        ps.BlockID,
		RefundTx:       ps.RefundTx,
		BinTxId:        ps.BinTxId,
		OBinTxId:       ps.OBinTxId,
		Status:         ps.Status,
		Utxos:          ps.Utxos,
		Mine:           true,

		SNodePubkey:          ps.SNodePubkey,
		OtherPubkey:          ps.OtherPubkey,
		MakerKey:             ps.MakerKey,
		Reason:               ps.Reason,
		Role:                 ps.Role,
		DepositSent:          ps.DepositSent,
		CounterpartyRedeemed: ps.CounterpartyRedeemed,
		OrigFromCurrency:     ps.OrigFromCurrency,
		OrigToCurrency:       ps.OrigToCurrency,
	}
	n.store.Add(o)

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
		state:            ps.State,
	}
	n.sessions[hexEncode(ps.ID[:])] = s
}
