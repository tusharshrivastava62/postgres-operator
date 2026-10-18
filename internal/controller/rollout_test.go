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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	dbv1alpha1 "github.com/tusharshrivastava62/postgres-operator/api/v1alpha1"
)

const (
	oldRevision = "rev-1"
	newRevision = "rev-2"

	// Pod names in the planning table, where the cluster is called c.
	pod1 = "c-1"
	pod2 = "c-2"
)

// rp builds a rolloutPod for the table below: ready, not terminating.
func rp(name string, ordinal int, revision string) rolloutPod {
	return rolloutPod{name: name, ordinal: ordinal, revision: revision, ready: true}
}

var _ = Describe("Planning a rolling update", func() {
	three := func(rev0, rev1, rev2 string) []rolloutPod {
		return []rolloutPod{rp("c-0", 0, rev0), rp(pod1, 1, rev1), rp(pod2, 2, rev2)}
	}
	lags := map[string]int64{"c-0": 0, pod1: 0, pod2: 0}
	plan := func(primary string, pods []rolloutPod, instances int32, healthy bool, lag map[string]int64) rolloutAction {
		return planRollout(newRevision, primary, pods, instances, healthy, "replica c-1 is not streaming", lag)
	}

	It("does nothing when every pod is on the current revision", func() {
		Expect(plan("c-0", three(newRevision, newRevision, newRevision), 3, true, lags).kind).To(Equal(rolloutNone))
	})

	It("does nothing when no revision is known yet", func() {
		got := planRollout("", "c-0", three(oldRevision, oldRevision, oldRevision), 3, true, "", lags)
		Expect(got.kind).To(Equal(rolloutNone))
	})

	It("starts with the replica of highest ordinal, never the primary", func() {
		got := plan("c-0", three(oldRevision, oldRevision, oldRevision), 3, true, lags)
		Expect(got).To(Equal(rolloutAction{kind: rolloutDeletePod, pod: pod2, message: "updating replica c-2"}))
	})

	It("skips a primary with a higher ordinal than the replicas", func() {
		got := plan(pod2, three(oldRevision, oldRevision, oldRevision), 3, true, lags)
		Expect(got.kind).To(Equal(rolloutDeletePod))
		Expect(got.pod).To(Equal(pod1))
	})

	It("moves to the next replica once the first is back on the new revision", func() {
		got := plan("c-0", three(oldRevision, oldRevision, newRevision), 3, true, lags)
		Expect(got.kind).To(Equal(rolloutDeletePod))
		Expect(got.pod).To(Equal(pod1))
	})

	It("hands leadership over before touching an outdated primary", func() {
		got := plan("c-0", three(oldRevision, newRevision, newRevision), 3, true, map[string]int64{pod1: 300, pod2: 100})
		Expect(got.kind).To(Equal(rolloutSwitchover))
		Expect(got.pod).To(Equal("c-0"))
		Expect(got.candidate).To(Equal(pod2), "the replica with the least lag")
	})

	It("breaks a tie in lag by name so the choice is stable", func() {
		got := plan("c-0", three(oldRevision, newRevision, newRevision), 3, true, map[string]int64{pod1: 0, pod2: 0})
		Expect(got.candidate).To(Equal(pod1))
	})

	It("finishes by replacing the old primary once it is a replica", func() {
		got := plan(pod2, three(oldRevision, newRevision, newRevision), 3, true, lags)
		Expect(got.kind).To(Equal(rolloutDeletePod))
		Expect(got.pod).To(Equal("c-0"))
	})

	It("waits for a replica that is not streaming rather than taking another down", func() {
		got := plan("c-0", three(oldRevision, oldRevision, oldRevision), 3, false, lags)
		Expect(got.kind).To(Equal(rolloutWait))
		Expect(got.message).To(ContainSubstring("replication to be healthy"))
	})

	It("waits for a pod that is not ready yet", func() {
		pods := three(oldRevision, oldRevision, newRevision)
		pods[2].ready = false
		got := plan("c-0", pods, 3, true, lags)
		Expect(got.kind).To(Equal(rolloutWait))
		Expect(got.message).To(ContainSubstring("c-2 to become ready"))
	})

	It("waits for a pod that is still terminating", func() {
		pods := three(oldRevision, oldRevision, oldRevision)
		pods[1].deleting = true
		Expect(plan("c-0", pods, 3, true, lags).kind).To(Equal(rolloutWait))
	})

	It("waits for a missing pod to be created", func() {
		got := plan("c-0", three(oldRevision, oldRevision, oldRevision)[:2], 3, true, lags)
		Expect(got.kind).To(Equal(rolloutWait))
		Expect(got.message).To(ContainSubstring("to be created"))
	})

	It("waits while there is no primary", func() {
		Expect(plan("", three(oldRevision, oldRevision, oldRevision), 3, true, lags).kind).To(Equal(rolloutWait))
	})

	It("waits when no replica is streaming to take over", func() {
		got := plan("c-0", three(oldRevision, newRevision, newRevision), 3, true, map[string]int64{})
		Expect(got.kind).To(Equal(rolloutWait))
		Expect(got.message).To(ContainSubstring("streaming replica"))
	})

	It("replaces a lone instance, which cannot avoid an outage", func() {
		got := plan("c-0", []rolloutPod{rp("c-0", 0, oldRevision)}, 1, true, map[string]int64{})
		Expect(got.kind).To(Equal(rolloutDeletePod))
		Expect(got.pod).To(Equal("c-0"))
	})

	It("stops after the first replica when the new revision cannot start", func() {
		pods := three(oldRevision, oldRevision, newRevision)
		pods[2].ready = false
		Expect(plan("c-0", pods, 3, true, lags).kind).To(Equal(rolloutWait))
	})

	It("reads the pod ordinal from its name", func() {
		pod := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "my-db-12", Labels: map[string]string{labelRevision: "r"}}}
		got := rolloutPodsOf([]corev1.Pod{pod})
		Expect(got[0].ordinal).To(Equal(12))
		Expect(got[0].revision).To(Equal("r"))
	})
})

// The same behaviour through the reconciler: pods in the API server, a fake
// Patroni, and a StatefulSet whose revisions are set by hand, since envtest
// runs no StatefulSet controller.
var _ = Describe("Rolling update through the reconciler", func() {
	const namespace = "default"
	ctx := context.Background()
	key := func(name string) types.NamespacedName { return types.NamespacedName{Name: name, Namespace: namespace} }

	var (
		fake *fakePatroni
		rec  *PostgresClusterReconciler
	)
	BeforeEach(func() {
		fake = &fakePatroni{live: map[string]any{}, switchoverErr: map[string]error{}}
		rec = &PostgresClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Patroni: fake}
	})
	reconcileOnce := func(name string) {
		_, err := rec.Reconcile(ctx, reconcile.Request{NamespacedName: key(name)})
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
	}

	// setup creates a cluster, reconciles it once so its StatefulSet exists,
	// then adds ready pods, a leader record, and gives the StatefulSet an
	// update revision that every pod with an older label is behind.
	setup := func(name string, instances int32, revisions map[string]string) {
		c := &dbv1alpha1.PostgresCluster{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: dbv1alpha1.PostgresClusterSpec{
				Version: "16", Instances: instances,
				Storage:   dbv1alpha1.StorageSpec{Size: resource.MustParse("1Gi")},
				Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}},
			},
		}
		ExpectWithOffset(1, k8sClient.Create(ctx, c)).To(Succeed())
		DeferCleanup(func() { ExpectWithOffset(1, client.IgnoreNotFound(k8sClient.Delete(ctx, c))).To(Succeed()) })
		reconcileOnce(name)

		for i := range instances {
			podName := fmt.Sprintf("%s-%d", name, i)
			role := "replica"
			if i == 0 {
				role = "primary"
			}
			labels := selectorLabelsFor(name)
			labels[labelRevision] = revisions[podName]
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: podName, Namespace: namespace, Labels: labels,
					Annotations: map[string]string{annotationStatus: fmt.Sprintf(
						`{"state":"running","role":"%s","xlog_location":100,"replication_state":"streaming","timeline":1}`, role)},
				},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: testPodImage}}},
			}
			ExpectWithOffset(1, k8sClient.Create(ctx, pod)).To(Succeed())
			DeferCleanup(func() {
				ExpectWithOffset(1, client.IgnoreNotFound(k8sClient.Delete(ctx, pod, client.GracePeriodSeconds(0)))).To(Succeed())
			})
			pod.Status.PodIP = fmt.Sprintf("10.9.0.%d", i+1)
			pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
			ExpectWithOffset(1, k8sClient.Status().Update(ctx, pod)).To(Succeed())
		}

		endpoints := &patroniEndpoints{ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: namespace, Labels: selectorLabelsFor(name),
			Annotations: map[string]string{annotationLeader: name + "-0"},
		}}
		ExpectWithOffset(1, k8sClient.Create(ctx, endpoints)).To(Succeed())
		DeferCleanup(func() { ExpectWithOffset(1, client.IgnoreNotFound(k8sClient.Delete(ctx, endpoints))).To(Succeed()) })
		ExpectWithOffset(1, k8sClient.Get(ctx, key(name), endpoints)).To(Succeed())
		endpoints.Annotations[annotationRenewTime] = metav1.Now().UTC().Format("2006-01-02T15:04:05.999999+00:00")
		endpoints.Annotations[annotationTTL] = "30"
		ExpectWithOffset(1, k8sClient.Update(ctx, endpoints)).To(Succeed())

		sts := &appsv1.StatefulSet{}
		ExpectWithOffset(1, k8sClient.Get(ctx, key(name), sts)).To(Succeed())
		sts.Status.ObservedGeneration = sts.Generation
		sts.Status.UpdateRevision = newRevision
		ExpectWithOffset(1, k8sClient.Status().Update(ctx, sts)).To(Succeed())
	}
	podExists := func(name string) bool {
		pod := &corev1.Pod{}
		err := k8sClient.Get(ctx, key(name), pod)
		return err == nil && pod.DeletionTimestamp == nil
	}

	It("creates the StatefulSet with the OnDelete strategy so the operator controls the order", func() {
		const name = "roll-strategy"
		setup(name, 1, map[string]string{name + "-0": newRevision})
		sts := &appsv1.StatefulSet{}
		Expect(k8sClient.Get(ctx, key(name), sts)).To(Succeed())
		Expect(sts.Spec.UpdateStrategy.Type).To(Equal(appsv1.OnDeleteStatefulSetStrategyType))
	})

	It("deletes the outdated replica of highest ordinal first and leaves the primary alone", func() {
		const name = "roll-first"
		setup(name, 3, map[string]string{name + "-0": oldRevision, name + "-1": oldRevision, name + "-2": oldRevision})
		reconcileOnce(name)

		Expect(podExists(name + "-2")).To(BeFalse())
		Expect(podExists(name + "-1")).To(BeTrue())
		Expect(podExists(name + "-0")).To(BeTrue())
		Expect(fake.switchovers).To(BeEmpty())
	})

	It("does not delete anything while a replica is not ready", func() {
		const name = "roll-wait"
		setup(name, 3, map[string]string{name + "-0": oldRevision, name + "-1": oldRevision, name + "-2": newRevision})
		pod := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, key(name+"-2"), pod)).To(Succeed())
		pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())

		reconcileOnce(name)
		Expect(podExists(name + "-1")).To(BeTrue())
		Expect(podExists(name + "-0")).To(BeTrue())
	})

	It("switches leadership to an updated replica, and deletes no pod, when only the primary is outdated", func() {
		const name = "roll-switch"
		setup(name, 3, map[string]string{name + "-0": oldRevision, name + "-1": newRevision, name + "-2": newRevision})
		reconcileOnce(name)

		Expect(fake.switchovers).To(ConsistOf(name + "-0->" + name + "-1"))
		Expect(podExists(name+"-0")).To(BeTrue(), "the primary is not deleted until it has been handed over")
	})

	It("lets Patroni choose when it refuses the replica asked for", func() {
		const name = "roll-refused"
		setup(name, 3, map[string]string{name + "-0": oldRevision, name + "-1": newRevision, name + "-2": newRevision})
		fake.switchoverErr[name+"-1"] = fmt.Errorf("HTTP 412: candidate is not the synchronous standby")
		reconcileOnce(name)

		Expect(fake.switchovers).To(Equal([]string{name + "-0->" + name + "-1", name + "-0->"}))
	})

	It("reports a rolling update in the Progressing condition while pods are outdated", func() {
		const name = "roll-status"
		setup(name, 3, map[string]string{name + "-0": oldRevision, name + "-1": oldRevision, name + "-2": oldRevision})
		reconcileOnce(name)

		c := &dbv1alpha1.PostgresCluster{}
		Expect(k8sClient.Get(ctx, key(name), c)).To(Succeed())
		var reason string
		for _, cond := range c.Status.Conditions {
			if cond.Type == "Progressing" {
				reason = cond.Reason
			}
		}
		Expect(reason).To(Equal("RollingUpdate"))
	})

	It("asks to be called again soon while a rollout is in progress", func() {
		const name = "roll-requeue"
		setup(name, 3, map[string]string{name + "-0": oldRevision, name + "-1": oldRevision, name + "-2": oldRevision})
		result, err := rec.Reconcile(ctx, reconcile.Request{NamespacedName: key(name)})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(Equal(rolloutRequeueDelay))
	})
})
