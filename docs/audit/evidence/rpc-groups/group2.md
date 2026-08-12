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
