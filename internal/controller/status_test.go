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
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	dbv1alpha1 "github.com/tusharshrivastava62/postgres-operator/api/v1alpha1"
)

func readyMember(name, role string, xlog int64, replication string) member {
	return member{name: name, ready: true, known: true, status: memberStatus{
		State: "running", Role: role, XLogLocation: xlog, ReplicationState: replication, Timeline: 1,
	}}
}

var _ = Describe("Observing a cluster", func() {
	three := func() *dbv1alpha1.PostgresCluster {
		return &dbv1alpha1.PostgresCluster{Spec: dbv1alpha1.PostgresClusterSpec{Instances: 3}}
	}
	healthy := func() []member {
		return []member{
			readyMember("c-0", "primary", 1000, ""),
			readyMember(pod1, "replica", 900, "streaming"),
			readyMember(pod2, "replica", 1000, "streaming"),
		}
	}

	It("finds the primary and reports every replica's lag when all is well", func() {
		obs := observeCluster(three(), "c-0", true, healthy())
		Expect(obs.primary).To(Equal("c-0"))
		Expect(obs.replicationHealthy).To(BeTrue())
		Expect(obs.lags).To(Equal(map[string]int64{pod1: 100, pod2: 0}))
	})

	It("never reports negative lag when a replica reads ahead of the primary's last report", func() {
		members := healthy()
		members[1].status.XLogLocation = 1100
		Expect(observeCluster(three(), "c-0", true, members).lags).To(HaveKeyWithValue(pod1, int64(0)))
	})

	It("has no primary when nothing has been elected", func() {
		obs := observeCluster(three(), "", true, healthy())
		Expect(obs.primary).To(BeEmpty())
		Expect(obs.replicationHealthy).To(BeFalse())
		Expect(obs.availableMessage).To(ContainSubstring("no leader"))
	})

	It("does not trust a leader whose lock has stopped being renewed", func() {
		obs := observeCluster(three(), "c-0", false, healthy())
		Expect(obs.primary).To(BeEmpty())
		Expect(obs.availableMessage).To(ContainSubstring("stopped renewing"))
	})

	It("does not trust a leader whose pod is missing or not ready", func() {
		Expect(observeCluster(three(), "c-9", true, healthy()).primary).To(BeEmpty())
		members := healthy()
		members[0].ready = false
		obs := observeCluster(three(), "c-0", true, members)
		Expect(obs.primary).To(BeEmpty())
		Expect(obs.availableMessage).To(ContainSubstring("not ready"))
	})

	It("follows the leader annotation, not the role the pods claim", func() {
		members := healthy()
		// c-0 hung and still calls itself primary; c-1 has been elected.
		members[1].status.Role = "primary"
		obs := observeCluster(three(), pod1, true, members)
		Expect(obs.primary).To(Equal(pod1))
		Expect(obs.replicationHealthy).To(BeFalse(), "two members claiming to be primary is not healthy")
		Expect(obs.replicationMessage).To(ContainSubstring("2 members report themselves as primary"))
	})

	It("flags a replica that is not streaming, and omits it from the lag map", func() {
		members := healthy()
		members[1].status.ReplicationState = "starting"
		obs := observeCluster(three(), "c-0", true, members)
		Expect(obs.replicationHealthy).To(BeFalse())
		Expect(obs.replicationMessage).To(ContainSubstring("c-1 is not streaming"))
		Expect(obs.lags).NotTo(HaveKey(pod1))
	})

	It("flags a replica further behind than maximumLagOnFailover", func() {
		c := three()
		c.Spec.HA.MaximumLagOnFailover = ptr.To(int64(50))
		obs := observeCluster(c, "c-0", true, healthy())
		Expect(obs.replicationHealthy).To(BeFalse())
		Expect(obs.replicationMessage).To(ContainSubstring("c-1 is 100 bytes behind"))
		Expect(obs.lags).To(HaveKeyWithValue(pod1, int64(100)), "still reported, just not healthy")
	})

	It("flags a replica that has not reported its state yet", func() {
		members := healthy()
		members[2] = member{name: pod2, ready: false}
		obs := observeCluster(three(), "c-0", true, members)
		Expect(obs.replicationHealthy).To(BeFalse())
		Expect(obs.replicationMessage).To(ContainSubstring("c-2 has not reported"))
	})

	It("flags missing replicas", func() {
		obs := observeCluster(three(), "c-0", true, healthy()[:2])
		Expect(obs.replicationHealthy).To(BeFalse())
		Expect(obs.replicationMessage).To(ContainSubstring("1 of 2 replicas exist"))
	})

	It("is healthy with a single instance and a primary", func() {
		one := &dbv1alpha1.PostgresCluster{Spec: dbv1alpha1.PostgresClusterSpec{Instances: 1}}
		obs := observeCluster(one, "c-0", true, []member{readyMember("c-0", "primary", 10, "")})
		Expect(obs.primary).To(Equal("c-0"))
		Expect(obs.replicationHealthy).To(BeTrue())
	})

	Describe("leader lock freshness", func() {
		now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
		lock := func(renewed time.Time, ttl string) map[string]string {
			return map[string]string{annotationRenewTime: renewed.Format("2006-01-02T15:04:05.999999-07:00"), annotationTTL: ttl}
		}

		It("is fresh while inside the ttl", func() {
			Expect(leaderLockFresh(lock(now.Add(-20*time.Second), "30"), now)).To(BeTrue())
		})
		It("tolerates a few seconds of clock difference past the ttl", func() {
			Expect(leaderLockFresh(lock(now.Add(-33*time.Second), "30"), now)).To(BeTrue())
		})
		It("is stale well past the ttl", func() {
			Expect(leaderLockFresh(lock(now.Add(-60*time.Second), "30"), now)).To(BeFalse())
		})
		It("reads Patroni's real timestamp format", func() {
			annotations := map[string]string{annotationRenewTime: "2026-10-17T10:34:25.224233+00:00", annotationTTL: "30"}
			Expect(leaderLockFresh(annotations, time.Date(2026, 10, 17, 10, 34, 40, 0, time.UTC))).To(BeTrue())
			Expect(leaderLockFresh(annotations, time.Date(2026, 10, 17, 10, 36, 0, 0, time.UTC))).To(BeFalse())
		})
		It("counts an unreadable lock as fresh rather than flapping", func() {
			Expect(leaderLockFresh(map[string]string{}, now)).To(BeTrue())
			Expect(leaderLockFresh(map[string]string{annotationRenewTime: "garbage", annotationTTL: "30"}, now)).To(BeTrue())
		})
	})
})

var _ = Describe("Which events wake the controller", func() {
	podWith := func(ready bool, status string) *corev1.Pod {
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Annotations: map[string]string{annotationStatus: status}}}
		cond := corev1.ConditionFalse
		if ready {
			cond = corev1.ConditionTrue
		}
		pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: cond}}
		return pod
	}
	update := func(o, n client.Object) event.UpdateEvent { return event.UpdateEvent{ObjectOld: o, ObjectNew: n} }
	const replicaAt = `{"state":"running","role":"replica","xlog_location":%d,"replication_state":"streaming","timeline":1}`

	It("ignores a pod whose only change is the WAL position", func() {
		old := podWith(true, fmt.Sprintf(replicaAt, 100))
		Expect(podChangePredicate().Update(update(old, podWith(true, fmt.Sprintf(replicaAt, 9000))))).To(BeFalse())
	})
	It("wakes for a change of role", func() {
		old := podWith(true, fmt.Sprintf(replicaAt, 100))
		promoted := podWith(true, `{"state":"running","role":"primary","xlog_location":100,"timeline":2}`)
		Expect(podChangePredicate().Update(update(old, promoted))).To(BeTrue())
	})
	It("wakes for a change of readiness", func() {
		Expect(podChangePredicate().Update(update(podWith(true, "{}"), podWith(false, "{}")))).To(BeTrue())
	})
	It("wakes for a replica that stops streaming", func() {
		old := podWith(true, fmt.Sprintf(replicaAt, 100))
		broken := podWith(true, `{"state":"running","role":"replica","xlog_location":100,"replication_state":"starting","timeline":1}`)
		Expect(podChangePredicate().Update(update(old, broken))).To(BeTrue())
	})
	It("wakes for the Endpoints only when the leader changes", func() {
		ep := func(leader, renewed string) *patroniEndpoints {
			return &patroniEndpoints{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
				annotationLeader: leader, annotationRenewTime: renewed,
			}}}
		}
		Expect(leaderChangePredicate().Update(update(ep("a", "1"), ep("a", "2")))).To(BeFalse())
		Expect(leaderChangePredicate().Update(update(ep("a", "1"), ep("b", "1")))).To(BeTrue())
	})
	It("maps database pods to their cluster and ignores backup pods", func() {
		db := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Labels: selectorLabelsFor("demo")}}
		Expect(clusterForObject(context.Background(), db)).To(Equal([]reconcile.Request{
			{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "demo"}},
		}))
		backup := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Labels: map[string]string{
			labelAppName: appNameValue, labelAppInstance: "demo",
		}}}
		Expect(clusterForObject(context.Background(), backup)).To(BeEmpty())
	})
})

// The same logic through the reconciler and the API server.
var _ = Describe("PostgresCluster status from Patroni", func() {
	const namespace = "default"
	ctx := context.Background()
	key := func(name string) types.NamespacedName { return types.NamespacedName{Name: name, Namespace: namespace} }

	var rec *PostgresClusterReconciler
	BeforeEach(func() {
		rec = &PostgresClusterReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(),
			Patroni: &fakePatroni{live: map[string]any{}},
		}
	})
	reconcileOnce := func(name string) {
		_, err := rec.Reconcile(ctx, reconcile.Request{NamespacedName: key(name)})
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
	}
	newCluster := func(name string, instances int32) {
		c := &dbv1alpha1.PostgresCluster{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: dbv1alpha1.PostgresClusterSpec{
				Version: "16", Instances: instances,
				Storage:   dbv1alpha1.StorageSpec{Size: resource.MustParse("1Gi")},
				Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}},
			},
		}
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, c))).To(Succeed()) })
		// The first reconcile creates the StatefulSet and records the image.
		// Pods are added after it, as in life: a cluster that already has pods
		// when it is first seen is treated as a leftover and waited out.
		reconcileOnce(name)
	}
	// member creates a pod the way Patroni and the kubelet leave it.
	member := func(name, role string, xlog int64, ready bool) *corev1.Pod {
		replication := ""
		if role == "replica" {
			replication = "streaming"
		}
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: namespace, Labels: selectorLabelsFor(name[:len(name)-2]),
				Annotations: map[string]string{annotationStatus: fmt.Sprintf(
					`{"state":"running","role":"%s","xlog_location":%d,"replication_state":"%s","timeline":1}`, role, xlog, replication)},
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: testPodImage}}},
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		DeferCleanup(func() {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, pod, client.GracePeriodSeconds(0)))).To(Succeed())
		})
		status := corev1.ConditionFalse
		if ready {
			status = corev1.ConditionTrue
		}
		pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: status}}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
		return pod
	}
	setLeader := func(name, leader string) {
		endpoints := &patroniEndpoints{}
		err := k8sClient.Get(ctx, key(name), endpoints)
		if err != nil {
			endpoints = &patroniEndpoints{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: selectorLabelsFor(name)}}
			Expect(k8sClient.Create(ctx, endpoints)).To(Succeed())
			DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, endpoints))).To(Succeed()) })
		}
		endpoints.Annotations = map[string]string{
			annotationLeader:    leader,
			annotationRenewTime: time.Now().UTC().Format(time.RFC3339Nano),
			annotationTTL:       "30",
		}
		Expect(k8sClient.Update(ctx, endpoints)).To(Succeed())
	}
	get := func(name string) *dbv1alpha1.PostgresCluster {
		c := &dbv1alpha1.PostgresCluster{}
		ExpectWithOffset(1, k8sClient.Get(ctx, key(name), c)).To(Succeed())
		return c
	}
	cond := func(c *dbv1alpha1.PostgresCluster, t string) *metav1.Condition {
		got := meta.FindStatusCondition(c.Status.Conditions, t)
		ExpectWithOffset(1, got).NotTo(BeNil(), "condition %s", t)
		return got
	}

	It("is waiting for a first primary, not failing over, before any leader exists", func() {
		const name = "status-boot"
		newCluster(name, 3)
		reconcileOnce(name)

		c := get(name)
		Expect(c.Status.PrimaryInstance).To(BeEmpty())
		Expect(cond(c, "Available").Status).To(Equal(metav1.ConditionFalse))
		Expect(cond(c, "Available").Reason).To(Equal("NoPrimary"))
		Expect(cond(c, "ReplicationHealthy").Status).To(Equal(metav1.ConditionUnknown))
		Expect(cond(c, "FailoverInProgress").Status).To(Equal(metav1.ConditionFalse))
		Expect(cond(c, "FailoverInProgress").Reason).To(Equal("Bootstrapping"))
		Expect(testutil.ToFloat64(hasPrimary.WithLabelValues(namespace, name))).To(Equal(float64(0)))
	})

	It("reports the primary, replica lag and healthy replication once Patroni has settled", func() {
		const name = "status-up"
		newCluster(name, 3)
		member(name+"-0", "primary", 5000, true)
		member(name+"-1", "replica", 4800, true)
		member(name+"-2", "replica", 5000, true)
		setLeader(name, name+"-0")
		reconcileOnce(name)

		c := get(name)
		Expect(c.Status.PrimaryInstance).To(Equal(name + "-0"))
		Expect(c.Status.ReplicationLagBytes).To(Equal(map[string]int64{name + "-1": 200, name + "-2": 0}))
		Expect(cond(c, "Available").Status).To(Equal(metav1.ConditionTrue))
		Expect(cond(c, "Available").Reason).To(Equal("PrimaryReady"))
		Expect(cond(c, "ReplicationHealthy").Status).To(Equal(metav1.ConditionTrue))
		Expect(cond(c, "FailoverInProgress").Status).To(Equal(metav1.ConditionFalse))
		Expect(testutil.ToFloat64(hasPrimary.WithLabelValues(namespace, name))).To(Equal(float64(1)))
		Expect(testutil.ToFloat64(replicationLagBytes.WithLabelValues(namespace, name, name+"-1"))).To(Equal(float64(200)))
	})

	It("does not rewrite status when nothing changed", func() {
		const name = "status-quiet"
		newCluster(name, 1)
		member(name+"-0", "primary", 1, true)
		setLeader(name, name+"-0")
		reconcileOnce(name)
		before := get(name).ResourceVersion

		reconcileOnce(name)
		Expect(get(name).ResourceVersion).To(Equal(before))
	})

	It("moves through a failover: flags it, then counts the change of primary", func() {
		const name = "status-failover"
		newCluster(name, 3)
		member(name+"-0", "primary", 5000, true)
		member(name+"-1", "replica", 5000, true)
		member(name+"-2", "replica", 5000, true)
		setLeader(name, name+"-0")
		reconcileOnce(name)
		Expect(get(name).Status.PrimaryInstance).To(Equal(name + "-0"))
		changesBefore := testutil.ToFloat64(primaryChangesTotal.WithLabelValues(namespace, name))

		By("losing the leader")
		setLeader(name, "")
		reconcileOnce(name)
		c := get(name)
		Expect(c.Status.PrimaryInstance).To(BeEmpty())
		Expect(cond(c, "Available").Status).To(Equal(metav1.ConditionFalse))
		Expect(cond(c, "FailoverInProgress").Status).To(Equal(metav1.ConditionTrue))
		Expect(cond(c, "FailoverInProgress").Reason).To(Equal("ElectingLeader"))
		Expect(testutil.ToFloat64(hasPrimary.WithLabelValues(namespace, name))).To(Equal(float64(0)))

		By("staying flagged while the election drags on")
		reconcileOnce(name)
		Expect(cond(get(name), "FailoverInProgress").Status).To(Equal(metav1.ConditionTrue))

		By("a replica taking over")
		setLeader(name, name+"-1")
		reconcileOnce(name)
		c = get(name)
		Expect(c.Status.PrimaryInstance).To(Equal(name + "-1"))
		Expect(cond(c, "Available").Status).To(Equal(metav1.ConditionTrue))
		Expect(cond(c, "FailoverInProgress").Status).To(Equal(metav1.ConditionFalse))
		Expect(testutil.ToFloat64(primaryChangesTotal.WithLabelValues(namespace, name))).To(Equal(changesBefore + 1))
	})

	It("does not count the same primary reappearing as a change", func() {
		const name = "status-flap"
		newCluster(name, 1)
		member(name+"-0", "primary", 1, true)
		setLeader(name, name+"-0")
		reconcileOnce(name)
		before := testutil.ToFloat64(primaryChangesTotal.WithLabelValues(namespace, name))

		setLeader(name, "")
		reconcileOnce(name)
		setLeader(name, name+"-0")
		reconcileOnce(name)
		Expect(testutil.ToFloat64(primaryChangesTotal.WithLabelValues(namespace, name))).To(Equal(before))
	})

	It("says replication is unhealthy when a replica lags too far", func() {
		const name = "status-lag"
		newCluster(name, 2)
		member(name+"-0", "primary", 10_000_000, true)
		member(name+"-1", "replica", 1_000, true)
		setLeader(name, name+"-0")
		reconcileOnce(name)

		c := get(name)
		Expect(cond(c, "Available").Status).To(Equal(metav1.ConditionTrue), "writes still work")
		Expect(cond(c, "ReplicationHealthy").Status).To(Equal(metav1.ConditionFalse))
		Expect(cond(c, "ReplicationHealthy").Message).To(ContainSubstring("behind"))
	})

	It("stops reporting a cluster's metrics after it is deleted", func() {
		const name = "status-forget"
		newCluster(name, 1)
		member(name+"-0", "primary", 1, true)
		setLeader(name, name+"-0")
		reconcileOnce(name)
		before := testutil.CollectAndCount(hasPrimary)
		Expect(before).To(BeNumerically(">=", 1))

		Expect(k8sClient.Delete(ctx, get(name))).To(Succeed())
		reconcileOnce(name)

		Expect(testutil.CollectAndCount(hasPrimary)).To(Equal(before - 1))
	})
})
