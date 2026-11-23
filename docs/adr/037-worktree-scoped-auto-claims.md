# ADR 037: Worktree-scoped automatic claims are a repository opt-in

Status: accepted, 2026-11-23. Alternative (a), the per-repository `claim.auto_scope` policy, is
the chosen design. Alternative (b), a hub-issued parent link that would exempt only one
coordinator's delegates, stays documented and is not implemented. Amends D10, D12 and ADR 033's
submodule guard consequence only for a repository that opts in. ADR 003 path identity is
unchanged, and ADR 033 still decides which repository owns a claim.

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

The repository remains the owner of the path and the policy. The exemption below is deliberately
narrow in claim origin and broad in participants: it applies to automatic claims in every worktree
of an opted-in repository, not only to a coordinator's delegates.

## Decision

1. **An owner opts a repository in.** The setting is `claim.auto_scope`, with two values:
   - `repository`, the default, keeps D10 exactly as it is;
   - `worktree`, makes only the cross-worktree automatic overlap defined below advisory.

   It lives in the hub's `Repository` record as `auto_claim_scope`, beside the existing case and
   conflict policies, not in a worktree's `.git/config`. Every worktree therefore reads the same
   policy. The hub owner sets it with
   `iris admin policy --repository <id> --claim-auto-scope repository|worktree`, through an
   owner-token administration route. A repository merge requires the two scopes to match, as it
   does the other repository policies. A change applies to subsequent checks and does not rewrite
   existing claims.

   Every behavioural change below is gated on `worktree`. Under `repository`, worktree keys are
   stored for visibility but ignored for conflict, reuse, renewal, release, guard, waiter and replay
   decisions. In particular, same-Execution automatic reuse and shell release stay path-wide across
   checkouts exactly as today. Adding a key must never split a claim or a release under the default.

   Only the owner token can write the Repository row's policy. No committed file, hook payload,
   coordination envelope or message body is an input to that decision. Hook payloads describe
   checkouts and paths, but never select the policy. Host tokens, agents and console accounts
   cannot set it. D13 still applies: on a local hub the owner token shares an OS user with the
   agents, so this is not a security boundary against a process running as that user.

2. **Only automatic claims in different, known worktrees are exempt.** With `worktree` in force,
   an overlap is advisory only when all of these hold:
   - the held claim and the incoming claim both have origin `auto`;
   - both have non-null canonical worktree keys;
   - the two keys differ.

   The exemption changes only the action on that pair. ADR 003's path keys, Unicode case folding,
   exact and prefix overlap, and component boundaries are identical under both policies. `src`
   still does not cover `src2`, and two paths that fold to one key are still the same claim path.
   Every overlap outside the exempted pair follows the current conflict rules:
   - an explicit claim stays repository-wide, whichever worktree made it and whether it is the held
     or incoming claim;
   - a held or incoming claim with a null worktree key is repository-wide;
   - two automatic claims in one worktree conflict as today;
   - only under `worktree`, same-Execution reuse of an automatic claim is restricted to the same
     worktree key; under `repository`, current path-based reuse is unchanged. An own explicit claim
     is still reused repository-wide, and the existing parent/child exemption is otherwise
     unchanged.

   An advisory is not passed through the repository's `deny`, `ask` or `warn` action. The touch
   takes or renews the caller's own automatic claim in its own worktree and returns `allow`, with
   the overlap reported separately from real conflicts. Both claims remain visible. Of concurrent
   overlapping grants, exactly one still wins unless the pair meets all three exemption conditions;
   only that pair can both win. A real conflict still applies the configured action, and R5's
   client limit is unchanged: on Codex, `ask` becomes `deny`, never `allow`.

   The hook's advisory names the path, the other holder, that claim's worktree and its branch:

   ```
   Iris: src/api.go is also auto-claimed by codex-host-delegate in worktree /src/app-api on branch delegate/api. This repository allows overlapping automatic claims across worktrees; this edit is allowed, and the overlap can meet immediately in a shared filesystem tree or surface at rebase or merge.
   ```

   The structured response carries the same holder, worktree and branch for each advisory. These
   values describe the holding claim and are displayed as data, not taken as instructions.

3. **Explicit claims and upgrades check the whole repository.** The MCP `claim` tool has no
   worktree-scoped form. Its claim has a null worktree key and conflicts across every worktree,
   including with another worktree's automatic claim. Upgrading a session's existing automatic
   claim to explicit first performs the same repository-wide check as a new explicit claim. An
   otherwise conflicting automatic claim in another worktree therefore refuses the upgrade; the
   exemption is never applied because the incoming claim is explicit. A failed upgrade leaves the
   original automatic claim unchanged. An explicit claim is never downgraded, as today.

   Under `worktree`, an exact-path upgrade uses the request checkout's key to select the caller's
   automatic row; the current own-row selection in `explicitIn` has no key. On success, that row
   becomes explicit, clears its key and keeps its display location. Every sibling automatic row of
   the same Execution, repository and path key, such as rows for one path in `/wt1` and `/wt2`, is
   revoked in the same transaction. The same revocation applies to a **new** exact explicit grant:
   an Execution with an automatic claim only in `/wt2` that is granted an explicit claim in `/wt1`
   has its `/wt2` automatic row revoked in the same transaction as the insert. This covers both
   `explicitIn`'s reuse and insert branches (`internal/store/claims.go:424-477`). Thus no keyed
   sibling remains for `releaseLeft` or `fileConflicts` to reuse or release later. Normal release
   and waiter evaluation run for the revoked siblings, but the new repository-wide explicit row
   still blocks overlapping waiters. Under `repository`, current own-row reuse is unchanged and no
   sibling-folding behaviour is introduced.

   Prefix claims keep ADR 003's case folding and component boundaries. The empty prefix still
   requires the explicit repository-wide flag, is explicit, has a null worktree key and covers the
   repository across worktrees. This decision introduces no automatic prefix or empty-prefix claim.

4. **A grouped edit remains one atomic grant, under each owning repository's policy.** ADR 033
   places every group in the repository of its file. The scope is read from that owning repository:
   a superproject's `worktree` opt-in does not narrow claims in a submodule, and a submodule's
   opt-in does not narrow its superproject. Only an exempt pair within the same owning repository
   becomes advisory.

   A rename is one idempotent grouped grant covering both ends, whether they fall in one group or
   in several. Any real conflict at either end, or in any other group of the call, grants nothing.
   An exempt automatic overlap at one or both ends is advisory and does not split the grant. Path
   identity, case folding, prefix boundaries and rename handling do not change; only the action on
   the exempted pair does. The strictest policy among repositories with a real conflict still
   decides the response, including Codex's `ask`-to-`deny` rule.

5. **Shell reconciliation shares the classification, with key-scoped release only under
   `worktree`.** `checkPaths` serves both the guard and shell reconciliation; reconcile calls it at
   `internal/store/reconcile.go:108`. The shared result distinguishes real conflicts from
   advisories. Under `worktree`, a shell reconcile can report and take an automatic claim across
   another worktree's automatic claim when the pair meets decision 2. `reconcileChanges` renders
   those advisories into the shell response's `message`, with the existing "Shell edit detected"
   and "cannot undo" wording. The current hook warns when that message is non-empty
   (`internal/hook/reconcile.go:155-156`). The guard instead keeps `message` empty for the same
   advisory-only result, as decision 6 requires. A real shell conflict keeps its current warning.
   Under `repository` the shared check has no advisories and keeps the previous same-checkout
   skip: a holder in another checkout, including a parent or child, is still a warning. Under
   `worktree` a relative Execution is skipped, matching the grant, and is not an advisory.

   Release is deliberately policy-dependent. Under `repository`, when a path leaves git status,
   the reconcile releases every matching automatic exact claim of that session on that repository
   and path, regardless of worktree key, exactly as today. One Execution claiming the same path
   from two checkouts therefore keeps the current path-wide release; no key splits it. Under
   `worktree`, the same event releases only the claim with the status checkout's worktree key. It
   then releases no explicit claim, no other session's claim, and no claim the same session holds
   on that path in another worktree. The existing refusal to attribute a status from another
   checkout to the Execution's baseline remains unchanged. Today `releaseLeft` matches only
   Execution, origin, kind and path (`internal/store/reconcile.go:193-203`), so the key filter is
   added only on the `worktree` path.

6. **The commit guard has a separate warning contract.** `iris guard` checks staged paths but makes
   no incoming claim. Its checkout supplies only the committing worktree key used for comparison.
   Currently `checkPaths` reads the holder Execution's host, worktree and branch and skips the same
   host/worktree (`internal/store/check.go:66-79`); it does not compare a claim worktree key. The
   guard also returns only `CheckResponse.Message`, and any non-empty message aborts
   `iris guard check` (`internal/guard/guard.go:202-206`, `cmd/iris/guard.go:38-46`). Those two
   interfaces must not be conflated with the edit-hook test.

   Under `repository`, the guard keeps its current holder-Execution host/worktree comparison and
   refuses every staged path claimed from another host/worktree exactly as today. Keys and
   advisories do not alter that path. Under `worktree`, it classifies each staged path against the
   owning repository's claims:
   - if every overlapping held claim is `auto`, has a non-null key, and has a key different from
     the committing checkout's non-null key, the path has advisories but no conflict; there is no
     incoming origin to test because the guard makes no claim;
   - outside the existing same-worktree exemption, an explicit claim, a held claim with a null key,
     or a committing checkout with no key is never exempt and still refuses;
   - the same-worktree exemption is applied first. It compares the claim checkout's canonical key
     when it has one. For a null-key explicit claim with a known display path, the holder
     Execution's `host_id` and that canonical path must both match the committing checkout's host
     and path; equal paths on two hosts do not qualify. A legacy row with no per-claim location
     retains the current holder-Execution host/path fallback. No fallback makes a null-key claim
     advisory.

   This settles ADR 033's own-session submodule guard case. Under `repository`, the existing
   refusal remains. Under `worktree`, a superproject session's claim placed in a submodule has that
   submodule's checkout identity, so committing in the same submodule checkout is same-worktree and
   the old refusal is lifted. A claim from a different worktree is still classified by origin and
   key. The guard still cannot identify who started Git.

   `CheckResponse` gains an additive `advisories` field. When a staged path's only overlaps are
   exempt, the hub leaves `message` empty and returns the advisories with holder, worktree and
   branch. The guard package returns that field separately to the CLI rather than folding it into
   the refusal message. A new guard prints the advisories to standard error and exits successfully.
   An older guard ignores the unknown field, sees an empty message and also allows the commit. If
   any staged path has a real conflict, `message` is non-empty and both old and new guards refuse
   as today; accompanying advisories do not weaken that refusal. Guard failures still warn and allow.

7. **A canonical worktree key is stable and separate from its display path.** The hook finds the
   git top level from the event's working directory by the existing `.git` walk. For an edit grouped
   under ADR 033, identity comes from each file's own checkout root, which can be a submodule or
   another worktree rather than the session's root. A plain touch and a shell reconcile use the
   Execution's registered worktree. A linked worktree's own git directory distinguishes it during
   discovery; the shared common git directory is not a worktree identity. A branch is display
   context only.

   A new hub-owned WorktreeIdentity record maps `(host_id, WorktreeID)` to an opaque, randomly
   generated `worktree_key`. Two hosts always receive different keys, even for the same directory.
   The hub derives the identity from the existing device-and-inode or Windows file ID; a client
   never supplies the key. Execution stores its session root's key, and each Claim stores the key
   of its own checkout. Where no usable WorktreeID establishes the directory, the key is null and
   behaviour remains repository-wide.

   Repository aliasing cannot be allowed to split that key. Today `canonicalWorktree` replaces a
   WorktreeAlias when `remote_url` or `root_commit` changes
   (`internal/store/sessions.go:846-864`). WorktreeIdentity does not participate in that
   replacement: the same `(host_id, WorktreeID)` keeps its key when repository metadata or the
   preferred display path changes. Repository resolution continues to use the existing alias rules. Reuse of a
   filesystem ID by another directory can therefore retain the old key, which may cause a false
   same-worktree conflict but cannot create a cross-worktree exemption between keys that differ.

   Claim also persists `worktree_path`, the canonical display path of its own checkout, and the
   branch observed for that checkout. These are per claim, rather than read only from Execution,
   because one Execution can edit `/wt1`, `/wt2` and a submodule. A claim group carries the branch
   it observed as optional additive metadata; if an older client omits it, the branch is unknown,
   not borrowed from a different checkout. Changing the display path or branch does not change the
   key.

   Display paths are absolute and lexically cleaned, with trailing separators removed without
   reducing the filesystem root. For comparison and legacy fallback, Windows drive-letter and UNC
   paths (`C:\repo` and `\\server\share\repo`) both use forward slashes and ADR 003's Unicode
   canonical caseless rule, `NFD(cases.Fold(NFD(path)))`, not ASCII-only folding. Drive letters are
   upper-case, the UNC server and share remain part of the root, and Win32 trailing dots and spaces
   are removed from each component. The stored display path retains its canonical
   spelling rather than the folded comparison spelling. POSIX paths retain case. On macOS,
   WorktreeID, rather than unconditional case folding, makes path spellings on a case-insensitive
   volume share an identity; case-sensitive volumes exist there too.

   ADR 034 is unchanged: a session outside git takes no claim and receives no worktree key. An
   outside-git context has no `claim.auto_scope` setting in effect.

8. **Only the `worktree` policy key-scopes claim operations.** The migration adds
   `Repository.auto_claim_scope TEXT NOT NULL DEFAULT 'repository'` with a check for the two values;
   nullable `Execution.worktree_key`; nullable `Claim.worktree_key`, `worktree_path` and
   `Claim.branch`; and the WorktreeIdentity mapping. Existing Claim rows are not backfilled with
   keys or per-claim display fields: a null key means repository-wide, including on an old automatic
   claim after its repository opts in. A legacy row's holder Execution root is not proof of the
   claim's own worktree, so its display location is unknown rather than guessed. New automatic
   claims carry their key when one can be established; explicit claims have a null
   key with a display location. Recovery and repository merge preserve keys and display locations.
   Turning the policy back to `repository` makes every live claim repository-wide without a data
   rewrite.

   Under `repository`, group de-duplication, conflict reuse, grant, renewal and replay keep their
   current repository and path identity without a worktree-key condition. In particular,
   `fileConflicts` currently reuses a same-Execution claim by path without a key
   (`internal/store/file_claims.go:206-216`), and grouped de-duplication folds equal paths across
   groups (`internal/store/file_claims.go:90-99`); the default deliberately retains both behaviours.

   Under `worktree`, those operations use repository, path identity and worktree key together. An
   Execution touching `/wt1/a.go`, `/wt2/a.go` and a submodule path neither reuses nor renews
   another checkout's automatic claim. Replay's path-based fallback and shell `autoClaim` own-claim
   renewal carry the key only on the `worktree` path. Release by fence and release-all keep their
   current ownership and generation checks under both policies.

9. **Waiters and their replay carry the key without changing the default.** Waiter gains nullable
   `worktree_key`. Under `repository`, waits, `took`, blocking, wakes and replay keep their current
   path-wide behaviour and ignore the key. Under `worktree`, an automatic wait stores the incoming
   claim's key, an explicit wait stores null, and `took` clears only the wait with the same
   repository, path and key. Advisories create no waiter. The existing
   one-wait-per-Execution/path/kind constraint remains for old-hub compatibility, so a later wait
   on another repository or key replaces the earlier one, as the current cross-repository rule
   already does.

   `blocked` (`internal/store/waiters.go:88-102`) is the single wake predicate. Under `repository`
   it is the current unrelated-overlap test. Under `worktree`, it additionally applies decision 2
   with the waiter's key and the owning repository's policy. A release wakes a waiter if and only
   if `blocked` returns false over every live claim in that repository, not merely when no real
   conflict remains for one key. Any overlapping explicit or null-key claim makes `blocked` true,
   as does any other overlap outside the sole exemption, so none can be skipped by a keyed waiter's
   wake.

   A `claim_available` Event also stores the waiter's key in a nullable `Event.worktree_key` and
   the repository that owned the wait in a nullable `Event.repository_id`. Both are necessary
   because waking deletes the Waiter while replay later reads the Event, and the Execution's
   repository is not the wait's repository when the file is in a submodule (ADR 033).
   `freedRewind` selects that key and repository with the event's path and keeps its conservative
   prefix comparison, own-claim treatment and age bound. It evaluates each event against that
   repository's claims and policy. Under `repository`, the overlap test is unchanged. Under
   `worktree`, only decision 2's exempted pair is non-blocking; every overlapping explicit or
   null-key claim still prevents the rewind. An old event with a null key retains repository-wide
   replay behaviour within the repository the event names. An event with no repository, or one
   whose repository is gone, is not rewound: guessing the Execution's repository could wake an
   edit into a claim the replay did not see, and a missed replay cannot. No claim event or wake
   is an authority input.

10. **Mixed versions fail toward repository-wide behaviour.** Registration and ADR 033 groups
    already carry worktree roots and WorktreeID, so no client-supplied scope key is added.

    An older hub has no setting or administration route for this policy. It is not safe on a
    database this hub has migrated. v0.1.13's waiter upsert replaces the wait's repository and
    leaves `worktree_key` as it was. After this hub is restored, that leftover key makes an
    explicit wait look automatic, and under `worktree` an overlap that should still block can
    wake early. Roll back only from the pre-deploy backup, which is the same rule `docs/deploy.md`
    states once a deployment has migrated. Do not point the old binary at the migrated database.

    The default `repository` scope keeps v0.1.13 conflict, reuse, release, guard and waiter
    behaviour, with two deliberate exceptions. Responses gain claim metadata: the claim's
    worktree, branch and key, `advisories` when a pair is exempt, and `claim_auto_scope`. Older
    clients ignore fields they do not know. Older guards ignore `advisories`, and a real conflict
    is still in `message`. A `claim_available` event written before this migration has no
    `repository_id`. Decision 9 no longer rewinds the first monitor for that event. The wake sent
    when the path was freed is unchanged. Substituting the Execution's repository could rewind
    into a claim the event did not see.

    A client that cannot establish a canonical identity still receives null-key, repository-wide
    claims. A new client never infers policy from a local file. The narrower behaviour needs the
    new hub, and mixed clients do not silently weaken the default.

11. **The policy and claim locations are visible.** `iris doctor` reports the `claim.auto_scope` in
    force for the checkout it examines. With `worktree`, it says that cross-worktree automatic
    overlaps are advisory and that explicit claims remain repository-wide. Against a hub that
    cannot report the setting, doctor says it is unknown rather than assuming either value.

    The console's repository data includes the policy, and the repository and claims views show it.
    Each claim row names the claim's display worktree and branch where recorded, with the host where
    needed to distinguish equal paths, and says whether its effective behaviour is worktree-scoped
    or repository-wide. A legacy row with no per-claim display path says that the location is
    unknown. A null-key or explicit claim is never presented as worktree-scoped merely because its
    holder names a worktree. Console visibility does not grant permission to change the
    owner-only setting.

## What this cannot do

- With `worktree`, overlapping automatic edits from different worktrees surface at rebase or merge
  rather than being prevented by Iris. Iris no longer prevents those collisions in that repository.
  The exemption covers every worktree, including a human's or an unrelated agent's, not only a
  coordinator's delegates.
- The same directory seen from two hosts, such as WSL and Windows, receives two keys. Those edits
  meet in one filesystem tree, but their automatic claims look cross-worktree to Iris and become
  advisory under this policy.
- The commit guard will not refuse an exempt cross-worktree automatic overlap. A new guard prints
  the advisory and allows; an older guard allows without printing it. This is deliberate removal of
  that checkpoint for the exempted pair, not enforcement.
- An advisory is information, not enforcement. The hook's allow and the guard's warning do not make
  concurrent edits safe, resolve their contents or prove that the delegates' work is disjoint.
  Explicit-claim checks and all other guard refusals remain cooperative, not a security boundary.
- A null key conservatively retains repository-wide blocking against every other session. The
  policy cannot narrow those claims for a client or filesystem that cannot supply canonical
  worktree identity. The holding session adopts its own null-key row when a later observation
  proves that checkout; that adoption is not a cross-worktree exemption.
- An explicit upgrade can be refused across worktrees. The incoming explicit claim never uses the
  automatic/automatic exemption, and sibling automatic rows are folded only after that check
  succeeds.
- Symlinked files inside worktrees are not resolved to their targets. The same underlying file can
  be edited through another path, and neither ADR 003 path identity nor this worktree key makes
  those claims meet. Worktree-root aliases are canonicalised; file symlinks are not.
- Moving a worktree across filesystems can change its WorktreeID and give it a new key. Claims made
  before and after the move can then be treated as different worktrees even at the same logical
  location, until the old claims expire or are released.
- The owner-token check is not isolation from agents running as the same OS user on a local hub
  (D13). They share that user's access; Iris does not claim otherwise.
- Existing observation limits remain: a shell edit no hook observes is unseen, hook failures follow
  D3, and a worktree or branch name is not proof of who edited a file.

## Alternatives considered

**(a) This per-repository policy.** It needs no delegate identity protocol and leaves every
repository that does not opt in under D10. The same automatic path can be claimed in several
worktrees, with the holder and location visible. It is wider than the proxy-fleet case: the owner
accepts the loss of automatic cross-worktree collision prevention for every participant in an
opted-in repository, including humans and unrelated agents.

**(b) The delegate runner holds its delegates' claims under the parent seat's identity through a
hub-issued parent link.** Parallel delegates of one coordinator would share an owner and would not
block each other, while claims from other seats would still block them. This is the narrower fit:
it preserves D10's guarantee for every seat outside the coordinator instead of exempting every
worktree in a repository.

Iris already has a narrower form of this relationship: a native subagent can register with a parent
session, the hub records `Execution.parent_id`, and related Executions do not conflict. Separately
registered proxy delegates do not acquire that link merely by being delegates. A coordination
envelope can name its caller in `callerAgentName`, but that value is delegation metadata, not a
hub-verified parent Execution or a credential. Neither a message body nor such a field may grant
identity: message bodies are untrusted data, and a delegate must not become its parent by saying so.

A safe version of this alternative therefore needs an authenticated runner registration and a
hub-issued delegation link bound to the parent and child Executions. Minting authority, proof,
expiry, revocation and the parent-end lifecycle are part of that protocol, not details that can be
inferred from `callerAgentName`. Sharing the parent as claim holder also conflates the actual
editors, fences and release authority, shares the parent's claim limit, and makes claim liveness
depend on the parent rather than the delegate that did the work.

**(c) Leave D10 unchanged and wait out short claims.** This keeps the strongest collision prevention
and needs no migration or new policy. Automatic claims already expire ten minutes after their last
touch, and delegates can message or release claims explicitly. Repeated work can nevertheless renew
a claim, and waiting repeatedly spends the idle time this decision addresses. This remains the
right behaviour for repositories that do not opt in.

**Accepted: (a), the wider, owner-accepted loss.** On 2026-11-23 the per-repository policy is the
chosen design. It solves the repository's accepted workflow without adding a new identity and
lifecycle protocol, is visible and reversible, and leaves the default unchanged. Alternative (b)
remains the documented alternative: the narrower boundary when only one coordinator's delegates
should be exempt. It preserves protection from other seats, and it should be chosen instead if that
narrower boundary is required. This ADR does not claim seat-level isolation and does not implement
(b). Alternative (c) remains the default for a repository that does not opt in.

## Consequences

- A coordinator can let delegates edit the same repository-relative path in separate worktrees
  without one delegate's automatic claim stopping another. The coordinator still has to divide the
  work and integrate it, and the same allowance applies to every other participant in that
  repository.
- Same-worktree automatic conflicts and every explicit-claim conflict keep their current meaning.
  The default `repository` policy keeps v0.1.13 conflict, reuse, release, guard and waiter
  behaviour, with the two exceptions in decision 10: responses carry claim metadata, and a
  pre-upgrade `claim_available` event without `repository_id` does not rewind the first monitor.
  Real conflicts remain all-or-nothing across grouped edits and retain Codex's `ask`-to-`deny`
  conversion.
- Under `worktree`, several automatic claims can coexist on one path in one repository. Claim
  reuse, renewal, release, waits, replay, doctor, the console and hook output carry worktree
  identity rather than implying that one Execution has only one checkout. Under `repository`, the
  same stored keys have no behavioural effect.
- Another session's null-key claim can continue to block after opt-in until it expires or is
  released. The holding session's own null-key row is adopted into the checkout that next proves
  it. This is the transition for a claim migrated from v0.1.13, not evidence that the policy failed.
- The hub owns the policy and classification. No repository content or agent message can change it,
  but the local OS-user trust boundary remains unchanged. Console claim-change triggers must also
  account for visible worktree-path, branch and key changes.

## Verification an implementation needs

- Configuration and authority: the default is `repository`; only the two values are accepted; only
  the owner token can set the policy; committed files, hook payloads, envelopes and bodies cannot;
  the policy persists across restart; changes affect new checks without rewriting claims;
  repository merge refuses different scopes.
- Conflict matrix and invariants: under both policies, cover automatic and explicit claims, exact
  and prefix overlap, empty prefixes, case folding, component boundaries, same and different
  worktrees, null and non-null keys, same Execution and related Executions, concurrent grants and a
  policy change in either direction. Prove that exactly one grant wins except for decision 2's
  automatic/non-null/different-key pair, that ADR 003 identity is unchanged, and that every real
  Codex `ask` conflict becomes `deny`. With keys present and one Execution using two checkouts,
  prove that `repository` retains today's reuse, path-wide release, guard, waiter and replay
  results.
- Explicit and grouped edits: upgrading auto to explicit checks another worktree's automatic claim,
  leaves the old claim unchanged on refusal, and under `worktree` selects by checkout key, clears
  that key and atomically revokes same-path sibling auto rows on success. Test both that upgrade
  and a new `/wt1` explicit grant while the Execution holds only a `/wt2` auto row. Prove that a
  sibling cannot later be reused or released and that the explicit row keeps waiters blocked. A
  rename is one grouped grant over both ends; any real conflict grants none. A superproject's policy does not
  affect a submodule, or vice versa. Only under `worktree` do reuse, de-duplication, replay and
  shell renewal distinguish one Execution's `/wt1`, `/wt2` and submodule claims.
- Shell and waiters: under `repository`, a status has today's path-wide release and replay; under
  `worktree`, it releases only the same session's automatic exact claim with the same repository,
  path and key, leaving explicit, other-session and other-worktree claims intact. Shared
  `checkPaths` advisories become the shell hook's non-empty-Message warning. Advisories create no
  wait; `took`, `blocked`, release wake and `freedRewind` use the key only under `worktree`; a wake
  occurs exactly when `blocked` is false, and an explicit or null-key overlap keeps it true; old
  null-key Waiter and Event rows retain repository-wide behaviour.
- Guard: with only exempt automatic overlaps, `message` is empty and `advisories` is populated; a
  new guard warns and allows and an old guard allows silently. An explicit claim, a null held key,
  a missing committing key or any other real conflict makes `message` non-empty and refuses. Test
  both policies' same-worktree behaviour, the legacy host/path fallback, equal paths on two hosts
  not skipping a null-key explicit claim, staged renames, mixed advisory plus real-conflict
  responses, and ADR 033's submodule refusal remaining under
  `repository` but lifting for the same submodule checkout under `worktree`.
- Identity: linked worktrees are distinct; branch and display-path changes do not change a key;
  remote or root alias replacement does not rekey WorktreeIdentity; symlink and bind root aliases
  share one; Windows drive-letter and UNC separators, root handling, case and Win32 component
  spellings agree; macOS case aliases share a key through WorktreeID; two hosts get different keys;
  missing identity produces null; filesystem moves and ID reuse follow the stated limits; outside
  git takes no claim.
- Migration and compatibility: existing and explicit claims have null keys, auto-to-explicit upgrade
  clears a key, recovery and merge preserve keys and display locations, and older clients work
  against the new hub. Roll back only from the pre-deploy backup. An older hub on a database this
  hub has migrated can leave a stale `worktree_key` on a wait whose repository it replaced, and
  rolling forward again can wake that wait early. Do not test rollback by running the old binary
  on the migrated database.
- Visibility: doctor distinguishes `repository`, `worktree` and unknown; the console shows each
  repository's policy and every claim's own display worktree, branch and effective scope where
  recorded, including unknown locations on legacy null rows; live updates reach the claims view.
- Run the behaviour through the real hook, guard and hub with two worktrees of one repository,
  including edit and commit advisories and the resulting pair of claims. Cover each client's output
  encoding, the store fault driver and console tests, and retain the project's full Go and web
  coverage requirements.
