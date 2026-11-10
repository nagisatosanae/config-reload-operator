# Images

The project ships three images on GitHub Container Registry.

| Image | What is inside | Who uses it |
|---|---|---|
| `ghcr.io/nagisatosanae/config-reload-operator/watcher` | the `watcher` binary alone, in an empty image (about 5 MB) | your own app image, see below |
| `ghcr.io/nagisatosanae/config-reload-operator/demo-nginx` | NGINX plus the watcher as PID 1 | the demo, the Helm example and the e2e test |
| `ghcr.io/nagisatosanae/config-reload-operator/manager` | the controller | the Helm chart |

## Tags

GitHub Actions builds and pushes all three when a version tag is pushed.
Nothing is pushed for branches or pull requests. The workflow is
`.github/workflows/images.yml`.

A tag `v1.2.3` gives each image three tags: `1.2.3`, `latest` and
`sha-<short commit>`.

Use the version tag for anything that should stay the same. `latest`
moves with every release.

To release:

```sh
git tag v0.1.1
git push origin v0.1.1
```

## Put the watcher in your own image

The watcher is one static binary. Copy it in, make it PID 1, and give it
your app as the child command after `--`:

```dockerfile
FROM nginx:stable-alpine
COPY --from=ghcr.io/nagisatosanae/config-reload-operator/watcher:0.1.1 /watcher /usr/local/bin/watcher
ENTRYPOINT ["/usr/local/bin/watcher"]
CMD ["--", "/docker-entrypoint.sh", "nginx", "-g", "daemon off;"]
```

`demo/nginx/Dockerfile` is this, written out for NGINX. The watcher only
runs the app until the controller sets `WATCH_DIR` on the container, so
the image is safe to run without a policy.

## Build locally

The Go build of the watcher happens outside Docker, in `build.sh`. From
the repository root:

```sh
./build.sh
docker build -f cmd/watcher/Dockerfile -t watcher:dev .
docker build -f demo/nginx/Dockerfile -t demo-nginx:dev .
make docker-build IMG=manager:dev
```

`make docker-build` builds the manager inside Docker the way kubebuilder
set it up: a multi-stage build that ends in a distroless image and runs as
a non-root user.

## First push

A package on ghcr.io starts out private. After the first push, open the
package on GitHub (your profile, Packages tab), go to Package settings and
change the visibility to public. Then clusters can pull it without a login.
