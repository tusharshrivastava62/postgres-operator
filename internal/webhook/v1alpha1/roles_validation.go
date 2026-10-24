/*
Copyright 2026.

Licensed under the Business Source License 1.1 (the "License"). You may not
use this file except in compliance with the License, a copy of which is in the
LICENSE file at the root of this repository. Production use requires a
commercial license from the Licensor. On the Change Date (2030-10-17) the
work becomes available under the Change License (Apache-2.0).
*/

package v1alpha1

import (
	"fmt"

	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	dbv1alpha1 "github.com/tusharshrivastava62/postgres-operator/api/v1alpha1"
)

// validateRoles holds the rules for spec.roles that the CRD schema cannot
// express. They live with the API types, so the controller checks the same
// ones when the webhook is not there.
func validateRoles(cluster *dbv1alpha1.PostgresCluster) field.ErrorList {
	return cluster.Spec.ValidateRoles(cluster.Name)
}

// roleWarnings says what a change to spec.roles will do that the person
// making it may not expect. Nothing here is refused.
func roleWarnings(oldCluster, newCluster *dbv1alpha1.PostgresCluster) admission.Warnings {
	warnings := rotationWarnings(oldCluster, newCluster)
	if oldCluster == nil {
		return warnings
	}
	now := map[string]dbv1alpha1.RoleSpec{}
	for _, role := range newCluster.Spec.Roles {
		now[role.Name] = role
	}
	for _, before := range oldCluster.Spec.Roles {
		after, kept := now[before.Name]
		switch {
		case !kept:
			warnings = append(warnings, fmt.Sprintf(
				"spec.roles: role %s was removed. Its login will be disabled and its sessions ended. "+
					"The role, and any database it owns, are kept: drop them yourself when you are sure.", before.Name))
		case before.Database != "" && after.Database != before.Database:
			warnings = append(warnings, fmt.Sprintf(
				"spec.roles: role %s no longer names database %s, which is kept as it is. "+
					"A database is never dropped by the operator.", before.Name, before.Database))
		}
	}
	return warnings
}

// rotationWarnings says what a schedule for the superuser's password costs. It
// is the only one that does anything beyond the database: each time it runs,
// every pod is replaced.
func rotationWarnings(oldCluster, newCluster *dbv1alpha1.PostgresCluster) admission.Warnings {
	now := newCluster.Spec.CredentialRotation
	if now == nil || now.SuperuserEvery == nil {
		return nil
	}
	if oldCluster != nil {
		if before := oldCluster.Spec.CredentialRotation; before != nil && before.SuperuserEvery != nil &&
			before.SuperuserEvery.Duration == now.SuperuserEvery.Duration {
			return nil
		}
	}
	return admission.Warnings{fmt.Sprintf(
		"spec.credentialRotation.superuserEvery: every %s the superuser and replication passwords are replaced, and every database pod "+
			"with them, one at a time, replicas first and the primary after a switchover. Writes are refused for about two seconds each time.",
		now.SuperuserEvery.Duration)}
}
