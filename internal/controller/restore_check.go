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
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	dbv1alpha1 "github.com/tusharshrivastava62/postgres-operator/api/v1alpha1"
)

const (
	componentRestoreCheck = "restore-check"

	// scriptInterpreter and scriptsDir are how the jobs run the scripts mounted from
	// the scripts ConfigMap.
	scriptInterpreter = "python3"
	scriptsDir        = "/scripts"

	// checkContainerName is the container of the restore check pod.
	checkContainerName = "check"

	// conditionRecoverySourceReady says whether the backups a new cluster is
	// restored from can be restored from.
	conditionRecoverySourceReady = "RecoverySourceReady"

	// restoreCheckDeadline bounds one check. The script gives up on the bucket
	// after two minutes by itself.
	restoreCheckDeadline = int64(10 * 60)
	// restoreCheckPollDelay is how soon to look at a check that is running, in
	// case the event that its pod finished is missed.
	restoreCheckPollDelay = 5 * time.Second
	// restoreCheckRecheckDelay is how long a failed check stands before it is
	// run again. The spec cannot change, but the bucket and its Secrets can.
	restoreCheckRecheckDelay = 2 * time.Minute

	// The verdicts of restore_check.py.
	verdictOK                = "ok"
	verdictUnreadable        = "unreadable"
	verdictNoBackups         = "no-backups"
	verdictTargetBeforeFirst = "target-before-oldest"
)

func restoreCheckName(cluster *dbv1alpha1.PostgresCluster) string {
	return cluster.Name + "-restore-check"
}

func restoreCheckLabels(cluster *dbv1alpha1.PostgresCluster) map[string]string {
	return labelsWithComponent(cluster, componentRestoreCheck)
}

// restoreCheckJob is the check that runs before a restored cluster's first pod
// is created: it reads the source's backups and says whether the restore can
// find one to start from. It holds only the source's bucket credentials.
func restoreCheckJob(cluster *dbv1alpha1.PostgresCluster, image string) batchv1.JobSpec {
	labels := restoreCheckLabels(cluster)
	env := recoveryClientEnv(cluster)
	if recovery := cluster.Spec.Recovery(); recovery != nil && recovery.TargetTime != nil {
		env = append(env, corev1.EnvVar{Name: "TARGET_TIME", Value: recovery.TargetTime.UTC().Format(time.RFC3339)})
	}
	return batchv1.JobSpec{
		BackoffLimit:          ptr.To(int32(0)),
		ActiveDeadlineSeconds: ptr.To(restoreCheckDeadline),
		Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: labels},
			Spec: corev1.PodSpec{
				RestartPolicy:                corev1.RestartPolicyNever,
				AutomountServiceAccountToken: ptr.To(false),
				SecurityContext: &corev1.PodSecurityContext{
					RunAsNonRoot:   ptr.To(true),
					RunAsUser:      ptr.To(spiloUID),
					RunAsGroup:     ptr.To(spiloGID),
					SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
				},
				Containers: []corev1.Container{{
					Name:            checkContainerName,
					Image:           image,
					ImagePullPolicy: corev1.PullIfNotPresent,
					Command:         []string{scriptInterpreter, scriptsDir + "/restore_check.py"},
					Env:             env,
					SecurityContext: &corev1.SecurityContext{
						AllowPrivilegeEscalation: ptr.To(false),
						Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{dropAllCapabilities}},
					},
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("50m"),
							corev1.ResourceMemory: resource.MustParse("128Mi"),
						},
						Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")},
					},
					VolumeMounts: []corev1.VolumeMount{{Name: scriptVolumeName, MountPath: scriptsDir, ReadOnly: true}},
				}},
				Volumes: []corev1.Volume{{
					Name: scriptVolumeName,
					VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{Name: backupScriptsName(cluster)},
					}},
				}},
			},
		},
	}
}

// restoreCheckReport is what the check wrote to its termination log.
type restoreCheckReport struct {
	Verdict string `json:"verdict"`
	Error   string `json:"error"`
	Backup  *struct {
		Name string    `json:"name"`
		Time time.Time `json:"time"`
	} `json:"backup"`
	Oldest *struct {
		Name string    `json:"name"`
		Time time.Time `json:"time"`
	} `json:"oldest"`
}

// recoverySourceCondition says what a finished check means. The messages say
// what to do, since spec.bootstrap cannot be edited: the only way to change a
// target is to create the cluster again.
func recoverySourceCondition(cluster *dbv1alpha1.PostgresCluster, report *restoreCheckReport) metav1.Condition {
	cond := metav1.Condition{Type: conditionRecoverySourceReady, Status: metav1.ConditionFalse}
	recovery := cluster.Spec.Recovery()
	if recovery == nil {
		return cond
	}
	where := recoveryPrefix(cluster)
	switch {
	case report == nil:
		cond.Reason = "CheckFailed"
		cond.Message = "the check of the backups to restore from did not finish; it will be run again"
	case report.Verdict == verdictOK && report.Backup != nil:
		cond.Status = metav1.ConditionTrue
		cond.Reason = "BackupFound"
		cond.Message = fmt.Sprintf("restoring from base backup %s, written %s", report.Backup.Name, report.Backup.Time.UTC().Format(time.RFC3339))
		if recovery.TargetTime != nil {
			cond.Message += ", then replaying WAL up to " + recovery.TargetTime.UTC().Format(time.RFC3339)
		}
	case report.Verdict == verdictTargetBeforeFirst && report.Oldest != nil && recovery.TargetTime != nil:
		oldest := report.Oldest.Time.UTC().Format(time.RFC3339)
		cond.Reason = "TargetBeforeOldestBackup"
		cond.Message = fmt.Sprintf("targetTime %s is earlier than the oldest base backup at %s (%s, written %s), "+
			"so there is no backup to start from. spec.bootstrap cannot be changed: delete this cluster and create it "+
			"again with a targetTime of %s or later, or without one to restore the latest state",
			recovery.TargetTime.UTC().Format(time.RFC3339), where, report.Oldest.Name, oldest, oldest)
	case report.Verdict == verdictNoBackups:
		cond.Reason = "NoBackups"
		cond.Message = fmt.Sprintf("there are no base backups at %s: check sourceCluster, sourceNamespace, the "+
			"destination's bucket and path, and that the source's PostgreSQL major version is %s", where, cluster.Spec.Version)
	case report.Verdict == verdictUnreadable:
		cond.Reason = "SourceUnreadable"
		cond.Message = fmt.Sprintf("the backups at %s could not be read: %s. Check the destination and the Secrets it "+
			"names; the check is repeated every %s", where, report.Error, restoreCheckRecheckDelay)
	default:
		cond.Reason = "CheckFailed"
		cond.Message = "the check of the backups to restore from gave an answer that was not understood; it will be run again"
	}
	return cond
}

// reconcileRecoveryCheck holds back the creation of a restored cluster's pods
// until the backups it names have been checked, so that a restore that cannot
// work is reported in status instead of showing as a pod that keeps
// restarting. It returns whether the cluster may be created, and when to look
// again if not.
//
// It does nothing for a cluster that starts empty, and nothing once the
// StatefulSet exists: from then on Patroni owns the bootstrap, and the source
// bucket changing must never stop an existing member from restarting.
func (r *PostgresClusterReconciler) reconcileRecoveryCheck(
	ctx context.Context, cluster *dbv1alpha1.PostgresCluster, image string,
) (bool, time.Duration, error) {
	if cluster.Spec.Recovery() == nil {
		return true, 0, nil
	}
	sts := &appsv1.StatefulSet{}
	err := r.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: cluster.Name}, sts)
	if err == nil {
		return true, 0, nil
	}
	if !apierrors.IsNotFound(err) {
		return false, 0, err
	}

	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: backupScriptsName(cluster), Namespace: cluster.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		cm.Labels = backupStatusLabels(cluster)
		cm.Data = backupScripts()
		return controllerutil.SetControllerReference(cluster, cm, r.Scheme)
	}); err != nil {
		return false, 0, fmt.Errorf("script ConfigMap: %w", err)
	}

	job := &batchv1.Job{}
	err = r.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: restoreCheckName(cluster)}, job)
	if apierrors.IsNotFound(err) {
		job = &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
			Name: restoreCheckName(cluster), Namespace: cluster.Namespace, Labels: restoreCheckLabels(cluster),
		}, Spec: restoreCheckJob(cluster, image)}
		if err := controllerutil.SetControllerReference(cluster, job, r.Scheme); err != nil {
			return false, 0, err
		}
		if err := r.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
			return false, 0, fmt.Errorf("creating the restore check: %w", err)
		}
		return false, restoreCheckPollDelay, r.reportChecking(ctx, cluster)
	}
	if err != nil {
		return false, 0, err
	}

	finished, finishedAt := jobFinished(job)
	if !finished {
		return false, restoreCheckPollDelay, r.reportChecking(ctx, cluster)
	}
	report, err := r.restoreCheckResult(ctx, cluster, job)
	if err != nil {
		return false, 0, err
	}
	cond := recoverySourceCondition(cluster, report)
	if cond.Status == metav1.ConditionTrue {
		return true, 0, r.writeRecoveryStatus(ctx, cluster, cond, nil)
	}
	progressing := &metav1.Condition{
		Type: conditionProgressing, Status: metav1.ConditionFalse, Reason: "RecoverySourceProblem", Message: cond.Message,
	}
	if err := r.writeRecoveryStatus(ctx, cluster, cond, progressing); err != nil {
		return false, 0, err
	}
	// Run the check again after a while: the bucket and the Secrets can be
	// fixed even though the spec cannot.
	wait := restoreCheckRecheckDelay - time.Since(finishedAt)
	if wait > 0 {
		return false, wait, nil
	}
	if err := r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground)); client.IgnoreNotFound(err) != nil {
		return false, 0, err
	}
	return false, restoreCheckPollDelay, nil
}

// reportChecking records that the backups are being looked at.
func (r *PostgresClusterReconciler) reportChecking(ctx context.Context, cluster *dbv1alpha1.PostgresCluster) error {
	source := metav1.Condition{
		Type: conditionRecoverySourceReady, Status: metav1.ConditionUnknown, Reason: "Checking",
		Message: "checking the backups to restore from before creating the cluster",
	}
	progressing := &metav1.Condition{
		Type: conditionProgressing, Status: metav1.ConditionTrue, Reason: "CheckingRecoverySource", Message: source.Message,
	}
	return r.writeRecoveryStatus(ctx, cluster, source, progressing)
}

func (r *PostgresClusterReconciler) writeRecoveryStatus(
	ctx context.Context, cluster *dbv1alpha1.PostgresCluster, source metav1.Condition, progressing *metav1.Condition,
) error {
	changed := meta.SetStatusCondition(&cluster.Status.Conditions, source)
	if progressing != nil {
		changed = meta.SetStatusCondition(&cluster.Status.Conditions, *progressing) || changed
	}
	if cluster.Status.ObservedGeneration != cluster.Generation {
		cluster.Status.ObservedGeneration = cluster.Generation
		changed = true
	}
	if !changed {
		return nil
	}
	return r.Status().Update(ctx, cluster)
}

// jobFinished reports whether a Job has completed or failed, and when.
func jobFinished(job *batchv1.Job) (bool, time.Time) {
	for _, c := range job.Status.Conditions {
		if (c.Type == batchv1.JobComplete || c.Type == batchv1.JobFailed) && c.Status == corev1.ConditionTrue {
			return true, c.LastTransitionTime.Time
		}
	}
	return false, time.Time{}
}

// restoreCheckResult reads what the check's pod reported. It returns nil when
// the pod left no readable report. Only pods owned by this Job count, so the
// pod of a check that was just deleted to be run again is never mistaken for
// the new one.
func (r *PostgresClusterReconciler) restoreCheckResult(
	ctx context.Context, cluster *dbv1alpha1.PostgresCluster, job *batchv1.Job,
) (*restoreCheckReport, error) {
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(cluster.Namespace), client.MatchingLabels(restoreCheckLabels(cluster))); err != nil {
		return nil, err
	}
	for i := range pods.Items {
		if !metav1.IsControlledBy(&pods.Items[i], job) {
			continue
		}
		for _, cs := range pods.Items[i].Status.ContainerStatuses {
			if cs.State.Terminated == nil || cs.State.Terminated.Message == "" {
				continue
			}
			report := &restoreCheckReport{}
			if err := json.Unmarshal([]byte(cs.State.Terminated.Message), report); err != nil {
				return nil, nil //nolint:nilerr // an unreadable report is reported as a failed check, not retried as an error
			}
			return report, nil
		}
	}
	return nil, nil
}
