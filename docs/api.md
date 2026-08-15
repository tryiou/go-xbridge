# XBridge JSON-RPC API

`xbridged` serves the Blocknet **`dx*`** XBridge API over HTTP. Point a dapp's
RPC URL at the daemon's `-rpcbind` address (default `127.0.0.1:41414`) and the
API works as a drop-in replacement for blocknetd's XBridge RPC.

All 24 methods are documented below: 23 `dx*` commands plus `getnetworkinfo`.
The C++ `gettradingdata` command is not exposed — only `dxGetTradingData` is.
This is a deliberate removal (RPC-F37): the lowercase command is a
blocknetd-internal registration (`rpcxbridge.cpp:3520`) with a different schema
and a duplicated `to` key; `xbridged` is a thin client and only surfaces the
`dxGetTradingData` variant. Calling `gettradingdata` against `xbridged` returns
the envelope error `-32601 Method not found`.

## Calling convention

JSON-RPC 1.0 over HTTP (`POST`). The request has **positional** parameters:

```sh
curl -s http://127.0.0.1:41414 \
  -H 'content-type: application/json' \
  -d '{"method":"dxGetOrderBook","params":[1,"BTC","BLOCK"],"id":1}'
```

- Request: `{"method":"<name>","params":[<pos0>,<pos1>,…],"id":<any>}`.
- Response: `{"result":…,"error":null,"id":<echo>}` (no `jsonrpc` field).
- **Auth:** HTTP Basic auth is enforced whenever **any** credential is
  configured — `-rpcuser`/`-rpcpassword` or one or more `-rpcauth`
  `user:salt$hash` entries (HMAC-SHA256). A request without valid credentials
  answers `401` with an **empty body** and `WWW-Authenticate:
  Basic realm="jsonrpc"` (no JSON envelope). A presented bad credential waits
  250 ms before the 401 (C++ brute-force deterrence `MilliSleep(250)`,
  `httprpc.cpp:168-171`; a missing header fails immediately). With **no**
  credentials configured the daemon is default-open on its loopback bind — a
  documented divergence (Go never auto-creates a cookie file; C++ always
  authenticates via `~/.cookie`). See [Transport](#transport).

## Transport

The HTTP transport matches the C++ `httprpc.cpp` conventions (audit branch B4
`fix/http-hardening`; details in
[`audit/remediation/B4-http.md`](audit/remediation/B4-http.md)).

- **POST only.** `GET` and other verbs answer `405 Method Not Allowed` (C++ is
  POST-only).
- **HTTP status routing** (C++ `JSONErrorReply`, `httprpc.cpp:70-85`): envelope
  `-32600` → `400`, `-32601` → `404`, all other envelope errors → `500`.
  Business errors ride in `result` and always return `200`.
- **Envelope error codes:** `-32700` parse error, `-32600` invalid request
  (`"Missing method"`, `"Method must be a string"`, `"Params must be an array
  or object"`), `-32601` `"Method not found"` (bare message — no method-name
  suffix), `-32603` internal, `-8` `"Unknown named parameter <key>"`.
- **Request id** is parsed before dispatch and echoed on the pre-dispatch
  errors where it is parseable (`-32600`, `-32601`, `-8`); an unparseable body
  yields a null id on `-32700` (as in C++).
- **Body cap:** 32 MiB (`rpcMaxBodyBytes`, matching C++ libevent). An oversized
  request answers a non-envelope `413` (empty body).
- **Batch requests** are supported: a top-level JSON array runs each element as
  its own RPC call (C++ `JSONRPCExecBatch`); each element's `id` is echoed
  independently and the response is an array. The overall HTTP status is `200`.
- **Named parameters** are rejected: the first named key produces `-8`
  `"Unknown named parameter <key>"` (C++ `transformNamedArguments`; `dx*`
  methods have empty argNames, so every named param is unknown).
- **Strict parameter typing.** No string coercion: `dx*` parameters must match
  the C++ types (`json_spirit`/UniValue parse and `RPCTypeCheck`). Type errors
  surface as envelope `-1` (`get_value< T > called on V Value` / `JSON value is
  not a X as expected`) on the throw methods and `-3` `Expected type t, got n`
  on the RPCTypeCheck-checked methods (e.g. `dxGetMyPartialOrderChain`,
  `dxPartialOrderChainDetails`, `dxGetTradingData`).
- **Extra positional parameters** are rejected exactly as C++ arity checks do:
  business methods answer `1025` `Invalid parameters: <arg>` in `result`; the
  throw methods answer an envelope `-1` with the method's `RPCHelpMan` help
  text.
- **Timeouts:** the HTTP server applies read/write/header/idle timeouts
  (`-rpcservertimeout`, default 30 s, mirroring C++ `DEFAULT_HTTP_SERVER_TIMEOUT`).

## Response conventions

- **Amounts** are fixed **6-decimal strings** (XBridge scale `TransactionDescr::COIN` = 1e6),
  truncating: `"1.500000"`. (Exceptions: `dxGetUtxos` amounts are fixed to the
  coin's `COIN` decimals — `"1.00000000"` for BTC (RPC-F44); `dxGetLockedUtxos`
  uses C++ default-double rendering (RPC-F31); `dxGetTokenBalances` renders
  fixed-6 — see [audit/register.md](audit/register.md).)
- **Timestamps** are ISO-8601 strings with millisecond precision:
  `"2026-08-07T12:00:00.000Z"`.
- **Order ids** are 64-hex strings in display order.

### Order object

The shared order-object fields (emitted by `dxGetOrders`, `dxGetOrder`,
`dxGetMyOrders`, `dxGetMyPartialOrderChain`, `dxMakeOrder`, `dxMakePartialOrder`,
`dxTakeOrder`):

| Field | Type | Meaning |
|-------|------|---------|
| `id` | string | 64-hex order id |
| `maker` | string | Ticker of the maker's (from) currency |
| `maker_size` | string | Maker-side amount, 6-decimal |
| `taker` | string | Ticker of the taker's (to) currency |
| `taker_size` | string | Taker-side amount, 6-decimal |
| `updated_at` | string | ISO-8601 last update |
| `created_at` | string | ISO-8601 creation time |
| `order_type` | string | `"exact"` or `"partial"` |
| `partial_minimum` | string | Minimum partial amount (6-decimal) |
| `partial_orig_maker_size` | string | Original maker size before partial splits |
| `partial_orig_taker_size` | string | Original taker size before partial splits |
| `partial_repost` | bool | Whether the partial order reposts the remainder |
| `partial_parent_id` | string | 64-hex parent order id (zero for a root order) |
| `status` | string | Lifecycle status (below) |

Command-specific additions:

- `dxGetMyOrders`, `dxGetMyPartialOrderChain` add `maker_address` and
  `taker_address`.
- `dxMakeOrder`, `dxMakePartialOrder` add `maker_address`, `taker_address` and
  `block_id`; status is `"created"`.
- `dxTakeOrder` returns the order with `maker`/`taker` swapped (the taker's view
  of the trade).
- `dxCancelOrder` returns a **distinct shape** with no `partial_*`/`order_type`
  fields: `id`, `maker`, `maker_size`, `maker_address`, `taker`, `taker_size`,
  `taker_address`, `refund_tx`, `updated_at`, `created_at`, `status`.

**Status strings:** `open`, `accepting`, `hold`, `initialized`, `created`,
`signed`, `commited`, `finished`, `canceled`, `expired`, `dropped`, `new`,
`offline`, `rolled back`, `rollback failed`, `invalid`.

## Errors

**Business errors** are returned as the **`result` object** — the JSON-RPC
envelope `error` stays `null`:

```json
{"result":{"error":"Transaction <id> not found","code":1021,"name":"dxGetOrder"},"error":null,"id":1}
```

**Transport failures** (parse error, invalid request, unknown method, internal
error) use the envelope `error`: `-32700` parse, `-32600` invalid request,
`-32601` method not found, `-32603` internal — with HTTP status `400`/`404`/
`500` respectively. Auth failure is an empty-body `401` (no envelope). An
oversized body is a non-envelope `413`. See [Transport](#transport).

### Error codes

| Code | `error` text |
|------|--------------|
| 1001 | `Unauthorized <arg>` |
| 1002 | `Internal Server Error` |
| 1004 | `Bad Request <arg>` |
| 1011 | `Invalid maker symbol <arg>` |
| 1012 | `Invalid taker symbol <arg>` |
| 1015 | `Invalid detail level, possible values: 1 - 3` |
| 1016 | `Invalid time format, ISO 8601 date format required` |
| 1017 | `Invalid coin <arg>` |
| 1018 | `No session for currency <arg>` |
| 1019 | `Insufficient funds for <arg>` |
| 1020 | `Funds not signed for <arg>` |
| 1021 | `Transaction <arg> not found` |
| 1022 | `Unknown session for <arg>` |
| 1023 | `Revert tx failed for <arg>` |
| 1024 | `Invalid amount <arg>` |
| 1025 | `Invalid parameters: <arg>` |
| 1026 | `Bad address <arg>` |
| 1027 | `Invalid signature <arg>` |
| 1028 | `invalid transaction state <arg>` |
| 1029 | `Blocknet is not running as an exchange node` |
| 1030 | `Amount is dust (very small)` |
| 1031 | `Blocknet wallet amount is too small to cover the fee payment` |
| 1032 | `Could not find a service node with required services: <arg>` |
| 1033 | `The order information could not be written to the blockchain` |
| 1034 | `Partial orders not allowed for this transaction` |

## Methods

Params are the positional `params` array elements; `[optional]` marks a
defaulted or trailing parameter.

### Order book & history

**`dxGetOrders`**
- `params`: none.
- `result`: array of order objects (open orders; `canceled`/`finished`/`expired`
  orders younger than 60 s are also shown). Orders for currencies with no
  configured wallet connector are hidden unless `ShowAllOrders` is set.

**`dxGetOrder`**
- `params`: `id` (string).
- `result`: order object.
- `errors`: 1025 (missing id), 1021 (unknown id), 1018 (no wallet for either
  currency).

**`dxGetOrderBook`**
- `params`: `detail` (int 1–4), `maker` (string), `taker` (string),
  `max_orders` (int, default `50`).
- `result`: `{"detail":…,"maker":…,"taker":…,"asks":[…],"bids":[…]}`. Each side
  entry is a row whose shape depends on `detail`:
  - `1` — best bid/ask only: `[price, amount, count_at_price]`.
  - `2` — aggregated levels, capped at `max_orders`: `[price, amount, count]`.
  - `3` — full per-side list capped at `max_orders`, with ids:
    `[price, amount, order_id]`.
  - `4` — best bid/ask only: `[price, amount, [order_ids_at_price]]`.
- `errors`: 1015 (detail outside 1–4), 1025 (missing/other params).

**`dxGetOrderFills`**
- `params`: `maker` (string), `taker` (string),
  `combined` (bool, default `true` — also match the reversed pair).
- `result`: array of fill objects: `id`, `time`, `maker`, `maker_size`,
  `taker`, `taker_size`, `order_type`, `partial_minimum`,
  `partial_orig_maker_size`, `partial_orig_taker_size`, `partial_repost`,
  `partial_parent_id`.
- `errors`: 1025 (missing `maker`/`taker`).

**`dxGetOrderHistory`**
- `params`: `maker`, `taker` (strings), `start`, `end`, `granularity` (unix
  seconds), `order_ids` (bool, default `false`), `with_inverse` (bool, default
  `false`), `limit` (int) [optional — the max number of returned buckets; the
  window is shifted to the most-recent tail. Without an explicit `limit` the
  bucket count is hard-capped at 100k].
- `result`: array of OHLCV rows `[time, low, high, open, close, volume]`
  (plus an order-ids array when `order_ids`). The thin client aggregates from
  session-local fills only — see Tier 3.
- `errors`: 1025 (missing/early/invalid params; `start` before 2018-02-25 is
  rejected as `Start time too early.`).

### Trading

**`dxMakeOrder`**
- `params`: `maker` (ticker), `maker_size` (string), `maker_address` (string),
  `taker` (ticker), `taker_size` (string), `taker_address` (string), `type`
  (must be `"exact"`), `use_all_funds` (bool, default `true`) [optional],
  `dryrun` (literal `"dryrun"`) [optional — only when exactly 9 params].
- `result`: order object (status `"created"`, `order_type` `"exact"`, plus
  `maker_address`, `taker_address`, `block_id`).
- `errors`: 1025 (missing params, non-`exact` type, invalid `use_all_funds`,
  misspelled/extra `dryrun`).

**`dxMakePartialOrder`**
- `params`: `maker`, `maker_size`, `maker_address`, `taker`, `taker_size`,
  `taker_address`, `minimum_size` (string), `repost` (bool, default `true`),
  `use_all_funds` (bool, default `true`), `auto_split` (bool, default `true`),
  `dryrun` (literal) [optional — only when exactly 11 params].
- `result`: order object (status `"created"`, `order_type` `"partial"`).
- `errors`: 1025 (same rules as `dxMakeOrder`).

**`dxTakeOrder`**
- `params`: `id` (string), `from_address` (string), `to_address` (string),
  `amount` (string) [optional — omitted = full take, present = partial take],
  `dryrun` (literal) [optional — only when exactly 5 params].
- `result`: order object with `maker`/`taker` swapped to the taker's view. A
  dryrun returns the pre-swap view with a zero `id` and status `"filled"`.
- `errors`: 1025 (missing params, misspelled/extra `dryrun`); `NO_SERVICE_NODE`
  (1032) when the order's hub is not a known servicenode in the local registry.

**`dxCancelOrder`**
- `params`: `id` (string).
- `result`: cancel-order object (see "Order object" above).
- `errors`: 1025 (malformed id), 1021 (unknown id), 1028 (order already
  `created` or later), 1018 (no wallet for either currency).

**`dxGetMyOrders`**
- `params`: none.
- `result`: array of order objects with `maker_address`/`taker_address`, sorted
  by update time.

**`dxGetMyPartialOrderChain`**
- `params`: `order_id` (string).
- `result`: array of order objects (the queried order plus its ancestors and
  descendants), empty `[]` for an unknown chain.
- `errors`: 1025 (malformed id).

**`dxPartialOrderChainDetails`**
- `params`: `order_id` (string).
- `result`: aggregate object (empty `{}` for an unknown chain):
  `first_order_id`, `maker`, `maker_address`, `taker`, `taker_address`,
  `partial_minimum`, `partial_orig_maker_size`, `partial_orig_taker_size`,
  `first_order_time`, `last_order_time`, `total_reported_sent`,
  `total_reported_received`, `total_reported_notsent`,
  `total_reported_notreceived`, `total_orders_open`, `total_orders_finished`,
  `total_orders_canceled`, `orders` (array), `p2sh_deposits` (array),
  `p2sh_deposits_counterparty` (array).
- `errors`: 1025 (malformed id).

**`dxFlushCancelledOrders`**
- `params`: `ageMillis` (int, default `0`) [optional].
- `result`: `{"ageMillis":…,"now":…,"durationMicrosec":…,"flushedOrders":[…]}`
  where each flushed order is `{"id":…,"txtime":…,"use_count":…}`.
- `errors`: 1025 (`ageMillis < 0` or more than 1 param).

### Tokens, wallet & UTXOs

**`dxGetLocalTokens`**
- `params`: none.
- `result`: sorted array of tickers from `ExchangeWallets` in `xbridge.conf`.

**`dxGetNetworkTokens`**
- `params`: none.
- `result`: sorted array of tickers advertised by the connected servicenodes
  (fallback to the conf `NetworkTokens` list). P2P-bounded — see Tier 3.

**`dxGetTokenBalances`**
- `params`: none.
- `result`: object mapping each configured exchange-wallet ticker to its
  spendable balance (6-decimal string, locked UTXOs subtracted). No `"Wallet"`
  key is synthesized (documented divergence RPC-F23/F24): the BLOCK fee
  balance is exposed under the `BLOCK` ticker, and C++'s key order is
  race-dependent thread-completion order anyway. Empty object when no wallet
  loads.

**`dxGetNewTokenAddress`**
- `params`: `ticker` (string).
- `result`: array with one fresh address; empty `[]` when no wallet is loaded
  for the ticker.
- `errors`: 1002 (wallet failure).

**`dxGetUtxos`**
- `params`: `token` (string), `include_used` (bool, default `false`).
- `result`: array of UTXO objects: `txid`, `vout`, `address`, `amount`,
  `scriptPubKey`, `confirmations`, `orderid` (the order locking it, or `""`).
  Locked UTXOs are excluded unless `include_used=true`.
- `errors`: 1025, 1018 (no wallet for token), 1002 (wallet failure).

**`dxGetLockedUtxos`**
- `params`: `id` (string) [optional].
- `result`: without `id` — `{"all_locked_utxo":["txid:vout:amount:address",…]}`;
  with `id` — `{"id":…,"<from>_and_<to>":[…],…}` keyed by the order's currency
  pair (accepted orders) or `<from>` (pending orders).
- `errors`: 1029 (no exchange wallets configured), 1025 (malformed id),
  1021 (unknown order).

**`dxSplitAddress`**
- `params`: `token` (string), `splitamount` (string), `address` (string),
  `include_fees` (bool, default `true`), `show_rawtx` (bool, default `false`),
  `submit` (bool, default `true`).
- `result`: `{"token":…,"include_fees":…,"split_amount_requested":…,
  "split_amount_with_fees":…,"split_utxo_count":…,"split_total":…,"txid":…,
  "rawtx":…}` (rawtx empty unless `show_rawtx`).
- `errors`: 1025, 1018, 1026 (bad address), 1004 (dust split amount), 1019
  (insufficient funds), 1002.

**`dxSplitInputs`**
- `params`: `token`, `splitamount`, `address`, `include_fees`, `show_rawtx`,
  `submit` (all required), `utxos` (array of `{txid, vout, amount,
  scriptPubKey, address}`). Exactly **7** params.
- `result`: same 8-field split object as `dxSplitAddress`.
- `errors`: as `dxSplitAddress`, plus 1025 (unknown coin / bad utxos) and 1004
  (empty utxos array).

### Other

**`dxGetTradingData`**
- `params`: `blocks` (int, default `43200`) [optional], `errors` (bool, default
  `false`) [optional].
- `result`: array of 8-field records `{timestamp, fee_txid, nodepubkey, id,
  taker, taker_size, maker, maker_size}`. Session-local fills only — see
  Tier 3. `fee_txid`/`nodepubkey` are always `""`: they come from the on-chain
  BLOCK scan (RPC-F38/F39) that a thin client cannot replay. `blocks`/`errors`
  are accepted for contract compatibility but cannot bound a BLOCK block scan.

**`dxLoadXBridgeConf`**
- `params`: none.
- `result`: `true`. Reloads `xbridge.conf` (keeps the last good config on
  failure).
- `errors`: 1025 (reload failure).

**`getnetworkinfo`**
- `params`: none.
- `result`: a bitcoind-shaped object: `version`, `subversion`, `protocolversion`,
  `xbridgeprotocolversion`, `xrouterprotocolversion`, `localservices`,
  `localrelay`, `timeoffset`, `networkactive`, `connections`, `networks`,
  `relayfee`, `incrementalfee`, `localaddresses`, `warnings`.
- `errors`: 1025 (any params).

## Tier 3 — thin-client limits

`xbridged` is a thin client: it has no BLOCK block index and no network-wide
view, so three families of commands are bounded by what this node has observed:

- **`dxGetOrderHistory` / `dxGetTradingData`** aggregate from **session-local
  fills** only (fills this node saw on its P2P feed); they cannot replay
  historical chain data. `dxGetTradingData` accordingly reports
  `fee_txid`/`nodepubkey` as `""` (they require the on-chain BLOCK scan,
  RPC-F38/F39).
- **`dxGetNetworkTokens`** is bounded by the servicenodes this node is currently
  connected to (their advertised wallet services), not the whole network.
- **Locked-UTXO accounting** (`dxGetUtxos`, `dxGetLockedUtxos`,
  `dxGetTokenBalances`) reflects orders **this node** has seen; a UTXO locked by
  an unseen order is not subtracted.

## Divergences from blocknetd

The full C++↔Go fidelity register — including the known response-shape
differences (order-book detail-4 nesting, `dxGetMyPartialOrderChain` unknown-id
behavior, `dxSplitInputs` utxo schema, per-command amount formats) — is tracked
in [`audit/register.md`](audit/register.md). This document is the stable
contract for the port as built.
