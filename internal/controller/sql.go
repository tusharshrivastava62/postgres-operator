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
	"crypto/tls"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	// sqlConnectTimeout bounds opening a connection. A network policy that
	// does not let the operator through shows up as this timing out.
	sqlConnectTimeout = 10 * time.Second

	// sqlApplicationName is how the operator's sessions appear in the server
	// log and in pg_stat_activity.
	sqlApplicationName = "postgres-operator"

	// Errors whose SQLSTATE says the server refused who the operator said it
	// was: a wrong password, or a role that does not exist.
	sqlStateInvalidPassword      = "28P01"
	sqlStateInvalidAuthorization = "28000"
)

// SQLTarget is a database the operator opens a connection to.
type SQLTarget struct {
	Host     string
	Port     int32
	Database string
	User     string
	Password string
}

// SQLSession is one open connection to a database.
type SQLSession interface {
	// Exec runs the statements in sql, which may be several, with the simple
	// query protocol. They run in one implicit transaction, so they all take
	// effect or none does.
	Exec(ctx context.Context, sql string) error
	// QueryBool runs a query that returns a single boolean.
	QueryBool(ctx context.Context, sql string) (bool, error)
	Close(ctx context.Context)
}

// SQLDialer opens connections. It is the only way the operator runs SQL, so
// everything that does can be tested against a fake.
type SQLDialer interface {
	Dial(ctx context.Context, target SQLTarget) (SQLSession, error)
}

// pgxDialer connects with pgx, over TLS, which Spilo's pg_hba requires for
// every connection that is not local.
type pgxDialer struct{}

// NewPGXDialer returns the dialer the operator runs with.
func NewPGXDialer() SQLDialer { return pgxDialer{} }

func (pgxDialer) Dial(ctx context.Context, target SQLTarget) (SQLSession, error) {
	// sslmode=require: no fallback to a plain connection.
	cfg, err := pgx.ParseConfig("sslmode=require")
	if err != nil {
		return nil, err
	}
	cfg.Host = target.Host
	cfg.Port = uint16(target.Port) //nolint:gosec // a TCP port
	cfg.Database = target.Database
	cfg.User = target.User
	cfg.Password = target.Password
	cfg.ConnectTimeout = sqlConnectTimeout
	cfg.RuntimeParams["application_name"] = sqlApplicationName
	// Spilo's server certificate is generated when the container starts and
	// is signed by nothing the operator could know, so it is not verified:
	// the connection is encrypted, not authenticated. Making the certificate
	// verifiable is part of the security hardening stage.
	cfg.TLSConfig = &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12} //nolint:gosec // see above
	cfg.Fallbacks = nil

	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &pgxSession{conn: conn}, nil
}

type pgxSession struct {
	conn *pgx.Conn
}

func (s *pgxSession) Exec(ctx context.Context, sql string) error {
	_, err := s.conn.PgConn().Exec(ctx, sql).ReadAll()
	return err
}

func (s *pgxSession) QueryBool(ctx context.Context, sql string) (bool, error) {
	var answer bool
	err := s.conn.QueryRow(ctx, sql, pgx.QueryExecModeSimpleProtocol).Scan(&answer)
	return answer, err
}

func (s *pgxSession) Close(ctx context.Context) { _ = s.conn.Close(ctx) }

// sqlRefusedCredentials reports whether err is the server saying the user or
// the password is wrong, as opposed to not being reachable at all.
func sqlRefusedCredentials(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) &&
		(pgErr.Code == sqlStateInvalidPassword || pgErr.Code == sqlStateInvalidAuthorization)
}

// sqlLiteral quotes s as a string literal. The statements the operator
// builds contain only values it generated itself, but a quote in one must
// never be able to end the literal.
func sqlLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
