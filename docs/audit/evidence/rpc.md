# RPC_CONFORMANCE.md

# RPC Conformance Audit — Blocknet Core xBridge (C++) vs go-xbridge (Go)

REF: blocknet_core/src/xbridge/rpcxbridge.cpp | CAND: go-xbridge/api/
Envelope: JSON-RPC 1.0. Business errors ride in `result` as {error,code,name}; C++ *throws* RPC errors for param/type/help cases (envelope `error`), Go returns business results. JSON built via encoding/json struct marshal (field order = struct order; map keys sorted) in Go vs UniValue/json_spirit insertion order in C++.

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


## RPC CONFORMANCE CARDS — GROUP 2 (dxGetNewTokenAddress, dxGetNetworkTokens, dxMakeOrder, dxMakePartialOrder, dxTakeOrder)

### dxGetNewTokenAddress
REF: rpcxbridge.cpp:150 | CAND: api/handlers.go:231

PARAMS: (per param: name | JSON type | required | default | REF vs CAND)
- ticker | string | required | — | REF: arity gate `params.size() != 1` → INVALID_PARAMETERS "(ticker)" (rpcxbridge.cpp:178-179); value read via `params[0].get_str()` (throws on non-string → RPC/envelope error). CAND: `strParam(params,0)` with NO arity check (handlers.go:232-235) — any extra params are silently ignored; wrong type → business error instead of a thrown RPC error.

ERRORS: (per reachable error: code | exact template | trigger | REF vs CAND)
- 1025 | "Invalid parameters: (ticker)" | missing OR extra params | REF: fires on `size != 1` (rpcxbridge.cpp:179); CAND: fires only on missing param 0 (handlers.go:234). DIVERGENT: extra-param case unreachable in CAND.
- 1002 | "Internal Server Error" | address generation fails | REF: unreachable — `getNewTokenAddress()` returns "" on `getNewAddress` failure → empty array (xbridgewalletconnector.cpp:78-88); CAND: `GetNewAddress` error → `makeError(errUnknown,...)` (handlers.go:243-245). DIVERGENT (REF returns `[]`, CAND returns error).

SUCCESS SHAPE: (ordered fields: name | type | nullability | REF vs CAND) + array-vs-null notes
- array of string | non-null | REF: `[addr]` or `[]` (rpcxbridge.cpp:182-192, default-constructed `Array`); CAND: `[]string{addr}` or `[]string{}` (handlers.go:240,246). Both always `[]`/`[...]`, never null. No object keys (scalar array), so field order n/a. MATCH.
- No wallet for ticker: REF → `[]` (conn null, rpcxbridge.cpp:186-190); CAND → `[]string{}` (connector error swallowed, handlers.go:236-241). MATCH.

SIDE EFFECTS: (db write / P2P broadcast / state transition | REF vs CAND)
- REF: `conn->getNewTokenAddress()` → wallet `getnewaddress` RPC only (xbridgewalletconnectorbtc.cpp getNewAddress); no engine state, no P2P. CAND: `conn.GetNewAddress()`; same. MATCH.

VECTORS (>=3):
1. success: `["LTC"]` with wallet → REF `["<newaddr>"]` → CAND `["<newaddr>"]`.
2. error: `[]` → REF `{error:"Invalid parameters: (ticker)", code:1025, name:"dxGetNewTokenAddress"}` → CAND identical.
3. edge (extra param): `["LTC","junk"]` → REF 1025 error; CAND returns `["<addr>"]`. DIVERGENT.
4. edge (wallet gen fails): REF `[]`; CAND `{error:"Internal Server Error", code:1002, name:"dxGetNewTokenAddress"}`. DIVERGENT.

VERDICT: DIVERGENT — (1) no arity gate in CAND (extra params accepted); (2) address-generation failure mapped to 1002 business error instead of C++'s empty array.

### dxGetNetworkTokens
REF: rpcxbridge.cpp:281 | CAND: api/handlers.go:178

PARAMS:
- (none) | — | — | — | REF: `params.size() > 0` → error (rpcxbridge.cpp:314-317); CAND: handler does NO param check at all (handlers.go:178-187) — any params ignored. DIVERGENT.

ERRORS:
- 1025 | "Invalid parameters: This function does not accept any parameters." | any param present | REF: rpcxbridge.cpp:315-316; CAND: unreachable (no check). DIVERGENT.

SUCCESS SHAPE:
- array of string (tickers) | non-null | REF: sorted array built from a `std::set` union of services advertised by RUNNING servicenodes (`App::walletServices`, xbridgeapp.cpp:2758-2780 — regex `^[^:]+$`, excludes xr/xrs) (rpcxbridge.cpp:319-326); CAND: `NetworkTokens()` = union of `snReg.WalletServices()` + config `NetworkTokens` + `ExchangeWallets`, sorted (node.go:660-684). Empty case: REF `[]`; CAND `[]string{}`. MATCH on shape/ordering.
- Membership DIVERGENCE: REF has NO config fallback — with no live SN advertising a token, the token is absent even if it is in xbridge.conf; CAND always adds the full config token set. e.g. no SNs connected: REF `[]`, CAND `["BLOCK","LTC",...]`.

SIDE EFFECTS: none (pure read). MATCH.

VECTORS:
1. success: `[]` → REF sorted array of network-advertised tokens → CAND sorted union incl. config tokens.
2. error: `["LTC"]` → REF `{error:"Invalid parameters: This function does not accept any parameters.", code:1025, name:"dxGetNetworkTokens"}`; CAND returns the token list (params ignored). DIVERGENT.
3. edge (empty/no SNs): REF `[]`; CAND `["BLOCK","LTC"]` from config. DIVERGENT.

VERDICT: DIVERGENT — (1) param-arity check absent in CAND; (2) membership includes config/ExchangeWallets tokens with no C++-equivalent (C++ is a pure network union).

### dxMakeOrder
REF: rpcxbridge.cpp:815 | CAND: api/handlers.go:253 (+ node.go:857 MakeOrder, utxo_select.go:79 selectUtxos)

PARAMS:
- maker | string | required | — | same (REF rpcxbridge.cpp:926; CAND node.go:858-868).
- maker_size | string | required | — | REF validates via `xBridgeValidCoin` then `lexical_cast<double>` (rpcxbridge.cpp:906,927); CAND `xBridgeValidCoin` then `parseXAmount` (node.go:864-873). DIVERGENT on non-numeric/negative: REF `lexical_cast` throws (envelope RPC error); CAND → business error "Invalid parameters: invalid maker_size".
- maker_address | string | required | — | same.
- taker | string | required | — | same.
- taker_size | string | required | — | same as maker_size (rpcxbridge.cpp:916,931; node.go:867-876).
- taker_address | string | required | — | same.
- type | string | required | "exact" | REF `type != "exact"` → "Only the exact type is supported at this time." (rpcxbridge.cpp:937-940); CAND same (handlers.go:292-294). MATCH.
- use_all_funds | bool | optional | true | REF: read when `size >= 8` via `get_bool()` (rpcxbridge.cpp:983-985) — a non-bool throws (envelope error); CAND: `boolParam` (handlers.go:267-274) tolerates JSON strings "true"/"false", else business error "invalid use_all_funds". DIVERGENT (CAND more lenient + different error channel/text).
- dryrun | string | optional (only when `size==9`) | "dryrun" | REF rpcxbridge.cpp:988-995; CAND handlers.go:278-289. Match incl. "misspelled dryrun → error with the literal value as arg".
- Arity: REF `<7` throws the RPCHelpMan help text (rpcxbridge.cpp:817); CAND `<7` → business error `INVALID_PARAMETERS` with the param-list string (handlers.go:255-257). DIVERGENT (envelope error vs result business error).

ERRORS (reachable; code | template | trigger | REF vs CAND):
- 1025 | "Invalid parameters: The maker_size/taker_size is too precise. The maximum precision supported is 6 digits." | precision > 6 | REF rpcxbridge.cpp:906-924; CAND node.go:864-869. MATCH.
- 1025 | "Invalid parameters: The maker_address and taker_address cannot be the same: <addr>" | addr equality | REF rpcxbridge.cpp:943-946; CAND node.go:887-889. MATCH.
- 1025 | "Invalid parameters: The maximum supported size is 100000000" | > MAX_COIN (1e8 whole coins) | REF rpcxbridge.cpp:949-953; CAND node.go:892-894 (maxXSize=1e14 base). MATCH.
- 1025 | "Invalid parameters: The minimum supported size is 0.000001" | amount <= 0 (incl. "0") | REF rpcxbridge.cpp:955-958; CAND node.go:895-897. MATCH for "0"; DIVERGENT for negative strings (CAND "invalid maker_size").
- 1018 | "No session for currency Unable to connect to wallet: <cur>" → full "Invalid parameters"? NO — full text "No session for currency Unable to connect to wallet: <cur>" | connector missing | REF `makeError(NO_SESSION,__FUNCTION__,"Unable to connect to wallet: "+cur)` (rpcxbridge.cpp:963-964); CAND node.go:928-933. MATCH (both include the same odd literal).
- 1026 | "Bad address <addr>" | invalid from/to address | REF rpcxbridge.cpp:968-973; CAND decodeAddr node.go:832-846 (name "dxMakeOrder"). MATCH for makes.
- 1025 | "Invalid parameters: unsupported currency: <ticker>" | currency not in coin registry | REF: no equivalent — unknown currency reaches the NO_SESSION gate (line 963) first, or checkCreateParams `INVALID_CURRENCY` when ticker length > 8 (xbridgeapp.cpp:2551-2555 → "Invalid coin <cur>"). DIVERGENT (code+text+precedence).
- 1017 | "Invalid coin <fromCurrency>" | currency length > 8 | REF: checkCreateParams / sendXBridgeTransaction (xbridgeapp.cpp:2551-2555, 1545-1549); CAND: no length check at all. DIVERGENT (unreachable in CAND).
- 1019 | "Insufficient funds for <fromCurrency>" (checkCreateParams balance gate, rpcxbridge.cpp:1032-1034) and "Insufficient funds for <fromAddress>" (selection fail, rpcxbridge.cpp:1069-1070) | balance < amount / selection fail | CAND: `"Insufficient funds for insufficient funds"` (node.go:1045) and `"Insufficient funds for <err>"` (node.go:1019). DIVERGENT text (C++ arg is currency/address; CAND arg is the literal "insufficient funds" or a wallet error).
- 1032 | "Could not find a service node with required services: " (bare, default branch rpcxbridge.cpp:1036-1038,1071-1073) | no hub | CAND: `"Could not find a service node with required services: BTC/SYS"` (node.go:916,922) and `requireWrite` with name "dx" (node.go:848-853). DIVERGENT (CAND adds the pair; name differs to "dx" on the conn==nil gate).

SUCCESS SHAPE (REF order rpcxbridge.cpp:1048-1067):
1 id | str | 2 maker_address | 3 maker | 4 maker_size | 5 taker_address | 6 taker | 7 taker_size | 8 created_at | 9 updated_at | 10 block_id | 11 order_type="exact" | 12 partial_minimum="0" | 13 partial_orig_maker_size="0" | 14 partial_orig_taker_size="0" | 15 partial_repost=false | 16 partial_parent_id="" | 17 status="created".
CAND `makeOrderResult` (response.go:57-62 + orderBase:26-41) emits: id, maker, maker_size, taker, taker_size, updated_at, created_at, order_type, partial_minimum, partial_orig_maker_size, partial_orig_taker_size, partial_repost, partial_parent_id, status, maker_address, taker_address, block_id.
DIVERGENT field ORDER (maker_address/taker_address at 2/5 vs 15/16; block_id at 10 vs 17; created_at/updated_at swapped) and DIVERGENT VALUES (C++ literal "0" vs CAND "0.000000" for the three partial_* fields; C++ updated_at = build-time now, later than created_at; CAND updated_at == created_at == order timestamp).
DRYRUN shape: REF returns a 14-field preview (rpcxbridge.cpp:1004-1021): id="000…0", maker, maker_size, maker_address, taker, taker_size, taker_address, order_type, partial_minimum="0", partial_orig_maker_size="0", partial_orig_taker_size="0", partial_repost=false, partial_parent_id="", status="created" — NO updated_at/created_at/block_id. CAND dryrun (handlers.go:303 → makeOrderResponse) returns the FULL 17-field success object with the REAL computed id (node.go:1067 id is never zeroed; verified by make_order_kat_test.go:41) and a real block_id. DIVERGENT shape + id.

SIDE EFFECTS:
- REF: utxo selection + proof signing (xbridgeapp.cpp:1610-1717), lockCoins (1720-1724), deterministic id + SEND broadcast + session + store insert (via sendXBridgeTransaction). CAND: same pipeline (utxo_select.go:79 selectUtxos, buildUtxoProofs node.go:463, sha256dOrderID utxo_select.go:308, SEND node.go:1259, store.Add/newMakerSession/persist node.go:1246-1268). MATCH on the non-dryrun path.
- Dryrun: REF returns BEFORE sendXBridgeTransaction — no selection, no signing (rpcxbridge.cpp:1003-1022). CAND dryrun RUNS the full selection + proof-signing + (partial) prep-tx build, skipping only broadcast/store/session (node.go:1197-1199,1274). DIVERGENT work performed and failure surface (a CAND dryrun can fail with INSUFFICIENT_FUNDS/dust where C++ previews success).

VECTORS:
1. success field order: REF JSON key order `[id,maker_address,maker,maker_size,taker_address,taker,taker_size,created_at,updated_at,block_id,order_type,partial_minimum,partial_orig_maker_size,partial_orig_taker_size,partial_repost,partial_parent_id,status]` vs CAND `[id,maker,maker_size,taker,taker_size,updated_at,created_at,order_type,partial_minimum,partial_orig_maker_size,partial_orig_taker_size,partial_repost,partial_parent_id,status,maker_address,taker_address,block_id]`. DIVERGENT; also partial_minimum "0" vs "0.000000".
2. INSUFFICIENT_FUNDS: unfunded wallet → REF `{error:"Insufficient funds for <fromAddress>", code:1019, name:"dxMakeOrder"}` vs CAND `{error:"Insufficient funds for insufficient funds", code:1019, name:"dxMakeOrder"}`. DIVERGENT text.
3. dryrun: `(…, "dryrun")` → REF 14-field zero-id preview vs CAND 17-field object with real id + block_id. DIVERGENT.
4. NO_SESSION: no connector for maker → both `{error:"No session for currency Unable to connect to wallet: LTC", code:1018, name:"dxMakeOrder"}`. MATCH.
5. NO_SERVICE_NODE: no hub → REF `{error:"Could not find a service node with required services: ", code:1032, name:"dxMakeOrder"}` vs CAND `{...services: LTC/BLOCK}`. DIVERGENT.
6. edge: `maker_size="-5"` → REF "The minimum supported size is 0.000001" (1025); CAND "Invalid parameters: invalid maker_size" (1025). DIVERGENT text.

VERDICT: DIVERGENT — success/dryrun response field order; partial_* literal "0" vs "0.000000"; dryrun returns real (non-zero) id + block_id + extra fields; INSUFFICIENT_FUNDS text arg; NO_SERVICE_NODE text arg; missing INVALID_CURRENCY (length>8) path; unknown-currency maps to INVALID_PARAMETERS instead of NO_SESSION/INVALID_CURRENCY; <7-param gate is a business error instead of a thrown help error; use_all_funds/amount-parse error channel.

### dxMakePartialOrder
REF: rpcxbridge.cpp:2910 | CAND: api/handlers.go:306 (+ node.go:857 MakeOrder partial branch, utxo_select.go:153 selectPartialUtxos)

PARAMS:
- maker, maker_size, maker_address, taker, taker_size, taker_address | string | required | same as dxMakeOrder (REF rpcxbridge.cpp:3033-3039; CAND handlers.go:311-317).
- minimum_size | string | required | — | REF: `lexical_cast<double>(params[6])`; `partialMinimum > fromAmount` → "The minimum_size can't be more than maker_size" (rpcxbridge.cpp:3043-3046); dust via `connFrom->isDustAmount(partialMinimum)` → "The partial minimum_size is dust, i.e. it's too small." (3088-3091). CAND: parseXAmount → minFrom; `minFrom > fromAmt` same text (node.go:956-958); dust via `minFrom < effectiveDust(cc, relayFee)` (node.go:961-963). TEXT MATCH but THRESHOLD DIVERGENT: C++ compares `partialMinimum(whole coins) * 1e8 < dustAmount` (xbridgewalletconnectorbtc.cpp:1900-1904, native 1e8 scale); CAND compares `minFrom` (XBridge 1e6 base units) against `effectiveDust` (native-1e8 units, handlers.go:1449-1457) — for 1e8-native coins the CAND gate is 100x more lenient (accepts minimums C++ rejects as dust).
- repost | bool | optional | true | REF rpcxbridge.cpp:3093-3095; CAND handlers.go:321-328. MATCH (with the same bool-coercion caveat as use_all_funds).
- use_all_funds | bool | optional | true | same as dxMakeOrder (REF 3097-3099; CAND 329-336).
- auto_split | bool | optional | true | REF 3101-3103; CAND 337-344. MATCH.
- dryrun | string | optional (size==11) | "dryrun" | REF 3105-3113; CAND 345-356. MATCH.
- Arity: REF `<6` throws help (rpcxbridge.cpp:2912); CAND `<7` → business error (handlers.go:308-310). With exactly 6 params REF throws (help gate passes, then params[6] index throws); CAND business error. DIVERGENT (envelope vs result).

ERRORS (reachable):
- 1025 "Invalid parameters: The minimum_size can't be more than maker_size" | min>maker | REF 3043-3046; CAND 956-958. MATCH.
- 1025 "Invalid parameters: The partial minimum_size is dust, i.e. it's too small." | dust min | REF 3088-3091; CAND 961-963. TEXT MATCH; threshold unit-mismatch DIVERGENT (see params).
- 1019 "Insufficient funds for <fromCurrency>/<fromAddress>" (checkCreateParams/selection, rpcxbridge.cpp:3150-3152,3190-3191) vs CAND "Insufficient funds for insufficient funds" (node.go:994,1045). DIVERGENT text (same as dxMakeOrder).
- 1032 "Could not find a service node with required services: " (bare default, rpcxbridge.cpp:3157-3158,3192-3194) vs CAND "...services: BTC/SYS" (node.go:916,922). DIVERGENT text.
- All other dxMakeOrder error families (1025 precision/size/address, 1018, 1026) match as in dxMakeOrder; unknown currency / >8-char currency and negative-size divergences carry over.

SUCCESS SHAPE (REF rpcxbridge.cpp:3169-3188): same 17-field interleaved layout as dxMakeOrder — id, maker_address, maker, maker_size, taker_address, taker, taker_size, created_at, updated_at, block_id, order_type="partial", partial_minimum=<real>, partial_orig_maker_size=<real>, partial_orig_taker_size=<real>, partial_repost=<repost>, partial_parent_id="", status="created".
CAND `makePartialOrderResponse` (order.go:262-272) = orderBase + MakerAddress + TakerAddress + BlockID. Same DIVERGENT field order as dxMakeOrder (addresses/block_id at end, updated_at before created_at). Values MATCH for the partial_* fields (real amounts via formatXAmount). partial_repost = caller repost ✓.
DRYRUN (REF rpcxbridge.cpp:3122-3139): 14-field zero-id preview (no updated_at/created_at/block_id). CAND: full success shape with real id + block_id. DIVERGENT (same as dxMakeOrder).

SIDE EFFECTS:
- REF: selectPartialUtxos + optional autoSplit prep tx (xbridgeapp.cpp:1579-1604, 1636-1656, 1783-1953), proofs, SEND/session/store. CAND mirrors (utxo_select.go:153 selectPartialUtxos, prep-tx build node.go:1089-1176). MATCH on non-dryrun.
- Dryrun: REF returns before sendXBridgeTransaction (rpcxbridge.cpp:3120-3140); CAND runs selection + (autoSplit) prep-tx build+signing, skipping only broadcast/store. DIVERGENT work + failure surface.

VECTORS:
1. success field order: same interleave divergence as dxMakeOrder — REF `[id,maker_address,…created_at,updated_at,block_id,…]` vs CAND `[…updated_at,created_at,…,maker_address,taker_address,block_id]`. DIVERGENT.
2. INSUFFICIENT_FUNDS: unfunded → REF "Insufficient funds for <fromAddress>" (1019) vs CAND "Insufficient funds for insufficient funds" (1019). DIVERGENT.
3. dust minimum: `minimum_size="0.000001"` on BTC (relay 0, dust 5460) → REF: 0.000001*1e8=100 < 5460 → dust error; CAND: 1 base unit < 5460 → dust error. MATCH here; divergence appears only in the band `[5460/100, 5460)` (1e6 units) where CAND proceeds and C++ errors. DIVERGENT threshold.
4. dryrun: → REF 14-field zero-id preview vs CAND full 17-field real-id object. DIVERGENT.

VERDICT: DIVERGENT — same response field-order / dryrun-shape / insufficient-funds-text / no-service-node-text issues as dxMakeOrder; plus the partial-minimum dust gate compares 1e6-scale units against a native-1e8 dust constant (unit mismatch).

### dxTakeOrder
REF: rpcxbridge.cpp:1076 | CAND: api/handlers.go:373 (+ node.go:1328 TakeOrder)

PARAMS:
- id | string(hex) | required | — | REF: `uint256S(sid)` (case-insensitive 64-hex, loose), lookup miss → TRANSACTION_NOT_FOUND with NO id arg (rpcxbridge.cpp:1139,1176-1179). CAND: strict `orderIDKey` (64 lowercase hex, order.go:504-517) → not-found carrying the id (node.go:1338-1345). DIVERGENT message (below).
- from_address | string | required | — | same.
- to_address | string | required | — | same.
- amount | string | optional | — | REF: read when `size>=4` and non-empty; `amount <= 0` → "The amount cannot be less than or equal to 0: <params[3]>" (rpcxbridge.cpp:1151-1161). CAND: `if p.Amount != ""` → parseXAmount; an explicit "0" gives a==0 → NOT an error, treated as a FULL take (node.go:1351-1374). DIVERGENT semantics for "0". Negative/non-numeric: REF lexical_cast throws (envelope); CAND business error "invalid amount". DIVERGENT channel.
- dryrun | string | optional (size==5) | "dryrun" | REF rpcxbridge.cpp:1163-1171; CAND handlers.go:385-396. MATCH.
- Arity: REF `<3` or `>5` throws help (rpcxbridge.cpp:1077); CAND `<3` → business error (handlers.go:375-377) and `>5` params are NOT rejected (extras ignored). DIVERGENT both ends.

ERRORS (reachable):
- 1025 "Invalid parameters: The from_address and to_address cannot be the same: <fromAddress>" | addr equality | REF rpcxbridge.cpp:1146-1149; CAND node.go:1333-1335. MATCH.
- 1021 "Transaction  not found" (id OMITTED, double space) | order lookup miss | REF rpcxbridge.cpp:1176-1179 (makeError with no third arg); CAND "Transaction <id> not found" (node.go:1340,1344). DIVERGENT text.
- 1025 "Invalid parameters: The minimum amount for this order is: <amt>" / "The maximum amount for this order is: <amt>" | partial amount out of bounds | REF rpcxbridge.cpp:1187-1193; CAND node.go:1365-1369. MATCH.
- 1034 "Partial orders not allowed for this transaction" | amount on a non-partial order | REF rpcxbridge.cpp:1198-1201; CAND node.go:1362. MATCH (name "dxTakeOrder" both).
- 1018 "No session for currency <toCurrency>" | checkAcceptParams balance gate | REF rpcxbridge.cpp:1252-1255; CAND node.go:1300. MATCH.
- 1018 "No session for currency Unable to connect to wallet: <cur>" | connector missing | REF rpcxbridge.cpp:1216-1217; CAND node.go:1398-1402. MATCH.
- 1019 "Insufficient funds for <fromAddress>" | balance < fromSize | REF rpcxbridge.cpp:1257-1260; CAND node.go:1304,1318. MATCH.
- 1026 INVALID_ADDRESS | bad from/to address | REF: `makeError(INVALID_ADDRESS,__FUNCTION__,": "+cur+" address is bad. Are you using the correct address?")` → full text "Bad address : LTC address is bad. Are you using the correct address?" (rpcxbridge.cpp:1219-1225); CAND: decodeAddr (node.go:832-846) → "Bad address <addr>" with `name:"dxMakeOrder"`. DOUBLE DIVERGENT (text AND name field).
- 1030 "Amount is dust (very small)" | dust take size | REF bare makeError (xbridgeapp.cpp:2148-2156 → rpcxbridge.cpp:1293); CAND `makeError(errDust,"dxTakeOrder","taker amount is dust")` → template ignores arg → same text (node.go:1441-1445). MATCH.
- 1031 "Blocknet wallet amount is too small to cover the fee payment" | BLOCK fee balance | REF bare (xbridgeapp.cpp:2159-2163 → rpcxbridge.cpp:1293); CAND same text via template (node.go:1450). MATCH.
- 1032 "Could not find a service node with required services: " (bare) | unknown SN key | REF bare (xbridgeapp.cpp:2172 → rpcxbridge.cpp:1293); CAND `makeError(errNoServiceNode,"dxTakeOrder",p.ID)` → "...services: <id>" (node.go:1464,1468). DIVERGENT text.
- 1004 "Bad Request " (bare) | order already accepted | REF (xbridgeapp.cpp:2122-2125 → bare); CAND "Bad Request not accepting, order already accepted" (node.go:1589). DIVERGENT text.

SUCCESS SHAPE (REF rpcxbridge.cpp:1271-1286): id | maker(=order.toCurrency) | maker_size(fromSize) | taker(=order.fromCurrency) | taker_size(toSize) | updated_at(now) | created_at(order created) | order_type | partial_minimum | partial_orig_maker_size | partial_orig_taker_size | partial_repost | partial_parent_id | status("accepting").
CAND `toTakeResult` (order.go:208-215) = orderListResult/orderBase — SAME 14-field order. MATCH. (makeOrderResult with the interleaved layout is NOT used here.)
DRYRUN (REF rpcxbridge.cpp:1227-1242): id="000…0", maker=order.fromCurrency (pre-swap), maker_size=fromSize, taker=order.toCurrency, taker_size=toSize, updated_at(now), created_at, order_type, partial_*, status="filled". CAND `toTakeDryrunResult` (order.go:220-229) — same field order, zero id, status "filled". MATCH.

SIDE EFFECTS:
- REF: checkAcceptParams → self-trade/connector/address gates → dryrun return → acceptXBridgeTransaction (state→trAccepting, fromAmount/toAmount swapped, dust, fee balance, SN check, funding selection, service-node fee tx, Accepting broadcast, session). CAND mirrors (node.go:1385-1684). MATCH on non-dryrun.

VECTORS:
1. success field order: `[id,maker,maker_size,taker,taker_size,updated_at,created_at,order_type,partial_minimum,partial_orig_maker_size,partial_orig_taker_size,partial_repost,partial_parent_id,status]` — CONFORMANT both (verified orderBase == C++ emission order).
2. TRANSACTION_NOT_FOUND: unknown id → REF `{error:"Transaction  not found", code:1021, name:"dxTakeOrder"}` vs CAND `{error:"Transaction <id> not found", code:1021, name:"dxTakeOrder"}`. DIVERGENT.
3. INVALID_ADDRESS: bad to_address → REF `{error:"Bad address : LTC address is bad. Are you using the correct address?", code:1026, name:"dxTakeOrder"}` vs CAND `{error:"Bad address <addr>", code:1026, name:"dxMakeOrder"}`. DIVERGENT (text + name).
4. amount "0" (explicit): REF 1025 "The amount cannot be less than or equal to 0: 0"; CAND proceeds as a full take (accepts). DIVERGENT.
5. INSUFFICIENT_FUNDS (balance gate): both `{error:"Insufficient funds for <fromAddress>", code:1019, name:"dxTakeOrder"}`. MATCH.
6. already-accepted: REF `{error:"Bad Request ", code:1004, name:"dxTakeOrder"}` vs CAND "Bad Request not accepting, order already accepted". DIVERGENT text.

VERDICT: DIVERGENT — TRANSACTION_NOT_FOUND message (id omitted in C++), INVALID_ADDRESS text + name field (CAND leaks "dxMakeOrder"), explicit amount "0" treated as full take instead of error, >5-param arity not rejected, and later-stage INSUFFICIENT_FUNDS / BAD_REQUEST / NO_SERVICE_NODE message arguments differ. Success and dryrun SHAPES are CONFORMANT (field order, statuses, zero dryrun id all match).

---
CROSS-CUTTING NOTES
- Envelope vs result errors: C++ throws (RPC/`error` envelope, code -1 + help text) for arity/type violations reached before the business gates (fHelp, `size<7`/`size<3||>5`/`size<6`, `get_str()`/`get_bool()`/`lexical_cast` on bad types); the Go port converts ALL of these into `{error,code,name}` business objects riding in `result` with a null envelope error. Every such path differs in envelope placement even when code 1025 matches.
- `makeError` template texts match 1:1 for the reachable cases (response.go:107-163 vs xbridgeerror.cpp:17-75), incl. the odd `errDust`/`errInsufficientFundsDX` ignoring the arg; the divergences are in the ARGUMENTS passed (currency vs "insufficient funds", bare vs pair/id).
- Amount rendering `formatXAmount` vs `xBridgeStringValueFromAmount`: byte-identical for all 6-decimal values exercised (the C++ `+1/::COIN` term never reaches the 6th decimal on 6-decimal-scale amounts) EXCEPT C++'s literal "0" strings on dxMakeOrder success/dryrun.
- Timestamps: both millisecond ISO-8601 (`iso8601` xutil.cpp:185-200 vs response.go:393-399, zero→1970-01-01T00:00:00.000Z). dxMakeOrder C++ updated_at is build-time-now (later than created_at); CAND emits updated_at==created_at==order timestamp.


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


## RPC CONFORMANCE CARDS — GROUP 5 (dxSplitAddress, dxSplitInputs, dxGetUtxos, getnetworkinfo) + ENVELOPE/TRANSPORT

Notes on conventions used below:
- REF = blocknet_core (C++, source of truth). CAND = go-xbridge/api (Go).
- REF "thrown" errors: UniValue `get_str()/get_bool()/get_array()` throw `std::runtime_error`
  (univalue_get.cpp:79-144, e.g. "JSON value is not a boolean as expected"); CRPCTable::execute
  catches and wraps in `JSONRPCError(RPC_MISC_ERROR=-1, e.what())` (rpc/server.cpp:584-587), which
  surfaces as the envelope `error` (HTTP 500 unless -32600→400 / -32601→404, httprpc.cpp:70-85).
- REF "business" errors: `makeError(code, __FUNCTION__, msg)` → json_spirit Object
  {error,code,name} → `uret()` (rpcxbridge.cpp:49-54) → returned as the RPC **result**, envelope
  error stays null, HTTP 200 (xutil.cpp:384-391; httprpc.cpp:192-196).
- Both envelopes are compact no-space JSON: REF UniValue::write() emits no spaces after ':'/','
  (univalue_write.cpp:29-112); the json_spirit::write_string(o, none, 8) intermediate is
  re-parsed by `uv.read()` and re-emitted compact (rpcxbridge.cpp:51), so its width-8 arg never
  reaches the wire. CAND encoding/json Encoder is likewise no-space (server.go:190-194).
- Field order inside CAND results that are `map[string]interface{}` is **sorted by key** by
  encoding/json; REF preserves UniValue pushKV insertion order. This changes bytes.

---

### dxSplitAddress
REF: rpcxbridge.cpp:3197 | CAND: api/handlers.go:1223
PARAMS:
- token | string | required | — | REF `request.params[0].get_str()` (rpcxbridge.cpp:3248); non-string throws → envelope -1 (univalue_get.cpp:97-100). CAND `strParam(params,0)` (handlers.go:1228); ok is **ignored** → proceeds with `""` → later NO_SESSION / address-decode error (handlers.go:1228-1230, 1335-1338).
- splitamount | string | required | — | REF `params[1].get_str()` then `boost::lexical_cast<double>` (rpcxbridge.cpp:3249,3267) → `xBridgeIntFromReal` = `int64(amt*COIN + 1/COIN)` (truncate, xutil.cpp:232-237). CAND `strParam(params,1)` → `parseXAmount` exact big.Int, no float (handlers.go:1229,1308, response.go:271-316). Float-vs-exact: identical for normal inputs; off-by-one-sat risk on double-boundary inputs (REF adds +1 sat rounding).
- address | string | required | — | REF `params[2].get_str()` (rpcxbridge.cpp:3250); validated inside splitUtxos "address is invalid or not in the wallet for token X" (xbridgewalletconnectorbtc.cpp:2639-2642). CAND `strParam(params,2)` (handlers.go:1230); validated via `legacyOutputScript` → 1026 "Bad address <addr>" or segwit rejection (handlers.go:1486-1501).
- include_fees | bool | optional | default=true | REF `if (!params[3].isNull()) includeFees = params[3].get_bool()` (rpcxbridge.cpp:3251-3255). CAND `mustBool(params,3,true,...)` (handlers.go:1231). DIVERGENT: JSON null → REF keeps default true; CAND null → `false` (dispatch.go:97-112, null no-op on bool). String "true"/"false" → REF get_bool() throws envelope -1; CAND tolerates (dispatch.go:103-108).
- show_rawtx | bool | optional | default=false | REF (rpcxbridge.cpp:3252,3256-3257); CAND (handlers.go:1235-1237). Same null/string leniency divergence as include_fees.
- submit | bool | optional | default=true | REF (rpcxbridge.cpp:3253,3258-3259); CAND (handlers.go:1239-1241). Same.
- Param count: REF requires size∈[3,6] else **throw** help text (rpcxbridge.cpp:3199-3246); CAND size∈[3,6] else **business** 1025 (handlers.go:1225-1227). Channel DIVERGENT.
ERRORS:
- (thrown) | REF: envelope {code:-1, message: RPCHelpMan help text} (rpcxbridge.cpp:3199-3246 → rpc/server.cpp:584-587) | params.size() not in [3,6] | CAND: business result {error:"Invalid parameters: (token) (splitamount) (address) (include_fees, default=true)[optional] (show_rawtx, default=false)[optional] (submit, default=true)[optional]", code:1025, name:"dxSplitAddress"} (handlers.go:1225-1227). DIVERGENT (channel/code/text).
- 1018 | REF: "No session for currency " + token, name="dxSplitAddress" (rpcxbridge.cpp:3262-3264) | no connector for token | CAND: identical text, but name="dx" (handlers.go:1298-1301 via connector() handlers.go:198-207). NAME FIELD DIVERGENT.
- 1004 | REF: "Bad Request " + failReason (rpcxbridge.cpp:3272-3273) where failReason ∈ {"split amount is dust [...]", "address is invalid...", "failed to get unspent transaction outputs for token X", "already split all unused utxos in address [X]", "unable to split further..."} (xbridgewalletconnectorbtc.cpp:2635-2646,2678-2680,2695-2697,2736-2739) | splitUtxos() failed | CAND: "split amount is dust [...]" code 1004 name "dxSplit" (handlers.go:1316-1319); "no UTXOs to split" code 1019 (handlers.go:1331-1333); "insufficient funds for split amount" code 1019 (handlers.go:1367-1369). DIVERGENT (code/name/text for several paths).
- 1004 | REF: "Bad Request " + errmsg (rpcxbridge.cpp:3277-3278) | sendRawTransaction failed | CAND: {error:"Internal Server Error", code:1002, name:"dxSplit"} (handlers.go:1420-1424; errUnknown→"Internal Server Error", response.go:113-114). DIVERGENT (code/text/name).
- (not reachable) | REF never errors "unknown coin": an unknown token just yields NO_SESSION (rpcxbridge.cpp:3262) | — | CAND: `coins.Get` miss → {error:"Invalid parameters: unknown coin: "+ticker, code:1025, name:"dxSplit"} (handlers.go:1302-1305). Only reachable when a connector exists for a ticker absent from the static registry — EXTRA error path.
SUCCESS SHAPE: Object, 8 fields in REF insertion order: token(str), include_fees(bool), split_amount_requested(str, fixed 6-dec), split_amount_with_fees(str), split_utxo_count(int), split_total(str), txid(str), rawtx(str) (rpcxbridge.cpp:3280-3289). CAND returns `map[string]interface{}` (handlers.go:1426-1435) → encoding/json sorts keys: include_fees, rawtx, split_amount_requested, split_amount_with_fees, split_total, split_utxo_count, token, txid. **FIELD ORDER DIVERGENT (bytes)**.
- split_amount_requested: REF xBridgeStringValueFromAmount(sa) fixed 6 decimals (rpcxbridge.cpp:3283, xutil.cpp:202-207); CAND formatXAmount(targetXB) fixed 6 (handlers.go:1429, response.go:237-241). MATCH for normal inputs (see splitamount parse note above).
- split_amount_with_fees: REF splitSize = splitAmount + (includeFees ? feesPerUtxo : 0), feesPerUtxo = minTxFee1(1,3)+minTxFee2(1,1) = (192·1+34·3)+(192·1+34·1) = 294+226 = 520 byte-equivalents × feePerByte, each floored at minTxFee (xbridgewalletconnectorbtc.cpp:2665-2668,1949-1972). CAND splitSize = target + estimateFee(cc, len(utxos), 2) = (192·nIn + 34·2)·feePerByte = 192·nIn+68 (handlers.go:1356-1362,1463-1482). **FEE FORMULA DIVERGENT** (520 vs 192·nIn+68; CAND also scales with input count).
- split_utxo_count: REF outputCount = vinsTotal/splitSize capped 100 (xbridgewalletconnectorbtc.cpp:2694-2700); CAND nSplits = total/splitSize capped 100 (handlers.go:1363-1366). Same shape; value differs because spend-set differs.
- split_total: REF = vinsTotal = sum of UTXOs at **`address` only**, after dropping utxos whose amount already equals splitSize and after dropping locked ones, inputs capped at 100 (xbridgewalletconnectorbtc.cpp:2670-2691,2762,2683-2685). CAND = sum of **ALL** wallet unspent (ListUnspent, no address filter, no size-exclusion) (handlers.go:1321-1330,1348-1353). **VALUE DIVERGENT** in any non-degenerate wallet.
- txid: REF from decodeRawTransaction of the signed tx (xbridgewalletconnectorbtc.cpp:2754-2761); CAND double-SHA256(signed raw)+byte-reverse (handlers.go:1411,1520-1531). Same derivation. MATCH.
- rawtx: REF showRawTx ? rawtx : "" (rpcxbridge.cpp:3288); CAND same (handlers.go:1416-1419). MATCH.
- Change output: REF sends change back to `addr` (xbridgewalletconnectorbtc.cpp:2703-2711); CAND sends change to a fresh GetNewAddress (handlers.go:1339,1398-1400). **DIVERGENT** (on-chain effect + change-address field in rawtx).
- Spend set / locked exclusion: REF excludes locked utxos (passes getAllLockedUtxos as `excluded` into getUnspent, rpcxbridge.cpp:3266,3272; xbridgewalletconnectorbtc.cpp:2644) and filters to `address` (2670-2676). CAND never consults the Store's locked set and never filters by address (handlers.go:1321-1330). **DIVERGENT**.
SIDE EFFECTS:
- REF: builds + signs tx via signRawTransaction (xbridgewalletconnectorbtc.cpp:2744-2752); if submit → sendRawTransaction broadcast (rpcxbridge.cpp:3277). No db/order-state writes. CAND: SignRawTransaction then conditional SendRawTransaction (handlers.go:1403,1420-1424). No db writes. Broadcast intent MATCH.
VECTORS:
1. [params ["BTC","0.5",addr], wallet has exactly one 1.0-BTC UTXO at addr] → REF {token:"BTC",include_fees:true,split_amount_requested:"0.500000",split_amount_with_fees:<520·fee 6-dec>,split_utxo_count:1,split_total:"1.000000",txid:<64hex>,rawtx:""} (rpcxbridge.cpp:3280-3289). CAND same values only if wallet has no other UTXO and no locked ones; if any other wallet UTXO exists split_total/count/inputs differ (handlers.go:1321-1330). Field bytes always differ (map-sorted keys). DIVERGENT.
2. [params ["BTC"]] (2 params) → REF thrown envelope {code:-1, message:"dxSplitAddress\n\nSplits unused coin..."} (rpcxbridge.cpp:3199) → CAND business result 1025 (handlers.go:1226). DIVERGENT.
3. [params ["BTC","0.5",addr,null]] (include_fees null) → REF includeFees=true (default kept, rpcxbridge.cpp:3251-3255) → CAND include_fees=false (dispatch.go:97-112 null no-op). DIVERGENT.
4. [unknown token "DOGE"] → REF result {error:"No session for currency DOGE", code:1018, name:"dxSplitAddress"} (rpcxbridge.cpp:3262-3264) → CAND {..., name:"dx"} (handlers.go:198-207). DIVERGENT (name field only).
5. [a wallet UTXO is locked by an order; dxSplitAddress] → REF excludes it (rpcxbridge.cpp:3266) → CAND includes it (splitTx ignores locked set). DIVERGENT.
6. [submit=true and broadcast fails] → REF {error:"Bad Request "+errmsg, code:1004, name:"dxSplitAddress"} (rpcxbridge.cpp:3277-3278) → CAND {error:"Internal Server Error", code:1002, name:"dxSplit"} (handlers.go:1420-1424). DIVERGENT.
VERDICT: DIVERGENT — param-count errors in business channel vs REF thrown -1; null include_fees→false vs REF default true; error `name` "dx"/"dxSplit" instead of "dxSplitAddress"; submit failure 1002 vs 1004; spend set not address-filtered and not locked-excluded; change goes to fresh address not `addr`; split_amount_with_fees fee formula differs (520·feePerByte vs (192·nIn+68)·feePerByte); split_total counts whole wallet not address; result object key order sorted vs REF insertion order (bytes).

### dxSplitInputs
REF: rpcxbridge.cpp:3293 | CAND: api/handlers.go:1246
PARAMS: (all of dxSplitAddress plus:)
- include_fees/show_rawtx/submit | bool | required (no default) | — | REF reads params[3..5] via `get_bool()` **unconditionally** (rpcxbridge.cpp:3355-3357); a missing/null/wrong-typed param throws envelope -1. CAND `mustBool(params,3..5,false,...)` (handlers.go:1260-1270). Null→false both sides here (REF would throw on null — get_bool on null throws; CAND null→false). DIVERGENT.
- utxos | array of {txid(str), vout(int)} | required | — | REF `params[6].get_array()`; per element reads only `utxo["txid"]` (get_str) and `utxo["vout"]` (get_int) into COutPoint (rpcxbridge.cpp:3358,3363-3367). CAND `parseUtxoParam` requires txid, vout, AND amount(str), scriptPubKey(str), address(str) — a missing amount errors 1025 "invalid utxo amount" (handlers.go:1627-1647,1640-1643). **DIVERGENT** (CAND requires fields C++ never reads).
- Param count: REF guards size∈[3,7] (rpcxbridge.cpp:3295) but only ever succeeds with all 7 present (params[3..6] read unconditionally; missing → UniValue operator[] yields Null → get_bool/get_array throw). CAND enforces exactly 7 (handlers.go:1254-1256). Reachable-success set MATCH; error channel DIVERGENT.
ERRORS:
- (thrown) | REF: envelope -1 with help text | size<3 or >7 (rpcxbridge.cpp:3295) | CAND: business 1025 "(token) (splitamount) (address) (include_fees) (show_rawtx) (submit) (utxos)" (handlers.go:1255). DIVERGENT (channel).
- (thrown) | REF: envelope -1 "JSON value is not a boolean as expected" (univalue_get.cpp:90-93) | missing/null/non-bool params[3..5] (rpcxbridge.cpp:3355-3357) | CAND: business 1025 "param N is not a boolean" (dispatch.go:154-160). DIVERGENT (channel/code/text).
- 1004 | REF: "Bad Request No utxos were specified" (rpcxbridge.cpp:3359-3360) | params[6] empty array | CAND: identical text/code/name (handlers.go:1280-1282). MATCH.
- 1004 | REF: "Bad Request Cannot split utxo already in use: <txid>:<vout>" (rpcxbridge.cpp:3374-3379) | a user-specified utxo is in the locked set | CAND: **no such gate** (splitTx never consults Store locked set). MISSING.
- 1004 | REF: "Bad Request user specified utxo was not found or is not available: <txid>:<vout>" (xbridgewalletconnectorbtc.cpp:2658-2661) | requested utxo not in wallet unspent | CAND: **no such check**; parseUtxoParam trusts caller (handlers.go:1635-1647); a bogus utxo surfaces later as a sign/parse error (handlers.go:1386-1389,1403-1409). DIVERGENT (missing gate + different failure point).
- 1018 | REF: "No session for currency " + token, name="dxSplitInputs" (rpcxbridge.cpp:3370-3372) | no connector | CAND: name="dx" (handlers.go:1298-1301,198-207). NAME DIVERGENT.
- 1004 | REF: "Bad Request " + failReason (rpcxbridge.cpp:3386-3387) | splitUtxos() failed | CAND: dust 1004 / no-utxos 1019 / insufficient 1019 / sign-incomplete 1002, all name "dxSplit" (handlers.go:1316-1319,1331-1333,1367-1369,1407-1409). DIVERGENT.
- 1004 | REF: "Bad Request " + errmsg (rpcxbridge.cpp:3391-3392) | sendRaw failed | CAND: 1002 "Internal Server Error" name "dxSplit" (handlers.go:1420-1424). DIVERGENT.
- 1025 | (REF n/a — CAND-only) | — | CAND: "unknown coin: "+ticker (handlers.go:1272-1275,1302-1305) — extra path, see dxSplitAddress.
SUCCESS SHAPE: Identical 8-field object/shape/order as dxSplitAddress (rpcxbridge.cpp:3394-3403; handlers.go:1426-1435), with all the same divergences: map-key-sorted bytes; fee formula 520·feePerByte vs (192·nIn+68)·feePerByte (fee comes from the same splitUtxos code for both dxSplit* in REF); change-to-fresh-address; result split_total = user-specified-utxos total (REF restricts unspent to userUtxos, xbridgewalletconnectorbtc.cpp:2648-2663; CAND totals the passed utxos, handlers.go:1348-1353 — consistent intent here). Locked-utxo pre-check absent in CAND.
SIDE EFFECTS: same as dxSplitAddress (sign + optional broadcast; REF rpcxbridge.cpp:3391-3392; CAND handlers.go:1403,1420-1424).
VECTORS:
1. [utxos elements have ONLY txid+vout — exactly the C++ help example shape (rpcxbridge.cpp:3347-3348)] → REF succeeds (parses txid/vout, 3363-3367) → CAND fails business 1025 "invalid utxo amount" (amount "" → coins.ParseAmount error, handlers.go:1640-1643, coins/amount.go:13-16). DIVERGENT.
2. [6 params (no utxos)] → REF params[6] is null → get_array() throws envelope -1 (rpcxbridge.cpp:3358, univalue_get.cpp:141-144) → CAND business 1025 (handlers.go:1255). DIVERGENT.
3. [7 params, utxos=[]] → REF {error:"Bad Request No utxos were specified", code:1004, name:"dxSplitInputs"} (rpcxbridge.cpp:3359-3360) → CAND identical (handlers.go:1280-1282). MATCH.
4. [utxo passed that is locked by an order] → REF {error:"Bad Request Cannot split utxo already in use: <txid>:<vout>", code:1004, name:"dxSplitInputs"} (rpcxbridge.cpp:3378) → CAND proceeds and splits. DIVERGENT (gate missing).
5. [all 7 valid params, success] → REF split_amount_with_fees = splitAmount + (minTxFee1(1,3)+minTxFee2(1,1)) = +520·feePerByte (xbridgewalletconnectorbtc.cpp:2665-2668) → CAND = + (192·nIn+68)·feePerByte (handlers.go:1360-1362). DIVERGENT (fee amount; equals only when nIn≈2.35, never exact).
VERDICT: DIVERGENT — CAND requires amount/scriptPubKey/address in utxos elements (REF only needs txid/vout, so the documented example input errors in CAND); "Cannot split utxo already in use" and "user specified utxo was not found" gates missing; param-count/type errors business-channel 1025 vs REF thrown envelope -1; submit failure 1002 vs 1004; name "dx"/"dxSplit" vs "dxSplitInputs"; fee formula and change-address divergences inherited from splitTx.

### dxGetUtxos
REF: rpcxbridge.cpp:3406 | CAND: api/handlers.go:1653
PARAMS:
- token | string | required | — | REF `params[0].get_str()` (rpcxbridge.cpp:3463); non-string throws envelope -1. CAND `strParam(params,0)` with ok check → business 1025 (handlers.go:1657-1659). DIVERGENT (channel).
- include_used | bool | optional | default=false | REF `if (!params[1].isNull()) includeUsed = params[1].get_bool()` (rpcxbridge.cpp:3464-3466). CAND len≥2 → boolParam (handlers.go:1662-1668). null→false both sides. String "true" → REF throws envelope -1; CAND accepts. DIVERGENT (leniency).
- Param count: REF size∈[1,2] else throw help text (rpcxbridge.cpp:3408-3461); CAND size∈[1,2] else business 1025 (handlers.go:1654-1655). Channel DIVERGENT.
ERRORS:
- (thrown) | REF: envelope -1 help text (rpcxbridge.cpp:3408) | size not in [1,2] | CAND: business 1025 "(token) (include_used, default=false)[optional]" (handlers.go:1655). DIVERGENT.
- (thrown) | REF: envelope -1 "JSON value is not a boolean as expected" (univalue_get.cpp:90-93) | params[1] present but not a bool | CAND: business 1025 "invalid include_used" (handlers.go:1666). DIVERGENT (channel/code/text).
- 1018 | REF: "No session for currency " + token, name="dxGetUtxos" (rpcxbridge.cpp:3469-3471) | no connector | CAND: name="dx" (handlers.go:1670-1672 via 198-207). NAME DIVERGENT.
- 1004 | REF: "Bad Request failed to get unspent transaction outputs" (rpcxbridge.cpp:3475-3476) | getUnspent() failed | CAND: {error:"Internal Server Error", code:1002, name:"dxGetUtxos"} (handlers.go:1678-1681; errUnknown→"Internal Server Error", response.go:113-114). DIVERGENT (code+text).
SUCCESS SHAPE: Array of objects. REF per-entry insertion order: txid(str), vout(int), amount(str), address(str), scriptPubKey(str), confirmations(int), orderid(str) (rpcxbridge.cpp:3480-3492). CAND per-entry `map[string]interface{}` (handlers.go:1698-1706) → sorted keys: address, amount, confirmations, orderid, scriptPubKey, txid, vout. **FIELD ORDER DIVERGENT (bytes)**.
- amount: REF `xBridgeStringValueFromPrice(utxo.amount, conn->COIN)` — fixed with xBridgeSignificantDigits(conn->COIN) decimals (rpcxbridge.cpp:3483, xutil.cpp:216-221,263-274): 8 decimals for BTC (COIN=1e8), 6 for BLOCK (COIN=1e6); conn->COIN is the per-coin denomination member (xbridgewallet.h:179). CAND `coins.FormatAmount(c, u.Amount)` **trims trailing zeros** (handlers.go:1702, coins/amount.go:65-81) → "1" for 1 BTC vs REF "1.00000000". **DIVERGENT (precision/trimming)**.
- vout | int | REF static_cast<int>(utxo.vout) (3482); CAND uint32 u.Vout (1700). Numeric both; int widths OK.
- confirmations | int | REF static_cast<int>(utxo.confirmations) (3486); CAND u.Confirmations (1704). OK.
- txid/address/scriptPubKey | str | REF utxo.txId/address/scriptPubKey (3481,3484-3485); CAND u.TxID/u.Address/u.ScriptPubKey (1699-1703). MATCH.
- orderid | str | REF "" or orderWithUtxo().GetHex() (3487-3491); CAND "" or Store.LockedUtxoInfo per-utxo id (1683,1694-1697). MATCH intent.
- Empty result: REF empty VARR → `[]` (rpcxbridge.cpp:3478); CAND `make([]map[string]interface{},0,len)` → `[]` (handlers.go:1684). MATCH (never null).
- Locked exclusion: REF getUnspent(unspent, !includeUsed ? excluded : {}) (rpcxbridge.cpp:3473-3475); CAND `if !includeUsed && keys[k] { continue }` (handlers.go:1691-1693). MATCH.
SIDE EFFECTS: none — read-only both sides (REF 3473-3475; CAND 1678-1683).
VECTORS:
1. [no unspent, params ["BLOCK"]] → REF `[]` (rpcxbridge.cpp:3478) → CAND `[]` (handlers.go:1684,1708). MATCH.
2. [one 1-BTC unspent, params ["BTC"]] → REF amount "1.00000000" (8-dec fixed, rpcxbridge.cpp:3483) → CAND amount "1" (trimmed, coins/amount.go:65-81). DIVERGENT.
3. [params ["BTC","true"]] (string bool) → REF get_bool() throws envelope -1 (rpcxbridge.cpp:3465-3466) → CAND accepts, includeUsed=true (dispatch.go:97-112). DIVERGENT.
4. [unknown token "X"] → REF {error:"No session for currency X", code:1018, name:"dxGetUtxos"} (rpcxbridge.cpp:3469-3471) → CAND {..., name:"dx"} (handlers.go:1670-1672,200). DIVERGENT (name).
5. [locked utxo, include_used=false] → REF excluded (rpcxbridge.cpp:3473-3475) → CAND excluded (handlers.go:1691-1693). MATCH.
6. [getUnspent/listunspent failure] → REF {error:"Bad Request failed to get unspent transaction outputs", code:1004, name:"dxGetUtxos"} (rpcxbridge.cpp:3475-3476) → CAND {error:"Internal Server Error", code:1002, name:"dxGetUtxos"} (handlers.go:1678-1681). DIVERGENT.
VERDICT: DIVERGENT — amount strings trimmed (no fixed 6/8 decimals); per-entry key order map-sorted vs REF insertion; param-count/type errors business 1025 vs REF thrown envelope -1; getUnspent failure 1002 "Internal Server Error" vs REF 1004 "Bad Request failed to get unspent transaction outputs"; NO_SESSION name "dx" vs "dxGetUtxos"; string include_used accepted instead of thrown.

### getnetworkinfo
REF: blocknet_core/src/rpc/net.cpp:447 (handler), field emission net.cpp:495-527 | CAND: api/handlers.go:1721
(This is a blocknetd **core** RPC — net.cpp:758 registration — not an xbridge command; CAND exposes it as a shim so BLOCK-DX's wallet-version gate sees a blocknet-shaped answer. Compared against the real daemon field set/types.)
PARAMS: none. REF size≠0 → **throw** help text (net.cpp:449-493). CAND size≠0 → business 1025 "no parameters" (handlers.go:1722-1724). Channel DIVERGENT.
ERRORS:
- (thrown) | REF: envelope -1, help text (net.cpp:449) | any params | CAND: business {error:"Invalid parameters: no parameters", code:1025, name:"getnetworkinfo"} (handlers.go:1723). DIVERGENT.
SUCCESS SHAPE: REF object, 15 fields in insertion order (net.cpp:495-527); CAND `map[string]interface{}` → 13 fields, sorted keys (handlers.go:1743-1761).
- version | int | REF CLIENT_VERSION = 1000000·MAJOR+10000·MINOR+100·REV+BUILD (clientversion.h:38-42); for the v4.4.1 tag = 4040100. CAND config WalletVersion, default 4040100 (handlers.go:1725-1728, node.go:90-94). MATCH at default; CAND is operator-configurable, REF is compiled-in.
- subversion | str | REF FormatSubVersion(CLIENT_NAME="Blocknet", CLIENT_VERSION, uacomments) → "/Blocknet:4.4.1/" (capital B; net.cpp:498, clientversion.cpp:15,87-102, init.cpp:1412). CAND default "/blocknet:4.4.1/" (lowercase; handlers.go:1729-1732). **DIVERGENT (case; also REF can carry -uacomment suffixes)**.
- protocolversion | int | REF PROTOCOL_VERSION = **70713** (version.h:12). CAND hardcoded **70015** (handlers.go:1746). **DIVERGENT value**.
- xbridgeprotocolversion | int | REF XBRIDGE_PROTOCOL_VERSION = **55** (net.cpp:500; xbridge/version.h:8). CAND: **field absent**. MISSING.
- xrouterprotocolversion | int | REF XROUTER_PROTOCOL_VERSION = **50** (net.cpp:501; xrouter/version.h:8). CAND: **field absent**. MISSING.
- localservices | str | REF strprintf("%016x", g_connman->GetLocalServices()) — live node services (net.cpp:502-503). CAND hardcoded "000000000000000d" (handlers.go:1747). DIVERGENT (static vs live; real daemon also advertises XBRIDGE/XROUTER bits).
- localrelay | bool | REF g_relay_txes (net.cpp:504); CAND true (1748). OK at default.
- timeoffset | int | REF GetTimeOffset() — median-filtered clock offset (net.cpp:505); CAND 0 (1749). OK at default; diverges on skewed daemons.
- networkactive | bool | REF g_connman->GetNetworkActive() (net.cpp:506-507); CAND true (1750). OK at default.
- connections | int | REF g_connman->GetNodeCount(CONNECTIONS_ALL) (net.cpp:508); CAND live peer count or 1 (handlers.go:1733-1742). MATCH intent.
- networks | array | REF GetNetworksInfo() → per-network {name, limited, reachable, proxy, proxy_randomize_credentials} (net.cpp:426-445,510). CAND entries {name, limited, reachable, proxy} only (handlers.go:1752-1756). **MISSING proxy_randomize_credentials per entry**.
- relayfee | num | REF ValueFromAmount(::minRelayTxFee.GetFeePerK()) → VNUM string "%d.%08d"; default minRelayTxFee = 10000 sat/kB (validation.h:59) → default `0.00010000` (net.cpp:511, core_write.cpp:19-27). CAND Go float 0.00001 → `0.00001` (handlers.go:1757). **DIVERGENT: value (0.00010000 vs 0.00001) AND bytes (trailing zeros)**, unless a -minrelaytxfee override happens to land on 1000 sat/kB.
- incrementalfee | num | REF ValueFromAmount(::incrementalRelayFee.GetFeePerK()); default incrementalRelayFee = 1000 sat/kB (policy.h:34) → default `0.00001000` (net.cpp:512). CAND `0.00000001` (handlers.go:1758). **DIVERGENT: value (0.00001000 vs 0.00000001) AND bytes**.
- localaddresses | array | REF mapLocalHost entries {address,port,score} (net.cpp:513-525); CAND always `[]` (handlers.go:1759). DIVERGENT (never populated).
- warnings | str | REF GetWarnings("statusbar") (net.cpp:526); CAND "" (1760). OK at default.
- Go map marshals keys sorted (connections, incrementalfee, localaddresses, localrelay, localservices, networkactive, networks, protocolversion, relayfee, subversion, timeoffset, version, warnings) vs REF insertion order above → **FIELD ORDER DIVERGENT (bytes)**.
SIDE EFFECTS: none (read-only both sides).
VECTORS:
1. [no params] → REF 15-field object (net.cpp:495-527) → CAND 13-field object missing xbridgeprotocolversion/xrouterprotocolversion, networks entries missing proxy_randomize_credentials (handlers.go:1743-1761). DIVERGENT.
2. [params ["x"]] → REF thrown envelope -1 with help text (net.cpp:449-493) → CAND business 1025 (handlers.go:1723). DIVERGENT.
3. [read protocolversion] → REF 70713 (version.h:12) → CAND 70015 (handlers.go:1746). DIVERGENT.
4. [read subversion on default config] → REF "/Blocknet:4.4.1/" (clientversion.cpp:15,87-102) → CAND "/blocknet:4.4.1/" (handlers.go:1731). DIVERGENT (case).
5. [read relayfee] → REF `0.00010000` (net.cpp:511, core_write.cpp:19-27; default 10000 sat/kB validation.h:59) → CAND `0.00001` (handlers.go:1757). DIVERGENT value+bytes.
6. [read localservices on an exchange node] → REF live "%016x" incl. XBRIDGE/XROUTER bits (net.cpp:502-503) → CAND "000000000000000d" (handlers.go:1747). DIVERGENT.
7. [read incrementalfee] → REF `0.00001000` (net.cpp:512; default 1000 sat/kB policy.h:34) → CAND `0.00000001` (handlers.go:1758). DIVERGENT value.
VERDICT: DIVERGENT — protocolversion 70713 vs 70015; missing xbridgeprotocolversion(55)/xrouterprotocolversion(50) fields; networks entries missing proxy_randomize_credentials; subversion default "/blocknet:" lowercase vs "/Blocknet:"; relayfee 0.00001 vs REF 0.00010000 and incrementalfee 0.00000001 vs REF 0.00001000 (value + bytes); localservices/localaddresses hardcoded/empty vs live; param-count error business 1025 vs REF thrown -1; object key order sorted vs REF insertion.

---

## ENVELOPE & TRANSPORT CONFORMANCE
Compare REF blocknet_core RPC transport against CAND go-xbridge/api transport.

Request parsing:
- Positional params: REF reads params positionally from `request.params` (server.cpp:457-464); named args are supported when params is an object via `transformNamedArguments` (server.cpp:459,510-554) — unknown key → RPC_INVALID_PARAMETER -8 (server.cpp:549-551). CAND parses `params []json.RawMessage` positionally only (dispatch.go:70-73); an object `params` makes the whole-body Unmarshal fail → -32700 "Parse error" (server.go:20,156-164). **Named-argument support MISSING in CAND** (REF honors it; CAND hard-errors).
- id: REF parses id first, before method/params, so pre-dispatch errors still echo it; missing id → null (server.cpp:441-442, protocol.cpp:47-48). CAND echoes req.ID RawMessage (server.go:166,187); on a whole-body Unmarshal failure the id is lost → null (server.go:157-163). Minor divergence on malformed bodies.
- Batch: REF accepts a top-level array of requests → JSONRPCExecBatch (httprpc.cpp:198-199; server.cpp:497-504). CAND has **no batch support**; array body fails Unmarshal → -32700 (server.go:157). **MISSING feature**.
- "method" missing: REF JSONRPCError(RPC_INVALID_REQUEST=-32600, "Missing method") → HTTP 400 (server.cpp:446-447, httprpc.cpp:76-77). CAND Method="" → -32601 "Method not found: " → HTTP 200 (server.go:168-177). DIVERGENT (code/message/status).
- "method" not a string: REF -32600 "Method must be a string" → 400 (server.cpp:448-449). CAND whole-body Unmarshal fails → -32700 "Parse error" → 200 (server.go:157). DIVERGENT.
- params not array/object: REF -32600 "Params must be an array or object" → 400 (server.cpp:459-464). CAND Unmarshal fails → -32700 → 200 (server.go:157). DIVERGENT.
- Top-level scalar/null/string: REF RPC_PARSE_ERROR "Top-level object parse error" → 500 (httprpc.cpp:200-201, JSONErrorReply default 500). CAND Unmarshal of null/scalar into the struct succeeds partially → handled as method "" → -32601 (server.go:157-177). DIVERGENT.
- CAND never emits RPC_INVALID_REQUEST (-32600) at all; its envelope codes are only -32700, -32601, -32603, -401 (server.go:122,135,150,161,173). REF uses -32600 for request-shape errors, -32700 for parse, -32601 for method, -1 for handler throws (server.cpp:438-464,584-587; protocol.h:50-95).

Response envelope:
- Field order result/error/id: REF JSONRPCReplyObj (protocol.cpp:40-50); CAND rpcResponse struct (server.go:28-32). MATCH.
- Envelope error object {code,message}: REF JSONRPCError (protocol.cpp:58-64); CAND envelopeError (server.go:36-39). Field order code,message both. MATCH.
- Success → "error":null: REF NullUniValue (protocol.cpp:46-47); CAND nil (server.go:187). MATCH.
- Envelope error → "result":null: REF (protocol.cpp:43-44); CAND nil (server.go:159-161). MATCH.
- Business errors ride in result, envelope error null, HTTP 200: REF makeError→uret→result (xutil.cpp:384-391, rpcxbridge.cpp:49-54, httprpc.cpp:192-196); CAND rpcError as Result (server.go:181-187). MATCH channel. The {error,code,name} object field order matches too (makeError emplace order xutil.cpp:386-389 vs rpcError struct order response.go:96-100). MATCH.
- id echo verbatim incl. null when absent: REF (server.cpp:442, protocol.cpp:48); CAND (server.go:187). MATCH.
- No "jsonrpc" version field either side (REF protocol.cpp:40-50; CAND server.go:24-32). MATCH.

JSON spacing / pretty-printing:
- REF: UniValue::write() compact — single characters ':'/',' with no spaces (univalue_write.cpp:90-112). Business-error results pass through `json_spirit::write_string(o, none, 8)` (rpcxbridge.cpp:51) then `uv.read()` then `write()` — the json_spirit string is re-parsed and re-emitted compact, so its pretty-width arg never appears on the wire (rpcxbridge.cpp:49-54). Final bytes: compact.
- CAND: encoding/json Encoder, compact, appends trailing '\n' (server.go:190-194). REF JSONRPCReply also appends "\n" (protocol.cpp:55) and JSONRPCExecBatch too (server.cpp:503). Trailing newline MATCH.
- Spacing MATCHES (both no-space compact). The only byte-level JSON divergence is **object key ordering** inside map-based results (dxSplitAddress/dxSplitInputs/dxGetUtxos/getnetworkinfo): REF preserves UniValue pushKV order, CAND sorts map keys. Envelope and rpcError objects have matching key order.

Error channels:
- REF thrown errors (param-count, UniValue type errors, help) → envelope error, HTTP 500 (or 400/404 per JSONErrorReply map httprpc.cpp:70-85). REF business makeError → result, HTTP 200 (httprpc.cpp:192-196).
- CAND maps every handler failure (incl. what REF throws) to a business-result error with HTTP 200 (server.go:181-187); only transport-level failures get envelope errors, and most of those also return HTTP 200 (server.go:159-177). So **HTTP status for envelope-error cases diverges** (REF 400/404/500; CAND mostly 200), even though envelope codes (-32700/-32601) align.

Auth model:
- REF: HTTP Basic always required — InitRPCAuthentication always sets credentials (cookie auth default, or rpcuser:rpcpassword, or HMAC-SHA256 rpcauth; httprpc.cpp:89-146,215-235). Missing/bad auth → 401 with `WWW-Authenticate: Basic realm="jsonrpc"` and **no body** (httprpc.cpp:26,156-161,173-176); brute-force mitigation 250ms sleep (httprpc.cpp:171).
- CAND: auth enforced only when BOTH user+pass are configured; otherwise accepts everything (server.go:62-68,78-81). On auth failure → 401 + WWW-Authenticate `Basic realm="jsonrpc"` **plus a JSON body** {result:null, error:{code:-401, message:"Authentication failed"}, id:null} (server.go:117-125). No anti-bruteforce sleep; no cookie or rpcauth support; custom envelope code -401 (REF never emits -401).
- DIVERGENT: REF always authenticates (cookie default) with empty 401 body; CAND default-open with a JSON 401 body; no cookie/rpcauth; no sleep.

Oversized-request handling:
- REF: libevent `evhttp_set_max_body_size(http, MAX_SIZE)` with MAX_SIZE = 0x02000000 = **32 MiB** (httpserver.cpp:395; serialize.h:27). Oversized → libevent's own HTTP error response (no JSON-RPC envelope).
- CAND: `rpcMaxBodyBytes = 4 MiB` via http.MaxBytesReader (server.go:111,129); oversized → HTTP **413** + JSON envelope {result:null, error:{code:-32700, message:"Parse error: request body too large"}, id:null} (server.go:129-139).
- DIVERGENT: limit 32 MiB vs 4 MiB; error body format and status differ.

Method-not-found handling:
- REF: -32601 "Method not found", HTTP **404** (server.cpp:566-568; httprpc.cpp:78-79,84).
- CAND: -32601 **"Method not found: <method>"** (method name appended), HTTP **200** (server.go:169-176).
- DIVERGENT (message text and HTTP status).

Summary: envelope shape/order, error-null conventions, compact spacing and trailing newline MATCH; divergences are (1) result-object key ordering in map-based handlers, (2) HTTP status codes for envelope errors (REF 400/404/500 vs CAND 200/401/413), (3) -32600 never emitted by CAND (missing-method/params-shape mapped to -32700/-32601), (4) batch & named-params unsupported in CAND, (5) auth default-open + JSON 401 body + no cookie/rpcauth/sleep, (6) body limit 4 MiB vs 32 MiB, (7) method-not-found message includes the method name.


