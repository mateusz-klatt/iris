# Shell reconciliation with submodules — 2026-11-09

**MEASURED on Git 2.53.0:** `--ignore-submodules=untracked` retains tracked-file dirtiness, including changes staged inside a submodule. `dirty` hides that dirtiness but still reports a changed gitlink. Explicit CLI `all` hides even gitlinks staged in the superproject. **Per-submodule configuration `ignore=all` differs at that last boundary:** staged superproject gitlinks remain visible.

Replacing the CLI option with `-c diff.ignoreSubmodules=untracked` changes precedence. That supplies a default which per-submodule settings can override; it is not equivalent to enforcing `--ignore-submodules=untracked` across every submodule.

## Fixture and exact commands

Logical capture archive: `submodules/run-1794247098/`. Two fresh local source repositories were added as `modules/one` and `modules/two` to a fresh superproject. Each initially contained one small tracked text file. No network clone, model call, production/shared-repository operation, build, test suite or benchmark was used.

All commands ran inside this private fixture. Inherited Git environment was omitted, system/user configuration was disabled, and conflicting global configuration was supplied through a **scratch file selected by `GIT_CONFIG_GLOBAL`**. No real global configuration was edited. Hooks and commit/tag signing were disabled for fixture writes. Local file transport was opted into only for these local additions; other Git transport protocols were disallowed.

The 24 core captures use the exact requested status shape:

```sh
git --no-optional-locks status --porcelain -z --untracked-files=all --ignore-submodules=MODE
```

The configuration captures replace that final option with a command-scoped default:

```sh
git --no-optional-locks -c diff.ignoreSubmodules=untracked status --porcelain -z --untracked-files=all
```

The same change was made inside both submodules for each case. Precedence cases configure **only `modules/one`**, leaving `modules/two` as a control. `.gitmodules` configuration changes were committed before observing status, so an unrelated dirty `.gitmodules` entry does not contaminate the result. “Staged inside” and “staged superproject gitlink” are separate cases.

## MEASURED: explicit CLI mode matrix

`W` means both submodule paths were returned with the worktree-column `M` (`20 4d 20` prefix). `S` means both were returned with the index-column `M` (`4d 20 20` prefix). `—` means exactly zero stdout bytes.

| Fixture change | CLI `none` | CLI `untracked` | CLI `dirty` | CLI `all` |
|---|---:|---:|---:|---:|
| Clean | — | — | — | — |
| Tracked edit, unstaged inside each submodule | W | W | — | — |
| Tracked edit, staged **inside** each submodule | W | W | — | — |
| Untracked file inside each submodule | W | — | — | — |
| New submodule commit; superproject gitlink unstaged | W | W | W | — |
| New gitlink staged **in the superproject** | S | S | S | — |

This table describes the **CLI flag**, not every setting with the same value. In particular, its `all` column must not be generalized to per-submodule configuration.

Porcelain v1 returns the same `W` bytes for tracked dirtiness, untracked files and a changed unstaged gitlink when those changes are included. These bytes alone do not identify which underlying cause occurred. Staging a file inside the submodule does not turn its superproject record into index-column `M`; staging the gitlink in the superproject does.

## MEASURED: fallback and per-submodule precedence

Every row below uses `-c diff.ignoreSubmodules=untracked` and **no CLI `--ignore-submodules` option**. Cells list visible submodule paths; each visible record has the `W` prefix. `one + two` therefore proves the same change was detected in both fixtures, while `two` alone shows suppression scoped to `one`.

| Additional setting for `one` | Tracked unstaged edit | Untracked file | Unstaged gitlink change |
|---|---|---|---|
| None | one + two | — | one + two |
| `.git/config`: `dirty` | two | — | one + two |
| `.gitmodules`: `dirty` | two | — | one + two |
| `.gitmodules`: `dirty`; `.git/config`: `none` | one + two | one | one + two |
| Scratch global: `all` | two | — | two |
| Scratch global: `all`; `.git/config`: `none` | one + two | one | one + two |
| Scratch global: `none`; `.git/config`: `dirty` | two | — | one + two |

Thus the tested local per-submodule rule overrides both `.gitmodules` and the conflicting global per-submodule rule. Both local and `.gitmodules` rules override the generic command-scoped diff default. Global per-submodule `all` also overrides that generic default. The global-versus-`.gitmodules` conflict without a local rule was not separately tested.

For explicit CLI overrides, the fixture had `.gitmodules` setting `one=dirty` plus the generic diff default. Adding CLI `none` returned both paths for tracked edits **and** untracked files; adding CLI `all` returned no bytes for either case. These are the measured override combinations; the full cross-product of modes/scopes is not claimed.

## MEASURED: configured `all` versus CLI `all` on staged gitlinks

A final two-command comparison uses identical staged gitlinks in both submodules and `.git/config` setting `submodule.modules/one.ignore=all`. Module `two` has no ignore rule; global/system configuration is disabled. The generic diff default remains `untracked`.

| Status form | Exact decoded stdout, with NUL displayed as `\0` |
|---|---|
| Configuration only; no CLI ignore option | `M  modules/one\0M  modules/two\0` |
| Same configuration plus `--ignore-submodules=all` | empty |

Both returned 0. `staged-boundary.json` contains a separate `git diff --cached --raw --ignore-submodules=none` capture proving staged mode-160000 changes for both paths. This verifies the distinction on the installed version without relying on a documentation inference.


## Documentation and version boundary

**[DOCS]** The configuration keys and local-over-`.gitmodules` precedence are documented in
[Git 2.13.7 git-config](https://git-scm.com/docs/git-config/2.13.7) and
[Git 2.53.0 git-config](https://git-scm.com/docs/git-config/2.53.0).
The [gitmodules manual](https://git-scm.com/docs/gitmodules) distinguishes configured `all`
from the staged-gitlink case; the explicit flag distinction above is an additional runtime
measurement. These are checked documentation versions, not a claim about the first version
that supported the settings or runtime testing of historical releases.

The [git-status manual](https://git-scm.com/docs/git-status) describes the scan-scope differences,
index-refresh locking and performance options. `.gitmodules` count alone supplies no deadline
guarantee; the cost discussion below is an inference from the operations and these limits.

**[MEASUREMENT LIMITS]** All 51 tiny status captures returned 0; binary stdout, NUL-rendered text
and recorded hex agreed. `results.json` and `staged-boundary.json` in the logical archive retain
those captures; setup command/source records are retained separately. This note contains no
large-repository performance measurements and no production-checkout measurement. Nested
submodules, sparse checkout, conflicts and unusual filenames were not exercised. No inherited
Git configuration, real credentials, network clone or model was used, and all Git subprocesses
finished. Peer-reported production timings are not claimed as measurements from this fixture.

## Proposed bounded fallback

**Design only; no implementation, new probes or tests.** Read-only code baseline: `08aa3fab3f3fd11f024a6acbd07c58a63d08ce58`. Behavioral evidence is the Git 2.53.0 measurement above. The proposed cooldowns and thresholds are tuning candidates, not measured performance results.

Recommend two separate changes: first, an observable per-worktree timeout breaker with explicit snapshot-scope semantics; separately, an opt-in policy that honors per-submodule ignore configuration. Combining them would obscure whether reduced visibility came from measured timeout recovery or from user policy. Both alter D12 and require an ADR before implementation.

### Current behavior and cost

`internal/gitinfo/status.go:16–24` runs fixed CLI `--ignore-submodules=untracked`, preserves `--no-optional-locks`, parses NUL porcelain, and rejects output exceeding 48 KiB (`status.go:30`). Here **full** means that existing D12 scope: ordinary root status and submodule gitlink/tracked dirtiness, with untracked files *inside* submodules excluded. It never means complete recursive file attribution.

`internal/hook/reconcile.go:20–22,77` bounds the whole reconcile to 300 ms locally or one second for a remote hub, with an override available. Git and hub work share this budget; the older-hub path can register before running status (`reconcile.go:175–188`). A generic reconciliation timeout therefore does not establish that Git was slow.

**[INFERENCE: cost model, not benchmark]** Cost depends on root worktree/index size, the number of submodules actually examined, each submodule's worktree/index size, filesystem behavior, cache warmth, and machine load. Many small submodules and one large submodule can both be expensive. A `.gitmodules` entry count is neither an active-scan count nor a safe deadline predictor. The tiny semantics fixture supplies no throughput or latency estimate; earlier D12 timing anecdotes are not measurements made for this proposal. Reducing submodule work cannot guarantee that scanning the superproject itself finishes in budget.

### Proposed timeout breaker

Keep shared state **per physical worktree on this host**, using Iris's canonical worktree identity, not per session/process or per repository across different checkouts. Independent hook processes must observe the same decision. Keep the state small, bounded and local; reading/updating it must not introduce a blocking lock or a hub round trip on the hook deadline.

Count only a classified **started full-status deadline expiration**: Git actually started with an eligible status allowance, and that operation exceeded its deadline. Do not trip for a hub timeout, cancellation before process start, exhausted budget before work begins, a missing executable, user cancellation, parse/output-cap errors or unrelated Git failures. Record the allowance and elapsed time. A legacy-path hub call that leaves too little time for a meaningful status attempt should be reported as overall budget exhaustion, not evidence against the worktree. Classification must originate at the process boundary; the current outer `shellOverBudget` diagnostic is insufficient by itself.

| State | Proposed behavior |
|---|---|
| Full | Use the current fixed `--ignore-submodules=untracked`. A confirmed started full-status timeout opens reduced mode for later eligible hooks. |
| Reduced, cooldown active | Use literal CLI `--ignore-submodules=dirty`, within the same outer deadline. Do not repeat a full scan in every new hook process. |
| Cooldown elapsed | An eligible later hook, or an explicitly invoked doctor probe, may acquire the single cross-process trial slot. Other processes remain reduced. No background retry loop. |
| Full trial succeeds | Close the breaker, reset backoff, advance scope epoch and establish a fresh full baseline. Success does not retroactively attribute changes made while scope was reduced. |
| Full trial times out | Remain reduced and extend the bounded cooldown. Other failures retain their actual reason; they do not become fabricated Git-timeout evidence. |

A possible starting policy is one confirmed timeout followed by **60 seconds**, doubling after failed full trials to a **10-minute cap**. These values and the one-timeout threshold are unmeasured candidates. Store the next eligible time and last result; clamp invalid/stale state so clock changes or an abandoned trial cannot disable full attempts indefinitely. An idle worktree waits for its next eligible hook rather than waking a process just to probe.

Trial ownership needs atomic cross-process acquisition, an owner token and crash recovery; a plain read/write JSON flag is not enough. Acquisition must be nonblocking. Keep ownership through child cleanup, reject late results from an obsolete trial/epoch, and do not let simultaneous hooks launch multiple recovery trials. Platform-specific locking and cancellation behavior still require validation. This recovery gate does not claim that full scans already launched before the first timeout can be recalled.

If reduced status also times out, **fail open without submitting any partial or empty-as-success snapshot**. Preserve prior valid evidence and report that no fresh reconciliation completed. Do not fall back to CLI `all`: the measured command hides even staged superproject gitlinks. Preserve `--no-optional-locks`, the output cap, NUL/rename parsing, deadline cancellation and existing session usability guarantees.

### Snapshot correctness is a prerequisite

The current store persists only a status map per execution (`internal/store/reconcile.go:62–79`), compares changed codes (`:85–98`), and releases automatic claims for paths that disappear (`:102–112`). A mode switch can consequently manufacture a change on recovery or release an ownership claim when a path merely became invisible.

Require a snapshot envelope with an observation-scope identifier/epoch and enough coverage information to distinguish **not observed** from **observed clean**. A pre/post comparison may claim, warn or release only when the scopes are comparable. Prefer a stable mode within a hook pair; if a shared breaker or effective policy changes between captures, establish a new baseline and make no attribution from that transition. A late capture from an earlier epoch must not replace the newer baseline. Keep the existing shared-worktree attribution prohibition (`reconcile.go:49–55`).

**Rebaselining alone is insufficient.** During two consecutive `dirty` scans, a submodule's changed gitlink can return to its original value while tracked content inside remains dirty. Its entry then disappears even though that submodule is not known clean. Reduced mode must forbid absence-based release for the hidden submodule scope, not just for the first transition. Use verified gitlink scope; if it cannot be identified reliably, conservatively suppress absence-based releases rather than guessing. Ordinary execution leases and automatic-claim TTLs still apply. Unknown-scope evidence alone must not renew automatic claims; normal touches or observed changes may still renew them. Heartbeats renew the execution lease, not automatic-claim TTLs. This may retain an automatic claim until expiry or later complete evidence; report that limitation.

Changing between fixed CLI modes or changing an enabled configuration policy requires epoch invalidation/rebaseline. If the optional policy below is implemented, its effective configuration revision must participate; do not compare across silently changed `.gitmodules`, local/global config or command overrides. Failed, truncated or incomparable captures never become a clean baseline.

The hook and hub must agree on these semantics before reduced snapshots are submitted. Sending extra fields to an older handler is not proof it honors them. If scope-aware reconciliation is unavailable, keep the session usable and report reconciliation unavailable rather than forwarding a bare reduced map to the existing release-by-absence logic.

### Doctor and the separate configuration policy

Doctor should expose worktree identity, current full/reduced/unavailable scope, whether the cause is timeout or configured policy, reason and time of the last relevant failure, last successful full observation, next eligible full probe, and any active trial. Show that internal tracked/index submodule dirt is unobserved in reduced mode; a successful reduced scan must not display full protection. An explicit manual doctor probe can request a full attempt through the same single-trial gate and a stated finite budget. Routine report reads do not run probes.

Separately, replacing the fixed CLI option with `-c diff.ignoreSubmodules=untracked` would honor per-submodule `none`, `dirty` and `all` rules. This is useful as an **explicit policy choice**, but changes D12 coverage: `none` can re-enable costly untracked scans; `dirty` hides tracked/index dirt; configured `all` can hide gitlink worktree changes, although our measured staged-superproject boundary differs from CLI `all`. It is not a drop-in optimization or an automatic timeout response. Keep configuration policy visible independently from breaker state, and apply the same scope/rebaseline rules. The fixed reduced fallback remains literal CLI `dirty`, not a generic default another rule can broaden unexpectedly.

### Cancellation concern: SOURCE + UNVERIFIED

Go documents that `CommandContext` defaults to killing its command's `Process` and leaves `WaitDelay` unset. With zero `WaitDelay`, output pipes may remain pending until descendants close inherited descriptors. Iris currently uses `CommandContext` with a writer-backed stdout and sets no custom cancellation or wait delay (`internal/gitinfo/status.go:17–21`). This is a reason to schedule a later **bounded cancellation/descendant-pipe check on WSL and supported platforms**, not evidence that Iris currently leaks an orphan or violates its visible hook deadline. Check cleanup before claiming a trial slot safely reusable; a wait bound alone is not proof that all descendants stopped. [Go `os/exec` documentation](https://pkg.go.dev/os/exec#Cmd).

Future validation should cover the timeout classification, concurrent trial ownership, both directions of scope change, repeated reduced scans with hidden submodule dirt, reduced-mode failure, and old-hub behavior. Those are acceptance conditions for an eventual implementation; no new test or benchmark was run for this note.

## Follow-up: missing submodule declarations

Additional measurement on 2026-11-09, Git 2.53.0: eight status captures in one new isolated fixture. An initialized gitlink remains visible to full status after its worktree `.gitmodules` declaration is removed or points elsewhere; reduced `dirty` status hides its tracked dirt. A `.gitmodules`-only exclusion list is not complete.

### Tiny isolated probe

One private superproject, one initialized submodule at `modules/one`, one tracked file in that submodule. The submodule commit remained unchanged throughout; its tracked file had an unstaged edit. Eight superproject status captures, all exit 0. This was a semantics probe, not a timing or production measurement.

Exact status command, varying only `MODE`:

```sh
git --no-optional-locks status --porcelain -z --untracked-files=all --ignore-submodules=MODE
```

Here `\0` represents an actual NUL; leading spaces are significant.

| Worktree / committed metadata | `MODE=untracked` | `MODE=dirty` |
|---|---|---|
| Valid declaration; control | ` M modules/one\0` | empty |
| Worktree `.gitmodules` deleted, index copy still present | ` D .gitmodules\0 M modules/one\0` | ` D .gitmodules\0` |
| Worktree declaration changed to `path = modules/wrong` | ` M .gitmodules\0 M modules/one\0` | ` M .gitmodules\0` |
| `.gitmodules` deletion committed, initialized gitlink retained | ` M modules/one\0` | empty |

The final case eliminates metadata status noise and proves the problem is not limited to a temporary unstaged edit. `git ls-files --stage -z` then returned only:

```text
160000 11acb18ad0fc1ad93beefb77619089e34ef18396 0<TAB>modules/one<NUL>
```

The submodule's HEAD was the same object. Its own porcelain status was ` M tracked.txt\0`; its `.git` file still referenced the initialized repository under the superproject's Git directory. Superproject HEAD was `cdbe12759ba2ea9d24de5d1834e61fbfa1b29f20`. Thus this was a live, dirty indexed gitlink, not an untracked directory mistaken for a submodule.

Final-case full stdout hex: `204d206d6f64756c65732f6f6e6500`. Reduced stdout: zero bytes. The raw bytes, decoded values and hex agree for all eight captures.

### Consequence and recommended fail-open rule

**Inference from the measurement and the existing reconciliation behavior:** a missing worktree `.gitmodules` gives an empty exclusion set; the wrong-path case excludes `modules/wrong` instead of the real hidden path. Either permits a reduced snapshot to misrepresent `modules/one` as observed. If a prior snapshot contained that path, absence-based release can consequently operate on hidden dirt. This probe did not run Iris or reproduce an actual claim release.

Do not use `.gitmodules` as the authoritative inventory of paths hidden by reduced status. Obtain a bounded inventory of actual Git gitlinks, including index mode `160000`, and preserve relevant hidden paths across the two compared snapshots. Any inventory change needs the same scope-epoch/comparability treatment as a mode change. This is a design requirement, not a claim that one index-listing command alone handles every Git state.

**Fail open when scope completeness is not established:** if inventory cannot be read/validated in budget, is truncated, conflicts with the capture's index state, or otherwise leaves hidden roots uncertain, submit no reduced reconciliation snapshot. Keep the user session running. Do not convert the result to an empty clean map or release/renew claims from unknown observations; normal expiry and independently observed touches retain their usual semantics. Conservative over-exclusion is safer than under-exclusion if a future design explicitly supports it.

The inventory must share the existing deadline/output bounds and avoid optional index locks. Its cost and consistency under concurrent index changes need later validation; no claim about that overhead was measured here. `.gitmodules` can remain useful descriptive metadata, but missing/stale declarations cannot establish that no hidden submodule exists.


Logical raw bundle: `submodules/adr018/run-1794249514976774389`, with the setup source, command log, version/index evidence, raw stdout, hex, decoded values and the reviewed draft checksum. This follow-up used no production checkout, network clone, model, build, test suite or background process. All commands finished.

**UNVERIFIED:** other Git versions, undeclared newly embedded repositories, uninitialized/nested submodules, sparse indexes/checkouts, index conflicts, concurrent mutation, full/reduced inventory implementation and end-to-end claim behavior. These cases are not needed for the demonstrated counterexample and were not added to this bounded probe.
