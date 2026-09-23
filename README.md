# go-xbridge

A portable, standalone **Go** reimplementation of the Blocknet **XBridge**
atomic-swap engine — a **thin client** that speaks the existing XBridge wire
protocol to the live Blocknet service-node P2P network and trades through your
own wallets, **without running `blocknetd`**.

This is a from-scratch reimplementation, **not** a wrapper around `blocknetd`.

## How it works

`xbridged` discovers the Blocknet P2P network like a core wallet: it resolves
the DNS seeds, connects to a few healthy peers, and learns more via `addr`
gossip. It never downloads or serves the blockchain — it only relays XBridge
order/swap traffic. Making/taking orders, deposits, and refunds are driven over
JSON-RPC against your connected wallets; `xbridged` itself holds no coin keys.

## What you need

- **Go 1.25+** to build (see `go.mod`; CI uses `1.25.x`).
- An **`xbridge.conf`** describing your coins and wallet(s). The file is
  **read-only** — `xbridged` never creates or mutates it. If `-conf` points at
  a missing file, startup is **fatal**.
- **Your own coin daemons, running and synced**, with JSON-RPC reachable at the
  `Ip`/`Port` in each `[TICKER]` section. For every ticker you list in
  `ExchangeWallets` you must run that coin's own wallet/node (e.g. `bitcoind`
  for BTC, the Blocknet BLOCK wallet for BLOCK). `xbridged` drives those daemons
  over RPC to sign, broadcast, and (for BLOCK) pay the service-node fee.
- A **connected wallet** for anything that signs or broadcasts. Read-only
  commands (`dxGetOrders`, etc.) need only the order book fed over P2P.

Trades incur the Blocknet **service-node fee**, paid by your connected **BLOCK**
wallet via core RPC — BLOCK is just another coin through the wallet connector.

## Build

Requires **Go 1.25+** (see `go.mod`; CI uses `1.25.x`).

```sh
# From the repo root. -o names the output; the binary lands in the
# current directory (here: ./xbridged):
go build -o xbridged ./cmd/xbridged

# Sanity check (no xbridge.conf or network needed):
./xbridged -h
./xbridged -version
```

Notes:

- `go build ./...` only **checks that everything compiles** — it writes
  no binaries. You always get a runnable daemon via `./cmd/xbridged`
  as above (bare `go build ./cmd/xbridged` works too and also drops
  `./xbridged` in the current directory; `-o` is just explicit).
- A plain local build reports `xbridged dev (commit=none, date=unknown)`;
  release builds stamp the real version via goreleaser ldflags.
- `cmd/liveprobe` is the ad-hoc live-node check, built the same way
  (`go build -o liveprobe ./cmd/liveprobe`), or run without building:
  `go run ./cmd/liveprobe -addr coreproxy.airdns.org:42111 -magic a1a0a2a3`.

```sh
go vet ./...        # static checks
go test ./...       # unit tests (hermetic, no live-network dials)
```

Point a dapp's RPC URL at `xbridged`'s JSON-RPC listener to use the `dx*`
surface unchanged. Full build/test/lint gates are in `AGENTS.md`.

## Configuration — `xbridge.conf`

The same INI format the original core wallet reads. Two section kinds:

- **`[Main]`** — global settings.
- **`[TICKER]`** — one section per coin (BTC, LTC, DOGE, BLOCK, …). **Every
  coin connector is defined entirely by its section** — there is no hardcoded
  coin data. The schema below mirrors `config/conf.go` (a faithful port of
  `src/xbridge/xbridgeapp.cpp createConf()`).

Section and key names are **case-sensitive** (`[main]` is a coin, not `[Main]`;
`"COIN"` is not `"coin"`). A section named exactly `[Rpc]` is skipped — it is
never treated as a coin.

### `[Main]`

| Key | Type | Meaning |
|-----|------|---------|
| `ExchangeWallets` | csv (`','`/`';'`/`':'` separated, uppercased, 1–8 chars) | Tickers that have a local wallet configured (the connectors `xbridged` drives). Overridden by `-dxnowallets`. |
| `ShowAllOrders` | bool | Show orders for coins without a local wallet. |
| `FullLog` | bool | Verbose logging. |

### `[TICKER]`

| Key | Type | Meaning |
|-----|------|---------|
| `Title` | string | Human name (empty when unset — it does **not** default to the section name). |
| `Address` | string | Wallet/RPC bind address (optional). |
| `Ip` / `Port` | string / int | Wallet/node JSON-RPC endpoint. |
| `Username` / `Password` | string | Wallet RPC auth. |
| `CreateTxMethod` | string | Selects the tx-construction path. Must be one of `BTC SYS LTC DGB BCH BTG DEVAULT`; anything else (incl. `PART BCD STEALTH XST`) is refused. `BCH` selects the BCH family (CashAddr; legacy base58 version bytes collide with BTC's). |
| `CashAddrPrefix` | string | BCH CashAddr HRP (e.g. `bitcoincash`); empty for non-BCH coins. |
| `AddressPrefix` / `ScriptPrefix` / `SecretPrefix` | int | base58check version bytes (P2PKH / P2SH / WIF) as decimals. |
| `COIN` | uint64 | Base-unit multiplier (e.g. `100000000`); decimals are derived from its trailing zeros. |
| `MinimumAmount` | uint64 | Dust / minimum amount source (base units); effective dust is the wallet relay fee when available, else this key, else the `5460` fallback. |
| `TxVersion` | int | Transaction version (default `1`). |
| `MinTxFee` / `FeePerByte` | uint64 | Fee rules (base units / sat-per-byte). |
| `DustAmount` | uint64 | createConf-template key never read back by C++; parsed for fidelity only. |
| `BlockTime` | int | Seconds per block (used for HTLC lock-time math and wallet admission gates). |
| `Confirmations` | int | Required confirmations. |
| `TxWithTimeField` | bool | Tx carries a time field. |
| `LockCoinsSupported` / `GetNewKeySupported` / `ImportWithNoScanSupported` | bool | Wallet capability flags. |
| `JSONVersion` | string | Wallet RPC JSON version (default empty; empty omits the `"jsonrpc"` request field — set `"1.0"` for bitcoind/blocknetd-style wallets). |
| `ContentType` | string | RPC `Content-Type` (default empty; empty leaves the header unset). |
| `OmitJSONVersion` | bool | Drop the `"jsonrpc"` field from RPC requests (XLite-style wallets require this). |

### Sample (`xbridge.conf`)

A minimal, working configuration (credentials redacted). It defines two coins
under `[Main]` — `BLOCK` (the service-node fee coin) and `BTC` — each
`[TICKER]` section pointing `xbridged` at that coin's local wallet RPC. Copy
it, restore your own `Username`/`Password`, and adjust `Ip`/`Port` to match
your wallets. Add more `[TICKER]` sections (and list them in
`ExchangeWallets`) for each coin you trade.

```ini
[Main]
ExchangeWallets=BLOCK,BTC
FullLog=true
ShowAllOrders=true

[BLOCK]
Title=Blocknet
Ip=127.0.0.1
Port=41419
Username=<your-rpc-user>
Password=<your-rpc-pass>
AddressPrefix=26
ScriptPrefix=28
SecretPrefix=154
COIN=100000000
MinimumAmount=0
TxVersion=1
DustAmount=0
CreateTxMethod=BTC
GetNewKeySupported=true
ImportWithNoScanSupported=true
MinTxFee=10000
BlockTime=60
FeePerByte=20
Confirmations=0
TxWithTimeField=false
LockCoinsSupported=false

[BTC]
Title=Bitcoin
Ip=127.0.0.1
Port=8332
Username=<your-rpc-user>
Password=<your-rpc-pass>
AddressPrefix=0
ScriptPrefix=5
SecretPrefix=128
COIN=100000000
MinimumAmount=0
TxVersion=2
DustAmount=0
CreateTxMethod=BTC
GetNewKeySupported=false
ImportWithNoScanSupported=false
MinTxFee=12000
BlockTime=600
FeePerByte=60
Confirmations=0
TxWithTimeField=false
LockCoinsSupported=false
```

> Only the coins listed in `[Main]`'s `ExchangeWallets` are driven as local
> connectors; the rest of the network's orders still appear via P2P subject to
> `ShowAllOrders`. See [`docs/architecture.md`](docs/architecture.md) for the
> contributor-side view.

## Running `xbridged`

```sh
# Default conf path is <home>/.blocknet/xbridge.conf (override with -conf):
./xbridged -network mainnet -conf ~/.blocknet/xbridge.conf

# Pin specific peers alongside discovered ones:
./xbridged -network mainnet -addnode 1.2.3.4:41412

# Legacy explicit-single-peer mode (skips discovery):
./xbridged -node coreproxy.airdns.org:42111
```

Network discovery resolves DNS seeds, connects to healthy peers, and learns
more via `addr` gossip — like a core wallet. Discovery picks the network magic
and default port from `-network` (`-magic` overrides the magic). The per-network
magics and default ports are in [`docs/protocol.md`](docs/protocol.md); there
is **no** Blocknet staging network (the third network is `regtest`).

### Flag reference

This table is the complete flag authority (`cmd/xbridged/main.go`).

| Flag | Default | Purpose |
|------|---------|---------|
| `-network` | `mainnet` | Network to discover on: `mainnet`\|`testnet`\|`regtest`. Still selects the P2P magic for an explicit `-node` dial (`-magic` overrides). |
| `-node` | `""` | Explicit service-node P2P address `host:port`; empty enables network discovery. Ignores `-addnode`. |
| `-addnode` | `""` | Comma-separated peer addresses added to the discovered set (discovery mode only). |
| `-conf` | `<home>/.blocknet/xbridge.conf` | Path to `xbridge.conf` (read-only; fatal if missing). |
| `-magic` | `""` | Network magic (4-byte hex); derived from `-network` if empty. |
| `-rpcbind` | `127.0.0.1:41414` | JSON-RPC listen address `host:port` for the `dx*` API. Defaults to **loopback only**; set explicitly to bind elsewhere. |
| `-rpcuser` / `-rpcpassword` | `""` / `""` | HTTP Basic auth pair. Auth is enforced when the pair is set **or** any `-rpcauth` entry is configured; a non-loopback `-rpcbind` without auth logs a warning. With no credentials the daemon is open on its loopback bind (there is no cookie-auth fallback, unlike C++). Each flag falls back to the environment when empty — `XBRIDGED_RPCUSER` / `XBRIDGED_RPCPASSWORD` (explicit flags win) — so secrets never have to appear in `ps`-visible argv. |
| `-rpcauth` | `""` | Comma-separated multi-user auth entries, `user:salt$hash` (HMAC-SHA256, the same credential path C++ offers). |
| `-walletversion` | `4040100` | Blocknet `CLIENT_VERSION` advertised in `getnetworkinfo`. |
| `-walletversionstr` | `/Blocknet:4.4.1/` | Subversion advertised in `getnetworkinfo`. |
| `-datadir` | OS config dir | Directory for local swap state. Empty uses the OS config dir: `~/.config/xbridged` (Linux), `~/Library/Application Support/xbridged` (macOS), `%AppData%\xbridged` (Windows). Created `0700`. |
| `-logfile` | `<datadir>/xbridged.log` | Log file path (stderr stays active). File logging is always on: an empty value selects the default file. Rotation: 10 MiB × 2 backups. |
| `-loglevel` | `debug` | Log verbosity: `debug`\|`info`\|`warn`\|`error` (debug default while in beta). |
| `-rpcservertimeout` | `30` | HTTP RPC timeout in seconds (C++ `DEFAULT_HTTP_SERVER_TIMEOUT` parity), applied to read/write; plus a 10 s header and 60 s idle timeout. |
| `-dxnowallets` | `false` | Show all orders for non-local wallets (C++ `-dxnowallets`; overrides `Main.ShowAllOrders`). |
| `-enableexchange` | `false` | Accepted for blocknetd CLI parity only; exchange mode is inherent to `xbridged` (no-op). |

## Making a trade

The `dx*` names below are **JSON-RPC methods, not shell commands** — you call
them over HTTP against `xbridged`'s `-rpcbind` (default `127.0.0.1:41414`),
exactly like bitcoind's RPC. Params are **positional** (a JSON array). For
example:

```sh
# With -rpcuser/-rpcpassword configured, pass HTTP Basic credentials:
curl -s -u user:password http://127.0.0.1:41414 \
  -H 'content-type: application/json' \
  -d '{"method":"dxGetOrderBook","params":[1,"BTC","BLOCK"],"id":1}'
```

A successful response is JSON-RPC 1.0 (`result`/`error`/`id`, no `jsonrpc`
field); business errors ride in `result`, not the envelope `error`:

```json
{"result":{"detail":1,"maker":"BTC","taker":"BLOCK","bids":[],"asks":[]},"error":null,"id":1}
```

Steps (params shown positionally — wrap them in the `"params"` array as above):

 1. **Start the daemon** with a valid `-conf`. Trading requires the wallets
    for both currencies to be reachable via their `[TICKER]` connector sections
    in `xbridge.conf` (the wallets fund, sign, and broadcast; `xbridged` itself
    holds no coin keys).
 2. **Browse** the order book:
    - `dxGetOrders` — all open orders.
    - `dxGetOrderBook 1 BTC BLOCK` — best bid/ask for a pair (detail level 1).
 3. **Make an order**:
    - `dxMakeOrder BTC 0.01 <maker_addr> BLOCK 100 <taker_addr> exact`
    - partial: `dxMakePartialOrder BTC 0.01 <maker_addr> BLOCK 100 <taker_addr> 0.001`
 4. **Take an order**:
    - `dxTakeOrder <order_id>` — full take (omit amount).
    - `dxTakeOrder <order_id> 0.005` — partial take.

    Per trade, `xbridged` generates a fresh ephemeral secp256k1 keypair that
    signs the order/accept packets and becomes the HTLC deposit pubkey — no
    operator key is configured. The swap driver runs the Maker ⇄ ServiceNode ⇄
    Taker handshake: it builds/broadcasts the HTLC deposits and claims/refunds
    as the hub advances the state. Every outbound packet is envelope-addressed
    to the trade's hub, and every inbound handshake packet is re-verified
    against that hub's pinned key. Before committing our own deposit (or
    redeeming the counterparty's), the counterparty HTLC deposit is validated
    on-chain; a bad deposit is wire-cancelled and rolled back.
    Details: [`docs/protocol.md`](docs/protocol.md) (wire),
    [`docs/architecture.md`](docs/architecture.md) (driver).
 5. **Cancel** an open order: `dxCancelOrder <order_id>`.

Full field/param contracts for every `dx*` command (positional params, response
shapes, error codes) are in [`docs/api.md`](docs/api.md). Thin-client
limitations (`dxGetOrderHistory` / `dxGetTradingData` reflect session-local
fills only) are documented under "Tier 3" there.

## Notes & troubleshooting

- **`-conf` is required and fatal if missing** — `xbridged` never creates it.
- The conf file is **read-only**; edit it yourself, don't expect regeneration.
  `dxLoadXBridgeConf` reloads it at runtime (keeps the last good config on
  failure; answers `false`, not an error, when the reload fails).
- Discovery needs at least one reachable service node; if all seeds/peers are
  unreachable, use `-node <host:port>` to pin one. `-addnode` only augments
  discovery mode — it is ignored with `-node`.
- **Local swap state survives a restart.** Each trade's order, per-trade key
  material, HTLC secret, and pre-signed refund are persisted to
  `<datadir>/xbridged-swaps.json` (there is no opt-out, so a restarted
  mid-flight swap can always auto-refund and re-sign cancels). Only
  locally-created orders are persisted. Persistence is best-effort: a
  corrupt/missing file is quarantined (`xbridged-swaps.json.bad.<unixnano>`)
  with individually-valid records salvaged, and the node starts rather than
  crashing.
- Independently, every broadcast deposit/refund/claim is appended with its
  order id, locktime, and full raw hex to the dedicated per-day transcript
  `<datadir>/log-tx/xbridgep2p_YYYYMMDD.log` — the manual-recovery record,
  usable with the deposit coin's own wallet even if the daemon and swap-state
  file are both gone. The general log (`xbridged.log`/stderr) never carries
  trade hex or private key material.
- `dxMakeOrder`/`dxTakeOrder`/`dxCancelOrder` require the relevant wallets to
  be connected (they fund/sign/broadcast); without a reachable wallet the call
  returns a "no session" / "unable to connect to wallet" error.
- **No operator key is configured.** Coin signing is delegated to your
  connected wallets.

## Documentation

| Document | Purpose |
|----------|---------|
| **This README** | How to install, configure, run, and trade. Flag + `xbridge.conf` authority. |
| [`docs/protocol.md`](docs/protocol.md) | The XBridge wire contract: transport, packet layout, commands, swap handshake, signing. Network magic/port authority. |
| [`docs/api.md`](docs/api.md) | The `dx*` JSON-RPC surface (params, response shapes, error codes). Method-inventory + auth/transport authority. |
| [`docs/architecture.md`](docs/architecture.md) | Contributor guide: package-by-package code architecture, build/test/verify, conventions. |
| [`AGENTS.md`](AGENTS.md) | Repo conventions and hard rules for coding agents/contributors. |
| [`conformance/`](conformance/) | Behavioral/wire conformance suite (external Go module, build-tag `conformance`; stage F of the parity gate). |
