# 003: Path identity for claims

Status: accepted. The decisions below were made by `claude-holzera-iris` under the owner's delegation.

## Problem

Claims compare paths as exact strings. The default macOS filesystem does not, so two spellings of
one file are two claims. Measured on macOS 26 with the real hooks and guard: with `src/b.go` claimed,
another worktree's Edit of `SRC/B.GO` was allowed. So was an Edit of a name in NFD while the NFC name
was claimed. With `src/d2.go` claimed, a commit staging `src/D2.go` passed the guard.

`Repository.case_policy` already exists, with the values `sensitive` and `folded`. Nothing outside the
tests sets it, and it folds with `strings.ToLower`, which misses most of what APFS treats as equal.

| Pair | Same file on APFS | `strings.EqualFold` | Proposed key |
|---|---|---|---|
| `a.go`, `A.GO` | yes | yes | yes |
| `café` in NFC and NFD | yes | no | yes |
| `straße`, `STRASSE` | yes | no | yes |
| `ﬁle`, `FILE` | yes | no | yes |
| `İx`, `i̇x` | yes | no | yes |
| `σ`, `ς` | yes | yes | yes |
| `K` (Kelvin sign), `k` | yes | yes | yes |

## Decision

**Claim key.** Keep `path` as written, for display. Add `path_key`, computed by the hub as
`NFD(fold(NFD(path)))`, and make `overlaps` compare keys, including prefix claims. This is the
canonical caseless match from the Unicode standard. It needs `golang.org/x/text`, which is pure Go
and so keeps `CGO_ENABLED=0`, but is a new direct dependency. A prototype matched every row above.

**Every repository, by default.** `folded` now means this key, and every repository uses it,
including those that existed before the migration. `sensitive` stays as an opt-out for a repository
that really has paths differing only by case. `SetRepositoryPolicy` no longer refuses a repository
with claims: it recomputes their keys. If two live claims come to share a key, both stay, and only a
new claim conflicts.

The opt-out is reachable only through `Store.SetRepositoryPolicy`. There is no command, route or
console control for it, by decision: nobody needs it yet.

**Refusals show both spellings.** A hook or guard refusal reads `SRC/A.GO is held by x as src/a.go`,
and a conflict carries `path` as requested and `claimed_path` as held. A prefix claim shows its own
path in the same way.

**Detecting a case-insensitive volume.** Enforcement needs no detection, which is the main reason to
fold by default. Detection is only for a warning: `iris doctor` reports a repository left `sensitive`
that a folding host works in. The signal is `core.ignorecase` in the worktree's Git config, which
`internal/gitinfo` can read without running Git. Git set it to `true` on this volume, and
`core.precomposeunicode` too. `pathconf(_PC_CASE_SENSITIVE)` also answered correctly, but it is
macOS-only and asks about one path.

**A mixed-host repository.** This repository is one, with macOS, Linux, WSL and Windows hosts. The key
belongs to the repository, not the host. A per-host rule fails here: a Linux claim on `src/b.go` has
to stop a macOS edit of `SRC/B.GO`, and the Linux host cannot tell that the two collide. The cost
falls on case-sensitive hosts, where `a.go` and `A.GO` are different files that now conflict. Such a
repository cannot be checked out intact on macOS, so the conflict is real for anyone on a folding
host. Otherwise the repository opts out.

## Rejected

- **Resolve the spelling on the client.** `fcntl(F_GETPATH)` returned the stored name for `MIXED.GO`,
  but only for a file that exists, so two sessions creating one new file would still miss. It also
  needs a separate implementation per OS and leaves an old client unprotected.
- **Ask Git for the spelling.** `git ls-files SRC/B.GO` returned nothing here, and hooks do not run
  the Git binary.
- **Lowercase in the existing policy.** See the table.

## What this still cannot catch

- A file reached by another name: symlinks, junctions, hard links, or a shell command whose paths no
  hook sees. Measured on NTFS for a file symlink and a directory junction.
- Rules that differ between filesystems. The key is one rule, so it can over-match. On NTFS it does:
  see the table below. Casefold ext4 was not measured.
- Windows short names such as `PROGRA~1`, for a file, a directory or the worktree root. Matching them
  needs the filesystem, and a hook resolves paths lexically. Git refuses to stage a short name, so
  only the hook is affected.
- A claim made in a repository that opted out.
- An absolute path whose worktree root is spelled in another case on a case-insensitive POSIX volume,
  such as macOS. `gitinfo.Relative` compares a POSIX root exactly. A drive-letter root now compares
  without case, and drops the trailing dots and spaces that Win32 removes.

Hooks and the guard remain cooperative, and a hook that misses its deadline still fails open.

## Rollout

The change is in the hub, so clients need no upgrade. Schema version 6 applies at a hub deploy with a
restart, inside the transaction that opens the database, so a failure leaves version 5 unchanged.

The migration can be rolled back by running the older binary, which opens version 6. That binary
writes claims with no key, compares exact strings, and folds with `ToLower`, because every repository
is now `folded`. The next start of this version fills in the missing keys. It does this at every
start, because an empty key on a prefix claim would otherwise read as repository-wide.

## Decisions

1. Fold by default, in every repository.
2. Accept `golang.org/x/text` as a direct dependency. All six release targets build with
   `CGO_ENABLED=0`.
3. Accept false conflicts between case-only paths. A missed collision costs more than a false
   refusal, and `sensitive` remains the opt-out.

## Measured

On one APFS volume with the real binary, hooks, Git and guard: every row of the table was refused by
the hook and by a commit, with a same-spelling control and an empty hook failure log. A database
written by the older binary migrated in place, and its claim refused another spelling. A rollback to
that binary and forward again kept every claim. Casefold ext4 was not measured.

On NTFS, Windows 11, with 8.3 names enabled, `core.ignorecase` true, the real hook and guard, two
worktrees and a local hub. Session A edited the first name, and session B then edited the second in
its own worktree. A same-spelling control was refused and no hook failed open.

| Pair | NTFS | Hook | Guard | Reading |
|---|---|---|---|---|
| `b.go`, `B.GO` | one file | refused | refused | caught |
| `straße`, `STRASSE` | two files | refused | | false conflict |
| `İx`, `i̇x` (`i` and U+0307) | two files | refused | | false conflict |
| `İx`, `Ix` | two files | allowed | | correct |
| `ıx`, `Ix` | two files | allowed | | correct |
| `café` in NFC and NFD | two files | refused | refused | false conflict |
| `ﬁle`, `FILE` | two files | refused | | false conflict |
| `σ`, `ς` | two files | refused | | false conflict |
| `K` (Kelvin sign), `k` | two files | refused | | false conflict |
| `t.go`, `t.go.` | one file | refused after the fix | Git cannot stage it | caught |
| `s.go`, `s.go ` | one file | refused after the fix | | caught |
| `d/x.go`, `d./x.go` | one file | refused after the fix | | caught |
| backslashes, forward slashes | one file | refused | | caught |
| root in upper case | one file | refused after the fix | | caught |
| drive letter in lower case | one file | refused | | caught |
| `longfilename.go`, `LONGFI~1.GO` | one file | allowed | Git cannot stage it | missed |
| directory and root as 8.3 | one file | allowed | | missed |
| a file symlink | one file | allowed | | missed |
| a directory junction | one file | allowed | | missed |

So NTFS folds less than the key. Every Unicode row that APFS treats as one file is two files here,
and a refusal between them is a false conflict. Decision 3 accepts that cost. Trailing dots, trailing
spaces and the case of the root were missed collisions before this measurement, and are refused now.
