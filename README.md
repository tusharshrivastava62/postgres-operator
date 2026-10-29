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
  and replaced only when you ask or schedule it (see below), so nothing rotates auth out from
  under a running cluster. A cluster with
  backups or compliance settings also gets a read-only database role, `pgoperator_monitor`, with a
  `<name>-monitor` Secret: the jobs that read the database connect as it, so none of them holds the
  superuser's password.
- **Manages database roles and passwords.** `spec.roles` creates a login role for each of your
  applications, with a database it owns if you want one, and a generated password in a Secret.
  Passwords are set as hashes, never sent in clear text, and are replaced when you annotate their
  Secret or on a schedule, including the superuser's and the replication role's, which rolls the
  pods the way any rolling update does. A role taken out of the spec has its login turned off and
  is kept, never dropped. See [`docs/access-control.md`](docs/access-control.md).
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
  was restored from. It needs the same PostgreSQL major version as the source. Before any pod is
  created, the operator reads the source's backups and reports in the `RecoverySourceReady`
  condition whether one can be started from, so a `targetTime` earlier than the oldest backup, a
  wrong path or a wrong key is a message in the status instead of a pod that keeps restarting.
- **Proves its backups restore.** With `backup.verification` on, a weekly (or any schedule)
  job restores the newest base backup into scratch space, starts PostgreSQL on a local socket
  that nothing else can reach, lets it replay the archived WAL, reads every database, and
  records how long that took in `.status.backup.verification.durationSeconds`: the measured
  time to recover. The `RestoreVerified` condition and a failed test in `Degraded` say whether
  it passed.
- **Applies a compliance preset and checks it.** `spec.compliance` (`hipaa`, `soc2` or `pci`)
  turns on `pgaudit` audit logging, ships the PostgreSQL log to a bucket from a sidecar,
  puts a `NetworkPolicy` around the database pods, and then, every ten minutes, reads the running
  database and the bucket and reports whether each of ten controls holds, with the evidence, in
  `.status.compliance`. **This configures and verifies. It does not make a cluster compliant with
  anything**: see [What a compliance preset is not](#what-a-compliance-preset-is-not) and
  [`docs/compliance.md`](docs/compliance.md).
- **Hardens a cluster when asked.** `spec.security` turns on a server certificate that clients can
  verify, a password and TLS for Patroni's REST API, a replication role limited to replicating, and
  a network policy that limits what can connect to the database pods and what they can connect to.
  All of it is off by default. See [`docs/security.md`](docs/security.md).
- **Reports real status.** `.status.primaryInstance` names the pod holding the leader lock,
  taken from Patroni's own record rather than a pod label that can go stale on a hung primary,
  and `.status.replicationLagBytes` gives each streaming replica's lag. `.status.conditions`
  says what that means: `Available` (a Ready primary exists), `ReplicationHealthy` (every
  replica is streaming within `maximumLagOnFailover`), `FailoverInProgress` (there was a
  primary and now there is not), `Progressing`, `Degraded` (a backup, WAL archiving or restore test is unhealthy),
  and, with compliance settings, `ControlsMet` (every compliance control passed its last check),
  with roles, `RolesReady`, and with emergency access allowed, `EmergencyAccessActive`.
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
  `_restore_verification_duration_seconds`, `_compliance_controls` (by status: Met, NotMet,
  Unknown), `_roles` (by state: Ready, Pending, Error, Disabled), `_credential_age_seconds` (by
  kind: superuser, replication, monitor, application) and `_emergency_access_active`. A deleted
  cluster's series are dropped.
- **Cleans up automatically.** Nearly every resource the operator creates is owner-referenced
  to the `PostgresCluster`, so deleting the custom resource deletes it; the exceptions are the
  data and backup volumes and the credentials `Secret` (see below). The `Endpoints` and
  `ConfigMap`s Patroni writes are removed too, so a new cluster with the same name starts clean.

## Emergency access

A cluster can allow temporary superuser access for an emergency (`spec.emergencyAccess`). A person
asks with a `PostgresEmergencyAccess`, which records who asked and why, and gets a login that
ends by itself. **While escalated, every statement and the values bound to its parameters are
written to the server log in clear text, whatever the audit settings say**, and pgaudit does not
record an escalated session's reads and writes. Read [docs/emergency-access.md](docs/emergency-access.md)
before using it.

## What a compliance preset is not

Enabling `spec.compliance` does **not** make a cluster compliant with HIPAA, SOC 2, PCI DSS or any
other standard. Compliance belongs to an organisation's whole practice, judged by qualified
people. A preset configures some technical controls and checks that they hold; the preset names
label configuration profiles and are not claims. The mapping from these controls to any
regulation has not been reviewed by anyone qualified, which is why none is cited anywhere. A
`ControlsMet` of `True` means only that every check this operator makes passed the last time it
ran. It says nothing about people and process, about encryption at rest, about whether the audit
log can be altered, about whether the audit is enough for your obligations, or about anything
else the operator does not check. [`docs/compliance.md`](docs/compliance.md) lists what is
checked and, at length, what is not. [`pg-compliance`](docs/compliance-report.md) prints the
status as a report with that same wording in it.

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
  cluster runs is recorded in `status.image`, and upgrading the operator never changes it. Scan
  the images with your own scanner before production use.
- **Patroni's REST API does not require authentication by default**, and answers over plain
  HTTP to any pod that can reach port 8008. Set `spec.security.patroniAPI.authentication` to make
  it refuse changes without a password, and `spec.security.tls.patroniAPI` to serve it over TLS.
  Limit which pods can reach the port with `spec.security.networkPolicy.ingress` or a compliance
  preset. See [`docs/security.md`](docs/security.md#patronis-rest-api).
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
- **Compliance settings isolate the database the moment they are applied.** The `NetworkPolicy`
  admits only pods labelled `db.youroperator.io/client: "true"` in the cluster's namespace (and
  peers you list), so an application without the label loses its connection. Label the pods
  first. Applying or removing the settings replaces the database pods one at a time. The controls
  are checked every ten minutes, not continuously. A custom `spec.image` must contain `pgaudit`
  and the extensions the default image preloads.
- **The server's TLS certificate is generated by the image by default**, so connections are
  encrypted and clients cannot verify the server. Set `spec.security.tls.secretName` to a Secret
  with a certificate you issue, and the operator and its jobs verify it. See
  [`docs/security.md`](docs/security.md#server-certificate).
- **The replication role can log in and run queries by default**, and `pg_rewind` runs as the
  superuser. Set `spec.security.restrictReplicationRole` to limit the role to replicating and give
  `pg_rewind` a login of its own. See [`docs/security.md`](docs/security.md#replication-role).
- **Network policies are off by default** unless a compliance preset sets an ingress policy.
  Set `spec.security.networkPolicy.ingress` and `.egress` to limit what can connect to the
  database pods and what they can connect to. A network policy cannot name hosts, so the object
  store that backups, restores and audit logs go to is listed in `egress.allow`, by address or by
  the pods that serve it. The operator does not verify that your network plugin enforces egress.
  The `network-isolation` probe checks ingress only.
- **Replacing an application's password refuses the old one at once.** There is one password per
  role, so an application that read it when it started is refused until it reads the Secret again,
  and open connections carry on. Replacing the superuser and replication passwords is a rolling
  update of every pod. A role taken out of `spec.roles` is disabled, never dropped.
  [`docs/access-control.md`](docs/access-control.md) says what each replacement costs.
- **Emergency access is audited, not contained.** A person with it is a superuser. Their
  statements, and the values bound to them, are in the server log in clear text, and pgaudit does not
  record an escalated session's reads and writes. [`docs/emergency-access.md`](docs/emergency-access.md)
  says what is and is not recorded.
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

**Prerequisite:** [cert-manager](https://cert-manager.io/) - the webhooks' TLS certificate is
issued by it, and there's no toggle to disable the webhooks and skip this.

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
| `api/v1alpha1/` | The `PostgresCluster` and `PostgresEmergencyAccess` CRD types |
| `cmd/pg-compliance/`, `internal/compliancereport/` | The `pg-compliance` command, which prints a cluster's compliance status and emergency access requests as a report |
| `internal/controller/` | The reconciler, status logic, and custom metrics |
| `internal/webhook/v1alpha1/` | The admission webhooks: validating for both kinds, and one that records who made an emergency access request |
| `config/` | Raw kustomize manifests (CRD, RBAC, manager, webhook, cert-manager) |
| `charts/postgres-operator/` | The Helm chart (see its own README for the `crds/` caveat) |
| `docs/` | Backup and restore runbook; the security options; the compliance guide, with what a preset does and does not do; the access control guide; emergency access, with its warning; the compliance report |
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
