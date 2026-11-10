# syntax=docker/dockerfile:1

# The watcher on its own: one static binary in an empty image. It is not
# meant to run by itself. Copy the binary into your app image, then make
# it PID 1 and give it your app as the child command:
#
#   COPY --from=ghcr.io/nagisatosanae/config-reload-operator/watcher:main /watcher /usr/local/bin/watcher
#   ENTRYPOINT ["/usr/local/bin/watcher"]
#   CMD ["--", "nginx", "-g", "daemon off;"]
#
# The Go build happens outside Docker (see build.sh). Build from the
# repository root:
#
#   ./build.sh
#   docker build -f cmd/watcher/Dockerfile -t watcher:dev .

FROM scratch

# Expects a static linux/amd64 binary at bin/watcher in the build context.
COPY bin/watcher /watcher

ENTRYPOINT ["/watcher"]