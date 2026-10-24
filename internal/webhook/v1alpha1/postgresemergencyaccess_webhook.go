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
	"reflect"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	dbv1alpha1 "github.com/tusharshrivastava62/postgres-operator/api/v1alpha1"
)

var postgresemergencyaccesslog = logf.Log.WithName("postgresemergencyaccess-resource")

// SetupPostgresEmergencyAccessWebhookWithManager registers the webhooks for PostgresEmergencyAccess in the manager.
func SetupPostgresEmergencyAccessWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &dbv1alpha1.PostgresEmergencyAccess{}).
		WithValidator(&PostgresEmergencyAccessCustomValidator{Reader: mgr.GetClient()}).
		WithDefaulter(&PostgresEmergencyAccessCustomDefaulter{}).
		Complete()
}

// +kubebuilder:webhook:path=/mutate-db-youroperator-io-v1alpha1-postgresemergencyaccess,mutating=true,failurePolicy=fail,sideEffects=None,groups=db.youroperator.io,resources=postgresemergencyaccesses,verbs=create,versions=v1alpha1,name=mpostgresemergencyaccess-v1alpha1.kb.io,admissionReviewVersions=v1

// PostgresEmergencyAccessCustomDefaulter records who made a request.
//
// It is the only thing that can: a validating webhook cannot change an object,
// and nothing in the object says who created it. The name is taken from the
// authenticated user of the API call and written over whatever the request
// already had there, so nobody can name somebody else. It runs on create only,
// because an update by somebody else must not take over the request, and the
// validating webhook below refuses a change to it.
//
// NOTE: The +kubebuilder:object:generate=false marker prevents controller-gen from generating DeepCopy methods,
// as it is used only for temporary operations and does not need to be deeply copied.
type PostgresEmergencyAccessCustomDefaulter struct{}

// Default implements webhook.CustomDefaulter.
func (d *PostgresEmergencyAccessCustomDefaulter) Default(ctx context.Context, obj *dbv1alpha1.PostgresEmergencyAccess) error {
	request, err := admission.RequestFromContext(ctx)
	if err != nil {
		// Without the request there is no one to name, and a request that does
		// not name anyone is not granted.
		return fmt.Errorf("could not tell who is asking: %w", err)
	}
	postgresemergencyaccesslog.Info("Recording the requester", "name", obj.GetName(), "user", request.UserInfo.Username)
	if obj.Annotations == nil {
		obj.Annotations = map[string]string{}
	}
	obj.Annotations[dbv1alpha1.AnnotationRequestedBy] = request.UserInfo.Username
	return nil
}

// +kubebuilder:webhook:path=/validate-db-youroperator-io-v1alpha1-postgresemergencyaccess,mutating=false,failurePolicy=fail,sideEffects=None,groups=db.youroperator.io,resources=postgresemergencyaccesses,verbs=create;update,versions=v1alpha1,name=vpostgresemergencyaccess-v1alpha1.kb.io,admissionReviewVersions=v1

// PostgresEmergencyAccessCustomValidator refuses a request the cluster would
// not grant, so the person finds out at once, and one that changes after it is
// made. The operator checks again before it grants anything.
//
// NOTE: The +kubebuilder:object:generate=false marker prevents controller-gen from generating DeepCopy methods,
// as this struct is used only for temporary operations and does not need to be deeply copied.
type PostgresEmergencyAccessCustomValidator struct {
	// Reader finds the cluster a request names.
	Reader client.Reader
}

// ValidateCreate implements webhook.CustomValidator.
func (v *PostgresEmergencyAccessCustomValidator) ValidateCreate(ctx context.Context, obj *dbv1alpha1.PostgresEmergencyAccess) (admission.Warnings, error) {
	postgresemergencyaccesslog.Info("Validation for PostgresEmergencyAccess upon creation", "name", obj.GetName())

	var errs field.ErrorList
	if obj.Annotations[dbv1alpha1.AnnotationRequestedBy] == "" {
		errs = append(errs, field.Forbidden(field.NewPath("metadata", "annotations").Key(dbv1alpha1.AnnotationRequestedBy),
			"the requester could not be identified, and a request that names nobody is not granted"))
	}

	limit := dbv1alpha1.MaxEmergencyDuration
	cluster := &dbv1alpha1.PostgresCluster{}
	err := v.Reader.Get(ctx, client.ObjectKey{Namespace: obj.Namespace, Name: obj.Spec.ClusterRef.Name}, cluster)
	clusterPath := field.NewPath("spec", "clusterRef", "name")
	switch {
	case apierrors.IsNotFound(err):
		errs = append(errs, field.NotFound(clusterPath, obj.Spec.ClusterRef.Name))
	case err != nil:
		return nil, err
	case !cluster.Spec.EmergencyAccess.Allowed():
		errs = append(errs, field.Forbidden(clusterPath, fmt.Sprintf(
			"cluster %s does not allow emergency access: set spec.emergencyAccess.enabled on it first", cluster.Name)))
	default:
		limit = cluster.Spec.EmergencyAccess.MaxAllowed()
	}
	errs = append(errs, obj.Spec.Validate(limit)...)
	if len(errs) > 0 {
		gk := dbv1alpha1.GroupVersion.WithKind("PostgresEmergencyAccess").GroupKind()
		return nil, apierrors.NewInvalid(gk, obj.Name, errs)
	}
	return admission.Warnings{
		"WARNING: " + dbv1alpha1.ParameterLoggingWarning,
		fmt.Sprintf("Access to %s will last %s from the moment it is granted, and ends by itself. "+
			"Your name (%s) and your reason are kept on this request and in the operator's log.",
			obj.Spec.ClusterRef.Name, obj.Spec.EffectiveDuration(), obj.Annotations[dbv1alpha1.AnnotationRequestedBy]),
	}, nil
}

// ValidateUpdate implements webhook.CustomValidator.
func (v *PostgresEmergencyAccessCustomValidator) ValidateUpdate(_ context.Context, oldObj, newObj *dbv1alpha1.PostgresEmergencyAccess) (admission.Warnings, error) {
	var errs field.ErrorList
	if !reflect.DeepEqual(oldObj.Spec, newObj.Spec) {
		errs = append(errs, field.Forbidden(field.NewPath("spec"), "a request cannot be changed: make a new one"))
	}
	if oldObj.Annotations[dbv1alpha1.AnnotationRequestedBy] != newObj.Annotations[dbv1alpha1.AnnotationRequestedBy] {
		errs = append(errs, field.Forbidden(field.NewPath("metadata", "annotations").Key(dbv1alpha1.AnnotationRequestedBy),
			"who made a request cannot be changed"))
	}
	if len(errs) > 0 {
		gk := dbv1alpha1.GroupVersion.WithKind("PostgresEmergencyAccess").GroupKind()
		return nil, apierrors.NewInvalid(gk, newObj.Name, errs)
	}
	return nil, nil
}

// ValidateDelete implements webhook.CustomValidator. Nothing is refused: a
// request can always be deleted, and that takes its access away.
func (v *PostgresEmergencyAccessCustomValidator) ValidateDelete(_ context.Context, _ *dbv1alpha1.PostgresEmergencyAccess) (admission.Warnings, error) {
	return nil, nil
}
