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
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"

	dbv1alpha1 "github.com/tusharshrivastava62/postgres-operator/api/v1alpha1"
)

const (
	// dropAllCapabilities is the Linux capability set dropped from every container.
	dropAllCapabilities corev1.Capability = "ALL"

	// scriptVolumeName is the volume the operator-owned jobs read their script from.
	scriptVolumeName = "script"

	postgresPort          int32 = 5432
	patroniPort           int32 = 8008
	postgresPortName            = "postgres"
	patroniPortName             = "patroni"
	postgresContainerName       = "postgres"
	postgresSuperuser           = "postgres"
	replicationUser             = "replicator"
	dataVolumeName              = "pgdata"
	dataMountPath               = "/home/postgres/pgdata"
	shmVolumeName               = "dshm"

	secretKeyUsername            = "username"
	secretKeyPassword            = "password"
	secretKeyReplicationPassword = "replication-password"

	// The user and group the Spilo image's postgres user runs as. They are
	// fixed by the image, so the pod must be run as exactly these for the
	// data volume to be writable (fsGroup) without running as root.
	spiloUID int64 = 101
	spiloGID int64 = 103

	// Defaults mirrored from the CRD, applied when a PostgresCluster is built
	// in code without going through the API server's defaulting.
	defaultFailoverTimeout       int32 = 30
	defaultMaximumLagOnFailover  int64 = 1048576
	defaultTerminationGraceSecs  int64 = 60
	maxSharedMemoryBytes         int64 = 256 << 20
	sharedMemoryFractionOfMemory int64 = 4
)

// spiloConfiguration renders the SPILO_CONFIGURATION environment variable,
// which Spilo merges over the Patroni configuration it generates itself.
//
// The dynamic settings under bootstrap.dcs are the *initial* values and are
// deliberately fixed rather than taken from spec.ha. They are only read when
// a cluster is first initialised, so anything derived from the spec would
// change the pod template, and therefore roll every pod, whenever spec.ha is
// edited, for no effect. The operator applies the real values to the running
// cluster through Patroni's REST API instead (see syncPatroniConfig), a
// moment after the first leader appears.
func spiloConfiguration(cluster *dbv1alpha1.PostgresCluster) (string, error) {
	config := map[string]any{
		"bootstrap": map[string]any{
			"dcs": map[string]any{
				dcsTTL:                  defaultFailoverTimeout,
				dcsLoopWait:             defaultFailoverTimeout / 3,
				dcsRetryTimeout:         defaultFailoverTimeout / 3,
				dcsMaximumLagOnFailover: defaultMaximumLagOnFailover,
				dcsFailsafeMode:         true,
				dcsSynchronousMode:      false,
			},
		},
		"kubernetes": map[string]any{
			// Patroni labels replicas with this value; the default is
			// "replica" only by convention, so say it explicitly because
			// the read-only Service selects on it.
			"follower_label_value": roleReplica,
			// Tells Patroni which port to publish in the Endpoints it
			// maintains for the read-write Service.
			"ports": []map[string]any{{"name": postgresPortName, "port": postgresPort}},
		},
	}
	// The one setting that cannot go through Patroni's REST API; see
	// paramPreload. A cluster without compliance settings keeps Spilo's list.
	if libraries := preloadLibraries(cluster); libraries != "" {
		config[dcsPostgresql] = map[string]any{dcsParameters: map[string]any{paramPreload: libraries}}
	}
	out, err := yaml.Marshal(config)
	if err != nil {
		return "", fmt.Errorf("rendering SPILO_CONFIGURATION: %w", err)
	}
	return string(out), nil
}

// spiloEnv is the environment that turns the generic Spilo image into this
// cluster's member. Patroni's DCS is the Kubernetes API itself, scoped by the
// cluster name, with the role and scope labels below telling it which pods
// belong to the cluster and where to publish each pod's role.
func spiloEnv(cluster *dbv1alpha1.PostgresCluster) ([]corev1.EnvVar, error) {
	config, err := spiloConfiguration(cluster)
	if err != nil {
		return nil, err
	}
	backup := append(backupEnv(cluster), recoveryEnv(cluster)...)
	env := make([]corev1.EnvVar, 0, 16+len(backup))
	env = append(env, []corev1.EnvVar{
		{Name: "SCOPE", Value: cluster.Name},
		{Name: "PGVERSION", Value: cluster.Spec.Version},
		{Name: "PGROOT", Value: dataMountPath + "/pgroot"},
		{Name: "DCS_ENABLE_KUBERNETES_API", Value: envTrue},
		{Name: "KUBERNETES_ROLE_LABEL", Value: labelRole},
		{Name: "KUBERNETES_LEADER_LABEL_VALUE", Value: rolePrimary},
		{Name: "KUBERNETES_SCOPE_LABEL", Value: labelAppInstance},
		// Patroni adds the scope label itself; these are the rest. Together
		// they are exactly podSelectorLabels, so it only ever treats this
		// cluster's database pods as members and labels its own Endpoints
		// and ConfigMaps in a way cleanup can find.
		{Name: "KUBERNETES_LABELS", Value: fmt.Sprintf(
			`{"%s":"%s","%s":"%s"}`, labelAppName, appNameValue, labelAppComponent, componentPostgres)},
		{Name: "PGUSER_SUPERUSER", ValueFrom: secretKeyRef(cluster, secretKeyUsername)},
		{Name: "PGPASSWORD_SUPERUSER", ValueFrom: secretKeyRef(cluster, secretKeyPassword)},
		{Name: "PGUSER_STANDBY", Value: replicationUser},
		{Name: "PGPASSWORD_STANDBY", ValueFrom: secretKeyRef(cluster, secretKeyReplicationPassword)},
		{Name: "POD_IP", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.podIP"}}},
		{Name: "POD_NAMESPACE", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"}}},
		{Name: "SPILO_CONFIGURATION", Value: config},
	}...)
	return append(env, backup...), nil
}

// sharedMemoryLimit sizes /dev/shm. Postgres uses it for parallel query, and
// the container runtime's default (64Mi) is easily exhausted. It is backed by
// memory and counts against the container's memory limit, so it is capped at
// a quarter of that limit.
func sharedMemoryLimit(cluster *dbv1alpha1.PostgresCluster) *resource.Quantity {
	limit := maxSharedMemoryBytes
	if mem, ok := cluster.Spec.Resources.Limits[corev1.ResourceMemory]; ok && !mem.IsZero() {
		if quarter := mem.Value() / sharedMemoryFractionOfMemory; quarter < limit {
			limit = quarter
		}
	}
	return resource.NewQuantity(limit, resource.BinarySI)
}

// desiredStatefulSetAt builds the full StatefulSet for a cluster, including
// the storage-backed volume claim template, for a given credentials version:
// the number of times the superuser and replication passwords have been
// replaced. The pods read those passwords from their environment when they
// start, so the version is on the pod template, and raising it is what makes
// the rolling update that gives every pod the new ones. A cluster whose
// passwords never have been replaced has none, and its template is as it
// always was.
//
// Every pod runs the same single Spilo container and elects its role through
// Patroni, even for instances: 1, so scaling from one instance to three is
// the same code path as creating three.
func desiredStatefulSetAt(cluster *dbv1alpha1.PostgresCluster, image, credentialsVersion string) (*appsv1.StatefulSet, error) {
	env, err := spiloEnv(cluster)
	if err != nil {
		return nil, err
	}
	labels := labelsForCluster(cluster)
	selector := podSelectorLabels(cluster)
	replicas := cluster.Spec.Instances

	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cluster.Name,
			Namespace: cluster.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.StatefulSetSpec{
			ServiceName: headlessServiceName(cluster),
			Replicas:    &replicas,
			Selector:    &metav1.LabelSelector{MatchLabels: selector},
			// Pods must not wait for each other to become Ready: the first
			// pod cannot be Ready until Patroni elects it, and a replica
			// replaced during an outage should rejoin immediately, not queue
			// behind ordinal order.
			PodManagementPolicy: appsv1.ParallelPodManagement,
			// The operator replaces pods itself, replicas first and the primary
			// only after a switchover; see rollout.go.
			UpdateStrategy: appsv1.StatefulSetUpdateStrategy{Type: appsv1.OnDeleteStatefulSetStrategyType},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: selector, Annotations: credentialsVersionAnnotation(credentialsVersion)},
				Spec: corev1.PodSpec{
					ServiceAccountName: patroniAccountName(cluster),
					// Time for Patroni to demote and hand over leadership
					// cleanly before the kubelet kills the pod.
					TerminationGracePeriodSeconds: ptr.To(defaultTerminationGraceSecs),
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot:   ptr.To(true),
						RunAsUser:      ptr.To(spiloUID),
						RunAsGroup:     ptr.To(spiloGID),
						FSGroup:        ptr.To(spiloGID),
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					// Soft spreading: prefer different nodes so one node
					// failure cannot take all members, but still schedule on
					// a small cluster where that is impossible.
					TopologySpreadConstraints: []corev1.TopologySpreadConstraint{{
						MaxSkew:           1,
						TopologyKey:       corev1.LabelHostname,
						WhenUnsatisfiable: corev1.ScheduleAnyway,
						LabelSelector:     &metav1.LabelSelector{MatchLabels: selector},
					}},
					Containers: []corev1.Container{{
						Name:            postgresContainerName,
						Image:           image,
						ImagePullPolicy: corev1.PullIfNotPresent,
						Ports: []corev1.ContainerPort{
							{Name: postgresPortName, ContainerPort: postgresPort},
							{Name: patroniPortName, ContainerPort: patroniPort},
						},
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: ptr.To(false),
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{dropAllCapabilities}},
						},
						Env:       env,
						Resources: cluster.Spec.Resources,
						// Patroni's own health endpoints rather than
						// pg_isready: readiness is true only for a member
						// that is a healthy primary or a streaming replica.
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
								Path: "/readiness", Port: intstr.FromInt32(patroniPort)}},
							PeriodSeconds:    5,
							FailureThreshold: 3,
						},
						// Generous: restarting a pod that is merely busy
						// recovering would turn a slow start into an outage.
						LivenessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
								Path: "/liveness", Port: intstr.FromInt32(patroniPort)}},
							InitialDelaySeconds: 30,
							PeriodSeconds:       10,
							FailureThreshold:    6,
						},
						VolumeMounts: []corev1.VolumeMount{
							{Name: dataVolumeName, MountPath: dataMountPath},
							{Name: shmVolumeName, MountPath: "/dev/shm"},
						},
					}},
					Volumes: []corev1.Volume{{
						Name: shmVolumeName,
						VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
							Medium:    corev1.StorageMediumMemory,
							SizeLimit: sharedMemoryLimit(cluster),
						}},
					}},
				},
			},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{{
				ObjectMeta: metav1.ObjectMeta{Name: dataVolumeName, Labels: labels},
				Spec: corev1.PersistentVolumeClaimSpec{
					AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					Resources: corev1.VolumeResourceRequirements{
						Requests: corev1.ResourceList{corev1.ResourceStorage: cluster.Spec.Storage.Size},
					},
					StorageClassName: cluster.Spec.Storage.StorageClassName,
				},
			}},
		},
	}
	// The audit shipper rides in the database pod, because the log it ships is
	// on the pod's own volume.
	if auditShipperEnabled(cluster) {
		pod := &sts.Spec.Template.Spec
		pod.Containers = append(pod.Containers, auditShipperContainer(cluster, image))
		pod.Volumes = append(pod.Volumes, auditShipperVolume(cluster))
	}
	return sts, nil
}

func credentialsVersionAnnotation(version string) map[string]string {
	if version == "" {
		return nil
	}
	return map[string]string{annotationCredentialsVersion: version}
}
