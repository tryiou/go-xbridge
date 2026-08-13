# go-xbridge ↔ C++ audit home

Single audit home for the `go-xbridge` port. This directory is the **index**:
what the audit found, how bad it is, how the pieces relate, and where each
piece of evidence lives. The register of every known divergence is in
[`register.md`](register.md); the code is authoritative and status can lag it.

## How the pieces relate

| File/dir | Role | Contains |
|---|---|---|
| [`register.md`](register.md) | **Canonical register** — the single source of truth | Every finding (axis-prefixed `RPC-F01..RPC-F59`, `WIRE-F57..F71`, `STATE-F71..F79`, `CRYPTO-F77..F96`, `CFG-F84..F91`, `CONC-F92..F102`, `INV-F97..F100`, `SEC-F01..F04`), severity, status, owner branch, ID-history appendix |
| [`findings.md`](findings.md) | Per-finding detail | 130 finding cards (REF/CAND/OBSERVED/IMPACT/FIX), severity roll-up, remediation priorities |
| [`evidence/`](evidence/) | Per-axis deep dives | `rpc.md`, `wire.md`, `state.md`, `crypto.md`, `config.md`, `concurrency.md`, `inventory.md`, `wire_p1/p2.md`, `rpc-groups/` |
| [`verify/`](verify/) | Double-check reports | Independent subagent re-traces of each finding (`verify_a–e.md`) |
| [`remediation-plan.md`](remediation-plan.md) | Todo list | Branch set (B1–B11), ordering, per-branch done criteria |
| [`remediation/`](remediation/) | Branch diff briefs | Per-branch working memory (C++ sources → Go call sites → tests) |
| [`../api.md`](../api.md) | dx* contract | Stable RPC surface; Tier-3 / deliberate-divergence decisions |

## Reference pins

- **C++ (reference):** Blocknet Core @ `ac930b7f8` (4.4.1 era), `src/xbridge/`
  + `src/protocol.h`, `src/net_processing.cpp`, `src/chainparams.cpp`,
  `src/version.h` (`XBRIDGE_PROTOCOL_VERSION = 55`, `PROTOCOL_VERSION = 70713`).
- **Go (subject):** `go-xbridge` @ HEAD (`main`, module `go-xbridge`, Go 1.25+).
- **Method:** comparative read-only audit by 6 subagents + orchestrator
  re-verification of every S1 finding against the C++ writers (reports in
  `verify/`). Baseline: `go build`, `go vet`, `go test`, `go test -race`,
  gofmt — all clean; hermetic (no live-network dials). The C++ header-comment
  enums are **stale**; the actual C++ writers are the contract.

## Severity taxonomy

- **S1** — Wire/interop-breaking: a C++ node rejects or misreads Go data (order,
  deposit, signature) → swap cannot complete. MUST FIX.
- **S2** — Protocol-semantic / behavioral: identical bytes but different meaning,
  ordering, validation, or a clear JSON-shape/behavior mismatch vs C++. MUST FIX.
- **S3** — Behavioral/config/thin-client divergence. FIX or document as
  deliberate.
- **S4** — Cosmetic / doc / strictness.

## Executive summary

The **wired contract is byte-for-byte faithful**: Bitcoin P2P framing, the
`xbridge` transport envelope, the 129-byte packet header, all 21 command enums,
all 19 body layouts (validated against a live `DOGE→BLOCK` capture), the
121-byte UTXO-entry encoding, and the signing digest all match the C++ writers.
The swap **state machine** (join, two-confirmation gate, drift check, locktimes,
HTLC script bytes) is faithful, and the **deposit construction path** matches the
C++ writers: the UTXO ownership proof (CRYPTO-F93), deposit input sequence
(CRYPTO-F94), `fee2` redeem margin (CRYPTO-F95), and time-field sighash
(CRYPTO-F96) are fixed, so a Go-created order/deposit is accepted by a C++ hub.

The `dx*` JSON-RPC surface is largely at parity (error codes, dates, amounts,
status strings, envelope); the 2026 full audit (`findings.md`, `register.md`)
found **130 divergences** (1×S1, 61×S2, 61×S3, 7×S4) across the RPC, wire,
state, crypto, config, and concurrency axes, including prior-audit findings
folded into the same namespace. The B1 (servicenode-registry), B2
(wire-acceptingbody), B3 (deposit-path), B6 (secrets-hygiene), and B5
(wire-hardening) branches are merged to `main`; B4, B7–B11 execute the
remaining open rows per [`remediation-plan.md`](remediation-plan.md).

## Attacker model

Inbound signature verification *is* enforced before state mutation against a
trusted hub key, and automatic peer discovery is rate-unbounded. An attacker
can pollute the book and trigger real deposits. B3's validated-deposit gate
(`wallet.CheckDepositTransaction` on the counterparty's HTLC before committing
or redeeming our own) plus pre-signed CLTV refunds that pay the depositor's own
address make the hostile outcome **recoverable lockup + fee-burn + state
corruption + DoS**, never direct **theft** — neither party can claim a deposit
they did not validate. (Corrected from the prior lockup-only model; the 2026
audit's hostile outcome is **theft**, not recoverable lockup — see
`register.md` SEC-F03 and `remediation-plan.md`.)

## Verification gaps still open

- Live-hub verification of the swap-handshake claim/refund spends (in-memory
  connectors only today; `cmd/liveprobe` dials + handshakes but does not drive a
  swap).
- Byte-level capture of `OrderBody`/`AcceptingBody`/`CancelBody` UTXO-entry
  encoding vs a live C++ node (currently validated on one captured packet).
- Cross-check `p2p/seeds.go` fixed-IP/DNS seeds against `chainparamsseeds.h`.
- Live-hub confirm of CRYPTO-F94/F95 (`checkDepositTransaction` behavior with a
  real deposit); the writers are matched but no C++ node has accepted a Go
  deposit on-chain yet.

## Ratings

| Area | Rating |
|---|---|
| Wire protocol & transport fidelity | 9 / 10 |
| RPC / `dx*` surface parity | 7 / 10 |
| Swap state machine (machine/scripts) | 8 / 10 |
| Swap deposit/execution path | 9 / 10 (CRYPTO-F93…F96 fixed) |
| Config / coins / crypto / wallet | 7 / 10 |
| Code quality & security (production-readiness) | 8 / 10 (SEC-F01, CONC-F97–F102, STATE-F77, WIRE-F71, CRYPTO-F84 fixed; CONC-F92 documented) |
| Docs & prior-audit accuracy | 7 / 10 |
| **Readiness to trade live vs C++ network** | **6 / 10** |

## Priority remediation order

1. **S1 / CRYPTO-F77** BCH forkid signing.
2. **STATE-F72** wire the expiry sweep to a timer (`IsExpiredByBlockNumber`).
3. **RPC S2 set** error channel (RPC-F01), names, ordering, `uint256S` parser,
   split fee formula, order-history encoding/data sources.
4. ~~WIRE hardening (B5)~~ **done** — caps, magic/version gates, checksum handling
   merged on B5.
5. **CFG startup hazards (B10)** `[Rpc]` section, missing-conf, admission gates.
6. Per-branch detail: see [`remediation-plan.md`](remediation-plan.md).
