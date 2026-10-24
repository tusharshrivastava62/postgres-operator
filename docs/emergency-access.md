# Emergency access

Emergency access gives a person temporary superuser access to a cluster's databases, for an
emergency, with a record of who asked, why, and what they did. It is granted for as long as the
request says, and it ends by itself.

> ## WARNING: while you are escalated, your statements and their values are logged in clear text
>
> **Read this before you run your first statement.**
>
> Once you become the superuser (`SELECT set_user_u('postgres')`), the server writes **every
> statement you run** to its log, and it writes **the values you bind to a statement's
> parameters** there too, **in clear text**. This happens whatever the cluster's audit settings
> say: `logParameters: false` does not stop it. The log is kept, and on a cluster with a
> compliance preset it is shipped to the audit bucket.
>
> So a query that carries a password, a token, a card number or a patient's name as a parameter
> puts that value in the log. If you must run one, build it so the value is not a bound parameter
> and does not appear in the statement, or do not run it. If a value has already been logged, the
> log has to be cleaned or the value changed; the operator cannot do either.
>
> If you are using emergency access as part of an audit, **your auditors will be able to read
> every value you used.** Tell them before you start.

## What it is not

It is not a way to make an emergency safe. The person is a superuser. They can read every table,
change any data, create roles, and load code. The server's `set_user` extension, which does the
escalating, refuses three things while the session is escalated (`ALTER SYSTEM`, `COPY ... PROGRAM`
and changing `log_statement`). That is all it refuses. The image also has `plpython3u` installed,
which lets a superuser run code on the database host. Emergency access is **audited, not
contained.**

What it adds to giving someone the superuser password:

- the person has a login of their own, so everything they do is under a name that traces to their
  request;
- it is limited to the time asked for, and it is taken away by the operator, not by someone
  remembering;
- the request records who asked (the API server's own record of the user, not something they
  typed) and the reason they gave;
- there is no standing password to rotate, leak or forget.

## Turn it on

It is off. A cluster allows it with `spec.emergencyAccess`:

```yaml
spec:
  emergencyAccess:
    enabled: true
    maxDuration: 2h     # the longest one request may ask for; at most 8h, and 8h if unset
```

Turning it off ends every grant that is active, at once.

## Ask for access

Create a `PostgresEmergencyAccess` in the cluster's namespace. See
[`config/samples/db_v1alpha1_postgresemergencyaccess.yaml`](../config/samples/db_v1alpha1_postgresemergencyaccess.yaml).

```yaml
apiVersion: db.youroperator.io/v1alpha1
kind: PostgresEmergencyAccess
metadata:
  name: orders-out-of-connections
spec:
  clusterRef:
    name: orders
  duration: 30m        # default 1h; between 1m and the cluster's maxDuration
  reason: orders database is out of connections and the application is down
```

`kubectl apply` prints the warning above. The `reason` needs at least ten characters that are not
spaces, because it is what someone reading the trail afterwards has to go on. A request cannot be
changed once made: make another.

**Who may ask** is Kubernetes RBAC, on the `postgresemergencyaccesses` resource, separately from who
may edit the cluster. The chart and the kustomize install include admin, editor and viewer
roles for it. Give the permission to create requests to the few people who should have it.

**The requester is recorded by a webhook**, from the authenticated user of the API call, over
whatever the manifest says. If that webhook is not running, a request names nobody, and the
operator refuses to grant it.

## Use it

```bash
kubectl get postgresemergencyaccess orders-out-of-connections
# NAME                        CLUSTER   STATE    REQUESTED BY        EXPIRES
# orders-out-of-connections   orders    Active   alice@example.com   2026-10-24T13:30:00Z

kubectl get secret orders-out-of-connections-access -o jsonpath='{.data.password}' | base64 -d
```

The Secret `<request>-access` holds `username` (a role named `bg_` and eight characters), `password`,
`host`, `port` and `dbname`. Reading it is recorded by the Kubernetes audit log, as who read it.
Connect from a pod that is allowed to reach the database (on a cluster with a compliance preset,
one labelled `db.youroperator.io/client: "true"`), then become the superuser:

```sql
SELECT set_user_u('postgres');
-- work
SELECT reset_user();
```

- **The escalation belongs to one connection to one database.** A connection to another database
  starts unescalated, and you must call `set_user_u` there too. Access is granted in every database
  that exists when the request is granted; one created afterwards is not covered.
- Before escalating, the role can connect and do nothing else. `reset_user()` or closing the
  connection ends the escalation.
- It works through the read-only Service, on a replica, as well as the primary.
- Anything you create while escalated belongs to `postgres`, not to your role, and stays when the
  access ends.

## What is recorded

| What | Where | Who sees it |
|---|---|---|
| Who asked, when, the reason | The request object (`status.requestedBy`, `spec.reason`) and its events | Anyone who can read requests |
| Who created the request, who read the login | The Kubernetes API audit log, if you keep one | Cluster administrators |
| That the person escalated, and when | The server log: `Role bg_xxxx transitioning to Superuser Role postgres`, and the same on the way back | The server log; the audit bucket if there is one |
| Every statement run while escalated, **with the values bound to its parameters** | The server log, as `statement:` lines, under the role's name | The server log; the audit bucket if there is one |

**pgaudit does not record an escalated session's reads and writes.** Measured on PostgreSQL 17 with
`set_user`: an application role and a plain superuser are audited, and a session that has escalated is
not, except for DDL. The `statement:` lines that `set_user` writes are what record it. They are in the
same log file, so the audit log shipper carries them to the bucket, but a reader looking for pgaudit's
`AUDIT:` lines will not find them. Search for the role's name.

To go from a role to a request: the role's comment says which (`SELECT shobj_description(oid,
'pg_authid') FROM pg_roles WHERE rolname = 'bg_xxxx'`), and so does the request's `status.role`.

## How it ends

- **The time runs out.** The operator stops the role logging in, ends its sessions, takes back what it
  was granted in each database, drops the role, and deletes the Secret. The request stays, as
  `Expired`, with the times.
- **The request is deleted.** The same, before the request goes (a finalizer holds it).
- **The cluster stops allowing it, or is deleted.** The same, as `Revoked`.

`VALID UNTIL` on the role stops new logins at the end even if the operator is down. A session that is
already open is ended only when the operator next runs, so an operator that is down at the end leaves
open sessions running until it is back, and it ends them first thing. It also sweeps away any
emergency role no request owns, such as one a failure left behind, or one that came in a restore from
another cluster's backups.

## States

| State | Meaning |
|---|---|
| `Pending` | Seen, not yet granted. |
| `Failed` | Could not be granted yet (for example, no primary). Tried again every 15 seconds; the end time does not move. |
| `Active` | The person can log in. `status.expiresAt` says until when. |
| `Expired` | The time ran out, and the access is gone. |
| `Revoked` | Taken away early: the request was deleted, or the cluster stopped allowing it. |
| `Denied` | Refused, and nothing was granted: the cluster does not allow it, the time is more than it allows, or nobody is named. A refused request stays refused. |

The cluster's `EmergencyAccessActive` condition says whether any grant is active, and the metric
`postgrescluster_emergency_access_active` counts them, so an active grant can be alerted on.
