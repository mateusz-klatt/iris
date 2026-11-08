# 009: Remote execution targets

Status: accepted. The decisions below were made by `claude-holzera-iris` under the owner's
delegation, from a proposal by `claude-home-iris` (gap review D1). This ADR specifies; it builds
nothing yet.

## Problem

Iris identifies a session by the machine its hook runs on and the repository it finds there. A
client can run its tools somewhere else: a remote host, a container, a cloud sandbox. Kimi's
documentation says its hooks run on the local client machine even when tools operate in a remote
environment. The hook then sees the local checkout, while the edits land in another filesystem.
A claim would protect the wrong files, and doctor would call the client healthy.

## Decision

**1. Two identities.** The *client host* is where the hook process runs. The *execution target*
is where the tools change files. Iris protects a session only when the two are the same, or when
a path mapping from the target to a repository Iris knows is available. No such mapping exists
today.

**2. No protection is claimed where it does not exist.** When a payload shows that tools run
elsewhere, the hook claims nothing and fails open (D3). It records an observation, "remote
execution target", which doctor reports for that client, and says once in the session that Iris
does not protect it there.

**3. Evidence first.** No payload is classified as remote from documentation alone. Each client's
remote mode gets a captured payload in `internal/hook/testdata/` before its detection is written,
as for every other client behaviour.

**4. The docs say it plainly.** `docs/join-hub.md` and `docs/status.md` state that remote execution
targets, such as remote hosts, containers and cloud sandboxes, are **unprotected**: Iris neither
claims nor checks their edits.

## Consequences

- Nothing changes in code until a remote payload is captured.
- The documentation stops implying that every session is protected.
