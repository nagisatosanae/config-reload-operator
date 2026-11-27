# Images

The project ships three images on GitHub Container Registry.

| Image | What is inside | Who uses it |
|---|---|---|
| `ghcr.io/nagisatosanae/config-reload-operator/watcher` | the `watcher` binary and its start script, in an empty image (about 3 MB) | your own app image, see below |
| `ghcr.io/nagisatosanae/config-reload-operator/demo-fluentd` | Fluentd with the watcher next to it | the demo and the quick start |
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
git tag v0.5.0
git push origin v0.5.0
```

## Put the watcher in your own image

The watcher is one static binary and a small start script. Copy both in,
make the script the start command, and give it the command your image ran
before:

```dockerfile
FROM fluent/fluentd:v1.20-debian-1
COPY --from=ghcr.io/nagisatosanae/config-reload-operator/watcher:0.5.0 \
     /watcher /watcher-entrypoint.sh /usr/local/bin/
ENTRYPOINT ["/usr/local/bin/watcher-entrypoint.sh"]
CMD ["tini", "--", "/bin/entrypoint.sh", "fluentd"]
```

`demo/fluentd/Dockerfile` makes the same image for the demo. It builds the
watcher itself and does not copy it from the released image.

The script starts the watcher in the background and then turns into the
command in `CMD`. So your app is PID 1, and the watcher is its child. The
script needs a shell in the image. The two files must be in the same
directory.

Setting `ENTRYPOINT` clears the `CMD` of the base image, so write it again.
It is what the base image ran before: its `ENTRYPOINT` followed by its
`CMD`. For Fluentd that is `tini -- /bin/entrypoint.sh` and then `fluentd`.
This command shows both for any image:

```sh
docker inspect --format '{{json .Config.Entrypoint}} {{json .Config.Cmd}}' fluent/fluentd:v1.20-debian-1
```

The Fluentd image starts with `tini`, a small init. Then `tini` is PID 1,
as it is without the watcher, and the watcher is its child. `tini` hands
every signal on to Fluentd, so the reload arrives.

Two things turn the reload off without a message:

- A `command:` in the Deployment replaces `ENTRYPOINT`. The script does not
  run, so no watcher starts. If you need `command:`, start it with the
  script and put your app's command after it.
- A command in `CMD` that runs your app as a child, as `npm start` does,
  gets the signal in place of your app. The command must be your app, end
  with an `exec` of it, or hand signals on. In the Fluentd image `tini`
  hands them on and `/bin/entrypoint.sh` ends with an `exec`.

The script starts no watcher until the operator sets `CONFIG_RELOAD_DIR` on
the container, so the image is safe to run without a policy.

An image made for version 0.1 used the watcher itself as the start command
(`watcher -- COMMAND`). The watcher of this version does not start the
app. Called that way it stops with a message that names the start script.

## Build locally

Every image builds its Go binary inside Docker, in a first stage. So you
need Docker and nothing else. From the repository root:

```sh
docker build -f cmd/watcher/Dockerfile -t watcher:dev .
docker build -f demo/fluentd/Dockerfile -t demo-fluentd:dev .
make docker-build IMG=manager:dev
```

The Go version is that of the `golang` image the Dockerfile names, not the
one on your machine. The two watcher Dockerfiles run the same build.
`make docker-build` builds the manager the way kubebuilder set it up: a
multi-stage build that ends in a distroless image and runs as a non-root
user.

## First push

A package on ghcr.io starts out private. After the first push, open the
package on GitHub (your profile, Packages tab), go to Package settings and
change the visibility to public. Then clusters can pull it without a login.
