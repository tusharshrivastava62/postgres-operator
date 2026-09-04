# postgres-operator

A Helm chart for the postgres-operator Kubernetes operator: installs the
`PostgresCluster` CRD, RBAC, the manager Deployment, and (since the
validating webhook is always on in this project) the cert-manager
`Issuer`/`Certificate` and `ValidatingWebhookConfiguration` it needs.

## Prerequisites

- [cert-manager](https://cert-manager.io/) installed in the cluster (the
  webhook's TLS certificate is issued by it; there's no toggle to disable
  the webhook and skip this).
- An operator image built and reachable from the cluster (see the
  project's `Makefile` - `make docker-build`, then push or `kind load
  docker-image` for a local Kind cluster).

## Install

```bash
helm install postgres-operator charts/postgres-operator \
  --namespace postgres-operator-system --create-namespace \
  --set controllerManager.manager.image.repository=<your-image-repo> \
  --set controllerManager.manager.image.tag=<your-image-tag>
```

## The CRD lives in `crds/`, not `templates/` - on purpose

Helm treats `crds/` specially: those manifests are applied once on
`helm install` and are **never touched by `helm upgrade` and never removed
by `helm uninstall`**. That's deliberate here, not a limitation worked
around - `PostgresCluster` is cluster-scoped and shared by every release
of this chart in the cluster, and every `PostgresCluster` object plus
everything it owns (StatefulSets, PVCs, Secrets, backups) is
garbage-collected the moment the CRD is deleted. A CRD template that gets
pruned by an ordinary `helm uninstall` would make deleting the *operator*
indistinguishable from deleting every *database* it manages. `crds/` is
Helm's built-in guard against exactly that.

The tradeoff is the one Helm always has for this directory: upgrading the
CRD's schema (e.g. a new `spec` field) isn't automatic. Apply it manually
after `helm upgrade`:

```bash
kubectl apply -f charts/postgres-operator/crds/postgrescluster-crd.yaml
```

If you regenerate this chart from `config/` via `kustomize build
config/default | helmify charts/postgres-operator`, helmify will put the
CRD back under `templates/` with `{{ include "postgres-operator.labels" .
}}` templating - move it back to `crds/` and swap that templating for the
static labels already in the file before shipping.

## Values

See `values.yaml` for the full set. The ones you'll actually change:

| Value | Purpose |
|---|---|
| `controllerManager.manager.image.repository` / `.tag` | The operator image to run. |
| `controllerManager.manager.resources` | CPU/memory requests and limits for the manager container. |
| `controllerManager.replicas` | Manager replica count (leader election is on, so only one is ever active). |
