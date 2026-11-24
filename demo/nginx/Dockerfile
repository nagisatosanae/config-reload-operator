# syntax=docker/dockerfile:1

# Demo image: the official NGINX image with the watcher next to it.
#
# The Go build happens outside Docker (see build.sh). This file only copies
# the prebuilt Linux binary, so build from the repository root:
#
#   ./build.sh
#   docker build -f demo/nginx/Dockerfile -t demo-nginx:dev .

ARG NGINX_IMAGE=nginx:stable-alpine
FROM ${NGINX_IMAGE}

# Expects a static linux/amd64 binary at bin/watcher in the build context.
COPY bin/watcher /usr/local/bin/watcher
COPY --chmod=755 cmd/watcher/watcher-entrypoint.sh /usr/local/bin/watcher-entrypoint.sh

# The script starts the watcher in the background, when the operator has set
# a config directory, and then turns into the command below. So NGINX is
# PID 1 and gets STOPSIGNAL (SIGQUIT, from the NGINX image) itself.
ENTRYPOINT ["/usr/local/bin/watcher-entrypoint.sh"]

# The command of the NGINX image, unchanged. Its entrypoint script runs the
# /docker-entrypoint.d hooks and then execs nginx.
CMD ["/docker-entrypoint.sh", "nginx", "-g", "daemon off;"]
