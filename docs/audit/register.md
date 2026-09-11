# Audit register — canonical finding list

Single source of truth for every known C++↔Go divergence and its status. The
detail behind each row lives in `findings.md` (per-finding cards) and
`evidence/` (per-axis deep dives); the remediation todo list lives in
[`remediation-plan.md`](remediation-plan.md). This file only tracks
**what is wrong and where it stands** — nothing else.

## §0 Fidelity standard (binding)

A swap sequence on go-xbridge must be identical to C++ in every observable:
wire bytes, state transitions, RPC shapes, error codes/messages, help texts,
fee/locktime math. No deviation is accepted on that path.

Each row therefore resolves to exactly one terminal verdict:
- **identical** — proven twin (proof pointer named, usually a test);
- **fix queued** — real divergence with a remediation work item;
- **waiver required** — unreachable for a thin client; decided explicitly,
  never by default. The sole standing waiver is the trading-data family below.
(`OPEN`/`FIXED`/`DOCUMENTED`/`RE-DECIDE` are workflow states tracking progress
toward a terminal verdict, not verdicts themselves.)

Rows still marked `DOCUMENTED` from the old "deliberate divergence" regime are
being re-triaged onto the three verdicts above; a `DOCUMENTED` row without a
verdict is unfinished work, not an accepted gap.

### Standing waiver: trading-data family only

`dxGetTradingData` (and lowercase `gettradingdata`, `fee_txid`/`nodepubkey`,
`blocks`/`errors` bounds, key order, error records) is excluded: it needs a
BLOCK block index and network-wide view (`api.md:375`, `B7:54`) a thin client
cannot replay. Everything else — every swap-phase packet, state, response,
error, and help text — falls under the standard above.

## Numbering & status legend

- **Canonical IDs** — every finding has exactly one axis-prefixed ID:
  `RPC-F01..F62`, `WIRE-F57..F72`, `STATE-F71..F86`, `CRYPTO-F77..F102`,
  `CFG-F84..F92`, `CONC-F92..F102`, `INV-F97..F100`, `SEC-F01..F04`. The 2026
  full audit (`F01–F99`) supplied the core; prior audits' `F1–F27`, `S2/S3/S4`
  and `S1-A…D` findings were folded in — duplicates mapped onto the surviving
  row, orphans promoted to new IDs. The one-to-one mapping lives in the
  [ID-history appendix](#id-history-appendix) at the bottom.

Status values:

| Status | Meaning |
|---|---|
| `OPEN` | Divergence present; assigned to a remediation branch |
| `FIXED` | Remediated at HEAD; branch or test that closed it is named |
| `DOCUMENTED` | Legacy mark: verdict pending re-triage under §0, or an explicit waiver ruling awaited — unfinished work, never an accepted gap |
| `RE-DECIDE` | Prior register documented it deliberate; the new audit disagrees — re-open the decision |
| `IDENTICAL` | Proven twin: no observable divergence; proof pointer named |
| `FIX-QUEUED` | Real divergence with a remediation work item (may combine with `WAIVER` for split rows) |
| `WAIVER` | Explicitly waived as unreachable for a thin client; reason named. Standing waiver: trading-data family (§0) |
| Combos | `FIXED + FIX-QUEUED` = closed with a tracked residual; `FIXED + WAIVER` likewise; `FIX-QUEUED + WAIVER` = split row (part fix-queued, part waived, e.g. RPC-F19). A `FIXED` row carries no open residual otherwise. |

Detail for every Current finding lives in [`findings.md`](findings.md)
(per-finding REF/CAND/IMPACT/FIX cards) and the per-axis deep dives in
[`evidence/`](evidence/); the register tables below carry only the status.

---

## Current register

### RPC axis (`RPC-F01`–`RPC-F62`) — evidence: `evidence/rpc.md`, `evidence/rpc-groups/`

| ID | Sev | Finding (one line) | Status | Owner |
|---|---|---|---|---|
| RPC-F01 | S2 | Error-channel policy: C++ *throws* envelope errors for param/type errors; Go returns business results | FIXED | B4 `fix/http-hardening` (strict parsers, envelope −1/−3, read-order, `limit` param; `TestStrictParamEnvelopeErrors`, `TestRpcTypeCheckErrors`) — no string coercion |
| RPC-F02 | S2 | `NO_SESSION` error `name` hardcoded `"dx"` instead of the method name | FIXED | B4 `fix/http-hardening` (`TestNoSessionNameIsMethodName`) |
| RPC-F03 | S2 | `dxGetOrders` array order random (Go map) vs id-ascending (C++ std::map) | FIXED | B7 `fix/rpc-surface` (`orderIDLess` LSB-first byte sort, C++ `std::map<uint256>` order — `TestDxGetOrdersSortedById`, `TestOrderIDLess`) |
| RPC-F04 | S3 | `dxGetOrders` 60 s filter boundary (µs-exact vs second-truncated) | FIXED | B7 `fix/rpc-surface` (second-truncated on creation time — `TestDxGetOrdersSixtySecondBoundary`) |
| RPC-F05 | S2 | Exactly-64-hex id gate vs C++ `uint256S` left-pad/truncate tolerance | FIXED | B7 `fix/rpc-surface` (tolerant `parseOrderIDS` — `TestParseOrderIDS`, `TestDxCancelOrderShortId`) |
| RPC-F06 | S3 | `dxGetOrder` not-found message renders id raw vs zero-padded | FIXED | B7 `fix/rpc-surface` (zero-padded 64-hex not-found id — `TestDxGetOrderNotFoundPadded`) |
| RPC-F07 | S3 | `dxCancelOrder` cancels *before* validating connectors (side-effect order) | FIXED | B7 `fix/rpc-surface` (connectors validated before cancel side effects — `TestDxCancelOrderSideEffectBeforeValidation`) |
| RPC-F08 | S3 | `dxCancelOrder` cancel-failure error codes/text set differs (incl. isLocal→1021) | FIXED | B7 `fix/rpc-surface` (C++ codes/texts incl. isLocal→1021 — `TestDxCancelOrderFromConnectorGate`, `TestDxCancelOrderNonLocal`, `TestDxCancelOrderNotFoundPadded`) |
| RPC-F09 | S2 | `dxMakeOrder` response field ORDER differs (updated/created swapped; addresses/block_id last) | FIXED | B7 `fix/rpc-surface` (flat Layout B make-order struct, C++ insertion order — `TestMakeOrderResponseLayoutB`) |
| RPC-F10 | S2 | `dxMakeOrder`/`dxMakePartialOrder` dryrun returns real id + extra fields vs C++ zero id | FIXED | B7 `fix/rpc-surface` (dryrun zero-id 14-field — `TestMakeDryrunResponse`) |
| RPC-F11 | S2 | `dxMakeOrder` non-partial `partial_*` values: literal `"0"` vs `"0.000000"` | FIXED | B7 `fix/rpc-surface` (Layout B `"0.000000"` literals — `TestMakeOrderResponseLayoutB`) |
| RPC-F12 | S3 | `dxMakeOrder` `NO_SERVICE_NODE` message includes the pair; C++ bare | FIXED | B7 `fix/rpc-surface` (bare `NO_SERVICE_NODE` — `TestMakeOrderResponseLayoutB`) |
| RPC-F13 | S2 | `dxMakePartialOrder` dust gate: 1e6-scale minFrom vs native 1e8-scale dust | FIXED | B7 `fix/rpc-surface` (native-scale dust gate — `TestMakePartialDustNativeScale`) |
| RPC-F14 | S2 | `dxTakeOrder` explicit amount `"0"` is a full take in Go, error 1025 in C++ | FIXED | B7 `fix/rpc-surface` (explicit 0 → 1025 — `TestDxTakeOrderFullTake`) |
| RPC-F15 | S3 | `dxTakeOrder` error texts/names leak (`INVALID_ADDRESS` text, `name` "dxMakeOrder") | FIXED | B7 `fix/rpc-surface` (C++ address text + `name` `"dxTakeOrder"` — `TestTakeOrderBadAddressMessage`, `TestTakeOrderNotFoundBare`) |
| RPC-F16 | S2 | `dxGetOrderHistory` sums the WRONG asset volume (taker vs from/maker) | FIXED | B7 `fix/rpc-surface` (from/maker volume side — `TestDxGetOrderHistoryInverse`) |
| RPC-F17 | S2 | `dxGetOrderHistory` encoding: shortest-roundtrip floats vs C++ fixed-8; raw ratio vs 1e-6-quantized price | FIXED | B7 `fix/rpc-surface` (fixed-8 OHLCV + 1e-6-quantized price; row time = bucket START per default `interval_timestamp=at_start` — `TestQuantizePrice`, `TestXFloat`, `TestDxGetOrderHistory*`) |
| RPC-F18 | S2 | `dxGetOrderHistory` validation missing (granularity whitelist, end≤start, limit) | FIXED | B7 `fix/rpc-surface` (granularity whitelist, end≤start, limit — `TestDxGetOrderHistoryValidations`, `TestDxGetOrderHistoryLimitTail`) |
| RPC-F19 | S2 | `dxGetOrderHistory` data source: fills store never written → always empty | FIX-QUEUED + WAIVER | Own fills: record at swap completion (fix queued). Non-local fills need chain view: waiver-required (trading-data family, §0) |
| RPC-F20 | S3 | `dxGetOrderBook` price formula omits C++ +1/COIN bump | FIXED | B7 `fix/rpc-surface` (+1/COIN price bump — `TestDxGetOrderBookPriceBump`) |
| RPC-F21 | S3 | `dxGetOrderBook` equal-best-price tie-break nondeterministic | FIXED | B7 `fix/rpc-surface` (deterministic `orderIDLess` tie-break — `TestDxGetOrderBookTieBreak`) |
| RPC-F22 | S3 | `dxGetOrderBook` int-width (Go `int` vs C++ int64_t) | FIXED | B7 `fix/rpc-surface` (int64 width, `TestDxGetOrderBook*`) |
| RPC-F23 | S2 | `dxGetTokenBalances` `"Wallet"` key derivation/presence differ | FIX-QUEUED | Synthesize `Wallet` first (= BLOCK balance), tickers after (fix queued; see F24 for order) |
| RPC-F24 | S3 | `dxGetTokenBalances` key order | FIX-QUEUED | Folded into the F23 fix: `Wallet` first requires ordered emission (map marshal sorts keys lexicographically, so `Wallet` lands in sorted position, not first), then sorted tickers. Twin proven at that point: fixed order vs race-dependent C++ order means every Go output is a possible C++ output. |
| RPC-F25 | S3 | `dxGetTokenBalances` precision (C++ per-UTXO double sum vs Go exact integer) | FIXED | B7 `fix/rpc-surface` (golden `TestDxGetTokenBalancesSum`; agreement to the 6th decimal) |
| RPC-F26 | S3 | `dxGetMyOrders` field order, param rejection, dedup, sort | FIXED | B7 `fix/rpc-surface` (`orderDetailResult` addresses at 3/6; `seen` dedup; µs sort — `TestDxGetMyOrdersDedupAndSort`, `TestDxGetMyOrdersFieldOrder`; arity already gated params) |
| RPC-F27 | S2 | `dxGetMyPartialOrderChain` chain membership differs (filters/sort) | FIXED + FIX-QUEUED | Filters/sort ported (B7). Residual FIX-QUEUED: full-lineage walk must become the C++ utxo-count child-walk — membership can differ (`TestDxPartialOrderChainDetailsAggregate`, `TestPartialOrderChainResolvesHistoryParent`) |
| RPC-F28 | S2 | `dxPartialOrderChainDetails` `p2sh_deposits` array length ≠ chain length | FIXED | B7 `fix/rpc-surface` (one entry per chain order incl. empty strings — `TestDxPartialOrderChainDetailsAggregate`) |
| RPC-F29 | S3 | `dxPartialOrderChainDetails` bad-id error text differs | FIXED | B7 `fix/rpc-surface` (`"bad order id"` — `TestDxPartialOrderChainDetailsBadId`) |
| RPC-F30 | S3 | `dxPartialOrderChainDetails` key order alphabetized (Go map) | FIXED | B7 `fix/rpc-surface` (ordered `partialChainDetailsResult` struct, C++ 2460-2480 — `TestDxPartialOrderChainDetailsKeyOrder`) |
| RPC-F31 | S3 | `dxGetLockedUtxos` amount encoding (default-float vs fixed-6/native) | FIXED | B7 `fix/rpc-surface` (`nativeAmountString`: C++ default-double prec 6 via `UtxoEntry::toString` — `TestDxGetLockedUtxosAmountDefaultDouble`, `TestDxLockedUtxoNativeAmount`) |
| RPC-F32 | S2 | `dxGetLockedUtxos` 1021 trigger too narrow in Go | FIXED | B7 `fix/rpc-surface` (1021 on no-reserved-utxos `getUtxoItems` miss + expired-state validity — `TestDxGetLockedUtxosNoReserved`) |
| RPC-F33 | S3 | `dxGetLockedUtxos` per-order key selection (status ordinal vs map membership) | FIXED | B7 `fix/rpc-surface` (pending/accepted map membership: `o.Mine || st >= accepting` → maker_and_taker — `TestDxGetLockedUtxosKeyByState`) |
| RPC-F34 | S3 | `dxGetLockedUtxos` id echo un-normalized | FIXED | B7 `fix/rpc-surface` (echo `orderIDString(parseOrderIDS(id))` — `TestDxGetLockedUtxosIdEchoNormalized`) |
| RPC-F35 | S2 | `dxFlushCancelledOrders` flushes only the cancelled ledger, not book/history | FIXED + FIX-QUEUED | Book+history scan ported (B7). Residual FIX-QUEUED: Go sets `Updated`=now at cancel (restarts the age clock); C++ leaves txtime — age-gated flushes diverge past age 0 |
| RPC-F36 | S3 | `dxFlushCancelledOrders` use_count/ordering/key order | FIXED | B7 (per-map uint256 id ordering, ordered struct keys; `TestFlushCancelledPrunesBookAndHistory`). use_count tracked separately as RPC-F62 (DOCUMENTED, ruling pending) |
| RPC-F37 | S2 | `gettradingdata` (lowercase) missing from Go dispatch | WAIVER | Trading-data family (§0): lowercase is blocknetd-internal with a different schema + duplicated `to` key (rpcxbridge.cpp:3520, 2761-2770); envelope `-32601` stands (`api.md:8`) |
| RPC-F38 | S2 | `dxGetTradingData` `fee_txid`/`nodepubkey` always `""` | WAIVER | Trading-data family (§0): fields need the on-chain BLOCK scan a thin client cannot replay (`api.md` Tier 3) |
| RPC-F39 | S2 | `dxGetTradingData` data source: local fills vs on-chain scan | WAIVER | Trading-data family (§0, `api.md`); own-fills recording half tracked under RPC-F19 |
| RPC-F40 | S2 | `dxSplitInputs` requires amount/scriptPubKey; C++ needs only txid/vout | FIXED | B7 `fix/rpc-surface` (txid/vout-only entries resolved against the wallet's unspent by txid/vout; locked-utxo guard — `TestDxSplitInputsTxidVoutOnly`, `TestDxSplitInputsLockedUtxo`) |
| RPC-F41 | S2 | `dxSplit` fee formula differs (520·fpb per output + claw-back) | FIXED | B7 `fix/rpc-surface` (`feesPerUtxo = minTxFee1(1,3)+minTxFee2(1,1)` in XBridge units; real tx fee deducted from change, dust claw-back incl. C++ `outputCount -= 1` quirk — `TestDxSplitFeesPerUtxo`; parity oracle `main.cpp` + `TestValueMatchesCppSplit`) |
| RPC-F42 | S2 | `dxSplit` change destination differs (requested address vs fresh address) | FIXED | B7 `fix/rpc-surface` (change output uses the requested address script — `TestDxSplitChangeToRequestedAddress`) |
| RPC-F43 | S3 | `dxSplit` submit-failure code and error names | FIXED | B7 `fix/rpc-surface` (1004 BAD_REQUEST named after the actual method — `TestDxSplitSubmitFailure`) |
| RPC-F44 | S2 | `dxGetUtxos` amounts trimmed vs C++ fixed-8 | FIXED | B7 `fix/rpc-surface` (`coins.FormatAmountFixed` — fixed `c.Decimals` places, no trimming, matching C++ `xBridgeStringValueFromPrice(amount, conn->COIN)` (xutil.cpp:216-221) — `TestFormatAmountFixed`, `TestDxGetUtxos`) |
| RPC-F45 | S3 | `dxGetUtxos` listunspent failure code/text (1004 vs 1002) | FIXED | B7 `fix/rpc-surface` (1004 BAD_REQUEST named `dxGetUtxos` with C++ text `"failed to get unspent transaction outputs"` — `TestDxGetUtxosListUnspentError`) |
| RPC-F46 | S2 | `getnetworkinfo` shim diverges from real blocknetd (protocolversion, fees, subversion, fields) | FIXED | B7 `fix/rpc-surface` (protocolversion 70713, xbridgeprotocolversion 55, xrouterprotocolversion 50, subversion `/Blocknet:4.4.1/`, relayfee/incrementalfee 8-decimal strings `"0.00010000"`/`"0.00001000"`, networks `proxy_randomize_credentials:false` — `TestGetNetworkInfo`) |
| RPC-F47 | S2 | JSON-RPC HTTP status for parse/method-not-found (C++ 500/404 vs Go 200) | FIXED | B4 `fix/http-hardening` (status routing −32600→400, −32601→404, else 500; `TestServerEnvelopeStatusCodes`, `TestServerParseErrorStatus500`) |
| RPC-F48 | S3 | method-not-found message appends the method name | FIXED | B4 `fix/http-hardening` (bare `"Method not found"`; `TestServerInvalidRequest`, `TestServerEnvelopeStatusCodes`) |
| RPC-F49 | S3 | request body limit 4 MiB vs C++ 32 MiB | FIXED | B4 `fix/http-hardening` (32 MiB `rpcMaxBodyBytes`, non-envelope 413; `TestServerMaxBodyBytes`) |
| RPC-F50 | S2 | auth model: Go open-by-default vs C++ always-auth | FIXED | B4 `fix/http-hardening` (always-auth when creds configured, 401 empty body, 250 ms delay; `TestServerRPCAuth`, `TestServerRpcAuthMultiUser`, `TestServerRpcAuthDelay`) — residual IDENTICAL: no auto-cookie file and loopback-open-when-unconfigured are process-local startup posture with zero swap-sequence effect (no dx response differs) |
| RPC-F51 | S3 | batch / named params / -32600 unsupported | FIXED | B4 `fix/http-hardening` (batch supported, named → −8; `TestServerBatch`, `TestServerNamedParamsRejected`) |
| RPC-F52 | S3 | extra positional params accepted where C++ errors (business 1025) | FIXED | B4 `fix/http-hardening` (arity registry + C++ help-text 1025; `TestArityBusinessMethods`, `TestArityThrowMethods`) |
| RPC-F53 | S3 | `dxGetLocalTokens` returns unconnected/duplicate tickers | FIXED | B7 `fix/rpc-surface` (returns the loaded connector map keys, deduplicated — C++ `availableCurrencies()` (xbridgeapp.cpp:808-821) — `TestDxGetLocalTokensConnectedOnly`) |
| RPC-F54 | S3 | `dxGetNetworkTokens` membership: Go unions config; C++ pure SN service union | FIXED | B7 `fix/rpc-surface` (pure SN service union via `Registry.WalletServices()`; config `NetworkTokens`/`ExchangeWallets` no longer contribute — `TestDxTokenListsFromConf`, `TestDxGetNetworkTokensLive`) |
| RPC-F55 | S3 | `dxGetNewTokenAddress` error path returns `[]` in C++, business 1002 in Go | FIXED | B7 `fix/rpc-surface` (GetNewAddress failure -> empty array, C++ `getNewTokenAddress()` empty-string (rpcxbridge.cpp:186-190) — `TestDxGetNewTokenAddressGetNewAddrError`) |
| RPC-F56 | S3 | `dxLoadXBridgeConf` reload failure shape and side effects differ | FIXED | B7 `fix/rpc-surface` (reload failure returns `false` result, not a business error — C++ `uret(success)` (rpcxbridge.cpp:229-234) — `TestDxLoadConfFailureFalse`, `TestDxLoadConfHotReloadMissingPath`); coin/connector rebuild side effects in `Node.reloadConf`; non-local-order clearing (`clearNonLocalOrders`, :232-233) tracked under B10 `fix/config-parity` |
| RPC-F57 | S2 | `dxGetOrderBook` detail-4 nesting `[[…]]` vs flat | FIXED | B7 `fix/rpc-surface` (detail-4 `[[…]]` nesting — `TestDxGetOrderBookDetail`) |
| RPC-F58 | S2 | HTTP auth/timeout hardening missing | FIXED | B4 `fix/http-hardening` (`-rpcservertimeout` + `http.Server` read/write/header/idle timeouts) |
| RPC-F59 | S3 | `dxGetMyPartialOrderChain` unknown/malformed id handling | FIXED | B7 (bad-order-id) |
| RPC-F60 | S3 | Bare `help` lists only `dx*` vs Core full command list | DOCUMENTED | Help-content divergence; ruling pending (mirror list vs waiver) |
| RPC-F61 | S3 | `getnetworkinfo` `localservices` bits reflect thin client, not full node | DOCUMENTED | Informational RPC; bits report the reporting node by construction — cannot echo Core's; waiver ruling pending |
| RPC-F62 | S3 | `dxFlushCancelledOrders` `use_count` always 1 vs shared_ptr refcount | DOCUMENTED | Debug-only field; refcounting has no Go equivalent; waiver ruling pending |

### WIRE axis (`WIRE-F57`–`WIRE-F72`) — evidence: `evidence/wire.md`, `evidence/wire_p1.md`, `evidence/wire_p2.md`

| ID | Sev | Finding (one line) | Status | Owner |
|---|---|---|---|---|
| WIRE-F57 | S2 | P2P max payload 67 MiB vs C++ 4,000,000 | FIXED | B5 `fix/wire-hardening` (`MaxPayloadSize`, conn cap test) |
| WIRE-F58 | S2 | Received magic never validated | FIXED | B5 `fix/wire-hardening` (`conn.readMessage`, `TestConnWrongMagicRejected`) |
| WIRE-F59 | S2 | No `MIN_PEER_PROTO_VERSION` gate | FIXED | B5 `fix/wire-hardening` (`MinPeerProtoVersion`, `TestConnVersionBelowMinimum`) |
| WIRE-F60 | S2 | `"staging"` network magic is actually C++ REGTEST | FIXED | B5 `fix/wire-hardening` (`RegtestMagic`, `-network regtest`) |
| WIRE-F61 | S2 | `snl` (SNLIST) responses ignored | FIXED | B5 `fix/wire-hardening` (raw accepted-ping echo, `TestPeerManagerSNListEcho`) |
| WIRE-F62 | S3 | `proto.Unmarshal` silently ignores trailing body bytes | FIXED | B5 `fix/wire-hardening` (`TestUnmarshalTrailingBytes`) |
| WIRE-F63 | S3 | Non-canonical CompactSize varint accepted | FIXED | B5 `fix/wire-hardening` (`readVarInt` canonicality, `TestReadVarIntCanonical`) |
| WIRE-F64 | S3 | Bad checksum disconnects instead of log-and-drop | FIXED | B5 `fix/wire-hardening` (`ErrChecksum` sentinel, `TestConnChecksumFrameDropped`) |
| WIRE-F65 | S3 | Go-only 1 MiB XBridge body cap (C++ has none) | DOCUMENTED | Hardening with no legit-swap impact (all swap bodies are bytes-small); waiver ruling pending — not assumed accepted (`protocol.md`) |
| WIRE-F66 | S3 | Command 4 has two C++ writers differing by trailing minFromAmount | DOCUMENTED | Go writer is byte-identical to the authoritative writer and the reader tolerates both forms (proof: `proto/body_test.go:130-157`, `conformance/conformance_suite_test.go:1064-1150`). Ruling pending: strict §0 reading says rejecting the 126 B form C++ itself rejects, but that form is emitted by C++ broadcast writers on the live wire — matching the reader would drop real broadcasts. Interop requires tolerance. |
| WIRE-F67 | S2 | Command 2 (xbcXChatMessage) body is speculative (no C++ writer) | FIXED | B5 `fix/wire-hardening` (type deleted; `DecodeBody` rejects) |
| WIRE-F68 | S2 | Command 50 (xbcServicesPing) body claim is unbacked | FIXED | B5 `fix/wire-hardening` (type deleted; `DecodeBody` rejects) |
| WIRE-F69 | S3 | getaddr policy and addr cap differ | FIXED | B5 `fix/wire-hardening` (outbound-ignore; 1000-cap, `TestPeerManagerAddrCapDropped`) |
| WIRE-F70 | S3 | Version handshake deadline differs | FIXED | B5 `fix/wire-hardening` (60 s deadline, matching C++ first-message deadline). Outbound SENDHEADERS/SENDCMPCT/pings tracked separately as WIRE-F72. |
| WIRE-F72 | S3 | Outbound SENDHEADERS/SENDCMPCT/pings never sent | DOCUMENTED | The XBridge engine never acts on these messages and inbound pings are answered (`p2p/discovery/peer_manager.go:425-426`); multi-hour live mainnet peerings stay connected. Ruling pending whether that operational evidence suffices or a waiver is recorded. |
| WIRE-F71 | S2 | Servicenode registration integrity: fields read-then-discarded; gates miss `isValid` subset | FIXED | B1 `fix/servicenode-registry` |

### STATE axis (`STATE-F71`–`STATE-F86`) — evidence: `evidence/state.md`

| ID | Sev | Finding (one line) | Status | Owner |
|---|---|---|---|---|
| STATE-F71 | S2 | OnHold/OnInit skip the C++ amount/identity/price verification | FIXED | B3 `fix/deposit-path`: `verifyHold` (C++ `processTransactionHold` :1404-1471) + `verifyInit` with the intended-OR order-detail check + state gate; `TestHoldInitVerification` |
| STATE-F72 | S2 | Expiry pruning never wired in Go (no order-book expiry sweep) | FIXED | B8 `fix/state-machinery`: `Store.PruneExpired` (open book `"created"`/`"open"`; strict `>` TTLs, block-height + time, `PrepTx` pending-partial guard, erase-without-history; the `swap` predicates are ported into the store sweep) on the 15 s `expirySweepInterval` ticker; `Order.BlockNumber` stamp at ingest/make; persist aligned 240 s → 60 s (C++ `checkAndEraseExpiredTransactions`/`saveOrders`, xbridgeapp.cpp:3573-3654/:3744); maker in-swap protection via the session `inSwap` guard; `TestPruneExpired*` |
| STATE-F73 | S3 | TxCancelReason enum + text table not ported (incl. C++ bugs) | FIXED | B8 `fix/state-machinery`: full enum (xbridgepacket.h:21-48) + `TxCancelReasonText` incl. the two C++ bugs (`crBadSettings`→`"crUnknown"`, `crUnknown`/default→`"crNone"`, xbridgeapp.cpp:4052-4107); `selfCancelErr`/`sendSelfCancel` retyped; `cancel_reason` log field wired; `TestTxCancelReason*` |
| STATE-F74 | S3 | `trRollbackFailed` never set (refund broadcast failure) | FIXED | B8 `fix/state-machinery`: `postRefundTask` apply writes `"rollback failed"` on broadcast failure for orders at session `csCreatedA+` (`rollbackGate`, C++ `redeemOrderDeposit` xbridgesession.cpp:3852-3908) and restores `"rolled back"` on a later success (:3911); never clobbers terminal/canceled; `TestRollbackFailed*`/`TestRefundFailureStateGate` |
| STATE-F75 | S3 | No peer penalty/Misbehaving analogue | FIXED | B8 `fix/state-machinery`: per-peer misbehaviour score (+10 undersized xbridge envelope, +20 rejected addr, ban at 100 = C++ `-banscore`) with disconnect + re-candidating exclusion, per-connection reset and ban pruning; direct-hub score gated on the new `p2p.ErrMalformedXBridge` sentinel; `TestPeerManagerMisbehave*`/`TestReaderLoopHub*` |
| STATE-F76 | S4 | Live handshake uses separate `clientState`, not ported `swap.State` | IDENTICAL | Proof: order statuses are twins end-to-end — full-handshake tests (`swap_test.go`) plus `TestHoldInitVerification` and response-status tests assert every C++ descr string at the phase that emits it (`findings.md`) |
| STATE-F77 | S2 | Post-completion handshake retransmit re-broadcasts deposit/claim | FIXED | state guards (`swap_guard_test.go`) |
| STATE-F78 | S2 | Handshake inbound packets re-verified against pinned hub key, no TOFU | FIXED | hub-key pinning (`swap.go`) |
| STATE-F79 | S3 | `tryJoinMatches` partial-order min-size guards unconfirmed | FIXED | B8 `fix/state-machinery`: confirmed 1:1 with C++ `Transaction::tryJoin` (xbridgetransaction.cpp:527 `other->m_destAmount < m_minPartialAmount`, strict `<`); `TestTryJoinPartialMinSizeGuard` |
| STATE-F80 | S2 | Go never marks `finished` at claim; waits for hub `Finished` (C++ finishes maker at ConfirmA, taker at ConfirmB) | FIX-QUEUED | Adopt C++ claim-completion locally; exclude claim-confirmed sessions from `scanRefunds` (taker liveness audit B1) |
| STATE-F81 | S2 | Go taker cannot complete without hub `ConfirmB`; no chain-watch claim | FIX-QUEUED | Port taker watch (spend-scan own deposit, learn payTx, derive secret, build claim) independent of hub packets (taker liveness audit B2) |
| STATE-F82 | S2 | No `processLater` retry queue; not-ready/deferred work depends on hub retransmit | FIX-QUEUED | Bounded local retry queue for not-ready/build/broadcast failures (taker liveness audit B3) |
| STATE-F83 | S3 | Claim broadcast lacks `ALREADY_IN_CHAIN`-as-success tolerance | FIX-QUEUED | Treat wallet already-in-chain as success and proceed to `Confirmed` send (xbridgesession.cpp:4012-4024) |
| STATE-F84 | S2 | Init check is OR (reject any mismatch) vs C++ `&&` bug (reject only when all mismatch) | FIX-QUEUED | Bug-for-bug `&&` required by the identity standard (xbridgesession.cpp:1750-1756). Risk noted: `&&` re-admits single-field-mismatch packets C++ accepts — required for interop (a Core hub emits what C++ accepts; rejecting them stalls our swaps), and amounts are re-verified at deposit/claim regardless. Implementation needs explicit security sign-off beyond this row. |
| STATE-F85 | S2 | Cancel writes `canceled` for deposit-sent orders vs C++ `trRollback` path | FIX-QUEUED | Route deposit-sent cancels through the rollback path (`xbridgesession.cpp:3384-3420`) |
| STATE-F86 | S3 | Refund sweep 60 s vs C++ 15 s timer | FIX-QUEUED | Tighten `refundCheckInterval` toward C++ `TIMER_INTERVAL` (xbridgeapp.cpp:90); Item 7 |

### CRYPTO axis (`CRYPTO-F77`–`CRYPTO-F102`) — evidence: `evidence/crypto.md`

| ID | Sev | Finding (one line) | Status | Owner |
|---|---|---|---|---|
| CRYPTO-F77 | S1 | BCH forkid `0x41` sighash missing in Go local signing | FIXED | B9 `fix/crypto-connectors`: parameterized BIP143 digest + forkid signing (fork value 0xffdead for live BCH mainnet replay protection, bch.cpp:203-209/497-499; DEVAULT 0, BTG 79) wired into `buildRefundTx`/`redeemCounterparty`/`DepositSpec.SignInput` via `SignTxInputForCoin`; DER byte 0x41; `TestBCHRefundForkidSigned`, `TestHashForSigningForkID`, `TestSignTxInputForCoinDispatch`, `TestForkidSignatureHashMatchesCpp` (parity oracle transcription of bch.cpp:191-256/btg.cpp:118-207) |
| CRYPTO-F78 | S2 | Deposit tx fee formula `minTxFee1(nIn,3)` vs Go `estimateFee(nIn,2)` | FIXED | B3 `fix/deposit-path`: `estimateFee(cc, nIn, 3)` at `api/swap.go` (`(192nIn+102)·FeePerByte`, C++ `xbridgesession.cpp:1994/:2526`); `TestEstimateFeeMatchesCppVsize` |
| CRYPTO-F79 | S3 | Fee fallback: C++ 0 vs Go 2 sat/vB when FeePerByte unset | FIX-QUEUED | Deposit fee math is on-chain visible; fallback must be C++ 0 exactly |
| CRYPTO-F80 | S3 | Go honors `DustAmount` conf key C++ never reads | FIXED | B10: the dust source moved to `MinimumAmount` (C++ maps it onto the exchange wallets' `dustAmount`, xbridgeexchange.cpp:145); `DustAmount` is now parsed-but-unread, matching C++ (createConf-template key); `TestEffectiveDust` |
| CRYPTO-F81 | S3 | Address decoding strictness differs (base58check version byte, cashaddr) | FIX-QUEUED | Go rejects addresses C++ accepts (swap-visible at make/take); match C++ `toXAddr` leniency exactly |
| CRYPTO-F82 | S4 | RNG top-bit bias in Go private-key generation | FIXED | B9: `crypto.NewPrivateKey` full-range 256-bit with retry into [1, N-1] (C++ `makeNewKey`, xbridgecryptoproviderbtc.cpp:204-210); `TestNewPrivateKeyFullRange` |
| CRYPTO-F83 | S3 | Block-hash byte order assumption unverified end-to-end | FIXED | B9: `wallet.revHashHex` pinned against a captured real block hash (Bitcoin genesis) = C++ `base_blob<256>::SetHex` internal bytes (uint256.cpp:27-53); `TestRevHashHexCapturedBlockHash` + parity `TestBlockHashByteOrderMatchesCpp` (oracle transcription) |
| CRYPTO-F84 | S2 | `TakeOrder` emits `AcceptingBody` with empty fee/utxos (156 B < 188 B) | FIXED | B2 `fix/wire-acceptingbody` |
| CRYPTO-F85 | S2 | No `checkDepositTransaction` in the Connector contract | FIXED | B3 `fix/deposit-path`: `wallet.CheckDepositTransaction` (interface + RPCConnector 1:1 port of `xbridgewalletconnectorbtc.cpp:1981-2194` + LocalConnector `ErrNoChainSource`) wired into `OnCreateB`/`OnConfirmA` with tri-state (wait→no reply / bad→Cancel / good→record); `wallet/rpc_test.go` goldens + `TestCreateBBadDepositCancels`/`TestCreateBWaitsOnNotReadyDeposit` |
| CRYPTO-F86 | S2 | `buildDeposit` broadcasts before building the refund | FIXED | B3 `fix/deposit-path`: sign → local txid → refund → broadcast; `TestDepositNotBroadcastWhenRefundFails` |
| CRYPTO-F87 | S2 | Deposit re-runs `ListUnspent` instead of `xtx->usedCoins` | FIXED | B3 `fix/deposit-path`: `Order.UsedCoins` recorded at make/take, `swapCtx.funding` snapshot consumed by `buildDeposit`; `TestDepositSpendsUsedCoins` |
| CRYPTO-F88 | S3 | Segwit/BIP143 signing dead code | FIXED | B9: the BIP143 digest is now LIVE — it is the base of the forkid signing path (`HashForSigningBIP143`). The "bech32 re-encoded legacy" sub-claim is unsubstantiated: the only bech32 codec usage is the documented per-coin segwit address path (`coins/address.go`), which tries base58check first and only accepts bech32 when the HRP matches the coin |
| CRYPTO-F89 | S3 | Coin-family misclassification / missing connectors (DEVAULT, DCR, PART, BTG) | FIXED | B9 (partial, residual new rows below): BTG classified forkid-79 + bech32 "btg"; DEVAULT classified BCH-family (cashaddr "devault", fork value 0); `TestBTGAddressRoundTrip`, `TestDevaultAddressCashaddr`. DCR is not in the live manifest (no `[DCR]` in blockchain-configuration-files); PART (`CRYPTO-F98`) and BCD (`CRYPTO-F99`) are non-portable tx formats, documented/deferred |
| CRYPTO-F90 | S3 | Refund/payment payout model (fee2 margin, oOverpayment) | FIXED | B3 `fix/deposit-path`: claim spends the validated deposit (exact `P2SHNative` at `DepositVout`), refund pays full nominal (fee2 implicit); `OBinTxVout/OBinTxP2SHAmount/OOverpayment` persisted; `TestRedeemCounterpartyPayout` |
| CRYPTO-F91 | S3 | `signrawtransaction` payload: Go sent `"ALL"` in the privkeys slot, C++ sends null | FIXED | B9: payload now `[rawtx, prevtxs|null, keys|null]` (xbridgewalletconnectorbtc.cpp:1055-1089), same for the `signrawtransactionwithwallet` fallback; `TestSignRawTransactionPayloadMatchesCpp` |
| CRYPTO-F92 | S3 | `secretFromPayTx` read only input 0; C++ scans all vins | FIXED | B9: `secretFromPayTx` scans every input's scriptSig for a KeyID-matching push (C++ `getSecretFromPaymentTransaction`, btc.cpp:2241-2276); `TestSecretFromPayTxScansAllInputs` |
| CRYPTO-F93 | S3 | UTXO ownership-proof challenge stream format | FIXED | `TestWholeCoinOstreamMatchesCppStream` |
| CRYPTO-F94 | S3 | Deposit inputs `SEQUENCE_FINAL`; refund spend `SEQUENCE_FINAL-1` | FIXED | `checkDepositTransaction` parity |
| CRYPTO-F95 | S3 | Deposit locks `Amount + fee2` (`minTxFee2(1,1)`); change after fee+fee2 | FIXED | `TestDepositLocksAmountPlusFee2` |
| CRYPTO-F96 | S3 | `nTime` committed in sighash on `TxWithTimeField` coins | FIXED | `TestHashForSigningWithTimeField` |
| CRYPTO-F97 | S1 | Deposit path mixed XBridge 1e6 and native base units (locked 100× too little for COIN≠1e6; BLOCK masked it) | FIXED | B3 `fix/deposit-path` (promoted from planning): `fromXBridgeAmt` at the `api/swap.go` boundary; `TestDepositNativeScale` |
| CRYPTO-F98 | S3 | PART (Particl) connector: `XParticlTransaction` serialization + confidential outputs + amount-committing digest not portable to `coins.Tx` | DOCUMENTED | Non-portable format; waiver ruling pending. Until ruled, `[PART]` refused loudly at admission (never malformed broadcast). Non-portable tier (`B9-crypto.md`) |
| CRYPTO-F99 | S3 | BCD (Bitcoin Diamond) connector: fork-version serialization (`CURRENT_VERSION_FORK` `preBlockHash` field) not portable to `coins.Tx` | DOCUMENTED | Non-portable format; waiver ruling pending. Until ruled, `[BCD]` refused loudly at admission. Non-portable tier (`B9-crypto.md`) |
| CRYPTO-F100 | S3 | Deposit change emitted when `>0` without the C++ dust check (`!isDustAmount`) | FIX-QUEUED | Gate change on dust like `xbridgesession.cpp:2621-2623`; dust def `:1902-1906` |
| CRYPTO-F101 | S3 | Secret scan hash-only, not bound to own deposit outpoint | FIX-QUEUED | Bind vin to `(theirDepositTxID, theirDepositVout)` like `getSecretFromPaymentTransaction` (`xbridgewalletconnectorbtc.cpp:2252-2253`) |
| CRYPTO-F102 | S3 | Dust source: live relay-derived (C++) vs 5460 fallback (Go) | DOCUMENTED | Thin client has no relay feed; waiver ruling pending. Keep swap amounts well above dust meanwhile |

### CONFIG axis (`CFG-F84`–`CFG-F92`) — evidence: `evidence/config.md`

| ID | Sev | Finding (one line) | Status | Owner |
|---|---|---|---|---|
| CFG-F84 | S2 | `[Rpc]` section in xbridge.conf aborts xbridged at startup | FIXED | B10 `fix/config-parity`: `[Rpc]`/`[Main]` whitelisted in `config.Load` (exact-case; dead section per util/settings.h:49-65); `TestLoadSkipsRpcSection` |
| CFG-F85 | S2 | Wallet admission validation gates absent (locktime/confirmation drift) | FIXED | B10: `config.Admit` ports xbridgeapp.cpp:1002-1090 (connect check, maker/taker locktime targets incl. slow chains, confirmation drift `max(900/blockTime,4)`, `CreateTxMethod` dispatch); applied via `config.Admitted` at startup/reload/sweep; `TestAdmitGates`, `TestAdmitCreateTxMethod`, `TestAdmittedFilters` |
| CFG-F86 | S2 | Missing conf: C++ creates template and runs; Go exits(1) | IDENTICAL | Proof: `config.Load` errors on missing file and never creates (`config/conf.go:102-108`); the daemon fatals on load error (`cmd/xbridged/main.go:181-184`). Startup posture with zero swap-sequence effect. Never-creates is a hard rule (`B10-config.md`) |
| CFG-F87 | S2 | Hot-reload semantics differ (ExchangeWallets keying, gates, order clearing) | FIXED | B10: `wallet.Activator` connects exactly `[Main].ExchangeWallets` ∩ gates ∩ reachability probe (C++ `updateActiveWallets`, xbridgeapp.cpp:917-1214, with the 300 s bad-wallet retry); reload preserves `ForceShowAllOrders`/`CheckReachability` and clears non-local orders unless ShowAllOrders (`clearNonLocalOrders`, rpcxbridge.cpp:229-233); 30 s sweep (:3674-3677); `TestActivateExchangeWalletsOnly`, `TestActivateProbe`, `TestActivateBadWalletRetry`, `TestReloadAppliesEWKeying`, `TestReloadPrunesUnconnectedOrders`, `TestSweepConnectors`, `TestPruneUnconnected` |
| CFG-F88 | S3 | ExchangeWallets parsing differs (`,` `;` `:` + validation) | FIXED | B10: split on `,;:` + `ccy::Symbol::validate` (uppercase, len 1..8, no trim, util/settings.cpp:143-166 / currency.h:29-47); `TestExchangeWalletsCppSemantics` |
| CFG-F89 | S3 | Case-insensitive keys in Go vs case-sensitive C++ | FIXED | B10: exact-case key and `[Main]` lookups (boost property_tree is case-sensitive); `TestCaseSensitiveKeys` |
| CFG-F90 | S3 | Missing CLI flags / flag differences (-enableexchange, -dxnowallets, version case) | FIXED | B10: `-dxnowallets` (ShowAllOrders override, kept across reload as `ForceShowAllOrders`) + `-enableexchange` (inert compat no-op); daemon now honors `Main.ShowAllOrders` at startup (pre-fix only after a reload); `-walletversionstr` case already fixed in `a0fee1e` (B7) |
| CFG-F91 | S3 | Go-only conf keys and ignored C++ keys (MinimumAmount, CashAddrPrefix, CreateTxMethod) | FIXED | B10: `Title` default `""` (Settings::get `_T()`, settings.h:75-84 — the findings card had the direction inverted); `MinimumAmount` is the dust source (xbridgeexchange.cpp:145), `DustAmount` template-only; `CashAddrPrefix` conf value wins with C++ fallbacks (bch.cpp:306-308 / devault.cpp:278-280); ETH/unknown `CreateTxMethod` rejected (xbridgeapp.cpp:1043-1090); `TestTitleDefaultsEmpty`, `TestEffectiveDust`, `TestFromConfCashAddrPrefix`, `TestAdmitCreateTxMethod` |
| CFG-F92 | S3 | Wallet RPC timeout 30 s vs C++ 120 s `-rpcxbridgetimeout` | FIX-QUEUED | Honor the C++ timeout on slow-wallet paths (timeout error vs success is swap-visible) |

### CONCURRENCY axis (`CONC-F92`–`CONC-F102`) — evidence: `evidence/concurrency.md`

| ID | Sev | Finding (one line) | Status | Owner |
|---|---|---|---|---|
| CONC-F92 | S2 | Engine goroutine can block on socket write / fsync, stalling packet processing + RPC | FIXED | B11 `fix/concurrency`: bounded buffered outbound writer in `p2p.Conn` (growing queue drained by a writer goroutine — the engine never blocks on a peer; 1 MB cap disconnects the peer like C++ `PushMessage`/`nSendBufferMaxSize`, net.cpp:2705-2730 — no silent frame drops) + background persist (`snapshotSwaps`/`writeSwaps`/`persistLoop`, coalesced latest-slot + final flush on Close, C++ `saveOrders` off-msghand); `TestConnWriteSlowPeerNonBlocking`, `TestConnWriteDeliversInOrderConcurrent`, `TestEngineWriteDoesNotBlockOnSlowPeer`, `TestPersistDoesNotBlockEngine`, `TestPersistCoalescesBurst`, `TestPersistFlushedOnClose` |
| CONC-F93 | S3 | Discovery peer goroutines never joined; Dedupe sweeper leaks without Flush | FIXED | B11 `fix/concurrency`: PeerManager `wg`-joins maintain/connectOne/readLoop on Close (C++ `join_all`, xbridgeapp.cpp:530-544) + ctx-cancellable dials (`DialContext`; the version handshake aborts on ctx cancellation via `NewConnCtx`, so a handshake-stalling peer cannot hold Close — follow-up `fix/concurrency-followup`); `Dedupe.startSweepLocked` re-registers so a restarted sweeper is still stopped by `FlushAll`; `Node.Close` calls `xlog.FlushAll()`; `TestPeerManagerCloseAbortsInflightDialAndJoins`, `TestPeerManagerCloseFastWithStalledHandshake`, `TestDedupe_RestartReRegisters`, `TestNodeCloseFlushesDedupeSweepers` |
| CONC-F94 | S3 | Conf reload mid-swap-task hazard (untested) | FIXED | B11 `fix/concurrency`: `swapCtx` snapshots `Connectors`/`Confs` at enqueue (C++ session holds the captured connector pointer; reload replaces the pool under `m_connectorsLock`); every worker-path read uses the snapshot (`checkCounterpartyDeposit`, claim broadcasts, `buildDeposit`, `computeLockTimeFor`, `conf()`); `postRefundTask` captures the connector at enqueue; `TestReloadMidSwapTaskKeepsConnectorSnapshot` (fails if reverted to live-config reads). Follow-up `fix/concurrency-followup`: `swapCtx` also snapshots the two currencies under one atomic load (`coins.Snapshot`) and workers resolve coins via `c.coin(cur)`; `LocalConnector` binds `TxWithTimeField` at construction. Tests: `TestReloadMidSwapTaskKeepsCoinSnapshot`, `coins.TestSnapshot`, `TestLocalConnectorSignAfterRegistryFlip` |
| CONC-F95 | S3 | Go hides C++'s transient "accepting" window | FIX-QUEUED | `dxGetOrder` during a take must show `accepting` like C++ (exposed during wallet RPCs); Go commits atomically. Fix must preserve commit atomicity |
| CONC-F96 | S4 | Go runs swap wallet I/O + RPC concurrently where C++ serializes | IDENTICAL | Proof: concurrency model is internal — every response and packet is serialized through the single-owner engine; `-race` clean full suite plus `TestPruneSessions*`/`TestConcurrent*` pin it (`findings.md`) |
| CONC-F97 | S2 | `SwapSession` fields single-owner, engine-only | FIXED | race tests |
| CONC-F98 | S2 | Coin registry `atomic.Pointer` hot-reload safe | FIXED | race tests |
| CONC-F99 | S2 | Unbounded growth bounded (pruneSessions, trimOldest, fills/history caps) | FIXED | `TestPruneSessions*` |
| CONC-F100 | S2 | Book/session maps never held across wallet I/O | FIXED | lock discipline |
| CONC-F101 | S2 | `dxMakeOrder` returned the store's LIVE `*Order` (data race) | FIXED | `TestMakeOrderReturnsStoreCopy` |
| CONC-F102 | S2 | Force-refund double-broadcast window | FIXED | `TestForceRefundTakesSweepGuard` |

### INVENTORY / DOC axis (`INV-F97`–`INV-F100`) — evidence: `evidence/inventory.md`

| ID | Sev | Finding (one line) | Status | Owner |
|---|---|---|---|---|
| INV-F97 | S4 | Unported C++ internal helpers (not dApp-facing) | IDENTICAL | Proof: absence is checkable — gap list in `evidence/inventory.md`; no helper has wire/RPC surface, so no observable exists to diverge |
| INV-F98 | S4 | `docs/protocol.md` says order `Created` is unix seconds; wire carries µs | FIXED | B11 `fix/concurrency` (doc): protocol.md §4.2 notes now state µs (`total_microseconds()`, xutil.cpp:280; Go passes the u64 through); stale `findings.md:366` ref corrected |
| INV-F99 | S4 | Stale C++ header-comment enums (commands 11/12/13/18/20/24) | IDENTICAL | Proof: Go follows the writers, documented at `proto/body_types.go:16-33` + `protocol.md` §4.1; their comments are wrong, so there is nothing to align on the Go side |
| INV-F100 | S4 | Vestigial `Server.verify`, `coins.MustGet`, unreferenced `swap`, LocalConnector sign/verify | FIXED | documented |

### SECURITY axis (`SEC-F01`–`SEC-F04`) — security/robustness findings outside the RPC/wire/config axes

| ID | Sev | Finding (one line) | Status | Owner |
|---|---|---|---|---|
| SEC-F01 | S2 | RPC binds to loopback by default (auth only when both creds set) | FIXED | loopback bind |
| SEC-F02 | S3 | Inbound order UTXO ownership proofs never verified before booking | FIXED | B8 `fix/state-machinery`: `wallet.Connector.GetTxOut` (gettxout) + `verifyAndBook`/`verifyOrderUtxos` verify each maker UTXO before booking (C++ snode `processTransaction`, xbridgesession.cpp:535-577: getTxOut existence + BIP137 verifyMessage vs the chain amount, skip bad entries, reject when no survivor covers fromAmount); wallet I/O offloaded so the engine never blocks; cmd-4 broadcasts carry no utxos so the gate fires for any utxo-bearing order body; `TestVerifyOrderUtxos*`/`TestVerifyAndBook*` |
| SEC-F03 | S2 | HTLC ELSE branch + CreateB-derived taker deposit composition sound; no standalone code | FIXED | B3 `fix/deposit-path` (composite): validated-deposit refusal kills the theft end-to-end — `TestSecF03CompositeRefusal` (taker refuses a bad A-deposit → Cancel + no B deposit; maker refuses a bad B-deposit at ConfirmA → Cancel + refund rollback). Attacker model corrected: the hostile outcome is **theft**, not recoverable lockup. |
| SEC-F04 | S2 | Plaintext secrets + debug-log leakage | FIXED | B6 `fix/secrets-hygiene`: refund/claim hex + RPC bodies dropped from logs; corrupt swap file logs at Error like C++ `loadOrders`. Recovery hardening (2026-09-10): the `-persistsecrets` opt-out was removed — secrets are now always persisted (strict C++ `orders.dat` parity, no opt-out); refund/claim hex stays out of logs pending the recovery logging item (Core TXLOG-hatch parity), privkeys/preimages stay out permanently. Tests: `TestPersistSecretsAlwaysOnDisk`, `TestCorruptSwapFileContinuesLikeCpp`. |

---

## ID-history appendix

Every prior-audit ID (`F1–F27`, `S2/S3/S4` series, `S1-A…D`) resolves to
exactly one canonical ID. This appendix exists for archaeology; the live
namespace is the Current register above.

| Legacy | Canonical | Notes |
|---|---|---|
| F1 | SEC-F01 | RPC loopback bind — FIXED |
| F3 | CONC-F97 | `SwapSession` single-owner — FIXED |
| F4 | CONC-F98 | coin registry atomic reload — FIXED |
| F5/F9 | CONC-F99 | bounded growth — FIXED |
| F6 | CONC-F100 | no wallet I/O under lock — FIXED |
| F7 | SEC-F02 | inbound UTXO proofs unverified — FIXED (B8) |
| F8 | CRYPTO-F88 | segwit dead code — FIXED (B9) |
| F10 | RPC-F49 | 4 MiB body cap — FIXED (B4 `fix/http-hardening`: 32 MiB `rpcMaxBodyBytes`, non-envelope 413) |
| F11–F14 | INV-F100 | vestigial helpers — FIXED |
| F15 | CONC-F101 | live `*Order` race — FIXED |
| F16 | STATE-F77 | post-completion retransmit — FIXED |
| F17 | CONC-F92 | blocking I/O on engine goroutine — FIXED (B11) |
| F18 | CONC-F102 | force-refund double-broadcast — FIXED |
| F19 | CRYPTO-F84 | AcceptingBody empty fee/utxos — FIXED (B2) |
| F20 | WIRE-F71 | registration integrity — FIXED (B1) |
| F21 | CRYPTO-F85 | `checkDepositTransaction` absent — FIXED (B3 `fix/deposit-path`) |
| F22 | SEC-F03 | taker-trust composite — FIXED (B3 `fix/deposit-path`) |
| F23 | CRYPTO-F86 | `buildDeposit` broadcast order — FIXED (B3 `fix/deposit-path`) |
| F24 | RPC-F58 | HTTP auth/timeout hardening — FIXED (B4 `fix/http-hardening`) |
| F25 | WIRE-F57/F63 | P2P addr/varint DoS — FIXED (B5 `fix/wire-hardening`) |
| F26 | SEC-F04 | plaintext secrets — FIXED (B6 `fix/secrets-hygiene`) |
| F27 | CRYPTO-F87 | `usedCoins` vs `ListUnspent` — FIXED (B3 `fix/deposit-path`) |
| S1-A | CRYPTO-F93 | ownership-proof challenge stream — FIXED |
| S1-B | CRYPTO-F94 | deposit `SEQUENCE_FINAL` — FIXED |
| S1-C | CRYPTO-F95 | deposit locks `Amount + fee2` — FIXED |
| S1-D | CRYPTO-F96 | `nTime` sighash on time-field coins — FIXED |
| S2-A | RPC-F57 | order-book detail-4 nesting — FIXED (B7) |
| S2-B | RPC-F59 | partial-chain unknown/malformed id — FIXED |
| S2-C | RPC-F40 | `dxSplitInputs` utxo schema — FIXED (B7) |
| S2-D | STATE-F72 | expiry sweep unwired — FIXED (B8: 15 s prune, persist 60 s, BlockNumber stamp) |
| S2-E | STATE-F78 | hub-key pinning, no TOFU — FIXED |
| S2-H | CRYPTO-F81 | base58check strictness — was DOCUMENTED; now FIX-QUEUED (C++ leniency required, §0) |
| S2-I | CRYPTO-F77 | BCH forkid sighash — FIXED (B9, fork value 0xffdead) |
| S2-J | CRYPTO-F89 | coin-family misclassification — FIXED (B9: BTG/DEVAULT; PART/BCD deferred to CRYPTO-F98/F99) |
| S2-K | CFG-F84 | `[Rpc]` aborts startup — FIXED (B10) |
| S2-L | CFG-F85 | admission gates absent — FIXED (B10: `config.Admit`) |
| S2-M | CFG-F86 | missing-conf template — IDENTICAL (startup posture, zero swap effect; B10: never-creates stance) |
| S2-N | CFG-F87 | hot-reload semantics — FIXED (B10: EW keying + gates + probe + 30 s sweep + order clearing) |
| S3-A | RPC-F11 | partial fields `"0"` literal — FIXED (B7) |
| S3-B | RPC-F44 | `dxGetUtxos` amounts trimmed — FIXED (B7) |
| S3-C | RPC-F31 | locked-utxos amount format — FIXED (B7) |
| S3-D | RPC-F28 | `p2sh_deposits` alignment — FIXED (B7) |
| S3-E | RPC-F55 | new-token-address `[]` vs error — FIXED (B7) |
| S3-F | CRYPTO-F79 | fee fallback 0 vs 2 sat/vB — was DOCUMENTED; now FIX-QUEUED (fallback must be C++ 0); CRYPTO-F80 dust key — FIXED (B10: `MinimumAmount`) |
| S3-G | CRYPTO-F90 | payout model (fee2 margin) — FIXED (B3 `fix/deposit-path`) |
| S3-H | CRYPTO-F91 | `signrawtransaction` payload — FIXED (B9) |
| S3-I | CRYPTO-F92 | secret-from-payTx input scan — FIXED (B9) |
| S3-J | STATE-F79 | `tryJoinMatches` min-size guards — FIXED (B8: confirmed vs C++ `tryJoin`) |
| S3-K | CFG-F88 | `ExchangeWallets` parsing — FIXED (B10) |
| S3-L | CFG-F89 | case-insensitive keys — FIXED (B10: exact-case) |
| S3-M | CFG-F90 | CLI flags — FIXED (B10: `-dxnowallets`/`-enableexchange`; version case done B7) |
| S3-N | CFG-F91 | conf-key set alignment — FIXED (B10: Title, MinimumAmount, CashAddrPrefix, CreateTxMethod) |
| S3-O | STATE-F73 | cancel-reason enum/text — FIXED (B8: `TxCancelReasonText` incl. C++ bugs) |
| S3-P | STATE-F74 | `trRollbackFailed` unset — FIXED (B8: `rollbackGate` on refund-broadcast failure) |
| S3-Q | STATE-F75 | no peer penalty — FIXED (B8: misbehaviour score + ban, pool + hub) |
| S3-R | SEC-F02 | inbound UTXO proofs unverified — FIXED (B8: `verifyOrderUtxos` before booking) |
| S2-S | CONC-F92 | blocking I/O on engine goroutine — FIXED (B11: buffered outbound writer + background persist) |
| S3-S | CONC-F93 | goroutine joins / Dedupe sweeper leak — FIXED (B11: WaitGroup + ctx dial + Close `FlushAll`) |
| S3-T | CONC-F94 | reload mid-swap-task — FIXED (B11: `swapCtx` connector/confs snapshot) |
| S4-D | INV-F98 | `Created` doc µs — FIXED (B11 doc) |
| S4 | RPC-F30/F36, WIRE-F57 | +1/COIN, help text, 64 MiB cap — re-triaged under §0 (F24 split to FIX-QUEUED; RPC-F52 leniency FIXED on B4) |

---

## How to maintain

- **Open a new divergence?** Add a Current-register row with the next free
  axis-prefixed ID; add its card to `findings.md` and its axis deep dive to
  `evidence/`.
- **Fix one?** Move the row's status to `FIXED` and name the branch/test that
  closed it, in the same branch that fixes the code (remediation-plan §done
  criteria step 5).
- **Think it's acceptable as-is?** It isn't — §0 forbids accepted gaps. Either
  prove it identical (name the proof pointer) or file it fix-queued; only the
  §0 waiver family is exempt, and new waivers need an explicit ruling recorded
  in the row.
- Never duplicate a finding's *status* in `findings.md` or `evidence/` — those
  files describe the divergence, this file tracks its disposition.

## Remediated at HEAD (detail)

The following prior findings are fixed and `-race`/golden-vector covered. Their
row status above is `FIXED`; this section records the closure detail so the
regression tests are locatable:

- **CRYPTO-F93.** UTXO ownership-proof challenge = C++ `UtxoEntry::toString()`:
  whole-coin `listunspent` `"value"` double streamed with
  `strconv.FormatFloat(v, 'g', 6, 64)`; golden vectors from real g++ output in
  `TestWholeCoinOstreamMatchesCppStream`.
- **CRYPTO-F94.** Deposit inputs `SEQUENCE_FINAL` (0xffffffff); refund spend keeps
  `SEQUENCE_FINAL-1` (C++ `checkDepositTransaction` hard-rejects non-final).
- **CRYPTO-F95.** Deposit locks `Amount + fee2` (`fee2 = minTxFee2(1,1)`), satisfying
  C++ `depositP2SHAmount >= amount + 0.95*fee2`; change = total − Amount − fee − fee2.
- **CRYPTO-F96.** `nTime` committed in the sighash on `TxWithTimeField` coins (4-byte
  LE `TxTime` after `nVersion` in `HashForSigning`); golden digests from a C++
  oracle in `TestHashForSigningWithTimeField`.
- **RPC-F59.** `dxGetMyPartialOrderChain` unknown id → `[]`, malformed id →
  `bad order id` (`api/handlers.go:863-865,858-861`).
- **STATE-F78.** Handshake inbound packets (Hold/Init/CreateA/B/ConfirmA/B/Finished)
  re-verified against the pinned hub key + registry membership — no TOFU; a
  forged `Finished` is dropped before `OnFinished`; non-registered hub →
  `NO_SERVICE_NODE`.
- **SEC-F01.** RPC binds loopback by default; Basic auth when both `-rpcuser` +
  `-rpcpassword` set (constant-time compare); non-loopback bind without auth
  warns.
- **CONC-F97/F98.** `SwapSession` single-owner (engine-only) +
  `atomic.Pointer[map]` coin registry; race-covered by
  `TestConcurrentRefundSweepAndDepositTask`, `TestConcurrentInitFromConfGet`.
- **CONC-F99.** Unbounded growth bounded: `pruneSessions` + `trimOldest` (1000
  each); `TestPruneSessionsRemovesTerminal`, `TestStoreHistoryBounded`.
- **CONC-F101.** MakeOrder returns a store snapshot copy, not the live pointer;
  `TestMakeOrderReturnsStoreCopy`.
- **STATE-F77.** Post-completion retransmit guards (`state >= csCreatedA/B`,
  `>= csConfirmedA/B`); `*StateGuard*` tests + `TestCreateAStateGuard*`.
- **CONC-F102.** Force-refund takes the `pendingRefunds` sweep guard;
  `TestForceRefundTakesSweepGuard`.
- **CRYPTO-F84.** AcceptingBody funded — B2 `fix/wire-acceptingbody`
  (atomic per-order reservation `Store.ReserveForTake`, p2pkh-25 funding filter,
  same-order `BAD_REQUEST` gate, A1–A7 gate closeout, per-token lock exclusion
  D4). Tests: `TestStoreReserveForTake`, `TestConcurrentTakeOrder*`,
  `TestTakeOrderFundingRejectsNonP2PKH`, `TestTakeOrderEmptyBlockWallet`,
  `TestLockedUtxoInfoFor` + `make parity`.
- **WIRE-F71.** Servicenode registration integrity — B1
  `fix/servicenode-registry` (retain + gate registration fields, `CreateSigHash`
  golden, `PaymentAddress()`); `TestParseServiceNode*`, `TestCreateSigHashGolden`,
  `TestAddRegistrationRejectMatrix`, `TestPaymentAddress*`.
