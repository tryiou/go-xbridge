## RPC CONFORMANCE CARDS — GROUP 1 (dxGetOrderFills, dxGetOrders, dxGetOrder, dxGetLocalTokens, dxLoadXBridgeConf)

### dxGetOrderFills
REF: rpcxbridge.cpp:475 | CAND: api/handlers.go:57
PARAMS:
- maker | string | required | — | REF reads `params[0].get_str()` (rpcxbridge.cpp:541); non-string throws json_spirit `std::runtime_error` → envelope error code -1 (json_spirit_value.h:383-393, rpc/server.cpp:584-587). CAND reads via `strParam(params,0)` (handlers.go:58); non-string → business result error code 1025 (handlers.go:60).
- taker | string | required | — | REF `params[1].get_str()` (rpcxbridge.cpp:542); CAND `strParam(params,1)` (handlers.go:62). Same type-error channel divergence as maker.
- combined | bool | optional | default=true | REF `params.size()==3 ? params[2].get_bool() : true` (rpcxbridge.cpp:539); CAND `mustBool(params,2,true,...)` (handlers.go:66). REF enforces param count ∈ {2,3} (rpcxbridge.cpp:532-537); CAND accepts any count ≥2 and ignores params[3..].
ERRORS:
- 1025 | REF: "Invalid parameters: (maker) (taker) (combined, default=true)[optional]" (template `xbridgeErrorText(INVALID_PARAMETERS,...)` = "Invalid parameters: " + arg, xbridgeerror.cpp:43-44; handler arg rpcxbridge.cpp:535-537) | params.size() not 2 or 3 | CAND: identical string at handlers.go:60,64. TEXT MATCH.
- (thrown) | REF: `get_value< bool > called on <type> Value` (json_spirit_value.h:389) as envelope error {code:-1} (rpc/server.cpp:584-587, 488-492) | params[2] not a bool (incl. JSON null or string "true"/"false") | CAND: boolParam tolerates string forms (dispatch.go:103-108) and treats JSON null as `false` (dispatch.go:101-110; encoding/json null is a no-op on the bool) → returns success, no error. DIVERGENT.
- (thrown) | REF: `get_value< str > called on <type> Value` envelope {code:-1} | params[0]/params[1] not a string | CAND: business result error 1025 "(maker) (taker) (combined, default=true)[optional]" (handlers.go:60,64). DIVERGENT (channel + code + text).
SUCCESS SHAPE: Array of objects, 12 fields in insertion order: id(str), time(str), maker(str), maker_size(str), taker(str), taker_size(str), order_type(str), partial_minimum(str), partial_orig_maker_size(str), partial_orig_taker_size(str), partial_repost(bool), partial_parent_id(str) (rpcxbridge.cpp:569-582 vs handlers.go:38-51 struct field order). Field order MATCHES. `time` = iso8601 millisecond-precision string both sides (xutil.cpp:185-200; response.go:393-399). Amounts fixed 6-decimal both sides (xutil.cpp:202-207; response.go:237-241). Empty result: REF default-constructed Array → `[]` (rpcxbridge.cpp:566,585); CAND `out := []fillOut{}` non-nil → `[]` (handlers.go:71). Array-vs-null OK. Ordering: REF sorts filtered set by txtime descending (rpcxbridge.cpp:560-564); CAND Store.Fills() returns newest-first (store.go:299-310). Equivalent for distinct times; tie-break order differs (REF std::sort unspecified, CAND stable reverse of insertion).
SIDE EFFECTS: none (read-only) — REF reads App::history() (rpcxbridge.cpp:546); CAND reads Store.Fills() (handlers.go:72). MATCH.
VECTORS:
1. [no fills, params ["LTC","SYS"]] → REF `[]` (rpcxbridge.cpp:566,585) → CAND `[]` (handlers.go:71,95). Success.
2. [params ["LTC","SYS",true,"EXTRA"]] → REF business error 1025 "Invalid parameters: (maker) (taker) (combined, default=true)[optional]" (rpcxbridge.cpp:532-537) → CAND returns fills, EXTRA ignored (handlers.go:57-95). DIVERGENT.
3. [params ["LTC","SYS","true"]] (string bool) → REF get_bool() throws → envelope {code:-1, message:"get_value< bool > called on str Value"} (json_spirit_value.h:383-393, rpc/server.cpp:584-587) → CAND accepts string "true" → combined=true (dispatch.go:103-108). DIVERGENT.
4. [params ["LTC","SYS",null]] → REF get_bool() on null throws (envelope -1) → CAND combined=false (dispatch.go:101-110 null no-op). DIVERGENT.
5. [params [123,"SYS"]] (non-string maker) → REF get_str() throws envelope -1 → CAND business error 1025, code 1025, name "dxGetOrderFills" (handlers.go:58-60). DIVERGENT (channel/code).
VERDICT: DIVERGENT (param-count not enforced beyond index 2 — extra params accepted; wrong-type/null params mapped to business error 1025 or silently coerced instead of C++'s thrown envelope error code -1; string/null `combined` accepted as false/true instead of throwing).

### dxGetOrders
REF: rpcxbridge.cpp:337 | CAND: api/handlers.go:102
PARAMS:
- (none) | — | — | — | REF rejects any param (rpcxbridge.cpp:424-427); CAND rejects any param (handlers.go:103-105). MATCH.
ERRORS:
- 1025 | REF: "Invalid parameters: This function does not accept any parameters." (xbridgeerror.cpp:43-44; rpcxbridge.cpp:425-426) | params non-empty | CAND: identical text at handlers.go:104. TEXT MATCH.
SUCCESS SHAPE: Array of objects, 14 fields in insertion order: id, maker, maker_size, taker, taker_size, updated_at, created_at, order_type, partial_minimum, partial_orig_maker_size, partial_orig_taker_size, partial_repost, partial_parent_id, status (rpcxbridge.cpp:453-467 vs response.go:26-41 orderBase). Field order MATCHES. Empty → `[]` both sides (rpcxbridge.cpp:472; handlers.go:107). ARRAY ELEMENT ORDER DIVERGENT: REF iterates `App::transactions()` — a `std::map<uint256,...>` (xbridgedef.h:29 typedef; xbridgeapp.cpp:430) → deterministic ascending-raw-byte id order; CAND iterates `h.Store.List()` — a Go map (store.go:154-162) → randomized per-run order (handlers.go:108).
SIDE EFFECTS: none. Filtering:
- 60s gate: REF `(currentTime - tr->txtime).total_seconds() > 60` with `currentTime` = second-resolution clock (rpcxbridge.cpp:431,439); CAND `now - o.Updated > 60*1e6` microsecond-exact (handlers.go:106,111; store.go:517-519). Boundary DIVERGENCE (60–61 s old terminal order: REF keeps, CAND skips).
- Wallet gate: REF `(!connFrom || !connTo) && !nowalletswitch` with `nowalletswitch = gArgs.GetBoolArg("-dxnowallets", settings().showAllOrders())` (rpcxbridge.cpp:432,446-450; settings.h:38 default false); CAND `if !h.Config().ShowAllOrders { connector both }` (handlers.go:120-126). Equivalent when no `-dxnowallets` CLI override is present; the C++ CLI-flag override has no CAND equivalent (thin client). Minor.
VECTORS:
1. [one open order, no params] → REF 1-element array, 14 fields (rpcxbridge.cpp:453-467) → CAND same (handlers.go:127, order.go:199-201). Success.
2. [params ["x"]] → REF business 1025 "Invalid parameters: This function does not accept any parameters." (rpcxbridge.cpp:424-427) → CAND same text (handlers.go:103-105). MATCH.
3. [cancelled order with age 60.5s] → REF `total_seconds()` = 60, not >60 → KEPT (rpcxbridge.cpp:439-443) → CAND `60.5e6 > 60e6` → SKIPPED (handlers.go:110-114). DIVERGENT.
4. [order in currency with no connector, ShowAllOrders=false] → REF skipped (rpcxbridge.cpp:446-450) → CAND skipped (handlers.go:120-126). MATCH.
5. [same set of N orders] → REF array ordered by ascending raw uint256 id (rpcxbridge.cpp:434) → CAND array in randomized Go-map order (store.go:154-162). DIVERGENT (ordering only).
VERDICT: DIVERGENT (array element ordering: REF sorted-by-id, CAND Go-map-random; 60-second filter boundary differs by up to 1s due to second-vs-microsecond clocks; `-dxnowallets` CLI override unmapped).

### dxGetOrder
REF: rpcxbridge.cpp:704 | CAND: api/handlers.go:136
PARAMS:
- id | string | required | — | REF `uint256S(params[0].get_str())` (rpcxbridge.cpp:778) case-insensitive, up-to-64-hex parse (uint256.cpp:27-53); CAND lowercases then `orderIDKey` requires exactly 64 hex chars (handlers.go:143-149, response.go:504-528). REF rejects params.size()!=1 (rpcxbridge.cpp:774-776); CAND ignores params beyond index 0 (handlers.go:137-139).
ERRORS:
- 1025 | REF: "Invalid parameters: (id)" (rpcxbridge.cpp:775) | params.size() != 1 | CAND: same text at handlers.go:139. TEXT MATCH (but only when params[0] is missing/unparseable — see vector 3).
- 1021 | REF: "Transaction " + id.ToString() + " not found" (rpcxbridge.cpp:785; xbridgeerror.cpp:31-32) where id.ToString() = GetHex() 64-char zero-padded lowercase (uint256.cpp:21-24) | lookup miss in transactions()/historic (xbridgeapp.cpp:1273-1294) | CAND: "Transaction " + lowercased raw input + " not found" (handlers.go:148,158). TEXT MATCH for 64-char hex ids; DIVERGENT (message) for short or non-hex ids (CAND echoes raw input; REF zero-pads / zero-fills).
- 1018 | REF: "No session for currency " + currency, name="dxGetOrder" (rpcxbridge.cpp:790-795) | no connector for fromCurrency/toCurrency | CAND: message identical (response.go:127-128), but name field = "dx" hardcoded in connector() (handlers.go:198-207). NAME FIELD DIVERGENT.
- (thrown) | REF: `get_value< str > called on <type> Value` envelope {code:-1} (json_spirit_value.h:389) | params[0] not a string | CAND: business result 1025 "(id)" (handlers.go:137-139). DIVERGENT.
SUCCESS SHAPE: Single object, 14 fields in the same insertion order as dxGetOrders (rpcxbridge.cpp:797-811 vs response.go:26-41). MATCH. Note: no maker_address/taker_address emitted (REF help text lists them but the writer does not emit them — rpcxbridge.cpp:797-811; CAND orderListResult omits them, response.go:45-47). MATCH.
SIDE EFFECTS: none (read-only).
VECTORS:
1. [valid 64-hex id of an open order, both connectors present] → REF 14-field object (rpcxbridge.cpp:797-811) → CAND same (handlers.go:150-167, order.go:199-201). Success.
2. [unknown but valid 64-hex lowercase id] → REF result {error:"Transaction <id> not found", code:1021, name:"dxGetOrder"} (rpcxbridge.cpp:784-786) → CAND {error:"Transaction <id> not found", code:1021, name:"dxGetOrder"} (handlers.go:148,158). MATCH.
3. [params ["<id>","EXTRA"]] → REF 1025 "(id)" (rpcxbridge.cpp:774-776) → CAND ignores EXTRA, returns the order (handlers.go:137-167). DIVERGENT.
4. [params ["deadbeef"]] (short hex) → REF 1021, message "Transaction 00000000000000000000000000000000000000000000000000000000deadbeef not found" (uint256.cpp:27-53 → rpcxbridge.cpp:785) → CAND 1021, message "Transaction deadbeef not found" (handlers.go:148). Same code, DIVERGENT message.
5. [order found but no connector for its toCurrency] → REF result {error:"No session for currency <cur>", code:1018, name:"dxGetOrder"} (rpcxbridge.cpp:793-795) → CAND {error:"No session for currency <cur>", code:1018, name:"dx"} (handlers.go:161-166, 200). DIVERGENT (name field).
6. [params [123]] (non-string id) → REF get_str() throws → envelope -1 → CAND business 1025 "(id)" (handlers.go:137-139). DIVERGENT (channel).
VERDICT: DIVERGENT (extra params accepted; error `name` field "dx" instead of "dxGetOrder" for NO_SESSION; not-found message zero-padding differs for short/invalid ids; wrong-type param returns business 1025 instead of C++ thrown envelope error).

### dxGetLocalTokens
REF: rpcxbridge.cpp:237 | CAND: api/handlers.go:174
PARAMS:
- (none) | — | — | — | REF rejects any param (rpcxbridge.cpp:267-270); CAND performs NO param check (handlers.go:174-176). DIVERGENT.
ERRORS:
- 1025 | REF: "Invalid parameters: This function does not accept any parameter." (xbridgeerror.cpp:43-44; rpcxbridge.cpp:268-269) | params non-empty | CAND: never emitted (no check). DIVERGENT (error path missing).
SUCCESS SHAPE: Array of ticker strings. Empty → `[]` both sides (rpcxbridge.cpp:272,278; handlers.go:189-194 `make([]string,0,...)`). ORDER: REF iterates `m_connectorCurrencyMap` — `std::map<string,WalletConnectorPtr>` (xbridgedef.h:29; xbridgeapp.cpp:814-818) → ascending lexicographic; CAND `sort.Strings` (handlers.go:192) → ascending lexicographic. ORDER MATCHES. SET SEMANTICS DIVERGENT: REF returns only connectors that successfully connected (built from exchangeWallets() settings.cpp:143-159, added only after `conn->init()` passes at xbridgeapp.cpp:1129-1143/1196-1200; failed/bad wallets removed via removeConnector xbridgeapp.cpp:1096,1204); CAND returns the raw `[Main].ExchangeWallets` list (handlers.go:175) with no connectivity filter and no dedup.
SIDE EFFECTS: none (read-only).
VECTORS:
1. [xbridge.conf ExchangeWallets=BTC,LTC, both connected] → REF ["BTC","LTC"] (xbridgeapp.cpp:808-821) → CAND ["BTC","LTC"] (handlers.go:189-194). Success, order matches.
2. [params ["BTC"]] → REF business 1025 "Invalid parameters: This function does not accept any parameter." (rpcxbridge.cpp:267-270) → CAND returns token list, param ignored (handlers.go:174-176). DIVERGENT.
3. [ExchangeWallets=BTC,LTC; LTC wallet unreachable] → REF ["BTC"] (LTC removed, xbridgeapp.cpp:1129-1134/1203-1204) → CAND ["BTC","LTC"] (raw config list, handlers.go:175). DIVERGENT (set, not order).
4. [ExchangeWallets=BTC,BTC] (duplicate) → REF ["BTC"] (std::map dedupes) → CAND ["BTC","BTC"] (no dedup, handlers.go:189-194). DIVERGENT (minor).
VERDICT: DIVERGENT (no parameter rejection; token set = raw ExchangeWallets list rather than only-connected connectors; no dedup).

### dxLoadXBridgeConf
REF: rpcxbridge.cpp:195 | CAND: api/handlers.go:213
PARAMS:
- (none) | — | — | — | REF rejects any param (rpcxbridge.cpp:218-220); CAND performs NO param check (handlers.go:213-221). DIVERGENT.
ERRORS:
- 1025 | REF: "Invalid parameters: This function does not accept any parameter." (rpcxbridge.cpp:219-220) | params non-empty | CAND: never emitted. DIVERGENT (error path missing).
- (thrown) | REF: runtime_error "dxLoadXBridgeConf\nFailed to reload the config because a shutdown request is in progress." → envelope {code:-1} (rpcxbridge.cpp:222-223; rpc/server.cpp:584-587) | ShutdownRequested() | CAND: no equivalent state → returns true. DIVERGENT (path absent).
- (thrown) | REF: runtime_error "dxLoadXBridgeConf\nAn existing wallet update is currently in progress, please wait until it is completed." → envelope {code:-1} (rpcxbridge.cpp:226-227) | app.isUpdatingWallets() | CAND: no equivalent state → returns true. DIVERGENT (path absent).
- (CAND-only) | REF: reload failure returns result bool `false`, NOT an error — `success = app.loadSettings()` (rpcxbridge.cpp:229-234; loadSettings returns false on read failure, xbridgeapp.cpp:502-517) | conf load/parse failure | CAND: business result 1025 "Invalid parameters: dxLoadXBridgeConf: <err>" (handlers.go:218; node.go:287-298). DIVERGENT (REF bool false vs CAND error object).
SUCCESS SHAPE: REF JSON bool `true`/`false` (rpcxbridge.cpp:234 `uret(success)`); CAND JSON bool `true` on success (handlers.go:215,220). MATCH on success; failure shape diverges (see above).
SIDE EFFECTS:
- REF: loadSettings() re-reads conf, then clearBadWallets(), updateActiveWallets(), and — when `!settings().showAllOrders()` — clearNonLocalOrders() which erases non-local orders lacking a connector for either currency (rpcxbridge.cpp:229-233; xbridgeapp.cpp:3811-3826). CAND: reloadConf() swaps a rebuilt Config (node.go:287-321) but does NOT clear bad wallets, update active wallets, or drop non-local orders from the Store. DIVERGENT (missing side effects; thin client has no updateActiveWallets analog).
VECTORS:
1. [no params, clean reload] → REF result `true` (rpcxbridge.cpp:234) → CAND `true` (handlers.go:220). Success.
2. [params ["x"]] → REF business 1025 "Invalid parameters: This function does not accept any parameter." (rpcxbridge.cpp:218-220) → CAND returns `true`, param ignored (handlers.go:213-221). DIVERGENT.
3. [unreadable/malformed conf] → REF result `false` (no error object; xbridgeapp.cpp:512-514 → rpcxbridge.cpp:234) → CAND result {error:"Invalid parameters: dxLoadXBridgeConf: ...", code:1025, name:"dxLoadXBridgeConf"} (handlers.go:218). DIVERGENT (response shape + code).
4. [showAllOrders=false, remote order present with missing connector] → REF removes that order from the book (rpcxbridge.cpp:232-233; xbridgeapp.cpp:3811-3826) → CAND order remains in Store (reloadConf touches only Config, node.go:287-321). DIVERGENT (side effect).
VERDICT: DIVERGENT (no parameter rejection; reload failure returns an error object where REF returns bool `false`; missing shutdown-request and wallet-update guards; missing clearBadWallets/clearNonLocalOrders side effects).

## CANDIDATE FINDINGS
- dxGetOrderFills / params / accepts 4+ positional params (REF requires exactly 2 or 3); wrong-type or null `combined` silently coerced (string→bool, null→false) instead of C++ thrown envelope error -1.
- dxGetOrderFills / error channel / non-string maker/taker returns business error 1025 in `result` where REF throws `std::runtime_error` (envelope error, code -1, "get_value< str > called on ... Value").
- dxGetOrders / ordering / array order is Go-map-random (store.go:154-162) vs REF ascending raw-uint256-id from std::map iteration (xbridgedef.h:29).
- dxGetOrders / boundary / 60s filter uses microsecond-exact compare (handlers.go:111) vs REF second-truncating `total_seconds() > 60` (rpcxbridge.cpp:439); 60–61s old terminal orders flip inclusion.
- dxGetOrder / params / extra params ignored (REF rejects params.size()!=1).
- dxGetOrder / error-name / NO_SESSION `name` field hardcoded "dx" (handlers.go:200,204) vs REF "dxGetOrder" (rpcxbridge.cpp:790-795).
- dxGetOrder / error-text / not-found message echoes raw (lowercased) input (handlers.go:148) vs REF zero-padded 64-hex id.ToString() for short/invalid ids (uint256.cpp:21-24).
- dxGetLocalTokens / params / no rejection of supplied params (REF: INVALID_PARAMETERS, rpcxbridge.cpp:267-270).
- dxGetLocalTokens / set / returns raw ExchangeWallets list incl. unconnected/duplicate tickers (handlers.go:175,189-194) vs REF only connected connectors, deduped (xbridgeapp.cpp:1129-1143,1196-1204).
- dxLoadXBridgeConf / params / no rejection of supplied params (REF: INVALID_PARAMETERS, rpcxbridge.cpp:218-220).
- dxLoadXBridgeConf / error-shape / reload failure returns business error 1025 object (handlers.go:218) vs REF result bool `false` (xbridgeapp.cpp:512-514).
- dxLoadXBridgeConf / side-effects / missing clearNonLocalOrders + clearBadWallets (REF rpcxbridge.cpp:230-233, xbridgeapp.cpp:3811-3826); remote orders survive reload in CAND.
- dxLoadXBridgeConf / guards / shutdown-request and wallet-update-in-progress throws absent in CAND (REF rpcxbridge.cpp:222-227).
