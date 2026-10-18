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
	"slices"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	dbv1alpha1 "github.com/tusharshrivastava62/postgres-operator/api/v1alpha1"
)

// labelRevision is set by the StatefulSet controller on each pod to the
// revision of the pod template it was created from.
const labelRevision = "controller-revision-hash"

// rolloutRequeueDelay is how soon to look again while pods are being
// replaced. Pod readiness events usually wake the controller sooner.
const rolloutRequeueDelay = 5 * time.Second

// A StatefulSet left to its default rolling update replaces pods from the
// highest ordinal down, which restarts the primary whenever it happens to be
// in the way: Patroni then needs the full failover timeout to promote a
// replica, and the database is unwritable meanwhile. The StatefulSet is
// therefore set to OnDelete and this file does the rolling update itself, in
// the order that avoids that outage: replicas first, one at a time, each only
// when the cluster is healthy again; then a Patroni switchover to an updated
// replica; then the old primary, which is by now a replica.

type rolloutKind int

const (
	rolloutNone rolloutKind = iota
	// rolloutWait means pods are out of date but it is not yet safe to touch
	// another one.
	rolloutWait
	rolloutDeletePod
	rolloutSwitchover
)

type rolloutAction struct {
	kind rolloutKind
	// pod is the pod to delete, or for a switchover the current primary.
	pod string
	// candidate is the replica to hand leadership to.
	candidate string
	message   string
}

type rolloutPod struct {
	name     string
	ordinal  int
	revision string
	ready    bool
	deleting bool
}

func rolloutPodsOf(pods []corev1.Pod) []rolloutPod {
	out := make([]rolloutPod, 0, len(pods))
	for i := range pods {
		pod := &pods[i]
		ordinal := -1
		if idx := strings.LastIndex(pod.Name, "-"); idx >= 0 {
			if n, err := strconv.Atoi(pod.Name[idx+1:]); err == nil {
				ordinal = n
			}
		}
		out = append(out, rolloutPod{
			name:     pod.Name,
			ordinal:  ordinal,
			revision: pod.Labels[labelRevision],
			ready:    podReady(pod),
			deleting: pod.DeletionTimestamp != nil,
		})
	}
	return out
}

// outdatedPods counts the pods not yet on the StatefulSet's current template.
// With no revision known there is nothing to compare, so nothing is outdated.
func outdatedPods(updateRevision string, pods []rolloutPod) int {
	if updateRevision == "" {
		return 0
	}
	n := 0
	for _, p := range pods {
		if p.revision != updateRevision {
			n++
		}
	}
	return n
}

// planRollout decides the single next step. It is a pure function of what is
// observed, so every ordering rule can be tested without a cluster.
//
// It never takes a step while anything is unsettled: a pod still starting or
// terminating, a missing pod, no primary, or replicas not streaming. Taking
// the next replica down then could leave no standby for a failover, and a new
// revision that cannot start (a bad image) stops the rollout after the first
// replica instead of working through the whole cluster.
func planRollout(
	updateRevision, primary string, pods []rolloutPod, instances int32,
	replicationHealthy bool, replicationMessage string, lags map[string]int64,
) rolloutAction {
	if outdatedPods(updateRevision, pods) == 0 {
		return rolloutAction{}
	}
	wait := func(format string, args ...any) rolloutAction {
		return rolloutAction{kind: rolloutWait, message: fmt.Sprintf(format, args...)}
	}

	for _, p := range pods {
		switch {
		case p.deleting:
			return wait("waiting for %s to finish terminating", p.name)
		case !p.ready:
			return wait("waiting for %s to become ready", p.name)
		}
	}
	if int32(len(pods)) < instances {
		return wait("waiting for %d of %d pods to be created", int(instances)-len(pods), instances)
	}
	if primary == "" {
		return wait("waiting for a primary")
	}

	// A lone instance has nobody to hand over to; replacing it is an outage
	// by nature.
	if len(pods) == 1 {
		return rolloutAction{kind: rolloutDeletePod, pod: pods[0].name,
			message: fmt.Sprintf("replacing %s, the only instance", pods[0].name)}
	}
	if !replicationHealthy {
		return wait("waiting for replication to be healthy: %s", replicationMessage)
	}

	var outdatedReplicas []rolloutPod
	primaryOutdated := false
	for _, p := range pods {
		if p.revision == updateRevision {
			continue
		}
		if p.name == primary {
			primaryOutdated = true
			continue
		}
		outdatedReplicas = append(outdatedReplicas, p)
	}

	if len(outdatedReplicas) > 0 {
		// Highest ordinal first, as a StatefulSet would.
		slices.SortFunc(outdatedReplicas, func(a, b rolloutPod) int { return b.ordinal - a.ordinal })
		next := outdatedReplicas[0]
		return rolloutAction{kind: rolloutDeletePod, pod: next.name,
			message: fmt.Sprintf("updating replica %s", next.name)}
	}
	if !primaryOutdated {
		return rolloutAction{}
	}

	// Only the primary is left. Hand leadership to the replica that is
	// furthest ahead, all of which are updated by now.
	candidate := ""
	for _, p := range pods {
		if p.name == primary {
			continue
		}
		lag, streaming := lags[p.name]
		if !streaming {
			continue
		}
		if candidate == "" || lag < lags[candidate] || (lag == lags[candidate] && p.name < candidate) {
			candidate = p.name
		}
	}
	if candidate == "" {
		return wait("waiting for a streaming replica to take over from %s", primary)
	}
	return rolloutAction{kind: rolloutSwitchover, pod: primary, candidate: candidate,
		message: fmt.Sprintf("switching leadership from %s to %s before updating it", primary, candidate)}
}

// reconcileRollout carries out one step of the rolling update. It reports
// whether to look again soon.
func (r *PostgresClusterReconciler) reconcileRollout(
	ctx context.Context, cluster *dbv1alpha1.PostgresCluster, view clusterView,
) (bool, error) {
	log := logf.FromContext(ctx)

	sts := &appsv1.StatefulSet{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: cluster.Name}, sts); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	// The StatefulSet controller computes its revisions after each change;
	// until it has seen the latest one they describe the previous template.
	// Its status update wakes this controller again, so there is nothing to
	// poll for.
	if sts.Status.ObservedGeneration < sts.Generation {
		return false, nil
	}

	action := planRollout(
		sts.Status.UpdateRevision, view.obs.primary, rolloutPodsOf(view.pods), cluster.Spec.Instances,
		view.obs.replicationHealthy, view.obs.replicationMessage, view.obs.lags)

	switch action.kind {
	case rolloutNone:
		return false, nil
	case rolloutWait:
		log.V(1).Info("rolling update waiting", "reason", action.message)
		return true, nil
	case rolloutDeletePod:
		pod := podNamed(view.pods, action.pod)
		if pod == nil {
			return true, nil
		}
		log.Info("rolling update", "step", action.message)
		// The precondition keeps a pod that was already replaced since the
		// plan was made from being deleted a second time.
		err := r.Delete(ctx, pod, client.Preconditions{UID: &pod.UID})
		if err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
			return false, fmt.Errorf("deleting pod %s: %w", pod.Name, err)
		}
		return true, nil
	case rolloutSwitchover:
		leader := podNamed(view.pods, action.pod)
		if leader == nil || leader.Status.PodIP == "" {
			return true, nil
		}
		log.Info("rolling update", "step", action.message)
		api := r.Patroni
		if api == nil {
			api = defaultPatroni
		}
		err := api.Switchover(ctx, leader.Status.PodIP, action.pod, action.candidate)
		if err != nil {
			// Patroni may refuse a particular candidate (in synchronous mode
			// only the synchronous standby is eligible). Let it choose.
			log.Info("switchover to the chosen replica failed, letting Patroni choose", "error", err.Error())
			err = api.Switchover(ctx, leader.Status.PodIP, action.pod, "")
		}
		if err != nil {
			log.Info("switchover failed, will retry", "error", err.Error())
		}
		return true, nil
	}
	return false, nil
}

func podNamed(pods []corev1.Pod, name string) *corev1.Pod {
	for i := range pods {
		if pods[i].Name == name {
			return &pods[i]
		}
	}
	return nil
}
