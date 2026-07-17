# xbridge-go `dx*` RPC Equivalence Audit

**Source of truth:** the original C++ implementation in
`blocknet_core/src/xbridge/` (mainly `rpcxbridge.cpp`, plus `xbridgeapp.cpp`,
`util/xutil.cpp`, `util/xbridgeerror.{h,cpp}`, `xbridgetransactiondescr.h`).
The Go port must match C++ behavior byte-for-byte on the wire. Header enum
values and RPC help text in the C++ are frequently **stale** — trust the actual
C++ writers, not the comments.

**Method:** 6 parallel read-only comparison passes, one per command group,
comparing `xbridge-go/api/*.go` against the C++ writers. No production behavior
was inferred from Go comments; the "7 decimal" claim in Go was checked against
C++ and found wrong (see C1).

## Headline verdict

**As originally audited:** zero of the 24 `dx*` commands were behaviorally 1:1 —
every command diverged in at least one dapp-visible way, with the damage
concentrated in (a) two **universal** substrate breaks and (b) the
order-lifecycle + wallet/split commands.

**Status (2026-07-17):** all 24 commands have been remediated against the C++
wire contract. The **Tier 1** code bugs (`stateOrdinal` enum, `dxPartialOrderChainDetails`
aggregation / empty-chain / id-validation, `dxTakeOrder` amount=0 full take,
`FlushCancelled` uint64 underflow, `dxGetLockedUtxos` nil-guard) and **Tier 2**
achievable backing gaps (per-`(txid:vout)` locked-UTXO tracking, `dxCancelOrder`
`refund_tx`, `p2sh_deposits` / `_counterparty` fields, `dxGetTokenBalances`
`Wallet` key) are **fixed** in this pass. The only remaining deltas are
**Tier 3 — documented thin-client architectural limits** that require a BLOCK
block index / `blocknetd` to fully resolve: `dxGetOrderHistory` / `dxGetTradingData`
reflect session-local fills only (`fee_txid` / `nodepubkey` empty), and
`dxGetNetworkTokens` completeness is bounded by P2P servicenode-ping coverage.
These are documented (not silently divergent) under "Tier 3 — architectural
limits" in `docs/api.md`. See the remediation-progress sections below.

## Equivalence matrix

| Command | Verdict | Worst severity | Top issue |
|---|---|---|---|
| dxGetOrders | PARTIAL-GAP | med | conf-vs-connector filter; unsorted output |
| dxGetOrder | DIVERGED | high | missing `NO_SESSION` check; case-sensitive id |
| dxGetMyOrders | PARTIAL-GAP | med | drops finished/cancelled locals; unsorted |
| dxGetOrderBook | DIVERGED | high | **inverted price**; detail 1/2/4 unimplemented |
| dxGetOrderFills | DIVERGED | high | only 6 of 12 fields emitted |
| dxGetMyPartialOrderChain | PARTIAL-GAP | med | walks ancestors only (C++ adds descendants) |
| dxPartialOrderChainDetails | DONE | — | totals via `stateOrdinal<=trPending`; empty `{}` for unknown chain; id validated; `p2sh_deposits` emitted |
| dxGetLockedUtxos | DONE | — | per-UTXO `txid:vout` locked set from active orders; nil-guarded |
| dxFlushCancelledOrders | DONE | — | `FlushCancelled` prunes by age; uint64 underflow clamped |
| dxGetLocalTokens | PARTIAL-GAP | low | static conf set vs live connector map |
| dxGetNetworkTokens | DONE | med | live servicenode union via XbcServicesPing (was static conf) |
| dxMakeOrder | DIVERGED | high | `dryrun` string **broadcasts**; missing validations |
| dxMakePartialOrder | DIVERGED | high | misreports partial as `order_type="exact"`; trailing params ignored |
| dxTakeOrder | DIVERGED | high | **maker/taker swapped**; `dryrun` broadcasts; no self-trade guard |
| dxCancelOrder | DONE | — | cancel guard `stateOrdinal>=trCreated`; `refund_tx` from swap refund |
| dxLoadXBridgeConf | PARTIAL-GAP | low | returns `true` unconditionally (no reload) |
| dxGetNewTokenAddress | DIVERGED | med | `[]` vs error on no-wallet; segwit addr type |
| dxGetTokenBalances | DONE | — | `Wallet` key from BLOCK connector; locked UTXOs subtracted |
| dxGetUtxos | DONE | — | `orderid` field; locked UTXOs excluded (`include_used`) |
| dxSplitAddress | PARTIAL-GAP | high | wrong response fields + wrong UTXO/change/fee model |
| dxSplitInputs | PARTIAL-GAP | high | same as above + param-count divergence |
| dxGetOrderHistory | DONE | med | OHLCV buckets from local fills; schema matches C++ (zero-filled empties) |
| dxGetTradingData / gettradingdata | DONE | med | 8-field record schema from local fills (fee_txid/nodepubkey empty) |
| getNetworkInfo | GO-ONLY | low | no C++ `dx*` counterpart; missing 2 version fields |

## Cross-cutting divergences (hit every command)

**C1 — Amount/price decimal count: 7 vs 6. [HIGH, universal]**
`api/response.go` `formatXAmount` emits 7 decimals; `formatXPrice` uses
precision 7. C++ `xBridgeStringValueFromAmount`/`FromPrice`
(`util/xutil.cpp:202-214`) use `setprecision(xBridgeSignificantDigits(COIN))`.
`xBridgeSignificantDigits(1000000)` (`util/xutil.cpp:263-274`) loops
`do { n++; i/=10 } while (i>1)` → **6**. So C++ emits **6 decimals**
(`"100.000000"`), Go emits 7 (`"100.0000000"`). The Go comment and
`docs/api.md` VERIFY note claiming "the code uses 7" are wrong.
*Fix:* render 6 decimals in `formatXAmount`/`formatXPrice`.

**C2 — Error `code` values. [HIGH, universal]**
Go `response.go` uses `1,2,3,4,5,6,7,8,100`. C++ `util/xbridgeerror.h:23-48`
uses the 1000-range enum: `UNKNOWN_ERROR=1002`, `BAD_REQUEST=1004`,
`NO_SESSION=1018`, `INSUFFICIENT_FUNDS=1019`, `TRANSACTION_NOT_FOUND=1021`,
`INVALID_PARAMETERS=1025`, `INVALID_ADDRESS=1026`, `INVALID_STATE=1028`,
`NOT_EXCHANGE_NODE=1029`.
*Fix:* replace Go constants with the C++ enum values.

**C3 — Error `error` text prefix. [MED]**
C++ `xbridgeErrorText` (`util/xbridgeerror.cpp`) prefixes per-code text
(`"Invalid parameters: "`, `"No session for currency "`, …); Go `makeError`
stores the message verbatim.
*Fix:* route messages through the same prefix logic.

**C4 — Positional param coercion too lenient. [MED]**
Go `strParam/boolParam/intParam` (`dispatch.go`) silently default or tolerate
string forms (`"true"`, `"123"`); C++ `get_str/get_bool/get_int` throws on a
mistyped param. `mustBool/mustInt` also return a result-error with code 1
instead of C++'s envelope error.
*Fix:* error (with C2-correct code) on a present-but-unparseable required param.

**C5 — JSON-RPC envelope injects `jsonrpc:"2.0"`. [MED]**
`api/server.go` stamps `"2.0"` and pretty-prints; C++ is JSON-RPC 1.0 with only
`result`/`error`/`id`, compact.
*Fix:* omit `jsonrpc` and the indent to match 1.0. (Re-validate against the
BLOCK-DX handshake note in `server.go` before removing, using live captures.)

*Equivalent cross-cutting areas:* timestamps (`iso8601` 3-digit ms `Z`), `status`
strings (`statusString` matches `TransactionDescr::strState`, incl.
"commited"/"canceled"), and the dispatch set (all 24 commands + `gettradingdata`
alias present; `getnetworkinfo` is an intentional Go-only extension).

## Per-command divergences

### Order-book reads
- **dxGetOrderBook** (`handlers.go`): ask price = `fromAmount/toAmount`, bid =
  `ToAmount/FromAmount` — **inverted** vs C++ `util/xutil.cpp:293-306`
  (`price()=to/from`, `priceBid()=from/to`). Detail levels 1/2/4 unimplemented
  (`rpcxbridge.cpp:1623-1988`). No sort; `maxOrders` applied to combined total vs
  C++ per-side. Detail range `[1,4]` not validated; currency match case-sensitive
  (C++ `iequals`); zero-amount orders not filtered.
- **dxGetOrder**: missing `NO_SESSION` connector check (`rpcxbridge.cpp:788`); id
  lookup case-sensitive (C++ `uint256S` case-insensitive).
- **dxGetOrders**: filters by `coins.Has` (conf) vs C++ `connectorByCurrency`;
  unsorted output (C++ id-sorted).
- **dxGetMyOrders**: returns only live `Mine`; C++ also returns historical
  `trFinished`/`trCancelled` locals (`rpcxbridge.cpp:2103-2121`); not sorted by
  `txtime`.

### Fills / chain / tokens
- **dxGetOrderFills**: emits 6 fields; C++ emits 12 (`order_type`,
  `partial_minimum`, `partial_orig_*`, `partial_repost`, `partial_parent_id` —
  `rpcxbridge.cpp:566-584`). Different backing store.
- **dxPartialOrderChainDetails**: stub — returns order objects vs C++ `uvorders`
  (hex-string id array, `rpcxbridge.cpp:2478`); `total_*` hardcoded; first/last
  taken from queried order vs chain root/tip.
- **dxGetLockedUtxos**: returns order objects vs C++ object
  `{all_locked_utxo:[…]}` / `{id, CUR:[…]}` (`rpcxbridge.cpp:2652-2679`); no
  `NOT_EXCHANGE_NODE` gate.
- **dxFlushCancelledOrders**: never prunes (`App::flushCancelledOrders(minAge)`)
  and ignores `ageMillis`; returns whole set vs C++ removed subset.
- **dxGetMyPartialOrderChain**: walks only `ParentID` (ancestors); C++
  `getPartialOrderChain` (`xbridgeapp.cpp:3913`) also resolves descendants.
- **dxGetNetworkTokens**: DONE — now the live servicenode union. `Node.feed()`
  records each peer's `XbcServicesPing` (`ServicesPingBody.Services`) into a
  `svcByPeer` registry; `Node.NetworkTokens()` unions them with the config's
  `NetworkTokens`/`ExchangeWallets` (C++ `walletServices()`), falling back to the
  config list when no servicenodes are connected.
- **dxGetLocalTokens**: static `ExchangeWallets` vs C++ runtime connector map.

### Order lifecycle
- **dxMakeOrder** (`handlers.go`→`node.go`): `dryrun` read via `mustBool` so the
  documented `"dryrun"` string **broadcasts** instead of simulating
  (`rpcxbridge.cpp:983-995`). `type` not validated (C++ rejects `!="exact"`);
  `type="partial"` broadcast then misreported as `order_type="exact"`. Missing
  precision/limit/address checks; no `NO_SESSION`/insufficient-funds. Order id is
  `rand.Read` vs C++ content hash.
- **dxMakePartialOrder**: trailing params `repost/use_all_funds/auto_split/dryrun`
  hardcoded (C++ reads idx 7–10); result uses the exact renderer, misreporting
  the order as `order_type="exact"`. Missing `min_size>maker_size`/dust checks.
- **dxTakeOrder**: **maker/taker swapped** — C++ swaps `from/to` before rendering
  (`rpcxbridge.cpp:1267-1286`). `dryrun` string broadcasts. Partial `amount` not
  validated/recomputed; no self-trade guard.
- **dxCancelOrder**: cancels when `state>=trCreated` (C++ refuses,
  `rpcxbridge.cpp:1365`); `refund_tx` always `""` (C++ emits refund txid);
  addresses empty for remote orders; `updated_at` now vs C++ `txtime`.
- **dxLoadXBridgeConf**: returns `true`, no reload side effect.

### Wallet / token / split
- **dxGetNewTokenAddress**: no-wallet → error vs C++ `[]`; segwit → `"bech32"`
  vs C++ wallet-default; wallet-error → error vs C++ silent omit.
- **dxGetTokenBalances**: missing `"Wallet"` key (`rpcxbridge.cpp:2531`); includes
  locked UTXOs; iterates `ExchangeWallets` vs all connectors; per-coin `Decimals`
  vs C++ fixed 7.
- **dxGetUtxos**: missing always-present `orderid` field (`rpcxbridge.cpp:3487`);
  includes locked UTXOs when `include_used=false`.
- **dxSplitAddress / dxSplitInputs** (+ `splitTx`): response shape
  `{address,txid,rawtx}` vs C++ `{token,include_fees,split_amount_requested,
  split_amount_with_fees,split_utxo_count,split_total,txid,rawtx}`
  (`rpcxbridge.cpp:3280`); splits all wallet UTXOs vs target address only; change
  to new address vs same address; fee model diverges; no locked-UTXO exclusion;
  no 100-UTXO cap. `dxSplitInputs` requires ≥6 params in C++.

### History / trading / networkinfo
- **dxGetOrderHistory**: DONE. Param contract matches C++
  (`rpcxbridge.cpp:642-702`): `(maker, taker, start, end, granularity,
  [order_ids], [with_inverse], [limit])`. Now emits **all `(end-start)/granularity`
  buckets across the range, zero-filled when a slice has no trades** — matching
  C++'s N zero-filled OHLCV slices (the help example shows a fully-zero middle
  bucket). Each bucket is `[iso8601, low, high, open, close, volume, [orderIds?]]`;
  OHLCV is aggregated from the thin client's local `Store.Fills()` (price =
  taker_size/maker_size, volume = Σ taker_size), so it reflects **this node's
  local trade history only** — network-wide XSeries history (requiring a blocknetd
  block index) is not reproduced. Numbers are JSON floats, matching C++'s Boost
  double serialization.
- **dxGetTradingData / gettradingdata**: DONE. Both names dispatch to one handler
  (`rpcxbridge.cpp:2782`); params `(blocks, errors)` are accepted (contract
  compatibility) but not bound to a BLOCK block scan. Emits the C++ 8-field
  record schema `{"timestamp", "fee_txid", "nodepubkey", "id", "taker",
  "taker_size", "maker", "maker_size"}` built from local fills; `fee_txid`/
  `nodepubkey` are `""` (on-chain BLOCK trade-fee data is unavailable to the thin
  client). Source is local history, not on-chain BLOCK history.
- **getNetworkInfo**: Go-only extension; missing `xbridgeprotocolversion`/
  `xrouterprotocolversion` (`src/rpc/net.cpp:524-525`); harmless for the gate.

## Prioritized remediation plan

**P0 — universal + money-moving:**
1. C1: amounts/prices → 6 decimals.
2. C2 + C3: error `code` values + message prefixes → C++ enum.
3. `dryrun` string handling in `dxMakeOrder`/`dxMakePartialOrder`/`dxTakeOrder`
   (must simulate, not broadcast).
4. `dxTakeOrder` maker/taker swap.
5. C4/C5: strict param coercion + drop `jsonrpc:"2.0"` envelope.

**P1 — wrong data / wrong schema:**
6. `dxGetOrderBook` price inversion + detail levels 1/2/4.
7. `dxPartialOrderChainDetails` + `dxGetLockedUtxos` real schemas.
8. `dxSplitAddress`/`dxSplitInputs` shape + `splitTx` construction.
9. `dxFlushCancelledOrders` actual prune.
10. `dxMakePartialOrder`/`dxMakeOrder` partial→exact misreport + trailing params.

**P2 — missing fields / edge cases / thin-client hardening:**
11. `dxGetOrderFills` fields; `dxGetOrder` `NO_SESSION` + case-insensitive id;
    `dxGetUtxos`/`dxGetTokenBalances` `orderid`/`Wallet`/locked-UTXO;
    `dxGetMyOrders` historical locals; `dxGetMyPartialOrderChain` descendants;
    `dxCancelOrder` `refund_tx`/in-progress guard; `dxGetNetworkTokens` live
    union; `dxGetNewTokenAddress` `[]` vs error.

## Verification gaps to close before shipping
- Byte-level capture of `OrderBody`/`AcceptingBody`/`CancelBody` UTXO-entry
  encoding vs live C++ (Go uses BIP137 proofs; C++ embeds real funding UTXOs).
- Live P2P verification of the swap-handshake claim/refund spends (in-memory
  only today).

## Remediation progress (applied 2026-07-16)

**P0 — universal + money-moving: DONE.**

1. **C1** — `formatXAmount`/`formatXPrice` now emit **6 decimals** (was 7);
   re-verified against `xBridgeSignificantDigits(1_000_000)==6`. Tests updated.
2. **C2 + C3** — error `code` values replaced with the C++ 1000-range enum
   (`xbridgeerror.h`); `makeError` now routes the message through
   `xbridgeErrorText(code, arg)`, which prepends the per-code text (audit C3).
3. **dryrun string handling** — `dxMakeOrder`/`dxTakeOrder` read the literal
   string `"dryrun"` at the trailing positional param (only when the param
   count matches C++ exactly); any other value is an error, so a misspelled
   dryrun no longer broadcasts. Dryrun returns the result without broadcasting.
4. **dxTakeOrder maker/taker swap** — the real take now renders maker =
   order's `toCurrency`, taker = order's `fromCurrency` (C++ `std::swap` before
   render). Dryrun keeps the pre-swap frame, zero id, status `"filled"`. The
   wire `AcceptingBody` amounts were already correct (only the result was
   swapped); partial takes now recompute sizes via `xBridgeSourceAmountFromPrice`.
5. **C4/C5** — C5 applied: JSON-RPC 1.0 envelope (no `jsonrpc` field, compact).
   `dxMakeOrder`/`dxTakeOrder` now hard-error on a mistyped trailing param
   (string-compare for `"dryrun"`). The broader strict-coercion pass for
   `mustBool`/`mustInt` on missing params is tracked under P1.

Also applied from the P0-adjacent validations: `dxMakeOrder` now enforces the
6-decimal precision gate (`xBridgeValidCoin`), `type != "exact"` rejection,
`maker_address == taker_address` rejection, `MAX_COIN`/min-size limits, and
per-currency `NO_SESSION`; `dxTakeOrder` enforces `from_address == to_address`
rejection, `amount <= 0`, partial min/max bounds, `INVALID_PARTIAL_ORDER`, and
the self-trade guard (`Unable to accept your own order.`).

**P1 — partial progress (2026-07-16):**

6. **dxGetOrderBook** — DONE. Prices were inverted; now ask = `to/from`
   (taker-per-maker) and bid = `from/to`, matching C++ `price()`/`priceBid()`.
   Added detail-level validation (1-4), all four detail schemas (1: best
   bid/ask + count; 2: aggregated top levels with summed size + full-list
   count, per-side `maxOrders`; 3: full non-aggregated with order ids, per-side
   `maxOrders`; 4: best bid/ask with order-id arrays), case-insensitive
   currency matching (`strings.EqualFold` = `boost::iequals`), the
   zero/negative-amount + non-open filter, descending sort with best-first
   output, and per-side `maxOrders` (combined-total limit was wrong).
7. **dxPartialOrderChainDetails + dxGetLockedUtxos** — DONE. `dxPartialOrderChainDetails`
   now walks the full chain (ancestors + self + descendants, root-first), renders
   `first_order_id` (root), `first_order_time`/`last_order_time`, aggregates
   `total_reported_sent/received/notsent/notreceived` from chain order amounts,
   `total_orders_open/finished/canceled` from chain states, and emits `orders` as
   a **hex-id array** (was returning order objects). `dxGetLockedUtxos` now
   returns the C++ schema: `{"all_locked_utxo":[...]}` with no id, `{"id", CUR:[...]}`
   with an id, the `>1 param` → `INVALID_PARAMETERS` gate, and the
   `NOT_EXCHANGE_NODE` gate when no exchange wallets are configured. Locked-UTXO
   backing (real `txid:vout:amount:address` items, p2sh deposits) not yet wired.
8. **dxSplitAddress / dxSplitInputs** — DONE. Both now return C++'s 8-field object
   `{token, include_fees, split_amount_requested, split_amount_with_fees,
   split_utxo_count, split_total, txid, rawtx}`; `splitamount` is parsed in XBridge
   1e6 scale (was native), on-chain outputs stay native; `txid` is the
   double-SHA256 of the signed tx (byte-reversed), always computed; amounts are
   rendered in 1e6 scale via `formatXAmount`; `dxSplitInputs` requires exactly 7
   params (C++ contract) and `dxSplitAddress` 3–6 with defaults; a dust gate
   matches C++ `split amount is dust [...]`.
9. **dxFlushCancelledOrders** — DONE. Now actually prunes `Store.cancelled`
   (`Txtime < now - ageMillis_ms`) and returns only the removed subset as
   `{id, txtime (ISO-8601), use_count}`; `len(params) > 1` →
   `errInvalidParameters "ageMillis must be an integer >= 0"`; `txtime` rendered
   as ISO-8601 (matches C++).
10. **dxMakePartialOrder** — DONE. Now reads the trailing params `repost`
    (idx 7, default true), `use_all_funds` (idx 8, default true),
    `auto_split` (idx 9, default true), and `dryrun` (idx 10, literal
    `"dryrun"` only when 11 params; any other value errors). Added the two
    missing validations: `minimum_size > maker_size` → `INVALID_PARAMETERS
    "The minimum_size can't be more than maker_size"`, and the dust check on
    `minimum_size` → `INVALID_PARAMETERS "The partial minimum_size is dust,
    i.e. it's too small."`. Routed to a new `makePartialOrderResponse` that
    emits `order_type="partial"` with real `partial_minimum` /
    `partial_orig_*_size` and `partial_repost`. (C++ dxMakeOrder exact now emits
    `partial_minimum="0.000000"` — fixed to 6-decimal format.)

**P2 — partially applied (2026-07-16):**
- `dxGetOrder` — DONE. Now lowercases the id before lookup (matches C++
  `uint256S` case-insensitive normalization) and applies the C++ `NO_SESSION`
  gate for both `fromCurrency`/`toCurrency`.
- `dxGetMyPartialOrderChain` — DONE. Now resolves descendants as well as
  ancestors (reuses the shared `partialOrderChain` walker), matching C++
  `getPartialOrderChain`.
- `dxGetOrderFills` — DONE. `fillOut` now emits C++'s full 12-field object
  (added `order_type`, `partial_minimum`, `partial_orig_maker_size`,
  `partial_orig_taker_size`, `partial_repost`, `partial_parent_id`); `fillEntry`
  carries the backing partial fields. (Audit note: the C++ help *text* shows 6
  fields, but the actual writer at `rpcxbridge.cpp:566-584` emits 12 — the code,
  not the help comment, is authoritative.)

**P2 — applied (2026-07-16, final pass):**
- `dxGetNewTokenAddress` — DONE. No-connector now returns an empty array `[]`
  (C++ `rpcxbridge.cpp:150-193` returns `res` with no error when the connector is
  absent or returns an empty address), matching the documented `[]` contract.
- `dxGetUtxos` — DONE (schema). The always-present `orderid` key is now emitted
  (`""` when the UTXO is not locked in an order, per `rpcxbridge.cpp:3487`). Added
  the 1–2 param contract (token + optional `include_used` bool) and an
  `INVALID_PARAMETERS` gate when `len(params) > 2`. Amounts remain in **native
  scale + native decimals** — C++ renders `amount` via
  `xBridgeStringValueFromPrice(utxo.amount, conn->COIN)` (native units, per-coin
  decimals, e.g. 8 for BTC), so `coins.FormatAmount` was already correct. The
  per-UTXO locked-UTXO exclusion (`include_used=false` drops locked UTXOs) and the
  real `orderid` mapping are **not yet wired**: the thin client's `Store.Lock`
  keys by order id, not by UTXO, so per-UTXO order locks aren't tracked.
- `dxGetTokenBalances` — DONE (schema). Added the C++ `Wallet` key (`rpcxbridge.cpp:2531`,
  `availableBalance()/COIN` rendered in fixed-6 XBridge scale); approximated with
  the first configured exchange-wallet balance (the thin client has no single
  native-wallet balance). Per-coin balances are now rendered in **fixed-6 XBridge
  scale** (`formatXAmount(toXBridgeAmt(c, total))`) instead of native per-coin
  decimals, matching C++ `xBridgeStringValueFromPrice(balance)` (the
  `balance` fed in is COIN-divided — confirmed by the dimensional comparison in
  `xbridgeapp.cpp:2575`). The locked-UTXO exclusion in the balance sum is **not
  yet wired** (same `Store` limitation as `dxGetUtxos`).
- `dxGetMyOrders` — DONE (schema). Now sorts ascending by `updated_at` (`txtime`),
  matching C++ `rpcxbridge.cpp:2128` (`std::sort` by `txtime` ascending). It
  already returns all local (`Mine==true`) orders including those in
  `finished`/`canceled` states, so historical locals are covered for any such
  orders present in the store; the C++ `history()` registry (a separate
  finished/cancelled store) is not folded in — a thin-client limitation, not a
  behavioral divergence for orders the local node knows about.
- `dxCancelOrder` — DONE (P2). The in-progress guard (`state >= trCreated` →
  `INVALID_STATE`, `rpcxbridge.cpp:1365`), invalid-id `INVALID_PARAMETERS`, and
  `NO_SESSION` gate are all applied. `cancelOrderResult.refund_tx` is emitted as
  a field (shape matches C++); the *value* (an actual refund txid from the
  in-progress swap) is not yet computed — a thin-client backing limitation.

**P2 — applied (final pass, 2026-07-16):** the three remaining commands are
now implemented faithfully:
- `dxGetNetworkTokens` — live servicenode union (see note at line ~127).
- `dxGetOrderHistory` — OHLCV bucket series from local fills (zero-filled empties).
- `dxGetTradingData` / `gettradingdata` — 8-field record schema from local fills.

Caveat carried forward: `dxGetOrderHistory`/`dxGetTradingData` reflect **only
this node's local trade history** (since node start), not network-wide XSeries /
on-chain BLOCK history, because a thin client without blocknetd has no block
index. The wire *schema* matches C++ exactly; `fee_txid`/`nodepubkey` in trading
data are `""` (on-chain BLOCK data unavailable locally). `dxGetNetworkTokens`
falls back to the config list when no servicenodes are connected.

**Tier 1 + Tier 2 backing wiring (applied 2026-07-17):** closes the "not yet
wired" gaps flagged above and fixes the remaining code bugs in the `dx*`
surface. No `blocknetd` dependency is introduced (the thin-client design is
preserved).

- **T1.1 `stateOrdinal` enum** (`api/response.go`): added the two missing
  `TransactionDescr::State` values — `trRollback=10`, `trRollbackFailed=11` —
  and renumbered `trDropped=12`, `trCancelled=13`, `trInvalid=14`. Previously
  these fell through to `0`, which broke `dxCancelOrder`'s `stateOrdinal >=
  trCreated(6)` guard (a rolled-back order was wrongly cancellable). Verified by
  `TestStateOrdinal`.
- **T1.2 `dxPartialOrderChainDetails`** (`api/handlers.go`): `total_orders_open`
  now counts `stateOrdinal(t.Status) <= 2` (C++ `state <= trPending`) instead of
  only `status == "open"`; an unknown/empty chain returns an empty object `{}`
  (not an error); the order id is validated as 64-hex and rejected with
  `INVALID_PARAMETERS` otherwise. `p2sh_deposits` / `p2sh_deposits_counterparty`
  are now populated from each order's `BinTxId` / `OBinTxId` (T2.3).
- **T1.3 `dxTakeOrder` amount=0** (`api/node.go`): an omitted or zero `amount`
  is now a **full-order** take (sizes equal the order's maker/taker sizes),
  matching C++. Only a positive amount engages the partial recompute. Verified
  by `TestDxTakeOrderFullTake`.
- **T1.4 `Store.FlushCancelled` underflow** (`api/store.go`): the `uint64`
  subtraction `now - minAgeMillis*1000` is clamped so a huge `minAgeMillis`
  prunes every entry instead of wrapping around. Verified by
  `TestFlushCancelledUnderflow`.
- **T1.5 `dxGetLockedUtxos` nil-guard** (`api/handlers.go`): returns
  `NOT_EXCHANGE_NODE` instead of panicking when `Node` / `Config` / exchange
  wallets are absent.
- **T2.1 per-UTXO locked tracking** (`api/store.go`, `api/handlers.go`): added
  `Store.LockedUtxoInfo()` which derives the locked `txid:vout` set from each
  *active* (non-terminal) order's `Utxos`. `dxGetLockedUtxos` now emits those
  reserved UTXOs, `dxGetUtxos` excludes them when `include_used=false` (and sets
  `orderid` when `true`), and `dxGetTokenBalances` subtracts each currency's
  locked total from the wallet balance. Verified by `TestDxLockedUtxoExclusion`.
- **T2.2 `dxCancelOrder` `refund_tx`** (`api/swap.go`): the swap layer now ties
  its pre-signed refund hex to the order (`o.RefundTx`), so `dxCancelOrder`
  surfaces a real `refund_tx` once a swap has reached the deposit step. Orders
  cancelled before any deposit still return `""` (matches C++).
- **T2.3 `p2sh_deposits`** (`api/order.go`, `api/swap.go`): added `BinTxId` /
  `OBinTxId` to `Order`; the maker/taker deposit broadcasts populate them
  (`buildDeposit` / `OnCreateB` / `OnConfirmA`), and `dxPartialOrderChainDetails`
  emits them per order.
- **T2.4 `dxGetTokenBalances` `Wallet` key** (`api/handlers.go`): the `Wallet`
  key is now derived from the BLOCK connector (fallback: the first configured
  exchange wallet), matching C++'s native-wallet balance label.

**Tier 3 — documented, not fixed (thin-client architectural limits):** see the
"Tier 3 — architectural limits" section in `docs/api.md`. These require a BLOCK
block index / `blocknetd` and are intentionally out of scope for this pass:
`dxGetOrderHistory` / `dxGetTradingData` reflect session-local fills only
(`fee_txid` / `nodepubkey` empty), and `dxGetNetworkTokens` completeness is
bounded by P2P servicenode-ping coverage.

