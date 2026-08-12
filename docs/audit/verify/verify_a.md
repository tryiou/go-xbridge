# Verification report — candidate conformance findings RPC-F01–RPC-F12

Re-read of the full call graph on both sides (C++ reference
`blocknet_core/src/xbridge/` + `src/rpc/`; Go candidate `go-xbridge/api/`).
For each finding: VERDICT, exact file:line evidence for BOTH sides, and a
corrected one-line statement of the divergence.

---

## RPC-F01 ERROR-CHANNEL POLICY (cross-cutting)

**VERDICT: PARTIAL** — the cross-cutting claim is directionally right but
mischaracterizes the C++ side for most of the methods it lists.

### What C++ actually does (two distinct patterns)

**Pattern A — returned in the `result` object, not thrown** (`makeError` →
`xbridgeErrorText`; `rpcxbridge.cpp` returns `uret(makeError(...))` which lands
in the JSON-RPC `result` as `{error, code, name}`, envelope `error: null`):

| method | count check | makeError code | file:line |
|---|---|---|---|
| dxGetOrderFills | `(size!=2 && size!=3)` | 1025 | rpcxbridge.cpp:532-537 |
| dxGetOrders | `!params.empty()` | 1025 | rpcxbridge.cpp:424-427 |
| dxGetOrder | `size!=1` | 1025 | rpcxbridge.cpp:774-776 |
| dxGetLocalTokens | `size>0` | 1025 | rpcxbridge.cpp:267-270 |
| dxLoadXBridgeConf | `size>0` | 1025 | rpcxbridge.cpp:218-220 |
| dxGetNetworkTokens | `size>0` | 1025 | rpcxbridge.cpp:314-317 |
| dxGetTokenBalances | `size!=0` | 1025 | rpcxbridge.cpp:2519-2526 |
| dxGetMyOrders | `!params.empty()` | 1025 | rpcxbridge.cpp:2083-2093 |
| dxGetOrderBook | `size<3 \|\| size>4` | 1025 | rpcxbridge.cpp:1527-1531 |
| dxCancelOrder | `size!=1` | 1025 | rpcxbridge.cpp:1347-1350 |
| dxGetLockedUtxos | `size>1` | 1025 | rpcxbridge.cpp:2612-2619 |
| dxGetOrderHistory | `size<5 \|\| size>8` | 1025 | rpcxbridge.cpp:643-650 |
| dxGetNewTokenAddress | `size!=1` | 1025 | rpcxbridge.cpp:178-179 |
| dxFlushCancelledOrders | `ageMillis<0` | 1025 | rpcxbridge.cpp:1460-1464 |

So for the methods the finding lists as "C++ THROWS", the C++ side **returns**
`{error:"Invalid parameters: …", code:1025, name:<fn>}` in `result` — the *same
channel Go uses* (Go `response.go:165-167 makeError`, `server.go:181-187` put
`*rpcError` in `result`). For these methods there is **no envelope-channel
divergence** for the *param-count* case.

**Pattern B — actually thrown → envelope `error`** (`throw runtime_error(RPCHelpMan…)`
at the top of the handler; caught at `src/rpc/server.cpp:584-587`
`catch(std::exception) → throw JSONRPCError(RPC_MISC_ERROR, e.what())`, then
`server.cpp:488-492` puts it in the envelope `error`; `result` becomes null —
`protocol.cpp:40-50`, error object `{code:-1, message:<full help text>}`
`protocol.cpp:58-64`; `RPC_MISC_ERROR=-1`, `protocol.h:50`):

| method | thrown for | file:line |
|---|---|---|
| dxMakeOrder | `fHelp \|\| size<7` | rpcxbridge.cpp:817 |
| dxMakePartialOrder | `fHelp \|\| size<6` | rpcxbridge.cpp:2912 |
| dxTakeOrder | `fHelp \|\| size<3 \|\| size>5` | rpcxbridge.cpp:1077 |
| gettradingdata / dxGetTradingData | `fHelp \|\| size>2` | rpcxbridge.cpp:2684 / 2784 |
| dxSplitAddress | `fHelp \|\| size<3 \|\| size>6` | rpcxbridge.cpp:3199 |
| dxSplitInputs | `fHelp \|\| size<3 \|\| size>7` | rpcxbridge.cpp:3295 |
| dxGetUtxos | `fHelp \|\| size<1 \|\| size>2` | rpcxbridge.cpp:3408 |
| dxGetMyPartialOrderChain / dxPartialOrderChainDetails | `empty \|\| >1` | rpcxbridge.cpp:2180 / 2328 |

**Param-type errors** are also *thrown* by json_spirit: `get_str()/get_bool()/get_int()`
call `check_type` which `throw std::runtime_error("get_value< … > called on … Value")`
(`src/json/json_spirit_value.h:382-433`); caught → envelope `{code:-1,…}`.
`RPCTypeCheck` throws `JSONRPCError(RPC_TYPE_ERROR=-3)` (envelope `{code:-3,…}`)
(`src/rpc/server.cpp:98-103`; used in dxGetTradingData `rpcxbridge.cpp:2721/2724`,
dxGetMyPartialOrderChain :2271, dxPartialOrderChainDetails :2411).

### What Go does

All param-count and param-type failures become `makeError(errInvalidParameters=1025,…)`
→ `rpcError` in `result` (dispatch.go:75-130 helpers, handlers.go throughout,
server.go:181-185). Wrong-type params are silently coerced where C++ throws:
`boolParam` tolerates JSON strings `"true"/"false"` (dispatch.go:103-108),
`intParam`/`int64Param` tolerate numeric strings (dispatch.go:120-128, 143-148),
and several handlers never inspect params at all.

### Net divergence for the claimed methods

- **dxGetOrderFills** — C++ **returns** 1025 in `result` for the count error
  (rpcxbridge.cpp:532-537), so the "channel" part of the claim is wrong for the
  count case; real divergences are (a) a **non-string maker/taker** → C++
  json_spirit `get_str()` throws → envelope `{code:-1, message:"get_value< str >
  called on … Value"}` (rpcxbridge.cpp:541-542 + json_spirit_value.h:382-401),
  while Go returns `{code:1025,name:"dxGetOrderFills"}` in `result`
  (handlers.go:58-65); (b) **non-bool / string-form `combined`** → C++ `get_bool()`
  throws envelope `-1` (rpcxbridge.cpp:539), Go coerces `"true"/"false"` strings
  and otherwise returns 1025 in `result` (dispatch.go:97-112, handlers.go:66-69).
- **dxGetOrders** — count error: both sides return 1025 in `result`
  (rpcxbridge.cpp:424-427 vs handlers.go:103-104). No channel divergence.
- **dxGetOrder** — count error: both 1025 in `result` (rpcxbridge.cpp:774-776 vs
  handlers.go:139). No channel divergence. (Wrong-typed `id` → C++ `get_str()`
  throws envelope -1; Go `strParam` → 1025 in `result` — divergence.)
- **dxGetLocalTokens / dxGetNetworkTokens / dxGetTokenBalances / dxGetMyOrders /
  dxLoadXBridgeConf** — C++ **returns** 1025 in `result` for any param
  (rpcxbridge.cpp:267-270, 314-317, 2519-2526, 2083-2093, 218-220); Go **silently
  ignores** params (handlers.go:174-175, 178-187, 762, 840, 213). So the divergence
  is "Go fails to reject params" — NOT a channel difference.
- **dxGetOrderBook** — count error both 1025 in `result` (rpcxbridge.cpp:1527-1531
  vs handlers.go:621-623); **non-int `detail`/`max_orders`** → C++ `get_int()` throws
  envelope `{code:-1}` (rpcxbridge.cpp:1540, 1547); Go `mustInt` returns 1025 in
  `result` (handlers.go:624-645) and coerces numeric strings.
- **dxGetTradingData** — `>2` params → C++ throws (envelope -1, help text,
  rpcxbridge.cpp:2784); type errors → `RPCTypeCheck` throws envelope `{code:-3}`
  (rpcxbridge.cpp:2849-2852); Go returns 1025 in `result` for `len>2` and does not
  type-check `blocks`/`errors` at all (handlers.go:1198-1200, 1201-1216).
- **dxSplitAddress / dxSplitInputs / dxGetUtxos** — C++ throws for out-of-range
  count (rpcxbridge.cpp:3199, 3295, 3408) → envelope `{code:-1, message:help}`; Go
  returns 1025 in `result` (handlers.go:1225-1226, 1254-1256, 1654-1655).
  dxSplitInputs additionally: C++ `size<7` means the unconditional
  `params[3].get_bool()` throws at the first missing index (rpcxbridge.cpp:3355-3358)
  → envelope -1; Go requires exactly 7 and returns 1025 in `result`.

### Corrected statement

C++ uses **two** error channels for param errors: (i) ~14 handlers **return**
`{error,code:1025,name}` in the JSON-RPC **`result`** — this is exactly Go's
channel for those methods, so there is no divergence for them; (ii) the
make/partial/take/tradingdata/split/utxo handlers **throw** `std::runtime_error`
(→ envelope `error` `{code:-1, message:<full help text>}`) and param-*type*
errors throw in **all** handlers (→ envelope `{code:-1, message:"get_value< … >"}`,
or `{code:-3}` via `RPCTypeCheck`) — for these Go instead returns 1025 in
`result`, and additionally Go silently ignores params (dxGetLocalTokens,
dxGetNetworkTokens, dxGetTokenBalances, dxGetMyOrders, dxLoadXBridgeConf,
dxGetTradingData) and coerces `"true"/"false"` and numeric strings where C++ throws.

---

## RPC-F02 NO_SESSION error `name` field

**VERDICT: CONFIRMED (with the affected-method list corrected)**

- Go: the shared helper `connector()` hardcodes the name as `"dx"` —
  handlers.go:200, 204 (`makeError(errNoSession, "dx", ticker)`); makeError sets
  `rpcError.Name` from that arg (response.go:165-167).
- Go call sites that *surface* the NO_SESSION error to the wire: dxGetOrder
  (handlers.go:161-166), dxCancelOrder (handlers.go:428-433), dxGetUtxos
  (handlers.go:1670-1673), dxSplitAddress/dxSplitInputs via `splitTx`
  (handlers.go:1298-1301). Call sites that swallow the error (no wire effect):
  dxGetOrders (handlers.go:121-122), dxGetNewTokenAddress (handlers.go:236-241),
  dxGetLockedUtxos (handlers.go:1063, 1104, 1123).
- C++: every NO_SESSION passes `__FUNCTION__` as the `name` — dxGetOrder
  (rpcxbridge.cpp:791, 794 → `"dxGetOrder"`), dxCancelOrder (1379, 1383 →
  `"dxCancelOrder"`), dxGetUtxos (3471 → `"dxGetUtxos"`), dxSplitAddress
  (3264 → `"dxSplitAddress"`), dxSplitInputs (3372 → `"dxSplitInputs"`).
- **Corrections to the claim's list:** dxGetOrderBook and dxGetTokenBalances do
  NOT emit NO_SESSION in Go at all (dxGetOrderBook reads only `Store.List()`,
  handlers.go:654; dxGetTokenBalances iterates `cfg().Connectors` and skips
  missing ones, handlers.go:800-803), so they are not affected. dxGetLockedUtxos
  never surfaces the "dx"-named NO_SESSION either (all three `connector()`
  calls discard the error); C++ dxGetLockedUtxos has no connector-based NO_SESSION
  at all (it gates on `NOT_EXCHANGE_NODE`, rpcxbridge.cpp:2621-2629).

### Corrected statement

Go's shared `connector()` helper hardcodes the NO_SESSION `name` as `"dx"`
(handlers.go:200,204) instead of C++ `__FUNCTION__`; wire-visible on dxGetOrder,
dxCancelOrder, dxGetUtxos, dxSplitAddress, dxSplitInputs (dxGetOrderBook /
dxGetTokenBalances / dxGetLockedUtxos are not affected).

---

## RPC-F03 dxGetOrders ARRAY ORDER

**VERDICT: CONFIRMED**

- C++: `TransactionMap trlist = xapp.transactions();` where
  `using TransactionMap = std::map<uint256, TransactionDescrPtr>`
  (rpcxbridge.cpp:42, 430) — `std::map` iterates in ascending key (raw uint256
  bytes) order; the loop `for (const auto& trEntry : trlist)` (rpcxbridge.cpp:434)
  emits in that order.
- Go: `for _, o := range h.Store.List()` (handlers.go:108) where `Store.List()`
  ranges over the Go `map[string]*Order` `s.orders` (store.go:26, 154-162) — no
  sort applied anywhere in the dxGetOrders path.

### Corrected statement

C++ emits dxGetOrders in deterministic ascending raw-id order (std::map<uint256>);
Go iterates an unsorted Go map, so array order is randomized per run.

---

## RPC-F04 dxGetOrders 60-SECOND FILTER

**VERDICT: CONFIRMED**

- C++: `auto currentTime = boost::posix_time::second_clock::universal_time();`
  (rpcxbridge.cpp:431 — whole-second granularity); filter
  `(currentTime - tr->txtime).total_seconds() > 60` (rpcxbridge.cpp:439).
  `total_seconds()` truncates the microsecond difference toward zero, and
  `txtime` is microsecond-precision (`microsec_clock`, xbridgetransactiondescr.h:594).
- Go: `now := NowMicro()` (handlers.go:106; NowMicro = microsecond epoch,
  store.go:517-519); filter `now-o.Updated > 60*1e6` (handlers.go:111) —
  microsecond-exact comparison.

Boundary behavior:
- 59.999 s old → C++ `59 > 60` false ⇒ **included**; Go `59.999e6 > 60e6` false ⇒ **included**. Agree.
- 60.000 s old → C++ `60 > 60` false ⇒ **included**; Go `60e6 > 60e6` false ⇒ **included**. Agree.
- 60.4 s old → C++ `total_seconds()==60` ⇒ **included**; Go `60.4e6 > 60e6` ⇒ **excluded**. **DIVERGENT.**
- 61.0 s old → C++ `61 > 60` ⇒ **excluded**; Go **excluded**. Agree.

### Corrected statement

C++ uses second-truncating `total_seconds() > 60` on a second-granularity clock,
so orders aged between 60 and <61 seconds are still included; Go's microsecond-exact
`now-o.Updated > 60e6` excludes them — divergence confined to the (60s, 61s) window.

---

## RPC-F13 dxMakePartialOrder DUST GATE UNIT MISMATCH

**VERDICT: CONFIRMED**

- C++: `partialMinimum = boost::lexical_cast<double>(params[6].get_str())`
  (real-coin double, rpcxbridge.cpp:3040); gate
  `if (connFrom->isDustAmount(partialMinimum))` (rpcxbridge.cpp:3088).
  `isDustAmount(amount)` = `int64(amount * COIN_native) < int64(dustAmount)`
  (xbridgewalletconnectorbtc.cpp:1900-1904), with `dustAmount =
  relayFee>0 ? 0.546*relayFee*COIN_native : 5460` (:1526). So C++ compares the
  minimum in **native (1e8) base units** against a **native** dust threshold.
  (The xbridgeapp.cpp:1566 re-check `isDustAmount(xBridgeValueFromAmount(partialMinimum))`
  is the same native-scale comparison.)
- Go: `minFrom = parseXAmount(p.MinSize)` — **XBridge 1e6 base units**
  (node.go:951-955, parseXAmount response.go:271-316, coinScale=1e6 response.go:221);
  gate `if cc != nil && minFrom < effectiveDust(cc, relayFee)`
  (node.go:961-963); `effectiveDust = 0.546*relayFee*cc.Coin` (or conf/5460) —
  **native 1e8 base units** (handlers.go:1449-1457; `cc.Coin` native multiplier,
  config/conf.go:58-60). So Go compares a 1e6-scale value against a native-scale
  threshold.
- Scale error factor: for a 1e8-native coin the C++ effective threshold in 1e6
  units is `dustAmount/100`; Go compares against `dustAmount` directly, so the
  Go gate is **100× more lenient** (accepts minimums C++ rejects as dust) in the
  band `[0.546*relayFee*1e6, 0.546*relayFee*1e8)` (1e6 units; e.g. with relayFee
  0, dust 5460: minimums 55…5459 base units pass in Go, dust-error in C++).
  `parity_dustfee_test.go:9-40` only exercises `effectiveDust` itself (correct
  native value) — it does not cover the `minFrom < effectiveDust` comparison at
  node.go:961.

### Corrected statement

Go compares `minFrom` (1e6 XBridge base units) directly against `effectiveDust`
(native 1e8 base units) at node.go:961, whereas C++ converts the minimum to
native units first (isDustAmount, xbridgewalletconnectorbtc.cpp:1900-1904) — the
Go dust gate is ~100× more lenient for 1e8-native coins.

---

## RPC-F14 dxTakeOrder explicit amount "0"

**VERDICT: CONFIRMED**

- C++: when `params.size()>=4` and the amount string is non-empty,
  `amount = lexical_cast<double>(amountStr); if (amount <= 0) return
  makeError(INVALID_PARAMETERS, …, "The amount cannot be less than or equal to 0: "
  + params[3].get_str())` (rpcxbridge.cpp:1151-1161). Explicit `"0"` (and negative)
  ⇒ 1025 business error in `result`; empty string `""` ⇒ full take.
- Go: `amount, _ := strParam(params, 3)` (handlers.go:381); in TakeOrder
  `if p.Amount != "" { a, err := parseXAmount(p.Amount); …; if a > 0 { partial
  recompute } }` (node.go:1351-1374). `parseXAmount("0")` succeeds with `a==0`,
  `a > 0` is false, so fromSize/toSize stay at full order size and the take
  proceeds — an explicit `"0"` is silently treated as a full take (node.go:1356-1359
  comment even states this). Negative `"-1"` errors in Go too (parseXAmount rejects,
  response.go:276-278), but with a different message ("invalid amount") and in
  `result`, where C++ `lexical_cast` throws → envelope -1.

### Corrected statement

Explicit `amount="0"`: C++ returns business 1025 "The amount cannot be less than
or equal to 0: 0" (rpcxbridge.cpp:1156-1159); Go proceeds as a full take
(node.go:1351-1374).

---

## RPC-F09/RPC-F10/RPC-F11 dxMakeOrder RESPONSE FIELD ORDER + DRYRUN + PARTIAL LITERAL

**VERDICT: CONFIRMED** (all three sub-claims; one detail corrected)

### (a) Field order

- C++ dxMakeOrder success (rpcxbridge.cpp:1049-1066): `id, maker_address, maker,
  maker_size, taker_address, taker, taker_size, created_at, updated_at, block_id,
  order_type, partial_minimum, partial_orig_maker_size, partial_orig_taker_size,
  partial_repost, partial_parent_id, status` — created_at **before** updated_at,
  addresses interleaved at positions 2/5, block_id at 10.
- Go `makeOrderResult` (response.go:57-62 + embedded orderBase :26-41) marshals:
  `id, maker, maker_size, taker, taker_size, updated_at, created_at, order_type,
  partial_minimum, partial_orig_maker_size, partial_orig_taker_size,
  partial_repost, partial_parent_id, status, maker_address, taker_address, block_id`
  — updated_at **before** created_at (swapped vs C++), and maker_address /
  taker_address / block_id appended at the **end**.
- Note: for dxGetOrders/dxGetOrder the C++ order is updated_at then created_at
  (rpcxbridge.cpp:458-459, 803-804), matching Go's orderBase — the swap is
  specific to dxMakeOrder/dxMakePartialOrder success.

### (b) dryrun

- C++ dxMakeOrder dryrun (rpcxbridge.cpp:1004-1021) and dxMakePartialOrder dryrun
  (3122-3139): 14 fields, `id = uint256().GetHex()` (zero id), and **no**
  updated_at / created_at / block_id.
- Go dryrun goes through the same `makeOrderResponse()` /
  `makePartialOrderResponse()` as a real order (handlers.go:303, 366) with the
  **real** computed id (node.go:1067, never zeroed; dryrun only skips
  hub-select/broadcast/store, node.go:1197-1199, 1274) and the real `BlockID`
  (node.go:1204-1205) → 17-field object including updated_at, created_at, block_id.

### (c) literal "0"

- C++ dxMakeOrder emits the literal string `"0"` for partial_minimum /
  partial_orig_maker_size / partial_orig_taker_size in BOTH the dryrun branch
  (rpcxbridge.cpp:1015-1017) and the real success branch (:1061-1063).
- Go `makeOrderResponse` sets them to `formatXAmount(0)` = `"0.000000"`
  (order.go:244-246; formatXAmount response.go:237-241). (Go's dxGetOrders /
  dxGetOrder rendering — `"0.000000"` — matches C++ there, which uses
  `xBridgeStringValueFromAmount`, rpcxbridge.cpp:461-463.)

### Corrected statement

dxMakeOrder/dxMakePartialOrder success-object key order diverges (C++ interleaves
maker_address/taker_address and puts created_at before updated_at, block_id before
order_type; Go appends addresses+block_id at the end and swaps created/updated);
C++ dryrun returns a 14-field object with a zero id and no block fields while Go
returns the full 17-field object with the real id and real block_id; the three
partial_* fields render as literal `"0"` in C++ vs `"0.000000"` in Go.

---

## RPC-F12 dxMakeOrder NO_SERVICE_NODE message arg

**VERDICT: CONFIRMED**

- C++: dxMakeOrder's `checkCreateParams` switch has **no** NO_SERVICE_NODE case
  (rpcxbridge.cpp:1001-1038; checkCreateParams can only return
  INVALID_CURRENCY/NO_SESSION/INSUFFICIENT_FUNDS/SUCCESS, xbridgeapp.cpp:2545-2557),
  and the real-path `sendXBridgeTransaction` failure falls to the `default`:
  `makeError(statusCode, __FUNCTION__)` — **bare, no argument** (rpcxbridge.cpp:1071-1073;
  sendXBridgeTransaction returns NO_SERVICE_NODE at xbridgeapp.cpp:1515/1525).
  Resulting wire text: `"Could not find a service node with required services: "`
  (xbridgeerror.cpp:67-68). dxMakePartialOrder's `NO_SERVICE_NODE` case *does*
  pass the pair `fromCurrency + "/" + toCurrency` (rpcxbridge.cpp:3153-3155) but is
  unreachable from checkCreateParams; its live send-path default is also bare
  (:3193).
- Go: `MakeOrder` (used by both dxMakeOrder and dxMakePartialOrder) returns
  `makeError(errNoServiceNode, "dxMakeOrder", p.Maker+"/"+p.Taker)` when no hub is
  found (node.go:916, 922) → wire text
  `"Could not find a service node with required services: LTC/BLOCK"`.

### Corrected statement

C++ dxMakeOrder (and the reachable dxMakePartialOrder path) emit NO_SERVICE_NODE
with a bare message (no pair) via the default makeError (rpcxbridge.cpp:1072);
Go emits the pair `"LTC/BLOCK"` appended (node.go:916, 922).

---

## Methods whose claims were substantially corrected

1. **RPC-F01** — C++ does NOT throw for param-count errors on dxGetOrderFills /
   dxGetOrders / dxGetOrder / dxGetLocalTokens / dxLoadXBridgeConf /
   dxGetNetworkTokens / dxGetTokenBalances / dxGetMyOrders / dxGetOrderBook /
   dxCancelOrder / dxGetLockedUtxos: those return `{error,code:1025,name}` in
   `result` (matching Go's channel). The envelope-throw divergence is real only
   for the make/take/tradingdata/split/utxo handlers and for param-*type* errors
   everywhere; the envelope code is **-1** (RPC_MISC_ERROR, help text) or **-3**
   (RPC_TYPE_ERROR), never 1025.
2. **RPC-F02** — affected methods are dxGetOrder / dxCancelOrder / dxGetUtxos /
   dxSplitAddress / dxSplitInputs; dxGetOrderBook and dxGetTokenBalances emit no
   NO_SESSION in Go, and dxGetLockedUtxos never surfaces the "dx"-named error.
3. **RPC-F03** — C++ order is std::map over `uint256` (typedef at rpcxbridge.cpp:42,
   not xbridgedef.h:29); substance confirmed.
4. **RPC-F04** — boundary is (60s, 61s): C++ includes via second-truncation; both
   sides include ≤60s and exclude ≥61s.
5. **RPC-F13** — confirmed; the dust band that diverges is `[55…5459]` 1e6-units with
   default dust (5460), Go being ~100× more lenient.
6. **RPC-F14** — confirmed; negative amounts error on both sides (different channel),
   only explicit `"0"` diverges (C++ errors, Go full-takes).
7. **RPC-F09** — confirmed with the note that dxGetOrders/dxGetOrder use updated-then-
   created on the C++ side too, so the swap is dxMakeOrder/dxMakePartialOrder-
   specific; the literal `"0"` is emitted in both C++ dryrun and success branches.
8. **RPC-F12** — confirmed; the C++ pair-arg case (rpcxbridge.cpp:3153-3155) is dead
   code on the checkCreateParams path.

Output file: `docs/audit/verify/verify_a.md`
