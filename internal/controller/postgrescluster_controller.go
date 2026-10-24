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
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	dbv1alpha1 "github.com/tusharshrivastava62/postgres-operator/api/v1alpha1"
)

// leftoverRequeueDelay is how long to wait before checking again whether the
// pods of a deleted cluster are gone.
const leftoverRequeueDelay = 5 * time.Second

// backupSecretRecheckDelay is how soon to look again for a backup Secret that
// is missing or malformed.
const backupSecretRecheckDelay = 30 * time.Second

// PostgresClusterReconciler reconciles a PostgresCluster object
type PostgresClusterReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// Patroni is how the operator reaches the members' REST API. Left nil it
	// uses a plain HTTP client; tests substitute a fake.
	Patroni PatroniAPI

	// Operator says where the operator itself runs, which a cluster's network
	// policy needs to let it reach Patroni. Left nil, a cluster that wants a
	// network policy is reported, not built.
	Operator *OperatorIdentity

	// lastPrimary remembers, per cluster, the last primary this process
	// observed, so a change of primary can be counted. It is kept in memory
	// because the status field is cleared in the gap between two primaries,
	// which is the very moment that has to be remembered.
	lastPrimary sync.Map

	// SQL is how the operator runs SQL against a cluster. Left nil, the
	// operator runs none, and does not manage database roles: tests that are
	// not about roles leave it out, and the manager always sets it.
	SQL SQLDialer

	// monitorVerified remembers, per cluster, when the monitor role was last
	// found in order, see reconcileMonitorRole.
	monitorVerified sync.Map

	// rolesVerified remembers, per cluster, when its application roles were
	// last found in order, see reconcileRoles.
	rolesVerified sync.Map
}

// +kubebuilder:rbac:groups=db.youroperator.io,resources=postgresclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=db.youroperator.io,resources=postgresclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=db.youroperator.io,resources=postgresclusters/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=cronjobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings,verbs=get;list;watch;create;update;patch;delete
//
// The next three are Patroni's own permissions (see patroniRules). The
// operator needs them itself because Kubernetes only lets a principal create
// a Role granting permissions it already holds. It also deletes the Endpoints
// and ConfigMaps Patroni leaves behind when a cluster is deleted, which is
// why those two have delete. Pods are deleted too, by the rolling update,
// which replaces them one at a time in a safe order (see rollout.go).
// +kubebuilder:rbac:groups="",resources=configmaps;endpoints,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;update;patch;delete

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
		return ctrl.Result{}, r.markInvalidSpec(ctx, cluster, "InvalidSpec",
			"spec.resources.limits.memory is required: PostgreSQL sizes its memory from the container limit")
	}

	// Backups name Secrets that the pods read as environment variables. A pod
	// whose Secret is missing cannot start, so a typo in a running cluster's
	// spec would take every restarted member down. Check first, and change
	// nothing until the Secrets are right. Nothing watches those Secrets, so
	// look again after a while.
	problem, err := r.backupSecretProblem(ctx, cluster)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("checking backup secrets: %w", err)
	}
	if problem != "" {
		log.Info("not reconciling: " + problem)
		return ctrl.Result{RequeueAfter: backupSecretRecheckDelay}, r.markInvalidSpec(ctx, cluster, "BackupSecretProblem", problem)
	}

	// The webhook refuses roles that clash with the system's own, or with each
	// other's Secrets, but a webhook is not always there. Roles are made with
	// the superuser, so a name that slipped through must not reach it.
	if errs := cluster.Spec.ValidateRoles(cluster.Name); len(errs) > 0 {
		message := errs.ToAggregate().Error()
		log.Info("not reconciling: invalid spec.roles: " + message)
		return ctrl.Result{}, r.markInvalidSpec(ctx, cluster, "InvalidRoles", message)
	}

	// A network policy has to name the operator, or it would cut the operator
	// off from Patroni. Without knowing which pods it is, build nothing.
	if networkPolicyWanted(cluster) && r.Operator == nil {
		const message = "the operator could not tell which pods it runs as, which the network policy needs to let it through: " +
			"set POD_NAMESPACE and POD_NAME from the downward API on its pod"
		log.Info("not reconciling: " + message)
		return ctrl.Result{}, r.markInvalidSpec(ctx, cluster, reasonOperatorIdentity, message)
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
		return ctrl.Result{}, r.markInvalidSpec(ctx, cluster, "InvalidSpec",
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

	// A cluster restored from backups is not created until the backups have
	// been checked, see reconcileRecoveryCheck.
	proceed, wait, err := r.reconcileRecoveryCheck(ctx, cluster, image)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("checking the backups to restore from: %w", err)
	}
	if !proceed {
		return ctrl.Result{RequeueAfter: wait}, nil
	}

	// In this order: the pods cannot hold or renew the leader lock without
	// Patroni's access objects (see reconcilePatroniAccess), and they must never
	// be reachable by more than their network policy allows, nor start without
	// the script they mount (see reconcileStatefulSet).
	var roleSecretProblems map[string]string
	steps := []struct {
		failure string
		run     func() error
	}{
		{"reconciling secret", func() error { return r.reconcileSecret(ctx, cluster) }},
		{"reconciling the monitor secret", func() error { return r.reconcileMonitorSecret(ctx, cluster) }},
		{"reconciling the role secrets", func() (err error) {
			roleSecretProblems, err = r.reconcileRoleSecrets(ctx, cluster)
			return err
		}},
		{"reconciling patroni access", func() error { return r.reconcilePatroniAccess(ctx, cluster) }},
		{"reconciling services", func() error { return r.reconcileServices(ctx, cluster) }},
		{"reconciling pod disruption budget", func() error { return r.reconcilePodDisruptionBudget(ctx, cluster) }},
		{"reconciling network policy", func() error { return r.reconcileNetworkPolicy(ctx, cluster) }},
		{"reconciling statefulset", func() error { return r.reconcileStatefulSet(ctx, cluster, image) }},
		{"removing the old backup job", func() error { return r.removeLegacyBackupCronJob(ctx, cluster) }},
		{"reconciling the backup jobs", func() error { return r.reconcileBackupJobs(ctx, cluster, image) }},
		{"reconciling the compliance jobs", func() error { return r.reconcileComplianceJobs(ctx, cluster, image) }},
	}
	for _, step := range steps {
		if err := step.run(); err != nil {
			return ctrl.Result{}, fmt.Errorf("%s: %w", step.failure, err)
		}
	}
	// Runs last and only on the happy path: status should reflect what the
	// steps above actually converged to, not guess at partial progress from
	// a step that just failed (that failure is already visible via the
	// reconcile error itself, surfaced in logs/events and retried).
	view, err := r.reconcileStatus(ctx, cluster)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling status: %w", err)
	}
	rolling, err := r.reconcileRollout(ctx, cluster, view)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("rolling update: %w", err)
	}

	// After status, so a Patroni that is slow to answer never delays
	// reporting. Nothing watches Patroni's configuration, so when there is no
	// leader to talk to yet the only way to get back here is to ask.
	synced, err := r.syncPatroniConfig(ctx, cluster)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("syncing patroni configuration: %w", err)
	}

	// Needs a primary, and so comes after everything that creates one. A role
	// that is not ready yet is looked at again soon.
	monitorReady, err := r.reconcileMonitorRole(ctx, cluster)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling the monitor role: %w", err)
	}
	rolesReady, err := r.reconcileRoles(ctx, cluster, roleSecretProblems)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling the application roles: %w", err)
	}
	// Last of the three: after the superuser's password is replaced, the first
	// two must see the new one, and they will on the next look.
	rotatingCredentials, err := r.reconcileCredentialRotation(ctx, cluster, view.healthy, rolling)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("replacing the superuser and replication passwords: %w", err)
	}
	r.recordCredentialAges(ctx, cluster)

	log.Info("reconciled PostgresCluster", "instances", cluster.Spec.Instances, "version", cluster.Spec.Version, "image", image)
	// Pod and Endpoints events trigger most re-observation, but replication
	// lag and an expiring leader lock change without one, so keep looking:
	// often while something is wrong, rarely when all is well.
	return ctrl.Result{RequeueAfter: refreshDelay(refreshInputs{
		rolling: rolling, synced: synced, healthy: view.healthy,
		monitorReady: monitorReady, rolesReady: rolesReady, rotatingCredentials: rotatingCredentials,
	})}, nil
}

// refreshInputs is what decides how soon a cluster is looked at again.
type refreshInputs struct {
	rolling, synced, healthy, monitorReady, rolesReady bool
	// rotatingCredentials is whether the superuser and replication passwords
	// are being replaced, or are due and waiting to be.
	rotatingCredentials bool
}

// refreshDelay is how long to wait before looking at a cluster again: soon
// while it is being rolled, often while something is not as it should be, and
// rarely otherwise.
func refreshDelay(in refreshInputs) time.Duration {
	switch {
	case in.rolling:
		return rolloutRequeueDelay
	case !in.synced || !in.healthy || !in.monitorReady || !in.rolesReady || in.rotatingCredentials:
		return unhealthyRefreshInterval
	}
	return healthyRefreshInterval
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
	// The cluster is gone, so stop reporting on it whatever happens next.
	r.forgetMetrics(key)
	r.monitorVerified.Delete(key.String())

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
// Progressing=False with the given reason, and does not return an error:
// retrying cannot fix a spec, and a change to it triggers a new reconcile.
func (r *PostgresClusterReconciler) markInvalidSpec(
	ctx context.Context, cluster *dbv1alpha1.PostgresCluster, reason, message string,
) error {
	changed := meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
		Type:    "Progressing",
		Status:  metav1.ConditionFalse,
		Reason:  reason,
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
		// Only spec changes (generation) are of interest. The controller
		// writes status itself, and replication lag makes that frequent, so
		// reacting to its own status updates would loop.
		For(&dbv1alpha1.PostgresCluster{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		Owns(&batchv1.CronJob{}).
		// The three objects the database pods authenticate to the API with,
		// and the budget protecting them from drains: watched so that
		// deleting one is repaired immediately, not on the next unrelated
		// event.
		Owns(&corev1.ConfigMap{}).
		Owns(&corev1.ServiceAccount{}).
		Owns(&rbacv1.Role{}).
		Owns(&rbacv1.RoleBinding{}).
		Owns(&policyv1.PodDisruptionBudget{}).
		Owns(&networkingv1.NetworkPolicy{}).
		// Neither of these is owned by the PostgresCluster: pods belong to the
		// StatefulSet and Patroni owns the Endpoints, so they are mapped back
		// to their cluster by label. Both are filtered to the changes that
		// matter, because Patroni rewrites them every few seconds.
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(clusterForObject),
			builder.WithPredicates(podChangePredicate())).
		Watches(&patroniEndpoints{}, handler.EnqueueRequestsFromMapFunc(clusterForObject),
			builder.WithPredicates(leaderChangePredicate())).
		Named("postgrescluster").
		Complete(r)
}
