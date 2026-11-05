# GitHub Copilot CLI report (1.0.105)

> Added by the orchestrator, who ran the experiment directly because nesting was refused: hooks fire under `-p --trust-folder`. A JSON deny blocked the tool. A hook exiting 1 also denied it ("hook errored"). `toolArgs` is a JSON object for most tools and a raw patch **string** for `apply_patch`. Hook `sessionId` equals `COPILOT_AGENT_SESSION_ID` in the stdio MCP environment, with the same parent pid. Section A and C statements marked as documentation-only are therefore confirmed.

**Status:** partial, not an adapter specification or golden-event corpus. [MEASURED: `copilot --no-auto-update --version`] The installed version is 1.0.105. [MEASURED: attempted `copilot -p` twice] The execution tool denied both nested launches before process start; no hook, deny or process-tree payload was captured. `fixtures/` contains the unexecuted harness and two real non-hook CLI outputs. Current web docs may describe behavior newer than this binary.

## A. Facts

**A1 Hooks.** [DOCS https://docs.github.com/en/copilot/reference/hooks-reference] Policy, user, repository and plugin hooks are combined, not overriding one another; policy loads first and cannot be disabled by `disableAllHooks`. Sources include `/etc/github-copilot/policy.d/*.json` (Linux/macOS), `C:\ProgramData\GitHub\Copilot\policy.d\*.json` and Windows registry policy, `.github/hooks/*.json`, `~/.copilot/hooks/*.json` or `%USERPROFILE%\.copilot\hooks\*.json` (`COPILOT_HOME` overrides), repo/user settings and plugin hooks. Project hooks require workspace admission; [MEASURED: `copilot --help`] `--trust-folder` admits one prompt-mode session without saving trust.

[DOCS https://docs.github.com/en/copilot/reference/hooks-reference] Events are `sessionStart`, `sessionEnd`, `userPromptSubmitted`, `userPromptTransformed`, `preToolUse`, `postToolUse`, `postToolUseFailure`, `agentStop`, `subagentStart`, `subagentStop`, `errorOccurred`, `preCompact`, `permissionRequest`, and `notification`. Command, direct `exec`/`args`, HTTP and, on `sessionStart`, prompt handlers exist. Native matchers are anchored regexes on supported events; PascalCase compatibility has different tool matching. Notification is asynchronous; `/clear` dispatches session-end detached. Prompt-type start hooks do **not** run in headless mode; command hooks are documented to.

[DOCS https://docs.github.com/en/copilot/reference/hooks-reference] camelCase inputs include `sessionId`, millisecond `timestamp`, `cwd`; pre-tool adds `toolName` and `toolArgs`. PascalCase event names instead yield snake_case fields. Start accepts `additionalContext`; pre-tool accepts `permissionDecision: allow|deny|ask` and deny requires `permissionDecisionReason`; stop uses `decision: block` plus `reason`, with an eight-block runaway limit. Submitted-prompt command output is ignored; transformed-prompt can replace model-facing text. Post-tool accepts `additionalContext`; use failure event for failed tools. Default timeout is 30 seconds. Exit 0 parses JSON, 2 denies pre-tool and permission-request; **any** non-timeout pre-tool command failure denies, whereas timeout always fails open. HTTP pre-tool network errors fail open. No configured timeout can enforce fail-closed on timeout.

**A2 Spawn and isolation.** [DOCS https://docs.github.com/en/copilot/reference/hooks-reference] Direct `exec`/`args` avoids shell parsing; otherwise bash on Unix and PowerShell on Windows; hook `cwd` is relative to repo root or absolute, and configured `env` supports expansion. Non-policy hooks run inside an enabled sandbox and cannot widen grants via `cwd` or `env`; policy hooks run outside it. [DOCS https://docs.github.com/en/copilot/how-tos/copilot-cli/customize-copilot/use-hooks] The Windows shell example requires PowerShell 7+. [UNVERIFIED] Process ancestry, precise inherited variables and sandbox loopback/MCP access: compare `fixtures/log.jsonl` and `fixtures/mcp-process.json` in an authorized run on each OS.

**A3 MCP.** [DOCS https://docs.github.com/en/copilot/how-tos/copilot-cli/customize-copilot/add-mcp-servers] stdio, streamable HTTP and legacy SSE work. Config: `~/.copilot/mcp-config.json` / `%USERPROFILE%\.copilot\mcp-config.json`, overridable by `COPILOT_HOME`; project `.mcp.json` or `.github/mcp.json` takes precedence. Project servers need trust; for prompt mode the docs also describe `GITHUB_COPILOT_PROMPT_MODE_WORKSPACE_MCP=true`. `copilot mcp add iris -- iris mcp` writes user config. PATH is inherited for local servers; configure other needed variables explicitly. [MEASURED: `copilot help permissions`] `--allow-tool='iris'` approves all Iris MCP tools; `iris(tool-name)` narrows it. [MEASURED: isolated `copilot --no-auto-update mcp list --json`] An untrusted listing returned `{ "mcpServers": {} }` despite the experimental project file; this does not prove trusted discovery or server startup. [UNVERIFIED] MCP env/header expansion, server notifications, resources and elicitation delivery to model/user: test a purpose-built server before relying on them.

**A4 Packaging.** [MEASURED: `copilot plugin --help`] CLI plugins may contain skills, agents, hooks, MCP and LSP configuration. [DOCS https://docs.github.com/en/copilot/concepts/customizations/plugins] Agent Plugins 1.0 uses root `plugin.json`, `skills/`, `mcp.json` and Copilot-specific hooks under `com.github.copilot/hooks/`; legacy layout differs. [UNVERIFIED] Bundled executable portability/install permissions require a platform test; prefer resolving the package-managed `iris` binary. [DOCS https://docs.github.com/en/copilot/how-tos/use-copilot-agents/use-copilot-cli] Repository `.github/copilot-instructions.md`, path instructions and agent instructions load automatically.

**A5 Wake-up.** [DOCS https://docs.github.com/en/copilot/reference/copilot-cli-reference/acp-server] `copilot --acp` exposes preview ACP over stdio or TCP, enabling a controlling client to issue session prompts; it is not evidence that an arbitrary existing terminal session can be attached. [MEASURED: `copilot --help`] `--resume=<id>` starts/resumes a session and `--connect` exists. [DOCS https://docs.github.com/en/copilot/reference/hooks-reference] Notification-hook `additionalContext` can trigger processing when idle, but needs a CLI notification; it is not arbitrary external push.

**A6 Identity.** [DOCS https://docs.github.com/en/copilot/reference/hooks-reference] Hook `sessionId` is present at start, pre-tool and end; start indicates `startup|resume|new`. Subagent hooks can include `agentId`, `agentType`, `parentAgentId`; built-in `general-purpose` does **not** emit subagent start/stop events. [UNVERIFIED] ID stability across resume/compaction, MCP-to-hook identity and behavior of subagents need captured experiments.

## B. Minimal Iris integration

**Short:** put the following at `<repo>/.github/hooks/iris.json` on Linux/macOS, or `<repo>\.github\hooks\iris.json` on Windows. The direct executable must be on PATH; replace each `"exec":"iris"` with `"exec":"iris.exe"` on Windows. [DOCS https://docs.github.com/en/copilot/reference/hooks-reference] The schema and decisions below are documented, not tested against 1.0.105:

```json
{
  "version": 1,
  "hooks": {
    "sessionStart": [{"type":"command","exec":"iris","args":["hook","--client","copilot","--event","sessionStart"],"timeoutSec":2}],
    "preToolUse": [{"type":"command","exec":"iris","args":["hook","--client","copilot","--event","preToolUse"],"timeoutSec":2}],
    "sessionEnd": [{"type":"command","exec":"iris","args":["hook","--client","copilot","--event","sessionEnd"],"timeoutSec":2}]
  }
}
```

Return `{"additionalContext":"..."}` at start; parse `toolArgs` defensively and return `{"permissionDecision":"deny","permissionDecisionReason":"Conflicting claim"}` for a conflicting edit; return `{}` for normal fallthrough and end. Treat shell tools separately and do not claim comprehensive enforcement. Install MCP separately with `copilot mcp add iris -- iris mcp` (Windows: `copilot mcp add iris -- iris.exe mcp`); use an explicit `--allow-tool='iris'` only with user consent. [DOCS https://docs.github.com/en/copilot/how-tos/copilot-cli/customize-copilot/add-mcp-servers] Admission and sandbox policy remain prerequisites.

## C. Capability matrix

These are documentation-based capabilities, **not measured on 1.0.105**; partial means conditional or incomplete.

- session-start-context: **yes** [DOCS https://docs.github.com/en/copilot/reference/hooks-reference]
- prompt-context: **partial** — transformed prompt replacement, not submitted-hook context [DOCS https://docs.github.com/en/copilot/reference/hooks-reference]
- pre-tool-deny: **yes** [DOCS https://docs.github.com/en/copilot/reference/hooks-reference]
- post-tool: **yes** — success and failure are separate [DOCS https://docs.github.com/en/copilot/reference/hooks-reference]
- stop-block: **partial** — bounded retries [DOCS https://docs.github.com/en/copilot/reference/hooks-reference]
- session-end: **yes** [DOCS https://docs.github.com/en/copilot/reference/hooks-reference]
- edit-path-visible: **partial** — tool arguments, not arbitrary shell effects [DOCS https://docs.github.com/en/copilot/reference/hooks-reference]
- shell-command-visible: **partial** — shell tool arguments, not every filesystem effect [DOCS https://docs.github.com/en/copilot/reference/hooks-reference]
- subagent-events: **partial** — general-purpose exception [DOCS https://docs.github.com/en/copilot/reference/hooks-reference]
- fires-headless: **partial** — command events documented; prompt-type start excluded [DOCS https://docs.github.com/en/copilot/reference/hooks-reference]
- async-handler: **partial** — notification and detached `/clear` end [DOCS https://docs.github.com/en/copilot/reference/hooks-reference]
- idle-wake: **partial** — controlled ACP session or notification-triggered injection, not arbitrary terminal attach [DOCS https://docs.github.com/en/copilot/reference/copilot-cli-reference/acp-server] [DOCS https://docs.github.com/en/copilot/reference/hooks-reference]

## D. Release engineering

**Correction:** the orchestrator permits release-vendor primary sources; this section supersedes the earlier source-scope objection in G.

**Pipeline — Medium.** Pin GoReleaser v2 and use the following configuration fragments, not a complete greenfield config. [DOCS https://goreleaser.com/customization/builds/] [DOCS https://goreleaser.com/customization/package/nfpm/] [DOCS https://goreleaser.com/customization/publish/homebrew_casks/] [DOCS https://goreleaser.com/customization/publish/scoop/] [DOCS https://goreleaser.com/customization/publish/winget/] [DOCS https://goreleaser.com/customization/sbom/] [DOCS https://goreleaser.com/customization/sign/sign/] [DOCS https://goreleaser.com/customization/package/dockers_v2/]

```yaml
version: 2
builds:
  - id: iris
    main: ./cmd/iris
    binary: iris
    env: [CGO_ENABLED=0]
    goos: [linux, darwin, windows]
    goarch: [amd64, arm64]
archives:
  - formats: [zip]
nfpms:
  - package_name: irisd
    formats: [deb, rpm]
homebrew_casks:
  - name: irisd
    binaries: [iris]
    repository: {owner: OWNER, name: homebrew-tap}
scoops:
  - name: irisd
    repository: {owner: OWNER, name: scoop-bucket}
winget:
  - name: Iris
    publisher: OWNER
    package_identifier: OWNER.Iris
    short_description: Agent coordination
    license: Apache-2.0
    repository: {owner: OWNER, name: winget-pkgs}
sboms:
  - artifacts: archive
    cmd: syft
signs:
  - artifacts: checksum
    cmd: cosign
    signature: '${artifact}.sigstore.json'
    args: [sign-blob, '--bundle=${signature}', '${artifact}', '--yes']
dockers_v2:
  - images: [ghcr.io/OWNER/irisd]
    tags: ['{{ .Version }}']
    platforms: [linux/amd64, linux/arm64]
    dockerfile: Dockerfile
```

Configure versioned asset names, metadata and credentials; the example omits publication/PR permissions and Dockerfile. Run `goreleaser check`, a snapshot, and native OS/architecture install and upgrade smoke tests before use. [DOCS https://goreleaser.com/customization/package/archives/] [DOCS https://go.dev/doc/install/source] [UNVERIFIED] The exact complete configuration and WinGet pipe's edition requirement need a release dry run; do not treat this fragment as tested.

[DOCS https://goreleaser.com/pro/] Pro includes native app/DMG/pkg notarization, MSI/NSIS, Cloudsmith publishing integration and versioned casks; cross-platform binary notarization via `notarize.macos` is not listed as a Pro feature. [DOCS https://goreleaser.com/customization/sbom/] Syft SBOM generation for archives is supported; image SBOMs must be generated separately. [DOCS https://goreleaser.com/customization/sign/sign/] Cosign signs the checksum file, not each archive; verify its bundle with the expected workflow identity and OIDC issuer before checking hashes. Also sign the OCI digest and generate GitHub attestations where eligible; grant the release job only the required `contents`, `packages`, `id-token` and `attestations` write permissions. [DOCS https://docs.sigstore.dev/cosign/signing/signing_with_containers/] [DOCS https://docs.github.com/en/actions/how-tos/secure-your-work/use-artifact-attestations/use-artifact-attestations]

**Homebrew — Short.** [DOCS https://goreleaser.com/customization/publish/homebrew_formulas/] GoReleaser deprecates `brews` in favor of `homebrew_casks`; this is not deprecation of Homebrew formulae. [DOCS https://docs.brew.sh/Cask-Cookbook] A cask can install a prebuilt CLI using `binary`. Recommend `brew install --cask OWNER/tap/irisd`; a fully qualified tap name disambiguates collisions, and a separately maintained source-building formula is the alternative. [DOCS https://docs.brew.sh/Taps] [DOCS https://docs.brew.sh/Formula-Cookbook] [DOCS https://goreleaser.com/customization/publish/homebrew_casks/] Quarantined, unsigned or unnotarized cask binaries can trigger Gatekeeper's “damaged” block; sign and notarize rather than silently remove quarantine. Test both Mac architectures. Keep package `irisd` distinct from executable `iris` after checking PATH collisions.

**Windows — Medium.** [DOCS https://learn.microsoft.com/en-us/windows/package-manager/package/manifest] [DOCS https://github.com/microsoft/winget-pkgs/blob/master/doc/manifest/schema/1.12.0/installer.md] For community winget submit version, installer and default-locale manifests; include unique `Publisher.Package`, architecture, direct public `InstallerUrl` and `InstallerSha256`. Use `InstallerType: portable` for an EXE, or `InstallerType: zip`, `NestedInstallerType: portable` and `NestedInstallerFiles` for a ZIP. [DOCS https://github.com/microsoft/winget-pkgs/issues/2701] Authenticode is not a universal prerequisite for ordinary portable EXEs; unsigned software may trigger Windows warnings. Validate and submit a PR; acceptance is not guaranteed. [DOCS https://goreleaser.com/customization/publish/scoop/] Publish the separate Scoop bucket with `scoops` and a Windows ZIP.

**Windows signing — Short.** [DOCS https://learn.microsoft.com/en-us/azure/artifact-signing/quickstart] Azure Artifact Signing public trust is **not available to an EU individual**: individuals must be in the US or Canada; eligible EU organizations are different. [DOCS https://azure.microsoft.com/en-us/pricing/details/artifact-signing/] Basic is $9.99/month; Premium $99.99/month, subject to location/tax. [DOCS https://signpath.org/terms.html] [DOCS https://signpath.org/] Apply for SignPath Foundation's free signing once Iris is public and qualifies; approval and its project/build restrictions are not guaranteed for greenfield. [DOCS https://www.ssl.com/products/software-integrity/code-signing/iv/] [DOCS https://www.ssl.com/guide/esigner-pricing-for-code-signing/] Alternative: SSL.com individual-validated certificate advertised at $129/year plus eSigner Tier 1 at $180/year, approximately $309/year before taxes; verify country eligibility and current checkout price.

**Apple — Medium.** [DOCS https://developer.apple.com/programs/] Individual Apple Developer membership costs $99/year. [DOCS https://goreleaser.com/customization/sign/notarize/] An individual can use Developer ID to sign/notarize a standalone CLI; GoReleaser `notarize.macos` embeds quill and works from Linux CI with a `.p12`, password and App Store Connect API credentials. Native `notarize.macos_native` is for app bundles/DMGs/pkg and requires macOS tools. Wait for notarization success before publication; protect keys.

**apt/dnf — Medium.** [DOCS https://docs.cloudsmith.com/resources/open-source-hosting-policy] [DOCS https://docs.cloudsmith.com/supply-chain-security/signing-keys] Prefer Cloudsmith's free eligible OSS hosting and managed signing for public Debian/RPM repositories; publish and independently verify the repository key fingerprint. [DOCS https://goreleaser.com/pro/] GoReleaser's direct Cloudsmith integration is Pro: use Cloudsmith's own upload tooling instead. [DOCS https://docs.github.com/en/pages/getting-started-with-github-pages/github-pages-limits] A static Pages alternative has size/bandwidth and acceptable-use constraints; `nfpm` builds packages, not repository indexes—generate and sign apt `InRelease` and RPM `repomd.xml` separately, or avoid self-hosting that maintenance.

**Private source and CI — Short.** [DOCS https://docs.github.com/en/rest/releases/assets#get-a-release-asset] Private release assets require authorized access; use public release assets or another public host for public package managers. [DOCS https://docs.github.com/en/packages/learn-github-packages/configuring-a-packages-access-control-and-visibility] GHCR package visibility can be public independently of source; private packages require access. [DOCS https://goreleaser.com/customization/publish/homebrew_casks/] A private tap/asset setup needs authenticated clients and a custom download strategy, not an ordinary public install. [DOCS https://docs.github.com/en/actions/how-tos/secure-your-work/use-artifact-attestations/use-artifact-attestations] GitHub Free/Pro/Team attestations require public repositories; private/internal requires Enterprise Cloud. [DOCS https://docs.github.com/en/billing/reference/product-usage-included] [DOCS https://docs.github.com/en/billing/reference/actions-runner-pricing] GitHub Free includes $12/month eligible compute; Windows standard costs $0.010/min, macOS $0.062/min, approximately 1,200 or 193 minutes if used exclusively, before rounding/other usage. Let package managers perform upgrades; do not silently self-update their binaries.

## E. Verdict on P1 to P12

- **P1 CHANGE — Medium:** MCP plus hooks and a git guard are advisory layers, not universal hard enforcement; git hooks can be bypassed and do not stop non-git writes.
- **P2 AGREE — Medium:** use one Iris adapter path and direct exec; HTTP is optional. [DOCS https://docs.github.com/en/copilot/reference/hooks-reference]
- **P3 CHANGE — Short:** benchmark p95 separately; fail-open on adapter errors by returning `{}` with exit 0. Fail-closed is best effort only: Copilot timeouts always fail open. [DOCS https://docs.github.com/en/copilot/reference/hooks-reference]
- **P4 CHANGE — Medium:** loopback is a local transport, not a cross-host/WSL/container topology. Explicitly configure authenticated routing to the hub; protect tokens and test sandbox access.
- **P5 REJECT — Medium:** PID ancestry is unproven, fragile for multiple sessions/subagents and inaccessible across host boundaries. Use an explicit per-execution binding via an Iris-controlled launcher and scoped capability; otherwise report unbound, never infer authority from PID alone.
- **P6 CHANGE — Short:** start with one explicit hub; defer per-host agents, autostart and lazy-start races until a demonstrated need. Never lazily start a second hub.
- **P7 CHANGE — Medium:** `git status` is useful reconciliation, not attribution or an edit firewall; guards are bypassable and ignore uncommitted/out-of-worktree effects.
- **P8 CHANGE — Medium:** add Iris-controlled ACP sessions as an optional wake/control path; no promise for independently started terminal sessions. [DOCS https://docs.github.com/en/copilot/reference/copilot-cli-reference/acp-server]
- **P9 CHANGE — Medium:** remote/root-commit pairs can be absent, rewritten or ambiguous. Support explicit stable repo IDs and aliases; document path case and symlink policy.
- **P10 AGREE — Short:** treat all messages as untrusted data, with provenance, limits and no authority delegation.
- **P11 CHANGE — Medium:** preview, opt-in and reversible per-client integration; avoid changing all clients by default. Doctor first checks offline prerequisites; live model canary is optional and billable.
- **P12 CHANGE — Medium:** stage signed archives, SBOM, package-managed installs and multiarch image first; fund OS signing when eligibility and cost are verified. No current GoReleaser config or public/private promise without permitted primary sources.

## F. From scratch

**Medium:** build the hub, explicit MCP tools and one Copilot command-hook adapter first. Require an authenticated explicit execution binding; degrade visibly when it cannot be established. Ship package-managed binaries with verifiable checksums and native smoke tests, then add repositories and higher-cost signing as demand warrants. Do not build background agents, universal push, silent lazy-start or pseudo-hard enforcement before evidence supports them.

## G. Unknowns

- **Blocking:** run `fixtures/README.md` in an environment permitting a nested authenticated CLI; capture sanitized real start/pre/post/end payloads, a verified deny, and MCP/hook ancestry, then repeat resume, compaction, subagent, timeout and sandbox tests on Linux, macOS and Windows. No golden hook fixtures exist yet.
- **Release blockers:** the source restriction prevents checking current GoReleaser keys, catalog submission rules, repository hosting limits, signing eligibility/prices and private-repository entitlements. Resolve the source-scope conflict before those claims or a working release configuration can be delivered.
