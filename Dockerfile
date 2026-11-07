# GoReleaser provides pure-Go binaries in platform-named directories.

# Distroless has no shell, so /data is made here and its owner set on the copy below.
# This stage runs on the build host, so an arm64 image needs no emulation.
FROM --platform=$BUILDPLATFORM busybox:1.37 AS data
RUN mkdir /data

FROM gcr.io/distroless/static-debian12:nonroot
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
