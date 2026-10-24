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
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

func dur(d time.Duration) *metav1.Duration { return &metav1.Duration{Duration: d} }

const (
	fieldDuration = "spec.duration"
	fieldReason   = "spec.reason"
)

const goodReason = "orders database is out of connections and the application is down"

func TestClusterSwitchForEmergencyAccess(t *testing.T) {
	var unset *EmergencyAccessSpec
	if unset.Allowed() {
		t.Error("a cluster with no setting allows emergency access")
	}
	if (&EmergencyAccessSpec{}).Allowed() {
		t.Error("a cluster allows it without enabled")
	}
	if !(&EmergencyAccessSpec{Enabled: true}).Allowed() {
		t.Error("enabled does not allow it")
	}
	for name, spec := range map[string]*EmergencyAccessSpec{
		"unset": nil, "no maximum": {Enabled: true}, "a zero maximum": {Enabled: true, MaxDuration: dur(0)},
	} {
		if got := spec.MaxAllowed(); got != MaxEmergencyDuration {
			t.Errorf("%s: MaxAllowed = %s, want %s", name, got, MaxEmergencyDuration)
		}
	}
	if got := (&EmergencyAccessSpec{MaxDuration: dur(2 * time.Hour)}).MaxAllowed(); got != 2*time.Hour {
		t.Errorf("MaxAllowed = %s", got)
	}
}

func TestClusterMaximumIsBetweenAMinuteAndEightHours(t *testing.T) {
	path := field.NewPath("spec", "emergencyAccess")
	for _, ok := range []time.Duration{time.Minute, time.Hour, MaxEmergencyDuration} {
		if errs := (&EmergencyAccessSpec{Enabled: true, MaxDuration: dur(ok)}).Validate(path); len(errs) != 0 {
			t.Errorf("%s was refused: %v", ok, errs)
		}
	}
	for _, bad := range []time.Duration{0, time.Second, 59 * time.Second, MaxEmergencyDuration + time.Second, 24 * time.Hour, -time.Hour} {
		errs := (&EmergencyAccessSpec{Enabled: true, MaxDuration: dur(bad)}).Validate(path)
		if len(errs) != 1 || !strings.Contains(errs[0].Field, "spec.emergencyAccess.maxDuration") {
			t.Errorf("%s was accepted: %v", bad, errs)
		}
	}
	var unset *EmergencyAccessSpec
	if errs := unset.Validate(path); len(errs) != 0 {
		t.Errorf("an unset setting has problems: %v", errs)
	}
	// It is part of the rules for the whole spec.
	spec := &PostgresClusterSpec{EmergencyAccess: &EmergencyAccessSpec{Enabled: true, MaxDuration: dur(24 * time.Hour)}}
	if errs := spec.ValidateAccessControl("orders"); len(errs) != 1 {
		t.Errorf("the cluster spec does not check it: %v", errs)
	}
}

func TestRequestDefaultsToAnHour(t *testing.T) {
	if got := (&PostgresEmergencyAccessSpec{}).EffectiveDuration(); got != DefaultEmergencyDuration {
		t.Errorf("EffectiveDuration = %s", got)
	}
	spec := &PostgresEmergencyAccessSpec{Duration: metav1.Duration{Duration: 20 * time.Minute}}
	if got := spec.EffectiveDuration(); got != 20*time.Minute {
		t.Errorf("EffectiveDuration = %s", got)
	}
}

func TestRequestIsCheckedAgainstWhatTheClusterAllows(t *testing.T) {
	spec := func(d time.Duration, reason string) *PostgresEmergencyAccessSpec {
		return &PostgresEmergencyAccessSpec{Duration: metav1.Duration{Duration: d}, Reason: reason}
	}
	tests := []struct {
		name  string
		spec  *PostgresEmergencyAccessSpec
		limit time.Duration
		want  string // a field that should be refused; empty means none
	}{
		{"an ordinary request", spec(time.Hour, goodReason), 8 * time.Hour, ""},
		{"no duration, so the default", spec(0, goodReason), 8 * time.Hour, ""},
		{"exactly the limit", spec(2*time.Hour, goodReason), 2 * time.Hour, ""},
		{"a minute", spec(time.Minute, goodReason), 2 * time.Hour, ""},
		{"over the limit", spec(3*time.Hour, goodReason), 2 * time.Hour, fieldDuration},
		{"under a minute", spec(30*time.Second, goodReason), 2 * time.Hour, fieldDuration},
		{"negative", spec(-time.Hour, goodReason), 2 * time.Hour, fieldDuration},
		{"a reason that says nothing", spec(time.Hour, "urgent"), time.Hour, fieldReason},
		{"a reason made of spaces", spec(time.Hour, "              "), time.Hour, fieldReason},
		{"no reason", spec(time.Hour, ""), time.Hour, fieldReason},
		{"ten characters is enough", spec(time.Hour, "0123456789"), time.Hour, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs := tt.spec.Validate(tt.limit)
			if tt.want == "" && len(errs) != 0 {
				t.Fatalf("unexpected problems: %v", errs)
			}
			if tt.want != "" && (len(errs) != 1 || errs[0].Field != tt.want) {
				t.Fatalf("problems = %v, want one at %s", errs, tt.want)
			}
		})
	}
	both := spec(48*time.Hour, "no").Validate(time.Hour)
	if len(both) != 2 {
		t.Errorf("a request with two problems reports %d: %v", len(both), both)
	}
}

func TestTheWarningNamesWhatIsLogged(t *testing.T) {
	for _, want := range []string{"every statement", "values you bind", "clear text", "whatever the audit settings say"} {
		if !strings.Contains(ParameterLoggingWarning, want) {
			t.Errorf("the warning does not say %q: %s", want, ParameterLoggingWarning)
		}
	}
}

func TestRolesMayNotUseTheEmergencyPrefix(t *testing.T) {
	spec := specWithRoles(RoleSpec{Name: "bg_alice"})
	if got := spec.ValidateAccessControl("orders").ToAggregate(); got == nil || !strings.Contains(got.Error(), "bg_") {
		t.Errorf("a role named like an emergency role was accepted: %v", got)
	}
}
