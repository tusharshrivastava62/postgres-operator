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
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dbv1alpha1 "github.com/tusharshrivastava62/postgres-operator/api/v1alpha1"
)

const (
	annotationStatus    = "status"
	annotationRenewTime = "renewTime"
	annotationTTL       = "ttl"

	// lockGrace is how far past its expiry the leader lock may be before the
	// operator calls it stale. It absorbs clock differences between the node
	// that stamped renewTime and the one running the operator.
	lockGrace = 5 * time.Second

	// Re-observe at this interval. Pod and Endpoints events cover everything
	// that changes a role or readiness, but replication lag drifts without
	// any event, and a leader lock that simply stops being renewed produces
	// none either.
	healthyRefreshInterval   = 30 * time.Second
	unhealthyRefreshInterval = 10 * time.Second

	conditionAvailable          = "Available"
	conditionProgressing        = "Progressing"
	conditionReplicationHealthy = "ReplicationHealthy"
	conditionFailoverInProgress = "FailoverInProgress"
)

// memberStatus is the JSON Patroni keeps in the "status" annotation of each
// member's Pod, refreshed every loop. Reading it needs no network access to
// the member, only the pod watch the operator already has.
type memberStatus struct {
	State            string `json:"state"`
	Role             string `json:"role"`
	XLogLocation     int64  `json:"xlog_location"`
	Timeline         int64  `json:"timeline"`
	ReplicationState string `json:"replication_state"`
}

func (m memberStatus) isPrimary() bool {
	return m.Role == rolePrimary || m.Role == "master"
}

type member struct {
	name   string
	ready  bool
	status memberStatus
	// known is false when the pod has no readable status annotation yet, as
	// while Patroni is still starting.
	known bool
}

func membersOf(pods []corev1.Pod) []member {
	members := make([]member, 0, len(pods))
	for i := range pods {
		pod := &pods[i]
		m := member{name: pod.Name, ready: podReady(pod)}
		if raw := pod.Annotations[annotationStatus]; raw != "" {
			m.known = json.Unmarshal([]byte(raw), &m.status) == nil
		}
		members = append(members, m)
	}
	slices.SortFunc(members, func(a, b member) int { return strings.Compare(a.name, b.name) })
	return members
}

func podReady(pod *corev1.Pod) bool {
	if pod.DeletionTimestamp != nil {
		return false
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// observation is what the cluster looks like right now, derived from
// Patroni's records only. It is a pure function of its inputs so every
// failure shape can be tested without a cluster.
type observation struct {
	// primary is the member that holds a live leader lock and is Ready. It
	// is empty when there is none, whether the cluster is still starting up
	// or a failover is under way.
	primary string
	// lags is bytes behind the primary for each replica that is streaming.
	lags map[string]int64

	replicationHealthy bool
	replicationMessage string
	availableMessage   string
}

// leaderLockFresh reports whether the lock recorded on the Endpoints is still
// within its time to live. The leader annotation outlives a dead primary:
// Patroni only clears it when another member takes over, so on its own it
// would keep naming a primary that has stopped renewing. An unreadable
// timestamp counts as fresh, because guessing "stale" would flap the
// Available condition on a format change.
func leaderLockFresh(annotations map[string]string, now time.Time) bool {
	renewed, err := time.Parse(time.RFC3339Nano, annotations[annotationRenewTime])
	if err != nil {
		return true
	}
	ttl, err := strconv.Atoi(annotations[annotationTTL])
	if err != nil {
		return true
	}
	return now.Before(renewed.Add(time.Duration(ttl) * time.Second).Add(lockGrace))
}

// observeCluster works out the primary and replication health.
//
// The primary is taken from the leader annotation, not from pod labels or the
// role in a pod's own status. A primary that hangs keeps reporting itself as
// primary until its pod is replaced, which is exactly when the answer matters.
func observeCluster(cluster *dbv1alpha1.PostgresCluster, leader string, lockFresh bool, members []member) observation {
	obs := observation{lags: map[string]int64{}}

	var primary *member
	for i := range members {
		if members[i].name == leader {
			primary = &members[i]
		}
	}
	switch {
	case leader == "":
		obs.availableMessage = "no leader has been elected"
	case !lockFresh:
		obs.availableMessage = fmt.Sprintf("leader %s has stopped renewing its lock", leader)
	case primary == nil:
		obs.availableMessage = fmt.Sprintf("leader %s has no pod", leader)
	case !primary.ready:
		obs.availableMessage = fmt.Sprintf("leader %s is not ready", leader)
	default:
		obs.primary = leader
		obs.availableMessage = fmt.Sprintf("primary is %s", leader)
	}

	if obs.primary == "" {
		obs.replicationMessage = "there is no primary to replicate from"
		return obs
	}

	maxLag := defaultMaximumLagOnFailover
	if cluster.Spec.HA.MaximumLagOnFailover != nil {
		maxLag = *cluster.Spec.HA.MaximumLagOnFailover
	}

	var problems []string
	claimants := 0
	replicas := 0
	for _, m := range members {
		if m.known && m.status.isPrimary() {
			claimants++
		}
		if m.name == obs.primary {
			continue
		}
		replicas++
		switch {
		case !m.known:
			problems = append(problems, fmt.Sprintf("%s has not reported its state", m.name))
		case m.status.State != "running":
			problems = append(problems, fmt.Sprintf("%s is %s", m.name, m.status.State))
		case m.status.ReplicationState != "streaming":
			problems = append(problems, fmt.Sprintf("%s is not streaming (%s)", m.name, orUnknown(m.status.ReplicationState)))
		default:
			lag := max(primary.status.XLogLocation-m.status.XLogLocation, 0)
			obs.lags[m.name] = lag
			if lag > maxLag {
				problems = append(problems, fmt.Sprintf("%s is %d bytes behind, over the limit of %d", m.name, lag, maxLag))
			}
		}
	}
	if claimants > 1 {
		problems = append(problems, fmt.Sprintf("%d members report themselves as primary", claimants))
	}
	if want := int(cluster.Spec.Instances) - 1; replicas < want {
		problems = append(problems, fmt.Sprintf("%d of %d replicas exist", replicas, want))
	}

	if len(problems) == 0 {
		obs.replicationHealthy = true
		obs.replicationMessage = fmt.Sprintf("%d replica(s) streaming within the lag limit", replicas)
	} else {
		obs.replicationMessage = strings.Join(problems, "; ")
	}
	return obs
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// reconcileStatus works out the cluster's conditions, primary and replica lag
// from what Patroni records, and writes them to .status. It returns whether
// the cluster is fully healthy, which sets how soon to look again.
//
// It only calls Status().Update when something actually changed
// (meta.SetStatusCondition reports whether it did, and the other fields are
// compared). Skipping the no-op write matters because replication lag moves
// constantly: without it every reconcile would write status.
func (r *PostgresClusterReconciler) reconcileStatus(ctx context.Context, cluster *dbv1alpha1.PostgresCluster) (bool, error) {
	sts := &appsv1.StatefulSet{}
	stsErr := r.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: cluster.Name}, sts)
	if stsErr != nil && !apierrors.IsNotFound(stsErr) {
		return false, stsErr
	}
	var ready int32
	if stsErr == nil {
		ready = sts.Status.ReadyReplicas
	}
	desired := cluster.Spec.Instances
	instancesReady.WithLabelValues(cluster.Namespace, cluster.Name).Set(float64(ready))

	leader, lockFresh, err := r.leaderOf(ctx, cluster)
	if err != nil {
		return false, err
	}
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(cluster.Namespace), client.MatchingLabels(podSelectorLabels(cluster))); err != nil {
		return false, err
	}
	obs := observeCluster(cluster, leader, lockFresh, membersOf(pods.Items))

	previousPrimary := cluster.Status.PrimaryInstance
	r.recordPrimaryMetrics(cluster, obs)

	available := metav1.Condition{
		Type: conditionAvailable, Status: metav1.ConditionFalse, Reason: "NoPrimary", Message: obs.availableMessage,
	}
	replication := metav1.Condition{
		Type: conditionReplicationHealthy, Status: metav1.ConditionUnknown, Reason: "NoPrimary", Message: obs.replicationMessage,
	}
	failover := metav1.Condition{
		Type: conditionFailoverInProgress, Status: metav1.ConditionFalse, Reason: "PrimaryAvailable",
		Message: "a primary is serving",
	}
	if obs.primary != "" {
		available.Status = metav1.ConditionTrue
		available.Reason = "PrimaryReady"
		replication.Reason = "ReplicasLagging"
		replication.Status = metav1.ConditionFalse
		if obs.replicationHealthy {
			replication.Status = metav1.ConditionTrue
			replication.Reason = "ReplicasStreaming"
		}
	} else {
		// A cluster that has never had a primary is starting up, not failing
		// over. One that had a primary a moment ago, or is already flagged,
		// is mid-failover.
		failoverBefore := meta.FindStatusCondition(cluster.Status.Conditions, conditionFailoverInProgress)
		wasFailingOver := failoverBefore != nil && failoverBefore.Status == metav1.ConditionTrue
		if previousPrimary != "" || wasFailingOver {
			failover.Status = metav1.ConditionTrue
			failover.Reason = "ElectingLeader"
			failover.Message = obs.availableMessage
		} else {
			failover.Reason = "Bootstrapping"
			failover.Message = "waiting for the first primary"
		}
	}

	progressing := metav1.Condition{
		Type:    conditionProgressing,
		Status:  metav1.ConditionTrue,
		Reason:  "ScalingUp",
		Message: fmt.Sprintf("%d/%d instances ready", ready, desired),
	}
	if desired > 0 && ready >= desired {
		progressing.Status = metav1.ConditionFalse
		progressing.Reason = "ReconcileComplete"
	}

	degraded, err := r.backupCondition(ctx, cluster)
	if err != nil {
		return false, err
	}

	changed := meta.SetStatusCondition(&cluster.Status.Conditions, available)
	for _, c := range []metav1.Condition{progressing, replication, failover, degraded} {
		changed = meta.SetStatusCondition(&cluster.Status.Conditions, c) || changed
	}
	if cluster.Status.PrimaryInstance != obs.primary {
		cluster.Status.PrimaryInstance = obs.primary
		changed = true
	}
	if !sameLags(cluster.Status.ReplicationLagBytes, obs.lags) {
		cluster.Status.ReplicationLagBytes = obs.lags
		if len(obs.lags) == 0 {
			cluster.Status.ReplicationLagBytes = nil
		}
		changed = true
	}
	if cluster.Status.ObservedGeneration != cluster.Generation {
		cluster.Status.ObservedGeneration = cluster.Generation
		changed = true
	}

	healthy := obs.primary != "" && obs.replicationHealthy
	if !changed {
		return healthy, nil
	}
	return healthy, r.Status().Update(ctx, cluster)
}

func sameLags(a, b map[string]int64) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// leaderOf reads the leader recorded by Patroni on the Endpoints named after
// the cluster, and whether its lock is still live.
func (r *PostgresClusterReconciler) leaderOf(ctx context.Context, cluster *dbv1alpha1.PostgresCluster) (string, bool, error) {
	endpoints := &corev1.Endpoints{} //nolint:staticcheck // Patroni's Kubernetes DCS stores the leader on Endpoints
	err := r.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: cluster.Name}, endpoints)
	if apierrors.IsNotFound(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return endpoints.Annotations[annotationLeader], leaderLockFresh(endpoints.Annotations, time.Now()), nil
}

// recordPrimaryMetrics publishes the primary and lag gauges, and counts a
// change of primary. A change is a new primary that differs from the last one
// this process saw; the very first primary, and one seen again after an
// operator restart, are not counted.
func (r *PostgresClusterReconciler) recordPrimaryMetrics(cluster *dbv1alpha1.PostgresCluster, obs observation) {
	ns, name := cluster.Namespace, cluster.Name

	hasPrimary.WithLabelValues(ns, name).Set(boolToFloat(obs.primary != ""))

	// Replace the per-replica series wholesale so a replica that went away
	// stops being reported instead of freezing at its last value.
	replicationLagBytes.DeletePartialMatch(map[string]string{labelNamespace: ns, labelName: name})
	for replica, lag := range obs.lags {
		replicationLagBytes.WithLabelValues(ns, name, replica).Set(float64(lag))
	}

	if obs.primary == "" {
		return
	}
	key := ns + "/" + name
	if last, ok := r.lastPrimary.Swap(key, obs.primary); ok && last != obs.primary {
		primaryChangesTotal.WithLabelValues(ns, name).Inc()
	}
}

// forgetMetrics drops every series of a deleted cluster, so deleted clusters
// do not accumulate in the metrics endpoint for the life of the process.
func (r *PostgresClusterReconciler) forgetMetrics(key client.ObjectKey) {
	match := map[string]string{labelNamespace: key.Namespace, labelName: key.Name}
	reconcileTotal.DeletePartialMatch(match)
	reconcileDurationSeconds.DeletePartialMatch(match)
	instancesReady.DeletePartialMatch(match)
	backupLastSuccessTimestampSeconds.DeletePartialMatch(match)
	hasPrimary.DeletePartialMatch(match)
	primaryChangesTotal.DeletePartialMatch(match)
	replicationLagBytes.DeletePartialMatch(match)
	r.lastPrimary.Delete(key.Namespace + "/" + key.Name)
}

func boolToFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
