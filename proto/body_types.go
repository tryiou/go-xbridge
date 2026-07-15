package proto

import "errors"

// This file ports the per-XBridgeCommand body layouts from the Blocknet C++
// source. The authoritative encoder for each command is its C++ packet writer
// (src/xbridge/xbridgeapp.cpp, src/xbridge/xbridgesession.cpp); the enum-block
// comments in src/xbridge/xbridgepacket.h are STALE in several places and were
// NOT followed where they disagree with the real writer. Differences handled:
//   - xbcTransaction (3) carries extra fields the header comment omits:
//     blockHash, partial-order flag, minFromAmount, and per-utxo rawAddress
//     + signature (from App::Impl::sendPendingTransaction).
//   - xbcTransactionAccepting (5) amounts are uint64, not uint32.
//   - xbcTransactionInit (8) lists "20 bytes source address" twice; the second
//     is the destination address.
//   - xbcPendingTransaction (4) optionally carries a trailing minFromAmount
//     (present in Session::Impl::sendTransaction, absent in the broadcast
//     writer) — the decoder tolerates both.
//
// All integers are little-endian. Hashes are raw 32-byte (Bitcoin internal LE
// order). Addresses are raw 20-byte (uint160). Currencies are 8-byte ASCII,
// left-aligned and null-padded. Strings are null-terminated.

// UtxoEntry is a used-coin entry embedded in order/pending/accepting bodies.
// rawAddress is exactly 20 bytes; signature is exactly 65 bytes (signmessage
// format: 1 recovery byte + 64). See src/xbridge/xbridgewallet.h.
type UtxoEntry struct {
	TxID       [32]byte
	Vout       uint32
	RawAddress [20]byte
	Signature  [65]byte
}

func (u *UtxoEntry) marshal(w *BodyWriter) {
	w.Hash(u.TxID)
	w.Uint32(u.Vout)
	w.Addr(u.RawAddress)
	w.Bytes(u.Signature[:])
}

func (u *UtxoEntry) unmarshal(r *BodyReader) error {
	var err error
	if u.TxID, err = r.Hash(); err != nil {
		return err
	}
	if u.Vout, err = r.Uint32(); err != nil {
		return err
	}
	if u.RawAddress, err = r.Addr(); err != nil {
		return err
	}
	sig, err := r.Bytes(65)
	if err != nil {
		return err
	}
	copy(u.Signature[:], sig)
	return nil
}

func marshalUtxoArray(w *BodyWriter, utxos []UtxoEntry) {
	w.Uint32(uint32(len(utxos)))
	for i := range utxos {
		utxos[i].marshal(w)
	}
}

func unmarshalUtxoArray(r *BodyReader) ([]UtxoEntry, error) {
	n, err := r.Uint32()
	if err != nil {
		return nil, err
	}
	utxos := make([]UtxoEntry, n)
	for i := range utxos {
		if err := utxos[i].unmarshal(r); err != nil {
			return nil, err
		}
	}
	return utxos, nil
}

// ---------------------------------------------------------------------------
// xbcTransaction (3) — maker order broadcast.
// ---------------------------------------------------------------------------

type OrderBody struct {
	ID             [32]byte
	From           [20]byte
	FromCurrency   string
	FromAmount     uint64
	To             [20]byte
	ToCurrency     string
	ToAmount       uint64
	Created        uint64
	BlockHash      [32]byte
	PartialAllowed bool
	MinFromAmount  uint64
	Utxos          []UtxoEntry
}

func (b *OrderBody) Marshal() []byte {
	w := NewBodyWriter()
	w.Hash(b.ID)
	w.Addr(b.From)
	w.Currency(b.FromCurrency)
	w.Uint64(b.FromAmount)
	w.Addr(b.To)
	w.Currency(b.ToCurrency)
	w.Uint64(b.ToAmount)
	w.Uint64(b.Created)
	w.Hash(b.BlockHash)
	if b.PartialAllowed {
		w.Uint16(1)
	} else {
		w.Uint16(0)
	}
	w.Uint64(b.MinFromAmount)
	marshalUtxoArray(w, b.Utxos)
	return w.Payload()
}

func (b *OrderBody) Unmarshal(data []byte) error {
	r := NewBodyReader(data)
	var err error
	if b.ID, err = r.Hash(); err != nil {
		return err
	}
	if b.From, err = r.Addr(); err != nil {
		return err
	}
	if b.FromCurrency, err = r.Currency(); err != nil {
		return err
	}
	if b.FromAmount, err = r.Uint64(); err != nil {
		return err
	}
	if b.To, err = r.Addr(); err != nil {
		return err
	}
	if b.ToCurrency, err = r.Currency(); err != nil {
		return err
	}
	if b.ToAmount, err = r.Uint64(); err != nil {
		return err
	}
	if b.Created, err = r.Uint64(); err != nil {
		return err
	}
	if b.BlockHash, err = r.Hash(); err != nil {
		return err
	}
	pf, err := r.Uint16()
	if err != nil {
		return err
	}
	b.PartialAllowed = pf != 0
	if b.MinFromAmount, err = r.Uint64(); err != nil {
		return err
	}
	if b.Utxos, err = unmarshalUtxoArray(r); err != nil {
		return err
	}
	return nil
}

// ---------------------------------------------------------------------------
// xbcPendingTransaction (4) — open-order broadcast (one per order; the body
// repeats once per advertised order on the wire).
// ---------------------------------------------------------------------------

type PendingTransactionBody struct {
	ID             [32]byte
	FromCurrency   string
	FromAmount     uint64
	ToCurrency     string
	ToAmount       uint64
	HubAddress     [20]byte
	Created        uint64
	BlockHash      [32]byte
	PartialAllowed bool
	MinFromAmount  uint64 // optional; only present in some writers
}

func (b *PendingTransactionBody) Marshal() []byte {
	w := NewBodyWriter()
	w.Hash(b.ID)
	w.Currency(b.FromCurrency)
	w.Uint64(b.FromAmount)
	w.Currency(b.ToCurrency)
	w.Uint64(b.ToAmount)
	w.Addr(b.HubAddress)
	w.Uint64(b.Created)
	w.Hash(b.BlockHash)
	if b.PartialAllowed {
		w.Uint16(1)
	} else {
		w.Uint16(0)
	}
	w.Uint64(b.MinFromAmount)
	return w.Payload()
}

func (b *PendingTransactionBody) Unmarshal(data []byte) error {
	r := NewBodyReader(data)
	var err error
	if b.ID, err = r.Hash(); err != nil {
		return err
	}
	if b.FromCurrency, err = r.Currency(); err != nil {
		return err
	}
	if b.FromAmount, err = r.Uint64(); err != nil {
		return err
	}
	if b.ToCurrency, err = r.Currency(); err != nil {
		return err
	}
	if b.ToAmount, err = r.Uint64(); err != nil {
		return err
	}
	if b.HubAddress, err = r.Addr(); err != nil {
		return err
	}
	if b.Created, err = r.Uint64(); err != nil {
		return err
	}
	if b.BlockHash, err = r.Hash(); err != nil {
		return err
	}
	pf, err := r.Uint16()
	if err != nil {
		return err
	}
	b.PartialAllowed = pf != 0
	// Optional trailing field: only present in writers that advertise a
	// minimum partial amount. Tolerated for compatibility with the broadcast
	// writer, which omits it.
	if r.remaining() >= 8 {
		if b.MinFromAmount, err = r.Uint64(); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// xbcTransactionAccepting (5) — taker accepts an open order.
// ---------------------------------------------------------------------------

type AcceptingBody struct {
	HubAddress       [20]byte
	ID               [32]byte
	ServiceNodeFeeTx []byte // variable; length-prefixed by uint32
	From             [20]byte
	FromCurrency     string
	FromAmount       uint64
	FromBlockHeight  uint32
	FromBlockHash    [8]byte // first 8 bytes of the block hash
	To               [20]byte
	ToCurrency       string
	ToAmount         uint64
	ToBlockHeight    uint32
	ToBlockHash      [8]byte
	Utxos            []UtxoEntry
}

func (b *AcceptingBody) Marshal() []byte {
	w := NewBodyWriter()
	w.Addr(b.HubAddress)
	w.Hash(b.ID)
	w.Uint32(uint32(len(b.ServiceNodeFeeTx)))
	w.Bytes(b.ServiceNodeFeeTx)
	w.Addr(b.From)
	w.Currency(b.FromCurrency)
	w.Uint64(b.FromAmount)
	w.Uint32(b.FromBlockHeight)
	w.Bytes(b.FromBlockHash[:])
	w.Addr(b.To)
	w.Currency(b.ToCurrency)
	w.Uint64(b.ToAmount)
	w.Uint32(b.ToBlockHeight)
	w.Bytes(b.ToBlockHash[:])
	marshalUtxoArray(w, b.Utxos)
	return w.Payload()
}

func (b *AcceptingBody) Unmarshal(data []byte) error {
	r := NewBodyReader(data)
	var err error
	if b.HubAddress, err = r.Addr(); err != nil {
		return err
	}
	if b.ID, err = r.Hash(); err != nil {
		return err
	}
	n, err := r.Uint32()
	if err != nil {
		return err
	}
	if b.ServiceNodeFeeTx, err = r.Bytes(int(n)); err != nil {
		return err
	}
	if b.From, err = r.Addr(); err != nil {
		return err
	}
	if b.FromCurrency, err = r.Currency(); err != nil {
		return err
	}
	if b.FromAmount, err = r.Uint64(); err != nil {
		return err
	}
	if b.FromBlockHeight, err = r.Uint32(); err != nil {
		return err
	}
	fbh, err := r.Bytes(8)
	if err != nil {
		return err
	}
	copy(b.FromBlockHash[:], fbh)
	if b.To, err = r.Addr(); err != nil {
		return err
	}
	if b.ToCurrency, err = r.Currency(); err != nil {
		return err
	}
	if b.ToAmount, err = r.Uint64(); err != nil {
		return err
	}
	if b.ToBlockHeight, err = r.Uint32(); err != nil {
		return err
	}
	tbh, err := r.Bytes(8)
	if err != nil {
		return err
	}
	copy(b.ToBlockHash[:], tbh)
	if b.Utxos, err = unmarshalUtxoArray(r); err != nil {
		return err
	}
	return nil
}

// ---------------------------------------------------------------------------
// xbcTransactionHold (6) / xbcTransactionHoldApply (7)
// ---------------------------------------------------------------------------

type HoldBody struct {
	HubAddress [20]byte
	ID         [32]byte
	FromAmount uint64
	ToAmount   uint64
}

func (b *HoldBody) Marshal() []byte {
	w := NewBodyWriter()
	w.Addr(b.HubAddress)
	w.Hash(b.ID)
	w.Uint64(b.FromAmount)
	w.Uint64(b.ToAmount)
	return w.Payload()
}

func (b *HoldBody) Unmarshal(data []byte) error {
	r := NewBodyReader(data)
	var err error
	if b.HubAddress, err = r.Addr(); err != nil {
		return err
	}
	if b.ID, err = r.Hash(); err != nil {
		return err
	}
	if b.FromAmount, err = r.Uint64(); err != nil {
		return err
	}
	if b.ToAmount, err = r.Uint64(); err != nil {
		return err
	}
	return nil
}

type HoldApplyBody struct {
	HubAddress    [20]byte
	ClientAddress [20]byte
	ID            [32]byte
}

func (b *HoldApplyBody) Marshal() []byte {
	w := NewBodyWriter()
	w.Addr(b.HubAddress)
	w.Addr(b.ClientAddress)
	w.Hash(b.ID)
	return w.Payload()
}

func (b *HoldApplyBody) Unmarshal(data []byte) error {
	r := NewBodyReader(data)
	var err error
	if b.HubAddress, err = r.Addr(); err != nil {
		return err
	}
	if b.ClientAddress, err = r.Addr(); err != nil {
		return err
	}
	if b.ID, err = r.Hash(); err != nil {
		return err
	}
	return nil
}

// ---------------------------------------------------------------------------
// xbcTransactionInit (8) / xbcTransactionInitialized (9)
// ---------------------------------------------------------------------------

type InitBody struct {
	ClientAddress [20]byte
	HubAddress    [20]byte
	ID            [32]byte
	FromAddress   [20]byte
	FromCurrency  string
	FromAmount    uint64
	ToAddress     [20]byte // the enum comment mistakenly labels this "source"
	ToCurrency    string
	ToAmount      uint64
}

func (b *InitBody) Marshal() []byte {
	w := NewBodyWriter()
	w.Addr(b.ClientAddress)
	w.Addr(b.HubAddress)
	w.Hash(b.ID)
	w.Addr(b.FromAddress)
	w.Currency(b.FromCurrency)
	w.Uint64(b.FromAmount)
	w.Addr(b.ToAddress)
	w.Currency(b.ToCurrency)
	w.Uint64(b.ToAmount)
	return w.Payload()
}

func (b *InitBody) Unmarshal(data []byte) error {
	r := NewBodyReader(data)
	var err error
	if b.ClientAddress, err = r.Addr(); err != nil {
		return err
	}
	if b.HubAddress, err = r.Addr(); err != nil {
		return err
	}
	if b.ID, err = r.Hash(); err != nil {
		return err
	}
	if b.FromAddress, err = r.Addr(); err != nil {
		return err
	}
	if b.FromCurrency, err = r.Currency(); err != nil {
		return err
	}
	if b.FromAmount, err = r.Uint64(); err != nil {
		return err
	}
	if b.ToAddress, err = r.Addr(); err != nil {
		return err
	}
	if b.ToCurrency, err = r.Currency(); err != nil {
		return err
	}
	if b.ToAmount, err = r.Uint64(); err != nil {
		return err
	}
	return nil
}

type InitializedBody struct {
	HubAddress    [20]byte
	ClientAddress [20]byte
	ID            [32]byte
}

func (b *InitializedBody) Marshal() []byte {
	w := NewBodyWriter()
	w.Addr(b.HubAddress)
	w.Addr(b.ClientAddress)
	w.Hash(b.ID)
	return w.Payload()
}

func (b *InitializedBody) Unmarshal(data []byte) error {
	r := NewBodyReader(data)
	var err error
	if b.HubAddress, err = r.Addr(); err != nil {
		return err
	}
	if b.ClientAddress, err = r.Addr(); err != nil {
		return err
	}
	if b.ID, err = r.Hash(); err != nil {
		return err
	}
	return nil
}

// ---------------------------------------------------------------------------
// xbcTransactionCreateA (10) / xbcTransactionCreatedA (11)
// xbcTransactionCreateB (12) / xbcTransactionCreatedB (13)
//
// Authoritative field orders (src/xbridge/xbridgesession.cpp writers —
// the xbridgepacket.h prose comments for these commands are STALE and were
// NOT followed):
//   CreateA  (10, hub→maker): hubAddr | id | B_pubkey
//   CreatedA (11, maker→hub): hubAddr | id | ADepositTxID | HashedSecret
//                                  | ALockTime | refTxId | refTx
//                                  (NO BLockTime)
//   CreateB  (12, hub→taker): hubAddr | id | A_pubkey | ADepositTxID
//                                  | HashedSecret | ALockTime
//                                  (NO BLockTime)
//   CreatedB (13, taker→hub): hubAddr | id | BDepositTxID | BLockTime
//                                  | refTxId | refTx
// ---------------------------------------------------------------------------

type CreateABody struct {
	HubAddress [20]byte
	ID         [32]byte
	BPubKey    [33]byte
}

func (b *CreateABody) Marshal() []byte {
	w := NewBodyWriter()
	w.Addr(b.HubAddress)
	w.Hash(b.ID)
	w.PubKey(b.BPubKey)
	return w.Payload()
}

func (b *CreateABody) Unmarshal(data []byte) error {
	r := NewBodyReader(data)
	var err error
	if b.HubAddress, err = r.Addr(); err != nil {
		return err
	}
	if b.ID, err = r.Hash(); err != nil {
		return err
	}
	if b.BPubKey, err = r.PubKey(); err != nil {
		return err
	}
	return nil
}

type CreatedABody struct {
	HubAddress   [20]byte
	ID           [32]byte
	ADepositTxID string // null-terminated string
	HashedSecret [20]byte
	ALockTime    uint32
	RefTxID      string // null-terminated refund-tx id string
	RefTx        string // null-terminated full refund-tx hex
}

func (b *CreatedABody) Marshal() []byte {
	w := NewBodyWriter()
	w.Addr(b.HubAddress)
	w.Hash(b.ID)
	w.String(b.ADepositTxID)
	w.Addr(b.HashedSecret)
	w.Uint32(b.ALockTime)
	w.String(b.RefTxID)
	w.String(b.RefTx)
	return w.Payload()
}

func (b *CreatedABody) Unmarshal(data []byte) error {
	r := NewBodyReader(data)
	var err error
	if b.HubAddress, err = r.Addr(); err != nil {
		return err
	}
	if b.ID, err = r.Hash(); err != nil {
		return err
	}
	if b.ADepositTxID, err = r.String(); err != nil {
		return err
	}
	if b.HashedSecret, err = r.Addr(); err != nil {
		return err
	}
	if b.ALockTime, err = r.Uint32(); err != nil {
		return err
	}
	if b.RefTxID, err = r.String(); err != nil {
		return err
	}
	if b.RefTx, err = r.String(); err != nil {
		return err
	}
	return nil
}

type CreateBBody struct {
	HubAddress   [20]byte
	ID           [32]byte
	APubKey      [33]byte
	ADepositTxID string
	HashedSecret [20]byte
	ALockTime    uint32
}

func (b *CreateBBody) Marshal() []byte {
	w := NewBodyWriter()
	w.Addr(b.HubAddress)
	w.Hash(b.ID)
	w.PubKey(b.APubKey)
	w.String(b.ADepositTxID)
	w.Addr(b.HashedSecret)
	w.Uint32(b.ALockTime)
	return w.Payload()
}

func (b *CreateBBody) Unmarshal(data []byte) error {
	r := NewBodyReader(data)
	var err error
	if b.HubAddress, err = r.Addr(); err != nil {
		return err
	}
	if b.ID, err = r.Hash(); err != nil {
		return err
	}
	if b.APubKey, err = r.PubKey(); err != nil {
		return err
	}
	if b.ADepositTxID, err = r.String(); err != nil {
		return err
	}
	if b.HashedSecret, err = r.Addr(); err != nil {
		return err
	}
	if b.ALockTime, err = r.Uint32(); err != nil {
		return err
	}
	return nil
}

type CreatedBBody struct {
	HubAddress   [20]byte
	ID           [32]byte
	BDepositTxID string
	BLockTime    uint32
	RefTxID      string
	RefTx        string
}

func (b *CreatedBBody) Marshal() []byte {
	w := NewBodyWriter()
	w.Addr(b.HubAddress)
	w.Hash(b.ID)
	w.String(b.BDepositTxID)
	w.Uint32(b.BLockTime)
	w.String(b.RefTxID)
	w.String(b.RefTx)
	return w.Payload()
}

func (b *CreatedBBody) Unmarshal(data []byte) error {
	r := NewBodyReader(data)
	var err error
	if b.HubAddress, err = r.Addr(); err != nil {
		return err
	}
	if b.ID, err = r.Hash(); err != nil {
		return err
	}
	if b.BDepositTxID, err = r.String(); err != nil {
		return err
	}
	if b.BLockTime, err = r.Uint32(); err != nil {
		return err
	}
	if b.RefTxID, err = r.String(); err != nil {
		return err
	}
	if b.RefTx, err = r.String(); err != nil {
		return err
	}
	return nil
}

// ---------------------------------------------------------------------------
// xbcTransactionConfirmA (18) / xbcTransactionConfirmedA (19)
// xbcTransactionConfirmB (20) / xbcTransactionConfirmedB (21)
// ---------------------------------------------------------------------------

type ConfirmABody struct {
	HubAddress   [20]byte
	ID           [32]byte
	BDepositTxID string
	BLockTime    uint32
}

func (b *ConfirmABody) Marshal() []byte {
	w := NewBodyWriter()
	w.Addr(b.HubAddress)
	w.Hash(b.ID)
	w.String(b.BDepositTxID)
	w.Uint32(b.BLockTime)
	return w.Payload()
}

func (b *ConfirmABody) Unmarshal(data []byte) error {
	r := NewBodyReader(data)
	var err error
	if b.HubAddress, err = r.Addr(); err != nil {
		return err
	}
	if b.ID, err = r.Hash(); err != nil {
		return err
	}
	if b.BDepositTxID, err = r.String(); err != nil {
		return err
	}
	if b.BLockTime, err = r.Uint32(); err != nil {
		return err
	}
	return nil
}

type ConfirmedABody struct {
	HubAddress [20]byte
	ID         [32]byte
	APayTxID   string // null-terminated string; the secret preimage is
	// revealed on-chain in this payTx, NOT carried in the packet.
}

func (b *ConfirmedABody) Marshal() []byte {
	w := NewBodyWriter()
	w.Addr(b.HubAddress)
	w.Hash(b.ID)
	w.String(b.APayTxID)
	return w.Payload()
}

func (b *ConfirmedABody) Unmarshal(data []byte) error {
	r := NewBodyReader(data)
	var err error
	if b.HubAddress, err = r.Addr(); err != nil {
		return err
	}
	if b.ID, err = r.Hash(); err != nil {
		return err
	}
	if b.APayTxID, err = r.String(); err != nil {
		return err
	}
	return nil
}

type ConfirmBBody struct {
	HubAddress [20]byte
	ID         [32]byte
	APayTxID   string // null-terminated string; carries A's payTx id so the
	// taker can recover the secret preimage from it.
}

func (b *ConfirmBBody) Marshal() []byte {
	w := NewBodyWriter()
	w.Addr(b.HubAddress)
	w.Hash(b.ID)
	w.String(b.APayTxID)
	return w.Payload()
}

func (b *ConfirmBBody) Unmarshal(data []byte) error {
	r := NewBodyReader(data)
	var err error
	if b.HubAddress, err = r.Addr(); err != nil {
		return err
	}
	if b.ID, err = r.Hash(); err != nil {
		return err
	}
	if b.APayTxID, err = r.String(); err != nil {
		return err
	}
	return nil
}

type ConfirmedBBody struct {
	HubAddress [20]byte
	ID         [32]byte
	BPayTxID   string
}

func (b *ConfirmedBBody) Marshal() []byte {
	w := NewBodyWriter()
	w.Addr(b.HubAddress)
	w.Hash(b.ID)
	w.String(b.BPayTxID)
	return w.Payload()
}

func (b *ConfirmedBBody) Unmarshal(data []byte) error {
	r := NewBodyReader(data)
	var err error
	if b.HubAddress, err = r.Addr(); err != nil {
		return err
	}
	if b.ID, err = r.Hash(); err != nil {
		return err
	}
	if b.BPayTxID, err = r.String(); err != nil {
		return err
	}
	return nil
}

// ---------------------------------------------------------------------------
// xbcTransactionCancel (22) / xbcTransactionReject (26)
// ---------------------------------------------------------------------------

type CancelBody struct {
	ID     [32]byte
	Reason uint32
}

func (b *CancelBody) Marshal() []byte {
	w := NewBodyWriter()
	w.Hash(b.ID)
	w.Uint32(b.Reason)
	return w.Payload()
}

func (b *CancelBody) Unmarshal(data []byte) error {
	r := NewBodyReader(data)
	var err error
	if b.ID, err = r.Hash(); err != nil {
		return err
	}
	if b.Reason, err = r.Uint32(); err != nil {
		return err
	}
	return nil
}

type RejectBody struct {
	ID     [32]byte
	Reason uint32
}

func (b *RejectBody) Marshal() []byte {
	w := NewBodyWriter()
	w.Hash(b.ID)
	w.Uint32(b.Reason)
	return w.Payload()
}

func (b *RejectBody) Unmarshal(data []byte) error {
	r := NewBodyReader(data)
	var err error
	if b.ID, err = r.Hash(); err != nil {
		return err
	}
	if b.Reason, err = r.Uint32(); err != nil {
		return err
	}
	return nil
}

// ---------------------------------------------------------------------------
// xbcTransactionFinished (24)
// ---------------------------------------------------------------------------

type FinishedBody struct {
	ID [32]byte
}

func (b *FinishedBody) Marshal() []byte {
	w := NewBodyWriter()
	w.Hash(b.ID)
	return w.Payload()
}

func (b *FinishedBody) Unmarshal(data []byte) error {
	r := NewBodyReader(data)
	var err error
	if b.ID, err = r.Hash(); err != nil {
		return err
	}
	return nil
}

// ---------------------------------------------------------------------------
// xbcXChatMessage (2) — relays a serialized Bitcoin p2p message.
// ---------------------------------------------------------------------------

type XChatMessageBody struct {
	Raw []byte
}

func (b *XChatMessageBody) Marshal() []byte {
	w := NewBodyWriter()
	w.Bytes(b.Raw)
	return w.Payload()
}

func (b *XChatMessageBody) Unmarshal(data []byte) error {
	b.Raw = make([]byte, len(data))
	copy(b.Raw, data)
	return nil
}

// ---------------------------------------------------------------------------
// xbcServicesPing (50) — array of supported-service name strings.
// ---------------------------------------------------------------------------

type ServicesPingBody struct {
	Services []string
}

func (b *ServicesPingBody) Marshal() []byte {
	w := NewBodyWriter()
	for _, s := range b.Services {
		w.String(s)
	}
	return w.Payload()
}

func (b *ServicesPingBody) Unmarshal(data []byte) error {
	r := NewBodyReader(data)
	b.Services = nil
	for r.remaining() > 0 {
		s, err := r.String()
		if err != nil {
			return err
		}
		b.Services = append(b.Services, s)
	}
	return nil
}

// DecodeBody parses the body of a packet for the given command into the
// appropriate typed struct. It returns the struct value (as interface{}) so the
// caller can type-assert; nil for commands without a typed body.
func DecodeBody(cmd XBridgeCommand, body []byte) (interface{}, error) {
	switch cmd {
	case XbcTransaction:
		var b OrderBody
		return &b, b.Unmarshal(body)
	case XbcPendingTransaction:
		var b PendingTransactionBody
		return &b, b.Unmarshal(body)
	case XbcTransactionAccepting:
		var b AcceptingBody
		return &b, b.Unmarshal(body)
	case XbcTransactionHold:
		var b HoldBody
		return &b, b.Unmarshal(body)
	case XbcTransactionHoldApply:
		var b HoldApplyBody
		return &b, b.Unmarshal(body)
	case XbcTransactionInit:
		var b InitBody
		return &b, b.Unmarshal(body)
	case XbcTransactionInitialized:
		var b InitializedBody
		return &b, b.Unmarshal(body)
	case XbcTransactionCreateA:
		var b CreateABody
		return &b, b.Unmarshal(body)
	case XbcTransactionCreatedA:
		var b CreatedABody
		return &b, b.Unmarshal(body)
	case XbcTransactionCreateB:
		var b CreateBBody
		return &b, b.Unmarshal(body)
	case XbcTransactionCreatedB:
		var b CreatedBBody
		return &b, b.Unmarshal(body)
	case XbcTransactionConfirmA:
		var b ConfirmABody
		return &b, b.Unmarshal(body)
	case XbcTransactionConfirmedA:
		var b ConfirmedABody
		return &b, b.Unmarshal(body)
	case XbcTransactionConfirmB:
		var b ConfirmBBody
		return &b, b.Unmarshal(body)
	case XbcTransactionConfirmedB:
		var b ConfirmedBBody
		return &b, b.Unmarshal(body)
	case XbcTransactionCancel:
		var b CancelBody
		return &b, b.Unmarshal(body)
	case XbcTransactionFinished:
		var b FinishedBody
		return &b, b.Unmarshal(body)
	case XbcTransactionReject:
		var b RejectBody
		return &b, b.Unmarshal(body)
	case XbcXChatMessage:
		var b XChatMessageBody
		return &b, b.Unmarshal(body)
	case XbcServicesPing:
		var b ServicesPingBody
		return &b, b.Unmarshal(body)
	default:
		return nil, errors.New("xbridge: no typed body for command " + cmd.String())
	}
}
