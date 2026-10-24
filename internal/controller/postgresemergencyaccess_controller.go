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
	"fmt"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	dbv1alpha1 "github.com/tusharshrivastava62/postgres-operator/api/v1alpha1"
)

const (
	// finalizerEmergencyAccess keeps a request from being deleted until the
	// access it granted has been taken away.
	finalizerEmergencyAccess = "db.youroperator.io/emergency-access"

	// emergencyRetryDelay is how soon to try again what could not be done.
	emergencyRetryDelay = 15 * time.Second

	// emergencyExpiryGrace is waited past a grant's end before acting on it,
	// so the clocks of the operator and of the database cannot disagree about
	// whether it has ended.
	emergencyExpiryGrace = time.Second

	// Events.
	reasonGranted = "AccessGranted"
	reasonEnded   = "AccessEnded"
	reasonDenied  = "AccessDenied"
	reasonFailed  = "AccessFailed"
)

// PostgresEmergencyAccessReconciler grants, and takes away, temporary superuser
// access to a cluster for the people who ask for it.
//
// What it grants is a login role, bg_<id>, with a password in a Secret that is
// deleted when the access ends. The role can do nothing until its owner calls
// set_user_u('postgres'), which the server logs, along with every statement
// that follows.
type PostgresEmergencyAccessReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// SQL is how the operator runs SQL against the cluster. Without it, no
	// access can be granted.
	SQL SQLDialer

	// Recorder says what happened, in events on the request.
	Recorder events.EventRecorder

	// Now is the time. Left nil it is the clock; specs set it.
	Now func() time.Time
}

// +kubebuilder:rbac:groups=db.youroperator.io,resources=postgresemergencyaccesses,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=db.youroperator.io,resources=postgresemergencyaccesses/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=db.youroperator.io,resources=postgresemergencyaccesses/finalizers,verbs=update
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

func (r *PostgresEmergencyAccessReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Reconcile moves a request through its life: asked for, granted, ended.
func (r *PostgresEmergencyAccessReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	request := &dbv1alpha1.PostgresEmergencyAccess{}
	if err := r.Get(ctx, req.NamespacedName, request); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !request.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, request)
	}
	if controllerutil.AddFinalizer(request, finalizerEmergencyAccess) {
		if err := r.Update(ctx, request); err != nil {
			return ctrl.Result{}, err
		}
	}

	cluster, err := r.clusterOf(ctx, request)
	if err != nil {
		return ctrl.Result{}, err
	}

	switch request.Status.State {
	case dbv1alpha1.EmergencyExpired, dbv1alpha1.EmergencyRevoked, dbv1alpha1.EmergencyDenied:
		return ctrl.Result{}, nil
	case dbv1alpha1.EmergencyActive:
		return r.keepOrEnd(ctx, request, cluster)
	}
	return r.grant(ctx, request, cluster)
}

// clusterOf finds the request's cluster; nil if there is none.
func (r *PostgresEmergencyAccessReconciler) clusterOf(ctx context.Context, request *dbv1alpha1.PostgresEmergencyAccess) (*dbv1alpha1.PostgresCluster, error) {
	cluster := &dbv1alpha1.PostgresCluster{}
	err := r.Get(ctx, client.ObjectKey{Namespace: request.Namespace, Name: request.Spec.ClusterRef.Name}, cluster)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	return cluster, err
}

// refusal is why a request is not granted, or "" if it can be.
func refusal(request *dbv1alpha1.PostgresEmergencyAccess, cluster *dbv1alpha1.PostgresCluster) string {
	switch {
	case request.Annotations[dbv1alpha1.AnnotationRequestedBy] == "":
		return "the operator could not tell who made this request, so it does not grant it: " +
			"the webhook that records the requester is not working"
	case cluster == nil:
		return fmt.Sprintf("there is no cluster %s in this namespace", request.Spec.ClusterRef.Name)
	case !cluster.Spec.EmergencyAccess.Allowed():
		return fmt.Sprintf("cluster %s does not allow emergency access: spec.emergencyAccess.enabled is not set", cluster.Name)
	case request.Spec.EffectiveDuration() > cluster.Spec.EmergencyAccess.MaxAllowed():
		return fmt.Sprintf("the request is for %s and cluster %s allows at most %s",
			request.Spec.EffectiveDuration(), cluster.Name, cluster.Spec.EmergencyAccess.MaxAllowed())
	}
	return ""
}

// grant makes the access, once, and records when it ends. A grant that cannot
// be made yet is tried again; one that must not be is denied.
func (r *PostgresEmergencyAccessReconciler) grant(
	ctx context.Context, request *dbv1alpha1.PostgresEmergencyAccess, cluster *dbv1alpha1.PostgresCluster,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	now := r.now()

	if reason := refusal(request, cluster); reason != "" {
		// A missing cluster may only not be created yet; the rest is final.
		if cluster == nil && request.Annotations[dbv1alpha1.AnnotationRequestedBy] != "" {
			return r.pending(ctx, request, dbv1alpha1.EmergencyFailed, reason)
		}
		r.event(request, "Warning", reasonDenied, "Deny", reason)
		return ctrl.Result{}, r.setStatus(ctx, request, func(s *dbv1alpha1.PostgresEmergencyAccessStatus) {
			s.State, s.Message, s.EndedAt = dbv1alpha1.EmergencyDenied, "Denied: "+reason, timePtr(now)
			s.RequestedBy = request.Annotations[dbv1alpha1.AnnotationRequestedBy]
		})
	}
	if r.SQL == nil {
		return r.pending(ctx, request, dbv1alpha1.EmergencyFailed, "the operator has no SQL channel to the cluster")
	}

	// The end is fixed the first time, and kept through every retry.
	expires := now.Add(request.Spec.EffectiveDuration())
	if request.Status.ExpiresAt != nil {
		expires = request.Status.ExpiresAt.Time
	}
	role := emergencyRoleName(request.UID)
	if err := r.setStatus(ctx, request, func(s *dbv1alpha1.PostgresEmergencyAccessStatus) {
		if s.State == "" {
			s.State = dbv1alpha1.EmergencyPending
		}
		s.RequestedBy = request.Annotations[dbv1alpha1.AnnotationRequestedBy]
		s.Role, s.SecretName, s.ExpiresAt = role, emergencySecretName(request), timePtr(expires)
	}); err != nil {
		return ctrl.Result{}, err
	}
	if !now.Before(expires) {
		return r.end(ctx, request, cluster, dbv1alpha1.EmergencyExpired, "the time ran out before the access could be granted")
	}

	password, err := r.ensureSecret(ctx, request, cluster)
	if err != nil {
		return r.pending(ctx, request, dbv1alpha1.EmergencyFailed, "could not write the login: "+err.Error())
	}
	if err := r.apply(ctx, request, cluster, role, password, expires); err != nil {
		log.Info("Could not grant emergency access yet", "request", request.Name, "error", err.Error())
		return r.pending(ctx, request, dbv1alpha1.EmergencyFailed, "could not grant the access yet: "+err.Error())
	}

	started := request.Status.StartedAt
	if started == nil {
		started = timePtr(now)
	}
	if err := r.setStatus(ctx, request, func(s *dbv1alpha1.PostgresEmergencyAccessStatus) {
		s.State, s.StartedAt = dbv1alpha1.EmergencyActive, started
		s.Message = grantedMessage(role, request.Status.SecretName, expires)
	}); err != nil {
		return ctrl.Result{}, err
	}
	r.event(request, "Normal", reasonGranted, "Grant", "Access granted to %s until %s", request.Annotations[dbv1alpha1.AnnotationRequestedBy], expires.UTC().Format(time.RFC3339))
	log.Info("Granted emergency access", "request", request.Name, "cluster", cluster.Name, "role", role,
		"requestedBy", request.Annotations[dbv1alpha1.AnnotationRequestedBy], "reason", request.Spec.Reason, "expires", expires)
	return ctrl.Result{RequeueAfter: expires.Sub(now) + emergencyExpiryGrace}, nil
}

func grantedMessage(role, secret string, expires time.Time) string {
	return fmt.Sprintf("Active until %s. Log in as %s with the details in Secret %s, then run SELECT set_user_u('postgres'). "+
		"WARNING: %s", expires.UTC().Format(time.RFC3339), role, secret, dbv1alpha1.ParameterLoggingWarning)
}

// keepOrEnd ends an active grant that has run out, or that the cluster no
// longer allows, and otherwise makes sure it is still whole.
func (r *PostgresEmergencyAccessReconciler) keepOrEnd(
	ctx context.Context, request *dbv1alpha1.PostgresEmergencyAccess, cluster *dbv1alpha1.PostgresCluster,
) (ctrl.Result, error) {
	now := r.now()
	expires := request.Status.ExpiresAt.Time
	switch {
	case !now.Before(expires):
		return r.end(ctx, request, cluster, dbv1alpha1.EmergencyExpired, "the time ran out")
	case cluster == nil:
		return r.end(ctx, request, cluster, dbv1alpha1.EmergencyRevoked, "the cluster is gone")
	case !cluster.Spec.EmergencyAccess.Allowed():
		return r.end(ctx, request, cluster, dbv1alpha1.EmergencyRevoked, "the cluster no longer allows emergency access")
	}
	// A login that was deleted is made again, with a new password.
	secret := &corev1.Secret{}
	err := r.Get(ctx, client.ObjectKey{Namespace: request.Namespace, Name: emergencySecretName(request)}, secret)
	if apierrors.IsNotFound(err) {
		return r.grant(ctx, request, cluster)
	}
	return ctrl.Result{RequeueAfter: expires.Sub(now) + emergencyExpiryGrace}, err
}

// end takes the access away, and says how it ended.
func (r *PostgresEmergencyAccessReconciler) end(
	ctx context.Context, request *dbv1alpha1.PostgresEmergencyAccess, cluster *dbv1alpha1.PostgresCluster, state, why string,
) (ctrl.Result, error) {
	if err := r.takeAway(ctx, request, cluster); err != nil {
		r.event(request, "Warning", reasonFailed, "Revoke", "Could not take the access away: %s", err)
		return r.pending(ctx, request, request.Status.State, "the access has ended but could not be taken away yet: "+err.Error())
	}
	now := r.now()
	r.event(request, "Normal", reasonEnded, "Revoke", "Access %s: %s", state, why)
	logf.FromContext(ctx).Info("Ended emergency access", "request", request.Name, "state", state, "why", why)
	return ctrl.Result{}, r.setStatus(ctx, request, func(s *dbv1alpha1.PostgresEmergencyAccessStatus) {
		s.State, s.EndedAt = state, timePtr(now)
		s.Message = fmt.Sprintf("%s: %s.", state, why)
		s.SecretName = ""
	})
}

// takeAway drops the role and deletes the login. Nothing is left to take
// away from a request that was never granted, or from a cluster that is gone.
func (r *PostgresEmergencyAccessReconciler) takeAway(ctx context.Context, request *dbv1alpha1.PostgresEmergencyAccess, cluster *dbv1alpha1.PostgresCluster) error {
	if request.Status.Role != "" && cluster != nil {
		ops, ok, err := r.opsFor(ctx, cluster)
		if err != nil {
			return err
		}
		if ok {
			if err := ops.revoke(ctx, request.Status.Role); err != nil {
				return err
			}
		}
	}
	return client.IgnoreNotFound(r.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: emergencySecretName(request), Namespace: request.Namespace}}))
}

// opsFor prepares the SQL of a cluster. ok is false while it has no primary,
// when there is nothing to connect to.
func (r *PostgresEmergencyAccessReconciler) opsFor(ctx context.Context, cluster *dbv1alpha1.PostgresCluster) (emergencyOps, bool, error) {
	if r.SQL == nil {
		return emergencyOps{}, false, fmt.Errorf("the operator has no SQL channel to the cluster")
	}
	_, address, err := leaderAddressOf(ctx, r.Client, cluster)
	if err != nil {
		return emergencyOps{}, false, err
	}
	if address == "" {
		return emergencyOps{}, false, fmt.Errorf("the cluster has no primary")
	}
	target, err := superuserTargetOf(ctx, r.Client, cluster, address)
	if err != nil {
		return emergencyOps{}, false, err
	}
	return emergencyOps{dialer: r.SQL, target: target}, true, nil
}

// apply sets the role up in the database.
func (r *PostgresEmergencyAccessReconciler) apply(
	ctx context.Context, request *dbv1alpha1.PostgresEmergencyAccess, cluster *dbv1alpha1.PostgresCluster,
	role, password string, expires time.Time,
) error {
	ops, _, err := r.opsFor(ctx, cluster)
	if err != nil {
		return err
	}
	verifier, err := newScramVerifier(password)
	if err != nil {
		return err
	}
	return ops.grant(ctx, role, verifier, emergencyMarker(request), expires)
}

// ensureSecret writes the login if it is not there, owned by the request so
// that it goes when the request does, and returns its password.
func (r *PostgresEmergencyAccessReconciler) ensureSecret(
	ctx context.Context, request *dbv1alpha1.PostgresEmergencyAccess, cluster *dbv1alpha1.PostgresCluster,
) (string, error) {
	secret := &corev1.Secret{}
	name := emergencySecretName(request)
	err := r.Get(ctx, client.ObjectKey{Namespace: request.Namespace, Name: name}, secret)
	if err == nil && len(secret.Data[secretKeyPassword]) > 0 {
		return string(secret.Data[secretKeyPassword]), nil
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return "", err
	}
	password, err := generatePassword()
	if err != nil {
		return "", err
	}
	secret = &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: request.Namespace},
		Type:       corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			secretKeyUsername: []byte(emergencyRoleName(request.UID)),
			secretKeyPassword: []byte(password),
			secretKeyHost:     []byte(cluster.Name + "." + cluster.Namespace + ".svc"),
			secretKeyPort:     []byte(strconv.Itoa(int(postgresPort))),
			secretKeyDBName:   []byte(monitorDatabase),
			"warning":         []byte(dbv1alpha1.ParameterLoggingWarning),
		},
	}
	if err := controllerutil.SetControllerReference(request, secret, r.Scheme); err != nil {
		return "", err
	}
	if err := r.Create(ctx, secret); err != nil {
		return "", err
	}
	return password, nil
}

// finalize takes the access away before a request goes.
func (r *PostgresEmergencyAccessReconciler) finalize(ctx context.Context, request *dbv1alpha1.PostgresEmergencyAccess) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(request, finalizerEmergencyAccess) {
		return ctrl.Result{}, nil
	}
	cluster, err := r.clusterOf(ctx, request)
	if err != nil {
		return ctrl.Result{}, err
	}
	if request.Status.Role != "" && request.Status.EndedAt == nil {
		if err := r.takeAway(ctx, request, cluster); err != nil {
			r.event(request, "Warning", reasonFailed, "Revoke", "Could not take the access away before deleting the request: %s", err)
			return ctrl.Result{RequeueAfter: emergencyRetryDelay}, nil
		}
		r.event(request, "Normal", reasonEnded, "Revoke", "Access revoked: the request was deleted")
	}
	controllerutil.RemoveFinalizer(request, finalizerEmergencyAccess)
	return ctrl.Result{}, r.Update(ctx, request)
}

// pending records that the request is waiting, and why, and tries again soon.
func (r *PostgresEmergencyAccessReconciler) pending(
	ctx context.Context, request *dbv1alpha1.PostgresEmergencyAccess, state, message string,
) (ctrl.Result, error) {
	if len(message) > 1000 {
		message = message[:1000]
	}
	err := r.setStatus(ctx, request, func(s *dbv1alpha1.PostgresEmergencyAccessStatus) {
		s.State, s.Message = state, message
	})
	return ctrl.Result{RequeueAfter: emergencyRetryDelay}, err
}

// setStatus changes the request's status, on the latest version of the object.
func (r *PostgresEmergencyAccessReconciler) setStatus(
	ctx context.Context, request *dbv1alpha1.PostgresEmergencyAccess, change func(*dbv1alpha1.PostgresEmergencyAccessStatus),
) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &dbv1alpha1.PostgresEmergencyAccess{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(request), latest); err != nil {
			return err
		}
		before := latest.Status.DeepCopy()
		change(&latest.Status)
		if equalStatus(before, &latest.Status) {
			request.Status = latest.Status
			return nil
		}
		if err := r.Status().Update(ctx, latest); err != nil {
			return err
		}
		request.Status = latest.Status
		request.ResourceVersion = latest.ResourceVersion
		return nil
	})
}

func equalStatus(a, b *dbv1alpha1.PostgresEmergencyAccessStatus) bool {
	same := func(x, y *metav1.Time) bool {
		if x == nil || y == nil {
			return x == y
		}
		return x.Equal(y)
	}
	return a.State == b.State && a.Message == b.Message && a.RequestedBy == b.RequestedBy && a.Role == b.Role &&
		a.SecretName == b.SecretName && same(a.StartedAt, b.StartedAt) && same(a.ExpiresAt, b.ExpiresAt) && same(a.EndedAt, b.EndedAt)
}

func (r *PostgresEmergencyAccessReconciler) event(request *dbv1alpha1.PostgresEmergencyAccess, kind, reason, action, format string, args ...any) {
	if r.Recorder != nil {
		r.Recorder.Eventf(request, nil, kind, reason, action, format, args...)
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *PostgresEmergencyAccessReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&dbv1alpha1.PostgresEmergencyAccess{}).
		Owns(&corev1.Secret{}).
		// A grant ends when its cluster stops allowing it, or goes: that is a
		// change to the cluster, not to the request.
		Watches(&dbv1alpha1.PostgresCluster{}, handler.EnqueueRequestsFromMapFunc(r.requestsOfCluster),
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("postgresemergencyaccess").
		Complete(r)
}

// requestsOfCluster maps a cluster to the requests that name it.
func (r *PostgresEmergencyAccessReconciler) requestsOfCluster(ctx context.Context, obj client.Object) []reconcile.Request {
	requests := &dbv1alpha1.PostgresEmergencyAccessList{}
	if err := r.List(ctx, requests, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var out []reconcile.Request
	for i := range requests.Items {
		if requests.Items[i].Spec.ClusterRef.Name == obj.GetName() {
			out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&requests.Items[i])})
		}
	}
	return out
}
