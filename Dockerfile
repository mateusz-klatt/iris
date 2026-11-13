# One runtime for the prebuilt release and source-built deployment targets.

# Distroless has no shell, so /data is made here and its owner set on the copy below.
# This stage runs on the build host, so an arm64 image needs no emulation.
FROM --platform=$BUILDPLATFORM busybox:1.38@sha256:a46257e06c82ad85242d0d38077d0a52d9896389ca60a1a59d3463db586e5fa4 AS data
RUN mkdir /data

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab AS runtime
COPY --from=data --chown=65532:65532 /data /data
# Keep SQLite temporary files writable on both runtime paths.
ENV TMPDIR=/data
USER 65532:65532
VOLUME ["/data"]
EXPOSE 7878
ENTRYPOINT ["/iris"]
# D1 requires TLS flags before listening beyond loopback.
CMD ["serve", "--db", "/data/iris.db", "--token-file", "/data/hub.token"]

# make image selects deploy; unused source-build stages are pruned for releases.
FROM node:26-bookworm-slim@sha256:5df79989ed685e09f0a98019db4cebfb22fb9df1a53c0161664a011f7ff6b0f1 AS console
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci --ignore-scripts
COPY web/ ./
RUN npm run build

FROM golang:1.27.4-bookworm@sha256:4fa7c968ba93f1635670522dbb01a2c78150443e1385746924566079f36570ea AS binary
ARG VERSION=dev
ARG COMMIT
ARG MODIFIED
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=console /src/web/dist/ ./internal/webui/dist/
# Use make image to pass the source checkout's identity; .git is intentionally not copied.
RUN test -n "$COMMIT" && test -n "$MODIFIED" && CGO_ENABLED=0 go build -buildvcs=false -trimpath -ldflags="-s -w -X github.com/mateusz-klatt/iris/internal/buildinfo.Version=$VERSION -X github.com/mateusz-klatt/iris/internal/buildinfo.Commit=$COMMIT -X github.com/mateusz-klatt/iris/internal/buildinfo.Modified=$MODIFIED" -o /out/iris ./cmd/iris

FROM runtime AS deploy
COPY --from=binary /out/iris /iris

# GoReleaser and release.yml supply pure-Go binaries in platform-named directories.
# Keep release last so GoReleaser snapshots use it without extra build flags.
FROM runtime AS release
ARG TARGETPLATFORM
COPY ${TARGETPLATFORM}/iris /iris
