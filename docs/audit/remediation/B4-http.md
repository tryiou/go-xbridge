# B4 — HTTP/RPC hardening (RPC-F01, RPC-F02, RPC-F47..F52, RPC-F58)

Branch: `fix/http-hardening` (off `main` @ B5 merge).
C++ reference: Blocknet Core @ `ac930b7f8` (v4.4.1 era).
Go subject: `api/server.go`, `api/dispatch.go`, `api/response.go`,
`api/handlers.go`, `cmd/xbridged/main.go`, `conformance/` (11 OPEN findings).

## Findings resolved on this branch

| ID | Title | C++ source of truth | Go call sites → fix |
|---|---|---|---|
| F47 | HTTP status for parse/method-not-found (500/404 vs 200) | `httprpc.cpp:70-85` `JSONErrorReply`: parse `-32700`→500; `-32600`→400; `-32601`→404; else (incl. `-1`,`-8`,`-32603`)→500 | `api/server.go:130-187` all error paths write 200 → route via `httpStatusForCode` |
| F48 | method-not-found message appends the name | `server.cpp:568` `"Method not found"` | `api/server.go:173` `fmt.Sprintf("Method not found: %s", …)` → bare message |
| F49 | body cap 4 MiB vs 32 MiB | `httpserver.cpp:395` `evhttp_set_max_body_size(http, MAX_SIZE)`; `MAX_SIZE = 0x02000000` (`serialize.h:27`) | `api/server.go:111` `rpcMaxBodyBytes = 4<<20` → `32<<20`; oversize → non-envelope 413 (libevent) |
| F50 | auth model: open-by-default vs always-auth | `httprpc.cpp:128-174` always-auth; `InitRPCAuthentication` `:215-235` (cookie default); `rpcauth` multi-user HMAC `:89-126`; `MilliSleep(250)` on failure `:171`; empty-body 401 + `WWW-Authenticate: Basic realm="jsonrpc"` | `api/server.go:62-89,117-126` (open when no creds; 401 JSON `-401` body) |
| F51 | batch / named params / `-32600` unsupported | `server.cpp:434-465` request parse (`-32600` cases); `:497-504` `JSONRPCExecBatch`; `:510-553,578-582` `transformNamedArguments` (dx* empty argNames ⇒ `-8 "Unknown named parameter <k>"`) | `api/server.go:156-177` struct-only Unmarshal |
| F52 | extra params accepted where C++ errors | per-method arity table below (business 1025 vs thrown-help) | `api/handlers.go` per-method gates → central registry `api/dispatch.go` |
| F58 | `http.Server` no timeouts | `httpserver.cpp:393` `evhttp_set_timeout(http, -rpcservertimeout)`; default `DEFAULT_HTTP_SERVER_TIMEOUT=30` (`httpserver.h:14`) | `cmd/xbridged/main.go:238` add Read/Write=30s + `-rpcservertimeout`, ReadHeader 10s, Idle 60s |
| F01 | error-channel: strict param types | every `get_str()/get_int()/get_int64()/get_bool()` throws json_spirit `runtime_error` on wrong/null type → envelope `-1` (`server.cpp:584-586`) | `api/dispatch.go:75-149` coercing parsers → strict envelope `-1` with json_spirit message |
| F02 | `NO_SESSION` `name` = `"dx"` not `__FUNCTION__` | every handler passes `__FUNCTION__` (e.g. `rpcxbridge.cpp:790-795`) | `api/handlers.go:198-207` `connector(ticker)` → `connector(ticker, method)`; 13 call sites |

## Contract correction (verified this session)

RPC-F01's card ("C++ throws envelope errors for param-count") is imprecise for
the current daemon. The actual split:

- **Param-COUNT** on the *old-style* methods → **business 1025 in `result`**
  (`uret(makeError(INVALID_PARAMETERS, __FUNCTION__, <param-list>))`).
- **Param-COUNT** on the *throw* methods → **envelope `-1`** with the full
  `RPCHelpMan::ToString()` help text (`throw std::runtime_error(...)`,
  wrapped at `server.cpp:584-586`).
- **Param-TYPE** errors (wrong JSON type / explicit `null`) → **envelope `-1`**
  everywhere, message `"get_value< <T> > called on <V> Value"` with
  `<T>/<V>` ∈ {`boolean`, `string`, `integer`, `real`, `null`, `Array`, `Object`}
  (json_spirit_value.h:383-393, 586-604).

### Arity table (F52) — per method, exact C++ behavior

Business 1025 (HTTP 200, result-error, exact message):

| Method | Gate | Message (makeError arg) |
|---|---|---|
| dxGetNewTokenAddress | !=1 | `(ticker)` |
| dxLoadXBridgeConf | >0 | `This function does not accept any parameter.` |
| dxGetLocalTokens | >0 | `This function does not accept any parameter.` |
| dxGetNetworkTokens | >0 | `This function does not accept any parameters.` |
| dxGetOrders | >0 | `This function does not accept any parameters.` |
| dxGetOrderFills | not 2/3 | `(maker) (taker) (combined, default=true)[optional]` |
| dxGetOrderHistory | not 5..8 | `(maker) (taker) (start time) (end time) (granularity) (order_ids, default=false)[optional] (with_inverse, default=false)[optional] (limit, default=2147483647)[optional]` |
| dxGetOrder | !=1 | `(id)` |
| dxCancelOrder | !=1 | `(id)` |
| dxGetOrderBook | not 3/4 | `(detail, 1-4) (maker) (taker) (max_orders, default=50)[optional]` |
| dxGetTokenBalances | !=0 | `This function does not accept any parameters.` |
| dxGetLockedUtxos | >1 | `Too many parameters.` |
| dxFlushCancelledOrders | >1 | `ageMillis must be an integer >= 0` |

Envelope -1 (HTTP 500, thrown help text) — 9 of the 10 C++ methods exist in the
Go dispatch (gettradingdata is B7/F37):

| Method | Gate | Message = RPCHelpMan::ToString() |
|---|---|---|
| dxMakeOrder | <7 | help constant (see `api/help_texts.go`) |
| dxMakePartialOrder | <6 | help constant |
| dxTakeOrder | not 3..5 | help constant |
| dxGetMyPartialOrderChain | empty or >1 | help constant |
| dxPartialOrderChainDetails | empty or >1 | help constant |
| dxSplitAddress | not 3..6 | help constant |
| dxSplitInputs | not 3..7 | help constant |
| dxGetUtxos | not 1/2 | help constant |
| dxGetTradingData | >2 | help constant |

**dxGetMyOrders has NO arity gate in C++** (only `fHelp` throws) — extra params
silently ignored; F52's mention is incorrect, we match C++ (no gate).

Go arity bugs fixed by the registry: dxMakePartialOrder `<7`→`<6`;
dxSplitInputs `!=7`→`3..7`; dxGetOrderFills no gate→`2|3`;
dxGetOrderHistory message missing `default=2147483647`.

## Design notes / decisions

- **Error channel** — `rpcError` gains an unexported `Envelope bool`. The server
  routes envelope errors to `{result:null,error:{code,message}}` + mapped HTTP
  status; business errors stay the `result` object, HTTP 200. Handlers keep the
  `(interface{}, *rpcError)` signature.
- **Tolerant request parse (id-echo)** — decode the body into
  `{ID,Method,Params json.RawMessage}` first, then validate field-by-field
  mirroring `JSONRPCRequest::parse` so `id` survives every pre-dispatch error
  (fixes the conformance `envelope/id-echo` row). Top-level array ⇒ batch;
  top-level scalar/string ⇒ `-32700 "Top-level object parse error"`.
- **Named params** — every dx* registers empty argNames
  (rpcxbridge.cpp:3498-3526) ⇒ any key ⇒ envelope `-8
  "Unknown named parameter <firstkey>"` HTTP 500; empty `{}` ⇒ positional `[]`.
- **Strict parsers (F01)** — `mustStr/mustInt/mustInt64/mustBool` reject
  present-null and wrong JSON types with the exact json_spirit message; `absent`
  keeps the default; C++ `isNull()`-guarded optionals get null-tolerant
  variants. Removes numeric-string and `"true"/"false"` coercions.
- **Always-auth (F50, decision)** — auth enforced whenever ANY credential is
  configured (`-rpcuser`+`-rpcpassword` **or** `-rpcauth user:salt$hash`
  repeated). Missing header ⇒ immediate 401 empty body; bad creds ⇒ log +
  250 ms sleep + 401 empty body; `WWW-Authenticate: Basic realm="jsonrpc"`.
  **No cookie** (documented divergence from C++ auto-cookie): no creds ⇒
  loopback-default-open + non-loopback warning. Drop the custom `-401`.
  `-rpcauth` is a CLI flag here; conf-key parsing stays B10.
- **POST-only** — non-POST ⇒ 405 `"JSONRPC server handles only POST requests"`
  (httprpc.cpp:151-154). No content-type/host checks (C++ has none).
- **Help-text constants** — transcribed byte-for-byte from each `RPCHelpMan`
  `ToString()` using the 0.18 rendering rules (`util.cpp:341-389`: oneline arg
  list with optional `( )` grouping; `\nArguments:\n` table with column pad =
  longest argname + 4; `\nResult:\n`; `\nExamples:\n`). If a C++ build is
  available later, cross-check via a `tools/parity`-style oracle (not required
  to land).
- **NO_SESSION name (F02)** — `connector(ticker, method)`; 13 call sites in
  `dxGetOrders`, `dxGetOrder`, `dxGetNewTokenAddress`, `dxCancelOrder`,
  `dxGetLockedUtxos`, `dxGetUtxos`, and `splitTx` (which takes its caller's
  name `dxSplitAddress`/`dxSplitInputs`).
- **Timeouts (F58)** — `http.Server{ReadTimeout,WriteTimeout: -rpcservertimeout
  (default 30), ReadHeaderTimeout: 10s, IdleTimeout: 60s}`. 30 s mirrors C++
  `DEFAULT_HTTP_SERVER_TIMEOUT`; ReadHeader/Idle are Go-only hardening.

## Tests

- `api/server_test.go` — 405 POST-only; parse→500; `-32600` cases + id echo;
  method-not-found 404 + bare message; batch (incl. empty `[]`); named→`-8`;
  32 MiB cap (4 MiB ok, >32 MiB ⇒ 413 non-envelope); envelope status routing;
  panic→500; auth rewrite (empty-body 401, `WWW-Authenticate`, 250 ms delay via
  a settable `milliSleep`, rpcauth multi-user HMAC, no-creds open).
- `api/dispatch_test.go` — strict-parser envelope matrix (wrong type / null per
  parser, exact json_spirit messages); null-tolerant optionals.
- `api/arity_test.go` — per-method arity: business exact 1025 text; throw
  methods envelope `-1` with help constant (byte equality); boundaries; the
  three Go arity-bug fixes; dxGetMyOrders extras ignored.
- `api/handlers_test.go` — NO_SESSION `name` per affected handler.
- `conformance/conformance_suite_test.go` — promote the 6 DIVERGENT
  `envelope/…` transport rows to strict.

## Verify

`gofmt` → `go build` / `go vet` / `go test ./...` → `go test -race ./api/...` →
from `tools/`: `make parity` + `make canary`. Register + remediation-plan
closeout on this branch, then merge approval.
