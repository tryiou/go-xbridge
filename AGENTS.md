# CLAUDE.md

This file gives coding agents (and contributors) the rules for working in this
repository. End users should read [`README.md`](README.md); the full contributor
guide (package-by-package architecture, build/test/verify, conventions) is
[`docs/architecture.md`](docs/architecture.md).

## What this is

`go-xbridge` is a **portable, standalone Go reimplementation** of the Blocknet
**XBridge** atomic-swap engine — a **thin client** that speaks the existing
XBridge wire protocol to the live Blocknet service-node P2P network, trading
through the user's own wallets **without running `blocknetd`**. It is a
from-scratch port, **not** a wrapper. The wire contract is the source of truth
(`docs/protocol.md`); the C++ reference is the upstream Blocknet Core XBridge
source (`src/xbridge/`), where header-comment enums are frequently **stale** —
trust the actual C++ writers, not the comments.

Repo history: standalone git repo (branch `main`, remote `origin`). It was
written from scratch as a Go port of the C++ XBridge engine (`src/xbridge/`),
**not** extracted from that repo — the root commit is the original
Go scaffold.

## Build, test, verify

```bash
go build ./...          # build all packages
go vet ./...            # static checks
go test ./...           # unit tests (add -run TestName to scope)
go test -race -count=1 ./...  # CI-equivalent race run
gofmt -l .              # must print nothing
go mod tidy -diff       # module hygiene gate
golangci-lint run ./... # lint gate (.golangci.yml): errcheck, ineffassign,
                        # staticcheck, unused
```

Requires Go 1.25+ (`go.mod`: `go 1.25.0`; CI uses `1.25.x`).

The lint gate is wired into CI (`.github/workflows/ci.yml`, golangci-lint
v2.11.4, `--timeout 5m`) and must stay green: **never** silence a finding with
a blanket nolint or a `//nolint:staticcheck` without a per-site justification
(the RIPEMD-160 import sites in `coins/htlc.go` and `swap/deposit.go` are the
template — HASH160 is the on-chain/wire identifier hash, so a replacement
would break parity).

Wire/`dx*` parity is checked by the conformance suite in `conformance/`
(separate Go module, build tag `conformance`; CI runs
`go test -tags conformance -count=1 ./...` there). The full cross-repo parity
gate (`make parity`, `make canary`) lives in external tooling outside this
repo — in-repo, run the conformance module. Run it after touching the port or
C++ XBridge.

## Dead-code hygiene

Dead code is enforced by the lint gate's `unused` linter and verified
entry-point-scoped with `x/tools` deadcode:

```bash
go run golang.org/x/tools/cmd/deadcode@latest ./cmd/xbridged
```

`deadcode` is **entry-point-scoped**: it reports every symbol unreachable from
the `xbridged` main, including legitimately-exported library surface (e.g.
`wallet.LocalConnector`, `crypto.SignCompact`, the `log` logger API, the
`coins` tx-verify helpers) that an embedding app or the test suite exercises.
That is not dead code — only symbols with **no caller anywhere** (production or
tests) should be deleted, and a deletion must also drop its documentation
claims. The engine dispatch points (`processSwap`, `handleRemoteCancel`,
`handleRemoteReject`, `scanRefunds`) are the production paths; off-engine
callers marshal them onto the engine goroutine with `submit`.

## Hard rules

- **Nothing is hardcoded.** Every coin connector (incl. BLOCK and BTC) is defined
  **entirely** by its `[TICKER]` section in `xbridge.conf`; the library only
  READS it (never creates or auto-runs `createConf`). Mirroring core-wallet
  XBridge behavior exactly is the goal.
- **Fidelity over shortcuts.** Ports must be byte-for-byte 1:1 with the C++ wire
  contract. Validate against live captured packets and the C++ writers, not
  comments. Run the conformance suite (above) after touching the port or C++
  XBridge.
- **Separate concerns in commits** — logic / style (gofmt) / refactor kept apart.
- Run `gofmt` before committing.
- Amounts are base units of the coin's `COIN` (XBridge scale is 1e6; BTC is
  1e8). `dx*` amounts display with `xBridgeSignificantDigits(COIN)` fractional
  digits — the digit-count of the coin's COIN (8 for COIN=1e8, 6 for COIN=1e6).
  XBridge-scale values (`TransactionDescr::COIN` = 1e6) render fixed-6; per-coin
  amounts render at the coin's COIN precision.

## Git policy

Local repo (remote `origin` configured). Do **not** push or open a PR unless the
user explicitly asks. Commit subjects follow the repo's existing `type(scope)`
style.

## Verification of network behavior

Uses the **live** Blocknet service-node P2P port (e.g.
`coreproxy.airdns.org:42111`) — `cmd/liveprobe` for ad-hoc checks, `-node` to
pin a peer.
