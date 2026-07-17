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
- `dxGetOrderFills`, `dxGetMyPartialOrderChain` — correct shapes from the store.
- `dxGetLocalTokens` / `dxGetNetworkTokens` — coin registry keys.
- `dxGetNewTokenAddress`, `dxSplitAddress`, `dxSplitInputs` — require a wallet
  connector (the `wallet` package exists); currently return empty/error shapes.

**Fully functional after the C++ parity pass (Tier 1 + Tier 2):**
- `dxPartialOrderChainDetails` — counts `total_orders_open` as
  `stateOrdinal(status) <= trPending(2)` (matches C++); returns an empty `{}`
  for an unknown chain instead of an error; validates the order id (64-hex,
  `INVALID_PARAMETERS` otherwise); emits `p2sh_deposits` /
  `p2sh_deposits_counterparty` from each order's `BinTxId` / `OBinTxId`.
- `dxTakeOrder` — an omitted or zero `amount` is a **full-order** take
  (sizes equal the order's maker/taker sizes), matching C++.
- `dxGetLockedUtxos` — nil-guarded; derives the locked set from each active
  order's reserved `Utxos` (`Store.LockedUtxoInfo`), keyed `txid:vout`.
- `dxGetUtxos` — excludes UTXOs reserved by active orders (`include_used=false`),
  returns them with `orderid` set when `include_used=true`.
- `dxGetTokenBalances` — subtracts each currency's locked UTXOs from the wallet
  total; the `Wallet` key is derived from the BLOCK connector (fallback: the
  first configured exchange wallet).
- `dxFlushCancelledOrders` — `Store.FlushCancelled` clamps the `uint64`
  subtraction so a huge `minAgeMillis` prunes everything instead of wrapping
  around.

## Tier 3 — architectural limits (thin-client, cannot fully match C++)

These divergences are inherent to the **no `blocknetd`** design: xbridge-go is a
client that speaks the XBridge wire protocol to live service nodes but holds no
BLOCK block index and replays no historical chain. They are **documented, not
silently divergent**.

- **`dxGetOrderHistory` / `dxGetTradingData` (`gettradingdata`)** — reflect
  *session-local* fills only (the `Store.fills` recorded by this client).
  `fee_txid` and `nodepubkey` are empty. C++ derives these from the BLOCK chain
  index across all servicenode-confirmed trades.
  - *Why:* no local block index; the client never replays historical BLOCK data.
  - *What parity would require:* embedding `blocknetd` (or a BLOCK block
    indexer + XSeries trade-history RPC) so fills can be fetched from chain
    rather than only from this session's memory.
- **`dxGetNetworkTokens`** — completeness is bounded by the P2P servicenode-ping
  coverage the client currently sees; it cannot enumerate every token C++
  learns from the full servicenode network.
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

The Tier 1 (code bugs) and Tier 2 (achievable backing gaps) items above are
**fixed**; see `audit-dx-equivalence.md` for the divergence register and the
fixes applied per method.

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
