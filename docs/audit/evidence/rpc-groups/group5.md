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
