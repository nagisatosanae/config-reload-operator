# syntax=docker/dockerfile:1

# Demo image: the official NGINX image plus the watcher running as PID 1.
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

# STOPSIGNAL (SIGQUIT) is inherited from the NGINX image. The watcher receives
# it as PID 1 and forwards it to the NGINX master process.
ENTRYPOINT ["/usr/local/bin/watcher"]

# Everything after "--" is the child command. Keep the official entrypoint
# script: it runs the /docker-entrypoint.d hooks and then execs nginx, so the
# NGINX master ends up with the PID the watcher started.
CMD ["--", "/docker-entrypoint.sh", "nginx", "-g", "daemon off;"]
