# ADR 039: Proposed defaults for open questions

These conservative defaults are proposed for the ADR 039 open questions so the coordinator can
review them with the owner before milestone M2 (client-side admission) lands. Milestone M1 leaves
every open question undecided in production code: client-side succession remains unimplemented/off,
and unsupported succession routes fail closed with `404 not_found` without mutation.

## 1. Enabling session-ID succession

**Question:** Which mechanism should the owner choose for enabling session-ID succession?

**Proposed default:** Gate session-ID succession behind an explicit opt-in setting (such as a
dedicated environment/config toggle that defaults to disabled) rather than enabling it automatically
from release version metadata. Gating by an explicit opt-in satisfies the upgrade prerequisite in
ADR 039 ("enable writing and using exact records only after every process that writes or prunes the
shared sessions directory uses the stage 1b lifecycle") and gives operators a reversible kill switch
while M1/M2 roll out across mixed client environments.

## 2. Hub-confirmed `SessionEnd` when local end-marker write fails

**Question:** Should the owner add a repair or later retry for a hub-confirmed `SessionEnd` whose
local end marker write fails?

**Proposed default:** Keep the conservative non-retry behavior: do not add background repair or
synthetic local end-marker rewriting after `SessionEnd` returns. Recording the failed local write as
an observation for `iris doctor` while leaving existing local evidence untouched preserves D3
execution deadlines and avoids introducing partially repaired or racy local files; any subsequent
register/succeed attempt will still observe hub-side `ended` refusals or later explicit user action.

## 3. Precedence when confirmed end coincides with damaged evidence or incomplete mandatory check

**Question:** Which hook output takes precedence when the end is independently confirmed and the
same attempt also sees damaged evidence or cannot complete another mandatory check?

**Proposed default:** When an attempt both independently confirms a local end floor and encounters
damaged evidence or an incomplete mandatory check (timeout/lock miss) within the same attempt, return
valid D3 allow output with the local failure/damage observation recorded (and grade `silent` if edit
or shell protection did not complete) while still vetoing exact-record/generation publication and
suppressing dependent hub requests. Preferring D3 allow under incomplete/damaged state upholds the
invariant that hook failures never block a session, while still guaranteeing zero forbidden hub
writes or unsafe local publication.

## 4. MCP error mapping for `succeed` refusals and unsupported routes

**Question:** Should the owner specify new MCP error mapping for a `succeed` refusal on a first
bind or for refusals other than a confirmed loss?

**Proposed default:** Retain the existing D9 MCP error vocabulary (`errLost` for confirmed loss or
local generation vetoes, `errBinding` / `unbound` when no usable binding exists on first bind) and
add no new MCP error codes in stage 1b. In particular, an older or stage-1a hub returning raw
`404 not_found` (`unknown API route`) for `POST /v1/sessions/succeed/exact` must be mapped through
existing D9 lookup rules to `errBinding` or `errLost` (and background monitor ticks remain silent)
so raw HTTP routing errors never leak to MCP callers.
