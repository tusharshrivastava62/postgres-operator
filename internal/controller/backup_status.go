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
	_ "embed" // the status script ships inside the operator binary
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	dbv1alpha1 "github.com/tusharshrivastava62/postgres-operator/api/v1alpha1"
)

// backupStatusScript reads the backups back and applies retention; see the
// file for what it does and why it is a script.
//
//go:embed scripts/backup_status.py
var backupStatusScript string

const (
	componentBackupStatus = "backup-status"
	// statusContainerName is the container of the status check pod.
	statusContainerName = "status"

	// backupStatusSchedule is how often the status check runs.
	backupStatusSchedule = "*/10 * * * *"

	// backupStatusStaleAfter is how old the last report may be before the
	// backup conditions stop trusting it: three missed runs and a little more.
	backupStatusStaleAfter = 35 * time.Minute

	// maxArchiveLagSegments is how many WAL segments (16 MB each by default)
	// the archiver may be behind before it is unhealthy.
	maxArchiveLagSegments = 8
	// archiveStallAfter is how long archiving may go without advancing while at
	// least a whole segment waits to be archived.
	archiveStallAfter = 15 * time.Minute
	// overdueGrace is added to two backup intervals before the newest backup
	// counts as overdue, so a run that is a little late is not an alarm.
	overdueGrace = 15 * time.Minute

	conditionBackupHealthy       = "BackupHealthy"
	conditionWALArchivingHealthy = "WALArchivingHealthy"
	conditionDegraded            = "Degraded"
)

func backupStatusName(cluster *dbv1alpha1.PostgresCluster) string {
	return cluster.Name + "-backup-status"
}

func backupStatusLabels(cluster *dbv1alpha1.PostgresCluster) map[string]string {
	labels := labelsForCluster(cluster)
	labels[labelAppComponent] = componentBackupStatus
	return labels
}

// reconcileBackupStatusJob keeps the CronJob that reads the backups back
// when backups are on, and removes it when they are not.
func (r *PostgresClusterReconciler) reconcileBackupStatusJob(
	ctx context.Context, cluster *dbv1alpha1.PostgresCluster, image string,
) error {
	meta := metav1.ObjectMeta{Name: backupStatusName(cluster), Namespace: cluster.Namespace}
	if !cluster.Spec.Backup.Enabled || cluster.Spec.Backup.Destination == nil {
		if err := r.Delete(ctx, &batchv1.CronJob{ObjectMeta: meta}); client.IgnoreNotFound(err) != nil {
			return err
		}
		return client.IgnoreNotFound(r.Delete(ctx, &corev1.ConfigMap{ObjectMeta: meta}))
	}

	script := &corev1.ConfigMap{ObjectMeta: meta}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, script, func() error {
		script.Labels = backupStatusLabels(cluster)
		script.Data = map[string]string{"backup_status.py": backupStatusScript}
		return controllerutil.SetControllerReference(cluster, script, r.Scheme)
	}); err != nil {
		return fmt.Errorf("script ConfigMap: %w", err)
	}

	cj := &batchv1.CronJob{ObjectMeta: meta}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, cj, func() error {
		cj.Labels = backupStatusLabels(cluster)
		cj.Spec = backupStatusCronJobSpec(cluster, image)
		return controllerutil.SetControllerReference(cluster, cj, r.Scheme)
	}); err != nil {
		return fmt.Errorf("CronJob: %w", err)
	}
	return nil
}

func backupStatusCronJobSpec(cluster *dbv1alpha1.PostgresCluster, image string) batchv1.CronJobSpec {
	labels := backupStatusLabels(cluster)
	backup := cluster.Spec.Backup

	env := append([]corev1.EnvVar{
		{Name: "RETAIN_FULL", Value: strconv.Itoa(int(backup.Retention.Full))},
		{Name: "RETAIN_DAYS", Value: strconv.Itoa(int(backup.Retention.Days))},
		// The read-write Service, over TLS: the database accepts nothing else.
		{Name: "PGHOST", Value: cluster.Name},
		{Name: "PGPORT", Value: strconv.Itoa(int(postgresPort))},
		{Name: "PGDATABASE", Value: "postgres"},
		{Name: "PGSSLMODE", Value: "require"},
		{Name: "PGCONNECT_TIMEOUT", Value: "10"},
		{Name: "PGUSER", ValueFrom: secretKeyRef(cluster, secretKeyUsername)},
		{Name: "PGPASSWORD", ValueFrom: secretKeyRef(cluster, secretKeyPassword)},
	}, walgClientEnv(cluster)...)

	return batchv1.CronJobSpec{
		Schedule: backupStatusSchedule,
		// A run that overlaps the last would race it over retention.
		ConcurrencyPolicy:          batchv1.ForbidConcurrent,
		StartingDeadlineSeconds:    ptr.To(int64(300)),
		SuccessfulJobsHistoryLimit: ptr.To(int32(2)),
		FailedJobsHistoryLimit:     ptr.To(int32(3)),
		JobTemplate: batchv1.JobTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: labels},
			Spec: batchv1.JobSpec{
				BackoffLimit:          ptr.To(int32(0)),
				ActiveDeadlineSeconds: ptr.To(int64(600)),
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: labels},
					Spec: corev1.PodSpec{
						RestartPolicy: corev1.RestartPolicyNever,
						// It talks to the bucket and the database, never to
						// the Kubernetes API.
						AutomountServiceAccountToken: ptr.To(false),
						SecurityContext: &corev1.PodSecurityContext{
							RunAsNonRoot:   ptr.To(true),
							RunAsUser:      ptr.To(spiloUID),
							RunAsGroup:     ptr.To(spiloGID),
							SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
						},
						Containers: []corev1.Container{{
							Name:            statusContainerName,
							Image:           image,
							ImagePullPolicy: corev1.PullIfNotPresent,
							Command:         []string{"python3", "/scripts/backup_status.py"},
							Env:             env,
							SecurityContext: &corev1.SecurityContext{
								AllowPrivilegeEscalation: ptr.To(false),
								Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
							},
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("50m"),
									corev1.ResourceMemory: resource.MustParse("128Mi"),
								},
								Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")},
							},
							VolumeMounts: []corev1.VolumeMount{{Name: "script", MountPath: "/scripts", ReadOnly: true}},
						}},
						Volumes: []corev1.Volume{{
							Name: "script",
							VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
								LocalObjectReference: corev1.LocalObjectReference{Name: backupStatusName(cluster)},
							}},
						}},
					},
				},
			},
		},
	}
}

// backupReport is what one run of the status script wrote to its termination
// log.
type backupReport struct {
	Backups *struct {
		Count  int        `json:"count"`
		Newest *backupRef `json:"newest"`
		Oldest *backupRef `json:"oldest"`
	} `json:"backups"`
	Archiver *struct {
		Archived          int64  `json:"archived"`
		Failed            int64  `json:"failed"`
		LastArchivedWAL   string `json:"lastArchivedWal"`
		LastArchivedEpoch int64  `json:"lastArchivedEpoch"`
		CurrentLSN        string `json:"currentLsn"`
		SegmentBytes      int64  `json:"segmentBytes"`
	} `json:"archiver"`
	Retention *struct {
		Deleted int `json:"deleted"`
	} `json:"retention"`
	Errors []string `json:"errors"`
}

type backupRef struct {
	Name      string    `json:"name"`
	Time      time.Time `json:"time"`
	SizeBytes int64     `json:"sizeBytes"`
}

// latestBackupReport finds the newest finished run of the status script and
// reads its result. It returns nil when no run has finished yet. Pods are used
// rather than Jobs because the result lives on the pod, and reading it needs
// no more permission than the operator already has.
func (r *PostgresClusterReconciler) latestBackupReport(
	ctx context.Context, cluster *dbv1alpha1.PostgresCluster,
) (*backupReport, time.Time, error) {
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(cluster.Namespace), client.MatchingLabels(backupStatusLabels(cluster))); err != nil {
		return nil, time.Time{}, err
	}
	var newest *corev1.ContainerStateTerminated
	for i := range pods.Items {
		for _, cs := range pods.Items[i].Status.ContainerStatuses {
			term := cs.State.Terminated
			if term == nil || term.Message == "" {
				continue
			}
			if newest == nil || term.FinishedAt.After(newest.FinishedAt.Time) {
				newest = term
			}
		}
	}
	if newest == nil {
		return nil, time.Time{}, nil
	}
	report := &backupReport{}
	if err := json.Unmarshal([]byte(newest.Message), report); err != nil {
		report = &backupReport{Errors: []string{"the backup status report could not be read"}}
	}
	return report, newest.FinishedAt.Time, nil
}

// backupEvaluation is what a report means.
type backupEvaluation struct {
	status dbv1alpha1.BackupStatus
	// hasBackups and hasArchiver say which parts of status came from the
	// report. A part the run could not read is left as it was, not blanked.
	hasBackups  bool
	hasArchiver bool
	conditions  []metav1.Condition
	// retentionFailed is set when the run could not delete expired backups.
	retentionFailed bool
}

func notObserved(reason, message string) []metav1.Condition {
	return []metav1.Condition{
		{Type: conditionBackupHealthy, Status: metav1.ConditionUnknown, Reason: reason, Message: message},
		{Type: conditionWALArchivingHealthy, Status: metav1.ConditionUnknown, Reason: reason, Message: message},
	}
}

// evaluateBackup turns a report into the backup status and conditions. Nothing
// in a message depends on the clock, so a condition is rewritten only when its
// substance changes, not on every reconcile.
func evaluateBackup(
	spec dbv1alpha1.BackupSpec, report *backupReport, observedAt, now time.Time,
) backupEvaluation {
	if report == nil {
		return backupEvaluation{conditions: notObserved("NotObserved", "waiting for the first backup status check")}
	}
	if now.Sub(observedAt) > backupStatusStaleAfter {
		return backupEvaluation{conditions: notObserved("StatusStale", fmt.Sprintf(
			"the backup status check has not reported since %s", observedAt.UTC().Format(time.RFC3339)))}
	}

	var eval backupEvaluation
	eval.status = statusFromReport(report)
	eval.hasBackups = report.Backups != nil
	eval.hasArchiver = report.Archiver != nil
	_, eval.retentionFailed = errorContaining(report, "wal-g delete")

	backupCond := metav1.Condition{Type: conditionBackupHealthy, Status: metav1.ConditionFalse}
	switch {
	case report.Backups == nil:
		backupCond.Reason = "BucketUnreadable"
		backupCond.Message = errorOrDefault(report, "backup-list")
	case report.Backups.Count == 0 || report.Backups.Newest == nil:
		backupCond.Reason = "NoBackup"
		backupCond.Message = "no base backup exists yet"
	default:
		interval := scheduleInterval(spec.Schedule, observedAt)
		age := observedAt.Sub(report.Backups.Newest.Time)
		if interval > 0 && age > 2*interval+overdueGrace {
			backupCond.Reason = "BackupOverdue"
			backupCond.Message = fmt.Sprintf("the newest base backup, %s, finished at %s, more than two scheduled intervals ago",
				report.Backups.Newest.Name, report.Backups.Newest.Time.UTC().Format(time.RFC3339))
		} else {
			backupCond.Status = metav1.ConditionTrue
			backupCond.Reason = "BackupFresh"
			backupCond.Message = fmt.Sprintf("the newest base backup is %s, finished at %s",
				report.Backups.Newest.Name, report.Backups.Newest.Time.UTC().Format(time.RFC3339))
		}
	}

	archiveCond := metav1.Condition{Type: conditionWALArchivingHealthy, Status: metav1.ConditionFalse}
	switch report.Archiver {
	case nil:
		archiveCond.Status = metav1.ConditionUnknown
		archiveCond.Reason = "ArchiverUnreadable"
		archiveCond.Message = errorOrDefault(report, "psql")
	default:
		lag := eval.status.WALArchiving.LagBytes
		segment := report.Archiver.SegmentBytes
		if segment <= 0 {
			segment = 16 << 20
		}
		stalled := lag >= segment && report.Archiver.LastArchivedEpoch > 0 &&
			observedAt.Sub(time.Unix(report.Archiver.LastArchivedEpoch, 0)) > archiveStallAfter
		switch {
		case lag > maxArchiveLagSegments*segment:
			archiveCond.Reason = "ArchivingBehind"
			archiveCond.Message = fmt.Sprintf("more than %d WAL segments are waiting to be archived", maxArchiveLagSegments)
		case stalled:
			archiveCond.Reason = "ArchivingStalled"
			archiveCond.Message = fmt.Sprintf("no WAL has been archived for more than %s while WAL is waiting", archiveStallAfter)
		default:
			archiveCond.Status = metav1.ConditionTrue
			archiveCond.Reason = "ArchivingKeepsUp"
			archiveCond.Message = "WAL is reaching the destination"
		}
	}
	eval.conditions = []metav1.Condition{backupCond, archiveCond}
	return eval
}

func statusFromReport(report *backupReport) dbv1alpha1.BackupStatus {
	var status dbv1alpha1.BackupStatus
	if b := report.Backups; b != nil {
		status.BackupCount = int32(min(b.Count, 1<<30)) //nolint:gosec // a backup count never nears the limit
		if b.Newest != nil {
			status.LastBackupName = b.Newest.Name
			status.LastBackupSizeBytes = b.Newest.SizeBytes
			status.LastBackupTime = ptr.To(metav1.NewTime(b.Newest.Time))
		}
		if b.Oldest != nil {
			status.OldestRecoveryPoint = ptr.To(metav1.NewTime(b.Oldest.Time))
		}
	}
	if a := report.Archiver; a != nil {
		if a.LastArchivedEpoch > 0 {
			status.WALArchiving.LastArchivedTime = ptr.To(metav1.NewTime(time.Unix(a.LastArchivedEpoch, 0)))
		}
		status.WALArchiving.LagBytes = archiveLagBytes(a.CurrentLSN, a.LastArchivedWAL, a.SegmentBytes)
	}
	return status
}

// archiveLagBytes is how much WAL lies between the end of the last archived
// segment and the primary's write position. A new cluster that has archived
// nothing yet reports everything it has written.
func archiveLagBytes(currentLSN, lastArchivedWAL string, segmentBytes int64) int64 {
	current, err := parseLSN(currentLSN)
	if err != nil {
		return 0
	}
	if segmentBytes <= 0 {
		segmentBytes = 16 << 20
	}
	archivedEnd := int64(0)
	if len(lastArchivedWAL) >= 24 {
		logID, errLog := strconv.ParseInt(lastArchivedWAL[8:16], 16, 64)
		seg, errSeg := strconv.ParseInt(lastArchivedWAL[16:24], 16, 64)
		if errLog == nil && errSeg == nil {
			segmentsPerLog := (int64(1) << 32) / segmentBytes
			archivedEnd = (logID*segmentsPerLog + seg + 1) * segmentBytes
		}
	}
	return max(current-archivedEnd, 0)
}

// parseLSN reads PostgreSQL's "X/Y" log sequence number as a byte position.
func parseLSN(lsn string) (int64, error) {
	hi, lo, found := strings.Cut(lsn, "/")
	if !found {
		return 0, errors.New("not an LSN")
	}
	high, err := strconv.ParseInt(hi, 16, 64)
	if err != nil {
		return 0, err
	}
	low, err := strconv.ParseInt(lo, 16, 64)
	if err != nil {
		return 0, err
	}
	return high<<32 | low, nil
}

// scheduleInterval is the time between two runs of a cron schedule, or zero
// for one that cannot be read.
func scheduleInterval(schedule string, around time.Time) time.Duration {
	parsed, err := cron.ParseStandard(schedule)
	if err != nil {
		return 0
	}
	first := parsed.Next(around)
	return parsed.Next(first).Sub(first)
}

// errorContaining returns the first reported error that mentions text.
func errorContaining(report *backupReport, text string) (string, bool) {
	for _, e := range report.Errors {
		if strings.Contains(e, text) {
			return e, true
		}
	}
	return "", false
}

// errorOrDefault is errorContaining with a stand-in message for the case where
// the step failed without saying why.
func errorOrDefault(report *backupReport, text string) string {
	if e, ok := errorContaining(report, text); ok {
		return e
	}
	return "the status check could not read this"
}

// recordBackupMetrics publishes the backup gauges from a status.
func recordBackupMetrics(cluster *dbv1alpha1.PostgresCluster, status dbv1alpha1.BackupStatus) {
	ns, name := cluster.Namespace, cluster.Name
	backupCount.WithLabelValues(ns, name).Set(float64(status.BackupCount))
	backupLastSizeBytes.WithLabelValues(ns, name).Set(float64(status.LastBackupSizeBytes))
	walArchiveLagBytes.WithLabelValues(ns, name).Set(float64(status.WALArchiving.LagBytes))
	if status.LastBackupTime != nil {
		backupLastSuccessTimestampSeconds.WithLabelValues(ns, name).Set(float64(status.LastBackupTime.Unix()))
	}
}
