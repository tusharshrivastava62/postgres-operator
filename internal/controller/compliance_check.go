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
	_ "embed" // for the check scripts
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	dbv1alpha1 "github.com/tusharshrivastava62/postgres-operator/api/v1alpha1"
)

// complianceCheckScript reads the database and the audit bucket for the facts
// that decide the controls; see the file.
//
//go:embed scripts/compliance_check.py
var complianceCheckScript string

// networkProbeScript tries to connect from a pod the network policy should
// refuse; see the file.
//
//go:embed scripts/network_probe.py
var networkProbeScript string

const (
	componentNetworkProbe = "network-probe"

	complianceCheckContainer = "check"
	networkProbeContainer    = "probe"

	complianceCheckFile = "compliance_check.py"
	networkProbeFile    = "network_probe.py"

	// complianceCheckSchedule is how often the controls are read from the
	// cluster. Both jobs share it, so their answers are about the same time.
	complianceCheckSchedule = "*/10 * * * *"

	// complianceStaleAfter is how old a report may be before its controls stop
	// being believed.
	complianceStaleAfter = dbv1alpha1.ComplianceStaleAfter

	// probeTimeoutSeconds is how long the probe waits for a connection.
	probeTimeoutSeconds = "5"
)

func complianceCheckName(cluster *dbv1alpha1.PostgresCluster) string {
	return cluster.Name + "-compliance-check"
}

func networkProbeName(cluster *dbv1alpha1.PostgresCluster) string {
	return cluster.Name + "-network-probe"
}

func complianceCheckLabels(cluster *dbv1alpha1.PostgresCluster) map[string]string {
	return labelsWithComponent(cluster, componentComplianceCheck)
}

// networkProbeLabels are deliberately not among the labels the network policy
// admits: the probe is a stranger, and its job is to be refused.
func networkProbeLabels(cluster *dbv1alpha1.PostgresCluster) map[string]string {
	return labelsWithComponent(cluster, componentNetworkProbe)
}

// complianceScripts is the content of the ConfigMap the cluster's compliance
// pods read their scripts from.
func complianceScripts() map[string]string {
	return map[string]string{
		auditScriptFile:     auditShipperScript,
		complianceCheckFile: complianceCheckScript,
		networkProbeFile:    networkProbeScript,
	}
}

// strictPodSpec is what every small job pod shares: the database user, no
// access to the Kubernetes API, and the strictest security settings.
func strictPodSpec(container corev1.Container, scripts string) corev1.PodSpec {
	return corev1.PodSpec{
		RestartPolicy:                corev1.RestartPolicyNever,
		AutomountServiceAccountToken: ptr.To(false),
		SecurityContext: &corev1.PodSecurityContext{
			RunAsNonRoot:   ptr.To(true),
			RunAsUser:      ptr.To(spiloUID),
			RunAsGroup:     ptr.To(spiloGID),
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
		Containers: []corev1.Container{container},
		Volumes: []corev1.Volume{{
			Name: scriptVolumeName,
			VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: scripts},
			}},
		}},
	}
}

func strictContainer(name, image, script string, env []corev1.EnvVar) corev1.Container {
	return corev1.Container{
		Name:            name,
		Image:           image,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Command:         []string{scriptInterpreter, scriptsDir + "/" + script},
		Env:             env,
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptr.To(false),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{dropAllCapabilities}},
		},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("50m"),
				corev1.ResourceMemory: resource.MustParse("64Mi"),
			},
			Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")},
		},
		VolumeMounts: []corev1.VolumeMount{{Name: scriptVolumeName, MountPath: scriptsDir, ReadOnly: true}},
	}
}

// cronJob wraps a pod in a schedule that runs it one at a time and keeps the
// last few finished runs, whose pods carry the reports the operator reads.
func cronJob(labels map[string]string, pod corev1.PodSpec, deadline int64) batchv1.CronJobSpec {
	return batchv1.CronJobSpec{
		Schedule:                   complianceCheckSchedule,
		ConcurrencyPolicy:          batchv1.ForbidConcurrent,
		StartingDeadlineSeconds:    ptr.To(int64(300)),
		SuccessfulJobsHistoryLimit: ptr.To(int32(2)),
		FailedJobsHistoryLimit:     ptr.To(int32(3)),
		JobTemplate: batchv1.JobTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: labels},
			Spec: batchv1.JobSpec{
				BackoffLimit:          ptr.To(int32(0)),
				ActiveDeadlineSeconds: ptr.To(deadline),
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: labels},
					Spec:       pod,
				},
			},
		},
	}
}

// complianceCheckCronJobSpec reads the database over SQL, as the read-only
// monitor role (as the backup status check does), and the audit bucket, with
// the audit destination's credentials. It is one of the cluster's own jobs, so the
// network policy lets it reach the database.
func complianceCheckCronJobSpec(cluster *dbv1alpha1.PostgresCluster, image string) batchv1.CronJobSpec {
	dest := cluster.Spec.Compliance.AuditLogs.Destination
	env := []corev1.EnvVar{
		// The read-write Service, over TLS: the database accepts nothing else.
		{Name: "PGHOST", Value: cluster.Name},
		{Name: "PGPORT", Value: strconv.Itoa(int(postgresPort))},
		{Name: "PGDATABASE", Value: "postgres"},
		{Name: "PGSSLMODE", Value: "require"},
		{Name: "PGCONNECT_TIMEOUT", Value: "10"},
		{Name: "PGUSER", ValueFrom: monitorSecretKeyRef(cluster, secretKeyUsername)},
		{Name: "PGPASSWORD", ValueFrom: monitorSecretKeyRef(cluster, secretKeyPassword)},
		{Name: "AUDIT_BUCKET", Value: dest.Bucket},
		{Name: "AUDIT_PREFIX", Value: cluster.Spec.Compliance.AuditPathFor(cluster.Namespace, cluster.Name)},
		{Name: "AUDIT_ENDPOINT", Value: dest.Endpoint},
		{Name: "AUDIT_REGION", Value: dest.Region},
		{Name: "AUDIT_FORCE_PATH_STYLE", Value: fmt.Sprint(dest.ForcePathStyle)},
		{Name: secretKeyAccessKey, ValueFrom: secretRef(dest.CredentialsSecret, secretKeyAccessKey)},
		{Name: secretKeySecretKey, ValueFrom: secretRef(dest.CredentialsSecret, secretKeySecretKey)},
	}
	pod := strictPodSpec(strictContainer(complianceCheckContainer, image, complianceCheckFile, env), complianceScriptsName(cluster))
	return cronJob(complianceCheckLabels(cluster), pod, 600)
}

// networkProbeCronJobSpec is the stranger: a pod with none of the labels the
// network policy admits, trying to reach the database.
func networkProbeCronJobSpec(cluster *dbv1alpha1.PostgresCluster, image string) batchv1.CronJobSpec {
	env := []corev1.EnvVar{
		{Name: "TARGET_HOST", Value: cluster.Name},
		{Name: "TARGET_PORT", Value: strconv.Itoa(int(postgresPort))},
		{Name: "CONNECT_TIMEOUT", Value: probeTimeoutSeconds},
	}
	pod := strictPodSpec(strictContainer(networkProbeContainer, image, networkProbeFile, env), complianceScriptsName(cluster))
	return cronJob(networkProbeLabels(cluster), pod, 120)
}

// reconcileComplianceJobs keeps the two jobs that read the cluster's controls
// while it has compliance settings, and removes them when they go.
func (r *PostgresClusterReconciler) reconcileComplianceJobs(ctx context.Context, cluster *dbv1alpha1.PostgresCluster, image string) error {
	check := &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: complianceCheckName(cluster), Namespace: cluster.Namespace}}
	probe := &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: networkProbeName(cluster), Namespace: cluster.Namespace}}
	if !auditShipperEnabled(cluster) {
		for _, job := range []*batchv1.CronJob{check, probe} {
			if err := client.IgnoreNotFound(r.Delete(ctx, job)); err != nil {
				return err
			}
		}
		return nil
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, check, func() error {
		check.Labels = complianceCheckLabels(cluster)
		check.Spec = complianceCheckCronJobSpec(cluster, image)
		return controllerutil.SetControllerReference(cluster, check, r.Scheme)
	}); err != nil {
		return fmt.Errorf("compliance check CronJob: %w", err)
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, probe, func() error {
		probe.Labels = networkProbeLabels(cluster)
		probe.Spec = networkProbeCronJobSpec(cluster, image)
		return controllerutil.SetControllerReference(cluster, probe, r.Scheme)
	}); err != nil {
		return fmt.Errorf("network probe CronJob: %w", err)
	}
	return nil
}

// complianceReport is what one run of the compliance check wrote to its
// termination log.
type complianceReport struct {
	SQL    *sqlFacts    `json:"sql"`
	Bucket *bucketFacts `json:"bucket"`
}

// sqlFacts is what the database said about itself.
type sqlFacts struct {
	OK             bool              `json:"ok"`
	Error          string            `json:"error"`
	Settings       map[string]string `json:"settings"`
	PlainRules     []string          `json:"plainRules"`
	PlainRuleCount int               `json:"plainRuleCount"`
	WeakRules      []string          `json:"weakRules"`
	WeakRuleCount  int               `json:"weakRuleCount"`
	HBAErrors      int               `json:"hbaErrors"`
	NonScramRoles  int               `json:"nonScramRoles"`
}

// bucketFacts is what the audit bucket said.
type bucketFacts struct {
	OK         bool                 `json:"ok"`
	Error      string               `json:"error"`
	Pods       map[string]podUpload `json:"pods"`
	Lifecycle  lifecycleFacts       `json:"lifecycle"`
	ObjectLock objectLockFacts      `json:"objectLock"`
}

type podUpload struct {
	Newest *time.Time `json:"newest"`
	Chunks int        `json:"chunks"`
}

type lifecycleFacts struct {
	Supported bool            `json:"supported"`
	Error     string          `json:"error"`
	Rules     []lifecycleRule `json:"rules"`
}

type lifecycleRule struct {
	ID      string `json:"id"`
	Enabled bool   `json:"enabled"`
	Prefix  string `json:"prefix"`
	Days    int    `json:"days"`
}

type objectLockFacts struct {
	Supported bool   `json:"supported"`
	Error     string `json:"error"`
	Days      int    `json:"days"`
	Mode      string `json:"mode"`
}

// probeReport is what the network probe wrote.
type probeReport struct {
	Result string `json:"result"`
	Detail string `json:"detail"`
}

// Results of the probe.
const (
	probeOpen  = "open"
	probeError = "error"
)

// checked is a report and when its run finished.
type checked[T any] struct {
	report *T
	at     time.Time
}

// latestReport reads the newest finished run among the pods carrying the
// given labels. It returns nil when none has finished, and a nil report with
// a time when the message could not be read.
func latestReport[T any](ctx context.Context, r *PostgresClusterReconciler, cluster *dbv1alpha1.PostgresCluster, labels map[string]string) (checked[T], error) {
	message, finishedAt, err := r.latestTermination(ctx, cluster, labels)
	if err != nil || message == "" {
		return checked[T]{}, err
	}
	report := new(T)
	if err := json.Unmarshal([]byte(message), report); err != nil {
		return checked[T]{at: finishedAt}, nil
	}
	return checked[T]{report: report, at: finishedAt}, nil
}
