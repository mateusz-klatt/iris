# 021: Prove Certum Windows signing before release integration

Status: accepted by fleet vote 4 on 2026-11-11; owner prerequisites below remain open.

## Problem

D15 puts SignPath Foundation before Certum. The owner already has a Certum cloud
certificate and has chosen signing in Actions, but unattended use of that account
has not been proved. Changing release publication before proving the signer would
couple that uncertainty to the first release.

## Decision

Use two pull requests. The first adds the separate `signing-proof.yml` workflow,
ADR and CI guards. It leaves `release.yml` and `.goreleaser.yaml` unchanged. Only
after a successful proof and a further vote does the second integrate signing
with the existing GoReleaser draft release. This supersedes D15's SignPath-first
order and its placement of the TOTP seed in environment secrets.

`CERTUM_EMAIL` and `CERTUM_OTP` stay at repository level. The proof accepts only
`workflow_dispatch`, and every job requires `refs/heads/main`. Its token has only
`contents: read`; it gets no OIDC identity, Apple credentials or release-write
permission. The `certum-signing` concurrency group never cancels an active run.
There is no automatic retry.

The jobs have three responsibilities:

1. `signer` builds `Le-Syl21/ssign` from full commit
   `f7ba7b2360a3e40bb8aa062d244d95c62a743b90` (v0.1.8, MIT), in its own checkout
   without secrets, using an exact Rust toolchain and `cargo build --release --locked`.
   It records the binary's SHA-256 in the job summary and transfers that binary
   in an artifact retained for one day. No ssign source text enters this
   repository, including patch context.
2. `sign` builds disposable `iris.exe` files for Windows amd64 and arm64 from the
   checked-out Iris commit, with `CGO_ENABLED=0`. There is no separate unsigned-input
   check because the inputs are fresh `go build` outputs of the same job.
   The job declares `environment: signing`, so the
   owner must first add `main` to that environment's deployment policy while
   retaining the required reviewer. Only the signing step receives the two
   Certum secrets. One invocation passes both file paths positionally, submitting
   each once. `XDG_RUNTIME_DIR` points the signer's cache at a fixed directory
   under the runner's temporary directory. There is no cleanup step because the
   hosted VM is discarded after the job and the owner's rule bars deletion through
   variable paths. The two signed files are
   transferred in an artifact retained for one day.
3. `verify` has no secrets and runs on Windows. For both files it requires
   `signtool verify /pa /all /v /tw` to exit 0; exit 2, including a timestamp
   warning, fails. It also requires `Get-AuthenticodeSignature` to report `Valid`,
   a non-null `TimeStamperCertificate` and the expected signer thumbprint
   committed in the workflow. An empty or missing expected thumbprint fails
   closed with a clear message. See Microsoft's [SignTool documentation](https://learn.microsoft.com/en-us/windows/win32/seccrypto/signtool)
   and [Get-AuthenticodeSignature documentation](https://learn.microsoft.com/en-us/powershell/module/microsoft.powershell.security/get-authenticodesignature).

## Interim and signer review

At the pinned revision ssign accepts positional file paths, has no `sign`
subcommand or certificate-inspection command, and chooses the first cloud card.
It can follow absolute task links with its bearer token, and authentication
errors can contain sensitive response material. Therefore the stock proof
discards all ssign stdout and stderr and prints only a fixed failure message.
Verification checks the certificate after signing, so choosing a wrong card can
already have spent quota.

**No credentialed run happens until the owner names a reviewed hardened revision
or explicitly accepts this interim.** That prerequisite is also a comment at the
top of the workflow. It is a rule for the owner and sessions, not an additional
machine-enforced approval gate.

The preferred hardened upstream revision checks the expected certificate's DER
SHA-256 after loading either a fresh or cached session, before a signing request;
restricts authenticated task links to Certum's HTTPS origin; and sanitizes
authentication errors. It must be selected by full commit SHA from upstream or
an owner-controlled fork, with the same revision proved before release
integration. A carried patch is forbidden. A review must identify exactly what
was read: ssign's own code and the locked dependencies that handle networking or
secrets. Preparation read the pinned signer's CLI, manifests and lockfile,
authentication, session/cache, card, task, signing, OTP and timestamp paths;
selected client, redirect, retry, cookie, body and multipart paths in locked
`reqwest` 0.13.5; and the `zeroize` 1.9.0 implementation. This was a targeted
review, not an exhaustive audit of transitive dependencies. Review of a hardened
revision must state its own coverage of signer code and locked dependencies.

## Alternatives

- **SignPath Foundation**, D15's original first choice: free if accepted, but it
  requires review and would name SignPath Foundation as publisher. The owner
  chose the existing Certum certificate for this proof.
- **SimplySign Desktop with GUI automation**: makes a desktop session and its
  dialogs part of the release procedure instead of proving an unattended CLI.
- **A carried ssign patch**: could add the certificate and origin checks locally,
  but the adopted rule forbids importing any third-party source text, even patch
  context, into Iris.
- **A hardened upstream revision or owner fork**: preferred before credentials
  are used; its reviewed full SHA and acceptance remain the owner's decision.

## What this cannot do

- The whole signing job is trusted. Step-scoped secrets are not process isolation.
  In the planned release integration, GoReleaser, hooks and toolchains share a
  job with Certum and Apple secrets, OIDC identity and the release-write token.
- A stolen TOTP seed can sign outside Actions until the owner revokes it at
  Certum. Deleting a GitHub secret does not revoke the seed.
- A workflow on another branch pushed to this repository can name repository
  secrets. The owner accepts that placement; this workflow's ref checks do not
  constrain other workflows.
- The required reviewer is the owner's GitHub account, also used by fleet
  sessions, and administrators can bypass the environment gate. The rule that
  sessions never approve, bypass or alter it is advice, not enforcement.
- Authenticode identifies who signed bytes; it does not establish that those
  bytes are the intended build. This proof does not test SmartScreen reputation,
  release ZIP contents, checksums, publication or a Windows program launch.
- Proof artifacts in a public repository are downloadable by signed-in users.
  Later release tags, Rekor entries and provenance attestations are public
  before Windows verification completes, even while release assets are drafts.
- Certum availability and quota can stop signing. A successful run costs two
  signatures; failed requests may also consume quota. Rerunning `sign` or the whole
  workflow signs again and can consume two further signatures; rerunning only
  `verify` does not sign.

## Owner-only steps

1. Grant the Windows GitHub CLI the `workflow` scope with
   `gh auth refresh -h github.com -s workflow`, or arrange for holzera to push.
   This implementation does not push or dispatch anything.
2. Describe the format of `CERTUM_OTP`, never its value: a Base32 TOTP seed, an
   `otpauth://` URI from the SimplySign QR, or a single code. Confirm compatibility
   with the selected signer before use. A single code cannot support unattended
   signing.
3. Confirm whether the account has exactly one cloud certificate and whether
   signing requires a PIN. Supply the public certificate, or its thumbprint,
   subject and validity dates. Commit its expected thumbprint as
   `EXPECTED_SIGNER_THUMBPRINT` in the `verify` job of `signing-proof.yml`; it is
   intentionally empty until supplied.
4. Confirm Certum's terms permit automated seed use for this product.
5. Add branch `main` to the `signing` environment's deployment policy, keeping
   the existing `v[0-9]*` tag policy and the owner as required reviewer. The
   present tag-only policy blocks the proof's signing job.
6. After all prerequisites, dispatch the proof from `main` and approve its two
   signatures in Review deployments. Later tag runs need separate approval.
   Sessions neither dispatch nor approve these runs.
7. Establish how to disable the seed at Certum if it leaks.
8. Name a reviewed hardened upstream/fork revision meeting the checks above, or
   explicitly accept the stock revision and its interim limits before the first
   credentialed run. Record review scope, including which locked dependencies
   handling network traffic or secrets were read. A hardened revision requires
   another proof before release integration.
