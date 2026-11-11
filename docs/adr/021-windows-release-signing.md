# 021: Prove Certum Windows signing before release integration

Status: accepted by fleet vote 4; the owner's decisions of 2026-11-11 are recorded below.

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
   The job declares no environment. There is no environment gate. Only the signing
   step receives the two Certum secrets. One invocation passes both file paths
   positionally, submitting each once. `XDG_RUNTIME_DIR` points the signer's cache at a fixed directory
   under the runner's temporary directory. There is no cleanup step because the
   hosted VM is discarded after the job and the owner's rule bars deletion through
   variable paths. The two signed files are
   transferred in an artifact retained for one day.
3. `verify` has no secrets and runs on Windows. It first checks that the artifact
   contains exactly the two expected executables. With an empty or missing expected
   thumbprint, it prints each signer's public `Subject`, `Issuer`, `Thumbprint`,
   `NotBefore` and `NotAfter`, and the timestamp signer's `Subject`, then fails closed.
   The owner must confirm the printed certificate and commit its thumbprint before
   verification can pass. With a committed expected thumbprint, both files require
   `signtool verify /pa /all /v /tw` to exit 0; exit 2, including a timestamp
   warning, fails. It also requires `Get-AuthenticodeSignature` to report `Valid`,
   a non-null `TimeStamperCertificate` and the expected signer thumbprint
   committed in the workflow. See Microsoft's [SignTool documentation](https://learn.microsoft.com/en-us/windows/win32/seccrypto/signtool)
   and [Get-AuthenticodeSignature documentation](https://learn.microsoft.com/en-us/powershell/module/microsoft.powershell.security/get-authenticodesignature).

## Interim and signer review

At the pinned revision ssign accepts positional file paths, has no `sign`
subcommand or certificate-inspection command, and chooses the first cloud card.
It can follow absolute task links with its bearer token, and authentication
errors can contain sensitive response material. Therefore the stock proof
discards all ssign stdout and stderr and prints only a fixed failure message.
Verification checks the certificate after signing, so choosing a wrong card can
already have spent quota.

On 2026-11-11 the owner allowed fleet sessions to dispatch the proof. By doing so,
the owner accepted this stock interim. There is no environment gate.

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
- **A hardened upstream revision or owner fork**: a future improvement. Any
  replacement must be reviewed and pinned by full SHA.

## What this cannot do

- The whole signing job is trusted. Step-scoped secrets are not process isolation.
  In the planned release integration, GoReleaser, hooks and toolchains share a
  job with Certum and Apple secrets, OIDC identity and the release-write token.
- A stolen TOTP seed can sign outside Actions until the owner revokes it at
  Certum. Deleting a GitHub secret does not revoke the seed.
- There is no human gate. Whoever can dispatch the workflow on `main` can spend
  two signatures a run. Every fleet session acts as the owner's GitHub account.
  A workflow on any pushed branch can name the repository-level secrets. The owner
  accepts this for now.
- Authenticode identifies who signed bytes; it does not establish that those
  bytes are the intended build. This proof does not test SmartScreen reputation,
  release ZIP contents, checksums, publication or a Windows program launch.
- Proof artifacts in a public repository are downloadable by signed-in users.
- A `v*` tag run does not publish by itself. `.goreleaser.yaml` sets
  `release.draft: true`, so the run makes a draft release, and publishing it is
  the owner's step. The tag, the signatures in Rekor and the build attestation
  are public as soon as the run makes them, before anyone publishes the draft.
- rustup downloads the pinned toolchain version at run time from the Rust project's
  servers. Its integrity rests on rustup's own checks; no checksum is committed here.
- Certum availability and quota can stop signing. A successful run costs two
  signatures; failed requests may also consume quota. Rerunning `sign` or the whole
  workflow signs again and can consume two further signatures; rerunning only
  `verify` does not sign.

## Owner-only steps

1. Answered on 2026-11-11: the Windows token has the `workflow` scope.
2. Answered by the owner on 2026-11-11: `CERTUM_OTP` holds the Base32 value of the
   provisioning URI's `secret` parameter, not the whole URI. ssign needs only the
   email and that secret and fetches the certificate from the cloud itself. The
   secret's value stays in GitHub.
3. Done on 2026-11-11. Run 43023491609 was dispatched on `main` at 7912e19. It
   signed both files and then failed `verify` on purpose, because
   `EXPECTED_SIGNER_THUMBPRINT` was empty. It printed the same certificate for both
   files:
   - Subject: CN=Open Source Developer Mateusz Klatt, O=Open Source Developer,
     L=Warszawa, S=mazowieckie, C=PL
   - Issuer: CN=Certum Code Signing 2021 CA, O=Asseco Data Systems S.A., C=PL
   - Thumbprint: 9C837C68415187A72D88612BDF14FCB0D6623D67
   - Validity: 2026-11-10 to 2027-11-10
   - Timestamp signer: CN=Certum Timestamp 2026, O=Asseco Data Systems S.A., C=PL

   The coordinator confirmed that this is the owner's certificate, and its
   thumbprint is pinned in the `verify` job. The pin must change when the
   certificate is renewed, by 2027-11-10. Finding the thumbprint costs four
   signatures in all: two in that run, and two in the verifying run after the pin.
4. Answered on 2026-11-11: Certum's terms allow automated use of the seed.
5. Answered on 2026-11-11: the owner chose no environment gate. The existing
   `signing` environment stays unused. No session changes or deletes it. The
   secrets stay at repository level.
6. Answered on 2026-11-11: the owner allowed fleet sessions to dispatch this proof.
   The owner or a fleet session may dispatch from `main`; each run spends two
   signatures. macOS dispatches the first run after merge.
7. A leaked seed. Certum's SimplySign manual (iOS 6.1.2, section 6.3, page 22)
   describes a reset of access to the service:
   - the owner contacts the Certum Help line (infolinia@certum.pl);
   - after verifying his personal details, Certum gives him a 6-digit secret and
     e-mails a one-time reset link, valid for 24 hours;
   - that link yields a new QR code or a 16-character reset code, which activates
     the application again.

   The manual does not say that a reset invalidates the previous token's seed. So
   it is not documented whether a reset stops a leaked seed from signing. The owner
   can still revoke the certificate itself in his Certum account. This is a stated
   limit.
8. Answered on 2026-11-11: allowing fleet dispatch accepted the stock ssign interim
   described above. A hardened replacement still requires review and another proof
   before release integration.

It is not known whether the account holds more than one cloud certificate. The stock
signer picks the first one, and in run 43023491609 that was the open-source
code-signing certificate above. If a renewal or a second certificate changes which
one comes first, `verify` fails on the pin, but only after both signatures are spent.
