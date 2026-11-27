# syntax=docker/dockerfile:1

# Demo image: the official Fluentd image with the watcher next to it.
# Build from the repository root:
#
#   docker build -f demo/fluentd/Dockerfile -t demo-fluentd:dev .

# Override the two images to build from another registry.
ARG GO_IMAGE=golang:1.26
ARG FLUENTD_IMAGE=fluent/fluentd:v1.20-debian-1

# The same build as in cmd/watcher/Dockerfile.
FROM ${GO_IMAGE} AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
COPY go.mod go.sum ./
COPY cmd/watcher/ cmd/watcher/

RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /watcher ./cmd/watcher

FROM ${FLUENTD_IMAGE}

COPY --from=builder /watcher /usr/local/bin/watcher
COPY --chmod=755 cmd/watcher/watcher-entrypoint.sh /usr/local/bin/watcher-entrypoint.sh

# The script starts the watcher in the background, when the operator has set
# a config directory, and then turns into the command below.
ENTRYPOINT ["/usr/local/bin/watcher-entrypoint.sh"]

# The command of the Fluentd image, unchanged: its ENTRYPOINT followed by
# its CMD. tini is PID 1 and hands every signal on to Fluentd, also the
# SIGHUP of the watcher.
CMD ["tini", "--", "/bin/entrypoint.sh", "fluentd"]
