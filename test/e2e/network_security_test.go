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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const (
	// dnsMetricsPort is a port every cluster's DNS answers on, which the policy
	// has no reason to allow.
	dnsMetricsPort = 9153

	reachCheck = `python3 - "$1" "$2" <<'PY'
import socket, sys
s = socket.socket()
s.settimeout(5)
try:
    s.connect((sys.argv[1], int(sys.argv[2])))
    print("OPEN")
except Exception:
    print("CLOSED")
PY`
)

// egressSecurity limits what the cluster's pods may reach to what the operator
// needs and to the object store, wherever the port is given.
func egressSecurity(port int) string {
	return fmt.Sprintf(`{"spec":{"security":{"networkPolicy":{"egress":{"mode":"Restricted","allow":[`+
		`{"to":[{"namespaceSelector":{"matchLabels":{"kubernetes.io/metadata.name":%q}},"podSelector":{"matchLabels":{"app":"s3"}}}],`+
		`"ports":[{"protocol":"TCP","port":%d}]}]}}}}}`, objectStoreNamespace, port)
}

// networkPolicyScenario limits what the main cluster's pods may reach, and
// checks what that is for: the database, its peers, the Kubernetes API and the
// object store go on working, anything else is refused, the rule that lets the
// object store through is what lets it through, and taking the limit away puts
// everything back without a pod being replaced.
func networkPolicyScenario(healthy func(Gomega), podUIDs func() string) {
	DeferCleanup(func() {
		_, _ = inCluster("patch", "postgrescluster", clusterName, "--type=merge", "-p", `{"spec":{"security":null}}`)
	})
	reaches := func(pod, host string, port int) bool {
		out, err := runInPod(pod, reachCheck, host, fmt.Sprint(port))
		ExpectWithOffset(2, err).NotTo(HaveOccurred())
		return strings.TrimSpace(out) == "OPEN"
	}
	primaryNow := func() string {
		primary, err := clusterField(".status.primaryInstance")
		ExpectWithOffset(2, err).NotTo(HaveOccurred())
		return primary
	}
	policyTypes := func() string {
		out, err := inCluster("get", "networkpolicy", clusterName, "-o", "jsonpath={.spec.policyTypes}")
		ExpectWithOffset(2, err).NotTo(HaveOccurred())
		return out
	}
	archivedCount := func(pod string) int {
		out, err := psql(pod, "", "select archived_count from pg_stat_archiver")
		ExpectWithOffset(2, err).NotTo(HaveOccurred())
		var n int
		_, err = fmt.Sscan(strings.TrimSpace(out), &n)
		ExpectWithOffset(2, err).NotTo(HaveOccurred(), out)
		return n
	}
	dnsIP, err := kubectlOut("-n", "kube-system", "get", "service", "kube-dns", "-o", "jsonpath={.spec.clusterIP}")
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	storeHost := "s3." + objectStoreNamespace + ".svc.cluster.local"

	primary := primaryNow()
	ExpectWithOffset(1, reaches(primary, dnsIP, dnsMetricsPort)).To(BeTrue(), "before the limit, a pod can reach whatever it likes")

	By("refusing a limit that leaves no way to the object store")
	out, err := inCluster("patch", "postgrescluster", clusterName, "--type=merge", "-p",
		`{"spec":{"security":{"networkPolicy":{"egress":{"mode":"Restricted"}}}}}`)
	ExpectWithOffset(1, err).To(HaveOccurred(), out)
	ExpectWithOffset(1, out).To(ContainSubstring("a network policy cannot name"))

	By("limiting what the pods may reach: the policy takes effect at once, and no pod is replaced")
	before := podUIDs()
	_, err = inCluster("patch", "postgrescluster", clusterName, "--type=merge", "-p", egressSecurity(8333))
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	EventuallyWithOffset(1, policyTypes, 2*time.Minute, pollEvery).Should(ContainSubstring("Egress"))
	EventuallyWithOffset(1, func() bool { return reaches(primary, dnsIP, dnsMetricsPort) }, 2*time.Minute, pollEvery).
		Should(BeFalse(), "something the policy does not name is refused")
	ExpectWithOffset(1, podUIDs()).To(Equal(before))

	By("seeing what the cluster needs still reachable: DNS, the object store, the other pods and the Kubernetes API")
	resolved, err := runInPod(primary, `getent hosts "$1"`, storeHost)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	ExpectWithOffset(1, resolved).NotTo(BeEmpty())
	ExpectWithOffset(1, reaches(primary, storeHost, 8333)).To(BeTrue(), "the object store is listed")
	replicaIP, err := inCluster("get", "pods", "-l", mainRole("replica"), "-o", "jsonpath={.items[0].status.podIP}")
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	ExpectWithOffset(1, reaches(primary, replicaIP, 5432)).To(BeTrue(), "its own pods")
	ExpectWithOffset(1, reaches(primary, replicaIP, 8008)).To(BeTrue())
	kubernetesHost, err := kubectlOut("get", "endpoints", "kubernetes", "-o", "jsonpath={.subsets[0].addresses[0].ip}:{.subsets[0].ports[0].port}")
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	address, port, _ := strings.Cut(kubernetesHost, ":")
	var apiPort int
	_, err = fmt.Sscan(port, &apiPort)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), kubernetesHost)
	ExpectWithOffset(1, reaches(primary, address, apiPort)).To(BeTrue(), "the Kubernetes API, which Patroni keeps its state in")

	By("seeing WAL archiving go on through the rule that allows it, and the cluster stay healthy")
	archived := archivedCount(primary)
	_, err = psql(primary, "", "select pg_switch_wal()")
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	EventuallyWithOffset(1, func() int { return archivedCount(primary) }, 3*time.Minute, pollEvery).Should(BeNumerically(">", archived))
	EventuallyWithOffset(1, healthy, 2*time.Minute, pollEvery).Should(Succeed())
	ExpectWithOffset(1, primaryNow()).To(Equal(primary), "no switchover")

	By("pointing the allowance at a port nothing serves: the object store is no longer reachable, so the rule is what let it through")
	_, err = inCluster("patch", "postgrescluster", clusterName, "--type=merge", "-p", egressSecurity(9999))
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	EventuallyWithOffset(1, func() bool { return reaches(primary, storeHost, 8333) }, 2*time.Minute, pollEvery).Should(BeFalse())
	_, err = inCluster("patch", "postgrescluster", clusterName, "--type=merge", "-p", egressSecurity(8333))
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	EventuallyWithOffset(1, func() bool { return reaches(primary, storeHost, 8333) }, 2*time.Minute, pollEvery).Should(BeTrue())

	By("taking the limit away: the rule goes, everything is reachable again, and still no pod is replaced")
	_, err = inCluster("patch", "postgrescluster", clusterName, "--type=merge", "-p", `{"spec":{"security":null}}`)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	EventuallyWithOffset(1, policyTypes, 2*time.Minute, pollEvery).ShouldNot(ContainSubstring("Egress"))
	EventuallyWithOffset(1, func() bool { return reaches(primary, dnsIP, dnsMetricsPort) }, 2*time.Minute, pollEvery).Should(BeTrue())
	ExpectWithOffset(1, podUIDs()).To(Equal(before))
	EventuallyWithOffset(1, healthy, 2*time.Minute, pollEvery).Should(Succeed())
}
