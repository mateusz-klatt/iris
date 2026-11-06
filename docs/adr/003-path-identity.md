# 003: Path identity for claims

Status: proposed. It needs the owner's decision and has not been implemented.

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

## Proposal

**Claim key.** Keep `path` as written, for display. Add `path_key`, computed by the hub as
`NFD(fold(NFD(path)))`, and make `overlaps` compare keys, including prefix claims. This is the
canonical caseless match from the Unicode standard. It needs `golang.org/x/text`, which is pure Go
and so keeps `CGO_ENABLED=0`, but is a new direct dependency. A prototype matched every row above.

**Every repository, by default.** Replace `folded` with this key and make it the default.
`sensitive` stays as an opt-out for a repository that really has paths differing only by case.
`SetRepositoryPolicy` refuses to change a repository that has claims. A migration that backfills
`path_key` removes the reason for that refusal.

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

- A file reached by another name: symlinks, hard links, or a shell command whose paths no hook sees.
- Rules that differ between filesystems. The key is one rule, so it can over-match. NTFS and casefold
  ext4 were not measured.
- Windows short names such as `PROGRA~1`, and trailing dots and spaces.
- A claim made in a repository that opted out.

Hooks and the guard remain cooperative, and a hook that misses its deadline still fails open.

## Rollout

The change is in the hub, so clients need no upgrade. It needs a schema migration, so it takes effect
at a hub deploy with a restart. The console and deny messages should keep showing `path`.

## Decisions for the owner

1. Fold by default, or opt in per repository. This proposal recommends the default.
2. Accept `golang.org/x/text` as a direct dependency.
3. Accept false conflicts between case-only paths on case-sensitive hosts.
