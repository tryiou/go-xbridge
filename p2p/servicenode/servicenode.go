// Package servicenode ports the Blocknet C++ servicenode P2P message handling
// used by a thin XBridge client to learn the network's token set.
//
// Reference (blocknet_core):
//   - src/servicenode/servicenode.h  ServiceNode::SerializationOp (355-367),
//     ServiceNodePing::SerializationOp (683-694), ServiceNode::parseConfig
//     (485-561), ServiceNode::running() (244).
//   - src/xbridge/xbridgeapp.cpp       App::walletServices() (2758).
//   - src/protocol.cpp                 command names snr/snp/snl/snlp (45-49).
//
// A stock XBridge wallet does NOT send SNLIST (only XRouter does,
// src/xrouter/xrouterpeermgr.cpp:536). It learns the servicenode set from
// relayed SNPING / SNREGISTER / SNLISTPING messages
// (src/net_processing.cpp:2976-2981 relays SNPING to all peers). This package
// parses exactly those incoming messages and derives the network token union via
// the same logic as walletServices(): keep only SPV-tier xbridge tokens matching
// ^[^:]+$, exclude the xr/xrs XRouter services, and only include servicenodes
// whose last ping is < 5 minutes old (running()).
package servicenode

import (
	"encoding/json"
	"regexp"
	"sort"
	"sync"
	"time"

	xlog "xbridge-go/log"
	"xbridge-go/p2p"
)

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
// the network token set are retained. For pings, PingTime is the peer-reported
// ping timestamp (seconds); 0 means unset (registry falls back to ingest time).
type ServiceNode struct {
	PubKey   [33]byte
	Tier     uint8
	Config   string // raw JSON config (carries the xbridge token array)
	Services []string
	PingTime uint32
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
		xlog.Warn("servicenode: SNREGISTER parse failed at pubkey", "err", err, "len", len(b))
		return sn, err
	}
	if sn.Tier, err = r.readUint8(); err != nil {
		xlog.Warn("servicenode: SNREGISTER parse failed at tier", "pubkey", hex33(sn.PubKey), "err", err)
		return sn, err
	}
	if _, err = r.readFixed20(); err != nil { // paymentAddress (CKeyID, 20 raw bytes)
		xlog.Warn("servicenode: SNREGISTER parse failed at paymentAddress", "pubkey", hex33(sn.PubKey), "err", err)
		return sn, err
	}
	if err = r.skipCollateral(); err != nil {
		xlog.Warn("servicenode: SNREGISTER parse failed at collateral", "pubkey", hex33(sn.PubKey), "err", err)
		return sn, err
	}
	if _, err = r.readInt32(); err != nil { // bestBlock (int32)
		xlog.Warn("servicenode: SNREGISTER parse failed at bestBlock", "pubkey", hex33(sn.PubKey), "err", err)
		return sn, err
	}
	if _, err = r.readUint256(); err != nil { // bestBlockHash
		xlog.Warn("servicenode: SNREGISTER parse failed at bestBlockHash", "pubkey", hex33(sn.PubKey), "err", err)
		return sn, err
	}
	if _, err = r.readVarBytes(); err != nil { // signature
		xlog.Warn("servicenode: SNREGISTER parse failed at signature", "pubkey", hex33(sn.PubKey), "err", err)
		return sn, err
	}
	xlog.Info("servicenode: SNREGISTER parsed", "pubkey", hex33(sn.PubKey), "tier", sn.Tier)
	return sn, nil
}

// ParseServiceNodePing parses an SNPING / SNLISTPING payload
// (ServiceNodePing::SerializationOp). It runs parseConfig on the embedded JSON
// config to populate the SPV xbridge token list.
func ParseServiceNodePing(b []byte) (ServiceNode, error) {
	r := &reader{b: b}
	sn := ServiceNode{}
	var err error
	if _, err = r.readCPubKey(); err != nil { // 1. ping snodePubKey
		xlog.Warn("servicenode: SNPING parse failed at pubkey", "err", err, "len", len(b))
		return sn, err
	}
	if _, err = r.readUint32(); err != nil { // 2. bestBlock (uint32 in ping)
		xlog.Warn("servicenode: SNPING parse failed at bestBlock", "err", err)
		return sn, err
	}
	if _, err = r.readUint256(); err != nil { // 3. bestBlockHash
		xlog.Warn("servicenode: SNPING parse failed at bestBlockHash", "err", err)
		return sn, err
	}
	var pingTime uint32
	if pingTime, err = r.readUint32(); err != nil { // 4. pingTime (uint32)
		xlog.Warn("servicenode: SNPING parse failed at pingTime", "err", err)
		return sn, err
	}
	var config string
	if config, err = r.readVarStr(); err != nil { // 5. config (JSON varstr)
		xlog.Warn("servicenode: SNPING parse failed at config", "err", err)
		return sn, err
	}
	// 6. embedded ServiceNode (carries tier; the record the SN list is built
	// from). parseConfig is applied to the ping's config string.
	inner, err := parseInnerServiceNode(r)
	if err != nil {
		xlog.Warn("servicenode: SNPING parse failed at inner servicenode", "err", err)
		return sn, err
	}
	sn.PubKey = inner.PubKey
	sn.Tier = inner.Tier
	sn.Config = config
	sn.Services = parseConfig(config, sn.Tier)
	sn.PingTime = pingTime
	if _, err = r.readVarBytes(); err != nil { // 7. signature (empty for SNPING)
		xlog.Warn("servicenode: SNPING parse failed at signature", "pubkey", hex33(sn.PubKey), "err", err)
		return sn, err
	}
	xlog.Info("servicenode: SNPING parsed", "pubkey", hex33(sn.PubKey), "tier", sn.Tier, "services", len(sn.Services))
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
// valid JSON, numeric xbridgeversion + xrouterversion, and (only for SPV tier)
// collect the xbridge array. Non-SPV tiers yield no wallet services — matching
// C++ ("xbridge only supports SPV nodes").
func parseConfig(config string, tier uint8) []string {
	if config == "" {
		return nil
	}
	var uv map[string]json.RawMessage
	if err := json.Unmarshal([]byte(config), &uv); err != nil {
		xlog.Warn("servicenode: config JSON invalid", "err", err)
		return nil
	}
	if !hasNumeric(uv, "xbridgeversion") || !hasNumeric(uv, "xrouterversion") {
		xlog.Warn("servicenode: config missing numeric xbridgeversion/xrouterversion")
		return nil
	}
	if tier != TierSPV {
		xlog.Debug("servicenode: non-SPV tier ignored for wallet services", "tier", tier)
		return nil
	}
	raw, ok := uv["xbridge"]
	if !ok {
		return nil
	}
	var arr []string
	if err := json.Unmarshal(raw, &arr); err != nil {
		xlog.Warn("servicenode: xbridge config array invalid", "err", err)
		return nil
	}
	return arr
}

func hasNumeric(uv map[string]json.RawMessage, key string) bool {
	raw, ok := uv[key]
	if !ok {
		return false
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err != nil {
		return false
	}
	_, err := n.Float64()
	return err == nil
}

// entry is the registry record for one servicenode.
type entry struct {
	services []string
	seen     time.Time
}

// Registry is the in-memory servicenode store. It is safe for concurrent use.
type Registry struct {
	mu    sync.RWMutex
	nodes map[[33]byte]*entry
	now   func() time.Time // overridable in tests
}

// NewRegistry builds an empty registry.
func NewRegistry() *Registry {
	return &Registry{
		nodes: make(map[[33]byte]*entry),
		now:   time.Now,
	}
}

// AddRegistration records a servicenode learned via SNREGISTER.
func (r *Registry) AddRegistration(sn ServiceNode) {
	r.mu.Lock()
	defer r.mu.Unlock()
	k := sn.PubKey
	e, ok := r.nodes[k]
	if !ok {
		e = &entry{}
		r.nodes[k] = e
	}
	if len(sn.Services) > 0 {
		e.services = sn.Services
	}
	xlog.Info("servicenode: registration stored", "pubkey", hex33(k), "services", len(e.services), "seen", ok)
}

// AddPing records a servicenode ping (SNPING / SNLISTPING). Both the token set
// (from parseConfig) and the ingest time (drives running()) are updated. C++
// ServiceNode::running() (servicenode.h:244) treats a node as running when its
// last ping is < 5 minutes ago; we stamp local ingest time and gate on that.
func (r *Registry) AddPing(sn ServiceNode) {
	r.mu.Lock()
	defer r.mu.Unlock()
	k := sn.PubKey
	e, ok := r.nodes[k]
	if !ok {
		e = &entry{}
		r.nodes[k] = e
	}
	if len(sn.Services) > 0 {
		e.services = sn.Services
	}
	e.seen = r.now()
	xlog.Info("servicenode: ping stored", "pubkey", hex33(k), "services", len(e.services))
}

const runningWindow = 5 * time.Minute

// running reports whether the servicenode was pinged within the 5-minute window
// (mirrors ServiceNode::running(), servicenode.h:244).
func (r *Registry) running(e *entry) bool {
	if e == nil {
		return false
	}
	age := r.now().Sub(e.seen)
	return age < runningWindow
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
	xlog.Debug("servicenode: WalletServices", "known", len(r.nodes), "running", running, "tokens", len(set))
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
