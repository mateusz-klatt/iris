# ADR 037: Worktree-scoped automatic claims are a repository opt-in

Status: proposed. Amends D10 only for a repository that opts in. ADR 033 still decides which
repository owns a claim.

## Context

D10 makes a claim apply across every worktree and branch of its repository. This is deliberate:
two worktrees editing one path meet at rebase or merge, which is the collision Iris exists to
prevent. D10 also allows a narrower policy per repository; this decision uses that allowance
without changing the default.

In proxy mode, one coordinator seat runs several delegates in parallel. Each delegate has its own
git worktree and branch of the same repository, and the coordinator divides the work between them.
An edit hook's automatic claim is nevertheless repository-wide. A delegate editing a path that
another delegate has auto-claimed in another worktree is refused even when the delegates are meant
to work independently. A collision commonly costs about 5–10 minutes of idle time, many times a
night.

The repository remains the owner of the path and the policy. The question is whether an automatic
claim in that repository excludes another worktree, not whether a branch or a delegate can declare
itself exempt.

## Decision

1. **An owner opts a repository in.** The setting is `claim.auto_scope`, with two values:
   - `repository`, the default, keeps D10 exactly as it is;
   - `worktree`, makes only the cross-worktree automatic overlap described below advisory.

   It lives in the hub's `Repository` record as `auto_claim_scope`, beside the existing case and
   conflict policies, not in a worktree's `.git/config`. Every worktree therefore reads the same
   policy. The hub owner sets it with
   `iris admin policy --repository <id> --claim-auto-scope repository|worktree`, through an
   owner-token administration route. Host tokens, agents and console accounts cannot change it.
   A repository merge requires the two scopes to match, as it does the other repository policies.
   A change applies to subsequent checks and does not rewrite existing claims.

2. **Only an automatic claim in another worktree becomes advisory.** With `worktree` in force, an
   edit touch or shell reconciliation classifies an overlap as advisory only when the held claim
   and the incoming automatic claim both have non-null, different worktree keys. The held claim
   must have origin `auto`. Every other overlap keeps the current conflict rules:
   - an explicit claim made with the `claim` tool stays repository-wide, whichever worktree made
     it and whether it is the held or incoming claim;
   - a claim with a null worktree key is repository-wide;
   - two automatic claims in one worktree conflict as today;
   - the existing same-Execution and parent/child exemptions are unchanged.

   An advisory is not passed through the repository's `deny`, `ask` or `warn` action. The touch
   takes or renews the caller's own automatic claim and returns `allow`, with the overlap reported
   separately from real conflicts. Both claims remain visible. An advisory creates no waiter and
   does not hold back a `claim_available` notification. Any real conflict in a grouped touch still
   prevents every grant in that call, and the strictest conflict policy still decides it (ADR 033).

   The hook's advisory names the path, the other holder, that holder's worktree and its branch:

   ```
   Iris: src/api.go is also auto-claimed by codex-host-delegate in worktree /src/app-api on branch delegate/api. This repository allows overlapping automatic claims across worktrees; this edit is allowed, and the overlap will otherwise surface at rebase or merge.
   ```

   The structured response carries the same holder, worktree and branch for each advisory. These
   values describe the holding Execution; they are displayed as data, not taken as instructions.

3. **The other claim paths use the same distinction.** A shell reconciliation can report and take
   an automatic claim across another worktree's automatic claim, but its message remains an
   after-the-fact observation and cannot undo the shell edit. The cooperative pre-commit guard
   reports the same cross-worktree automatic overlap and allows the commit. Its existing
   same-worktree exemption is unchanged, and an explicit claim in another worktree still refuses
   the commit. Leaving the guard to refuse an automatic overlap would retain enforcement that this
   policy expressly removes.

4. **A worktree key names a canonical checkout, not a branch.** The hook finds the git top level
   from the event's working directory by the existing `.git` walk. For an edit grouped under ADR
   033, the key comes from each file's own checkout root, which can be a submodule rather than the
   session's root. A plain touch and a shell reconcile use the Execution's registered worktree.
   The hub derives the key from this established checkout identity; a caller cannot supply an
   arbitrary scope key.

   The key contains the host id and the canonical absolute worktree root. The existing
   device-and-inode `WorktreeID` and `WorktreeAlias` rules first fold symlink, bind-mount and other
   path aliases to the root the host first recorded for that directory and repository. A linked
   worktree's own git directory distinguishes it during discovery; the shared common git directory
   is not a worktree key. A branch is display context only, because a branch can change without
   changing the worktree.

   Paths are lexically cleaned and trailing separators are removed without reducing a filesystem
   root. Windows keys use forward slashes, an upper-case drive letter, case folding and the Win32
   removal of trailing dots and spaces from each component. POSIX paths retain their case. On
   macOS, `WorktreeID`, rather than unconditional case folding, makes different spellings of one
   directory on a case-insensitive volume share a key; case-sensitive volumes exist there too.
   Display keeps the canonical path rather than its folded key. Where the hub cannot establish a
   canonical worktree identity, including when no usable `WorktreeID` is available, it leaves the
   key null and retains repository-wide behaviour instead of guessing that two paths are different
   worktrees.

   ADR 034 is unchanged: a session outside git takes no claim and receives no worktree key. An
   outside-git context has no `claim.auto_scope` setting in effect.

5. **The schema preserves old claims and old behaviour.** A migration adds
   `Repository.auto_claim_scope TEXT NOT NULL DEFAULT 'repository'` with a check for the two values,
   and nullable `Claim.worktree_key`. Existing rows are not backfilled: a null key means
   repository-wide, including on an old automatic claim after its repository opts in. New automatic
   claims carry their canonical key when one can be established; explicit claims carry null.
   Upgrading an automatic claim to an explicit one clears its key. Recovery, a repository merge and
   ordinary release or expiry preserve the meaning of the key. Turning the policy back to
   `repository` makes every live claim repository-wide again without a data rewrite.

   No new request field is required from clients. Registration and an ADR 033 claim group already
   carry the worktree and its identity, and the hub makes the decision. Response additions are
   additive, so older clients can still show the advisory text. A client that cannot establish a
   canonical identity keeps repository-wide claims. A new client never infers the policy from a
   local file.

   An older hub has no setting or administration route and continues repository-wide conflict
   checks. If an upgraded database is rolled back, that hub ignores the new columns and writes new
   claims with null keys; it does not honour `worktree`. Moving forward again treats all such null
   rows as repository-wide. The narrower behaviour therefore needs the new hub, but mixed clients
   do not silently weaken the default.

6. **The policy and claim locations are visible.** `iris doctor` reports the `claim.auto_scope` in
   force for the checkout it examines. With `worktree`, it says that cross-worktree automatic
   overlaps are advisory and that explicit claims remain repository-wide. Against a hub that
   cannot report the setting, doctor says it is unknown rather than assuming either value.

   The console's repository data includes the policy, and the repository and claims views show it.
   Each claim row names its holder's worktree and branch, with the host where needed to distinguish
   equal paths, and says whether that claim is worktree-scoped or repository-wide. A null-key claim
   is never presented as worktree-scoped merely because its holder's Execution names a worktree.
   Console visibility does not grant permission to change the owner-only setting.

## What this cannot do

- With `worktree`, overlapping automatic edits from different worktrees surface at rebase or merge
  rather than being prevented by Iris. Iris no longer prevents those collisions in that repository.
- An advisory is information, not enforcement. The hook's allow and the guard's warning do not make
  concurrent edits safe, resolve their contents or prove that the delegates' work is disjoint.
- The setting is not enforcement across worktrees. It deliberately removes the existing automatic
  claim barrier there; explicit claims and the commit guard remain cooperative mechanisms, not a
  security boundary.
- It is meant for coordinator-led fleets whose coordinator splits work between delegates. It is not
  a general recommendation for repositories where independent worktrees routinely change shared
  files.
- A null key conservatively retains repository-wide blocking. In particular, the policy cannot
  narrow claims for a client or filesystem that cannot supply canonical worktree identity.
- Existing limits remain: a shell edit no hook observes is unseen, hook failures follow D3, and a
  worktree or branch name is not proof of who edited a file.

## Alternatives considered

**(a) This per-repository policy.** It directly matches the unit in which parallel delegates work,
needs no delegate identity protocol, and leaves every repository that does not opt in under D10.
The same automatic path can be claimed in several worktrees, with the holder and location visible.
Its cost is the explicit loss of cross-worktree collision prevention in an opted-in repository.

**(b) The delegate runner holds its delegates' claims under the parent seat's identity.** Parallel
delegates of one coordinator would then share an owner and would not block each other, while claims
from other seats would still block them. This preserves repository-wide protection between seats
without a worktree policy. It also gives the coordinator a natural lifecycle and ownership boundary.

Iris already has a narrower form of this relationship: a native subagent can register with a parent
session, the hub records `Execution.parent_id`, and related Executions do not conflict. Separately
registered proxy delegates do not acquire that link merely by being delegates. A coordination
envelope can name its caller in `callerAgentName`, but that value is delegation metadata, not a
hub-verified parent Execution or a credential. Neither a message body nor such a field may grant
identity: message bodies are untrusted data, and a delegate must not become its parent by saying so.

A safe version of this alternative therefore needs an authenticated runner registration or a
hub-issued delegation link bound to the parent and child Executions. The runner would have to
mediate every edit, shell reconcile, renewal and release. Sharing the parent as claim holder also
conflates the actual editors, fences and release authority, shares the parent's claim limit, and
makes claim liveness depend on the parent rather than the delegate that did the work. These are
solvable, but they are a trusted delegation protocol rather than a small change to claim scope.

**(c) Leave D10 unchanged and wait out short claims.** This keeps the strongest collision prevention
and needs no migration or new policy. Automatic claims already expire ten minutes after their last
touch, and delegates can message or release claims explicitly. Repeated work can nevertheless renew
a claim, and waiting repeatedly spends the idle time this decision addresses. This remains the
right behaviour for repositories that do not opt in.

**Recommendation: choose (a).** It addresses the coordinator-led worktree fleet with an explicit,
visible and reversible repository decision. Alternative (b) is attractive if Iris later needs a
general trusted delegation model, but its identity and ownership machinery should not be replaced
by a `callerAgentName` assertion. Alternative (c) remains the default, not the answer for a fleet
that has accepted the merge-time trade-off.

## Consequences

- A coordinator can let delegates edit the same repository-relative path in separate worktrees
  without one delegate's automatic claim stopping another. The coordinator still has to divide the
  work and integrate it.
- Same-worktree automatic conflicts and every explicit-claim conflict keep their current meaning.
  The default `repository` policy keeps all current behaviour.
- Several automatic claims can coexist on one path in one repository. Doctor, the console and hook
  output must make their locations visible rather than implying that one holder excludes all others.
- Null-key claims can continue to block after opt-in until they expire or are released. This is
  intentional compatibility behaviour, not evidence that the policy failed.
- The hub owns the policy and the classification, so hooks, shell reconciliation, the commit guard,
  waiters and console output must share one interpretation. The console's claim-change triggers
  also need to account for a visible worktree-key change.

## Verification an implementation needs

- Configuration: the default is `repository`; only the two values are accepted; only the owner can
  set the policy; it persists across restart; changes affect new checks without rewriting claims;
  repository merge refuses different scopes.
- Conflict matrix: under both policies, cover automatic and explicit claims, exact and prefix
  overlap, same and different worktrees, null and non-null keys, same Execution and related
  Executions, and a policy change in either direction. Prove that `repository` is unchanged in
  every case and that `worktree` relaxes only the stated automatic cross-worktree pair.
- Edit behaviour: an advisory returns allow, names holder, worktree and branch, takes or renews the
  caller's claim, leaves both claims visible, and records no waiter. A real conflict still applies
  `deny`, `ask` or `warn`; a grouped call with one real conflict grants nothing; retries and
  recovery cannot turn an expired grant into permission.
- Shell and guard: cross-worktree automatic overlaps warn without blocking, real explicit conflicts
  keep their current result, same-worktree guard behaviour is unchanged, and waiter availability
  uses the new classification.
- Identity: linked worktrees are distinct; branch changes do not change a key; symlink and bind
  aliases share one; Windows separator, drive, case and Win32 component spellings agree; macOS case
  aliases share a key through `WorktreeID`; missing identity produces a null key; outside git takes
  no claim. Include grouped submodule claims as well as the session's own checkout.
- Migration and compatibility: existing and explicit claims have null keys, auto-to-explicit upgrade
  clears a key, recovery and merge preserve it, an older client works against the new hub, and an
  older hub on a migrated or rolled-back database remains repository-wide. Exercise rollback and
  forward again with live claims.
- Visibility: doctor distinguishes `repository`, `worktree` and unknown; the console shows each
  repository's policy and every claim's holder worktree, branch and effective scope, including
  legacy null rows; live updates reach the claims view.
- Run the behaviour through the real hook and hub with two worktrees of one repository, including
  the advisory output and the resulting pair of claims. Cover each client's output encoding, the
  store fault driver and the console tests, and retain the project's full Go and web coverage
  requirements.
