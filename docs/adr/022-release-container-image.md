# 022: Publish signed release container images separately

Status: accepted on the owner's direction, 2026-11-12. The shipping tag `v0.1.1`
is a final release and remains the owner's decision.

## Problem

D15 and the snapshot pipeline prepare container images but disable release image
publication. The owner wants every `v*` release tag to publish an image to
`ghcr.io/mateusz-klatt/iris` with keyless signatures and build provenance. Giving
the existing release job registry write permission would extend the authority
of the runner that holds the Apple and Certum credentials.

The root and deployment Dockerfiles also describe different runtime layouts.
The public hub's deployment procedure must retain `/iris`, writable `/data`,
the existing source metadata arguments and its CI-built image handoff.

## Decision

Add an `image` job to `release.yml`. It requires an owner-repository tag push
and successful `release` and `verify-windows` jobs. The release job uploads the
two Linux archives and original checksums through an artifact retained for one
day. The image job downloads that exact immutable artifact ID, checks the
archive hashes and extracts the two binaries. It packages those bytes without
running the Node build or compiling another binary.

Only `image` receives `packages: write`. It also receives `contents: read`,
`id-token: write` and `attestations: write`. The existing release job keeps its
permissions and its GoReleaser step keeps the same signing-secret environment.
The image job authenticates to GHCR with its own GitHub token and receives no
Apple or Certum credentials. A job boundary keeps registry writes out of the
signing runner; it does not protect against a compromised repository or service.

Publish one index containing `linux/amd64` and `linux/arm64`, under the full
release tag. Stable ref names without `-` also update `latest`; RC and other
prerelease tags publish only the version tag, with the same signing and attestation.
This keeps prereleases from replacing the stable image. The container tag rule
is explicit; GoReleaser's `prerelease: auto` controls only the GitHub release.
Cosign signs the returned index digest keylessly;
GitHub build provenance attests the same digest and is stored in GitHub and
the registry. GoReleaser v2.20.0 keeps `dockers_v2` disabled outside snapshots,
so the secret-bearing release step has no second image publication path.
Snapshots and pull requests push nothing. All actions use full commit pins.

Use one root `Dockerfile`. Its `deploy` target retains the source build used
by `make image`, while the default `release` target copies prebuilt binaries
from platform directories. Both use the same distroless runtime with numeric
uid/gid 65532, `/iris`, writable `/data` and `TMPDIR=/data`. The release target
needs no emulation because it runs no target-platform commands.

The public hub continues to consume the CI-exported `iris-iris` image from
`main`. This decision publishes a distribution image; it changes no running
deployment. The two builds may have different commits, versions and bytes.

## Limits

Push, signing and attestation are separate operations. A failure can leave an
image tag without all verification evidence. Consumers must require both
checks against the same index digest and the expected workflow/tag identity.
Docker does not enforce those commands. The owner must confirm GHCR package
visibility and repository access before relying on public pulls.

Version tags can move. Every successful stable-tag run (no `-` in the ref name)
updates `latest`, including an older stable version; it does not promise the
greatest stable semantic version. Prereleases publish only their version tag.
GitHub Releases' Latest designation is independent.

Signatures and provenance establish origin under the workflow's trust model,
not safe behaviour, owner approval, complete container SBOM coverage or the
identity of the deployed public hub. Non-root execution does not configure
TLS, account access, volume ownership or backups. Local snapshots cannot
prove registry access or real OIDC signing and attestation publication.
