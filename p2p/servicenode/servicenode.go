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

// ServiceNode is the parsed servicenode record. Only the fields needed to derive
// the network token set are retained. For pings, PingTime is the raw
// peer-reported ping timestamp (seconds); the registry clamps it like C++
// updatePing (servicenode.h:254-260) before gating running().
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

// skipCollateral advances past a std::vector<COutPoint> (varint count + count
// COutPoints). Each COutPoint is txid(32) + vout(4). The collateral is not
// needed for token discovery.
func (r *reader) skipCollateral() error {
	n, off, err := p2p.ReadVarInt(r.b, r.pos)
	if err != nil {
		return err
	}
	r.pos = off
	if r.remaining() < n*(32+4) {
		return errShort("collateral")
	}
	r.pos += n * (32 + 4)
	return nil
}

// ParseServiceNode parses an SNREGISTER payload (ServiceNode::SerializationOp).
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
	if _, err = r.readFixed20(); err != nil { // paymentAddress (CKeyID, 20 raw bytes)
		return sn, err
	}
	if err = r.skipCollateral(); err != nil {
		return sn, err
	}
	if _, err = r.readInt32(); err != nil { // bestBlock (int32)
		return sn, err
	}
	if _, err = r.readUint256(); err != nil { // bestBlockHash
		return sn, err
	}
	if _, err = r.readVarBytes(); err != nil { // signature
		return sn, err
	}
	xlog.Debug("servicenode: SNREGISTER parsed", "pubkey", hex33(sn.PubKey), "tier", sn.Tier)
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
	if !validCPubKey(outerPubkey) || outerPubkey != inner.PubKey {
		return sn, errPubkeyMismatch
	}
	sn.PubKey = inner.PubKey
	sn.Tier = inner.Tier
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
	// NOTE: the per-copy "SNPING parsed" log was removed — a single
	// ping is relayed by every peer, so the parser fired once per copy.
	// The logical ping is logged once (per genuine ping) by Registry.AddPing.
	return sn, nil
}

// parseInnerServiceNode parses the trailing embedded ServiceNode of a
// ServiceNodePing (snodePubKey, tier, paymentAddress, collateral, bestBlock,
// bestBlockHash, signature). It does NOT re-read config (the ping carries it at
// the outer level).
func parseInnerServiceNode(r *reader) (ServiceNode, error) {
	sn := ServiceNode{}
	var err error
	if sn.PubKey, err = r.readCPubKey(); err != nil {
		return sn, err
	}
	if sn.Tier, err = r.readUint8(); err != nil {
		return sn, err
	}
	if _, err = r.readFixed20(); err != nil {
		return sn, err
	}
	if err = r.skipCollateral(); err != nil {
		return sn, err
	}
	if _, err = r.readInt32(); err != nil {
		return sn, err
	}
	if _, err = r.readUint256(); err != nil {
		return sn, err
	}
	if _, err = r.readVarBytes(); err != nil {
		return sn, err
	}
	return sn, nil
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
	pingTime       uint32 // last ping time, clamped like C++ updatePing (servicenode.h:254-260); 0 = never pinged
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
// reset to 0/empty and the node fails the Pick gate. isValid requires the SPV
// tier (servicenode.h:409-410) and a fully valid pubkey (:405-406);
// collateral/sig/block checks need a full chain index (see docs/AUDIT.md). A
// fresh snode's pingtime is 0 (not serialized, servicenode.h:354-367), so the
// node is not running() until its next ping.
func (r *Registry) AddRegistration(sn ServiceNode) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if sn.Tier != TierSPV || !validCPubKey(sn.PubKey) {
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
func (r *Registry) AddPing(sn ServiceNode) {
	r.mu.Lock()
	defer r.mu.Unlock()
	k := sn.PubKey
	if sn.Tier == TierSPV && len(sn.Services) > 0 && validCPubKey(sn.PubKey) {
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
			e.pingTime = clampPingTime(sn.PingTime, r.now().Unix())
			xlog.Debug("servicenode: ping stored", "pubkey", hex33(k), "services", len(e.services))
		}
	}
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

// validCPubKey mirrors CPubKey::IsFullyValid (servicenode.h:405,791): a 33-byte
// compressed secp256k1 pubkey (prefix 02 or 03).
func validCPubKey(pk [33]byte) bool {
	return pk[0] == 0x02 || pk[0] == 0x03
}

var errPubkeyMismatch = errors.New("servicenode: ping outer pubkey invalid or not equal to embedded snode pubkey")

var errBadSignature = errors.New("servicenode: ping signature does not match pubkey")

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
