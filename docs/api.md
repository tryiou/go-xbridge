# XBridge JSON-RPC API (`api` package)

`go-xbridge/api` is a **1:1 port of blocknetd's XBridge `dx*` JSON-RPC surface**
(`src/xbridge/rpcxbridge.cpp`). The goal is a drop-in replacement: point a
dapp's RPC URL at `xbridged` and it reaches the XBridge API unchanged.

## Contract (derived from `rpcxbridge.cpp`)

- **Transport:** JSON-RPC 1.0 over HTTP (bitcoind-style). Request
  `{"method":..., "params":[...positional...], "id":...}`.
- **Params are positional**, not named (C++ uses a `json_spirit` array).
- **Method names** — 23 distinct `dx*` commands (the C++ `gettradingdata`
  command is intentionally NOT exposed; only `dxGetTradingData` is), exact:
  see `dispatch.go`.
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

## Implementation status

All 23 `dx*` commands are ported against the C++ writers; the remaining
divergences (beyond the **Tier 3** limits below) are tracked in the per-command
verdict matrix in [`AUDIT.md`](AUDIT.md). For per-package state see
[`STATUS.md`](STATUS.md). This document is the stable **contract**, not the
status log.

## Tier 3 — architectural limits (thin-client, cannot fully match C++)

These divergences are inherent to the **no `blocknetd`** design: go-xbridge is a
client that speaks the XBridge wire protocol to live service nodes but holds no
BLOCK block index and replays no historical chain. They are **documented, not
silently divergent**.

- **`dxGetOrderHistory` / `dxGetTradingData`** — reflect *session-local* fills
  only (the `Store.fills` recorded by this client). `dxGetTradingData` emits the
  8-field record (`fee_txid`/`nodepubkey` empty, session-local data source).
  The C++ `gettradingdata` command is **not exposed** in the Go interface — only
  `dxGetTradingData` is. C++ derives these from the BLOCK chain index across all
  servicenode-confirmed trades.
  - *Why:* no local block index; the client never replays historical BLOCK data.
  - *What parity would require:* embedding `blocknetd` (or a BLOCK block
    indexer + XSeries trade-history RPC) so fills can be fetched from chain
    rather than only from this session's memory.
- **`dxGetNetworkTokens`** — completeness is bounded by the P2P servicenode-ping
  coverage the client currently sees; it cannot enumerate every token C++
  learns from the full servicenode network. The token set is now learned from
  real `SNREGISTER`/`SNPING`/`SNLISTPING` messages via `p2p/servicenode.Registry`
  (wallet-token regex, `xr`/`xrs` exclusion, 5-minute running window), but
  remains P2P-bounded.
  - *Why:* P2P discovery is incremental and depends on which servicenodes the
    client has connected to.
  - *What parity would require:* a fuller servicenode handshake / network-state
    sync, or a trusted token-list source, to match C++'s network-wide view.
- **`dxGetLockedUtxos` / `dxGetUtxos` locked set** — the locked set is derived
  from orders *this client knows about* (its `Store`). A UTXO locked by an order
  the client has not seen is not subtracted from balances.
  - *Why:* the client only learns of orders it has observed on the P2P feed.
  - *What parity would require:* tracking every in-flight order network-wide
    (same root cause as `dxGetNetworkTokens`).

Everything outside Tier 3 is behaviorally 1:1; see
[`AUDIT.md`](AUDIT.md) for the per-command divergence register.

## Known deviations

1. **JSON field order.** Result structs reuse an embedded `orderBase`; Go
   preserves struct declaration order, which differs from C++'s emission order
   in a few responses (notably `dxMakeOrder` puts `id` then `maker_address`
   first, while we emit `id`, `maker`, `maker_size`, ... then addresses). JSON
   object key order is not semantically significant and dapps parse by key;
   content is identical. Flagged for awareness.

The full divergence register (S1–S4) and per-command verdict matrix are in
[`AUDIT.md`](AUDIT.md); implementation status (incl. the remaining live-hub
verification gap for the swap handshake) is in [`STATUS.md`](STATUS.md).
