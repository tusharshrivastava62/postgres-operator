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
	"regexp"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

const (
	// databasePostgres is both the superuser's name and the database every
	// cluster is made with.
	databasePostgres  = "postgres"
	databaseTemplate0 = "template0"
	databaseTemplate1 = "template1"
)

// reservedRoleNames are the roles PostgreSQL, Patroni, Spilo and this operator
// make for themselves. An application role of the same name would take one
// over, or be taken over by it.
var reservedRoleNames = []string{
	databasePostgres, "replicator", "admin", "cron_admin", "robot_zmon", "zalandos", "pgoperator_monitor", "pgoperator_rewind",
}

// Role names that start with these are never an application's: PostgreSQL's
// own roles, and the temporary ones the operator makes for emergency access.
var reservedRolePrefixes = []string{"pg_", "bg_"}

// reservedDatabaseNames are the databases that exist in every cluster.
var reservedDatabaseNames = []string{databasePostgres, databaseTemplate0, databaseTemplate1}

var identifier = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// RoleSecretName is the Secret a role's connection details are written to:
// the one the spec names, or <cluster>-role-<name> with the underscores made
// hyphens, which a Secret name allows and a role name does not. The mapping
// cannot make two roles share a name, since a role name has no hyphens.
func (r *RoleSpec) RoleSecretName(cluster string) string {
	if r.SecretName != "" {
		return r.SecretName
	}
	return cluster + "-role-" + strings.ReplaceAll(r.Name, "_", "-")
}

// MinRotateEvery is the shortest time between replacing a password. A password
// that changes more often than an application can be restarted is one the
// application never has.
const MinRotateEvery = 24 * time.Hour

func validateRotateEvery(path *field.Path, every *metav1.Duration) *field.Error {
	if every != nil && every.Duration < MinRotateEvery {
		return field.Invalid(path, every.Duration.String(), "must be at least 24h: an application needs time to pick up a new password")
	}
	return nil
}

// ValidateAccessControl holds the rules for spec.roles, spec.credentialRotation and
// spec.emergencyAccess
// that the CRD schema cannot express: which names are taken, and which are
// shared, and how often a password may change. The webhook refuses a spec that
// breaks them, and the controller checks again, since a webhook is not always
// there.
func (s *PostgresClusterSpec) ValidateAccessControl(cluster string) field.ErrorList {
	var errs field.ErrorList
	path := field.NewPath("spec", "roles")

	errs = append(errs, s.EmergencyAccess.Validate(field.NewPath("spec", "emergencyAccess"))...)
	if rotation := s.CredentialRotation; rotation != nil {
		rotationPath := field.NewPath("spec", "credentialRotation")
		if err := validateRotateEvery(rotationPath.Child("superuserEvery"), rotation.SuperuserEvery); err != nil {
			errs = append(errs, err)
		}
		if err := validateRotateEvery(rotationPath.Child("monitorEvery"), rotation.MonitorEvery); err != nil {
			errs = append(errs, err)
		}
	}

	// The Secrets the operator makes, and the ones the spec asks it to read.
	// A role's Secret must not be any of them.
	taken := map[string]string{
		cluster + "-credentials": "the cluster's own credentials",
		cluster + "-monitor":     "the monitor role's credentials",
	}
	for _, named := range s.namedSecrets() {
		taken[named.name] = named.what
	}

	names := map[string]int{}
	databases := map[string]int{}
	secrets := map[string]int{}
	for i := range s.Roles {
		role := &s.Roles[i]
		at := path.Index(i)

		if err := validateRotateEvery(at.Child("rotateEvery"), role.RotateEvery); err != nil {
			errs = append(errs, err)
		}
		errs = append(errs, validateRoleName(at.Child("name"), role.Name)...)
		if first, dup := names[role.Name]; dup {
			errs = append(errs, field.Duplicate(at.Child("name"), fmt.Sprintf("%s (also entry %d)", role.Name, first)))
		}
		names[role.Name] = i

		if role.Database != "" {
			errs = append(errs, validateDatabaseName(at.Child("database"), role.Database)...)
			if first, dup := databases[role.Database]; dup {
				errs = append(errs, field.Duplicate(at.Child("database"),
					fmt.Sprintf("%s (also entry %d): a database has one owner", role.Database, first)))
			}
			databases[role.Database] = i
		}

		secret := role.RoleSecretName(cluster)
		if role.SecretName != "" {
			for _, msg := range validation.IsDNS1123Subdomain(role.SecretName) {
				errs = append(errs, field.Invalid(at.Child("secretName"), role.SecretName, msg))
			}
		}
		if what, clash := taken[secret]; clash {
			errs = append(errs, field.Invalid(at.Child("secretName"), secret,
				"is already used for "+what+": choose another Secret name"))
		}
		if first, dup := secrets[secret]; dup {
			errs = append(errs, field.Duplicate(at.Child("secretName"),
				fmt.Sprintf("%s (also entry %d): each role needs a Secret of its own", secret, first)))
		}
		secrets[secret] = i
	}
	return errs
}

func validateRoleName(path *field.Path, name string) field.ErrorList {
	var errs field.ErrorList
	if !identifier.MatchString(name) || len(name) > 63 {
		return append(errs, field.Invalid(path, name,
			"must be lower-case letters, digits and underscores, starting with a letter, at most 63 characters"))
	}
	for _, prefix := range reservedRolePrefixes {
		if strings.HasPrefix(name, prefix) {
			errs = append(errs, field.Invalid(path, name, fmt.Sprintf("must not start with %s: those roles are not an application's", prefix)))
		}
	}
	for _, reserved := range reservedRoleNames {
		if name == reserved {
			errs = append(errs, field.Invalid(path, name, "is a role PostgreSQL, Patroni or the operator uses itself"))
		}
	}
	return errs
}

func validateDatabaseName(path *field.Path, name string) field.ErrorList {
	var errs field.ErrorList
	if !identifier.MatchString(name) || len(name) > 63 {
		return append(errs, field.Invalid(path, name,
			"must be lower-case letters, digits and underscores, starting with a letter, at most 63 characters"))
	}
	for _, reserved := range reservedDatabaseNames {
		if name == reserved {
			errs = append(errs, field.Invalid(path, name, "is a database every cluster already has"))
		}
	}
	return errs
}

type namedSecret struct{ name, what string }

// namedSecrets are the Secrets the spec points at for backups, restores and
// audit logs, which a role's Secret must not overwrite.
func (s *PostgresClusterSpec) namedSecrets() []namedSecret {
	var out []namedSecret
	add := func(name, what string) {
		if name != "" {
			out = append(out, namedSecret{name, what})
		}
	}
	if s.Backup.Destination != nil {
		add(s.Backup.Destination.CredentialsSecret, "the backup credentials")
	}
	if s.Backup.Encryption != nil {
		add(s.Backup.Encryption.KeySecret, "the backup encryption key")
	}
	if recovery := s.Recovery(); recovery != nil {
		add(recovery.Destination.CredentialsSecret, "the restore credentials")
		if recovery.Encryption != nil {
			add(recovery.Encryption.KeySecret, "the restore encryption key")
		}
	}
	if s.Compliance != nil && s.Compliance.AuditLogs.Destination != nil {
		add(s.Compliance.AuditLogs.Destination.CredentialsSecret, "the audit log credentials")
	}
	return out
}
