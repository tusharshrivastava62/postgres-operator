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
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	dbv1alpha1 "github.com/tusharshrivastava62/postgres-operator/api/v1alpha1"
)

// emergencySweepInterval is how long a sweep that found nothing is trusted.
const emergencySweepInterval = 5 * time.Minute

// emergencyRolesSQL lists the roles made for emergency access, which are the
// ones that start with bg_ and carry the operator's comment.
const emergencyRolesSQL = `select coalesce(json_agg(rolname order by rolname), '[]'::json)::text
from pg_catalog.pg_roles
where rolname like 'bg\_%' and pg_catalog.shobj_description(oid, 'pg_authid') like 'Emergency access for %'`

// requestsFor lists the emergency access requests that name a cluster.
func (r *PostgresClusterReconciler) requestsFor(ctx context.Context, cluster *dbv1alpha1.PostgresCluster) ([]dbv1alpha1.PostgresEmergencyAccess, error) {
	all := &dbv1alpha1.PostgresEmergencyAccessList{}
	if err := r.List(ctx, all, client.InNamespace(cluster.Namespace)); err != nil {
		return nil, err
	}
	var mine []dbv1alpha1.PostgresEmergencyAccess
	for i := range all.Items {
		if all.Items[i].Spec.ClusterRef.Name == cluster.Name {
			mine = append(mine, all.Items[i])
		}
	}
	return mine, nil
}

// emergencyAccessWanted reports whether a cluster has anything to do about
// emergency access: it allows it, or it is a restore, and so may hold the
// roles of the cluster it came from.
func emergencyAccessWanted(cluster *dbv1alpha1.PostgresCluster) bool {
	return cluster.Spec.EmergencyAccess.Allowed() || cluster.Spec.Recovery() != nil
}

// liveEmergencyRoles are the roles that belong to requests still in progress.
func liveEmergencyRoles(requests []dbv1alpha1.PostgresEmergencyAccess) map[string]bool {
	live := map[string]bool{}
	for i := range requests {
		switch requests[i].Status.State {
		case dbv1alpha1.EmergencyExpired, dbv1alpha1.EmergencyRevoked, dbv1alpha1.EmergencyDenied:
		default:
			if requests[i].DeletionTimestamp.IsZero() {
				live[emergencyRoleName(requests[i].UID)] = true
			}
		}
	}
	return live
}

// reconcileEmergencyAccess reports whether emergency access is in use, and
// sweeps away roles it left behind: those of requests that are over, which a
// failure kept from being cleaned up, and those a restore brought in from the
// cluster it was made from, whose requests are not here.
func (r *PostgresClusterReconciler) reconcileEmergencyAccess(ctx context.Context, cluster *dbv1alpha1.PostgresCluster) error {
	key := cluster.Namespace + "/" + cluster.Name
	if !emergencyAccessWanted(cluster) {
		emergencyAccessActive.DeletePartialMatch(map[string]string{labelNamespace: cluster.Namespace, labelName: cluster.Name})
		if meta.RemoveStatusCondition(&cluster.Status.Conditions, dbv1alpha1.ConditionEmergencyAccessActive) {
			return r.Status().Update(ctx, cluster)
		}
		return nil
	}
	requests, err := r.requestsFor(ctx, cluster)
	if err != nil {
		return err
	}
	changed := r.setEmergencyCondition(cluster, requests)

	if r.SQL != nil {
		if at, ok := r.emergencyVerified.Load(key); !ok || time.Since(at.(time.Time)) >= emergencySweepInterval {
			swept, err := r.sweepEmergencyRoles(ctx, cluster, requests)
			if err != nil {
				logf.FromContext(ctx).Info("Could not sweep emergency access roles", "error", err.Error())
			} else if swept {
				r.emergencyVerified.Store(key, time.Now())
			}
		}
	}
	if changed {
		return r.Status().Update(ctx, cluster)
	}
	return nil
}

// setEmergencyCondition says whether any grant is active, and reports whether
// that changed what status holds. Only a cluster that allows emergency access
// has the condition.
func (r *PostgresClusterReconciler) setEmergencyCondition(cluster *dbv1alpha1.PostgresCluster, requests []dbv1alpha1.PostgresEmergencyAccess) bool {
	var active []string
	for i := range requests {
		if requests[i].Status.State == dbv1alpha1.EmergencyActive {
			active = append(active, requests[i].Name)
		}
	}
	slices.Sort(active)
	emergencyAccessActive.WithLabelValues(cluster.Namespace, cluster.Name).Set(float64(len(active)))
	if !cluster.Spec.EmergencyAccess.Allowed() {
		return meta.RemoveStatusCondition(&cluster.Status.Conditions, dbv1alpha1.ConditionEmergencyAccessActive)
	}
	condition := metav1.Condition{
		Type: dbv1alpha1.ConditionEmergencyAccessActive, Status: metav1.ConditionFalse,
		Reason: "NoActiveGrants", Message: "no emergency access is active",
	}
	if len(active) > 0 {
		condition.Status, condition.Reason = metav1.ConditionTrue, "AccessGranted"
		condition.Message = fmt.Sprintf("%d active: %s", len(active), strings.Join(active, ", "))
	}
	return meta.SetStatusCondition(&cluster.Status.Conditions, condition)
}

// sweepEmergencyRoles removes every emergency access role that no request in
// progress owns. It reports whether it could look.
func (r *PostgresClusterReconciler) sweepEmergencyRoles(
	ctx context.Context, cluster *dbv1alpha1.PostgresCluster, requests []dbv1alpha1.PostgresEmergencyAccess,
) (bool, error) {
	_, address, err := r.leaderAddress(ctx, cluster)
	if err != nil || address == "" {
		return false, err
	}
	target, err := r.superuserTarget(ctx, cluster, address)
	if err != nil {
		return false, err
	}
	ops := emergencyOps{dialer: r.SQL, target: target}
	session, err := ops.dial(ctx, monitorDatabase)
	if err != nil {
		return false, err
	}
	answer, err := session.QueryText(ctx, emergencyRolesSQL)
	session.Close(ctx)
	if err != nil {
		return false, err
	}
	var found []string
	if err := json.Unmarshal([]byte(answer), &found); err != nil {
		return false, err
	}
	live := liveEmergencyRoles(requests)
	for _, role := range found {
		if live[role] {
			continue
		}
		if err := ops.revoke(ctx, role); err != nil {
			return false, fmt.Errorf("removing role %s: %w", role, err)
		}
		logf.FromContext(ctx).Info("Removed an emergency access role that no request owns", "role", role)
	}
	return true, nil
}
