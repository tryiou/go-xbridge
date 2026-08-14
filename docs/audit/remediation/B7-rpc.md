# B7 — RPC response-surface parity (RPC-F03..F57, F59)

Branch: `fix/rpc-surface` (off `main` @ B4 merge `d7394ee`).
Status: MERGED into `main` @ `7799943` (fast-forward).
C++ reference: Blocknet Core @ `ac930b7f8` (v4.4.1 era).
Go subject: `api/response.go`, `api/handlers.go`, `api/order.go`,
`api/store.go`, `api/node.go`, `coins/amount.go` (45 OPEN findings — F19,
F37, F38, F39 documented; F47-F52, F58 owned by B4; per `register.md`
Owner B7).

The RPC semantic pass over the **result shape / order / validation** of the
`dx*` methods (B4 owned the transport, strict param types, and arity gates;
B7 owns what the handlers put in the result object). F57 and F59 were
confirmed conforming at HEAD and are pinned by tests only.

## Findings resolved on this branch

| ID | Title | C++ source of truth | Go call sites → fix |
|---|---|---|---|
| F03 | `dxGetOrders` array order random (Go map) vs id-ascending (C++ `std::map`) | `xbridgeapp.cpp` `m_transactions` = `std::map<uint256, …>`; uint256 `operator<` = memcmp from `data[0]` (**LSB-first**, `uint256.h:45-49`) | `api/handlers.go:118` `Store.List()` → sort by new `orderIDLess` |
| F04 | `dxGetOrders` 60 s filter boundary (µs-exact vs second-truncated) | `rpcxbridge.cpp:438-444` `(currentTime - tr->txtime).total_seconds() > 60` | `api/handlers.go:121` `now-o.Updated > 60*1e6` → second-truncated on creation time |
| F05 | Exactly-64-hex id gate vs C++ `uint256S` left-pad/truncate tolerance | `uint256.cpp:27-53` `SetHex` (spaces/`0x`, fill from string end, <64 left-pad, ≥64 truncate) | `api/response.go:517` `parseOrderID` → tolerant `parseOrderIDS`; per-method `IsNull` |
| F06 | `dxGetOrder` not-found message renders id raw vs zero-padded | `rpcxbridge.cpp:771-796` not-found uses `id.ToString()` (= `GetHex`) | `api/handlers.go:161,171` raw `id` → `orderIDString(parsed)` |
| F07 | `dxCancelOrder` cancels *before* validating connectors (side-effect order) | `rpcxbridge.cpp:1345-1401` cancel runs before connector lookups; `state>=trCreated`→1028 | `api/handlers.go:474-480` move `Node.CancelOrder` before `connector` |
| F08 | `dxCancelOrder` cancel-failure codes/text (incl. `isLocal`→1021) | `xbridgeapp.cpp:2473-2501` `cancelXBridgeTransaction`; isLocal→1021 `"Transaction  not found"` (double space), NO_SESSION(from)→1018 | `api/node.go:1743-1837` `Node.CancelOrder` code/text set |
| F09 | `dxMakeOrder` response field ORDER differs | `rpcxbridge.cpp:1048-1067`: `created_at` BEFORE `updated_at`; `maker_address` 2nd, `taker_address` 5th, `block_id` 10th | `api/response.go:57-62` `makeOrderResult` → flat Layout B |
| F10 | `dxMakeOrder` dryrun returns real id + extra fields vs C++ zero id | `rpcxbridge.cpp:1004-1021` dryrun → zero id, 14 fields, no timestamps/block_id | `api/node.go:1222-1304` → `dryrunOrderResult` |
| F11 | `dxMakeOrder` non-partial `partial_*` values: literal `"0"` vs `"0.000000"` | `rpcxbridge.cpp:1057-1066` `"0"` | `api/order.go:264-266` |
| F12 | `dxMakeOrder` `NO_SERVICE_NODE` message includes the pair; C++ bare | `rpcxbridge.cpp:1036-1037` default-case `makeError` | `api/node.go:929-944`; `requireWrite` `name:"dx"` leak → method name |
| F13 | `dxMakePartialOrder` dust gate: 1e6-scale minFrom vs native 1e8-scale dust | `xbridgewalletconnectorbtc.cpp:1900-1904` `isDustAmount` (native) vs `xBridgeAmountFromReal` | `api/node.go:978-982` normalize scales |
| F14 | `dxTakeOrder` explicit amount `"0"` is a full take in Go, error 1025 in C++ | `rpcxbridge.cpp:1152-1161` `"The amount cannot be less than or equal to 0: <n>"` | `api/node.go:1383-1407`; invert `TestDxTakeOrderFullTake` |
| F15 | `dxTakeOrder` error texts/names leak (`INVALID_ADDRESS` text, `name` "dxMakeOrder") | `rpcxbridge.cpp:1208-1225` address/own-order/connector-swap messages | `api/node.go:851-865,1440-1496` thread method name |
| F16 | `dxGetOrderHistory` sums the WRONG asset volume (taker vs from/maker) | `rpcxbridge.cpp:663-679` `x.fromVolume` | `api/handlers.go:596-614` `volume += makerNum` |
| F17 | `dxGetOrderHistory` encoding: shortest-roundtrip vs C++ fixed-8; raw ratio vs quantized price | `uret(…, 8)`; `ccy::Asset::Price` 1e-6 rational round-half-up (`currency.h:107-123`) | `api/handlers.go:596-614` fixed-8 + quantize |
| F18 | `dxGetOrderHistory` validation missing (whitelist, end≤start, limit) | `xQuery` ctor + `rpcxbridge.cpp:588-704` (messages below) | `api/handlers.go:492-638` |
| F20 | `dxGetOrderBook` price omits C++ +1/COIN bump | `xutil.cpp:223-227` `xBridgeValueFromAmount`; `:293-312` | `api/handlers.go:717-726` |
| F21 | `dxGetOrderBook` equal-best-price tie-break nondeterministic | `rpcxbridge.cpp:1654/1704,1879/1931` `max_element`/`min_element` over id-ordered map | `api/handlers.go:731-732` smallest id via `orderIDLess` |
| F22 | `dxGetOrderBook` int-width (Go `int` vs C++ int64_t) | `rpcxbridge.cpp:1666-1697` | `api/handlers.go:682,696-698` |
| F23 | `dxGetTokenBalances` `"Wallet"` key derivation/presence | non-conformable (thread-completion order) | **DOCUMENTED** |
| F24 | `dxGetTokenBalances` key order (Go map-sorted, Wallet last) | non-conformable | **DOCUMENTED** |
| F25 | `dxGetTokenBalances` precision (C++ per-UTXO double sum vs Go exact) | — | golden test only |
| F26 | `dxGetMyOrders` field order, dedup, sort | `rpcxbridge.cpp:2128-2171` addr 4th/7th; dedup; `a->txtime < b->txtime` | `api/handlers.go:896-919`, `api/response.go:50-54` |
| F27 | `dxGetMyPartialOrderChain` chain membership differs | `xbridgeapp.cpp:3913-3939` live+history, `isLocal && (parent || partialAllowed)`; sort by created `:3985-3988` | `api/handlers.go:958-1019` |
| F28 | `dxPartialOrderChainDetails` `p2sh_deposits` array length ≠ chain length | `rpcxbridge.cpp:2455-2457` one entry per order (empty strings) | `api/handlers.go:1066-1073` |
| F29 | `dxPartialOrderChainDetails` bad-id error text | `rpcxbridge.cpp:2413-2414` `"bad order id"` | `api/handlers.go:1031` |
| F30 | `dxPartialOrderChainDetails` key order alphabetized (Go map) | `rpcxbridge.cpp:2460-2480` insertion order | `api/handlers.go:1021-1103` ordered emission |
| F31 | `dxGetLockedUtxos` amount encoding (default-float vs fixed-6/native) | `xbridgewalletconnector.cpp:25-30` `UtxoEntry::toString` default-double | `api/handlers.go:1141-1144,1177-1180` |
| F32 | `dxGetLockedUtxos` 1021 trigger too narrow in Go | `rpcxbridge.cpp:2637,2660-2670` pending valid OR accepted valid | `api/handlers.go:1150-1157` |
| F33 | `dxGetLockedUtxos` per-order key selection (status ordinal vs map membership) | `rpcxbridge.cpp:2660-2670` pending→`a_currency`, else accepted→`a_and_b` | `api/handlers.go:1163-1167` |
| F34 | `dxGetLockedUtxos` id echo un-normalized | `id.ToString()` | `api/handlers.go:1204-1207` |
| F35 | `dxFlushCancelledOrders` flushes only the cancelled ledger, not book/history | `xbridgeapp.cpp:1331-1354` erases `m_transactions` + `m_historicTransactions` | `api/store.go:573-595` |
| F36 | `dxFlushCancelledOrders` use_count/ordering/key order | id-sorted map; `use_count` from refcount | `api/store.go`, `api/handlers.go:1244-1249` |
| F37 | `gettradingdata` (lowercase) missing from Go dispatch | deliberate (C++ key shape differs) | **DOCUMENTED** (kept removed) |
| F38 | `dxGetTradingData` `fee_txid`/`nodepubkey` always `""` | 43200-block/30-day BLOCK-chain scan | **DOCUMENTED** (thin client) |
| F39 | (trading-data interval membership) | same scan | **DOCUMENTED** |
| F40 | `dxSplitInputs` requires amount/scriptPubKey; C++ needs only txid/vout | `rpcxbridge.cpp:3363-3367` re-fetch via `getUnspent` | `api/handlers.go:1716-1733` |
| F41 | `dxSplit` fee formula differs | `xbridgewalletconnectorbtc.cpp:1949-1972,2665-2734` `minTxFee1/minTxFee2` | `api/handlers.go:1445,1552-1571` |
| F42 | `dxSplit` change destination (requested address vs fresh) | `rpcxbridge.cpp:3371-3385` | `api/handlers.go:1428-1435` |
| F43 | `dxSplit` submit-failure code and error names | `rpcxbridge.cpp:3391-3395` 1004 | `api/handlers.go:1510-1512` |
| F44 | `dxGetUtxos` amounts trimmed vs C++ fixed-8 | `xutil.cpp:216-219` `setprecision(xBridgeSignificantDigits(denomination))` = 8 for `COIN=1e8` | `api/handlers.go:1786`, `coins/amount.go` |
| F45 | `dxGetUtxos` listunspent failure code/text (1004 vs 1002) | `rpcxbridge.cpp:3421-3424` | `api/handlers.go:1762-1765` |
| F46 | `getnetworkinfo` shim diverges from real blocknetd | `src/rpc/net.cpp:447-512` (values below) | `api/handlers.go:1805-1846` |
| F53 | `dxGetLocalTokens` returns unconnected/duplicate tickers | `xbridgeapp.cpp:808-821` `availableCurrencies()` over connected connectors | `api/handlers.go:187-189` |
| F54 | `dxGetNetworkTokens` membership: Go unions config; C++ pure SN service union | `rpcxbridge.cpp:281-327` | `api/node.go:679-703` |
| F55 | `dxGetNewTokenAddress` error path returns `[]` in C++, business 1002 in Go | `rpcxbridge.cpp:150-192` empty-array return | `api/handlers.go:254-264` |
| F56 | `dxLoadXBridgeConf` reload failure shape and side effects | `xbridgeapp.cpp` `clearNonLocalOrders`/`clearBadWallets` | `api/handlers.go:228-236`, `api/node.go:306-353` |
| F57 | `dxGetOrderBook` detail-4 nesting `[[…]]` vs flat | already conforming | golden test only |
| F59 | `dxGetMyPartialOrderChain` unknown/malformed id handling | `rpcxbridge.cpp:2273-2274` 1025 `"bad order id"` | already FIXED (`register.md:98`); pin in G8 |

## Contract corrections (verified this session)

- **F09 — `findings.md` card is wrong about C++.** `dxMakeOrder` emits
  `created_at` **before** `updated_at` (rpcxbridge.cpp:1048-1067), with
  `maker_address` 2nd, `taker_address` 5th, `block_id` 10th. `dxGetOrders` /
  `dxGetOrder` / `dxGetMyOrders` / `dxTakeOrder` use the **opposite** layout
  (`updated_at` before `created_at`). `dxGetMyOrders` interleaves
  `maker_address` as the **4th** field and `taker_address` as the **7th**
  (rpcxbridge.cpp:2151-2163: `id, maker, maker_size, maker_address, taker,
  taker_size, taker_address, updated_at, …`). So `orderBase` (Layout A) is
  already correct for the list methods; only `makeOrderResult` (Layout B) and
  `orderDetailResult` (addresses interleaved at 4/7) need restructuring.
- **F14 — `findings.md` card was inverted.** C++ rejects `amount <= 0` with
  1025 `"The amount cannot be less than or equal to 0: <n>"`; the Go full-take
  on `"0"` is the bug.
- **F41 — fee is NOT `520·fpb`.** Split fee per utxo = `minTxFee1(1,3) +
  minTxFee2(1,1)` where `minTxFee = max((192·nIn + 34·nOut)·feePerByte,
  MinTxFee)`; the real tx fee is `minTxFee1(vins, vouts)` clawed back from the
  change/last vout.
- **F03/F21/F36 — uint256 map order is LSB-first memcmp**, not display-hex
  ascending (`uint256.h:45-49`). Internal `[32]byte` ids already store
  LSB-first (`id[0]` = last display byte), so `orderIDLess` is
  `bytes.Compare(a[:], b[:]) < 0`.
- **F05 — `uint256S` tolerance.** `SetHex` (uint256.cpp:27-53) skips leading
  spaces and `0x`, consumes a contiguous hex run, fills nibbles **from the
  string end** into `data[0]` upward (so <64 hex left-pads with zeros, ≥64
  truncates the leading chars); it never errors on content. Null-checking is
  **per method**, not global: `dxCancelOrder` → 1025 `"Invalid order id
  [<raw sid>]"`; `dxGetMyPartialOrderChain` / `dxPartialOrderChainDetails` →
  1025 `"bad order id"`; `dxGetOrder` / `dxTakeOrder` have **no** null gate
  (null id just misses → 1021). `dxGetLockedUtxos` is special: a null id
  returns **`{"all_locked_utxo": […]}`** of every locked entry (getUtxoItems
  returns all for `txid.IsNull()`, xbridgeexchange.cpp:278-283; rpcxbridge.cpp
  :2648-2652) — it never 1021s on a null/garbage id.
- **F46 — real-daemon `getnetworkinfo`** (src/rpc/net.cpp:447-512):
  `protocolversion` 70713; `subversion` `/Blocknet:4.4.1/`; adds
  `xbridgeprotocolversion` 55 (`src/xbridge/version.h`), `xrouterprotocolversion`
  50 (`src/xrouter/version.h`); `relayfee`/`incrementalfee` are **strings**
  (`ValueFromAmount`, 8-decimal fixed: `"0.00010000"` / `"0.00001000"`);
  `networks[]` entries carry `proxy_randomize_credentials` (false here).
- **F17 — history row time is the bucket START.** C++ emits
  `ArrayIL{ iso8601(x.timeEnd - offset), low, high, open, close, volume }`
  (rpcxbridge.cpp:681-682) where offset = granularity for `at_start` (the
  DEFAULT `interval_timestamp`) and 0 for `at_end`; so with the default the
  row timestamp is `x.timeEnd - granularity` = the bucket start. Go emits
  `iso8601(bucketStart * 1e6)` (handlers.go:742-748), matching. Volume =
  **from/maker side**.
- **F04/F26 — txtime source.** Go `Order` has no `Txtime` field; the store
  tracks `Created`/`Updated` only. C++ `txtime` is the **last-update** time
  (mutated by `updateTimestamp`, xbridgetransactiondescr.h:630-634), so for
  the 60 s filter on long-lived cancelled/finished orders `o.Created` diverges
  from `txtime`; the G1 golden decides whether that divergence is observable
  for the local-store lifecycle, otherwise `o.Created` is used.

## Design notes / decisions

- **`orderIDLess`** — `bytes.Compare(a[:], b[:]) < 0` on the internal `[32]byte`
  id (LSB-first = C++ `std::map<uint256>` order). Used by F03 (dxGetOrders
  sort), F21 (book tie-break), F36 (flush ordering).
- **`parseOrderIDS`** — tolerant parser, no error return; callers decide the
  null policy per method (table above). Keeps the B4 `spStr` type gate: a
  non-string param still surfaces the json_spirit type error.
- **Order layouts** — `orderBase` (Layout A) stays; `makeOrderResult` becomes a
  flat Layout B struct; `orderDetailResult` interleaves addresses at 4/7.
- **Fixed rendering** — new `coins.FormatAmountFixed(c, v)` (fixed digit-count
  of `c.Decimals`, no trim) for dxGetUtxos (F44); a fixed-8 string formatter
  for the order-history OHLCV doubles (F17); `xBridgeStringValueFromPrice`
  equivalent for the order book (F20). A `xBridgeValueFromAmount` helper
  (`amount/COIN + 1/::COIN` round-up-sat) for the price bump.
- **DOCUMENTED divergences (no code change)** — F23/F24 (no synthesized
  `"Wallet"` key; C++ ticker order is race-dependent), F37 (gettradingdata
  alias deliberately absent), F38/F39 (trading-data needs a 43200-block/30-day
  on-chain scan the thin client cannot do). Each has a register row +
  rationale.
- **Scope** — STATE-F79 and CRYPTO-F89 were tagged Owner B7 in the register but
  belong to B8 / B9; G15 reassigned them (STATE-F79 → B8, CRYPTO-F89 → B9,
  SEC-F02 → B8) instead of expanding scope.
- **Doc correction (done at G15)** — CLAUDE.md + AGENTS.md + `docs/architecture.md`
  + `docs/api.md` "6-decimal fixed" rule was stale for `COIN=1e8` coins; display
  precision is `xBridgeSignificantDigits(COIN)` (digit-count of COIN).

## Tests

- `api/response_test.go` — `TestParseOrderIDS` matrix (1-63 hex left-pad, 64,
  65 truncate, `0x`, whitespace, non-hex suffix, uppercase); `orderIDLess`
  ordering; Layout B make-order bytes; `orderDetailResult` address interleave.
- `api/handlers_test.go` — dxGetOrders id order + 60/61 s boundary; dxGetOrder
  padded not-found; dxCancelOrder short-id 1025 + side-effect-before-validation
  + code/text set; dxGetOrderHistory fixed-8/volume-side/end-time/validation
  matrix; dxGetOrderBook +1/COIN price golden, deterministic tie-break, detail-4
  golden; dxGetMyOrders order/dedup/sort; partial-chain filters/bad-id/key-order/
  p2sh alignment/totals; dxGetLockedUtxos 1021-validity/key-selection/id-echo/
  amount; dxFlushCancelledOrders book+history prune, use_count, ordering,
  key order; dxSplit txid/vout-only, fee golden, change-address, 1004 names;
  dxGetUtxos fixed-8 + 1004; getnetworkinfo daemon-aligned strings/versions/
  networks; dxGetLocalTokens connected filter; dxGetNetworkTokens pure union;
  dxGetNewTokenAddress `[]`; dxLoadXBridgeConf `false` + side effects/guards.
- `api/node_test.go` — MakeOrder Layout B + dryrun + dust-at-native-scale;
  TakeOrder explicit-0/own-order/message texts; CancelOrder codes.
- `coins/coins_test.go` — `FormatAmountFixed` cases.
- `conformance/conformance_suite_test.go` — verify no RPC result-shape rows are
  broken by the reorderings (the shape test is still unwired).

## Verify

`gofmt` → `go build` / `go vet` / `go test ./...` → `go test -race ./api/...` →
from `tools/`: `make parity` + `make canary`. Register + remediation-plan
closeout on this branch, then merge approval.
