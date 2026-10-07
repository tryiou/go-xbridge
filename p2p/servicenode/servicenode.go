// Package servicenode parses the servicenode P2P messages a thin XBridge client uses to learn the network's token set.
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
	"go-xbridge/version"
)

var walletServicesLog = xlog.NewDedupe(60*time.Second, nil)

const (
	CmdSNRegister = "snr"
	CmdSNPing     = "snp"
	CmdSNList     = "snl" // SNLIST: a request; this code never sends it
	CmdSNListPing = "snlp"
)

// walletTokenRe matches a bare wallet token; XRouter service names embed ':' and are excluded.
var walletTokenRe = regexp.MustCompile(`^[^:]+$`)

var xrExclude = map[string]bool{"xr": true, "xrs": true}

// CollateralUTXO is one COutPoint of a servicenode registration's collateral vector.
type CollateralUTXO struct {
	TxID [32]byte
	Vout uint32
}

// ServiceNode is the parsed servicenode record.
type ServiceNode struct {
	PubKey   [33]byte
	Tier     uint8
	Config   string // raw JSON config (carries the xbridge token array)
	Services []string
	PingTime uint32

	// XBridgeVersion is the SN's advertised xbridgeversion; hub selection requires it to match XBRIDGE_PROTOCOL_VERSION.
	XBridgeVersion uint32

	PaymentAddress [20]byte
	Collateral     []CollateralUTXO
	BestBlock      int32
	BestBlockHash  [32]byte
	Signature      []byte
}

const (
	// TierOpen and TierSPV are the servicenode tier values; only TierSPV nodes advertise XBridge wallet services.
	TierOpen = 0
	TierSPV  = 50
)

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
	out := make([]CollateralUTXO, n)
	for i := range out {
		copy(out[i].TxID[:], r.b[r.pos:r.pos+32])
		out[i].Vout = littleEndian32(r.b[r.pos+32 : r.pos+36])
		r.pos += 32 + 4
	}
	return out, nil
}

// ParseServiceNode parses an SNREGISTER payload.
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
	xlog.Debug("servicenode: SNREGISTER parsed", "pubkey", hex33(sn.PubKey), "tier", sn.Tier, "collateral", len(sn.Collateral))
	return sn, nil
}

// ParseServiceNodePing parses an SNPING / SNLISTPING payload.
func ParseServiceNodePing(b []byte) (ServiceNode, error) {
	r := &reader{b: b}
	sn := ServiceNode{}
	var err error
	var outerPubkey [33]byte
	if outerPubkey, err = r.readCPubKey(); err != nil {
		return sn, err
	}
	if _, err = r.readUint32(); err != nil {
		return sn, err
	}
	if _, err = r.readUint256(); err != nil {
		return sn, err
	}
	var pingTime uint32
	if pingTime, err = r.readUint32(); err != nil {
		return sn, err
	}
	var config string
	if config, err = r.readVarStr(); err != nil {
		return sn, err
	}
	inner, err := parseInnerServiceNode(r)
	if err != nil {
		return sn, err
	}
	if !fullyValidCPubKey(outerPubkey) || outerPubkey != inner.PubKey {
		return sn, errPubkeyMismatch
	}
	sn = inner
	sn.Config = config
	sn.Services, sn.XBridgeVersion = parseConfig(config, sn.Tier)
	sn.PingTime = pingTime
	sigStart := r.pos
	signature, err := r.readVarBytes()
	if err != nil {
		return sn, err
	}
	hash := crypto.DoubleSHA256(b[:sigStart])
	pub, err := crypto.RecoverCompact(signature, hash[:])
	if err != nil || !bytes.Equal(pub, outerPubkey[:]) {
		return sn, errBadSignature
	}
	if !registrationValid(sn) {
		return sn, errInvalidRegistration
	}
	return sn, nil
}

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

// maxCollateralCount mirrors snMaxCollateralCount (params.h): the consensus cap on collateral UTXOs per servicenode.
const maxCollateralCount = 10

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

// serializeSigHashFields mirrors CreateSigHash (servicenode.h): the exact bytes the registration signature commits to.
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

func verifyRegistrationSignature(sn ServiceNode) bool {
	if len(sn.Signature) == 0 {
		return false
	}
	hash := crypto.DoubleSHA256(serializeSigHashFields(sn))
	_, err := crypto.RecoverCompact(sn.Signature, hash[:])
	return err == nil
}

// SignRegistration signs the registration sigHash with priv and returns a copy with Signature set.
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

// registrationValid applies the thin-client-enforceable subset of ServiceNode::isValid; on-chain checks are documented in docs/architecture.md.
func registrationValid(sn ServiceNode) bool {
	if sn.Tier != TierSPV {
		return false
	}
	if !fullyValidCPubKey(sn.PubKey) {
		return false
	}
	if sn.PaymentAddress == ([20]byte{}) {
		return false
	}
	if len(sn.Collateral) == 0 || len(sn.Collateral) > maxCollateralCount {
		return false
	}
	if hasDupCollateral(sn.Collateral) {
		return false
	}
	if !verifyRegistrationSignature(sn) {
		return false
	}
	return true
}

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
		// A bad xrouterversion keeps the already-parsed xbridgeversion: only the services are dropped.
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

func numericUint32(uv map[string]json.RawMessage, key string) (uint32, bool) {
	raw, ok := uv[key]
	if !ok {
		return 0, false
	}
	t := bytes.TrimSpace(raw)
	if len(t) > 0 && t[0] == '"' {
		return 0, false
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

type entry struct {
	services       []string
	xbridgeVersion uint32
	paymentAddress [20]byte
	pingTime       uint32 // last accepted ping time; 0 = never pinged
}

// Registry is the in-memory servicenode store. It is safe for concurrent use.
type Registry struct {
	mu    sync.RWMutex
	nodes map[[33]byte]*entry
	pings map[[33]byte]uint32 // last raw reported pingTime; the newer gate compares raw values, not the clamped entry time
	// raw holds each accepted ping's payload so the SNLIST echo re-serializes
	// byte-identically: one overwrite per accepted pubkey, never appended.
	raw map[[33]byte][]byte
	now func() time.Time // overridable in tests
}

// NewRegistry builds an empty registry.
func NewRegistry() *Registry {
	return &Registry{
		nodes: make(map[[33]byte]*entry),
		pings: make(map[[33]byte]uint32),
		raw:   make(map[[33]byte][]byte),
		now:   time.Now,
	}
}

// AddRegistration records an SNREGISTER-learned servicenode; a registration carries no config or ping time, so the node serves nothing and is not running until its next strictly-newer ping.
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

// AddPing records an SPV-tier ping (services non-empty, pubkey fully valid) only when its
// raw ping time is strictly newer than the last; reports whether it was stored and retains optional raw for the SNLIST echo.
func (r *Registry) AddPing(sn ServiceNode, raw ...[]byte) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	k := sn.PubKey
	if sn.Tier == TierSPV && len(sn.Services) > 0 && fullyValidCPubKey(sn.PubKey) {
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
			if len(raw) > 0 {
				r.raw[k] = append([]byte(nil), raw[0]...)
			}
			xlog.Debug("servicenode: ping stored", "pubkey", hex33(k), "services", len(e.services))
			return true
		}
	}
	return false
}

// AcceptedRawPings returns the stored accepted ping payloads with their pubkeys in ascending pubkey order, giving deterministic SNLIST echoes.
func (r *Registry) AcceptedRawPings() ([][33]byte, [][]byte) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	keys := make([][33]byte, 0, len(r.raw))
	for k := range r.raw {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return bytes.Compare(keys[i][:], keys[j][:]) < 0 })
	pings := make([][]byte, 0, len(keys))
	for _, k := range keys {
		pings = append(pings, append([]byte(nil), r.raw[k]...))
	}
	return keys, pings
}

func clampPingTime(reported uint32, now int64) uint32 {
	if reported == 0 || int64(reported) > now {
		return uint32(now)
	}
	return reported
}

const runningWindow = 5 * time.Minute

// running reports whether e's ping time lies within the running window; the
// subtraction is signed, so a hypothetical future time would still count as running.
func (r *Registry) running(e *entry) bool {
	if e == nil {
		return false
	}
	return r.now().Unix()-int64(e.pingTime) < int64(runningWindow/time.Second)
}

// Pick selects a running servicenode advertising the current protocol version and every token in need, shuffled at random; pubkeys in exclude are never selected.
func (r *Registry) Pick(need []string, exclude ...[33]byte) ([33]byte, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	banned := make(map[[33]byte]bool, len(exclude))
	for _, k := range exclude {
		banned[k] = true
	}
	var list [][33]byte
	for k, e := range r.nodes {
		if banned[k] {
			continue
		}
		if e.xbridgeVersion != version.XBridgeProtocolVersion || !r.running(e) {
			continue
		}
		// An empty need must select nothing: containsAll would match vacuously,
		// so the explicit guard is required.
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

// WalletServices returns the sorted de-duplicated union of wallet tokens advertised by currently running servicenodes.
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

// Count returns the number of known servicenodes.
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

// PaymentAddress returns the registration payment address of a known servicenode, the XBridge fee destination; an unknown pubkey reports false.
func (r *Registry) PaymentAddress(key [33]byte) ([20]byte, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.nodes[key]
	if !ok {
		return [20]byte{}, false
	}
	return e.paymentAddress, true
}

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

func hex33(pk [33]byte) string {
	const hx = "0123456789abcdef"
	out := make([]byte, 66)
	for i := 0; i < 33; i++ {
		out[i*2] = hx[pk[i]>>4]
		out[i*2+1] = hx[pk[i]&0x0f]
	}
	return string(out)
}
