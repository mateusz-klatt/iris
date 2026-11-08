# Build from the repository root: docker build -f deploy/Dockerfile .
FROM node:26-bookworm-slim@sha256:add5c17c14752c54d2f3d519ec46a3815684d41c33f198bcb09fb998f7dcbf68 AS console
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci --ignore-scripts
COPY web/ ./
RUN npm run build

FROM golang:1.27.3-bookworm@sha256:dc0963f32053e4bc451d24d87e3892d42d01021b61f093e98eb95c0352bb84b0 AS binary
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=console /src/web/dist/ ./internal/webui/dist/
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/iris ./cmd/iris
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
