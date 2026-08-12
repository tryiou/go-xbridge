# Verification Report B — go-xbridge vs Blocknet Core xBridge conformance findings

Re-read of the full call graphs on both sides. Reference = C++
`blocknet_core/src/xbridge/` + `src/rpc/net.cpp`; candidate = `go-xbridge/api/*`.
All paths below are relative to the workspace root.

---

## RPC-F16 dxGetOrderHistory VOLUME ASSET — CONFIRMED

**C++:** the emitted volume is the **maker/from asset** (the "from" side of the
trade pair), accumulated per-fill as `fromVolume += x.from` in
`blocknet_core/src/xbridge/util/xseries.cpp:195` and emitted as
`double volume = x.fromVolume.amount<double>();` in
`blocknet_core/src/xbridge/rpcxbridge.cpp:684`. `x.from` is built from
`tr.fromAmount` (maker/from amount) in `xseries.cpp:39-42`.
Note the C++ help text (`rpcxbridge.cpp:627-628`) claims "Total volume of the
taker asset" — the *code* disagrees with its own doc; the code sums the maker asset.

**Go:** `volume += takerNum` where `takerNum` is the parsed fill **taker** size
(`go-xbridge/api/handlers.go:541` price = `takerNum/makerNum`, `:552`
`volume += takerNum`). So Go sums the taker asset; C++ sums the maker asset.

Corrected statement: C++ OHLCV volume is the sum of maker/from asset amounts
(fromVolume, xseries.cpp:195 → rpcxbridge.cpp:684) while Go sums the taker asset
size (handlers.go:552).

---

## RPC-F17 dxGetOrderHistory ENCODING — CONFIRMED (all sub-claims)

**Fixed-8 vs shortest-roundtrip:** every dxGetOrderHistory row is serialized by
`uret()` = `json_spirit::write_string(o, json_spirit::none, 8)`
(`rpcxbridge.cpp:49-54`), which drives json_spirit's
`os_ << std::fixed << std::setprecision(precision_of_doubles_) << d` with
`precision_of_doubles_ = 8`
(`blocknet_core/src/json/json_spirit_writer_template.h:193-195,121-137`). So C++
emits OHLCV doubles as fixed-8 strings, e.g. price 0.5 → `0.50000000`, volume
1000 → `1000.00000000`. Go serializes via `xfloat.MarshalJSON` =
`strconv.FormatFloat(f, 'f', -1, 64)` + trailing ".0" guarantee
(`go-xbridge/api/handlers.go:24-32`), i.e. shortest-roundtrip: `0.5`, `1000.0`.

**Price computation:** C++ price comes from `ccy::Asset::Price<T>` which
computes the exact rational `to/from` and rounds to the nearest `1/basis`
(`basis = COIN = 1e6`, `blocknet_core/src/xbridge/currency.h:107-123`), so the
price is quantized to 1e-6. Go computes the raw float ratio `takerNum/makerNum`
(`handlers.go:541`).

**Granularity whitelist:** exists in C++ —
`xQuery::supported_seconds() = {{1*60, 5*60, 15*60, 1*60*60, 6*60*60, 24*60*60}}`
= `{60,300,900,3600,21600,86400}` (`blocknet_core/src/xbridge/util/xseries.h:119-121`),
enforced via `validate_granularity`/`reason` (`xseries.h:85,91-92,122-126`), error
surfaced at `rpcxbridge.cpp:671-672`. Absent in Go: the only check is
`granularity <= 0` (`go-xbridge/api/handlers.go:460-463`).

**end<=start:** C++ `period.is_null()` → reason `"Start time >= end time."`
(`xseries.h:94`) → returned as INVALID_PARAMETERS 1025 (`rpcxbridge.cpp:671-672`).
Go returns an empty array `[]interface{}{}` (`handlers.go:487-489`).

**Fills never written in production:** `Store.AddFill` is defined at
`go-xbridge/api/store.go:292` but the only production *reader* is
`Store.Fills()` (`store.go:300`; read at `handlers.go:72,506,1202`). The only
writers are unit tests (`api/handlers_test.go:595-599,657`). No swap/engine
code path records a fill, so production `dxGetOrderHistory` always aggregates an
empty `s.fills` and returns zero OHLCV rows.

Corrected statement: C++ emits fixed-8-decimal OHLCV doubles (uret precision 8,
rpcxbridge.cpp:51) with prices quantized to 1e-6 (currency.h:118-120) and
enforces the {60,300,900,3600,21600,86400} granularity whitelist plus a 1025
"Start time >= end time." error; Go emits shortest-roundtrip floats
(handlers.go:26-32), raw float ratios, accepts any granularity, returns `[]` on
end<=start, and — because Store.AddFill is only ever called from tests — always
returns an empty series.

---

## RPC-F20 dxGetOrderBook PRICE FORMULA — PARTIAL

**+1/COIN bump: CONFIRMED to exist, sample number WRONG.**
C++ `price()`/`priceBid()` (`blocknet_core/src/xbridge/util/xutil.cpp:293-312`)
call `xBridgeValueFromAmount(a) = a/COIN + 1.0/COIN` (`xutil.cpp:223-227`), so
both numerator and denominator carry a +1/COIN bump. Go computes a plain ratio
`p := float64(o.ToAmount)/float64(o.FromAmount)` (ask) /
`float64(o.FromAmount)/float64(o.ToAmount)` (bid)
(`go-xbridge/api/handlers.go:663-669`).

For the sample "amounts 1/3" (maker 3, taker 1):
- C++ = `1.000001/3.000001` = `0.333333555…` → `xBridgeStringValueFromPrice`
  6-decimal = **`0.333334`** (`xutil.cpp:209-214`).
- Go = `1/3 = 0.333333…` → `formatXPrice` = **`0.333333`** (`response.go:322-324`).

So the bump is real and does flip the rendered last digit, but the claimed
`"0.335548"` is fabricated — the correct pair is `0.333334` vs `0.333333`.

**Tie-break: CONFIRMED.**
C++ detail 1 picks the best entry with `std::max_element`/`std::min_element`
over `asksList`/`bidsList`, which are `std::map<uint256, TransactionDescrPtr>`
(`rpcxbridge.cpp:1578-1579,1654-1671,1704-1720`). `std::map` iterates ascending
by uint256, and max/min_element return the *first* extreme, so equal best prices
resolve to the **smallest order id**. Go sorts the collected slice with
unstable `sort.Slice(…, price > price)` (`handlers.go:675-676`) and takes
`asks[len-1]`/`bids[0]` (`handlers.go:688-695`); for equal prices the result is
sort/iteration-order dependent, not smallest-id.

Corrected statement: the +1/COIN bump exists on both C++ numerator and
denominator (xutil.cpp:223-227,293-312) making 1/3 render "0.333334" vs Go's
plain-ratio "0.333333" (the finding's "0.335548" is wrong), and C++ breaks
equal-price ties by smallest order id via map max_element/min_element
(rpcxbridge.cpp:1654-1671) while Go uses an unstable sort (handlers.go:675-676).

---

## RPC-F23/RPC-F24/RPC-F25 dxGetTokenBalances — PARTIAL

**(a) "Wallet" key derivation: REFUTED as stated (mapping reversed).**
C++ emits `"Wallet"` **first and always**, from the native core-wallet balance:
`double walletBalance = xbridge::availableBalance()/COIN` where
`availableBalance()` sums `wallet->GetBalance()` over all core (blocknetd)
wallets (`rpcxbridge.cpp:2531-2532`; `xbridgeapp.h:798-806`). The per-connector
balances are then appended separately (`rpcxbridge.cpp:2535-2566`). Go derives
`"Wallet"` from the **BLOCK connector** (`h.Node.cfg().Connectors["BLOCK"]`
ListUnspent, `go-xbridge/api/handlers.go:785-799`), falls back to the **first
exchange wallet** (`handlers.go:824-827`), and omits the key entirely when no
BLOCK connector and no exchange wallet exist (`handlers.go:830-832`). So:
C++ always returns `{"Wallet":…}`; Go returns `{}` when nothing is configured.
The finding's claimed direction (C++ = connector/wallet, Go = native-BLOCK) is
the opposite of the code.

**(b) Key order: CONFIRMED.** C++ inserts `"Wallet"` first, then connectors in
connector iteration order; json_spirit::Object preserves insertion order
(`rpcxbridge.cpp:2532,2556`). Go returns a `map[string]string` marshaled by
encoding/json, which sorts keys; `"Wallet"` (ASCII `W`) sorts after the
uppercase tickers, so it is **last** (`handlers.go:763,830-832`).

**(c) Precision: PARTIAL.** C++ per-connector balance =
`getWalletBalance` summing per-UTXO `double` native-unit amounts
(`xbridgewalletconnector.cpp:52-73`; amounts come already-native from
`listunspent` `value.get_real()`, `xbridgewalletconnectorbtc.cpp:558-560`),
rendered fixed-6 via `xBridgeStringValueFromPrice` (`rpcxbridge.cpp:2556`).
There is **no explicit "divide by 10^Decimals"** in C++ at this layer — the
values are native-unit doubles. Go accumulates native `uint64` amounts exactly
(`handlers.go:788-796,809-812`) and then does a single
`float64(native)/float64(10^Decimals)` + `FormatFloat('f',6)` in
`formatBalanceNative` (`response.go:253-263`), which deliberately reproduces
C++'s double→fixed-6 rendering. Outputs converge; only float-accumulation
edge cases can differ in the last digit.

Corrected statement: C++ always emits "Wallet" first from the core-wallet
availableBalance (rpcxbridge.cpp:2531-2532) while Go derives it from the BLOCK
connector or first exchange wallet and may omit it (handlers.go:785-832); key
order is insertion-order "Wallet"-first in C++ vs sorted-"Wallet"-last in Go;
C++ sums native-unit doubles per-UTXO (no explicit division) and Go sums exact
uint64 then divides once — both render fixed-6.

---

## RPC-F31/RPC-F32/RPC-F33/RPC-F34 dxGetLockedUtxos — PARTIAL

**(a) amount encoding: PARTIAL.** C++ `UtxoEntry::toString()` streams the
native-unit `double` `amount` with default `ostream` formatting
(`xbridgewalletconnector.cpp:25-30`), so 0.1 → `"0.1"` (shortest, 6 sig digits).
Go uses `formatXAmount` (fixed 6, `handlers.go:1077,1113,1132`) or, for any coin
known to the `coins` registry — which is populated from the same xbridge.conf
the connectors come from (`coins/coin.go:72-83`) — `coins.FormatAmount`, which
**trims** trailing zeros (`coins/amount.go:65-81`), so 0.1 → `"0.1"` matching C++.
The `"0.1" vs "0.100000"` divergence therefore only manifests for a coin absent
from the registry (practically never for configured coins); the mechanism
difference (default float vs fixed-6/trimmed) is real.

**(b) 1021 trigger: CONFIRMED.** C++ returns TRANSACTION_NOT_FOUND (1021) if
`Exchange::getUtxoItems` misses (`rpcxbridge.cpp:2637-2645`) AND again if neither
`pendingTx` nor `acceptedTx` is valid (a default-constructed Transaction has
`m_state=trInvalid` → `isValid()==false`, `xbridgetransaction.cpp:252-256`;
`rpcxbridge.cpp:2660-2670`). Go only errors when `Store.Get` misses
(`go-xbridge/api/handlers.go:1090-1093`, errTxNotFound=1021) — an order present
in the Store but without a valid pending/accepted transaction still returns UTXOs.

**(c) per-order key selection: CONFIRMED.** C++ keys by pending-vs-accepted map
membership: valid pending → `pendingTx->a_currency()`, else valid accepted →
`a_currency_and_b_currency` (`rpcxbridge.cpp:2674-2677`). Go keys by status
ordinal: `FromCurrency`, or `FromCurrency_and_ToCurrency` when
`stateOrdinal(o.Status) >= DescrAccepting` (`handlers.go:1100-1103`). These are
different selection mechanisms (map membership incl. the not-found/invalid case
vs a status threshold).

**(d) id echo: CONFIRMED.** C++ echoes `id.GetHex()` (lowercase-normalized,
`rpcxbridge.cpp:2672`); Go echoes the raw caller string
(`map[string]interface{}{"id": id}`, `handlers.go:1141`), so an uppercase input
is echoed as-is by Go but lowercased by C++.

Corrected statement: C++ UtxoEntry::toString uses default double formatting
(xbridgewalletconnector.cpp:25-30) vs Go fixed-6/native-trimmed
(handlers.go:1077-1081) — matching for registered coins, diverging otherwise;
the 1021 trigger and per-order key selection are map-membership/validity based in
C++ (rpcxbridge.cpp:2637-2645,2660-2677) vs Store presence + status ordinal in Go
(handlers.go:1090-1103); Go echoes the raw id instead of GetHex-normalized.

---

## RPC-F35/RPC-F36 dxFlushCancelledOrders — CONFIRMED

**(a) flush target:** C++ `App::flushCancelledOrders` erases trCancelled
entries older than keepTime from **both** `m_transactions` **and**
`m_historicTransactions` (`xbridgeapp.cpp:1336,1340-1351`). Go prunes only the
`Store.cancelled` ledger (`store.go:491-513`, populated by
`RecordCancelled` at `node.go:1729-1731`); it never removes the order from
`Store.orders`/`Store.history`.

**(b) use_count:** C++ records the live `shared_ptr` refcount
`ptr.use_count()` (`xbridgeapp.cpp:1345`; emitted `it.use_count`
`rpcxbridge.cpp:1485`). Go hardcodes `UseCount: 1` (`store.go:482`).

**(c) ordering + key order:** C++ result Object is built in fixed order
`ageMillis, now, durationMicrosec, flushedOrders` (`rpcxbridge.cpp:1474-1489`).
Go returns a `map[string]interface{}` marshaled with sorted keys →
`ageMillis, durationMicrosec, flushedOrders, now` (`handlers.go:1180-1185`), so
`now` moves from 2nd to 4th position. (Flushed entries are likewise id-ordered
in C++ — std::map ascending — vs insertion-ordered in Go.)

Corrected statement: C++ erases cancelled orders from both the live and
historic transaction maps (xbridgeapp.cpp:1336-1351) with a live shared_ptr
use_count (rpcxbridge.cpp:1485), whereas Go prunes only the Store.cancelled
ledger (store.go:491-513) with hardcoded use_count 1 (store.go:482), and emits
keys in alphabetical order (now last) instead of C++'s
ageMillis/now/durationMicrosec/flushedOrders.

---

## RPC-F37/RPC-F38/RPC-F39 gettradingdata / dxGetTradingData — CONFIRMED

**(a) gettradingdata registration:** registered in C++
(`rpcxbridge.cpp:3520`) and **absent** from the Go dispatch map
(`go-xbridge/api/dispatch.go:38-63`, which explicitly documents the omission).
Go's server returns `-32601 Method not found` for unknown methods
(`server.go:168-173`).

**(b) fee_txid/nodepubkey:** C++ fills `fee_txid` = block txid and
`nodepubkey` = service-node address extracted from the on-chain multisig/OP_RETURN
(`rpcxbridge.cpp:2891-2892`). Go hardcodes both to `""`
(`handlers.go:1207-1208`).

**(c) data source:** C++ scans the BLOCK chain from `chainActive.Tip()`
backwards, bounded by `countOfBlocks` (default **43200**, `rpcxbridge.cpp:2845,2862`)
and a **30-day** window `timeBegin-30*24*60*60` (`rpcxbridge.cpp:2860-2863`;
gettradingdata identical at 2732-2735), parsing `TxOutToCurrencyPair`
(`rpcxbridge.cpp:2877-2898`). Go has no chain scan; it reads only
`h.Store.Fills()` (which is never written in production, see RPC-F17)
(`handlers.go:1202-1215`).

Corrected statement: C++ registers both gettradingdata (rpcxbridge.cpp:3520) and
dxGetTradingData and scans up to 43200 blocks / 30 days of BLOCK history filling
on-chain fee_txid+nodepubkey (rpcxbridge.cpp:2860-2892), while Go omits
gettradingdata (dispatch.go:38-63 → -32601, server.go:173), hardcodes
fee_txid/nodepubkey to "" (handlers.go:1207-1208), and reads only the never-
written local fills ledger.

---

## RPC-F40/RPC-F41/RPC-F42/RPC-F43 dxSplitAddress / dxSplitInputs — PARTIAL

**(a) utxos schema: CONFIRMED.** C++ `dxSplitInputs` only needs `txid`+`vout`
per element (`COutPoint{uint256S(utxo["txid"].get_str()), utxo["vout"].get_int()}`
`rpcxbridge.cpp:3362-3367`). Go's `parseUtxoParam` unconditionally requires
`amount` (`coins.ParseAmount(c, x.Amount)`) and also reads scriptPubKey/address
(`go-xbridge/api/handlers.go:1627-1647`). For the C++-documented example
`[{"txid":"…","vout":0},…]` (no amount field), `ParseAmount(c,"")` returns
`coins: empty amount` (`coins/amount.go:12-16`) → Go returns
`{"code":1025,"name":"dxSplitInputs","error":"Invalid parameters: invalid utxo amount"}`
(`handlers.go:1640-1643`).

**(b) fee formula: CONFIRMED.** C++: `fee1 = minTxFee1(1,3)` = (192·1+34·3)·fpb =
294·fpb, `fee2 = minTxFee2(1,1)` = 226·fpb, `feesPerUtxo = fee1+fee2` = **520·feePerByte
per split output** added into `splitSize` when include_fees
(`xbridgewalletconnectorbtc.cpp:2665-2668`; fee formula `1949-1969`). Go adds a
single **`(192·nIn + 68)·FeePerByte`** (i.e. `estimateFee(cc, len(utxos), 2)`)
to `splitSize` (`handlers.go:1356-1362`; `fee_tx.go` comment / `estimateFee`
`handlers.go:1463-1475`).

**(c) change destination: CONFIRMED.** C++ sends both the split outputs and the
change vout to the requested `address` (`vouts.emplace_back(addr, splitSize)` /
`vouts.emplace_back(addr, change)`, `xbridgewalletconnectorbtc.cpp:2705,2711`).
Go sends change to a **fresh address** from `conn.GetNewAddress()`
(`handlers.go:1339-1343,1398-1400`).

**(d) include_fees default: REFUTED.** C++ `dxSplitAddress` defaults
`includeFees=true` when param[3] is null (`rpcxbridge.cpp:3251-3254`). Go
`dxSplitAddress` also defaults to `true` (`mustBool(params,3,true,…)`,
`handlers.go:1231`). `dxSplitInputs` has no default on either side (C++ reads
params[3..6] unconditionally and throws when absent; Go requires exactly 7
params, `handlers.go:1254`). No false-default divergence exists.

**(e) error names: CONFIRMED.** Go business errors use `"dxSplit"` (or `"dx"`
from `HandlerCtx.connector`, `handlers.go:200,204`) e.g.
`handlers.go:1304,1310,1318,1328,1332,1341,1368,1388,1405,1408,1422`; C++ uses
`__FUNCTION__` `"dxSplitAddress"`/`"dxSplitInputs"`
(`rpcxbridge.cpp:3264,3273,3278,3360,3372,3378,3387,3392`).

**(f) submit failure code: CONFIRMED.** C++ returns BAD_REQUEST **1004** on
`sendRawTransaction` failure (`rpcxbridge.cpp:3277-3278,3391-3392`); Go returns
errUnknown **1002** (`handlers.go:1421-1423`). (Go also uses 1002 where C++
uses 1004 for splitUtxos/getUnspent failures.)

Corrected statement: Go dxSplitInputs requires amount/scriptPubKey/address per
utxo and rejects the C++-documented txid+vout-only input with 1025 "invalid utxo
amount" (handlers.go:1627-1647) vs C++ needing only txid/vout
(rpcxbridge.cpp:3366); fee is 520·feePerByte per output in C++
(xbridgewalletconnectorbtc.cpp:2665-2668) vs one (192·nIn+68)·feePerByte in Go
(handlers.go:1362); C++ sends change to the requested address
(xbridgewalletconnectorbtc.cpp:2711) vs Go fresh address (handlers.go:1339);
include_fees defaults are true on both sides (refuted); error names differ
(dxSplit/dx vs dxSplitAddress/dxSplitInputs) and submit failure is 1002 vs 1004.

---

## RPC-F44/RPC-F45 dxGetUtxos — CONFIRMED

**(a) amount strings:** C++ renders fixed-width
`xBridgeStringValueFromPrice(utxo.amount, conn->COIN)` — 8 decimals for
COIN=1e8 (`xBridgeSignificantDigits(1e8)==8`, `rpcxbridge.cpp:3483`;
`xutil.cpp:209-221,263-274`) — so 1.0 → `"1.00000000"`. Go renders
`coins.FormatAmount` which **trims** trailing zeros (`coins/amount.go:65-81`,
`handlers.go:1702`) → `"1"`.

**(b) listunspent failure:** C++ returns BAD_REQUEST **1004**
`"failed to get unspent transaction outputs"` (`rpcxbridge.cpp:3475-3476`); Go
returns errUnknown **1002** `"Internal Server Error"` with the raw wallet error
(`handlers.go:1678-1681`, text at `response.go:113-114`).

**(c) NO_SESSION name:** C++ uses `__FUNCTION__` = `"dxGetUtxos"`
(`rpcxbridge.cpp:3471`); Go's no-connector error carries name `"dx"`
(`HandlerCtx.connector`, `handlers.go:200,204`).

**(d) include_used as string:** C++ `params[1].get_bool()` throws on a string
(`rpcxbridge.cpp:3465-3466`); Go `boolParam` **accepts** `"true"/"false"` string
forms (`dispatch.go:97-112`, used at `handlers.go:1663-1668`).

Corrected statement: C++ emits fixed-8 amount strings (rpcxbridge.cpp:3483) vs
Go trimmed (coins/amount.go:65-81); listunspent failure is 1004 "Bad Request
failed to get unspent transaction outputs" (rpcxbridge.cpp:3476) vs Go 1002
"Internal Server Error" (handlers.go:1680); the no-session error name is
"dxGetUtxos" vs Go "dx" (handlers.go:200); and Go tolerates string
include_used while C++ throws.

---

## RPC-F46 getnetworkinfo (Go shim vs real daemon RPC) — PARTIAL

Note: getnetworkinfo is **not an xbridge method** — it is the standard Bitcoin
Core RPC (`blocknet_core/src/rpc/net.cpp:758`) which the Go shim implements to
satisfy BLOCK-DX's getinfo() wrapper. All divergences below are therefore against
the real blocknetd RPC, not against XBridge.

**(a) protocolversion: REFUTED (values swapped in the finding).**
C++ `PROTOCOL_VERSION = 70713` (`blocknet_core/src/version.h:12`); the Go shim
hardcodes **70015** (`go-xbridge/api/handlers.go:1746`). The finding's mapping
("Go 70713 vs C++ 70015") is backwards.

**(b) relayfee / incrementalfee: CONFIRMED.**
C++ emits `ValueFromAmount(::minRelayTxFee.GetFeePerK())` =
`"0.00010000"` (DEFAULT_MIN_RELAY_TX_FEE=10000 sat/kB,
`validation.h:59`; `rpc/net.cpp:511`) and
`ValueFromAmount(::incrementalRelayFee.GetFeePerK())` = `"0.00001000"`
(DEFAULT_INCREMENTAL_RELAY_FEE=1000, `policy/policy.h:34`; `rpc/net.cpp:512`);
`ValueFromAmount` produces fixed 8-decimal strings
(`core_write.cpp:19-27`). Go emits `relayfee: 0.00001` and
`incrementalfee: 0.00000001` as bare numbers (`handlers.go:1757-1758`) — 10× and
1000× different magnitudes respectively (not just width).

**(c) xbridge/xrouter protocolversion fields: CONFIRMED.**
C++ adds `xbridgeprotocolversion = XBRIDGE_PROTOCOL_VERSION = 55`
(`rpc/net.cpp:500`; `xbridge/version.h:8`) and
`xrouterprotocolversion = XROUTER_PROTOCOL_VERSION = 50`
(`rpc/net.cpp:501`; `xrouter/version.h:8`). The Go shim emits neither
(`handlers.go:1743-1761`).

**(d) subversion case: CONFIRMED.**
C++ `strSubVersion = FormatSubVersion(CLIENT_NAME="Blocknet", …)`
(`clientversion.cpp:15`; `init.cpp:1412`) → `/Blocknet:4.4.1/`. Go default
`sub = "/blocknet:4.4.1/"` (lowercase `b`, `handlers.go:1729-1732`).

**(e) networks missing proxy_randomize_credentials: CONFIRMED.**
C++ `GetNetworksInfo()` adds `proxy_randomize_credentials` per network
(`rpc/net.cpp:426-445,441`); Go network entries only carry
name/limited/reachable/proxy (`handlers.go:1752-1756`).

Corrected statement: the Go shim's protocolversion is 70015 vs C++ 70713 (the
finding had them reversed; version.h:12 / handlers.go:1746); relayfee differs
10× (Go 0.00001 vs C++ "0.00010000") and incrementalfee 1000× (Go 0.00000001 vs
C++ "0.00001000"); xbridgeprotocolversion(55)/xrouterprotocolversion(50) are
missing in Go; subversion is lowercase "/blocknet:4.4.1/" vs "/Blocknet:4.4.1/";
and networks entries lack proxy_randomize_credentials — all vs the real daemon
RPC (rpc/net.cpp:447-528), since getnetworkinfo is not an XBridge method.

---

## Summary

| F   | Verdict  | Key correction |
|-----|----------|----------------|
| RPC-F16  | CONFIRMED | — |
| RPC-F17 | CONFIRMED | — |
| RPC-F20 | PARTIAL  | sample "0.335548" → correct "0.333334" vs "0.333333" |
| RPC-F23 | PARTIAL  | (a) mapping reversed; C++ Wallet is always-present core-wallet, Go is BLOCK-connector/fallback, omitted when none |
| RPC-F31 | PARTIAL  | (a) amount divergence only for coins absent from registry |
| RPC-F35 | CONFIRMED | — |
| RPC-F37 | CONFIRMED | — |
| RPC-F40 | PARTIAL  | (d) include_fees default true on both sides (refuted) |
| RPC-F44 | CONFIRMED | — |
| RPC-F46 | PARTIAL  | (a) protocolversion values swapped — C++ 70713, Go 70015 |

Findings whose claims were substantially corrected: **RPC-F20** (sample value),
**RPC-F23(a)** (Wallet-key mapping reversed), **RPC-F31(a)** (divergence is conditional),
**RPC-F40(d)** (defaults match — refuted), **RPC-F46(a)** (protocolversion swapped).
