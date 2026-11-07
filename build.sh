#!/usr/bin/env sh
# Builds the watcher as a static linux/amd64 binary into bin/watcher.
# Safe to run from any directory: it moves to the repository root first.
#
#   ./build.sh
#   docker build -f demo/nginx/Dockerfile -t demo-nginx:dev .
set -eu

cd "$(dirname "$0")"
mkdir -p bin

CGO_ENABLED=0 GOOS=linux GOARCH="${GOARCH:-amd64}" \
  go build -trimpath -ldflags="-s -w" -o bin/watcher ./cmd/watcher

echo "built bin/watcher ($(du -h bin/watcher | cut -f1))"
