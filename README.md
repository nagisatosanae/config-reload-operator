# config-reload-operator

[![Tests](https://github.com/nagisatosanae/config-reload-operator/actions/workflows/test.yml/badge.svg)](https://github.com/nagisatosanae/config-reload-operator/actions/workflows/test.yml)
[![Lint](https://github.com/nagisatosanae/config-reload-operator/actions/workflows/lint.yml/badge.svg)](https://github.com/nagisatosanae/config-reload-operator/actions/workflows/lint.yml)
[![Images](https://github.com/nagisatosanae/config-reload-operator/actions/workflows/images.yml/badge.svg)](https://github.com/nagisatosanae/config-reload-operator/actions/workflows/images.yml)
[![Go version](https://img.shields.io/github/go-mod/go-version/nagisatosanae/config-reload-operator)](go.mod)
[![Latest tag](https://img.shields.io/github/v/tag/nagisatosanae/config-reload-operator?include_prereleases&sort=semver)](https://github.com/nagisatosanae/config-reload-operator/tags)

Reload an app's config with a signal when its ConfigMap changes. The Pod is
not replaced.

## Background

Many apps do not notice when their config files change. But they read the
config again when they get a signal, usually `SIGHUP`. NGINX, Apache httpd,
PostgreSQL, pgbouncer and Fluentd all work this way.

On Kubernetes, the usual way to apply a changed ConfigMap is to restart the
Deployment, so every Pod is replaced. Tools like
[Reloader](https://github.com/stakater/Reloader) do that for you. This is a
good fit for many apps. It costs more for apps that hold long connections,
keep data in memory or start slowly.

The other way is to keep the Pod and send the signal yourself. I did this
once for Fluentd running as a sidecar: a small program watched the config
files and sent the signal. It worked, but it had weak points.

| Weak point | What went wrong |
|---|---|
| A shell script started the app and the reloader as two processes. | When the reloader died, nobody noticed. The container looked healthy and reloads just stopped. Stop signals and exit codes did not always reach the right process. |
| Every file event caused a reload. | One ConfigMap update makes several file events, so one change could reload the app more than once. Saving the same content again also caused a reload. |
| There was no check before the reload. | A broken config went to the app as it was. |
| The path and the signal were fixed in the image. | Every app needed its own build. |
| Nothing reported a wrong setup. | It failed without a message. |

This project is that idea made general, with these weak points fixed or
made smaller. What is left is under [Limits](#limits).

## How it works

There are three parts.

- The **watcher** is a small Go binary inside the app's image. A start
  script starts it next to the app and then turns into the app. So the app
  is PID 1 and the watcher is its child. The watcher watches the config
  directory. When the content changes, it checks the new config and sends
  the signal to the app.
- The **webhook** reads `ConfigReloadPolicy` objects. A policy names a
  Deployment, a container, a ConfigMap and the path to mount it at. When a
  Pod of that Deployment is created, the webhook adds the ConfigMap volume,
  its mount and the watcher's settings as `CONFIG_RELOAD_*` environment
  variables. It only does this for a Pod that asks for it with an
  annotation. The Deployment itself is never changed.
- The **controller** writes into the status of the policy how many Pods
  carry the settings. It writes nothing else.

```text
When a Pod starts:

  ConfigReloadPolicy, and the annotation on the Pod template
    --> the webhook adds the volume, the mount and CONFIG_RELOAD_* env
    --> the start script starts the watcher and turns into the app

Every config change after that:

  the ConfigMap changes
    --> the kubelet updates the files in the Pod (up to about a minute)
    --> the watcher sees new content and waits for a quiet moment
    --> the watcher runs the check command, for example "nginx -t"
    --> the watcher sends SIGHUP to the app
    --> the app reads its config again: same Pod, same process
```

The webhook and the controller are not on the reload path. Reloads keep
working while both are down. The watcher never talks to the Kubernetes API,
so app Pods need no extra permissions.

The webhook is only needed when a Pod is created. A Pod that runs is never
changed, also not when its policy is deleted. Why it is built this way is in
[docs/design.md](docs/design.md).

## What is better now

| What it does | Why it helps |
|---|---|
| Sends a signal instead of restarting the Pod. | Connections and data in memory stay. Nothing has to start again. |
| Compares the content of the files, not the file events. | No reload when nothing really changed. |
| Waits for a quiet time, 3 seconds by default. | The six file events of one ConfigMap update become one reload. |
| Can run a check command first, such as `nginx -t`. | A broken config is not loaded. The app keeps running with the old one. |
| The app is PID 1 and the watcher runs next to it. | Stop signals reach the app directly, and the container exits with the app's exit code. A watcher that fails cannot stop the app. |
| Settings come from a policy, not from the image. | One image works for any path and any signal. |
| The policy has a status. | A wrong setup shows up as `Ready=False` with a reason, and you see how many Pods carry the settings. |
| Only new Pods get the settings. | Your Deployment is never changed and a Pod that runs is never touched, so there are no surprise rollouts. |
| Deleting a policy changes nothing that runs. | Pods keep their settings until they are replaced. |

Measured on a real cluster with the NGINX demo: one ConfigMap update made
six file events and one `SIGHUP`. The NGINX master process kept its PID. The
Pod UID and the restart count did not change.

## Quick start

You need a Kubernetes cluster (kind, minikube or Docker Desktop), `kubectl`
and Helm 3 or newer.

![Demo: NGINX goes from v1 to v2 in the same Pod](docs/demo.gif)

The GIF shows steps 3 to 5 below. It is drawn from the output of a real run,
and the wait for the kubelet is cut out.

```sh
git clone https://github.com/nagisatosanae/config-reload-operator.git
cd config-reload-operator
```

**1. Install the operator.** This is the webhook and the controller.

```sh
helm upgrade --install config-reload-operator ./charts/config-reload-operator \
  --namespace config-reload-operator-system --create-namespace
```

`upgrade --install` installs the chart, or updates it when it is already
there. So this step is safe to run again.

**2. Add a policy and start the demo app.** The demo is NGINX with the
watcher in its image. Its config comes from the ConfigMap `nginx-config`.
The policy goes in first. A Deployment whose Pods could not get their
settings is turned down when you apply it.

```sh
kubectl create namespace demo --save-config
kubectl apply -f config/samples/reload_v1alpha1_configreloadpolicy.yaml
kubectl apply -f demo/nginx/k8s.yaml
kubectl rollout status deployment/nginx-demo -n demo
kubectl wait --for=condition=Ready configreloadpolicy/nginx-reload -n demo --timeout=60s
kubectl get configreloadpolicies -n demo
```

```text
NAME           WORKLOAD     READY   REASON     PODS   INJECTED   AGE
nginx-reload   nginx-demo   True    Injected   1      1          30s
```

The Pod template of the demo Deployment has the annotation
`reload.nagisatosanae.github.io/inject: "true"`. So the webhook gives the
new Pod the ConfigMap volume, its mount and the watcher's settings. The
Deployment has none of them, and
`kubectl get deployment nginx-demo -n demo -o yaml` shows it as you applied
it. The Pod starts one time and is not replaced in the next steps.

**3. Look at the Pod and its answer.** Remember the Pod name.

```sh
kubectl get pods -n demo
kubectl exec -n demo deploy/nginx-demo -- wget -qO- http://127.0.0.1/
```

```text
v1
```

**4. Change the config from v1 to v2.**

```sh
sed -e 's/Version v1;/Version v2;/' -e 's/"v1/"v2/' demo/nginx/k8s.yaml | kubectl apply -f -
```

**5. Follow the watcher log.** The kubelet needs up to about a minute to
bring the new files into the Pod. This command stays open and shows the
lines when it happens.

```sh
kubectl logs -f -n demo deploy/nginx-demo | grep -E "content changed|sending"
```

```text
2026/11/10 07:02:05 6 events, content changed
2026/11/10 07:02:05 sending SIGHUP
```

When you see these two lines, press `Ctrl+C`. Then look at the answer and
the Pod.

```sh
kubectl exec -n demo deploy/nginx-demo -- wget -qO- http://127.0.0.1/
kubectl get pods -n demo
```

```text
v2
```

The answer is `v2`. The Pod name is the same and `RESTARTS` is still 0.

**6. Try a broken config.** This takes the `;` away after `listen 80`.

```sh
sed -e 's/Version v1;/Version v2;/' -e 's/"v1/"v2/' -e 's/listen 80;/listen 80/' demo/nginx/k8s.yaml | kubectl apply -f -
```

Follow the log again. After up to about a minute it shows that the check
failed. Press `Ctrl+C` when you see the line.

```sh
kubectl logs -f -n demo deploy/nginx-demo | grep "check failed"
```

```text
2026/11/10 07:48:58 check failed, not reloading: nginx: exit status 1: ... directive "listen" is not terminated by ";" in /etc/nginx/conf.d/default.conf:3
```

No signal was sent, and NGINX still answers `v2`.

```sh
kubectl exec -n demo deploy/nginx-demo -- wget -qO- http://127.0.0.1/
```

```text
v2
```

**7. Clean up.**

```sh
kubectl delete -f config/samples/reload_v1alpha1_configreloadpolicy.yaml
kubectl delete -f demo/nginx/k8s.yaml
helm uninstall config-reload-operator --namespace config-reload-operator-system
```

**Start again.** After the clean-up you can go back to step 1. On a kind
cluster you can also throw the cluster away and make a new one. That takes
about a minute.

```sh
kind delete cluster && kind create cluster
```

## Use it with your own app

The steps below use NGINX as the example. NGINX does not read its config
again by itself when the file changes. Something has to send it a signal.
These steps make the watcher do that. Any other app that reloads on a signal
works the same way.

**1. Put the watcher in your image.** It is one static binary and a small
start script. Copy both, make the script the entrypoint, and give it the
command your image ran before.

```dockerfile
FROM nginx:stable-alpine
COPY --from=ghcr.io/nagisatosanae/config-reload-operator/watcher:0.2.0 \
     /watcher /watcher-entrypoint.sh /usr/local/bin/
ENTRYPOINT ["/usr/local/bin/watcher-entrypoint.sh"]
CMD ["/docker-entrypoint.sh", "nginx", "-g", "daemon off;"]
```

`CMD` is what the base image ran before: its `ENTRYPOINT` followed by its
`CMD`. This command shows both:

```sh
docker inspect --format '{{json .Config.Entrypoint}} {{json .Config.Cmd}}' nginx:stable-alpine
```

The script needs a shell in the image. Without a policy it only starts the
app, so the image is safe to use anywhere. More about the images is in
[docs/images.md](docs/images.md).

**2. Write a policy** in the same namespace as the Deployment.

```yaml
apiVersion: reload.nagisatosanae.github.io/v1alpha1
kind: ConfigReloadPolicy
metadata:
  name: nginx-reload
  namespace: demo
spec:
  workloadRef:
    name: nginx-demo
  containerName: nginx
  configMapRef:
    name: nginx-config
  mountPath: /etc/nginx/conf.d
  reload:
    signal: SIGHUP
    debounceSeconds: 3
    validateCommand: ["nginx", "-t"]
```

| Field | Default | Meaning |
|---|---|---|
| `spec.workloadRef.name` | required | The Deployment to reload. It cannot be changed later. |
| `spec.containerName` | required | The container that runs the watcher and the app. It cannot be changed later. |
| `spec.configMapRef.name` | required | The ConfigMap with the config files. The webhook mounts it in that container. |
| `spec.mountPath` | required | The directory where the ConfigMap is mounted. The watcher watches it. An absolute path with no slash at the end. |
| `spec.reload.signal` | `SIGHUP` | `SIGHUP`, `SIGUSR1` or `SIGUSR2`. |
| `spec.reload.debounceSeconds` | `3` | How long the files must stay quiet before the reload. 0 to 3600. |
| `spec.reload.validateCommand` | none | A command that checks the new config. It runs in the container, without a shell, for at most 10 seconds. If it fails, no signal is sent. |

`kubectl get configreloadpolicies` (short name `crp`) shows if a policy is
working.

| Reason | Ready | Meaning |
|---|---|---|
| `Injected` | True | Every Pod carries the settings. |
| `Accepted` | True | There are no Pods yet. New ones get the settings. |
| `NotInjected` | False | A Pod runs without the settings. |
| `Outdated` | False | A Pod carries the settings from before the policy was changed. |
| `WorkloadNotFound` | False | There is no Deployment with that name. |
| `ContainerNotFound` | False | The Deployment has no container with that name. |
| `NotOptedIn` | False | The Pod template of the Deployment does not have the annotation. |

The controller only reports. A Pod that runs gets the current settings when
it is replaced, for example with `kubectl rollout restart`.

**3. Add the annotation to the Pod template of your Deployment.**

```yaml
spec:
  template:
    metadata:
      annotations:
        reload.nagisatosanae.github.io/inject: "true"
```

You do not mount the ConfigMap yourself. The webhook mounts it in every new
Pod, at the path the policy names. If the container already mounts the same
ConfigMap at that path, that mount is used. Do not use `subPath`: the
kubelet never updates files that are mounted with it.

The policy must be there before the Deployment. A Deployment whose Pods
cannot get their settings is rejected when you apply it, and the message
says why. That is `webhook.failurePolicy: Fail`, the default. With `Ignore`
it is let in with a warning, and its Pods start as you wrote them, without
the ConfigMap. Keep `Fail` for an app that cannot run without its
ConfigMap.

The Helm chart has its own page:
[charts/config-reload-operator](charts/config-reload-operator/README.md).

## Run the tests

You need Go 1.26 and `make`, on Linux or WSL. The watcher uses Linux system
calls, so its tests do not run on Windows or macOS.

```sh
go test -short ./...   # fast: plain Go tests only, a few seconds
make test              # everything
make lint              # golangci-lint
```

`make test` also runs the controller tests. They start a real API server and
etcd on your machine with envtest. No cluster is needed, and `make`
downloads the two binaries the first time.

| Part | Tests | What they cover |
|---|---|---|
| watcher | 25 Go tests | Content hash with the kubelet's symlink layout, quiet time, the check command, settings from the environment, the start script, a watch process that fails without hurting the app. |
| controller | 63 envtest cases and 6 Go tests | Defaults and bad values at the API server, what the webhook puts on a new Pod, every reason a Pod is rejected and the same Pods under Ignore, the same check when a Deployment is applied, the Pod counts in the status, every not-ready reason, nothing written to a Deployment or to a Pod that runs. |

GitHub Actions runs the tests and the linter on every push.

**What the tests do not cover.** No automated test runs the whole chain on a
real cluster: the kubelet, NGINX, the images and the Helm chart together.
The parts are tested one by one. The watcher tests copy what the kubelet
does when it swaps the files, in a temporary directory. The controller tests
use a real API server. Joining the parts is checked by hand with the quick
start above. An end-to-end test on a kind cluster is the next thing to add.

## Limits

- The app's image must contain the watcher and a shell. You have to build
  an image once.
- Only Deployments, and one ConfigMap directory for each container.
- The kubelet needs up to about a minute to update the files. This is not
  for changes that must apply at once.
- A changed policy reaches a Pod only when the Pod is replaced, because the
  settings are environment variables. The operator does not replace Pods.
  Config changes do not need that.
- Nothing restarts the watcher. It starts watching again by itself after a
  failure, but when its process is killed, reloads stop until the Pod is
  replaced. `watcher --healthcheck` lets a probe notice.
- With `failurePolicy: Fail`, no Pod with the annotation can start while no
  webhook Pod is up. The chart runs two of them.

You do not need this project for apps that read their config again on their
own, or for apps where a restart is cheap.

## Not done yet

- **Metrics and Kubernetes Events** for reloads and failed checks.
- **More kinds of workloads and config**: StatefulSet, DaemonSet, Secrets,
  reload by HTTP call.

## Repository layout

| Path | What is there |
|---|---|
| `api/v1alpha1/` | The `ConfigReloadPolicy` type. |
| `internal/controller/` | The controller, the webhook and their tests. |
| `cmd/manager/` | The binary of the controller and the webhook. |
| `cmd/watcher/` | The watcher binary, its start script, its tests and its Dockerfile. |
| `charts/config-reload-operator/` | The Helm chart. |
| `demo/nginx/` | The demo image and its manifests. |
| `config/` | Generated CRD and RBAC, and the sample policy. |
| `docs/` | More documentation. |
