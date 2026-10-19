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
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
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
func (r *PostgresClusterReconciler) reconcileStatus(ctx context.Context, cluster *dbv1alpha1.PostgresCluster) (clusterView, error) {
	sts := &appsv1.StatefulSet{}
	stsErr := r.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: cluster.Name}, sts)
	if stsErr != nil && !apierrors.IsNotFound(stsErr) {
		return clusterView{}, stsErr
	}
	var ready int32
	var updateRevision string
	if stsErr == nil {
		ready = sts.Status.ReadyReplicas
		updateRevision = sts.Status.UpdateRevision
	}
	desired := cluster.Spec.Instances
	instancesReady.WithLabelValues(cluster.Namespace, cluster.Name).Set(float64(ready))

	leader, lockFresh, err := r.leaderOf(ctx, cluster)
	if err != nil {
		return clusterView{}, err
	}
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(cluster.Namespace), client.MatchingLabels(podSelectorLabels(cluster))); err != nil {
		return clusterView{}, err
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
			if recovery := cluster.Spec.Recovery(); recovery != nil {
				failover.Message = fmt.Sprintf("restoring from the backups of %s, then waiting for the first primary",
					recovery.SourceCluster)
			}
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
	// Pods still on the previous template mean a rolling update is under way,
	// even while every one of them is Ready.
	if outdated := outdatedPods(updateRevision, rolloutPodsOf(pods.Items)); outdated > 0 {
		progressing.Status = metav1.ConditionTrue
		progressing.Reason = "RollingUpdate"
		progressing.Message = fmt.Sprintf("%d of %d pods are still being updated", outdated, len(pods.Items))
	}

	backupEval, err := r.evaluateBackupNow(ctx, cluster)
	if err != nil {
		return clusterView{}, err
	}
	degraded := degradedCondition(cluster, backupEval)

	changed := meta.SetStatusCondition(&cluster.Status.Conditions, available)
	for _, c := range []metav1.Condition{progressing, replication, failover, degraded} {
		changed = meta.SetStatusCondition(&cluster.Status.Conditions, c) || changed
	}
	changed = applyBackupStatus(cluster, backupEval) || changed
	complianceChanged, err := r.reconcileComplianceStatus(ctx, cluster, pods.Items)
	if err != nil {
		return clusterView{}, err
	}
	changed = complianceChanged || changed
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

	view := clusterView{obs: obs, pods: pods.Items, healthy: obs.primary != "" && obs.replicationHealthy}
	if !changed {
		return view, nil
	}
	return view, r.Status().Update(ctx, cluster)
}

// clusterView is what one reconcile saw of the running cluster.
type clusterView struct {
	obs     observation
	pods    []corev1.Pod
	healthy bool
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
	backupCount.DeletePartialMatch(match)
	backupLastSizeBytes.DeletePartialMatch(match)
	walArchiveLagBytes.DeletePartialMatch(match)
	restoreVerificationLastSuccess.DeletePartialMatch(match)
	restoreVerificationDuration.DeletePartialMatch(match)
	complianceControls.DeletePartialMatch(match)
	r.lastPrimary.Delete(key.Namespace + "/" + key.Name)
}

func boolToFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// evaluateBackupNow reads the newest backup report and works out what it
// means. With backups off there is nothing to evaluate.
func (r *PostgresClusterReconciler) evaluateBackupNow(
	ctx context.Context, cluster *dbv1alpha1.PostgresCluster,
) (backupEvaluation, error) {
	if !cluster.Spec.Backup.Enabled {
		return backupEvaluation{}, nil
	}
	report, observedAt, err := r.latestBackupReport(ctx, cluster)
	if err != nil {
		return backupEvaluation{}, err
	}
	eval := evaluateBackup(cluster.Spec.Backup, report, observedAt, time.Now())
	if report != nil {
		recordBackupMetrics(cluster, eval.status)
	}
	if cluster.Spec.Backup.Verification.Enabled {
		outcome, err := r.latestVerification(ctx, cluster)
		if err != nil {
			return backupEvaluation{}, err
		}
		eval.verification = outcome
		eval.conditions = append(eval.conditions, verificationCondition(cluster.Spec.Backup.Verification, outcome, time.Now()))
		recordVerificationMetrics(cluster, mergeVerification(cluster.Status.Backup.Verification, outcome))
	}
	return eval, nil
}

// mergeBackupStatus returns the status.backup to store and whether it differs
// from what is stored. Each part is refreshed only when the report carried it:
// a run that could not read the bucket still has the archiver to report, and
// must not wipe the last known backups. The restore-test fields come from the
// newest finished test, and stay as they were when there is none.
func mergeBackupStatus(cluster *dbv1alpha1.PostgresCluster, eval backupEvaluation) (dbv1alpha1.BackupStatus, bool) {
	current := cluster.Status.Backup
	if !cluster.Spec.Backup.Enabled {
		cleared := dbv1alpha1.BackupStatus{}
		return cleared, !equality.Semantic.DeepEqual(current, cleared)
	}
	updated := current
	if eval.hasBackups {
		updated.LastBackupTime = eval.status.LastBackupTime
		updated.LastBackupName = eval.status.LastBackupName
		updated.LastBackupSizeBytes = eval.status.LastBackupSizeBytes
		updated.BackupCount = eval.status.BackupCount
		updated.OldestRecoveryPoint = eval.status.OldestRecoveryPoint
	}
	if eval.hasArchiver {
		updated.WALArchiving = eval.status.WALArchiving
	}
	if cluster.Spec.Backup.Verification.Enabled {
		updated.Verification = mergeVerification(current.Verification, eval.verification)
	} else {
		updated.Verification = dbv1alpha1.VerificationStatus{}
	}
	return updated, !equality.Semantic.DeepEqual(current, updated)
}

// degradedCondition rolls the backup conditions up into one. It is Unknown
// until a status check has reported, True when anything about backups needs
// attention, and False otherwise.
func degradedCondition(cluster *dbv1alpha1.PostgresCluster, eval backupEvaluation) metav1.Condition {
	if !cluster.Spec.Backup.Enabled {
		return metav1.Condition{Type: conditionDegraded, Status: metav1.ConditionFalse,
			Reason: "BackupDisabled", Message: "backups are not enabled"}
	}
	var problems []string
	allUnknown := len(eval.conditions) > 0
	for _, c := range eval.conditions {
		// Only the backup and archiving conditions say whether the status
		// check has reported; a restore test result does not.
		if c.Type != conditionRestoreVerified && c.Status != metav1.ConditionUnknown {
			allUnknown = false
		}
		if c.Status == metav1.ConditionFalse {
			problems = append(problems, c.Type+": "+c.Message)
		}
	}
	if eval.retentionFailed {
		problems = append(problems, "retention could not delete expired backups")
	}
	switch {
	case len(problems) > 0:
		return metav1.Condition{Type: conditionDegraded, Status: metav1.ConditionTrue,
			Reason: "BackupUnhealthy", Message: strings.Join(problems, "; ")}
	case allUnknown || len(eval.conditions) == 0:
		return metav1.Condition{Type: conditionDegraded, Status: metav1.ConditionUnknown,
			Reason: "BackupNotObserved", Message: "no recent backup status check has reported"}
	}
	return metav1.Condition{Type: conditionDegraded, Status: metav1.ConditionFalse,
		Reason: "BackupHealthy", Message: "backups and WAL archiving are healthy"}
}

// applyBackupStatus writes the backup conditions and status.backup into the
// cluster, reporting whether anything changed. The conditions for a part that
// is switched off are removed, not left to go stale.
func applyBackupStatus(cluster *dbv1alpha1.PostgresCluster, eval backupEvaluation) bool {
	changed := false
	if cluster.Spec.Backup.Enabled {
		for _, c := range eval.conditions {
			changed = meta.SetStatusCondition(&cluster.Status.Conditions, c) || changed
		}
	} else {
		changed = meta.RemoveStatusCondition(&cluster.Status.Conditions, conditionBackupHealthy) || changed
		changed = meta.RemoveStatusCondition(&cluster.Status.Conditions, conditionWALArchivingHealthy) || changed
	}
	if !cluster.Spec.Backup.Enabled || !cluster.Spec.Backup.Verification.Enabled {
		changed = meta.RemoveStatusCondition(&cluster.Status.Conditions, conditionRestoreVerified) || changed
	}
	if updated, ok := mergeBackupStatus(cluster, eval); ok {
		cluster.Status.Backup = updated
		changed = true
	}
	return changed
}
