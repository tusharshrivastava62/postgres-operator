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
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
)

const secretOK = "orders-tls"

func TestSecurityIsOffUnlessSet(t *testing.T) {
	var unset *SecuritySpec
	if unset.TLSSecretName() != "" {
		t.Error("a cluster with no security settings names a Secret")
	}
	if (&SecuritySpec{}).TLSSecretName() != "" {
		t.Error("empty security settings name a Secret")
	}
	if got := (&SecuritySpec{TLS: &TLSSpec{SecretName: secretOK}}).TLSSecretName(); got != secretOK {
		t.Errorf("got %q", got)
	}
	if errs := (&PostgresClusterSpec{}).ValidateSecurity(); len(errs) != 0 {
		t.Errorf("nothing set, and %v", errs)
	}
}

func TestReplicationRoleIsLimitedOnlyWhenAskedAndTheLoginIsReserved(t *testing.T) {
	var unset *SecuritySpec
	if unset.RestrictsReplicationRole() || (&SecuritySpec{}).RestrictsReplicationRole() {
		t.Error("a cluster that did not ask is limited")
	}
	if !(&SecuritySpec{RestrictReplicationRole: true}).RestrictsReplicationRole() {
		t.Error("a cluster that asked is not")
	}
	// An application role of the rewind login's name would take it over.
	spec := &PostgresClusterSpec{Roles: []RoleSpec{{Name: "pgoperator_rewind"}}}
	errs := spec.ValidateAccessControl("orders")
	if len(errs) == 0 || !strings.Contains(errs.ToAggregate().Error(), "pgoperator_rewind") {
		t.Errorf("the login's name is not reserved: %v", errs)
	}
}

func TestTheCertificateSecretNameMustBeAName(t *testing.T) {
	cases := map[string]bool{
		secretOK:                 true,
		"orders.tls":             true,
		"a":                      true,
		"":                       false,
		"Orders-TLS":             false,
		"orders_tls":             false,
		"-orders":                false,
		"orders/tls":             false,
		strings.Repeat("a", 254): false,
	}
	for name, ok := range cases {
		spec := &PostgresClusterSpec{Security: &SecuritySpec{TLS: &TLSSpec{SecretName: name}}}
		errs := spec.ValidateSecurity()
		if (len(errs) == 0) != ok {
			t.Errorf("%q: valid=%v, errors %v", name, ok, errs)
		}
		for _, err := range errs {
			if err.Field != "spec.security.tls.secretName" {
				t.Errorf("%q: error on %q", name, err.Field)
			}
		}
	}
}

func egressOnly(allow ...networkingv1.NetworkPolicyEgressRule) *SecuritySpec {
	return &SecuritySpec{NetworkPolicy: &NetworkPolicySpec{Egress: &EgressPolicySpec{Mode: NetworkPolicyRestricted, Allow: allow}}}
}

func TestADirectionIsLimitedOnlyWhenItsModeSaysSo(t *testing.T) {
	var unset *SecuritySpec
	if unset.RestrictsIngress() || unset.RestrictsEgress() || len(unset.IngressAllowFrom()) != 0 || len(unset.EgressAllow()) != 0 {
		t.Error("a cluster with no security settings is limited")
	}
	if (&SecuritySpec{NetworkPolicy: &NetworkPolicySpec{}}).RestrictsEgress() {
		t.Error("an empty policy limits egress")
	}
	odd := &SecuritySpec{NetworkPolicy: &NetworkPolicySpec{
		Ingress: &IngressPolicySpec{Mode: "Open", AllowFrom: []networkingv1.NetworkPolicyPeer{{}}},
		Egress:  &EgressPolicySpec{Mode: "Open", Allow: []networkingv1.NetworkPolicyEgressRule{{}}},
	}}
	if odd.RestrictsIngress() || odd.RestrictsEgress() || len(odd.IngressAllowFrom()) != 0 || len(odd.EgressAllow()) != 0 {
		t.Error("a mode other than Restricted limits a direction, or its list is used")
	}
}

func TestEgressNeedsSomewhereToReachTheObjectStoreWhenThereIsOne(t *testing.T) {
	store := networkingv1.NetworkPolicyEgressRule{To: []networkingv1.NetworkPolicyPeer{{}}}
	backups := func(spec *PostgresClusterSpec) {
		spec.Backup = BackupSpec{Enabled: true, Destination: &BackupDestination{Bucket: "b"}}
	}
	audit := func(spec *PostgresClusterSpec) {
		spec.Compliance = &ComplianceSpec{Standard: StandardHIPAA, AuditLogs: AuditLogsSpec{Destination: &BackupDestination{Bucket: "a"}}}
	}
	restore := func(spec *PostgresClusterSpec) {
		spec.Bootstrap = &BootstrapSpec{Recovery: &RecoverySpec{Destination: BackupDestination{Bucket: "r"}}}
	}
	cases := []struct {
		name    string
		uses    []func(*PostgresClusterSpec)
		allow   []networkingv1.NetworkPolicyEgressRule
		refused string
	}{
		{"nothing goes to a store", nil, nil, ""},
		{"backups, nowhere listed", []func(*PostgresClusterSpec){backups}, nil, "backups go to an object store"},
		{"audit logs, nowhere listed", []func(*PostgresClusterSpec){audit}, nil, "audit logs go to an object store"},
		{"a restore, nowhere listed", []func(*PostgresClusterSpec){restore}, nil, "restores go to an object store"},
		{"all three, nowhere listed", []func(*PostgresClusterSpec){backups, audit, restore}, nil, "backups, audit logs, restores go to an object store"},
		{"backups, somewhere listed", []func(*PostgresClusterSpec){backups}, []networkingv1.NetworkPolicyEgressRule{store}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			spec := &PostgresClusterSpec{Security: egressOnly(c.allow...)}
			for _, use := range c.uses {
				use(spec)
			}
			errs := spec.ValidateSecurity()
			switch {
			case c.refused == "" && len(errs) != 0:
				t.Errorf("refused: %v", errs)
			case c.refused != "" && (len(errs) != 1 || !strings.Contains(errs[0].Error(), c.refused) ||
				errs[0].Field != "spec.security.networkPolicy.egress.allow"):
				t.Errorf("errors %v, want one on spec.security.networkPolicy.egress.allow about %q", errs, c.refused)
			}
		})
	}

	t.Run("backups that are not enabled go nowhere", func(t *testing.T) {
		spec := &PostgresClusterSpec{Security: egressOnly(), Backup: BackupSpec{Enabled: false, Destination: &BackupDestination{Bucket: "b"}}}
		if errs := spec.ValidateSecurity(); len(errs) != 0 {
			t.Errorf("refused: %v", errs)
		}
	})
	t.Run("limiting ingress needs nothing listed", func(t *testing.T) {
		spec := &PostgresClusterSpec{Security: &SecuritySpec{NetworkPolicy: &NetworkPolicySpec{
			Ingress: &IngressPolicySpec{Mode: NetworkPolicyRestricted}}}}
		backups(spec)
		if errs := spec.ValidateSecurity(); len(errs) != 0 {
			t.Errorf("refused: %v", errs)
		}
	})
}
