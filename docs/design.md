# Design

Three choices in this project are not the usual ones. This page says why
they were made, and what each one costs.

One rule is behind all of them. Reloading a config is an extra. It has to
work well, but it must never be the reason the app goes down.

## The watcher is in the app's image, not a sidecar

A sidecar would be easier to add: no image to build. It was not chosen for
these reasons.

- **It needs a shared process namespace.** A container can only signal a
  process it can see. A sidecar sees the app only with
  `shareProcessNamespace: true` on the Pod. Then every container in the Pod
  sees the processes of all the others, can read their files through
  `/proc/<pid>/root`, and can signal them. The Pod Security Standards of
  Kubernetes do not forbid this, but some organizations do with rules of
  their own.
- **It has to guess.** A sidecar must find the app by its process name, and
  it must run as the same user or hold a capability to signal it. Inside
  the app's container the watcher simply signals its parent.
- **It does not have the check.** The check before a reload is the app's own
  command, for example `nginx -t`. A sidecar image does not have it. A
  sidecar that runs the app's image would, but then the watcher must be in
  that image, and it has to be built anyway.
- **Every sidecar costs something in every Pod.** A Pod with a service mesh
  proxy, Dapr and a telemetry collector already runs four containers. Each
  one asks for memory and CPU, times the number of replicas. Each one is an
  image to pull and to patch, a start order to get right, and a log to
  read. The proxies also add a hop to every request. A fifth container is a
  lot for a job this small.

What it costs: you build your image once with the watcher in it, and the
image needs a shell for the start script.

## The app is PID 1, and the watcher runs next to it

The start command of the image is a script of five lines:

```sh
if [ -n "${CONFIG_RELOAD_DIR:-}" ]; then
  "${0%/*}/watcher" &
fi

exec "$@"
```

It starts the watcher in the background and then turns into the app. So the
app is the first process of the container, and the watcher is its child.

- **Nothing in the watcher can stop the container.** If the watcher fails,
  hangs or cannot start at all, the app keeps running. A missing watcher
  binary gives one line in the log, and the app starts.
- **The app runs the way it runs without the watcher.** It gets its own
  stop signal and ends with its own exit code. The command your image ran
  before is PID 1 again.
- **With no settings there is no watcher.** When the operator has put
  nothing on the Pod, the script starts no watcher. The container holds the
  app and nothing else.

An earlier version did it the other way round: the watcher was PID 1 and
started the app. Then a watcher that died took the container with it. That
made a dead watcher easy to see, but it let an extra take the app down.

What it costs: nothing restarts the watcher. It is softened in two ways.
The watcher starts watching again by itself when watching stops, also after
a bug in its own code. And `watcher --healthcheck` tells a probe if the
watch process is gone.

Many tools start an app this way, by turning into it: `runc` when it starts
any container, `gosu`, `linkerd-await`, and the entrypoint script of the
NGINX image itself.

## The webhook and the controller are two Deployments

Both are the same binary. One Deployment could run both, and more replicas
with leader election would cover a lost Pod or node. They still run apart,
each started with its own part switched on.

- **Replicas do not help when a part causes the fault itself.** A
  controller that loses its leader lease ends its process. A controller
  holds every Pod's metadata in memory and can run out of it. In one
  process the webhook would go down with it, and the same would happen
  again on the replica that takes over.
- **They matter in different ways.** The webhook is in the path of every
  new Pod that asks for the settings. With the failure policy `Fail`, no
  such Pod starts while it is down. The controller is a loop in the
  background. While it is down, the status is old and nothing else happens.
- **They scale in different ways.** Every webhook replica answers requests.
  Only one controller replica works, the others wait.
- **They need different rights.** The webhook reads the policies and
  nothing else. The controller reads Pods and Deployments and writes the
  status of the policies. Neither has the right to write a Deployment or a
  Pod. The webhook changes a Pod only in the moment it is created.

What it costs: two Deployments to run, and a little more memory.
