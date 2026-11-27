# ADR 041: Criteria for Iris 1.0

Status: accepted by the owner on 2026-11-27, together with live, billable client measurements in batches.

**In short.** Iris stays at 0.x until all six criteria below hold. 1.0.0 is a promise that clients and operators can rely on the hub's contract and its rollback path. These criteria are the minimum evidence for that promise.

## Context

- **v0.1.17** is the current release, deployed on 2026-11-27.
- **No 1.0 bar was written down before this ADR.** Neither `docs/` nor the ADRs, nor the ADR 039 plan, said what would make a release final.
- **What remains open:**
  - Session succession ([ADR 039](039-session-succession-client-admission.md) stage 1b) is still being implemented, and it defaults off.
  - Host tokens are not bound to their host ([ADR 040](040-host-bound-tokens.md), [#442](https://github.com/mateusz-klatt/iris/issues/442)).
  - An authoritative claim refusal still fails open ([#464](https://github.com/mateusz-klatt/iris/issues/464)).
  - Several client paths in [status.md](../status.md) are marked "not measured".

## Decision

Iris 1.0.0 is tagged only when all of the following hold:

1. **Session succession is on across the fleet and stable.**
   - ADR 039 stage 1b has shipped.
   - The owner has enabled `session_succession` on every fleet host.
   - The fleet has run with it for several days without a regression attributed to succession.
2. **Host tokens are bound to their host.** ADR 040 is implemented, and enforcement is switched on in production. A host token can no longer act for another host's sessions.
3. **Authoritative claim refusals deny the edit.** #464 is closed. Tool calls carry a per-occurrence identity, and `lost` and verified `key_reused` refusals of a pre-edit claim deny the edit. Every other failure keeps D3 fail-open.
4. **The compatibility and rollback policy is written down.**
   - A document states what 1.x keeps compatible:
     - the MCP tools;
     - the HTTP API and its error codes;
     - the hook outputs per client;
     - `client.json`;
     - the database schema across upgrades.
   - It also states how each release says whether rolling back the image alone is supported or a backup restore is needed.
   - A change that breaks the policy needs a new major version.
5. **One release candidate runs in production unchanged.** A `1.0.0-rc` tag is deployed to the production hub and the fleet's CLIs. The final tag points at the same commit as the candidate. A fix found during the candidate means a new candidate.
6. **Every supported client path is measured, or marked unsupported.** Each row in `status.md` that says "not measured" falls into one of two groups:
   - measured live on Linux, macOS and Windows, with the evidence recorded;
   - marked unsupported on that system, with the reason.

   This covers the wake and hook integration rows of every client that Iris integrates. Distribution checks (Defender, SmartScreen, Gatekeeper) and service start-up are tracked separately and are not part of this criterion.

## Consequences

- **The expected order:**
  - 0.1.18 ships ADR 039 stage 1b with succession off by default. The owner then enables it.
  - A later 0.x release implements ADR 040 and #464.
  - Then the compatibility document, the release candidate and the final tag.
- **Measurements spend client budget.**
  - The owner approved them in batches.
  - Each batch starts with a usage check and follows the fleet's routing for model and effort.
  - The seats split the work by operating system.
- **A criterion can only be dropped or changed by a new decision recorded here.** Neither a deadline nor a single failing measurement changes it. Marking a path unsupported is allowed, and saying so is preferred to leaving it unmeasured.
- **This ADR sets no date.**
