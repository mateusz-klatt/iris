# GoReleaser provides pure-Go binaries in platform-named directories.

# Distroless has no shell, so /data is made here and its owner set on the copy below.
# This stage runs on the build host, so an arm64 image needs no emulation.
FROM --platform=$BUILDPLATFORM busybox:1.38@sha256:a46257e06c82ad85242d0d38077d0a52d9896389ca60a1a59d3463db586e5fa4 AS data
RUN mkdir /data

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
ARG TARGETPLATFORM
COPY ${TARGETPLATFORM}/iris /usr/local/bin/iris
COPY --from=data --chown=65532:65532 /data /data
USER 65532:65532
VOLUME ["/data"]
EXPOSE 7878
ENTRYPOINT ["/usr/local/bin/iris"]
# D1 requires TLS flags for a non-loopback listen address. Supply them explicitly
# when exposing the hub beyond a loopback-only container configuration.
CMD ["serve", "--db", "/data/iris.db", "--token-file", "/data/hub.token"]
