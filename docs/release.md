# Stage 0 snapshots

The private repository does not publish releases or package-manager updates.
`.goreleaser.yaml` disables GitHub releases and sets Homebrew, Scoop and winget
`skip_upload: true`. Docker builds are enabled only for snapshots, which never push. Do not enable publishing until D15's later stages are approved.
The GoReleaser dependency is not part of the application module.

Use GoReleaser **v2.20.0** (the official binary, Docker image, or `go run github.com/goreleaser/goreleaser/v2@v2.20.0`) from the repository root:

```
goreleaser check
goreleaser release --snapshot --clean --skip=publish,sign,sbom
```

In v2.20, `check` returns **2** for this structurally valid configuration because
`brews` is deprecated. This is intentional until Apple notarization credentials
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
It stores the database and token under `/data`. For externally reachable serving,
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
