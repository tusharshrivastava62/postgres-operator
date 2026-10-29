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
ip = subprocess.check_output(["hostname", "-i"]).decode().split()[0]
ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
ctx.check_hostname = False
ctx.verify_mode = ssl.CERT_NONE
def fingerprint(sock):
    return hashlib.sha256(ctx.wrap_socket(sock).getpeercert(binary_form=True)).hexdigest()[:16]
sock = socket.create_connection((ip, 5432), timeout=5)
sock.sendall(struct.pack("!II", 8, 80877103))
assert sock.recv(1) == b"S"
served = fingerprint(sock)
try:
    api = fingerprint(socket.create_connection((ip, 8008), timeout=5))
except Exception:
    api = "none"
first = open("/tls/tls.crt").read().split("-----END CERTIFICATE-----")[0] + "-----END CERTIFICATE-----"
print(served, hashlib.sha256(ssl.PEM_cert_to_DER_cert(first)).hexdigest()[:16], api)
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
func servedAndMounted(pod string) (served, mounted, api string) {
	out, err := runInPod(pod, tlsFileCheck)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	fields := strings.Fields(out)
	ExpectWithOffset(1, fields).To(HaveLen(3), out)
	return fields[0], fields[1], fields[2]
}

// apiStatus is the status code Patroni's API answers a request from a pod with,
// or 000 when it does not answer. It is made from inside the cluster, to the pod
// at ip, as anyone who could reach port 8008 could make it.
func apiStatus(from, scheme, ip, method, path, password string) string {
	script := `curl -sk -m 5 -o /dev/null -w '%{http_code}' -X "$1" ${5:+-u "operator:$5"} -H 'Content-Type: application/json' -d '{}' "$2://$3:8008$4" || true`
	out, _ := runInPod(from, script, method, scheme, ip, path, password)
	return strings.TrimSpace(out)
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

	By("asking for Patroni's API to need a password and to answer over TLS: every pod is replaced to start that way")
	beforeAPI := podUIDs()
	_, err = inCluster("patch", "postgrescluster", clusterName, "--type=merge", "-p",
		fmt.Sprintf(`{"spec":{"security":{"tls":{"secretName":%q,"patroniAPI":true},"patroniAPI":{"authentication":true}}}}`, tlsSecret))
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	EventuallyWithOffset(1, func(g Gomega) {
		for uid := range strings.FieldsSeq(beforeAPI) {
			g.Expect(podUIDs()).NotTo(ContainSubstring(uid), "every pod was replaced")
		}
		started, err := clusterField(".status.security.patroniAPI")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(started).To(Equal(`{"authenticated":true,"tls":true}`), "the primary was started that way")
		healthy(g)
	}, clusterUpTimeout+5*time.Minute, pollEvery).Should(Succeed())

	By("seeing that it reads for anyone, changes things only for whoever has the password, and speaks TLS only")
	primary = primaryNow()
	replica, err := inCluster("get", "pods", "-l", mainRole("replica"), "-o", "jsonpath={.items[0].metadata.name}")
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	ip, err = inCluster("get", "pod", primary, "-o", "jsonpath={.status.podIP}")
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	password, err := secretValue(clusterName+"-credentials", "patroni-api-password")
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	ExpectWithOffset(1, password).To(HavePrefix("pw"))
	ExpectWithOffset(1, apiStatus(replica, "http", ip, "GET", "/health", "")).To(Equal("000"), "plain HTTP is not served")
	ExpectWithOffset(1, apiStatus(replica, "https", ip, "GET", "/health", "")).To(Equal("200"), "reading is open: the kubelet needs it")
	ExpectWithOffset(1, apiStatus(replica, "https", ip, "GET", "/config", "")).To(Equal("200"))
	ExpectWithOffset(1, apiStatus(replica, "https", ip, "POST", "/reload", "")).To(Equal("401"), "anyone could reload without the password")
	ExpectWithOffset(1, apiStatus(replica, "https", ip, "PATCH", "/config", "wrong")).To(Equal("401"))
	ExpectWithOffset(1, apiStatus(replica, "https", ip, "PATCH", "/config", "")).To(Equal("401"))
	ExpectWithOffset(1, apiStatus(replica, "https", ip, "POST", "/reload", password)).To(Equal("202"), "with it, it is accepted")
	template, err := inCluster("get", "statefulset", clusterName, "-o", "jsonpath={.spec.template}")
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	ExpectWithOffset(1, template).NotTo(ContainSubstring(password), "the password is in the Secret and not in the pod's spec")

	By("seeing the operator go on managing Patroni through it: a change to spec.ha reaches the running cluster")
	_, err = inCluster("patch", "postgrescluster", clusterName, "--type=merge", "-p", `{"spec":{"ha":{"failoverTimeout":45}}}`)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	EventuallyWithOffset(1, func() (string, error) {
		return runInPod(replica, `curl -sk -m 5 "https://$1:8008/config" | python3 -c "import json,sys; print(json.load(sys.stdin)['ttl'])"`, ip)
	}, 3*time.Minute, pollEvery).Should(Equal("45"))

	By("replacing the password with the others: the pods are replaced to start with it, and the operator is answered meanwhile")
	beforeRotation := podUIDs()
	_, err = inCluster("annotate", "secret", clusterName+"-credentials", "db.youroperator.io/rotate=e2e-api-1", "--overwrite")
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	EventuallyWithOffset(1, func(g Gomega) {
		for uid := range strings.FieldsSeq(beforeRotation) {
			g.Expect(podUIDs()).NotTo(ContainSubstring(uid), "every pod was replaced")
		}
		g.Expect(clusterCondition("Available")).To(Equal("True"))
		state, err := clusterField(".status.accessControl.credentials.state")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(state).To(Equal("Idle"))
		keys, err := inCluster("get", "secret", clusterName+"-credentials", "-o", "jsonpath={.data}")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(keys).NotTo(ContainSubstring("patroni-api-password-previous"), "the old one is dropped once every pod has the new")
		healthy(g)
	}, 2*clusterUpTimeout, pollEvery).Should(Succeed())
	replaced, err := secretValue(clusterName+"-credentials", "patroni-api-password")
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	ExpectWithOffset(1, replaced).NotTo(Equal(password))
	primary = primaryNow()
	replica, err = inCluster("get", "pods", "-l", mainRole("replica"), "-o", "jsonpath={.items[0].metadata.name}")
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	ip, err = inCluster("get", "pod", primary, "-o", "jsonpath={.status.podIP}")
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	ExpectWithOffset(1, apiStatus(replica, "https", ip, "POST", "/reload", password)).To(Equal("401"), "the old password no longer works")
	ExpectWithOffset(1, apiStatus(replica, "https", ip, "POST", "/reload", replaced)).To(Equal("202"))

	By("renewing the certificate: the server presents the new one without a pod being replaced")
	primary = primaryNow()
	servedBefore, mountedBefore, apiBefore := servedAndMounted(primary)
	ExpectWithOffset(1, servedBefore).To(Equal(mountedBefore))
	ExpectWithOffset(1, apiBefore).To(Equal(mountedBefore))
	uids := podUIDs()
	_, err = inCluster("delete", "secret", tlsSecret)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	EventuallyWithOffset(1, func(g Gomega) {
		served, mounted, api := servedAndMounted(primary)
		g.Expect(mounted).NotTo(Equal(mountedBefore), "cert-manager made a new certificate and the pod has it")
		g.Expect(served).To(Equal(mounted), "and the server presents it")
		g.Expect(api).To(Equal(mounted), "and so does Patroni's API, which the operator asked to reload")
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
	primary = primaryNow()
	replica, err = inCluster("get", "pods", "-l", mainRole("replica"), "-o", "jsonpath={.items[0].metadata.name}")
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	ip, err = inCluster("get", "pod", primary, "-o", "jsonpath={.status.podIP}")
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	ExpectWithOffset(1, apiStatus(replica, "http", ip, "GET", "/health", "")).To(Equal("200"), "Patroni's API is back to plain HTTP")
	ExpectWithOffset(1, apiStatus(replica, "http", ip, "POST", "/reload", "")).To(Equal("202"), "and asks for no password")
	started, err := clusterField(".status.security")
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	ExpectWithOffset(1, started).To(BeEmpty())
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
