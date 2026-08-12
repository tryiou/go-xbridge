# CONFIGURATION & CLI CONFORMANCE AUDIT — C++ reference vs go-xbridge

**Reference (source of truth):** `blocknet_core/src/xbridge/` — trust code, not comments.
**Candidate:** `go-xbridge/`.
**Date:** 2026-08-12.

Every claim below cites `file:line` on **both** sides. Verdict markers:
- **MATCH** — behavior identical.
- **DIFF** — behavioral difference (a dApp could observe it).
- **GO-ONLY** — key/flag accepted by Go but never read by C++ (accepting a key the reference ignores is itself a behavioral surface).
- **CPP-DEAD** — key/flag has accessors in C++ but **no caller anywhere in `src/`** (verified by grep).
- **TBD** — could not be fully resolved.

---

## 0. Ground truth: how C++ reads keys

`Settings::get<T>` (`settings.h:68-90`) throws on a missing/invalid key, falls back to the
passed default, and then **calls `set()` (`settings.h:92-106`), which `m_pt.put`s the default
and `write()`s the file back** (`settings.cpp:86-111`). Consequences:

1. C++ **mutates `xbridge.conf` at runtime**: every missing key is backfilled with its default
   on first access (first `updateActiveWallets`, `xbridgeapp.cpp:917`). Go is strictly read-only.
2. C++'s INI section **names and keys are case-sensitive** (boost property_tree). Go's are
   case-insensitive (`config/conf.go:112` for `Main`, `config/conf.go:167-174` for keys).
3. `Settings::read()` (`settings.cpp:58-82`) swallows parse exceptions and returns `false`; the
   caller (`App::init()`, `xbridgeapp.cpp:427-451`) ignores the return value. A malformed file
   does **not** stop C++ (may leave `m_pt` partially populated).

---

## 1. CARD 1 — GLOBAL `[Main]` section and `[Rpc]` section

| Key | C++ reads | Go reads | C++ type / default | Go type / default | Verdict |
|---|---|---|---|---|---|
| `Main.ExchangeWallets` | yes — `settings.cpp:143-166` (split `,;:`; each token validated via `ccy::Symbol::validate` → uppercase, len 1..8, invalid dropped; `currency.h:25-47`) | yes — `config/conf.go:234-243` (split **`,` only**; trim; **no validation, no uppercase**) | `std::string` → `[]string`, default empty | `[]string`, default empty | **DIFF** (separators + validation + case normalization) |
| `Main.ShowAllOrders` | yes — `settings.h:38`, default `false` | yes — `config/conf.go:246`, default `false` | `bool`, false | `bool`, false | MATCH |
| `Main.FullLog` | yes — `settings.h:36-37`, default `false`; **used** at `xbridgeapp.cpp:4000`, `xbridgetransaction.cpp:603` | yes — parsed `config/conf.go:247` but **never used** anywhere in Go logic (only `README.md:63`, tests) | `bool`, false (createConf template writes `FullLog=true`, `xbridgeapp.cpp:325`) | `bool`, false | **DIFF** (Go parses then ignores) |
| `Main.Peers` | **CPP-DEAD** — accessor `settings.h:45`, split `,;` `settings.cpp:125-139`; `m_peers` never populated; **zero callers** in `src/` | not parsed at all | `[]string` | n/a | **DIFF** (neither side acts on it; C++ still "accepts" it) |
| `Rpc.Enable` | **CPP-DEAD** — `settings.h:49-50` | not parsed | `bool`, default false | n/a | see below |
| `Rpc.Port` | **CPP-DEAD** — `settings.h:51-52` | not parsed | `uint32_t`, caller-supplied default | n/a | see below |
| `Rpc.UserName` | **CPP-DEAD** — `settings.h:54-55` | not parsed | `std::string`, default `""` | n/a | see below |
| `Rpc.Password` | **CPP-DEAD** — `settings.h:56-57` | not parsed | `std::string` | n/a | see below |
| `Rpc.UseSSL` | **CPP-DEAD** — `settings.h:58-59` | not parsed | `bool`, default false | n/a | see below |
| `Rpc.SertFile` | **CPP-DEAD** — `settings.h:60-61` | not parsed | `std::string` | n/a | see below |
| `Rpc.PKeyFile` | **CPP-DEAD** — `settings.h:62-63` | not parsed | `std::string` | n/a | see below |
| `Rpc.SslCiphers` | **CPP-DEAD** — `settings.h:64-65` | not parsed | `std::string` | n/a | see below |

### [Rpc] analysis — the notable difference is Go's side effect
- The eight `Rpc.*` accessors in `settings.h:49-65` have **no caller anywhere in `src/`**
  (grep for `rpcEnabled()/rpcServerUserName()/rpcPort(/rpcUseSsl()` finds only the
  declarations). The `[Rpc]` section is effectively **ignored by C++** — and the real
  blocknetd RPC server is driven by the core `-rpcbind/-rpcport/-rpcuser/-rpcpassword/
  -rpcallowip` args (`init.cpp:552-559`, loopback default `httpserver.cpp:308-310`,
  mainnet port `41414` `chainparamsbase.cpp:37`), not by `[Rpc]`.
- Go never reads `[Rpc]` either, and instead authenticates its own JSON-RPC server with
  `-rpcbind/-rpcuser/-rpcpassword` flags (`cmd/xbridged/main.go:91-96`, auth gate
  `main.go:218-225`; bind default `127.0.0.1:41414`, matching the C++ core port).
- **GO-ONLY / startup hazard:** `config.Load` treats **every non-`[Main]` section as a coin**
  (`config/conf.go:111-118`). A `[Rpc]` section (harmless to C++, and present in the schema)
  becomes a `CoinConf` ticker `"Rpc"` with `Coin==0`, so `coins.InitFromConf` fails
  (`coins/coin.go:87-89`) and **`xbridged` exits** (`cmd/xbridged/main.go:163-165`). A valid
  C++ config containing `[Rpc]` **kills the Go daemon at startup**.
- **DIFF — case sensitivity:** C++ requires the exact section name `Main` and exact key
  spelling (case-sensitive ptree); Go `EqualFold`s section names (`conf.go:112`) and keys
  (`conf.go:169`). `[main]` or `FULLLOG` work in Go, are ignored by C++.
- **DIFF — bare keys:** Go `parseINI` errors on a `key=value` outside any `[section]`
  (`config/conf.go:147-149`) → startup fatal; boost ini_parser also throws but C++ catches
  (`settings.cpp:75-79`) and keeps running.

---

## 2. CARD 2 — PER-`[TICKER]` key table

C++ read sites: `xbridgeapp.cpp:974-997` (app connectors) and `xbridgeexchange.cpp:118-128`
(exchange wallets, only when `-enableexchange`). Go read sites: `config/conf.go:251-282`
(parse) plus consumers. Defaults: C++ `WalletParam` ctor `xbridgewallet.h:110-126`.

| Key | C++ reads | Go reads | Type C++ / Go | Default C++ | Default Go | Units | Verdict |
|---|---|---|---|---|---|---|---|
| `Title` | yes `xbridgeapp.cpp:976`; also `xbridgeexchange.cpp:118` | yes `conf.go:255` | str / str | `""` | **section name** (`s.str("Title", name)`) | — | **DIFF** (default) |
| `Address` | yes `xbridgeapp.cpp:977`; `xbridgeexchange.cpp:119` | yes `conf.go:256` | str / str | `""` | `""` | — | MATCH |
| `Ip` | yes `xbridgeapp.cpp:978`; `xbridgeexchange.cpp:120` | yes `conf.go:257` | str / str | `""` | `""` | — | MATCH |
| `Port` | yes `xbridgeapp.cpp:979`; `xbridgeexchange.cpp:121` — stored as `std::string`, `lexical_cast<int>` at RPC time `xbridgewalletconnectorbtc.h:117` | yes `conf.go:258` | str / int | `""` | `0` | — | **DIFF** (type + missing/garble handling: C++ throws `bad_lexical_cast` at call time; Go `intp` → 0 → rejected in `wallet/conf.go:15`) |
| `Username` | yes `xbridgeapp.cpp:980`; `xbridgeexchange.cpp:122` | yes `conf.go:259` | str / str | `""` | `""` | — | MATCH |
| `Password` | yes `xbridgeapp.cpp:981`; `xbridgeexchange.cpp:123` | yes `conf.go:260` | str / str | `""` | `""` | — | MATCH |
| `AddressPrefix` | yes `xbridgeapp.cpp:982` — read as `std::string`, converted to single byte via `static_cast<char>(lexical_cast<int>(...))` in connector `init()` `xbridgewalletconnectorbtc.cpp:1516` | yes `conf.go:262` — `int` → `byte()` `coins/coin.go:94` | str→char / int | `""` (single NUL char after ctor `xbridgewallet.h:123`) | `0` | base58check version byte | **DIFF** (missing value: C++ `lexical_cast<int>("")` throws inside connector init; Go silently `byte(0)`; note xrouter reads the same key as `int`, `xrouterserver.cpp:68-70`) |
| `ScriptPrefix` | yes `xbridgeapp.cpp:983`; conversion `xbridgewalletconnectorbtc.cpp:1517` | yes `conf.go:263` | str→char / int | `""` | `0` | base58check version byte | **DIFF** (same as AddressPrefix) |
| `SecretPrefix` | yes `xbridgeapp.cpp:984`; conversion `xbridgewalletconnectorbtc.cpp:1518` | yes `conf.go:264` | str→char / int | `""` | `0` | WIF version byte | **DIFF** (same as AddressPrefix) |
| `COIN` | yes `xbridgeapp.cpp:985` | yes `conf.go:265` | `uint64_t` / `uint64` | `0` | `0` | base units of `COIN` | MATCH (both drop coin if 0: C++ `xbridgeapp.cpp:1002-1006`, Go `coins/coin.go:87-89`); note Go derives decimals from trailing zeros `coin.go:130-141` |
| `TxVersion` | yes `xbridgeapp.cpp:986`; `xbridgeexchange.cpp:125` | yes `conf.go:267` | `uint32_t` / int | `1` | `1` | tx version int | MATCH |
| `MinTxFee` | yes `xbridgeapp.cpp:987` | yes `conf.go:269`, used `api/handlers.go:1478-1479` | `uint64_t` / `uint64` | `0` | `0` | satoshis | MATCH |
| `FeePerByte` | yes `xbridgeapp.cpp:988` | yes `conf.go:271`, used `api/handlers.go:1470-1474` | `uint64_t` / `uint64` | `0` | `0` | sat/byte | MATCH |
| `CreateTxMethod` | yes `xbridgeapp.cpp:989`; selects connector class `xbridgeapp.cpp:1043-1090` (BTC/SYS, BCD, BCH, DGB, BTG, DEVAULT, STEALTH/XST, PART; **ETH/ETHER/ETHEREUM rejected** `:1043-1046`; unknown → drop `:1092-1098`) | yes `conf.go:261`; selects **family** `coins/coin.go:107-113` (BCH vs BTC-family; **unknown/ETH methods silently map to utxo-btc**, never rejected) | str / str | `""` | `""` | method string | **DIFF** (Go never rejects ETH/unknown methods; C++ refuses them. Go also hardcodes per-method segwit/HRP tables `coin.go:147-170`) |
| `BlockTime` | yes `xbridgeapp.cpp:990` | yes `conf.go:270`, used `api/locktime.go:45-46` (default 60 at runtime), `api/swap.go:1004-1005` | `int` / int | `0` | `0` | seconds per block | **DIFF** (both gate `==0` at load, but Go re-defaults to 60 at use sites — see Card 3) |
| `Confirmations` | yes `xbridgeapp.cpp:992` | yes `conf.go:272`, used `api/handlers.go:1324,1676`, `api/node.go:1014`, `wallet/rpc.go:250` | `int` / int | `0` | `0` | confirmations | MATCH (read; **Gate** diverges — Card 3) |
| `TxWithTimeField` | yes `xbridgeapp.cpp:993` | yes `conf.go:273`, carried on `Coin` `coins/coin.go:100` | `bool` / bool | `false` | `false` | — | MATCH |
| `LockCoinsSupported` | yes `xbridgeapp.cpp:994` | yes `conf.go:274` (parsed; no consumer found) | `bool` / bool | `false` | `false` | — | **DIFF** (Go parses but nothing consumes it) |
| `JSONVersion` | yes `xbridgeapp.cpp:995`; `xbridgeexchange.cpp:126`; empty → `jsonrpc` field omitted `xbridgewalletconnectorbtc.h:103-104` | yes `conf.go:278`; empty → omitted via `omitempty` `wallet/rpc.go:93-97` | str / str | `""` | `""` | RPC version string | MATCH |
| `ContentType` | yes `xbridgeapp.cpp:996`; `xbridgeexchange.cpp:127`; empty → header unset `xbridgewalletconnectorbtc.h:139-140` | yes `conf.go:279`; same `wallet/rpc.go:109-111` | str / str | `""` | `""` | Content-Type | MATCH |
| `CashAddrPrefix` | yes `xbridgeapp.cpp:997`; `xbridgeexchange.cpp:128`; **used** by BCH connector `xbridgewalletconnectorbch.cpp:328-358` | **parsed `conf.go:277` but the value is IGNORED** — `Coin.CashAddrPrefix` is derived from `CreateTxMethod` (hardcoded `"bitcoincash"` for BCH) `coins/coin.go:99,119-126` | str / str | `""` | `""` | cashaddr HRP | **DIFF** (Go accepts the key, ignores its value, substitutes a method-derived constant) |
| `MinimumAmount` | yes — **exchange path only** `xbridgeexchange.cpp:124` (sets `dustAmount`, `:145`); not read by `updateActiveWallets` | parsed `conf.go:266` but **never used** | `uint64_t` / `uint64` | `0` | `0` | base units | **DIFF** (C++ consumes it as exchange dust; Go parses-and-ignores) |
| `DustAmount` | **never read** — appears only in the createConf template comment `xbridgeapp.cpp:345`; C++ computes dust live from relayfee (`xbridgewalletconnectorbtc.cpp:1526`) | **GO-ONLY** — parsed `conf.go:268`, **used** `api/handlers.go:1453-1454`, `api/utxo_select.go:62` (5460 fallback) | n/a / `uint64` | n/a | `0` | base units | **GO-ONLY + USED** — a key C++ never accepts but Go honors (dApp can tune dust in Go, not in C++) |
| `GetNewKeySupported` | **never read** — template comment only `xbridgeapp.cpp:347` | parsed `conf.go:275`; **no consumer** | n/a / bool | n/a | `false` | — | GO-ONLY (parsed, inert) |
| `ImportWithNoScanSupported` | **never read** — template comment only `xbridgeapp.cpp:348` | parsed `conf.go:276`; **no consumer** | n/a / bool | n/a | `false` | — | GO-ONLY (parsed, inert) |
| `OmitJSONVersion` | **never read** — no such key in C++ | **GO-ONLY — used** `conf.go:280`, `wallet/conf.go:32`, `wallet/rpc.go:94-96` (drops `jsonrpc` even when `JSONVersion` set; C++'s empty-`JSONVersion` behavior is otherwise replicated) | n/a / bool | n/a | `false` | — | GO-ONLY + USED |

### Card-2 summary of flagged differences
1. **`DustAmount`, `GetNewKeySupported`, `ImportWithNoScanSupported`, `OmitJSONVersion` are Go-only** — C++ never reads any of them (`GetNewKeySupported`/`ImportWithNoScanSupported` exist only in the createConf template *comments*, `xbridgeapp.cpp:347-348`). `DustAmount` and `OmitJSONVersion` are actively consumed by Go, so a dApp gets behavior in Go that stock XBridge never had.
2. **`CashAddrPrefix` value ignored in Go** (`coins/coin.go:99`), replaced by method-derived `"bitcoincash"`.
3. **`MinimumAmount` consumed by C++ exchange but ignored by Go.**
4. **Prefix type mismatch** (`std::string`+`lexical_cast` in C++ vs `int` in Go) with different failure modes on missing/garbage values.
5. **`Port` string vs int** — same end behavior on valid input, different on garbage.
6. **`Title` default** differs (`""` vs section name).
7. **`CreateTxMethod`** — Go never rejects ETH/unknown methods; C++ drops them.
8. **`LockCoinsSupported`, `FullLog`** parsed by Go but never used.

---

## 3. CARD 3 — VALIDATION GATES (wallet acceptance)

C++ gates live in `updateActiveWallets` (`xbridgeapp.cpp:917-1223`); a violated gate **drops
the wallet** (`removeConnector` + `continue`). Constants at `xbridgewallet.h:96-102`:
`XMIN_LOCKTIME_BLOCKS=6`, `XMAX_LOCKTIME_DRIFT_BLOCKS=4`,
`XMAKER_LOCKTIME_TARGET_SECONDS=7200`, `XTAKER_LOCKTIME_TARGET_SECONDS=1800`,
`XSLOW_TAKER_LOCKTIME_TARGET_SECONDS=3600`, `XSLOW_BLOCKTIME_SECONDS=600`,
`XLOCKTIME_DRIFT_SECONDS=900`.

| Gate | C++ (drops wallet) | Go | Verdict |
|---|---|---|---|
| Missing `Ip`/`Port`, `COIN==0`, `BlockTime==0` | yes — `xbridgeapp.cpp:1002-1006` | partial — `wallet/conf.go:15` requires `Ip`/`Port`; `coins/coin.go:87-89` requires `COIN`; **no BlockTime gate** | **DIFF** — Go loads a coin with `BlockTime==0` (uses fallback 60 at runtime, `api/locktime.go:44-48`); C++ drops it |
| Empty credentials (`Username`/`Password`) | **WARN only, wallet kept** — `xbridgeapp.cpp:999-1000` (also `xbridgeexchange.cpp:130-131`) | no check | MATCH (neither drops) |
| Maker locktime: `blockTime*6 > 7200` | yes — `xbridgeapp.cpp:1008-1013` | **absent** (Go only has per-swap locktime *drift* checks, `api/locktime.go:29-39`, `api/swap.go:28-30`) | **DIFF** — Go never rejects a maker-violating coin at load |
| Taker locktime (non-slow): `blockTime<600 && blockTime*6 > 1800` | yes — `xbridgeapp.cpp:1014-1019` | absent | **DIFF** |
| Slow-taker locktime: `blockTime>=600 && blockTime*6 > 3600` | yes — `xbridgeapp.cpp:1020-1027` | absent | **DIFF** |
| Confirmation drift: `requiredConfirmations > max(900/blockTime, 4)` | yes — `xbridgeapp.cpp:1029-1035` | absent | **DIFF** — Go passes `Confirmations` straight to `ListUnspent` minconf (`wallet/rpc.go:250`, `api/handlers.go:1324`) with no ceiling |
| `blockSize < 1024` floor | `xbridgeapp.cpp:1037-1040` — **dead code**: `blockSize` is never set from conf (read commented out `:991`, ctor default 1024 `xbridgewallet.h:117`) | n/a | MATCH in effect (neither side reads a `BlockSize` key; C++ floor can never trigger) |
| Unknown `CreateTxMethod` / ETH | yes — `xbridgeapp.cpp:1043-1090` | **no** — `coins/coin.go:107-113` maps everything non-BCH to utxo-btc | **DIFF** |
| Live connection check (`conn->init()`) + 5-min bad-wallet cooldown | yes — `xbridgeapp.cpp:1125-1212` (async, `-rpcthreads` bound) | no equivalent (Go builds a stateless RPC connector) | **DIFF** (design; Go never probes the wallet at load) |

Card-3 summary: **Go replicates only the Ip/Port/COIN gate.** Every locktime-target and
confirmation-drift gate C++ enforces at load time is absent in Go. (Go does implement the
*same constants* for per-swap locktime **drift** validation — `api/locktime.go:11-15`,
`api/swap.go:28-30` — but that is a runtime handshake check, not the config-time wallet
admission gate.) A coin C++ refuses to load will happily load and trade in Go.

---

## 4. CARD 4 — FILE LIFECYCLE

| Aspect | C++ | Go | Verdict |
|---|---|---|---|
| Creates file when absent | **yes** — `App::createConf()` writes the template `xbridgeapp.cpp:310-366`, invoked from `init.cpp:1920` before `xapp.init()`. Template `[Main]`: `ExchangeWallets=`, `FullLog=true`, `ShowAllOrders=false` plus a commented sample `[BLOCK]` block (`xbridgeapp.cpp:323-356`) | **never** — `config.Load` only `os.Open`s (`config/conf.go:98-103`; header `conf.go:1-15`) | **DIFF** |
| Missing file → daemon start | starts fine; `createConf` writes template, `loadSettings` parses it → empty wallet list, no error (`xbridgeapp.cpp:502-517`; return ignored by `App::init`) | **exits(1)** — `config.Load` error → `fatalf` (`cmd/xbridged/main.go:159-162`) | **DIFF** — missing `xbridge.conf` kills Go, is recovered by C++ |
| Missing file → `ExchangeWallets`/`COIN` | empty list → zero connectors; no coin to error on | process dies before any config is available | n/a |
| Parse-error / malformed INI at startup | caught — `Settings::read` returns `false` (`settings.cpp:75-79`); daemon continues (possibly partial `m_pt`) | `parseINI` error → `config.Load` error → `fatalf` (`cmd/xbridged/main.go:159-162`) | **DIFF** — C++ survives, Go exits |
| Parse-error INI at reload | `dxLoadXBridgeConf` returns `false` (RPC error), daemon keeps running with **partial/previous** ptree state (`rpcxbridge.cpp:229-234`; `loadSettings` `xbridgeapp.cpp:502-517`) | `reloadConf` returns error, **last-good config retained** (`api/node.go:287-334`, documented last-good) | **DIFF** — C++ = possibly-partial; Go = last-good |
| Mutates the file at runtime | **yes** — every missing-key `get<T>` backfills the default and `write()`s it back (`settings.h:68-90,92-106`), and `createConf` rewrites the template | **never** — read-only | **DIFF** |
| Runtime write trigger | first `updateActiveWallets` on missing keys; `settings().get` callers throughout | n/a | **DIFF** |

---

## 5. CARD 5 — CLI FLAGS / ENV

### C++ command-line flags relevant to XBridge (declared `init.cpp:567-573`, category XBRIDGE)

| C++ flag | Declared | Used at | Go equivalent |
|---|---|---|---|
| `-enableexchange` | `init.cpp:569` | `settings.cpp:45`, `xbridgeexchange.cpp:162`, `xbridgesession.cpp:188` (gates exchange/hub mode) | **none** — Go is a thin client; `exchangeStarted` is a test-only toggle defaulting false (`api/node.go:189-195`) |
| `-dxnowallets` | `init.cpp:572` | `xbridgeapp.cpp:372` (isEnabled), `rpcxbridge.cpp:432`, `xbridgesession.cpp:744` — default `settings().showAllOrders()`, i.e. overrides `Main.ShowAllOrders` | **none** — Go honors only `Main.ShowAllOrders` (`api/handlers.go:120`); no CLI override |
| `-servicenode` | `init.cpp:568` | snode registration | **none** |
| `-orderinputscheck` | `init.cpp:570` (900s) | `xbridgeexchange.cpp:836` | none |
| `-maxmempoolxbridge` | `init.cpp:571` (128MB) | `xbridgeapp.cpp:3806` | none |
| `-rpcxbridgetimeout` | `init.cpp:573` (120s) | `xbridgewalletconnectorbtc.h:124` (wallet RPC timeout) | **none** — Go wallet RPC timeout is a constant 30s (`wallet/rpc.go:61,71-75`) |
| `-rpcthreads` (core) | RPC category | `xbridgeapp.cpp:1118` | none |

### Go `xbridged` flags (`cmd/xbridged/main.go:91-106`)

| Go flag | Default | C++ equivalent | Verdict |
|---|---|---|---|
| `-rpcbind` | `127.0.0.1:41414` (`main.go:91`; matches core mainnet RPC port `chainparamsbase.cpp:37`, loopback default `httpserver.cpp:308`) | core `-rpcbind` (`init.cpp:552`); not the xbridge `[Rpc]` section | MATCH in spirit; **not** the `[Rpc]` section either side honors |
| `-rpcuser` / `-rpcpassword` | `""` (`main.go:95-96`) | core `-rpcuser/-rpcpassword` (`init.cpp:554,559`) | MATCH in spirit (Go: Basic auth only when both set, `main.go:218-225`) |
| `-network` | `mainnet` (`main.go:97`) | core `-chain`/testnet selection | **no exact equivalent** (Go-only conveniences) |
| `-node` | `""` (`main.go:98`) | n/a (core always connects to P2P) | Go-only |
| `-addnode` | `""` (`main.go:99`) | core `-addnode` | matches core arg name; Go-only within xbridge surface |
| `-conf` | `~/.blocknet/xbridge.conf` (`main.go:45-51`) | implicit: `GetDataDir(false)/xbridge.conf` (`xbridgeapp.cpp:508`) | MATCH (default path `~/.blocknet` for both) |
| `-magic` | derived from `-network` (`main.go:101,140-156`) | none | Go-only |
| `-walletversion` | `4040100` (`main.go:102`) | **not a flag** — `CLIENT_VERSION` compile-time `4.4.1.0` (`configure.ac:3-7`, `clientversion.h:38-42` = 4040100) | value MATCHes C++ 4.4.1 |
| `-walletversionstr` | `"/blocknet:4.4.1/"` lowercase (`main.go:103`) | **not a flag** — C++ derives `"/Blocknet:4.4.1/"` from `CLIENT_NAME("Blocknet")` (`clientversion.cpp:15`), `FormatVersion` (`clientversion.cpp:71-77`), `FormatSubVersion` (`clientversion.cpp:87-101`) | **DIFF — case**: `blocknet` vs `Blocknet` |
| `-loglevel` | `info` (`main.go:104`) | core log level args | Go-only |
| `-datadir` | OS config dir + `/xbridged` (`main.go:58-67,105`) | core `-datadir` controls `GetDataDir` (and thus conf path) | **DIFF** — Go `-datadir` is only for swap state; it does **not** relocate the conf search path (that is `-conf`), unlike C++ where conf always lives under the data dir |
| `-logfile` | `<datadir>/xbridged.log` (`main.go:106`) | core `-debuglogfile` | Go-only |

### Environment variables
- **Neither side reads any env var.** Go: zero `os.Getenv` matches in `go-xbridge/`.
  C++: zero `getenv` matches in `blocknet_core/src/xbridge/`. MATCH.

---

## 6. CARD 6 — HOT-RELOAD (`dxLoadXBridgeConf`)

### C++ (`rpcxbridge.cpp:195-235`)
1. Rejects when `ShutdownRequested` or `isUpdatingWallets()` (`:222-227`).
2. `loadSettings()` re-reads the file into `m_pt` (`:229` → `xbridgeapp.cpp:502-517`).
3. `clearBadWallets()` clears the 5-min cooldown (`:230`).
4. `updateActiveWallets()` (`:231` → `xbridgeapp.cpp:917-1223`):
   - computes `wallets = exchangeWallets()` (`:929`),
   - **removes** connectors not in the new list (`:931-948`),
   - re-reads every per-ticker key and **re-runs all Card-3 gates**, dropping violators
     (`:974-1098`),
   - async re-checks each wallet's live connection with the 5-min bad-wallet cooldown
     (`:1103-1212`),
   - pushes the surviving set into the Exchange via `loadWallets(validWallets)` (`:1217`).
5. If `!showAllOrders` → `clearNonLocalOrders()` (`:232-233`).

C++ **also** re-runs `updateActiveWallets` periodically on the 15s timer every other tick
(~30s) — `xbridgeapp.cpp:3674-3677` — but that path only re-runs connection checks against the
**already-parsed** `m_pt`; it does **not** re-read the file (no `loadSettings`).

### Go (`api/node.go:287-334`, handler `api/handlers.go:213-221`)
1. Errors out if `ConfPath == ""` (`node.go:289-291`) — C++ has no such condition (conf path
   always exists).
2. `config.Load` + `coins.InitFromConf` (last-good on failure, `coins/coin.go:72-83`) (`:292-298`).
3. Rebuilds connectors from **every `[TICKER]` section** (`:299-307`), not from
   `ExchangeWallets`; skips sections failing `wallet/conf.go:15` (Ip/Port) or `coins/coin.go:87`
   (COIN). **No Card-3 gates.**
4. Swaps the whole `*Config` under the `cfgMu` write lock (`:329-331`) — atomic pointer
   replacement (registry `atomic.Pointer` too, `coins/coin.go:61`).

### Comparison — what is/ isn't swapped

| Item | C++ | Go |
|---|---|---|
| Re-reads file | yes (`loadSettings`, `:229`) | yes (`config.Load`, `:292`) |
| Rebuilds connectors | yes, from `ExchangeWallets` only (`:929`) | yes, from **all sections** (`:299-307`) |
| Drops connectors absent from `ExchangeWallets` | **yes** (`:931-948`) | **no** — Go never keys connectors off `ExchangeWallets` |
| Applies locktime/confirmation admission gates | **yes** (`:1008-1035`) | **no** |
| Re-runs live connection probe | yes + 5-min cooldown (`:1103-1212`) | no |
| Updates exchange wallet set | yes (`loadWallets(validWallets)`, `:1217`) | n/a (no exchange role) |
| Clears non-local orders when `ShowAllOrders` off | **yes** (`:232-233`) | **no** |
| Failure semantics | partial-state, RPC returns error, daemon runs | last-good, RPC error, daemon runs |
| In-flight swap / order book | untouched in both | untouched in both |
| Concurrency safety | per-connector add/remove under `m_connectorsLock`; window where both old+new exist | atomic pointer swap; readers see old-or-new snapshot |

### dApp-observable consequence
- C++ `dxLoadXBridgeConf` **drops** a wallet whose new conf violates a gate (e.g. a coin with
  `BlockTime` raised so `blockTime*6 > 7200`) or is removed from `ExchangeWallets`. Go
  `dxLoadXBridgeConf` keeps **every** section that passes only Ip/Port/COIN, gate-free, and
  ignores `ExchangeWallets` membership for the connector set. Conversely a dApp that removes a
  `[TICKER]` from `ExchangeWallets` in C++ loses the connector; in Go the connector survives as
  long as the section remains.
- Go additionally requires the daemon to have been started with `-conf` (`node.go:289-291`);
  C++ has no analog (the conf path always resolves).

---

## CANDIDATE FINDINGS (Go-vs-reference gaps, one line each)

- **axis=Card1** — `Main.ExchangeWallets` parsing: Go splits only `,` and does no
  `Symbol::validate` uppercase/length-1..8 filtering (C++ splits `,;:`, drops invalid, uppercases;
  `settings.cpp:143-166` vs `conf.go:234-243`).
- **axis=Card1** — `Main.FullLog` parsed by Go but never consumed; C++ gates verbose logging on it
  (`xbridgeapp.cpp:4000`, `xbridgetransaction.cpp:603`).
- **axis=Card1** — `[Rpc]` section: ignored by C++ (accessors dead, zero callers) but Go
  misparses it as a coin section → `COIN not set` → **xbridged exits at startup**
  (`conf.go:111-118`, `coins/coin.go:87-89`, `main.go:163-165`).
- **axis=Card1** — case sensitivity: Go EqualFolds section/key names, C++ ptree is case-sensitive
  (`conf.go:112,169`).
- **axis=Card2** — Go-only keys: `DustAmount` (used, `handlers.go:1453`, `utxo_select.go:62`),
  `OmitJSONVersion` (used, `wallet/rpc.go:94-96`), `GetNewKeySupported`/`ImportWithNoScanSupported`
  (parsed-inert) — none are read by C++.
- **axis=Card2** — `CashAddrPrefix` conf value ignored by Go; substituted with method-derived
  `"bitcoincash"` (`coins/coin.go:99,119-126` vs C++ `xbridgewalletconnectorbch.cpp:328-358`).
- **axis=Card2** — `MinimumAmount` consumed by C++ exchange (`xbridgeexchange.cpp:124,145`) but
  parsed-and-ignored by Go.
- **axis=Card2** — prefix types: C++ reads `AddressPrefix/ScriptPrefix/SecretPrefix` as string then
  `lexical_cast<char>` (missing value throws in connector `init`, `xbridgewalletconnectorbtc.cpp:1516-1518`);
  Go `int` defaults to `byte(0)` silently.
- **axis=Card2** — `Title` default differs: C++ `""` (`xbridgeapp.cpp:976`) vs Go section name
  (`conf.go:255`).
- **axis=Card2** — `CreateTxMethod`: C++ rejects ETH/unknown methods (`xbridgeapp.cpp:1043-1090`),
  Go silently maps every non-BCH method to utxo-btc (`coins/coin.go:107-113`).
- **axis=Card3** — Go omits all C++ wallet-admission gates: maker locktime (`xbridgeapp.cpp:1008-1013`),
  taker locktime (`:1014-1027`), confirmation drift (`:1029-1035`); a coin C++ refuses to load
  trades in Go.
- **axis=Card3** — Go has no BlockTime==0 admission gate (`wallet/conf.go:15` checks Ip/Port only;
  runtime fallback 60s `api/locktime.go:44-48`); C++ drops BlockTime==0 wallets (`xbridgeapp.cpp:1002-1006`).
- **axis=Card4** — lifecycle: C++ `createConf` writes the template and survives a missing/malformed
  file (`init.cpp:1920`, `settings.cpp:75-79`); Go `fatalf`s/exits on missing or unparseable
  `xbridge.conf` (`main.go:159-162`).
- **axis=Card4** — C++ mutates `xbridge.conf` at runtime by backfilling missing-key defaults
  (`settings.h:88,98`); Go is strictly read-only.
- **axis=Card5** — no Go equivalent for `-enableexchange` and `-dxnowallets` (the latter overrides
  `Main.ShowAllOrders` in C++, `xbridgeapp.cpp:372`); Go honors conf only.
- **axis=Card5** — `-walletversionstr` default `"/blocknet:4.4.1/"` (lowercase) differs from C++
  `"/Blocknet:4.4.1/"` (`main.go:103` vs `clientversion.cpp:15,71-77,87-101`).
- **axis=Card5** — no env vars on either side (Go `os.Getenv` 0 matches; C++ xbridge `getenv` 0 matches).
- **axis=Card6** — Go `reloadConf` keys connectors off all `[TICKER]` sections, never
  `ExchangeWallets` membership, and applies no Card-3 gates; C++ `dxLoadXBridgeConf` drops wallets
  leaving `ExchangeWallets` or failing a gate (`xbridgeapp.cpp:929-948,1008-1035`).
- **axis=Card6** — Go reload is last-good on failure; C++ reload keeps partial ptree state
  (`api/node.go:292-298` vs `rpcxbridge.cpp:229` + `settings.cpp:73-79`).
- **axis=Card6** — Go never clears non-local orders on reload when `ShowAllOrders` is off (C++
  does, `rpcxbridge.cpp:232-233`).

---

Report written to `docs/audit/evidence/config.md`..
