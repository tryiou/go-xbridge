# xbridge-go

A portable, standalone **Go** reimplementation of the Blocknet **XBridge**
atomic-swap engine. It is a **thin client**: it speaks the existing XBridge
wire protocol to the live Blocknet service-node P2P network, so a user can trade
by connecting their own (SPV) wallets — **without running `blocknetd`**.

Service-node fees are paid by the user's connected **Blocknet SPV wallet**
(via Blocknet core RPC); BLOCK is treated like any other coin through the
wallet-connector abstraction.

> This is a from-scratch reimplementation, not a wrapper around `blocknetd`.
> The wire contract lives in [`docs/protocol.md`](docs/protocol.md).

## Status

| Package | State | What it does |
|---------|-------|--------------|
| `proto` | foundational | Packet header/body codec, command set, signing digest. Stdlib only; unit-tested. |
| `p2p`   | partial | Bitcoin P2P message framing + connection read/write; `version`/`verack` handshake implemented (`p2p/version.go`), **live-verify against a real node still TODO** (see `docs/protocol.md` §1.3). |
| `crypto`| wired + tested | `BtcSigner` over `btcd/btcec/v2`: 64-byte compact ECDSA over `Packet.Digest()`; round-trip + tamper tests pass. |
| `coins` | partial | Stdlib-only per-coin foundation: coin registry + amount parsing (`amount.go`) + address codec — base58check (P2PKH/P2SH) and bech32/bech32m (P2WPKH/P2WSH/P2TR) (`base58*.go`, `bech32.go`, `address.go`); unit-tested (`docs/coins.md`). Per-coin **tx construction** still todo. |
| `wallet`| todo | RPC wallet-connector (signs + pays fees via connected SPV wallet). |
| `swap`  | partial | `Transaction` state machine (port of `xbridgetransaction*`): join + two-confirmation progression + expiry; unit-tested. Session/deposit layer still todo. |
| `api`   | todo | `dx*` operations as Go calls (port of `rpcxbridge.cpp`). |

## Build

```sh
cd xbridge-go
go build ./...
go test ./...
```

## Layout

```
xbridge-go/
  proto/     packet + body codec, command set, signing digest
  p2p/       Bitcoin P2P framing, connection, version/verack handshake
  crypto/    secp256k1 Signer interface (btcec recipe)
  docs/      protocol.md, swap.md, coins.md — canonical specs
  coins/     coin registry, amount parsing, address codec (base58/bech32)
  wallet/    (todo) RPC wallet connectors
  swap/      Transaction state machine (port of xbridgetransaction*)
  api/       (todo) dx* API surface
```

## Next steps

1. ~~Wire `crypto.Signer` with `btcd/btcec/v2` and add a signing test.~~ ✅ done.
2. ~~Implement the Bitcoin `version`/`verack` handshake (`p2p/conn.go`,
   `p2p/version.go`).~~ ✅ done (unit-tested; **live-verify against a real node
   still TODO** — see `docs/protocol.md` §1.3).
3. ~~Build `swap/` state machine core (join + two-confirmation progression +
   expiry), unit-tested.~~ ✅ done (`swap/`, `docs/swap.md`). `Session`/deposit
   layer (`trSigned`/`trCommited`, per-coin tx) still todo — needs `coins/` +
   `wallet/`.
4. ~~Build `coins/` foundation (coin registry, amount parsing, address
   codec).~~ ✅ done (`coins/`, `docs/coins.md`). Add per-coin **tx
   construction** (deposit/refund UTXO tx) and `wallet/` (RPC connector) to drive
   the `swap` Session/deposit layer (`trSigned`/`trCommited`).
