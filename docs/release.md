# Snapshots and releases

[v0.1.0](https://github.com/mateusz-klatt/iris/releases/tag/v0.1.0) was published on 2026-11-12
at 06:07 UTC from `c4983205cb320a1b4313d114c1b2ab644023e59c`, after the signed release
workflow [43117855817](https://github.com/mateusz-klatt/iris/actions/runs/43117855817) succeeded.
The configuration below includes changes made after that commit, including ADR 022's image
job; that earlier release does not validate the later job.

A pushed `v*` tag runs `.github/workflows/release.yml`, which makes a **draft**
GitHub release (`release: draft: true`) for the owner to publish. After the release and
Windows verification jobs succeed, a separate job publishes a signed, attested
Linux amd64/arm64 image to `ghcr.io/mateusz-klatt/iris`, under the full tag.
Stable tags (ref names without `-`) also update `latest`; prereleases do not.
The image, tag, Rekor entries, attestations and final archive bytes in workflow artifacts
can be public before the owner publishes the draft. Homebrew, Scoop and winget remain
`skip_upload: true`. Snapshots and pull requests push no images.
[ADR 022](adr/022-release-container-image.md) records this extension of D15.
The intended shipping tag, `v0.1.1`, is a **final release** and the owner's call;
this configuration does not create that tag or authorize anyone else to create it.
The GoReleaser dependency is not part of the application module.

The web tooling pins Vitest and its coverage provider together. The `minimatch` override
in `web/package.json` selects 11.0.1 for GHSA-g3hv-75wg-m249 while ESLint and
typescript-eslint still request affected 10.x versions. Remove the override when those
upstream ranges accept a fixed version, after running audit, lint, build and coverage.
Vitest 4.1.12 fixes GHSA-qxg6-89mg-rqv8 and GHSA-xp26-3454-mjgp. These are build/test
dependencies; the final distroless server image contains the Go binary, not node_modules.

Use GoReleaser **v2.20.0** (the official binary, Docker image, or `go run github.com/goreleaser/goreleaser/v2@v2.20.0`) from the repository root:

```sh
go run github.com/goreleaser/goreleaser/v2@v2.20.0 check
go run github.com/goreleaser/goreleaser/v2@v2.20.0 release --snapshot --clean --skip=publish,sign,sbom
```

Images are built last. Without a running Docker daemon that step fails with
`could not build docker image`, after the archives and manifests are already in `dist/`. Start Docker, or
add `docker` to `--skip` when the archives are all you need. Measured on Windows 11 with Docker stopped.

On Windows, `make web` needs `rm`, `mkdir -p` and `cp -a`. The measured snapshot ran from Git Bash. It
has not been tried from PowerShell alone.

The configuration uses `homebrew_casks` instead of the deprecated `brews` key. `check`
must exit 0 without the old deprecation warning. The cask is generated locally but its
upload stays disabled until a real tag's CI run has signed and notarized the binaries. `dist/` is
ignored by Git; keep snapshot output out of commits.

The before hook runs `make web`, requiring the Node version in `web/package.json`
and npm. Release builds embed the rebuilt console, use `CGO_ENABLED=0`, `-trimpath`,
and stamp the release version, full commit and source dirty state through ldflags, using
GoReleaser's Git metadata captured before the console build. `iris version`, the doctor bundle,
MCP server identity and authenticated hub status use the same value, such as
`1.2.3 (commit <full hash>; source clean; console unverified)`. Plain `go build` and `make build` use Go's embedded VCS
metadata; a source archive or `-buildvcs=false` without explicit linker metadata reports unknown
identity rather than claiming a clean checkout. `make build VERSION=...` overrides the version
label. `source clean` describes only the checkout state reported by the VCS metadata (or
explicit linker metadata). It does not cover the git-ignored `internal/webui/dist` assets.
For local installation, run `make build`: it runs `make web` first, then compiles the
binary with `CGO_ENABLED=0`. The default label stays `dev`, qualified by the commit and
source state. The generated index and assets are ignored; only an empty embed seed is
tracked, so rebuilding a clean checkout does not create source dirt or require a Git restore.
Both `make web` paths (Node build and `WEB_DIST` handover) retain that seed.

`web/go.mod` marks the Node-only workspace as a separate Go module boundary.
Root `./...` commands, including vet, tests, race, coverage, lint and vulnerability
checks, therefore never discover Go packages shipped inside `web/node_modules`.
The package discovery test exercises a dependency package under `node_modules`
and a new first-party package outside `web/`. A layout test rejects first-party
Go files inside `web/`, so that boundary cannot silently hide application code.
Keep application Go source in the root module; no filtered package list is used.

Plain `go build ./...` and `go test ./...` require no Node toolchain. With no embedded
index, the version says `console missing` and console routes return HTTP 503 with build
instructions, even if old assets remain. With an index, the version says `console unverified`:
its presence cannot establish that the assets are complete or current with the source.
Release and image builds still rebuild the console through their existing build steps, but
their explicit source metadata does not verify the embedded assets either.
These fields identify a build; they do not attest to its provenance.

Alternatives rejected:

- Keep the generated index tracked: rebuilding would still create source dirt.
- Force clean linker metadata for local builds: that could hide real source edits.
- Track the whole compiled console: that adds generated churn and still needs a rebuild to stay current.

Outputs cover Linux, macOS and Windows on
amd64 and arm64: four tar.gz archives, two Windows zip archives, checksums, and
Linux deb/rpm packages named `iris`. Package-manager manifests remain local. Their explicit download URL templates refer to
future GitHub assets; private snapshot assets are not public install channels.

The server image uses `dockers_v2` for local Linux amd64/arm64 snapshots, which load
platform images without pushing. GoReleaser disables that image path on real releases;
the separate workflow job below publishes the release image.
It runs as uid/gid 65532 and stores the database and token under `/data`, which must be
writable by that user. A fresh named volume is; a bind mount needs its owner set. For externally reachable serving,
pass `--listen`, `--tls-cert` and `--tls-key` explicitly, as required by D1.
Snapshot checksum signing and SBOM generation are skipped by the command above. The tag
workflow already installs syft and cosign, generates an SBOM per archive, signs checksums
with keyless cosign using its OIDC identity, and creates GitHub build provenance. These
release steps ran in the successful v0.1.0 workflow named above. Later changes require their
own tag-run evidence. Apple signing and notarization stay disabled without `MACOS_SIGN_P12`.

## Release container image

The `image` job waits for `release` and `verify-windows`, and requires a push of a `v*`
tag in `mateusz-klatt/iris`. The release job hands over both Linux archives and the
original `checksums.txt` through one immutable artifact ID. The image job checks their
hashes and packages the extracted GoReleaser binaries using the root `Dockerfile`'s
`release` target. It does not rebuild the binary or console. The two platform manifests
are published as one multi-architecture index under the version tag, such as `v0.1.1`.
Stable ref names without `-` also update `latest`. RC and other prerelease tags, such as
`v0.1.1-rc1`, publish only their version tag and are still signed and attested.
An older stable tag can still move `latest`; this does not select the greatest semantic
version. This rule is separate from `prerelease: auto` and GitHub Releases' Latest designation.
The conditional `latest` input is empty for prereleases; the pinned action's
[bundled list parser](https://github.com/docker/build-push-action/blob/c3c9e263c25d99ce0380d002d59b67737d91b0dc/dist/index.cjs)
ignores empty entries, so that input adds no tag.

Only `image` has `packages: write`. Its other permissions are `contents: read` for checkout,
`id-token: write` for keyless identity, and `attestations: write` for GitHub provenance.
Registry authentication uses that job's GitHub token. The release job retains its existing
permissions and the Certum/Apple secrets remain on its GoReleaser step. A separate job
keeps registry writes out of that signing runner. This is a job boundary, not protection
from a compromised repository or GitHub Actions service.

Cosign signs the pushed **index digest** keylessly using the workflow's GitHub OIDC identity.
`actions/attest-build-provenance` attests that same digest and stores provenance in GitHub
and the registry. The root `Dockerfile` also has a `deploy` target for `make image`; both
targets share the distroless non-root runtime, `/iris`, uid/gid 65532 and writable `/data`,
with `TMPDIR=/data`. The public hub continues to use its CI-built `iris-iris` deployment
artifact from `main`; publishing GHCR does not change the running hub or select a deployment.
Its source, version label and image bytes can differ from the released image.

Before relying on anonymous pulls, the owner must confirm that the GHCR package is public
and that this repository's Actions token can write it. Repository visibility alone does
not configure package access. No registry publication or keyless signing is established by
a local snapshot; the first successful tag run must supply that evidence.

To pull a version (or use `:latest` for the moving tag):

```sh
docker pull ghcr.io/mateusz-klatt/iris:v0.1.1
docker buildx imagetools inspect ghcr.io/mateusz-klatt/iris:v0.1.1
```

Record the top-level `Digest` from the index, not one platform's child digest. Compare it
with the successful image job's output, then use that digest throughout verification and
deployment. Substitute it below, and substitute the actual release tag in both identities.
Use current Cosign and GitHub CLI clients; authenticate `gh` and log in to GHCR with
`docker login ghcr.io` before the GitHub CLI's OCI verification.

```sh
IMAGE='ghcr.io/mateusz-klatt/iris@sha256:<index-digest>'
cosign verify "$IMAGE" \
  --certificate-identity 'https://github.com/mateusz-klatt/iris/.github/workflows/release.yml@refs/tags/v0.1.1' \
  --certificate-oidc-issuer 'https://token.actions.githubusercontent.com'
gh attestation verify "oci://$IMAGE" \
  --repo mateusz-klatt/iris \
  --signer-workflow mateusz-klatt/iris/.github/workflows/release.yml \
  --source-ref refs/tags/v0.1.1 \
  --cert-identity 'https://github.com/mateusz-klatt/iris/.github/workflows/release.yml@refs/tags/v0.1.1'
docker pull "$IMAGE"
```

These commands check the expected [Cosign identity](https://docs.sigstore.dev/cosign/verifying/verify/)
and [GitHub workflow provenance](https://cli.github.com/manual/gh_attestation_verify).
Verification covers the index and the platform manifests it references. Run the verified
digest rather than resolving a mutable tag again. The default `gh` command retrieves
attestations from GitHub; add `--bundle-from-oci` to check the registry copy instead.

This cannot make push, signing and attestation atomic: a failure can leave a published
tag without all verification evidence. Require both checks before use. Neither signature
nor provenance establishes that code is safe, correct, approved by the owner or deployed
on the public hub. Docker does not enforce these verification commands. Non-root execution
does not configure TLS, access controls, backups or volume permissions; bind mounts must
be writable by uid 65532. Snapshots exercise image construction without proving registry
access, OIDC signing or provenance publication. The archive SBOMs describe archives,
not the complete container filesystem.

## Windows signing proof

[ADR 021](adr/021-windows-release-signing.md) adopts a separate Certum proof before
release integration. On 2026-11-11 the owner allowed fleet sessions to dispatch
the proof and accepted the stock ssign interim. `CERTUM_OTP` holds the Base32 value
of the provisioning URI's `secret` parameter, not the whole URI. ssign needs only
the email and that secret and fetches the certificate from the cloud itself.
There is no environment gate. Secrets stay at repository level.

The owner or a fleet session may dispatch it on `main`, with the owner's leave of
2026-11-11. In Actions, select **Windows signing proof** and **Run workflow** on
`main`. Vote 7 required the revised proof once after #273 merged and before any
rc1 tag; run 43084453101 satisfied that requirement. Its `sign` job builds
`iris-release-tools`, then one step holding the Certum secrets runs
`sign-windows` twice, once per file in separate processes.
This exercises two separate helper processes and the allowlisted child
environment; the native `verify` policy is unchanged. The acceptance record
must include the signing step's UTC start and end from the run's job-step
timestamps. Convert them to Unix seconds as `startUnix` and `endUnix` and
demonstrate `floor(startUnix/30) == floor(endUnix/30)` to accept the same-window
condition. A passing run whose signing step crosses a 30-second boundary is
inconclusive for that condition. Each run
spends two signatures, one per architecture; failed requests may also count.
There is no automatic retry. Rerunning `sign` or the whole workflow signs again
and can consume two further signatures; rerunning only `verify` does not sign.

When `EXPECTED_SIGNER_THUMBPRINT` is empty, `verify` first checks that the artifact
holds exactly the two expected files. It then prints each signer's public Subject,
Issuer, Thumbprint, NotBefore and NotAfter, and the timestamp signer's Subject, and
fails closed. Such a discovery run spends two signatures.

The first run, 43023491609 on 2026-11-11, showed the owner's Certum open-source
code-signing certificate: issuer Certum Code Signing 2021 CA, valid until
2027-11-10. After it was confirmed, its thumbprint
9C837C68415187A72D88612BDF14FCB0D6623D67 was pinned in the `verify` job of
`.github/workflows/signing-proof.yml`. With the pin, verification requires both
signatures to be valid, timestamped and from that certificate. A run signed by any
other certificate fails. The pin in both `signing-proof.yml` and `release.yml`,
and the CI guard's expected pin, must change when the certificate is renewed,
by 2027-11-10.

Run 43028728881 on main 658cfc8 passed all three jobs with Rust 1.99.1 and
`cargo build --release --locked`. That run signed both files in one stock ssign
process. On 2026-11-11, run 43084453101 passed all jobs through two separate
helper processes. Its signing step ran from 19:11:20 to 19:11:28 UTC, wholly
within the 19:11:00–19:11:29 TOTP window. Both endpoints have the same
`floor(unixSeconds/30)`: the same-window result is conclusive.

A passing run proves that this signer revision can sign disposable Windows
amd64 and arm64 Go builds unattended with this account, and that Windows accepts
both timestamped signatures from the committed certificate. The signer hash is
in the job summary; the signer and signed executable artifacts are retained for
one day and are not private in a public repository. The hosted signing VM is
discarded after the job. This does not prove SmartScreen reputation, program
execution, console assets, release ZIPs, checksums or publication. Vote 7 adopted
release integration after the successful stock proof, conditional on the revised
proof run described above.

## Windows release signing

The v0.1.0 release workflow exercised the Windows release path and succeeded. The following
describes the current configuration; compare each tag's handoff and verification evidence
before publication. A successful standalone stock proof does not substitute for that tag run.

`release.yml` shares the `certum-signing` concurrency group with the proof and
does not cancel an active signing run. `web` and `signer` build without secrets
and transfer attempt-specific artifacts by id. `signer` uses stock ssign at
`f7ba7b2360a3e40bb8aa062d244d95c62a743b90`, Rust 1.99.1 and `--locked`, with its
executable hash recorded in the summary. No ssign source enters Iris.

`release` builds the standard-library Go helper once with `CGO_ENABLED=0` and runs
GoReleaser v2.20.0 with `--parallelism=1`. All targets and their post-build hooks
are serialized. For each Windows tag target, the hook runs:

```text
iris-release-tools sign-windows --arch=<amd64|arm64> --path=<iris.exe>
```

`IRIS_RELEASE_HELPER` and `IRIS_WINDOWS_SIGNER` name absolute helper and signer
paths. Missing helper or credentials fails a Windows tag build; Windows snapshots
stay unsigned even with credentials present. Signing happens before ZIP creation,
checksums and cosign. The helper passes each input path positionally to ssign,
once, with a five-minute deadline, suppressed stdout/stderr and fixed messages.
There is no automatic retry or cleanup step. The signer's cached session is
`$XDG_RUNTIME_DIR/ssign/session.json` on the disposable hosted runner.

Within `release`, only the GoReleaser step receives `GITHUB_TOKEN`, `CERTUM_EMAIL`, `CERTUM_OTP`
and the five `MACOS_*` secrets. GoReleaser, hooks and the compiler inherit that
environment. ssign's child environment contains only `CERTUM_EMAIL`, `CERTUM_OTP`,
`XDG_RUNTIME_DIR`, `HOME` and `PATH`. The release token, OIDC values, Apple secrets
and GitHub command-file paths do not reach ssign through its environment. This
does not isolate malicious code sharing the runner's user and filesystem; the
whole release job is trusted, and ssign still receives the TOTP seed.

After GoReleaser creates the draft and build provenance, the Windows handoff contains only
`iris_<version>_windows_amd64.zip`, `iris_<version>_windows_arm64.zip` and the
untouched `checksums.txt` that cosign signed. The artifact is retained for one day.
The Linux image handoff is a separate artifact described [above](#release-container-image).
The secret-free `verify-windows` job downloads the Windows artifact id with digest
verification, then runs the helper:

```text
iris-release-tools verify-windows-archives --version=<tag without v> --dir=<handoff> --out=<verified>
```

It requires exactly those three files and exactly one SHA-256 entry for each ZIP,
checks both ZIP hashes before extraction, and rejects unsafe, duplicate or
unexpected members. Each ZIP must contain `iris.exe`, `LICENSE` and `docs/guard.md`.
Only `iris.exe` is extracted, to `<verified>/amd64/iris.exe` or
`<verified>/arm64/iris.exe`, with its PE machine type checked. Manifest, ZIP and
executable hashes go to the job summary. Both executables must then pass
`signtool verify /pa /all /v /tw` with exit 0, `Get-AuthenticodeSignature` status
`Valid`, a timestamp certificate and the pinned signer thumbprint. Exit 2 fails.

A wrong card, trust failure or timestamp failure is detected after the draft
exists and after signatures may have been spent. A failing GoReleaser target does
not cancel another target, so a failed release can spend two signatures. Rerunning
`release` signs again; rerunning only `verify-windows` signs nothing. Serial hooks
normally reuse the cached session, but a cache failure can stop the second sign.
Run 43084453101 met the two-process, same-window condition above. Crossing a
boundary in another proof does not authorize an automatic retry.

Same-run verification cannot show that mutable draft assets stay unchanged.
Before the owner publishes, holzera compares the draft's Windows ZIP hashes with
the `verify-windows` record and records the comparison in #271's acceptance
record. The owner only publishes. The runbook below derives the Windows procedure
from merged #273 and retains publication, withdrawal, reviewers and rerun authorization.
The tag, Rekor entries, attestations and the handoff's final ZIP bytes are already
public at that point. This integration does not establish SmartScreen reputation
or prove that either signed program runs.

The commit identifiers in the 2026-11-07 artifact measurements below predate the repository's
history rewrite. They are retained as historical evidence, not current checkout references.

To collect process-inclusive hook latency without a timing threshold:

```
IRIS_MEASURE_LATENCY=1 CGO_ENABLED=0 go test ./cmd/iris -run '^TestHookLatency$' -count=1 -v
```

The test runs 100 real binary invocations against a real hub, reports nearest-rank
p50/p95/max, and verifies 100 claims were granted. CI collects the same numbers on
Ubuntu, macOS and Windows. Local WSL/Linux under compilation load measured
p50 49.849711 ms, p95 103.718962 ms, max 133.017137 ms on 2026-11-06.

## Windows artifacts, measured

Measured on 2026-11-07 on Windows 11, with ESET Security active and Microsoft Defender off. Nothing was
published. The snapshot built from `ed0e8cc`.

- The amd64 zip holds `iris.exe`, `LICENSE` and `docs/guard.md`. It has no install instructions.
- From an empty profile, with only the unzipped folder on PATH: `iris setup --hub <hub> --clients claude
  --yes` and `iris doctor` reported `hub: ok`, `token: ok` and `claude ok`. A headless Claude Code session
  on the installed plugin joined the hub. `iris service install`, `status`, `stop`, `start` and
  `uninstall` worked, and the Run value started the hub with only `C:\Windows\System32` on PATH.
- Scoop installed the generated manifest from a local URL, checked the hash, and ran `iris` through its
  shim. A wrong hash was refused. Scoop added its shims folder to the user PATH.
- `winget validate` accepted the generated manifests. A local `winget install` was not tried, because
  enabling local manifests needs an administrator.

An unsigned `iris.exe` with Mark of the Web:

| Step | Result |
|---|---|
| Zip extracted by Explorer | The exe keeps `ZoneId=3` |
| Zip extracted by `Expand-Archive` | The exe has no mark |
| Scoop install | The exe has no mark |
| ESET on-demand scan of the zip and the exe | No detection |
| First console start | 587 ms, then 25 ms, consistent with a scan on first start |
| Start through the shell, which SmartScreen checks | `smartscreen.exe` ran, and `iris` started and exited 0 after 2.7 s |

Not measured: a real browser download, because no public asset exists yet; whether SmartScreen showed a
window, because the screen was not watched; Defender, which is off on this machine; and a machine that
enforces SmartScreen by policy. So this does not settle whether signing is needed. It shows that a
console user and a Scoop user never meet the mark, and that an Explorer user does.

## macOS artifacts, measured

Measured on 2026-11-07 on an Intel Mac with macOS 26.7.1 and Homebrew 7.0.12. Nothing was published. The
snapshot was built from `ed7e334` with `--skip=publish,sign,sbom,docker`.

- Each darwin archive holds `iris`, `LICENSE` and `docs/guard.md`. The amd64 binary is not signed at
  all. The arm64 binary carries the ad-hoc signature that Go's linker adds, which Apple silicon requires
  to run anything. The arm64 archive was not run, because the host is Intel.
- From an empty home directory with a minimal environment, the amd64 archive extracted into
  `~/.local/bin` with that directory on PATH. With a loopback hub, `iris setup --hub <hub> --clients
  claude --yes` installed and started the LaunchAgent, and `iris service status` reported `active: true`.
  `iris doctor` reported `hub: ok`, `token: ok` and `claude ok`.
- A real Claude Code session then ran on the plugin that setup generated. It called `whoami` and wrote a
  file, and the hub recorded the agent and an automatic claim. Claude Code's login belongs to the user's
  real home, so this session ran there with `--plugin-dir` pointing at the generated plugin, and with the
  installed Iris plugin turned off.
- Every archive entry is owned by `root:root` (uid and gid 0), with mode 0644 or 0755 and the commit
  time, so an archive records nothing about who built it. Two snapshots of one commit gave identical
  archive checksums.

### Gatekeeper and an unnotarized binary

Each case below used the unsigned amd64 binary.

| How it arrived | `com.apple.quarantine` on `iris` | What happened at start |
|---|---|---|
| Browser download, extracted with `tar` | yes, copied from the archive | Held: no output, never exited |
| Browser download, extracted with Archive Utility | yes | Held the same way |
| `curl` download, extracted with `tar` | no | Ran at once |
| Homebrew formula from a local tap | no | Ran at once |
| Homebrew cask from a local tap | yes, set by Homebrew Cask | Held the same way |
| Any of the held ones after `xattr -d com.apple.quarantine` | no | Ran |

The browser download was simulated: the archive was given the mark that Safari writes. `spctl --assess
--type execute` rejected the binary in every case (`no usable signature`), but only a marked binary was
held. A held process waited in state `S` until it was killed, which suggests that macOS was waiting on a
dialog. The screen was not watched, so the dialog itself, and System Settings' "Open Anyway", were not
seen. The table does not cover Apple silicon or a Mac whose policy forbids running unsigned code.

So for the first release, notarization matters for anyone who downloads with a browser, and for any
cask. It does not matter for `curl` or the formula.

### Homebrew cask, upload disabled

`.goreleaser.yaml` uses `homebrew_casks`, replacing GoReleaser v2.20's deprecated `brews`.
The block below generated a cask that installed from a local tap and linked `iris` in the
historical macOS rehearsal above. That installation measurement used an unnotarized binary.

```yaml
homebrew_casks:
  - name: iris
    repository: {owner: mateusz-klatt, name: homebrew-tap}
    homepage: https://github.com/mateusz-klatt/iris
    description: Cooperative coordination for coding agents
    skip_upload: true
    binaries: [iris]
    url:
      template: 'https://github.com/mateusz-klatt/iris/releases/download/{{ .Tag }}/{{ .ArtifactName }}'
```

A cask quarantines what it installs, which caused the held start above with the unnotarized
binary. **Keep `skip_upload: true` until a real tag's CI run has signed and notarized the
binaries.** The local signing run below does not satisfy that gate.
The local manifest does not create a published package channel. A cask hook that strips
quarantine would work around Gatekeeper for every user, so it is not proposed.

### Signing and notarization

`.goreleaser.yaml` has a `notarize.macos` entry, and `release.yml` passes it five secrets. With no
`MACOS_SIGN_P12`, the step reports `skipped: disabled`. An unset GitHub secret arrives empty, and that
also counts as unset. Measured with local snapshots:

| Secrets | Result |
|---|---|
| None, or all empty | The step is skipped and the binaries are unchanged |
| A certificate that is not a PKCS #12 file | The release stops: `unable to decode p12 file` |
| A self-signed code-signing certificate | The release stops: quill finds no chain to an Apple root |
| A raw `.p8` PEM in `MACOS_NOTARY_KEY` instead of base64 | Signing succeeded, then notarization stopped: `unable to load JWT private key bytes: unable to base64 decode key: illegal base64 data at input byte 0` |

On 2026-11-11, a Claude session ran a local snapshot at `c28c1a4` on the owner's Intel Mac
(x86_64), with macOS 26.7.3 and all five `MACOS_*` values set in the environment:

```sh
go run github.com/goreleaser/goreleaser/v2@v2.20.0 release --snapshot --clean --skip=docker,sbom,sign
```

Nothing was published. Both darwin binaries, `dist/iris_darwin_amd64_v1` and
`dist/iris_darwin_arm64_v8.0`, were signed and notarized. GoReleaser reported `notarized`
for each. The whole release took 1m17s. The configured wait for Apple's answer is up to 20 minutes.

- `codesign -dv --verbose=4` showed Developer ID Application: Mateusz Klatt (26MP7QQP95),
  then Developer ID Certification Authority and Apple Root CA. `TeamIdentifier` was `26MP7QQP95`,
  with a secure timestamp and `flags=0x10000(runtime)` (hardened runtime).
- `spctl -a -vvv -t install` accepted each binary with `source=Notarized Developer ID`.
  A ticket cannot be stapled to a bare binary. Whether this assessment fetched the ticket online
  was not measured.
- The amd64 binary ran: `iris version` reported `0.0.0-SNAPSHOT-c28c1a4 ...`.
  The arm64 binary was not run on the Intel host.

Not measured: `release.yml` in GitHub Actions with these secrets (it needs a tag; none was
pushed), a quarantined browser download of a notarized binary starting under Gatekeeper,
and Apple silicon.

| Secret | Value |
|---|---|
| `MACOS_SIGN_P12` | The Developer ID Application certificate with its private key, as a `.p12` file, base64-encoded |
| `MACOS_SIGN_PASSWORD` | The password the `.p12` was exported with |
| `MACOS_NOTARY_ISSUER_ID` | The App Store Connect issuer ID |
| `MACOS_NOTARY_KEY_ID` | The App Store Connect API key ID |
| `MACOS_NOTARY_KEY` | The API key's `.p8` file, base64-encoded |

All five GitHub repository secrets above are set as of 2026-11-11. The local run used the same
values. For credential setup in the owner's Apple account:

1. If no usable certificate is available, confirm Apple Developer Program membership. A Developer ID
   certificate needs a paid membership, and only the account holder can create one.
2. Only if needed, create a **Developer ID Application** certificate. In Xcode, it is under Settings,
   Accounts, Manage Certificates. On the developer website, it is under Certificates, from a signing
   request made in Keychain Access. Reuse the existing certificate if its private key is available.
3. In Keychain Access, export the certificate together with its private key as a `.p12` file with a
   password. `base64 -i developer-id.p12` gives `MACOS_SIGN_P12`.
4. In App Store Connect, under Users and Access, Integrations, create a team API key with the Developer
   role. Download the `.p8` file: it can be downloaded only once. Note the key ID and the issuer ID.
   `base64 -i AuthKey_<id>.p8` gives `MACOS_NOTARY_KEY`.
5. Add the five values as repository secrets under Settings, Secrets and variables, Actions.
6. Use the manual proof below to check the Actions credentials without a tag. Push a test tag only
   after deciding what it publishes: the release is a draft, but notarization submits binaries to Apple.

### macOS signing proof in Actions, configured

`.github/workflows/macos-signing-proof.yml` runs only by manual dispatch on `main` in
`mateusz-klatt/iris`. In Actions, select **macOS signing proof** and **Run workflow** on
`main`. No run is measured here. No tag is needed, and the workflow creates no release,
publishes no package or image, and requests no OIDC or write permission.

The `web` job builds the console without signing credentials. The Ubuntu `sign` job
downloads that run's console by immutable artifact id and installs GoReleaser v2.20.0.
Only its snapshot step receives the five `MACOS_*` secrets above and `WEB_DIST`:

```sh
goreleaser release --snapshot --clean --skip=docker,sbom,sign,publish
```

These four skip names are valid in [v2.20.0's release skip list](https://github.com/goreleaser/goreleaser/blob/v2.20.0/internal/skips/skips.go).
`sign` skips checksum signing by cosign; macOS signing and notarization use the separate
`notarize` stage, which remains enabled. Snapshots also [disable publishing themselves](https://github.com/goreleaser/goreleaser/blob/v2.20.0/cmd/release.go#L148-L153),
and the Windows signing hook renders empty. All five credentials must be nonempty.
The job requires a `notarized` log entry for each of `dist/iris_darwin_amd64_v1/iris`
and `dist/iris_darwin_arm64_v8.0/iris`: [this GoReleaser version](https://github.com/goreleaser/goreleaser/blob/v2.20.0/internal/pipe/notary/macos.go#L132-L151)
records no acceptance flag in artifact metadata and can return successfully after a
timeout or pending result. GoReleaser, its hooks and the compiler share the signing
step's environment; this is credential scoping, not a sandbox for untrusted code.

Only those two darwin binaries are uploaded. The secret-free `verify` job downloads
that artifact by id on `macos-latest`, requires an arm64 runner, and checks both binaries
with `codesign --verify --strict --verbose=2` and `codesign -dv --verbose=4`. It requires
Developer ID Application: Mateusz Klatt (26MP7QQP95), TeamIdentifier `26MP7QQP95`, hardened
runtime and a secure timestamp. Each `spctl -a -vvv -t install` assessment must accept
with `source=Notarized Developer ID`. Only the arm64 binary runs `iris version`.
Acceptance, native verification output and the version output go to the job summaries.
The console and binary artifacts have one-day retention and are downloadable in this
public repository.

Each signing run submits two binaries to Apple's notary service. Rerunning `sign` or
the whole workflow submits again; rerunning only `verify` submits nothing. There is no
automatic retry. A passing proof establishes signing, notarization, native assessment
of both snapshot binaries and arm64 execution in these Actions jobs. It does not test
`release.yml` itself, cask/Homebrew installation, a quarantined browser download under
Gatekeeper, or amd64 execution. Its snapshot version string differs from a tag's.
The Homebrew upload gate above still requires a real tag's signed and notarized CI run.

## Release candidate runbook

This retains the rc1 procedure and decisions below for archive verification. It does
not authorize an rc1 tag. The intended next shipping tag is the owner's final `v0.1.1`;
the [container publication procedure](#release-container-image) supplements this record.

Derived from main `951f0545b312e65b5368a732763390be27c87364` (#273) and this branch's `prerelease: auto`.
Recheck `release.yml`, `signing-proof.yml`, `.goreleaser.yaml` and ADRs 020/021 at the approved commit. Commands below are for the release operator; no rc1 tag run is measured here.

The owner decisions below are dated 2026-11-11. The rc1 tag is literally `v0.1.0-rc1`. Holzera hand-fills `<approved-40-char-sha>` from the stop/go approval record; never derive approval from the tag.

**Pre-tag stop/go record**

- Holzera cuts rc1 from `main` after Go 1.27.4 (#266), the macOS record (#264), wording fixes, the fakebin fix (#270), Windows signing (#273) and console-login cleanup merge, and that exact `main` is deployed.
  Record every merge SHA and deployment evidence. Windows signing is merged; `iris ui` and account import are removed (ADR 020, #269/#274). Open the hub URL directly.
- Record successful main CI and local `make test`, `make race`, `make cov`, `make cov-check`, `make check-all`, `make fixtures`, plus console web checks, for the approved full SHA.
  Record deployment evidence and the running hub's full identity. Stop while any required result is missing (`AGENTS.md`, `docs/deploy.md`).
- The owner signs in at the console's HTTPS address with username and password, reloads and checks the inbox. Record the result without credentials (`docs/console-activation.md`).
- Record metadata-only `gh secret list -R mateusz-klatt/iris`: `MACOS_SIGN_P12`, `MACOS_SIGN_PASSWORD`, `MACOS_NOTARY_ISSUER_ID`, `MACOS_NOTARY_KEY_ID`, `MACOS_NOTARY_KEY`, `CERTUM_EMAIL`, `CERTUM_OTP`.
  Name presence does not validate values; both Apple and Certum credentials are used by the configured release step.
- Inspect effective tag creation, update and deletion permissions, including legacy protection and bypass actors; record what is actually restricted. Holzera-only cutting and main/deployment approval are fleet rules, not workflow enforcement (`.github/workflows/release.yml`).
- Record a named verifier for every OS/architecture. Stop if required signing is absent, skipped or unverifiable, except for unavailable arm64 darwin measurements as described below.
- Record both measured Windows proofs on 2026-11-11: [43028728881](https://github.com/mateusz-klatt/iris/actions/runs/43028728881) passed with the certificate pin; [43084453101](https://github.com/mateusz-klatt/iris/actions/runs/43084453101) passed all jobs with two helper processes, signing from 19:11:20 to 19:11:28 UTC in one 30-second TOTP window (conclusive). These satisfy vote 7's proof requirement, not rc1 ZIP or publication verification.
- No session publishes the draft or makes a final, non-rc tag. Only the owner publishes after the acceptance record below passes. These stop/go records do not authorize publication by a session.

**Prerelease configuration**

`prerelease: auto` is configured in `.goreleaser.yaml`, decided by fleet vote 6 on 2026-11-11. Expect `isPrerelease=true` for `v0.1.0-rc1` (`prerelease=true` in REST); check the flag before owner publication ([pinned prerelease handling](https://github.com/goreleaser/goreleaser/blob/b8f79e0399c46895220e8479e24d599576b563a8/internal/pipe/release/release.go#L74-L84)).
GoReleaser v2.20.0 creates the draft with that flag and does not set Latest on its draft path. GitHub excludes drafts and prereleases from Latest, so an rc published with the prerelease flag retained cannot become Latest ([draft path](https://github.com/goreleaser/goreleaser/blob/b8f79e0399c46895220e8479e24d599576b563a8/internal/client/github.go#L537-L597), [GitHub REST documentation](https://docs.github.com/en/rest/releases/releases#create-a-release)). This is documented behaviour; rc1 publication is **not measured**.

**What pushing the tag does — expected, not measured**

1. `v0.1.0-rc1` matches the version-tag pattern. The workflow checks the repository name, but not the pusher, approved SHA, main ancestry, test results or deployment. It shares `certum-signing` concurrency with the proof, with `cancel-in-progress: false` (`.github/workflows/release.yml`, `.github/workflows/signing-proof.yml`).
2. `web` builds the console with Node 26; `signer` independently builds stock ssign at `f7ba7b2360a3e40bb8aa062d244d95c62a743b90` with Rust 1.99.1 and `cargo build --release --locked`, recording its SHA-256. Both jobs have read-only tokens and no secrets. Their artifacts are named `webui-dist-<run-id>-<attempt>` and `certum-signer-<run-id>-<attempt>`, retained for one day.
3. `release` needs both jobs and downloads each artifact by immutable id outside the checkout. It builds `iris-release-tools` with `CGO_ENABLED=0`, installs GoReleaser v2.20.0, cosign and Syft, then runs `goreleaser release --clean --parallelism=1`. The before hook copies the console without rerunning npm (`.github/workflows/release.yml`, `.goreleaser.yaml`, `Makefile`).
4. Six binaries cover Linux, darwin and Windows on amd64/arm64, with `CGO_ENABLED=0`, `-trimpath` and stamped version/full commit/source state. Each Windows post-build hook calls `iris-release-tools sign-windows --arch=<amd64|arm64> --path=<iris.exe>` once, before archives, checksums and cosign. Missing Windows helper or credentials fails the build. All targets are serialized; snapshots skip the signing hook. Apple signing/notarization also precedes packaging; provenance follows GoReleaser (`.goreleaser.yaml`; [pinned pipeline](https://github.com/goreleaser/goreleaser/blob/b8f79e0399c46895220e8479e24d599576b563a8/internal/pipeline/pipeline.go#L99-L173)).
5. A nonempty P12 enables darwin signing. Missing any notary credential permits signed-only output; missing P12 permits output without Developer ID signing and a green run. Require all five names before tagging and `notarized` for both binaries afterward (`.goreleaser.yaml`; [skip conditions](https://github.com/goreleaser/goreleaser/blob/b8f79e0399c46895220e8479e24d599576b563a8/internal/pipe/notary/macos.go#L65-L151)).
6. A tag run creates a **draft**, with `prerelease=true` for rc1 (`.goreleaser.yaml`).
   An unpublished draft stays a draft until deleted; its assets require write/push access ([draft access](https://github.com/github/rest-api-description/blob/e59da575720241498e75762a659b7ccb86e512fa/descriptions/api.github.com/api.github.com.json#L93448)). The tag is public immediately; Rekor entries and public build attestations are public when created, before owner publication (irreversible effects below).
7. After draft creation and provenance, `release` uploads `windows-release-<run-id>-<attempt>`, retained for one day: the two final Windows ZIPs and the untouched, cosign-signed `checksums.txt`. These ZIP bytes are publicly downloadable before publication.
8. `verify-windows` needs `release`, runs on `windows-latest` without secrets and downloads that handoff by artifact id. The Go helper requires exactly those three files and one SHA-256 entry per ZIP, checks both ZIP hashes before extraction, rejects unsafe/duplicate/unexpected members, requires `iris.exe`, `LICENSE` and `docs/guard.md`, and extracts only each executable to a fixed architecture path. It checks AMD64/ARM64 PE machine types and records manifest, ZIP and executable hashes. Each executable then needs `signtool verify /pa /all /v /tw` exit 0, Authenticode `Valid`, a timestamp certificate and thumbprint `9C837C68415187A72D88612BDF14FCB0D6623D67` (`.github/workflows/release.yml`, ADR 021). A failure leaves the draft for review.
9. This first rc is also the first release-workflow run and first Actions test of Apple signing/notarization, **expected**, not established by the [local snapshot](#signing-and-notarization).
   Both darwin binaries were notarized locally; only amd64 ran. Apple silicon and quarantined notarized startup remain **not measured**.

**Expected assets and destinations**

| Output | Draft assets or other destination |
|---|---|
| Six archives | `iris_0.1.0-rc1_{linux,darwin}_{amd64,arm64}.tar.gz` and `iris_0.1.0-rc1_windows_{amd64,arm64}.zip`; each has its binary, `LICENSE`, `docs/guard.md` (`.goreleaser.yaml`, `archives`). |
| Four packages | `iris_0.1.0~rc1_amd64.deb`, `iris_0.1.0~rc1_arm64.deb`, `iris-0.1.0~rc1-1.x86_64.rpm`, `iris-0.1.0~rc1-1.aarch64.rpm`; package `iris`, binary `/usr/bin/iris` (`.goreleaser.yaml`, `nfpms`; [deb names](https://github.com/goreleaser/nfpm/blob/db22372bfbddb6b2968ff71e9c25e6bb1a2355c2/deb/deb.go#L87-L105), [rpm names](https://github.com/goreleaser/nfpm/blob/db22372bfbddb6b2968ff71e9c25e6bb1a2355c2/rpm/rpm.go#L75-L103)). |
| Six SBOMs | `<archive filename>.sbom.json`, SPDX JSON; no package SBOM configured (`.goreleaser.yaml`, `sboms`; [defaults](https://github.com/goreleaser/goreleaser/blob/b8f79e0399c46895220e8479e24d599576b563a8/internal/pipe/sbom/sbom.go#L61-L87)). |
| Checksum and bundle | `checksums.txt` (SHA-256) and `checksums.txt.sigstore.json`; no separate signature/certificate files (`.goreleaser.yaml`, `checksum`/`signs`; [checksum default](https://github.com/goreleaser/goreleaser/blob/b8f79e0399c46895220e8479e24d599576b563a8/internal/pipe/checksums/checksums.go#L34-L47)). |
| Provenance | GitHub attestation storage for the 16 subjects inside `checksums.txt`, not an attached provenance asset (`.github/workflows/release.yml`, `release`; [checksum exclusions](https://github.com/goreleaser/goreleaser/blob/b8f79e0399c46895220e8479e24d599576b563a8/internal/pipe/checksums/checksums.go#L183-L217)). |

Expect 18 uploaded release assets. The Windows and Linux-image Actions handoffs add no release assets; GitHub source archive links are separate ([upload types](https://github.com/goreleaser/goreleaser/blob/b8f79e0399c46895220e8479e24d599576b563a8/internal/artifact/artifact.go#L130-L153)). The separate `image` job publishes to GHCR after release and Windows verification succeed, as described above. Homebrew, Scoop and winget manifests stay local through `skip_upload: true` (`.goreleaser.yaml`).

**What this cannot do**

- There is no human gate on the tag run. The owner chose no workflow environment gate; all Apple/Certum secrets stay at repository level (ADR 021). The unused `signing` environment stays unchanged.
- A workflow on any branch pushed to this repository can name repository secrets. Every fleet session uses the owner's GitHub account, so tag protection cannot distinguish owner from fleet. Inspect effective permissions before the cut.
- Within `release`, only the GoReleaser step receives the release token and Apple/Certum secrets. Its hooks and compiler inherit them. The helper gives ssign only `CERTUM_EMAIL`, `CERTUM_OTP`, `XDG_RUNTIME_DIR`, `HOME` and `PATH`; this filters the environment, not other processes sharing the runner. Earlier actions can alter tools or leave processes. Write-token/OIDC permissions cover the release job (`.github/workflows/release.yml`; [GitHub token context](https://github.com/github/docs/blob/b1faf75dbd3e7627ecdfaab5241b91953e308ddb/content/actions/tutorials/authenticate-with-github_token.md)).
  Protect Apple/Windows signing identities and credentials, token, OIDC identity and candidate from operator mistakes, malicious pushers and compromised actions/installers/dependencies. Suspected compromise requires revocation/rotation, not merely draft deletion.
- Console and signer artifact digest validation checks transport integrity, not the producer's honesty. A compromised producer can supply code that is embedded or used to sign; provenance establishes origin, not approval or safe behaviour (`.github/workflows/release.yml`; [download mismatch failure](https://github.com/actions/download-artifact/blob/9000827ccba6bdab643e8b6fd33ac0654aef8333/src/download-artifact.ts#L233-L241)).
- Draft access is not secrecy: tag/source and public workflow logs are public; signed-in readers can download public-repository workflow artifacts. Keep credentials out of logs/artifacts ([artifact access](https://github.com/github/docs/blob/b1faf75dbd3e7627ecdfaab5241b91953e308ddb/content/actions/how-tos/manage-workflow-runs/download-workflow-artifacts.md#L1-L22)); transparency is public too.

**Acceptance and evidence record**

Record literal tag, approved SHA, merge/deployment evidence, release ID, run ID/attempt, console/signer/handoff artifact IDs and digests, signer executable SHA-256, installed tool versions, both proof runs and each result below.
Each platform has a named verifier. Unavailable measurements stay **not measured**; never report them as passed.
Unavailable arm64 darwin `codesign`, `spctl` and `iris version` results do not by themselves block acceptance; the owner weighs that gap before publication. Other unavailable measurements block acceptance.
Use Bash with `set -euo pipefail`, jq and an authenticated account with draft access. Record `gh version` and `cosign version`; these commands target gh 2.103.3 and cosign v3.1.4.
Use a fresh download directory and stop on any failure. Operator versions are **not verified** until recorded; candidate verification is **not measured** ([gh flags](https://github.com/cli/cli/blob/63837fb778d6a39f02be0ce76ff89f2bc8d74bdc/pkg/cmd/attestation/verify/verify.go#L260-L268)).
At cut time holzera uses the hand-filled approved SHA, after the stop/go record passes. First confirm `git remote get-url --push origin` names `mateusz-klatt/iris`:

```sh
set -euo pipefail
APPROVED='<approved-40-char-sha>'
[[ "$APPROVED" =~ ^[0-9a-f]{40}$ ]]
test "$(git rev-parse main)" = "$APPROVED"
test "$(git rev-parse HEAD)" = "$APPROVED"
test -z "$(git status --porcelain)"
test "$(gh api --method GET repos/mateusz-klatt/iris/git/ref/heads/main --jq .object.sha)" = "$APPROVED"
git tag v0.1.0-rc1 "$APPROVED"
git push origin refs/tags/v0.1.0-rc1
test "$(git rev-parse 'v0.1.0-rc1^{commit}')" = "$APPROVED"
gh run list -R mateusz-klatt/iris --workflow release.yml --branch v0.1.0-rc1 --event push --commit "$APPROVED" --json databaseId,headBranch,headSha,event,attempt,status,conclusion,url
RUN_ID='<selected-run-id>'
RUN_ATTEMPT='<selected-run-attempt>'
WORKFLOW_ID=$(gh api --method GET repos/mateusz-klatt/iris/actions/workflows/release.yml --jq .id)
RUN_JSON=$(gh run view "$RUN_ID" -R mateusz-klatt/iris --attempt "$RUN_ATTEMPT" --json headBranch,headSha,event,workflowDatabaseId,attempt,status,conclusion,jobs,url)
printf '%s\n' "$RUN_JSON" | jq .
printf '%s\n' "$RUN_JSON" | jq -e --arg approved "$APPROVED" --argjson workflow "$WORKFLOW_ID" --argjson attempt "$RUN_ATTEMPT" '.headBranch == "v0.1.0-rc1" and .headSha == $approved and .event == "push" and .workflowDatabaseId == $workflow and .attempt == $attempt and .status == "completed" and .conclusion == "success"'
```

Wait for the intended attempt to complete. Preserve that exact attempt's job results, logs and console artifact metadata.
Require successful `web`, `signer`, `release` and `verify-windows` jobs, with no skipped required signing. Record both Apple `notarized` messages and the Windows verification summary, including manifest, ZIP and executable SHA-256 values and both timestamped signatures with the committed pin. In the install logs require GoReleaser download checksum **and** cosign verification success, without skip/fallback warnings ([action verification branches](https://github.com/goreleaser/goreleaser-action/blob/a4f02ab5355a589de60d917a659ce617ed19b3de/src/goreleaser.ts#L72-L146)).
Record actual cosign/Syft versions; pinned defaults are [v3.1.4](https://github.com/sigstore/cosign-installer/blob/756fe2d284c7292c4e9b62c8a28f1e5629c59532/action.yml#L10-L13) and [v1.54.0](https://github.com/anchore/sbom-action/blob/66cbf4bc1f1c0d2edc94016e65bc221b6bb0ad6c/src/SyftVersion.ts#L1).
Record all attempt-specific artifact names, IDs/digests and producer run, including the Linux-image handoff, from the workflow metadata/logs (`.github/workflows/release.yml`). Record the GHCR index digest and both verification results separately.
Before validation, rerun or cleanup, enumerate **all** matching releases. Require exactly one intended draft, stop if any is published, and record its numeric ID. The absent `replace_existing_draft`, `use_existing_draft` and `replace_existing_artifacts` options default false; GoReleaser reruns may create another draft; a published release is not redrafted, and existing asset names fail by default ([default fields](https://github.com/goreleaser/goreleaser/blob/b8f79e0399c46895220e8479e24d599576b563a8/pkg/config/config.go#L694-L713), [release lookup/update](https://github.com/goreleaser/goreleaser/blob/b8f79e0399c46895220e8479e24d599576b563a8/internal/client/github.go#L609-L674), [asset conflicts](https://github.com/goreleaser/goreleaser/blob/b8f79e0399c46895220e8479e24d599576b563a8/internal/client/github.go#L834-L842)).
If there is no release, investigate the failed run; do not pass this guard.
gh's tag lookup races published-release REST and draft GraphQL requests; it does not establish uniqueness ([pinned lookup](https://github.com/cli/cli/blob/63837fb778d6a39f02be0ce76ff89f2bc8d74bdc/pkg/cmd/release/shared/fetch.go#L182-L225)).
gh 2.103.3 rejects `--slurp` together with `--jq`; pipe to external jq ([CLI restriction](https://github.com/cli/cli/blob/63837fb778d6a39f02be0ce76ff89f2bc8d74bdc/pkg/cmd/api/api.go#L260-L267)).

```sh
matches=$(gh api --method GET --paginate --slurp repos/mateusz-klatt/iris/releases | jq '[.[][] | select(.tag_name == "v0.1.0-rc1")]')
printf '%s\n' "$matches" | jq '.[] | {id,tag_name,draft,prerelease,assets:[.assets[].name]}'
printf '%s\n' "$matches" | jq -e 'length == 1 and all(.[]; .draft == true and .prerelease == true and (.assets | length) == 18)'
DRAFT_ID='<literal-draft-id>'
test "$(printf '%s\n' "$matches" | jq -r '.[0].id')" = "$DRAFT_ID"
test ! -e rc-assets-v0.1.0-rc1
gh release download v0.1.0-rc1 -R mateusz-klatt/iris --dir rc-assets-v0.1.0-rc1
cd rc-assets-v0.1.0-rc1
cosign verify-blob checksums.txt --bundle checksums.txt.sigstore.json \
  --certificate-identity 'https://github.com/mateusz-klatt/iris/.github/workflows/release.yml@refs/tags/v0.1.0-rc1' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
shasum -a 256 -c checksums.txt
test "$(wc -l < checksums.txt | tr -d ' ')" = 16
while read -r digest asset; do
  gh attestation verify "$asset" --repo mateusz-klatt/iris \
    --cert-identity 'https://github.com/mateusz-klatt/iris/.github/workflows/release.yml@refs/tags/v0.1.0-rc1' \
    --cert-oidc-issuer https://token.actions.githubusercontent.com \
    --source-ref refs/tags/v0.1.0-rc1 --source-digest "$APPROVED"
done < checksums.txt
```

Require the complete inventory above: 18 release assets, 16 checksum/provenance subjects and `prerelease=true`.
Under vote 7 2A, holzera compares the SHA-256 of each downloaded draft Windows ZIP with that exact attempt's `verify-windows` summary. Record both filenames, both sets of hashes and equality in the acceptance record. Stop on any mismatch; same-run handoff verification cannot establish that mutable draft assets stayed unchanged.
On Linux use `sha256sum -c checksums.txt` instead. Record signature, checksum and every provenance result; signatures do not establish that the source was approved.
Extract each darwin archive separately. Run the commands below against the amd64 binary on the owner's Intel Mac (x86_64).
For arm64, run them on a GitHub `macos-latest` runner (Apple silicon), or record them as **not measured** for the owner to weigh before publishing the rc ([runner specifications](https://docs.github.com/en/actions/reference/runners/github-hosted-runners#standard-github-hosted-runners-for-public-repositories)). No runner execution is measured here.
For measured results, require Developer ID Application: Mateusz Klatt (26MP7QQP95), TeamIdentifier `26MP7QQP95`, secure timestamp, hardened runtime and accepted Notarized Developer ID assessment ([signing evidence](#signing-and-notarization)).

```sh
codesign -dv --verbose=4 ./iris
codesign --verify --strict --verbose=2 ./iris
spctl -a -vvv -t install ./iris
./iris version
```

For Gatekeeper, use a separate authenticated browser download of the darwin amd64 archive on the owner's Intel Mac. **Before extraction or execution**, compare that archive's SHA-256 with the already cosign-verified `checksums.txt`.
Preserve quarantine; check `xattr -p com.apple.quarantine ./iris` before first execution. Record host, OS/architecture, network, assessment, prompts, output and any hang. A download without quarantine proves no quarantined-startup result ([Gatekeeper measurement](#gatekeeper-and-an-unnotarized-binary)).
Inspect all four Linux packages using explicit filenames; expected Debian Version `0.1.0~rc1`, RPM Version `0.1.0~rc1` and Release `1`, correct architectures, package `iris` and `/usr/bin/iris` ([deb metadata](https://github.com/goreleaser/nfpm/blob/db22372bfbddb6b2968ff71e9c25e6bb1a2355c2/deb/deb.go#L774-L783), [rpm version](https://github.com/goreleaser/nfpm/blob/db22372bfbddb6b2968ff71e9c25e6bb1a2355c2/rpm/rpm.go#L134-L145), [rpm release](https://github.com/goreleaser/nfpm/blob/db22372bfbddb6b2968ff71e9c25e6bb1a2355c2/rpm/builder.go#L44-L51)).

```sh
for deb in iris_0.1.0~rc1_amd64.deb iris_0.1.0~rc1_arm64.deb; do
  dpkg-deb -f "$deb" Package Version Architecture; dpkg-deb -c "$deb"
done
for rpm in iris-0.1.0~rc1-1.x86_64.rpm iris-0.1.0~rc1-1.aarch64.rpm; do
  rpm -qp --queryformat '%{NAME} %{VERSION} %{RELEASE} %{ARCH}\n' "$rpm"; rpm -qpl "$rpm"
done
```

On Windows, extract the two matched draft ZIPs separately. For each `iris.exe`, require `signtool verify /pa /all /v /tw .\iris.exe` to exit 0 (exit 2 fails), then record `Get-AuthenticodeSignature .\iris.exe | Format-List Status,StatusMessage,SignerCertificate,TimeStamperCertificate`.
Require `Valid`, a non-null timestamp certificate and signer thumbprint `9C837C68415187A72D88612BDF14FCB0D6623D67`. Expected signer subject: `CN=Open Source Developer Mateusz Klatt, O=Open Source Developer, L=Warszawa, S=mazowieckie, C=PL` (ADR 021). Missing/skipped/unverifiable signing blocks acceptance. Native signature verification does not establish SmartScreen reputation or successful program execution.
Run `iris version` from each native binary (`.\iris.exe version` on Windows), with the arm64 darwin option above. Expect `0.1.0-rc1 (commit <approved-40-char-sha>; source clean; console unverified)`; record actual output. The console field proves neither freshness nor completeness (`.goreleaser.yaml`, `builds.ldflags`; `internal/buildinfo/buildinfo.go`).
Before publishing, the owner reviews the complete acceptance record: approved SHA/run, one complete draft, prerelease flag, tool verification, console/signer/handoff artifacts, all assets/checksums/provenance, both Apple notarizations, Intel Mac Gatekeeper measurements, both Windows signatures/verify job, holzera's two draft ZIP hash comparisons, package metadata and native execution results, including any **not measured** arm64 darwin checks.
Only he publishes; a draft nobody publishes remains a draft until deleted.

**Failure handling and take-back**

Signing errors, Apple rejection/API errors and per-binary 20-minute polling timeout are **expected** to fail GoReleaser. Notarization fails before fresh draft creation, but Apple submissions may already exist; later uploads can leave partial drafts and provenance failure can leave complete assets without provenance (`.goreleaser.yaml`; [notary errors](https://github.com/goreleaser/goreleaser/blob/b8f79e0399c46895220e8479e24d599576b563a8/internal/pipe/notary/macos.go#L103-L151), [polling](https://github.com/goreleaser/quill/blob/8310f3e9a321345133031727c1fda85f6fe162d3/quill/notary/status.go#L25-L64), [pipeline](https://github.com/goreleaser/goreleaser/blob/b8f79e0399c46895220e8479e24d599576b563a8/internal/pipeline/pipeline.go#L99-L173)).
`verify-windows` fails after draft creation. Each helper call submits once with a five-minute deadline and no retry; a failed release may still spend two signatures. A rerun of `release` signs again; a rerun of only `verify-windows` signs nothing. No automatic retry or variable-path cleanup is authorized (ADR 021).
Freeze release changes first; identify and cancel every active run for this tag, then confirm all are stopped. Preserve run IDs/attempts, logs, release IDs, hashes and failure evidence.
Do not rerun blindly: enumerate releases again. For cleanup require exactly one intended draft and no published match; otherwise stop for review.
Keep an issued public tag by default and record the candidate as withdrawn. Never repoint or reuse it. Use the next fleet-agreed rc after changed source/configuration/signing inputs or issued external records; this numbering rule is **proposed, not verified as adopted**. An unchanged transient retry still needs explicit state review.
The following deletion is documentary only. Replace every `<literal-draft-id>` below with the same reviewed numeric ID **literally**, without variables or command substitution in the deletion command.
GET-to-DELETE is not atomic; keep changes frozen ([ID endpoint](https://github.com/github/rest-api-description/blob/e59da575720241498e75762a659b7ccb86e512fa/descriptions/api.github.com/api.github.com.json#L94010-L94030)).

```sh
set -euo pipefail
matches=$(gh api --method GET --paginate --slurp repos/mateusz-klatt/iris/releases | jq '[.[][] | select(.tag_name == "v0.1.0-rc1")]')
printf '%s\n' "$matches" | jq '.[] | {id,tag_name,draft,prerelease}'
printf '%s\n' "$matches" | jq -e 'length == 1 and all(.[]; .draft == true)'
test "$(printf '%s\n' "$matches" | jq -r '.[0].id')" = '<literal-draft-id>'
gh api --method GET 'repos/mateusz-klatt/iris/releases/<literal-draft-id>' | jq -e '.tag_name == "v0.1.0-rc1" and .draft == true'
gh api --method DELETE 'repos/mateusz-klatt/iris/releases/<literal-draft-id>'
remaining=$(gh api --method GET --paginate --slurp repos/mateusz-klatt/iris/releases | jq '[.[][] | select(.tag_name == "v0.1.0-rc1")] | length')
test "$remaining" = 0
```

An API error is not absence. Delete a tag only after separately agreed cleanup; verify `git remote get-url --push origin` names this repository first. No session substitutes a variable into a deletion target:

```sh
git push origin --delete v0.1.0-rc1
remote_tags=$(git ls-remote --tags origin refs/tags/v0.1.0-rc1)
test -z "$remote_tags"
```

If this checkout contains local tag `v0.1.0-rc1`, delete that local copy with `git tag -d v0.1.0-rc1`. A missing local tag requires no local cleanup.
Deleting release/tag does not roll back the hub. Follow `docs/deploy.md` and [Rollback while preserving the browser boundary](console-activation.md#rollback-while-preserving-the-browser-boundary): retain a supported named-authentication backend; before any older backend the owner establishes the documented public-browser isolation boundary. Review database compatibility/backups and lost writes separately.

**Irreversible effects**

Apple submissions and cosign transparency entries are **expected** before fresh draft creation; deleting GitHub objects recalls neither submissions, immutable transparency evidence nor downloaded copies (`.goreleaser.yaml`; [pipeline order](https://github.com/goreleaser/goreleaser/blob/b8f79e0399c46895220e8479e24d599576b563a8/internal/pipeline/pipeline.go#L99-L173)).
Public-repository attestations are stored by GitHub and copied to a public immutable Sigstore log ([pinned GitHub documentation](https://github.com/github/docs/blob/b1faf75dbd3e7627ecdfaab5241b91953e308ddb/content/actions/concepts/security/artifact-attestations.md#L24-L30)).
GitHub-stored attestations, Actions logs/artifacts and Apple records have separate lifecycles. Console, signer, Linux-image and Windows handoff artifacts have one-day retention (`.github/workflows/release.yml`); repository retention at cut time and Apple retention/deletion policy are **not verified** for this runbook. The handoff's final ZIP bytes and GHCR image can be public before owner publication; draft deletion recalls neither those copies nor signatures and provenance already published.
