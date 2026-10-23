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

const (
	nameRule        = "lower-case letters"
	appName         = "app"
	oneName         = "one"
	everyClusterHas = "every cluster already has"
	usesItself      = "uses itself"
	twoName         = "two"
)

func specWithRoles(roles ...RoleSpec) *PostgresClusterSpec {
	return &PostgresClusterSpec{Roles: roles}
}

func problemsFor(t *testing.T, spec *PostgresClusterSpec) string {
	t.Helper()
	return spec.ValidateRoles("orders").ToAggregate().Error()
}

func TestValidateRolesAcceptsOrdinaryRoles(t *testing.T) {
	spec := specWithRoles(
		RoleSpec{Name: "app_orders", Database: "orders"},
		RoleSpec{Name: "reporting", SecretName: "reporting-db"},
		RoleSpec{Name: "a"},
	)
	if errs := spec.ValidateRoles("orders"); len(errs) != 0 {
		t.Fatalf("unexpected problems: %v", errs)
	}
	if errs := (&PostgresClusterSpec{}).ValidateRoles("orders"); len(errs) != 0 {
		t.Fatalf("a spec with no roles has problems: %v", errs)
	}
}

func TestValidateRolesRefusesNames(t *testing.T) {
	long := "a" + strings.Repeat("b", 63)
	tests := []struct{ name, role, want string }{
		{"upper case", "App", nameRule},
		{"starts with a digit", "1app", nameRule},
		{"has a hyphen", "my-app", nameRule},
		{"empty", "", nameRule},
		{"too long", long, "63 characters"},
		{"postgres' own prefix", "pg_app", "must not start with pg_"},
		{"emergency access prefix", "bg_alice", "must not start with bg_"},
		{"the superuser", databasePostgres, usesItself},
		{"the replication role", "replicator", usesItself},
		{"spilo's admin", "admin", usesItself},
		{"spilo's cron admin", "cron_admin", usesItself},
		{"spilo's monitoring role", "robot_zmon", usesItself},
		{"spilo's pam group", "zalandos", usesItself},
		{"the operator's monitor", "pgoperator_monitor", usesItself},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := problemsFor(t, specWithRoles(RoleSpec{Name: tt.role}))
			if !strings.Contains(got, tt.want) || !strings.Contains(got, "spec.roles[0].name") {
				t.Errorf("problems = %q, want one at spec.roles[0].name containing %q", got, tt.want)
			}
		})
	}
}

func TestValidateRolesAcceptsANameThatOnlyContainsAReservedOne(t *testing.T) {
	// postgres_app and my_admin are not the reserved names, and pg and bg
	// without the underscore are not the reserved prefixes.
	for _, name := range []string{"postgres_app", "my_admin", "pgapp", "bgjob", "app_pg_"} {
		if errs := specWithRoles(RoleSpec{Name: name}).ValidateRoles("orders"); len(errs) != 0 {
			t.Errorf("%s was refused: %v", name, errs)
		}
	}
}

func TestValidateRolesRefusesDuplicates(t *testing.T) {
	got := problemsFor(t, specWithRoles(RoleSpec{Name: appName}, RoleSpec{Name: appName, SecretName: "other"}))
	if !strings.Contains(got, "spec.roles[1].name") || !strings.Contains(got, "also entry 0") {
		t.Errorf("a repeated name was not refused at the second entry: %q", got)
	}
}

func TestValidateRolesRefusesDatabases(t *testing.T) {
	tests := []struct{ name, db, want string }{
		{databasePostgres, databasePostgres, everyClusterHas},
		{databaseTemplate0, databaseTemplate0, everyClusterHas},
		{databaseTemplate1, databaseTemplate1, everyClusterHas},
		{"bad characters", "My-DB", nameRule},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := problemsFor(t, specWithRoles(RoleSpec{Name: appName, Database: tt.db}))
			if !strings.Contains(got, "spec.roles[0].database") || !strings.Contains(got, tt.want) {
				t.Errorf("problems = %q", got)
			}
		})
	}
	got := problemsFor(t, specWithRoles(
		RoleSpec{Name: oneName, Database: "shared"}, RoleSpec{Name: twoName, Database: "shared"}))
	if !strings.Contains(got, "spec.roles[1].database") || !strings.Contains(got, "one owner") {
		t.Errorf("two roles named one database: %q", got)
	}
}

func TestRoleSecretNameDefaultsAndHyphenates(t *testing.T) {
	if got := (&RoleSpec{Name: "app_orders_v2"}).RoleSecretName("orders"); got != "orders-role-app-orders-v2" {
		t.Errorf("default = %q", got)
	}
	if got := (&RoleSpec{Name: appName, SecretName: "mine"}).RoleSecretName("orders"); got != "mine" {
		t.Errorf("explicit = %q", got)
	}
}

func TestValidateRolesRefusesSecretsThatAreTakenOrShared(t *testing.T) {
	backup := BackupSpec{
		Enabled:     true,
		Destination: &BackupDestination{Bucket: "b", CredentialsSecret: "backup-s3"},
		Encryption:  &BackupEncryption{KeySecret: "backup-key"},
	}
	audit := &ComplianceSpec{AuditLogs: AuditLogsSpec{Destination: &BackupDestination{Bucket: "a", CredentialsSecret: "audit-s3"}}}
	recovery := &BootstrapSpec{Recovery: &RecoverySpec{
		SourceCluster: "old", Destination: BackupDestination{Bucket: "r", CredentialsSecret: "restore-s3"},
		Encryption: &BackupEncryption{KeySecret: "restore-key"},
	}}
	tests := []struct{ secret, want string }{
		{"orders-credentials", "the cluster's own credentials"},
		{"orders-monitor", "the monitor role's credentials"},
		{"backup-s3", "the backup credentials"},
		{"backup-key", "the backup encryption key"},
		{"audit-s3", "the audit log credentials"},
		{"restore-s3", "the restore credentials"},
		{"restore-key", "the restore encryption key"},
	}
	for _, tt := range tests {
		t.Run(tt.secret, func(t *testing.T) {
			spec := &PostgresClusterSpec{Backup: backup, Compliance: audit, Bootstrap: recovery,
				Roles: []RoleSpec{{Name: appName, SecretName: tt.secret}}}
			got := spec.ValidateRoles("orders").ToAggregate().Error()
			if !strings.Contains(got, "spec.roles[0].secretName") || !strings.Contains(got, tt.want) {
				t.Errorf("problems = %q", got)
			}
		})
	}

	got := problemsFor(t, specWithRoles(
		RoleSpec{Name: oneName, SecretName: "same"}, RoleSpec{Name: twoName, SecretName: "same"}))
	if !strings.Contains(got, "spec.roles[1].secretName") || !strings.Contains(got, "each role needs a Secret of its own") {
		t.Errorf("two roles shared a Secret: %q", got)
	}
	// An explicit name that equals another role's default is shared too.
	got = problemsFor(t, specWithRoles(RoleSpec{Name: oneName}, RoleSpec{Name: twoName, SecretName: "orders-role-one"}))
	if !strings.Contains(got, "each role needs a Secret of its own") {
		t.Errorf("a name equal to another role's default was accepted: %q", got)
	}
}

func TestValidateRolesRefusesASecretNameKubernetesWouldRefuse(t *testing.T) {
	for _, bad := range []string{"Has_Upper", "under_score", "-leading", strings.Repeat("a", 254)} {
		got := problemsFor(t, specWithRoles(RoleSpec{Name: appName, SecretName: bad}))
		if !strings.Contains(got, "spec.roles[0].secretName") {
			t.Errorf("%q was accepted: %q", bad, got)
		}
	}
}

func TestValidateRolesReportsEveryProblem(t *testing.T) {
	spec := specWithRoles(RoleSpec{Name: databasePostgres}, RoleSpec{Name: "BAD"}, RoleSpec{Name: "pg_x"})
	if n := len(spec.ValidateRoles("orders")); n != 3 {
		t.Errorf("found %d problems, want 3", n)
	}
}
