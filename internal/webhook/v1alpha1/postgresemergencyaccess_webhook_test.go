/*
Copyright 2026.

Licensed under the Business Source License 1.1 (the "License"). You may not
use this file except in compliance with the License, a copy of which is in the
LICENSE file at the root of this repository. Production use requires a
commercial license from the Licensor. On the Change Date (2030-10-17) the
work becomes available under the Change License (Apache-2.0).
*/

package v1alpha1

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	dbv1alpha1 "github.com/tusharshrivastava62/postgres-operator/api/v1alpha1"
)

const (
	defaultNamespace = "default"
	aliceUser        = "alice"
	enabledCluster   = "ea-enabled"
	disabledCluster  = "ea-disabled"
	reasonText       = "orders database is out of connections and the application is down"
)

// requestAs makes a request as a user, in the context the webhook gets.
func requestAs(ctx context.Context, user string) context.Context {
	req := admission.Request{}
	req.UserInfo = authenticationv1.UserInfo{Username: user}
	return admission.NewContextWithRequest(ctx, req)
}

func newRequest(name, cluster string) *dbv1alpha1.PostgresEmergencyAccess {
	return &dbv1alpha1.PostgresEmergencyAccess{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: defaultNamespace},
		Spec: dbv1alpha1.PostgresEmergencyAccessSpec{
			ClusterRef: dbv1alpha1.ClusterReference{Name: cluster},
			Duration:   metav1.Duration{Duration: 30 * time.Minute},
			Reason:     reasonText,
		},
	}
}

var _ = Describe("PostgresEmergencyAccess admission", func() {
	var validator *PostgresEmergencyAccessCustomValidator

	cluster := func(name string, access *dbv1alpha1.EmergencyAccessSpec) {
		GinkgoHelper()
		c := &dbv1alpha1.PostgresCluster{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: defaultNamespace},
			Spec: dbv1alpha1.PostgresClusterSpec{
				Version: "17", Storage: dbv1alpha1.StorageSpec{Size: resource.MustParse("1Gi")},
				Resources:       corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}},
				EmergencyAccess: access,
			},
		}
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, c))).To(Succeed()) })
	}

	BeforeEach(func() {
		validator = &PostgresEmergencyAccessCustomValidator{Reader: k8sClient}
	})

	Describe("recording who asked", func() {
		It("writes the name of the user making the call", func() {
			request := newRequest("stamp", enabledCluster)
			Expect((&PostgresEmergencyAccessCustomDefaulter{}).Default(requestAs(ctx, "alice@example.com"), request)).To(Succeed())
			Expect(request.Annotations).To(HaveKeyWithValue(dbv1alpha1.AnnotationRequestedBy, "alice@example.com"))
		})

		It("writes over a name the requester put there", func() {
			request := newRequest("forged", enabledCluster)
			request.Annotations = map[string]string{dbv1alpha1.AnnotationRequestedBy: "the-cto", "other": "kept"}
			Expect((&PostgresEmergencyAccessCustomDefaulter{}).Default(requestAs(ctx, "mallory"), request)).To(Succeed())
			Expect(request.Annotations).To(HaveKeyWithValue(dbv1alpha1.AnnotationRequestedBy, "mallory"))
			Expect(request.Annotations).To(HaveKeyWithValue("other", "kept"))
		})

		It("fails when it cannot tell who is asking, so nobody is named by default", func() {
			err := (&PostgresEmergencyAccessCustomDefaulter{}).Default(ctx, newRequest("anon", enabledCluster))
			Expect(err).To(MatchError(ContainSubstring("could not tell who is asking")))
		})
	})

	Describe("a new request", func() {
		BeforeEach(func() {
			cluster(enabledCluster, &dbv1alpha1.EmergencyAccessSpec{Enabled: true, MaxDuration: &metav1.Duration{Duration: time.Hour}})
			cluster(disabledCluster, &dbv1alpha1.EmergencyAccessSpec{})
			Eventually(func() error {
				return k8sClient.Get(ctx, client.ObjectKey{Namespace: defaultNamespace, Name: enabledCluster}, &dbv1alpha1.PostgresCluster{})
			}).Should(Succeed())
		})
		stamped := func(name, cluster string) *dbv1alpha1.PostgresEmergencyAccess {
			r := newRequest(name, cluster)
			r.Annotations = map[string]string{dbv1alpha1.AnnotationRequestedBy: aliceUser}
			return r
		}

		It("is admitted for a cluster that allows it, with the warning about the log first", func() {
			warnings, err := validator.ValidateCreate(ctx, stamped("ok", enabledCluster))
			Expect(err).NotTo(HaveOccurred())
			Expect(warnings).To(HaveLen(2))
			Expect(warnings[0]).To(HavePrefix("WARNING: While you are escalated"))
			Expect(warnings[0]).To(ContainSubstring("clear text"))
			Expect(warnings[1]).To(ContainSubstring("30m0s"))
			Expect(warnings[1]).To(ContainSubstring(aliceUser))
		})

		It("is refused for a cluster that does not allow it, saying what to set", func() {
			_, err := validator.ValidateCreate(ctx, stamped("no", disabledCluster))
			Expect(err).To(MatchError(ContainSubstring("spec.emergencyAccess.enabled")))
		})

		It("is refused for a cluster that does not exist", func() {
			_, err := validator.ValidateCreate(ctx, stamped("none", "ea-nowhere"))
			Expect(err).To(MatchError(ContainSubstring("spec.clusterRef.name")))
		})

		It("is refused for longer than the cluster allows", func() {
			request := stamped("long", enabledCluster)
			request.Spec.Duration = metav1.Duration{Duration: 2 * time.Hour}
			_, err := validator.ValidateCreate(ctx, request)
			Expect(err).To(MatchError(ContainSubstring("spec.duration")))
			Expect(err).To(MatchError(ContainSubstring("1h0m0s")))
		})

		It("is refused with a reason that says nothing", func() {
			request := stamped("terse", enabledCluster)
			request.Spec.Reason = "urgent"
			_, err := validator.ValidateCreate(ctx, request)
			Expect(err).To(MatchError(ContainSubstring("spec.reason")))
		})

		It("is refused when nobody is named, whatever else is right", func() {
			request := newRequest("unnamed", enabledCluster)
			_, err := validator.ValidateCreate(ctx, request)
			Expect(err).To(MatchError(ContainSubstring("requester could not be identified")))
		})

		It("reports every problem at once", func() {
			request := newRequest("many", disabledCluster)
			request.Spec.Reason = "no"
			_, err := validator.ValidateCreate(ctx, request)
			Expect(err).To(MatchError(And(
				ContainSubstring("requester could not be identified"), ContainSubstring("does not allow"), ContainSubstring("spec.reason"))))
		})
	})

	Describe("a request that exists", func() {
		It("cannot have its spec changed", func() {
			old := newRequest("fixed", enabledCluster)
			old.Annotations = map[string]string{dbv1alpha1.AnnotationRequestedBy: aliceUser}
			changed := old.DeepCopy()
			changed.Spec.Duration = metav1.Duration{Duration: 8 * time.Hour}
			_, err := validator.ValidateUpdate(ctx, old, changed)
			Expect(err).To(MatchError(ContainSubstring("a request cannot be changed")))
		})

		It("cannot be taken over by changing who made it", func() {
			old := newRequest("owned", enabledCluster)
			old.Annotations = map[string]string{dbv1alpha1.AnnotationRequestedBy: aliceUser}
			taken := old.DeepCopy()
			taken.Annotations[dbv1alpha1.AnnotationRequestedBy] = "bob"
			_, err := validator.ValidateUpdate(ctx, old, taken)
			Expect(err).To(MatchError(ContainSubstring("who made a request cannot be changed")))

			removed := old.DeepCopy()
			delete(removed.Annotations, dbv1alpha1.AnnotationRequestedBy)
			_, err = validator.ValidateUpdate(ctx, old, removed)
			Expect(err).To(HaveOccurred())
		})

		It("can still be given a finalizer and labels, which the operator and people do", func() {
			old := newRequest("labelled", enabledCluster)
			old.Annotations = map[string]string{dbv1alpha1.AnnotationRequestedBy: aliceUser}
			updated := old.DeepCopy()
			updated.Finalizers = []string{"db.youroperator.io/emergency-access"}
			updated.Labels = map[string]string{"ticket": "INC-1234"}
			_, err := validator.ValidateUpdate(ctx, old, updated)
			Expect(err).NotTo(HaveOccurred())
		})

		It("can always be deleted", func() {
			_, err := validator.ValidateDelete(ctx, newRequest("gone", enabledCluster))
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("through the real API server and the deployed webhooks", func() {
		as := func(user string) client.Client {
			GinkgoHelper()
			config := rest.CopyConfig(cfg)
			// In a group that may do anything, so what is tested is the webhooks and not RBAC.
			config.Impersonate = rest.ImpersonationConfig{UserName: user, Groups: []string{"system:masters"}}
			c, err := client.New(config, client.Options{Scheme: scheme.Scheme})
			Expect(err).NotTo(HaveOccurred())
			return c
		}
		BeforeEach(func() {
			cluster(enabledCluster, &dbv1alpha1.EmergencyAccessSpec{Enabled: true})
			Eventually(func() error {
				return k8sClient.Get(ctx, client.ObjectKey{Namespace: defaultNamespace, Name: enabledCluster}, &dbv1alpha1.PostgresCluster{})
			}).Should(Succeed())
		})

		It("stamps the user who made the call, over a name they wrote", func() {
			request := newRequest("through-api", enabledCluster)
			request.Annotations = map[string]string{dbv1alpha1.AnnotationRequestedBy: "the-cto"}
			Eventually(func() error { return as("alice@example.com").Create(ctx, request) }, 30*time.Second, time.Second).Should(Succeed())
			DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, request))).To(Succeed()) })

			stored := &dbv1alpha1.PostgresEmergencyAccess{}
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(request), stored)).To(Succeed())
			Expect(stored.Annotations).To(HaveKeyWithValue(dbv1alpha1.AnnotationRequestedBy, "alice@example.com"))
			Expect(stored.Spec.Duration.Duration).To(Equal(30 * time.Minute))
		})

		It("gives a request with no duration the default, and keeps it unchanged afterwards", func() {
			request := newRequest("through-api-default", enabledCluster)
			request.Spec.Duration = metav1.Duration{}
			Expect(as(aliceUser).Create(ctx, request)).To(Succeed())
			DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, request))).To(Succeed()) })
			stored := &dbv1alpha1.PostgresEmergencyAccess{}
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(request), stored)).To(Succeed())
			Expect(stored.Spec.Duration.Duration).To(Equal(time.Hour))

			By("refusing another user's attempt to take it over")
			stored.Annotations[dbv1alpha1.AnnotationRequestedBy] = "bob"
			Expect(as("bob").Update(ctx, stored)).NotTo(Succeed())

			By("refusing a change to what it asks for")
			stored = &dbv1alpha1.PostgresEmergencyAccess{}
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(request), stored)).To(Succeed())
			stored.Spec.Reason = "something else entirely, to get more time"
			Expect(as(aliceUser).Update(ctx, stored)).NotTo(Succeed())

			By("allowing a label, which does not change who or what")
			stored = &dbv1alpha1.PostgresEmergencyAccess{}
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(request), stored)).To(Succeed())
			stored.Labels = map[string]string{"ticket": "INC-1"}
			Expect(as("bob").Update(ctx, stored)).To(Succeed())
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(request), stored)).To(Succeed())
			Expect(stored.Annotations).To(HaveKeyWithValue(dbv1alpha1.AnnotationRequestedBy, aliceUser), "bob editing a label did not become the requester")
		})

		It("refuses a request the cluster would not grant, before it exists", func() {
			Expect(as(aliceUser).Create(ctx, newRequest("through-api-nowhere", "ea-nowhere"))).NotTo(Succeed())
			err := k8sClient.Get(ctx, client.ObjectKey{Namespace: defaultNamespace, Name: "through-api-nowhere"}, &dbv1alpha1.PostgresEmergencyAccess{})
			Expect(client.IgnoreNotFound(err)).To(Succeed())
			Expect(err).To(HaveOccurred())
		})
	})
})
