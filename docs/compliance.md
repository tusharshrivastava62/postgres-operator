# Compliance presets

A compliance preset turns on audit logging, ships the audit log to a bucket, isolates the database
pods on the network, and then checks every ten minutes whether each of those things is really in
place. It reports the result in the cluster's status.

## What a preset does not do

Read this section before the rest.

**Enabling a preset does not make a cluster compliant with HIPAA, SOC 2, PCI DSS or anything
else.** Compliance is a property of an organisation's whole practice, judged by qualified people
against a regulation or an auditor's criteria. A database configuration cannot be compliant on its
own, and neither can the operator that sets it up.

- A preset **configures** some technical controls and **checks** that they hold. That is all. The
  name `hipaa`, `soc2` or `pci` labels a configuration profile. It is not a claim that the profile
  satisfies any requirement of that standard.
- **The mapping from these controls to any regulation has not been reviewed by anyone qualified.**
  The project deliberately cites no regulation clauses, in code or in documentation, because a
  wrong citation is worse than none. Which controls you need, and whether these are enough, is a
  decision for your compliance officer or auditor.
- **`ControlsMet` being `True` means one thing:** every check this operator performs passed the
  last time it ran. It does not mean your cluster is compliant, secure, or audit-ready. Do not
  present it to an auditor as proof of compliance. If you use it at all, use it as one piece of
  evidence, together with the evidence text behind each control.
- Do not use the preset names in contracts, marketing or questionnaires as if they meant
  certification.

The rest of this page says exactly what is configured, exactly what is checked, and, in
[What you remain responsible for](#what-you-remain-responsible-for), the things that matter and
that the operator does not do or check.

## Turning it on

```yaml
spec:
  compliance:
    standard: hipaa          # hipaa, soc2 or pci
    auditLogs:
      destination:
        bucket: my-audit-logs
        region: eu-west-1
        credentialsSecret: audit-s3
      retentionDays: 2190    # optional; the standard's default if left out
      logParameters: false   # leave off, see below
    network:
      allowFrom: []          # more peers that may connect, besides labelled client pods
```

A complete example is in
[`config/samples/db_v1alpha1_postgrescluster_compliance.yaml`](../config/samples/db_v1alpha1_postgrescluster_compliance.yaml).
Before you apply it:

1. **Create the audit bucket and a credentials Secret** (`AWS_ACCESS_KEY_ID` and
   `AWS_SECRET_ACCESS_KEY`) in the cluster's namespace. The operator checks the Secret before it
   changes anything, and says what is wrong instead of starting pods that cannot start. The audit
   path must not overlap the cluster's backup path in the same bucket (the same, inside it, or
   around it); the webhook refuses it.
2. **Label every application pod that connects to the database** with
   `db.youroperator.io/client: "true"`, in the cluster's namespace, or list the pod in
   `network.allowFrom`. The network policy admits nothing else. **An application without the
   label loses its connection the moment the policy is created.** Do this first.
3. **Apply it.** On a running cluster this replaces every pod, one at a time, replicas first and
   the primary after a switchover; measured, writes were refused for about two seconds. A
   single-instance cluster is simply restarted. The webhook warns you about this when you apply.
4. **Wait up to ten minutes** for the first check, or run it now:

   ```bash
   kubectl create job check-now --from=cronjob/<cluster>-compliance-check
   kubectl create job probe-now --from=cronjob/<cluster>-network-probe
   ```

Changing `standard` from one to another only changes which statements are audited, applied to the
running pods without a restart. Removing `compliance` replaces the pods again, takes out the
shipper, the network policy and the check jobs, and clears the status. Logs already in the bucket
are left alone: the operator never deletes them.

## What a preset changes

| | |
|---|---|
| **Audit logging** | `pgaudit` is loaded, which needs the pod replacement above. `pgaudit.log` is set to the classes below, `pgaudit.log_catalog` is off, and `log_statement` is set to `none` so each statement is not logged twice. |
| **Audit log delivery** | A container beside each database container uploads the pod's PostgreSQL log to the bucket every five minutes, as gzipped chunks under `audit/<namespace>/<cluster>/<pod>/...` (or your `path`). |
| **Network policy** | A `NetworkPolicy` named after the cluster limits ingress to the database pods, see below. |
| **Checks** | Two small jobs run every ten minutes, see below. |

| Standard | Statements audited | Default `retentionDays` |
|---|---|---|
| `hipaa` | read, write, ddl, role | 2190 |
| `soc2` | read, write, ddl | 365 |
| `pci` | read, write, ddl, role | 365 |

**These are defaults the project chose. They are not advice about what your obligations require.**
All three standards check the same ten controls; the only differences are the statements audited
and the default retention.

**The network policy** admits, to PostgreSQL (5432): pods labelled as clients in the cluster's own
namespace, any peers you list in `allowFrom`, the cluster's own pods, its own jobs, and the
operator, which manages the database's roles over SQL. It admits, to Patroni's REST port (8008):
the cluster's own pods and the operator, and nothing else. A preset restricts ingress. To limit
what the database pods can connect to as well, set `spec.security.networkPolicy.egress`; see
[security.md](security.md#network-policy).

**`logParameters` writes values in clear text.** With it on, the values bound to a statement's
parameters are written to the audit log. With it off they are not. Even with it off, the text of a
statement is logged as the client sent it, so a value written directly into the SQL (`insert ...
values ('123-45-6789')`) is in the log. A role's password in `CREATE ROLE` is redacted. Treat the
audit log as sensitive data, and protect the bucket like the database.

## What the operator checks

Each control is `Met`, `NotMet` or `Unknown`, with the evidence in words. A control is `Unknown`,
never `Met`, when its evidence could not be read, or when the report it rests on is more than 35
minutes old.

| Control | What is checked, and where it comes from |
|---|---|
| `audit-logging` | **The running database:** `pgaudit` is in `shared_preload_libraries`, and `pgaudit.log` is exactly the standard's classes (in any order). A restart that has not happened shows up here. |
| `connection-logging` | **The running database:** `log_connections` and `log_disconnections` are on. |
| `encryption-in-transit` | **The running database:** `ssl` is on, and no connection rule can accept a connection without TLS. Rules that match only loopback addresses are ignored: they can be reached only from inside the pod. The evidence says who issued the server certificate when `spec.security.tls` supplies one, and that the image generated it otherwise. |
| `authentication` | **The running database:** `password_encryption` is `scram-sha-256`, every role's stored password hash is scram, no network connection rule is `trust`, `password` or `ident`, and every rule could be read. |
| `data-integrity` | **The running database:** `data_checksums` is on. It cannot be turned on for an existing cluster without stopping it. |
| `audit-log-delivery` | **The audit bucket:** each running pod has an upload no more than 15 minutes older than the check. A pod that has just started is `Unknown` until its first upload. |
| `audit-log-retention` | **The bucket's own configuration:** no enabled lifecycle rule can expire objects under the audit path in fewer than `retentionDays`, or the bucket's default object lock keeps them at least that long. `Unknown` if the store will not say. |
| `network-isolation` | **The network policy and a probe:** the policy exists and is the one the operator would write, and a pod with none of the allowed labels was refused when it tried to connect. A refusal counts only if the database could be reached in the same check. |
| `backup-recovery` | **The operator's own status:** backups are on, encrypted, healthy, and a restore test has passed. See [backup-and-restore.md](backup-and-restore.md). |
| `availability` | **The operator's own status:** more than one instance, a ready primary, and replication healthy. |

The first seven are read by the compliance check job, which connects to the database as a
read-only role, `pgoperator_monitor`, as the backup status job does, and reads the audit bucket.
That role can read the server's settings, the connection rules, and a count of the roles whose
password is not stored as SCRAM-SHA-256; it cannot read data, the password hashes, or other
sessions' queries, or change anything. No job holds the superuser's password. `network-isolation` also
uses the network probe, a pod that carries none of the labels the policy admits and tries to
connect. The jobs report facts; the operator decides what the facts mean.

### Reading the result

```bash
# One line per control
kubectl get postgrescluster <name> -o jsonpath=\
'{range .status.compliance.controls[*]}{.id}{"\t"}{.status}{"\t"}{.evidence}{"\n"}{end}'

# The roll-up
kubectl get postgrescluster <name> \
  -o jsonpath='{.status.conditions[?(@.type=="ControlsMet")]}'
```

- `ControlsMet` is **True** only when every control is `Met`, **False** when any is `NotMet`
  (the message names them), and **Unknown** while some are not yet known. It is never `True`
  because nothing has been checked.
- `status.compliance.lastChecked` is when the jobs last reported. A control's `observedAt` is that
  time. The same report always gives the same evidence.
- The metric `postgrescluster_compliance_controls{namespace,cluster,status}` counts the controls
  in each state. Alert on `status="NotMet"` above zero for fifteen minutes, and on `Unknown` for
  more than an hour, which means the jobs are not reporting.
- `pg-compliance` prints all of this, with the emergency access requests, as a report a person
  can read, and says when the newest report is too old to believe, which the status itself cannot
  say when the operator has stopped. See [compliance-report.md](compliance-report.md).

## What you remain responsible for

Anything that is not in the table above is not checked. These are yours to do:

- **People and process.** Policies, risk analysis, staff training, agreements with vendors,
  incident response, access reviews, and reading the audit log and acting on it.
- **The scope of the audit.** The operator checks that `pgaudit` logs the classes in the table.
  Decide whether those classes cover your obligations. Object-level auditing is not configured;
  set it up if you need it. A database superuser can change the audit settings for their own
  session, so limit who is one. A check is a snapshot every ten minutes.
- **Protecting the audit log.** The shipper and the check job use one Secret, and its credentials
  can write to the bucket. Anyone who holds them, or who can `kubectl exec` into a database pod,
  can overwrite or delete audit logs. Turn on bucket versioning and an object lock, set a
  restrictive bucket policy, and limit `exec` access.
- **Retention.** `audit-log-retention` reads the bucket's lifecycle rules. `Met` means no rule is
  configured to delete the logs early. Set the rules, and the object lock, on the bucket.
- **Encryption at rest.** Encrypt the data volumes and the audit bucket. Use an `https` endpoint
  for the audit destination. Backups are encrypted before upload when you set a key; store the key
  safely.
- **The network.** A preset limits ingress. Limit egress with `spec.security.networkPolicy.egress`
  ([security.md](security.md#network-policy)). The label that admits a client is a declaration by
  the pod, so limit who can create pods in the namespace. Use a network plugin that enforces
  `NetworkPolicy`; the probe checks ingress only.
- **Certificates.** Supply a certificate with `spec.security.tls.secretName`, renew it, and
  configure clients to verify it ([security.md](security.md#server-certificate)).
- **Roles and credentials.** Grants, least privilege and role reviews are yours. Set `rotateEvery`
  and the `credentialRotation` schedules, and alert on `postgrescluster_credential_age_seconds`
  ([access-control.md](access-control.md#replacing-passwords)).
- **Emergency access, if you allow it.** A person who has escalated is not recorded by pgaudit.
  Their statements are in the server log as `statement:` lines, which the shipper carries to the
  bucket, with the values bound to parameters in clear text. Protect the bucket accordingly
  ([emergency-access.md](emergency-access.md)).
- **The cluster around it.** Kubernetes RBAC, who can read Secrets, node security, image
  provenance, and the host the cluster runs on.
- **Checks that stop.** If the jobs stop running, the controls that rest on their reports go
  `Unknown` after 35 minutes. They do not go `NotMet`. Alert on `Unknown`.

The operator provides technical controls that support compliance programs. It is not a
certification and does not replace an audit.

## Operating a compliance cluster

- **A custom `spec.image`** must contain `pgaudit` and every extension the default image
  preloads, or the database pods will not start. The webhook warns about this.
- **Audit logs live on the data volume until shipped**, one file per weekday, reused after a week.
  If the bucket is unreachable for more than a week, the oldest lines are lost. Size the volume
  for your audit volume as well as your data. A cluster running backups and restore tests wrote
  about 1 MB of audit log in its first minutes; the `read` class is the noisy one. Measure on
  your own workload.
- **The shipper cannot take the database down.** Its container loop never exits, so a shipper that
  cannot reach its bucket costs only uploads, and shows as `audit-log-delivery`.
- **Permissions for the audit credentials.** The shipper needs to list the bucket and put and read
  objects under the audit path. The check job needs to list it and to read its lifecycle
  configuration and object lock configuration. An example for Amazon S3; check your provider's
  documentation for the exact names:

  ```json
  {
    "Version": "2012-10-17",
    "Statement": [
      {
        "Effect": "Allow",
        "Action": ["s3:GetLifecycleConfiguration", "s3:GetBucketObjectLockConfiguration"],
        "Resource": "arn:aws:s3:::my-audit-logs"
      },
      {
        "Effect": "Allow",
        "Action": "s3:ListBucket",
        "Resource": "arn:aws:s3:::my-audit-logs",
        "Condition": {"StringLike": {"s3:prefix": ["audit/prod/my-database/*"]}}
      },
      {
        "Effect": "Allow",
        "Action": ["s3:PutObject", "s3:GetObject"],
        "Resource": "arn:aws:s3:::my-audit-logs/audit/prod/my-database/*"
      }
    ]
  }
  ```

  The shipper deletes an object only if the size check right after uploading it fails.
- **Running the operator outside the cluster** (`make run`) cannot build a network policy, because
  the operator reads which pods it is from its own pod. A cluster with compliance settings is
  held back, with the reason in its `Progressing` condition.

## When a control is not met

| Control | What to look at |
|---|---|
| `audit-logging` | "not loaded": a restart is pending, a rollout is stuck, or the image lacks `pgaudit`; look at the pods' state and `kubectl describe postgrescluster`. "pgaudit.log is ...": someone changed it. A value written through Patroni is put back at the operator's next reconcile; one set with `ALTER SYSTEM` or in a session is not undone. |
| `connection-logging` | Something has changed `log_connections` or `log_disconnections`. The operator does not put these back. |
| `encryption-in-transit` | The evidence names the rules. The operator does not manage `pg_hba.conf`; the rules come from the image or from someone editing them. |
| `authentication` | A role with an old hash: set the password again with `password_encryption = 'scram-sha-256'`. A rule: the evidence names it. |
| `data-integrity` | Cannot be fixed in place. Restore into a new cluster, created with checksums on. |
| `audit-log-delivery` | `kubectl logs <pod> -c audit-shipper`. Usual causes: wrong credentials or endpoint, a missing bucket, a bucket policy that refuses writes. |
| `audit-log-retention` | The evidence names the lifecycle rule. Change the rule, or lower `retentionDays` if it is wrong. `Unknown` means the store does not report its lifecycle configuration. |
| `network-isolation` | "could connect": your network plugin does not enforce `NetworkPolicy`. "does not exist" or "not the one the operator writes": it is put back at the next reconcile. |
| `backup-recovery` | The evidence repeats the backup conditions. See [backup-and-restore.md](backup-and-restore.md). |
| `availability` | A single instance, no primary, or replication unhealthy: see `kubectl get postgrescluster`. |
