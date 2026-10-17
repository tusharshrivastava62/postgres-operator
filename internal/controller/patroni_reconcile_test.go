/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/yaml"

	dbv1alpha1 "github.com/tusharshrivastava62/postgres-operator/api/v1alpha1"
)

// testPodImage is the image of the stand-in pods the specs create; no kubelet
// runs in envtest, so it is never pulled.
const testPodImage = "busybox"

// Pure functions first: these need no API server.
var _ = Describe("Patroni configuration", func() {
	newCluster := func() *dbv1alpha1.PostgresCluster {
		return &dbv1alpha1.PostgresCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "unit", Namespace: "default"},
			Spec:       dbv1alpha1.PostgresClusterSpec{Version: "16", Instances: 3},
		}
	}
	dcs := func(c *dbv1alpha1.PostgresCluster) map[string]any {
		return desiredDynamicConfig(c)
	}

	It("uses the CRD defaults when ha is empty", func() {
		got := dcs(newCluster())
		Expect(got).To(HaveKeyWithValue(dcsTTL, int64(30)))
		Expect(got).To(HaveKeyWithValue(dcsLoopWait, int64(10)))
		Expect(got).To(HaveKeyWithValue(dcsRetryTimeout, int64(10)))
		Expect(got).To(HaveKeyWithValue(dcsMaximumLagOnFailover, int64(1048576)))
		Expect(got).To(HaveKeyWithValue(dcsFailsafeMode, true))
		Expect(got).To(HaveKeyWithValue(dcsSynchronousMode, false))
	})

	It("keeps Patroni's timing rule for every allowed failoverTimeout", func() {
		for ttl := int32(20); ttl <= 300; ttl++ {
			c := newCluster()
			c.Spec.HA.FailoverTimeout = ttl
			got := dcs(c)
			loop := got[dcsLoopWait].(int64)
			retry := got[dcsRetryTimeout].(int64)
			Expect(got[dcsTTL]).To(Equal(int64(ttl)))
			Expect(loop).To(BeNumerically(">=", 1), "ttl %d", ttl)
			Expect(retry).To(BeNumerically(">=", 3), "ttl %d", ttl)
			Expect(loop+2*retry).To(BeNumerically("<=", int64(ttl)), "ttl %d", ttl)
		}
	})

	It("passes an explicit zero lag limit through instead of re-defaulting it", func() {
		c := newCluster()
		c.Spec.HA.MaximumLagOnFailover = ptr.To(int64(0))
		Expect(dcs(c)).To(HaveKeyWithValue(dcsMaximumLagOnFailover, int64(0)))
	})

	It("turns synchronous mode on when asked", func() {
		c := newCluster()
		c.Spec.HA.SynchronousMode = true
		Expect(dcs(c)).To(HaveKeyWithValue(dcsSynchronousMode, true))
	})

	It("keeps spec.ha out of the pod template, so editing it never rolls the pods", func() {
		plain := newCluster()
		tuned := newCluster()
		tuned.Spec.HA = dbv1alpha1.HASpec{
			FailoverTimeout: 120, SynchronousMode: true, MaximumLagOnFailover: ptr.To(int64(0)),
		}
		a, err := spiloEnv(plain)
		Expect(err).NotTo(HaveOccurred())
		b, err := spiloEnv(tuned)
		Expect(err).NotTo(HaveOccurred())
		Expect(b).To(Equal(a))
	})

	Describe("diffing against the live configuration", func() {
		It("is empty when everything already matches, despite JSON decoding numbers as floats", func() {
			live := map[string]any{
				dcsTTL: float64(30), dcsLoopWait: float64(10), dcsRetryTimeout: float64(10),
				dcsMaximumLagOnFailover: float64(1048576), dcsFailsafeMode: true, dcsSynchronousMode: false,
				"postgresql": map[string]any{"use_slots": true},
			}
			Expect(configDiff(dcs(newCluster()), live)).To(BeEmpty())
		})

		It("returns only what differs and ignores keys the operator does not manage", func() {
			live := map[string]any{
				dcsTTL: float64(30), dcsLoopWait: float64(10), dcsRetryTimeout: float64(10),
				dcsMaximumLagOnFailover: float64(1048576), dcsFailsafeMode: true, dcsSynchronousMode: false,
				"postgresql": map[string]any{"use_slots": true},
			}
			c := newCluster()
			c.Spec.HA.SynchronousMode = true
			Expect(configDiff(dcs(c), live)).To(Equal(map[string]any{dcsSynchronousMode: true}))
		})

		It("sends a key the live configuration is missing", func() {
			live := map[string]any{dcsTTL: float64(30)}
			Expect(configDiff(dcs(newCluster()), live)).To(HaveKey(dcsFailsafeMode))
		})
	})

	It("labels the cluster so Patroni's own objects can be found for cleanup", func() {
		env, err := spiloEnv(newCluster())
		Expect(err).NotTo(HaveOccurred())
		byName := map[string]corev1.EnvVar{}
		for _, e := range env {
			byName[e.Name] = e
		}
		Expect(byName["SCOPE"].Value).To(Equal("unit"))
		Expect(byName["KUBERNETES_SCOPE_LABEL"].Value).To(Equal(labelAppInstance))
		labels := map[string]string{}
		Expect(yaml.Unmarshal([]byte(byName["KUBERNETES_LABELS"].Value), &labels)).To(Succeed())
		labels[labelAppInstance] = "unit"
		Expect(labels).To(Equal(podSelectorLabels(newCluster())))
		Expect(byName["PGPASSWORD_SUPERUSER"].ValueFrom.SecretKeyRef.Key).To(Equal(secretKeyPassword))
		Expect(byName["PGPASSWORD_STANDBY"].ValueFrom.SecretKeyRef.Key).To(Equal(secretKeyReplicationPassword))
	})
})

var _ = Describe("Image resolution", func() {
	cluster := func(specImage, statusImage string) *dbv1alpha1.PostgresCluster {
		return &dbv1alpha1.PostgresCluster{
			Spec:   dbv1alpha1.PostgresClusterSpec{Version: "17", Image: specImage},
			Status: dbv1alpha1.PostgresClusterStatus{Image: statusImage},
		}
	}

	It("takes the built-in default for a cluster never reconciled before", func() {
		got, ok := resolveImage(cluster("", ""))
		Expect(ok).To(BeTrue())
		Expect(got).To(Equal(defaultImages["17"]))
	})

	It("keeps the recorded image even when the built-in default has moved on", func() {
		got, ok := resolveImage(cluster("", "registry.example/spilo-17:old"))
		Expect(ok).To(BeTrue())
		Expect(got).To(Equal("registry.example/spilo-17:old"))
	})

	It("lets spec.image override the recorded image", func() {
		got, ok := resolveImage(cluster("registry.example/spilo-17:new", "registry.example/spilo-17:old"))
		Expect(ok).To(BeTrue())
		Expect(got).To(Equal("registry.example/spilo-17:new"))
	})

	It("reports no image for a version with no default and nothing recorded", func() {
		c := cluster("", "")
		c.Spec.Version = "14"
		_, ok := resolveImage(c)
		Expect(ok).To(BeFalse())
	})
})

// Everything below runs the reconciler against the envtest API server.
var _ = Describe("PostgresCluster Patroni resources", func() {
	const namespace = "default"
	ctx := context.Background()
	reconciler := func() *PostgresClusterReconciler {
		return &PostgresClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
	}
	reconcileOnce := func(name string) (reconcile.Result, error) {
		return reconciler().Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: namespace}})
	}
	key := func(name string) types.NamespacedName { return types.NamespacedName{Name: name, Namespace: namespace} }

	newCluster := func(name string, mutate ...func(*dbv1alpha1.PostgresCluster)) *dbv1alpha1.PostgresCluster {
		c := &dbv1alpha1.PostgresCluster{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: dbv1alpha1.PostgresClusterSpec{
				Version:   "16",
				Instances: 3,
				Storage:   dbv1alpha1.StorageSpec{Size: resource.MustParse("1Gi")},
				Resources: corev1.ResourceRequirements{
					Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
				},
			},
		}
		for _, m := range mutate {
			m(c)
		}
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		DeferCleanup(func() {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, c))).To(Succeed())
		})
		return c
	}

	Context("a new cluster", func() {
		const name = "patroni-new"

		BeforeEach(func() {
			newCluster(name)
			_, err := reconcileOnce(name)
			Expect(err).NotTo(HaveOccurred())
		})

		It("creates credentials for both the superuser and replication", func() {
			secret := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, key(name+"-credentials"), secret)).To(Succeed())
			DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, secret))).To(Succeed()) })
			Expect(secret.Data).To(HaveKeyWithValue("username", []byte("postgres")))
			Expect(secret.Data[secretKeyPassword]).NotTo(BeEmpty())
			Expect(secret.Data[secretKeyReplicationPassword]).NotTo(BeEmpty())
			Expect(secret.Data[secretKeyPassword]).NotTo(Equal(secret.Data[secretKeyReplicationPassword]))
		})

		It("does not own the credentials Secret, so it outlives the cluster with the data it unlocks", func() {
			secret := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, key(name+"-credentials"), secret)).To(Succeed())
			DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, secret))).To(Succeed()) })
			Expect(secret.OwnerReferences).To(BeEmpty())
		})

		It("creates the account the pods use to reach the API, scoped by a Role", func() {
			Expect(k8sClient.Get(ctx, key(name+"-patroni"), &corev1.ServiceAccount{})).To(Succeed())
			role := &rbacv1.Role{}
			Expect(k8sClient.Get(ctx, key(name+"-patroni"), role)).To(Succeed())
			Expect(role.Rules).To(Equal(patroniRules))
			binding := &rbacv1.RoleBinding{}
			Expect(k8sClient.Get(ctx, key(name+"-patroni"), binding)).To(Succeed())
			Expect(binding.RoleRef.Name).To(Equal(name + "-patroni"))
			Expect(binding.Subjects).To(ConsistOf(rbacv1.Subject{
				Kind: "ServiceAccount", Name: name + "-patroni", Namespace: namespace,
			}))
		})

		It("creates a read-write Service with no selector, so only Patroni decides the primary", func() {
			svc := &corev1.Service{}
			Expect(k8sClient.Get(ctx, key(name), svc)).To(Succeed())
			Expect(svc.Spec.Selector).To(BeEmpty())
			Expect(svc.Spec.Ports).To(HaveLen(1))
		})

		It("creates a read-only Service that selects replicas only", func() {
			svc := &corev1.Service{}
			Expect(k8sClient.Get(ctx, key(name+"-readonly"), svc)).To(Succeed())
			Expect(svc.Spec.Selector).To(HaveKeyWithValue(labelRole, roleReplica))
			Expect(svc.Spec.Selector).To(HaveKeyWithValue(labelAppComponent, componentPostgres))
		})

		It("creates the headless and config Services", func() {
			headless := &corev1.Service{}
			Expect(k8sClient.Get(ctx, key(name+"-headless"), headless)).To(Succeed())
			Expect(headless.Spec.ClusterIP).To(Equal(corev1.ClusterIPNone))
			Expect(headless.Spec.PublishNotReadyAddresses).To(BeTrue())
			config := &corev1.Service{}
			Expect(k8sClient.Get(ctx, key(name+"-config"), config)).To(Succeed())
			Expect(config.Spec.ClusterIP).To(Equal(corev1.ClusterIPNone))
			Expect(config.Spec.Selector).To(BeEmpty())
		})

		It("creates a PodDisruptionBudget allowing one member down at a time", func() {
			pdb := &policyv1.PodDisruptionBudget{}
			Expect(k8sClient.Get(ctx, key(name), pdb)).To(Succeed())
			Expect(pdb.Spec.MaxUnavailable.IntValue()).To(Equal(1))
			Expect(pdb.Spec.Selector.MatchLabels).To(HaveKeyWithValue(labelAppComponent, componentPostgres))
		})

		It("builds a single non-root Spilo container run by Patroni", func() {
			sts := &appsv1.StatefulSet{}
			Expect(k8sClient.Get(ctx, key(name), sts)).To(Succeed())
			Expect(*sts.Spec.Replicas).To(Equal(int32(3)))
			Expect(sts.Spec.PodManagementPolicy).To(Equal(appsv1.ParallelPodManagement))
			Expect(sts.Spec.VolumeClaimTemplates).To(HaveLen(1))
			Expect(sts.Spec.VolumeClaimTemplates[0].Name).To(Equal(dataVolumeName))

			pod := sts.Spec.Template.Spec
			Expect(pod.ServiceAccountName).To(Equal(name + "-patroni"))
			Expect(*pod.TerminationGracePeriodSeconds).To(Equal(int64(60)))
			Expect(*pod.SecurityContext.RunAsUser).To(Equal(spiloUID))
			Expect(*pod.SecurityContext.FSGroup).To(Equal(spiloGID))
			Expect(*pod.SecurityContext.RunAsNonRoot).To(BeTrue())
			Expect(pod.Containers).To(HaveLen(1))
			c := pod.Containers[0]
			Expect(c.Image).To(Equal(defaultImages["16"]))
			Expect(c.SecurityContext.Capabilities.Drop).To(ConsistOf(corev1.Capability("ALL")))
			Expect(*c.SecurityContext.AllowPrivilegeEscalation).To(BeFalse())
			Expect(c.ReadinessProbe.HTTPGet.Path).To(Equal("/readiness"))
			Expect(c.LivenessProbe.HTTPGet.Path).To(Equal("/liveness"))
			Expect(c.Resources.Limits.Memory().String()).To(Equal("1Gi"))
		})

		It("selects only database pods, never backup Job pods", func() {
			sts := &appsv1.StatefulSet{}
			Expect(k8sClient.Get(ctx, key(name), sts)).To(Succeed())
			Expect(sts.Spec.Selector.MatchLabels).To(Equal(podSelectorLabels(&dbv1alpha1.PostgresCluster{
				ObjectMeta: metav1.ObjectMeta{Name: name},
			})))
			Expect(labelsForCluster(&dbv1alpha1.PostgresCluster{ObjectMeta: metav1.ObjectMeta{Name: name}})).
				NotTo(HaveKey(labelAppComponent))
		})

		It("records the image it started with in status", func() {
			cluster := &dbv1alpha1.PostgresCluster{}
			Expect(k8sClient.Get(ctx, key(name), cluster)).To(Succeed())
			Expect(cluster.Status.Image).To(Equal(defaultImages["16"]))
		})

		It("sizes /dev/shm from the memory limit", func() {
			sts := &appsv1.StatefulSet{}
			Expect(k8sClient.Get(ctx, key(name), sts)).To(Succeed())
			shm := sts.Spec.Template.Spec.Volumes[0].EmptyDir
			Expect(shm.Medium).To(Equal(corev1.StorageMediumMemory))
			Expect(shm.SizeLimit.Value()).To(Equal(int64(256 << 20)))
		})

		It("repairs a deleted RoleBinding on the next reconcile", func() {
			binding := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name + "-patroni", Namespace: namespace}}
			Expect(k8sClient.Delete(ctx, binding)).To(Succeed())
			_, err := reconcileOnce(name)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, key(name+"-patroni"), &rbacv1.RoleBinding{})).To(Succeed())
		})

		It("scales the StatefulSet without touching its volume claim templates", func() {
			cluster := &dbv1alpha1.PostgresCluster{}
			Expect(k8sClient.Get(ctx, key(name), cluster)).To(Succeed())
			cluster.Spec.Instances = 1
			Expect(k8sClient.Update(ctx, cluster)).To(Succeed())
			_, err := reconcileOnce(name)
			Expect(err).NotTo(HaveOccurred())
			sts := &appsv1.StatefulSet{}
			Expect(k8sClient.Get(ctx, key(name), sts)).To(Succeed())
			Expect(*sts.Spec.Replicas).To(Equal(int32(1)))
			Expect(sts.Spec.VolumeClaimTemplates).To(HaveLen(1))
		})
	})

	Context("syncing Patroni's dynamic configuration", func() {
		var fake *fakePatroni
		var rec *PostgresClusterReconciler

		BeforeEach(func() {
			fake = &fakePatroni{live: map[string]any{
				dcsTTL: float64(30), dcsLoopWait: float64(10), dcsRetryTimeout: float64(10),
				dcsMaximumLagOnFailover: float64(1048576), dcsFailsafeMode: true, dcsSynchronousMode: false,
			}}
			rec = &PostgresClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Patroni: fake}
		})
		reconcileWithFake := func(name string) (reconcile.Result, error) {
			return rec.Reconcile(ctx, reconcile.Request{NamespacedName: key(name)})
		}
		// startLeader stands in for what Patroni and the kubelet do: a pod with an
		// address, and the leader annotation on the Endpoints named after the cluster.
		startLeader := func(name, ip string) {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: name + "-0", Namespace: namespace, Labels: selectorLabelsFor(name)},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: testPodImage}}},
			}
			Expect(k8sClient.Create(ctx, pod)).To(Succeed())
			DeferCleanup(func() {
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, pod, client.GracePeriodSeconds(0)))).To(Succeed())
			})
			pod.Status.PodIP = ip
			Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())

			endpoints := &patroniEndpoints{ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: namespace, Labels: selectorLabelsFor(name),
				Annotations: map[string]string{annotationLeader: pod.Name},
			}}
			Expect(k8sClient.Create(ctx, endpoints)).To(Succeed())
			DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, endpoints))).To(Succeed()) })
		}

		It("asks to be called again while there is no leader", func() {
			const name = "patroni-noleader"
			newCluster(name)
			result, err := reconcileWithFake(name)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(leaderRequeueDelay))
			Expect(fake.calls).To(BeEmpty())
		})

		It("sends nothing when the live configuration already matches", func() {
			const name = "patroni-insync"
			newCluster(name)
			_, err := reconcileWithFake(name)
			Expect(err).NotTo(HaveOccurred())
			startLeader(name, "10.1.2.3")

			_, err = reconcileWithFake(name)
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.calls).To(ConsistOf("get 10.1.2.3"))
		})

		It("sends only the changed settings to the leader", func() {
			const name = "patroni-changed"
			newCluster(name, func(c *dbv1alpha1.PostgresCluster) {
				c.Spec.HA = dbv1alpha1.HASpec{FailoverTimeout: 60, SynchronousMode: true, MaximumLagOnFailover: ptr.To(int64(1048576))}
			})
			_, err := reconcileWithFake(name)
			Expect(err).NotTo(HaveOccurred())
			startLeader(name, "10.1.2.4")

			_, err = reconcileWithFake(name)
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.patched).To(Equal(map[string]any{
				dcsTTL: int64(60), dcsLoopWait: int64(20), dcsRetryTimeout: int64(20), dcsSynchronousMode: true,
			}))
			Expect(fake.addresses).To(HaveEach("10.1.2.4"))
		})

		It("treats an unreachable leader as not ready yet, not as a failed reconcile", func() {
			const name = "patroni-unreachable"
			newCluster(name)
			_, err := reconcileWithFake(name)
			Expect(err).NotTo(HaveOccurred())
			startLeader(name, "10.1.2.5")
			fake.getErr = fmt.Errorf("connection refused")

			result, err := reconcileWithFake(name)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(leaderRequeueDelay))
		})

		It("fails the reconcile when Patroni rejects a change", func() {
			const name = "patroni-rejected"
			newCluster(name, func(c *dbv1alpha1.PostgresCluster) { c.Spec.HA.SynchronousMode = true })
			_, err := reconcileWithFake(name)
			Expect(err).NotTo(HaveOccurred())
			startLeader(name, "10.1.2.6")
			fake.patchErr = fmt.Errorf("HTTP 422")

			_, err = reconcileWithFake(name)
			Expect(err).To(MatchError(ContainSubstring("updating Patroni configuration")))
		})
	})

	Context("credentials", func() {
		It("adds the replication password to an older Secret without rotating the others", func() {
			const name = "patroni-oldsecret"
			Expect(k8sClient.Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: name + "-credentials", Namespace: namespace},
				Data: map[string][]byte{
					secretKeyUsername: []byte("postgres"),
					secretKeyPassword: []byte("keep-me"),
				},
			})).To(Succeed())
			DeferCleanup(func() {
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{Name: name + "-credentials", Namespace: namespace},
				}))).To(Succeed())
			})
			newCluster(name)

			_, err := reconcileOnce(name)
			Expect(err).NotTo(HaveOccurred())

			secret := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, key(name+"-credentials"), secret)).To(Succeed())
			Expect(secret.Data[secretKeyPassword]).To(Equal([]byte("keep-me")))
			Expect(secret.Data[secretKeyReplicationPassword]).NotTo(BeEmpty())

			replication := secret.Data[secretKeyReplicationPassword]
			_, err = reconcileOnce(name)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, key(name+"-credentials"), secret)).To(Succeed())
			Expect(secret.Data[secretKeyReplicationPassword]).To(Equal(replication), "generated once, never regenerated")
		})
	})

	Context("a spec without a memory limit", func() {
		It("is refused without creating anything, and says why", func() {
			const name = "patroni-nolimit"
			newCluster(name, func(c *dbv1alpha1.PostgresCluster) { c.Spec.Resources = corev1.ResourceRequirements{} })

			_, err := reconcileOnce(name)
			Expect(err).NotTo(HaveOccurred())

			Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key(name), &appsv1.StatefulSet{}))).To(BeTrue())
			Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key(name+"-credentials"), &corev1.Secret{}))).To(BeTrue())
			cluster := &dbv1alpha1.PostgresCluster{}
			Expect(k8sClient.Get(ctx, key(name), cluster)).To(Succeed())
			progressing := meta.FindStatusCondition(cluster.Status.Conditions, "Progressing")
			Expect(progressing).NotTo(BeNil())
			Expect(progressing.Status).To(Equal(metav1.ConditionFalse))
			Expect(progressing.Reason).To(Equal("InvalidSpec"))
			Expect(progressing.Message).To(ContainSubstring("limits.memory"))
		})
	})

	Context("image handling", func() {
		It("does not move an existing cluster when the built-in default changes", func() {
			const name = "patroni-sticky"
			original := defaultImages["16"]
			DeferCleanup(func() { defaultImages["16"] = original })
			newCluster(name)
			_, err := reconcileOnce(name)
			Expect(err).NotTo(HaveOccurred())

			defaultImages["16"] = "ghcr.io/example/spilo-16:newer"
			_, err = reconcileOnce(name)
			Expect(err).NotTo(HaveOccurred())

			sts := &appsv1.StatefulSet{}
			Expect(k8sClient.Get(ctx, key(name), sts)).To(Succeed())
			Expect(sts.Spec.Template.Spec.Containers[0].Image).To(Equal(original))
			cluster := &dbv1alpha1.PostgresCluster{}
			Expect(k8sClient.Get(ctx, key(name), cluster)).To(Succeed())
			Expect(cluster.Status.Image).To(Equal(original))
		})

		It("follows spec.image when it is changed deliberately", func() {
			const name = "patroni-override"
			newCluster(name)
			_, err := reconcileOnce(name)
			Expect(err).NotTo(HaveOccurred())

			cluster := &dbv1alpha1.PostgresCluster{}
			Expect(k8sClient.Get(ctx, key(name), cluster)).To(Succeed())
			cluster.Spec.Image = "ghcr.io/example/spilo-16:pinned"
			Expect(k8sClient.Update(ctx, cluster)).To(Succeed())
			_, err = reconcileOnce(name)
			Expect(err).NotTo(HaveOccurred())

			sts := &appsv1.StatefulSet{}
			Expect(k8sClient.Get(ctx, key(name), sts)).To(Succeed())
			Expect(sts.Spec.Template.Spec.Containers[0].Image).To(Equal("ghcr.io/example/spilo-16:pinned"))
			Expect(k8sClient.Get(ctx, key(name), cluster)).To(Succeed())
			Expect(cluster.Status.Image).To(Equal("ghcr.io/example/spilo-16:pinned"))
		})
	})

	Context("Patroni state left by a deleted cluster", func() {
		// What Patroni writes for a cluster: Endpoints and ConfigMaps labelled
		// with the cluster's labels and owned by nothing.
		patroniObjects := func(name string) []client.Object {
			labels := selectorLabelsFor(name)
			return []client.Object{
				&patroniEndpoints{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels}},
				&patroniEndpoints{ObjectMeta: metav1.ObjectMeta{Name: name + "-config", Namespace: namespace, Labels: labels}},
				&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name + "-failover", Namespace: namespace, Labels: labels}},
			}
		}
		createLeftovers := func(name string) {
			for _, obj := range patroniObjects(name) {
				Expect(k8sClient.Create(ctx, obj)).To(Succeed())
				DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, obj))).To(Succeed()) })
			}
		}
		expectGone := func(name string) {
			for _, obj := range patroniObjects(name) {
				err := k8sClient.Get(ctx, client.ObjectKeyFromObject(obj), obj)
				Expect(apierrors.IsNotFound(err)).To(BeTrue(), "%T %s should have been deleted, got %v", obj, obj.GetName(), err)
			}
		}

		It("is removed before a same-named cluster is created for the first time", func() {
			const name = "patroni-recreate"
			createLeftovers(name)
			newCluster(name)

			_, err := reconcileOnce(name)
			Expect(err).NotTo(HaveOccurred())
			expectGone(name)
		})

		It("leaves another cluster's Patroni state alone", func() {
			const name = "patroni-bystander"
			createLeftovers("patroni-other")
			newCluster(name)

			_, err := reconcileOnce(name)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, key("patroni-other"), &patroniEndpoints{})).To(Succeed())
		})

		It("is not removed from a cluster that is already running", func() {
			const name = "patroni-running"
			newCluster(name)
			_, err := reconcileOnce(name)
			Expect(err).NotTo(HaveOccurred())
			// Patroni's real state for this cluster, created after the first reconcile.
			for _, obj := range patroniObjects(name) {
				Expect(k8sClient.Create(ctx, obj)).To(Succeed())
				DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, obj))).To(Succeed()) })
			}

			_, err = reconcileOnce(name)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, key(name+"-config"), &patroniEndpoints{})).To(Succeed())
		})

		It("waits while the old pods are still terminating", func() {
			const name = "patroni-terminating"
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: name + "-0", Namespace: namespace, Labels: selectorLabelsFor(name)},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: testPodImage}}},
			}
			Expect(k8sClient.Create(ctx, pod)).To(Succeed())
			DeferCleanup(func() {
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, pod, client.GracePeriodSeconds(0)))).To(Succeed())
			})
			createLeftovers(name)
			newCluster(name)

			result, err := reconcileOnce(name)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(leftoverRequeueDelay))
			Expect(k8sClient.Get(ctx, key(name), &patroniEndpoints{})).To(Succeed(), "must not clean up under a live member")
			Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key(name), &appsv1.StatefulSet{}))).To(BeTrue())
		})

		It("is removed after the cluster is deleted and its pods are gone", func() {
			const name = "patroni-deleted"
			cluster := newCluster(name)
			_, err := reconcileOnce(name)
			Expect(err).NotTo(HaveOccurred())
			createLeftovers(name)

			// envtest runs no garbage collector, so delete the StatefulSet the
			// way it would be deleted along with its owner.
			Expect(k8sClient.Delete(ctx, &appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			})).To(Succeed())
			Expect(k8sClient.Delete(ctx, cluster)).To(Succeed())

			_, err = reconcileOnce(name)
			Expect(err).NotTo(HaveOccurred())
			expectGone(name)
		})

		It("keeps the state of a StatefulSet that was orphaned on purpose", func() {
			const name = "patroni-orphan"
			cluster := newCluster(name)
			_, err := reconcileOnce(name)
			Expect(err).NotTo(HaveOccurred())
			createLeftovers(name)

			sts := &appsv1.StatefulSet{}
			Expect(k8sClient.Get(ctx, key(name), sts)).To(Succeed())
			sts.OwnerReferences = nil
			Expect(k8sClient.Update(ctx, sts)).To(Succeed())
			DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, sts))).To(Succeed()) })
			Expect(k8sClient.Delete(ctx, cluster)).To(Succeed())

			result, err := reconcileOnce(name)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeZero())
			Expect(k8sClient.Get(ctx, key(name+"-config"), &patroniEndpoints{})).To(Succeed())
		})
	})
})

// fakePatroni records what the operator sends and serves a canned live
// configuration.
type fakePatroni struct {
	live      map[string]any
	getErr    error
	patchErr  error
	calls     []string
	addresses []string
	patched   map[string]any
}

func (f *fakePatroni) GetConfig(_ context.Context, address string) (map[string]any, error) {
	f.calls = append(f.calls, "get "+address)
	f.addresses = append(f.addresses, address)
	return f.live, f.getErr
}

func (f *fakePatroni) PatchConfig(_ context.Context, address string, patch map[string]any) error {
	f.calls = append(f.calls, "patch "+address)
	f.addresses = append(f.addresses, address)
	f.patched = patch
	return f.patchErr
}
