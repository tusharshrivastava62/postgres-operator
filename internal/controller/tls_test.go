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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	dbv1alpha1 "github.com/tusharshrivastava62/postgres-operator/api/v1alpha1"
)

var _ = Describe("A cluster's certificate", func() {
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
	namesFor := func(name string) []string {
		return []string{name + "." + namespace + ".svc", "*." + name + "-headless." + namespace + ".svc.cluster.local"}
	}
	// leaf is a certificate for the cluster's names, valid now, from the CA.
	leaf := func(issuer *testCert, names []string, notBefore, notAfter time.Duration) *testCert {
		return issue(GinkgoT(), issuer, "orders", false, names, time.Now().Add(notBefore), time.Now().Add(notAfter), serverAuth)
	}
	validFor := func(name string) *testCert { return leaf(ca, namesFor(name), -time.Minute, time.Hour) }

	makeSecret := func(name string, cert, authority *testCert) {
		GinkgoHelper()
		secret := secretFor(GinkgoT(), name, cert, authority)
		secret.Namespace = namespace
		Expect(k8sClient.Create(ctx, secret)).To(Succeed())
		DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, secret))).To(Succeed()) })
	}
	create := func(name string, mutate func(*dbv1alpha1.PostgresCluster)) {
		GinkgoHelper()
		c := plainCluster()
		c.Name = name
		c.Spec.Storage = dbv1alpha1.StorageSpec{Size: resource.MustParse("1Gi")}
		c.Spec.Resources = corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}}
		c.Spec.Security = &dbv1alpha1.SecuritySpec{TLS: &dbv1alpha1.TLSSpec{SecretName: name + "-tls"}}
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
	// startLeader makes the primary's pod. serving says whether it was started
	// with the certificate: whether its spec mounts the Secret.
	startLeader := func(name string, serving bool) {
		GinkgoHelper()
		spec := corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: testPodImage}}}
		if serving {
			spec.Volumes = []corev1.Volume{tlsVolume(name + "-tls")}
		}
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name + "-0", Namespace: namespace, Labels: selectorLabelsFor(name)},
			Spec:       spec,
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
	}
	// withCredentials gives the fake database the superuser's password.
	withCredentials := func(name string) {
		GinkgoHelper()
		secret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, key(credentialsSecretName(&dbv1alpha1.PostgresCluster{ObjectMeta: metav1.ObjectMeta{Name: name}})), secret)).To(Succeed())
		sql.superuserPassword = string(secret.Data[secretKeyPassword])
	}
	condition := func(name string) *metav1.Condition {
		return meta.FindStatusCondition(get(name).Status.Conditions, dbv1alpha1.ConditionTLSReady)
	}
	statefulSet := func(name string) (*appsv1.StatefulSet, error) {
		sts := &appsv1.StatefulSet{}
		return sts, k8sClient.Get(ctx, key(name), sts)
	}
	hasTLSVolume := func(sts *appsv1.StatefulSet) bool {
		for _, v := range sts.Spec.Template.Spec.Volumes {
			if v.Name == tlsVolumeName {
				return true
			}
		}
		return false
	}

	Describe("before the pods are changed", func() {
		It("holds the cluster back, changing nothing, while the Secret does not exist, and goes on once it does", func() {
			create("tls-missing", nil)
			res := reconcileOnce("tls-missing")
			Expect(res.RequeueAfter).To(Equal(tlsRecheckDelay), "nothing watches the Secret, so it is looked at again")
			progressing := meta.FindStatusCondition(get("tls-missing").Status.Conditions, "Progressing")
			Expect(progressing.Status).To(Equal(metav1.ConditionFalse))
			Expect(progressing.Reason).To(Equal(reasonTLSSecretProblem))
			Expect(progressing.Message).To(ContainSubstring(`"tls-missing-tls" does not exist`))
			_, err := statefulSet("tls-missing")
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "no pod is started that could not find its certificate")
			Expect(sql.dials).To(BeEmpty())

			makeSecret("tls-missing-tls", validFor("tls-missing"), ca)
			reconcileOnce("tls-missing")
			sts, err := statefulSet("tls-missing")
			Expect(err).NotTo(HaveOccurred())
			Expect(hasTLSVolume(sts)).To(BeTrue())
		})

		It("holds the cluster back for a certificate that is not valid for its names, and says which", func() {
			create("tls-names", nil)
			makeSecret("tls-names-tls", leaf(ca, []string{"somewhere.else.example.com"}, -time.Minute, time.Hour), ca)
			reconcileOnce("tls-names")
			progressing := meta.FindStatusCondition(get("tls-names").Status.Conditions, "Progressing")
			Expect(progressing.Reason).To(Equal(reasonTLSSecretProblem))
			Expect(progressing.Message).To(ContainSubstring("tls-names.default.svc"))
			_, err := statefulSet("tls-names")
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		})

		It("does not hold back a running cluster for a certificate that has expired: it says so instead", func() {
			create("tls-old", nil)
			makeSecret("tls-old-tls", leaf(ca, namesFor("tls-old"), -48*time.Hour, -time.Hour), ca)
			reconcileOnce("tls-old")
			_, err := statefulSet("tls-old")
			Expect(err).NotTo(HaveOccurred())
		})

		It("gives the pods the certificate and the jobs the CA, and only the CA", func() {
			create("tls-ca", nil)
			makeSecret("tls-ca-tls", validFor("tls-ca"), ca)
			reconcileOnce("tls-ca")

			cm := &corev1.ConfigMap{}
			Expect(k8sClient.Get(ctx, key("tls-ca-tls-ca"), cm)).To(Succeed())
			Expect(cm.Data).To(HaveLen(1))
			Expect(cm.Data).To(HaveKeyWithValue("ca.crt", string(ca.certPEM())))
			Expect(cm.OwnerReferences).To(HaveLen(1), "it goes with the cluster")

			sts, err := statefulSet("tls-ca")
			Expect(err).NotTo(HaveOccurred())
			Expect(hasTLSVolume(sts)).To(BeTrue())
		})

		It("takes it all away again when the certificate is no longer asked for", func() {
			create("tls-gone", nil)
			makeSecret("tls-gone-tls", validFor("tls-gone"), ca)
			reconcileOnce("tls-gone")
			Expect(k8sClient.Get(ctx, key("tls-gone-tls-ca"), &corev1.ConfigMap{})).To(Succeed())

			c := get("tls-gone")
			c.Spec.Security = nil
			Expect(k8sClient.Update(ctx, c)).To(Succeed())
			reconcileOnce("tls-gone")

			err := k8sClient.Get(ctx, key("tls-gone-tls-ca"), &corev1.ConfigMap{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the CA goes")
			sts, err := statefulSet("tls-gone")
			Expect(err).NotTo(HaveOccurred())
			Expect(hasTLSVolume(sts)).To(BeFalse())
			Expect(get("tls-gone").Status.Security.TLS).To(BeNil())
			Expect(condition("tls-gone")).To(BeNil())
		})
	})

	Describe("checking what the primary presents", func() {
		// prepare makes a cluster whose Secret is right and whose primary has
		// the certificate in its spec, and lets the Secrets exist.
		prepare := func(name string, serving bool) {
			GinkgoHelper()
			create(name, nil)
			makeSecret(name+"-tls", validFor(name), ca)
			reconcileOnce(name)
			withCredentials(name)
			startLeader(name, serving)
		}
		tlsDials := func() []SQLTarget {
			var withCheck []SQLTarget
			for _, d := range sql.dials {
				if d.TLS != nil {
					withCheck = append(withCheck, d)
				}
			}
			return withCheck
		}

		It("does not check the certificate of a primary that was started before it, and says it is waiting", func() {
			prepare("tls-wait", false)
			sql.serving = validFor("tls-wait").cert // whatever it presents, nothing is checked yet
			reconcileOnce("tls-wait")

			cond := condition("tls-wait")
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal(dbv1alpha1.TLSWaitingForPods))
			Expect(get("tls-wait").Status.Security.TLS.Verified).To(BeFalse())
			Expect(tlsDials()).To(BeEmpty(), "a pod that does not serve it cannot be checked against it")
		})

		It("checks it once the primary serves it, and records who issued it and when it expires", func() {
			prepare("tls-ok", true)
			served := validFor("tls-ok")
			sql.serving = served.cert
			reconcileOnce("tls-ok")

			cond := condition("tls-ok")
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			Expect(cond.Reason).To(Equal(dbv1alpha1.TLSVerified))
			status := get("tls-ok").Status.Security.TLS
			Expect(status.Verified).To(BeTrue())
			Expect(status.Issuer).To(Equal("orders-ca"))
			Expect(status.NotAfter.UTC()).To(BeTemporally("~", served.cert.NotAfter, time.Second))

			Expect(tlsDials()).NotTo(BeEmpty())
			for _, d := range tlsDials() {
				Expect(d.TLS.ServerName).To(Equal("tls-ok.default.svc"))
			}
			for _, d := range sql.dials {
				Expect(d.TLS).NotTo(BeNil(), "every connection to a primary that serves the certificate checks it: %s", d.User)
			}
		})

		It("refuses a primary whose certificate is from another CA, and says so", func() {
			prepare("tls-other", true)
			sql.serving = leaf(newTestCA(GinkgoT(), "an-impostor"), namesFor("tls-other"), -time.Minute, time.Hour).cert
			reconcileOnce("tls-other")

			cond := condition("tls-other")
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal(dbv1alpha1.TLSVerificationFailed))
			Expect(cond.Message).To(ContainSubstring("not accepted"))
			Expect(get("tls-other").Status.Security.TLS.Verified).To(BeFalse())
		})

		It("refuses a certificate made for another name", func() {
			prepare("tls-name", true)
			sql.serving = leaf(ca, []string{"orders.elsewhere.svc"}, -time.Minute, time.Hour).cert
			reconcileOnce("tls-name")
			Expect(condition("tls-name").Reason).To(Equal(dbv1alpha1.TLSVerificationFailed))
		})

		It("says a certificate has expired, not only that it was refused", func() {
			prepare("tls-expired", true)
			sql.serving = leaf(ca, namesFor("tls-expired"), -48*time.Hour, -time.Hour).cert
			reconcileOnce("tls-expired")

			Expect(condition("tls-expired").Reason).To(Equal(dbv1alpha1.TLSExpired))
			Expect(get("tls-expired").Status.Security.TLS.Verified).To(BeFalse())
		})

		It("keeps what it last saw while there is no primary, and says there is nothing to check", func() {
			prepare("tls-nolead", true)
			sql.serving = validFor("tls-nolead").cert
			reconcileOnce("tls-nolead")
			Expect(get("tls-nolead").Status.Security.TLS.Verified).To(BeTrue())

			endpoints := &patroniEndpoints{ObjectMeta: metav1.ObjectMeta{Name: "tls-nolead", Namespace: namespace}}
			Expect(k8sClient.Delete(ctx, endpoints)).To(Succeed())
			reconcileOnce("tls-nolead")

			Expect(condition("tls-nolead").Status).To(Equal(metav1.ConditionUnknown))
			Expect(condition("tls-nolead").Reason).To(Equal(dbv1alpha1.TLSNoPrimary))
			Expect(get("tls-nolead").Status.Security.TLS.Verified).To(BeTrue(), "a failover is no reason to stop checking")
		})

		It("refuses to connect without checking when the Secret cannot be read while the primary serves it", func() {
			prepare("tls-direct", true)
			c := get("tls-direct")
			Expect(k8sClient.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "tls-direct-tls", Namespace: namespace}})).To(Succeed())

			_, err := sqlTLSFor(ctx, k8sClient, c, testLeaderIP)
			Expect(err).To(MatchError(ContainSubstring("cannot be checked")))

			By("though a pod that does not serve it is simply not checked")
			check, err := sqlTLSFor(ctx, k8sClient, c, "10.99.99.99")
			Expect(err).NotTo(HaveOccurred())
			Expect(check).To(BeNil())
		})

		It("stops, with the reason, when the Secret goes while the primary serves it", func() {
			prepare("tls-lost", true)
			sql.serving = validFor("tls-lost").cert
			reconcileOnce("tls-lost")
			sql.dials = nil

			Expect(k8sClient.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "tls-lost-tls", Namespace: namespace}})).To(Succeed())
			res := reconcileOnce("tls-lost")
			Expect(res.RequeueAfter).To(Equal(tlsRecheckDelay))
			Expect(sql.dials).To(BeEmpty(), "the operator does not fall back to an unchecked connection")
			Expect(meta.FindStatusCondition(get("tls-lost").Status.Conditions, "Progressing").Reason).To(Equal(reasonTLSSecretProblem))
		})
	})

	Describe("the jobs", func() {
		complianceEnv := func(name string) (map[string]string, *corev1.PodSpec) {
			GinkgoHelper()
			cj := &batchv1.CronJob{}
			Expect(k8sClient.Get(ctx, key(name+"-compliance-check"), cj)).To(Succeed())
			pod := cj.Spec.JobTemplate.Spec.Template.Spec
			env := map[string]string{}
			for _, e := range pod.Containers[0].Env {
				env[e.Name] = e.Value
			}
			return env, &pod
		}

		It("check the certificate from the moment the operator has, and not before", func() {
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: auditSecret, Namespace: namespace},
				StringData: map[string]string{secretKeyAccessKey: "id", secretKeySecretKey: testSecretValue},
			}
			if err := k8sClient.Create(ctx, secret); err != nil {
				Expect(apierrors.IsAlreadyExists(err)).To(BeTrue())
			}
			create("tls-jobs", func(c *dbv1alpha1.PostgresCluster) { withCompliance(c, dbv1alpha1.StandardHIPAA) })
			makeSecret("tls-jobs-tls", validFor("tls-jobs"), ca)
			reconcileOnce("tls-jobs")
			withCredentials("tls-jobs")
			startLeader("tls-jobs", true)

			env, _ := complianceEnv("tls-jobs")
			Expect(env).To(HaveKeyWithValue("PGSSLMODE", "require"))
			Expect(env).NotTo(HaveKey("PGSSLROOTCERT"))
			Expect(env).To(HaveKeyWithValue("PGHOST", "tls-jobs"))

			sql.serving = validFor("tls-jobs").cert
			res := reconcileOnce("tls-jobs") // verifies; the jobs are changed on the next look
			Expect(get("tls-jobs").Status.Security.TLS.Verified).To(BeTrue())
			Expect(res.RequeueAfter).To(Equal(unhealthyRefreshInterval), "look again soon to give the jobs the CA")

			reconcileOnce("tls-jobs")
			env, pod := complianceEnv("tls-jobs")
			Expect(env).To(HaveKeyWithValue("PGSSLMODE", "verify-full"))
			Expect(env).To(HaveKeyWithValue("PGSSLROOTCERT", "/tls-ca/ca.crt"))
			Expect(env).To(HaveKeyWithValue("PGHOST", "tls-jobs.default.svc"), "a name the certificate is valid for")
			var source *corev1.ConfigMapVolumeSource
			for _, v := range pod.Volumes {
				if v.Name == tlsCAVolumeName {
					source = v.ConfigMap
				}
			}
			Expect(source).NotTo(BeNil())
			Expect(source.Name).To(Equal("tls-jobs-tls-ca"), "the CA only: the jobs never hold the private key")
			for _, v := range pod.Volumes {
				Expect(v.Secret).To(BeNil(), "no job mounts a Secret as a volume")
			}

			// Taken away, they go back at once.
			c := get("tls-jobs")
			c.Spec.Security = nil
			Expect(k8sClient.Update(ctx, c)).To(Succeed())
			reconcileOnce("tls-jobs")
			env, _ = complianceEnv("tls-jobs")
			Expect(env).To(HaveKeyWithValue("PGHOST", "tls-jobs"))
			Expect(env).To(HaveKeyWithValue("PGSSLMODE", "require"))
			Expect(env).NotTo(HaveKey("PGSSLROOTCERT"))
		})
	})
})
