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

This project is that idea made general, with these weak points fixed.

## How it works

There are two parts.

- The **watcher** is a small Go binary inside the app's image. It runs as
  PID 1 and starts the app as its child. It watches the config directory.
  When the content changes, it checks the new config and sends the signal.
- The **controller** reads `ConfigReloadPolicy` objects. A policy names a
  Deployment, a container and a ConfigMap. The controller checks them, finds
  where the ConfigMap is mounted, and puts the watcher's settings on the
  container as `WATCH_*` environment variables.

```text
Setup, once:

  ConfigReloadPolicy --> controller --> WATCH_* env on the Deployment

Every config change after that:

  the ConfigMap changes
    --> the kubelet updates the files in the Pod (up to about a minute)
    --> the watcher sees new content and waits for a quiet moment
    --> the watcher runs the check command, for example "nginx -t"
    --> the watcher sends SIGHUP to the app
    --> the app reads its config again: same Pod, same process
```

The controller is not on the reload path. Reloads keep working while the
controller is down. The watcher never talks to the Kubernetes API, so app
Pods need no extra permissions.

## What is better now

| What it does | Why it helps |
|---|---|
| Sends a signal instead of restarting the Pod. | Connections and data in memory stay. Nothing has to start again. |
| Compares the content of the files, not the file events. | No reload when nothing really changed. |
| Waits for a quiet time, 3 seconds by default. | The six file events of one ConfigMap update become one reload. |
| Can run a check command first, such as `nginx -t`. | A broken config is not loaded. The app keeps running with the old one. |
| The watcher is PID 1 and the app is its child. | Stop signals reach the app. The container exits with the app's exit code. Leftover child processes are cleaned up. |
| Settings come from a policy, not from the image. | One image works for any path and any signal. |
| The policy has a status. | A wrong setup shows up as `Ready=False` with a reason. |
| The controller writes only when something is different. | Running it again changes nothing, so there are no surprise rollouts. |
| Deleting a policy takes its settings off the Deployment. | Your own environment variables are left alone. |

Measured on a real cluster with the NGINX demo: one ConfigMap update made
six file events and one `SIGHUP`. The NGINX master process kept its PID. The
Pod UID and the restart count did not change.

## Quick start

You need a Kubernetes cluster (kind, minikube or Docker Desktop), `kubectl`
and Helm 3 or newer.

**No cluster? Use GitHub Codespaces.** It opens this repository in your
browser with a kind cluster, `kubectl` and Helm ready. You need a GitHub
account, and the first start takes a few minutes. In a codespace, skip the
`git clone` lines below and start at step 1.

[![Open in GitHub Codespaces](https://github.com/codespaces/badge.svg)](https://codespaces.new/nagisatosanae/config-reload-operator)

On your own machine, get the code first.

```sh
git clone https://github.com/nagisatosanae/config-reload-operator.git
cd config-reload-operator
```

**1. Install the controller.**

```sh
helm upgrade --install config-reload-operator ./charts/config-reload-operator \
  --namespace config-reload-operator-system --create-namespace
```

`upgrade --install` installs the chart, or updates it when it is already
there. So this step is safe to run again.

**2. Start the demo app and add a policy.** The demo is NGINX with the
watcher in its image. Its config comes from the ConfigMap `nginx-config`.

```sh
kubectl apply -f demo/nginx/k8s.yaml
kubectl apply -f config/samples/reload_v1alpha1_configreloadpolicy.yaml
kubectl wait --for=condition=Ready configreloadpolicy/nginx-reload -n demo --timeout=60s
kubectl rollout status deployment/nginx-demo -n demo
kubectl get configreloadpolicies -n demo
```

```text
NAME           WORKLOAD     READY   REASON       AGE
nginx-reload   nginx-demo   True    Configured   10s
```

The policy puts the watcher's settings on the Deployment, so the Pod is
replaced one time here. It is not replaced again in the next steps.

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

**5. Wait about a minute and look again.** The kubelet needs that long to
bring the new files into the Pod.

```sh
kubectl logs -n demo deploy/nginx-demo | grep -E "content changed|sending"
kubectl exec -n demo deploy/nginx-demo -- wget -qO- http://127.0.0.1/
kubectl get pods -n demo
```

```text
2026/11/10 07:02:05 6 events, content changed
2026/11/10 07:02:05 sending SIGHUP
v2
```

The answer is `v2`. The Pod name is the same and `RESTARTS` is still 0.

**6. Try a broken config.** This takes the `;` away after `listen 80`.

```sh
sed -e 's/Version v1;/Version v2;/' -e 's/"v1/"v2/' -e 's/listen 80;/listen 80/' demo/nginx/k8s.yaml | kubectl apply -f -
```

After about a minute the watcher log shows that the check failed. No signal
is sent, and NGINX still answers `v2`.

```sh
kubectl logs -n demo deploy/nginx-demo | grep "check failed"
kubectl exec -n demo deploy/nginx-demo -- wget -qO- http://127.0.0.1/
```

```text
2026/11/10 07:48:58 check failed, not reloading: nginx: exit status 1: ... directive "listen" is not terminated by ";" in /etc/nginx/conf.d/default.conf:3
v2
```

**7. Clean up.** Delete the policy first. The controller takes its settings
off the Deployment at that moment, so it must still be running.

```sh
kubectl delete -f config/samples/reload_v1alpha1_configreloadpolicy.yaml
kubectl delete -f demo/nginx/k8s.yaml
helm uninstall config-reload-operator --namespace config-reload-operator-system
```

**Start again.** After the clean-up you can go back to step 1. In a
codespace you can also throw the cluster away and make a new one. That takes
about a minute.

```sh
kind delete cluster && kind create cluster
```

## Use it with your own app

The steps below use NGINX as the example. NGINX does not read its config
again by itself when the file changes. Something has to send it a signal.
These steps make the watcher do that. Any other app that reloads on a signal
works the same way.

**1. Put the watcher in your image.** It is one static binary. Make it the
entrypoint and give it your app as the command after `--`.

```dockerfile
FROM nginx:stable-alpine
COPY --from=ghcr.io/nagisatosanae/config-reload-operator/watcher:0.1.0 /watcher /usr/local/bin/watcher
ENTRYPOINT ["/usr/local/bin/watcher"]
CMD ["--", "/docker-entrypoint.sh", "nginx", "-g", "daemon off;"]
```

Without a policy the watcher only runs the app, so the image is safe to use
anywhere. More about the images is in [docs/images.md](docs/images.md).

**2. Mount the ConfigMap as a directory.** Do not use `subPath`. The kubelet
never updates files that are mounted with `subPath`.

**3. Write a policy** in the same namespace as the Deployment.

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
  reload:
    signal: SIGHUP
    debounceSeconds: 3
    validateCommand: ["nginx", "-t"]
```

| Field | Default | Meaning |
|---|---|---|
| `spec.workloadRef.name` | required | The Deployment to reload. |
| `spec.containerName` | required | The container that runs the watcher and the app. |
| `spec.configMapRef.name` | required | The ConfigMap with the config files. It must be mounted in that container. |
| `spec.reload.signal` | `SIGHUP` | `SIGHUP`, `SIGUSR1` or `SIGUSR2`. |
| `spec.reload.debounceSeconds` | `3` | How long the files must stay quiet before the reload. 0 to 3600. |
| `spec.reload.validateCommand` | none | A command that checks the new config. It runs in the container, without a shell, for at most 10 seconds. If it fails, no signal is sent. |

`kubectl get configreloadpolicies` (short name `crp`) shows if a policy is
working.

| Reason | Ready | Meaning |
|---|---|---|
| `Configured` | True | The watcher's settings are on the Deployment. |
| `WorkloadNotFound` | False | There is no Deployment with that name. |
| `ContainerNotFound` | False | The Deployment has no container with that name. |
| `ConfigMapNotMounted` | False | The container does not mount that ConfigMap. |
| `SubPathNotSupported` | False | The ConfigMap is mounted with `subPath`. |

When a policy is not ready, the controller looks again every 30 seconds and
does not touch the Deployment.

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
| watcher | 18 Go tests | Content hash with the kubelet's symlink layout, quiet time, the check command, settings from the environment, cleaning up leftover child processes. |
| controller | 17 envtest cases and 3 Go tests | Defaults and bad values at the API server, settings written once and not again, your own environment variables kept, every not-ready reason, clean-up when a policy is deleted. |

GitHub Actions runs the tests and the linter on every push.

## Limits

- The app's image must contain the watcher. You have to build an image once.
- Only Deployments, and only ConfigMaps mounted as a directory.
- The kubelet needs up to about a minute to update the files. This is not
  for changes that must apply at once.
- Creating, changing or deleting a policy replaces the Pod one time, because
  the settings are environment variables. Config changes after that do not
  replace it.
- The policy, the Deployment and the ConfigMap must be in the same namespace.
- The app must have a reload signal that its documentation describes.
- The watcher is PID 1. If it crashes, the container stops. That is why it
  is small and well tested.
- Images are built for linux/amd64 only.

You do not need this project for apps that read their config again on their
own, or for apps where a restart is cheap.

## Not done yet

- **Two policies for one container.** Today they overwrite each other. A
  rule where the older policy wins and the other one reports `Conflict` is
  designed and tested, but not merged.
- **Watching Deployments.** If someone removes the settings by hand, the
  controller puts them back only at its next full check, hours later. A
  Deployment watch that reacts at once is designed and tested, but not
  merged.
- **Changing the target of a policy.** The settings stay on the old
  Deployment. They are removed only when the policy is deleted.
- **End-to-end test in CI** on a kind cluster.
- **Metrics and Kubernetes Events** for reloads and failed checks.
- **More kinds of workloads and config**: StatefulSet, DaemonSet, Secrets,
  reload by HTTP call.
- **No image build**: add the watcher to a Pod without building a new image.
- **arm64 images**, and the Helm chart in a registry.
- **Startup wait.** The watcher sends no signal in its first 5 seconds. This
  should be a setting.

## Repository layout

| Path | What is there |
|---|---|
| `api/v1alpha1/` | The `ConfigReloadPolicy` type. |
| `internal/controller/` | The controller and its tests. |
| `cmd/manager/` | The controller binary. |
| `cmd/watcher/` | The watcher binary, its tests and its Dockerfile. |
| `charts/config-reload-operator/` | The Helm chart. |
| `demo/nginx/` | The demo image and its manifests. |
| `config/` | Generated CRD and RBAC, and the sample policy. |
| `docs/` | More documentation. |
