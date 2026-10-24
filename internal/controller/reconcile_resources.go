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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	dbv1alpha1 "github.com/tusharshrivastava62/postgres-operator/api/v1alpha1"
)

// patroniRules is everything Patroni needs from the Kubernetes API when the
// API is its DCS: it keeps the leader lock and dynamic configuration in
// Endpoints/ConfigMaps and each member's role and status on its own Pod.
//
// This is deliberately the smallest set that was seen to work, not the
// broader one other operators grant. The operator itself must hold these
// permissions to be allowed to create this Role (Kubernetes blocks
// privilege escalation through Roles), so they are also in its own RBAC.
var patroniRules = []rbacv1.PolicyRule{
	{APIGroups: []string{""}, Resources: []string{"configmaps", "endpoints"}, Verbs: []string{"create", "get", "list", "patch", "update", "watch"}},
	{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get", "list", "patch", "update", "watch"}},
}

// reconcilePatroniAccess ensures the ServiceAccount, Role and RoleBinding the
// database pods run as. They are on the critical path of the database, not
// plumbing: if the RoleBinding disappears the primary loses the ability to
// renew its leader lock and demotes itself within seconds, and failsafe mode
// does not help because the pod is denied rather than the API unreachable.
// That is why the controller owns and watches all three, so a deletion is
// repaired on the next reconcile, and why they are created before the
// StatefulSet.
func (r *PostgresClusterReconciler) reconcilePatroniAccess(ctx context.Context, cluster *dbv1alpha1.PostgresCluster) error {
	name := patroniAccountName(cluster)
	meta := metav1.ObjectMeta{Name: name, Namespace: cluster.Namespace}

	sa := &corev1.ServiceAccount{ObjectMeta: meta}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, sa, func() error {
		sa.Labels = labelsForCluster(cluster)
		return controllerutil.SetControllerReference(cluster, sa, r.Scheme)
	}); err != nil {
		return fmt.Errorf("service account: %w", err)
	}

	role := &rbacv1.Role{ObjectMeta: meta}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, role, func() error {
		role.Labels = labelsForCluster(cluster)
		role.Rules = patroniRules
		return controllerutil.SetControllerReference(cluster, role, r.Scheme)
	}); err != nil {
		return fmt.Errorf("role: %w", err)
	}

	binding := &rbacv1.RoleBinding{ObjectMeta: meta}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, binding, func() error {
		binding.Labels = labelsForCluster(cluster)
		binding.RoleRef = rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: name}
		binding.Subjects = []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: name, Namespace: cluster.Namespace}}
		return controllerutil.SetControllerReference(cluster, binding, r.Scheme)
	}); err != nil {
		return fmt.Errorf("role binding: %w", err)
	}
	return nil
}

// reconcileServices ensures the four Services of a cluster:
//
//   - <cluster>: read-write. It has no selector on purpose. Patroni writes the
//     Endpoints object of the same name and points it at the pod that holds
//     the leader lock, which is the only authority on who the primary is; a
//     label selector would follow a stale label to a hung primary.
//   - <cluster>-readonly: replicas only, selected by the role label.
//   - <cluster>-headless: the StatefulSet's governing Service (per-pod DNS).
//   - <cluster>-config: see configServiceName.
func (r *PostgresClusterReconciler) reconcileServices(ctx context.Context, cluster *dbv1alpha1.PostgresCluster) error {
	postgresServicePort := []corev1.ServicePort{
		{Name: postgresPortName, Port: postgresPort, TargetPort: intstr.FromInt32(postgresPort)},
	}
	selector := podSelectorLabels(cluster)

	services := []struct {
		name  string
		build func(*corev1.Service)
	}{
		{cluster.Name, func(svc *corev1.Service) {
			svc.Spec.Selector = nil
			svc.Spec.Ports = postgresServicePort
		}},
		{readOnlyServiceName(cluster), func(svc *corev1.Service) {
			svc.Spec.Selector = map[string]string{
				labelAppName:      selector[labelAppName],
				labelAppInstance:  selector[labelAppInstance],
				labelAppComponent: selector[labelAppComponent],
				labelRole:         roleReplica,
			}
			svc.Spec.Ports = postgresServicePort
		}},
		{headlessServiceName(cluster), func(svc *corev1.Service) {
			svc.Spec.ClusterIP = corev1.ClusterIPNone
			svc.Spec.Selector = selector
			// Not-yet-ready pods still need resolvable DNS so members can
			// find each other while they start and rejoin.
			svc.Spec.PublishNotReadyAddresses = true
			svc.Spec.Ports = postgresServicePort
		}},
		{configServiceName(cluster), func(svc *corev1.Service) {
			svc.Spec.ClusterIP = corev1.ClusterIPNone
			svc.Spec.Selector = nil
		}},
	}

	for _, s := range services {
		svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: s.name, Namespace: cluster.Namespace}}
		if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
			svc.Labels = labelsForCluster(cluster)
			s.build(svc)
			return controllerutil.SetControllerReference(cluster, svc, r.Scheme)
		}); err != nil {
			return fmt.Errorf("service %s: %w", s.name, err)
		}
	}
	return nil
}

// reconcilePodDisruptionBudget keeps node drains from evicting more than one
// member at a time, so a rolling node upgrade cannot take a primary and its
// only synchronous standby down together.
func (r *PostgresClusterReconciler) reconcilePodDisruptionBudget(ctx context.Context, cluster *dbv1alpha1.PostgresCluster) error {
	pdb := &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: cluster.Name, Namespace: cluster.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, pdb, func() error {
		maxUnavailable := intstr.FromInt32(1)
		pdb.Labels = labelsForCluster(cluster)
		pdb.Spec.MaxUnavailable = &maxUnavailable
		pdb.Spec.Selector = &metav1.LabelSelector{MatchLabels: podSelectorLabels(cluster)}
		return controllerutil.SetControllerReference(cluster, pdb, r.Scheme)
	})
	return err
}

// reconcileStatefulSet ensures the StatefulSet running Postgres exists and
// matches the spec.
//
// This intentionally does NOT use the CreateOrUpdate/mutate pattern used
// for the Services. spec.volumeClaimTemplates (like the selector, service
// name and pod management policy) is immutable once a StatefulSet is
// created, and the API server also defaults sub-fields on it (e.g.
// VolumeMode) that our builder doesn't set - so rebuilding and re-submitting
// that slice on every reconcile would trigger a spurious diff against the
// persisted object and the Update would be rejected. Instead: build the full
// object only for Create, and on Update touch just the fields Kubernetes
// allows to change after creation (replicas, pod template, update strategy, labels).
func (r *PostgresClusterReconciler) reconcileStatefulSet(ctx context.Context, cluster *dbv1alpha1.PostgresCluster, image string) error {
	// The audit shipper's script comes first: a pod that mounts it cannot start
	// without it.
	if err := r.reconcileAuditScripts(ctx, cluster); err != nil {
		return fmt.Errorf("reconciling the audit shipper script: %w", err)
	}
	creds := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: credentialsSecretName(cluster)}, creds); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	desired, err := desiredStatefulSetAt(cluster, image, credentialsVersion(creds))
	if err != nil {
		return err
	}
	if err := controllerutil.SetControllerReference(cluster, desired, r.Scheme); err != nil {
		return err
	}

	existing := &appsv1.StatefulSet{}
	err = r.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: cluster.Name}, existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}

	existing.Labels = desired.Labels
	existing.Spec.Replicas = desired.Spec.Replicas
	existing.Spec.Template = desired.Spec.Template
	existing.Spec.UpdateStrategy = desired.Spec.UpdateStrategy
	return r.Update(ctx, existing)
}

// hasMemoryLimit reports whether the spec carries the memory limit the
// webhook requires. The reconciler checks it again because the webhook is
// not the only way a spec can arrive: uninstalling the chart removes the
// webhook but leaves the CRD and its objects, and a Postgres image sizes
// shared_buffers from the container's memory limit (or from the whole node's
// memory when there is none).
func hasMemoryLimit(cluster *dbv1alpha1.PostgresCluster) bool {
	mem, ok := cluster.Spec.Resources.Limits[corev1.ResourceMemory]
	return ok && !mem.IsZero()
}

// cleanupPatroniState deletes the Endpoints and ConfigMaps Patroni created
// for a cluster. They carry the cluster's labels but no owner reference, so
// deleting the PostgresCluster leaves them behind, and a new cluster with the
// same name would find the old leader and initialisation records, believe it
// is already bootstrapped, and wait forever for a leader that does not exist.
func (r *PostgresClusterReconciler) cleanupPatroniState(ctx context.Context, namespace, name string) error {
	opts := []client.ListOption{client.InNamespace(namespace), client.MatchingLabels(selectorLabelsFor(name))}

	endpoints := &corev1.EndpointsList{} //nolint:staticcheck // Patroni creates core Endpoints, so they are what is left behind
	if err := r.List(ctx, endpoints, opts...); err != nil {
		return err
	}
	for i := range endpoints.Items {
		if err := r.Delete(ctx, &endpoints.Items[i]); client.IgnoreNotFound(err) != nil {
			return err
		}
	}

	configMaps := &corev1.ConfigMapList{}
	if err := r.List(ctx, configMaps, opts...); err != nil {
		return err
	}
	for i := range configMaps.Items {
		if err := r.Delete(ctx, &configMaps.Items[i]); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	return nil
}

// podsExist reports whether any database pod of the named cluster is still
// present, including ones that are terminating. Cleaning up Patroni's state
// while a member is still running would just see it written again.
func (r *PostgresClusterReconciler) podsExist(ctx context.Context, namespace, name string) (bool, error) {
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(namespace), client.MatchingLabels(selectorLabelsFor(name))); err != nil {
		return false, err
	}
	return len(pods.Items) > 0, nil
}
