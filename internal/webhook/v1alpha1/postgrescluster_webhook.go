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
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	dbv1alpha1 "github.com/tusharshrivastava62/postgres-operator/api/v1alpha1"
)

// nolint:unused
// log is for logging in this package.
var postgresclusterlog = logf.Log.WithName("postgrescluster-resource")

// SetupPostgresClusterWebhookWithManager registers the webhook for PostgresCluster in the manager.
func SetupPostgresClusterWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &dbv1alpha1.PostgresCluster{}).
		WithValidator(&PostgresClusterCustomValidator{}).
		Complete()
}

// This webhook holds validation the CRD's OpenAPI schema can't express:
// rules that span multiple fields (backup.schedule required only when
// backup.enabled is true; a memory limit that must exist and be at least the
// request), or that compare old vs. new state on update (storage can't
// shrink). Without it these were either unchecked or only caught as a
// reconcile-time error, well after `kubectl apply` returned success. It also
// emits non-blocking admission warnings for advice that shouldn't stop an
// apply. The backup rules (a destination when backups are on, restore testing
// needing backups, a restore target that is not in the future, and a cluster
// not archiving into the backups it was restored from) are in
// backup_validation.go.
// +kubebuilder:webhook:path=/validate-db-youroperator-io-v1alpha1-postgrescluster,mutating=false,failurePolicy=fail,sideEffects=None,groups=db.youroperator.io,resources=postgresclusters,verbs=create;update,versions=v1alpha1,name=vpostgrescluster-v1alpha1.kb.io,admissionReviewVersions=v1

// PostgresClusterCustomValidator struct is responsible for validating the PostgresCluster resource
// when it is created, updated, or deleted.
//
// NOTE: The +kubebuilder:object:generate=false marker prevents controller-gen from generating DeepCopy methods,
// as this struct is used only for temporary operations and does not need to be deeply copied.
type PostgresClusterCustomValidator struct {
	// TODO(user): Add more fields as needed for validation
}

// ValidateCreate implements webhook.CustomValidator so a webhook will be registered for the type PostgresCluster.
func (v *PostgresClusterCustomValidator) ValidateCreate(_ context.Context, obj *dbv1alpha1.PostgresCluster) (admission.Warnings, error) {
	postgresclusterlog.Info("Validation for PostgresCluster upon creation", "name", obj.GetName())

	var errs field.ErrorList
	if err := validateBackupSchedule(obj); err != nil {
		errs = append(errs, err)
	}
	errs = append(errs, validateResources(obj)...)
	errs = append(errs, validateBackup(obj, time.Now())...)
	return append(warningsFor(obj), backupWarnings(nil, obj)...), invalidError(obj, errs)
}

// ValidateUpdate implements webhook.CustomValidator so a webhook will be registered for the type PostgresCluster.
func (v *PostgresClusterCustomValidator) ValidateUpdate(_ context.Context, oldObj, newObj *dbv1alpha1.PostgresCluster) (admission.Warnings, error) {
	postgresclusterlog.Info("Validation for PostgresCluster upon update", "name", newObj.GetName())

	var errs field.ErrorList
	if err := validateBackupSchedule(newObj); err != nil {
		errs = append(errs, err)
	}
	if err := validateStorageNotShrinking(oldObj, newObj); err != nil {
		errs = append(errs, err)
	}
	errs = append(errs, validateResources(newObj)...)
	errs = append(errs, validateBackup(newObj, time.Now())...)
	return append(warningsFor(newObj), backupWarnings(oldObj, newObj)...), invalidError(newObj, errs)
}

// ValidateDelete implements webhook.CustomValidator so a webhook will be registered for the type PostgresCluster.
func (v *PostgresClusterCustomValidator) ValidateDelete(_ context.Context, obj *dbv1alpha1.PostgresCluster) (admission.Warnings, error) {
	postgresclusterlog.Info("Validation for PostgresCluster upon deletion", "name", obj.GetName())

	// Nothing to validate on delete, so this webhook is only registered for
	// create/update - see the verbs on the marker above.
	return nil, nil
}

// validateBackupSchedule enforces the one rule the CRD schema can't: a
// schedule is meaningless without backups enabled and vice versa, but
// enabled/schedule live in different fields, and OpenAPI validation only
// ever sees one field at a time.
func validateBackupSchedule(cluster *dbv1alpha1.PostgresCluster) *field.Error {
	if cluster.Spec.Backup.Enabled && cluster.Spec.Backup.Schedule == "" {
		return field.Required(
			field.NewPath("spec", "backup", "schedule"),
			"must be set when spec.backup.enabled is true",
		)
	}
	return nil
}

// validateStorageNotShrinking rejects reducing spec.storage.size on update.
// The StatefulSet's PersistentVolumeClaims can't be shrunk once created -
// Kubernetes itself rejects that at the PVC level - so failing fast here
// gives a clear error at `kubectl apply` time instead of a StatefulSet
// stuck failing reconciliation later.
func validateStorageNotShrinking(oldCluster, newCluster *dbv1alpha1.PostgresCluster) *field.Error {
	if newCluster.Spec.Storage.Size.Cmp(oldCluster.Spec.Storage.Size) < 0 {
		return field.Invalid(
			field.NewPath("spec", "storage", "size"),
			newCluster.Spec.Storage.Size.String(),
			fmt.Sprintf("cannot shrink storage from %s; PersistentVolumeClaims cannot be reduced in size", oldCluster.Spec.Storage.Size.String()),
		)
	}
	return nil
}

// minRecommendedMemory is the smallest memory limit that doesn't draw an
// admission warning. It isn't a hard floor: it's the point below which
// Postgres plus Patroni gets uncomfortably tight (the failover testing ran
// at 1Gi).
var minRecommendedMemory = resource.MustParse("512Mi")

// validateResources requires a memory limit and checks requests against
// limits.
//
// The limit is mandatory because the Postgres image sizes shared_buffers from
// the container's memory limit, and with no limit from the node's total
// memory: on an 8Gi node that was observed to be 1567MB for a single
// database, so several clusters on one node would be OOM-killed. A missing
// limit is therefore never a harmless omission, which is why it is rejected
// rather than defaulted or warned about.
//
// requests > limits is rejected here because Kubernetes only notices when it
// builds the StatefulSet's pods, i.e. long after `kubectl apply` returned
// success and as a reconcile error loop.
func validateResources(cluster *dbv1alpha1.PostgresCluster) field.ErrorList {
	var errs field.ErrorList
	res := cluster.Spec.Resources
	limitsPath := field.NewPath("spec", "resources", "limits")
	requestsPath := field.NewPath("spec", "resources", "requests")

	memLimit, hasMemLimit := res.Limits[corev1.ResourceMemory]
	if !hasMemLimit || memLimit.IsZero() {
		errs = append(errs, field.Required(
			limitsPath.Key(string(corev1.ResourceMemory)),
			"must be set: PostgreSQL sizes its memory from the container's memory limit, and with no limit "+
				"from the whole node's memory, so the pod can be OOM-killed",
		))
	}

	for _, name := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
		request, hasRequest := res.Requests[name]
		limit, hasLimit := res.Limits[name]
		if hasRequest && hasLimit && !limit.IsZero() && request.Cmp(limit) > 0 {
			errs = append(errs, field.Invalid(
				requestsPath.Key(string(name)),
				request.String(),
				fmt.Sprintf("must be less than or equal to the %s limit (%s)", name, limit.String()),
			))
		}
	}
	return errs
}

// warningsFor returns non-blocking advice. These are surfaced by kubectl as
// "Warning: ..." lines on an otherwise successful apply.
func warningsFor(cluster *dbv1alpha1.PostgresCluster) admission.Warnings {
	var warnings admission.Warnings

	if memLimit, ok := cluster.Spec.Resources.Limits[corev1.ResourceMemory]; ok && !memLimit.IsZero() &&
		memLimit.Cmp(minRecommendedMemory) < 0 {
		warnings = append(warnings, fmt.Sprintf(
			"spec.resources.limits.memory %s is below the recommended minimum of %s; PostgreSQL and Patroni may be unstable or OOM-killed",
			memLimit.String(), minRecommendedMemory.String()))
	}

	// instances defaults to 1 via the CRD, so unset counts as a single instance.
	if cluster.Spec.HA.SynchronousMode && cluster.Spec.Instances <= 1 {
		warnings = append(warnings,
			"spec.ha.synchronousMode has no effect with a single instance: there is no standby to synchronize with")
	}
	return warnings
}

func invalidError(cluster *dbv1alpha1.PostgresCluster, errs field.ErrorList) error {
	if len(errs) == 0 {
		return nil
	}
	gk := dbv1alpha1.GroupVersion.WithKind("PostgresCluster").GroupKind()
	return apierrors.NewInvalid(gk, cluster.Name, errs)
}
