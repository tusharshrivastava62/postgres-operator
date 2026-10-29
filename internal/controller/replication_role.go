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
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	dbv1alpha1 "github.com/tusharshrivastava62/postgres-operator/api/v1alpha1"
)

const (
	// rewindRoleName is the login Patroni runs pg_rewind as.
	rewindRoleName = "pgoperator_rewind"

	// The password lives in the credentials Secret with the superuser's and the
	// replication role's, and is replaced with them.
	secretKeyRewind     = "rewind-password"
	secretKeyRewindNext = "rewind-password-next"

	// envRewindPassword carries the password into the pod; see
	// envRESTAPIPassword.
	envRewindPassword = "REWIND_PASSWORD"

	// rewindConnectionLimit is more than Patroni ever uses at once.
	rewindConnectionLimit = 3
)

// restrictsReplication is whether the cluster limits the replication role.
func restrictsReplication(cluster *dbv1alpha1.PostgresCluster) bool {
	return cluster.Spec.Security.RestrictsReplicationRole()
}

// restrictedPgHBA is the server's rules for who may connect, as the image writes
// them, with one more: the replication role is refused everything that is not
// replication. The rules are matched in order, and the image's last one lets any
// role in over TLS with a password, which is how the replication role can log in
// as an ordinary user. The rule before it closes that. The image's rules for a
// group role it never creates, which authenticate through PAM, are left out.
//
// Patroni takes this list as it stands, and only at the pod's start: changing
// the list in its dynamic configuration changes nothing, because the image's
// own copy of the setting is read first.
func restrictedPgHBA() []string {
	return []string{
		"local all all trust",
		"host all all 127.0.0.1/32 md5",
		"host all all ::1/128 md5",
		"local replication " + replicationUser + " trust",
		"hostssl replication " + replicationUser + " all md5",
		"hostnossl all all all reject",
		"hostssl all " + replicationUser + " all reject",
		"hostssl all all all md5",
	}
}

// restrictedPostgresql is the part of SPILO_CONFIGURATION that limits the
// replication role and names the login pg_rewind runs as. The password is not in
// it: the pod's environment variable of that name is, which Kubernetes fills in
// from the Secret.
func restrictedPostgresql() map[string]any {
	return map[string]any{
		"authentication": map[string]any{
			"rewind": map[string]any{
				"username": rewindRoleName,
				"password": "$(" + envRewindPassword + ")",
			},
		},
		"pg_hba": restrictedPgHBA(),
	}
}

// rewindRolePrivileges are the functions pg_rewind calls on the server it
// rewinds from, to list and read its files. They are all it needs.
var rewindRolePrivileges = []string{
	"pg_catalog.pg_ls_dir(text, boolean, boolean)",
	"pg_catalog.pg_stat_file(text, boolean)",
	"pg_catalog.pg_read_binary_file(text)",
	"pg_catalog.pg_read_binary_file(text, bigint, bigint, boolean)",
}

// rewindSetupSQL makes the role, with the password the verifier is of, and gives
// it exactly the privileges pg_rewind needs. It can be run again at any time and
// leaves the same result. The password is set from a SCRAM verifier, never a
// clear-text password: the server logs the statement that sets it.
func rewindSetupSQL(cluster *dbv1alpha1.PostgresCluster, verifier string) string {
	var sql strings.Builder
	fmt.Fprintf(&sql, `
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
`, sqlLiteral(rewindRoleName), rewindRoleName, rewindConnectionLimit, sqlLiteral(verifier), sqlLiteral(roleMarker(cluster)))
	for _, function := range rewindRolePrivileges {
		fmt.Fprintf(&sql, "grant execute on function %s to %s;\n", function, rewindRoleName)
	}
	return sql.String()
}

// rewindVerifySQL is run as the role itself. It is true only if the role is as
// unprivileged as it should be, can call what pg_rewind calls, and is the
// operator's.
func rewindVerifySQL(cluster *dbv1alpha1.PostgresCluster) string {
	var sql strings.Builder
	sql.WriteString(`
select (select not rolsuper and not rolcreatedb and not rolcreaterole and not rolreplication and not rolbypassrls
        from pg_catalog.pg_roles where rolname = current_user)
   and coalesce(pg_catalog.shobj_description((select oid from pg_catalog.pg_roles where rolname = current_user), 'pg_authid')
        = ` + sqlLiteral(roleMarker(cluster)) + `, false)`)
	for _, function := range rewindRolePrivileges {
		sql.WriteString("\n   and pg_catalog.has_function_privilege(" + sqlLiteral(function) + ", 'execute')")
	}
	return sql.String()
}

// rewindDisableSQL takes the login away from the role, if the operator made it.
// It is not dropped: what it owns, and who it was, stay.
const rewindDisableSQL = `
do $$
begin
  if exists (select from pg_catalog.pg_roles where rolname = '` + rewindRoleName + `') then
    alter role ` + rewindRoleName + ` nologin;
  end if;
end
$$;`

// anyPodRewindsAs is whether a pod was started to run pg_rewind as the role, and
// so still needs it.
func (r *PostgresClusterReconciler) anyPodRewindsAs(ctx context.Context, cluster *dbv1alpha1.PostgresCluster) (bool, error) {
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(cluster.Namespace), client.MatchingLabels(podSelectorLabels(cluster))); err != nil {
		return false, err
	}
	for i := range pods.Items {
		if podHasEnv(&pods.Items[i], envRewindPassword) {
			return true, nil
		}
	}
	return false, nil
}

// reconcileRewindRole makes sure the login Patroni runs pg_rewind as exists in
// the database, with the password in the credentials Secret and nothing more,
// and records whether it does. It reports whether it is settled.
//
// It logs in as the role first: if that works and the role is as it should be,
// the superuser is not used at all. When the cluster stops asking for it, the
// login is taken away once no pod is left that was started to use it.
func (r *PostgresClusterReconciler) reconcileRewindRole(ctx context.Context, cluster *dbv1alpha1.PostgresCluster) (bool, error) {
	if r.SQL == nil {
		return true, nil
	}
	if !restrictsReplication(cluster) {
		return r.disableRewindRole(ctx, cluster)
	}
	log := logf.FromContext(ctx)

	secret := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: credentialsSecretName(cluster)}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return false, r.setRewindStatus(ctx, cluster, dbv1alpha1.RolePending, "the credentials Secret does not exist yet")
		}
		return false, err
	}
	leader, address, err := r.leaderAddress(ctx, cluster)
	if err != nil {
		return false, err
	}
	if address == "" {
		return false, r.setRewindStatus(ctx, cluster, dbv1alpha1.RolePending, "the cluster has no primary yet")
	}
	tlsCheck, err := sqlTLSFor(ctx, r.Client, cluster, address)
	if err != nil {
		return false, err
	}
	target := SQLTarget{Host: address, Port: postgresPort, Database: monitorDatabase,
		User: rewindRoleName, Password: string(secret.Data[secretKeyRewind]), TLS: tlsCheck}

	verified, err := r.verifyRewindRole(ctx, cluster, target)
	if err != nil {
		return false, r.setRewindStatus(ctx, cluster, dbv1alpha1.RoleError,
			fmt.Sprintf("could not connect to %s on port %d: %v", leader, postgresPort, err))
	}
	if !verified {
		log.Info("setting up the rewind role", "role", rewindRoleName, "primary", leader)
		if err := r.setUpRewindRole(ctx, cluster, target); err != nil {
			return false, r.setRewindStatus(ctx, cluster, dbv1alpha1.RoleError, "could not set up the role: "+err.Error())
		}
		if verified, err = r.verifyRewindRole(ctx, cluster, target); err != nil || !verified {
			reason := roleLacksPrivileges
			if err != nil {
				reason = err.Error()
			}
			return false, r.setRewindStatus(ctx, cluster, dbv1alpha1.RoleError, "set up the role, but it cannot be used: "+reason)
		}
	}
	return true, r.setRewindStatus(ctx, cluster, dbv1alpha1.RoleReady, "")
}

// verifyRewindRole logs in as the role and asks whether it is as it should be.
// It returns an error only when the database could not be reached at all; a
// refused password, or a role that lacks something, is "not verified".
func (r *PostgresClusterReconciler) verifyRewindRole(
	ctx context.Context, cluster *dbv1alpha1.PostgresCluster, target SQLTarget,
) (bool, error) {
	session, err := r.SQL.Dial(ctx, target)
	if err != nil {
		if sqlRefusedCredentials(err) {
			return false, nil
		}
		return false, err
	}
	defer session.Close(ctx)
	ok, err := session.QueryBool(ctx, rewindVerifySQL(cluster))
	if err != nil {
		return false, nil //nolint:nilerr // the setup puts it right
	}
	return ok, nil
}

// setUpRewindRole runs the setup as the superuser.
func (r *PostgresClusterReconciler) setUpRewindRole(ctx context.Context, cluster *dbv1alpha1.PostgresCluster, role SQLTarget) error {
	verifier, err := newScramVerifier(role.Password)
	if err != nil {
		return err
	}
	superuser, err := r.superuserTarget(ctx, cluster, role.Host)
	if err != nil {
		return err
	}
	session, err := r.SQL.Dial(ctx, superuser)
	if err != nil {
		return fmt.Errorf("connecting as the superuser: %w", err)
	}
	defer session.Close(ctx)
	return session.Exec(ctx, rewindSetupSQL(cluster, verifier))
}

// disableRewindRole takes the login away once the cluster stops asking for the
// role and every pod that was started to use it has been replaced. A cluster
// that never asked has nothing to do.
func (r *PostgresClusterReconciler) disableRewindRole(ctx context.Context, cluster *dbv1alpha1.PostgresCluster) (bool, error) {
	state := cluster.Status.AccessControl.RewindRole.State
	if state == "" || state == dbv1alpha1.RoleDisabled {
		return true, nil
	}
	using, err := r.anyPodRewindsAs(ctx, cluster)
	if err != nil {
		return false, err
	}
	if using {
		return false, nil // the pods that need it are still being replaced
	}
	_, address, err := r.leaderAddress(ctx, cluster)
	if err != nil {
		return false, err
	}
	if address == "" {
		return false, nil
	}
	superuser, err := r.superuserTarget(ctx, cluster, address)
	if err != nil {
		return false, err
	}
	session, err := r.SQL.Dial(ctx, superuser)
	if err != nil {
		return false, r.setRewindStatus(ctx, cluster, dbv1alpha1.RoleError, "could not connect to take the login away: "+err.Error())
	}
	defer session.Close(ctx)
	if err := session.Exec(ctx, rewindDisableSQL); err != nil {
		return false, r.setRewindStatus(ctx, cluster, dbv1alpha1.RoleError, "could not take the login away: "+err.Error())
	}
	logf.FromContext(ctx).Info("Took the login away from the rewind role", "role", rewindRoleName)
	return true, r.setRewindStatus(ctx, cluster, dbv1alpha1.RoleDisabled, "no pod uses it any more")
}

// setRewindStatus records the role's state, writing status only if it changed.
func (r *PostgresClusterReconciler) setRewindStatus(ctx context.Context, cluster *dbv1alpha1.PostgresCluster, state, message string) error {
	if len(message) > monitorMessageLimit {
		message = message[:monitorMessageLimit]
	}
	want := dbv1alpha1.RoleStatus{Name: rewindRoleName, State: state, SecretName: credentialsSecretName(cluster), Message: message}
	secret := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: credentialsSecretName(cluster)}, secret); err == nil {
		want.LastRotated = timePtr(lastRotated(secret))
	}
	if equality.Semantic.DeepEqual(cluster.Status.AccessControl.RewindRole, want) {
		return nil
	}
	cluster.Status.AccessControl.RewindRole = want
	return r.Status().Update(ctx, cluster)
}
