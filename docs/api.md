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
  - amounts are **strings** (`"100.0000000"`) to preserve precision;
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
`xBridgeStringValueFromAmount`: `(amt/1e6 + 1e-8)` rendered with
`std::fixed` `setprecision(7)` → **7 decimal places**. `parseXAmount` mirrors
`xBridgeAmountFromString`: `floor(val*1e6 + 1e-8)`.

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
  the exact `makeOrderResult` (`partial_*` = `"0"`, `status` = `"created"`).
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
2. **Amount decimal count (7 vs 6).** `setprecision(
   xBridgeSignificantDigits(1_000_000))` = `setprecision(7)`. The C++ help-text
   examples show 6 decimals, but the code uses 7. If real blocknetd is observed
   emitting 6, change `formatXAmount`/`formatXPrice` to 6.
3. **Take-order handshake.** `dxTakeOrder` broadcasts the accepting packet and
   returns the shaped response, but does not yet drive the full
   accept→hold→init→create→confirm deposit handshake (the `swap` package, not
   yet wired to P2P). This is the remaining swap-phase work.
