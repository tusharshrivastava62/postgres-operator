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
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	dbv1alpha1 "github.com/tusharshrivastava62/postgres-operator/api/v1alpha1"
)

const (
	// componentRoleCredentials labels the Secrets that hold an application
	// role's connection details. A Secret without it, and without the
	// cluster's instance label, is not the operator's to change.
	componentRoleCredentials = "role-credentials"

	secretKeyHost   = "host"
	secretKeyPort   = "port"
	secretKeyDBName = "dbname"

	// maxRoleStatuses is the most entries status.accessControl.roles holds.
	maxRoleStatuses = 64

	// rolesVerifyInterval is how long roles found in order are trusted. Reading
	// them back on every reconcile would put the operator's own query in the
	// server log, and the audit log, twice a minute.
	rolesVerifyInterval = 5 * time.Minute
)

// scramPrefix starts every SCRAM-SHA-256 verifier PostgreSQL stores.
const scramPrefix = "SCRAM-SHA-256$"

// rolesObservedSQL reads what the planner needs from the database, as one
// JSON document: every role that is not PostgreSQL's own, with its attributes
// and its comment, and every database that accepts connections. A password
// verifier is read only for the roles the spec names, so the others' never
// reach the operator.
func rolesObservedSQL(names []string) string {
	literals := make([]string, len(names))
	for i, name := range names {
		literals[i] = sqlLiteral(name)
	}
	named := "false"
	if len(literals) > 0 {
		named = "r.rolname in (" + strings.Join(literals, ", ") + ")"
	}
	return `select json_build_object(
  'roles', (select coalesce(json_agg(json_build_object(
      'name', r.rolname,
      'comment', coalesce(pg_catalog.shobj_description(r.oid, 'pg_authid'), ''),
      'login', r.rolcanlogin, 'super', r.rolsuper, 'createdb', r.rolcreatedb, 'createrole', r.rolcreaterole,
      'replication', r.rolreplication, 'bypassrls', r.rolbypassrls, 'limit', r.rolconnlimit,
      'password', case when ` + named + ` then coalesce(a.rolpassword, '') else '' end)), '[]'::json)
    from pg_catalog.pg_roles r join pg_catalog.pg_authid a on a.oid = r.oid
    where r.rolname !~ '^pg_'),
  'databases', (select coalesce(json_agg(json_build_object(
      'name', d.datname, 'owner', pg_catalog.pg_get_userbyid(d.datdba),
      'publicConnect', exists (select from pg_catalog.aclexplode(coalesce(d.datacl, pg_catalog.acldefault('d', d.datdba))) x
                               where x.grantee = 0 and x.privilege_type = 'CONNECT'))), '[]'::json)
    from pg_catalog.pg_database d where d.datallowconn)
)::text`
}

// observedRole is a role as the database has it.
type observedRole struct {
	Name        string `json:"name"`
	Comment     string `json:"comment"`
	Login       bool   `json:"login"`
	Super       bool   `json:"super"`
	CreateDB    bool   `json:"createdb"`
	CreateRole  bool   `json:"createrole"`
	Replication bool   `json:"replication"`
	BypassRLS   bool   `json:"bypassrls"`
	Limit       int32  `json:"limit"`
	Password    string `json:"password"`
}

type observedDatabase struct {
	Name          string `json:"name"`
	Owner         string `json:"owner"`
	PublicConnect bool   `json:"publicConnect"`
}

// observedRoles is what rolesObservedSQL answers with.
type observedRoles struct {
	Roles     []observedRole     `json:"roles"`
	Databases []observedDatabase `json:"databases"`
}

func (o *observedRoles) role(name string) *observedRole {
	for i := range o.Roles {
		if o.Roles[i].Name == name {
			return &o.Roles[i]
		}
	}
	return nil
}

func (o *observedRoles) database(name string) *observedDatabase {
	for i := range o.Databases {
		if o.Databases[i].Name == name {
			return &o.Databases[i]
		}
	}
	return nil
}

// roleOutcome is what to do about one role, and what to say about it.
type roleOutcome struct {
	status dbv1alpha1.RoleStatus
	// statements run in order, each on its own: CREATE DATABASE cannot share
	// a transaction with anything.
	statements []string
}

func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// ownedByOperator reports whether a role carries a comment this operator
// wrote, for this cluster or for the one whose data this cluster was restored
// from. A restored cluster inherits its source's roles, comments included,
// and takes them over.
func ownedByOperator(comment string) bool {
	return strings.HasPrefix(comment, roleMarkerPrefix)
}

// verifierMatches reports whether stored is the SCRAM-SHA-256 verifier of
// password, by hashing the password with the salt and iteration count stored.
// Anything else stored, such as an md5 hash, or nothing, does not match.
func verifierMatches(stored, password string) bool {
	rest, ok := strings.CutPrefix(stored, scramPrefix)
	if !ok {
		return false
	}
	params, _, ok := strings.Cut(rest, "$")
	if !ok {
		return false
	}
	iterations, salt, ok := strings.Cut(params, ":")
	if !ok {
		return false
	}
	count, err := strconv.Atoi(iterations)
	if err != nil || count < 1 {
		return false
	}
	raw, err := base64.StdEncoding.DecodeString(salt)
	if err != nil {
		return false
	}
	want, err := scramVerifier(password, raw, count)
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(want), []byte(stored)) == 1
}

// roleAttributes is the ALTER ROLE that leaves a role able to log in and to do
// nothing else.
func roleAttributes(role *dbv1alpha1.RoleSpec) string {
	limit := int32(-1)
	if role.ConnectionLimit != nil {
		limit = *role.ConnectionLimit
	}
	return fmt.Sprintf("alter role %s with login nosuperuser nocreatedb nocreaterole noreplication nobypassrls inherit connection limit %d",
		quoteIdent(role.Name), limit)
}

func attributesInOrder(have *observedRole, role *dbv1alpha1.RoleSpec) bool {
	limit := int32(-1)
	if role.ConnectionLimit != nil {
		limit = *role.ConnectionLimit
	}
	return have.Login && !have.Super && !have.CreateDB && !have.CreateRole && !have.Replication && !have.BypassRLS && have.Limit == limit
}

// planRole works out what a role in the spec needs. password is the one in its
// Secret; problem, if set, is why the role cannot be set up at all.
func planRole(cluster *dbv1alpha1.PostgresCluster, observed *observedRoles, role *dbv1alpha1.RoleSpec, password, problem string) roleOutcome {
	status := dbv1alpha1.RoleStatus{Name: role.Name, State: dbv1alpha1.RoleReady, SecretName: role.RoleSecretName(cluster.Name)}
	fail := func(message string) roleOutcome {
		status.State, status.Message = dbv1alpha1.RoleError, message
		return roleOutcome{status: status}
	}
	if problem != "" {
		return fail(problem)
	}
	if password == "" {
		return fail("the Secret " + status.SecretName + " has no password yet")
	}

	var statements []string
	have := observed.role(role.Name)
	switch {
	case have == nil:
		verifier, err := newScramVerifier(password)
		if err != nil {
			return fail(err.Error())
		}
		statements = append(statements,
			"create role "+quoteIdent(role.Name),
			roleAttributes(role)+" password "+sqlLiteral(verifier),
			"comment on role "+quoteIdent(role.Name)+" is "+sqlLiteral(roleMarker(cluster)))
	case !ownedByOperator(have.Comment):
		return fail("a role named " + role.Name + " already exists and was not made by this operator, so it is left alone: " +
			"choose another name, or drop that role")
	default:
		passwordRight := verifierMatches(have.Password, password)
		if !passwordRight || !attributesInOrder(have, role) {
			alter := roleAttributes(role)
			if !passwordRight {
				verifier, err := newScramVerifier(password)
				if err != nil {
					return fail(err.Error())
				}
				alter += " password " + sqlLiteral(verifier)
			}
			statements = append(statements, alter)
		}
		if have.Comment != roleMarker(cluster) {
			statements = append(statements, "comment on role "+quoteIdent(role.Name)+" is "+sqlLiteral(roleMarker(cluster)))
		}
	}

	if role.Database != "" {
		db := observed.database(role.Database)
		switch {
		case db == nil:
			statements = append(statements,
				"create database "+quoteIdent(role.Database)+" owner "+quoteIdent(role.Name),
				"revoke connect on database "+quoteIdent(role.Database)+" from public")
		case db.Owner != role.Name:
			// The role is set up; the database is somebody else's.
			outcome := fail("database " + role.Database + " already exists and is owned by " + db.Owner +
				", so it is left alone: choose another name, or change its owner yourself")
			outcome.statements = statements
			return outcome
		case db.PublicConnect:
			statements = append(statements, "revoke connect on database "+quoteIdent(role.Database)+" from public")
		}
	}
	return roleOutcome{status: status, statements: statements}
}

// planDisabled turns off the login of every role the operator made that the
// spec no longer lists. Nothing is dropped: a role may own tables, and
// PostgreSQL would refuse to drop it while it does.
func planDisabled(observed *observedRoles, listed map[string]bool) []roleOutcome {
	var out []roleOutcome
	for i := range observed.Roles {
		have := &observed.Roles[i]
		if listed[have.Name] || have.Name == monitorRoleName || !ownedByOperator(have.Comment) {
			continue
		}
		outcome := roleOutcome{status: dbv1alpha1.RoleStatus{
			Name: have.Name, State: dbv1alpha1.RoleDisabled,
			Message: "removed from spec.roles: login is disabled; the role and anything it owns are kept, drop them yourself when you are sure",
		}}
		if have.Login {
			outcome.statements = []string{
				"alter role " + quoteIdent(have.Name) + " nologin",
				"select pg_catalog.pg_terminate_backend(pid) from pg_catalog.pg_stat_activity where usename = " + sqlLiteral(have.Name),
			}
		}
		out = append(out, outcome)
	}
	slices.SortFunc(out, func(a, b roleOutcome) int { return strings.Compare(a.status.Name, b.status.Name) })
	return out
}

// planRoles is everything the operator will do to the database's roles and
// databases, and the status it will report, in the order it is reported: the
// spec's roles, then the ones that were removed from it.
func planRoles(cluster *dbv1alpha1.PostgresCluster, observed *observedRoles, passwords, problems map[string]string) []roleOutcome {
	listed := map[string]bool{}
	var out []roleOutcome
	for i := range cluster.Spec.Roles {
		role := &cluster.Spec.Roles[i]
		listed[role.Name] = true
		out = append(out, planRole(cluster, observed, role, passwords[role.Name], problems[role.Name]))
	}
	return append(out, planDisabled(observed, listed)...)
}

// roleSecretData is what a role's Secret says besides the password.
func roleSecretData(cluster *dbv1alpha1.PostgresCluster, role *dbv1alpha1.RoleSpec) map[string][]byte {
	data := map[string][]byte{
		secretKeyUsername: []byte(role.Name),
		secretKeyHost:     []byte(cluster.Name + "." + cluster.Namespace + ".svc"),
		secretKeyPort:     []byte(strconv.Itoa(int(postgresPort))),
	}
	if role.Database != "" {
		data[secretKeyDBName] = []byte(role.Database)
	}
	return data
}

func roleSecretLabels(cluster *dbv1alpha1.PostgresCluster) map[string]string {
	labels := labelsForCluster(cluster)
	labels[labelAppComponent] = componentRoleCredentials
	return labels
}

func roleSecretManaged(cluster *dbv1alpha1.PostgresCluster, secret *corev1.Secret) bool {
	return secret.Labels[labelAppInstance] == cluster.Name && secret.Labels[labelAppComponent] == componentRoleCredentials
}

// reconcileRoleSecrets makes sure each role has a Secret with its connection
// details and a password. It returns the roles whose Secret it could not use,
// and why: a Secret of that name that the operator did not make is left alone.
//
// Like the cluster's own credentials Secret, these are not owned by the
// cluster. The password is in the database, which outlives the cluster with
// its volumes. A password that is missing is generated again, and the role is
// set to it.
func (r *PostgresClusterReconciler) reconcileRoleSecrets(ctx context.Context, cluster *dbv1alpha1.PostgresCluster) (map[string]string, error) {
	problems := map[string]string{}
	for i := range cluster.Spec.Roles {
		role := &cluster.Spec.Roles[i]
		name := role.RoleSecretName(cluster.Name)
		secret := &corev1.Secret{}
		err := r.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: name}, secret)
		if err != nil && !apierrors.IsNotFound(err) {
			return nil, err
		}
		create := apierrors.IsNotFound(err)
		if !create && !roleSecretManaged(cluster, secret) {
			problems[role.Name] = "the Secret " + name + " exists and was not made by this operator, so it is left alone: " +
				"delete it, or choose another secretName"
			continue
		}
		if create {
			secret = &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: cluster.Namespace, Labels: roleSecretLabels(cluster)},
				Type:       corev1.SecretTypeOpaque,
			}
		}
		patch := client.MergeFrom(secret.DeepCopy())
		changed := create
		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		want := roleSecretData(cluster, role)
		for key, value := range want {
			if string(secret.Data[key]) != string(value) {
				secret.Data[key] = value
				changed = true
			}
		}
		if _, has := want[secretKeyDBName]; !has {
			if _, stale := secret.Data[secretKeyDBName]; stale {
				delete(secret.Data, secretKeyDBName)
				changed = true
			}
		}
		if len(secret.Data[secretKeyPassword]) == 0 {
			password, err := generatePassword()
			if err != nil {
				return nil, fmt.Errorf("generating the password for role %s: %w", role.Name, err)
			}
			secret.Data[secretKeyPassword] = []byte(password)
			changed = true
		}
		switch {
		case create:
			err = r.Create(ctx, secret)
		case changed:
			err = r.Patch(ctx, secret, patch)
		}
		if err != nil {
			return nil, fmt.Errorf("the Secret for role %s: %w", role.Name, err)
		}
	}
	return problems, nil
}

// rolesTrust remembers that roles were found in order, and for which inputs.
type rolesTrust struct {
	inputs string
	at     time.Time
}

// rolesWanted reports whether there is anything to do about roles. The spec
// may list some. Roles removed from it are still listed in status until they
// are dropped. And a cluster restored from backups holds the roles of the
// cluster it came from, with that cluster's passwords, so it must look even if
// its own spec lists none: whatever is not in the spec cannot log in.
func rolesWanted(cluster *dbv1alpha1.PostgresCluster) bool {
	return len(cluster.Spec.Roles) > 0 || len(cluster.Status.AccessControl.Roles) > 0 || cluster.Spec.Recovery() != nil
}

// reconcileRoles makes the database's roles and databases match spec.roles,
// and records in status how that went. It reports whether every role is ready.
//
// The operator connects to the primary as the superuser, reads the roles back,
// and changes only what differs, so a cluster in order costs one read every few
// minutes. The superuser is used because creating roles and databases needs it.
// A role that cannot be set up is reported in status and is not an error of the
// reconcile.
func (r *PostgresClusterReconciler) reconcileRoles(
	ctx context.Context, cluster *dbv1alpha1.PostgresCluster, secretProblems map[string]string,
) (bool, error) {
	if r.SQL == nil || !rolesWanted(cluster) {
		return true, nil
	}
	key := cluster.Namespace + "/" + cluster.Name
	now := time.Now()

	held, err := r.readRoleSecrets(ctx, cluster, secretProblems, now)
	if err != nil {
		return false, err
	}
	inputs := strconv.FormatInt(cluster.Generation, 10) + "|" + strings.Join(held.versions, ",") + "|" + fmt.Sprint(len(secretProblems))
	if trusted, ok := r.rolesVerified.Load(key); ok && len(held.rotating) == 0 {
		t := trusted.(rolesTrust)
		if t.inputs == inputs && time.Since(t.at) < rolesVerifyInterval && rolesSettled(cluster) {
			return true, nil
		}
	}

	_, address, err := r.leaderAddress(ctx, cluster)
	if err != nil {
		return false, err
	}
	fail := func(state, message string) (bool, error) {
		return false, r.setRoleStatuses(ctx, cluster, r.allSpecRoles(cluster, state, message))
	}
	if address == "" {
		return fail(dbv1alpha1.RolePending, "the cluster has no primary yet")
	}
	target, err := r.superuserTarget(ctx, cluster, address)
	if err != nil {
		return fail(dbv1alpha1.RoleError, err.Error())
	}
	session, err := r.SQL.Dial(ctx, target)
	if err != nil {
		return fail(dbv1alpha1.RoleError, fmt.Sprintf("could not connect to the primary on port %d: %v", postgresPort, err))
	}
	defer session.Close(ctx)

	observed, err := readObservedRoles(ctx, session, cluster)
	if err != nil {
		return fail(dbv1alpha1.RoleError, "could not read the roles: "+err.Error())
	}
	outcomes := planRoles(cluster, observed, held.passwords, secretProblems)
	statuses, ready, err := r.applyRoleOutcomes(ctx, session, outcomes, held, now)
	if err != nil {
		return false, err
	}
	if ready {
		r.rolesVerified.Store(key, rolesTrust{inputs: inputs, at: time.Now()})
	} else {
		r.rolesVerified.Delete(key)
	}
	return ready, r.setRoleStatuses(ctx, cluster, statuses)
}

// roleSecrets is what the operator holds for the roles in the spec: their
// Secrets, the passwords in them, and the new password of each role whose
// password is due to be replaced.
type roleSecrets struct {
	passwords map[string]string
	secrets   map[string]*corev1.Secret
	// rotating holds the new password of each role due for one. It is set in
	// the database and only then written to the Secret.
	rotating map[string]string
	versions []string
}

// readRoleSecrets reads each role's Secret and works out which are due for a
// new password.
func (r *PostgresClusterReconciler) readRoleSecrets(
	ctx context.Context, cluster *dbv1alpha1.PostgresCluster, problems map[string]string, now time.Time,
) (*roleSecrets, error) {
	held := &roleSecrets{
		passwords: map[string]string{}, secrets: map[string]*corev1.Secret{}, rotating: map[string]string{},
		versions: make([]string, 0, len(cluster.Spec.Roles)),
	}
	for i := range cluster.Spec.Roles {
		role := &cluster.Spec.Roles[i]
		secret := &corev1.Secret{}
		err := r.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: role.RoleSecretName(cluster.Name)}, secret)
		switch {
		case apierrors.IsNotFound(err):
			// Only for a Secret that was not made because it clashed.
			continue
		case err != nil:
			return nil, err
		}
		held.secrets[role.Name] = secret
		held.passwords[role.Name] = string(secret.Data[secretKeyPassword])
		held.versions = append(held.versions, role.Name+"="+secret.ResourceVersion)
		if due, _ := rotationDue(secret, role.RotateEvery, now); due && problems[role.Name] == "" && len(secret.Data[secretKeyPassword]) > 0 {
			fresh, err := generatePassword()
			if err != nil {
				return nil, err
			}
			held.rotating[role.Name] = fresh
			held.passwords[role.Name] = fresh
		}
	}
	return held, nil
}

// readObservedRoles asks the database what the planner needs to know.
func readObservedRoles(ctx context.Context, session SQLSession, cluster *dbv1alpha1.PostgresCluster) (*observedRoles, error) {
	names := make([]string, 0, len(cluster.Spec.Roles))
	for i := range cluster.Spec.Roles {
		names = append(names, cluster.Spec.Roles[i].Name)
	}
	answer, err := session.QueryText(ctx, rolesObservedSQL(names))
	if err != nil {
		return nil, err
	}
	observed := &observedRoles{}
	if err := json.Unmarshal([]byte(answer), observed); err != nil {
		return nil, err
	}
	return observed, nil
}

// applyRoleOutcomes runs the statements of each outcome, writes the new
// password of a role that was replaced to its Secret once the database has it,
// and returns the status of every role and whether all are fine.
func (r *PostgresClusterReconciler) applyRoleOutcomes(
	ctx context.Context, session SQLSession, outcomes []roleOutcome, held *roleSecrets, now time.Time,
) ([]dbv1alpha1.RoleStatus, bool, error) {
	log := logf.FromContext(ctx)
	statuses := make([]dbv1alpha1.RoleStatus, 0, len(outcomes))
	ready := true
	for i := range outcomes {
		outcome := &outcomes[i]
		applied := true
		for _, statement := range outcome.statements {
			if err := session.Exec(ctx, statement); err != nil {
				// The statement can hold a verifier; the error does not.
				outcome.status.State, outcome.status.Message = dbv1alpha1.RoleError, "could not apply the change: "+err.Error()
				applied = false
				break
			}
		}
		if len(outcome.statements) > 0 && outcome.status.State == dbv1alpha1.RoleReady {
			log.Info("Updated database role", "role", outcome.status.Name, "statements", len(outcome.statements))
		}
		if secret, ok := held.secrets[outcome.status.Name]; ok {
			rotated := lastRotated(secret)
			if fresh, isRotating := held.rotating[outcome.status.Name]; isRotating && applied && len(outcome.statements) > 0 {
				// The database has the new password; now the Secret does.
				if err := r.patchSecret(ctx, secret, func(s *corev1.Secret) {
					s.Data[secretKeyPassword] = []byte(fresh)
					stampRotation(s, now)
				}); err != nil {
					return nil, false, fmt.Errorf("writing the new password of role %s: %w", outcome.status.Name, err)
				}
				log.Info("Replaced a role's password", "role", outcome.status.Name)
				rotated = now
			}
			if outcome.status.State != dbv1alpha1.RoleDisabled {
				outcome.status.LastRotated = timePtr(rotated)
			}
		}
		ready = ready && outcome.status.State != dbv1alpha1.RoleError
		statuses = append(statuses, outcome.status)
	}
	if len(statuses) > maxRoleStatuses {
		statuses = statuses[:maxRoleStatuses]
	}
	return statuses, ready, nil
}

// rolesSettled reports whether status shows every role in order, so that
// trusting the last look is right.
func rolesSettled(cluster *dbv1alpha1.PostgresCluster) bool {
	for _, role := range cluster.Status.AccessControl.Roles {
		if role.State == dbv1alpha1.RoleError || role.State == dbv1alpha1.RolePending {
			return false
		}
	}
	return len(cluster.Status.AccessControl.Roles) >= len(cluster.Spec.Roles)
}

// allSpecRoles is a status for every role in the spec, all the same, keeping
// the roles that were already removed as they were.
func (r *PostgresClusterReconciler) allSpecRoles(cluster *dbv1alpha1.PostgresCluster, state, message string) []dbv1alpha1.RoleStatus {
	statuses := make([]dbv1alpha1.RoleStatus, 0, len(cluster.Spec.Roles))
	listed := map[string]bool{}
	for i := range cluster.Spec.Roles {
		role := &cluster.Spec.Roles[i]
		listed[role.Name] = true
		statuses = append(statuses, dbv1alpha1.RoleStatus{
			Name: role.Name, State: state, SecretName: role.RoleSecretName(cluster.Name), Message: message,
		})
	}
	for _, before := range cluster.Status.AccessControl.Roles {
		if before.State == dbv1alpha1.RoleDisabled && !listed[before.Name] {
			statuses = append(statuses, before)
		}
	}
	return statuses
}

// superuserTarget is how to connect to the primary as the superuser.
func (r *PostgresClusterReconciler) superuserTarget(ctx context.Context, cluster *dbv1alpha1.PostgresCluster, address string) (SQLTarget, error) {
	creds := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: credentialsSecretName(cluster)}, creds); err != nil {
		return SQLTarget{}, fmt.Errorf("reading the superuser credentials: %w", err)
	}
	return SQLTarget{
		Host: address, Port: postgresPort, Database: monitorDatabase,
		User: string(creds.Data[secretKeyUsername]), Password: string(creds.Data[secretKeyPassword]),
	}, nil
}

// setRoleStatuses records the roles' states and the RolesReady condition,
// writing status only if something changed.
func (r *PostgresClusterReconciler) setRoleStatuses(ctx context.Context, cluster *dbv1alpha1.PostgresCluster, statuses []dbv1alpha1.RoleStatus) error {
	if len(statuses) == 0 {
		statuses = nil
	}
	changed := !equality.Semantic.DeepEqual(cluster.Status.AccessControl.Roles, statuses)
	cluster.Status.AccessControl.Roles = statuses

	if len(cluster.Spec.Roles) == 0 {
		changed = meta.RemoveStatusCondition(&cluster.Status.Conditions, dbv1alpha1.ConditionRolesReady) || changed
	} else {
		changed = meta.SetStatusCondition(&cluster.Status.Conditions, rolesCondition(cluster, statuses)) || changed
	}
	recordRoleMetrics(cluster, statuses)
	if !changed {
		return nil
	}
	return r.Status().Update(ctx, cluster)
}

// rolesCondition says whether every role in the spec is ready, and names the
// ones that are not.
func rolesCondition(cluster *dbv1alpha1.PostgresCluster, statuses []dbv1alpha1.RoleStatus) metav1.Condition {
	inSpec := map[string]bool{}
	for i := range cluster.Spec.Roles {
		inSpec[cluster.Spec.Roles[i].Name] = true
	}
	var notReady []string
	var first string
	for _, status := range statuses {
		if inSpec[status.Name] && status.State != dbv1alpha1.RoleReady {
			notReady = append(notReady, status.Name)
			if first == "" {
				first = status.Name + ": " + status.Message
			}
		}
	}
	if len(notReady) == 0 {
		return metav1.Condition{
			Type: dbv1alpha1.ConditionRolesReady, Status: metav1.ConditionTrue, Reason: "AllRolesReady",
			Message: fmt.Sprintf("%d roles are ready", len(cluster.Spec.Roles)),
		}
	}
	return metav1.Condition{
		Type: dbv1alpha1.ConditionRolesReady, Status: metav1.ConditionFalse, Reason: "RolesNotReady",
		Message: fmt.Sprintf("%d of %d roles are not ready (%s); first: %s",
			len(notReady), len(cluster.Spec.Roles), strings.Join(notReady, ", "), first),
	}
}

// recordRoleMetrics publishes how many roles are in each state, or drops the
// series when the cluster has no roles.
func recordRoleMetrics(cluster *dbv1alpha1.PostgresCluster, statuses []dbv1alpha1.RoleStatus) {
	match := map[string]string{labelNamespace: cluster.Namespace, labelName: cluster.Name}
	managedRoles.DeletePartialMatch(match)
	counts := map[string]int{}
	for _, status := range statuses {
		counts[status.State]++
	}
	for _, state := range []string{dbv1alpha1.RoleReady, dbv1alpha1.RolePending, dbv1alpha1.RoleError, dbv1alpha1.RoleDisabled} {
		if len(statuses) > 0 {
			managedRoles.WithLabelValues(cluster.Namespace, cluster.Name, state).Set(float64(counts[state]))
		}
	}
}
