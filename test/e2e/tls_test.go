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
	tlsSecret    = clusterName + "-tls"
	tlsCASecret  = clusterName + "-ca"
	tlsServer    = clusterName + "." + clusterNamespace + ".svc"
	tlsFileCheck = `python3 - <<'PY'
import hashlib, socket, ssl, struct, subprocess
sock = socket.create_connection((subprocess.check_output(["hostname", "-i"]).decode().split()[0], 5432), timeout=5)
sock.sendall(struct.pack("!II", 8, 80877103))
assert sock.recv(1) == b"S"
ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
ctx.check_hostname = False
ctx.verify_mode = ssl.CERT_NONE
served = hashlib.sha256(ctx.wrap_socket(sock).getpeercert(binary_form=True)).hexdigest()[:16]
first = open("/tls/tls.crt").read().split("-----END CERTIFICATE-----")[0] + "-----END CERTIFICATE-----"
print(served, hashlib.sha256(ssl.PEM_cert_to_DER_cert(first)).hexdigest()[:16])
PY`
)

// certificatesManifest is a CA and a server certificate for the cluster, made by
// cert-manager, with the names the operator asks for.
func certificatesManifest() string {
	return fmt.Sprintf(`
apiVersion: cert-manager.io/v1
kind: Issuer
metadata: {name: e2e-selfsigned, namespace: %[1]s}
spec: {selfSigned: {}}
---
apiVersion: cert-manager.io/v1
kind: Certificate
metadata: {name: %[2]s, namespace: %[1]s}
spec:
  isCA: true
  commonName: e2e-ca
  secretName: %[2]s
  privateKey: {algorithm: ECDSA, size: 256}
  issuerRef: {name: e2e-selfsigned, kind: Issuer}
---
apiVersion: cert-manager.io/v1
kind: Issuer
metadata: {name: e2e-ca, namespace: %[1]s}
spec: {ca: {secretName: %[2]s}}
---
apiVersion: cert-manager.io/v1
kind: Certificate
metadata: {name: %[3]s, namespace: %[1]s}
spec:
  secretName: %[3]s
  privateKey: {algorithm: ECDSA, size: 256}
  usages: [server auth, client auth]
  dnsNames:
  - %[4]s
  - "*.%[5]s-headless.%[1]s.svc.cluster.local"
  issuerRef: {name: e2e-ca, kind: Issuer}
`, clusterNamespace, tlsCASecret, tlsSecret, tlsServer, clusterName)
}

// servedAndMounted says what the primary presents and what its mounted
// certificate is, as short fingerprints: the same once the server has picked up
// the file.
func servedAndMounted(pod string) (served, mounted string) {
	out, err := runInPod(pod, tlsFileCheck)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	fields := strings.Fields(out)
	ExpectWithOffset(1, fields).To(HaveLen(2), out)
	return fields[0], fields[1]
}

// tlsScenario gives the main cluster a certificate from cert-manager, and checks
// what that is for: the operator and the jobs verify what the database presents,
// a renewed certificate reaches the server without a restart, and taking it away
// puts everything back.
func tlsScenario(healthy func(Gomega), podUIDs func() string) {
	primaryNow := func() string {
		primary, err := clusterField(".status.primaryInstance")
		ExpectWithOffset(2, err).NotTo(HaveOccurred())
		return primary
	}
	DeferCleanup(func() {
		_, _ = inCluster("patch", "postgrescluster", clusterName, "--type=merge", "-p", `{"spec":{"security":null}}`)
		_, _ = inCluster("delete", "certificate", tlsSecret, tlsCASecret, "--ignore-not-found", "--wait=false")
		_, _ = inCluster("delete", "issuer", "e2e-ca", "e2e-selfsigned", "--ignore-not-found", "--wait=false")
		_, _ = inCluster("delete", "secret", tlsSecret, tlsCASecret, "--ignore-not-found")
	})

	By("making a CA and a certificate for the cluster's Service and pods")
	_, err := kubectlIn(certificatesManifest(), "apply", "-f", "-")
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	_, err = inCluster("wait", "--for=condition=Ready", "certificate/"+tlsCASecret, "certificate/"+tlsSecret, "--timeout=3m")
	ExpectWithOffset(1, err).NotTo(HaveOccurred())

	By("asking for it: every pod is replaced to present it, and the operator checks what the primary then presents")
	before := podUIDs()
	_, err = inCluster("patch", "postgrescluster", clusterName, "--type=merge", "-p",
		fmt.Sprintf(`{"spec":{"security":{"tls":{"secretName":%q}}}}`, tlsSecret))
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	EventuallyWithOffset(1, func(g Gomega) {
		for uid := range strings.FieldsSeq(before) {
			g.Expect(podUIDs()).NotTo(ContainSubstring(uid), "every pod was replaced")
		}
		ready, err := conditionStatus("TLSReady")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(ready).To(Equal("True"))
		healthy(g)
	}, clusterUpTimeout+5*time.Minute, pollEvery).Should(Succeed())
	verified, err := clusterField(".status.security.tls.verified")
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	ExpectWithOffset(1, verified).To(Equal("true"))
	issuer, err := clusterField(".status.security.tls.issuer")
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	ExpectWithOffset(1, issuer).To(Equal("e2e-ca"))

	By("seeing that a client can verify the server, by its Service name, and refuses it by any other")
	primary := primaryNow()
	ip, err := inCluster("get", "pod", primary, "-o", "jsonpath={.status.podIP}")
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	connect := func(host string) string {
		out, _ := runInPod(primary, `PGPASSWORD="$PGPASSWORD_SUPERUSER" psql -X -At \
			"host=$1 hostaddr=$2 user=postgres dbname=postgres sslmode=verify-full sslrootcert=/tls/ca.crt" \
			-c "select 'verified'" 2>&1 | head -1`, host, ip)
		return strings.TrimSpace(out)
	}
	ExpectWithOffset(1, connect(tlsServer)).To(Equal("verified"))
	ExpectWithOffset(1, connect("wrong.example.com")).To(ContainSubstring("does not match host name"))

	By("running the jobs, which now check it too: every control is met, and encryption-in-transit says who issued it")
	runComplianceJobs("tls")
	var state complianceState
	EventuallyWithOffset(1, func(g Gomega) {
		state, err = readCompliance()
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(state.Controls["encryption-in-transit"].Evidence).To(ContainSubstring("issued by e2e-ca"))
		for id, c := range state.Controls {
			g.Expect(c.Status).To(Equal("Met"), "%s: %s", id, c.Evidence)
		}
	}, 3*time.Minute, pollEvery).Should(Succeed())

	By("renewing the certificate: the server presents the new one without a pod being replaced")
	servedBefore, mountedBefore := servedAndMounted(primary)
	ExpectWithOffset(1, servedBefore).To(Equal(mountedBefore))
	uids := podUIDs()
	_, err = inCluster("delete", "secret", tlsSecret)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	EventuallyWithOffset(1, func(g Gomega) {
		served, mounted := servedAndMounted(primary)
		g.Expect(mounted).NotTo(Equal(mountedBefore), "cert-manager made a new certificate and the pod has it")
		g.Expect(served).To(Equal(mounted), "and the server presents it")
	}, 10*time.Minute, 10*time.Second).Should(Succeed())
	ExpectWithOffset(1, podUIDs()).To(Equal(uids), "no pod was replaced")
	EventuallyWithOffset(1, func() (string, error) { return conditionStatus("TLSReady") }, 3*time.Minute, pollEvery).Should(Equal("True"))

	By("taking it away: every pod goes back to the certificate the image made, and the jobs to an encrypted connection")
	_, err = inCluster("patch", "postgrescluster", clusterName, "--type=merge", "-p", `{"spec":{"security":null}}`)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	EventuallyWithOffset(1, func(g Gomega) {
		for uid := range strings.FieldsSeq(uids) {
			g.Expect(podUIDs()).NotTo(ContainSubstring(uid), "every pod was replaced")
		}
		healthy(g)
		out, err := inCluster("get", "pods", "-l", "app.kubernetes.io/component=postgres", "-o",
			`jsonpath={.items[*].spec.volumes[?(@.name=="tls")].name}`)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(out).To(BeEmpty())
	}, clusterUpTimeout+5*time.Minute, pollEvery).Should(Succeed())
	ExpectWithOffset(1, clusterCondition("TLSReady")).To(BeEmpty(), "nothing to report once it is not asked for")
	_, err = inCluster("get", "configmap", clusterName+"-tls-ca")
	ExpectWithOffset(1, err).To(HaveOccurred(), "the CA goes with it")
	mode, err := inCluster("get", "cronjob", clusterName+"-compliance-check", "-o",
		`jsonpath={.spec.jobTemplate.spec.template.spec.containers[0].env[?(@.name=="PGSSLMODE")].value}`)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	ExpectWithOffset(1, mode).To(Equal("require"))
}

func clusterCondition(conditionType string) string {
	out, err := conditionStatus(conditionType)
	ExpectWithOffset(2, err).NotTo(HaveOccurred())
	return out
}
