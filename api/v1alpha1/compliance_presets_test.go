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
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

const (
	ordersPath       = "prod/orders"
	defaultAuditPath = "audit/prod/orders"
)

// parameterName is the shape of a PostgreSQL setting name: lower-case words
// joined by underscores, with an optional extension prefix and a dot.
var parameterName = regexp.MustCompile(`^([a-z][a-z0-9_]*\.)?[a-z][a-z0-9_]*$`)

// pgauditClasses are the statement classes pgaudit.log accepts, apart from the
// two that mean everything and nothing.
var pgauditClasses = []string{"read", "write", "function", "role", "ddl", "misc", "misc_set"}

// knownParameters reads the snapshot of real PostgreSQL setting names the
// presets are checked against. See the header of the file for how it is made.
func knownParameters(t *testing.T) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile("testdata/postgres_parameters.txt")
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for line := range strings.SplitSeq(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			names[line] = true
		}
	}
	if len(names) < 300 {
		t.Fatalf("the parameter snapshot has only %d names; it should hold the settings of a whole PostgreSQL", len(names))
	}
	return names
}

func TestEveryPresetSettingIsARealPostgresParameter(t *testing.T) {
	known := knownParameters(t)
	for _, standard := range Standards() {
		preset, _ := PresetFor(standard)
		for _, logParameters := range []bool{false, true} {
			for name := range preset.Parameters(logParameters) {
				if !parameterName.MatchString(name) {
					t.Errorf("%s: %q is not shaped like a PostgreSQL parameter name", standard, name)
				}
				if !known[name] {
					t.Errorf("%s: %q is not a setting of PostgreSQL 16 and 18 with pgaudit loaded", standard, name)
				}
			}
		}
	}
}

func TestPresetValuesAreValid(t *testing.T) {
	for _, standard := range Standards() {
		preset, _ := PresetFor(standard)
		params := preset.Parameters(false)

		classes := strings.Split(params["pgaudit.log"], ",")
		if len(classes) == 0 || classes[0] == "" {
			t.Errorf("%s: pgaudit.log is empty", standard)
		}
		seen := map[string]bool{}
		for _, class := range classes {
			if !slices.Contains(pgauditClasses, class) {
				t.Errorf("%s: %q is not a pgaudit class", standard, class)
			}
			if seen[class] {
				t.Errorf("%s: class %q is listed twice", standard, class)
			}
			seen[class] = true
		}

		for _, name := range []string{"pgaudit.log_catalog", "pgaudit.log_parameter"} {
			if v := params[name]; v != settingOn && v != settingOff {
				t.Errorf("%s: %s = %q, want on or off", standard, name, v)
			}
		}
		if v := params["log_statement"]; !slices.Contains([]string{"none", "ddl", "mod", "all"}, v) {
			t.Errorf("%s: log_statement = %q is not a valid value", standard, v)
		}
	}
}

func TestParameterLoggingFollowsTheSpecAndIsOffByDefault(t *testing.T) {
	for _, standard := range Standards() {
		preset, _ := PresetFor(standard)
		if got := preset.Parameters(false)["pgaudit.log_parameter"]; got != settingOff {
			t.Errorf("%s: pgaudit.log_parameter = %q when not asked for, want off: it writes values in clear text", standard, got)
		}
		if got := preset.Parameters(true)["pgaudit.log_parameter"]; got != settingOn {
			t.Errorf("%s: pgaudit.log_parameter = %q when asked for, want on", standard, got)
		}
	}
}

func TestPresetDefaults(t *testing.T) {
	want := map[string]struct {
		days    int32
		classes string
	}{
		StandardHIPAA: {2190, "read,write,ddl,role"},
		StandardSOC2:  {365, "read,write,ddl"},
		StandardPCI:   {365, "read,write,ddl,role"},
	}
	if len(Standards()) != len(want) {
		t.Fatalf("Standards() = %v, want exactly %d", Standards(), len(want))
	}
	for standard, w := range want {
		preset, ok := PresetFor(standard)
		if !ok {
			t.Fatalf("no preset for %s", standard)
		}
		if preset.DefaultRetentionDays != w.days {
			t.Errorf("%s: default retention = %d days, want %d", standard, preset.DefaultRetentionDays, w.days)
		}
		if got := preset.Parameters(false)["pgaudit.log"]; got != w.classes {
			t.Errorf("%s: pgaudit.log = %q, want %q", standard, got, w.classes)
		}
	}
}

func TestPresetControlsAreKnownAndNotRepeated(t *testing.T) {
	known := []string{
		ControlAuditLogging, ControlConnectionLogging, ControlEncryptionInTransit, ControlAuthentication,
		ControlDataIntegrity, ControlAuditLogDelivery, ControlAuditLogRetention, ControlNetworkIsolation,
		ControlBackupRecovery, ControlAvailability,
	}
	for _, standard := range Standards() {
		preset, _ := PresetFor(standard)
		if len(preset.Controls) == 0 {
			t.Errorf("%s asks for no controls", standard)
		}
		seen := map[string]bool{}
		for _, id := range preset.Controls {
			if !slices.Contains(known, id) {
				t.Errorf("%s: %q is not a known control", standard, id)
			}
			if seen[id] {
				t.Errorf("%s: control %q is listed twice", standard, id)
			}
			seen[id] = true
			if len(id) > 63 {
				t.Errorf("%s: control %q is longer than the 63 characters the API allows", standard, id)
			}
		}
	}
}

func TestPresetForUnknownStandard(t *testing.T) {
	for _, standard := range []string{"", "HIPAA", "iso27001"} {
		if _, ok := PresetFor(standard); ok {
			t.Errorf("PresetFor(%q) found a preset", standard)
		}
	}
}

// The CRD lists the standards in an enum, and the table lists them in Go. They
// must agree, or a standard could be accepted with no preset behind it, or
// have a preset the API refuses.
func TestCRDEnumMatchesPresets(t *testing.T) {
	raw, err := os.ReadFile("../../config/crd/bases/db.youroperator.io_postgresclusters.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var crd struct {
		Spec struct {
			Versions []struct {
				Schema struct {
					OpenAPIV3Schema struct {
						Properties struct {
							Spec struct {
								Properties struct {
									Compliance struct {
										Properties struct {
											Standard struct {
												Enum []string `json:"enum"`
											} `json:"standard"`
										} `json:"properties"`
									} `json:"compliance"`
								} `json:"properties"`
							} `json:"spec"`
						} `json:"properties"`
					} `json:"openAPIV3Schema"`
				} `json:"schema"`
			} `json:"versions"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		t.Fatal(err)
	}
	if len(crd.Spec.Versions) != 1 {
		t.Fatalf("expected one CRD version, found %d", len(crd.Spec.Versions))
	}
	enum := slices.Clone(crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties.Spec.Properties.Compliance.Properties.Standard.Enum)
	got := Standards()
	slices.Sort(enum)
	slices.Sort(got)
	if !slices.Equal(enum, got) {
		t.Errorf("the CRD allows standards %v but the preset table has %v", enum, got)
	}
}

func TestEffectiveRetentionDays(t *testing.T) {
	tests := []struct {
		name string
		spec *ComplianceSpec
		want int32
	}{
		{"nil", nil, 0},
		{"unset uses the standard's default", &ComplianceSpec{Standard: StandardHIPAA}, 2190},
		{"set wins", &ComplianceSpec{Standard: StandardHIPAA, AuditLogs: AuditLogsSpec{RetentionDays: 400}}, 400},
		{"pci default", &ComplianceSpec{Standard: StandardPCI}, 365},
		{"unknown standard has none", &ComplianceSpec{Standard: "other"}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.spec.EffectiveRetentionDays(); got != tt.want {
				t.Errorf("EffectiveRetentionDays() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestAuditPathFor(t *testing.T) {
	custom := &ComplianceSpec{AuditLogs: AuditLogsSpec{Destination: &BackupDestination{Path: "/team/audit/"}}}
	tests := []struct {
		name string
		spec *ComplianceSpec
		want string
	}{
		{"nil spec uses the default", nil, defaultAuditPath},
		{"no destination uses the default", &ComplianceSpec{}, defaultAuditPath},
		{"empty path uses the default", &ComplianceSpec{AuditLogs: AuditLogsSpec{Destination: &BackupDestination{}}}, defaultAuditPath},
		{"explicit path wins, trimmed", custom, "team/audit"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.spec.AuditPathFor("prod", "orders"); got != tt.want {
				t.Errorf("AuditPathFor() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDefaultAuditPathDoesNotNestWithDefaultBackupPath(t *testing.T) {
	backup := (*BackupDestination)(nil).PathFor("prod", "orders")
	audit := (*ComplianceSpec)(nil).AuditPathFor("prod", "orders")
	if PathsOverlap(backup, audit) {
		t.Errorf("the default audit path %q overlaps the default backup path %q", audit, backup)
	}
}

func TestPathsOverlap(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{ordersPath, ordersPath, true},
		{ordersPath, "/prod/orders/", true},
		{ordersPath, "prod/orders/audit", true},
		{"prod/orders/audit", ordersPath, true},
		{"prod", ordersPath, true},
		{ordersPath, "prod/orders-audit", false},
		{ordersPath, "staging/orders", false},
		{"a", "b", false},
	}
	for _, tt := range tests {
		if got := PathsOverlap(tt.a, tt.b); got != tt.want {
			t.Errorf("PathsOverlap(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
}
