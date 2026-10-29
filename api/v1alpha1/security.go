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
	"strings"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

// ConditionTLSReady is true when the database serves the certificate in
// spec.security.tls.secretName and the operator has verified it.
const ConditionTLSReady = "TLSReady"

// The reasons of the TLSReady condition.
const (
	// TLSVerified: the operator connected to the primary and checked its
	// certificate against the CA in the Secret.
	TLSVerified = "Verified"
	// TLSWaitingForPods: the pods still present the certificate the image made,
	// because the one in the Secret is not on the primary yet.
	TLSWaitingForPods = "WaitingForPods"
	// TLSExpired: the certificate the server presents has expired.
	TLSExpired = "Expired"
	// TLSVerificationFailed: the server's certificate was not accepted.
	TLSVerificationFailed = "VerificationFailed"
	// TLSNoPrimary: there is no primary to check.
	TLSNoPrimary = "NoPrimary"
)

// SecuritySpec holds the settings that harden a cluster beyond its defaults.
// Every one of them is off unless set, and setting or unsetting one replaces
// the database pods, one at a time.
type SecuritySpec struct {
	// tls makes the database present a certificate you supply, so that a client
	// can check who it is talking to. Without it the server presents one the
	// image made, which encrypts the connection and proves nothing.
	// +optional
	TLS *TLSSpec `json:"tls,omitempty"`

	// patroniAPI protects the Patroni REST API, which can change the cluster's
	// configuration and hand its leadership over, and which answers to anyone who
	// can reach port 8008 unless this is set.
	// +optional
	PatroniAPI *PatroniAPISpec `json:"patroniAPI,omitempty"`

	// restrictReplicationRole makes the replication role good for replicating and
	// nothing else, and gives pg_rewind a login of its own. By default the role
	// the replicas stream with can also log in like any other user and run
	// queries, and pg_rewind, which Patroni runs to bring a former primary back
	// as a replica, does so as the superuser. With this set the server refuses the
	// replication role everything but replication, and Patroni rewinds as
	// pgoperator_rewind, a login that can do nothing but read the files pg_rewind
	// needs. The password of that login is in <name>-credentials as
	// rewind-password and is replaced with the superuser's. The server's rules for
	// who may connect are written out by the operator, in place of the image's;
	// the only ones that go are the image's rules for a group role it does not
	// create. Setting it replaces the pods one at a time.
	// +optional
	RestrictReplicationRole bool `json:"restrictReplicationRole,omitempty"`

	// networkPolicy limits what can reach the database pods, and what they can
	// reach. A compliance preset already limits what can reach them.
	// +optional
	NetworkPolicy *NetworkPolicySpec `json:"networkPolicy,omitempty"`
}

// The one value a network policy mode has: the direction is limited.
const NetworkPolicyRestricted = "Restricted"

// NetworkPolicySpec limits the database pods' traffic.
type NetworkPolicySpec struct {
	// ingress limits what can connect to the database pods.
	// +optional
	Ingress *IngressPolicySpec `json:"ingress,omitempty"`

	// egress limits what the database pods can connect to.
	// +optional
	Egress *EgressPolicySpec `json:"egress,omitempty"`
}

// IngressPolicySpec limits what can connect to the database pods.
type IngressPolicySpec struct {
	// mode is Restricted: only pods labelled db.youroperator.io/client=true in
	// the cluster's own namespace, the operator, the cluster's own pods and jobs,
	// and the peers listed below may connect. It is what a compliance preset
	// already does, for a cluster that has none. It depends on the cluster's
	// network plugin enforcing NetworkPolicy.
	// +kubebuilder:validation:Enum=Restricted
	// +kubebuilder:validation:Required
	Mode string `json:"mode"`

	// allowFrom lists additional peers that may connect to PostgreSQL.
	// +kubebuilder:validation:MaxItems=20
	// +optional
	AllowFrom []networkingv1.NetworkPolicyPeer `json:"allowFrom,omitempty"`
}

// EgressPolicySpec limits what the database pods can connect to.
type EgressPolicySpec struct {
	// mode is Restricted: the database pods may connect only to DNS, to each
	// other, to the Kubernetes API server (which Patroni keeps its leader lock
	// in), and to what is listed below. A network policy cannot name a host, so
	// the object store that backups, restores and audit logs go to has to be
	// listed, by address or by the pods that serve it, and a cluster that has
	// any of them must list something. It depends on the cluster's network
	// plugin enforcing NetworkPolicy for egress, which not every plugin does.
	// +kubebuilder:validation:Enum=Restricted
	// +kubebuilder:validation:Required
	Mode string `json:"mode"`

	// allow lists where else the database pods may connect.
	// +kubebuilder:validation:MaxItems=20
	// +optional
	Allow []networkingv1.NetworkPolicyEgressRule `json:"allow,omitempty"`
}

// PatroniAPISpec says who may change a cluster through Patroni's REST API.
type PatroniAPISpec struct {
	// authentication makes the API refuse every request that changes anything
	// (the configuration, a restart, a reload, a switchover) unless it carries
	// the cluster's API password, which the operator generates into
	// <name>-credentials as patroni-api-password and replaces with the
	// superuser's. Requests that only read, such as the kubelet's health checks,
	// are still answered to everyone. Setting it replaces the pods one at a time.
	// +optional
	Authentication bool `json:"authentication,omitempty"`
}

// TLSSpec says which certificate the database presents.
type TLSSpec struct {
	// secretName names a Secret in the cluster's namespace with tls.crt (the
	// certificate, then any intermediates), tls.key and ca.crt, such as one
	// cert-manager issues. The certificate must be valid for the cluster's
	// Service, <name>.<namespace>.svc, and for the pods' names under the headless
	// Service, *.<name>-headless.<namespace>.svc.cluster.local. The operator
	// checks the certificate against ca.crt before it changes anything, and
	// connects to the database with that check on. Renewing the certificate in
	// the Secret needs no restart.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Required
	SecretName string `json:"secretName"`

	// patroniAPI makes Patroni's REST API answer over TLS with this certificate,
	// and nothing else, and makes the members reach each other by their names
	// under the headless Service, which the certificate is valid for. The
	// operator checks the certificate when it calls the API. Plain HTTP on port
	// 8008 stops being served. A backup the image takes when a pod starts waits
	// for Patroni over plain HTTP, and does not run; the scheduled backups are
	// not affected.
	// +optional
	PatroniAPI bool `json:"patroniAPI,omitempty"`
}

// SecurityStatus is what the operator observed about the settings in
// spec.security.
type SecurityStatus struct {
	// tls is the certificate the primary presented when the operator last
	// connected to it.
	// +optional
	TLS *TLSStatus `json:"tls,omitempty"`

	// patroniAPI is how the primary's Patroni API was started. It differs from
	// spec.security while the pods are being replaced.
	// +optional
	PatroniAPI *PatroniAPIStatus `json:"patroniAPI,omitempty"`
}

// PatroniAPIStatus is how the primary's Patroni API was started.
type PatroniAPIStatus struct {
	// authenticated is true when the primary's API refuses requests that change
	// anything unless they carry the API password.
	Authenticated bool `json:"authenticated"`

	// tls is true when the primary's API answers over TLS only.
	TLS bool `json:"tls"`
}

// TLSStatus is the certificate a database presented.
type TLSStatus struct {
	// verified is true when the operator connected to the primary and the
	// certificate it presented chained to the CA in the Secret and was valid for
	// the cluster's Service.
	Verified bool `json:"verified"`

	// issuer is the issuer of the certificate the primary presented.
	// +optional
	Issuer string `json:"issuer,omitempty"`

	// notAfter is when the certificate the primary presented expires.
	// +optional
	NotAfter *metav1.Time `json:"notAfter,omitempty"`
}

// TLSSecretName is the Secret with the certificate, or empty if none is asked for.
func (s *SecuritySpec) TLSSecretName() string {
	if s == nil || s.TLS == nil {
		return ""
	}
	return s.TLS.SecretName
}

// RestrictsIngress is whether the cluster limits what can connect to its
// database pods because spec.security says so. A compliance preset does it too.
func (s *SecuritySpec) RestrictsIngress() bool {
	return s != nil && s.NetworkPolicy != nil && s.NetworkPolicy.Ingress != nil &&
		s.NetworkPolicy.Ingress.Mode == NetworkPolicyRestricted
}

// RestrictsEgress is whether the cluster limits what its database pods can
// connect to.
func (s *SecuritySpec) RestrictsEgress() bool {
	return s != nil && s.NetworkPolicy != nil && s.NetworkPolicy.Egress != nil &&
		s.NetworkPolicy.Egress.Mode == NetworkPolicyRestricted
}

// IngressAllowFrom is the peers spec.security lets connect, besides the ones
// that always may.
func (s *SecuritySpec) IngressAllowFrom() []networkingv1.NetworkPolicyPeer {
	if !s.RestrictsIngress() {
		return nil
	}
	return s.NetworkPolicy.Ingress.AllowFrom
}

// EgressAllow is where spec.security lets the database pods connect, besides
// where they always may.
func (s *SecuritySpec) EgressAllow() []networkingv1.NetworkPolicyEgressRule {
	if !s.RestrictsEgress() {
		return nil
	}
	return s.NetworkPolicy.Egress.Allow
}

// RestrictsReplicationRole is whether the replication role is to be limited to
// replicating, and pg_rewind given a login of its own.
func (s *SecuritySpec) RestrictsReplicationRole() bool {
	return s != nil && s.RestrictReplicationRole
}

// PatroniAPIAuthentication is whether the Patroni API is to require the
// operator's password for requests that change anything.
func (s *SecuritySpec) PatroniAPIAuthentication() bool {
	return s != nil && s.PatroniAPI != nil && s.PatroniAPI.Authentication
}

// PatroniAPITLS is whether the Patroni API is to answer over TLS.
func (s *SecuritySpec) PatroniAPITLS() bool {
	return s != nil && s.TLS != nil && s.TLS.PatroniAPI
}

// ValidateSecurity checks the shape of spec.security. The webhook refuses a spec
// that fails it, and the controller checks again, since a webhook is not always
// there.
func (s *PostgresClusterSpec) ValidateSecurity() field.ErrorList {
	path := field.NewPath("spec", "security")
	errs := s.Security.validate(path)
	// A cluster whose pods may connect to nothing else cannot reach its object
	// store, and WAL would pile up on the primary's disk with nothing saying why.
	// A network policy cannot name a host, so the person says where it is.
	if s.Security.RestrictsEgress() && len(s.Security.EgressAllow()) == 0 {
		if what := s.objectStoreUses(); what != "" {
			errs = append(errs, field.Required(path.Child("networkPolicy", "egress", "allow"),
				what+" go to an object store, which a network policy cannot name: list where it is, by address or by the pods that serve it"))
		}
	}
	return errs
}

// objectStoreUses says what in the spec connects to an object store, or is
// empty if nothing does.
func (s *PostgresClusterSpec) objectStoreUses() string {
	var uses []string
	if s.Backup.Enabled && s.Backup.Destination != nil {
		uses = append(uses, "backups")
	}
	if s.Compliance != nil && s.Compliance.AuditLogs.Destination != nil {
		uses = append(uses, "audit logs")
	}
	if s.Recovery() != nil {
		uses = append(uses, "restores")
	}
	return strings.Join(uses, ", ")
}

// validate checks the shape of the settings. What is in the Secret is for the
// controller to check, since the Secret may be made after the cluster.
func (s *SecuritySpec) validate(path *field.Path) field.ErrorList {
	if s == nil || s.TLS == nil {
		return nil
	}
	var errs field.ErrorList
	for _, msg := range validation.IsDNS1123Subdomain(s.TLS.SecretName) {
		errs = append(errs, field.Invalid(path.Child("tls", "secretName"), s.TLS.SecretName, msg))
	}
	return errs
}
