# Reduced shell reconcile scope on WSL

Measured on 2026-11-10 from `feat/reduced-reconcile-hook` at `43743f8`, with `main` at `254271b`
for comparison. One machine and one run of each binary, so this is an observation, not a
benchmark. [ADR 018](../adr/018-reduced-reconcile-scope.md) asks for it under Verification.

## Setup

- WSL 2, kernel 6.6.87.2-microsoft-standard-WSL2, 16 CPUs. Git 2.53.0, Go 1.27.3. The fixture was
  on ext4 on the WSL disk, checked with `statfs` (0xef53), not under `/mnt/c`.
- The harness is `TestReducedScopeMeasurement` in `cmd/iris/shell_scope_measure_linux_test.go`. It
  runs only with `IRIS_MEASURE_SCOPE=1`, builds the branch's binary, generates the fixture, and
  writes `report.json` with every hook's figures. That file is not kept; the figures below come
  from it.
- The fixture is a superproject with seven submodules. One holds 448 MiB in four files rewritten
  with the same content after the checkout. Their stat no longer matches the index, and nothing
  refreshes it, because the hooks and the warm-up pass `--no-optional-locks`. So every status that
  looks inside that submodule hashes the files again. The six others hold one small file each.
  The fixture is synthetic. The production checkout's 1.04 s status was mostly system time, while
  this one's cost is user time spent hashing.
- Warm-up ran the hook's two statuses directly, three times each. The full one took 874 to 898 ms
  of wall time, and as much CPU. The reduced one took 2.0 to 2.5 ms.
- The hub ran in the harness's process on loopback, so the budget was 300 ms, with a scratch
  database and token. A second session, registered directly, held `top.txt`, `attributed.txt` and
  `baseline.txt`, so a shell command's edit to one of them is a conflict the hook should report.
- Each binary had a token directory of its own, so its session memory, breaker and capability
  started empty. A SessionStart hook registered first, which taught the branch's hook that the hub
  takes reduced snapshots.
- A command was a pre-tool hook, the command's edit if it made one, and a post-tool hook. Each hook
  was a real `iris hook --client claude` process.
- CPU: the harness is a child subreaper (`PR_SET_CHILD_SUBREAPER`). Before each hook it reaped what
  earlier work had left, and after it reaped everything reparented to it. The growth of its
  `RUSAGE_CHILDREN` therefore covers the hook, the git processes it started, and any git that
  outlived its parent.
- Processes: right after each hook exited, every process below the harness was listed from
  `/proc` with its pid and start time. None was allowed to be alive 100 ms later, nor before the
  next hook started. The git processes a hook waited for itself are not seen one by one; their
  CPU is in the hook's.

To reproduce, with `<main>` a directory for the comparison binary and `<dir>` one on the disk to
measure:

```sh
git archive 254271b | tar -x -C <main>
(cd <main> && CGO_ENABLED=0 go build -trimpath -o <main>/iris ./cmd/iris)
IRIS_MEASURE_SCOPE=1 IRIS_MEASURE_SCOPE_DIR=<dir> IRIS_MEASURE_SCOPE_BASELINE=<main>/iris \
  CGO_ENABLED=0 go test -count=1 -timeout 20m -run '^TestReducedScopeMeasurement$' -v ./cmd/iris
```

## Phases

On the branch's binary:

1. The cold trip: one command, with no breaker yet.
2. Steady reduced: 30 commands within the cooldown the trip set.
3. A wait until the cooldown was over, a minute after the trip.
4. The due full attempt: one command.

On `main`'s binary: 30 commands. In each run of 30, command 15 edited a file the other session
held: `attributed.txt` on the branch, `baseline.txt` on `main`. The cold trip's command edited
`top.txt`.

## Results

| Binary | Phase | Hook | Hooks | Wall p50 / p95 / max, ms | CPU p50 / max, ms | Past 300 ms | Warned |
|---|---|---|---:|---:|---:|---:|---:|
| branch | cold trip | pre | 1 | 304.9 | 307.0 | 1 | 0 |
| branch | cold trip | post | 1 | 13.8 | 9.8 | 0 | 0 |
| branch | steady reduced | pre | 30 | 14.7 / 28.7 / 54.4 | 9.6 / 10.8 | 0 | 0 |
| branch | steady reduced | post | 30 | 14.5 / 17.5 / 27.6 | 9.6 / 10.3 | 0 | 1 |
| branch | due full attempt | pre | 1 | 305.0 | 307.1 | 1 | 0 |
| branch | due full attempt | post | 1 | 14.3 | 9.4 | 0 | 0 |
| `main` | 30 commands | pre | 30 | 304.6 / 304.8 / 305.0 | 306.7 / 307.7 | 30 | 0 |
| `main` | 30 commands | post | 30 | 304.6 / 304.9 / 305.3 | 306.7 / 307.5 | 30 | 0 |

The SessionStart hook took 9.9 ms on the branch and 10.8 ms on `main`. Every hook exited 0 with
valid output. Each hook past 300 ms logged one entry, `shell status: shell reconciliation passed
its deadline`, and no other hook logged any.

- **Cost per command.** The 30 steady commands used 576 ms of CPU in all on the branch, 19 ms each,
  against 18.4 s on `main`, 614 ms each. A hook whose full status is killed costs about 307 ms of
  CPU, a little more than its wall time, because git hashed on another core until the deadline.
- **The breaker.** The cold trip's pre-tool hook ran out of budget and tripped it: reduced, a
  cooldown of 60 s, reason "a full git status passed its 300ms budget". All 30 steady commands ran
  within that cooldown. The first hook after it ran full, ran out of budget again, and doubled the
  cooldown to 120 s.
- **Attribution.** On the branch, command 15's post-tool hook warned about `attributed.txt`. The
  cold trip's edit of `top.txt` was not attributed: its pre-tool hook sent nothing, and its
  post-tool hook only set the first reduced baseline. On `main`, command 15's edit of
  `baseline.txt` was not attributed, because every status ran out of budget.
- **Processes.** No process below the harness was alive 100 ms after any of the 126 hooks, nor
  before any of them started. Each status killed at its deadline left git processes that had
  outlived their parent: one for each of the branch's two killed statuses, and 90 for `main`'s 60.
  All had exited by the time the hook did; they were reparented to the harness and reaped.

Two earlier runs of the harness, on `e989799` and on `43743f8`, before it reaped ahead of each
hook, gave the same figures within a few milliseconds. In the second of them the first hook also
reaped eight git processes whose start times fall in the harness's own setup, before that hook
ran. That is why the harness now reaps before each hook and fails if anything is left then.

## What this does not show

- Windows, where Iris takes no reduced snapshot, and macOS, where none of this was measured.
  Nor other git versions, or the total cost of hooks running at once.
- The production checkout itself: this fixture is slow for another reason than it is.
- A hub across a network, with its 1 s budget.
- That no git process outlives the hook: the check covers 100 ms after each exit, on this machine.
