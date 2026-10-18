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

	It("brings up three instances with one primary and two streaming replicas", func() {
		Eventually(func(g Gomega) {
			_, err := kubectlIn(clusterManifest(), "apply", "-f", "-")
			g.Expect(err).NotTo(HaveOccurred())
		}, time.Minute, pollEvery).Should(Succeed())

		Eventually(expectHealthy, clusterUpTimeout, pollEvery).Should(Succeed())

		primary := ""
		Eventually(func(g Gomega) { primary = currentPrimary(g) }, time.Minute, pollEvery).Should(Succeed())

		By("labelling exactly that pod as the primary, and the rest as replicas")
		Expect(podsOf("db.youroperator.io/role=primary")).To(ConsistOf(primary))
		Expect(podsOf("db.youroperator.io/role=replica")).To(HaveLen(instances - 1))

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

		for _, replica := range podsOf("db.youroperator.io/role=replica") {
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

		By("finding a base backup in the bucket, read back with the key")
		primary, err := clusterField(".status.primaryInstance")
		Expect(err).NotTo(HaveOccurred())
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

		By("showing the new settings in the pod's backup environment, running as non-root")
		out, err := runInPod(primary, `echo "$(id -u) $(cat /run/etc/wal-e.d/env/WALG_S3_PREFIX) `+
			`$(cat /run/etc/wal-e.d/env/WALG_LIBSODIUM_KEY_TRANSFORM)"`)
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(Equal("101 s3://" + backupBucket + "/" + clusterNamespace + "/" + clusterName + "/wal/17 hex"))
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
		Expect(podsOf("db.youroperator.io/role=primary")).To(ConsistOf(fresh))
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
			return inCluster("get", "pods", "-o", "name")
		}, 3*time.Minute, pollEvery).Should(BeEmpty())
		leftovers := "app.kubernetes.io/instance=" + clusterName + ",app.kubernetes.io/component=postgres"
		Eventually(func() (string, error) {
			return inCluster("get", "endpoints,configmaps", "-l", leftovers, "-o", "name")
		}, time.Minute, pollEvery).Should(BeEmpty())
		volumes, err := inCluster("get", "pvc", "-o", "name")
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
