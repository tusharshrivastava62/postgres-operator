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
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

const (
	// AnnotationRequestedBy records who created an emergency access request.
	// The webhook sets it from the authenticated user of the API call, on
	// create, and refuses to let it change. Nothing a requester writes in it
	// is kept.
	AnnotationRequestedBy = "db.youroperator.io/requested-by"

	// MinEmergencyDuration and MaxEmergencyDuration bound how long one grant of
	// emergency access can last, whatever the cluster allows.
	MinEmergencyDuration = time.Minute
	MaxEmergencyDuration = 8 * time.Hour

	// DefaultEmergencyDuration is how long a grant lasts when the request does
	// not say.
	DefaultEmergencyDuration = time.Hour

	// MinReasonLength is the shortest reason a request can give. A reason is
	// what a reader of the audit trail has to go on.
	MinReasonLength = 10

	// ParameterLoggingWarning is shown to anyone asking for emergency access,
	// and written in the documentation. It is the one thing about emergency
	// access that surprises people who have read the rest.
	ParameterLoggingWarning = "While you are escalated, every statement you run is written to the server log, and so are " +
		"the values you bind to its parameters, in clear text, whatever the audit settings say. " +
		"Do not run a statement whose values must not be logged."
)

// ConditionEmergencyAccessActive is true while at least one grant of emergency
// access to the cluster is active. A cluster that does not allow emergency
// access does not have it.
const ConditionEmergencyAccessActive = "EmergencyAccessActive"

// EmergencyAccessSpec is a cluster's switch for emergency access.
type EmergencyAccessSpec struct {
	// enabled lets people with permission to create PostgresEmergencyAccess
	// resources ask for temporary superuser access to this cluster's
	// databases. It is off unless set. Turning it off ends every grant that is
	// active.
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// maxDuration is the longest one grant can last. It defaults to, and
	// cannot exceed, eight hours.
	// +optional
	MaxDuration *metav1.Duration `json:"maxDuration,omitempty"`
}

// MaxAllowed is the longest grant the cluster allows.
func (e *EmergencyAccessSpec) MaxAllowed() time.Duration {
	if e == nil || e.MaxDuration == nil || e.MaxDuration.Duration <= 0 {
		return MaxEmergencyDuration
	}
	return e.MaxDuration.Duration
}

// Allowed reports whether the cluster accepts requests.
func (e *EmergencyAccessSpec) Allowed() bool { return e != nil && e.Enabled }

// Validate checks the cluster's settings for emergency access.
func (e *EmergencyAccessSpec) Validate(path *field.Path) field.ErrorList {
	if e == nil || e.MaxDuration == nil {
		return nil
	}
	d := e.MaxDuration.Duration
	if d < MinEmergencyDuration || d > MaxEmergencyDuration {
		return field.ErrorList{field.Invalid(path.Child("maxDuration"), d.String(),
			fmt.Sprintf("must be between %s and %s", MinEmergencyDuration, MaxEmergencyDuration))}
	}
	return nil
}

// ClusterReference names a PostgresCluster in the request's own namespace.
type ClusterReference struct {
	// name is the PostgresCluster to get access to.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Required
	Name string `json:"name"`
}

// PostgresEmergencyAccessSpec is a request for temporary access. It cannot be
// changed: to ask again, or for longer, make a new request. The webhook
// refuses a change; a rule in the schema would compare the text of the
// duration, and "30m" is not "30m0s", which is how every client writes it back.
type PostgresEmergencyAccessSpec struct {
	// clusterRef is the cluster to get access to. It must be in this
	// namespace, and have spec.emergencyAccess.enabled set.
	// +kubebuilder:validation:Required
	ClusterRef ClusterReference `json:"clusterRef"`

	// duration is how long the access lasts, from the moment it is granted. It
	// defaults to one hour, and cannot be longer than the cluster's
	// spec.emergencyAccess.maxDuration, which is at most eight hours.
	// +kubebuilder:default="1h"
	// +optional
	Duration metav1.Duration `json:"duration,omitzero"`

	// reason says why access is needed. It is kept with the request, and is
	// what someone reading the audit trail afterwards has to go on.
	//
	// WARNING: while you are escalated, every statement you run is written to
	// the server log, and so are the values you bind to its parameters, in
	// clear text, whatever the cluster's audit settings say. Do not run a
	// statement whose values must not be logged.
	// +kubebuilder:validation:MinLength=10
	// +kubebuilder:validation:MaxLength=500
	// +kubebuilder:validation:Required
	Reason string `json:"reason"`
}

// Validate checks a request against what the cluster allows.
func (s *PostgresEmergencyAccessSpec) Validate(limit time.Duration) field.ErrorList {
	var errs field.ErrorList
	path := field.NewPath("spec")
	if d := s.Duration.Duration; d != 0 && (d < MinEmergencyDuration || d > limit) {
		errs = append(errs, field.Invalid(path.Child("duration"), d.String(),
			fmt.Sprintf("must be between %s and %s, the most the cluster allows", MinEmergencyDuration, limit)))
	}
	if len(strings.TrimSpace(s.Reason)) < MinReasonLength {
		errs = append(errs, field.Invalid(path.Child("reason"), s.Reason,
			fmt.Sprintf("must say why, in at least %d characters that are not spaces", MinReasonLength)))
	}
	return errs
}

// EffectiveDuration is how long the grant lasts.
func (s *PostgresEmergencyAccessSpec) EffectiveDuration() time.Duration {
	if s.Duration.Duration == 0 {
		return DefaultEmergencyDuration
	}
	return s.Duration.Duration
}

// States of a request.
const (
	// EmergencyPending means the request has been seen and the access is not
	// yet granted.
	EmergencyPending = "Pending"
	// EmergencyActive means the person can log in now.
	EmergencyActive = "Active"
	// EmergencyExpired means the time ran out, and the access has been taken away.
	EmergencyExpired = "Expired"
	// EmergencyRevoked means the access was taken away before the time ran
	// out: the request was deleted, or the cluster stopped allowing it.
	EmergencyRevoked = "Revoked"
	// EmergencyDenied means the request was refused, and nothing was granted.
	EmergencyDenied = "Denied"
	// EmergencyFailed means the operator could not grant the access.
	EmergencyFailed = "Failed"
)

// PostgresEmergencyAccessStatus is the state of a request.
type PostgresEmergencyAccessStatus struct {
	// state is Pending, Active, Expired, Revoked, Denied or Failed.
	// +kubebuilder:validation:Enum=Pending;Active;Expired;Revoked;Denied;Failed
	// +optional
	State string `json:"state,omitempty"`

	// message says what the state means, and what to do next.
	// +kubebuilder:validation:MaxLength=1024
	// +optional
	Message string `json:"message,omitempty"`

	// requestedBy is who created the request, as the API server knew them.
	// +optional
	RequestedBy string `json:"requestedBy,omitempty"`

	// role is the database role to log in as.
	// +optional
	Role string `json:"role,omitempty"`

	// secretName is the Secret holding the role's login details. It is deleted
	// when the access ends.
	// +optional
	SecretName string `json:"secretName,omitempty"`

	// startedAt is when the access was granted.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// expiresAt is when the access ends.
	// +optional
	ExpiresAt *metav1.Time `json:"expiresAt,omitempty"`

	// endedAt is when the access was taken away.
	// +optional
	EndedAt *metav1.Time `json:"endedAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=".spec.clusterRef.name"
// +kubebuilder:printcolumn:name="State",type=string,JSONPath=".status.state"
// +kubebuilder:printcolumn:name="Requested by",type=string,JSONPath=".status.requestedBy"
// +kubebuilder:printcolumn:name="Expires",type=date,JSONPath=".status.expiresAt"

// PostgresEmergencyAccess is a request for temporary superuser access to a
// cluster's databases, for an emergency. It is granted for as long as the
// request says, is audited, and ends by itself.
//
// WARNING: while escalated, every statement is written to the server log, and
// so are the values bound to its parameters, in clear text.
type PostgresEmergencyAccess struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec is the request.
	// +required
	Spec PostgresEmergencyAccessSpec `json:"spec"`

	// status is the state of the request.
	// +optional
	Status PostgresEmergencyAccessStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// PostgresEmergencyAccessList contains a list of PostgresEmergencyAccess
type PostgresEmergencyAccessList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []PostgresEmergencyAccess `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &PostgresEmergencyAccess{}, &PostgresEmergencyAccessList{})
		return nil
	})
}
