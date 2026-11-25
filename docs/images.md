# Images

The project ships three images on GitHub Container Registry.

| Image | What is inside | Who uses it |
|---|---|---|
| `ghcr.io/nagisatosanae/config-reload-operator/watcher` | the `watcher` binary and its start script, in an empty image (about 5 MB) | your own app image, see below |
| `ghcr.io/nagisatosanae/config-reload-operator/demo-nginx` | NGINX with the watcher next to it | the demo and the quick start |
| `ghcr.io/nagisatosanae/config-reload-operator/manager` | the controller and the webhook | the Helm chart |

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
git tag v0.2.1
git push origin v0.2.1
```

## Put the watcher in your own image

The watcher is one static binary and a small start script. Copy both in,
make the script the start command, and give it the command your image ran
before:

```dockerfile
FROM nginx:stable-alpine
COPY --from=ghcr.io/nagisatosanae/config-reload-operator/watcher:0.2.1 \
     /watcher /watcher-entrypoint.sh /usr/local/bin/
ENTRYPOINT ["/usr/local/bin/watcher-entrypoint.sh"]
CMD ["/docker-entrypoint.sh", "nginx", "-g", "daemon off;"]
```

`demo/nginx/Dockerfile` is this, written out for NGINX.

The script starts the watcher in the background and then turns into the
command in `CMD`. So your app is PID 1, and the watcher is its child. The
script needs a shell in the image. The two files must be in the same
directory.

Setting `ENTRYPOINT` clears the `CMD` of the base image, so write it again.
It is what the base image ran before: its `ENTRYPOINT` followed by its
`CMD`. For NGINX that is `/docker-entrypoint.sh` and then `nginx -g "daemon
off;"`. This command shows both for any image:

```sh
docker inspect --format '{{json .Config.Entrypoint}} {{json .Config.Cmd}}' nginx:stable-alpine
```

Two things turn the reload off without a message:

- A `command:` in the Deployment replaces `ENTRYPOINT`. The script does not
  run, so no watcher starts. If you need `command:`, start it with the
  script and put your app's command after it.
- A command in `CMD` that runs your app as a child, as `npm start` does,
  gets the signal in place of your app. The command must be your app, or
  end with an `exec` of it. `/docker-entrypoint.sh` of the NGINX image does
  that.

The script starts no watcher until the operator sets `CONFIG_RELOAD_DIR` on
the container, so the image is safe to run without a policy.

An image made for version 0.1 used the watcher itself as the start command
(`watcher -- nginx ...`). The watcher of this version does not start the
app. Called that way it stops with a message that names the start script.

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
