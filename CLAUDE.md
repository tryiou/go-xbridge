# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Overview

`go-xbridge` is a **portable, standalone Go reimplementation** of the Blocknet
**XBridge** atomic-swap engine. It is a **thin client**: it speaks the existing
XBridge wire protocol to the live Blocknet service-node P2P network, so a user can
trade by connecting their own (SPV) wallets — **without running `blocknetd`**.

This is a from-scratch reimplementation, **not** a wrapper around `blocknetd`. The
wire contract is the source of truth and is documented in [`docs/protocol.md`](docs/protocol.md).
The C++ reference lives in the separate `blocknet_core` repo (`src/xbridge/`); its
header-comment enum values are frequently **stale** — trust the actual C++ writers
under `src/xbridge/`, not the comments.

Repo history: standalone git repo (branch `main`), extracted from `blocknet_core`
via `git filter-branch --subdirectory-filter go-xbridge`. Commit subjects are
unchanged but **SHA-1s differ from any pre-extraction references**.

## Layout

- `api/` — 1:1 port of blocknetd's `dx*` JSON-RPC surface (drop-in for dapps).
- `p2p/` — Bitcoin-style P2P framing, `version`/`verack` handshake, XBridge
  transport envelope (`encodeXBridgePayload`/`DecodeXBridgePayload`).
- `proto/` — XBridge packet header/body codec + per-`XBridgeCommand` body layouts.
- `crypto/` — secp256k1 compact ECDSA signer (`BtcSigner`).
- `coins/` — per-coin model; **all coin definitions come from `xbridge.conf`**.
- `wallet/` — RPC connector + local signer to the connected SPV wallet.
- `config/` — read-only INI loader mirroring `xbridgeapp.cpp::createConf()`.
- `swap/` — `Transaction` state machine + HTLC deposit layer.
- `cmd/xbridged` — the daemon; flags include `-conf` (default
  `<home>/.blocknet/xbridge.conf`, fatal if missing) and `-node`.

## Build, test, verify

```bash
go build ./...          # build all packages
go vet ./...            # static checks
go test ./...           # unit tests
```

Requires Go 1.25+ (toolchain 1.26 works). Add `-run TestName` to scope tests.

## Conventions & hard rules

- **Nothing is hardcoded.** Every coin connector (incl. BLOCK and BTC) is defined
  **entirely** by its `[TICKER]` section in `xbridge.conf`; the library only READS
  it (never creates or auto-runs `createConf`). Mirroring core-wallet XBridge
  behavior exactly is the goal (A1 session rule).
- **Fidelity over shortcuts.** Ports must be byte-for-byte 1:1 with the C++ wire
  contract. Validate against live captured packets and the C++ writers, not comments.
- **Separate concerns in commits** — logic / style (gofmt) / refactor kept apart.
- Run `gofmt` before committing.
- Amounts are base units of `COIN = 1_000_000`; `dx*` amounts display as
  **6-decimal** fixed strings (matches C++ `setprecision(
  xBridgeSignificantDigits(COIN))` = `setprecision(6)`).

## Status & repo policy

- **Local-only:** no git remote is configured. Do **not** push or open a PR unless
  the user explicitly asks. To push later:
  `git remote add origin <url> && git push -u origin main`.
- Verification of network behavior uses the **live** Blocknet service-node P2P port
  (e.g. `coreproxy.airdns.org:42111`);
