# syntax=docker/dockerfile:1

# The watcher on its own: a static binary and its start script in an empty
# image. It is not meant to run by itself. Copy both files into your app
# image, make the script the start command and give it your app:
#
#   COPY --from=ghcr.io/nagisatosanae/config-reload-operator/watcher:0.5.0 \
#        /watcher /watcher-entrypoint.sh /usr/local/bin/
#   ENTRYPOINT ["/usr/local/bin/watcher-entrypoint.sh"]
#   CMD ["tini", "--", "/bin/entrypoint.sh", "fluentd"]
#
# CMD is what the base image ran before: its ENTRYPOINT followed by its CMD.
# The script needs a shell in your image. It starts the watcher in the
# background and then turns into your app, so your app is PID 1.
#
# Build from the repository root:
#
#   docker build -f cmd/watcher/Dockerfile -t watcher:dev .

# Override GO_IMAGE to build from another registry.
ARG GO_IMAGE=golang:1.26
FROM ${GO_IMAGE} AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
# Only what the watcher needs. Go then downloads only the modules the
# watcher uses, not those of the manager.
COPY go.mod go.sum ./
COPY cmd/watcher/ cmd/watcher/

# GOARCH has no default here, as in the Dockerfile of the manager: the
# binary is built for the platform the image is built for.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /watcher ./cmd/watcher

FROM scratch

COPY --from=builder /watcher /watcher
COPY --chmod=755 cmd/watcher/watcher-entrypoint.sh /watcher-entrypoint.sh

ENTRYPOINT ["/watcher"]
