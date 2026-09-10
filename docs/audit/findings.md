# FINDINGS.md

# Blocknet Core xBridge ↔ go-xbridge Conformance Audit — Consolidated Findings

REF (source of truth): `blocknet_core/src/xbridge/` (+ `src/rpc/server.cpp`, `src/httpserver.cpp`, `src/net_processing.cpp`, `src/protocol.*`, `src/init.cpp`, `src/clientversion.cpp`, `src/chainparams.cpp`)
CAND: `go-xbridge/` (`api/`, `proto/`, `p2p/`, `swap/`, `coins/`, `crypto/`, `config/`, `wallet/`, `cmd/xbridged`)

Severity scale:
- **S1 Critical** — byte-level wire/on-chain divergence or data-integrity break (a dApp or an on-chain transaction is provably different).
- **S2 High** — observable behavioral divergence a dApp hits on normal inputs (different result value / shape / ordering / side effect / status).
- **S3 Medium** — divergence on specific/edge inputs (validation, formatting, units, robustness).
- **S4 Low** — cosmetic / internal / documentation / robustness-only.

Every finding was double-checked by a verification subagent that re-traced the full call graph on both sides (reports: `verify/verify_a.md`…`verify/verify_e.md`). Status: **CONFIRMED** unless noted. Zero TBD entries; every finding has a concrete FIX.

---

## A. RPC AXIS

### RPC-F01 · S2 · RPC · Error-channel policy: C++ *throws* envelope errors for param/type errors; Go returns business results
- REF: `rpcxbridge.cpp:159-168` (dxGetOrderBook param parse throw), and the count/type throws on dxMakeOrder, dxMakePartialOrder, dxTakeOrder, dxGetTradingData, dxSplitAddress, dxSplitInputs, dxGetUtxos, dxGetOrderHistory, dxGetMyPartialOrderChain, dxPartialOrderChainDetails; thrown RPC errors become envelope `error` via `src/rpc/server.cpp` (code −1 RPC_INVALID_PARAMS / −3 help text).
- CAND: `api/server.go:181-186` (business errors ride in `result`), `api/handlers.go` param parsers coerce wrong types (`api/dispatch.go:75-149` `strParam/intParam/…`), never an envelope error.
- OBSERVED_REF: envelope `{"error":{"code":-1,"message":…},"result":null,"id":…}`.
- OBSERVED_CAND: `{"error":null,"result":{"error":"Invalid parameters: …","code":1025,"name":"dxXxx"},"id":…}`.
- IMPACT: Bitcoin-Core-style clients that gate on envelope `error` see null on Go and a real error on C++; wrong-typed params that C++ rejects are silently coerced/ignored by Go. Affects make/take/tradingdata/split/utxo/orderbook/orderhistory/chain methods.
- FIX: add an `rpcInvalidParams(msg)` path that returns the envelope error object (code −1, exact C++ message) for param-count and param-type errors on those methods; make param parsers strict (reject non-int for int params with a throw-equivalent) instead of coercing.

### RPC-F02 · S2 · RPC · NO_SESSION error `name` hardcoded `"dx"` instead of the method name
- REF: every handler passes `__FUNCTION__`, e.g. `"dxGetOrder"` (`rpcxbridge.cpp:790-795`, and all `makeError(NO_SESSION, __FUNCTION__, …)` sites).
- CAND: `api/handlers.go:200,204` — `connector()` helper returns `makeError(errNoSession, "dx", …)`.
- OBSERVED_REF: `"name":"dxGetOrder"`.
- OBSERVED_CAND: `"name":"dx"`.
- IMPACT: dApps matching the `name` field (a documented error field) mis-identify the failing method. Affects: dxGetOrder, dxCancelOrder, dxGetUtxos, dxSplitAddress, dxSplitInputs (NOT OrderBook/TokenBalances/LockedUtxos).
- FIX: pass the handler name into `makeError` at every NO_SESSION call site (or inject the method name into the error builder).

### RPC-F03 · S2 · RPC · dxGetOrders array order is random (Go map) vs id-ascending (C++ std::map)
- REF: `rpcxbridge.cpp:42` (`std::map<uint256, …>` typedef), `rpcxbridge.cpp:430,434` iterate ascending-by-id.
- CAND: `api/store.go:154-162` iterates a Go map; `api/handlers.go:108` appends in map order (unsorted).
- OBSERVED_REF: `[id1,id2,id3…]` ascending by `uint256 operator<` — memcmp from `data[0]`, i.e. **LSB-first** byte order (`uint256.h:45-49`), which is NOT display-hex ascending.
- OBSERVED_CAND: nondeterministic order across calls.
- IMPACT: dApps that assume ordering or render a stable list see nondeterministic output.
- FIX: sort the returned slice by the internal `[32]byte` id via `orderIDLess` (LSB-first), matching `std::map<uint256>` — resolved at B7 (`TestDxGetOrdersSortedById`, `TestOrderIDLess`).

### RPC-F04 · S3 · RPC · dxGetOrders 60-second filter boundary differs (µs-exact vs second-truncated)
- REF: `rpcxbridge.cpp:431,439` — `total_seconds() > 60` (second granularity), includes the (60s, 61s) window.
- CAND: `api/handlers.go:111` — `> 60*1e6` microseconds, excludes orders in that window.
- IMPACT: an order aged 60.4s is returned by C++ but dropped by Go.
- FIX: truncate to seconds before comparing (`> 60` at second granularity).

### RPC-F05 · S2 · RPC · Exactly-64-hex id gate vs C++ `uint256S` tolerance
- REF: `blocknet_core/src/uint256.cpp:27-53` — `uint256S` left-pads 1–63 hex, truncates ≥64; only 3 of the 5 methods add an `IsNull` check.
- CAND: `api/response.go:506-508` `parseOrderID` requires exactly 64 hex chars; used at `api/handlers.go:144,414,877,969,1086` (dxGetOrder, dxCancelOrder, dxGetMyPartialOrderChain, dxPartialOrderChainDetails, dxGetLockedUtxos).
- OBSERVED_REF: a 63-hex id resolves (zero-padded) on C++.
- OBSERVED_CAND: `"Invalid parameters"` error on Go.
- IMPACT: dApps passing short/odd-length ids (valid on blocknetd) break against go-xbridge.
- FIX: implement a `uint256S`-equivalent parser (left-pad to 64, truncate >64) for those five methods, retaining the C++ `IsNull` checks.

### RPC-F06 · S3 · RPC · dxGetOrder not-found message renders id differently (zero-padded vs raw)
- REF: `rpcxbridge.cpp:784-786` — `"Transaction " + uint256S(id).ToString() + " not found"`, id zero-padded to 64 hex.
- CAND: `api/handlers.go:148,158` — echoes the raw input string.
- IMPACT: for short/odd ids the exact error string differs ("Transaction 000…abc not found" vs "Transaction abc not found").
- FIX: render the parsed id via the same display-hex ordering (see RPC-F05 parser).

### RPC-F07 · S3 · RPC · dxCancelOrder performs the cancel *before* validating connectors
- REF: `rpcxbridge.cpp:1370` (cancel) runs before the NO_SESSION checks at `:1376-1384`.
- CAND: `api/handlers.go:428-433` validates connectors first, then cancels (`api/node.go:1706+`).
- IMPACT: on C++, a dApp can observe the cancel side effect even when the RPC returns an error; on Go it cannot. Divergent side-effect visibility.
- FIX: mirror C++ ordering (cancel first, then validate) or document the divergence explicitly.

### RPC-F08 · S3 · RPC · dxCancelOrder cancel-failure error codes/text set differs
- REF: cancel failure paths emit 1018 / 1021 / 1028 (plus an isLocal→1021 branch).
- CAND: `api/node.go:1773-1790` emits 1004 / 1002 / 1032 / 1021; no isLocal branch.
- IMPACT: dApps branching on error code get different codes.
- FIX: align codes and add the isLocal→1021 branch.

### RPC-F09 · S2 · RPC · dxMakeOrder response field ORDER differs (created_at/updated_at swapped; maker_address/taker_address/block_id at end)
- REF: `rpcxbridge.cpp:1004-1021,1049-1066` — key insertion order differs from CAND.
- CAND: `api/response.go:57-62` `makeOrderResult` struct order — `created_at` before `updated_at` while C++ emits `updated_at` before `created_at`; `maker_address`/`taker_address`/`block_id` appended last.
- IMPACT: byte-level JSON body differs (encoding/json uses struct order). Parsers that depend on byte order (rare) or diff harnesses fail.
- FIX: reorder the Go response struct fields to match C++ insertion order.

### RPC-F10 · S2 · RPC · dxMakeOrder / dxMakePartialOrder dryrun returns real id + extra fields vs C++ zero id
- REF: dryrun returns a zero order id and 14 fields (no block fields / no timestamps populated).
- CAND: dryrun computes and returns a real id + `block_id` + timestamps (17 fields).
- IMPACT: dApp using dryrun to preview "what the id would be" sees a real id from Go (collision/misuse risk) vs zero from C++.
- FIX: on dryrun, return the zero id and omit `block_id`.

### RPC-F11 · S2 · RPC · dxMakeOrder non-partial `partial_*` values: literal `"0"` vs `"0.000000"`
- REF: `rpcxbridge.cpp:1004-1021` emits literal `"0"` for partial_minimum / partial_orig_maker_size / partial_orig_taker_size on a non-partial order.
- CAND: `api/order.go:242-255` formats as `"0.000000"`.
- IMPACT: value-level mismatch on a returned field.
- FIX: emit literal `"0"` when the order is not partial.

### RPC-F12 · S3 · RPC · dxMakeOrder NO_SERVICE_NODE message includes the pair; C++ bare
- REF: `rpcxbridge.cpp:1072` — bare message; the pair-argument branch at `:3153` is dead code.
- CAND: `api/node.go:916,922` — appends e.g. `"LTC/BLOCK"`.
- IMPACT: error text differs.
- FIX: drop the pair from the message.

### RPC-F13 · S2 · RPC · dxMakePartialOrder dust gate compares 1e6-scale minFrom against native 1e8-scale dust (~100× off)
- REF: `xbridgewalletconnectorbtc.cpp:1900-1904` — `isDustAmount` at native base units (1e8-style).
- CAND: `api/node.go:961` compares the 1e6-scale minFromAmount against the native-scale `effectiveDust`.
- IMPACT: partial orders C++ rejects as dust are accepted by Go (~100× threshold drift).
- FIX: normalize both operands to the same base-unit scale before comparing.

### RPC-F14 · S2 · RPC · dxTakeOrder explicit amount `"0"` is a full take in Go, error in C++
- REF: `rpcxbridge.cpp:1156-1159` — explicit 0 → error 1025.
- CAND: `api/node.go:1351-1374` — treats 0 as full take.
- IMPACT: a dApp sending 0 expecting an error gets a live take order.
- FIX: reject explicit 0 with 1025.

### RPC-F15 · S3 · RPC · dxTakeOrder error texts/names leak (INVALID_ADDRESS text, `name` "dxMakeOrder")
- REF: `rpcxbridge.cpp:1139-1166` — INVALID_ADDRESS arg text and `name` = `"dxTakeOrder"`.
- CAND: `api/handlers.go:373-407` — message arg differs; `name` field leaks `"dxMakeOrder"` on some paths; later-stage BAD_REQUEST/NO_SERVICE_NODE messages carry args C++ leaves bare.
- IMPACT: exact error string/name mismatch.
- FIX: correct message templates and the `name` field per path.

### RPC-F16 · S2 · RPC · dxGetOrderHistory sums the WRONG asset volume
- REF: `rpcxbridge.cpp:684` `fromVolume` — sums the maker/from asset (`xseries.cpp:195`).
- CAND: `api/handlers.go:552` — `volume += takerNum` (taker asset).
- IMPACT: wrong volume numbers for any pair where the two sides differ.
- FIX: sum the from/maker side.

### RPC-F17 · S2 · RPC · dxGetOrderHistory encoding: shortest-roundtrip floats vs C++ fixed-8; raw ratio vs 1e-6-quantized price
- REF: `rpcxbridge.cpp:51` `uret(…, none, 8)` 8-decimal rendering; prices 1e-6-quantized via `currency.h:118-120` boost::rational.
- CAND: `api/handlers.go:26-32` `xfloat` emits shortest-roundtrip (e.g. `"0.5"`); price is a raw float ratio.
- IMPACT: OHLCV fields render differently at the value level ("0.00000000" vs "0").
- FIX: format OHLCV to fixed 8 decimals and quantize price to the 1e-6 grid.

### RPC-F18 · S2 · RPC · dxGetOrderHistory validation missing (granularity whitelist, end≤start, too-large, limit)
- REF: `xseries.h:119-121` whitelist {60,300,900,3600,21600,86400}; `xseries.h:94` `"Start time >= end time."` 1025; too-large and interval-limit-range errors.
- CAND: none of these enforced; `end<=start` → empty `[]`; 8th `limit` param ignored.
- IMPACT: invalid/unsupported requests silently succeed with empty data instead of erroring.
- FIX: port the whitelist, the exact 1025 messages, and limit handling.

### RPC-F19 · S2 · RPC · dxGetOrderHistory data source: Go fills store is never written → always empty
- REF: C++ scans on-chain XSeries (source: `xseries.cpp` / `rpcxbridge.cpp:588-704`).
- CAND: `api/handlers.go` reads `Store.Fills()` which is only populated in tests (grep: no production `AddFill` caller) → production series is always empty.
- IMPACT: a core trading-history RPC returns empty on Go.
- FIX: populate fills from an on-chain scan (or a filled-order feed); until then document.

### RPC-F20 · S3 · RPC · dxGetOrderBook price formula omits C++ +1/COIN bump
- REF: `xutil.cpp:223-227,293-312` — +1/COIN per amount before division → 1/3 = `"0.333334"`.
- CAND: `api/handlers.go` plain ratio → `"0.333333"`.
- IMPACT: last-digit price divergence on non-divisible amounts.
- FIX: apply the +1/COIN bump.

### RPC-F21 · S3 · RPC · dxGetOrderBook equal-best-price tie-break nondeterministic
- REF: `rpcxbridge.cpp:1654-1671` — `max/min_element` picks smallest id.
- CAND: `api/handlers.go:675-676` — unstable sort → arbitrary pick.
- IMPACT: best bid/ask source order flips across calls.
- FIX: deterministic smallest-id tie-break.

### RPC-F22 · S3 · RPC · dxGetOrderBook int-width (Go `int` vs C++ int64_t)
- REF: count parsed as int64_t.
- CAND: `api/handlers.go` uses Go `int`.
- IMPACT: platform-dependent width; 32-bit builds would truncate.
- FIX: use int64.

### RPC-F23 · S2 · RPC · dxGetTokenBalances "Wallet" key derivation and presence differ
- REF: `rpcxbridge.cpp:2531-2532` — always emits `"Wallet"` first, from the core wallet `availableBalance`.
- CAND: `api/handlers.go:785-832` — derives from the BLOCK connector (or first exchange wallet), omits the key when no BLOCK connector → `{}` vs `{"Wallet":{…}}`.
- IMPACT: a dApp sees a missing "Wallet" entry from Go.
- FIX: always emit "Wallet" from the core-wallet balance path.

### RPC-F24 · S3 · RPC · dxGetTokenBalances key order (Go map-sorted, Wallet last)
- REF: C++ `"Wallet"` first + insertion order.
- CAND: `api/handlers.go:762-834` map → `encoding/json` sorts keys; Wallet last.
- IMPACT: byte-level and positional differences.
- FIX: emit keys in insertion order (ordered builder) with Wallet first.

### RPC-F25 · S3 · RPC · dxGetTokenBalances precision (C++ per-UTXO double sum vs Go exact integer)
- REF: `rpcxbridge.cpp` sums native-unit doubles per-UTXO (no explicit divide).
- CAND: exact uint64 sum then one divide.
- IMPACT: possible last-digit differences on large/awkward sums.
- FIX: confirm against the C++ double-sum path; add a golden value test.

### RPC-F26 · S3 · RPC · dxGetMyOrders field order, param rejection, dedup, sort
- REF: `rpcxbridge.cpp:1992` — `maker_address`/`taker_address` at positions 3/6; rejects params; `seen` dedup across live+history; sorts by µs txtime.
- CAND: `api/handlers.go:840-868` + `api/response.go:50-54` — addresses appended last; no param rejection; no `seen` dedup; sorts by ms-rendered string.
- IMPACT: key position, duplicate rows, and ordering differences.
- FIX: reorder struct fields, reject params, add dedup, sort by µs numeric.

### RPC-F27 · S2 · RPC · dxGetMyPartialOrderChain chain membership differs (filters/sort)
- REF: `xbridgeapp.cpp:3922,3934,3985-3988` — filters local partial/partial-child orders in live+history and sorts by created time.
- CAND: `api/handlers.go:900-961` — applies Mine only to history, skips the partial-parent filter and created sort, walks the full descendant tree.
- IMPACT: the returned chain has different members and order.
- FIX: port the chain-walk filter + created-time sort.

### RPC-F28 · S2 · RPC · dxPartialOrderChainDetails p2sh_deposits array length ≠ chain length
- REF: `rpcxbridge.cpp:2456-2457` — one entry per order including empty strings for missing binTxId.
- CAND: `api/handlers.go:1008-1013` — drops empty entries.
- IMPACT: dApps indexing by position get misaligned arrays.
- FIX: emit empty entries to preserve per-order alignment.

### RPC-F29 · S3 · RPC · dxPartialOrderChainDetails bad-id error text
- REF: `rpcxbridge.cpp:2414` — `"Invalid parameters: bad order id"`.
- CAND: `api/handlers.go:971` — `"Invalid parameters: Invalid order id [<id>]"`.
- IMPACT: exact message mismatch.
- FIX: match C++ text.

### RPC-F30 · S3 · RPC · dxPartialOrderChainDetails key order alphabetized (Go map)
- REF: C++ insertion order.
- CAND: `api/handlers.go:1015-1036` map → sorted keys.
- IMPACT: byte-level JSON order differs.
- FIX: ordered output.

### RPC-F31 · S3 · RPC · dxGetLockedUtxos amount encoding (default-float vs fixed-6/native)
- REF: `xbridgewalletconnector.cpp:25-30` — `UtxoEntry::toString` default-double (6 significant digits).
- CAND: `api/handlers.go:1077-1081` — fixed-6/native-trimmed.
- IMPACT: diverges for coins absent from the Go registry (e.g. `0.1` vs `0.100000`).
- FIX: match C++ default-float rendering or restrict to registry coins.

### RPC-F32 · S2 · RPC · dxGetLockedUtxos 1021 trigger too narrow in Go
- REF: `rpcxbridge.cpp:2637,2660-2670` — 1021 requires a getUtxoItems hit + pending/accepted validity.
- CAND: `api/handlers.go:1090-1093` — only `Store.Get` presence; returns `[]` where C++ errors 1021.
- IMPACT: different success/error outcome for the same id.
- FIX: replicate the validity check.

### RPC-F33 · S3 · RPC · dxGetLockedUtxos per-order key selection (status ordinal vs map membership)
- REF: `rpcxbridge.cpp:2674-2677` — pending/accepted map membership.
- CAND: `api/handlers.go:1100-1103` — status ordinal ≥ DescrAccepting.
- IMPACT: from/to key placement differs for edge states.
- FIX: mirror the membership logic.

### RPC-F34 · S3 · RPC · dxGetLockedUtxos id echo un-normalized
- REF: C++ normalizes via `GetHex`.
- CAND: `api/handlers.go:1084,1140-1143` echoes the raw param.
- IMPACT: echoed id differs for short/odd ids (see RPC-F05/RPC-F06).
- FIX: normalize via display-hex.

### RPC-F35 · S2 · RPC · dxFlushCancelledOrders flushes only the cancelled ledger, not the book/history
- REF: `xbridgeapp.cpp:1336-1351` — erases cancelled orders from `m_transactions` AND `m_historicTransactions`.
- CAND: `api/store.go:491-513` — prunes only the `s.cancelled` ledger.
- IMPACT: cancelled orders remain visible in Go after flush.
- FIX: remove from both live and history stores.

### RPC-F36 · S3 · RPC · dxFlushCancelledOrders use_count / ordering / key order
- REF: `xbridgeapp.cpp:1345` — live `shared_ptr` use_count; id-sorted map iteration; `ageMillis, now, durationMicrosec` key order.
- CAND: hardcoded `use_count:1`; insertion order; Go map sorts keys (durationMicrosec before now).
- IMPACT: value + ordering mismatch.
- FIX: source use_count from the store's refcount, sort by id, emit keys in C++ order.

### RPC-F37 · S2 · RPC · `gettradingdata` (lowercase) missing from Go dispatch
- REF: `rpcxbridge.cpp:3520-3521` registers `gettradingdata`.
- CAND: `api/dispatch.go:38-63` — absent → `-32601 Method not found`.
- IMPACT: a dApp calling `gettradingdata` (a registered blocknetd command) breaks against go-xbridge.
- FIX: add the lowercase alias.

### RPC-F38 · S2 · RPC · dxGetTradingData fee_txid/nodepubkey always `""`
- REF: `rpcxbridge.cpp:2860-2863` — on-chain txid + service-node address.
- CAND: `api/handlers.go:1207-1208` — hardcoded `""`.
- IMPACT: fee-tracking dApps get empty data.
- FIX: populate from the on-chain scan (see RPC-F39).

### RPC-F39 · S2 · RPC · dxGetTradingData data source: local fills vs on-chain scan
- REF: scans the BLOCK chain (43200 blocks / 30-day window) with `errors=true` records.
- CAND: reads local `Store.Fills()` only (never written in production).
- IMPACT: always-empty result from Go.
- FIX: port the on-chain scan (or a filled-order feed).

### RPC-F40 · S2 · RPC · dxSplitInputs requires amount/scriptPubKey/address; C++ needs only txid/vout
- REF: C++ `utxos` entries need only txid/vout (documented example works).
- CAND: `api/handlers.go:1246+` — rejects txid+vout-only input with `1025 "invalid utxo amount"`.
- IMPACT: the documented dxSplitInputs example fails on Go.
- FIX: accept txid/vout-only entries.

### RPC-F41 · S2 · RPC · dxSplit fee formula differs
- REF: `xbridgewalletconnectorbtc.cpp:2665-2668,2707-2734` — 520·feePerByte per output + real-tx-fee claw-back from change.
- CAND: `api/handlers.go:1356-1379` — whole-tx `(192·nIn+68)·feePerByte`, no change deduction.
- IMPACT: different on-chain fees for split transactions.
- FIX: port the C++ fee formula (per-output 520·feePerByte + claw-back).

### RPC-F42 · S2 · RPC · dxSplit change destination differs
- REF: change sent to the requested `address`.
- CAND: change sent to a fresh address.
- IMPACT: dApp expecting change at its address gets a fresh address.
- FIX: send change to the requested address (mirror C++).

### RPC-F43 · S3 · RPC · dxSplit submit-failure code and error names
- REF: `1004` and names `"dxSplitAddress"`/`"dxSplitInputs"`.
- CAND: `1002` and names `"dxSplit"`/`"dx"` (splitTx helper).
- IMPACT: error code/name mismatch.
- FIX: correct codes and names.

### RPC-F44 · S2 · RPC · dxGetUtxos amounts trimmed vs C++ fixed-8
- REF: `rpcxbridge.cpp:3483` — `"1.00000000"`.
- CAND: `api/coins amount.go:65-81` — `"1"`.
- IMPACT: value-level mismatch on returned amounts.
- FIX: fixed-8 formatting.

### RPC-F45 · S3 · RPC · dxGetUtxos listunspent failure code/text
- REF: `rpcxbridge.cpp:3476` — `1004 "Bad Request failed to get unspent transaction outputs"`.
- CAND: `api/handlers.go:1680` — `1002 "Internal Server Error"`.
- IMPACT: different error code/text.
- FIX: match C++ code and text.

### RPC-F46 · S2 · RPC · getnetworkinfo shim diverges from the real blocknetd RPC
- REF (real daemon): `blocknet_core/src/version.h:12` protocolversion `70713`; getnetworkinfo relayfee `0.00010000`, incrementalfee `0.00001000`; fields `xbridgeprotocolversion`(55) and `xrouterprotocolversion`(50); `subversion "/Blocknet:4.4.1/"` (`clientversion.cpp`); networks entries with `proxy_randomize_credentials`.
- CAND (pre-fix): `api/handlers.go` — protocolversion `70015`; relayfee `0.00001`; incrementalfee `0.00000001`; missing xbridge/xrouter protocol fields; `"/blocknet:4.4.1/"` lowercase; networks entries missing proxy_randomize_credentials. Resolved at B7: the shim now emits all 15 daemon fields/values (sorted-map key order remains the only divergence).
- IMPACT: dApps feature-detecting via getnetworkinfo get wrong capabilities.
- FIX: mirror the real daemon's getnetworkinfo fields and values.

### RPC-F47 · S2 · RPC · JSON-RPC transport: HTTP status for parse/method-not-found (C++ 500/404 vs Go 200)
- REF: `src/rpc/server.cpp` — HTTP 500 for parse errors, HTTP 404 for unknown methods.
- CAND: `api/server.go:130-139,171-176` — HTTP 200 with envelope error `-32700`/`-32601`.
- IMPACT: dApps relying on HTTP status codes observe different behavior.
- FIX: emit 500 for parse errors and 404 for unknown methods.

### RPC-F48 · S3 · RPC · JSON-RPC method-not-found message appends the method name
- REF: `server.cpp:568` — `"Method not found"`.
- CAND: `api/server.go:173` — `"Method not found: <method>"`.
- IMPACT: envelope message mismatch.
- FIX: drop the appended method name.

### RPC-F49 · S3 · RPC · JSON-RPC request body limit 4 MiB vs 32 MiB
- REF: `MAX_REQUEST_BODY_LENGTH` 32 MiB.
- CAND: `api/server.go:111` `rpcMaxBodyBytes = 4 << 20`.
- IMPACT: large-but-legal requests rejected by Go.
- FIX: raise to 32 MiB.

### RPC-F50 · S2 · RPC · JSON-RPC auth model: Go open-by-default vs C++ always-auth
- REF: `httprpc.cpp:171,215-235` — always authenticates when RPC enabled; cookie auth, rpcauth, and a 250 ms sleep on failed auth.
- CAND: `api/server.go:62-81` — Basic auth only when BOTH `-rpcuser` and `-rpcpassword` are set; otherwise open.
- IMPACT: an unauthenticated go-xbridge daemon is wide open; dApp clients that omit credentials succeed against Go but are rejected by C++.
- FIX: enforce auth whenever serving non-loopback; implement cookie/rpcauth and the failure delay.

### RPC-F51 · S3 · RPC · JSON-RPC batch / named params / -32600 unsupported
- REF: core server supports batch arrays and named-parameter objects.
- CAND: `api/server.go` single object + positional params only; no `-32600` path.
- IMPACT: batch/named requests fail on Go.
- FIX: add batch + named-param support + `-32600` invalid-request handling.

### RPC-F52 · S3 · RPC · Extra positional params accepted where C++ errors (business 1025)
- REF: C++ returns `1025` (business) for extra params on dxGetOrderFills (needs ≤3), dxGetOrders, dxGetOrder (needs 1), dxGetLocalTokens, dxLoadXBridgeConf, dxGetNetworkTokens, dxGetTokenBalances, dxGetMyOrders.
- CAND: `api/handlers.go` — extra params silently ignored; also arity missing for dxGetNewTokenAddress (C++ needs exactly 1) and `include_used` accepted as a string by dxGetUtxos where C++ throws.
- IMPACT: Go succeeds where C++ errors; dApps relying on validation see different behavior.
- FIX: add arity gates returning the C++-equivalent 1025 (or envelope throw per RPC-F01) on each method.

### RPC-F53 · S3 · RPC · dxGetLocalTokens returns unconnected/duplicate tickers
- REF: `xbridgeapp.cpp:1129-1204` — only connected connectors.
- CAND: `api/handlers.go:174+` — raw ExchangeWallets incl. unconnected/duplicate tickers.
- IMPACT: token list content differs.
- FIX: filter to connected connectors.

### RPC-F54 · S3 · RPC · dxGetNetworkTokens membership: Go unions config/ExchangeWallets; C++ pure SN service union
- REF: `rpcxbridge.cpp:281` — pure running service-node union.
- CAND: `api/handlers.go:178+` — unions config tokens into the result.
- IMPACT: token list content differs.
- FIX: return only the running-SN service union.

### RPC-F55 · S3 · RPC · dxGetNewTokenAddress error path returns `[]` in C++, business 1002 in Go
- REF: `rpcxbridge.cpp:150-192` — wallet address-gen failure → empty array.
- CAND: `api/handlers.go:231-251` — 1002 "Internal Server Error".
- IMPACT: success/failure shape differs.
- FIX: return `[]` on address-gen failure.

### RPC-F56 · S3 · RPC · dxLoadXBridgeConf reload failure shape and side effects differ
- REF: `rpcxbridge.cpp:218-220` rejects params; on reload failure returns result bool `false` (`xbridgeapp.cpp:512-514`); clears non-local orders and bad wallets; guards on shutdown/wallet-update (throws).
- CAND: `api/handlers.go:213-230` — no param rejection; returns an error object (1025) instead of bool `false`; missing clearNonLocalOrders/clearBadWallets side effects; missing shutdown/updating guards.
- IMPACT: reload success/failure signaling and follow-on state differ.
- FIX: return bool `false`, add param gate, port the clearing side effects and guards.

---

## B. WIRE AXIS

### WIRE-F57 · S2 · WIRE · P2P max payload 67 MiB vs C++ 4,000,000
- REF: `net.h:55`, `net.cpp:583` — `MAX_PROTOCOL_MESSAGE_LENGTH = 4,000,000`; disconnect beyond.
- CAND: `p2p/message.go:15,70-75` — `MaxPayloadSize = 1<<26` (67 MiB) + 64 MiB read cap.
- OBSERVED: a 10 MiB message passes Go, rejected by C++.
- IMPACT: memory exhaustion / protocol-deviation; a C++ node drops what a Go node accepts.
- FIX: cap at 4,000,000.

### WIRE-F58 · S2 · WIRE · Received magic never validated
- REF: `net_processing.cpp:3117-3121` — disconnect on wrong message start.
- CAND: `p2p/message.go:58` — reads but never compares inbound magic.
- IMPACT: cross-network frames could be accepted/processed.
- FIX: validate magic against the configured network on receive.

### WIRE-F59 · S2 · WIRE · No MIN_PEER_PROTO_VERSION gate
- REF: `version.h:27` `MIN_PEER_PROTO_VERSION=70712`; disconnect at `net_processing.cpp:1617-1626`.
- CAND: none in `p2p/version.go` / `p2p/conn.go`.
- IMPACT: Go accepts old/incompatible peers C++ rejects.
- FIX: enforce the minimum version gate in the handshake.

### WIRE-F60 · S2 · WIRE · "staging" network magic is actually C++ REGTEST
- REF: `chainparams.cpp:417-421` — `a1cf7eac:41489` is REGTEST.
- CAND: `p2p/params.go:4-8` labels `a1cf7eac:41489` "staging".
- IMPACT: `-network staging` peers with regtest nodes (isolation/labeling divergence); no real Blocknet staging net exists.
- FIX: rename to regtest or point at a real staging network; document.

### WIRE-F61 · S2 · WIRE · `snl` (SNLIST) responses ignored
- REF: `net_processing.cpp:2992-2998` — answers SNLIST with SNLISTPING.
- CAND: `p2p/discovery/peer_manager.go:341-344` — constant only, ignored; `p2p/servicenode/servicenode.go` never uses `snl` (comment stale).
- IMPACT: service-node discovery/ping lists not consumed → stale registry.
- FIX: implement `snl` receive + use.

### WIRE-F62 · S3 · WIRE · proto.Unmarshal silently ignores trailing body bytes
- REF: `xbridgepacket.h:489-493` — `copyFrom` rejects length mismatch.
- CAND: `proto/packet.go:117-123` — ignores trailing bytes.
- IMPACT: malformed packets accepted by Go.
- FIX: reject size mismatch.

### WIRE-F63 · S3 · WIRE · Non-canonical CompactSize varint accepted
- REF: `serialize.h:289-303` — `ReadCompactSize` throws on non-canonical encodings.
- CAND: `p2p/envelope.go:101-129` — accepts non-canonical/unbounded.
- IMPACT: lenient decode of malformed lengths.
- FIX: reject non-canonical CompactSize.

### WIRE-F64 · S3 · WIRE · Bad checksum disconnects instead of log-and-drop
- REF: `net_processing.cpp:3138-3145` — logs and drops.
- CAND: `p2p/message.go:78-79` → `peer_manager.go:281-299` — returns error and closes the connection.
- IMPACT: single bad frame kills a peer (DoS amplification) — behavior differs.
- FIX: log-and-drop.

### WIRE-F65 · S3 · WIRE · Go-only 1 MiB XBridge body cap
- REF: no per-packet body cap (only the 4 MB P2P cap).
- CAND: `proto/packet.go:31` `MaxBodySize = 1<<20`.
- IMPACT: legal oversized packets rejected by Go (robustness; no C++ interop break).
- FIX: align or document as a deliberate hardening limit.

### WIRE-F66 · S3 · WIRE · Command 4 (xbcPendingTransaction) has two C++ writers differing by a trailing minFromAmount
- REF: `xbridgesession.cpp:3626` (`sendListOfTransactions`, 126 B, omits minFromAmount) vs `:3666` (`sendTransaction`, 134 B, appends it); the C++ reader requires exactly 134 B, so the minFromAmount writer is authoritative.
- CAND: `proto/body_types.go` always writes minFromAmount and tolerates both on read — matches the authoritative writer.
- IMPACT: none interop-wise; the 126 B variant would fail decode. Document only.
- FIX: document; no code change required (Go is correct).

### WIRE-F67 · S2 · WIRE · Command 2 (xbcXChatMessage) body is speculative
- REF: no C++ writer (unimplemented); header comment claims `uint160 + message`.
- CAND: `proto/body_types.go:913-927` raw-bytes body.
- IMPACT: if ever implemented, the wire format is undefined; currently both sides never send it.
- FIX: either implement the C++ writer to define the format or delete the Go body type until then.

### WIRE-F68 · S2 · WIRE · Command 50 (xbcServicesPing) body claim is unbacked
- REF: no C++ writer; never sent; handler unbound (`xbridgesession.cpp:186-220`).
- CAND: `proto/body_types.go:933-956` `ServicesPingBody` exists but `DecodeBody` excludes it (body_types.go:1020-1027) and no go-xbridge code parses a services-ping body.
- IMPACT: dead/speculative type; the `p2p/servicenode parses it` claim is false.
- FIX: implement a parser or remove the type and correct the comment.

### WIRE-F69 · S3 · WIRE · getaddr policy and addr cap differ
- REF: answers getaddr only from inbound peers, once, ≤1000; rejects inbound addr >1000 (`net_processing.cpp:2665-2686,1825-1830`, `net.h:53`).
- CAND: answers any requester with 64 addrs; no 1000-cap on received addr (`peer_manager.go`).
- IMPACT: different gossip/privacy behavior and unbounded addr ingestion.
- FIX: inbound-only once policy + 1000-cap on receive.

### WIRE-F70 · S3 · WIRE · Version handshake differences (deadline, SENDHEADERS/SENDCMPCT, pings)
- REF: 60 s first-message deadline; sends SENDHEADERS/SENDCMPCT; pings every ~2 min.
- CAND: `p2p/conn.go:26` 30 s deadline; never sends SENDHEADERS/SENDCMPCT; never initiates pings.
- IMPACT: interop with strict C++ peers could drop Go (deadline) or miss feature negotiation; low severity since xbridge doesn't need those messages.
- FIX: align deadline to 60 s; optionally send the negotiation messages.

---

## C. STATE MACHINE AXIS

### STATE-F71 · S2 · STATE · OnHold/OnInit skip the C++ amount/identity/price verification
- REF: `xbridgesession.cpp:1364,1407-1471,1750-1760` — processTransactionHold/Init re-verify service-node key, amounts, price drift, state, and order details.
- CAND: `api/swap.go:283-308` — OnHold/OnInit skip all of it; only per-packet hub-key pinning mitigates (`api/node.go:770-777`).
- IMPACT: a mismatched/forged hold is accepted by Go that C++ would reject (swap-integrity divergence).
- FIX: port the verification (amounts, currency, price drift, identity, state).

### STATE-F72 · S2 · STATE · Expiry pruning never wired in Go
- REF: C++ erases expired pending orders on a ~15 s timer and persists every 60 s (`xbridgeapp.cpp:90,3684`; `xbridgeexchange.cpp:712`).
- CAND: `swap/transaction.go:219,241` `IsExpired`/`IsExpiredByBlockNumber` have no non-test caller; the 60 s/240 s tickers never prune the Store.
- IMPACT: expired orders persist in Go indefinitely (hidden from dxGetOrders but still present).
- FIX: wire a periodic prune using the existing predicates; align cadence (15 s / 60 s).
- FIX (B8): `Store.PruneExpired` (open book `"created"`/`"open"` only; strict `>` time + block-height TTLs, `PrepTx` pending-partial guard, erase-without-history, deterministic ids) driven by the new 15 s `expirySweepInterval` engine ticker; `Order.BlockNumber` stamped at ingest/make from the cached BLOCK tip; persist aligned to every 60 s tick (C++ `saveOrders` every 4th 15 s tick); maker in-swap orders protected via the session `inSwap` guard (C++ advances the descriptor to trHold). Tests: `api/store_expiry_test.go` (`TestPruneExpired*`, `TestNodePruneExpired`).

### STATE-F73 · S3 · STATE · TxCancelReason enum + text table not ported
- REF: `xbridgepacket.h:21-48` (0–24) + `xbridgeapp.cpp:4052-4107` `TxCancelReasonText`, including the C++ bugs (`crBadSettings` → `"crUnknown"`, default → `"crNone"`).
- CAND: reason kept as raw uint32; no text table.
- IMPACT: cancel-reason strings/logs differ; on-wire value is a bare int on the Go side.
- FIX: port the enum + exact text table (including the C++ bugs for parity).
- FIX (B8): `api/cancel_reason.go` — full `TxCancelReason` enum (0–24) + `TxCancelReasonText` reproducing both C++ bugs byte-for-byte; `selfCancelErr`/`sendSelfCancel` retyped to the enum (wire boundary converts back to uint32); the `cancel_reason` order-log field wired at the cancel/reject sites (C++ xbridgesession.cpp:3312,3536). Tests: `TestTxCancelReasonEnumOrdinals`, `TestTxCancelReasonText`.

### STATE-F74 · S3 · STATE · `trRollbackFailed` never set
- REF: `xbridgesession.cpp:3908` — sets `trRollbackFailed=11` on refund-broadcast failure.
- CAND: `api/response.go:450-451` maps the string but `api/node.go:1917-1929` only ever sets "rolled back".
- IMPACT: rollback-failed orders report a different status.
- FIX: set the rollback-failed descriptor state on refund failure.
- FIX (B8): `postRefundTask`'s apply — on a failed refund broadcast, `rollbackGate` (session state ≥ `csCreatedA` = C++ state ≥ trCreated, with a `RefundTx` fallback for session-less orders) writes `"rollback failed"` (never clobbering a terminal/canceled order); a later successful broadcast restores `"rolled back"` (C++ trRollbackFailed → trRollback, :3911). Tests: `TestRollbackFailedStatusOnRefundBroadcastFailure`, `TestRefundFailureStateGate`.

### STATE-F75 · S3 · STATE · No peer penalty/Misbehaving analogue
- REF: `net_processing.cpp:2877,2904-2907` — Misbehaving-scores xbridge peers.
- CAND: no penalty/ban mechanism (only per-packet drop / connection error).
- IMPACT: misbehaving peers accumulate no penalty in Go.
- FIX: add a misbehavior score/ban.
- FIX (B8): per-peer misbehaviour score in `discovery.PeerManager` (+10 undersized xbridge envelope, +20 rejected addr, ban at 100 = C++ `-banscore`; disconnect + exclude from re-candidating, per-connection reset, expired bans pruned). Direct hub: the reader loop scores +10 per envelope-decode failure via the new `p2p.ErrMalformedXBridge` sentinel and drops the connection at 100; sized-but-malformed bodies are dropped unscored (C++ DoS 0). Tests: `TestPeerManagerMisbehave*`, `TestReaderLoopHub*`.

### STATE-F76 · S4 · STATE · Go live handshake uses a separate `clientState`, not the ported `swap.State`
- REF: C++ drives the live session through `Transaction::State` (`xbridgesession.cpp` process* handlers call `increaseStateCounter`).
- CAND: `api/swap.go:40-53` `clientState` drives the live handshake; `swap.State` has no non-test caller.
- IMPACT: none observable — the same transitions/guards are reproduced (verification: "internal-implementation choice, not a behavioral divergence").
- FIX: none required; note for maintainers.

---

## D. CRYPTO / FEES / UTXO AXIS

### CRYPTO-F77 · S1 · CRYPTO · BCH forkid sighash missing in Go
- REF: `xbridgewalletconnectorbch.cpp:391-406` — signs BCH refund/payment with `SigHashType(SIGHASH_ALL).withForkId()` = `0x41`, digest via the BIP143 branch of `SignatureHash` (`:191-256`), then `push_back(0x41)`. Live mainnet replay protection (`:203-209,497-499`) rewrites the fork value to `0xffdead` (median time ≥ 1605441600, permanent since the 2020-11-15 upgrade), so the digest commits hashType `0xffdead41` while the DER byte stays `0x41`. BTG uses fork value 79 (`btg.cpp:69`, digest hashType `0x4F41`, DER byte `0x41`).
- CAND: `coins/tx.go` emitted only `0x01`; the HTLC refund/claim signed via `SignTxInput` (`api/swap.go:1507,1582`).
- IMPACT: a locally-signed BCH refund/claim committed the wrong sighash and would be rejected on-chain (fund-loss risk).
- FIX (B9): parameterized `HashForSigningBIP143(idx, scriptCode, amount, hashType)` + `SignTxInputForkID`/`VerifyTxInputForkID`/`SignTxInputForCoin`; per-coin `SignatureKind`/`ForkValue` derived from `CreateTxMethod` (BCH `0xffdead`, DEVAULT `0` — replay protection disabled per `devault.cpp:171`, BTG `79`). `buildRefundTx` commits the deposit's recorded P2SH value, `redeemCounterparty` the validated deposit amount. Parity oracle transcribes the C++ forkid `SignatureHash`; `TestBCHRefundForkidSigned`, `TestForkidSignatureHashMatchesCpp`.

### CRYPTO-F78 · S2 · CRYPTO · Deposit tx fee formula differs
- REF: `xbridgesession.cpp:1993,2526` — `minTxFee1(nIn,3)` = `(192·nIn + 102)·feePerByte` (3 outputs).
- CAND: `api/swap.go:1064` — `estimateFee(nIn,2)` = `(192·nIn + 68)·feePerByte` (2 outputs).
- OBSERVED: 2-input deposit: C++ 486 fee units vs Go 452 (Go underpays the network fee by 34·feePerByte).
- IMPACT: on-chain fee shortfall; C++ counterparties may reject or the tx may be slow.
- FIX: use `(192·nIn + 102)·feePerByte`.

### CRYPTO-F79 · S3 · CRYPTO · Fee fallback: C++ 0 vs Go 2 sat/vB when FeePerByte unset
- REF: `xbridgeapp.cpp:988`, `xbridgewallet.h:113-114` — unset FeePerByte → fee 0.
- CAND: `api/handlers.go:1470-1472` — falls back to 2 sat/vB.
- IMPACT: different tx fees for configs without FeePerByte.
- FIX: no fallback (fee 0) when FeePerByte unset, or align explicitly.

### CRYPTO-F80 · S3 · CRYPTO · Go honors `DustAmount` conf key C++ never reads
- REF: C++ dust comes only from `0.546·relayFee·COIN` or `MinimumAmount`.
- CAND: `api/handlers.go:1453` reads a `DustAmount` key.
- IMPACT: a conf with DustAmount produces different dust thresholds between the two daemons.
- FIX (B10): the Go dust source moved to `MinimumAmount` (C++ maps it onto the exchange wallets' `dustAmount`, xbridgeexchange.cpp:145); `DustAmount` is parsed-but-unread (createConf-template key), matching C++; `TestEffectiveDust`.

### CRYPTO-F81 · S3 · CRYPTO · Address decoding strictness differences
- REF: C++ is lenient — `toXAddr` erases the cashaddr prefix unconditionally and accepts hash lengths up to 64 bytes; base58check prefix mismatches tolerated.
- CAND: `coins/cashaddr.go` rejects prefix-less/legacy BCH cashaddr; `coins/base58check.go` rejects non-matching version bytes; only 20/24/28/32-byte hashes.
- IMPACT: addresses C++ accepts are rejected by Go (and vice versa).
- FIX: align decoding leniency to C++ (or document the stricter policy).

### CRYPTO-F82 · S4 · CRYPTO · RNG top-bit bias in Go private-key generation
- REF: C++ uses full-range random with secp retry.
- CAND: `crypto/signer.go` `NewPrivateKey` cleared the top bit.
- IMPACT: 1-bit entropy reduction; no interop impact (keys are local).
- FIX (B9): full-range 256-bit with retry into [1, N-1]; `TestNewPrivateKeyFullRange`.

### CRYPTO-F83 · S3 · CRYPTO · Block-hash byte order assumption unverified end-to-end
- REF/CAND: both sides assume internal-LE for the block hash in the packet body; not verified against a live getblockhash (display order) source.
- IMPACT: potential body divergence if a wallet RPC ever returns display-order hashes.
- FIX (B9): `wallet.revHashHex` pinned against a captured real block hash (Bitcoin genesis) — the internal bytes equal `base_blob<256>::SetHex` (uint256.cpp:27-53, display reversed); `TestRevHashHexCapturedBlockHash` + parity `TestBlockHashByteOrderMatchesCpp` (oracle transcription of SetHex).

---

## E. CONFIG AXIS

### CFG-F84 · S2 · CONFIG · `[Rpc]` section in xbridge.conf aborts xbridged at startup
- REF: `util/settings.h:49-65` — `Rpc.*` keys defined but never read (dead section).
- CAND: `config/conf.go:111-118` — every non-Main section is parsed as a coin; a stock config with `[Rpc]` → `"COIN not set"` fatal (`coins/coin.go:87-89`, `cmd/xbridged/main.go:163-165`).
- IMPACT: a config C++ runs fine with kills go-xbridge at startup.
- FIX (B10): whitelist `[Main]`/`[Rpc]` in `config.Load` (exact-case); `TestLoadSkipsRpcSection`.

### CFG-F85 · S2 · CONFIG · Wallet admission validation gates absent in Go
- REF: `xbridgeapp.cpp:1002-1040` — drops wallets failing maker/taker locktime targets, confirmation drift, reachability; BlockTime==0 gate at `:1002-1006`.
- CAND: `wallet/conf.go:15-17`, `coins/coin.go:87-89` — only Ip/Port/COIN presence.
- IMPACT: coins C++ refuses to load trade in Go (potentially unsafe locktimes).
- FIX (B10): new `config.Admit` (connect check, maker/taker targets incl. slow chains, confirmation drift `max(900/blockTime,4)`, `CreateTxMethod` dispatch) + `config.Admitted`, applied at startup/reload/sweep; constants single-sourced in `config` (`xbridgewallet.h:96-102`); `TestAdmitGates`, `TestAdmitCreateTxMethod`, `TestAdmittedFilters`.

### CFG-F86 · S2 · CONFIG · Missing conf: C++ creates template and runs; Go exits(1)
- REF: `init.cpp:1920`, `settings.cpp:75-79` — creates the default xbridge.conf and continues.
- CAND: `config/conf.go:98-103`, `cmd/xbridged/main.go:159-162` — exits(1).
- IMPACT: first-run behavior differs; a dApp's unattended Go daemon fails to start.
- FIX (B10): **DOCUMENTED** — the never-creates hard rule stands (library AND daemon require an existing conf); C++ `createConf` template at `xbridgeapp.cpp:306-358` is not ported (`B10-config.md`).

### CFG-F87 · S2 · CONFIG · Hot-reload semantics differ
- REF: reload re-applies admission gates, drops wallets leaving ExchangeWallets, clears non-local orders, keeps prior state on failure (`rpcxbridge.cpp:229-233`; `xbridgeapp.cpp:931-948,3811-3826`).
- CAND: `api/node.go:287-334` — atomic last-good on failure; keys connectors off ALL `[TICKER]` sections (never ExchangeWallets); no gates; never clears non-local orders.
- IMPACT: post-reload state differs (wallet set, gates, stale orders).
- FIX (B10): `wallet.Activator` connects exactly `[Main].ExchangeWallets` ∩ gates ∩ reachability probe (`updateActiveWallets` `xbridgeapp.cpp:917-1214`, 300 s bad-wallet retry); reload preserves `ForceShowAllOrders`/`CheckReachability` and clears non-local orders via `Store.PruneUnconnected` unless ShowAllOrders (`clearNonLocalOrders`, `rpcxbridge.cpp:229-233`); 30 s sweep re-probes in-memory settings (`:3674-3677`), guarded against clobbering a reload; `TestActivateExchangeWalletsOnly`, `TestActivateProbe`, `TestActivateBadWalletRetry`, `TestReloadAppliesEWKeying`, `TestReloadPrunesUnconnectedOrders`, `TestSweepConnectors`, `TestPruneUnconnected`.

### CFG-F88 · S3 · CONFIG · ExchangeWallets parsing differs
- REF: `util/settings.cpp:143-166` — splits on `,` `;` `:` with symbol validation/uppercase.
- CAND: `config/conf.go:232-249` — splits on `,` only, no validation.
- IMPACT: wallets list parsed differently for `;`-separated configs.
- FIX (B10): split on `,;:` + `ccy::Symbol::validate` (uppercase, len 1..8, no trim, `currency.h:29-47`); `TestExchangeWalletsCppSemantics`.

### CFG-F89 · S3 · CONFIG · Case-insensitive keys in Go vs case-sensitive C++
- REF: boost property_tree is case-sensitive (`COIN` ≠ `coin`).
- CAND: `config/conf.go:112,169` — case-insensitive lookups.
- IMPACT: configs with wrong-cased keys are silently accepted by Go.
- FIX (B10): exact-case key + `[Main]` lookups; `TestCaseSensitiveKeys`.

### CFG-F90 · S3 · CONFIG · Missing CLI flags / flag differences
- REF: `-enableexchange`, `-dxnowallets` (`init.cpp:572`; `xbridgeapp.cpp:372`).
- CAND: no equivalents; ShowAllOrders only via programmatic `api.Config` (`api/node.go:78-81`); `-walletversionstr` default lowercase `"/blocknet:4.4.1/"` vs C++ `"/Blocknet:4.4.1/"`.
- IMPACT: operator can't toggle exchange mode / show-all via CLI; version string differs.
- FIX (B10): `-dxnowallets` (ShowAllOrders override, kept across reload as `Config.ForceShowAllOrders`) + `-enableexchange` (inert compat no-op); daemon honors `Main.ShowAllOrders` at startup (pre-fix only after a reload). The `-walletversionstr` case sub-item was already fixed in `a0fee1e` (B7).

### CFG-F91 · S3 · CONFIG · Go-only conf keys and ignored C++ keys
- REF: C++ never reads `DustAmount`, `GetNewKeySupported`, `ImportWithNoScanSupported`, `OmitJSONVersion`; C++ reads `MinimumAmount` (`xbridgeexchange.cpp:124,145`) and `CashAddrPrefix`; C++ `AddressPrefix` is a string (`xbridgewalletconnectorbtc.cpp:1516-1518`) vs Go int; C++ `Title` defaults to `""` on a missing key (`Settings::get` `_T()`, `settings.h:75-84`) — NOT the section name (this card previously had the direction inverted); C++ rejects ETH/unknown `CreateTxMethod` (`xbridgeapp.cpp:1043-1090`) while Go silently maps to BTC (`coins/coin.go:107-113`); Go ignores the `CashAddrPrefix` conf value (`coins/coin.go:99,119-126`).
- CAND: Go reads `DustAmount` + `OmitJSONVersion` (used), `GetNewKeySupported`/`ImportWithNoScanSupported` (inert); ignores `MinimumAmount` and the `CashAddrPrefix` value.
- IMPACT: config surface accepts keys the reference never reads and drops keys the reference uses; behavior on ETH/unknown method diverges.
- FIX (B10): `Title` default `""`; `effectiveDust`/`isDustNative` use `MinimumAmount` (folded `CRYPTO-F80`; `DustAmount` parsed-but-unread, template-only); `coins.FromConf` honors the `CashAddrPrefix` conf value with C++'s fallbacks (`bch.cpp:306-308`/`devault.cpp:278-280`); `config.Admit` rejects ETH/unknown `CreateTxMethod`; `TestTitleDefaultsEmpty`, `TestEffectiveDust`, `TestFromConfCashAddrPrefix`, `TestAdmitCreateTxMethod`.

---

## F. CONCURRENCY AXIS

### CONC-F92 · S2 · CONC · Engine goroutine can block on socket write / fsync, stalling all packet processing and RPC
- REF: C++ `PushMessage` and `saveOrders` never block the packet-processing path.
- CAND: `api/node.go:802-814` → `p2p/conn.go:140-145` (synchronous socket write) and `persist.go:154-176` (fsync) run on the engine goroutine — a slow peer or slow disk stalls every packet and every awaiting RPC.
- IMPACT: head-of-line blocking / perceived daemon stall.
- FIX (B11): `p2p.Conn` grows an outbound queue drained by a writer goroutine (lazy-started, so read-only/handshake-failed conns spawn nothing; `maxSendBufferSize` 1 MB cap disconnects the peer like C++ `PushMessage`/`nSendBufferMaxSize`, net.cpp:2705-2730 — frames are never dropped, avoiding the CreatedA-retransmit double-deposit hazard; write errors surface on the next send + tear the conn down in both directions). Persist splits into `snapshotSwaps` (engine) + `writeSwaps` (background `persistLoop`, coalesced latest-slot, final flush after `Close` joins). Tests: `p2p/conn_write_test.go` + `api/slow_peer_test.go` + `api/persist_async_test.go`.

### CONC-F93 · S3 · CONC · Discovery peer goroutines never joined; Dedupe sweeper leaks without Flush
- REF: C++ joins all threads on shutdown (`xbridgeapp.cpp:530-544`).
- CAND: `p2p/discovery/peer_manager.go:128,201,270,433-448` — signalled via `m.done` but never joined (no WaitGroup); `log/dedup.go:137-148` sweeper stops only on Flush (mitigated by `main.go:263` FlushAll on graceful shutdown).
- IMPACT: goroutine leaks on library-level Close; less severe under the daemon.
- FIX (B11): PeerManager tracks maintain/connectOne/readLoop with a `WaitGroup` joined by `Close`, drives an internal lifecycle context so in-flight dials abort immediately (`p2p.DialContext`, `net.Dialer.DialContext`; the version handshake aborts on ctx cancellation via `NewConnCtx`, so a handshake-stalling peer cannot hold the join); `Dedupe.startSweepLocked` re-registers so a restarted sweeper is stopped by a later `FlushAll`; `Node.Close` calls `xlog.FlushAll()`. Tests: `TestPeerManagerCloseAbortsInflightDialAndJoins`, `TestPeerManagerCloseFastWithStalledHandshake`, `TestDedupe_RestartReRegisters`, `TestNodeCloseFlushesDedupeSweepers`.

### CONC-F94 · S3 · CONC · Conf reload mid-swap-task hazard (untested)
- REF: C++ wallet list is stable during a session task.
- CAND: `reloadConf` (`api/node.go:287-334`) can swap the connector a deposit task builds against mid-task; not covered by `-race` tests.
- IMPACT: torn connector state across a reload boundary.
- FIX (B11): `swapCtx` snapshots `Connectors`/`Confs` at enqueue (shallow copies under `cfg()`) and the session's two currencies from the coin registry under one atomic load (`coins.Snapshot`, `swapCtx.coin`); all worker-path connector/confs/coin reads go through the snapshot, and `postRefundTask` captures the connector at enqueue — a mid-task reload can no longer redirect a deposit/claim/refund build or change the coin parameters (decimals/prefix/codec) it is built with (C++ session holds the captured connector pointer and coin params; reload replaces the pool under `m_connectorsLock`). `LocalConnector` binds `TxWithTimeField` at construction. Tests: `TestReloadMidSwapTaskKeepsConnectorSnapshot` (real `reloadConf` mid-task; fails when `buildDeposit` is reverted to live-config reads), `TestReloadMidSwapTaskKeepsCoinSnapshot` (reload changes BTC P2PKH/P2SH mid-task; fails when the worker re-reads the live registry), `coins.TestSnapshot`, `TestLocalConnectorSignAfterRegistryFlip`.

### CONC-F95 · S3 · CONC · Go hides C++'s transient "accepting" window
- REF: `xbridgeapp.cpp:2126-2135` — a transient `trAccepting` + modified-sizes state is observable during an in-flight take.
- CAND: `api/node.go:1637-1671` — snapshot reads never expose it (field-consistent reads).
- IMPACT: dApps polling during a take observe different transient states.
- FIX: document; optionally mirror the accepting window.

### CONC-F96 · S4 · CONC · Go runs swap wallet I/O and RPC concurrently where C++ serializes
- REF: `net.cpp:1949` (single msghand thread) + `http` thread; blocking wallet RPC inline.
- CAND: `api/engine.go:15,227-250` (4 wallet workers) + per-request RPC goroutines.
- IMPACT: side-effect interleaving may differ under load (C++ stalls the whole feed on a slow wallet; Go only that swap). Not a conformance bug per se.
- FIX: document the model; ensure per-order serialization of state mutations.

---

## G. INVENTORY / DOC AXIS

### INV-F97 · S4 · INVENTORY · Unported C++ internal helpers (not part of the dApp-facing surface)
- REF: `xbridgerpc.cpp` wallet-RPC client methods (`eth_gasPrice/eth_accounts/eth_getBalance/eth_sendTransaction`, `requestAddressBook`, `addMultisigAddress`, `dumpPrivKey/importPrivKey`, `getNewPubKey`, `getTransaction`), `xutil` base64, `XSeries/XSeriesCache`, `xuiconnector`, `txlog`, `fastdelegate`, per-coin connector subclasses (bch/bcd/btg/dgb/part/devault/stealth), `Session::getAddressBook`.
- CAND: no equivalents (see INVENTORY.md GAP list, 16 gaps).
- IMPACT: none for the dx* RPC / wire contract, but any future surface expansion or wallet-RPC feature will be missing.
- FIX: document as known gaps; port on demand.

### INV-F98 · S4 · DOC · `docs/protocol.md` says order `Created` is unix seconds; the wire carries microseconds
- REF: `total_microseconds()` (`xutil.cpp:280`) — µs in the wire body and envelope timestamp.
- CAND: `docs/protocol.md:395` (§4.2 notes) — said seconds; Go code correctly passes through µs (`store.go:517-519`).
- IMPACT: documentation-only; a reader would build a wrong encoder.
- FIX (B11): protocol.md §4.2 note corrected — order `Created` is µs since epoch (the 8-byte envelope timestamp is also µs; only the packet-header timestamp is seconds, §2.1).

### INV-F99 · S4 · DOC · Stale C++ header-comment enums
- REF: header comments for commands 11/12/13/18/20/24 and locktime threshold prose in `xbridgepacket.h` / `xbridgesession.h` are stale; the writers are authoritative.
- CAND: correctly follows the writers (`body_types.go:16-33`).
- IMPACT: none behavioral.
 - FIX: update the C++ header comments.

---

## H. PROMOTED FINDINGS (folded from prior audits)

These findings predate the 2026 audit or were only covered by it indirectly;
each is now a first-class Current-register ID. The register's ID-history
appendix maps every old ID to its canonical home.

### RPC-F57 · S2 · dxGetOrderBook detail-4 nesting
- C++ emits the order-book detail-4 field as `[[…]]`-nested; Go flattens it.
- FIX: match the nesting at B7.

### RPC-F58 · S2 · HTTP auth/timeout hardening
- Go `http.Server` lacks read/write/idle timeouts and an explicit auth gate.
- FIX: add at B4.

### RPC-F59 · S3 · dxGetMyPartialOrderChain unknown/malformed id
- Unknown id → `[]`, malformed id → `bad order id` (`api/handlers.go`). FIXED.

### WIRE-F71 · S2 · Servicenode registration integrity
- Registration fields read-then-discarded; gates miss the `isValid` subset.
  FIXED — B1 `fix/servicenode-registry`.

### STATE-F77 · S2 · Post-completion retransmit re-broadcast
- Retransmitted CreateA/B/ConfirmA/B after completion re-broadcast deposit/claim.
  FIXED by state guards (`swap_guard_test.go`).

### STATE-F78 · S2 · Hub-key pinning, no TOFU
- Every inbound handshake packet re-verified against the pinned hub key +
  registry membership; forged `Finished` dropped before `OnFinished`. FIXED.

### STATE-F79 · S3 · tryJoinMatches partial-order min-size guards
- Partial-order minimum-size guards unconfirmed against C++. FIXED (B8): `Transaction::tryJoin` (xbridgetransaction.cpp:527 `other->m_destAmount < m_minPartialAmount`, strict `<`) is confirmed 1:1 with the Go `tryJoinMatches` (`swap/transaction.go:106`), including the drift → bounds → min-guard ordering; `TestTryJoinPartialMinSizeGuard` isolates the guard with a 2:1-price fixture (exactly-the-minimum joins, one below rejected).

### CRYPTO-F84 · S2 · AcceptingBody empty fee/utxos
- `TakeOrder` emitted `AcceptingBody` with empty fee/utxos (156 B < 188 B).
  FIXED — B2 `fix/wire-acceptingbody`.

### CRYPTO-F85 · S2 · No checkDepositTransaction in the Connector contract
- C++ `Connector::checkDepositTransaction` has no Go analogue. FIXED (B3): `wallet.CheckDepositTransaction` (interface + RPCConnector 1:1 port of `xbridgewalletconnectorbtc.cpp:1981-2194` + LocalConnector `ErrNoChainSource`) wired into `OnCreateB`/`OnConfirmA` with tri-state (wait→no reply / bad→Cancel / good→record); `wallet/rpc_test.go` goldens + `TestCreateBBadDepositCancels`/`TestCreateBWaitsOnNotReadyDeposit`.

### CRYPTO-F86 · S2 · buildDeposit broadcasts before the refund is built
- Go broadcasts the deposit before constructing the refund; C++ builds first.
  FIXED (B3): sign → local txid → refund → broadcast; `TestDepositNotBroadcastWhenRefundFails`.

### CRYPTO-F87 · S2 · Deposit re-runs ListUnspent instead of usedCoins
- C++ spends the maker's `xtx->usedCoins`; Go re-lists unspent. FIXED (B3): `Order.UsedCoins` recorded at make/take, `swapCtx.funding` snapshot consumed by `buildDeposit`; `TestDepositSpendsUsedCoins`.

### CRYPTO-F88 · S3 · Segwit/BIP143 signing dead code
- Go `coins/tx.go` carried unused segwit/BIP143 signing paths. The "bech32
  re-encoded legacy" sub-claim is unsubstantiated: the only bech32 codec usage
  is the documented per-coin segwit address path (`coins/address.go`), which
  tries base58check first and accepts bech32 only when the HRP matches the
  coin. FIXED (B9): the BIP143 digest is now LIVE — it is the base of the
  forkid signing path (`HashForSigningBIP143`).

### CRYPTO-F89 · S3 · Coin-family misclassification
- DEVAULT/DCR/PART/BTG connectors missing or misfiled as BTC family.
  FIXED (B9, partial): BTG classified forkid-79 + bech32 "btg"
  (`TestBTGAddressRoundTrip`); DEVAULT classified BCH-family, cashaddr
  "devault", fork value 0 (`TestDevaultAddressCashaddr`). Residuals split into
  new rows: DCR is not in the live manifest (no `[DCR]` in
  blockchain-configuration-files), PART → `CRYPTO-F98`, BCD → `CRYPTO-F99`.

### CRYPTO-F90 · S3 · Refund/payment payout model
- C++ fee2 margin / `oOverpayment` handling not ported. FIXED (B3).

### CRYPTO-F91 · S3 · signrawtransaction param payload
- The pre-fix Go sent `"ALL"` (a sighash string) in the privkeys slot; C++
  sends `null` there (`[rawtx, prevtxs|null, keys|null]`,
  xbridgewalletconnectorbtc.cpp:1055-1089). FIXED (B9): payload aligned;
  `TestSignRawTransactionPayloadMatchesCpp`.

### CRYPTO-F92 · S3 · secret-from-payTx scans only input 0
- Go `secretFromPayTx` read only `Inputs[0]`; C++
  `getSecretFromPaymentTransaction` scans every vin's scriptSig for a push
  whose getKeyId equals the secret hash (btc.cpp:2241-2276). FIXED (B9):
  all inputs scanned; `TestSecretFromPayTxScansAllInputs`.

### CRYPTO-F93 · S3 · Ownership-proof challenge stream format
- `UtxoEntry::toString()` golden-vector parity
  (`TestWholeCoinOstreamMatchesCppStream`). FIXED.

### CRYPTO-F94 · S3 · Deposit SEQUENCE_FINAL
- Deposit inputs final; refund spend `SEQUENCE_FINAL-1` per C++
  `checkDepositTransaction`. FIXED.

### CRYPTO-F95 · S3 · Deposit locks Amount + fee2
- P2SH locks `Amount + fee2` (`minTxFee2(1,1)`); change = total − Amount − fee
  − fee2. FIXED (`TestDepositLocksAmountPlusFee2`).

### CRYPTO-F96 · S3 · nTime committed in sighash
- 4-byte LE `TxTime` after `nVersion` in `HashForSigning` on time-field coins.
  FIXED (`TestHashForSigningWithTimeField`).

### CRYPTO-F98 · S3 · PART (Particl) connector non-portable
- `XParticlTransaction` serialization (version marker 0xA0), confidential
  outputs (`vpout` vector-of-pointers) and the amount-committing digest
  (`xbridgewalletconnectorpart.cpp:113-195`) cannot be faithfully reproduced
  by `coins.Tx`; a thin client broadcasting a PART deposit/refund in BTC format
  would put malformed bytes on-chain. DOCUMENTED (B9): deferred, non-portable
  tier (`B9-crypto.md`); `[PART]` is one conf in the live manifest
  (particl--v0.19.2.5.conf).

### CRYPTO-F99 · S3 · BCD (Bitcoin Diamond) connector non-portable
- `BCDTransaction` serialization writes an extra `preBlockHash` (uint256) field
  when `nVersion == CURRENT_VERSION_FORK` (`xbridgewalletconnectorbcd.cpp:106-108`),
  and its `SignatureHash` mirrors that; `coins.Tx` has no conditional-serialize
  flag. DOCUMENTED
  (B9): deferred, non-portable tier (`B9-crypto.md`); `[BCD]` is one conf in the
  live manifest (bitcoindiamond--v1.3.0.conf).

### CONC-F97 · S2 · SwapSession fields single-owner
- Engine-only ownership; race-covered. FIXED.

### CONC-F98 · S2 · Coin registry atomic reload
- `atomic.Pointer` coin registry hot-reload safe. FIXED.

### CONC-F99 · S2 · Unbounded growth bounded
- `pruneSessions` + `trimOldest` (1000 each), fills/history caps. FIXED.

### CONC-F100 · S2 · No wallet I/O under lock
- Book/session maps never held across wallet I/O. FIXED.

### CONC-F101 · S2 · dxMakeOrder returned the live *Order
- Response rendered from the store's live record (data race). FIXED
  (`TestMakeOrderReturnsStoreCopy`).

### CONC-F102 · S2 · Force-refund double-broadcast window
- Force-refund takes the `pendingRefunds` sweep guard. FIXED
  (`TestForceRefundTakesSweepGuard`).

### INV-F100 · S4 · Vestigial helpers
- `Server.verify`, `coins.MustGet`, unreferenced `swap`, LocalConnector
  sign/verify. FIXED (documented).

### SEC-F01 · S2 · RPC loopback bind
- RPC binds loopback by default; Basic auth when both creds set. FIXED.

### SEC-F02 · S3 · Inbound UTXO ownership proofs unverified
- Order UTXO proofs never verified before booking. FIXED (B8): `wallet.Connector.GetTxOut` (gettxout) + `verifyAndBook`/`verifyOrderUtxos` verify each maker UTXO before booking, mirroring the C++ snode gate (`processTransaction`, xbridgesession.cpp:535-577: getTxOut existence + BIP137 verifyMessage against the chain amount, per-entry skip, reject when no survivor covers fromAmount); wallet I/O offloaded so the engine never blocks. cmd-4 broadcasts carry no UTXO entries on the trader wire (C++ `processPendingTransaction` parses none), so the gate fires for any utxo-bearing order body. Tests: `api/verify_inbound_test.go`.

### SEC-F03 · S2 · Taker-trust composite
- HTLC ELSE branch + CreateB-derived taker deposit; composition sound, no
  standalone code. OPEN (closed by B3).

### SEC-F04 · S2 · Plaintext secrets + debug-log leakage
- Secrets logged in plaintext. FIXED (B6): refund/claim hex + RPC bodies dropped from logs; corrupt swap file logs at Error like C++ `loadOrders`. Tests: `TestCorruptSwapFileContinuesLikeCpp`. (The `-persistsecrets` opt-out from the original B6 was removed 2026-09-10 — secrets now always persist; see `TestPersistSecretsAlwaysOnDisk`.)

---

## Severity roll-up

| Severity | Count | Notes |
|---|---|---|
| S1 Critical | 1 | CRYPTO-F77 BCH forkid sighash (on-chain failure) |
| S2 High | 61 | 43 audit + 18 promoted |
| S3 Medium | 61 | 49 audit + 12 promoted |
| S4 Low | 7 | 6 audit + 1 promoted (INV-F100) |

Total: **130 findings** (99 from the 2026 audit + 31 promoted from prior audits;
all CONFIRMED or PARTIAL, zero TBD).

## Cross-cutting remediation priorities
1. **RPC-F01 error-channel policy** — decide throw-vs-result once; it colors ~10 methods.
2. **RPC-F02/RPC-F43/RPC-F12/RPC-F15 error `name`/text corrections** — one pass over the error builder.
3. **JSON key/field ORDER (RPC-F03, RPC-F09, RPC-F24, RPC-F30, RPC-F36)** — switch response builders to ordered emission or reorder structs; byte-level parity is the stated goal.
4. **Wire hardening (WIRE-F57–F60, WIRE-F62–F64)** — align caps, magic/version gates, checksum handling.
5. **BCH sighash (CRYPTO-F77)** and **fee formulas (CRYPTO-F78, RPC-F41)** — on-chain correctness first.
6. **Startup/survival (CFG-F84, CFG-F86)** — a stock xbridge.conf must not kill xbridged.

## Evidence
- RPC per-method cards with vectors: `evidence/rpc.md` + `evidence/rpc-groups/`
- Wire cards with hex vectors: `evidence/wire.md`, `evidence/wire_p1.md`, `evidence/wire_p2.md`
- State/lifecycle: `evidence/state.md`
- Crypto/fees/utxo: `evidence/crypto.md`
- Concurrency: `evidence/concurrency.md`
- Config: `evidence/config.md`
- Symbol inventory + gaps: `evidence/inventory.md`
- Double-check reports: `verify/verify_a.md` … `verify/verify_e.md`
- Regression vectors: `conformance/` (build-tagged Go suite)
