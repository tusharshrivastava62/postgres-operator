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
	"encoding/json"
	"strings"
	"testing"

	dbv1alpha1 "github.com/tusharshrivastava62/postgres-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	appRole         = "app"
	appOrdersRole   = "app_orders"
	keptRole        = "kept"
	firstRole       = "first"
	secondRole      = "second"
	passwordClause  = "password 'SCRAM"
	passwordWord    = "password"
	testClusterName = "orders"
	ordersDatabase  = testClusterName
	testNamespace   = "prod"
	testPassword    = "a-password-the-secret-holds-0123456789"
)

func rolesCluster(roles ...dbv1alpha1.RoleSpec) *dbv1alpha1.PostgresCluster {
	return &dbv1alpha1.PostgresCluster{
		ObjectMeta: metav1.ObjectMeta{Name: testClusterName, Namespace: testNamespace},
		Spec:       dbv1alpha1.PostgresClusterSpec{Roles: roles},
	}
}

// goodRole is a role the way the operator leaves one: able to log in, nothing
// more, marked as the cluster's, with the password testPassword.
func goodRole(t *testing.T, name string) observedRole {
	t.Helper()
	verifier, err := newScramVerifier(testPassword)
	if err != nil {
		t.Fatal(err)
	}
	return observedRole{
		Name: name, Comment: roleMarker(rolesCluster()), Login: true, Limit: -1, Password: verifier,
	}
}

func outcomeFor(t *testing.T, outcomes []roleOutcome, name string) roleOutcome {
	t.Helper()
	for _, o := range outcomes {
		if o.status.Name == name {
			return o
		}
	}
	t.Fatalf("no outcome for role %s in %v", name, outcomes)
	return roleOutcome{}
}

func joined(statements []string) string { return strings.Join(statements, "\n") }

func TestVerifierMatchesOnlyTheVerifierOfThatPassword(t *testing.T) {
	stored, _ := newScramVerifier(testPassword)
	if !verifierMatches(stored, testPassword) {
		t.Error("the verifier of a password does not match it")
	}
	if verifierMatches(stored, testPassword+"x") {
		t.Error("a different password matches")
	}
	other, _ := newScramVerifier("another")
	if verifierMatches(other, testPassword) {
		t.Error("another password's verifier matches")
	}
	// It uses the salt and the iteration count that were stored, so a verifier
	// made with other parameters still matches its own password.
	salted, _ := scramVerifier(testPassword, []byte("0123456789abcdef"), 1000)
	if !verifierMatches(salted, testPassword) {
		t.Error("a verifier with another salt and iteration count does not match")
	}
	for _, bad := range []string{
		"", "md5" + strings.Repeat("a", 32), testPassword,
		"SCRAM-SHA-256$", "SCRAM-SHA-256$4096", "SCRAM-SHA-256$x:AAAA$a:b", "SCRAM-SHA-256$0:AAAA$a:b",
		"SCRAM-SHA-256$4096:!!!$a:b", "SCRAM-SHA-256$4096:AAAA",
	} {
		if verifierMatches(bad, testPassword) {
			t.Errorf("%q matches", bad)
		}
	}
}

func TestPlanCreatesAMissingRoleWithAVerifierNotThePassword(t *testing.T) {
	limit := int32(20)
	cluster := rolesCluster(dbv1alpha1.RoleSpec{Name: appOrdersRole, Database: ordersDatabase, ConnectionLimit: &limit})
	out := planRoles(cluster, &observedRoles{}, map[string]string{appOrdersRole: testPassword}, nil)
	if len(out) != 1 {
		t.Fatalf("outcomes = %v", out)
	}
	got := out[0]
	if got.status.State != dbv1alpha1.RoleReady || got.status.SecretName != "orders-role-app-orders" {
		t.Errorf("status = %+v", got.status)
	}
	want := []string{
		`create role "app_orders"`,
		`alter role "app_orders" with login nosuperuser nocreatedb nocreaterole noreplication nobypassrls inherit connection limit 20 password 'SCRAM-SHA-256$`,
		`comment on role "app_orders" is 'Managed by postgres-operator for prod/orders'`,
		`create database "orders" owner "app_orders"`,
		`revoke connect on database "orders" from public`,
	}
	if len(got.statements) != len(want) {
		t.Fatalf("statements = %v", got.statements)
	}
	for i, prefix := range want {
		if !strings.HasPrefix(got.statements[i], prefix) {
			t.Errorf("statement %d = %q, want it to start %q", i, got.statements[i], prefix)
		}
	}
	if strings.Contains(joined(got.statements), testPassword) {
		t.Error("the password is in a statement")
	}
	if !strings.Contains(joined(got.statements), "SCRAM-SHA-256$4096:") {
		t.Error("no SCRAM verifier in the statements")
	}
}

func TestPlanGivesAnUnlimitedRoleNoLimit(t *testing.T) {
	out := planRoles(rolesCluster(dbv1alpha1.RoleSpec{Name: appRole}), &observedRoles{}, map[string]string{appRole: testPassword}, nil)
	if !strings.Contains(joined(out[0].statements), "connection limit -1 ") {
		t.Errorf("statements = %v", out[0].statements)
	}
}

func TestPlanLeavesARoleInOrderAlone(t *testing.T) {
	cluster := rolesCluster(dbv1alpha1.RoleSpec{Name: appRole})
	observed := &observedRoles{Roles: []observedRole{goodRole(t, appRole)}}
	out := planRoles(cluster, observed, map[string]string{appRole: testPassword}, nil)
	if len(out[0].statements) != 0 || out[0].status.State != dbv1alpha1.RoleReady {
		t.Errorf("outcome = %+v", out[0])
	}
}

func TestPlanPutsRightWhatDrifted(t *testing.T) {
	limit := int32(5)
	tests := []struct {
		name     string
		spec     dbv1alpha1.RoleSpec
		mutate   func(*observedRole)
		password string
		want     []string // substrings that must appear
		not      []string // substrings that must not
	}{
		{"the password changed in the Secret", dbv1alpha1.RoleSpec{Name: appRole}, func(*observedRole) {}, "a-new-password",
			[]string{`alter role "app"`, passwordClause}, []string{"comment on"}},
		{"the stored password is md5", dbv1alpha1.RoleSpec{Name: appRole}, func(r *observedRole) { r.Password = "md5abc" }, testPassword,
			[]string{passwordClause}, nil},
		{"no password at all", dbv1alpha1.RoleSpec{Name: appRole}, func(r *observedRole) { r.Password = "" }, testPassword,
			[]string{passwordClause}, nil},
		{"the role became a superuser", dbv1alpha1.RoleSpec{Name: appRole}, func(r *observedRole) { r.Super = true }, testPassword,
			[]string{"nosuperuser"}, []string{passwordWord}},
		{"the role can create databases", dbv1alpha1.RoleSpec{Name: appRole}, func(r *observedRole) { r.CreateDB = true }, testPassword,
			[]string{"nocreatedb"}, []string{passwordWord}},
		{"the role can create roles", dbv1alpha1.RoleSpec{Name: appRole}, func(r *observedRole) { r.CreateRole = true }, testPassword,
			[]string{"nocreaterole"}, nil},
		{"the role can replicate", dbv1alpha1.RoleSpec{Name: appRole}, func(r *observedRole) { r.Replication = true }, testPassword,
			[]string{"noreplication"}, nil},
		{"the role bypasses row security", dbv1alpha1.RoleSpec{Name: appRole}, func(r *observedRole) { r.BypassRLS = true }, testPassword,
			[]string{"nobypassrls"}, nil},
		{"the role cannot log in", dbv1alpha1.RoleSpec{Name: appRole}, func(r *observedRole) { r.Login = false }, testPassword,
			[]string{"with login"}, []string{passwordWord}},
		{"the limit changed", dbv1alpha1.RoleSpec{Name: appRole, ConnectionLimit: &limit}, func(*observedRole) {}, testPassword,
			[]string{"connection limit 5"}, []string{passwordWord}},
		{"the limit was removed", dbv1alpha1.RoleSpec{Name: appRole}, func(r *observedRole) { r.Limit = 7 }, testPassword,
			[]string{"connection limit -1"}, nil},
		{"the comment names another cluster: a restored one takes the role over",
			dbv1alpha1.RoleSpec{Name: appRole}, func(r *observedRole) { r.Comment = roleMarkerPrefix + "prod/old-orders" }, testPassword,
			[]string{`comment on role "app" is 'Managed by postgres-operator for prod/orders'`}, []string{"alter role"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			have := goodRole(t, appRole)
			tt.mutate(&have)
			out := planRoles(rolesCluster(tt.spec), &observedRoles{Roles: []observedRole{have}}, map[string]string{appRole: tt.password}, nil)
			text := joined(out[0].statements)
			for _, want := range tt.want {
				if !strings.Contains(text, want) {
					t.Errorf("statements %q lack %q", text, want)
				}
			}
			for _, not := range tt.not {
				if strings.Contains(text, not) {
					t.Errorf("statements %q contain %q", text, not)
				}
			}
			if out[0].status.State != dbv1alpha1.RoleReady {
				t.Errorf("status = %+v", out[0].status)
			}
		})
	}
}

func TestPlanDoesNotTouchARoleItDidNotMake(t *testing.T) {
	for _, comment := range []string{"", "someone else's role", "Managed by something else for prod/orders"} {
		have := goodRole(t, appRole)
		have.Comment = comment
		have.Super = true
		out := planRoles(rolesCluster(dbv1alpha1.RoleSpec{Name: appRole}), &observedRoles{Roles: []observedRole{have}},
			map[string]string{appRole: testPassword}, nil)
		if len(out[0].statements) != 0 {
			t.Errorf("comment %q: statements %v", comment, out[0].statements)
		}
		if out[0].status.State != dbv1alpha1.RoleError || !strings.Contains(out[0].status.Message, "was not made by this operator") {
			t.Errorf("comment %q: status %+v", comment, out[0].status)
		}
	}
}

func TestPlanDatabases(t *testing.T) {
	cluster := rolesCluster(dbv1alpha1.RoleSpec{Name: appRole, Database: ordersDatabase})
	role := goodRole(t, appRole)
	run := func(dbs ...observedDatabase) roleOutcome {
		return planRoles(cluster, &observedRoles{Roles: []observedRole{role}, Databases: dbs}, map[string]string{appRole: testPassword}, nil)[0]
	}

	t.Run("one the role owns, closed to everyone else, needs nothing", func(t *testing.T) {
		if out := run(observedDatabase{Name: ordersDatabase, Owner: appRole}); len(out.statements) != 0 || out.status.State != dbv1alpha1.RoleReady {
			t.Errorf("outcome = %+v", out)
		}
	})
	t.Run("one the role owns but anyone can connect to is closed", func(t *testing.T) {
		out := run(observedDatabase{Name: ordersDatabase, Owner: appRole, PublicConnect: true})
		if joined(out.statements) != `revoke connect on database "orders" from public` {
			t.Errorf("statements = %v", out.statements)
		}
	})
	t.Run("one somebody else owns is left alone, and said so", func(t *testing.T) {
		out := run(observedDatabase{Name: ordersDatabase, Owner: testSuperuserName, PublicConnect: true})
		if len(out.statements) != 0 || out.status.State != dbv1alpha1.RoleError ||
			!strings.Contains(out.status.Message, "owned by postgres") {
			t.Errorf("outcome = %+v", out)
		}
	})
	t.Run("one that does not exist is made, owned by the role", func(t *testing.T) {
		out := run()
		if !strings.Contains(joined(out.statements), `create database "orders" owner "app"`) {
			t.Errorf("statements = %v", out.statements)
		}
	})
	t.Run("a role with no database gets none", func(t *testing.T) {
		out := planRoles(rolesCluster(dbv1alpha1.RoleSpec{Name: appRole}), &observedRoles{Roles: []observedRole{role}},
			map[string]string{appRole: testPassword}, nil)[0]
		if len(out.statements) != 0 {
			t.Errorf("statements = %v", out.statements)
		}
	})
}

func TestPlanReportsProblemsWithoutTouchingTheDatabase(t *testing.T) {
	cluster := rolesCluster(dbv1alpha1.RoleSpec{Name: appRole})
	out := planRoles(cluster, &observedRoles{}, map[string]string{appRole: testPassword}, map[string]string{appRole: "the Secret is not ours"})
	if len(out[0].statements) != 0 || out[0].status.State != dbv1alpha1.RoleError || out[0].status.Message != "the Secret is not ours" {
		t.Errorf("outcome = %+v", out[0])
	}
	out = planRoles(cluster, &observedRoles{}, map[string]string{}, nil)
	if len(out[0].statements) != 0 || !strings.Contains(out[0].status.Message, "has no password yet") {
		t.Errorf("a role with no password was planned: %+v", out[0])
	}
}

func TestPlanDisablesRolesRemovedFromTheSpecAndNeverDropsThem(t *testing.T) {
	cluster := rolesCluster(dbv1alpha1.RoleSpec{Name: keptRole})
	removed := goodRole(t, "removed")
	stranger := goodRole(t, "stranger")
	stranger.Comment = "not ours"
	monitor := goodRole(t, monitorRoleName)
	already := goodRole(t, "already_off")
	already.Login = false
	restored := goodRole(t, "from_the_source")
	restored.Comment = roleMarkerPrefix + "prod/old-orders"
	observed := &observedRoles{Roles: []observedRole{goodRole(t, keptRole), removed, stranger, monitor, already, restored}}

	out := planRoles(cluster, observed, map[string]string{keptRole: testPassword}, nil)
	names := make([]string, 0, len(out))
	for _, o := range out {
		names = append(names, o.status.Name)
	}
	if strings.Join(names, ",") != "kept,already_off,from_the_source,removed" {
		t.Fatalf("outcomes for %v: the spec's roles first, then the removed ones by name, never the monitor or a stranger's", names)
	}
	gone := outcomeFor(t, out, "removed")
	if gone.status.State != dbv1alpha1.RoleDisabled {
		t.Errorf("status = %+v", gone.status)
	}
	text := joined(gone.statements)
	if !strings.Contains(text, `alter role "removed" nologin`) || !strings.Contains(text, "pg_terminate_backend") ||
		!strings.Contains(text, "usename = 'removed'") {
		t.Errorf("statements = %v", gone.statements)
	}
	for _, o := range out {
		if strings.Contains(strings.ToLower(joined(o.statements)), "drop ") {
			t.Errorf("a role is dropped: %v", o.statements)
		}
	}
	if len(outcomeFor(t, out, "already_off").statements) != 0 {
		t.Error("a role that cannot log in is changed again")
	}
	if outcomeFor(t, out, "from_the_source").status.State != dbv1alpha1.RoleDisabled {
		t.Error("a role restored from another cluster's data, and not in the spec, is not disabled")
	}
}

func TestPlanGivesTheLoginBackToARoleThatIsInTheSpecAgain(t *testing.T) {
	// A role that is in the spec again after being disabled gets its login back.
	have := goodRole(t, appRole)
	have.Login = false
	out := planRoles(rolesCluster(dbv1alpha1.RoleSpec{Name: appRole}), &observedRoles{Roles: []observedRole{have}},
		map[string]string{appRole: testPassword}, nil)
	if !strings.Contains(joined(out[0].statements), "with login") {
		t.Errorf("statements = %v", out[0].statements)
	}
}

func TestObservedSQLNamesOnlyTheSpecsRolesForPasswords(t *testing.T) {
	sql := rolesObservedSQL([]string{appRole, "other"})
	if !strings.Contains(sql, "case when r.rolname in ('app', 'other') then coalesce(a.rolpassword, '') else '' end") {
		t.Errorf("the roles whose verifier is read are not named: %s", sql)
	}
	if !strings.Contains(rolesObservedSQL(nil), "case when false then") {
		t.Error("with no roles in the spec a verifier is still read")
	}
	if !strings.HasSuffix(strings.TrimSpace(sql), ")::text") {
		t.Error("the answer is not text")
	}
}

func TestObservedRolesDecodeWhatTheQueryBuilds(t *testing.T) {
	answer := `{"roles":[{"name":"app","comment":"c","login":true,"super":false,"createdb":false,"createrole":false,` +
		`"replication":false,"bypassrls":false,"limit":-1,"password":"SCRAM-SHA-256$4096:x$y:z"}],` +
		`"databases":[{"name":"orders","owner":"app","publicConnect":false}]}`
	var observed observedRoles
	if err := json.Unmarshal([]byte(answer), &observed); err != nil {
		t.Fatal(err)
	}
	if got := observed.role(appRole); got == nil || !got.Login || got.Limit != -1 || got.Password == "" {
		t.Errorf("role = %+v", got)
	}
	if got := observed.database("orders"); got == nil || got.Owner != appRole || got.PublicConnect {
		t.Errorf("database = %+v", got)
	}
	if observed.role("nobody") != nil || observed.database("nothing") != nil {
		t.Error("something found that is not there")
	}
}

func TestQuotingKeepsANameInsideItsQuotes(t *testing.T) {
	if got := quoteIdent(`a"b`); got != `"a""b"` {
		t.Errorf("quoteIdent = %s", got)
	}
}

func TestOwnershipMarker(t *testing.T) {
	cluster := rolesCluster()
	if !ownedByOperator(roleMarker(cluster)) || !ownedByOperator(roleMarkerPrefix+"other/cluster") {
		t.Error("a comment the operator writes is not recognised")
	}
	for _, not := range []string{"", "owner", "Managed by hand"} {
		if ownedByOperator(not) {
			t.Errorf("%q is recognised", not)
		}
	}
}
