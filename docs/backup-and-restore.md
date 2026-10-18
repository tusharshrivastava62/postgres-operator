# Backup and restore

How backups work, how to restore, and what to watch. The fields are documented in the CRD
(`kubectl explain postgrescluster.spec.backup`); this page covers how to use them.

## What is backed up

With `backup.enabled`, three things run:

- **WAL archiving.** The primary sends each WAL segment to the bucket as soon as it is complete
  (wal-g, inside the database pod). This is what makes a restore to a chosen moment possible.
- **Base backups.** On `backup.schedule`, a full copy of the database is taken (in the pod, by
  Spilo's own cron) and sent to the bucket.
- **A status job every 10 minutes.** It reads the bucket back, reports what it found into
  `.status.backup` and the conditions, and deletes backups past `retention`.

A restore to any moment needs a base backup from before it, plus all WAL since. You can restore
to any time from the oldest kept base backup (`.status.backup.oldestRecoveryPoint`) to the latest
archived WAL.

Backups are physical, so they restore only to the **same PostgreSQL major version**. They are
written to `s3://<bucket>/<path>/wal/<major version>`, so a cluster never mixes versions.

## Where backups live

Inside the bucket, at `destination.path`, which defaults to `<namespace>/<cluster name>`. Two
clusters must not share a path, and the operator does not check that for you, except that the API
refuses a cluster that would archive into the path it was restored from. Changing the destination
starts a new backup history and the webhook warns you of that. Set `path` explicitly if you
recreate a cluster under a different name but want to keep its history.

## Credentials and encryption

Create both Secrets in the cluster's namespace before the cluster. The operator checks them first
and reports what is wrong instead of starting pods that cannot start.

```bash
kubectl create secret generic backup-s3 \
  --from-literal=AWS_ACCESS_KEY_ID=... --from-literal=AWS_SECRET_ACCESS_KEY=...
kubectl create secret generic backup-key \
  --from-literal=libsodium-key="$(openssl rand -hex 32)"
```

The key is 32 bytes written as 64 hexadecimal characters. **Keep a copy somewhere else.** Without
it, every encrypted backup is unreadable and nothing can recover them. The operator never creates,
changes or deletes either Secret.

## Bucket permissions

The credentials need to list the bucket and read, write and delete objects under the cluster's
path (delete is for retention). A policy for Amazon S3 that
limits one cluster to its own prefix:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": ["s3:ListBucket", "s3:GetBucketLocation"],
      "Resource": "arn:aws:s3:::my-database-backups",
      "Condition": {"StringLike": {"s3:prefix": ["prod/my-database/*", "prod/my-database"]}}
    },
    {
      "Effect": "Allow",
      "Action": ["s3:GetObject", "s3:PutObject", "s3:DeleteObject"],
      "Resource": "arn:aws:s3:::my-database-backups/prod/my-database/*"
    }
  ]
}
```

To restore from another cluster's backups, the restoring credentials need read access
(`s3:ListBucket`, `s3:GetObject`) to the source's prefix only. Turn on bucket versioning or object
lock if you want protection against accidental deletion; the operator never deletes outside its
own path.

## Retention

`retention.full` (default 7) and `retention.days` (default 14) both apply: a base backup is
deleted only when more than `full` backups exist **and** it is older than `days`. So `full: 7,
days: 14` with a daily backup keeps 14 days; with a weekly backup it keeps 7 backups, about 7
weeks. WAL older than the oldest kept base backup is deleted with it.

## Restore into a new cluster

A restore never overwrites a running cluster. Create a new cluster with `spec.bootstrap.recovery`;
see `config/samples/db_v1alpha1_postgrescluster_restore.yaml`.

1. **Find the moment to restore to**, and check it is inside the window:

   ```bash
   kubectl get postgrescluster my-database \
     -o jsonpath='{.status.backup.oldestRecoveryPoint}{"\n"}{.status.backup.lastBackupTime}{"\n"}'
   ```

   Leave `targetTime` out to restore to the latest archived WAL.

2. **Copy the source's Secrets** into the new cluster's namespace if it differs
   (`credentialsSecret`, and the encryption key if the source's backups were encrypted).

3. **Apply the new cluster.** Point `recovery.destination` at the source's bucket (and its `path`, if the
   source set one; otherwise it is `<namespace>/<name>` of the source), set the same `version`, and give the new cluster its own `backup.destination` (or leave backups
   off). It gets new passwords, and writes its own backups to its own path.

4. **Wait for `Available`**, then check the data:

   ```bash
   kubectl get postgrescluster my-database-restored -w
   kubectl get secret my-database-restored-credentials -o jsonpath='{.data.password}' | base64 -d
   ```

5. **Move traffic** to the new cluster's Service once you are satisfied, and delete the old one
   (its volumes and Secret stay until you delete them; see the README).

A target time earlier than the oldest base backup cannot be restored, and the new cluster will not
come up. The message is not friendly yet, so check step 1 first.

## Measuring recovery time

Turn on `backup.verification` (weekly is typical). The test restores the newest base backup into
scratch space, replays archived WAL, reads every database, and records the duration:

```bash
kubectl get postgrescluster my-database -o jsonpath='{.status.backup.verification}'
```

Treat `durationSeconds` as a floor for your recovery time: a real restore also has to start pods
and schedule volumes, and a big database may restore more slowly than a small one. The test pod
needs scratch space as large as the database (an `emptyDir` capped at `spec.storage.size`).

## What to alert on

| Signal | Meaning | Alert when |
|---|---|---|
| `WALArchivingHealthy` is `False` | `ArchivingBehind`: more than 8 WAL segments (16 MiB each) are waiting. `ArchivingStalled`: nothing archived for 15 minutes while WAL waits | immediately (it means you cannot restore past the last archived point) |
| `BackupHealthy` is `False` | `BucketUnreadable`, `NoBackup`, or `BackupOverdue` (newest is more than two schedule intervals plus 15 minutes old) | for more than one status interval |
| `RestoreVerified` is `False` | `RestoreFailed` or `RestoreTestOverdue` | immediately |
| `Degraded` is `True` | any of the above | immediately |
| any of these `Unknown` for over 35 minutes (`StatusStale`) | the status job itself is not running | for more than one hour |
| `postgrescluster_wal_archive_lag_bytes` | WAL waiting to be archived | `> 8 * 16MiB` for 10 minutes |
| `time() - postgrescluster_restore_verification_last_success_timestamp_seconds` | age of the last passing restore test | `> 2 * test interval` |
| `postgrescluster_restore_verification_duration_seconds` | measured time to recover | above your recovery time objective |

The status job runs every 10 minutes, so conditions can lag a real outage by that much. Archiving
lag is measured directly on the primary, so a stalled archiver is seen even when wal-g reports no
errors.

## Sizing

- **Data volume.** While the bucket is unreachable, WAL accumulates on the primary's volume.
  Size the volume for your WAL rate times the longest outage you will tolerate, on top of the
  database. A full volume stops the primary.
- **Bucket.** Roughly `retention` base backups (compressed, about the size of the database) plus
  the WAL for the retention window.
- **Restore test.** Ephemeral storage on the node equal to `spec.storage.size`, and CPU for
  replaying WAL.
- **New replicas** are built from the primary with `pg_basebackup`, which waits for the archiver.
  While archiving is failing, a new replica may not finish building.

## When something is wrong

- *`BackupHealthy` `BucketUnreadable`*: the message holds the wal-g error. Check the endpoint,
  region, credentials and the policy above. A store that hangs rather than refusing is reported
  after a 2 minute timeout.
- *The cluster does not start after you enabled backups*: the condition message says which Secret
  or key is missing or malformed. Fix the Secret; no pod restart is needed beyond what the
  operator does.
- *A restore starts but never becomes ready*: read the first pod's log. The usual causes are a
  wrong `destination.path`, a different major `version`, a missing or wrong encryption key, or a
  `targetTime` outside the recovery window.
