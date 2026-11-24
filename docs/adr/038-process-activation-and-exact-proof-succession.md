# ADR 038: Process activation and exact-proof succession

Status: accepted by the owner on 2026-11-24, with the staged plan. It amends
[ADR 035](035-post-window-shim-succession.md) (Choice A) and is sequenced with
[ADR 036](036-single-session-clear-supersession.md). Until the implementation merges, `main` keeps
today's behaviour. The ADR 035 implementation (#411) is reworked to follow this ADR, starting with
the session-ID path.

## Context

The ADR 035 implementation (#411) went through three Codex review rounds. Each round closed its
findings and found new races in the client-side witness and succession model. At its last head, the
review left three findings open:

- **A late SessionStart can strand a synthetic fallback's mail.** The native `SessionStart` publishes
  its process witness before its hub registration completes. The next refresh then marks the fallback
  gone, and the mailbox handover never runs.
- **Windows can block both publication and withdrawal.** Both write the same `sessions/proc-*` file. A
  reader that opened it without `FILE_SHARE_DELETE` blocks both replacements, so the old session can
  stay eligible after `/clear`.
- **A losing shim can miss the winner.** If the winner publishes G+1 while the loser's reattach request
  is still in flight, the loser returns `lost` without confirming that winner.

These are ordering problems, which a model built on one replaceable file per process cannot settle.
This ADR replaces that model with one in which the hub decides creation and replay, and the client keeps
only evidence that it can write without replacing anything.

## Decision

Keep Choice A. Make succession one create-or-replay operation decided on the hub. Keep the synthetic
mailbox handover independent of local native-session evidence.

**Order of delivery:**
1. Ship succession first for shims that can positively identify their current native session.
2. Enable process-bound succession together with ADR 036, once both the local start intent and the
   hub activation fence exist.

### Invariants

- **One eligible hop.**
  - Only the newest exact `(generation, execution_id)` of a LOST, hook-registered native session can
    create G+1.
  - The child keeps the mailbox and repository context. It starts with zero claims and
    `hook_seen_ms = 0`.
  - It cannot produce another automatic hop until a real hook registers it.
- **End and supersession fences apply to both creation and replay.**
  - The predecessor and any child the hub returns must be strictly above `ended_through`.
  - A committed hub end, or a newer process activation, fences stale requests.
  - A newer local end or start that the shim observes before it accepts a reply vetoes adoption and
    proof publication.
  - The `.ended` marker remains a veto, as #417 and #424 define it.
- **Established bindings keep their upgrade path.**
  - Missing process evidence does not invalidate a healthy legacy binding or a verified synthetic
    fallback.
  - A process-bound automatic successor carries durable provenance, so an ordinary lookup cannot adopt
    it through the legacy exception after a failed witness check.
- **D3 and D13 still apply.**
  - Local I/O and lock waits are bounded, and no local lock is held across an HTTP request.
  - Where a failed evidence write is observable, it closes the optional recovery path. The hook still
    emits its normal ALLOW and records an observation.
  - Evidence remains self-declared, within the boundary of the host token.

### Process evidence and `/clear`

| Option | Trade-off |
|---|---|
| Replace a current-session file, plus a separate sticky withdrawal marker | **Pro:** a small change, and a create-new marker survives a reader holding the old file. **Con:** a safe reset and overlapping starts still need a protocol. Keeping the withdrawal until the process exits is simpler, but the process loses recovery. |
| Append immutable start intents locally | **Pro:** works before git discovery and while the hub is unreachable, and a held old file cannot block a fresh intent. **Con:** ordering, incomplete writes and bounded retention must all be explicit. |
| Commit a process activation on the hub | **Pro:** registration, succession and replay share one transaction order. **Con:** an offline start stays invisible until it is delivered. |
| Use a native activation ID or sequence | **Pro:** the strongest ordering, where the client exposes one. **Con:** availability and semantics vary by client. |

**The choice is immutable local intents, plus an activation fence on the hub.** Both are keyed by host,
client, verified PID and process start identity.

**On the client:**
- **The intent:** each eligible top-level `SessionStart` creates a uniquely named intent *before* git
  discovery, wake-helper I/O or any contended lock.
- **Withdrawal:** an unresolved intent withdraws the previous activation simply by existing.
- **Completion:** it is published under a second fresh name, carrying the native session ID, the
  activation ID and the expected previous activation.
- **Ordering:** a short process-local lock orders publication. Duplicate starts are idempotent, and
  competing starts with no proven order leave succession unavailable. Neither wall-clock time nor
  filename order is evidence of order.
- **Reads:** they are bounded, non-blocking and regular-file only. An incomplete record, malformed
  evidence, an incomplete scan or an exhausted scan budget each close succession.
- **Retention:** withdrawal evidence is kept for the live incarnation of the process.
- **Wake state:** Antigravity's helper JSON and Kimi's wake map remain wake state only, independent of
  this evidence.

**On the hub:**
- **The commit:** the hub commits the activation together with a positively identified native
  registration. In the same transaction it compares the expected previous activation, so a delayed
  start cannot roll the head back.
- **The shim's checks:** the shim checks the same activation before it sends a request, and again
  before it accepts any automatic successor.

**Local publication fences what the client uses. The hub commit fences what the hub creates and
replays.**

**Limits:**
- A request that was already sent can commit before the hub sees the new start.
- If neither the local intent nor the hub activation can be recorded, Iris cannot observe the
  transition.
- Process-bound succession therefore stays disabled on launchers that need a stronger guarantee and
  give no native activation evidence.

### Synthetic fallback handover

**Identity:** the fallback is identified by its canonical synthetic session ID and its verified process
tuple. A native start intent changes only *native* succession eligibility.

**While the native registration is pending:** the fallback stays bound and renewable while the
registration is pending or has failed, and while any legacy handover still awaits confirmation.

**On a capable hub,** the accepted native registration cuts the fallback over in the same transaction:
- **The Agent:** it is reused where the mailbox scope allows.
- **Where a distinct mailbox is required:** the existing delivery-transfer rules apply within the
  permitted scope. Mail stays in the original mailbox across a scope change that is not allowed.
- **Retirement:** the fallback is retired, and the exact source and destination are recorded, so a
  retry or a restarted shim can confirm the cutover.

**Edge cases:**
- Sends that race the transaction see a coherent mailbox state.
- A retired address with no valid destination returns a visible refusal.
- A fallback that has genuinely expired, or been reassigned, needs a separate ownership check before
  any transfer.

### Arbitration on the hub

**The route:** a versioned succession route, keyed by the exact predecessor pair, and for process-bound
requests by the activation ID.

**In one writer transaction,** the hub validates identity, generation, floors and activation. It then
either:
- **creates G+1,** if G is eligible; or
- **returns the recorded G+1,** if that child is still live, current and eligible for the same proof.

**Bookkeeping:**
- Predecessor provenance is recorded whether a shim or an eligible registering hook wins.
- A replay does not renew the lease, grant claims, mark the hook as reachable or emit another event.
- A recovering child follows D9 reattachment.
- An ended, expired, superseded or unrelated child is refused.

**The loser's side:**
- **Retained proof:** an established loser keeps its exact G pair across reattachment, succession and
  transport retries, and the hub resolves the race whenever the local pair advances.
- **Advancing local proof:** the shim then checks the current end floor and activation, and advances its
  local proof by an exact compare-and-swap, or accepts an identical pair that is already installed.
- **A fresh shim** can verify an exact live pair through ordinary binding.

### Schema and compatibility

**Schema:**
- **New:** nullable predecessor and creation provenance, a process activation record, and a cutover
  receipt if one is needed.
- **Unchanged:** old rows get no inferred activation evidence, and the generation-file format and the
  reattachment path stay as they are.

**An older hub:**
- **Preserved:** live binding, recovery inside the window, and the legacy handover, while both sides stay
  renewable.
- **Closed:** if the versioned route is missing, recovery after the window is closed, with no retry at
  generation zero.

**Older clients and hooks:**
- **Preserved:** their ordinary paths, while the new hub enforces Choice A over retained history.
- **Not possible:** old hooks cannot establish an activation, or supply a missing end floor.

**Restores:**
- Restore checks still need the exact lineage and the applicable activation, as
  [ADR 006](006-retention-and-restore.md) describes.
- A backup with identical retained evidence remains indistinguishable from the live hub.

## Verification

| Area | Deterministic cases |
|---|---|
| Local evidence and D3 | **Windows:** a reader holds the actual old witness. **Other cases:** an incomplete intent, a lock miss, duplicate and unordered starts, a FIFO or symlink, and a bounded ALLOW with an observation. |
| Supersession and end | **Commit orders:** both orders of `/clear` and of SessionEnd against succession. **After a rejected reply:** ordinary bind, refresh and renewal. **Edge cases:** an equal end floor, and an equal generation with a different Execution ID. |
| Concurrent winner | **Timing:** G+1 published before the loser reads, during reattachment, and after dispatch. **Winners:** both a shim winner and a hook winner, followed by a lost committed reply. **Expect:** one child ID and one set of side effects. |
| Mailbox cutover | **Setup:** pause after the native intent while the fallback receives mail and renews. **Cases:** a failed registration, a concurrent send, a lost cutover reply, a shim restart, and PID reuse. |
| Upgrade | **Directions:** both version directions. **Cases:** a healthy legacy binding without a witness, a restarted hookless shim, recovery inside the window kept, and no proofless retry. |

## Effort and sequencing

- **The session-ID path plus fallback protection:** 2–3 engineering days.
- **The full design:** 6–9 days in total, covering process activation, durable cutover, the migrations
  and the Linux and Windows test matrix.

The process-bound work is sequenced with ADR 036, so that the hub has one supersession protocol.

## Consequences

- #411 stays a draft and is not part of v0.1.15. Its hub succeed transaction, exact-proof checks and the
  reviewed hook fixes are kept as a reference for the implementation that follows this ADR.
- **The question for the owner:** whether to accept the staged plan, with the session-ID path first and
  then process activation together with ADR 036.
