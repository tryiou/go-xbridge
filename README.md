# go-xbridge

A portable, standalone **Go** reimplementation of the Blocknet **XBridge**
atomic-swap engine — a **thin client** that speaks the existing XBridge wire
protocol to the live Blocknet service-node P2P network and trades through your
own (SPV) wallets, **without running `blocknetd`**.

This is a from-scratch reimplementation, **not** a wrapper around `blocknetd`.

## How it works

`xbridged` discovers the Blocknet P2P network like a core wallet: it resolves
the DNS seeds, connects to a few healthy peers, and learns more via `addr`
gossip. It never downloads or serves the blockchain — it only relays XBridge
order/swap traffic. Making/taking orders, deposits, and refunds are driven over
JSON-RPC against your connected wallets; `xbridged` itself holds no coin keys.

## What you need

- **Go 1.25+** (toolchain 1.26 works) to build.
- An **`xbridge.conf`** describing your coins and wallet(s). The file is
  **read-only** — `xbridged` never creates or mutates it. If `-conf` points at
  a missing file, startup is **fatal**.
- **Your own coin daemons, running and synced**, with JSON-RPC reachable at the
  `Ip`/`Port` in each `[TICKER]` section. For every ticker you list in
  `ExchangeWallets` you must run that coin's own wallet/node (e.g. `bitcoind`
  for BTC, the Blocknet BLOCK wallet for BLOCK). `xbridged` drives those daemons
  over RPC to sign, broadcast, and (for BLOCK) pay the service-node fee.
- A **connected Blocknet-core-compatible wallet** (or local keys via
  `LocalConnector`) for anything that signs or broadcasts. Read-only commands
  (`dxGetOrders`, etc.) need only the order book fed over P2P.

Trades incur the Blocknet **service-node fee**, paid by your connected **BLOCK**
wallet via core RPC — BLOCK is just another coin through the wallet connector.

## Build

```sh
go build ./...      # builds cmd/xbridged and cmd/liveprobe
go test ./...       # optional, runs the unit tests
```

This produces the `xbridged` daemon (plus `cmd/liveprobe` for ad-hoc live node
checks). Point a dapp's RPC URL at `xbridged`'s JSON-RPC listener to use the
`dx*` surface unchanged.

## Configuration — `xbridge.conf`

The same INI format the original core wallet reads. Two section kinds:

- **`[Main]`** — global settings.
- **`[TICKER]`** — one section per coin (BTC, LTC, DOGE, BLOCK, …). **Every
  coin connector is defined entirely by its section** — there is no hardcoded
  coin data. The schema below mirrors `config/conf.go` (a faithful port of
  `src/xbridge/xbridgeapp.cpp createConf()`).

### `[Main]`

| Key | Type | Meaning |
|-----|------|---------|
| `ExchangeWallets` | csv | Tickers that have a local wallet configured (the connectors `xbridged` drives). |
| `ShowAllOrders` | bool | Show orders for coins without a local wallet. |
| `FullLog` | bool | Verbose logging. |

### `[TICKER]`

| Key | Type | Meaning |
|-----|------|---------|
| `Title` | string | Human name (defaults to the section name). |
| `Address` | string | Wallet/RPC bind address (optional). |
| `Ip` / `Port` | string / int | Wallet/node JSON-RPC endpoint. |
| `Username` / `Password` | string | Wallet RPC auth. |
| `CreateTxMethod` | string | Selects the tx-construction path (e.g. `"BTC"`). Drives segwit/bech32 support in `coins/`. |
| `CashAddrPrefix` | string | BCH CashAddr HRP (e.g. `bitcoincash`); empty for non-BCH coins. |
| `AddressPrefix` / `ScriptPrefix` / `SecretPrefix` | int | base58check version bytes (P2PKH / P2SH / WIF) as decimals. |
| `COIN` | uint64 | Base-unit multiplier (e.g. `100000000`); decimals are derived from its trailing zeros. |
| `MinimumAmount` | uint64 | Minimum trade amount (base units). |
| `TxVersion` | int | Transaction version (default `1`). |
| `DustAmount` / `MinTxFee` / `FeePerByte` | uint64 | Dust / fee rules (base units). |
| `BlockTime` | int | Seconds per block (used for HTLC lock-time math). |
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
> connectors; the rest of the network's orders still appear via P2P. BCH uses
> **CashAddr** and would set `CreateTxMethod=BCH` (its legacy base58 version
> bytes collide with BTC's). See [`docs/architecture.md`](docs/architecture.md).

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
magics and default ports are in [`docs/protocol.md`](docs/protocol.md).

### Flag reference

| Flag | Default | Purpose |
|------|---------|---------|
| `-network` | `mainnet` | Network to discover on: `mainnet`\|`testnet`\|`staging`. Still selects the P2P magic for an explicit `-node` dial (`-magic` overrides). |
| `-node` | `""` | Explicit service-node P2P address `host:port`; empty enables network discovery. |
| `-addnode` | `""` | Comma-separated peer addresses added to the discovered set. |
| `-conf` | `<home>/.blocknet/xbridge.conf` | Path to `xbridge.conf` (read-only; fatal if missing). |
| `-magic` | `""` | Network magic (4-byte hex); derived from `-network` if empty. |
| `-rpcbind` | `127.0.0.1:41414` | JSON-RPC listen address `host:port` for the `dx*` API. Defaults to **loopback only**; set explicitly to bind elsewhere. |
| `-rpcuser` / `-rpcpassword` | `""` / `""` | HTTP Basic auth for RPC. Enforced only when **both** are set (no cookie fallback); a non-loopback `-rpcbind` without auth logs a warning. |
| `-walletversion` | `4040100` | Blocknet `CLIENT_VERSION` advertised in `getnetworkinfo`. |
| `-walletversionstr` | `/blocknet:4.4.1/` | Subversion advertised in `getnetworkinfo`. |
| `-datadir` | OS config dir | Directory for local swap state (incl. each trade's per-trade M keypair). Empty uses the OS config dir: `~/.config/xbridged` (Linux), `~/Library/Application Support/xbridged` (macOS), `%AppData%\xbridged` (Windows). |
| `-logfile` | `<datadir>/xbridged.log` | Log file path (stderr stays active). |
| `-loglevel` | `info` | Log verbosity: `debug`\|`info`\|`warn`\|`error`. |

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
{"result":{"maker":"BTC","taker":"BLOCK","bids":[],"asks":[]},"error":null,"id":1}
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

    Per trade, `xbridged` generates a fresh ephemeral secp256k1 keypair
    (C++ `mPubKey`/`mPrivKey`) that signs the order/accept packets and becomes
    the HTLC deposit pubkey. No operator key is configured. The client driver
    (`api/swap.go`) runs the Maker ⇄ ServiceNode ⇄ Taker handshake: it
    builds/broadcasts the HTLC deposits and claims/refunds as the hub advances
    the state. Every outbound packet is envelope-addressed to the trade's hub
    (chosen at `dxMakeOrder`; `dxTakeOrder` adopts the order's hub), and every
    inbound handshake packet is re-verified against that hub's pinned key — a
    forged packet can never disable the auto-refund watcher. An order with no
    trusted hub cannot be taken (`NO_SERVICE_NODE`).
 5. **Cancel** an open order: `dxCancelOrder <order_id>`.

Full field/param contracts for every `dx*` command (positional params, response
shapes, error codes) are in [`docs/api.md`](docs/api.md). Thin-client
limitations (`dxGetOrderHistory` / `dxGetTradingData` reflect session-local
fills only) are documented under "Tier 3" there.

## Notes & troubleshooting

- **`-conf` is required and fatal if missing** — `xbridged` never creates it.
- The conf file is **read-only**; edit it yourself, don't expect regeneration.
- Discovery needs at least one reachable service node; if all seeds/peers are
  unreachable, use `-node <host:port>` to pin one.
- **Local swap state survives a restart** (matches C++ `orders.dat` /
  `loadOrders()` / `saveOrders()`). Each trade's order and its per-trade M
  keypair are persisted to `<datadir>/xbridged-swaps.json` (default OS config
  dir: `~/.config/xbridged` Linux, `~/Library/Application Support/xbridged`
  macOS, `%AppData%\xbridged` Windows; override with `-datadir`). Persistence
  is **on by default**; to point it at a throwaway location, pass `-datadir`.
  On `xbridged` start the node reloads these swaps *before* it dials, so
  `dxCancelOrder` and the refund path keep working after a restart using the
  restored key. Only locally-created orders (`Mine=true`) are persisted
  (mirroring C++'s `isLocal()` filter). Persistence is best-effort: a
  corrupt/missing file is logged and the node starts fresh rather than crashing.
- `dxMakeOrder`/`dxTakeOrder`/`dxCancelOrder` require the relevant wallets to
  be connected (they fund/sign/broadcast); without a reachable wallet the call
  returns a "no session" / "unable to connect to wallet" error.
- **No operator key is configured.** `xbridged` generates a fresh ephemeral
  secp256k1 keypair per trade to sign packets and build the HTLC deposit (the
  trader identity is not a static, operator-supplied key). Coin signing is
  delegated to your connected wallets.

## Documentation

| Document | Purpose |
|----------|---------|
| **This README** | How to install, configure, run, and trade. |
| [`docs/protocol.md`](docs/protocol.md) | The XBridge wire contract: transport, packet layout, commands, swap handshake, signing. |
| [`docs/api.md`](docs/api.md) | The `dx*` JSON-RPC surface (params, response shapes, error codes). |
| [`docs/architecture.md`](docs/architecture.md) | Contributor guide: package-by-package code architecture, build/test/verify, conventions. |
| [`docs/audit/`](docs/audit/README.md) | C++↔Go audit home: canonical register (`audit/register.md`), per-finding detail, axis evidence, verify reports. |
| [`conformance/`](conformance/) | Behavioral/wire conformance suite (external Go module, build-tag `conformance`; runs via `make parity` stage F). |
| [`CLAUDE.md`](CLAUDE.md) | Repo conventions and hard rules for coding agents/contributors. |
