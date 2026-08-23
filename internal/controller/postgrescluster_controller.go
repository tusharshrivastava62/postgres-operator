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
	"crypto/rand"
	"encoding/base64"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	dbv1alpha1 "github.com/tusharshrivastava62/postgres-operator/api/v1alpha1"
)

const (
	postgresPort          int32 = 5432
	postgresPortName            = "postgres"
	postgresContainerName       = "postgres"
	postgresSuperuser           = "postgres"
	dataVolumeName              = "data"
	backupVolumeName            = "backup"
	secretKeyUsername           = "username"
	secretKeyPassword           = "password"
)

// backupScript runs a full-cluster dump (all databases plus roles and other
// global objects) and writes it, gzip-compressed, to the backup PVC with a
// timestamped name so successive runs don't clobber each other.
const backupScript = `set -eu
pg_dumpall | gzip > "/backups/$(date -u +%Y%m%dT%H%M%SZ).sql.gz"
`

// PostgresClusterReconciler reconciles a PostgresCluster object
type PostgresClusterReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=db.db.youroperator.io,resources=postgresclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=db.db.youroperator.io,resources=postgresclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=db.db.youroperator.io,resources=postgresclusters/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=batch,resources=cronjobs,verbs=get;list;watch;create;update;patch;delete

// Reconcile moves the actual cluster state (Secret, Services, StatefulSet)
// closer to what a PostgresCluster's spec describes.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.24.1/pkg/reconcile
func (r *PostgresClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	cluster := &dbv1alpha1.PostgresCluster{}
	if err := r.Get(ctx, req.NamespacedName, cluster); err != nil {
		// Deletion: owner references handle cleanup of everything we create
		// below, so there's nothing left to do.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if err := r.reconcileSecret(ctx, cluster); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling secret: %w", err)
	}
	if err := r.reconcileHeadlessService(ctx, cluster); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling headless service: %w", err)
	}
	if err := r.reconcileService(ctx, cluster); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling service: %w", err)
	}
	if err := r.reconcileStatefulSet(ctx, cluster); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling statefulset: %w", err)
	}
	if err := r.reconcileBackup(ctx, cluster); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling backup: %w", err)
	}

	log.Info("reconciled PostgresCluster", "instances", cluster.Spec.Instances, "version", cluster.Spec.Version)
	return ctrl.Result{}, nil
}

// reconcileSecret ensures the credentials Secret exists, generating a random
// password the first time. It deliberately never touches the Secret again
// once created: overwriting StringData on every reconcile would rotate the
// running cluster's password out from under it, since nothing here restarts
// Postgres or updates its password to match.
func (r *PostgresClusterReconciler) reconcileSecret(ctx context.Context, cluster *dbv1alpha1.PostgresCluster) error {
	existing := &corev1.Secret{}
	err := r.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: credentialsSecretName(cluster)}, existing)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}

	password, err := generatePassword()
	if err != nil {
		return fmt.Errorf("generating password: %w", err)
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      credentialsSecretName(cluster),
			Namespace: cluster.Namespace,
			Labels:    labelsForCluster(cluster),
		},
		Type: corev1.SecretTypeOpaque,
		StringData: map[string]string{
			secretKeyUsername: postgresSuperuser,
			secretKeyPassword: password,
		},
	}
	if err := controllerutil.SetControllerReference(cluster, secret, r.Scheme); err != nil {
		return err
	}
	return r.Create(ctx, secret)
}

// reconcileHeadlessService ensures the headless (ClusterIP: None) Service
// StatefulSet needs for stable per-pod DNS.
func (r *PostgresClusterReconciler) reconcileHeadlessService(ctx context.Context, cluster *dbv1alpha1.PostgresCluster) error {
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: headlessServiceName(cluster), Namespace: cluster.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		svc.Labels = labelsForCluster(cluster)
		svc.Spec.ClusterIP = corev1.ClusterIPNone
		svc.Spec.Selector = labelsForCluster(cluster)
		// Not-yet-ready pods still need resolvable DNS so peers can find
		// them during startup/replication - a StatefulSet-with-Postgres
		// convention, not just a headless-Service default.
		svc.Spec.PublishNotReadyAddresses = true
		svc.Spec.Ports = []corev1.ServicePort{
			{Name: postgresPortName, Port: postgresPort, TargetPort: intstr.FromInt32(postgresPort)},
		}
		return controllerutil.SetControllerReference(cluster, svc, r.Scheme)
	})
	return err
}

// reconcileService ensures the regular ClusterIP Service clients connect
// through.
func (r *PostgresClusterReconciler) reconcileService(ctx context.Context, cluster *dbv1alpha1.PostgresCluster) error {
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: cluster.Name, Namespace: cluster.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		svc.Labels = labelsForCluster(cluster)
		svc.Spec.Selector = labelsForCluster(cluster)
		svc.Spec.Ports = []corev1.ServicePort{
			{Name: postgresPortName, Port: postgresPort, TargetPort: intstr.FromInt32(postgresPort)},
		}
		return controllerutil.SetControllerReference(cluster, svc, r.Scheme)
	})
	return err
}

// reconcileStatefulSet ensures the StatefulSet running Postgres exists and
// matches the spec.
//
// This intentionally does NOT use the CreateOrUpdate/mutate pattern used
// for the Services above. spec.volumeClaimTemplates is immutable once a
// StatefulSet is created, and the API server also defaults sub-fields on it
// (e.g. VolumeMode) that our builder doesn't set - so rebuilding and
// re-submitting that slice on every reconcile would trigger a spurious diff
// against the persisted object and the Update would be rejected. Instead:
// build the full object (including VolumeClaimTemplates) only for Create,
// and on Update touch just the fields Kubernetes actually allows to change
// after creation (replicas, pod template, labels).
func (r *PostgresClusterReconciler) reconcileStatefulSet(ctx context.Context, cluster *dbv1alpha1.PostgresCluster) error {
	desired := desiredStatefulSet(cluster)
	if err := controllerutil.SetControllerReference(cluster, desired, r.Scheme); err != nil {
		return err
	}

	existing := &appsv1.StatefulSet{}
	err := r.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: cluster.Name}, existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}

	existing.Labels = desired.Labels
	existing.Spec.Replicas = desired.Spec.Replicas
	existing.Spec.Template = desired.Spec.Template
	return r.Update(ctx, existing)
}

// desiredStatefulSet builds the full StatefulSet for a cluster, including
// the storage-backed volume claim template. Only used on first create; see
// reconcileStatefulSet for why updates go through a narrower path.
func desiredStatefulSet(cluster *dbv1alpha1.PostgresCluster) *appsv1.StatefulSet {
	labels := labelsForCluster(cluster)
	replicas := cluster.Spec.Instances

	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cluster.Name,
			Namespace: cluster.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.StatefulSetSpec{
			ServiceName: headlessServiceName(cluster),
			Replicas:    &replicas,
			Selector:    &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  postgresContainerName,
							Image: fmt.Sprintf("postgres:%s", cluster.Spec.Version),
							Ports: []corev1.ContainerPort{
								{Name: postgresPortName, ContainerPort: postgresPort},
							},
							Env: []corev1.EnvVar{
								{Name: "POSTGRES_USER", ValueFrom: secretKeyRef(cluster, secretKeyUsername)},
								{Name: "POSTGRES_PASSWORD", ValueFrom: secretKeyRef(cluster, secretKeyPassword)},
								// Postgres refuses to initdb straight into a
								// volume's mount point if it isn't empty
								// (e.g. it has a lost+found dir), so this
								// puts $PGDATA in a subdirectory instead -
								// the standard workaround the official image
								// documents.
								{Name: "PGDATA", Value: "/var/lib/postgresql/data/pgdata"},
							},
							Resources: cluster.Spec.Resources,
							VolumeMounts: []corev1.VolumeMount{
								{Name: dataVolumeName, MountPath: "/var/lib/postgresql/data"},
							},
							ReadinessProbe: pgIsReadyProbe(5, 10),
							LivenessProbe:  pgIsReadyProbe(30, 10),
						},
					},
				},
			},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
				{
					ObjectMeta: metav1.ObjectMeta{Name: dataVolumeName, Labels: labels},
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
						Resources: corev1.VolumeResourceRequirements{
							Requests: corev1.ResourceList{corev1.ResourceStorage: cluster.Spec.Storage.Size},
						},
						StorageClassName: cluster.Spec.Storage.StorageClassName,
					},
				},
			},
		},
	}
}

// reconcileBackup ensures the scheduled backup CronJob matches
// spec.backup, creating it (and its destination PVC) when enabled and
// removing it when not - so flipping backup.enabled off actually stops
// the schedule instead of leaving a stale CronJob behind.
//
// There's no dedicated backup-storage field on the spec yet, so the
// backup PVC reuses spec.storage.size as its own size. That's a
// deliberately simple default for a single dev/small-prod cluster; a
// cluster that needs a different backup retention footprint will need
// its own size/destination field, which is a CRD change, not something
// to guess at here.
func (r *PostgresClusterReconciler) reconcileBackup(ctx context.Context, cluster *dbv1alpha1.PostgresCluster) error {
	if !cluster.Spec.Backup.Enabled {
		return r.deleteBackupCronJob(ctx, cluster)
	}
	if cluster.Spec.Backup.Schedule == "" {
		return fmt.Errorf("backup.enabled is true but backup.schedule is empty")
	}

	if err := r.reconcileBackupPVC(ctx, cluster); err != nil {
		return err
	}

	cj := &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: backupName(cluster), Namespace: cluster.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, cj, func() error {
		labels := labelsForCluster(cluster)
		successHistory := int32(3)
		failHistory := int32(3)
		backoffLimit := int32(2)

		cj.Labels = labels
		cj.Spec.Schedule = cluster.Spec.Backup.Schedule
		cj.Spec.ConcurrencyPolicy = batchv1.ForbidConcurrent
		cj.Spec.SuccessfulJobsHistoryLimit = &successHistory
		cj.Spec.FailedJobsHistoryLimit = &failHistory
		cj.Spec.JobTemplate = batchv1.JobTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: labels},
			Spec: batchv1.JobSpec{
				BackoffLimit: &backoffLimit,
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: labels},
					Spec: corev1.PodSpec{
						RestartPolicy: corev1.RestartPolicyOnFailure,
						Containers: []corev1.Container{
							{
								Name:    "backup",
								Image:   fmt.Sprintf("postgres:%s", cluster.Spec.Version),
								Command: []string{"sh", "-c", backupScript},
								Env: []corev1.EnvVar{
									{Name: "PGHOST", Value: cluster.Name},
									{Name: "PGPORT", Value: fmt.Sprintf("%d", postgresPort)},
									{Name: "PGUSER", ValueFrom: secretKeyRef(cluster, secretKeyUsername)},
									{Name: "PGPASSWORD", ValueFrom: secretKeyRef(cluster, secretKeyPassword)},
								},
								VolumeMounts: []corev1.VolumeMount{
									{Name: backupVolumeName, MountPath: "/backups"},
								},
							},
						},
						Volumes: []corev1.Volume{
							{
								Name: backupVolumeName,
								VolumeSource: corev1.VolumeSource{
									PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: backupName(cluster)},
								},
							},
						},
					},
				},
			},
		}
		return controllerutil.SetControllerReference(cluster, cj, r.Scheme)
	})
	return err
}

func (r *PostgresClusterReconciler) deleteBackupCronJob(ctx context.Context, cluster *dbv1alpha1.PostgresCluster) error {
	cj := &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: backupName(cluster), Namespace: cluster.Namespace}}
	err := r.Delete(ctx, cj)
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// reconcileBackupPVC ensures the backup destination volume exists. Like
// the credentials Secret, this only creates it once: existing backups live
// on this volume, so nothing here should ever delete or resize it out from
// under a running schedule - including if backups are later disabled and
// re-enabled.
func (r *PostgresClusterReconciler) reconcileBackupPVC(ctx context.Context, cluster *dbv1alpha1.PostgresCluster) error {
	existing := &corev1.PersistentVolumeClaim{}
	err := r.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: backupName(cluster)}, existing)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      backupName(cluster),
			Namespace: cluster.Namespace,
			Labels:    labelsForCluster(cluster),
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: cluster.Spec.Storage.Size},
			},
			StorageClassName: cluster.Spec.Storage.StorageClassName,
		},
	}
	if err := controllerutil.SetControllerReference(cluster, pvc, r.Scheme); err != nil {
		return err
	}
	return r.Create(ctx, pvc)
}

func pgIsReadyProbe(initialDelaySeconds, periodSeconds int32) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			Exec: &corev1.ExecAction{Command: []string{"pg_isready", "-U", postgresSuperuser}},
		},
		InitialDelaySeconds: initialDelaySeconds,
		PeriodSeconds:       periodSeconds,
		TimeoutSeconds:      5,
	}
}

func secretKeyRef(cluster *dbv1alpha1.PostgresCluster, key string) *corev1.EnvVarSource {
	return &corev1.EnvVarSource{
		SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: credentialsSecretName(cluster)},
			Key:                  key,
		},
	}
}

func generatePassword() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func labelsForCluster(cluster *dbv1alpha1.PostgresCluster) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":     "postgrescluster",
		"app.kubernetes.io/instance": cluster.Name,
	}
}

func credentialsSecretName(cluster *dbv1alpha1.PostgresCluster) string {
	return cluster.Name + "-credentials"
}

func headlessServiceName(cluster *dbv1alpha1.PostgresCluster) string {
	return cluster.Name + "-headless"
}

func backupName(cluster *dbv1alpha1.PostgresCluster) string {
	return cluster.Name + "-backup"
}

// SetupWithManager sets up the controller with the Manager.
func (r *PostgresClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&dbv1alpha1.PostgresCluster{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.Secret{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Owns(&batchv1.CronJob{}).
		Named("postgrescluster").
		Complete(r)
}
