## RPC CONFORMANCE CARDS — GROUP 4 (dxGetMyPartialOrderChain, dxPartialOrderChainDetails, dxGetLockedUtxos, dxFlushCancelledOrders, dxGetTradingData/gettradingdata)

### dxGetMyPartialOrderChain
REF: rpcxbridge.cpp:2179 | CAND: api/handlers.go:869

PARAMS:
- order_id | string (hex) | required | — | REF: arity gate `fHelp || params.empty() || params.size() > 1` throws the help string (rpcxbridge.cpp:2180-2181), surfaced by the RPC layer as an *envelope* error `JSONRPCError(RPC_MISC_ERROR, what())` (src/rpc/server.cpp:584-586); `RPCTypeCheck(params,{VSTR})` (rpcxbridge.cpp:2271); then `uint256S(params[0].get_str())` and `IsNull()` → 1025 "Invalid parameters: bad order id" (rpcxbridge.cpp:2272-2274). CAND: `strParam(0)` failure (missing OR non-string) → 1025 result-error "(order_id)" (handlers.go:870-872); id validated by `orderIDKey` which requires **exactly 64 hex chars** (response.go:522-528, 504-517) → 1025 "bad order id" (handlers.go:877-879); params[1..] are silently ignored. DIVERGENT: (a) missing/extra-param errors ride the envelope in REF vs result-1025 in CAND; (b) id-shape gate stricter in CAND — C++ `base_blob::SetHex` parses 1..63-hex inputs (non-null) and truncates ≥64 (uint256.cpp:24-51), so only empty/no-hex input triggers "bad order id" in REF; (c) extra params never rejected in CAND.

ERRORS:
- 1025 | "Invalid parameters: bad order id" | uint256S null / not-64-hex | REF trigger (rpcxbridge.cpp:2274) vs CAND trigger (handlers.go:879): text MATCH, trigger DIVERGENT (CAND rejects short/odd-length hex that REF accepts).
- (missing/extra params) | REF throws → envelope RPC_MISC_ERROR (server.cpp:584-586) | CAND returns result `{error:"Invalid parameters: (order_id)", code:1025, name:"dxGetMyPartialOrderChain"}` (handlers.go:872). DIVERGENT transport + text.

SUCCESS SHAPE: array of order objects. REF insertion order (rpcxbridge.cpp:2298-2319): id, maker, maker_size, maker_address, taker, taker_size, taker_address, updated_at, created_at, order_type, partial_minimum, partial_orig_maker_size, partial_orig_taker_size, partial_repost, partial_parent_id, status. CAND `orderDetailResult` (response.go:26-54) marshals: id, maker, maker_size, taker, taker_size, updated_at, created_at, order_type, partial_minimum, partial_orig_maker_size, partial_orig_taker_size, partial_repost, partial_parent_id, status, maker_address, taker_address. DIVERGENT key order — maker_address/taker_address moved from positions 4/7 (REF) to the END (CAND, response.go:52-53).
- id | string hex | non-null | MATCH (REF GetHex rpcxbridge.cpp:2299; CAND orderIDString order.go:182).
- maker / taker | string | non-null | MATCH (REF from/toCurrency rpcxbridge.cpp:2302,2306; CAND order.go:183,185).
- maker_size / taker_size | fixed 6-dp string | non-null | MATCH (REF xBridgeStringValueFromAmount xutil.cpp:202-207 → %.6f(amt/1e6 + 1e-8), CAND formatXAmount response.go:237-241 — byte-identical for integer base units; the +1e-8 bump never crosses the 6dp rounding boundary).
- maker_address / taker_address | string | non-null ("" if no connector) | VALUE MATCH, ORDER DIVERGENT (see above).
- updated_at / created_at | ISO8601(ms,Z) | non-null | MATCH (REF iso8601(txtime/created) rpcxbridge.cpp:2310-2311 = xutil.cpp:185-200; CAND iso8601(Updated/Created) order.go:187-188).
- order_type | "partial"/"exact" | non-null | MATCH (REF orderType() xbridgetransactiondescr.h:403-407; CAND orderTypeString order.go:189).
- partial_minimum / partial_orig_maker_size / partial_orig_taker_size | 6-dp string | non-null | MATCH (REF rpcxbridge.cpp:2314-2316; CAND order.go:190-192).
- partial_repost | bool | non-null | MATCH (REF `t->repostOrder` rpcxbridge.cpp:2317; CAND o.PartialRepost order.go:193).
- partial_parent_id | string | "" when none | MATCH (REF parseParentId rpcxbridge.cpp:56-60; CAND parentIDString response.go:413-418).
- status | string | non-null | MATCH (REF strState xbridgetransactiondescr.h:665-688; CAND statusString response.go:424-461).
- Empty chain → `[]`: REF empty UniValue VARR (rpcxbridge.cpp:2276) vs CAND `[]interface{}{}` (handlers.go:882-884). MATCH (both `[]`, never null).
- Chain membership/order: REF `getPartialOrderChain` (xbridgeapp.cpp:3913-3991) includes only orders with `isLocal()` (from+to addresses set, xbridgetransactiondescr.h:646-650) AND (parent set OR partial-allowed) (xbridgeapp.cpp:3922,3934), iterating live `transactions()` + `history()`, deduped by `seen` (rpcxbridge.cpp:2281-2287) and FINAL-sorted by `created` ascending (xbridgeapp.cpp:3985-3988). CAND `partialOrderChain` (handlers.go:900-961): live idx includes ALL store orders with NO isLocal/partial gate (handlers.go:901-904), history gated on `Mine` (handlers.go:905-908), chain order = parent-linkage walk + DFS over a `map[string][]string` children index (handlers.go:919-960) — **no created-time sort**. DIVERGENT membership (remote/addressless orders may appear in CAND) and ordering (multi-child chains / equal-timestamp chains can reorder).

SIDE EFFECTS: none (read-only query of App maps). REF vs CAND MATCH.

VECTORS (>=3):
1. success, single order chain (order_id = 64-hex, valid) → REF `[{"id":...,"maker":"SYS","maker_size":"100.000000","maker_address":"SVT...","taker":"LTC","taker_size":"10.500000","taker_address":"LVv...","updated_at":"...","created_at":"...","order_type":"partial","partial_minimum":"10.000000","partial_orig_maker_size":"100.000000","partial_orig_taker_size":"10.500000","partial_repost":true,"partial_parent_id":"","status":"open"}]` → CAND identical values but `maker_address`/`taker_address` appear AFTER `status`. DIVERGENT key order.
2. error: `["xyz"]` (no hex digits) → REF `{error:"Invalid parameters: bad order id", code:1025, name:"dxGetMyPartialOrderChain"}` → CAND identical. MATCH. BUT `["<32 hex chars>"]` → REF parses a non-null uint256 and proceeds; CAND returns 1025 "bad order id". DIVERGENT trigger.
3. edge: unknown but well-formed 64-hex id → REF `[]`; CAND `[]`. MATCH.
4. edge: `[]` (no params) → REF envelope RPC error (MISC_ERROR, help text); CAND result `{error:"Invalid parameters: (order_id)", code:1025, name:"dxGetMyPartialOrderChain"}`. DIVERGENT transport.

VERDICT: DIVERGENT — (1) missing/extra-param error transport (C++ throws envelope vs CAND result-1025); (2) id-shape gate (CAND requires exactly 64 hex; C++ uint256S accepts partial/truncates); (3) JSON key order (maker_address/taker_address at end in CAND); (4) chain membership filter (C++ isLocal + partial-parent gate absent in CAND) and missing C++ created-time sort in CAND.

### dxPartialOrderChainDetails
REF: rpcxbridge.cpp:2327 | CAND: api/handlers.go:963

PARAMS:
- order_id | string (hex) | required | — | REF: same arity gate (throws, rpcxbridge.cpp:2328), RPCTypeCheck VSTR (2411), `uint256S(...).IsNull()` → 1025 "Invalid parameters: bad order id" (2413-2414). CAND: `strParam(0)` fail → 1025 "(order_id)" (handlers.go:964-967); `orderIDKey` fail → 1025 "Invalid parameters: Invalid order id [<id>]" (handlers.go:969-972). DIVERGENT: (a) missing-param transport (envelope vs result); (b) error text ("bad order id" vs "Invalid order id [<id>]"); (c) trigger (64-hex vs uint256S tolerance).

ERRORS:
- 1025 | "Invalid parameters: bad order id" | bad id | REF (rpcxbridge.cpp:2414) vs CAND "Invalid parameters: Invalid order id [<id>]" (handlers.go:971). DIVERGENT text (+ trigger width as above).
- (missing param) | REF throws (envelope) | CAND 1025 "(order_id)" result (handlers.go:966). DIVERGENT.

SUCCESS SHAPE: object. REF insertion order (rpcxbridge.cpp:2460-2480): first_order_id, maker, maker_address, taker, taker_address, partial_minimum, partial_orig_maker_size, partial_orig_taker_size, first_order_time, last_order_time, total_reported_sent, total_reported_received, total_reported_notsent, total_reported_notreceived, total_orders_open, total_orders_finished, total_orders_canceled, orders, p2sh_deposits, p2sh_deposits_counterparty. CAND builds a `map[string]interface{}` (handlers.go:1015-1036) → Go marshals keys **alphabetically**. DIVERGENT key order (CAND fully alphabetized).
- first_order_id | string | non-null | MATCH (REF firstOrderId.GetHex rpcxbridge.cpp:2461; CAND orderIDString(first.ID) handlers.go:1016).
- maker / maker_address / taker / taker_address | string | non-null (addr "" without connector) | MATCH values (REF 2462-2465; CAND 1017-1020).
- partial_minimum / partial_orig_maker_size / partial_orig_taker_size | 6-dp string | non-null | MATCH (REF 2466-2468 from `firstOrder`; CAND 1021-1023).
- first_order_time | ISO8601(ms) | non-null | MATCH (REF iso8601(firstOrder->created) 2433/2469; CAND iso8601(first.Created) 1024).
- last_order_time | ISO8601(ms) | non-null | MATCH (REF iso8601(lastOrder->txtime) 2434/2470; CAND iso8601(last.Updated) 1025).
- total_reported_sent/received/notsent/notreceived | 6-dp string | non-null | MATCH (REF 2471-2474; CAND formatXAmount 1026-1029).
- total_orders_open/finished/canceled | int | non-null | MATCH (REF 2475-2477; CAND int 1030-1032).
- orders | array of string | non-null | MATCH (REF 2455/2478 one id per chain order; CAND 1005/1033).
- p2sh_deposits / p2sh_deposits_counterparty | array of string | length must equal chain length | DIVERGENT — REF pushes `t->binTxId` / `t->oBinTxId` UNCONDITIONALLY for every chain order, including empty-string entries (rpcxbridge.cpp:2456-2457 → "1 for each order", doc 2400-2401); CAND appends ONLY when `BinTxId`/`OBinTxId` is non-empty (handlers.go:1008-1013), so the arrays can be SHORTER than the chain and later non-empty values shift index.
- State aggregation (REF 2440-2454 vs CAND 986-1004): trFinished → sent/received/finished; every other state (incl. trCancelled) → notsent/notreceived; `state <= trPending` (-1..2, i.e. expired/new/offline/open) → open; trCancelled → canceled. CAND replicates with `stateOrdinal(t.Status) <= 2` inside the non-finished/non-canceled branch (handlers.go:999-1003; swap/state.go:78-95). Aggregation MATCH. (C++ also computes totalInProgress 3..8 — not emitted; CAND omits. No output impact.)
- Empty chain → `{}`: REF `return UniValue(UniValue::VOBJ)` (rpcxbridge.cpp:2418-2419); CAND `map[string]interface{}{}` (handlers.go:975-977). MATCH.

SIDE EFFECTS: none. MATCH.

VECTORS (>=3):
1. success, 3-order chain (middle order has no deposit) → REF `{first_order_id, maker, maker_address, taker, taker_address, partial_minimum, partial_orig_maker_size, partial_orig_taker_size, first_order_time, last_order_time, total_reported_sent:"0.200000", ..., total_orders_open:0, total_orders_finished:2, total_orders_canceled:1, orders:[id1,id2,id3], p2sh_deposits:["d1","","d3"], p2sh_deposits_counterparty:[...]}` in that key order → CAND identical VALUES but (a) keys alphabetized, (b) `p2sh_deposits` = ["d1","d3"] (empty dropped → length 2 ≠ chain length 3). DIVERGENT.
2. empty chain (valid unknown id) → REF `{}`; CAND `{}`. MATCH.
3. error: bad id → REF `{error:"Invalid parameters: bad order id", code:1025, name:"dxPartialOrderChainDetails"}`; CAND `{error:"Invalid parameters: Invalid order id [<id>]", code:1025, name:"dxPartialOrderChainDetails"}`. DIVERGENT text.
4. edge aggregation: chain [finished, canceled, open] → REF totals sent=finished.from, received=finished.to, notsent=canceled.from+open.from, notreceived=canceled.to+open.to, open=1, finished=1, canceled=1 → CAND identical. MATCH.

VERDICT: DIVERGENT — (1) p2sh_deposits / p2sh_deposits_counterparty omit empty per-order entries (CAND lengths < chain length; C++ always "1 for each order"); (2) JSON key order alphabetized (Go map) vs C++ insertion order; (3) bad-id error text differs ("bad order id" vs "Invalid order id [<id>]"); (4) missing-param error transport (envelope vs result 1025); (5) 64-hex vs uint256S id gate.

### dxGetLockedUtxos
REF: rpcxbridge.cpp:2571 | CAND: api/handlers.go:1044

PARAMS:
- id | string (hex) | optional | — (omitted → all locked utxos) | REF: `params.size() > 1` → 1025 "Invalid parameters: Too many parameters." (rpcxbridge.cpp:2612-2619); Exchange-node gate `!e.isStarted()` → 1029 (2621-2629); `id = uint256S(params[0])` when present (2633-2634). CAND: `len(params) > 1` → 1025 same text (handlers.go:1045-1047); node gate `h.Node==nil || cfg()==nil || len(ExchangeWallets)==0` → 1029 (handlers.go:1052-1054); id via `orderIDKey` (1086-1089). DIVERGENT: (a) 1029 trigger semantics (C++ Exchange::isStarted vs CAND "has ExchangeWallets configured"); (b) id gate 64-hex vs uint256S.

ERRORS:
- 1025 | "Invalid parameters: Too many parameters." | >1 param | REF (2615) vs CAND (handlers.go:1046): text MATCH, transport MATCH (both result-errors).
- 1029 | "Blocknet is not running as an exchange node" | exchange not started | REF (xbridgeerror.cpp:61-62; trigger 2621-2623) vs CAND (response.go:149-150 ignores arg → same text; trigger handlers.go:1052-1054). Text/code MATCH, trigger DIVERGENT.
- 1021 | "Transaction <id> not found" | unknown id | REF arg `id.GetHex()` via `getUtxoItems` false (2637-2645) OR pendingTx+acceptedTx both invalid (2660-2670); CAND arg = raw param via `Store.Get` nil (handlers.go:1090-1093). Text MATCH; trigger DIVERGENT — CAND never runs the pending/accepted validity check and returns an empty result when the order exists in the store but has no locked utxos.
- (bad id shape) | CAND-only 1025 "Invalid parameters: Invalid order id [<id>]" (handlers.go:1088) | REF uint256S tolerates short hex. DIVERGENT.

SUCCESS SHAPE:
- No-id → `{"all_locked_utxo": [str, ...]}`. REF (rpcxbridge.cpp:2652-2658); CAND (handlers.go:1084). Key/type MATCH; empty → REF `[]` (empty Array) / CAND `[]` (make([]string,0)). MATCH.
- With-id → `{"id": <hex>, <key>: [str, ...]}`. REF key = `pendingTx->a_currency()` if pending valid, else `acceptedTx->a_currency()+"_and_"+b_currency()` (rpcxbridge.cpp:2672-2677); CAND key = `o.FromCurrency` unless `stateOrdinal(o.Status) >= DescrAccepting(3)` then `FromCurrency+"_and_"+ToCurrency` (handlers.go:1100-1103,1140-1143). DIVERGENT key-decision logic (C++ map membership vs CAND status ordinal).
- `id` value: REF `id.GetHex()` (normalized lowercase 64-hex, 2672); CAND echoes the RAW param string (handlers.go:1141). DIVERGENT for non-normalized caller input.
- utxo string format: REF `UtxoEntry::toString()` = `txId:vout:amount:address` where `amount` is the raw double at **default ostream precision (6 significant digits)** (xbridgewalletconnector.cpp:25-30). CAND = `TxID:vout:amtStr:address` with `amtStr` = `formatXAmount` (fixed 6-dp, response.go:237-241) or `coins.FormatAmount` (native coin decimals, trimmed; coins/amount.go:65-80) (handlers.go:1077-1081,1113-1117,1133-1136). DIVERGENT amount encoding — e.g. 123456789 base: REF "1.23457" (6 sig) vs CAND "1.23456789" (8-dp BLOCK).
- Data source: REF returns the Exchange's **tracked** locked-utxo set (`m_utxoTxMap` / `m_utxoItems`, xbridgeexchange.cpp:275-296); CAND scans live wallet `ListUnspent` filtered by the store's reserved-key set (`LockedUtxoInfo` store.go:402-406), silently producing `[]` when a connector is missing (handlers.go:1062-1069,1104-1119). DIVERGENT source.
- JSON key order: REF `{"id":..., <currency>:...}` insertion (2672-2677); CAND Go map → alphabetical (handlers.go:1140-1143). DIVERGENT.

SIDE EFFECTS: none (read-only). MATCH.

VECTORS (>=3):
1. no-id, 2 locked utxos → REF `{"all_locked_utxo":["<tx>:0:1.5:<addr>","<tx>:1:0.123457:<addr>"]}` (default-float amounts) → CAND `{"all_locked_utxo":[...]}` with `1.50000000`-style native-decimal / fixed-6 amounts. DIVERGENT amount strings.
2. error: 2 params → both `{error:"Invalid parameters: Too many parameters.", code:1025, name:"dxGetLockedUtxos"}`. MATCH.
3. error: well-formed unknown id → REF `{error:"Transaction <id> not found", code:1021, name:"dxGetLockedUtxos"}`; CAND 1021 same text. MATCH (id in neither book nor utxo map).
4. edge: order exists in store but has NO locked utxos (or connector down) → REF 1021 (getUtxoItems false / tx not pending+accepted); CAND `{"id":"<id>","<cur>":[]}`. DIVERGENT.
5. edge: not an exchange node → both 1029; REF text "Blocknet is not running as an exchange node"; CAND same text (arg ignored). Trigger differs (C++ isStarted vs CAND wallet-list emptiness). DIVERGENT trigger.

VERDICT: DIVERGENT — (1) utxo string amount encoding (C++ default-float 6-sig vs CAND fixed-6/native-decimal); (2) 1021 trigger set (C++ getUtxoItems + pending/accepted validity; CAND store-lookup only → empty result instead of error); (3) per-order key selection logic (C++ pending/accepted map membership vs CAND status ordinal); (4) `id` echoed un-normalized in CAND; (5) JSON key order (map-sorted); (6) 1029 trigger semantics; (7) data source (tracked locked set vs live wallet scan with silent skips).

### dxFlushCancelledOrders
REF: rpcxbridge.cpp:1404 | CAND: api/handlers.go:1150

PARAMS:
- ageMillis | number | optional | 0 | REF: `params.size()==0 → 0; ==1 → params[0].get_int(); >1 → -1` then `< 0` → 1025 (rpcxbridge.cpp:1456-1464). CAND switch identical (handlers.go:1153-1168). MATCH arity logic; DIVERGENT type-tolerance: REF `get_int()` on a JSON string throws → envelope error; CAND `intParam` deliberately accepts numeric strings (dispatch.go:114-130).

ERRORS:
- 1025 | "Invalid parameters: ageMillis must be an integer >= 0" | ageMillis<0 OR >1 param | REF (1460-1464) vs CAND (1160-1168). Text/code/transport MATCH.

SUCCESS SHAPE: `{ageMillis, now, durationMicrosec, flushedOrders:[{id,txtime,use_count},...]}`.
- REF insertion order (1474-1489): ageMillis, now, durationMicrosec, flushedOrders. CAND `map[string]interface{}` → alphabetical: ageMillis, durationMicrosec, flushedOrders, now (handlers.go:1180-1185). DIVERGENT key order.
- ageMillis | int | non-null | MATCH (REF int from get_int 1456-1458; CAND int 1153-1168).
- now | ISO8601(ms,Z) of `microsec_clock::universal_time()` | non-null | MATCH (REF iso8601(now) 1470/1476 = xutil.cpp:185-200; CAND iso8601(NowMicro()) 1169/1182).
- durationMicrosec | int | non-null | DIVERGENT width — REF `static_cast<int>(micros.total_microseconds())` (int32, 1477); CAND `int64(dur)` (1183). Values > 2^31 µs (~35.8 min) wrap in C++.
- flushedOrders | array | non-null (`[]` when none) | array itself MATCH; membership/order DIVERGENT (below).
  - id | string | non-null | MATCH (REF `it.id.GetHex()` 1483; CAND `c.ID` 1175).
  - txtime | ISO8601(ms) | non-null | MATCH (REF iso8601(it.txtime) 1484; CAND iso8601(c.Txtime) 1176).
  - use_count | int | non-null | DIVERGENT — REF `ptr.use_count()` = std::shared_ptr<TransactionDescr> reference count (xbridgeapp.cpp:1345; xbridgedef.h:44; xbridgeapp.h:158-166), a dynamic debug value; CAND hardcodes `UseCount: 1` in `RecordCancelled` (store.go:479-484).
  - ordering: REF iterates `std::map<uint256,...>` for BOTH `m_transactions` and `m_historicTransactions` → sorted by numeric id (xbridgeapp.cpp:1336-1351); CAND iterates the append-only `s.cancelled` slice in insertion order (store.go:504-510). DIVERGENT ordering.

SIDE EFFECTS:
- REF actually DELETES the cancelled orders from the order book AND history: `mp->erase(it++)` on `m_transactions` and `m_historicTransactions` for `state==trCancelled && txtime < keepTime` (xbridgeapp.cpp:1340-1351). CAND flushes only the separate `s.cancelled` ledger — the live orders / history are NOT touched, and `s.cancelled` is populated only by `RecordCancelled` (node.go:1731 on the local cancel path; store.go:479-484). DIVERGENT: CAND never flushes the actual book, and only locally-cancelled orders ever appear in the output (C++ also surfaces remote-cancelled / historic-cancelled orders).
- `keepTime` math: REF `keepTime = microsec_clock::universal_time() - minAge(ms)` (1335); CAND `keepTime = NowMicro() - minAgeMillis*1000` with uint64 underflow clamp (store.go:494-501). Units MATCH (microseconds); clamp is a CAND-only guard (REF ptime arithmetic won't underflow the same way).

VECTORS (>=3):
1. success ageMillis=0 (2 cancelled orders) → REF prunes all cancelled from BOTH maps, output ordered by numeric id: `{ageMillis:0, now:"...", durationMicrosec:0, flushedOrders:[{id,txtime,use_count:2},{id,txtime,use_count:1}]}` (refcounts vary) → CAND flushes `s.cancelled` in insertion order with use_count always 1. DIVERGENT membership-source, ordering, use_count, key order.
2. error `["-1"]` → both `{error:"Invalid parameters: ageMillis must be an integer >= 0", code:1025, name:"dxFlushCancelledOrders"}`. MATCH.
3. error 2+ params → both 1025 same text. MATCH.
4. edge `["600000"]` (numeric STRING) → REF `get_int()` on VSTR throws → envelope RPC error; CAND parses and flushes. DIVERGENT.
5. edge large duration > 2^31 µs → REF `durationMicrosec` int32 wraps; CAND int64 exact. DIVERGENT (edge only).

VERDICT: DIVERGENT — (1) flush target (CAND prunes the `s.cancelled` ledger, never the order book/history, and only locally-cancelled orders appear — C++ erases from both App maps); (2) `use_count` hardcoded 1 vs dynamic shared_ptr refcount; (3) flushedOrders ordering (insertion vs id-sorted map iteration); (4) JSON key order (map-sorted); (5) `durationMicrosec` int32 vs int64; (6) numeric-string param accepted by CAND.

### dxGetTradingData / gettradingdata
REF: rpcxbridge.cpp:2782 (dxGetTradingData) / :2682 (gettradingdata) | CAND: api/handlers.go:1192

PARAMS:
- blocks | number | optional | 43200 | REF `countOfBlocks = params[0].get_int()` bounds the on-chain scan (rpcxbridge.cpp:2717-2726, 2845-2854), scanning back until `blockTime <= timeBegin - 30*24*60*60` OR count exhausted (2862-2863). CAND reads nothing (handlers.go:1192-1216) — `blocks` is accepted but never bounds anything. DIVERGENT.
- errors | bool | optional | false | REF `showErrors = params[1].get_bool()` toggles parse-error records (2721-2722, 2879-2887). CAND ignores it. DIVERGENT.
- arity: REF `params.size() > 2` throws (2684, 2784); typecheck `{VNUM,VBOOL}` (2 params) / `{VNUM}` (1) (2720-2725, 2848-2853). CAND `len(params) > 2` → 1025 result-error `"(blocks, default=43200)[optional] (errors, default=false)[optional]"` (handlers.go:1198-1200), no type checking. DIVERGENT transport + text.
- Note: the task's "`n`/`data`" param names do NOT exist in either surface — the C++ commands take only `(blocks, errors)` (2684-2726, 2784-2854) and CAND matches that arity; there is no `n` or `data` parameter to compare (TBD/absent).

ERRORS:
- 1025 | "Invalid parameters: (blocks, default=43200)[optional] (errors, default=false)[optional]" | >2 params | CAND only (handlers.go:1199); REF throws (envelope). DIVERGENT transport + text.
- No other business errors in either implementation (REF scan failures `continue` silently, 2737-2742; CAND has none).

SUCCESS SHAPE: array of records. REF Valid record insertion order (2889-2898): timestamp, fee_txid, nodepubkey, id, taker, taker_size, maker, maker_size. CAND per-record `map[string]interface{}` (handlers.go:1205-1214) → keys alphabetized: fee_txid, id, maker, maker_size, nodepubkey, taker, taker_size, timestamp. DIVERGENT key order.
- timestamp | int (seconds) | non-null | units MATCH, source DIVERGENT — REF `block.GetBlockTime()` on-chain (2871/2890); CAND `int64(f.Time/1e6)` local-fill microseconds→seconds (handlers.go:1206).
- fee_txid | string (64-hex) | non-null | DIVERGENT — REF the real on-chain trade-fee tx hash `tx->GetHash().GetHex()` (2874/2891); CAND hardcodes `""` (handlers.go:1207).
- nodepubkey | string | non-null | DIVERGENT — REF `snode_pubkey` (address of the SN that received the fee, decoded from the multisig, 2889-2892 + TxOutToCurrencyPair rpcxbridge.cpp:85-95); CAND hardcodes `""` (handlers.go:1208).
- id | string | non-null | MATCH-ish — REF `p.xid()` (parsed on-chain record id, 2893); CAND `f.ID` (local fill id, 1209). Source differs.
- taker / maker | string | non-null | MATCH (REF currency strings 2894,2896; CAND f.Taker/f.Maker 1210,1212).
- taker_size / maker_size | JSON number | non-null | REF `p.from.amount<double>()` / `p.to.amount<double>()` = `boost::rational_cast<double>(amount/1e6)` (2895,2897; currency.h:141-143); CAND `strconv.ParseFloat(f.TakerSize/f.MakerSize,64)` (handlers.go:1203-1204,1211,1213). Both JSON numbers, values near-equal; TBD binary-exact equality.
- showErrors=true path: REF appends `{timestamp, fee_txid, id:<error string>}` records for txs whose OP_RETURN data fails validation (2879-2887, 127-139); CAND has no such records. DIVERGENT missing path.
- Empty result → REF `[]` (uret(records) 2907); CAND `[]` (make([]interface{},0) 1201). MATCH.

SIDE EFFECTS:
- REF read-only chain scan under `LOCK(cs_main)` (2856). CAND read-only store read. MATCH (both read-only; data sources differ — REF on-chain trade-fee records vs CAND local `Store.Fills()`, store.go:300-310, whose writers are the swap engine only).

VECTORS (>=3):
1. success (one on-chain trade) → REF `[{"timestamp":1559970139,"fee_txid":"4b409e...fefb","nodepubkey":"Bqtm...","id":"9eb57b...","taker":"BLOCK","taker_size":0.001111,"maker":"SYS","maker_size":0.001000}]` → CAND `[{"fee_txid":"","id":"...","maker":"SYS","maker_size":0.001,"nodepubkey":"","taker":"BLOCK","taker_size":0.001111,"timestamp":...}]` — fee_txid/nodepubkey empty, keys sorted, record from local fills. DIVERGENT.
2. params `[43200]` (n/blocks count) → REF scans at most 43200 blocks (plus 30-day window); CAND returns all local fills regardless. DIVERGENT.
3. params `[43200,true]` (errors=true) → REF appends error records for malformed trade txs; CAND returns no error records. DIVERGENT.
4. gettradingdata (lowercase): REF is a separate registered command (rpcxbridge.cpp:3520) returning a DIFFERENT schema — `{timestamp, txid, to:<snode_pubkey>, xid, from, fromAmount, to:<currency>, toAmount}` with a DUPLICATE "to" key (2761-2770) and Error-case `{timestamp, txid, xid}` (2754-2758). CAND dispatch has NO `gettradingdata` entry (dispatch.go:38-63 — only `dxGetTradingData` at :58) → server returns envelope `{error:{code:-32601,message:"Method not found: gettradingdata"}}` (server.go:168-177). DIVERGENT — command absent. (Go's `getnetworkinfo` at dispatch.go:62/handlers.go:1721 is a wallet-version RPC, unrelated to trading data.)

VERDICT: DIVERGENT — (1) `gettradingdata` (lowercase) missing from the Go dispatch map → envelope -32601 Method not found (REF registers it, rpcxbridge.cpp:3520); (2) `fee_txid` and `nodepubkey` always empty strings in CAND (REF on-chain values); (3) data source is local `Store.Fills()`, not the on-chain block scan — `blocks`/`errors` params ignored (no 43200 bound, no 30-day window, no error records); (4) JSON key order (map-sorted); (5) >2-param error transport/text (CAND result-1025 vs REF envelope throw).
