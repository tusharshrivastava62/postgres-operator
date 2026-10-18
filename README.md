# postgres-operator

[![Tests](https://github.com/tusharshrivastava62/postgres-operator/actions/workflows/test.yml/badge.svg)](https://github.com/tusharshrivastava62/postgres-operator/actions/workflows/test.yml)
[![Lint](https://github.com/tusharshrivastava62/postgres-operator/actions/workflows/lint.yml/badge.svg)](https://github.com/tusharshrivastava62/postgres-operator/actions/workflows/lint.yml)
[![E2E Tests](https://github.com/tusharshrivastava62/postgres-operator/actions/workflows/test-e2e.yml/badge.svg)](https://github.com/tusharshrivastava62/postgres-operator/actions/workflows/test-e2e.yml)
[![License: BSL 1.1](https://img.shields.io/badge/License-BSL%201.1-blue.svg)](LICENSE)

A Kubernetes operator, written in Go with [Kubebuilder](https://book.kubebuilder.io/) and
[controller-runtime](https://github.com/kubernetes-sigs/controller-runtime), that turns a
single custom resource into a running, self-healing, backed-up PostgreSQL cluster.

```yaml
apiVersion: db.youroperator.io/v1alpha1
kind: PostgresCluster
metadata:
  name: my-database
spec:
  instances: 3
  version: "16"
  storage:
    size: 10Gi
  resources:
    limits:
      memory: 1Gi
  backup:
    enabled: true
    schedule: "0 2 * * *"
    destination:
      bucket: my-database-backups
      region: eu-west-1
      credentialsSecret: backup-s3
```

Apply that, and the operator's reconcile loop takes it from there.

## What it does

- **Provisions a Patroni-managed PostgreSQL cluster.** Every instance runs the same
  [Spilo](https://github.com/zalando/spilo) container (PostgreSQL plus
  [Patroni](https://github.com/patroni/patroni)), which elects one primary and streams to the
  rest, even when `instances` is 1. The operator creates the `StatefulSet`, four `Service`s
  (`<name>` always routes to the primary, `<name>-readonly` to replicas, plus a headless and a
  config one), a `PodDisruptionBudget`, the `ServiceAccount`/`Role`/`RoleBinding` Patroni needs,
  and a `Secret` with randomly generated superuser and replication passwords - generated once,
  never rewritten, so nothing rotates auth out from under a running cluster.
- **Fails over.** Losing the primary promotes a replica; on a 3-instance cluster that took
  about 2 seconds for a graceful stop. Failover timing, the permitted replica lag, and
  synchronous replication are set under `spec.ha` and applied to the running cluster without
  restarting any pod.
- **Backs up continuously.** With `backup.enabled`, the primary archives its WAL to an S3 or
  S3-compatible bucket as it is produced and takes a base backup on `schedule`, so a
  database can be restored to any moment since the oldest kept base backup. Backups can be
  encrypted with a key you hold, and are written under a path that includes the PostgreSQL
  major version. Credentials and the key come from Secrets you create; the operator checks them
  before changing anything and reports what is wrong instead of starting pods that cannot start.
  A cluster that archives into another's path is refused rather than overwriting it.
  Every 10 minutes a small job reads the bucket back, reports what it finds, and deletes backups
  that are past `retention` (at least `full` of them are kept, and every one newer than `days`).
- **Restores into a new cluster, never in place.** Set `spec.bootstrap.recovery` on a new
  cluster to create it from another cluster's backups, to the latest moment or to a
  `targetTime`. The source is only read. The new cluster gets its own passwords and writes its
  own backups to its own path, and the API refuses a cluster that would archive into the path it
  was restored from. It needs the same PostgreSQL major version as the source.
- **Proves its backups restore.** With `backup.verification` on, a weekly (or any schedule)
  job restores the newest base backup into scratch space, starts PostgreSQL on a local socket
  that nothing else can reach, lets it replay the archived WAL, reads every database, and
  records how long that took in `.status.backup.verification.durationSeconds`: the measured
  time to recover. The `RestoreVerified` condition and a failed test in `Degraded` say whether
  it passed.
- **Reports real status.** `.status.primaryInstance` names the pod holding the leader lock,
  taken from Patroni's own record rather than a pod label that can go stale on a hung primary,
  and `.status.replicationLagBytes` gives each streaming replica's lag. `.status.conditions`
  says what that means: `Available` (a Ready primary exists), `ReplicationHealthy` (every
  replica is streaming within `maximumLagOnFailover`), `FailoverInProgress` (there was a
  primary and now there is not), `Progressing`, and `Degraded` (a backup, WAL archiving or restore test is unhealthy).
  `kubectl get postgrescluster` shows version, instances, primary and availability. With
  backups on, `.status.backup` shows the newest backup, how many exist, the earliest time a
  restore can reach, and how far WAL archiving is behind, and the `BackupHealthy` and
  `WALArchivingHealthy` conditions say whether that is acceptable.
- **Validates at admission time**, not just at reconcile time: a webhook rejects
  `backup.enabled: true` with no `backup.schedule` (a cross-field rule the CRD schema alone
  can't express) and rejects shrinking `spec.storage.size` on update (Kubernetes
  `PersistentVolumeClaim`s can't shrink, so this fails fast at `kubectl apply` instead of
  leaving a `StatefulSet` stuck). It also requires `spec.resources.limits.memory` - PostgreSQL
  sizes its memory from the container limit, and with none it sizes from the whole node - and
  rejects a resource request that exceeds its limit. Non-blocking advice (a memory limit under
  512Mi, `synchronousMode` on a single instance) comes back as a warning on an otherwise
  successful apply.
- **Exposes metrics** on the `/metrics` endpoint controller-runtime already serves, labeled per
  cluster: `postgrescluster_reconcile_total`, `_reconcile_duration_seconds`, `_instances_ready`,
  `_backup_last_success_timestamp_seconds`, `_has_primary`, `_primary_changes_total` and
  `_replication_lag_bytes` (per replica), `_backup_count`, `_backup_last_size_bytes` and
  `_wal_archive_lag_bytes`, `_restore_verification_last_success_timestamp_seconds` and
  `_restore_verification_duration_seconds`. A deleted cluster's series are dropped.
- **Cleans up automatically.** Nearly every resource the operator creates is owner-referenced
  to the `PostgresCluster`, so deleting the custom resource deletes it; the exceptions are the
  data and backup volumes and the credentials `Secret` (see below). The `Endpoints` and
  `ConfigMap`s Patroni writes are removed too, so a new cluster with the same name starts clean.

## What it's *not*: a data-loss shortcut

- Deleting a `PostgresCluster` does **not** delete its data or backup `PersistentVolumeClaim`s,
  or its `<name>-credentials` `Secret`. Kubernetes doesn't auto-delete `StatefulSet`-managed
  PVCs by default, and this operator doesn't override that. The `Secret` stays because the
  passwords are fixed inside the database when it is first created: recreate a cluster of the
  same name on the old volumes and it must find the same passwords. Delete the PVCs and the
  `Secret` together if you actually want the data gone.
- `helm uninstall` does **not** delete the CRD (or, by extension, any `PostgresCluster` it
  manages anywhere in the cluster) - the CRD lives in Helm's `crds/` directory specifically
  because it's cluster-scoped and shared, and an ordinary `helm uninstall` deleting it would be
  indistinguishable from deleting every database the operator manages. See
  [`charts/postgres-operator/README.md`](charts/postgres-operator/README.md).

## Known limitations

- **The database images are built by this project, not upstream.** The default for each
  PostgreSQL version is a build of upstream Spilo published to `ghcr.io/tusharshrivastava62`
  (see [`images/README.md`](images/README.md)); set `spec.image` to use another. The image a
  cluster runs is recorded in `status.image`, and upgrading the operator never changes it.
  The bundled wal-g binary has known critical findings in its Go dependencies, tolerated in
  [`images/spilo/.trivyignore`](images/spilo/.trivyignore) until upstream updates it.
- **Patroni's REST API is unauthenticated** inside the cluster, and pods can reach each
  other's. The operator uses it to apply `spec.ha`. Restrict it with a `NetworkPolicy` that
  allows the operator and the cluster's own pods to reach port 8008.
- **Pod-level changes replace pods one at a time, and the primary goes last.** A new image or
  new resource limits are rolled out by the operator, not by the `StatefulSet`: replicas first,
  each only once replication is healthy again, then a Patroni switchover to an updated replica
  (about 2 seconds of refused writes), then the old primary. A new revision that cannot start
  stops the rollout after the first replica. A single-instance cluster has nobody to hand over
  to, so its pod is simply replaced.
- **A bucket that stops accepting writes lets WAL pile up on the primary's disk** until it is
  fixed. Archiving lag is reported (`WALArchivingHealthy`,
  `postgrescluster_wal_archive_lag_bytes`), so alert on it, and size the data volume for an outage.
- **Backups and restores are S3 only, and a restore always makes a new cluster.** There is no
  in-place restore. Restoring, encrypting, alerting and sizing are covered in
  [`docs/backup-and-restore.md`](docs/backup-and-restore.md).
- **A restore test needs scratch space as large as the database.** Its pod writes the restored
  data to an `emptyDir` capped at `spec.storage.size`, so the node needs that much ephemeral
  storage while it runs.
- **The PostgreSQL major version is fixed at creation.** Supported versions are 16, 17 and 18,
  and `spec.version` can't be changed afterwards - the API server rejects it. Pointing newer
  Postgres binaries at an older data directory doesn't work, and an in-place major-version
  upgrade (`pg_upgrade`) isn't implemented. To move to a newer major version, create a new
  cluster and migrate the data into it with `pg_dumpall` or logical replication. The operator's
  backups are physical, so they restore only to the same major version.

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
NAME                     VERSION   INSTANCES   PRIMARY   AVAILABLE   AGE
postgrescluster-sample   17        3                     True        48s
```

Connect using the generated credentials:

```bash
kubectl get secret postgrescluster-sample-credentials \
  -o jsonpath='{.data.password}' | base64 -d
kubectl run psql --rm -it --restart=Never --image=postgres:17 -- \
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
| `docs/` | Backup and restore runbook, bucket policy, sizing and alerting |
| `images/` | The pinned upstream Spilo release and the workflow inputs for the database images |
| `test/e2e/` | End-to-end tests that deploy the full stack into a real Kind cluster and break real clusters |

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

This project is licensed under the [Business Source License 1.1](LICENSE).
Production use requires a commercial license. Contact tusharshrivastava62@gmail.com.
Converts to Apache 2.0 on 2030-10-17.
