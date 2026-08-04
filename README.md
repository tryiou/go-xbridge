# go-xbridge

A portable, standalone **Go** reimplementation of the Blocknet **XBridge**
atomic-swap engine — a **thin client** that speaks the existing XBridge wire
protocol to the live Blocknet service-node P2P network and trades through your
own (SPV) wallets, **without running `blocknetd`**.

> This is a from-scratch reimplementation, not a wrapper around `blocknetd`.
> Repo context and contributor conventions live in [`CLAUDE.md`](CLAUDE.md);
> the wire contract is in [`docs/protocol.md`](docs/protocol.md); how to run
> and trade is in [`docs/usage.md`](docs/usage.md) — start there as a user.

## Build

```sh
go build ./...
go test ./...
```

Requires Go 1.25+ (toolchain 1.26 works).

## Running

`xbridged` discovers the Blocknet P2P network like a core wallet: it resolves
the DNS seeds, connects to a few healthy peers, and learns more via `addr`
gossip. No `-node` (service-node URL) is required, and it never downloads or
serves the blockchain — it only relays XBridge order/swap traffic.

```sh
./xbridged -network mainnet -conf ~/.blocknet/xbridge.conf
```

The `xbridge.conf` schema + sample, the complete flag reference, the network
table, and a make/take-order walkthrough are all in
[`docs/usage.md`](docs/usage.md) — **start there as a user.**

## Documentation

| Document | Purpose |
|----------|---------|
| [`docs/usage.md`](docs/usage.md) | **How to run and use the app** — install, `xbridge.conf`, flags, trading walkthrough. Start here as a user. |
| [`docs/protocol.md`](docs/protocol.md) | The XBridge wire contract: transport, packet layout, commands, swap handshake, signing. |
| [`docs/api.md`](docs/api.md) | The `dx*` JSON-RPC surface and its C++ parity status. |
| [`docs/swap.md`](docs/swap.md) | The swap state machine + HTLC deposit layer (canonical spec). |
| [`docs/coins.md`](docs/coins.md) | Per-coin metadata, amounts, address codec, tx construction. |
| [`docs/wallet.md`](docs/wallet.md) | The wallet `Connector` contract (RPC + local). |
| [`docs/AUDIT.md`](docs/AUDIT.md) | Consolidated audit register: C++↔Go fidelity, `dx*` equivalence matrix, security findings. |
| [`docs/STATUS.md`](docs/STATUS.md) | Per-package implementation status and roadmap (contributor-facing, not required to use the app). |

Contributor guidance lives in [`CLAUDE.md`](CLAUDE.md).
