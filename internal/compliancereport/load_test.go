/*
Copyright 2026.

Licensed under the Business Source License 1.1 (the "License"). You may not
use this file except in compliance with the License, a copy of which is in the
LICENSE file at the root of this repository. Production use requires a
commercial license from the Licensor. On the Change Date (2030-10-17) the
work becomes available under the Change License (Apache-2.0).
*/

package compliancereport

import (
	"errors"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestARefusalNamesThePermissionThatIsMissing(t *testing.T) {
	forbidden := apierrors.NewForbidden(schema.GroupResource{Group: "db.youroperator.io", Resource: "postgresemergencyaccesses"}, "", errors.New("no"))
	err := readError(forbidden, "list the requests in shop", "list", "postgresemergencyaccesses")
	for _, want := range []string{"not allowed to list the requests in shop", "needs list on postgresemergencyaccesses.db.youroperator.io"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q lacks %q", err, want)
		}
	}
	other := readError(errors.New("connection refused"), "get the cluster", "get", "postgresclusters")
	if strings.Contains(other.Error(), "not allowed") || !strings.Contains(other.Error(), "connection refused") {
		t.Errorf("an error that is not a refusal was reported as one: %v", other)
	}
}
