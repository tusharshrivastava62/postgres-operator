# Security options

`spec.security` holds the options that harden a cluster. **Every one of them is off by default.** A
cluster without them runs as described in the [defaults](#defaults) below. This page says what
each option does, how to turn it on, and what stays with you.

| Option | Default | What it changes | Pods replaced |
|---|---|---|---|
| `tls.secretName` | The server presents a certificate the image generates | The server presents a certificate you supply, and the operator and its jobs verify it | Yes |
| `tls.patroniAPI` | Patroni's REST API answers over plain HTTP | The API answers over TLS only | Yes |
| `patroniAPI.authentication` | The API changes the cluster for anyone who can reach port 8008 | The API refuses requests that change anything unless they carry a password | Yes |
| `restrictReplicationRole` | The replication role can log in and run queries; `pg_rewind` runs as the superuser | The replication role can only replicate; `pg_rewind` runs as a login of its own | Yes |
| `networkPolicy.ingress` | Any pod that can reach the database can connect | Only labelled client pods, the operator, the cluster's own pods and jobs, and peers you list can connect | No |
| `networkPolicy.egress` | The database pods can connect anywhere | The database pods can connect only to DNS, each other, the Kubernetes API server, and what you list | No |

Setting or removing an option marked "Yes" replaces every database pod, one at a time, replicas
first and the primary after a switchover. Writes are refused for about two seconds. A
single-instance cluster is restarted. The webhook warns you about this when you apply the change.
The two network policy options take effect at once.

[`config/samples/db_v1alpha1_postgrescluster_security.yaml`](../config/samples/db_v1alpha1_postgrescluster_security.yaml)
sets all of them.

## Server certificate

By default the database encrypts connections with a certificate the image generates. Clients
cannot check who they are talking to. With `tls.secretName` the server presents a certificate you
supply, and a client can verify it.

```yaml
spec:
  security:
    tls:
      secretName: my-database-tls
```

The Secret is in the cluster's namespace and has three keys:

| Key | Content |
|---|---|
| `tls.crt` | The certificate, followed by any intermediates |
| `tls.key` | Its private key |
| `ca.crt` | The CA that issued it |

The certificate must be valid for the cluster's Service, `<name>.<namespace>.svc`, and for the pods'
names under the headless Service, `*.<name>-headless.<namespace>.svc.cluster.local`. A
[cert-manager](https://cert-manager.io/) `Certificate` with `usages: [server auth, client auth]` and
those two `dnsNames` produces such a Secret.

**What the operator does.** It checks the Secret before it changes anything. If the Secret is
missing, is incomplete, or does not match the names above, the cluster is held back and
`Progressing` says why. Once the pods are replaced, it connects to the primary with verification
on, and so do the jobs it runs against the database. A certificate that does not verify, or has
expired, shows in the status:

```bash
kubectl get postgrescluster <name> -o jsonpath='{.status.security.tls}'
kubectl get postgrescluster <name> -o jsonpath='{.status.conditions[?(@.type=="TLSReady")]}'
```

`.status.security.tls` has `verified`, `issuer` and `notAfter`. `TLSReady` is `True` when the
operator has verified what the primary presents.

**Renewing.** Renew the certificate in the Secret. No pod is replaced. The server, and Patroni's API
if `tls.patroniAPI` is on, present the new certificate after they reload it, which can take a few
minutes.

**Connecting.** Give your clients `ca.crt` and connect to the Service name with
`sslmode=verify-full`. A client that connects by any other name is refused.

**Removing it.** Taking `tls` out replaces the pods again. The server goes back to the certificate the
image generates, and the operator stops verifying it.

**You are responsible for** issuing and renewing the certificate, and for configuring your clients
to verify it. The operator does not renew it.

## Patroni's REST API

Patroni's REST API listens on port 8008 of every database pod. It can change the cluster's
configuration, restart a member and hand leadership over. **By default it does not require
authentication and answers over plain HTTP**, to any pod that can reach the port.

```yaml
spec:
  security:
    tls:
      secretName: my-database-tls
      patroniAPI: true          # answer over TLS only
    patroniAPI:
      authentication: true      # changes need the password
```

**Authentication.** With `patroniAPI.authentication`, the API refuses every request that changes
anything (a configuration change, a restart, a reload, a switchover) unless it carries the cluster's
API password. The operator generates the password into `<name>-credentials` under
`patroni-api-password`, and uses it for its own requests. Requests that only read, such as the
kubelet's health checks, are answered to everyone. The password is replaced together with the
superuser's and the replication role's (see [access-control.md](access-control.md#replacing-passwords)).

**TLS.** With `tls.patroniAPI`, the API answers over TLS with the certificate from `tls.secretName`,
and the operator verifies it when it calls the API. Plain HTTP on port 8008 is not served:
anything of yours that calls the port over HTTP stops working. The backup the image takes when a
pod starts does not run, because it waits for Patroni over plain HTTP. Scheduled backups are not
affected.

Authentication without `tls.patroniAPI` sends the password over the pod network unencrypted. Set
both.

**Status.** `.status.security.patroniAPI` shows how the primary's API was started
(`authenticated` and `tls`).

**You are responsible for** limiting which pods can reach port 8008. See
[Network policy](#network-policy).

## Replication role

By default the role the replicas stream with, `replicator`, can also log in like any other user and
run queries. `pg_rewind`, which Patroni runs to bring a former primary back as a replica, runs as
the superuser.

```yaml
spec:
  security:
    restrictReplicationRole: true
```

With it set:

- The server refuses the replication role everything but replication.
- Patroni runs `pg_rewind` as `pgoperator_rewind`, a login that can do nothing but read the files
  `pg_rewind` needs. Its password is in `<name>-credentials` under `rewind-password`, and is
  replaced together with the superuser's.
- The operator writes the server's rules for who may connect, in place of the image's. The only
  rules that go are the image's rules for a group role it does not create.

Anything of yours that logs in as `replicator` to run queries stops working. Its state is in
`.status.accessControl.rewindRole`. Removing the option disables the `pgoperator_rewind` login. The
login is not dropped.

## Network policy

By default the database pods accept connections from any pod that can reach them and connect to
anywhere. A compliance preset limits what can connect to them (see
[compliance.md](compliance.md)). The options under `networkPolicy` give a cluster the same
ingress limit without a preset, and add a limit on egress. Both are written into one
`NetworkPolicy` named after the cluster.

```yaml
spec:
  security:
    networkPolicy:
      ingress:
        mode: Restricted
        allowFrom: []           # more peers that may connect to PostgreSQL
      egress:
        mode: Restricted
        allow:                  # where else the database pods may connect
        - to:
          - namespaceSelector:
              matchLabels:
                kubernetes.io/metadata.name: object-store
            podSelector:
              matchLabels:
                app: s3
          ports:
          - protocol: TCP
            port: 8333
```

**Ingress.** With `ingress.mode: Restricted`, these may connect to PostgreSQL (5432): pods labelled
`db.youroperator.io/client: "true"` in the cluster's own namespace, the cluster's own pods and jobs,
the operator, and the peers in `allowFrom`. To Patroni's port (8008), only the cluster's own pods and
the operator may connect. **An application without the label loses its connection the moment the
policy is created.** Label the application's pods first.

**Egress.** With `egress.mode: Restricted`, the database pods may connect only to:

- DNS: the `kube-dns` pods in `kube-system`, port 53 over UDP and TCP.
- Each other, on ports 5432 and 8008.
- The Kubernetes API server, at the addresses the `kubernetes` Endpoints object in the `default`
  namespace lists. Patroni keeps its leader lock there.
- What is in `allow`, as `NetworkPolicy` egress rules.

Backups, restores and audit logs go to an object store. A `NetworkPolicy` cannot name a host, so
list the object store in `allow`, by address (`ipBlock`) or by the pods that serve it. The webhook
refuses `egress` without an `allow` entry when the cluster has any of them. A DNS other than
`kube-dns` in `kube-system` is listed in `allow` too. Anything the database pods need and `allow`
does not list fails.

**Enforcement.** A `NetworkPolicy` does nothing unless the cluster's network plugin enforces it.
The operator does not verify that the plugin enforces egress. Test it: from a database pod, try to
connect to an address `allow` does not list.

**Removing it.** The policy is deleted, unless a compliance preset still needs it, in which case it
goes back to the preset's ingress rules.

**You are responsible for** labelling your application pods, listing the object store and any
other destination, and choosing a network plugin that enforces egress.

## Defaults

With no `spec.security` and no compliance preset:

- The server encrypts connections with a certificate the image generates. Clients cannot verify it.
- Patroni's REST API on port 8008 does not require authentication and answers over plain HTTP.
- The replication role can log in and run queries. `pg_rewind` runs as the superuser.
- No `NetworkPolicy` is created. Any pod that can reach the database can connect, and the database
  pods can connect anywhere.

## What the options do not cover

The options on this page do not cover who can read the cluster's Secrets, who can create pods in
its namespace (the client label is a declaration by the pod), encryption of the data volumes, or the
security of the nodes and of the object store. Those belong to your Kubernetes and storage setup.

The operator provides technical controls that support compliance programs. It is not a
certification and does not replace an audit.
