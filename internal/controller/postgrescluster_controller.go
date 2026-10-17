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
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	dbv1alpha1 "github.com/tusharshrivastava62/postgres-operator/api/v1alpha1"
)

// leftoverRequeueDelay is how long to wait before checking again whether the
// pods of a deleted cluster are gone.
const leftoverRequeueDelay = 5 * time.Second

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

	// Patroni is how the operator reaches the members' REST API. Left nil it
	// uses a plain HTTP client; tests substitute a fake.
	Patroni PatroniAPI
}

// +kubebuilder:rbac:groups=db.youroperator.io,resources=postgresclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=db.youroperator.io,resources=postgresclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=db.youroperator.io,resources=postgresclusters/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=batch,resources=cronjobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings,verbs=get;list;watch;create;update;patch;delete
//
// The next three are Patroni's own permissions (see patroniRules). The
// operator needs them itself because Kubernetes only lets a principal create
// a Role granting permissions it already holds. It also deletes the Endpoints
// and ConfigMaps Patroni leaves behind when a cluster is deleted, which is
// why those two have delete and pods does not.
// +kubebuilder:rbac:groups="",resources=configmaps;endpoints,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;update;patch

// Reconcile moves the actual cluster state (Secret, Patroni's access
// objects, Services, PodDisruptionBudget, StatefulSet) closer to what a
// PostgresCluster's spec describes.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.24.1/pkg/reconcile
func (r *PostgresClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (res ctrl.Result, reterr error) {
	log := logf.FromContext(ctx)

	cluster := &dbv1alpha1.PostgresCluster{}
	if err := r.Get(ctx, req.NamespacedName, cluster); err != nil {
		if apierrors.IsNotFound(err) {
			// Owner references clean up everything the operator created.
			// What Patroni created is not owned by anything, see
			// reconcileDeleted. There is no series left to record metrics
			// against either - the object is gone.
			res, err = r.reconcileDeleted(ctx, req.NamespacedName)
		}
		return res, err
	}

	start := time.Now()
	defer func() {
		// A Conflict here means another goroutine updated the object between
		// our Get and a later Update (e.g. reconcileStatus's Status().Update
		// racing an owned-resource-triggered reconcile) - controller-runtime
		// already requeues on any returned error, so this self-heals on the
		// next attempt. It's labeled apart from "error" so an alert on
		// reconcile errors doesn't page someone for a normal, expected race.
		outcome := "success"
		if reterr != nil {
			outcome = "error"
			if apierrors.IsConflict(reterr) {
				outcome = "conflict"
			}
		}
		reconcileTotal.WithLabelValues(cluster.Namespace, cluster.Name, outcome).Inc()
		reconcileDurationSeconds.WithLabelValues(cluster.Namespace, cluster.Name).Observe(time.Since(start).Seconds())
	}()

	// Refuse to roll out a spec without a memory limit even though the
	// webhook rejects it: see hasMemoryLimit. Nothing existing is touched, so
	// a running cluster keeps running while the spec is wrong.
	if !hasMemoryLimit(cluster) {
		log.Info("not reconciling: spec.resources.limits.memory is required")
		return ctrl.Result{}, r.markInvalidSpec(ctx, cluster,
			"spec.resources.limits.memory is required: PostgreSQL sizes its memory from the container limit")
	}

	// A cluster that has never been reconciled has no status.image yet. Start
	// it from a clean slate: anything Patroni left from a deleted cluster of
	// the same name must go before the first pod starts, and that cannot be
	// done while the old pods are still shutting down.
	if cluster.Status.Image == "" {
		running, err := r.podsExist(ctx, cluster.Namespace, cluster.Name)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("checking for leftover pods: %w", err)
		}
		if running {
			log.Info("waiting for pods of a previous cluster with this name to terminate")
			return ctrl.Result{RequeueAfter: leftoverRequeueDelay}, nil
		}
		if err := r.cleanupPatroniState(ctx, cluster.Namespace, cluster.Name); err != nil {
			return ctrl.Result{}, fmt.Errorf("cleaning up leftover Patroni state: %w", err)
		}
	}

	image, ok := resolveImage(cluster)
	if !ok {
		return ctrl.Result{}, r.markInvalidSpec(ctx, cluster,
			fmt.Sprintf("no default image for version %q and spec.image is not set", cluster.Spec.Version))
	}
	// Record the image before anything uses it, so that if the operator is
	// upgraded between creating the first pod and finishing this reconcile
	// the cluster is not started on a different default.
	if cluster.Status.Image != image {
		cluster.Status.Image = image
		if err := r.Status().Update(ctx, cluster); err != nil {
			return ctrl.Result{}, fmt.Errorf("recording image: %w", err)
		}
	}

	if err := r.reconcileSecret(ctx, cluster); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling secret: %w", err)
	}
	// Before the StatefulSet: the pods cannot hold or renew the leader lock
	// without these, see reconcilePatroniAccess.
	if err := r.reconcilePatroniAccess(ctx, cluster); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling patroni access: %w", err)
	}
	if err := r.reconcileServices(ctx, cluster); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling services: %w", err)
	}
	if err := r.reconcilePodDisruptionBudget(ctx, cluster); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling pod disruption budget: %w", err)
	}
	if err := r.reconcileStatefulSet(ctx, cluster, image); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling statefulset: %w", err)
	}
	if err := r.reconcileBackup(ctx, cluster); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling backup: %w", err)
	}
	// Runs last and only on the happy path: status should reflect what the
	// steps above actually converged to, not guess at partial progress from
	// a step that just failed (that failure is already visible via the
	// reconcile error itself, surfaced in logs/events and retried).
	if err := r.reconcileStatus(ctx, cluster); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling status: %w", err)
	}

	// After status, so a Patroni that is slow to answer never delays
	// reporting. Nothing watches Patroni's configuration, so when there is no
	// leader to talk to yet the only way to get back here is to ask.
	synced, err := r.syncPatroniConfig(ctx, cluster)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("syncing patroni configuration: %w", err)
	}

	log.Info("reconciled PostgresCluster", "instances", cluster.Spec.Instances, "version", cluster.Spec.Version, "image", image)
	if !synced {
		return ctrl.Result{RequeueAfter: leaderRequeueDelay}, nil
	}
	return ctrl.Result{}, nil
}

// reconcileDeleted runs after a PostgresCluster is gone. The StatefulSet and
// the rest are garbage-collected through their owner references, but the
// Endpoints and ConfigMaps Patroni created are not owned by anything, so they
// are removed here, once the pods that would rewrite them have terminated.
//
// There is deliberately no finalizer: it would let a stuck cleanup block
// deleting the custom resource. If the operator is down when a cluster is
// deleted this never runs, and the same cleanup runs instead before a
// cluster of that name is next created (see Reconcile).
func (r *PostgresClusterReconciler) reconcileDeleted(ctx context.Context, key types.NamespacedName) (ctrl.Result, error) {
	sts := &appsv1.StatefulSet{}
	err := r.Get(ctx, key, sts)
	switch {
	case err == nil && len(sts.OwnerReferences) == 0:
		// Orphaned on purpose (kubectl delete --cascade=orphan): the
		// database is still running and its state must stay.
		return ctrl.Result{}, nil
	case err == nil:
		return ctrl.Result{RequeueAfter: leftoverRequeueDelay}, nil
	case !apierrors.IsNotFound(err):
		return ctrl.Result{}, err
	}

	running, err := r.podsExist(ctx, key.Namespace, key.Name)
	if err != nil {
		return ctrl.Result{}, err
	}
	if running {
		return ctrl.Result{RequeueAfter: leftoverRequeueDelay}, nil
	}
	return ctrl.Result{}, r.cleanupPatroniState(ctx, key.Namespace, key.Name)
}

// markInvalidSpec reports a spec the reconciler will not act on as
// Progressing=False with reason InvalidSpec, and does not return an error:
// retrying cannot fix a spec, and a change to it triggers a new reconcile.
func (r *PostgresClusterReconciler) markInvalidSpec(ctx context.Context, cluster *dbv1alpha1.PostgresCluster, message string) error {
	changed := meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
		Type:    "Progressing",
		Status:  metav1.ConditionFalse,
		Reason:  "InvalidSpec",
		Message: message,
	})
	if cluster.Status.ObservedGeneration != cluster.Generation {
		cluster.Status.ObservedGeneration = cluster.Generation
		changed = true
	}
	if !changed {
		return nil
	}
	return r.Status().Update(ctx, cluster)
}

// reconcileSecret ensures the credentials Secret exists, generating random
// passwords the first time.
//
// The Secret is deliberately NOT owned by the PostgresCluster, unlike
// everything else the operator creates. The data volumes outlive the cluster
// (they are never deleted automatically), and the passwords inside them are
// fixed when the database is first initialised: Patroni never resets them
// afterwards. If deleting a cluster also deleted this Secret, recreating a
// cluster of the same name on the retained volumes would generate new
// passwords that no longer match the database, and nobody could log in or
// replicate. Keeping the Secret means the old credentials are still there for
// the old data. Delete it by hand together with the volumes.
//
// A key that is already present is never rewritten:
// overwriting it on every reconcile would rotate the running cluster's
// password out from under it, since nothing here restarts Postgres or updates
// its password to match. A key that is missing from an existing Secret (the
// replication password, for a Secret created before replication existed) is
// added on its own, leaving the others alone.
func (r *PostgresClusterReconciler) reconcileSecret(ctx context.Context, cluster *dbv1alpha1.PostgresCluster) error {
	existing := &corev1.Secret{}
	err := r.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: credentialsSecretName(cluster)}, existing)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}

	if apierrors.IsNotFound(err) {
		existing = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      credentialsSecretName(cluster),
				Namespace: cluster.Namespace,
				Labels:    labelsForCluster(cluster),
			},
			Type: corev1.SecretTypeOpaque,
		}
		if err := fillMissingCredentials(existing); err != nil {
			return err
		}
		return r.Create(ctx, existing)
	}

	patch := client.MergeFrom(existing.DeepCopy())
	if !missingCredentials(existing) {
		return nil
	}
	if err := fillMissingCredentials(existing); err != nil {
		return err
	}
	return r.Patch(ctx, existing, patch)
}

// missingCredentials reports whether any key the cluster needs is absent.
func missingCredentials(secret *corev1.Secret) bool {
	for _, key := range []string{secretKeyUsername, secretKeyPassword, secretKeyReplicationPassword} {
		if _, ok := secret.Data[key]; !ok {
			return true
		}
	}
	return false
}

// fillMissingCredentials adds whichever of the credential keys are absent,
// and only those.
func fillMissingCredentials(secret *corev1.Secret) error {
	if secret.Data == nil {
		secret.Data = map[string][]byte{}
	}
	if _, ok := secret.Data[secretKeyUsername]; !ok {
		secret.Data[secretKeyUsername] = []byte(postgresSuperuser)
	}
	for _, key := range []string{secretKeyPassword, secretKeyReplicationPassword} {
		if _, ok := secret.Data[key]; ok {
			continue
		}
		password, err := generatePassword()
		if err != nil {
			return fmt.Errorf("generating %s: %w", key, err)
		}
		secret.Data[key] = []byte(password)
	}
	return nil
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

// reconcileStatus computes Available/Progressing/Degraded conditions from
// the actual state of the resources reconciled above and writes them to
// .status.
//
// It only calls Status().Update when something actually changed
// (meta.SetStatusCondition reports whether it did). Skipping the no-op
// write matters here specifically because this controller watches its own
// owned StatefulSet/Service/Secret/PVC/CronJob via Owns(), and .status
// changes on the PostgresCluster itself also re-trigger Reconcile - an
// unconditional Update on every pass would turn into a self-sustaining
// reconcile loop instead of settling once state stops changing.
func (r *PostgresClusterReconciler) reconcileStatus(ctx context.Context, cluster *dbv1alpha1.PostgresCluster) error {
	sts := &appsv1.StatefulSet{}
	stsErr := r.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: cluster.Name}, sts)
	if stsErr != nil && !apierrors.IsNotFound(stsErr) {
		return stsErr
	}

	var ready int32
	if stsErr == nil {
		ready = sts.Status.ReadyReplicas
	}
	desired := cluster.Spec.Instances
	instancesReady.WithLabelValues(cluster.Namespace, cluster.Name).Set(float64(ready))

	available := metav1.Condition{
		Type:    "Available",
		Status:  metav1.ConditionFalse,
		Reason:  "InstancesNotReady",
		Message: fmt.Sprintf("%d/%d instances ready", ready, desired),
	}
	progressing := metav1.Condition{
		Type:    "Progressing",
		Status:  metav1.ConditionTrue,
		Reason:  "ScalingUp",
		Message: fmt.Sprintf("%d/%d instances ready", ready, desired),
	}
	if desired > 0 && ready >= desired {
		available.Status = metav1.ConditionTrue
		available.Reason = "InstancesReady"
		progressing.Status = metav1.ConditionFalse
		progressing.Reason = "ReconcileComplete"
	}

	degraded, err := r.backupCondition(ctx, cluster)
	if err != nil {
		return err
	}

	changed := meta.SetStatusCondition(&cluster.Status.Conditions, available)
	changed = meta.SetStatusCondition(&cluster.Status.Conditions, progressing) || changed
	changed = meta.SetStatusCondition(&cluster.Status.Conditions, degraded) || changed
	if cluster.Status.ObservedGeneration != cluster.Generation {
		cluster.Status.ObservedGeneration = cluster.Generation
		changed = true
	}

	if !changed {
		return nil
	}
	return r.Status().Update(ctx, cluster)
}

// backupCondition reports backup health as a Degraded condition, driven by
// the most recently finished Job the backup CronJob spawned. There's no
// owner reference from PostgresCluster straight to those Jobs (they're
// owned by the CronJob), so this finds them the same way the CronJob
// controller's Pods find each other: by the labels the CronJob's JobTemplate
// stamps on every Job it creates.
func (r *PostgresClusterReconciler) backupCondition(ctx context.Context, cluster *dbv1alpha1.PostgresCluster) (metav1.Condition, error) {
	condition := metav1.Condition{Type: "Degraded", Status: metav1.ConditionFalse}

	if !cluster.Spec.Backup.Enabled {
		condition.Reason = "BackupDisabled"
		condition.Message = "scheduled backups are not enabled"
		return condition, nil
	}

	jobs := &batchv1.JobList{}
	if err := r.List(ctx, jobs, client.InNamespace(cluster.Namespace), client.MatchingLabels(labelsForCluster(cluster))); err != nil {
		return metav1.Condition{}, err
	}

	var latest *batchv1.Job
	for i := range jobs.Items {
		job := &jobs.Items[i]
		if !jobComplete(job) && !jobFailed(job) {
			continue
		}
		if latest == nil || job.CreationTimestamp.After(latest.CreationTimestamp.Time) {
			latest = job
		}
	}

	switch {
	case latest == nil:
		condition.Reason = "BackupPending"
		condition.Message = "no backup has finished yet"
	case jobFailed(latest):
		condition.Status = metav1.ConditionTrue
		condition.Reason = "BackupFailed"
		condition.Message = fmt.Sprintf("backup job %s failed", latest.Name)
	default:
		condition.Reason = "BackupHealthy"
		condition.Message = fmt.Sprintf("last backup job %s completed successfully", latest.Name)
		if latest.Status.CompletionTime != nil {
			backupLastSuccessTimestampSeconds.WithLabelValues(cluster.Namespace, cluster.Name).
				Set(float64(latest.Status.CompletionTime.Unix()))
		}
	}
	return condition, nil
}

func jobComplete(job *batchv1.Job) bool {
	return jobHasCondition(job, batchv1.JobComplete)
}

func jobFailed(job *batchv1.Job) bool {
	return jobHasCondition(job, batchv1.JobFailed)
}

func jobHasCondition(job *batchv1.Job, condType batchv1.JobConditionType) bool {
	for _, c := range job.Status.Conditions {
		if c.Type == condType && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
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

// SetupWithManager sets up the controller with the Manager.
func (r *PostgresClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&dbv1alpha1.PostgresCluster{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Owns(&batchv1.CronJob{}).
		// The three objects the database pods authenticate to the API with,
		// and the budget protecting them from drains: watched so that
		// deleting one is repaired immediately, not on the next unrelated
		// event.
		Owns(&corev1.ServiceAccount{}).
		Owns(&rbacv1.Role{}).
		Owns(&rbacv1.RoleBinding{}).
		Owns(&policyv1.PodDisruptionBudget{}).
		Named("postgrescluster").
		Complete(r)
}
