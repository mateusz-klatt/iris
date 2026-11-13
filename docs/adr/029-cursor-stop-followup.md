# 029: Cursor end-of-turn follow-up

Status: accepted on 2026-11-13 (holzera). The implementation follows in its own PR.

## Problem

Cursor hooks and MCP can add context on a later turn. They do not start a turn in a session
that is already idle. Cursor's `stop` hook can return `followup_message`, which the client
may treat as another turn at the moment a turn ends.

That is not an idle wake. A session which has not started a turn does not fire `stop`, so
there is nothing to attach a follow-up to.

## Measurement

Cursor Agent `2026.11.11-b912975`, model `composer-2.5-fast`, on Linux under WSL, on
2026-11-13. The process ran under a PTY with `--trust --sandbox disabled --force`. It was
not `cursor-agent -p`. `HOME` and `XDG_CONFIG_HOME` were a scratch directory with a copied
login and no user hooks, so the installed Iris hooks did not run. The scratch workspace had
its own `.cursor/hooks.json`. The hook appended each payload to a local log. On `stop` it
printed one fixed `followup_message` telling the model to reply `FOLLOWED` and not to use
tools. Other events printed `{}`. The log is not a fixture. Payloads below omit the email,
the transcript path and the workspace path.

An idle launch, killed after about 20 seconds, fired `sessionStart` and then `sessionEnd`
with `reason` and `final_status` `aborted` and `duration_ms` 18366. It did not fire `stop`.

A launch whose initial prompt was `Reply with exactly the word PING and nothing else. Do not use tools.` fired:

| UTC time | Event | What was visible |
|---|---|---|
| 19:41:22 | `beforeSubmitPrompt` | that prompt |
| 19:41:25 | `afterAgentResponse` | text `PING` (9691 input tokens, 29 output) |
| 19:41:25 | `stop` | `status` `completed`, `loop_count` 0 |
| 19:41:26 | `afterAgentResponse` | text `FOLLOWED` |
| 19:41:26 | `stop` | `loop_count` 1 |
| 19:41:28 | `afterAgentResponse` | text `FOLLOWED` |
| 19:41:28 | `stop` | `loop_count` 2 |
| 19:41:30 | `afterAgentResponse` | text `FOLLOWED` |
| 19:41:30 | `stop` | `loop_count` 3 |
| 19:41:32 | `afterAgentResponse` | text `FOLLOWED` |
| 19:41:32 | `stop` | `loop_count` 4 |
| 19:41:35 | `afterAgentResponse` | text `FOLLOWED`, and no further `stop` |

The process then stayed up until it was killed at 150 seconds. `sessionEnd` was `aborted`
with `duration_ms` 149557. The model saw the follow-up text: each of those turns answered
`FOLLOWED`. Returning `followup_message` on every `stop` repeated the turn. Earlier `-p`
runs still stand: they did not fire `stop`. This run does not replace that result.

## Decision

The tier is end of turn. An idle session still waits for the next prompt. `stop` fires when
a turn ends, and it does not fire again while the session sits idle after that. New mail
during that wait has no `stop` to answer. This cannot wake an already idle session.

`loop_count` greater than zero must never print a follow-up. The measurement returned
`followup_message` on every `stop`. The client then ran another turn, and the model answered
`FOLLOWED`, at `loop_count` 1, 2, 3 and 4, and once more after that with no further `stop`.
A follow-up is itself a turn that ends in `stop`. Printing another follow-up there is how
the loop happened. `loop_count` above zero means this `stop` is already a follow-up, so the
hook prints `{}`.

"Attention newer than what the session was last told" is the same identity the next-turn
tier already uses. The hub names the newest pending attention. The session's told state is
the identity recorded beside its generation. The `stop` hook may print `followup_message`
only when that newest identity is newer than the recorded one. Printing it records the
identity, so the follow-up turn's own `stop` sees the same identity and prints nothing.
The comparison is the hook's ordinary read under the sessions-directory lock. It stays
inside the hook's 1.5 s internal deadline. Cursor's installed hook timeout is 3 s. A miss,
a timeout or any other failure prints `{}` and does not deny the stop. Cursor's own
timeout result is still unknown. This proposal does not depend on a timeout denying the turn.

On 2026-11-13, about 20:20 UTC, the owner decided that a CLI Iris can wake is on by default
unless that implementation is incomplete or does not work. For Cursor the default turns on
once the installed hooks are verified live on a scratch session. A fresh `iris integrate
cursor` and `iris setup` then write wake on. `--no-wake` turns it off, and reintegration
keeps an explicit choice. This ADR does not add that code. The implementation PR defaults on.

What this does not do: it does not start a closed session, inject a keystroke, or deliver
mail while the session is sitting idle between turns. A queued follow-up can still be empty
if the turn that is ending already consumed the same attention.

## Verification

The measurement above is one interactive scratch session and one idle launch, on one CLI
version and one model. It is not a test of `encodeCursor`, of `--wake`, or of a session
whose hooks are the installed Iris hooks. `encodeCursor` still does not emit
`followup_message`. No fixture in the repository is changed by this draft.
