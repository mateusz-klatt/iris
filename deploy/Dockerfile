# Build from the repository root with source metadata: make image
FROM node:26-bookworm-slim@sha256:add5c17c14752c54d2f3d519ec46a3815684d41c33f198bcb09fb998f7dcbf68 AS console
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
RUN mkdir -p /data && chown 65532:65532 /data

FROM scratch
COPY --from=binary /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=binary /out/iris /iris
COPY --from=binary --chown=65532:65532 /data /data
# Scratch has no /tmp. SQLite needs a temporary directory for large deletes, sorts and
# migrations, and fails with IOERR_GETTEMPPATH (6410) without one.
ENV TMPDIR=/data
USER 65532:65532
VOLUME ["/data"]
ENTRYPOINT ["/iris"]
