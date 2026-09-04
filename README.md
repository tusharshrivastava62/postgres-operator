# postgres-operator

[![Tests](https://github.com/tusharshrivastava62/postgres-operator/actions/workflows/test.yml/badge.svg)](https://github.com/tusharshrivastava62/postgres-operator/actions/workflows/test.yml)
[![Lint](https://github.com/tusharshrivastava62/postgres-operator/actions/workflows/lint.yml/badge.svg)](https://github.com/tusharshrivastava62/postgres-operator/actions/workflows/lint.yml)
[![E2E Tests](https://github.com/tusharshrivastava62/postgres-operator/actions/workflows/test-e2e.yml/badge.svg)](https://github.com/tusharshrivastava62/postgres-operator/actions/workflows/test-e2e.yml)
[![License: Apache 2.0](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](LICENSE)

A Kubernetes operator, written in Go with [Kubebuilder](https://book.kubebuilder.io/) and
[controller-runtime](https://github.com/kubernetes-sigs/controller-runtime), that turns a
single custom resource into a running, self-healing, backed-up PostgreSQL cluster.

```yaml
apiVersion: db.db.youroperator.io/v1alpha1
kind: PostgresCluster
metadata:
  name: my-database
spec:
  instances: 3
  version: "16"
  storage:
    size: 10Gi
  backup:
    enabled: true
    schedule: "0 2 * * *"
```

Apply that, and the operator's reconcile loop takes it from there.

## What it does

- **Provisions** a `StatefulSet`, a headless `Service` (stable per-pod DNS) and a client
  `Service`, and a `Secret` with a randomly generated superuser password — generated once,
  never rewritten, so nothing rotates auth out from under a running cluster.
- **Backs up** on a schedule: a `CronJob` runs `pg_dumpall`, gzip-compresses the output, and
  writes a timestamped dump to its own `PersistentVolumeClaim` when `backup.enabled` is `true`.
  Disabling backups removes the `CronJob` (and stops the schedule) while keeping the PVC and
  every backup already on it.
- **Reports real status.** `.status.conditions` (`Available`, `Progressing`, `Degraded`) is
  driven by actual `StatefulSet` readiness and backup `Job` outcomes, not guessed — and
  `kubectl get postgrescluster` shows version, instance count, and availability directly via
  printer columns.
- **Validates at admission time**, not just at reconcile time: a webhook rejects
  `backup.enabled: true` with no `backup.schedule` (a cross-field rule the CRD schema alone
  can't express) and rejects shrinking `spec.storage.size` on update (Kubernetes
  `PersistentVolumeClaim`s can't shrink, so this fails fast at `kubectl apply` instead of
  leaving a `StatefulSet` stuck).
- **Exposes metrics** — `postgrescluster_reconcile_total`, `_reconcile_duration_seconds`,
  `_instances_ready`, and `_backup_last_success_timestamp_seconds` — on the same `/metrics`
  endpoint controller-runtime already serves, labeled per cluster.
- **Cleans up automatically.** Every resource the operator creates is owner-referenced to the
  `PostgresCluster` that owns it, so deleting the custom resource deletes everything it created
  (data PVCs and the backup PVC are intentionally the exception — see below).

## What it's *not*: a data-loss shortcut

- Deleting a `PostgresCluster` does **not** delete its data or backup `PersistentVolumeClaim`s.
  Kubernetes doesn't auto-delete `StatefulSet`-managed PVCs by default, and this operator
  doesn't override that - clean those up explicitly if you actually want the data gone.
- `helm uninstall` does **not** delete the CRD (or, by extension, any `PostgresCluster` it
  manages anywhere in the cluster) - the CRD lives in Helm's `crds/` directory specifically
  because it's cluster-scoped and shared, and an ordinary `helm uninstall` deleting it would be
  indistinguishable from deleting every database the operator manages. See
  [`charts/postgres-operator/README.md`](charts/postgres-operator/README.md).

## Known limitations

- **`instances > 1` is not high availability.** Each instance is an independent Postgres
  process with its own `PersistentVolumeClaim` - there's no streaming replication, no
  primary/replica designation, and no leader election between them. The client `Service`
  load-balances across all of them, so two connections can land on two databases that have
  already diverged. Today, `instances` scales how many *unrelated* single-node databases share
  a name and a `Service`, not how many members are in one replicated cluster. Don't run
  `instances > 1` against real data. Real HA - streaming replication plus failover, likely via
  [Patroni](https://github.com/patroni/patroni) or hand-rolled leader election - is a natural
  next step, not yet built.

## Install

**Prerequisite:** [cert-manager](https://cert-manager.io/) - the validating webhook's TLS
certificate is issued by it, and there's no toggle to disable the webhook and skip this.

### Helm (recommended)

```bash
helm install postgres-operator charts/postgres-operator \
  --namespace postgres-operator-system --create-namespace \
  --set controllerManager.manager.image.repository=<your-image-repo> \
  --set controllerManager.manager.image.tag=<your-image-tag>
```

Once a release is tagged (see [`RELEASING.md`](RELEASING.md)), the chart is also published as
an OCI artifact and installable without a local checkout:

```bash
helm install postgres-operator oci://ghcr.io/tusharshrivastava62/charts/postgres-operator \
  --version <released-version> --namespace postgres-operator-system --create-namespace
```

### kustomize

```bash
make docker-build docker-push IMG=<your-image-repo>:<tag>
make deploy IMG=<your-image-repo>:<tag>
```

## Quickstart

```bash
kubectl apply -f config/samples/db_v1alpha1_postgrescluster.yaml
kubectl get postgrescluster
```

```
NAME                     VERSION   INSTANCES   AVAILABLE   AGE
postgrescluster-sample   16        3           True        48s
```

Connect using the generated credentials:

```bash
kubectl get secret postgrescluster-sample-credentials \
  -o jsonpath='{.data.password}' | base64 -d
kubectl run psql --rm -it --restart=Never --image=postgres:16 -- \
  psql -h postgrescluster-sample -U postgres
```

## Project layout

| Path | What's there |
|---|---|
| `api/v1alpha1/` | The `PostgresCluster` CRD types |
| `internal/controller/` | The reconciler, status logic, and custom metrics |
| `internal/webhook/v1alpha1/` | The validating admission webhook |
| `config/` | Raw kustomize manifests (CRD, RBAC, manager, webhook, cert-manager) |
| `charts/postgres-operator/` | The Helm chart (see its own README for the `crds/` caveat) |
| `test/e2e/` | End-to-end tests that deploy the full stack into a real Kind cluster |

## Development

```bash
make test          # envtest - a real API server, not a mock client
make lint           # golangci-lint
make manifests      # regenerate CRD/RBAC/webhook manifests after changing api/ or markers
make deploy IMG=... # deploy to whatever cluster your kubeconfig points at
```

See [`RELEASING.md`](RELEASING.md) for how a version tag turns into a published image and
Helm chart.

## License

[Apache License 2.0](LICENSE)
