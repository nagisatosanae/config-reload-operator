# config-reload-operator

Helm chart for the config-reload-operator controller. It installs the
`ConfigReloadPolicy` CRD, the controller Deployment, the permissions the
controller needs, and the admission webhook with its certificate.

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
| `webhook.enabled` | `true` | Installs the admission webhook. It puts the watcher's settings on a Deployment while it is created or updated. |
| `webhook.failurePolicy` | `Ignore` | What the API server does when it cannot reach the webhook. `Ignore` lets the Deployment through, and the controller sets the settings afterwards. `Fail` rejects it. |
| `webhook.timeoutSeconds` | `5` | How long the API server waits for the webhook. |
| `nodeSelector`, `tolerations`, `affinity` | empty | Where the controller Pods may run. |

## Webhook

The webhook only looks at a Deployment with the annotation
`reload.nagisatosanae.github.io/inject: "true"`. Every other Deployment in
the cluster is left alone, also when the webhook is down.

The API server talks to a webhook over TLS. The chart makes the certificate
itself when it is installed, so cert-manager is not needed. An upgrade keeps
the certificate that is already there. It is valid for ten years. To make a
new one, delete the Secret, upgrade, and restart the controller:

```sh
kubectl delete secret config-reload-operator-webhook-cert -n config-reload-operator-system
helm upgrade config-reload-operator ./charts/config-reload-operator \
  --namespace config-reload-operator-system
kubectl rollout restart deployment/config-reload-operator -n config-reload-operator-system
```

A tool that renders the chart with `helm template`, such as Argo CD, cannot
read the Secret from the cluster, so it makes a new certificate on every
render. Set `webhook.enabled=false` there.

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
