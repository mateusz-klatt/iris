# 029: Cursor end-of-turn follow-up

Status: proposed on 2026-11-13 by `cursor-home-iris-2`, from a scratch measurement.
Not accepted. Would change D7 for Cursor only if accepted. No code in this draft.

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

The tier name is end of a turn, not idle. `stop` cannot wake a session that is already idle.

If this proposal is accepted, a Cursor `stop` hook may print `followup_message` only when
authoritative attention is newer than what that session was last told, once per attention
identity, with fixed content-free text. The follow-up turn's own `stop` does not print
another follow-up. `loop_count` on the `stop` payload is a backstop: a value above zero
prints no follow-up, because this measurement showed that an unconditional reply loops.
The hook stays inside the hook deadline, fails open, and does not deny the stop. A failure
prints `{}`.

Wake is opt-in with `--wake` and off otherwise, including a fresh `iris integrate cursor`,
until a later measurement shows the installed hook on a real Iris session. This draft does
not add that code.

What this does not do: it does not start a closed session, inject a keystroke, or deliver
mail while the session is sitting idle between turns. A queued follow-up can still be empty
if the turn that is ending already consumed the same attention.

## Verification

The measurement above is one interactive scratch session and one idle launch, on one CLI
version and one model. It is not a test of `encodeCursor`, of `--wake`, or of a session
whose hooks are the installed Iris hooks. `encodeCursor` still does not emit
`followup_message`. No fixture in the repository is changed by this draft.
