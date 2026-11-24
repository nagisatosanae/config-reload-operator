# syntax=docker/dockerfile:1

# The watcher on its own: a static binary and its start script in an empty
# image. It is not meant to run by itself. Copy both files into your app
# image, make the script the start command and give it your app:
#
#   COPY --from=ghcr.io/nagisatosanae/config-reload-operator/watcher:0.2.0 \
#        /watcher /watcher-entrypoint.sh /usr/local/bin/
#   ENTRYPOINT ["/usr/local/bin/watcher-entrypoint.sh"]
#   CMD ["nginx", "-g", "daemon off;"]
#
# The script needs a shell in your image. It starts the watcher in the
# background and then turns into your app, so your app is PID 1.
#
# The Go build happens outside Docker (see build.sh). Build from the
# repository root:
#
#   ./build.sh
#   docker build -f cmd/watcher/Dockerfile -t watcher:dev .

FROM scratch

# Expects a static linux/amd64 binary at bin/watcher in the build context.
COPY bin/watcher /watcher
COPY --chmod=755 cmd/watcher/watcher-entrypoint.sh /watcher-entrypoint.sh

ENTRYPOINT ["/watcher"]
