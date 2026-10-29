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
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"maps"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/yaml"

	dbv1alpha1 "github.com/tusharshrivastava62/postgres-operator/api/v1alpha1"
)

var _ = Describe("The Patroni API", func() {
	const namespace = "default"
	ctx := context.Background()
	key := func(name string) types.NamespacedName { return types.NamespacedName{Name: name, Namespace: namespace} }

	var (
		sql  *fakeSQL
		rec  *PostgresClusterReconciler
		pats *fakePatroni
		ca   *testCert
	)
	BeforeEach(func() {
		testMonitorPasswords = nil
		sql = &fakeSQL{}
		pats = &fakePatroni{live: map[string]any{}}
		rec = &PostgresClusterReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), Patroni: pats, Operator: testOperator, SQL: sql,
		}
		ca = newTestCA(GinkgoT(), "orders-ca")
	})

	reconcileOnce := func(name string) reconcile.Result {
		GinkgoHelper()
		res, err := rec.Reconcile(ctx, reconcile.Request{NamespacedName: key(name)})
		Expect(err).NotTo(HaveOccurred())
		return res
	}
	get := func(name string) *dbv1alpha1.PostgresCluster {
		GinkgoHelper()
		c := &dbv1alpha1.PostgresCluster{}
		Expect(k8sClient.Get(ctx, key(name), c)).To(Succeed())
		return c
	}
	secretOf := func(name string) *corev1.Secret {
		GinkgoHelper()
		s := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, key(name), s)).To(Succeed())
		return s
	}
	stsOf := func(name string) *appsv1.StatefulSet {
		GinkgoHelper()
		sts := &appsv1.StatefulSet{}
		Expect(k8sClient.Get(ctx, key(name), sts)).To(Succeed())
		return sts
	}
	envOf := func(sts *appsv1.StatefulSet) map[string]corev1.EnvVar {
		out := map[string]corev1.EnvVar{}
		for _, e := range sts.Spec.Template.Spec.Containers[0].Env {
			out[e.Name] = e
		}
		return out
	}
	create := func(name string, mutate func(*dbv1alpha1.PostgresCluster)) {
		GinkgoHelper()
		c := plainCluster()
		c.Name = name
		c.Spec.Storage = dbv1alpha1.StorageSpec{Size: resource.MustParse("1Gi")}
		c.Spec.Resources = corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}}
		if mutate != nil {
			mutate(c)
		}
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		DeferCleanup(func() {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, c))).To(Succeed())
			for _, n := range []string{credentialsSecretName(c), monitorSecretName(c)} {
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: n, Namespace: namespace}}))).To(Succeed())
			}
		})
	}
	withAPIAuth := func(c *dbv1alpha1.PostgresCluster) {
		c.Spec.Security = &dbv1alpha1.SecuritySpec{PatroniAPI: &dbv1alpha1.PatroniAPISpec{Authentication: true}}
	}
	withAPITLS := func(c *dbv1alpha1.PostgresCluster) {
		c.Spec.Security = &dbv1alpha1.SecuritySpec{TLS: &dbv1alpha1.TLSSpec{SecretName: c.Name + "-tls", PatroniAPI: true}}
	}
	makeCertSecret := func(name string) *testCert {
		GinkgoHelper()
		leaf := issue(GinkgoT(), ca, "orders", false,
			[]string{name + "." + namespace + ".svc", "*." + name + "-headless." + namespace + ".svc.cluster.local"},
			time.Now().Add(-time.Minute), time.Now().Add(time.Hour), serverAuth)
		secret := secretFor(GinkgoT(), name+"-tls", leaf, ca)
		secret.Namespace = namespace
		Expect(k8sClient.Create(ctx, secret)).To(Succeed())
		DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, secret))).To(Succeed()) })
		return leaf
	}
	// startLeader makes the primary's pod, with whatever the test says of it.
	startLeader := func(name string, shape func(*corev1.Pod)) *corev1.Pod {
		GinkgoHelper()
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name + "-0", Namespace: namespace, Labels: selectorLabelsFor(name)},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: testPodImage}}},
		}
		if shape != nil {
			shape(pod)
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		DeferCleanup(func() {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, pod, client.GracePeriodSeconds(0)))).To(Succeed())
		})
		pod.Status.PodIP = testLeaderIP
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
		endpoints := &patroniEndpoints{ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: namespace, Labels: selectorLabelsFor(name),
			Annotations: map[string]string{annotationLeader: pod.Name},
		}}
		Expect(k8sClient.Create(ctx, endpoints)).To(Succeed())
		DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, endpoints))).To(Succeed()) })
		return pod
	}
	withEnv := func(names ...string) func(*corev1.Pod) {
		return func(pod *corev1.Pod) {
			for _, n := range names {
				pod.Spec.Containers[0].Env = append(pod.Spec.Containers[0].Env, corev1.EnvVar{Name: n, Value: "x"})
			}
		}
	}

	Describe("by default", func() {
		It("asks for no password: no key in the Secret, no variable in the pod, nothing in the configuration", func() {
			create("api-off", nil)
			reconcileOnce("api-off")
			Expect(secretOf("api-off-credentials").Data).NotTo(HaveKey(secretKeyPatroniAPI))
			sts := stsOf("api-off")
			Expect(envOf(sts)).NotTo(HaveKey(envRESTAPIPassword))
			Expect(envOf(sts)[envSpiloConfiguration].Value).NotTo(ContainSubstring("restapi"))
			Expect(envOf(sts)).NotTo(HaveKey(envRESTAPICertificate))
			Expect(envOf(sts)).NotTo(HaveKey(envRESTAPIConnectAddress))
			for _, probe := range []*corev1.Probe{sts.Spec.Template.Spec.Containers[0].ReadinessProbe, sts.Spec.Template.Spec.Containers[0].LivenessProbe} {
				Expect(probe.HTTPGet.Scheme).NotTo(Equal(corev1.URISchemeHTTPS))
			}
		})

		It("calls Patroni as it always has: plain HTTP, no credentials", func() {
			create("api-plain", nil)
			reconcileOnce("api-plain")
			startLeader("api-plain", nil)
			reconcileOnce("api-plain")
			Expect(pats.targets).NotTo(BeEmpty())
			for _, t := range pats.targets {
				Expect(t.Passwords).To(BeEmpty())
				Expect(t.TLS).To(BeNil())
			}
		})
	})

	Describe("asking for a password", func() {
		It("makes one, once, and puts it in the pod from the Secret and never into the pod's spec", func() {
			create("api-auth", withAPIAuth)
			reconcileOnce("api-auth")
			password := string(secretOf("api-auth-credentials").Data[secretKeyPatroniAPI])
			Expect(password).To(HavePrefix("pw"), "it starts with a letter, since YAML reads it")
			Expect(len(password)).To(BeNumerically(">=", 30))

			reconcileOnce("api-auth")
			Expect(string(secretOf("api-auth-credentials").Data[secretKeyPatroniAPI])).To(Equal(password), "kept across reconciles")

			sts := stsOf("api-auth")
			env := envOf(sts)
			ref := env[envRESTAPIPassword].ValueFrom.SecretKeyRef
			Expect(ref.Name).To(Equal("api-auth-credentials"))
			Expect(ref.Key).To(Equal(secretKeyPatroniAPI))
			raw, err := json.Marshal(sts.Spec.Template)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(raw)).NotTo(ContainSubstring(password), "the password is in the Secret, not the pod's spec")

			By("telling Patroni to ask for it, by the variable and not the value")
			var config struct {
				RestAPI struct {
					Authentication struct{ Username, Password string }
				} `json:"restapi"`
			}
			Expect(yaml.Unmarshal([]byte(env[envSpiloConfiguration].Value), &config)).To(Succeed())
			Expect(config.RestAPI.Authentication.Username).To(Equal("operator"))
			Expect(config.RestAPI.Authentication.Password).To(Equal("$(REST_API_PASSWORD)"))

			By("with the variable defined before the configuration that refers to it")
			podEnv := sts.Spec.Template.Spec.Containers[0].Env
			names := make([]string, 0, len(podEnv))
			for _, e := range podEnv {
				names = append(names, e.Name)
			}
			Expect(indexOf(names, envRESTAPIPassword)).To(BeNumerically("<", indexOf(names, envSpiloConfiguration)))
		})

		It("keeps the password when it is no longer asked for, and takes the variable out of the pod", func() {
			create("api-keep", withAPIAuth)
			reconcileOnce("api-keep")
			password := string(secretOf("api-keep-credentials").Data[secretKeyPatroniAPI])

			c := get("api-keep")
			c.Spec.Security = nil
			Expect(k8sClient.Update(ctx, c)).To(Succeed())
			reconcileOnce("api-keep")

			Expect(envOf(stsOf("api-keep"))).NotTo(HaveKey(envRESTAPIPassword))
			Expect(envOf(stsOf("api-keep"))[envSpiloConfiguration].Value).NotTo(ContainSubstring("restapi"))
			Expect(string(secretOf("api-keep-credentials").Data[secretKeyPatroniAPI])).To(Equal(password))
		})

		It("sends it with every call to a primary, so that the pods that ask for it are answered", func() {
			create("api-send", withAPIAuth)
			reconcileOnce("api-send")
			startLeader("api-send", withEnv(envRESTAPIPassword))
			pats.targets = nil
			reconcileOnce("api-send")
			Expect(pats.targets).NotTo(BeEmpty())
			password := string(secretOf("api-send-credentials").Data[secretKeyPatroniAPI])
			for _, t := range pats.targets {
				Expect(t.Username).To(Equal("operator"))
				Expect(t.Passwords).To(Equal([]string{password}))
				Expect(t.TLS).To(BeNil(), "no TLS was asked for")
			}
		})
	})

	Describe("asking for a password after the cluster was made", func() {
		It("adds the password to a credentials Secret that was made without one, and leaves the others alone", func() {
			create("api-later", nil)
			reconcileOnce("api-later")
			before := secretOf("api-later-credentials")
			Expect(before.Data).NotTo(HaveKey(secretKeyPatroniAPI))

			c := get("api-later")
			withAPIAuth(c)
			Expect(k8sClient.Update(ctx, c)).To(Succeed())
			reconcileOnce("api-later")

			after := secretOf("api-later-credentials")
			Expect(string(after.Data[secretKeyPatroniAPI])).To(HavePrefix("pw"))
			Expect(after.Data[secretKeyPassword]).To(Equal(before.Data[secretKeyPassword]), "no password the database holds is changed")
			Expect(after.Data[secretKeyReplicationPassword]).To(Equal(before.Data[secretKeyReplicationPassword]))
		})
	})

	Describe("asking for TLS", func() {
		It("serves the API with the certificate, by names it is valid for, and has the kubelet ask over HTTPS", func() {
			create("api-tls", withAPITLS)
			makeCertSecret("api-tls")
			reconcileOnce("api-tls")
			sts := stsOf("api-tls")
			env := envOf(sts)
			Expect(env[envRESTAPICertificate].Value).To(Equal("/tls/tls.crt"))
			Expect(env[envRESTAPIPrivateKey].Value).To(Equal("/tls/tls.key"))
			Expect(env[envRESTAPICA].Value).To(Equal("/tls/ca.crt"))
			Expect(env[envPodName].ValueFrom.FieldRef.FieldPath).To(Equal("metadata.name"))
			Expect(env[envRESTAPIConnectAddress].Value).To(Equal("$(POD_NAME).api-tls-headless.default.svc.cluster.local:8008"))
			container := sts.Spec.Template.Spec.Containers[0]
			Expect(container.ReadinessProbe.HTTPGet.Scheme).To(Equal(corev1.URISchemeHTTPS))
			Expect(container.LivenessProbe.HTTPGet.Scheme).To(Equal(corev1.URISchemeHTTPS))
			Expect(env).NotTo(HaveKey(envRESTAPIPassword), "TLS is not a password")
		})

		It("leaves the API as it is when the certificate is for the database alone", func() {
			create("api-dbonly", func(c *dbv1alpha1.PostgresCluster) {
				c.Spec.Security = &dbv1alpha1.SecuritySpec{TLS: &dbv1alpha1.TLSSpec{SecretName: c.Name + "-tls"}}
			})
			makeCertSecret("api-dbonly")
			reconcileOnce("api-dbonly")
			sts := stsOf("api-dbonly")
			Expect(envOf(sts)).NotTo(HaveKey(envRESTAPICertificate))
			Expect(envOf(sts)).NotTo(HaveKey(envRESTAPIConnectAddress))
			Expect(sts.Spec.Template.Spec.Containers[0].ReadinessProbe.HTTPGet.Scheme).NotTo(Equal(corev1.URISchemeHTTPS))
		})
	})

	Describe("calling a pod", func() {
		target := func(name, pod string) PatroniTarget {
			GinkgoHelper()
			t, err := patroniTargetFor(ctx, k8sClient, get(name), pod, testLeaderIP)
			Expect(err).NotTo(HaveOccurred())
			return t
		}
		servesAPITLS := func(name string) func(*corev1.Pod) {
			return func(pod *corev1.Pod) {
				withEnv(envRESTAPICertificate)(pod)
				pod.Spec.Volumes = []corev1.Volume{tlsVolume(name + "-tls")}
			}
		}

		It("sends the current password, and the previous one after it while the pods are being replaced", func() {
			create("api-call", withAPIAuth)
			reconcileOnce("api-call")
			secret := secretOf("api-call-credentials")
			current := string(secret.Data[secretKeyPatroniAPI])
			Expect(target("api-call", "api-call-0").Passwords).To(Equal([]string{current}))

			secret.Data[secretKeyPatroniAPIPrevious] = []byte("pw-earlier")
			Expect(k8sClient.Update(ctx, secret)).To(Succeed())
			got := target("api-call", "api-call-0")
			Expect(got.Username).To(Equal("operator"))
			Expect(got.Passwords).To(Equal([]string{current, "pw-earlier"}))
		})

		It("uses TLS, checked against the cluster's CA and the pod's own name, only for a pod that serves it", func() {
			create("api-pod", withAPITLS)
			makeCertSecret("api-pod")
			reconcileOnce("api-pod")
			startLeader("api-pod", servesAPITLS("api-pod"))
			got := target("api-pod", "api-pod-0")
			Expect(got.TLS).NotTo(BeNil())
			Expect(got.TLS.ServerName).To(Equal("api-pod-0.api-pod-headless.default.svc.cluster.local"))
			Expect(got.TLS.Roots).NotTo(BeNil())

			By("and plain HTTP for a pod that was started before, or is not known")
			Expect(target("api-pod", "api-pod-9").TLS).To(BeNil())
		})

		It("does not use TLS for a pod that has the certificate for the database and not for the API", func() {
			create("api-half", withAPITLS)
			makeCertSecret("api-half")
			reconcileOnce("api-half")
			startLeader("api-half", func(pod *corev1.Pod) { pod.Spec.Volumes = []corev1.Volume{tlsVolume("api-half-tls")} })
			Expect(target("api-half", "api-half-0").TLS).To(BeNil())
		})

		It("goes on calling over TLS a pod that was started with it after the spec stops asking, until the pod is replaced", func() {
			create("api-undo", withAPITLS)
			makeCertSecret("api-undo")
			reconcileOnce("api-undo")
			startLeader("api-undo", servesAPITLS("api-undo"))
			c := get("api-undo")
			c.Spec.Security = nil
			Expect(k8sClient.Update(ctx, c)).To(Succeed())

			got := target("api-undo", "api-undo-0")
			Expect(got.TLS).NotTo(BeNil(), "a pod that answers over TLS only cannot be reached in plain HTTP")
			Expect(got.TLS.ServerName).To(Equal("api-undo-0.api-undo-headless.default.svc.cluster.local"))
		})

		It("refuses to call a pod that serves TLS when the certificate cannot be read, instead of calling it unchecked", func() {
			create("api-lost", withAPITLS)
			makeCertSecret("api-lost")
			reconcileOnce("api-lost")
			startLeader("api-lost", servesAPITLS("api-lost"))
			Expect(k8sClient.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "api-lost-tls", Namespace: namespace}})).To(Succeed())
			_, err := patroniTargetFor(ctx, k8sClient, get("api-lost"), "api-lost-0", testLeaderIP)
			Expect(err).To(MatchError(ContainSubstring("cannot be checked")))
		})
	})

	Describe("its certificate", func() {
		servesAPITLS := func(name string) func(*corev1.Pod) {
			return func(pod *corev1.Pod) {
				pod.Spec.Containers[0].Env = append(pod.Spec.Containers[0].Env, corev1.EnvVar{Name: envRESTAPICertificate, Value: "x"})
				pod.Spec.Volumes = []corev1.Volume{tlsVolume(name + "-tls")}
			}
		}
		stale := func() *x509.Certificate {
			return issue(GinkgoT(), ca, "orders", false, []string{"stale.example.com"},
				time.Now().Add(-time.Hour), time.Now().Add(time.Hour), serverAuth).cert
		}
		reconcileCert := func(name string) bool {
			GinkgoHelper()
			asked, err := rec.reconcileAPICertificate(ctx, get(name))
			Expect(err).NotTo(HaveOccurred())
			return asked
		}

		It("is presented again after it is renewed: a member that presents the old one is asked to reload, once", func() {
			create("api-renew", withAPITLS)
			leaf := makeCertSecret("api-renew")
			reconcileOnce("api-renew")
			startLeader("api-renew", servesAPITLS("api-renew"))
			pats.served = map[string]*x509.Certificate{testLeaderIP: stale()}
			pats.reloadsTo = leaf.cert
			pats.reloads = nil

			Expect(reconcileCert("api-renew")).To(BeTrue(), "look again soon, to see that it took")
			Expect(pats.reloads).To(Equal([]string{testLeaderIP}))
			reloaded := pats.targets[len(pats.targets)-1]
			Expect(reloaded.TLS).NotTo(BeNil(), "the call is checked like any other")

			Expect(reconcileCert("api-renew")).To(BeFalse())
			Expect(pats.reloads).To(HaveLen(1), "a member that presents the certificate is left alone")
		})

		It("sends the API password with the request, when there is one", func() {
			create("api-renew2", func(c *dbv1alpha1.PostgresCluster) {
				withAPITLS(c)
				c.Spec.Security.PatroniAPI = &dbv1alpha1.PatroniAPISpec{Authentication: true}
			})
			makeCertSecret("api-renew2")
			reconcileOnce("api-renew2")
			startLeader("api-renew2", servesAPITLS("api-renew2"))
			pats.served = map[string]*x509.Certificate{testLeaderIP: stale()}
			Expect(reconcileCert("api-renew2")).To(BeTrue())
			sent := pats.targets[len(pats.targets)-1]
			Expect(sent.Username).To(Equal("operator"))
			Expect(sent.Passwords).To(Equal([]string{string(secretOf("api-renew2-credentials").Data[secretKeyPatroniAPI])}))
		})

		It("is left alone for a member that was not started to serve it, one with no address, and one on its way out", func() {
			create("api-skip", withAPITLS)
			makeCertSecret("api-skip")
			reconcileOnce("api-skip")
			startLeader("api-skip", func(pod *corev1.Pod) { pod.Spec.Volumes = []corev1.Volume{tlsVolume("api-skip-tls")} })
			pats.served = map[string]*x509.Certificate{testLeaderIP: stale()}
			Expect(reconcileCert("api-skip")).To(BeFalse(), "it has the certificate for the database and not for its API")
			Expect(pats.reloads).To(BeEmpty())
		})

		It("does nothing for a cluster whose API is not over TLS", func() {
			create("api-notls", withAPIAuth)
			reconcileOnce("api-notls")
			startLeader("api-notls", withEnv(envRESTAPICertificate))
			pats.served = map[string]*x509.Certificate{testLeaderIP: stale()}
			Expect(reconcileCert("api-notls")).To(BeFalse())
			Expect(pats.reloads).To(BeEmpty())
		})

		It("goes on when a member cannot be asked, or cannot be read, and looks again", func() {
			create("api-fail", withAPITLS)
			makeCertSecret("api-fail")
			reconcileOnce("api-fail")
			startLeader("api-fail", servesAPITLS("api-fail"))
			pats.served = map[string]*x509.Certificate{testLeaderIP: stale()}
			pats.reloadErr = errors.New("refused")
			Expect(reconcileCert("api-fail")).To(BeFalse())

			pats.servedErr = errors.New("connection refused")
			Expect(reconcileCert("api-fail")).To(BeFalse())
		})

		It("is looked at again soon, by the reconcile, after a member was asked", func() {
			create("api-soon", withAPITLS)
			makeCertSecret("api-soon")
			reconcileOnce("api-soon")
			startLeader("api-soon", servesAPITLS("api-soon"))
			sql.superuserPassword = string(secretOf("api-soon-credentials").Data[secretKeyPassword])
			pats.served = map[string]*x509.Certificate{testLeaderIP: stale()}
			pats.reloads = nil
			Expect(reconcileOnce("api-soon").RequeueAfter).To(Equal(unhealthyRefreshInterval))
			Expect(pats.reloads).To(Equal([]string{testLeaderIP}), "the reconcile itself asked the member to reload")
		})
	})

	Describe("how the primary was started", func() {
		status := func(name string) *dbv1alpha1.PatroniAPIStatus { return get(name).Status.Security.PatroniAPI }

		It("says whether it asks for the password and serves TLS, from the primary's own spec", func() {
			create("api-status", func(c *dbv1alpha1.PostgresCluster) {
				withAPITLS(c)
				c.Spec.Security.PatroniAPI = &dbv1alpha1.PatroniAPISpec{Authentication: true}
			})
			makeCertSecret("api-status")
			reconcileOnce("api-status")
			startLeader("api-status", nil)
			reconcileOnce("api-status")
			Expect(status("api-status")).To(Equal(&dbv1alpha1.PatroniAPIStatus{Authenticated: false, TLS: false}),
				"the primary was started before: it is not what the spec says yet")
		})

		It("says so once it has been replaced", func() {
			create("api-status2", func(c *dbv1alpha1.PostgresCluster) {
				withAPITLS(c)
				c.Spec.Security.PatroniAPI = &dbv1alpha1.PatroniAPISpec{Authentication: true}
			})
			makeCertSecret("api-status2")
			reconcileOnce("api-status2")
			startLeader("api-status2", withEnv(envRESTAPIPassword, envRESTAPICertificate))
			reconcileOnce("api-status2")
			Expect(status("api-status2")).To(Equal(&dbv1alpha1.PatroniAPIStatus{Authenticated: true, TLS: true}))
		})

		It("has nothing to say when nothing was asked for, and clears what it said when it is no longer", func() {
			create("api-status3", withAPIAuth)
			reconcileOnce("api-status3")
			startLeader("api-status3", withEnv(envRESTAPIPassword))
			reconcileOnce("api-status3")
			Expect(status("api-status3")).NotTo(BeNil())

			c := get("api-status3")
			c.Spec.Security = nil
			Expect(k8sClient.Update(ctx, c)).To(Succeed())
			reconcileOnce("api-status3")
			Expect(status("api-status3")).NotTo(BeNil(), "the primary still asks for it until it is replaced")

			pod := &corev1.Pod{}
			Expect(k8sClient.Get(ctx, key("api-status3-0"), pod)).To(Succeed())
			Expect(k8sClient.Delete(ctx, pod, client.GracePeriodSeconds(0))).To(Succeed())
			startLeaderAgain := func() {
				fresh := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "api-status3-0", Namespace: namespace, Labels: selectorLabelsFor("api-status3")},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: testPodImage}}},
				}
				Eventually(func() error { return k8sClient.Create(ctx, fresh) }, 10*time.Second).Should(Succeed())
				DeferCleanup(func() {
					Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, fresh, client.GracePeriodSeconds(0)))).To(Succeed())
				})
				fresh.Status.PodIP = testLeaderIP
				Expect(k8sClient.Status().Update(ctx, fresh)).To(Succeed())
			}
			startLeaderAgain()
			reconcileOnce("api-status3")
			Expect(status("api-status3")).To(BeNil())
		})
	})

	Describe("when the passwords are replaced", func() {
		const creds = "-credentials"
		rotate := func(name string, healthy, rolling bool) {
			GinkgoHelper()
			_, err := rec.reconcileCredentialRotation(ctx, get(name), healthy, rolling)
			Expect(err).NotTo(HaveOccurred())
		}
		annotate := func(secret string, annotations map[string]string) {
			GinkgoHelper()
			s := secretOf(secret)
			if s.Annotations == nil {
				s.Annotations = map[string]string{}
			}
			maps.Copy(s.Annotations, annotations)
			Expect(k8sClient.Update(ctx, s)).To(Succeed())
		}
		setup := func(name string, mutate func(*dbv1alpha1.PostgresCluster)) {
			GinkgoHelper()
			create(name, mutate)
			reconcileOnce(name)
			startLeader(name, nil)
			sql.superuserPassword = string(secretOf(name + creds).Data[secretKeyPassword])
			sql.afterExec = func(statement string) {
				if strings.Contains(statement, `alter role "postgres" password`) {
					sql.superuserPassword = string(secretOf(name + creds).Data[secretKeyPasswordNext])
				}
			}
		}

		It("replaces the API password with them, and keeps the old one for the pods that have not been replaced", func() {
			setup("api-rot", withAPIAuth)
			before := string(secretOf("api-rot" + creds).Data[secretKeyPatroniAPI])
			annotate("api-rot"+creds, map[string]string{annotationRotate: tokenNow})
			rotate("api-rot", true, false)

			after := secretOf("api-rot" + creds)
			Expect(string(after.Data[secretKeyPatroniAPI])).NotTo(Equal(before))
			Expect(string(after.Data[secretKeyPatroniAPI])).To(HavePrefix("pw"))
			Expect(string(after.Data[secretKeyPatroniAPIPrevious])).To(Equal(before))
			Expect(after.Data).NotTo(HaveKey(secretKeyPatroniAPINext), "the waiting password is the password now")
			Expect(after.Annotations[annotationCredentialsVersion]).To(Equal("1"), "the pods are replaced to start with it")
		})

		It("leaves a password alone that nothing asks for", func() {
			setup("api-norot", withAPIAuth)
			c := get("api-norot")
			c.Spec.Security = nil
			Expect(k8sClient.Update(ctx, c)).To(Succeed())
			before := string(secretOf("api-norot" + creds).Data[secretKeyPatroniAPI])
			annotate("api-norot"+creds, map[string]string{annotationRotate: tokenNow})
			rotate("api-norot", true, false)
			after := secretOf("api-norot" + creds)
			Expect(string(after.Data[secretKeyPatroniAPI])).To(Equal(before))
			Expect(after.Data).NotTo(HaveKey(secretKeyPatroniAPIPrevious))
		})

		It("does not make one for a cluster that has none", func() {
			setup("api-none", nil)
			annotate("api-none"+creds, map[string]string{annotationRotate: tokenNow})
			rotate("api-none", true, false)
			Expect(secretOf("api-none" + creds).Data).NotTo(HaveKey(secretKeyPatroniAPI))
			Expect(secretOf("api-none" + creds).Data).NotTo(HaveKey(secretKeyPatroniAPIPrevious))
		})

		It("drops the old password only when no pod is being replaced and every pod has the new one", func() {
			setup("api-drop", withAPIAuth)
			annotate("api-drop"+creds, map[string]string{annotationRotate: tokenNow})
			rotate("api-drop", true, false)
			version := secretOf("api-drop" + creds).Annotations[annotationCredentialsVersion]
			Expect(secretOf("api-drop" + creds).Data).To(HaveKey(secretKeyPatroniAPIPrevious))

			By("not while the pods are being replaced")
			rotate("api-drop", true, true)
			Expect(secretOf("api-drop" + creds).Data).To(HaveKey(secretKeyPatroniAPIPrevious))

			By("not while a pod still has the old version, even if nothing seems to be rolling")
			rotate("api-drop", true, false)
			Expect(secretOf("api-drop" + creds).Data).To(HaveKey(secretKeyPatroniAPIPrevious))

			By("not while the pods are being replaced, though every pod has the new one")
			pod := &corev1.Pod{}
			Expect(k8sClient.Get(ctx, key("api-drop-0"), pod)).To(Succeed())
			pod.Annotations = map[string]string{annotationCredentialsVersion: version}
			Expect(k8sClient.Update(ctx, pod)).To(Succeed())
			// The cluster has one pod, so one pod is every pod. A status write
			// returns the object as the server has it, so the instances are
			// set again for each look.
			oneInstance := func() *dbv1alpha1.PostgresCluster {
				c := get("api-drop")
				c.Spec.Instances = 1
				return c
			}
			_, err := rec.reconcileCredentialRotation(ctx, oneInstance(), true, true)
			Expect(err).NotTo(HaveOccurred())
			Expect(secretOf("api-drop" + creds).Data).To(HaveKey(secretKeyPatroniAPIPrevious))

			By("once every pod has the new one and nothing is being replaced")
			_, err = rec.reconcileCredentialRotation(ctx, oneInstance(), true, false)
			Expect(err).NotTo(HaveOccurred())
			Expect(secretOf("api-drop" + creds).Data).NotTo(HaveKey(secretKeyPatroniAPIPrevious))
		})

		It("does not drop it while fewer pods exist than the cluster has instances", func() {
			setup("api-few", withAPIAuth)
			annotate("api-few"+creds, map[string]string{annotationRotate: tokenNow})
			rotate("api-few", true, false)
			version := secretOf("api-few" + creds).Annotations[annotationCredentialsVersion]
			pod := &corev1.Pod{}
			Expect(k8sClient.Get(ctx, key("api-few-0"), pod)).To(Succeed())
			pod.Annotations = map[string]string{annotationCredentialsVersion: version}
			Expect(k8sClient.Update(ctx, pod)).To(Succeed())
			rotate("api-few", true, false) // the cluster has three instances and one pod
			Expect(secretOf("api-few" + creds).Data).To(HaveKey(secretKeyPatroniAPIPrevious))
		})
	})
})

func indexOf(names []string, name string) int {
	for i, n := range names {
		if n == name {
			return i
		}
	}
	return -1
}
