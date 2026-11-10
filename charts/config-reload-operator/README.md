# config-reload-operator

Helm chart for the config-reload-operator controller. It installs the
`ConfigReloadPolicy` CRD, the controller Deployment and the permissions the
controller needs.

## Install

From the repository root:

```sh
helm install config-reload-operator ./charts/config-reload-operator \
  --namespace config-reload-operator-system --create-namespace
```

Check that the controller is running:

```sh
kubectl get pods -n config-reload-operator-system
```

## Values

| Key | Default | What it does |
|---|---|---|
| `replicaCount` | `1` | Number of controller Pods. Only the leader works, the others wait to take over. |
| `image.repository` | `ghcr.io/nagisatosanae/config-reload-operator/manager` | Controller image. |
| `image.tag` | the chart's `appVersion` | Image tag. |
| `image.pullPolicy` | `IfNotPresent` | When the node pulls the image. |
| `imagePullSecrets` | `[]` | Secrets for a private registry. |
| `resources` | requests 10m / 64Mi, limits 500m / 128Mi | CPU and memory of the controller. |
| `nodeSelector`, `tolerations`, `affinity` | empty | Where the controller Pods may run. |

## Upgrade

```sh
helm upgrade config-reload-operator ./charts/config-reload-operator \
  --namespace config-reload-operator-system
```

Helm installs the CRD on the first install only and leaves it alone after
that. When a new version changes the CRD, apply it yourself before the
upgrade:

```sh
kubectl apply -f charts/config-reload-operator/crds/
```

## Uninstall

Delete your policies first. When a policy is deleted, the controller takes
its `WATCH_*` settings off the Deployment, and it can only do that while it
is running.

```sh
kubectl delete configreloadpolicies --all --all-namespaces
helm uninstall config-reload-operator --namespace config-reload-operator-system
```

If the controller goes first, a deleted policy stays in `Terminating` until
the controller is installed again.

Helm leaves the CRD in place. To remove it as well:

```sh
kubectl delete crd configreloadpolicies.reload.nagisatosanae.github.io
```
