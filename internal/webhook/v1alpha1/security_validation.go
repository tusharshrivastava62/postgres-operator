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
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	dbv1alpha1 "github.com/tusharshrivastava62/postgres-operator/api/v1alpha1"
)

// validateSecurity holds the rules for spec.security that the CRD schema cannot
// express. They live with the API types, so the controller checks the same ones
// when the webhook is not there. What is in the Secret is not checked here: it
// may be made after the cluster, and the controller holds the cluster back,
// with the reason in its status, until it is right.
func validateSecurity(cluster *dbv1alpha1.PostgresCluster) field.ErrorList {
	return cluster.Spec.ValidateSecurity()
}

// securityWarnings says what a change to spec.security will do that the person
// making it may not expect. Nothing here is refused.
func securityWarnings(old, cluster *dbv1alpha1.PostgresCluster) admission.Warnings {
	was, now := "", cluster.Spec.Security.TLSSecretName()
	if old != nil {
		was = old.Spec.Security.TLSSecretName()
	}
	if was == now {
		return nil
	}
	if now == "" {
		return admission.Warnings{"spec.security.tls: removing the certificate replaces every database pod, one at a time, replicas " +
			"first and the primary after a switchover. Writes are refused for about two seconds. The server goes back to presenting " +
			"a certificate it made for itself, and the operator stops checking it."}
	}
	return admission.Warnings{"spec.security.tls: every database pod is replaced to present the certificate, one at a time, replicas " +
		"first and the primary after a switchover. Writes are refused for about two seconds. Until the primary has been replaced " +
		"the operator does not check its certificate. The cluster is held back, with the reason in its status, if the Secret " +
		"is missing, incomplete, or not valid for the cluster's Service and pod names."}
}
