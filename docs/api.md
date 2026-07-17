# XBridge JSON-RPC API (`api` package)

`xbridge-go/api` is a **1:1 port of blocknetd's XBridge `dx*` JSON-RPC surface**
(`src/xbridge/rpcxbridge.cpp`). The goal is a drop-in replacement: point a
dapp's RPC URL at `xbridged` and it reaches the XBridge API unchanged.

## Contract (derived from `rpcxbridge.cpp`)

- **Transport:** JSON-RPC 1.0 over HTTP (bitcoind-style). Request
  `{"method":..., "params":[...positional...], "id":...}`.
- **Params are positional**, not named (C++ uses a `json_spirit` array).
- **Method names** (24, exact — including the lowercase `gettradingdata`
  alias): see `dispatch.go`.
- **Response object field names and JSON value types match C++ exactly:**
  - amounts are **strings** (`"100.000000"`) to preserve precision;
  - dates are **ISO-8601 strings with millisecond precision**
    (`"2018-01-15T18:15:30.123Z"`) — see `iso8601`;
  - `partial_repost` is a **bool**;
  - errors are returned as the **`result` object**
    `{"error":..., "code":..., "name":...}`; the JSON-RPC envelope `error`
    stays `null`. Only transport failures (parse error, unknown method) use
    the envelope `error` field.

## Amounts

XBridge order amounts are stored in base units of `COIN = 1_000_000`
(`xbridgetransactiondescr.h`). `formatXAmount` mirrors C++
`xBridgeStringValueFromAmount` rendered with `std::fixed`
`setprecision(xBridgeSignificantDigits(COIN))` == `setprecision(6)` → **6
decimal places**. `formatXPrice` uses the same precision. `parseXAmount`
mirrors `xBridgeAmountFromString` (`floor(val*COIN)`, truncating to 6 decimals).

## Timestamps

Order timestamps are **microseconds** since epoch (`timeToInt` →
`total_microseconds`). `iso8601` renders them as `YYYY-MM-DDTHH:MM:SS.mmmZ`
(3-digit ms, matching C++ `iso8601` — the help-text `.12345Z` examples are
misleading; the code emits 3 ms digits).

## Status strings

`statusString` maps both the raw `trXxx` enum names and the
`TransactionDescr::strState()` strings to the latter (the values the API
returns): `open`, `created`, `accepting`, `hold`, `initialized`, `signed`,
`commited`, `finished`, `canceled`, `expired`, `dropped`, `invalid`, ...

## Implemented vs. shaped

**Fully functional (real behavior):**
- `dxGetOrders`, `dxGetOrder`, `dxGetMyOrders`, `dxGetOrderBook` — read from the
  live P2P-fed order book (`Store`, populated by `xbcPendingTransaction` /
  `xbcTransaction` broadcasts).
- `dxMakeOrder` / `dxMakePartialOrder` — build, sign (secp256k1 compact ECDSA
  via `crypto.BtcSigner`), and broadcast a real `xbcTransaction` packet; return
  the exact `makeOrderResult`. `dxMakeOrder` (exact) emits `partial_*` =
  `"0.000000"` and `status` = `"created"`; `dxMakePartialOrder` emits
  `order_type` = `"partial"` with the real `partial_minimum` /
  `partial_orig_*_size` and `partial_repost`, `status` = `"created"`.
- `dxTakeOrder` — broadcasts an `xbcTransactionAccepting` packet, returns the
  exact order-list shape.
- `dxCancelOrder` — broadcasts an `xbcTransactionCancel` packet, returns the
  exact shape (no `partial_*` fields, per C++).

**Shaped / partial (correct object shape, backing not yet wired):**
- `dxGetOrderFills`, `dxGetMyPartialOrderChain`, `dxPartialOrderChainDetails`,
  `dxGetLockedUtxos`, `dxFlushCancelledOrders` — correct shapes from the store.
- `dxGetLocalTokens` / `dxGetNetworkTokens` — coin registry keys.
- `dxGetTokenBalances`, `dxGetUtxos`, `dxGetNewTokenAddress`, `dxSplitAddress`,
  `dxSplitInputs` — require a wallet connector (the `wallet` package exists);
  currently return empty/error shapes.
- `dxGetOrderHistory`, `dxGetTradingData` / `gettradingdata` — require
  historical blockchain data a thin client lacks; return empty arrays.

## Known deviations / VERIFY

1. **JSON field order.** Result structs reuse an embedded `orderBase`; Go
   preserves struct declaration order, which differs from C++'s emission order
   in a few responses (notably `dxMakeOrder` puts `id` then `maker_address`
   first, while we emit `id`, `maker`, `maker_size`, ... then addresses). JSON
   object key order is not semantically significant and dapps parse by key;
   content is identical. Flagged for awareness.
2. **Amount decimal count (now 6).** C++ `setprecision(
   xBridgeSignificantDigits(1_000_000))` == `setprecision(6)` (the loop in
   `xBridgeSignificantDigits` returns 6). `formatXAmount`/`formatXPrice` render
   **6 decimals**. This is now fixed (audit C1); the old "7 decimals" claim in
   this doc and in CLAUDE.md was wrong and has been corrected.
3. **Error `code`/`error` (now C++-faithful).** Error `code` values are the C++
   1000-range enum (`xbridgeerror.h`: `INVALID_PARAMETERS=1025`,
   `NO_SESSION=1018`, `TRANSACTION_NOT_FOUND=1021`, `INVALID_ADDRESS=1026`,
   `INSUFFICIENT_FUNDS=1019`, `INVALID_STATE=1028`, `BAD_REQUEST=1004`,
   `NOT_EXCHANGE_NODE=1029`, `UNKNOWN=1002`, …). The `error` string is built by
   `xbridgeErrorText(code, arg)` which prepends the per-code text (audit C2/C3).
4. **JSON-RPC envelope (now 1.0).** Responses are JSON-RPC 1.0 — compact, with
   only `result`/`error`/`id` (no `jsonrpc` field). Business errors live in the
   `result` object; the envelope `error` stays null (audit C5).
5. **Take-order handshake.** `dxTakeOrder` broadcasts the accepting packet and
   registers a client-side `SwapSession`; the hold→init→create→confirm deposit
   handshake **is now wired** — `dxMakeOrder`/`dxTakeOrder` spawn sessions
   (`newMakerSession`/`newTakerSession`) and `Node.feed` dispatches the
   hub-originated handshake packets (Hold/Init/CreateA/CreateB/ConfirmA/ConfirmB/
   Finished) to the session handlers in `api/swap.go`, which build/broadcast the
   HTLC deposits and claim/refund spends. Covered by `TestSwapHandshake` with
   in-memory connectors; the remaining gap is verification against a **live**
   Blocknet hub over `p2p`.
