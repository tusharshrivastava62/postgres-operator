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
