# Reconcile Git process lifetime and index inventory

Research observation, 2026-11-09. This supplements the submodule reconcile note and
ADR018; it does not implement either. Small Linux fixtures only. Git 2.53.0 and
Go 1.27.3. The cancellation helper mirrors `internal/gitinfo/status.go` at
`f6e3bd9829c2b43a6ed573c69c2001c113874bba`. The index protocol is a candidate
experiment, not current Iris behavior.

## Findings

- Default Go cancellation left real submodule Git descendants alive after `Run`
  returned. Killing a newly owned process group ended the observed tree.
- The actual Git case returned promptly despite those descendants. A separate
  non-Git inherited-stdout control demonstrated delayed `Run` return.
- `ls-files --stage -z`, including all stages, found the expected gitlinks in the
  small fixtures. All 68 status observations succeeded.
- Equal before/after path sets do not establish index identity. A private logical
  index is a stronger candidate, conditional on split dependencies, sparse
  expansion, timestamp semantics and bounded failure handling.
- These results establish neither production cost nor arbitrary concurrent,
  cross-platform or full-hook correctness.

## Linux process cancellation

The standalone helper used a 300 ms context, the current Iris Git argv, a 48 KiB
limited in-memory stdout writer, nil stderr and default `WaitDelay=0`:

```sh
git --no-optional-locks status --porcelain -z --untracked-files=all --ignore-submodules=untracked
```

The tiny repository had one initialized submodule. Its synthetic fsmonitor slept
800 ms and then deliberately spent about 40 ms CPU. The full Iris hook was not
executed; timings measure the standalone `CommandContext`/`Run` path.

| One bounded observation | Context event | `Run` returned | Descendants |
|---|---:|---:|---|
| Actual Git, default cancellation | 300.18 ms | 302.33 ms | Inner Git and monitor still alive at least 647.23 ms after return |
| Actual Git, owned process group | 301.04 ms | 301.95 ms | Observed descendants terminated before artificial CPU phase |
| Non-Git inherited stdout, default | 300.40 ms | 980.06 ms | Holder kept output pipe open until its exit |
| Non-Git inherited stdout, owned group | 301.05 ms | 308.85 ms | Parent and holder terminated |

In the actual Git case, the inner command was `git status --porcelain=2 -uno`.
Its stdout pipe differed from the outer Git pipe attached to Go. The inner Git
ultimately ended with SIGPIPE after the monitor finished. This observation does
not show a Git output-drain delay; the separate control proves only the generic
inherited-pipe risk.

The monitor's deliberately artificial CPU phase measured 40.312 ms and began
about 570.4 ms after `Run` returned. Git's own sampled CPU stayed below the 10 ms
counter resolution. This is evidence of post-return descendant CPU, not a measure
of native Git scanning or production repository CPU. Cumulative child resource usage
must not be attributed entirely to the Git process itself.

The smallest measured Linux candidate sets `SysProcAttr.Setpgid=true` before start
and replaces `Cmd.Cancel` with a signal to that owned process group, mapping
ESRCH to `os.ErrProcessDone`. It preserves the stdout writer. Product integration
still needs start/cancel races, errors and deadline acceptance checks. Processes
that escape the group and uninterruptible OS work are outside its containment;
this is not a strict total wall-time guarantee. macOS and Windows were not tested;
the Linux implementation cannot be claimed as a portable solution.

Go documents direct-process cancellation, waiting for non-file output writers and
`WaitDelay`. A finite `WaitDelay` may address pipe-drain waiting but does not itself
terminate a descendant tree; no `WaitDelay` variant was measured here.
[Go os/exec documentation](https://pkg.go.dev/os/exec).

The observer adopted and reaped its own orphans as a subreaper, unlike normal Iris.
All recorded owned PID/start identities ended; no fallback cleanup signals were
needed. The standalone helper was built once with one Go build worker, cgo and
module fetching disabled. No Iris build, project suite or production action ran.

## Candidate inventory

From the resolved worktree root:

```sh
git --no-optional-locks ls-files --stage -z
```

Parse each NUL record as `mode SP object SP stage TAB path`; split at the first TAB, not arbitrary whitespace. Collect paths with mode `160000` from **all stages**, deduplicating the path set while retaining stage/object evidence separately. The command has no `--sparse`: that option intentionally leaves sparse-directory records unexpanded. Raw byte captures retain path bytes without shell parsing. Unusual/non-UTF-8 filenames were not exercised by these fixtures; the Python harness is research instrumentation, not a proposed production parser.

| Requested state | Inventory result and qualification | Classification |
|---|---|---|
| Conflicted gitlink | `modules/one` appears three times, mode `160000`, stages **1, 2, 3**, different objects; there is no stage 0 entry | Complete for fixture; mixed gitlink/file conflict unverified |
| Actual sparse index | `--sparse` reports `040000 cold/`; ordinary inventory expands it to `cold/deep/file.txt`. Both `drop/mod` and `keep/mod` are found; excluded gitlink-containing `drop/` stayed explicit | Complete for fixture; arbitrary sparse representations unverified |
| Split index | Logical inventory resolves `modules/one`; copied overlay still references the source shared base. Private normalization succeeds before paired captures | Complete while dependencies available; unreserved base lifetime unknown |
| Linked worktree | `modules/one`; effective index resolves to `primary/.git/worktrees/linked/index`, not the primary index | Complete for fixture; primary index bytes unchanged |
| Newly staged, uncommitted gitlink | Undeclared `modules/new` is mode `160000`, stage 0, although absent from parent HEAD and `.gitmodules` | Complete for fixture |
| Removed index gitlink, directory retained | Inventory is correctly empty; HEAD still contains `modules/one`, and the initialized directory remains dirty | Complete current-index inventory; absence does not establish a clean directory |
| Nested submodule | Root inventory contains `modules/outer`; outer inventory separately contains `nested/inner` | Complete at each inspected level; root exclusion must cover descendants |

The first sparse setup enabled sparse-index configuration but produced no actual `040000` entry. Its results are retained as a negative setup check, **not** counted as proof of sparse representation support. The corrected fixture adds one excluded plain tree; it produces a genuine sparse-directory entry beside the two gitlinks. No command rewrote either source or private sparse index bytes while logically expanding the listing.

## Exact status behavior

Command, changing only `MODE`:

```sh
git --no-optional-locks status --porcelain -z --untracked-files=all --ignore-submodules=MODE
```

`\0` below means a raw NUL, and leading spaces matter. Each row was captured against the real index and the **same private copy** for that fixture; results agreed except for the deliberate source-index change described below.

| Precisely what changed | `none` | `untracked` | `dirty` | `all` |
|---|---|---|---|---|
| Conflict: root gitlink stages 1–3; tracked dirt inside initialized child | `UU modules/one\0` | same | same | same |
| Sparse: tracked edit inside included `keep/mod`; excluded `drop/mod` uninitialized | ` M keep/mod\0` | same | empty | empty |
| Nested: tracked edit inside inner; neither inner nor outer commit advanced | ` M modules/outer\0` | same | empty | empty |
| Nested: inner commit advanced; outer commit unchanged | ` M modules/outer\0` | same | empty | empty |
| Nested: outer commit then advanced to record inner gitlink; root index unchanged | ` M modules/outer\0` | same | same | empty |

Two useful refinements: CLI `all` did **not** hide the unresolved `UU` record; and root `dirty` sees an immediate submodule commit change, but hides a deeper commit while the immediate outer commit stays unchanged.

The other requested states use only the necessary `untracked`/`dirty` pair:

| State | `untracked` | `dirty` |
|---|---|---|
| Split / linked worktree, tracked child edit | ` M modules/one\0` | empty |
| Newly staged gitlink plus tracked child edit | `AM modules/new\0` | `A  modules/new\0` |
| Index gitlink removed, initialized dirty directory left | `D  modules/one\0?? modules/one/\0` | same |

The removed-directory shape is ordinary in both compared modes here. It is not evidence of a new reduced-only omission, nor evidence of clean interior contents. A scope-set transition still requires the ADR's non-attributing baseline.

## Index identity and the private copy

Every ordinary capture lists the inventory before and after status, records full mode/object/stage entries, and hashes the selected and source index files. These endpoints agreed. A separate **sequential** mutation changes the object for `modules/new`, retaining the identical gitlink path set. The source then reports `A  modules/new\0` in both modes, while the unchanged private copy reports `AM modules/new\0` in both modes: the child HEAD advanced beyond the copy's indexed object. Thus equal path sets do not mean equal logical indexes or identical status behavior. This is not an ABA/concurrency experiment.

Before/after set equality, or even endpoint byte equality, cannot exclude an intervening A→B→A history. The consistency argument below explains that limitation. The private-copy route more directly binds both commands to a selected index, but does not freeze working files, HEAD, submodule HEADs or configuration. It does not prove freshness when a snapshot reaches the hub.

The probe resolves the per-worktree index with `git rev-parse --path-format=absolute --git-path index`, then opens it once. Bytes and `fstat` timestamps come from the **same open FD**, with size/mtime/ctime checked again after reading. The private file receives that captured mtime rather than copy time; metadata, hash and copy duration are recorded. This avoids the known copy-time timestamp discrepancy in this harness; actual racy-clean behavior, timestamp precision across filesystems and normalization semantics remain **unverified** acceptance conditions, as the consistency review explains.

In the split fixture, `rev-parse --shared-index-path` **with the private `GIT_INDEX_FILE`** still names `.git/sharedindex.98aafc5cbd7997fa67e4cd6039e47b4407fd1876`. Copying only the overlay therefore does not isolate all dependencies. `git update-index --no-split-index` applied only to the private copy succeeds; the shared-index-path query then becomes empty, and paired inventory/status use this one normalized copy. Source index bytes remain unchanged. Captured mtime is restored after normalization. This proves success for the fixture, not safe dependency lifetime under concurrent shared-base cleanup or full semantic equivalence of normalization.

## Small-fixture cost, not a benchmark

Each command has a 10-second harness ceiling and records its subprocess-inclusive wall time. This was one finite main run plus its sparse correction, not a statistical performance experiment. Index copies were **206–704 bytes**. Across the captures, inventory commands took **3.43–19.23 ms**, status commands **4.35–38.46 ms**, and individual FD-read/write/mtime-preserving copies **0.20–1.65 ms**. The one private split normalization took **11.01 ms**. Full per-command timings are retained; these ranges neither predict a hook deadline nor establish production benefit. The harness performs extra inventories solely to audit the protocol; it is not an implementation cost model.

Copy cost depends on index size; expanded listing can require tree-object reads; split normalization adds work. None is safely predicted by `.gitmodules` count. No large filesystem, cold-cache, platform comparison or production measurement was performed.

## Practical completeness rule and remaining unknowns

For these stationary fixtures, the candidate listing yields the expected set. A production capture must still bound **all** work: source resolution/copy, split dependencies, sparse expansion, complete NUL parsing and status. Use one selected private index for both commands; retain `--no-optional-locks`; preserve relevant timestamp semantics. Reject unsupported/unresolved representations, missing dependencies, incomplete output, timeout or inconsistent capture by submitting **no reduced snapshot**. Never translate an unknown inventory to an empty set. Do not claim that reading stage-0 records or literal entries in an unexpanded sparse representation is complete.

**UNKNOWN outside these fixtures:** concurrent/ABA mutation; mixed-type unmerged entries; alternate index formats/versions and missing/corrupt index; arbitrary sparse and split-base lifecycle states; racy-clean copying and normalization behavior; unusual path encodings; cache extensions; frozen HEAD/config/worktree semantics; supported-platform timing. Neither a one-read claim nor production-ready implementation is established.

## Consistency argument and implementation limits

This section is reasoning from the measured behavior and public Git documentation,
not a concurrent mutation measurement.

Consider HEAD with gitlinks `p` and `q`, where `q` has tracked interior dirt. Index A
stages removal of `q`, leaving the directory; an inventory reads `{p}`. A writer
restores `q` to produce B; reduced status sees B and hides its interior dirt. A
writer then restores A; the second inventory again reads `{p}`. Equal endpoint
sets, their union, and even equal endpoint bytes do not exclude this A→B→A history.
The measured same-path/different-object case above is a separate sequential
example; the ABA history was not run.

A private index narrows this uncertainty. Under cooperating Git writers and an
atomic-rename filesystem, copying from one already-open published index handle
can retain one file generation even if its pathname is replaced. Arbitrary
in-place writers and other storage semantics need separate treatment.
[Git lockfile contract](https://git-scm.com/docs/api-lockfile).

Resolve the effective per-worktree source before installing the private override;
respect any original `GIT_INDEX_FILE`. Both commands must use the same absolute
private path, worktree, repository and invocation policy. Never silently fall back
to the live index. Normalize before either reader, if normalization is supported.
[Git path resolution](https://git-scm.com/docs/git-rev-parse),
[linked worktrees](https://git-scm.com/docs/git-worktree#_details).

A copied split overlay still depends on a shared base. The successful private
normalization in the fixture is one candidate; it does not reserve the base against
concurrent cleanup. Either capture validated dependencies, complete an accepted
normalization, or decline the attempt. Reading can update base metadata even when
index bytes remain unchanged.
[Split-index format](https://git-scm.com/docs/gitformat-index#_split_index),
[split-index maintenance](https://git-scm.com/docs/git-update-index#_split_index).

Index file mtime participates in Git's racy-clean checks. Inference: assigning a
later copy-time mtime could weaken a check that the original index would trigger.
The fixture preserved captured mtime; it did not establish full racy-clean
correctness or timestamp behavior after normalization and across filesystems.
[Git racy-clean explanation](https://git-scm.com/docs/racy-git).

Sparse-directory expansion must finish, and every active gitlink stage must be
included. Mixed gitlink/file conflicts and arbitrary sparse formats remain
unverified. Unknown or unsupported inventory must not become the empty set.
[Git ls-files options and output](https://git-scm.com/docs/git-ls-files),
[sparse-index representation](https://git-scm.com/docs/sparse-index).

A private index does not freeze HEAD, working files, submodule HEADs, ignore rules
or configuration. For example, advancing HEAD after capture can make the old
private index appear to contain staged reversals. It does not order hub deliveries
or make an old result fresh. Keep ADR018's actual invocation scope, changed-scope
baseline and prohibition on inferring clean interiors from omitted paths. This
research provides a conditional implementation direction, not production approval.

## Evidence and reproducibility

Retained logical artifact root: `adr018-implementation/` in the coordinator's
research handover. The accompanying manifest records content hashes.

- `cancellation/REPORT.json`, `REPORT.txt`, `SHA256.json`: exact commands,
  source snapshots, process identities, CPU samples and ended-process audit.
  `statusprobe.go`, `harness.py`, `delayed_fsmonitor.py` and `synthetic_pipe.py`
  are the standalone research sources. Four variant directories retain events,
  samples and per-run summaries.
- `index/run-1794252721839694643`: seven fixture states, retained initial sparse
  setup, 60 status observations; results, command list, raw stdout and source.
- `index/run-1794252793591152987`: corrected actual sparse representation,
  eight status observations and matching artifacts.
- `index/SUMMARY.json` and `REPORT.md`: exact status bytes, timings, completeness
  qualifications and source/private index hash checks.
- `consistency-review.txt`: independent reasoning and primary documentation.

The index runs comprised 367 small successful Git subprocesses including setup
and audit reads. Each had a 10-second harness ceiling; cancellation captures were
separately bounded and completed under one second. Temporary repositories used
fresh homes, isolated Git configuration and local-file transport. No network
clone, hosted model call, shared-checkout change, production measurement or project
suite was performed. All experiment processes ended. Retained fixtures are not
product test coverage, and none of these observations substitutes for the normal
PR validation required by AGENTS.md.

## One observation on a production checkout

Added on 2026-11-10, after the record above was written. It is one trial, not a benchmark.

The session that works in the checkout with the market-data submodule ran this once, in that
checkout, at a load average of 15.5 on four cores:

```sh
timeout --foreground -s KILL 0.3 git --no-optional-locks status --porcelain -z --untracked-files=all --ignore-submodules=untracked
```

It then looked in `/proc` every 50 ms for a `git status` that had not been running before the
trial. `timeout` killed the git it started after 300 ms and nothing else, as the hook did at
the time. A `git status` whose working directory was the market-data submodule ran on until
1.02 s after the trial began, and had used 1.30 s of CPU at the last sample before it ended.

Limits: one trial on one host. Another session shares that checkout, so the process could have
come from its hook, although none was running before the trial and the directory and the timing
match the trial. The hook itself was not run, and `timeout` here was the uutils implementation.
