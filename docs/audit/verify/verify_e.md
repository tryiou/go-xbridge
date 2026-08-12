# Verification report — candidate conformance findings RPC-F06–CFG-F90 (batch E)

Independent re-read of both call graphs (Blocknet Core C++ `blocknet_core/` vs
`go-xbridge/`). Each finding is marked CONFIRMED / REFUTED / PARTIAL with
precise `file:line` evidence on both sides and a corrected one-liner.

Cross-cut confirmations used below:

- C++ error text construction: `util/xbridgeerror.cpp:17-75`;
  `TRANSACTION_NOT_FOUND` → `"Transaction <arg> not found"` (line 31-32),
  `INVALID_PARAMETERS` → `"Invalid parameters: <arg>"` (line 43-44).
- Error codes (both sides agree): Go `errTxNotFound=1021`,
  `errInvalidParameters=1025` (`api/response.go:191,195`) match C++
  `Error` enum values used in `rpcxbridge.cpp`.
- C++ `uint256` parse: `uint256S` = `SetHex` (`uint256.h:132-147`,
  `uint256.cpp:27-53`); `ToString()` = `GetHex()` = zero-padded 64-hex
  (`uint256.cpp:21-24,62-65`).
- Live-wire proof: `rpc_parity.log` (workspace root) captures byte-level C++
  response bodies (e.g. lines 198, 458) — compact JSON, no spaces after `:` or
  `,`, consistent with `univalue_write.cpp`.

---

## RPC-dxGetOrder not-found message: `id.ToString()` vs raw input

VERDICT: **CONFIRMED** (with one correction: the error *code* is
`TRANSACTION_NOT_FOUND`=1021 on BOTH sides, not `INVALID_PARAMETERS`).

REF (C++): `rpcxbridge.cpp:778` `uint256 id = uint256S(params[0].get_str());`
then `:784-786` `if(order == nullptr) return
uret(xbridge::makeError(xbridge::TRANSACTION_NOT_FOUND, __FUNCTION__,
id.ToString()));`. `id.ToString()` renders the zero-padded 64-char `GetHex()`
(`uint256.cpp:62-65`). For input `"abc"`, `uint256S("abc")` is NOT null —
`SetHex` parses `"abc"` → `0x0000…0abc` (`uint256.cpp:40-52`), so C++ emits:
`Transaction 0000000000000000000000000000000000000000000000000000000000000abc not found`.
(Only a wholly non-hex string yields the null id; then the arg is 64 `0`s.)

CAN (Go): `api/handlers.go:136-168`; `:144` `orderIDKey(id)` rejects len≠64
(`api/response.go:504-517`), `:148` and `:158`
`makeError(errTxNotFound, "dxGetOrder", id)` where `id` is the lowercased **raw
input string** (`:143`). So Go emits `Transaction abc not found`.

Divergence is in the rendered id argument only: C++ zero-pads to 64 hex, Go
echoes the raw input. Error code (1021) and envelope shape match.

Corrected one-liner: dxGetOrder not-found: both sides use TRANSACTION_NOT_FOUND
(1021) with `"Transaction <id> not found"`, but C++ renders
`uint256S(...).ToString()` (zero-padded 64-hex, rpcxbridge.cpp:784-786) while Go
echoes the raw (lowercased) input (api/handlers.go:148,158).

---

## RPC-dxCancelOrder side-effect ordering

VERDICT: **CONFIRMED**.

REF (C++): `rpcxbridge.cpp:1297-1402`. Sequence: param-count check `:1347-1350`;
null-id check `:1354-1355`; lookup `:1359-1363`; state gate `:1365-1368`; **the
cancel happens at `:1370`** `cancelXBridgeTransaction(id, crRpcRequest)`; then
and only then `:1376-1384` the connector checks return NO_SESSION errors, and
`:1385-1401` builds the result. A missing connector therefore returns an RPC
error *after* the order has already been cancelled (side effect observable by a
dApp even though the RPC failed).

CAN (Go): `api/handlers.go:408-439` performs the connector checks at
`:428-433` BEFORE the actual cancel at `:434` `h.Node.CancelOrder(...)`; the
cancel side effect (`node.go:1706-1731`: `sendCancelTransaction`, store update,
persist) only runs after all validation has passed. A missing connector yields
NO_SESSION with no cancel side effect.

Corrected one-liner: C++ runs the connector NO_SESSION check AFTER
cancelXBridgeTransaction (rpcxbridge.cpp:1370 then :1376-1384) so a cancel can
be observable despite an RPC error; Go validates connectors first
(api/handlers.go:428-433) and only then cancels (node.go:1694+).

---

## RPC-64-hex order-id gate

VERDICT: **CONFIRMED**.

REF (C++): `uint256S`/`SetHex` behavior (`uint256.cpp:27-53`):
- leading whitespace skipped, optional `0x` skipped (`:31-37`);
- walks hex digits, then consumes nibbles from the END backwards (`:40-52`);
- 1–63 hex chars → left-zero-padded, non-null (e.g. `"a"` → `0x…0a`);
- ≥64 hex chars → only the least-significant 64 nibbles kept (truncation);
- wholly non-hex / empty → null (all-zero) id.

Per-handler use in C++: dxGetOrder `:778` (no null check); dxCancelOrder
`:1354-1357` (null check → `"Invalid order id [sid]"`); dxGetMyPartialOrderChain
`:2272-2274` (null check → `"bad order id"`); dxPartialOrderChainDetails
`:2412-2414` (null check → `"bad order id"`); dxGetLockedUtxos `:2634`
(no null check). A 63-hex string passes ALL of these C++ checks (never null) and
resolves to the left-zero-padded id.

CAN (Go): `parseOrderID` requires `len(s) == 64` (`api/response.go:506-508`)
and is reached via `orderIDKey` in all five methods: dxGetOrder
`api/handlers.go:144`, dxCancelOrder `:414`, dxGetMyPartialOrderChain `:877`,
dxPartialOrderChainDetails `:969`, dxGetLockedUtxos `:1086`. A 63-hex id is
rejected with an error on each (dxGetOrder → errTxNotFound `:148`, others →
errInvalidParameters `:416/:879/:971/:1088`).

A 63-char hex id therefore yields a valid (padded) lookup on C++ but an RPC
error on Go — the divergence the finding predicts. Also note a purely-hex-but-
odd-length string (`hex.DecodeString`) would fail in Go even if the length check
were relaxed.

Corrected one-liner: C++ uint256S left-pads 1–63 hex strings and truncates ≥64
(uint256.cpp:27-53) and only dxCancelOrder/dxGetMyPartialOrderChain/
dxPartialOrderChainDetails add an IsNull null-check; Go requires exactly 64 hex
chars in all five methods (api/response.go:506-508 via orderIDKey at
handlers.go:144,414,877,969,1086), so a 63-hex id errors on Go but resolves on
C++.

---

## RPC-dxGetMyPartialOrderChain chain membership logic

VERDICT: **CONFIRMED** — all three claimed filter/sort pieces are missing or
incomplete in Go (plus an additional walk-algorithm divergence).

REF (C++): helper `App::getPartialOrderChain` `xbridgeapp.cpp:3913-3991`:
- candidate filter `:3922` and `:3934`:
  `!t->isLocal() || (t->getParentOrder().IsNull() && !t->isPartialOrderAllowed())`
  — keeps only LOCAL orders that are either partial orders or children of
  partial orders (applied to BOTH `transactions()` `:3918-3927` and `history()`
  `:3929-3939`); a plain exact order (null parent, exact type) is excluded, so
  querying one yields `!rorder` → `{}` `:3941-3942`.
- utxo-count sort of candidates `:3944-3948`; chain = ancestors (full parent
  walk) + direct children only `:3953-3981`.
- **final sort by `created` ascending** `:3983-3988`.

CAN (Go): `api/handlers.go:900-961`:
- isLocal filter: applied ONLY to `Store.History()` (`:906-908`, `e.Order.Mine`);
  the live `Store.List()` loop (`:902-904`) has NO `Mine` filter (remote live
  orders can enter the chain) — C++ filters both sources.
- partial-parent filter (`null parent && !partial-allowed`) is ABSENT: a plain
  exact order resolves to a 1-element chain `[cur]` `:915-919`, whereas C++
  returns an empty chain for it.
- created-time sort is ABSENT: Go returns walk order (ancestors prepended
  `:928`, then recursive descendants `:940-959`), not `created` order.
- additionally Go walks the FULL descendant tree (`descend` recursion `:941-953`)
  while C++ only finds direct children (`rpcxbridge`-side walk
  `xbridgeapp.cpp:3960-3967`; `currentid` is never advanced to the child).

Corrected one-liner: C++ getPartialOrderChain filters to local partial/partial-
child orders in both live and history and sorts the final chain by `created`
(xbridgeapp.cpp:3922,3934,3985-3988); Go applies the Mine filter only to
history, skips the partial-parent filter and the created sort, and also walks
the full descendant tree instead of direct children (api/handlers.go:900-961).

---

## RPC-dxPartialOrderChainDetails

VERDICT: **CONFIRMED** for all three sub-claims (a), (b), (c).

(a) p2sh array length: REF `rpcxbridge.cpp:2456-2457`
`uvp2sh.push_back(t->binTxId); uvp2shcparty.push_back(t->oBinTxId);`
— unconditional push per order, empty string when the txid is missing (help
example even shows `""` at `:2365,2370`). CAN `api/handlers.go:1008-1013`
appends only when non-empty: `if t.BinTxId != "" { deposits = append(...) }`,
so array length < chain length whenever any deposit is missing.

(b) bad-id error text: REF dxPartialOrderChainDetails `:2412-2414`:
`makeError(INVALID_PARAMETERS, __FUNCTION__, "bad order id")` →
`"Invalid parameters: bad order id"`. CAN `api/handlers.go:971`:
`makeError(errInvalidParameters, "dxPartialOrderChainDetails",
"Invalid order id ["+id+"]")` → `"Invalid parameters: Invalid order id [<id>]"`.
Note the sibling handlers actually match C++: dxCancelOrder both use
`"Invalid order id [<id>]"` (C++ `:1355`, Go `:416`) and
dxGetMyPartialOrderChain both use `"bad order id"` (C++ `:2274`, Go `:879`);
only dxPartialOrderChainDetails (and dxGetLockedUtxos, where C++ has no such
check at all) diverge.

(c) map key order: CAN returns `map[string]interface{}`
(`api/handlers.go:1015`) and `encoding/json` sorts object keys
(`server.go:190-193`); REF returns `UniValue::VOBJ` which preserves insertion
order (`univalue_write.cpp:90-112`). So the JSON body field order differs
byte-level.

Corrected one-liner: (a) C++ emits one p2sh_deposits / _counterparty entry per
chain order including empty strings (rpcxbridge.cpp:2456-2457) while Go drops
empties (handlers.go:1008-1013); (b) dxPartialOrderChainDetails bad-id text is
`"Invalid parameters: bad order id"` (C++ :2414) vs `"Invalid parameters:
Invalid order id [<id>]"` (Go :971); (c) Go's map returns alphabetized keys
vs C++ insertion order.

---

## RPC-F47/RPC-F48/RPC-F49/RPC-F50/RPC-F51/RPC-F52 JSON-RPC transport

VERDICT: (a) **CONFIRMED**, (b) **CONFIRMED** (exact strings corrected),
(c) **CONFIRMED**, (d) **CONFIRMED**, (e) **CONFIRMED**, (f) **REFUTED**.

(a) HTTP status for parse error / method-not-found.
REF `httprpc.cpp:70-85` `JSONErrorReply`: default `HTTP_INTERNAL_SERVER_ERROR`
(500); `code == RPC_INVALID_REQUEST` → 400; `code == RPC_METHOD_NOT_FOUND` →
`HTTP_NOT_FOUND` (404). Parse error (`RPC_PARSE_ERROR` -32700) is thrown at
`httprpc.cpp:181-182` / `:201`, so single-request parse errors get **HTTP 500**
with a `{"result":null,"error":{...},"id":null}` body; unknown method is thrown
at `server.cpp:566-568` → **HTTP 404**.
CAN `server.go:157-165` (parse → HTTP 200, -32700 envelope) and `:168-177`
(method-not-found → HTTP 200, -32601 envelope). Both divergences hold.

(b) method-not-found envelope. REF `server.cpp:568`
`throw JSONRPCError(RPC_METHOD_NOT_FOUND, "Method not found");` — C++ does NOT
append the method name. CAN `server.go:173`
`Message: fmt.Sprintf("Method not found: %s", req.Method)` — Go DOES append it.
Exact strings: C++ `"Method not found"`, Go `"Method not found: <method>"`
(the finding's quoted Go string `"Method not found"` was itself slightly off).

(c) request body limit. REF `httpserver.cpp:395`
`evhttp_set_max_body_size(http, MAX_SIZE);` with `MAX_SIZE = 0x02000000`
(`serialize.h:27`) = 32 MiB. CAN `server.go:111` `rpcMaxBodyBytes = 4 << 20`
= 4 MiB (oversize → HTTP 413 + -32700 envelope, `server.go:129-139`). Both
numbers confirmed; 32 MiB vs 4 MiB.

(d) auth. REF: auth is ALWAYS required when the HTTP RPC server runs —
`InitRPCAuthentication` (`httprpc.cpp:215-235`) falls back to a random cookie
(`GenerateAuthCookie`, `protocol.cpp:83-113`) when `-rpcpassword` is empty;
`RPCAuthorized` rejects when `strRPCUserColonPass` is empty (`httprpc.cpp:130-
131`); missing header → 401 + WWW-Authenticate (`:156-161`); bad credentials →
`MilliSleep(250)` (250 ms) then 401 (`:165-176`); cookie auth also supported.
CAN: `SetAuth` enables auth only when BOTH user and pass are set
(`server.go:62-68`) and `authorized` returns true when `s.user == ""`
(`server.go:78-81`) → open by default (loopback-only by default in
`cmd/xbridged/main.go:91,218-225`); failed auth returns 401 immediately with no
250 ms delay (`server.go:117-126`). C++ always-auth (cookie fallback) + 250 ms
sleep vs Go default-open and no sleep — confirmed.

(e) batch / named params / -32600. REF: batch supported — array request handled
by `JSONRPCExecBatch` (`httprpc.cpp:197-199`, `server.cpp:497-504`); named-
parameter objects supported — `params` may be an object (`server.cpp:458-460`)
and `CRPCTable::execute` runs `transformNamedArguments` (`server.cpp:578-579`,
`:510-554`); the `RPC_INVALID_REQUEST` (-32600) path exists
(`server.cpp:437-438,446-449,464`).
CAN: `rpcRequest` is a single struct with `Params []json.RawMessage`
(`server.go:18-22`); a JSON array body or object `params` fails `json.Unmarshal`
and is reported as -32700 `Parse error` (`server.go:156-165`); there is no
-32600 branch anywhere in `server.go`. Confirmed: Go cannot do batch or named
params and has no -32600 path.

(f) JSON spacing. REF `univalue_write.cpp:90-112`: when `prettyIndent==0` (the
default, `univalue.h:146-147`) the object writer emits `"key":value` and `,`
with NO spaces (`if (prettyIndent) s += " "` at `:100-101`); `JSONRPCReply`
appends exactly `"\n"` (`protocol.cpp:52-56`). Live C++ bodies in
`rpc_parity.log` (e.g. line 198 `{"result":{"detail":3,...},"error":null,
"id":null}`) confirm compact, no-spaces output. CAN `encoding/json` is also
compact, and `json.Encoder.Encode` appends a trailing `\n` (`server.go:190-193`).
So C++ does NOT emit single spaces after `:`/`,` — both sides are compact and
newline-terminated; the claimed spacing divergence does not exist.

Corrected one-liner: RPC-F47 — (a) C++ returns HTTP 500 for single-request parse
errors and HTTP 404 for unknown methods (httprpc.cpp:73-79) vs Go's HTTP 200
for both (server.go:157-177); (b) C++ message is plain `"Method not found"`
(server.cpp:568) vs Go `"Method not found: <method>"` (server.go:173); (c)
C++ MAX_SIZE=32 MiB (serialize.h:27, httpserver.cpp:395) vs Go 4 MiB
(server.go:111); (d) C++ always requires auth (cookie fallback,
httprpc.cpp:215-235) with a 250 ms sleep on bad credentials (httprpc.cpp:171)
vs Go open-by-default when either credential is empty (server.go:62-68,78-81)
with no sleep; (e) C++ supports batch and named-params and a -32600 path
(httprpc.cpp:198-199, server.cpp:458-460,578-579) vs Go which maps both to
-32700 and has no -32600 path (server.go:156-165); (f) REFUTED — C++
UniValue::write is compact with NO spaces (univalue_write.cpp:99-104) matching
Go, so there is no spacing divergence (both newline-terminated).

---

## CFG-`-dxnowallets` CLI flag

VERDICT: **CONFIRMED**.

REF (C++): `init.cpp:572`
`gArgs.AddArg("-dxnowallets", strprintf("Show all orders across the network for
non-local wallets"), false, OptionsCategory::XBRIDGE);` — declared command-line
arg (alongside `-enableexchange` `init.cpp:570`). Read as a CLI override at
`rpcxbridge.cpp:432`
`gArgs.GetBoolArg("-dxnowallets", settings().showAllOrders())` (also
`xbridgesession.cpp:744`, `xbridgeapp.cpp:372`); `settings().showAllOrders()` is
the `Main.ShowAllOrders` conf default (`util/settings.h:38`).

CAN (Go): `ShowAllOrders` is a programmatic `api.Config` field
(`api/node.go:78-81`), populated only from `conf.Main.ShowAllOrders`
(`api/node.go:320`, i.e. xbridge.conf's `Main` section). `cmd/xbridged/main.go`
declares flags `-rpcbind -rpcuser -rpcpassword -network -node -addnode -conf
-magic -walletversion -walletversionstr -loglevel -datadir -logfile`
(`:91-106`) and there is NO `-dxnowallets` flag.

Corrected one-liner: C++ declares `-dxnowallets` in init.cpp:572 and reads it as
a CLI override in rpcxbridge.cpp:432 / xbridgesession.cpp:744 /
xbridgeapp.cpp:372; go-xbridge exposes ShowAllOrders only via api.Config
populated from xbridge.conf Main.ShowAllOrders (api/node.go:78-81,320) with no
CLI flag in cmd/xbridged/main.go.

---

## Findings whose claims were substantially corrected

- RPC-F06 — the error *code* is TRANSACTION_NOT_FOUND (1021) on both sides, not
  INVALID_PARAMETERS; the divergence is purely the rendered id argument.
- RPC-F28(b) — only dxPartialOrderChainDetails (and dxGetLockedUtxos, where C++
  has no such check) show the `"Invalid order id [<id>]"` vs `"bad order id"`
  mismatch; dxCancelOrder and dxGetMyPartialOrderChain actually match C++
  verbatim.
- RPC-F47(b) — Go's actual method-not-found message is `"Method not found: <method>"`
  (appends the name); C++ is plain `"Method not found"`.
- RPC-F47(f) — REFUTED entirely: C++ UniValue::write is compact (no spaces) with
  default prettyIndent=0, so there is no JSON-spacing divergence.
