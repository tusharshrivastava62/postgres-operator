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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/types"

	dbv1alpha1 "github.com/tusharshrivastava62/postgres-operator/api/v1alpha1"
)

const (
	// emergencyRolePrefix starts the name of every role made for emergency
	// access. A role in spec.roles may not start with it.
	emergencyRolePrefix = "bg_"

	// emergencyMarkerPrefix starts the comment on those roles. It is not the
	// comment of an application role, so the code that disables application
	// roles that left the spec never touches one, and the other way round.
	emergencyMarkerPrefix = "Emergency access for "

	// emergencyConnectionLimit is how many connections one grant can hold: one
	// to work in, and one to spare.
	emergencyConnectionLimit = 2

	// emergencyExpired is a time long past, which an ended grant's role is
	// left valid until, in case it is ever seen before it is dropped.
	emergencyExpired = "1970-01-01 00:00:00+00"
)

// emergencyRoleName is the role made for a request. It comes from the
// request's UID, so a retry finds the role it made before, and no two
// requests share one.
func emergencyRoleName(uid types.UID) string {
	sum := sha256.Sum256([]byte(uid))
	return emergencyRolePrefix + hex.EncodeToString(sum[:])[:8]
}

// emergencyMarker is the comment on a request's role, which says whose it is.
func emergencyMarker(req *dbv1alpha1.PostgresEmergencyAccess) string {
	return fmt.Sprintf("%s%s/%s: request %s", emergencyMarkerPrefix, req.Namespace, req.Spec.ClusterRef.Name, req.Name)
}

// emergencySecretName is the Secret that holds a request's login.
func emergencySecretName(req *dbv1alpha1.PostgresEmergencyAccess) string {
	return req.Name + "-access"
}

func sqlTimestamp(t time.Time) string { return t.UTC().Format("2006-01-02 15:04:05+00") }

// emergencyRoleSQL makes the role, or puts it right if it exists: able to log
// in, with a password, until the grant ends, and able to do nothing else. What
// lets it become the superuser is granted separately, in each database.
func emergencyRoleSQL(role, verifier, marker string, until time.Time) string {
	return fmt.Sprintf(`
do $$
begin
  if not exists (select from pg_catalog.pg_roles where rolname = %[1]s) then
    create role %[2]s login;
  end if;
end
$$;
alter role %[2]s with login nosuperuser nocreatedb nocreaterole noreplication nobypassrls inherit
  connection limit %[3]d password %[4]s valid until %[5]s;
comment on role %[2]s is %[6]s;
`, sqlLiteral(role), quoteIdent(role), emergencyConnectionLimit, sqlLiteral(verifier), sqlLiteral(sqlTimestamp(until)), sqlLiteral(marker))
}

// emergencyGrantSQL lets the role connect to a database and become the
// superuser in it. The ability to call set_user_u is held per database, and
// so is the escalation: a connection to another database starts unescalated.
// A database made from template0 has no set_user, so it is added first.
func emergencyGrantSQL(role, database string) string {
	return fmt.Sprintf(`
create extension if not exists set_user schema public;
grant connect on database %[1]s to %[2]s;
grant execute on function set_user_u(text) to %[2]s;
grant execute on function reset_user() to %[2]s;
`, quoteIdent(database), quoteIdent(role))
}

// emergencyEndSQL stops the role being used and ends the sessions it has.
func emergencyEndSQL(role string) string {
	return fmt.Sprintf(`
alter role %[1]s nologin valid until %[2]s;
select pg_catalog.pg_terminate_backend(pid) from pg_catalog.pg_stat_activity where usename = %[3]s;
`, quoteIdent(role), sqlLiteral(emergencyExpired), sqlLiteral(role))
}

const listDatabasesSQL = `select coalesce(json_agg(datname order by datname), '[]'::json)::text
from pg_catalog.pg_database where datallowconn and not datistemplate`

func roleExistsSQL(role string) string {
	return "select exists (select from pg_catalog.pg_roles where rolname = " + sqlLiteral(role) + ")"
}

// emergencyOps does the SQL of emergency access, as the superuser.
type emergencyOps struct {
	dialer SQLDialer
	target SQLTarget
}

func (o emergencyOps) dial(ctx context.Context, database string) (SQLSession, error) {
	target := o.target
	target.Database = database
	return o.dialer.Dial(ctx, target)
}

// databases lists the databases a person could work in.
func (o emergencyOps) databases(ctx context.Context) ([]string, error) {
	session, err := o.dial(ctx, monitorDatabase)
	if err != nil {
		return nil, err
	}
	defer session.Close(ctx)
	answer, err := session.QueryText(ctx, listDatabasesSQL)
	if err != nil {
		return nil, err
	}
	var names []string
	if err := json.Unmarshal([]byte(answer), &names); err != nil {
		return nil, err
	}
	return names, nil
}

// grant makes the role and lets it become the superuser in every database.
func (o emergencyOps) grant(ctx context.Context, role, verifier, marker string, until time.Time) error {
	session, err := o.dial(ctx, monitorDatabase)
	if err != nil {
		return fmt.Errorf("connecting as the superuser: %w", err)
	}
	err = session.Exec(ctx, emergencyRoleSQL(role, verifier, marker, until))
	session.Close(ctx)
	if err != nil {
		return fmt.Errorf("making the role: %w", err)
	}
	names, err := o.databases(ctx)
	if err != nil {
		return fmt.Errorf("listing the databases: %w", err)
	}
	for _, name := range names {
		db, err := o.dial(ctx, name)
		if err != nil {
			return fmt.Errorf("connecting to database %s: %w", name, err)
		}
		err = db.Exec(ctx, emergencyGrantSQL(role, name))
		db.Close(ctx)
		if err != nil {
			return fmt.Errorf("granting access to database %s: %w", name, err)
		}
	}
	return nil
}

// revoke takes the access away and drops the role: its login first, so
// nothing new starts, and its sessions; then what it was granted, in each
// database; then the role. Whatever it did while escalated belongs to the
// superuser and stays. It can be run again, and does nothing if the role is gone.
func (o emergencyOps) revoke(ctx context.Context, role string) error {
	session, err := o.dial(ctx, monitorDatabase)
	if err != nil {
		return fmt.Errorf("connecting as the superuser: %w", err)
	}
	defer session.Close(ctx)
	exists, err := session.QueryBool(ctx, roleExistsSQL(role))
	if err != nil {
		return fmt.Errorf("looking for the role: %w", err)
	}
	if !exists {
		return nil
	}
	if err := session.Exec(ctx, emergencyEndSQL(role)); err != nil {
		return fmt.Errorf("ending the role's login and sessions: %w", err)
	}
	names, err := o.databases(ctx)
	if err != nil {
		return fmt.Errorf("listing the databases: %w", err)
	}
	for _, name := range names {
		db, err := o.dial(ctx, name)
		if err != nil {
			return fmt.Errorf("connecting to database %s: %w", name, err)
		}
		err = db.Exec(ctx, "drop owned by "+quoteIdent(role))
		db.Close(ctx)
		if err != nil {
			return fmt.Errorf("taking back what the role was granted in database %s: %w", name, err)
		}
	}
	if err := session.Exec(ctx, "drop role "+quoteIdent(role)); err != nil {
		return fmt.Errorf("dropping the role: %w", err)
	}
	return nil
}
