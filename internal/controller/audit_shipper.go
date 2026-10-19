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
	_ "embed" // for the shipper script
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	dbv1alpha1 "github.com/tusharshrivastava62/postgres-operator/api/v1alpha1"
)

// auditShipperScript uploads the pod's PostgreSQL log, which holds the pgaudit
// lines, to the audit destination; see the file.
//
//go:embed scripts/audit_shipper.py
var auditShipperScript string

const (
	auditContainerName = "audit-shipper"

	// auditLogDir is where Spilo writes the CSV log, under the data volume.
	auditLogDir = dataMountPath + "/pgroot/pg_log"

	// auditShipInterval is how often new log lines are uploaded. It bounds how
	// far the audit trail in the bucket can trail the database.
	auditShipInterval = "300"

	auditScriptFile = "audit_shipper.py"

	// auditSecretPurpose names the credentials in messages about them.
	auditSecretPurpose = "audit log"

	// auditLoop runs the shipper again and again, and never exits. The shipper
	// is a sidecar in the database pod, and a container that stops makes the pod
	// not Ready, so a shipper that cannot reach its bucket, or a script that
	// fails, must cost nothing worse than missing uploads. Each run starts the
	// script afresh, so an updated script is used from the next run without
	// restarting anything.
	auditLoop = `trap 'exit 0' TERM INT
while true; do
  python3 ` + scriptsDir + `/` + auditScriptFile + ` || true
  sleep "$AUDIT_INTERVAL_SECONDS" & wait $!
done`
)

func auditScriptsName(cluster *dbv1alpha1.PostgresCluster) string {
	return cluster.Name + "-audit-scripts"
}

// auditShipperEnabled reports whether the database pods get the shipper: the
// cluster has compliance settings and somewhere to send the logs. Without a
// destination the webhook has refused the spec, and the reconciler reports a
// problem instead of building the pod.
func auditShipperEnabled(cluster *dbv1alpha1.PostgresCluster) bool {
	return cluster.Spec.Compliance != nil && cluster.Spec.Compliance.AuditLogs.Destination != nil
}

// auditShipperContainer is the sidecar that uploads the pod's PostgreSQL log.
// It mounts the data volume read-only, since it only ever reads the log, and
// takes the destination's credentials from the Secret the spec names.
func auditShipperContainer(cluster *dbv1alpha1.PostgresCluster, image string) corev1.Container {
	dest := cluster.Spec.Compliance.AuditLogs.Destination
	env := []corev1.EnvVar{
		{Name: "AUDIT_BUCKET", Value: dest.Bucket},
		{Name: "AUDIT_PREFIX", Value: cluster.Spec.Compliance.AuditPathFor(cluster.Namespace, cluster.Name)},
		{Name: "AUDIT_ENDPOINT", Value: dest.Endpoint},
		{Name: "AUDIT_REGION", Value: dest.Region},
		{Name: "AUDIT_FORCE_PATH_STYLE", Value: fmt.Sprint(dest.ForcePathStyle)},
		{Name: "AUDIT_INTERVAL_SECONDS", Value: auditShipInterval},
		{Name: "LOG_DIR", Value: auditLogDir},
		{Name: "POD_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}},
		{Name: secretKeyAccessKey, ValueFrom: secretRef(dest.CredentialsSecret, secretKeyAccessKey)},
		{Name: secretKeySecretKey, ValueFrom: secretRef(dest.CredentialsSecret, secretKeySecretKey)},
	}
	return corev1.Container{
		Name:            auditContainerName,
		Image:           image,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Command:         []string{"bash", "-c", auditLoop},
		Env:             env,
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptr.To(false),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{dropAllCapabilities}},
		},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("20m"),
				corev1.ResourceMemory: resource.MustParse("64Mi"),
			},
			// A chunk is read and compressed in memory: at most 8 MiB of log.
			Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: dataVolumeName, MountPath: dataMountPath, ReadOnly: true},
			{Name: scriptVolumeName, MountPath: scriptsDir, ReadOnly: true},
		},
	}
}

// auditShipperVolume is the script the sidecar runs.
func auditShipperVolume(cluster *dbv1alpha1.PostgresCluster) corev1.Volume {
	return corev1.Volume{
		Name: scriptVolumeName,
		VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: auditScriptsName(cluster)},
		}},
	}
}

// reconcileAuditScripts keeps the ConfigMap holding the shipper's script while
// the cluster has compliance settings, and removes it when they go. It runs
// before the StatefulSet, because a pod cannot start without the volume.
func (r *PostgresClusterReconciler) reconcileAuditScripts(ctx context.Context, cluster *dbv1alpha1.PostgresCluster) error {
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: auditScriptsName(cluster), Namespace: cluster.Namespace}}
	if !auditShipperEnabled(cluster) {
		return client.IgnoreNotFound(r.Delete(ctx, cm))
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		cm.Labels = labelsForCluster(cluster)
		cm.Data = map[string]string{auditScriptFile: auditShipperScript}
		return controllerutil.SetControllerReference(cluster, cm, r.Scheme)
	})
	return err
}
