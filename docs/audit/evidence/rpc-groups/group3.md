## RPC CONFORMANCE CARDS — GROUP 3 (dxCancelOrder, dxGetOrderHistory, dxGetOrderBook, dxGetTokenBalances, dxGetMyOrders)

### dxCancelOrder
REF: rpcxbridge.cpp:1297 | CAND: api/handlers.go:408

PARAMS:
- id | string (hex) | required | — | REF: arity gate `params.size() != 1` → 1025 "(id)" (rpcxbridge.cpp:1347-1350); id validated by `uint256S(sid).IsNull()` → 1025 (rpcxbridge.cpp:1354-1355); stored as `uint256`. CAND: `len(params) != 1` → same 1025 "(id)" (handlers.go:409-411); id validated by `orderIDKey` (64-hex, response.go:522-528) → 1025 "Invalid order id [<id>]" (handlers.go:414-417). MATCH (CAND is stricter on the id shape: requires exactly 64 hex chars, C++ uint256S tolerates shorter/odd-length inputs up to a point — minor).

ERRORS:
- 1025 | "Invalid parameters: (id)" | arity != 1 | REF (rpcxbridge.cpp:1349) vs CAND (handlers.go:410). MATCH.
- 1025 | "Invalid parameters: Invalid order id [<sid>]" | bad id | REF (rpcxbridge.cpp:1355) vs CAND "Invalid order id [<id>]" (handlers.go:416). MATCH.
- 1021 | "Transaction <id> not found" | unknown order | REF `App::transaction(id)` (rpcxbridge.cpp:1362) vs CAND `Store.Get` (handlers.go:420). MATCH.
- 1028 | "invalid transaction state The order is already <strState>" | `state >= trCreated` (=6) | REF (rpcxbridge.cpp:1365-1368; enum xbridgetransactiondescr.h:43-61) vs CAND `stateOrdinal(o.Status) >= 6` (handlers.go:424-426; swap/state.go:86 DescrCreated=6, response.go:468-470). MATCH (ordinals agree).
- 1018 | "No session for currency <cur>" | missing wallet for maker/taker | REF checks connFrom/connTo AFTER `cancelXBridgeTransaction` ran (rpcxbridge.cpp:1376-1384) with name="dxCancelOrder"; CAND checks both connectors BEFORE `Node.CancelOrder` (handlers.go:428-433) and returns the connector() error with name="dx" (handlers.go:198-207). DIVERGENT: (a) `name` field is "dx" not "dxCancelOrder"; (b) check order vs the cancel side-effect — REF cancels the order and THEN errors, CAND errors without cancelling.
- Cancel-failure path: REF `cancelXBridgeTransaction` returns TRANSACTION_NOT_FOUND (non-local order), INVALID_STATE (`state > trCreated`), NO_SESSION (from-connector missing) → makeError(res,__FUNCTION__) (xbridgeapp.cpp:2473-2501; rpcxbridge.cpp:1370-1374). CAND `Node.CancelOrder` returns errNoServiceNode 1032 (node.go:848-853), errBadRequest 1004 "Bad Request no active session for order", errUnknown 1002 (node.go:1773-1790), errTxNotFound 1021. DIVERGENT error-code set for the cancel-failure path; CAND has no isLocal guard (REF returns 1021 for a non-local order; CAND proceeds to broadcast).

SUCCESS SHAPE: 11 fields in insertion order (REF rpcxbridge.cpp:1386-1400 vs CAND cancelOrderResult response.go:67-79):
- id | string | non-null | MATCH
- maker | string | non-null | MATCH
- maker_size | string | non-null | MATCH (see amount note)
- maker_address | string | non-null | MATCH (REF `connFrom->fromXAddr(tx->from)` rpcxbridge.cpp:1390 vs CAND stored `o.MakerAddress` order.go:279)
- taker | string | non-null | MATCH
- taker_size | string | non-null | MATCH
- taker_address | string | non-null | MATCH (REF rpcxbridge.cpp:1394 vs CAND order.go:282)
- refund_tx | string | non-null ("" when no deposit) | MATCH — REF `tx->refTx` (rpcxbridge.cpp:1395); CAND `o.RefundTx` (order.go:283, set on refund broadcast swap.go:378/480). Field PRESENT always in both. MATCH.
- updated_at | str ISO8601(ms) | non-null | MATCH (REF iso8601(tx->txtime) rpcxbridge.cpp:1397 vs CAND iso8601(o.Updated) order.go:284)
- created_at | str ISO8601(ms) | non-null | MATCH (REF rpcxbridge.cpp:1398 vs CAND order.go:285)
- status | string | non-null | MATCH (REF `tx->strState()` rpcxbridge.cpp:1400 vs CAND `statusString(o.Status)` order.go:286)
- No order_type / partial_* keys in either — MATCH (C++ emits none; CAND cancelOrderResult is a flat struct, not orderBase).
- Amount strings: REF `xBridgeStringValueFromAmount` = `%.6f(amount/1e6 + 1/::COIN)` (xutil.cpp:202-207, 223-227); CAND `formatXAmount` = exact integer division (response.go:237-241). Byte-identical for integer base units sampled (0.000001…1.23e14) — the +1e-8 bump never crosses the 6dp rounding boundary; TBD only for amounts within 1e-8 of a %.6f rounding boundary.

SIDE EFFECTS: (db write / P2P broadcast / state transition | REF vs CAND)
- REF: `cancelXBridgeTransaction(id, crRpcRequest)` → `session->sendCancelTransaction` (P2P cancel broadcast) + state change (xbridgeapp.cpp:2497-2500), executed BEFORE the connector NO_SESSION checks. CAND: `Node.CancelOrder` submits engine task → `sendCancelTransaction` broadcast, Store.Update status="canceled", RecordCancelled, best-effort refund broadcast, persist (node.go:1706-1740), gated on connectors BEFORE the call (handlers.go:428-433). MATCH on what happens; DIVERGENT on error-order vs side-effects (REF mutates then may error; CAND errors before mutating).

VECTORS (>=3):
1. success: order exists, state open, connectors present → REF `{id, maker, maker_size, maker_address, taker, taker_size, taker_address, refund_tx, updated_at, created_at, status:"canceled"}` (11 fields, no partial_*) → CAND byte-identical shape/order.
2. error: `["badid"]` → REF `{error:"Invalid parameters: Invalid order id [badid]", code:1025, name:"dxCancelOrder"}` → CAND identical.
3. error: unknown id → REF `{error:"Transaction <id> not found", code:1021, name:"dxCancelOrder"}` → CAND identical.
4. edge: order state = "created" (ordinal 6) → REF 1028 "The order is already created" → CAND identical (stateOrdinal>=6).
5. edge: connector missing for toCurrency → REF cancels the order then returns 1018 "No session for currency <to>" with name "dxCancelOrder"; CAND returns 1018 with name "dx" and does NOT cancel. DIVERGENT.

VERDICT: DIVERGENT — (1) connector NO_SESSION error `name` = "dx" (handlers.go:200) vs "dxCancelOrder"; (2) connector checks run BEFORE the cancel vs after in REF, changing side-effects on that error path; (3) cancel-failure error set differs (CAND 1004/1002/1032 vs REF 1018/1021/1028) and CAND lacks the isLocal→1021 guard; (4) id-format validation stricter in CAND (64-hex only).

### dxGetOrderHistory
REF: rpcxbridge.cpp:588 | CAND: api/handlers.go:446

PARAMS:
- maker | string | required | — | REF params[0] `get_str()`; CAND `strParam(0)`. MATCH.
- taker | string | required | — | MATCH.
- start_time | int64 (seconds) | required | — | REF `get_int64()` (rpcxbridge.cpp:655); CAND `int64Param` (handlers.go:452). MATCH (both seconds).
- end_time | int64 (seconds) | required | — | MATCH.
- granularity | int (seconds) | required | — | REF must be one of {60,300,900,3600,21600,86400} else error (xseries.h:91-92,119-129); CAND only requires `granularity > 0` (handlers.go:460-463). DIVERGENT.
- order_ids | bool | optional | false | REF `params.size()>5 && params[5].get_bool()` (rpcxbridge.cpp:657-659); CAND `boolParam(5,false)` (handlers.go:470-477). MATCH.
- with_inverse | bool | optional | false | REF (rpcxbridge.cpp:660-662); CAND (handlers.go:478-485). MATCH.
- limit | int | optional | 2147483647 (INT_MAX, xseries.h:40-50) | REF `IntervalLimit{params[7].get_int()}` clamps out-of-range to 0 → error (rpcxbridge.cpp:663-665); CAND NEVER reads params[7] (handlers.go:446-562). DIVERGENT (limit ignored).
- interval_timestamp (9th) | str | optional | "at_start" | REF code path is dead: `params.size()>8` is rejected by the arity gate `>8` (rpcxbridge.cpp:643); default AtStart always. CAND rejects >8 params. MATCH in effect.

ERRORS:
- 1025 | "Invalid parameters: (maker) (taker) (start time) (end time) (granularity) (order_ids, default=false)[optional] (with_inverse, default=false)[optional] (limit, default=2147483647)[optional]" | arity<5 or >8 | REF (rpcxbridge.cpp:644-650); CAND same text but "(limit)[optional]" — drops "default=2147483647" (handlers.go:448). MINOR text DIVERGENT.
- 1025 | "Invalid parameters: Start time too early." | start < 2018-02-25 (1519516800) | REF xseries.h:93,107-109; CAND hardcodes xSeriesEarliest=1519516800 (handlers.go:464-469). MATCH.
- 1025 | "Invalid parameters: granularity=N must be one of: 60,300,900,3600,21600,86400" | unsupported granularity | REF xseries.h:91-92; CAND does not implement (accepts any >0). DIVERGENT (missing path).
- 1025 | "Invalid parameters: Start time >= end time." | aligned period null (begin>=end) | REF xseries.h:94 (via period.is_null()); CAND returns `[]` (no error) for raw `end <= start` (handlers.go:487-489). DIVERGENT — CAND never emits this error, and C++ would only error when the ALIGNED period is null, not on raw end<=start.
- 1025 | "Invalid parameters: Start/end times are too large." | aligned end > now + 1 day | REF xseries.h:95 (oneDayFromNow); CAND missing. DIVERGENT (missing path).
- 1025 | "Invalid parameters: interval_limit must be in range 1 to 2147483647." | 8-param limit <1 or >INT_MAX | REF xseries.h:96-98; CAND ignores the param. DIVERGENT (missing path).
- 1002 | "Internal Server Error" / "unknown exception" | exception during query | REF try/catch (rpcxbridge.cpp:697-701); CAND none. DIVERGENT (missing path).

SUCCESS SHAPE: array of rows `[time, low, high, open, close, volume]` (+ `order_ids` array when order_ids=true) — REF (rpcxbridge.cpp:679-696), CAND (handlers.go:527-560).
- time | string ISO8601(ms, Z) | non-null | MATCH — bucket start = alignedStart + i*g (µs×1e6); REF `iso8601(x.timeEnd - granularity)` (rpcxbridge.cpp:680-686 + xseries.cpp:116-120), CAND `iso8601(uint64(bucketStart)*1e6)` (handlers.go:555). Same "YYYY-MM-DDTHH:MM:SS.000Z" names.
- low/high/open/close/volume | double | non-null | VALUE-close, ENCODING DIVERGENT — REF numbers are emitted as fixed-8 decimals: `uret()` renders through json_spirit `write_string(o, none, 8)` → `std::fixed setprecision(8)` (rpcxbridge.cpp:49-53, json_spirit_writer_template.h:193-196), e.g. `0.00000000`; CAND `xfloat` = shortest-roundtrip `FormatFloat('f',-1)` + ".0" suffix (handlers.go:22-32), e.g. `0.0`, `1000.0`, `1.234567890123`. Byte-level DIVERGENT for every non-integer/full-integer row (0 vs 0.00000000, 6 vs 6.00000000).
- order_ids | array of string | non-null, `[]` when none | MATCH (REF Array{} rpcxbridge.cpp:688-693; CAND `ids` make([]string,0,…) handlers.go:534,556-557).
- Empty bucket rows: REF emits a zero row for EVERY aligned interval (series resized to num_intervals, xseries.cpp:108-120), never fewer; CAND emits numBuckets rows (handlers.go:502-504,528). MATCH when limit not hit.
- Bucket count / tail window: REF `num_intervals = min(query_seconds/g, interval_limit)` and, when limit is hit, shifts the window to the LAST limit intervals (xseries.cpp:106-112). CAND ignores limit → returns all buckets from alignedStart. DIVERGENT when 8th param used.
- Volume semantics: REF = `x.fromVolume.amount<double>()` = FROM (maker) asset volume (rpcxbridge.cpp:684; xseries.cpp:188-199; currency.h:141-143); CAND = sum of `f.TakerSize` (taker asset, handlers.go:552). DIVERGENT quantity (maker-vs-taker volume; help-text "taker asset" is stale).
- Price quantization: REF `CurrencyPair::price()` uses boost::rational `to/from` ROUNDED to 1e-6 (currencypair.h:38-40; currency.h:107-121); CAND raw `float64(taker)/float64(maker)` (handlers.go:541). DIVERGENT (e.g. from=3,to=1 → REF price 0.333333 vs CAND 0.333333…333).
- Data source: REF pulls the on-chain XSeries cache (`getXAggregateSeries`→`getChainXAggregateSeries`, xseries.cpp:102-132,232-259). CAND aggregates `Store.Fills()` which has NO production writer — `AddFill` is only called from tests (store.go:292; rg: no non-test callers). In practice CAND returns all-zero buckets. DIVERGENT (data availability, documented in handlers.go:442-443).
- Empty-array semantics: for a valid window C++ returns one zero row per interval, never `[]`; CAND for `end <= start` returns `[]` where C++ (aligned-null) errors. Array-vs-error DIVERGENT.

SIDE EFFECTS: none (read-only query; REF may lazily fill the XSeries cache via updateSeriesCache — xseries.cpp:122-123,149-172 — CAND has no cache). MINOR.

VECTORS (>=3):
1. success buckets/keys: `["SYS","LTC",1540660180,1540660420,60]` → REF alignedStart=floor(1540660180/60)*60=1540660140, alignedEnd=ceil(1540660420/60)*60=1540660440, 5 buckets; row[0] time strings "2018-10-27T17:49:00.000Z" … "2018-10-27T17:53:00.000Z"; empty bucket row `["2018-10-27T17:49:00.000Z",0.00000000,0.00000000,0.00000000,0.00000000,0.00000000]` → CAND identical time keys but numbers encode `0.0`. DIVERGENT encoding.
2. error: `["SYS","LTC",1540660180,1540660420,120]` (granularity 120 unsupported) → REF `{error:"Invalid parameters: granularity=120 must be one of: 60,300,900,3600,21600,86400", code:1025, name:"dxGetOrderHistory"}`; CAND returns buckets (accepts 120). DIVERGENT.
3. error: `["SYS","LTC",100,0,60]` → REF aligned begin=60=end → 1025 "Start time >= end time."; CAND `end<=start` → `[]`. DIVERGENT.
4. edge: `["SYS","LTC",1540660180,1540660420,60,true,false,2]` → REF returns only the LAST 2 buckets (tail-window 2 intervals); CAND returns all 5. DIVERGENT.
5. volume: fill maker=1.0 BTC / taker=2.0 LTC, pair BTC LTC → REF volume = 1.0 (maker fromVolume); CAND volume = 2.0 (taker sum). DIVERGENT.

VERDICT: DIVERGENT — (1) granularity whitelist not enforced; (2) end<=start returns `[]` instead of the C++ 1025 "Start time >= end time."; (3) "Start/end times are too large." and interval_limit range checks missing; (4) `limit` param ignored (no tail-window truncation); (5) volume = taker asset not maker/from asset; (6) OHLCV doubles encoded shortest-roundtrip vs C++ fixed-8 ("0.0" vs "0.00000000"); (7) OHLCV prices not 1e-6-quantized (rational vs float); (8) empty series in production because Store.Fills() has no writer.

### dxGetOrderBook
REF: rpcxbridge.cpp:1493 | CAND: api/handlers.go:620

PARAMS:
- detail | int | required | — | REF `params[0].get_int()` then `<1||>4` → 1015 (rpcxbridge.cpp:1540,1552-1555); CAND `mustInt(0)` then same range → 1015 (handlers.go:624-630). MATCH. (Non-int input: REF json_spirit get_int throws → RPC/envelope error; CAND 1025 "param 0 is not an integer". EDGE DIVERGENT.)
- maker | string | required | — | REF params[1].get_str() (rpcxbridge.cpp:1541); CAND strParam → 1025 usage (handlers.go:631-634). MATCH for valid input.
- taker | string | required | — | MATCH (rpcxbridge.cpp:1542; handlers.go:635-638).
- max_orders | int | optional | 50, clamped <1→1 | REF (rpcxbridge.cpp:1544-1550); CAND (handlers.go:639-648). MATCH.

ERRORS:
- 1025 | "Invalid parameters: (detail, 1-4) (maker) (taker) (max_orders, default=50)[optional]" | arity<3 or >4 | REF (rpcxbridge.cpp:1527-1531); CAND (handlers.go:621-623). MATCH.
- 1015 | "Invalid detail level, possible values: 1 - 3" | detail<1 or >4 | REF (rpcxbridge.cpp:1552-1555,1986-1987); CAND (handlers.go:628-630). MATCH — both copy C++'s stale "1 - 3" text verbatim (xbridgeerror.cpp:57-58; response.go:121-122).

SUCCESS SHAPE: object `{detail, maker, taker, asks, bids}` — REF insertion order (rpcxbridge.cpp:1557-1559,1573-1574,1752-1753,1831-1832,1871-1872,1981-1982) vs CAND orderBookResult struct order (response.go:82-88). MATCH (Detail, Maker, Taker, Asks, Bids).
- asks/bids | array of arrays | non-null `[]` on empty | MATCH — REF default-constructed Arrays for empty book incl. the trList.empty() early-return (rpcxbridge.cpp:1568-1576); CAND non-nil `[][]interface{}{}` (handlers.go:682-683).
- Side filtering: REF asks = from=maker/to=taker, bids = from=taker/to=maker, case-insensitive, only state==trPending and fromAmount/toAmount>0 (rpcxbridge.cpp:1584-1611); CAND same two-if filtering with EqualFold, Status=="open", amounts>0 (handlers.go:652-671). MATCH (one order can land on both sides when maker==taker, both ports).
- d1 entry | [priceStr, sizeStr, count int] | REF best bid = max priceBid, best ask = min price, count=count_if over full side at that price (rpcxbridge.cpp:1650-1754); CAND best bid=bids[0], best ask=asks[len-1], countAtPrice over full side (handlers.go:686-695). MATCH. TIE-BREAK: REF max/min_element over the id-sorted TransactionMap picks the smallest id among ties; CAND sort.Slice is unstable → arbitrary tie pick for the size/amount rendered. DIVERGENT (ties only).
- d2 entry | [priceStr, sizeStr, count int] aggregated | REF walks best window (bids 0..bound; asks asks_len-bound..asks_len) merging equal-price neighbours with C++'s `while((++i < bound) && floatCompare(...))` quirk and count_if over the FULL side (rpcxbridge.cpp:1756-1833); CAND reproduces the identical window + every-other skip + merge + count (handlers.go:696-729). MATCH (quirk-for-quirk, verified).
- d3 entry | [priceStr, amountStr, id str] | REF bids best→worst (0..bound); asks worst→best within window (asks_len-bound..asks_len-1), i.e. best ask LAST (rpcxbridge.cpp:1835-1873); CAND identical window/order (handlers.go:730-743). MATCH.
- d4 entry | [priceStr, amountStr, ids array] | REF best bid/ask then the ids array = best id first + remaining equal-price ids in ascending uint256 order (rpcxbridge.cpp:1875-1983); CAND idsAtPrice = best first + rest sorted ascending raw bytes (handlers.go:596-618,744-753). C++ uint256 ordering is `memcmp(data, …)` byte-0-up (uint256.h:45) == Go `bytes.Compare` on the same wire bytes → the rest-order MATCHES; but the "best" id under price ties is again arbitrary in CAND (unstable sort) vs smallest-id in REF. MINOR DIVERGENT (ties only).
- priceStr | string fixed-6dp | both `%.6f` — REF xBridgeStringValueFromPrice (xutil.cpp:209-214); CAND formatXPrice (response.go:322-324). MATCH on rendering.
- sizeStr | string fixed-6dp | REF xBridgeStringValueFromAmount (+1/::COIN bump, xutil.cpp:202-227); CAND formatXAmount (truncation, response.go:237-241). Byte-equal for sampled integer base units; TBD at %.6f rounding boundaries.
- PRICE FORMULA: REF ask price = `(toAmount/1e6 + 1/::COIN)/(fromAmount/1e6 + 1/::COIN)`, bid priceBid = reciprocal (xutil.cpp:293-312); CAND = plain `float64(To)/float64(From)` and `float64(From)/float64(To)` (handlers.go:663-669). For small base-unit amounts the +1/::COIN term is significant: to=1,from=3 → REF "0.335548" vs CAND "0.333333". DIVERGENT (only observable for tiny amounts; negligible at realistic sizes).
- count | int | C++ int64_t vs CAND `int` (64-bit on linux/amd64 → same JSON) | MATCH on target; narrows to 32-bit on 32-bit platforms. NOTE.

SIDE EFFECTS: none. MATCH.

VECTORS (>=3):
1. d1: two asks BTC→LTC (from=1.5M,to=300k→0.2; from=1.5M,to=150k→0.1) and two bids LTC→BTC (0.2 and 0.1 in priceBid terms) → REF asks `[["0.100000","1.500000",1]]` (best ask, size=fromAmount 1.5M), bids `[["0.200000","1.500000",1]]` → CAND identical strings (realistic amounts: formula bump not visible).
2. d3 ordering: seeded asks {0.4,0.3,0.2,0.2,0.15,0.1} max_orders=4 → REF asks = [0.200000,0.200000,0.150000,0.100000] (worst→best, best LAST); bids = best→worst — CAND identical order. Asserts asks/bids ordering + 6dp price strings.
3. d2 aggregation window: max_orders=4, asks {0.4,0.3,0.2,0.2,0.15,0.1} → REF rows `[0.200000,3.000000,2]` then `[0.100000,1.500000,1]` (0.15 row skipped by the `++i<bound` quirk); CAND identical (handlers.go:705-728).
4. d4 ids: two asks at best price → REF ids = [<smallest-id>, <other-id>]; CAND ids = [best, rest-sorted] — full id SET matches, first element under ties arbitrary. MINOR DIVERGENT.
5. error: detail=0 → REF `{error:"Invalid detail level, possible values: 1 - 3", code:1015, name:"dxGetOrderBook"}` → CAND identical.
6. edge: empty book → REF `{"detail":1,"maker":"BTC","taker":"LTC","asks":[],"bids":[]}` → CAND identical (`[]`, never null).
7. price formula divergence: an ask with from=3 base units, to=1 base unit → REF price "0.335548" vs CAND "0.333333". DIVERGENT.

VERDICT: DIVERGENT (minor) — (1) price formula differs (C++ adds 1/::COIN=1e-8 to each amount before dividing; CAND plain ratio) — observable only for tiny orders; (2) tie-breaking among equal best prices is arbitrary in CAND (unstable sort) vs deterministic smallest-id in C++ (affects d1/d4 rendered size and first id); (3) non-numeric detail/max_orders handled as a 1025 business error vs C++ thrown RPC error; (4) count marshaled as Go `int` (64-bit here, narrower on 32-bit).

### dxGetTokenBalances
REF: rpcxbridge.cpp:2484 | CAND: api/handlers.go:762

PARAMS:
- (none) | — | — | REF rejects ANY param: `params.size() != 0` → 1025 (rpcxbridge.cpp:2519-2526); CAND does NOT check params (handlers.go:762). DIVERGENT (missing arity gate).

ERRORS:
- 1025 | "Invalid parameters: This function does not accept any parameters." | non-empty params | REF (rpcxbridge.cpp:2522-2524); CAND unreachable. DIVERGENT (missing path).

SUCCESS SHAPE: FLAT object (NOT the "nested {Wallet:{ticker:…}}" shape — the code is flat on both sides): keys = ticker → balance-string. The parity contract `dxGetTokenBalances.json` keys:["Wallet"] confirms flat.
- Key ORDER | DIVERGENT — REF emits "Wallet" FIRST (rpcxbridge.cpp:2532), then each connector in `connectors()` VECTOR order (insertion/config order — `m_connectors` is a `std::vector` populated by push_back, xbridgeapp.cpp:863-871,1227-1231; xbridgedef.h:27). CAND builds a `map[string]string` which encoding/json marshals with keys SORTED (handlers.go:763,831-832) → "Wallet" sorts last (uppercase W after B/L/…). REF first vs CAND last.
- Wallet | string fixed-6dp | REF ALWAYS present: `availableBalance()/COIN` (wallet GetBalance over all wallets — includes locked funds, xbridgeapp.h:798-806; rpcxbridge.cpp:2531-2532). CAND emits "Wallet" ONLY if a BLOCK connector (or fallback: first configured ExchangeWallet) yields a listable balance, and SUBTRACTS locked UTXOs (handlers.go:780-832); no connector → `{}`. DIVERGENT: (a) no-wallet case REF `{"Wallet":"0.000000"}` vs CAND `{}`; (b) CAND "Wallet" may be derived from a non-BLOCK exchange wallet; (c) locked-fund subtraction differs (REF does not subtract for Wallet).
- per-ticker values | string fixed-6dp | REF `xBridgeStringValueFromPrice(balance)` where balance = double from `getWalletBalance(excluded)` = sum of per-UTXO doubles (xbridgewalletconnector.cpp:52-73); `balance >= 0` gate skips disconnected wallets (rpcxbridge.cpp:2555). CAND `formatBalanceNative` = `native_sum / 10^Decimals` as float64, %.6f (response.go:253-263), skips on ListUnspent error, subtracts locked UTXOs (handlers.go:800-828). Value-level equal for exact sums; REF sums doubles (per-UTXO float drift) vs CAND exact integer sum → possible last-digit TBD; locked-subtraction present in CAND, REF excludes via getAllLockedUtxos too (rpcxbridge.cpp:2551) — MATCH in spirit for tickers.
- Empty wallet set | REF still `{"Wallet": …}`; CAND `{}`. DIVERGENT empty shape.

SIDE EFFECTS: none (read-only). REF spawns a concurrency-limited thread group to fetch balances (rpcxbridge.cpp:2535-2566) — no externally visible side effect. MATCH.

VECTORS (>=3):
1. success (BLOCK + LTC connectors): REF `{"Wallet":"250.834921","BLOCK":"123.000000","LTC":"0.568942"}` with Wallet FIRST; CAND `{"BLOCK":"123.000000","LTC":"0.568942","Wallet":"250.834921"}` — same key/value set, key order DIVERGENT (Wallet first vs sorted-last).
2. error: `["BTC"]` → REF `{error:"Invalid parameters: This function does not accept any parameters.", code:1025, name:"dxGetTokenBalances"}`; CAND returns balances. DIVERGENT.
3. edge: no wallets configured → REF `{"Wallet":"0.000000"}`; CAND `{}`. DIVERGENT.
4. locked exclusion: an order locks UTXOs of ticker T → REF T-balance excludes them (getAllLockedUtxos, rpcxbridge.cpp:2551) and so does CAND (lockedOf, handlers.go:769-779); but CAND also subtracts locked from the "Wallet" figure while REF's availableBalance() does not. DIVERGENT (Wallet figure).

VERDICT: DIVERGENT — (1) key order (Wallet first in C++ vs sorted, Wallet-last in CAND); (2) no-param rejection missing in CAND; (3) "Wallet" key derivation differs (C++: native BLOCK availableBalance(), always present, locked-inclusive; CAND: BLOCK-connector or first exchange wallet, optional, locked-exclusive); (4) empty case `{}` vs `{"Wallet":…}`; (5) per-ticker balance = exact integer / 10^Decimals vs C++ double sum (last-digit TBD).

### dxGetMyOrders
REF: rpcxbridge.cpp:1992 | CAND: api/handlers.go:840

PARAMS:
- (none) | — | — | REF rejects ANY param: `!params.empty()` → 1025 (rpcxbridge.cpp:2083-2093); CAND does NOT check params (handlers.go:840). DIVERGENT (missing arity gate).

ERRORS:
- 1025 | "Invalid parameters: This function does not accept any parameters." | non-empty params | REF (rpcxbridge.cpp:2086-2091); CAND unreachable. DIVERGENT (missing path).

SUCCESS SHAPE: array of orderDetailResult = orderBase + maker_address + taker_address (CAND response.go:50-54; REF rpcxbridge.cpp:2151-2171).
- FIELD ORDER DIVERGENT — REF order: id, maker, maker_size, maker_address, taker, taker_size, taker_address, updated_at, created_at, order_type, partial_minimum, partial_orig_maker_size, partial_orig_taker_size, partial_repost, partial_parent_id, status (rpcxbridge.cpp:2151-2171). CAND: encoding/json flattens the embedded orderBase FIRST (id, maker, maker_size, taker, taker_size, updated_at, created_at, order_type, partial_minimum, partial_orig_maker_size, partial_orig_taker_size, partial_repost, partial_parent_id, status — response.go:26-41) then appends maker_address, taker_address at positions 15/16 (response.go:52-53). So maker_address/taker_address sit at index 3/6 in REF but 14/15 in CAND. DIVERGENT.
- id | string | non-null | MATCH.
- maker/maker_size/taker/taker_size | string | non-null | MATCH (amount rendering: xBridgeStringValueFromAmount +1e-8 bump vs formatXAmount — byte-equal for sampled integer base units; TBD at %.6f boundary).
- maker_address/taker_address | string | non-null, "" when no connector | REF derived live via `connFrom->fromXAddr(tx->from)` / `connTo->fromXAddr(tx->to)`, "" if connector absent (rpcxbridge.cpp:2140-2148); CAND stored `o.MakerAddress/o.TakerAddress` (order.go:234-235). MATCH conceptually.
- updated_at/created_at | ISO8601(ms) | non-null | MATCH.
- order_type | string ("exact"/"partial") | non-null | MATCH (orderTypeString response.go:403-408 vs TransactionDescr::orderType).
- partial_* fields | string/bool | non-null | MATCH (partial_repost bool; partial_parent_id "" when no parent — parseParentId rpcxbridge.cpp:56-60 vs parentIDString response.go:413-418).
- status | string | non-null | MATCH.
- Sources: REF merges live local orders (isLocal) + history orders in trFinished/trCancelled, deduped by id via a `seen` map (rpcxbridge.cpp:2103-2121,2133-2138); CAND merges Store.Mine() + Store.History() where statusString∈{finished,canceled}, with NO dedup (handlers.go:842-857). MINOR DIVERGENT (no dedup guard; overlap is unusual because Store.MoveToHistory removes the live record, store.go:200-209).
- Sort: REF ascending by `txtime` (full µs, rpcxbridge.cpp:2127-2131); CAND stable sort by rendered UpdatedAt ISO string (ms precision, handlers.go:859-861). Equal-ms orders: REF orders by µs, CAND preserves Mine-then-History insertion order. MINOR DIVERGENT.
- Empty: REF empty `Array` → `[]` (rpcxbridge.cpp:2124-2125); CAND `[]orderDetailResult{}` → `[]`. MATCH (array, never null).

SIDE EFFECTS: none (read-only). MATCH.

VECTORS (>=3):
1. success: one local open order (maker=SYS, taker=LTC, partial) → REF `[{"id", "maker":"SYS", "maker_size":"100.000000", "maker_address":…, "taker":"LTC", "taker_size":"10.500000", "taker_address":…, "updated_at", "created_at", "order_type":"partial", "partial_minimum", "partial_orig_maker_size", "partial_orig_taker_size", "partial_repost":true, "partial_parent_id":"", "status":"open"}]`; CAND same object but maker_address/taker_address emitted as the LAST two keys. DIVERGENT field order.
2. success: finished local order in history → both return it with status "finished" (REF rpcxbridge.cpp:2116-2119; CAND handlers.go:853-856). MATCH.
3. error: `["SYS"]` → REF `{error:"Invalid parameters: This function does not accept any parameters.", code:1025, name:"dxGetMyOrders"}`; CAND returns the orders list. DIVERGENT.
4. edge: no orders → both `[]`. MATCH.
5. edge: two local orders with identical updated_at (same ms) → REF orders by µs; CAND stable insertion order (Mine then History). MINOR DIVERGENT.

VERDICT: DIVERGENT — (1) field ORDER of maker_address/taker_address (REF positions 3/6, CAND 14/15 — JSON key order is observable to clients); (2) no-param rejection missing in CAND; (3) no `seen`-style dedup guard; (4) sort key uses ms-rendered string vs C++ µs txtime.

## GROUP 3 CANDIDATE FINDINGS
- dxCancelOrder / error-name / NO_SESSION business error carries name="dx" (handlers.go:200) not "dxCancelOrder".
- dxCancelOrder / side-effect-order / connector checks run before the cancel; REF cancels first, so the missing-connector path leaves the order cancelled in REF but untouched in CAND.
- dxCancelOrder / error-set / cancel-failure path uses 1004/1002/1032/1021 (node.go:1773-1790) vs REF 1018/1021/1028; no isLocal→1021 guard.
- dxCancelOrder / id-validation / CAND requires exactly 64 hex chars; C++ uint256S is looser.
- dxGetOrderHistory / validation / granularity whitelist {60,300,900,3600,21600,86400} not enforced (only >0).
- dxGetOrderHistory / semantics / `end<=start` returns `[]` instead of C++ 1025 "Start time >= end time." (aligned-null test differs from raw comparison).
- dxGetOrderHistory / validation / "Start/end times are too large." (now+1d) and interval_limit-range errors missing.
- dxGetOrderHistory / params / `limit` (8th param) ignored — no tail-window truncation.
- dxGetOrderHistory / volume / sums taker asset (handlers.go:552); C++ sums from/maker asset (rpcxbridge.cpp:684).
- dxGetOrderHistory / encoding / OHLCV doubles as shortest-roundtrip ("0.0","1000.0") vs C++ fixed-8 ("0.00000000") via uret→json_spirit precision-8.
- dxGetOrderHistory / price / raw float ratio vs C++ 1e-6-quantized boost::rational price.
- dxGetOrderHistory / data / Store.Fills() has no production writer → always-empty/zero series vs on-chain XSeries.
- dxGetOrderBook / price-formula / C++ adds 1/::COIN to each amount pre-division (xutil.cpp:293-312); CAND plain ratio — diverges for tiny orders (1/3 → 0.335548 vs 0.333333).
- dxGetOrderBook / tie-break / unstable sort makes the equal-best-price pick (d1/d4 size + first id) arbitrary vs C++ smallest-id (map order).
- dxGetOrderBook / param-types / non-int detail/max_orders → 1025 business error vs C++ thrown RPC error.
- dxGetOrderBook / int-width / count marshaled as Go `int` (64-bit on target, 32-bit elsewhere) vs C++ int64_t.
- dxGetTokenBalances / key-order / C++ "Wallet" first + connector insertion order; CAND sorted map, "Wallet" last.
- dxGetTokenBalances / params / no-param rejection missing (C++ 1025).
- dxGetTokenBalances / wallet-key / C++ always emits native-BLOCK availableBalance()/COIN (locked-inclusive); CAND derives from BLOCK connector or first exchange wallet, optional, locked-exclusive → `{}` vs `{"Wallet":…}`.
- dxGetTokenBalances / precision / exact integer/10^Decimals vs C++ per-UTXO double sum (last-digit TBD).
- dxGetMyOrders / field-order / maker_address/taker_address emitted last (embedded struct flatten) vs C++ positions 3/6.
- dxGetMyOrders / params / no-param rejection missing (C++ 1025).
- dxGetMyOrders / dedup / no `seen` guard across live+history merge (C++ rpcxbridge.cpp:2133-2138).
- dxGetMyOrders / sort / ms-rendered string sort vs C++ µs txtime.

OUTPUT FILE: `docs/audit/evidence/rpc-groups/group3.md` (created).
