# GoReleaser provides pure-Go binaries in platform-named directories.
FROM gcr.io/distroless/static-debian12
ARG TARGETPLATFORM
COPY ${TARGETPLATFORM}/iris /usr/local/bin/iris
VOLUME ["/data"]
EXPOSE 7878
ENTRYPOINT ["/usr/local/bin/iris"]
# D1 requires TLS flags for a non-loopback listen address. Supply them explicitly
# when exposing the hub beyond a loopback-only container configuration.
CMD ["serve", "--db", "/data/iris.db", "--token-file", "/data/hub.token"]
