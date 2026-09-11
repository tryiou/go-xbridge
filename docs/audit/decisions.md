# Audit decisions — rulings with rationale

Why the port diverges where it does. Code shows *what*; this file records
*why it was decided*. Each entry carries its date. New waivers need an
explicit ruling here — §0 (`register.md`) forbids accepted gaps without one.

C++ reference throughout: Blocknet Core @ `ac930b7f8` (4.4.1 era). Header
comments in `src/xbridge/` are frequently stale — the C++ *writers* are the
contract, not the comments.

## Standing waivers

| Area | Ruling | Why | Date |
|---|---|---|---|
| Trading-data family (`dxGetTradingData`, lowercase `gettradingdata`, `fee_txid`/`nodepubkey`, `blocks`/`errors` bounds, key order, error records) | WAIVED | Needs a BLOCK block index and network-wide view a thin client cannot replay | 2026-09-11 (§0 re-triage) |
| RPC-F19 non-local fills | WAIVED (split row: own-fills fix-queued) | Same reason: only fills this node saw on its P2P feed exist locally | 2026-09-11 |
| `dxGetTokenBalances` `Wallet` key (RPC-F23/F24) | NEVER SYNTHESIZED — closed by design | C++ `Wallet` names the embedded core wallet of the combined core+xbridge binary; in this standalone port BLOCK is just another `[TICKER]` connector and its balance is already reported under its own ticker — a `Wallet` key would name an entity that does not exist | 2026-09-11 |

## Pending rulings (refuse loudly meanwhile)

| Area | Stance until ruled | Why it may be unfixable |
|---|---|---|
| PART (`CRYPTO-F98`) | `[PART]` refused at admission, never malformed broadcast | `XParticlTransaction` serialization + confidential outputs + amount-committing digest not portable to `coins.Tx` |
| BCD (`CRYPTO-F99`) | `[BCD]` refused at admission | Fork-version serialization (`CURRENT_VERSION_FORK` `preBlockHash` field) not portable to `coins.Tx` |
| DCR / STEALTH / XST | Refused at admission, no register row (deliberate) | DCR not in the live manifest; STEALTH/XST non-portable or absent |
| Dust source (`CRYPTO-F102`) | 5460 fallback; keep swap amounts well above dust | Thin client has no relay-fee feed; C++ derives dust live |
| 1 MiB XBridge body cap (`WIRE-F65`) | Hardening stands | No legit swap body approaches it; C++ has no cap |
| cmd-4 dual writer (`WIRE-F66`) | Reader tolerates both 126 B and 134 B forms | The 126 B form is emitted by live C++ broadcast writers — rejecting it drops real broadcasts |
| Outbound SENDHEADERS/SENDCMPCT/pings (`WIRE-F72`) | Never sent; inbound pings answered | The XBridge engine never acts on them; multi-hour live peerings stay connected |
| Bare `help` list (`RPC-F60`) | This-node subset only | Mirroring Core's full command list from a thin client is a claim about commands this node doesn't serve |
| `localservices` bits (`RPC-F61`) | Thin-client bits | Bits report the reporting node by construction — cannot echo a full node's |
| `use_count` (`RPC-F62`) | Constant 1 | Debug-only field; Go has no shared_ptr refcount to report |

## Judgments (decided, load-bearing)

- **Threat outcome is theft, not lockup (2026-08-13, B3).** The validated-deposit
  gate (`wallet.CheckDepositTransaction` before committing or redeeming our own)
  plus pre-signed CLTV refunds paying the depositor's own address mean neither
  party can claim a deposit they did not validate. Corrected from the earlier
  lockup-only model.
- **Init `&&` bug-for-bug (STATE-F84, open).** C++ rejects only when *every*
  field mismatches; matching that re-admits single-field-mismatch packets. It is
  still required for interop (a Core hub emits what C++ accepts; rejecting them
  stalls our swaps), amounts are re-verified at deposit/claim regardless — but
  implementation needs explicit security sign-off, recorded here when given.
- **Secrets always persist (2026-09-10, B6 amendment).** The `-persistsecrets`
  opt-out was removed for strict `orders.dat` parity: a restarted mid-flight
  swap must always auto-refund and re-sign cancels. Refund/claim hex stays out
  of logs (recovery transcript `log-tx/` carries it); privkeys/preimages stay
  out permanently.
- **Never create `xbridge.conf` (B10).** C++ scaffolds a template on first run;
  go-xbridge fatals on a missing file instead. Startup posture with zero
  swap-sequence effect — deliberate, not a gap.
- **No cookie auth (B4).** Unlike C++, there is no auto-cookie fallback; loopback
  without credentials is open by design, non-loopback without auth warns. Same
  justification as above: process-local posture, zero swap-sequence effect.
- **Durable-write-before-send is identical by construction (2026-09-11).**
  Backs the §0 durability exclusion in `register.md`: the disk write precedes
  the same send C++ performs, so wire bytes, state transitions, and RPC shapes
  are unaffected — there is no divergence to fix, only a local crash window
  C++ also closes via `orders.dat` (`saveOrders`/`loadOrders`). Go mirrors the
  resume side (`persist()` every 60 s, `restoreLocalSwaps`). Strictly additive
  robustness, not a §0 observable.
