# xbridge-go

A portable, standalone **Go** reimplementation of the Blocknet **XBridge**
atomic-swap engine. It is a **thin client**: it speaks the existing XBridge
wire protocol to the live Blocknet service-node P2P network, so a user can trade
by connecting their own (SPV) wallets — **without running `blocknetd`**.

Service-node fees are paid by the user's connected **Blocknet SPV wallet**
(via Blocknet core RPC); BLOCK is treated like any other coin through the
wallet-connector abstraction.

> This is a from-scratch reimplementation, not a wrapper around `blocknetd`.
> The wire contract lives in [`docs/protocol.md`](docs/protocol.md); how to
> run and trade is in [`docs/usage.md`](docs/usage.md).

## Build

```sh
go build ./...
go test ./...
```

Requires Go 1.25+ (toolchain 1.26 works).

## Running — network discovery (no manual node URL)

`xbridged` discovers the Blocknet P2P network like a core wallet: it resolves
the DNS seeds, connects to a few healthy peers, and learns more via `addr`
gossip. No `-node` (service-node URL) is required, and it never downloads or
serves the blockchain — it only relays XBridge order/swap traffic.

```sh
# Discover the mainnet network automatically (default; -node is empty):
./xbridged -network mainnet -conf ~/.blocknet/xbridge.conf

# Optional: pin specific peers alongside discovered ones (the -addnode flag):
./xbridged -network mainnet -addnode 1.2.3.4:41412

# Legacy explicit-single-peer mode still works (skips discovery):
./xbridged -node coreproxy.airdns.org:42111
```

Full configuration (`xbridge.conf` schema + sample), the complete flag
reference, and a make/take-order walkthrough are in
[`docs/usage.md`](docs/usage.md).

## Documentation

| Document | Purpose |
|----------|---------|
| [`docs/usage.md`](docs/usage.md) | **How to run and use the app** — install, `xbridge.conf`, flags, trading walkthrough. Start here as a user. |
| [`docs/protocol.md`](docs/protocol.md) | The XBridge wire contract: transport, packet layout, commands, swap handshake, signing. |
| [`docs/api.md`](docs/api.md) | The `dx*` JSON-RPC surface and its C++ parity status. |
| [`docs/swap.md`](docs/swap.md) | The swap state machine + HTLC deposit layer (canonical spec). |
| [`docs/coins.md`](docs/coins.md) | Per-coin metadata, amounts, address codec, tx construction. |
| [`docs/wallet.md`](docs/wallet.md) | The wallet `Connector` contract (RPC + local). |
| [`docs/audit-dx-equivalence.md`](docs/audit-dx-equivalence.md) | `dx*` RPC equivalence audit vs the C++ source. |
| [`docs/STATUS.md`](docs/STATUS.md) | Per-package implementation status and roadmap (contributor-facing, not required to use the app). |

Contributor guidance lives in [`CLAUDE.md`](CLAUDE.md).
