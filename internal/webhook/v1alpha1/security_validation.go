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
	var was dbv1alpha1.SecuritySpec
	if old != nil && old.Spec.Security != nil {
		was = *old.Spec.Security
	}
	var now dbv1alpha1.SecuritySpec
	if cluster.Spec.Security != nil {
		now = *cluster.Spec.Security
	}

	var warnings admission.Warnings
	if was.TLSSecretName() != now.TLSSecretName() {
		warnings = append(warnings, certificateWarning(now.TLSSecretName()))
	}
	if was.PatroniAPIAuthentication() != now.PatroniAPIAuthentication() {
		warnings = append(warnings, authenticationWarning(now.PatroniAPIAuthentication(), now.PatroniAPITLS()))
	}
	if was.PatroniAPITLS() != now.PatroniAPITLS() {
		warnings = append(warnings, apiTLSWarning(now.PatroniAPITLS()))
	}
	if was.RestrictsIngress() != now.RestrictsIngress() {
		warnings = append(warnings, ingressWarning(now.RestrictsIngress()))
	}
	if was.RestrictsEgress() != now.RestrictsEgress() {
		warnings = append(warnings, egressWarning(now.RestrictsEgress()))
	}
	if was.RestrictsReplicationRole() != now.RestrictsReplicationRole() {
		warnings = append(warnings, replicationRoleWarning(now.RestrictsReplicationRole()))
	}
	return warnings
}

const replacesPods = "every database pod is replaced, one at a time, replicas first and the primary after a switchover. " +
	"Writes are refused for about two seconds."

func certificateWarning(secret string) string {
	if secret == "" {
		return "spec.security.tls: removing the certificate replaces every database pod, one at a time, replicas " +
			"first and the primary after a switchover. Writes are refused for about two seconds. The server goes back to presenting " +
			"a certificate it made for itself, and the operator stops checking it."
	}
	return "spec.security.tls: every database pod is replaced to present the certificate, one at a time, replicas " +
		"first and the primary after a switchover. Writes are refused for about two seconds. Until the primary has been replaced " +
		"the operator does not check its certificate. The cluster is held back, with the reason in its status, if the Secret " +
		"is missing, incomplete, or not valid for the cluster's Service and pod names."
}

const appliedAtOnce = "No pod is replaced: the policy takes effect at once, and only if the cluster's network plugin enforces NetworkPolicy."

func ingressWarning(on bool) string {
	if !on {
		return "spec.security.networkPolicy.ingress: any pod can connect to the database again, unless a compliance preset limits it. " + appliedAtOnce
	}
	return "spec.security.networkPolicy.ingress: only pods labelled db.youroperator.io/client=true in the cluster's namespace, " +
		"the operator, the cluster's own pods and jobs, and the peers you list can connect, so an application without the label " +
		"loses its connection. " + appliedAtOnce
}

func egressWarning(on bool) string {
	if !on {
		return "spec.security.networkPolicy.egress: the database pods can connect anywhere again. " + appliedAtOnce
	}
	return "spec.security.networkPolicy.egress: the database pods can then connect only to DNS, to each other, to the Kubernetes " +
		"API server, and to what you list. Anything else they need, such as a backup destination or a DNS that is not kube-dns " +
		"in kube-system, fails until you list it. " + appliedAtOnce
}

func replicationRoleWarning(on bool) string {
	if !on {
		return "spec.security.restrictReplicationRole: " + replacesPods + " The replication role can log in like any other user " +
			"again, the server's connection rules are the image's, and Patroni rewinds as the superuser. The pgoperator_rewind " +
			"login is disabled, not dropped, once no pod uses it."
	}
	return "spec.security.restrictReplicationRole: " + replacesPods + " The server then refuses the replication role " +
		"everything but replication, so anything of yours that logs in as it to run queries stops working. Patroni rewinds " +
		"as pgoperator_rewind, a login that can only read the files pg_rewind needs, with its password in the credentials " +
		"Secret as rewind-password. The operator writes the server's connection rules in place of the image's: the only ones " +
		"that go are for a group role the image never creates."
}

func authenticationWarning(on, overTLS bool) string {
	if !on {
		return "spec.security.patroniAPI.authentication: " + replacesPods +
			" Patroni's API goes back to changing the cluster's configuration for anyone who can reach port 8008."
	}
	message := "spec.security.patroniAPI.authentication: " + replacesPods +
		" Patroni's API then refuses to change anything, or hand leadership over, without the password in the credentials " +
		"Secret under patroni-api-password. Reading is still open."
	if !overTLS {
		message += " Without spec.security.tls.patroniAPI the password crosses the pod network unencrypted."
	}
	return message
}

func apiTLSWarning(on bool) string {
	if !on {
		return "spec.security.tls.patroniAPI: " + replacesPods + " Patroni's API goes back to plain HTTP."
	}
	return "spec.security.tls.patroniAPI: " + replacesPods + " Patroni's API then answers over TLS only: anything of yours " +
		"that calls port 8008 over plain HTTP stops working. The backup the image takes when a pod starts waits for Patroni " +
		"over plain HTTP, and does not run; the scheduled backups are not affected."
}
