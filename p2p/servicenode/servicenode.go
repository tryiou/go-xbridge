// Package servicenode ports the Blocknet C++ servicenode P2P message handling
// used by a thin XBridge client to learn the network's token set. A stock
// XBridge wallet does not send SNLIST (only XRouter does,
// src/xrouter/xrouterpeermgr.cpp:536); it learns the servicenode set from
// relayed SNPING / SNREGISTER / SNLISTPING messages
// (src/net_processing.cpp:2976-2981). This package parses exactly those and
// derives the network token union the way walletServices() does (xbridgeapp.cpp:
// 2758): SPV-tier xbridge tokens only, matching ^[^:]+$, excluding xr/xrs, from
// servicenodes pinged < 5 minutes ago (running(), servicenode.h:244).
package servicenode

import (
	"bytes"
	"encoding/json"
	"errors"
	"math/rand"
	"regexp"
	"sort"
	"strconv"
	"sync"
	"time"

	"go-xbridge/crypto"
	xlog "go-xbridge/log"
	"go-xbridge/p2p"
	"go-xbridge/proto"
)

// walletServicesLog gates the per-poll "WalletServices" metric to at most once
// per 60s, mirroring the dial/cancel dedupes: the live token set is
// polled every few seconds, so without this it would log an identical
// line on every dxGetNetworkTokens call.
var walletServicesLog = xlog.NewDedupe(60*time.Second, nil)

// P2P command names (src/protocol.cpp:45-49).
const (
	CmdSNRegister = "snr"  // SNREGISTER  -> ServiceNode
	CmdSNPing     = "snp"  // SNPING      -> ServiceNodePing
	CmdSNList     = "snl"  // SNLIST      -> request (we do NOT send this)
	CmdSNListPing = "snlp" // SNLISTPING  -> ServiceNodePing
)

// walletTokenRe matches a bare token/wallet name (no ':'); XRouter service
// names contain ':' and are excluded. Mirrors the C++ std::regex("^[^:]+$")
// applied in walletServices() (xbridgeapp.cpp:2762).
var walletTokenRe = regexp.MustCompile(`^[^:]+$`)

// xrExclude are XRouter service names that must not appear as network wallets.
var xrExclude = map[string]bool{"xr": true, "xrs": true}

// CollateralUTXO is one COutPoint of a servicenode registration's collateral
// vector (servicenode.h:623-625): txid(32) + vout(4).
type CollateralUTXO struct {
	TxID [32]byte
	Vout uint32
}

// ServiceNode is the parsed servicenode record. For pings, PingTime is the raw
// peer-reported ping timestamp (seconds); the registry clamps it like C++
// updatePing (servicenode.h:254-260) before gating running(). The registration
// fields (PaymentAddress, Collateral, BestBlock, BestBlockHash, Signature) are
// retained — C++ validates them in ServiceNode::isValid (servicenode.h:398-484)
// and XBridge uses the payment address as the service-node fee destination
// (xbridgeapp.cpp:2394-2401), so discarding them is not an option.
type ServiceNode struct {
	PubKey   [33]byte
	Tier     uint8
	Config   string // raw JSON config (carries the xbridge token array)
	Services []string
	PingTime uint32

	// XBridgeVersion is the SN's advertised xbridgeversion from its config
	// (servicenode.h:542-546). Hub selection gates on it matching
	// XBRIDGE_PROTOCOL_VERSION (xbridgeapp.cpp:2910).
	XBridgeVersion uint32

	// Registration fields (servicenode.h:354-384 wire order).
	PaymentAddress [20]byte
	Collateral     []CollateralUTXO
	BestBlock      int32
	BestBlockHash  [32]byte
	Signature      []byte
}

const (
	// TierOpen and TierSPV mirror the C++ ServiceNode::Tier enum
	// (servicenode.h:90-93). NOTE: SPV is 50 (NOT 1) — easy to get
	// wrong from stale comments. Only SPV servicenodes advertise XBridge
	// wallet services (servicenode.h:604, "xbridge only supports SPV nodes").
	TierOpen = 0
	TierSPV  = 50
)

// reader is a minimal cursor over a byte slice used by the parse functions.
type reader struct {
	b   []byte
	pos int
}

func (r *reader) remaining() int { return len(r.b) - r.pos }

func (r *reader) readCPubKey() ([33]byte, error) {
	var pk [33]byte
	n, off, err := p2p.ReadVarInt(r.b, r.pos)
	if err != nil {
		return pk, err
	}
	if n != 33 || len(r.b)-off < 33 {
		return pk, errShort("cpubkey")
	}
	copy(pk[:], r.b[off:off+33])
	r.pos = off + 33
	return pk, nil
}

func (r *reader) readUint8() (uint8, error) {
	if r.remaining() < 1 {
		return 0, errShort("uint8")
	}
	v := r.b[r.pos]
	r.pos++
	return v, nil
}

func (r *reader) readFixed20() ([20]byte, error) {
	var a [20]byte
	if r.remaining() < 20 {
		return a, errShort("paymentAddress")
	}
	copy(a[:], r.b[r.pos:r.pos+20])
	r.pos += 20
	return a, nil
}

func (r *reader) readInt32() (int32, error) {
	if r.remaining() < 4 {
		return 0, errShort("int32")
	}
	v := int32(littleEndian32(r.b[r.pos:]))
	r.pos += 4
	return v, nil
}

func (r *reader) readUint32() (uint32, error) {
	if r.remaining() < 4 {
		return 0, errShort("uint32")
	}
	v := littleEndian32(r.b[r.pos:])
	r.pos += 4
	return v, nil
}

func (r *reader) readUint256() ([32]byte, error) {
	var h [32]byte
	if r.remaining() < 32 {
		return h, errShort("uint256")
	}
	copy(h[:], r.b[r.pos:r.pos+32])
	r.pos += 32
	return h, nil
}

func (r *reader) readVarStr() (string, error) {
	s, off, err := p2p.UnmarshalVarStr(r.b[r.pos:])
	if err != nil {
		return "", err
	}
	r.pos += off
	return s, nil
}

func (r *reader) readVarBytes() ([]byte, error) {
	n, off, err := p2p.ReadVarInt(r.b, r.pos)
	if err != nil {
		return nil, err
	}
	r.pos = off
	if r.remaining() < n {
		return nil, errShort("varbytes")
	}
	v := make([]byte, n)
	copy(v, r.b[r.pos:r.pos+n])
	r.pos += n
	return v, nil
}

// readCollateral reads a std::vector<COutPoint> (varint count + count COutPoints).
// Each COutPoint is txid(32) + vout(4). The collateral is retained so
// registration validation (count/dups, servicenode.h:430-436) can run.
func (r *reader) readCollateral() ([]CollateralUTXO, error) {
	n, off, err := p2p.ReadVarInt(r.b, r.pos)
	if err != nil {
		return nil, err
	}
	r.pos = off
	if r.remaining() < n*(32+4) {
		return nil, errShort("collateral")
	}
	if n == 0 {
		return nil, nil
	}
	// n is already bounded by remaining()/36 above, so the allocation cannot
	// exceed the input size; the count-vs-snMaxCollateralCount check belongs to
	// validation (ServiceNode::isValid, servicenode.h:430), not parsing.
	out := make([]CollateralUTXO, n)
	for i := range out {
		copy(out[i].TxID[:], r.b[r.pos:r.pos+32])
		out[i].Vout = littleEndian32(r.b[r.pos+32 : r.pos+36])
		r.pos += 32 + 4
	}
	return out, nil
}

// ParseServiceNode parses an SNREGISTER payload (ServiceNode::SerializationOp).
// All registration fields are retained for validation (ServiceNode::isValid,
// servicenode.h:398-484).
func ParseServiceNode(b []byte) (ServiceNode, error) {
	r := &reader{b: b}
	sn := ServiceNode{}
	var err error
	if sn.PubKey, err = r.readCPubKey(); err != nil {
		return sn, err
	}
	if sn.Tier, err = r.readUint8(); err != nil {
		return sn, err
	}
	if sn.PaymentAddress, err = r.readFixed20(); err != nil { // paymentAddress (CKeyID, 20 raw bytes)
		return sn, err
	}
	if sn.Collateral, err = r.readCollateral(); err != nil {
		return sn, err
	}
	if sn.BestBlock, err = r.readInt32(); err != nil { // bestBlock (int32)
		return sn, err
	}
	if sn.BestBlockHash, err = r.readUint256(); err != nil { // bestBlockHash
		return sn, err
	}
	if sn.Signature, err = r.readVarBytes(); err != nil { // signature
		return sn, err
	}
	xlog.Debug("servicenode: SNREGISTER parsed", "pubkey", hex33(sn.PubKey), "tier", sn.Tier, "collateral", len(sn.Collateral))
	return sn, nil
}

// ParseServiceNodePing parses an SNPING / SNLISTPING payload
// (ServiceNodePing::SerializationOp). It runs parseConfig on the embedded JSON
// config to populate the SPV xbridge token list.
func ParseServiceNodePing(b []byte) (ServiceNode, error) {
	r := &reader{b: b}
	sn := ServiceNode{}
	var err error
	var outerPubkey [33]byte
	if outerPubkey, err = r.readCPubKey(); err != nil { // 1. ping snodePubKey
		return sn, err
	}
	if _, err = r.readUint32(); err != nil { // 2. bestBlock (uint32 in ping)
		return sn, err
	}
	if _, err = r.readUint256(); err != nil { // 3. bestBlockHash
		return sn, err
	}
	var pingTime uint32
	if pingTime, err = r.readUint32(); err != nil { // 4. pingTime (uint32)
		return sn, err
	}
	var config string
	if config, err = r.readVarStr(); err != nil { // 5. config (JSON varstr)
		return sn, err
	}
	// 6. embedded ServiceNode (carries tier; the record the SN list is built
	// from). parseConfig is applied to the ping's config string.
	inner, err := parseInnerServiceNode(r)
	if err != nil {
		return sn, err
	}
	// isValid (servicenode.h:791): the outer pubkey must be fully valid and
	// match the embedded snode pubkey.
	if !fullyValidCPubKey(outerPubkey) || outerPubkey != inner.PubKey {
		return sn, errPubkeyMismatch
	}
	// Carry the embedded registration (pubkey, tier, paymentAddress, collateral,
	// bestBlock, bestBlockHash, signature) into the returned record; parseConfig
	// adds the service list from the ping's config.
	sn = inner
	sn.Config = config
	sn.Services, sn.XBridgeVersion = parseConfig(config, sn.Tier)
	sn.PingTime = pingTime
	sigStart := r.pos
	signature, err := r.readVarBytes() // 7. ping signature
	if err != nil {
		return sn, err
	}
	// isValid (servicenode.h:810-815): recover the signer from the compact
	// signature over sigHash() (servicenode.h:748-752) and require it to match
	// the outer pubkey. b[:sigStart] is exactly the serialized sigHash fields.
	// RecoverCompact serializes per the header's compression bit
	// (pubkey.cpp:199-205): an uncompressed header yields a 65-byte key that
	// never equals the 33-byte compressed snodePubKey, so it is rejected here.
	hash := crypto.DoubleSHA256(b[:sigStart])
	pub, err := crypto.RecoverCompact(signature, hash[:])
	if err != nil || !bytes.Equal(pub, outerPubkey[:]) {
		return sn, errBadSignature
	}
	// isValid (servicenode.h:817-818): unless skipBlockchainValidation, the
	// embedded registration must pass ServiceNode::isValid. Wire pings arrive
	// with skipValidation=false (net_processing.cpp:2976-2981 processPing
	// default), so a registration failing the thin-client-enforceable subset
	// drops the whole ping, mirroring processPing (servicenodemgr.h:186-187).
	if !registrationValid(sn) {
		return sn, errInvalidRegistration
	}
	// NOTE: the per-copy "SNPING parsed" log was removed — a single
	// ping is relayed by every peer, so the parser fired once per copy.
	// The logical ping is logged once (per genuine ping) by Registry.AddPing.
	return sn, nil
}

// parseInnerServiceNode parses the trailing embedded ServiceNode of a
// ServiceNodePing (snodePubKey, tier, paymentAddress, collateral, bestBlock,
// bestBlockHash, signature). It does NOT re-read config (the ping carries it at
// the outer level). Registration fields are retained; C++ validates the embedded
// registration inside ping.isValid (servicenode.h:818).
func parseInnerServiceNode(r *reader) (ServiceNode, error) {
	sn := ServiceNode{}
	var err error
	if sn.PubKey, err = r.readCPubKey(); err != nil {
		return sn, err
	}
	if sn.Tier, err = r.readUint8(); err != nil {
		return sn, err
	}
	if sn.PaymentAddress, err = r.readFixed20(); err != nil {
		return sn, err
	}
	if sn.Collateral, err = r.readCollateral(); err != nil {
		return sn, err
	}
	if sn.BestBlock, err = r.readInt32(); err != nil {
		return sn, err
	}
	if sn.BestBlockHash, err = r.readUint256(); err != nil {
		return sn, err
	}
	if sn.Signature, err = r.readVarBytes(); err != nil {
		return sn, err
	}
	return sn, nil
}

// maxCollateralCount mirrors consensus snMaxCollateralCount (params.h): the
// maximum utxos a servicenode may use as collateral (servicenode.h:430).
const maxCollateralCount = 10

// compactSize encodes n as a Bitcoin CompactSize varint (p2p writeVarInt,
// envelope.go:72-92). The registration sigHash serialization needs it for the
// collateral vector (servicenode.h:104-111).
func compactSize(n int) []byte {
	switch {
	case n < 0xfd:
		return []byte{byte(n)}
	case n <= 0xffff:
		return []byte{0xfd, byte(n), byte(n >> 8)}
	case n <= 0xffffffff:
		return []byte{0xfe, byte(n), byte(n >> 8), byte(n >> 16), byte(n >> 24)}
	default:
		b := make([]byte, 9)
		b[0] = 0xff
		for i := 0; i < 8; i++ {
			b[i+1] = byte(n >> (8 * i))
		}
		return b
	}
}

// serializeSigHashFields serializes the exact bytes ServiceNode::CreateSigHash
// hashes over (servicenode.h:104-111): snodePubKey (varstr), tier (uint8),
// paymentAddress (20 bytes), collateral (CompactSize count + count COutPoints),
// bestBlock (uint32 LE — the wire's int32 has identical bytes), bestBlockHash
// (32 bytes). CHashWriter(SER_GETHASH,0) is SHA256d over exactly these bytes.
func serializeSigHashFields(sn ServiceNode) []byte {
	b := p2p.MarshalVarStr(string(sn.PubKey[:]))
	b = append(b, sn.Tier)
	b = append(b, sn.PaymentAddress[:]...)
	b = append(b, compactSize(len(sn.Collateral))...)
	for _, op := range sn.Collateral {
		b = append(b, op.TxID[:]...)
		b = append(b, byte(op.Vout), byte(op.Vout>>8), byte(op.Vout>>16), byte(op.Vout>>24))
	}
	b = append(b, byte(sn.BestBlock), byte(sn.BestBlock>>8), byte(sn.BestBlock>>16), byte(sn.BestBlock>>24))
	b = append(b, sn.BestBlockHash[:]...)
	return b
}

// verifyRegistrationSignature mirrors the signature step of ServiceNode::isValid
// (servicenode.h:438-441): RecoverCompact over the CreateSigHash must succeed.
// C++ then matches the recovered pubkey's ID against the collateral utxos on
// chain (:447-476) — that needs a full chain index and is a documented
// thin-client limitation, so only the recoverability of a valid key is enforced
// here.
func verifyRegistrationSignature(sn ServiceNode) bool {
	if len(sn.Signature) == 0 {
		return false
	}
	hash := crypto.DoubleSHA256(serializeSigHashFields(sn))
	_, err := crypto.RecoverCompact(sn.Signature, hash[:])
	return err == nil
}

// SignRegistration signs the registration's CreateSigHash with priv and returns
// a copy with Signature set, mirroring ServiceNode::sign (servicenode.h:376-380).
// For SPV nodes the registration is signed by the collateral privkey; the caller
// chooses the key. Exposed for tests and for a node registering itself.
func SignRegistration(sn ServiceNode, priv []byte) (ServiceNode, error) {
	if len(priv) != 32 {
		return sn, errors.New("servicenode: registration signer must be 32 bytes")
	}
	hash := crypto.DoubleSHA256(serializeSigHashFields(sn))
	sig, err := crypto.SignCompact(priv, hash[:])
	if err != nil {
		return sn, err
	}
	sn.Signature = sig
	return sn, nil
}

// hasDupCollateral reports duplicate COutPoints, mirroring the std::set
// dedupe check (servicenode.h:434-436).
func hasDupCollateral(collateral []CollateralUTXO) bool {
	if len(collateral) < 2 {
		return false
	}
	seen := make(map[CollateralUTXO]struct{}, len(collateral))
	for _, op := range collateral {
		if _, dup := seen[op]; dup {
			return true
		}
		seen[op] = struct{}{}
	}
	return false
}

// registrationValid applies the thin-client-enforceable subset of
// ServiceNode::isValid (servicenode.h:398-484). The on-chain checks it cannot
// make — block ancestry (:401) and collateral utxo existence/amount/ownership
// (:447-478, total >= COLLATERAL_SPV) — need a full chain index and are
// documented as a thin-client limitation in docs/audit/register.md (WIRE-F71).
func registrationValid(sn ServiceNode) bool {
	if sn.Tier != TierSPV {
		return false // servicenode.h:409
	}
	if !fullyValidCPubKey(sn.PubKey) {
		return false // servicenode.h:405
	}
	if sn.PaymentAddress == ([20]byte{}) {
		return false // servicenode.h:426, CKeyID::IsNull
	}
	if len(sn.Collateral) == 0 || len(sn.Collateral) > maxCollateralCount {
		return false // servicenode.h:430
	}
	if hasDupCollateral(sn.Collateral) {
		return false // servicenode.h:434-436
	}
	if !verifyRegistrationSignature(sn) {
		return false // servicenode.h:438-441
	}
	return true
}

// parseConfig mirrors ServiceNode::parseConfig (servicenode.h:485-561): require
// valid JSON, strict numeric xbridgeversion + xrouterversion (both via
// get_int(), so float/exponent/out-of-int32 forms abort the parse exactly like
// C++). A bad xrouterversion aborts after xbridgeversion was assigned, so the
// parsed version is retained with no services (servicenode.h:546,550-551) —
// such an entry passes the version gate but fails the services gate. Only SPV
// tiers collect the xbridge array.
func parseConfig(config string, tier uint8) ([]string, uint32) {
	if config == "" {
		return nil, 0
	}
	var uv map[string]json.RawMessage
	if err := json.Unmarshal([]byte(config), &uv); err != nil {
		xlog.Warn("servicenode: config JSON invalid", "err", err)
		return nil, 0
	}
	ver, ok := numericUint32(uv, "xbridgeversion")
	if !ok {
		xlog.Warn("servicenode: config missing numeric xbridgeversion/xrouterversion")
		return nil, 0
	}
	if _, ok := numericUint32(uv, "xrouterversion"); !ok {
		xlog.Warn("servicenode: config missing numeric xbridgeversion/xrouterversion")
		// C++ returns false here AFTER assigning xbridgeversion (servicenode.h:
		// 546, 550-551), so the parsed version is retained with no services.
		return nil, ver
	}
	if tier != TierSPV {
		xlog.Debug("servicenode: non-SPV tier ignored for wallet services", "tier", tier)
		return nil, ver
	}
	raw, ok := uv["xbridge"]
	if !ok {
		return nil, ver
	}
	var arr []string
	if err := json.Unmarshal(raw, &arr); err != nil {
		xlog.Warn("servicenode: xbridge config array invalid", "err", err)
		return nil, ver
	}
	return arr, ver
}

// numericUint32 mirrors UniValue::get_int() (univalue_get.cpp:104-112): a bare
// int32 token only; floats/exponents/overflow and quoted strings throw, so
// parseConfig keeps version 0 (servicenode.h:614-615).
func numericUint32(uv map[string]json.RawMessage, key string) (uint32, bool) {
	raw, ok := uv[key]
	if !ok {
		return 0, false
	}
	t := bytes.TrimSpace(raw)
	if len(t) > 0 && t[0] == '"' {
		return 0, false // C++ get_int(): VNUM only, a quoted string throws
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, false
	}
	i, err := strconv.ParseInt(string(n), 10, 32)
	if err != nil {
		return 0, false
	}
	return uint32(i), true
}

// entry is the registry record for one servicenode.
type entry struct {
	services       []string
	xbridgeVersion uint32
	paymentAddress [20]byte // registration CKeyID; XBridge fee destination (xbridgeapp.cpp:2394-2401)
	pingTime       uint32   // last ping time, clamped like C++ updatePing (servicenode.h:254-260); 0 = never pinged
}

// Registry is the in-memory servicenode store. It is safe for concurrent use.
type Registry struct {
	mu    sync.RWMutex
	nodes map[[33]byte]*entry
	pings map[[33]byte]uint32 // last RAW reported pingTime, addPing gate (servicenodemgr.h:843-852)
	now   func() time.Time    // overridable in tests
}

// NewRegistry builds an empty registry.
func NewRegistry() *Registry {
	return &Registry{
		nodes: make(map[[33]byte]*entry),
		pings: make(map[[33]byte]uint32),
		now:   time.Now,
	}
}

// AddRegistration records a servicenode learned via SNREGISTER. C++ addSn
// replaces the entry wholesale (servicenodemgr.h:861-871); a registration
// carries no config on the wire (servicenode.h:354-367), so version/services
// reset to 0/empty and the node fails the Pick gate. A registration failing
// the thin-client subset of isValid is dropped exactly like C++ addSn
// (servicenodemgr.h:862, servicenode.h:398-484). A fresh snode's pingtime is 0
// (not serialized, servicenode.h:354-367), so the node is not running() until
// its next ping.
func (r *Registry) AddRegistration(sn ServiceNode) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !registrationValid(sn) {
		return
	}
	k := sn.PubKey
	e, ok := r.nodes[k]
	if !ok {
		e = &entry{}
		r.nodes[k] = e
	}
	e.services = sn.Services
	e.xbridgeVersion = sn.XBridgeVersion
	e.paymentAddress = sn.PaymentAddress
	e.pingTime = 0
	xlog.Debug("servicenode: registration stored", "pubkey", hex33(k), "services", len(e.services), "seen", ok)
}

// AddPing records a servicenode ping (SNPING / SNLISTPING). C++ processPing
// (servicenodemgr.h:177-194): only SPV-tier pings with a non-empty service list
// pass isValid (servicenode.h:787,794-795) and reach addPing's strict-newer
// gate (:843-852) then addSn's wholesale replace (:861-871, at :192) via
// setConfig (servicenode.h:277-281). A ping failing isValid never creates a
// node — C++ never knows it (:186-187). The stored pingtime is clamped like
// updatePing (servicenode.h:254-260) and drives running() (:244-247).
// It returns whether the ping was actually stored (the strict-newer gate
// passed), which lets callers mirror the wire response set for SNLIST.
func (r *Registry) AddPing(sn ServiceNode) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	k := sn.PubKey
	if sn.Tier == TierSPV && len(sn.Services) > 0 && fullyValidCPubKey(sn.PubKey) {
		// addPing gate compares the RAW reported pingTime against the last one
		// for this pubkey (servicenodemgr.h:843-852); equal is rejected.
		last, known := r.pings[k]
		if !known || sn.PingTime > last {
			r.pings[k] = sn.PingTime
			e, ok := r.nodes[k]
			if !ok {
				e = &entry{}
				r.nodes[k] = e
			}
			e.services = sn.Services
			e.xbridgeVersion = sn.XBridgeVersion
			e.paymentAddress = sn.PaymentAddress
			e.pingTime = clampPingTime(sn.PingTime, r.now().Unix())
			xlog.Debug("servicenode: ping stored", "pubkey", hex33(k), "services", len(e.services))
			return true
		}
	}
	return false
}

// clampPingTime mirrors ServiceNode::updatePing (servicenode.h:254-260): a 0 or
// future reported time falls back to the current time, else the reported time.
func clampPingTime(reported uint32, now int64) uint32 {
	if reported == 0 || int64(reported) > now {
		return uint32(now)
	}
	return reported
}

const runningWindow = 5 * time.Minute

// running reports whether the servicenode was pinged within the 5-minute window
// (mirrors ServiceNode::running(), servicenode.h:244-247). The subtraction is
// signed like C++ GetAdjustedTime() - pingtime, so a pingtime in the future
// (clock skew) still counts as running.
func (r *Registry) running(e *entry) bool {
	if e == nil {
		return false
	}
	return r.now().Unix()-int64(e.pingTime) < int64(runningWindow/time.Second)
}

// Pick selects a hub servicenode for an order pair. It is the faithful port of
// App::findNodeWithService → App::Impl::findShuffledNodesWithService
// (xbridgeapp.cpp:2784-2793, 2901-2936): keep nodes whose advertised xbridge
// version equals XBRIDGE_PROTOCOL_VERSION, that are running(), and whose service
// list contains every requested currency; shuffle; return the first. An empty
// result means no eligible hub (C++ sendXBridgeTransaction fails the order
// with NO_SERVICE_NODE, xbridgeapp.cpp:1515). C++ also excludes a `notIn`
// key set (:2910), omitted because Go's sole caller (api/node.go:921) passes
// an empty set like sendXBridgeTransaction (:1507-1508); rebroadcast callers
// (:3274, :3311) pass non-empty sets but are not ported here. The shuffle
// uses Go's rand rather than C++'s seed-0 default_random_engine — the choice
// has no wire effect.
func (r *Registry) Pick(need []string) ([33]byte, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var list [][33]byte
	for k, e := range r.nodes {
		if e.xbridgeVersion != proto.ProtocolVersion || !r.running(e) {
			continue
		}
		// C++ searchCounter = requested_services.size() and a candidate is only
		// pushed when --searchCounter == 0, so an empty request never pushes a
		// node (xbridgeapp.cpp:2924-2930) and findNodeWithService returns false.
		// containsAll would vacuously match, so reproduce the C++ no-push.
		if len(need) == 0 || !containsAll(e.services, need) {
			continue
		}
		list = append(list, k)
	}
	if len(list) == 0 {
		return [33]byte{}, false
	}
	rand.Shuffle(len(list), func(i, j int) { list[i], list[j] = list[j], list[i] })
	return list[0], true
}

// containsAll reports whether set contains every element of need.
func containsAll(set, need []string) bool {
	for _, want := range need {
		found := false
		for _, have := range set {
			if have == want {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// WalletServices ports xbridgeapp.cpp:2758 App::walletServices(): for each
// running servicenode, take its service list, keep tokens matching ^[^:]+$ and
// exclude xr/xrs, keyed by pubkey. It returns the de-duplicated union of token
// names (not keyed) for use by dxGetNetworkTokens.
func (r *Registry) WalletServices() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	set := map[string]bool{}
	running := 0
	for _, e := range r.nodes {
		if !r.running(e) {
			continue
		}
		running++
		for _, s := range e.services {
			if !walletTokenRe.MatchString(s) || xrExclude[s] {
				continue
			}
			set[s] = true
		}
	}
	if walletServicesLog.Event("walletservices") {
		xlog.Debug("servicenode: WalletServices", "known", len(r.nodes), "running", running, "tokens", len(set))
	}
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// Count returns the number of known servicenodes (for diagnostics/tests).
func (r *Registry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.nodes)
}

// Known reports whether a servicenode pubkey is present in the registry.
func (r *Registry) Known(key [33]byte) bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.nodes[key]
	return ok
}

// PaymentAddress returns the registration payment address of a known
// servicenode (its CKeyID, servicenode.h:180). XBridge uses it as the
// service-node fee destination on the accepting side (xbridgeapp.cpp:2394-2401);
// the Go taker looks it up by hub pubkey when building the AcceptingBody fee
// transaction (B2). An unknown pubkey returns false.
func (r *Registry) PaymentAddress(key [33]byte) ([20]byte, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.nodes[key]
	if !ok {
		return [20]byte{}, false
	}
	return e.paymentAddress, true
}

// fullyValidCPubKey mirrors CPubKey::IsFullyValid (servicenode.h:405,791): a
// 33-byte compressed secp256k1 pubkey whose point is on the curve. The trivial
// 02/03-prefix check is insufficient — C++ rejects invalid points (pubkey.cpp:
// 157-196).
func fullyValidCPubKey(pk [33]byte) bool {
	return crypto.FullyValidPubKey(pk)
}

var errPubkeyMismatch = errors.New("servicenode: ping outer pubkey invalid or not equal to embedded snode pubkey")

var errBadSignature = errors.New("servicenode: ping signature does not match pubkey")

var errInvalidRegistration = errors.New("servicenode: embedded registration fails the thin-client isValid subset")

type shortErr string

func (e shortErr) Error() string { return "servicenode: " + string(e) + " truncated" }

func errShort(f string) error { return shortErr(f) }

func littleEndian32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

// hex33 renders a 33-byte compressed pubkey as hex for log lines.
func hex33(pk [33]byte) string {
	const hx = "0123456789abcdef"
	out := make([]byte, 66)
	for i := 0; i < 33; i++ {
		out[i*2] = hx[pk[i]>>4]
		out[i*2+1] = hx[pk[i]&0x0f]
	}
	return string(out)
}
