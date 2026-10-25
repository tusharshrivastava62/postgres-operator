# The compliance report

`pg-compliance` reads the status the operator keeps for a cluster with a
[compliance preset](compliance.md), and the
[emergency access](emergency-access.md) requests made for it, and prints them as a report a
person can read.

It reads and nothing else. It does not connect to a database, does not read a Secret, and does
not change anything in the cluster.

## Read this first

Every report says the following, in every format, and no flag turns it off. It is the same
wording as the pages above.

- Enabling a preset does not make a cluster compliant with HIPAA, SOC 2, PCI DSS or anything else.
- The mapping from these controls to any regulation has not been reviewed by anyone qualified.
- `ControlsMet` being `True` means one thing: every check this operator performs passed the last
  time it ran. It does not mean your cluster is compliant, secure, or audit-ready. Do not present
  it to an auditor as proof of compliance.
- Emergency access: pgaudit does not record an escalated session's reads and writes, and the
  values bound to their statements are in the server log in clear text, whatever `logParameters`
  says. The report prints the whole warning whenever the cluster allows emergency access or any
  request exists.

The report is a readable copy of what the operator recorded. It adds no check of its own, and
it is not evidence that anything is compliant.

## Get it

From a release, which has the command for Linux and macOS on amd64 and arm64, with a checksum
file:

```bash
tar -xzf pg-compliance_0.2.0_linux_amd64.tar.gz
sha256sum --check --ignore-missing pg-compliance_0.2.0_checksums.txt
```

From the source:

```bash
make build-cli                                   # writes bin/pg-compliance
go install github.com/tusharshrivastava62/postgres-operator/cmd/pg-compliance@latest
```

If the file is named `kubectl-pg_compliance` and is on your `PATH`, `kubectl pg-compliance` runs
it.

## Run it

Against a cluster, using your kubeconfig:

```bash
pg-compliance -n shop orders
pg-compliance -n shop orders -o markdown > report.md      # for a ticket
pg-compliance -n shop orders -o json > report.json        # for a script
```

From a capture, for someone who has no access to the cluster:

```bash
kubectl -n shop get postgrescluster,postgresemergencyaccess -o json > capture.json
pg-compliance -f capture.json
kubectl -n shop get postgrescluster,postgresemergencyaccess -o json | pg-compliance -f -
```

| Flag | Meaning |
|---|---|
| `-n`, `--namespace` | The cluster's namespace. With a cluster, the kubeconfig context's own is the default. With a capture, it picks the cluster if the capture holds more than one. |
| `--name`, or the argument | The cluster's name. |
| `-f` | Read a capture from this file, or `-` for standard input, instead of the cluster. |
| `-o`, `--output` | `text` (the default), `markdown` or `json`. |
| `--at` | Measure the age of the report at this time (RFC 3339), not now. |
| `--no-color` | Do not use colour. It is also off when `NO_COLOR` is set or the output is not a terminal. |
| `--kubeconfig`, `--context`, `--as` | As for `kubectl`. `--as` shows what a read-only user would see. |

The exit code says whether the tool worked: `0` when it wrote a report, whatever the report
says, and `1` when it could not (a bad flag, a cluster it cannot read, a file it cannot parse).
A cluster with controls that are not met still exits `0`. There is no option to fail on the
result: a pipeline that passes or fails on this report would make `ControlsMet` the pass or fail
signal the compliance page says it is not. To be told when a control is not met, alert on the
operator's metrics (`postgrescluster_compliance_controls`).

## The permission it needs

`get` on `postgresclusters` and `list` on `postgresemergencyaccesses`, both in
`db.youroperator.io`, in the cluster's namespace. Nothing else: no Secrets, no events. The two
viewer roles the operator ships (`postgrescluster-viewer-role` and
`postgresemergencyaccess-viewer-role`) grant exactly this when bound with a `RoleBinding`:

```bash
kubectl -n shop create rolebinding auditor-reads-postgres \
  --clusterrole=postgres-operator-postgrescluster-viewer-role --user=auditor@example.com
kubectl -n shop create rolebinding auditor-reads-requests \
  --clusterrole=postgres-operator-postgresemergencyaccess-viewer-role --user=auditor@example.com
```

Without the second, the command stops with an error that names the missing permission. It
does not print a report that leaves the emergency access record out.

## Reading it

The report has five parts, always in this order: the header, *Read this first*, *The operator's
own checks*, *Not checked by the operator*, and *Emergency access*.

### The checks

One line per control, in the order of the table in [compliance.md](compliance.md#what-the-operator-checks),
with the result and the evidence the operator wrote. The results are the operator's own words:
`Met`, `NotMet`, `Unknown`, and one more that only this command adds, `Out of date`.

**Out of date.** The operator reads the cluster every ten minutes. If the newest report is
more than 35 minutes old, the report shows every control as `Out of date` and puts the result
that was recorded next to it (`Recorded as Met.`). The operator cannot do this itself when it
is the thing that has stopped: its status simply stops changing, and a recorded `True` stays
`True`. The age is measured when you run the command, so a capture saved yesterday reads as out
of date today. `--at` measures it at another time, for example the moment the capture was taken,
and the header says which time was used.

**Not checked yet.** A cluster whose first report has not come in shows `Checked  never`. The
results that rest on the jobs are `Unknown`; a result the operator reads from its own status
may already be shown.

### Emergency access

The requests made for this cluster that exist now, newest first: who asked (as the API server
recorded it when the request was created), the reason they gave, when the access started, and
when it ended or ends. Each shows the database role (`bg_` and eight characters), so the server
log can be searched for what that role did.

This is the requests that exist now. The operator never deletes a request, but a person can,
and a deleted request leaves nothing in this report. The server log, and the Kubernetes API
server's audit log if you keep one, hold the rest of the record.

## JSON

The JSON has a `schemaVersion` (currently `1`) and carries the same wording as the other
formats, in `readThisFirst`, `notChecked` and `emergencyAccess.warning`, so a script that takes
the numbers has the caveats in the same document. Fields are added without a version change;
one is removed or changes meaning only with a new `schemaVersion`.

Each control has a `status` (what the report shows, including `OutOfDate`) and a `recorded`
(what the operator wrote).
