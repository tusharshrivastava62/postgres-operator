/*
Copyright 2026.

Licensed under the Business Source License 1.1 (the "License"). You may not
use this file except in compliance with the License, a copy of which is in the
LICENSE file at the root of this repository. Production use requires a
commercial license from the Licensor. On the Change Date (2030-10-17) the
work becomes available under the Change License (Apache-2.0).
*/

package controller

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	dbv1alpha1 "github.com/tusharshrivastava62/postgres-operator/api/v1alpha1"
)

// tester is the part of a test that the certificate helpers need, which a
// Ginkgo spec has as well as a Go test.
type tester interface {
	Helper()
	Fatal(args ...any)
}

// testCert is a certificate and its key, for building the Secrets a cluster
// is given.
type testCert struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

func (c *testCert) certPEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.cert.Raw})
}

func (c *testCert) keyPEM(tb tester) []byte {
	tb.Helper()
	der, err := x509.MarshalECPrivateKey(c.key)
	if err != nil {
		tb.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
}

const (
	testTLSSecret          = "orders-tls"
	testServiceName        = "orders.shop.svc"
	wantNotValidForService = "not valid for " + testServiceName
)

var serialNumber int64

// issue makes a certificate signed by parent, or self-signed if parent is nil.
func issue(tb tester, parent *testCert, name string, isCA bool, names []string, notBefore, notAfter time.Time,
	usages []x509.ExtKeyUsage,
) *testCert {
	tb.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		tb.Fatal(err)
	}
	serialNumber++
	template := &x509.Certificate{
		SerialNumber: big.NewInt(serialNumber), Subject: pkix.Name{CommonName: name},
		NotBefore: notBefore, NotAfter: notAfter, DNSNames: names, ExtKeyUsage: usages,
		KeyUsage: x509.KeyUsageDigitalSignature, BasicConstraintsValid: true, IsCA: isCA,
	}
	if isCA {
		template.KeyUsage |= x509.KeyUsageCertSign
	}
	signer, signerKey := template, key
	if parent != nil {
		signer, signerKey = parent.cert, parent.key
	}
	der, err := x509.CreateCertificate(rand.Reader, template, signer, &key.PublicKey, signerKey)
	if err != nil {
		tb.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		tb.Fatal(err)
	}
	return &testCert{cert: cert, key: key}
}

var (
	tlsNow       = time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	serverAuth   = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	testClusterT = &dbv1alpha1.PostgresCluster{ObjectMeta: metav1.ObjectMeta{Name: "orders", Namespace: "shop"}}
)

func newTestCA(tb tester, name string) *testCert {
	return issue(tb, nil, name, true, nil, tlsNow.Add(-100*24*time.Hour), tlsNow.Add(365*24*time.Hour), nil)
}

// rightNames are the names the database is reached by.
func rightNames() []string {
	return []string{testServiceName, "*.orders-headless.shop.svc.cluster.local"}
}

func secretFor(tb tester, name string, leaf, ca *testCert, chain ...*testCert) *corev1.Secret {
	tb.Helper()
	crt := leaf.certPEM()
	for _, c := range chain {
		crt = append(crt, c.certPEM()...)
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "shop"},
		Data:       map[string][]byte{tlsCertKey: crt, tlsKeyKey: leaf.keyPEM(tb), tlsCAKey: ca.certPEM()},
	}
}

func TestACertificateThatIsRightIsAccepted(t *testing.T) {
	ca := newTestCA(t, "orders-ca")
	leaf := issue(t, ca, "orders", false, rightNames(), tlsNow.Add(-time.Minute), tlsNow.Add(time.Hour), serverAuth)
	material, problem := checkTLS(secretFor(t, testTLSSecret, leaf, ca), testClusterT, tlsNow)
	if problem != "" || material == nil || material.Expired {
		t.Fatalf("problem %q, material %+v", problem, material)
	}
	if string(material.CA) != string(ca.certPEM()) {
		t.Error("the CA the jobs are given is not the Secret's ca.crt")
	}
}

func TestACertificateIssuedThroughAnIntermediateIsAccepted(t *testing.T) {
	root := newTestCA(t, "root")
	intermediate := issue(t, root, "intermediate", true, nil, tlsNow.Add(-50*24*time.Hour), tlsNow.Add(time.Hour*24), nil)
	leaf := issue(t, intermediate, "orders", false, rightNames(), tlsNow.Add(-time.Minute), tlsNow.Add(time.Hour), serverAuth)

	if _, problem := checkTLS(secretFor(t, testTLSSecret, leaf, root, intermediate), testClusterT, tlsNow); problem != "" {
		t.Errorf("with the intermediate in tls.crt: %s", problem)
	}
	if _, problem := checkTLS(secretFor(t, testTLSSecret, leaf, root), testClusterT, tlsNow); problem == "" {
		t.Error("without the intermediate the chain is incomplete, and the server could not present it")
	}
}

func TestACertificateThatIsWrongIsAProblemToFixBeforeAnythingChanges(t *testing.T) {
	ca := newTestCA(t, "orders-ca")
	other := newTestCA(t, "someone-else")
	good := func() *testCert {
		return issue(t, ca, "orders", false, rightNames(), tlsNow.Add(-time.Minute), tlsNow.Add(time.Hour), serverAuth)
	}
	cases := []struct {
		name   string
		secret func() *corev1.Secret
		want   string
	}{
		{"no private key", func() *corev1.Secret {
			s := secretFor(t, testTLSSecret, good(), ca)
			delete(s.Data, tlsKeyKey)
			return s
		}, "has no tls.key"},
		{"no CA", func() *corev1.Secret {
			s := secretFor(t, testTLSSecret, good(), ca)
			s.Data[tlsCAKey] = nil
			return s
		}, "has no ca.crt"},
		{"a key that is not the certificate's", func() *corev1.Secret {
			s := secretFor(t, testTLSSecret, good(), ca)
			s.Data[tlsKeyKey] = good().keyPEM(t)
			return s
		}, "private key"},
		{"a CA that holds no certificate", func() *corev1.Secret {
			s := secretFor(t, testTLSSecret, good(), ca)
			s.Data[tlsCAKey] = []byte("not a certificate")
			return s
		}, "holds no certificate"},
		{"issued by another CA", func() *corev1.Secret {
			return secretFor(t, testTLSSecret, good(), other)
		}, wantNotValidForService},
		{"not valid for the Service", func() *corev1.Secret {
			leaf := issue(t, ca, "orders", false, []string{"*.orders-headless.shop.svc.cluster.local"},
				tlsNow.Add(-time.Minute), tlsNow.Add(time.Hour), serverAuth)
			return secretFor(t, testTLSSecret, leaf, ca)
		}, wantNotValidForService},
		{"not valid for the pods", func() *corev1.Secret {
			leaf := issue(t, ca, "orders", false, []string{testServiceName}, tlsNow.Add(-time.Minute), tlsNow.Add(time.Hour), serverAuth)
			return secretFor(t, testTLSSecret, leaf, ca)
		}, "orders-0.orders-headless.shop.svc.cluster.local"},
		{"made for another namespace", func() *corev1.Secret {
			leaf := issue(t, ca, "orders", false, []string{"orders.other.svc", "*.orders-headless.other.svc.cluster.local"},
				tlsNow.Add(-time.Minute), tlsNow.Add(time.Hour), serverAuth)
			return secretFor(t, testTLSSecret, leaf, ca)
		}, wantNotValidForService},
		{"not for a server", func() *corev1.Secret {
			leaf := issue(t, ca, "orders", false, rightNames(), tlsNow.Add(-time.Minute), tlsNow.Add(time.Hour),
				[]x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth})
			return secretFor(t, testTLSSecret, leaf, ca)
		}, wantNotValidForService},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			material, problem := checkTLS(c.secret(), testClusterT, tlsNow)
			if material != nil || !strings.Contains(problem, c.want) {
				t.Errorf("material %v, problem %q, want one containing %q", material != nil, problem, c.want)
			}
			if !strings.Contains(problem, "orders-tls") {
				t.Errorf("the problem does not say which Secret: %q", problem)
			}
		})
	}
}

// An expired certificate is not a reason to stop: nothing the operator would
// change makes it worse, and it is reported instead.
func TestAnExpiredCertificateIsReportedAndNotHeldBack(t *testing.T) {
	ca := newTestCA(t, "orders-ca")
	leaf := issue(t, ca, "orders", false, rightNames(), tlsNow.Add(-48*time.Hour), tlsNow.Add(-time.Hour), serverAuth)
	material, problem := checkTLS(secretFor(t, testTLSSecret, leaf, ca), testClusterT, tlsNow)
	if problem != "" || material == nil || !material.Expired {
		t.Fatalf("problem %q, material %+v", problem, material)
	}

	// An expired certificate for the wrong names is still the wrong names.
	wrong := issue(t, ca, "orders", false, []string{"elsewhere.example.com"}, tlsNow.Add(-48*time.Hour), tlsNow.Add(-time.Hour), serverAuth)
	if _, problem := checkTLS(secretFor(t, testTLSSecret, wrong, ca), testClusterT, tlsNow); !strings.Contains(problem, "not valid for") {
		t.Errorf("problem %q", problem)
	}
}

func TestACertificateNotYetValidIsAProblem(t *testing.T) {
	ca := newTestCA(t, "orders-ca")
	leaf := issue(t, ca, "orders", false, rightNames(), tlsNow.Add(time.Hour), tlsNow.Add(2*time.Hour), serverAuth)
	if _, problem := checkTLS(secretFor(t, testTLSSecret, leaf, ca), testClusterT, tlsNow); problem == "" {
		t.Error("a certificate that is not valid yet was accepted")
	}
}

func TestPodsServeTheCertificateOnlyIfTheySpecMountTheSecretTheClusterNames(t *testing.T) {
	mounts := func(secret string) *corev1.Pod {
		return &corev1.Pod{Spec: corev1.PodSpec{Volumes: []corev1.Volume{{
			Name: tlsVolumeName, VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: secret}},
		}}}}
	}
	if !podServesTLS(mounts(testTLSSecret), testTLSSecret) {
		t.Error("a pod with the Secret is not serving it")
	}
	if podServesTLS(mounts("the-old-one"), testTLSSecret) {
		t.Error("a pod with another Secret is serving this one")
	}
	if podServesTLS(&corev1.Pod{}, testTLSSecret) {
		t.Error("a pod with no volumes is serving a certificate")
	}
}

func TestTheDatabasePodGetsTheCertificateAndTheKeyIsNotReadableByOthers(t *testing.T) {
	cluster := testClusterT.DeepCopy()
	cluster.Spec.Instances = 1
	cluster.Spec.Version = "17"
	cluster.Spec.Security = &dbv1alpha1.SecuritySpec{TLS: &dbv1alpha1.TLSSpec{SecretName: testTLSSecret}}

	sts, err := desiredStatefulSetAt(cluster, "image", "")
	if err != nil {
		t.Fatal(err)
	}
	pod := sts.Spec.Template.Spec
	var found *corev1.Volume
	for i := range pod.Volumes {
		if pod.Volumes[i].Name == tlsVolumeName {
			found = &pod.Volumes[i]
		}
	}
	if found == nil || found.Secret.SecretName != testTLSSecret {
		t.Fatalf("volumes %+v", pod.Volumes)
	}
	if found.Secret.DefaultMode == nil || *found.Secret.DefaultMode != 0o440 {
		t.Errorf("the key's mode is %v: PostgreSQL refuses a key others can read", found.Secret.DefaultMode)
	}
	container := pod.Containers[0]
	mounted := false
	for _, m := range container.VolumeMounts {
		mounted = mounted || (m.Name == tlsVolumeName && m.MountPath == "/tls" && m.ReadOnly)
	}
	if !mounted {
		t.Errorf("mounts %+v", container.VolumeMounts)
	}
	env := map[string]string{}
	for _, e := range container.Env {
		env[e.Name] = e.Value
	}
	for name, want := range map[string]string{
		"SSL_CERTIFICATE_FILE": "/tls/tls.crt", "SSL_PRIVATE_KEY_FILE": "/tls/tls.key", "SSL_CA_FILE": "/tls/ca.crt",
	} {
		if env[name] != want {
			t.Errorf("%s = %q, want %q", name, env[name], want)
		}
	}

	// And without a certificate, none of it.
	cluster.Spec.Security = nil
	plain, err := desiredStatefulSetAt(cluster, "image", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range plain.Spec.Template.Spec.Volumes {
		if v.Name == tlsVolumeName {
			t.Error("a cluster with no certificate mounts one")
		}
	}
	for _, e := range plain.Spec.Template.Spec.Containers[0].Env {
		if strings.HasPrefix(e.Name, "SSL_") {
			t.Errorf("a cluster with no certificate sets %s", e.Name)
		}
	}
}

func TestJobsCheckTheCertificateOnlyOnceTheOperatorHas(t *testing.T) {
	cluster := testClusterT.DeepCopy()
	if jobsVerifyTLS(cluster) {
		t.Error("a cluster with no certificate checks one")
	}
	cluster.Spec.Security = &dbv1alpha1.SecuritySpec{TLS: &dbv1alpha1.TLSSpec{SecretName: testTLSSecret}}
	if jobsVerifyTLS(cluster) {
		t.Error("jobs check a certificate the operator has not verified")
	}
	cluster.Status.Security.TLS = &dbv1alpha1.TLSStatus{Verified: false}
	if jobsVerifyTLS(cluster) {
		t.Error("jobs check a certificate that failed")
	}
	cluster.Status.Security.TLS = &dbv1alpha1.TLSStatus{Verified: true}
	if !jobsVerifyTLS(cluster) {
		t.Error("jobs do not check a certificate the operator verified")
	}
	env := map[string]string{}
	for _, e := range jobSSLEnv(cluster) {
		env[e.Name] = e.Value
	}
	if env["PGSSLMODE"] != "verify-full" || env["PGSSLROOTCERT"] != "/tls-ca/ca.crt" {
		t.Errorf("env %v", env)
	}
	// Removing the setting stops it at once, whatever status says.
	cluster.Spec.Security = nil
	if jobsVerifyTLS(cluster) {
		t.Error("jobs go on checking after the certificate is taken away")
	}
	if got := jobSSLEnv(cluster); len(got) != 1 || got[0].Value != "require" {
		t.Errorf("env %v", got)
	}
}
