# Stage 0 snapshots

The repository is public, but stage 0 still publishes nothing: no release, no
package-manager update and no image. `.goreleaser.yaml` disables GitHub releases and sets Homebrew, Scoop and winget
`skip_upload: true`. Docker builds are enabled only for snapshots, which never push. Do not enable publishing until D15's later stages are approved.
The GoReleaser dependency is not part of the application module.

Use GoReleaser **v2.20.0** (the official binary, Docker image, or `go run github.com/goreleaser/goreleaser/v2@v2.20.0`) from the repository root:

```
goreleaser check
goreleaser release --snapshot --clean --skip=publish,sign,sbom
```

Images are built last. Without a running Docker daemon that step fails with
`could not build docker image`, after the archives and manifests are already in `dist/`. Start Docker, or
add `docker` to `--skip` when the archives are all you need. Measured on Windows 11 with Docker stopped.

On Windows, `make web` needs `rm`, `mkdir -p` and `cp -a`. The measured snapshot ran from Git Bash. It
has not been tried from PowerShell alone.

In v2.20, `check` returns **2** for this structurally valid configuration because
`brews` is deprecated. Through `go run` the same result prints `exit status 2`, and `go run` itself exits 1. This is intentional until Apple notarization credentials
are available. CI reports that warning and accepts exit 2; invalid configuration
(exit 1) still fails. The formula avoids cask quarantine but does not establish
notarization. Replace `brews` before GoReleaser v3.

The before hook runs `make web`, requiring the Node version in `web/package.json`
and npm. Release builds embed the rebuilt console, use `CGO_ENABLED=0`, `-trimpath`,
and set `main.version` through ldflags. Outputs cover Linux, macOS and Windows on
amd64 and arm64: four tar.gz archives, two Windows zip archives, checksums, and
Linux deb/rpm packages named `iris`. Package-manager manifests remain local. Their explicit download URL templates refer to
future GitHub assets; private snapshot assets are not public install channels.

The server image uses `dockers_v2` for Linux amd64/arm64. Snapshots load local
platform images without pushing; release publication remains a separate opt-in.
It runs as uid/gid 65532 and stores the database and token under `/data`, which must be
writable by that user. A fresh named volume is; a bind mount needs its owner set. For externally reachable serving,
pass `--listen`, `--tls-cert` and `--tls-key` explicitly, as required by D1.
Snapshot signing and SBOM generation are skipped by the command above. Later
releases can generate archive SBOMs with syft and sign checksums with keyless
cosign; this requires those tools and an OIDC identity.

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
