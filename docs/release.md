# Snapshots and releases

The repository is public, and nothing is published yet: no release, no package-manager update
and no image. A pushed version tag runs `.github/workflows/release.yml`, which makes a **draft**
GitHub release (`release: draft: true`) that stays private until the owner publishes it. The tag
itself is public as soon as it is pushed. Homebrew,
Scoop and winget are `skip_upload: true`. Docker builds are enabled only for snapshots, which never
push. Do not enable publishing until D15's later stages are approved.
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
upload stays disabled until signing and notarization have really succeeded. `dist/` is
ignored by Git; keep snapshot output out of commits.

The before hook runs `make web`, requiring the Node version in `web/package.json`
and npm. Release builds embed the rebuilt console, use `CGO_ENABLED=0`, `-trimpath`,
and stamp the release version, full commit and source dirty state through ldflags, using
GoReleaser's Git metadata captured before the console build. `iris version`, the doctor bundle,
MCP server identity and authenticated hub status use the same value, such as
`1.2.3 (commit <full hash>; clean)`. Plain `go build` and `make build` use Go's embedded VCS
metadata; a source archive or `-buildvcs=false` without explicit linker metadata reports unknown
identity rather than claiming a clean checkout. `make build VERSION=...` overrides the version
label. These fields identify a build; they do not attest to its provenance.
Outputs cover Linux, macOS and Windows on
amd64 and arm64: four tar.gz archives, two Windows zip archives, checksums, and
Linux deb/rpm packages named `iris`. Package-manager manifests remain local. Their explicit download URL templates refer to
future GitHub assets; private snapshot assets are not public install channels.

The server image uses `dockers_v2` for Linux amd64/arm64. Snapshots load local
platform images without pushing; release publication remains a separate opt-in.
It runs as uid/gid 65532 and stores the database and token under `/data`, which must be
writable by that user. A fresh named volume is; a bind mount needs its owner set. For externally reachable serving,
pass `--listen`, `--tls-cert` and `--tls-key` explicitly, as required by D1.
Snapshot checksum signing and SBOM generation are skipped by the command above. The tag
workflow already installs syft and cosign, generates an SBOM per archive, signs checksums
with keyless cosign using its OIDC identity, and creates GitHub build provenance. These
release steps are configured; their first real tag run is still pending. Apple signing
and notarization are separate and stay disabled without `MACOS_SIGN_P12`.

The commit identifiers in the measured artifact sections below predate the repository's
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
binary. **Do not enable cask uploads before real signing and notarization have completed
successfully.** Creating the certificate or adding secrets alone does not satisfy that gate.
The local manifest does not create a published package channel. A cask hook that strips
quarantine would work around Gatekeeper for every user, so it is not proposed.

### Signing and notarization

`.goreleaser.yaml` has a `notarize.macos` entry, and `release.yml` passes it five secrets. With no
`MACOS_SIGN_P12`, the step reports `skipped: disabled`. An unset GitHub secret arrives empty, and that
also counts as unset. Measured with the snapshot, without any Apple credential:

| Secrets | Result |
|---|---|
| None, or all empty | The step is skipped and the binaries are unchanged |
| A certificate that is not a PKCS #12 file | The release stops: `unable to decode p12 file` |
| A self-signed code-signing certificate | The release stops: quill finds no chain to an Apple root |

Real signing and notarization have not been run. With the certificate alone the binaries would be
signed but not notarized. With the API key as well, they are notarized, and GoReleaser waits up to 20
minutes for Apple's answer. Apple does not staple a ticket to a bare binary, so Gatekeeper is expected
to look it up online the first time it starts. That was not measured.

| Secret | Value |
|---|---|
| `MACOS_SIGN_P12` | The Developer ID Application certificate with its private key, as a `.p12` file, base64-encoded |
| `MACOS_SIGN_PASSWORD` | The password the `.p12` was exported with |
| `MACOS_NOTARY_ISSUER_ID` | The App Store Connect issuer ID |
| `MACOS_NOTARY_KEY_ID` | The App Store Connect API key ID |
| `MACOS_NOTARY_KEY` | The API key's `.p8` file, base64-encoded |

The owner already has a Developer ID Application certificate. The App Store Connect API key
and all five GitHub repository secrets above are still missing. The remaining setup is for
the owner in the owner's Apple account; no credentials were created or configured here.

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
6. Push a test tag only after deciding what it publishes: the release is a draft, but notarization
   submits the binaries to Apple.
