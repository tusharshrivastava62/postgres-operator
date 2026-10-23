//go:build e2e

/*
Copyright 2026.

Licensed under the Business Source License 1.1 (the "License"). You may not
use this file except in compliance with the License, a copy of which is in the
LICENSE file at the root of this repository. Production use requires a
commercial license from the Licensor. On the Change Date (2030-10-17) the
work becomes available under the Change License (Apache-2.0).
*/

package e2e

import (
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const (
	clusterNamespace = "e2e-postgres"
	clusterName      = "demo"
	instances        = 3

	// Bringing up a cluster pulls a ~1GB image on a fresh node and waits for
	// Patroni to initialise a database and three members to join, so this is
	// far longer than any other wait here.
	clusterUpTimeout = 8 * time.Minute
	// Patroni's default failover timeout is 30s and the readiness probe takes
	// 15s to notice a hung pod, so a failover is done within about a minute.
	failoverWait = 2 * time.Minute
	pollEvery    = 3 * time.Second
)

// The cluster the scenarios run against. By default it sets no spec.image, so
// the operator's own default image for the version is used: the project's
// published Spilo build, which is the path every user without an override
// takes. E2E_POSTGRES_VERSION picks another PostgreSQL major version, and
// E2E_POSTGRES_IMAGE overrides the image (for trying a local build).
const defaultPostgresVersion = "17"

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func clusterManifest() string {
	version := envOr("E2E_POSTGRES_VERSION", defaultPostgresVersion)
	imageLine := ""
	if image := os.Getenv("E2E_POSTGRES_IMAGE"); image != "" {
		imageLine = "  image: " + image + "\n"
	}
	return fmt.Sprintf(`apiVersion: db.youroperator.io/v1alpha1
kind: PostgresCluster
metadata:
  name: %s
  namespace: %s
spec:
  version: "%s"
  instances: %d
%s  storage:
    size: 1Gi
  resources:
    requests:
      cpu: 100m
      memory: 256Mi
    limits:
      memory: 1Gi
`, clusterName, clusterNamespace, version, instances, imageLine)
}

// mainSelector selects the main cluster's objects, so scenarios that add other
// clusters to the namespace do not count theirs.
const mainSelector = "app.kubernetes.io/instance=" + clusterName

// mainRole selects the main cluster's pods that Patroni has given a role.
func mainRole(role string) string {
	return mainSelector + ",db.youroperator.io/role=" + role
}

// operatorDeployment is the controller Deployment created by make deploy.
const operatorDeployment = "postgres-operator-controller-manager"

// clusterSpecs are the end-to-end scenarios that create real PostgreSQL
// clusters and break them. They run in order against one three-instance
// cluster, because bringing one up is the slow part, and each builds on the
// state the previous left. They are registered inside the "Manager" container
// so they share the operator deployed there.
func clusterSpecs() {
	podsOf := func(selector string) []string {
		out, err := inCluster("get", "pods", "-l", selector, "-o", "jsonpath={.items[*].metadata.name}")
		Expect(err).NotTo(HaveOccurred())
		return strings.Fields(out)
	}
	currentPrimary := func(g Gomega) string {
		out, err := clusterField(".status.primaryInstance")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(out).NotTo(BeEmpty(), "no primary reported yet")
		return out
	}
	// leaderRecord is Patroni's own record of the leader, read without the
	// operator. It is what the read-write Service follows.
	leaderRecord := func(g Gomega) string {
		out, err := inCluster("get", "endpoints", clusterName, "-o", "jsonpath={.metadata.annotations.leader}")
		g.Expect(err).NotTo(HaveOccurred())
		return out
	}
	expectHealthy := func(g Gomega) {
		available, err := conditionStatus("Available")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(available).To(Equal("True"), "Available")
		replication, err := conditionStatus("ReplicationHealthy")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(replication).To(Equal("True"), "ReplicationHealthy")
		ready, err := inCluster("get", "statefulset", clusterName, "-o", "jsonpath={.status.readyReplicas}")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(ready).To(Equal(fmt.Sprint(instances)), "ready replicas")
	}
	// writeThroughService inserts a row by the read-write Service, from a pod,
	// exactly as an application would.
	writeThroughService := func(g Gomega, fromPod, value string) {
		_, err := psql(fromPod, clusterName, fmt.Sprintf("insert into e2e_rows(v) values ('%s')", value))
		g.Expect(err).NotTo(HaveOccurred())
	}
	// rowCount counts the rows the scenarios insert by name. The continuous
	// writer's rows are counted separately, by the scenario that runs it.
	rowCount := func(pod, host string) (string, error) {
		return psql(pod, host, "select count(*) from e2e_rows where v <> 'writer'")
	}

	BeforeAll(func() {
		_, err := kubectlOut("create", "namespace", clusterNamespace)
		Expect(err).NotTo(HaveOccurred())
		// The strictest Pod Security level. Admission rejects any pod that
		// runs as root or can escalate, so a cluster that comes up here has
		// proved it does neither.
		_, err = kubectlOut("label", "namespace", clusterNamespace, "pod-security.kubernetes.io/enforce=restricted")
		Expect(err).NotTo(HaveOccurred())
	})

	AfterAll(func() {
		_, _ = kubectlOut("scale", "deployment", operatorDeployment, "-n", namespace, "--replicas=1")
		_, _ = kubectlOut("delete", "namespace", clusterNamespace, "--wait=false")
		stopObjectStore()
	})

	AfterEach(func() {
		if !CurrentSpecReport().Failed() {
			return
		}
		const get = "get"
		for _, args := range [][]string{
			{get, "postgrescluster", clusterName, "-o", "yaml"},
			{get, "pods", "-o", "wide", "--show-labels"},
			{get, "endpoints", clusterName, "-o", "yaml"},
			{get, "events", "--sort-by=.lastTimestamp"},
		} {
			out, err := inCluster(args...)
			_, _ = fmt.Fprintf(GinkgoWriter, "--- kubectl %s\n%s\n%v\n", strings.Join(args, " "), out, err)
		}
		if pods := podsOf("app.kubernetes.io/component=postgres"); len(pods) > 0 {
			out, err := inCluster("exec", pods[0], "-c", "postgres", "--", "patronictl", "list")
			_, _ = fmt.Fprintf(GinkgoWriter, "--- patronictl list\n%s\n%v\n", out, err)
		}
		out, _ := kubectlOut("logs", "-n", namespace, "deployment/"+operatorDeployment, "--tail=100")
		_, _ = fmt.Fprintf(GinkgoWriter, "--- operator log\n%s\n", out)
	})

	It("refuses, through the real webhook, a cluster with no memory limit", func() {
		manifest := strings.Replace(clusterManifest(), "limits:\n      memory: 1Gi\n", "", 1)
		manifest = strings.Replace(manifest, "name: "+clusterName, "name: no-limit", 1)
		Eventually(func(g Gomega) {
			_, err := kubectlIn(manifest, "apply", "-f", "-")
			g.Expect(err).To(HaveOccurred())
			// The webhook may not be serving yet; only its own rejection counts.
			g.Expect(err.Error()).To(ContainSubstring("limits[memory]"))
		}, time.Minute, pollEvery).Should(Succeed())
	})

	It("applies the compliance rules through the real webhook", func() {
		// complianceManifest is a cluster with backups and a compliance block.
		// None of these are stored: the API server only runs them through
		// admission.
		complianceManifest := func(auditDestination string) string {
			return fmt.Sprintf(`apiVersion: db.youroperator.io/v1alpha1
kind: PostgresCluster
metadata:
  name: compliance-check
  namespace: %[1]s
spec:
  version: "17"
  instances: 1
  storage:
    size: 1Gi
  resources:
    limits:
      memory: 1Gi
  backup:
    enabled: true
    schedule: "0 2 * * *"
    destination:
      bucket: %[2]s
      endpoint: %[3]s
      forcePathStyle: true
      credentialsSecret: backup-s3
  compliance:
    standard: hipaa
    auditLogs:
%[4]s`, clusterNamespace, backupBucket, objectStoreEndpoint, auditDestination)
		}
		destination := func(bucket, path string, extra string) string {
			return fmt.Sprintf(`      destination:
        bucket: %s
        endpoint: %s
        forcePathStyle: true
        credentialsSecret: audit-s3
        path: %s
%s`, bucket, objectStoreEndpoint, path, extra)
		}

		By("refusing a standard with nowhere to send audit logs")
		Eventually(func(g Gomega) {
			out, err := kubectlDryRun(complianceManifest("      logParameters: false\n"))
			g.Expect(err).To(HaveOccurred(), out)
			// The webhook may not be serving yet; only its own rejection counts.
			g.Expect(out).To(ContainSubstring("spec.compliance.auditLogs.destination"))
		}, time.Minute, pollEvery).Should(Succeed())

		By("refusing audit logs inside the cluster's backup path")
		inside := destination(backupBucket, clusterNamespace+"/compliance-check/audit", "")
		out, err := kubectlDryRun(complianceManifest(inside))
		Expect(err).To(HaveOccurred(), out)
		Expect(out).To(ContainSubstring("spec.compliance.auditLogs.destination.path"))
		Expect(out).To(ContainSubstring("backups"))

		By("accepting a separate path, and warning that logged parameters are written in clear text")
		logged := destination(backupBucket, "audit-logs/compliance-check", "      logParameters: true\n")
		out, err = kubectlDryRun(complianceManifest(logged))
		Expect(err).NotTo(HaveOccurred(), out)
		Expect(out).To(ContainSubstring("Warning:"))
		Expect(out).To(ContainSubstring("clear text"))

		By("accepting the same path in another bucket without a warning")
		out, err = kubectlDryRun(complianceManifest(destination("audit-bucket", clusterNamespace+"/compliance-check", "")))
		Expect(err).NotTo(HaveOccurred(), out)
		Expect(out).NotTo(ContainSubstring("Warning:"))
	})

	It("brings up three instances with one primary and two streaming replicas", func() {
		Eventually(func(g Gomega) {
			_, err := kubectlIn(clusterManifest(), "apply", "-f", "-")
			g.Expect(err).NotTo(HaveOccurred())
		}, time.Minute, pollEvery).Should(Succeed())

		Eventually(expectHealthy, clusterUpTimeout, pollEvery).Should(Succeed())

		primary := ""
		Eventually(func(g Gomega) { primary = currentPrimary(g) }, time.Minute, pollEvery).Should(Succeed())

		By("labelling exactly that pod as the primary, and the rest as replicas")
		Expect(podsOf(mainRole("primary"))).To(ConsistOf(primary))
		Expect(podsOf(mainRole("replica"))).To(HaveLen(instances - 1))

		By("pointing the read-write Service at the primary only")
		ip, err := inCluster("get", "pod", primary, "-o", "jsonpath={.status.podIP}")
		Expect(err).NotTo(HaveOccurred())
		addresses, err := inCluster("get", "endpoints", clusterName, "-o", "jsonpath={.subsets[*].addresses[*].ip}")
		Expect(err).NotTo(HaveOccurred())
		Expect(addresses).To(Equal(ip))

		By("recording the image and a lag figure for each replica")
		image, err := clusterField(".status.image")
		Expect(err).NotTo(HaveOccurred())
		Expect(image).NotTo(BeEmpty())
		lag, err := clusterField(".status.replicationLagBytes")
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.Count(lag, ":")).To(Equal(instances - 1))
	})

	It("runs as a non-root user", func() {
		primary, err := clusterField(".status.primaryInstance")
		Expect(err).NotTo(HaveOccurred())
		uid, err := runInPod(primary, "id -u")
		Expect(err).NotTo(HaveOccurred())
		Expect(uid).To(Equal("101"))
		securityContext, err := inCluster("get", "pod", primary, "-o",
			"jsonpath={.spec.securityContext.runAsNonRoot} {.spec.containers[0].securityContext.allowPrivilegeEscalation}")
		Expect(err).NotTo(HaveOccurred())
		Expect(securityContext).To(Equal("true false"))
	})

	It("rejects the image's well-known default password and unencrypted connections", func() {
		primary, err := clusterField(".status.primaryInstance")
		Expect(err).NotTo(HaveOccurred())

		_, err = runInPod(primary, `PGPASSWORD=zalando psql -h "$1" -U postgres -d postgres -tAX -c "select 1"`, clusterName)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("password authentication failed"))

		_, err = runInPod(primary,
			`PGSSLMODE=disable PGPASSWORD="$PGPASSWORD_SUPERUSER" `+
				`psql -h "$1" -U postgres -d postgres -tAX -c "select 1"`, clusterName)
		Expect(err).To(HaveOccurred())

		By("accepting the generated password over TLS")
		out, err := psql(primary, clusterName, "select 1")
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(Equal("1"))
	})

	It("replicates a write made through the read-write Service to every replica", func() {
		primary, err := clusterField(".status.primaryInstance")
		Expect(err).NotTo(HaveOccurred())
		_, err = psql(primary, clusterName, "create table e2e_rows (id serial primary key, v text)")
		Expect(err).NotTo(HaveOccurred())
		writeThroughService(Default, primary, "first")

		for _, replica := range podsOf(mainRole("replica")) {
			Eventually(func(g Gomega) {
				out, err := rowCount(replica, "")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(Equal("1"))
			}, time.Minute, pollEvery).Should(Succeed(), "replica %s", replica)
		}

		By("serving reads from replicas through the read-only Service")
		Eventually(func(g Gomega) {
			out, err := psql(primary, clusterName+"-readonly", "select pg_is_in_recovery()")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).To(Equal("t"))
		}, time.Minute, pollEvery).Should(Succeed())
	})

	It("applies a change to spec.ha to the running cluster without restarting any pod", func() {
		before, err := inCluster("get", "pods", "-o", "jsonpath={.items[*].metadata.uid}")
		Expect(err).NotTo(HaveOccurred())
		primary, err := clusterField(".status.primaryInstance")
		Expect(err).NotTo(HaveOccurred())

		_, err = inCluster("patch", "postgrescluster", clusterName, "--type=merge",
			"-p", `{"spec":{"ha":{"failoverTimeout":45}}}`)
		Expect(err).NotTo(HaveOccurred())

		Eventually(func(g Gomega) {
			out, err := inCluster("exec", primary, "-c", "postgres", "--", "patronictl", "show-config")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).To(ContainSubstring("ttl: 45"))
			g.Expect(out).To(ContainSubstring("loop_wait: 15"))
		}, 2*time.Minute, pollEvery).Should(Succeed())

		after, err := inCluster("get", "pods", "-o", "jsonpath={.items[*].metadata.uid}")
		Expect(err).NotTo(HaveOccurred())
		Expect(after).To(Equal(before), "pods were replaced")
	})

	It("repairs a deleted RoleBinding without disturbing the primary", func() {
		primary, err := clusterField(".status.primaryInstance")
		Expect(err).NotTo(HaveOccurred())

		_, err = inCluster("delete", "rolebinding", clusterName+"-patroni")
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() error {
			_, err := inCluster("get", "rolebinding", clusterName+"-patroni")
			return err
		}, time.Minute, pollEvery).Should(Succeed())

		Consistently(func(g Gomega) {
			g.Expect(currentPrimary(g)).To(Equal(primary))
		}, 20*time.Second, pollEvery).Should(Succeed())
	})

	It("updates every pod one at a time, handing leadership over before it replaces the primary", func() {
		image, err := clusterField(".status.image")
		Expect(err).NotTo(HaveOccurred())
		originalPrimary, err := clusterField(".status.primaryInstance")
		Expect(err).NotTo(HaveOccurred())

		_, err = psql(originalPrimary, "", "create table if not exists e2e_rows (id serial primary key, v text)")
		Expect(err).NotTo(HaveOccurred())

		_, err = kubectlIn(writerManifest(image), "apply", "-f", "-")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _, _ = inCluster("delete", "pod", "writer", "--wait=false") })
		Eventually(func(g Gomega) {
			log, err := inCluster("logs", "writer")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(parseWriterLog(log).acknowledged).To(BeNumerically(">=", 20))
		}, 3*time.Minute, pollEvery).Should(Succeed())

		By("changing the pod template, which requires replacing every pod")
		_, err = inCluster("patch", "postgrescluster", clusterName, "--type=merge",
			"-p", `{"spec":{"resources":{"requests":{"cpu":"150m"}}}}`)
		Expect(err).NotTo(HaveOccurred())

		Eventually(func(g Gomega) {
			requests, err := inCluster("get", "pods", "-l", "app.kubernetes.io/component=postgres", "-o",
				"jsonpath={.items[*].spec.containers[0].resources.requests.cpu}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.Fields(requests)).To(HaveLen(instances))
			g.Expect(strings.Fields(requests)).To(HaveEach("150m"))
			expectHealthy(g)
		}, clusterUpTimeout+5*time.Minute, pollEvery).Should(Succeed())

		By("checking what the application saw")
		log, err := inCluster("logs", "writer")
		Expect(err).NotTo(HaveOccurred())
		result := parseWriterLog(log)
		AddReportEntry("rolling update as seen by a writer",
			fmt.Sprintf("%d acknowledged, %d failed, longest gap %s", result.acknowledged, result.failed, result.longestGap))
		finalPrimary, err := clusterField(".status.primaryInstance")
		Expect(err).NotTo(HaveOccurred())
		Expect(finalPrimary).NotTo(Equal(originalPrimary),
			"leadership must have moved off the first primary before it was replaced")

		// A switchover takes about 2 seconds (2.0 measured; the same cluster
		// updated by an ordinary StatefulSet rolling update measured 2.3).
		// The bound is generous: what it guards against is a handover that
		// fails and leaves writes refused until Patroni's failover timeout.
		Expect(result.longestGap).To(BeNumerically("<", 15*time.Second))

		By("keeping every acknowledged write")
		stored, err := psql(finalPrimary, "", "select count(*) from e2e_rows where v = 'writer'")
		Expect(err).NotTo(HaveOccurred())
		count, err := strconv.Atoi(stored)
		Expect(err).NotTo(HaveOccurred())
		Expect(count).To(BeNumerically(">=", result.acknowledged))
	})

	It("keeps application roles: a login of their own, a database and a Secret, and nothing more", func() {
		const (
			ordersSecret    = clusterName + "-role-app-orders"
			reportingSecret = clusterName + "-role-reporting"
			tempSecret      = clusterName + "-role-temp-job"
		)
		patchRoles := func(roles string) {
			_, err := inCluster("patch", "postgrescluster", clusterName, "--type=merge", "-p", `{"spec":{"roles":`+roles+`}}`)
			ExpectWithOffset(1, err).NotTo(HaveOccurred())
		}
		const kept = `[{"name":"app_orders","database":"orders","connectionLimit":20},{"name":"reporting"}`

		By("asking for two roles, one with a database of its own")
		patchRoles(kept + `]`)
		Eventually(func(g Gomega) {
			g.Expect(conditionStatus("RolesReady")).To(Equal("True"))
			for _, role := range []string{"app_orders", "reporting"} {
				g.Expect(roleState(clusterName, role)).To(Equal("Ready"), role)
			}
		}, 3*time.Minute, pollEvery).Should(Succeed())
		primary, err := clusterField(".status.primaryInstance")
		Expect(err).NotTo(HaveOccurred())

		By("logging in through the Service with the details in the Secret")
		out, err := asRole(primary, clusterName, ordersSecret, "orders",
			"create table orders_t (i int); insert into orders_t values (1), (2); select count(*) from orders_t")
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(ContainSubstring("INSERT 0 2"))
		Expect(strings.TrimSpace(out)).To(HaveSuffix("2"))
		dbname, err := secretValue(ordersSecret, "dbname")
		Expect(err).NotTo(HaveOccurred())
		Expect(dbname).To(Equal("orders"))
		host, err := secretValue(ordersSecret, "host")
		Expect(err).NotTo(HaveOccurred())
		Expect(host).To(Equal(clusterName + "." + clusterNamespace + ".svc"))

		By("keeping the other role out of that database, and giving neither any power over the server")
		out, err = asRole(primary, clusterName, reportingSecret, "orders", "select 1")
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(ContainSubstring("permission denied for database"))
		for _, statement := range []string{"create role evil", "create database evil", "alter role app_orders superuser"} {
			out, err = asRole(primary, clusterName, ordersSecret, "orders", statement)
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(ContainSubstring("permission denied"), statement)
		}
		attributes, err := psql(primary, "", "select rolname, rolsuper, rolcreatedb, rolcreaterole, rolreplication, rolbypassrls, "+
			"rolcanlogin, rolconnlimit from pg_roles where rolname in ('app_orders', 'reporting') order by 1")
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.Fields(attributes)).To(Equal([]string{"app_orders|f|f|f|f|f|t|20", "reporting|f|f|f|f|f|t|-1"}))

		By("never writing a role's password in the server log, whatever sets it")
		for _, secret := range []string{ordersSecret, reportingSecret} {
			password, err := secretValue(secret, "password")
			Expect(err).NotTo(HaveOccurred())
			pods, err := inCluster("get", "pods", "-l", mainSelector+",app.kubernetes.io/component=postgres", "-o", "name")
			Expect(err).NotTo(HaveOccurred())
			for pod := range strings.FieldsSeq(pods) {
				count, err := runInPod(strings.TrimPrefix(pod, "pod/"),
					fmt.Sprintf(`cat /home/postgres/pgdata/pgroot/pg_log/*.csv | grep -c -F '%s' || true`, password))
				Expect(err).NotTo(HaveOccurred())
				Expect(strings.TrimSpace(count)).To(Equal("0"), "%s in the log of %s", secret, pod)
			}
		}

		By("putting right a password changed behind the operator's back")
		original, err := secretValue(ordersSecret, "password")
		Expect(err).NotTo(HaveOccurred())
		_, err = psql(primary, "", "alter role app_orders password 'changed-behind-its-back'")
		Expect(err).NotTo(HaveOccurred())
		Eventually(func(g Gomega) {
			// The operator trusts what it found for a few minutes; a change to
			// the Secret is what makes it look at once.
			_, err := inCluster("annotate", "secret", ordersSecret, "--overwrite", "e2e/poke="+time.Now().Format(time.RFC3339Nano))
			g.Expect(err).NotTo(HaveOccurred())
			out, err := asRole(primary, clusterName, ordersSecret, "orders", "select count(*) from orders_t")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(out)).To(Equal("2"))
		}, 3*time.Minute, pollEvery).Should(Succeed())
		current, err := secretValue(ordersSecret, "password")
		Expect(err).NotTo(HaveOccurred())
		Expect(current).To(Equal(original), "the Secret's password is the one the role has again")

		By("disabling a role that is removed from the spec, and keeping it")
		patchRoles(kept + `,{"name":"temp_job"}]`)
		Eventually(func() (string, error) { return roleState(clusterName, "temp_job") }, 3*time.Minute, pollEvery).Should(Equal("Ready"))
		out, err = asRole(primary, clusterName, tempSecret, "postgres", "select current_user")
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.TrimSpace(out)).To(Equal("temp_job"))
		patchRoles(kept + `]`)
		Eventually(func() (string, error) { return roleState(clusterName, "temp_job") }, 3*time.Minute, pollEvery).Should(Equal("Disabled"))
		out, err = asRole(primary, clusterName, tempSecret, "postgres", "select 1")
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(ContainSubstring("is not permitted to log in"))
		exists, err := psql(primary, "", "select rolcanlogin from pg_roles where rolname = 'temp_job'")
		Expect(err).NotTo(HaveOccurred())
		Expect(exists).To(Equal("f"), "the role is kept, and cannot log in")
		Expect(conditionStatus("RolesReady")).To(Equal("True"), "a disabled role is not one that is not ready")

		By("reporting a role it did not make, and leaving it as it was")
		_, err = psql(primary, "", "create role handmade login superuser")
		Expect(err).NotTo(HaveOccurred())
		patchRoles(kept + `,{"name":"handmade"}]`)
		Eventually(func() (string, error) { return roleState(clusterName, "handmade") }, 3*time.Minute, pollEvery).Should(Equal("Error"))
		message, err := clusterField(`.status.accessControl.roles[?(@.name=="handmade")].message`)
		Expect(err).NotTo(HaveOccurred())
		Expect(message).To(ContainSubstring("was not made by this operator"))
		super, err := psql(primary, "", "select rolsuper from pg_roles where rolname = 'handmade'")
		Expect(err).NotTo(HaveOccurred())
		Expect(super).To(Equal("t"), "the role is not touched")
		patchRoles(kept + `]`)
		_, err = psql(primary, "", "drop role handmade")
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() (string, error) { return conditionStatus("RolesReady") }, 3*time.Minute, pollEvery).Should(Equal("True"))
	})

	It("archives WAL and takes encrypted base backups to an object store, then keeps them across a failover", func() {
		startObjectStore()
		for name, data := range map[string][]string{
			"backup-s3":  {"AWS_ACCESS_KEY_ID=" + objectStoreAccessKey, "AWS_SECRET_ACCESS_KEY=" + objectStoreSecretKey},
			"backup-key": {"libsodium-key=" + strings.Repeat("0123456789abcdef", 4)},
		} {
			args := make([]string, 0, 4+len(data))
			args = append(args, "create", "secret", "generic", name)
			for _, kv := range data {
				args = append(args, "--from-literal="+kv)
			}
			_, err := inCluster(args...)
			Expect(err).NotTo(HaveOccurred())
		}

		By("turning backups on, which updates every pod")
		patch := fmt.Sprintf(`{"spec":{"backup":{"enabled":true,"schedule":"*/2 * * * *",`+
			`"destination":{"bucket":%q,"endpoint":%q,"forcePathStyle":true,"credentialsSecret":"backup-s3"},`+
			`"encryption":{"keySecret":"backup-key"},"retention":{"full":2,"days":1}}}}`, backupBucket, objectStoreEndpoint)
		_, err := inCluster("patch", "postgrescluster", clusterName, "--type=merge", "-p", patch)
		Expect(err).NotTo(HaveOccurred())
		Eventually(func(g Gomega) {
			envs, err := inCluster("get", "pods", "-l", "app.kubernetes.io/component=postgres", "-o",
				`jsonpath={range .items[*]}{.spec.containers[0].env[?(@.name=="USE_WALG_BACKUP")].value}{" "}{end}`)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.Fields(envs)).To(HaveLen(instances))
			g.Expect(strings.Fields(envs)).To(HaveEach("true"))
			expectHealthy(g)
		}, clusterUpTimeout+5*time.Minute, pollEvery).Should(Succeed())

		By("making the read-only role the backup status job connects as, with a Secret of its own")
		Eventually(func() (string, error) { return clusterField(".status.accessControl.monitorRole.state") },
			3*time.Minute, pollEvery).Should(Equal("Ready"))
		secrets, err := cronJobSecrets(clusterName + "-backup-status")
		Expect(err).NotTo(HaveOccurred())
		Expect(secrets).To(ContainSubstring(clusterName + "-monitor"))
		Expect(secrets).NotTo(ContainSubstring(clusterName+"-credentials"), "no job holds the superuser's password")

		By("finding a base backup in the bucket, read back with the key")
		primary, err := clusterField(".status.primaryInstance")
		Expect(err).NotTo(HaveOccurred())
		attributes, err := monitorRoleAttributes(primary)
		Expect(err).NotTo(HaveOccurred())
		Expect(attributes).To(Equal("f|f|f|f|f|t|5|pg_read_all_settings"),
			"no superuser, create database, create role, replication or bypass-RLS; five connections; one membership")
		list := `envdir /run/etc/wal-e.d/env wal-g backup-list 2>/dev/null | grep -c '^base_'`
		Eventually(func(g Gomega) {
			out, err := runInPod(primary, list)
			g.Expect(err).NotTo(HaveOccurred())
			count, err := strconv.Atoi(out)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(count).To(BeNumerically(">=", 1))
		}, 6*time.Minute, pollEvery).Should(Succeed())

		By("archiving WAL as it is produced")
		_, err = psql(primary, "", "select pg_switch_wal()")
		Expect(err).NotTo(HaveOccurred())
		Eventually(func(g Gomega) {
			out, err := psql(primary, "", "select archived_count > 0 from pg_stat_archiver")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).To(Equal("t"))
		}, 2*time.Minute, pollEvery).Should(Succeed())

		By("reading the backups back, as the operator's status check does, without waiting for its schedule")
		_, err = inCluster("create", "job", "manual-status", "--from=cronjob/"+clusterName+"-backup-status")
		Expect(err).NotTo(HaveOccurred())
		Eventually(func(g Gomega) {
			count, err := clusterField(".status.backup.backupCount")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(count).NotTo(BeEmpty())
			n, err := strconv.Atoi(count)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(n).To(BeNumerically(">=", 1))
			for _, condition := range []string{"BackupHealthy", "WALArchivingHealthy"} {
				status, err := conditionStatus(condition)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(status).To(Equal("True"), condition)
			}
			degraded, err := conditionStatus("Degraded")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(degraded).To(Equal("False"))
			name, err := clusterField(".status.backup.lastBackupName")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(name).To(HavePrefix("base_"))
			size, err := clusterField(".status.backup.lastBackupSizeBytes")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(size).NotTo(BeEmpty())
		}, 5*time.Minute, pollEvery).Should(Succeed())

		By("showing the new settings in the pod's backup environment, running as non-root")
		out, err := runInPod(primary, `echo "$(id -u) $(cat /run/etc/wal-e.d/env/WALG_S3_PREFIX) `+
			`$(cat /run/etc/wal-e.d/env/WALG_LIBSODIUM_KEY_TRANSFORM)"`)
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(Equal("101 s3://" + backupBucket + "/" + clusterNamespace + "/" + clusterName + "/wal/17 hex"))
	})

	Context("restoring into a new cluster", Ordered, func() {
		const (
			restoredName = "restored"
			latestName   = "restored-latest"
			// The rows written by the first scenario, in the order they were written.
			rowBeforeBackup = "before-backup"
			rowBeforeTarget = "before-target"
			rowAfterTarget  = "after-target"
		)
		var targetTime string

		// restoreManifest is a one-instance cluster restored from the main cluster's
		// backups, to targetTime when it is not empty.
		restoreManifest := func(name, target string) string {
			targetLine := ""
			if target != "" {
				targetLine = fmt.Sprintf("      targetTime: %q\n", target)
			}
			// The first restored cluster asks for one of the source's roles, not
			// the other: the one it asks for gets a new password, and the one it
			// does not is switched off. The others ask for none, and the roles
			// they inherited are switched off too.
			rolesBlock := ""
			if name == restoredName {
				rolesBlock = "  roles:\n  - name: app_orders\n    database: orders\n"
			}
			return fmt.Sprintf(`apiVersion: db.youroperator.io/v1alpha1
kind: PostgresCluster
metadata:
  name: %[1]s
  namespace: %[2]s
spec:
  version: "17"
  instances: 1
  storage:
    size: 1Gi
  resources:
    limits:
      memory: 1Gi
%[7]s  bootstrap:
    recovery:
      sourceCluster: %[3]s
      destination:
        bucket: %[4]s
        endpoint: %[5]s
        forcePathStyle: true
        credentialsSecret: backup-s3
      encryption:
        keySecret: backup-key
%[6]s`, name, clusterNamespace, clusterName, backupBucket, objectStoreEndpoint, targetLine, rolesBlock)
		}
		waitAvailable := func(name string) {
			Eventually(func(g Gomega) {
				status, err := conditionOf(name, "Available")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(status).To(Equal("True"))
			}, clusterUpTimeout, pollEvery).Should(Succeed())
		}
		rowsOf := func(pod string) []string {
			out, err := psql(pod, "", "select v from pitr order by id")
			Expect(err).NotTo(HaveOccurred())
			return strings.Fields(out)
		}

		It("sets up a database with rows on both sides of a moment, and a backup before it", func() {
			primary, err := clusterField(".status.primaryInstance")
			Expect(err).NotTo(HaveOccurred())
			_, err = psql(primary, "", "create table pitr (id serial primary key, v text)")
			Expect(err).NotTo(HaveOccurred())
			_, err = psql(primary, "", "insert into pitr(v) values ('"+rowBeforeBackup+"')")
			Expect(err).NotTo(HaveOccurred())

			By("taking a base backup now, as the schedule would")
			_, err = runInPod(primary, `PGUSER=postgres envdir /run/etc/wal-e.d/env `+
				`/scripts/postgres_backup.sh /home/postgres/pgdata/pgroot/data`)
			Expect(err).NotTo(HaveOccurred())

			_, err = psql(primary, "", "insert into pitr(v) values ('"+rowBeforeTarget+"')")
			Expect(err).NotTo(HaveOccurred())
			time.Sleep(2 * time.Second)
			targetTime, err = psql(primary, "",
				`select to_char(date_trunc('second', now() at time zone 'utc'), 'YYYY-MM-DD"T"HH24:MI:SS"Z"')`)
			Expect(err).NotTo(HaveOccurred())
			time.Sleep(2 * time.Second)
			_, err = psql(primary, "", "insert into pitr(v) values ('"+rowAfterTarget+"')")
			Expect(err).NotTo(HaveOccurred())

			By("waiting for the WAL holding those rows to reach the bucket")
			closed, err := psql(primary, "", "select pg_walfile_name(pg_switch_wal() - 1)")
			Expect(err).NotTo(HaveOccurred())
			Eventually(func(g Gomega) {
				out, err := psql(primary, "", fmt.Sprintf("select last_archived_wal >= '%s' from pg_stat_archiver", closed))
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(Equal("t"))
			}, 2*time.Minute, pollEvery).Should(Succeed())
		})

		It("restores a new cluster to the chosen moment, leaving the source as it was", func() {
			Expect(targetTime).NotTo(BeEmpty())
			_, err := kubectlIn(restoreManifest(restoredName, targetTime), "apply", "-f", "-")
			Expect(err).NotTo(HaveOccurred())
			waitAvailable(restoredName)

			Expect(rowsOf(restoredName+"-0")).To(Equal([]string{rowBeforeBackup, rowBeforeTarget}),
				"data from before the target time is there, data after it is not")

			source, err := clusterField(".status.primaryInstance")
			Expect(err).NotTo(HaveOccurred())
			Expect(rowsOf(source)).To(Equal([]string{rowBeforeBackup, rowBeforeTarget, rowAfterTarget}))
		})

		It("gives the restored cluster its own passwords and its own place for backups", func() {
			original, err := inCluster("get", "secret", clusterName+"-credentials", "-o", "jsonpath={.data.password}")
			Expect(err).NotTo(HaveOccurred())
			restored, err := inCluster("get", "secret", restoredName+"-credentials", "-o", "jsonpath={.data.password}")
			Expect(err).NotTo(HaveOccurred())
			Expect(restored).NotTo(BeEmpty())
			Expect(restored).NotTo(Equal(original))

			By("logging in with the new password")
			out, err := psql(restoredName+"-0", restoredName, "select count(*) from pitr")
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(Equal("2"))
		})

		It("gives a role the source made a new password, and switches off the ones it was not asked to keep", func() {
			Eventually(func() (string, error) { return roleState(restoredName, "app_orders") }, 5*time.Minute, pollEvery).Should(Equal("Ready"))
			const sourceSecret, restoredSecret = clusterName + "-role-app-orders", restoredName + "-role-app-orders"
			before, err := secretValue(sourceSecret, "password")
			Expect(err).NotTo(HaveOccurred())
			after, err := secretValue(restoredSecret, "password")
			Expect(err).NotTo(HaveOccurred())
			Expect(after).NotTo(BeEmpty())
			Expect(after).NotTo(Equal(before), "a restored cluster does not share the source's passwords")

			By("finding the role's database and data, and logging in with the restored cluster's own password")
			out, err := asRole(restoredName+"-0", restoredName, restoredSecret, "orders", "select count(*) from orders_t")
			Expect(err).NotTo(HaveOccurred())
			Expect(strings.TrimSpace(out)).To(Equal("2"))

			By("refusing the source's password, which the restored data also held")
			out, err = asRole(restoredName+"-0", restoredName, sourceSecret, "orders", "select 1")
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(ContainSubstring("password authentication failed"))

			By("switching off the role it was not asked to keep")
			Eventually(func() (string, error) { return roleState(restoredName, "reporting") }, 3*time.Minute, pollEvery).Should(Equal("Disabled"))
			out, err = asRole(restoredName+"-0", restoredName, clusterName+"-role-reporting", "postgres", "select 1")
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(ContainSubstring("is not permitted to log in"))
		})

		It("restores to the latest moment when no time is given", func() {
			_, err := kubectlIn(restoreManifest(latestName, ""), "apply", "-f", "-")
			Expect(err).NotTo(HaveOccurred())
			waitAvailable(latestName)
			Expect(rowsOf(latestName + "-0")).To(Equal([]string{rowBeforeBackup, rowBeforeTarget, rowAfterTarget}))

			By("switching off the roles it inherited, though its spec lists none")
			for _, role := range []string{"app_orders", "reporting"} {
				Eventually(func() (string, error) { return roleState(latestName, role) }, 5*time.Minute, pollEvery).Should(Equal("Disabled"), role)
				out, err := asRole(latestName+"-0", latestName, clusterName+"-role-"+strings.ReplaceAll(role, "_", "-"), "postgres", "select 1")
				Expect(err).NotTo(HaveOccurred())
				Expect(out).To(ContainSubstring("is not permitted to log in"), role)
			}
		})

		It("says so in the conditions, and creates no pods, when the target is before the oldest backup", func() {
			const earlyName = "restored-early"
			_, err := kubectlIn(restoreManifest(earlyName, "2020-01-01T00:00:00Z"), "apply", "-f", "-")
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { _, _ = inCluster("delete", "postgrescluster", earlyName, "--wait=false") })

			field := func(path string) (string, error) {
				return inCluster("get", "postgrescluster", earlyName, "-o", "jsonpath={"+path+"}")
			}
			sourceReady := `.status.conditions[?(@.type=="RecoverySourceReady")]`
			Eventually(func(g Gomega) {
				reason, err := field(sourceReady + ".reason")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(reason).To(Equal("TargetBeforeOldestBackup"))
			}, 3*time.Minute, pollEvery).Should(Succeed())

			message, err := field(sourceReady + ".message")
			Expect(err).NotTo(HaveOccurred())
			Expect(message).To(ContainSubstring("targetTime 2020-01-01T00:00:00Z is earlier than the oldest base backup"))
			Expect(message).To(ContainSubstring("delete this cluster and create it again"))
			status, err := field(sourceReady + ".status")
			Expect(err).NotTo(HaveOccurred())
			Expect(status).To(Equal("False"))
			reason, err := field(`.status.conditions[?(@.type=="Progressing")].reason`)
			Expect(err).NotTo(HaveOccurred())
			Expect(reason).To(Equal("RecoverySourceProblem"))

			By("having created nothing that could crash-loop")
			pods, err := inCluster("get", "pods", "-o", "name",
				"-l", "app.kubernetes.io/instance="+earlyName+",app.kubernetes.io/component=postgres")
			Expect(err).NotTo(HaveOccurred())
			Expect(pods).To(BeEmpty())
			sts, err := inCluster("get", "statefulset", "-o", "name")
			Expect(err).NotTo(HaveOccurred())
			Expect(sts).NotTo(ContainSubstring(earlyName))
		})
	})

	It("proves that the newest backup restores, and reports how long it took", func() {
		By("turning restore testing on")
		_, err := inCluster("patch", "postgrescluster", clusterName, "--type=merge",
			"-p", `{"spec":{"backup":{"verification":{"enabled":true,"schedule":"0 4 * * 0"}}}}`)
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() error {
			_, err := inCluster("get", "cronjob", clusterName+"-restore-verify")
			return err
		}, 2*time.Minute, pollEvery).Should(Succeed())

		By("running one now, without waiting for the schedule")
		_, err = inCluster("create", "job", "manual-verify", "--from=cronjob/"+clusterName+"-restore-verify")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _, _ = inCluster("delete", "job", "manual-verify", "--wait=false") })

		Eventually(func(g Gomega) {
			status, err := conditionStatus("RestoreVerified")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(status).To(Equal("True"), "RestoreVerified")
			succeeded, err := clusterField(".status.backup.verification.lastSuccessTime")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(succeeded).NotTo(BeEmpty())
			degraded, err := conditionStatus("Degraded")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(degraded).To(Equal("False"))
		}, 15*time.Minute, pollEvery).Should(Succeed())

		duration, err := clusterField(".status.backup.verification.durationSeconds")
		Expect(err).NotTo(HaveOccurred())
		AddReportEntry("restore test", fmt.Sprintf("restored the newest backup in %s seconds", duration))
	})

	It("reports an unreachable bucket and falling-behind archiving, keeps accepting writes, and recovers", func() {
		primary, err := clusterField(".status.primaryInstance")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			// Leave the store running if the scenario fails part-way.
			_, _ = kubectlOut("-n", objectStoreNamespace, "scale", "deployment", "s3", "--replicas=1")
		})

		By("stopping the object store")
		stopObjectStoreService()

		By("writing enough WAL that archiving falls more than eight segments behind")
		for i := range 12 {
			_, err = psql(primary, "", fmt.Sprintf("insert into pitr(v) values ('outage-%d')", i))
			Expect(err).NotTo(HaveOccurred(), "the database must keep accepting writes while it cannot archive")
			_, err = psql(primary, "", "select pg_switch_wal()")
			Expect(err).NotTo(HaveOccurred())
		}

		By("running the status check, as the schedule would")
		_, err = inCluster("create", "job", "outage-status", "--from=cronjob/"+clusterName+"-backup-status")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _, _ = inCluster("delete", "job", "outage-status", "--wait=false") })
		Eventually(func(g Gomega) {
			archiving, err := conditionStatus("WALArchivingHealthy")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(archiving).To(Equal("False"), "WALArchivingHealthy")
			backups, err := conditionStatus("BackupHealthy")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(backups).To(Equal("False"), "BackupHealthy")
			degraded, err := conditionStatus("Degraded")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(degraded).To(Equal("True"))
		}, 8*time.Minute, pollEvery).Should(Succeed())

		reason, err := inCluster("get", "postgrescluster", clusterName, "-o",
			`jsonpath={.status.conditions[?(@.type=="WALArchivingHealthy")].reason}`)
		Expect(err).NotTo(HaveOccurred())
		Expect(reason).To(Equal("ArchivingBehind"))
		lag, err := clusterField(".status.backup.walArchiving.lagBytes")
		Expect(err).NotTo(HaveOccurred())
		lagBytes, err := strconv.Atoi(lag)
		Expect(err).NotTo(HaveOccurred())
		Expect(lagBytes).To(BeNumerically(">", 8*16<<20))

		By("bringing the store back")
		restartObjectStoreService()

		By("letting archiving catch up on its own")
		closed, err := psql(primary, "", "select pg_walfile_name(pg_switch_wal() - 1)")
		Expect(err).NotTo(HaveOccurred())
		Eventually(func(g Gomega) {
			out, err := psql(primary, "", fmt.Sprintf("select last_archived_wal >= '%s' from pg_stat_archiver", closed))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).To(Equal("t"))
		}, 6*time.Minute, pollEvery).Should(Succeed())

		By("waiting for the next scheduled base backup, since the store came back empty")
		list := `envdir /run/etc/wal-e.d/env wal-g backup-list 2>/dev/null | grep -c '^base_'`
		Eventually(func(g Gomega) {
			out, err := runInPod(primary, list)
			g.Expect(err).NotTo(HaveOccurred())
			count, err := strconv.Atoi(out)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(count).To(BeNumerically(">=", 1))
		}, 6*time.Minute, pollEvery).Should(Succeed())

		By("seeing the cluster healthy again")
		_, err = inCluster("create", "job", "recovered-status", "--from=cronjob/"+clusterName+"-backup-status")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _, _ = inCluster("delete", "job", "recovered-status", "--wait=false") })
		Eventually(func(g Gomega) {
			for _, condition := range []string{"WALArchivingHealthy", "BackupHealthy"} {
				status, err := conditionStatus(condition)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(status).To(Equal("True"), condition)
			}
		}, 8*time.Minute, pollEvery).Should(Succeed())
	})

	It("loads pgaudit through the pod template, audits what is asked, and takes it out again", func() {
		const probeTable = "audit_probe"
		podUIDs := func() string {
			out, err := inCluster("get", "pods", "-l", mainSelector+",app.kubernetes.io/component=postgres",
				"-o", "jsonpath={range .items[*]}{.metadata.uid} {end}")
			Expect(err).NotTo(HaveOccurred())
			fields := strings.Fields(out)
			slices.Sort(fields)
			return strings.Join(fields, " ")
		}
		show := func(pod, setting string) string {
			out, err := psql(pod, "", "show "+setting)
			ExpectWithOffset(1, err).NotTo(HaveOccurred())
			return out
		}
		pods := func() []string {
			out, err := inCluster("get", "pods", "-l", mainSelector+",app.kubernetes.io/component=postgres", "-o", "name")
			ExpectWithOffset(1, err).NotTo(HaveOccurred())
			names := strings.Fields(out)
			for i, name := range names {
				names[i] = strings.TrimPrefix(name, "pod/")
			}
			return names
		}
		patchCompliance := func(patch string) {
			_, err := inCluster("patch", "postgrescluster", clusterName, "--type=merge", "-p", patch)
			ExpectWithOffset(1, err).NotTo(HaveOccurred())
		}
		auditLog := func(pod string) string {
			out, err := runInPod(pod, `cat "$(ls -t /home/postgres/pgdata/pgroot/pg_log/*.csv | head -1)"`)
			ExpectWithOffset(1, err).NotTo(HaveOccurred())
			return out
		}
		DeferCleanup(func() {
			_, _ = inCluster("patch", "postgrescluster", clusterName, "--type=merge", "-p", `{"spec":{"compliance":null}}`)
		})

		primary, err := clusterField(".status.primaryInstance")
		Expect(err).NotTo(HaveOccurred())

		By("asking the image itself what it would load, so a newer Spilo cannot change the answer unnoticed")
		defaultList, err := runInPod(primary, `python3 -c "
import sys; sys.path.insert(0, '/scripts')
from spilo_commons import append_extensions
print(append_extensions('bg_mon,pg_stat_statements,pgextwlist,pg_auth_mon,set_user', 17.0))"`)
		Expect(err).NotTo(HaveOccurred())
		Expect(show(primary, "shared_preload_libraries")).To(Equal(defaultList), "before the change, Spilo's own list")

		image, err := clusterField(".status.image")
		Expect(err).NotTo(HaveOccurred())
		_, err = kubectlIn(writerManifest(image), "apply", "-f", "-")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _, _ = inCluster("delete", "pod", "writer", "--wait=false") })
		Eventually(func(g Gomega) {
			log, err := inCluster("logs", "writer")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(parseWriterLog(log).acknowledged).To(BeNumerically(">=", 20))
		}, 3*time.Minute, pollEvery).Should(Succeed())

		before := podUIDs()

		By("turning the hipaa standard on, which replaces every pod")
		enable := fmt.Sprintf(`{"spec":{"compliance":{"standard":"hipaa","auditLogs":{"destination":`+
			`{"bucket":%q,"endpoint":%q,"forcePathStyle":true,"credentialsSecret":"backup-s3"}}}}}`,
			backupBucket, objectStoreEndpoint)
		patchCompliance(enable)
		Eventually(func(g Gomega) {
			g.Expect(podUIDs()).NotTo(Equal(before))
			for uid := range strings.FieldsSeq(before) {
				g.Expect(podUIDs()).NotTo(ContainSubstring(uid), "every pod was replaced")
			}
			g.Expect(strings.Fields(podUIDs())).To(HaveLen(instances))
			expectHealthy(g)
		}, clusterUpTimeout+5*time.Minute, pollEvery).Should(Succeed())

		By("loading pgaudit on every member, after Spilo's own list, and nothing it already loaded left out")
		for _, pod := range pods() {
			Expect(show(pod, "shared_preload_libraries")).To(Equal(defaultList+",pgaudit"), pod)
		}

		primary, err = clusterField(".status.primaryInstance")
		Expect(err).NotTo(HaveOccurred())
		By("applying the preset's settings without a restart")
		Eventually(func(g Gomega) {
			g.Expect(show(primary, "pgaudit.log")).To(Equal("read,write,ddl,role"))
			g.Expect(show(primary, "pgaudit.log_parameter")).To(Equal("off"))
			g.Expect(show(primary, "log_statement")).To(Equal("none"))
		}, 3*time.Minute, pollEvery).Should(Succeed())

		By("logging a read, a write, a DDL statement and a role change, and not writing DDL twice")
		for _, sql := range []string{
			"create table " + probeTable + " (secret text)",
			"insert into " + probeTable + " values ('plain-literal')",
			"select * from " + probeTable,
			"create role audit_probe_role",
		} {
			_, err = psql(primary, "", sql)
			Expect(err).NotTo(HaveOccurred())
		}
		Eventually(func(g Gomega) {
			log := auditLog(primary)
			for _, want := range []string{"DDL,CREATE TABLE,", "WRITE,INSERT,", "READ,SELECT,", "ROLE,CREATE ROLE,"} {
				g.Expect(log).To(ContainSubstring("AUDIT: SESSION,"), want)
				g.Expect(log).To(MatchRegexp(regexp.QuoteMeta(want)+`[^\n]*`+probeTable), want)
			}
			g.Expect(log).NotTo(ContainSubstring("statement: create table "+probeTable),
				"log_statement is off, so pgaudit's line is the only record of the statement")
		}, time.Minute, pollEvery).Should(Succeed())

		By("not writing the value bound to a statement's parameter, since that would be clear text")
		_, err = runInPod(primary, `PGPASSWORD="$PGPASSWORD_SUPERUSER" psql -U postgres -d postgres -tAX `+
			`-c "prepare audit_probe_stmt(text) as insert into `+probeTable+` values (\$1)" `+
			`-c "execute audit_probe_stmt('bound-parameter-value')"`)
		Expect(err).NotTo(HaveOccurred())
		Eventually(func(g Gomega) {
			log := auditLog(primary)
			g.Expect(log).To(ContainSubstring("prepare audit_probe_stmt"))
			g.Expect(log).NotTo(ContainSubstring("bound-parameter-value"))
		}, time.Minute, pollEvery).Should(Succeed())

		By("running a shipper beside every database container, which cannot unready the pod")
		for _, pod := range pods() {
			names, err := inCluster("get", "pod", pod, "-o", "jsonpath={.spec.containers[*].name}")
			Expect(err).NotTo(HaveOccurred())
			Expect(strings.Fields(names)).To(Equal([]string{postgresContainer, "audit-shipper"}), pod)
		}

		By("shipping the audit log to the bucket, byte for byte, from the primary and from a replica")
		replica := ""
		for _, pod := range pods() {
			if pod != primary {
				replica = pod
				break
			}
		}
		for _, pod := range []string{primary, replica} {
			// The shipper has been running since the pod started; run it once
			// more now, so the statements above are certainly in the bucket.
			_, err = inCluster("exec", pod, "-c", "audit-shipper", "--", "python3", "/scripts/audit_shipper.py")
			Expect(err).NotTo(HaveOccurred(), pod)
			verified := verifyShippedAuditLog(pod, "AUDIT: SESSION")
			Expect(verified.Series).To(BeNumerically(">=", 1), pod)
			Expect(verified.Bytes).To(BeNumerically(">", 0), pod)
			Expect(verified.Contiguous).To(BeTrue(), "%s: the chunks of a series are contiguous byte ranges", pod)
			Expect(verified.Matches).To(BeTrue(), "%s: the chunks, joined, are the start of the log file", pod)
			Expect(verified.Markers).To(HaveKeyWithValue("AUDIT: SESSION", true), pod)
		}
		probed := verifyShippedAuditLog(primary, probeTable,
			"DDL,CREATE TABLE,", "WRITE,INSERT,", "READ,SELECT,", "ROLE,CREATE ROLE,")
		Expect(probed.Markers).To(HaveLen(5))
		for marker, found := range probed.Markers {
			Expect(found).To(BeTrue(), "the audit trail in the bucket has %q", marker)
		}
		Expect(probed.Series).To(BeNumerically(">=", 1))

		By("isolating the database pods: the operator, replication and the cluster's own jobs still work")
		policy, err := inCluster("get", "networkpolicy", clusterName, "-o", "jsonpath={range .spec.ingress[*]}x{end}")
		Expect(err).NotTo(HaveOccurred())
		Expect(policy).To(Equal("xxxx"), "clients, the cluster's own pods, its jobs, and the operator")
		Expect(conditionStatus("ReplicationHealthy")).To(Equal("True"), "replication runs through the policy")
		// The audit settings above reached the primary through Patroni's REST
		// API from the operator, and the writer is a client that declared itself.
		_, err = inCluster("create", "job", "status-under-policy", "--from=cronjob/"+clusterName+"-backup-status")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _, _ = inCluster("delete", "job", "status-under-policy", "--wait=false") })
		Eventually(func() (string, error) {
			return inCluster("get", "job", "status-under-policy", "-o", "jsonpath={.status.succeeded}")
		}, 5*time.Minute, pollEvery).Should(Equal("1"), "the cluster's own job reaches the database through the policy")

		By("connecting from the operator on port 5432 under the policy, as the read-only role, and giving the jobs nothing more")
		operatorPorts, err := inCluster("get", "networkpolicy", clusterName, "-o",
			`jsonpath={.spec.ingress[?(@.from[0].namespaceSelector)].ports[*].port}`)
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.Fields(operatorPorts)).To(ConsistOf("5432", "8008"), "the rule that lets the operator in opens both ports")
		operatorIP, err := kubectlOut("-n", namespace, "get", "pods", "-l", "control-plane=controller-manager",
			"-o", "jsonpath={.items[0].status.podIP}")
		Expect(err).NotTo(HaveOccurred())
		Expect(operatorIP).NotTo(BeEmpty())
		Eventually(func(g Gomega) {
			g.Expect(clusterField(".status.accessControl.monitorRole.state")).To(Equal("Ready"))
			// The server's own record of who connected, from where: the operator
			// logs in as the monitor role every few minutes, from its pod's address.
			found := false
			for _, pod := range pods() {
				log := auditLog(pod)
				for line := range strings.SplitSeq(log, "\n") {
					if strings.Contains(line, "connection authorized: user="+monitorRole) &&
						strings.Contains(line, `"`+operatorIP+":") && strings.Contains(line, "application_name=postgres-operator") {
						found = true
					}
				}
			}
			g.Expect(found).To(BeTrue(), "the server log shows the operator, at %s, connecting as %s", operatorIP, monitorRole)
		}, 8*time.Minute, pollEvery).Should(Succeed())
		for _, job := range []string{"backup-status", "restore-verify", "compliance-check", "network-probe"} {
			secrets, err := cronJobSecrets(clusterName + "-" + job)
			Expect(err).NotTo(HaveOccurred(), job)
			Expect(secrets).NotTo(ContainSubstring(clusterName+"-credentials"), job+" holds no superuser credentials")
		}
		primaryIP, err := inCluster("get", "pod", primary, "-o", "jsonpath={.status.podIP}")
		Expect(err).NotTo(HaveOccurred())
		for _, probe := range []string{"select * from pg_authid", "select pg_read_file('/etc/passwd')", "select set_user_u('postgres')"} {
			out, err := asMonitor(primary, primaryIP, probe)
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(ContainSubstring("permission denied"), probe)
		}
		out, err := asMonitor(primary, primaryIP, "select pgoperator.non_scram_roles()")
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.TrimSpace(out)).To(Equal("0"), "the one thing it may ask of the password table")
		service := clusterName + "." + clusterNamespace + ".svc.cluster.local"
		client := map[string]string{clientLabel: clientLabelValue}
		_, err = kubectlOut("create", "namespace", "e2e-other")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _, _ = kubectlOut("delete", "namespace", "e2e-other", "--wait=false") })
		Expect(canConnect(clusterNamespace, "probe-client-sql", image, client, service, 5432)).To(BeTrue(),
			"a pod that declares itself a client reaches PostgreSQL")
		Expect(canConnect(clusterNamespace, "probe-client-patroni", image, client, primaryIP, 8008)).To(BeFalse(),
			"but not Patroni's REST API")
		Expect(canConnect(clusterNamespace, "probe-stranger-sql", image, nil, service, 5432)).To(BeFalse(),
			"a pod that does not is refused")
		Expect(canConnect(clusterNamespace, "probe-stranger-patroni", image, nil, primaryIP, 8008)).To(BeFalse())
		Expect(canConnect("e2e-other", "probe-other-sql", image, client, service, 5432)).To(BeFalse(),
			"the client label counts only in the cluster's own namespace")

		By("running a restore test: the one an earlier scenario ran was cleaned up with its job, and the schedule is weekly")
		_, err = inCluster("create", "job", "restore-for-compliance", "--from=cronjob/"+clusterName+"-restore-verify")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _, _ = inCluster("delete", "job", "restore-for-compliance", "--wait=false") })
		Eventually(func() (string, error) {
			return inCluster("get", "job", "restore-for-compliance", "-o", "jsonpath={.status.succeeded}")
		}, 8*time.Minute, pollEvery).Should(Equal("1"))

		By("reading the controls back from the running cluster: all of them met")
		runComplianceJobs("one")
		var state complianceState
		Eventually(func(g Gomega) {
			state, err = readCompliance()
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(state.LastChecked).NotTo(BeEmpty(), "the jobs have reported")
			g.Expect(state.ControlCount).To(Equal(10))
			for id, c := range state.Controls {
				g.Expect(c.Status).To(Equal("Met"), "%s: %s", id, c.Evidence)
			}
			g.Expect(state.MetStatus).To(Equal("True"), state.MetMessage)
		}, 3*time.Minute, pollEvery).Should(Succeed())
		Expect(state.Controls["network-isolation"].Evidence).To(ContainSubstring("was refused"),
			"the probe, a stranger to the policy, really was turned away")
		Expect(state.Controls["audit-log-delivery"].Evidence).To(ContainSubstring("3 pod(s) each shipped"))
		Expect(state.Controls["audit-log-retention"].Evidence).To(ContainSubstring("not protected from deletion"),
			"the store has no object lock, and says so")

		By("noticing a role whose password is not stored as scram")
		_, err = psql(primary, "", "set password_encryption = 'md5'; create role e2e_weak login password 'x'")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _, _ = psql(primary, "", "drop role if exists e2e_weak") })
		runComplianceJobs("weak")
		Eventually(func(g Gomega) {
			state, err = readCompliance()
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(state.Controls["authentication"].Status).To(Equal("NotMet"))
		}, 3*time.Minute, pollEvery).Should(Succeed())
		Expect(state.Controls["authentication"].Evidence).To(
			ContainSubstring("1 role(s) have a password that is not stored as scram-sha-256"))
		Expect(state.MetStatus).To(Equal("False"))
		Expect(state.MetMessage).To(ContainSubstring("authentication"))
		Expect(state.Controls["audit-logging"].Status).To(Equal("Met"), "and only what is wrong is reported")

		By("seeing it fixed")
		_, err = psql(primary, "", "drop role e2e_weak")
		Expect(err).NotTo(HaveOccurred())
		runComplianceJobs("fixed")
		Eventually(func(g Gomega) {
			state, err = readCompliance()
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(state.Controls["authentication"].Status).To(Equal("Met"))
			g.Expect(state.MetStatus).To(Equal("True"), state.MetMessage)
		}, 3*time.Minute, pollEvery).Should(Succeed())

		By("noticing a lifecycle rule that would expire the audit logs too soon")
		const putRule = `
import boto3, os
from botocore.config import Config
c = boto3.client("s3", endpoint_url=os.environ["AUDIT_ENDPOINT"] or None, region_name="us-east-1",
                 config=Config(s3={"addressing_style": "path"}))
c.put_bucket_lifecycle_configuration(Bucket=os.environ["AUDIT_BUCKET"], LifecycleConfiguration={"Rules": [
    {"ID": "tidy-up", "Status": "Enabled", "Filter": {"Prefix": "audit/"}, "Expiration": {"Days": 30}}]})
`
		const removeRule = `
import boto3, os
from botocore.config import Config
c = boto3.client("s3", endpoint_url=os.environ["AUDIT_ENDPOINT"] or None, region_name="us-east-1",
                 config=Config(s3={"addressing_style": "path"}))
c.delete_bucket_lifecycle(Bucket=os.environ["AUDIT_BUCKET"])
`
		onShipper := func(script string) error {
			_, err := inCluster("exec", primary, "-c", "audit-shipper", "--", "python3", "-c", script)
			return err
		}
		Expect(onShipper(putRule)).To(Succeed())
		DeferCleanup(func() { _ = onShipper(removeRule) })
		runComplianceJobs("early")
		Eventually(func(g Gomega) {
			state, err = readCompliance()
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(state.Controls["audit-log-retention"].Status).To(Equal("NotMet"))
		}, 3*time.Minute, pollEvery).Should(Succeed())
		Expect(state.Controls["audit-log-retention"].Evidence).To(
			And(ContainSubstring(`"tidy-up"`), ContainSubstring("30 days")))
		Expect(onShipper(removeRule)).To(Succeed())

		By("swapping one standard for another by reloading, without replacing a pod")
		during := podUIDs()
		patchCompliance(`{"spec":{"compliance":{"standard":"soc2"}}}`)
		Eventually(func() string { return show(primary, "pgaudit.log") }, 3*time.Minute, pollEvery).
			Should(Equal("read,write,ddl"))
		Expect(podUIDs()).To(Equal(during), "a change of standard is only a reload")

		By("turning it off, which replaces every pod again and puts Spilo's settings back")
		patchCompliance(`{"spec":{"compliance":null}}`)
		Eventually(func(g Gomega) {
			for uid := range strings.FieldsSeq(during) {
				g.Expect(podUIDs()).NotTo(ContainSubstring(uid), "every pod was replaced")
			}
			g.Expect(strings.Fields(podUIDs())).To(HaveLen(instances))
			expectHealthy(g)
		}, clusterUpTimeout+5*time.Minute, pollEvery).Should(Succeed())
		primary, err = clusterField(".status.primaryInstance")
		Expect(err).NotTo(HaveOccurred())
		for _, pod := range pods() {
			Expect(show(pod, "shared_preload_libraries")).To(Equal(defaultList), pod)
			names, err := inCluster("get", "pod", pod, "-o", "jsonpath={.spec.containers[*].name}")
			Expect(err).NotTo(HaveOccurred())
			Expect(strings.Fields(names)).To(Equal([]string{postgresContainer}), "%s: the shipper goes with the settings", pod)
		}
		_, err = inCluster("get", "networkpolicy", clusterName)
		Expect(err).To(HaveOccurred(), "the policy goes with the settings")
		Expect(canConnect(clusterNamespace, "probe-after-sql", image, nil, service, 5432)).To(BeTrue(),
			"and an ordinary pod can reach PostgreSQL again")
		scripts, err := inCluster("get", "configmap", "-o", "name")
		Expect(err).NotTo(HaveOccurred())
		Expect(scripts).NotTo(ContainSubstring(clusterName+"-compliance-scripts"), "nor do the scripts stay behind")
		cronjobs, err := inCluster("get", "cronjob", "-o", "name")
		Expect(err).NotTo(HaveOccurred())
		Expect(cronjobs).NotTo(ContainSubstring("-compliance-check"), "the compliance jobs go with the settings")
		Expect(cronjobs).NotTo(ContainSubstring("-network-probe"))
		state, err = readCompliance()
		Expect(err).NotTo(HaveOccurred())
		Expect(state.ControlCount).To(BeZero(), "what the standard reported is cleared")
		Expect(state.MetStatus).To(BeEmpty(), "and so is the condition")
		Eventually(func(g Gomega) {
			config, err := runInPod(primary, `curl -s localhost:8008/config`)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(config).NotTo(ContainSubstring("pgaudit"), "the settings the operator wrote are deleted")
			g.Expect(show(primary, "log_statement")).To(Equal("ddl"))
		}, 3*time.Minute, pollEvery).Should(Succeed())

		By("checking what the application saw across both replacements")
		log, err := inCluster("logs", "writer")
		Expect(err).NotTo(HaveOccurred())
		result := parseWriterLog(log)
		AddReportEntry("enabling and disabling compliance as seen by a writer",
			fmt.Sprintf("%d acknowledged, %d failed, longest gap %s", result.acknowledged, result.failed, result.longestGap))
		Expect(result.longestGap).To(BeNumerically("<", 15*time.Second))
		stored, err := psql(primary, "", "select count(*) from e2e_rows where v = 'writer'")
		Expect(err).NotTo(HaveOccurred())
		count, err := strconv.Atoi(stored)
		Expect(err).NotTo(HaveOccurred())
		Expect(count).To(BeNumerically(">=", result.acknowledged), "every acknowledged write is still there")
	})

	It("fails over when the primary is deleted, keeping the data and the service", func() {
		old := ""
		Eventually(func(g Gomega) { old = currentPrimary(g) }).Should(Succeed())

		_, err := inCluster("delete", "pod", old, "--wait=false")
		Expect(err).NotTo(HaveOccurred())

		var fresh string
		Eventually(func(g Gomega) {
			fresh = currentPrimary(g)
			g.Expect(fresh).NotTo(Equal(old))
		}, failoverWait, pollEvery).Should(Succeed())

		Eventually(func(g Gomega) {
			writeThroughService(g, fresh, "after-delete")
		}, failoverWait, pollEvery).Should(Succeed())
		Eventually(expectHealthy, clusterUpTimeout, pollEvery).Should(Succeed())
		out, err := rowCount(fresh, "")
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(Equal("2"), "no committed row may be lost")
	})

	It("fails over when the primary hangs, which a pod deletion does not exercise", func() {
		old := ""
		Eventually(func(g Gomega) { old = currentPrimary(g) }).Should(Succeed())

		// The bracketed pattern matches the words patroni and postgres but not
		// its own text, so the shell running this script (whose command line
		// contains the pattern) is not stopped along with them.
		signalAll := func(signal string) string {
			return `for p in $(pgrep -f "[p]atroni|[p]ostgres"); do ` +
				`if [ "$p" != 1 ] && [ "$p" != "$$" ]; then kill -` + signal + ` "$p"; fi; done`
		}
		freeze, thaw := signalAll("STOP"), signalAll("CONT")
		DeferCleanup(func() { _, _ = runInPod(old, thaw) })
		_, err := runInPod(old, freeze)
		Expect(err).NotTo(HaveOccurred())

		var fresh string
		Eventually(func(g Gomega) {
			fresh = currentPrimary(g)
			g.Expect(fresh).NotTo(Equal(old))
			available, err := conditionStatus("Available")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(available).To(Equal("True"))
		}, failoverWait, pollEvery).Should(Succeed())

		Eventually(func(g Gomega) {
			writeThroughService(g, fresh, "after-hang")
		}, failoverWait, pollEvery).Should(Succeed())

		By("taking the old primary back as a replica once it wakes")
		_, err = runInPod(old, thaw)
		Expect(err).NotTo(HaveOccurred())
		Eventually(expectHealthy, clusterUpTimeout, pollEvery).Should(Succeed())
		Expect(podsOf(mainRole("primary"))).To(ConsistOf(fresh))
	})

	It("fails over with the operator stopped, then catches its status up when it returns", func() {
		old := ""
		Eventually(func(g Gomega) { old = currentPrimary(g) }).Should(Succeed())

		_, err := kubectlOut("scale", "deployment", operatorDeployment, "-n", namespace, "--replicas=0")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			_, _ = kubectlOut("scale", "deployment", operatorDeployment, "-n", namespace, "--replicas=1")
		})
		Eventually(func() (string, error) {
			return kubectlOut("get", "pods", "-n", namespace, "-l", "control-plane=controller-manager", "-o", "name")
		}, time.Minute, pollEvery).Should(BeEmpty())

		_, err = inCluster("delete", "pod", old, "--wait=false")
		Expect(err).NotTo(HaveOccurred())

		var fresh string
		Eventually(func(g Gomega) {
			fresh = leaderRecord(g)
			g.Expect(fresh).NotTo(BeEmpty())
			g.Expect(fresh).NotTo(Equal(old))
		}, failoverWait, pollEvery).Should(Succeed(), "Patroni must elect a leader on its own")
		Eventually(func(g Gomega) {
			writeThroughService(g, fresh, "operator-down")
		}, failoverWait, pollEvery).Should(Succeed())

		_, err = kubectlOut("scale", "deployment", operatorDeployment, "-n", namespace, "--replicas=1")
		Expect(err).NotTo(HaveOccurred())
		Eventually(func(g Gomega) {
			g.Expect(currentPrimary(g)).To(Equal(fresh))
			expectHealthy(g)
		}, clusterUpTimeout, pollEvery).Should(Succeed())
	})

	It("recreates a deleted cluster on its old volumes with the same passwords and data", func() {
		password, err := inCluster("get", "secret", clusterName+"-credentials", "-o", "jsonpath={.data.password}")
		Expect(err).NotTo(HaveOccurred())
		primary, err := clusterField(".status.primaryInstance")
		Expect(err).NotTo(HaveOccurred())
		rows, err := rowCount(primary, "")
		Expect(err).NotTo(HaveOccurred())

		_, err = inCluster("delete", "postgrescluster", clusterName)
		Expect(err).NotTo(HaveOccurred())

		By("removing the pods and what Patroni left behind, but keeping the volumes and the Secret")
		Eventually(func() (string, error) {
			return inCluster("get", "pods", "-l", mainSelector, "-o", "name")
		}, 3*time.Minute, pollEvery).Should(BeEmpty())
		leftovers := "app.kubernetes.io/instance=" + clusterName + ",app.kubernetes.io/component=postgres"
		Eventually(func() (string, error) {
			return inCluster("get", "endpoints,configmaps", "-l", leftovers, "-o", "name")
		}, time.Minute, pollEvery).Should(BeEmpty())
		volumes, err := inCluster("get", "pvc", "-l", mainSelector, "-o", "name")
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.Fields(volumes)).To(HaveLen(instances))
		_, err = inCluster("get", "secret", clusterName+"-credentials")
		Expect(err).NotTo(HaveOccurred())

		By("creating a cluster of the same name")
		_, err = kubectlIn(clusterManifest(), "apply", "-f", "-")
		Expect(err).NotTo(HaveOccurred())
		Eventually(expectHealthy, clusterUpTimeout, pollEvery).Should(Succeed())

		again, err := inCluster("get", "secret", clusterName+"-credentials", "-o", "jsonpath={.data.password}")
		Expect(err).NotTo(HaveOccurred())
		Expect(again).To(Equal(password))
		primary, err = clusterField(".status.primaryInstance")
		Expect(err).NotTo(HaveOccurred())
		out, err := rowCount(primary, "")
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(Equal(rows), "the old data must still be there")
	})
}
