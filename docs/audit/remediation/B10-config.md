# B10 — Config-parity (CFG-F84–F91)

Branch: `fix/config-parity` (off `main` @ B9 merge `639cd28`).
Status: MERGED into `main` @ `537d316` (fast-forward).
C++ reference: Blocknet Core @ `e9ddbc2bd` (v4.4.1 era).
Go subject: `config/conf.go`, `config/admit.go`, `cmd/xbridged/main.go`,
`coins/coin.go`, `wallet/conf.go`, `wallet/activate.go`, `api/node.go`,
`api/store.go`, `api/handlers.go`, `api/utxo_select.go`, `api/engine.go` (8
CONFIG findings, plus `CRYPTO-F80` folded and `CFG-F86` documented; per
`register.md` Owner B10).

The conf-parity pass. The headline items: a stock `[Rpc]` section in
xbridge.conf no longer aborts `xbridged` at startup (S2 CFG-F84), the wallet
admission gates C++ applies before connecting a wallet are now enforced
(CFG-F85), and hot-reload — connector activation, order clearing, and a
30-second reachability sweep — now mirrors C++ `updateActiveWallets`
(CFG-F87). The branch also aligns `ExchangeWallets` parsing, key case
sensitivity, CLI flags, and the conf-key set with the C++ reader.

## Findings resolved on this branch

| ID | Title | C++ source of truth | Go call sites → fix |
|---|---|---|---|
| F84 | `[Rpc]` section aborts startup (S2) | `util/settings.h:49-65` — `Rpc.*` keys defined but never read (dead section) | `config.Load` whitelists `[Main]`/`[Rpc]` (exact-case); the `"COIN not set"` fatal is unreachable for stock confs; `TestLoadSkipsRpcSection` |
| F85 | Wallet admission gates absent (S2) | `xbridgeapp.cpp:1002-1040` — connect check, maker/taker locktime targets (incl. slow chains), confirmation drift `max(900/blockTime,4)`; dispatch `:1043-1090` | new `config.Admit` + `config.Admitted`, constants single-sourced (`xbridgewallet.h:96-102`) and aliased by `api`; applied at startup/reload/sweep; `TestAdmitGates`, `TestAdmitCreateTxMethod`, `TestAdmittedFilters` |
| F86 | Missing conf: C++ creates template and runs; Go exits(1) (S2) | `xbridgeapp.cpp:306-358` `createConf` writes a template to `GetDataDir(false)/xbridge.conf` | **DOCUMENTED** — the never-creates hard rule stands: the library AND daemon require an existing conf; deliberate divergence |
| F87 | Hot-reload semantics differ (S2) | `xbridgeapp.cpp:917-1214` `updateActiveWallets` (EW keying, gates, `init()` probe, bad-wallet retry `:963-971`), `:3674-3677` (30 s re-post), `rpcxbridge.cpp:229-233` (ClearBad + clearNonLocalOrders unless showAllOrders) | `wallet.Activator` (EW ∩ gates ∩ probe, 300 s bad-wallet retry); reload preserves `ForceShowAllOrders`/`CheckReachability`/`PersistSecrets` and prunes non-local orders via `Store.PruneUnconnected`; 30 s `sweepLoop` re-probes in-memory settings (no file re-read, no prune); `TestActivateExchangeWalletsOnly`, `TestActivateProbe`, `TestActivateBadWalletRetry`, `TestActivateClearBad`, `TestReloadAppliesEWKeying`, `TestReloadPrunesUnconnectedOrders`, `TestReloadPreservesFlagOverrides`, `TestSweepConnectors`, `TestPruneUnconnected` |
| F88 | `ExchangeWallets` parsing differs (S3) | `util/settings.cpp:143-166` split on `,;:`; `ccy::Symbol::validate` (`currency.h:29-47`) — uppercase, len 1..8, no trim | `config.parseMain` mirrors the separator set + validation; `TestExchangeWalletsCppSemantics` |
| F89 | Case-insensitive keys in Go (S3) | boost property_tree case-sensitivity (`COIN` ≠ `coin`) | exact-case key + `[Main]` lookups in `config`; `TestCaseSensitiveKeys` |
| F90 | Missing CLI flags (S3) | `init.cpp:569/572` `-enableexchange`/`-dxnowallets`; `xbridgeapp.cpp:372` `gArgs.GetBoolArg("-dxnowallets", showAllOrders())` | `-dxnowallets` (ShowAllOrders override, kept across reload) + `-enableexchange` (inert compat no-op); daemon honors `Main.ShowAllOrders` at startup (pre-fix only after reload); version-case sub-item already fixed in `a0fee1e` (B7) |
| F91 | Go-only / ignored conf keys (S3) | `Settings::get` `_T()` default = `""` (`settings.h:75-84`) so C++ `Title` is `""` (card had this inverted); `MinimumAmount` → exchange dustAmount (`xbridgeexchange.cpp:145`); `CashAddrPrefix` conf value wins (`bch.cpp:306-308`/`devault.cpp:278-280`); ETH/unknown method rejected (`xbridgeapp.cpp:1043-1090`) | `Title` default `""`; `effectiveDust`/`isDustNative` use `MinimumAmount` (folded `CRYPTO-F80`); `coins.FromConf` honors the `CashAddrPrefix` conf value; `config.Admit` rejects ETH/unknown; `TestTitleDefaultsEmpty`, `TestEffectiveDust`, `TestFromConfCashAddrPrefix` |

## Activation pipeline reference

C++ activates a wallet exactly as follows; go-xbridge mirrors it in
`wallet.Activator.Activate` (used by startup, `dxLoadXBridgeConf` and the 30 s
sweep):

```
[Main].ExchangeWallets symbol
  └─ not in conf            → drop "not found in config"
  └─ config.Admit          → drop (gate reason; xbridgeapp.cpp:1002-1040)
  └─ bad-wallet window?     → drop "retry pending" (300 s, :963-971)
  └─ NewConnectorFromConf   → drop on error
  └─ reachability probe     → drop "wallet not reachable" + mark bad
       (GetBlockCount, the thin-client analog of conn->init() ≈ getInfo,
        xbridgewalletconnectorbtc.cpp:1513-1525; bounded by probeTimeout)
  └─ connected
```

`dxLoadXBridgeConf` = `ClearBad` + `Activate` + config swap (preserving
`ForceShowAllOrders`/`CheckReachability`/`PersistSecrets`) + `PruneUnconnected`
unless ShowAllOrders. The 30 s sweep re-runs `Activate` against the in-memory
conf only and swaps connectors when the ticker set changed; it never prunes
orders and never re-reads the file.

## Decision log

- **Probe seam:** `Config.CheckReachability` (daemon `true`, tests `false`) —
  reload/sweep tests stay hermetic without a live wallet. The `wallet.Activator
  .Probe` field is injectable.
- **Probe divergence:** Go probes `getblockcount`; C++ `init()` also validates
  `getinfo` relay-fee/mediantime fields. A wallet answering `getblockcount` but
  otherwise misbehaving would pass the Go probe.
- **Sweep concurrency:** C++ probes with `-rpcthreads` workers and serializes
  reload vs update with `m_updatingWalletsLock`; Go probes sequentially
  (bounded per-probe) and `sweepConnectors` bails out under `cfgMu` if a reload
  replaced the config during the probe window — no lost update.
- **CFG-F86 (documented):** never-creates stance — `config.Load` and the daemon
  require an existing xbridge.conf; C++'s `createConf` template is not ported.
- **`CreateTxMethod` set:** Go accepts `BTC/SYS/LTC/DGB/BCH/BTG/DEVAULT`
  (LTC a documented Go extension — C++ has no LTC dispatch), refuses
  `BCD/PART/STEALTH/XST` (non-portable, `CRYPTO-F98/F99`), rejects ETH/unknown.

## Golden vectors

- `config/conf_test.go` — `TestLoadSkipsRpcSection`, `TestExchangeWalletsCppSemantics`,
  `TestCaseSensitiveKeys`, `TestTitleDefaultsEmpty`.
- `config/admit_test.go` — `TestAdmitConstantsMatchCpp`, `TestAdmitGates`,
  `TestAdmitCreateTxMethod`, `TestAdmittedFilters`.
- `wallet/activate_test.go` — `TestActivateExchangeWalletsOnly`,
  `TestActivateGateDrops`, `TestActivateMissingConf`, `TestActivateProbe`
  (mockRPC vs dead port), `TestActivateBadWalletRetry`, `TestActivateClearBad`.
- `api/node_reload_test.go` — `TestReloadAppliesEWKeying`,
  `TestReloadPrunesUnconnectedOrders`, `TestReloadPreservesFlagOverrides`,
  `TestSweepConnectors`.
- `api/store_test.go` — `TestPruneUnconnected`.
- `api/parity_dustfee_test.go` — `TestEffectiveDust` (`MinimumAmount`).
- `coins/coin_test.go` — `TestFromConfCashAddrPrefix`.

## Verify

- `gofmt -l .` clean; `go build ./...`, `go vet ./...`, `go test ./...`,
  `go test -count=1 -race ./...`.
- `make parity` (A–F) and `make canary` green from `tools/`.
- Manual: start `xbridged` with a conf carrying `[Rpc]` + a non-EW coin and a
  down wallet — it starts, connects only EW wallets, drops unreachable ones,
  and `dxGetLocalTokens` reflects the connected set.
