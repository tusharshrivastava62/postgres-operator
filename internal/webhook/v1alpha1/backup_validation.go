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
	"time"

	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	dbv1alpha1 "github.com/tusharshrivastava62/postgres-operator/api/v1alpha1"
)

// validateBackup holds the backup rules the CRD schema cannot express, because
// they relate one field to another.
func validateBackup(cluster *dbv1alpha1.PostgresCluster, now time.Time) field.ErrorList {
	var errs field.ErrorList
	backup := cluster.Spec.Backup
	backupPath := field.NewPath("spec", "backup")

	if backup.Enabled && backup.Destination == nil {
		errs = append(errs, field.Required(backupPath.Child("destination"),
			"must be set when spec.backup.enabled is true: backups need somewhere to go"))
	}

	if backup.Verification.Enabled {
		verifyPath := backupPath.Child("verification")
		if !backup.Enabled {
			errs = append(errs, field.Invalid(verifyPath.Child("enabled"), true,
				"restore testing needs backups: set spec.backup.enabled to true"))
		}
		if backup.Verification.Schedule == "" {
			errs = append(errs, field.Required(verifyPath.Child("schedule"),
				"must be set when spec.backup.verification.enabled is true"))
		}
	}

	if recovery := recoveryOf(cluster); recovery != nil {
		recoveryPath := field.NewPath("spec", "bootstrap", "recovery")
		if recovery.TargetTime != nil && recovery.TargetTime.After(now) {
			errs = append(errs, field.Invalid(recoveryPath.Child("targetTime"), recovery.TargetTime.UTC().Format(time.RFC3339),
				"must not be in the future: no backup holds data from a time that has not happened yet"))
		}

		// A cluster that archives into the place it was restored from would
		// overwrite the history it depends on, and a second timeline there
		// makes every later restore ambiguous.
		if backup.Enabled && backup.Destination != nil && backup.Destination.SameStore(&recovery.Destination) {
			sourceNamespace := recovery.SourceNamespace
			if sourceNamespace == "" {
				sourceNamespace = cluster.Namespace
			}
			own := backup.Destination.PathFor(cluster.Namespace, cluster.Name)
			source := recovery.Destination.PathFor(sourceNamespace, recovery.SourceCluster)
			if own == source {
				errs = append(errs, field.Invalid(backupPath.Child("destination", "path"), own,
					"must differ from the path this cluster is restored from, or its backups would overwrite them"))
			}
		}
	}
	return errs
}

func recoveryOf(cluster *dbv1alpha1.PostgresCluster) *dbv1alpha1.RecoverySpec {
	if cluster.Spec.Bootstrap == nil {
		return nil
	}
	return cluster.Spec.Bootstrap.Recovery
}

// backupWarnings returns advice about backup settings that are allowed but
// likely to be a mistake. old is nil on create.
func backupWarnings(old, cluster *dbv1alpha1.PostgresCluster) admission.Warnings {
	var warnings admission.Warnings
	backup := cluster.Spec.Backup
	if !backup.Enabled {
		return warnings
	}

	if backup.Retention.Full == 1 {
		warnings = append(warnings,
			"spec.backup.retention.full is 1: with a single base backup there is nothing to fall back on if it is damaged")
	}
	if backup.Retention.Days > 0 && backup.Retention.Days < 2 {
		warnings = append(warnings,
			"spec.backup.retention.days is 1: a restore can only reach back one day")
	}

	if old != nil && old.Spec.Backup.Enabled {
		before, after := old.Spec.Backup, backup
		if destinationMoved(before.Destination, after.Destination, old, cluster) {
			warnings = append(warnings, fmt.Sprintf(
				"the backup destination changed: a new backup history starts at %s, and backups already in the old one "+
					"are neither used nor deleted", after.Destination.PathFor(cluster.Namespace, cluster.Name)))
		}
		if encryptionKey(before.Encryption) != encryptionKey(after.Encryption) {
			warnings = append(warnings,
				"the backup encryption key changed: backups made with the previous setting cannot be read with the new one")
		}
	}
	return warnings
}

func encryptionKey(e *dbv1alpha1.BackupEncryption) string {
	if e == nil {
		return ""
	}
	return e.KeySecret
}

func destinationMoved(before, after *dbv1alpha1.BackupDestination, old, cluster *dbv1alpha1.PostgresCluster) bool {
	if before == nil || after == nil {
		return false
	}
	return !before.SameStore(after) ||
		before.PathFor(old.Namespace, old.Name) != after.PathFor(cluster.Namespace, cluster.Name)
}
