/*
Copyright 2026.

Licensed under the Business Source License 1.1 (the "License"). You may not
use this file except in compliance with the License, a copy of which is in the
LICENSE file at the root of this repository. Production use requires a
commercial license from the Licensor. On the Change Date (2030-10-17) the
work becomes available under the Change License (Apache-2.0).
*/

package controller

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	dbv1alpha1 "github.com/tusharshrivastava62/postgres-operator/api/v1alpha1"
)

const (
	// monitorRoleName is the database role the operator's own jobs connect as.
	// It is reserved: a role of the same name that the operator did not make is
	// not something to adopt.
	monitorRoleName = "pgoperator_monitor"

	// monitorSchema holds the one function the role is allowed to call.
	monitorSchema = "pgoperator"

	// monitorConnectionLimit is how many connections the role may hold at
	// once: the backup status job and the compliance check, with room to spare.
	monitorConnectionLimit = 5

	// monitorDatabase is where the role and its function are made. Spilo
	// creates it, and the jobs connect to it.
	monitorDatabase = "postgres"

	// monitorVerifyInterval is how long a verified monitor role is trusted
	// before the operator logs in as it again. Logging in on every reconcile
	// would fill the server log, and the audit log, with the operator's own
	// connections.
	monitorVerifyInterval = 5 * time.Minute

	// roleMarkerPrefix starts the comment the operator puts on every role it
	// creates, which is how it recognises them.
	roleMarkerPrefix = "Managed by postgres-operator for "

	monitorMessageLimit = 512
)

// monitorRoleWanted reports whether anything runs that needs the role. Both
// jobs that read the database over SQL are optional.
// roleLacksPrivileges is what status says of a role that cannot be used.
const roleLacksPrivileges = "the role does not have the expected privileges"

func monitorRoleWanted(cluster *dbv1alpha1.PostgresCluster) bool {
	return (cluster.Spec.Backup.Enabled && cluster.Spec.Backup.Destination != nil) || cluster.Spec.Compliance != nil
}

func monitorSecretName(cluster *dbv1alpha1.PostgresCluster) string {
	return cluster.Name + "-monitor"
}

// roleMarker is the comment that says a role belongs to this cluster's operator.
func roleMarker(cluster *dbv1alpha1.PostgresCluster) string {
	return roleMarkerPrefix + cluster.Namespace + "/" + cluster.Name
}

// monitorSecretKeyRef points a job's environment at the monitor role's
// credentials. The jobs never see the superuser's.
func monitorSecretKeyRef(cluster *dbv1alpha1.PostgresCluster, key string) *corev1.EnvVarSource {
	return &corev1.EnvVarSource{
		SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: monitorSecretName(cluster)},
			Key:                  key,
		},
	}
}

// monitorSetupSQL makes the role, its one function and its grants. It can be
// run again at any time and leaves the same result.
//
// pg_monitor is not used: it includes pg_read_all_stats, which shows other
// sessions' query text. pg_read_all_settings is enough to read the settings,
// and the two catalog grants are the only other things the checks need.
// pg_authid cannot be read, because it holds password hashes, so what the
// compliance check asks of it goes through a function that only counts.
//
// The password is set from a SCRAM verifier, never a clear-text password:
// the server logs the statement that sets it.
func monitorSetupSQL(cluster *dbv1alpha1.PostgresCluster, verifier string) string {
	return fmt.Sprintf(`
do $$
begin
  if not exists (select from pg_catalog.pg_roles where rolname = %[1]s) then
    create role %[2]s login;
  end if;
end
$$;
alter role %[2]s with login nosuperuser nocreatedb nocreaterole noreplication nobypassrls inherit
  connection limit %[3]d password %[4]s;
comment on role %[2]s is %[5]s;
grant pg_read_all_settings to %[2]s;
create schema if not exists %[6]s authorization postgres;
revoke all on schema %[6]s from public;
create or replace function %[6]s.non_scram_roles() returns bigint
  language sql stable security definer set search_path = pg_catalog, pg_temp
  as $function$
    select count(*) from pg_catalog.pg_authid
    where rolpassword is not null and rolpassword not like 'SCRAM-SHA-256$%%'
  $function$;
revoke all on function %[6]s.non_scram_roles() from public;
grant usage on schema %[6]s to %[2]s;
grant execute on function %[6]s.non_scram_roles() to %[2]s;
grant select on pg_catalog.pg_hba_file_rules to %[2]s;
grant execute on function pg_catalog.pg_hba_file_rules() to %[2]s;
`, sqlLiteral(monitorRoleName), monitorRoleName, monitorConnectionLimit, sqlLiteral(verifier),
		sqlLiteral(roleMarker(cluster)), monitorSchema)
}

// monitorVerifySQL is run as the monitor role itself. It is true only if the
// role is as unprivileged as it should be and can do everything the jobs need.
// A missing function is an error, which counts as not verified.
const monitorVerifySQL = `
select (select not rolsuper and not rolcreatedb and not rolcreaterole and not rolreplication and not rolbypassrls
        from pg_catalog.pg_roles where rolname = current_user)
   and pg_catalog.pg_has_role(current_user, 'pg_read_all_settings', 'usage')
   and pg_catalog.has_table_privilege('pg_catalog.pg_hba_file_rules', 'select')
   and pg_catalog.has_function_privilege('pg_catalog.pg_hba_file_rules()', 'execute')
   and pg_catalog.has_function_privilege('` + monitorSchema + `.non_scram_roles()', 'execute')`

// reconcileMonitorSecret makes sure the monitor role's credentials exist.
//
// Like the cluster's own credentials Secret it is not owned by the cluster:
// the role's password lives in the database, which outlives the cluster with
// its volumes, so a Secret deleted with the cluster would leave a password
// nobody knows. A password that goes missing is replaced, and the role is set
// to match it by reconcileMonitorRole.
func (r *PostgresClusterReconciler) reconcileMonitorSecret(ctx context.Context, cluster *dbv1alpha1.PostgresCluster) error {
	if !monitorRoleWanted(cluster) {
		return nil
	}
	secret := &corev1.Secret{}
	err := r.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: monitorSecretName(cluster)}, secret)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	create := apierrors.IsNotFound(err)
	if create {
		secret = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name: monitorSecretName(cluster), Namespace: cluster.Namespace, Labels: labelsForCluster(cluster),
			},
			Type: corev1.SecretTypeOpaque,
		}
	}
	patch := client.MergeFrom(secret.DeepCopy())
	changed := false
	if secret.Data == nil {
		secret.Data = map[string][]byte{}
	}
	if string(secret.Data[secretKeyUsername]) != monitorRoleName {
		secret.Data[secretKeyUsername] = []byte(monitorRoleName)
		changed = true
	}
	if len(secret.Data[secretKeyPassword]) == 0 {
		password, err := generatePassword()
		if err != nil {
			return fmt.Errorf("generating the monitor password: %w", err)
		}
		secret.Data[secretKeyPassword] = []byte(password)
		changed = true
	}
	switch {
	case create:
		return r.Create(ctx, secret)
	case changed:
		return r.Patch(ctx, secret, patch)
	}
	return nil
}

// monitorTrust remembers that the role was verified, and against which
// version of its Secret, so a changed Secret is checked at once.
type monitorTrust struct {
	secretVersion string
	at            time.Time
}

// reconcileMonitorRole makes sure the role exists in the database with the
// password in its Secret, and records whether it does in status. It reports
// whether the role is ready.
//
// The operator logs in as the role first. If that works and the role has what
// it should, nothing else is needed, and the superuser is not used at all. Only
// when the role is missing, has the wrong password or has drifted does the
// operator connect as the superuser to put it right.
//
// A role that cannot be set up is reported in status and is not an error of
// the reconcile: the rest of the cluster does not depend on it, and the jobs
// that do simply fail until it is.
func (r *PostgresClusterReconciler) reconcileMonitorRole(ctx context.Context, cluster *dbv1alpha1.PostgresCluster) (bool, error) {
	if r.SQL == nil || !monitorRoleWanted(cluster) {
		return true, nil
	}
	log := logf.FromContext(ctx)
	key := cluster.Namespace + "/" + cluster.Name

	secret := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: monitorSecretName(cluster)}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return false, r.setMonitorStatus(ctx, cluster, dbv1alpha1.RolePending, "the monitor Secret does not exist yet", time.Time{})
		}
		return false, err
	}
	now := time.Now()
	due, _ := rotationDue(secret, rotationSpec(cluster).MonitorEvery, now)
	rotated := lastRotated(secret)
	if trusted, ok := r.monitorVerified.Load(key); ok && !due {
		t := trusted.(monitorTrust)
		if t.secretVersion == secret.ResourceVersion && time.Since(t.at) < monitorVerifyInterval &&
			cluster.Status.AccessControl.MonitorRole.State == dbv1alpha1.RoleReady {
			return true, nil
		}
	}

	leader, address, err := r.leaderAddress(ctx, cluster)
	if err != nil {
		return false, err
	}
	if address == "" {
		return false, r.setMonitorStatus(ctx, cluster, dbv1alpha1.RolePending, "the cluster has no primary yet", rotated)
	}
	tlsCheck, err := sqlTLSFor(ctx, r.Client, cluster, address)
	if err != nil {
		return false, err
	}
	target := SQLTarget{Host: address, Port: postgresPort, Database: monitorDatabase,
		User: monitorRoleName, Password: string(secret.Data[secretKeyPassword]), TLS: tlsCheck}

	if due {
		return r.rotateMonitorPassword(ctx, cluster, secret, target, now)
	}

	verified, err := r.verifyMonitorRole(ctx, target)
	if err != nil {
		return false, r.setMonitorStatus(ctx, cluster, dbv1alpha1.RoleError,
			fmt.Sprintf("could not connect to %s on port %d: %v", leader, postgresPort, err), rotated)
	}
	if !verified {
		log.Info("setting up the monitor role", "role", monitorRoleName, "primary", leader)
		if err := r.setUpMonitorRole(ctx, cluster, target); err != nil {
			return false, r.setMonitorStatus(ctx, cluster, dbv1alpha1.RoleError, "could not set up the role: "+err.Error(), rotated)
		}
		if verified, err = r.verifyMonitorRole(ctx, target); err != nil || !verified {
			reason := roleLacksPrivileges
			if err != nil {
				reason = err.Error()
			}
			return false, r.setMonitorStatus(ctx, cluster, dbv1alpha1.RoleError, "set up the role, but it cannot be used: "+reason, rotated)
		}
	}
	r.monitorVerified.Store(key, monitorTrust{secretVersion: secret.ResourceVersion, at: time.Now()})
	return true, r.setMonitorStatus(ctx, cluster, dbv1alpha1.RoleReady, "", rotated)
}

// rotateMonitorPassword replaces the monitor role's password: the database
// first, checked by logging in with it, then the Secret. The jobs read the
// Secret each time they run, so a job that started a moment before may fail
// once, and the next run is right.
func (r *PostgresClusterReconciler) rotateMonitorPassword(
	ctx context.Context, cluster *dbv1alpha1.PostgresCluster, secret *corev1.Secret, target SQLTarget, now time.Time,
) (bool, error) {
	rotated := lastRotated(secret)
	fresh, err := generatePassword()
	if err != nil {
		return false, err
	}
	target.Password = fresh
	if err := r.setUpMonitorRole(ctx, cluster, target); err != nil {
		return false, r.setMonitorStatus(ctx, cluster, dbv1alpha1.RoleError, "could not replace the password: "+err.Error(), rotated)
	}
	verified, err := r.verifyMonitorRole(ctx, target)
	if err != nil || !verified {
		reason := roleLacksPrivileges
		if err != nil {
			reason = err.Error()
		}
		return false, r.setMonitorStatus(ctx, cluster, dbv1alpha1.RoleError, "replaced the password, but the role cannot be used: "+reason, rotated)
	}
	if err := r.patchSecret(ctx, secret, func(s *corev1.Secret) {
		s.Data[secretKeyPassword] = []byte(fresh)
		stampRotation(s, now)
	}); err != nil {
		return false, fmt.Errorf("writing the new monitor password: %w", err)
	}
	logf.FromContext(ctx).Info("Replaced the monitor role's password")
	r.monitorVerified.Store(cluster.Namespace+"/"+cluster.Name, monitorTrust{secretVersion: secret.ResourceVersion, at: now})
	return true, r.setMonitorStatus(ctx, cluster, dbv1alpha1.RoleReady, "", now)
}

// verifyMonitorRole logs in as the role and asks whether it is as it should be.
// It returns an error only when the database could not be reached at all; a
// refused password, or a role that lacks something, is "not verified".
func (r *PostgresClusterReconciler) verifyMonitorRole(ctx context.Context, target SQLTarget) (bool, error) {
	session, err := r.SQL.Dial(ctx, target)
	if err != nil {
		if sqlRefusedCredentials(err) {
			return false, nil
		}
		return false, err
	}
	defer session.Close(ctx)
	ok, err := session.QueryBool(ctx, monitorVerifySQL)
	if err != nil {
		// The function is missing, or something else about the role is.
		return false, nil //nolint:nilerr // the setup puts it right
	}
	return ok, nil
}

// setUpMonitorRole runs the setup as the superuser.
func (r *PostgresClusterReconciler) setUpMonitorRole(ctx context.Context, cluster *dbv1alpha1.PostgresCluster, monitor SQLTarget) error {
	verifier, err := newScramVerifier(monitor.Password)
	if err != nil {
		return err
	}
	superuser, err := r.superuserTarget(ctx, cluster, monitor.Host)
	if err != nil {
		return err
	}
	session, err := r.SQL.Dial(ctx, superuser)
	if err != nil {
		return fmt.Errorf("connecting as the superuser: %w", err)
	}
	defer session.Close(ctx)
	return session.Exec(ctx, monitorSetupSQL(cluster, verifier))
}

// setMonitorStatus records the role's state, writing status only if it changed.
func (r *PostgresClusterReconciler) setMonitorStatus(
	ctx context.Context, cluster *dbv1alpha1.PostgresCluster, state, message string, rotated time.Time,
) error {
	if len(message) > monitorMessageLimit {
		message = message[:monitorMessageLimit]
	}
	want := dbv1alpha1.RoleStatus{Name: monitorRoleName, State: state, SecretName: monitorSecretName(cluster), Message: message}
	if !rotated.IsZero() {
		want.LastRotated = timePtr(rotated)
	}
	if equality.Semantic.DeepEqual(cluster.Status.AccessControl.MonitorRole, want) {
		return nil
	}
	cluster.Status.AccessControl.MonitorRole = want
	return r.Status().Update(ctx, cluster)
}
