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
	"strconv"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	dbv1alpha1 "github.com/tusharshrivastava62/postgres-operator/api/v1alpha1"
)

const (
	conditionRestoreVerified = "RestoreVerified"

	// verifyContainerName is the container of the restore test pod, and
	// restoreVolumeName its scratch volume.
	verifyContainerName = "verify"
	restoreVolumeName   = "restore"

	// restoreTestDeadline bounds one restore test. It is longer than the
	// script's own recovery timeout, so the script reports a failure itself
	// before the Job is killed.
	restoreTestDeadline = int64(4*60*60 + 30*60)

	// restoreVerifyOverdueGrace is added to two test intervals before the last
	// passing test counts as overdue.
	restoreVerifyOverdueGrace = time.Hour
)

// restoreVerifyCronJobSpec is the restore test. Its pod fetches the newest base
// backup into scratch space, starts PostgreSQL on a local socket, lets it
// replay the archived WAL, checks every database, and reports how long that
// took. It runs as the database user with the cluster's own memory limit, and
// the scratch volume is capped at the size of the data volume.
func restoreVerifyCronJobSpec(cluster *dbv1alpha1.PostgresCluster, image string) batchv1.CronJobSpec {
	labels := restoreVerifyLabels(cluster)

	memory := resource.MustParse("1Gi")
	if limit, ok := cluster.Spec.Resources.Limits[corev1.ResourceMemory]; ok {
		memory = limit
	}
	requestMemory := resource.MustParse("256Mi")
	if memory.Cmp(requestMemory) < 0 {
		requestMemory = memory
	}

	env := append([]corev1.EnvVar{
		{Name: "PGVERSION", Value: cluster.Spec.Version},
		{Name: "RESTORE_DIR", Value: "/restore"},
	}, walgClientEnv(cluster)...)

	return batchv1.CronJobSpec{
		Schedule:                   cluster.Spec.Backup.Verification.Schedule,
		ConcurrencyPolicy:          batchv1.ForbidConcurrent,
		SuccessfulJobsHistoryLimit: ptr.To(int32(3)),
		FailedJobsHistoryLimit:     ptr.To(int32(3)),
		JobTemplate: batchv1.JobTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: labels},
			Spec: batchv1.JobSpec{
				BackoffLimit:          ptr.To(int32(0)),
				ActiveDeadlineSeconds: ptr.To(restoreTestDeadline),
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: labels},
					Spec: corev1.PodSpec{
						RestartPolicy:                corev1.RestartPolicyNever,
						AutomountServiceAccountToken: ptr.To(false),
						SecurityContext: &corev1.PodSecurityContext{
							RunAsNonRoot:   ptr.To(true),
							RunAsUser:      ptr.To(spiloUID),
							RunAsGroup:     ptr.To(spiloGID),
							FSGroup:        ptr.To(spiloGID),
							SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
						},
						Containers: []corev1.Container{{
							Name:            verifyContainerName,
							Image:           image,
							ImagePullPolicy: corev1.PullIfNotPresent,
							Command:         []string{"python3", "/scripts/restore_verify.py"},
							Env:             env,
							SecurityContext: &corev1.SecurityContext{
								AllowPrivilegeEscalation: ptr.To(false),
								Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{dropAllCapabilities}},
							},
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("100m"),
									corev1.ResourceMemory: requestMemory,
								},
								Limits: corev1.ResourceList{corev1.ResourceMemory: memory},
							},
							VolumeMounts: []corev1.VolumeMount{
								{Name: scriptVolumeName, MountPath: "/scripts", ReadOnly: true},
								{Name: restoreVolumeName, MountPath: "/restore"},
								{Name: shmVolumeName, MountPath: "/dev/shm"},
							},
						}},
						Volumes: []corev1.Volume{
							{Name: scriptVolumeName, VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
								LocalObjectReference: corev1.LocalObjectReference{Name: backupScriptsName(cluster)},
							}}},
							{Name: restoreVolumeName, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
								SizeLimit: &cluster.Spec.Storage.Size,
							}}},
							{Name: shmVolumeName, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
								Medium: corev1.StorageMediumMemory, SizeLimit: sharedMemoryLimit(cluster),
							}}},
						},
					},
				},
			},
		},
	}
}

// verifyReport is what one run of the restore test wrote to its termination
// log.
type verifyReport struct {
	Success         bool   `json:"success"`
	Backup          string `json:"backup"`
	DurationSeconds int64  `json:"durationSeconds"`
	FetchSeconds    int64  `json:"fetchSeconds"`
	ReplaySeconds   int64  `json:"replaySeconds"`
	Databases       int    `json:"databases"`
	Message         string `json:"message"`
}

// verifyOutcome is the newest finished restore test.
type verifyOutcome struct {
	finishedAt time.Time
	success    bool
	duration   int64
	backup     string
	message    string
}

// latestVerification reads the newest finished restore test, or nil when none
// has finished.
func (r *PostgresClusterReconciler) latestVerification(
	ctx context.Context, cluster *dbv1alpha1.PostgresCluster,
) (*verifyOutcome, error) {
	message, finishedAt, err := r.latestTermination(ctx, cluster, restoreVerifyLabels(cluster))
	if err != nil || message == "" {
		return nil, err
	}
	report := &verifyReport{}
	if err := json.Unmarshal([]byte(message), report); err != nil {
		return &verifyOutcome{finishedAt: finishedAt, message: "the restore test report could not be read"}, nil
	}
	return &verifyOutcome{
		finishedAt: finishedAt, success: report.Success, duration: report.DurationSeconds,
		backup: report.Backup, message: report.Message,
	}, nil
}

// verificationCondition is RestoreVerified: whether the newest backup has been
// shown to restore, recently enough. Nothing in a message depends on the clock.
func verificationCondition(
	spec dbv1alpha1.BackupVerification, outcome *verifyOutcome, now time.Time,
) metav1.Condition {
	cond := metav1.Condition{Type: conditionRestoreVerified, Status: metav1.ConditionUnknown}
	switch {
	case outcome == nil:
		cond.Reason = "NotRunYet"
		cond.Message = "waiting for the first restore test"
	case !outcome.success:
		cond.Status = metav1.ConditionFalse
		cond.Reason = "RestoreFailed"
		cond.Message = "the last restore test failed: " + outcome.message
	default:
		interval := scheduleInterval(spec.Schedule, outcome.finishedAt)
		if interval > 0 && now.Sub(outcome.finishedAt) > 2*interval+restoreVerifyOverdueGrace {
			cond.Status = metav1.ConditionFalse
			cond.Reason = "RestoreTestOverdue"
			cond.Message = fmt.Sprintf("the last passing restore test finished at %s, more than two scheduled intervals ago",
				outcome.finishedAt.UTC().Format(time.RFC3339))
			return cond
		}
		cond.Status = metav1.ConditionTrue
		cond.Reason = "RestoreVerified"
		cond.Message = "the newest backup " + outcome.backup + " was restored and read in " +
			strconv.FormatInt(outcome.duration, 10) + " seconds"
	}
	return cond
}

// mergeVerification returns the restore-test fields to store.
func mergeVerification(current dbv1alpha1.VerificationStatus, outcome *verifyOutcome) dbv1alpha1.VerificationStatus {
	if outcome == nil {
		return current
	}
	updated := current
	updated.LastRunTime = ptr.To(metav1.NewTime(outcome.finishedAt))
	if outcome.success {
		updated.LastSuccessTime = updated.LastRunTime
		updated.DurationSeconds = outcome.duration
		updated.Message = ""
	} else {
		updated.Message = outcome.message
	}
	return updated
}

// recordVerificationMetrics publishes the outcome of a passing restore test.
func recordVerificationMetrics(cluster *dbv1alpha1.PostgresCluster, status dbv1alpha1.VerificationStatus) {
	if status.LastSuccessTime == nil {
		return
	}
	restoreVerificationLastSuccess.WithLabelValues(cluster.Namespace, cluster.Name).Set(float64(status.LastSuccessTime.Unix()))
	restoreVerificationDuration.WithLabelValues(cluster.Namespace, cluster.Name).Set(float64(status.DurationSeconds))
}
