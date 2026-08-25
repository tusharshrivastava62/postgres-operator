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

package v1alpha1

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

// This webhook exists for exactly the validation the CRD's OpenAPI schema
// can't express: rules that span multiple fields (backup.schedule required
// only when backup.enabled is true) or compare old vs. new state on update
// (storage can't shrink). Both were previously either unchecked or only
// caught as a reconcile-time error, well after `kubectl apply` returned
// success.
// +kubebuilder:webhook:path=/validate-db-db-youroperator-io-v1alpha1-postgrescluster,mutating=false,failurePolicy=fail,sideEffects=None,groups=db.db.youroperator.io,resources=postgresclusters,verbs=create;update,versions=v1alpha1,name=vpostgrescluster-v1alpha1.kb.io,admissionReviewVersions=v1

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
	return nil, invalidError(obj, errs)
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
	return nil, invalidError(newObj, errs)
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

func invalidError(cluster *dbv1alpha1.PostgresCluster, errs field.ErrorList) error {
	if len(errs) == 0 {
		return nil
	}
	gk := dbv1alpha1.GroupVersion.WithKind("PostgresCluster").GroupKind()
	return apierrors.NewInvalid(gk, cluster.Name, errs)
}
