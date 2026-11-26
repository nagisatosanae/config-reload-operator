# config-reload-operator

Helm chart for config-reload-operator. It installs the `ConfigReloadPolicy`
CRD, the webhook and the controller as two Deployments, the permissions
each of them needs, and the certificate of the webhook.

## Install

The cluster must be Kubernetes 1.30 or newer. The webhook uses
`matchConditions`.

From the repository root:

```sh
helm install config-reload-operator ./charts/config-reload-operator \
  --namespace config-reload-operator-system --create-namespace
```

Check that the Pods are running. There is one controller Pod and there are
two webhook Pods:

```sh
kubectl get pods -n config-reload-operator-system
```

Until a webhook Pod is ready, the webhook does not answer. With the default
`webhook.failurePolicy: Fail`, a Deployment with the annotation is turned
down in that time when you apply it. Add `--wait` to the install and Helm
comes back only when the Pods are ready.

## Values

| Key | Default | What it does |
|---|---|---|
| `replicaCount` | `1` | Number of controller Pods. Only the leader works, the others wait to take over. |
| `image.repository` | `ghcr.io/nagisatosanae/config-reload-operator/manager` | Image of the controller and the webhook. |
| `image.tag` | the chart's `appVersion` | Image tag. |
| `image.pullPolicy` | `IfNotPresent` | When the node pulls the image. |
| `imagePullSecrets` | `[]` | Secrets for a private registry. |
| `resources` | requests 10m / 64Mi, limits 500m / 128Mi | CPU and memory of each Pod. |
| `webhook.enabled` | `true` | Installs the admission webhook. It puts the watcher's settings on a Pod while it is created. Without it no Pod gets them. |
| `webhook.replicas` | `2` | Number of webhook Pods. The webhook runs as its own Deployment, and every Pod of it answers requests. |
| `webhook.failurePolicy` | `Fail` | What happens to a Pod that has the annotation but cannot get the settings: the webhook cannot be reached, no policy points at its Deployment, or the policy cannot be applied. `Fail` rejects the Pod and says why. `Ignore` lets it start as you wrote it. |
| `webhook.timeoutSeconds` | `5` | How long the API server waits for the webhook. |
| `nodeSelector`, `tolerations`, `affinity` | empty | Where the Pods may run. |

## Webhook

The webhook only looks at a Pod with the annotation
`reload.nagisatosanae.github.io/inject: "true"`. Put it on the Pod template
of your Deployment. Every other Pod in the cluster is left alone, also when
the webhook is down.

A Deployment with that annotation on its Pod template is checked when you
apply it. It goes through what its Pods will go through, and it is not
changed. With `Fail` a Deployment whose Pods could not get their settings
is rejected at once. With `Ignore` it is let in with a warning.

So with `Fail` the policy must be in the cluster before the Deployment. In a
Helm chart that holds both, make the policy a `pre-install,pre-upgrade`
hook. Helm creates a custom resource after the Deployments, and the
Deployment would be rejected.

A new policy is checked too. A Deployment can have only one policy, so a
second policy for the same Deployment is rejected.

With `Fail`, no Pod with the annotation and no policy can be created while
no webhook Pod is up. That is why there are two, with a PodDisruptionBudget
that keeps one of them. Pods that already run are not affected.

The API server talks to a webhook over TLS. The chart makes the certificate
itself when it is installed, so cert-manager is not needed. An upgrade keeps
the certificate that is already there. It is valid for ten years. To make a
new one, delete the Secret, upgrade, and restart the webhook:

```sh
kubectl delete secret config-reload-operator-webhook-cert -n config-reload-operator-system
helm upgrade config-reload-operator ./charts/config-reload-operator \
  --namespace config-reload-operator-system
kubectl rollout restart deployment/config-reload-operator-webhook -n config-reload-operator-system
```

A tool that renders the chart with `helm template`, such as Argo CD, cannot
read the Secret from the cluster, so it makes a new certificate on every
render. The chart is not made for that yet.

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

Coming from version 0.1: write your policies again in the form of this
version, with `spec.containers`, and put the annotation on the Pod
template. Images must be built again with the start script, see
`docs/images.md` in the repository. Policies made by 0.1 carry a finalizer.
The controller of this version takes it off. The `WATCH_*` env that 0.1 put
on your Deployments stays there. Nothing reads it any more, so take it off
yourself.

Coming from version 0.2.0 or 0.2.1: write your policies again. A policy
now lists its containers under `spec.containers`, and a Deployment has one
policy. Save your policies first, with
`kubectl get configreloadpolicies -A -o yaml`. Once the new CRD is applied,
the fields that moved are no longer shown. Then apply the CRD, upgrade, and
apply each policy in the new form under the name it has. If a Deployment
had one policy for each container, put the containers into one of them and
delete the others. From the moment the CRD is applied until the chart is
upgraded and the policy is written again, the policy reports
`ContainerNotFound`, and a new Pod of its Deployment is rejected, or starts
without the settings with `Ignore`. Pods that run keep their settings. They count as `Outdated` until they are
replaced, because the webhook now writes a hash of what the policy says
about each container on the Pod, and the controller compares it. From now
on a Pod counts as `Outdated` when it carries settings that differ from
the policy as it is now. A policy that is deleted and made again with the
same content finds its Pods `Injected`.

## Uninstall

```sh
helm uninstall config-reload-operator --namespace config-reload-operator-system
```

Pods that run keep their settings. With the webhook gone, new Pods start as
you wrote them, without the settings. Your policies stay in the cluster.

Helm leaves the CRD in place. Removing it also deletes every policy:

```sh
kubectl delete crd configreloadpolicies.reload.nagisatosanae.github.io
```
