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
	"crypto/x509"
	"encoding/base64"
	"errors"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	dbv1alpha1 "github.com/tusharshrivastava62/postgres-operator/api/v1alpha1"
)

const (
	testAuthFailed    = "password authentication failed"
	testSuperuserName = "postgres"
	testLeaderIP      = "10.20.30.40"
)

// fakeSQL stands in for a database: it knows the superuser's password, and
// what the monitor role's password and privileges are, and it answers the
// way PostgreSQL does. A statement that sets the monitor's password is only
// believed if it carries a SCRAM verifier of the password the Secret holds.
type fakeSQL struct {
	mu sync.Mutex

	superuserPassword string
	// monitorPassword is what the database holds for the monitor role; empty
	// means the role does not exist.
	monitorPassword string
	// monitorVerifier is the verifier the last setup statement set, which a
	// login is checked against when the password is not one the test knows.
	monitorVerifier string
	// monitorPrivileged is whether the role has everything the verify query asks.
	monitorPrivileged bool
	// The rewind login: whether it exists and can log in, the verifier of its
	// password, and whether it has what pg_rewind needs.
	rewindExists     bool
	rewindLogin      bool
	rewindVerifier   string
	rewindPrivileged bool
	// unreachable, when set, is what every Dial fails with: the network is down.
	unreachable error
	// serving, when set, is the certificate the server presents. A connection
	// that asks to check it is refused, as a real one would be, if it does not
	// pass.
	serving *x509.Certificate

	// observed is what the role query answers with; execFailure, if it
	// returns an error for a statement, is what running that statement does.
	observed    string
	execFailure func(statement string) error
	// afterExec, if set, is told of each statement that ran, so a test can
	// make the fake database change the way a real one would.
	afterExec func(statement string)

	// textFor, if set, answers a query for text instead of observed; boolFor
	// answers a query for a boolean instead of monitorPrivileged.
	textFor func(database, sql string) string
	boolFor func(database, sql string) bool

	// ran is every statement, with the database it ran in, in order.
	ran []ranStatement

	dials    []SQLTarget
	executed []string
	queries  []string
	queried  []string
}

func (f *fakeSQL) Dial(_ context.Context, target SQLTarget) (SQLSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dials = append(f.dials, target)
	if f.unreachable != nil {
		return nil, f.unreachable
	}
	if target.TLS != nil && f.serving != nil {
		_, err := f.serving.Verify(x509.VerifyOptions{DNSName: target.TLS.ServerName, Roots: target.TLS.Roots})
		if err != nil {
			return nil, &tls.CertificateVerificationError{UnverifiedCertificates: []*x509.Certificate{f.serving}, Err: err}
		}
	}
	switch target.User {
	case rewindRoleName:
		if !f.rewindExists || !f.rewindLogin || f.rewindVerifier == "" || !verifierMatches(f.rewindVerifier, target.Password) {
			return nil, &pgconn.PgError{Code: sqlStateInvalidPassword, Message: testAuthFailed}
		}
	case monitorRoleName:
		matchesPassword := f.monitorPassword != "" && target.Password == f.monitorPassword
		matchesVerifier := f.monitorVerifier != "" && verifierMatches(f.monitorVerifier, target.Password)
		if !matchesPassword && !matchesVerifier {
			return nil, &pgconn.PgError{Code: sqlStateInvalidPassword, Message: testAuthFailed}
		}
	case testSuperuserName:
		if target.Password != f.superuserPassword {
			return nil, &pgconn.PgError{Code: sqlStateInvalidPassword, Message: testAuthFailed}
		}
	default:
		return nil, &pgconn.PgError{Code: sqlStateInvalidAuthorization, Message: "role does not exist"}
	}
	return &fakeSession{db: f, user: target.User, password: target.Password, database: target.Database}, nil
}

func (f *fakeSQL) usersDialed() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	users := make([]string, 0, len(f.dials))
	for _, d := range f.dials {
		users = append(users, d.User)
	}
	return users
}

type ranStatement struct{ database, sql string }

type fakeSession struct {
	db       *fakeSQL
	user     string
	password string
	database string
}

var verifierInSQL = regexp.MustCompile(`SCRAM-SHA-256\$(\d+):([A-Za-z0-9+/=]+)\$[A-Za-z0-9+/=]+:[A-Za-z0-9+/=]+`)

func (s *fakeSession) Exec(_ context.Context, sql string) error {
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	if s.user != testSuperuserName {
		return &pgconn.PgError{Code: "42501", Message: "permission denied"}
	}
	s.db.executed = append(s.db.executed, sql)
	s.db.ran = append(s.db.ran, ranStatement{s.database, sql})
	if s.db.execFailure != nil {
		if err := s.db.execFailure(sql); err != nil {
			return err
		}
	}
	if s.db.afterExec != nil {
		defer s.db.afterExec(sql)
	}
	if strings.Contains(sql, rewindRoleName) {
		s.db.applyRewindStatement(sql)
	}
	if m := verifierInSQL.FindString(sql); m != "" && !strings.Contains(sql, rewindRoleName) {
		// Believe the password only if the statement is the verifier of it.
		s.db.monitorPassword = s.db.passwordHashedBy(m)
		s.db.monitorVerifier = m
		s.db.monitorPrivileged = true
	}
	return nil
}

// passwordHashedBy returns the one password in the candidates that the
// verifier is of, or "" if it is of none. The candidates are the passwords the
// test puts in the monitor Secret.
func (f *fakeSQL) passwordHashedBy(verifier string) string {
	m := verifierInSQL.FindStringSubmatch(verifier)
	salt, _ := base64Decode(m[2])
	for _, candidate := range testMonitorPasswords {
		if v, err := scramVerifier(candidate, salt, 4096); err == nil && v == verifier {
			return candidate
		}
	}
	return ""
}

var rewindVerifierInSQL = regexp.MustCompile(`alter role "?` + rewindRoleName + `"?(?: with [^;]*?)? password '(SCRAM-SHA-256\$[^']+)'`)

// applyRewindStatement changes the fake database as a statement about the rewind
// login would change a real one.
func (f *fakeSQL) applyRewindStatement(sql string) {
	if m := rewindVerifierInSQL.FindStringSubmatch(sql); m != nil {
		f.rewindVerifier = m[1]
	}
	switch {
	case strings.Contains(sql, "create role "+rewindRoleName):
		f.rewindExists, f.rewindLogin = true, true
		f.rewindPrivileged = strings.Contains(sql, "grant execute on function pg_catalog.pg_ls_dir")
	case strings.Contains(sql, "alter role "+rewindRoleName+" nologin"):
		f.rewindLogin = false
	}
}

func (s *fakeSession) QueryBool(_ context.Context, sql string) (bool, error) {
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	s.db.queries = append(s.db.queries, s.user)
	if s.user == rewindRoleName {
		return s.db.rewindPrivileged, nil
	}
	if s.db.boolFor != nil {
		return s.db.boolFor(s.database, sql), nil
	}
	return s.db.monitorPrivileged, nil
}

func (s *fakeSession) QueryText(_ context.Context, sql string) (string, error) {
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	s.db.queries = append(s.db.queries, s.user)
	s.db.queried = append(s.db.queried, sql)
	if s.db.textFor != nil {
		return s.db.textFor(s.database, sql), nil
	}
	return s.db.observed, nil
}

func (s *fakeSession) PeerCertificate() *x509.Certificate { return s.db.serving }

func (s *fakeSession) Close(context.Context) {}

// testMonitorPasswords are the passwords a test has put in a monitor Secret.
var testMonitorPasswords []string

var _ = Describe("The monitor role", func() {
	const namespace = "default"
	ctx := context.Background()
	key := func(name string) types.NamespacedName { return types.NamespacedName{Name: name, Namespace: namespace} }

	var (
		sql  *fakeSQL
		rec  *PostgresClusterReconciler
		pats *fakePatroni
	)
	BeforeEach(func() {
		testMonitorPasswords = nil
		sql = &fakeSQL{}
		pats = &fakePatroni{live: map[string]any{}}
		rec = &PostgresClusterReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), Patroni: pats, Operator: testOperator, SQL: sql,
		}
	})

	reconcileOnce := func(name string) reconcile.Result {
		GinkgoHelper()
		res, err := rec.Reconcile(ctx, reconcile.Request{NamespacedName: key(name)})
		Expect(err).NotTo(HaveOccurred())
		return res
	}
	get := func(name string) *dbv1alpha1.PostgresCluster {
		GinkgoHelper()
		c := &dbv1alpha1.PostgresCluster{}
		Expect(k8sClient.Get(ctx, key(name), c)).To(Succeed())
		return c
	}
	// create makes a cluster that wants the role: compliance settings.
	create := func(name string) {
		GinkgoHelper()
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: auditSecret, Namespace: namespace},
			StringData: map[string]string{secretKeyAccessKey: "id", secretKeySecretKey: testSecretValue},
		}
		if err := k8sClient.Create(ctx, secret); err == nil {
			DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, secret))).To(Succeed()) })
		} else {
			Expect(apierrors.IsAlreadyExists(err)).To(BeTrue())
		}
		c := withCompliance(plainCluster(), dbv1alpha1.StandardHIPAA)
		c.Name = name
		c.Spec.Storage = dbv1alpha1.StorageSpec{Size: resource.MustParse("1Gi")}
		c.Spec.Resources = corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}}
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		DeferCleanup(func() {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, c))).To(Succeed())
			for _, n := range []string{credentialsSecretName(c), monitorSecretName(c)} {
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: n, Namespace: namespace}}))).To(Succeed())
			}
		})
	}
	startLeader := func(name string) {
		GinkgoHelper()
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name + "-0", Namespace: namespace, Labels: selectorLabelsFor(name)},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: testPodImage}}},
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		DeferCleanup(func() {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, pod, client.GracePeriodSeconds(0)))).To(Succeed())
		})
		pod.Status.PodIP = testLeaderIP
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
		endpoints := &patroniEndpoints{ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: namespace, Labels: selectorLabelsFor(name),
			Annotations: map[string]string{annotationLeader: pod.Name},
		}}
		Expect(k8sClient.Create(ctx, endpoints)).To(Succeed())
		DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, endpoints))).To(Succeed()) })
	}
	secretOf := func(name string) *corev1.Secret {
		GinkgoHelper()
		s := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, s)).To(Succeed())
		return s
	}
	// prepare creates the cluster, reconciles once so the Secrets exist, and
	// gives the fake database the superuser password the Secret holds.
	prepare := func(name string) {
		GinkgoHelper()
		create(name)
		reconcileOnce(name)
		sql.superuserPassword = string(secretOf(name + "-credentials").Data[secretKeyPassword])
		testMonitorPasswords = []string{string(secretOf(name + "-monitor").Data[secretKeyPassword])}
	}
	monitorState := func(name string) dbv1alpha1.RoleStatus {
		GinkgoHelper()
		return get(name).Status.AccessControl.MonitorRole
	}

	Describe("its Secret", func() {
		It("is made with the role's name and a generated password, and is not owned by the cluster", func() {
			create("mon-secret")
			reconcileOnce("mon-secret")
			secret := secretOf("mon-secret-monitor")
			Expect(string(secret.Data[secretKeyUsername])).To(Equal(monitorRoleName))
			Expect(len(secret.Data[secretKeyPassword])).To(BeNumerically(">=", 32))
			Expect(secret.OwnerReferences).To(BeEmpty(), "the password lives in the database, which outlives the cluster")
			Expect(secret.Labels).To(HaveKeyWithValue(labelAppInstance, "mon-secret"))
		})

		It("keeps its password across reconciles, and puts back one that went missing", func() {
			create("mon-keep")
			reconcileOnce("mon-keep")
			first := string(secretOf("mon-keep-monitor").Data[secretKeyPassword])
			reconcileOnce("mon-keep")
			Expect(string(secretOf("mon-keep-monitor").Data[secretKeyPassword])).To(Equal(first))

			secret := secretOf("mon-keep-monitor")
			delete(secret.Data, secretKeyPassword)
			Expect(k8sClient.Update(ctx, secret)).To(Succeed())
			reconcileOnce("mon-keep")
			Expect(string(secretOf("mon-keep-monitor").Data[secretKeyPassword])).NotTo(BeEmpty())
		})

		It("is not made for a cluster that runs nothing that needs it", func() {
			c := plainCluster()
			c.Name = "mon-none"
			c.Spec.Storage = dbv1alpha1.StorageSpec{Size: resource.MustParse("1Gi")}
			c.Spec.Resources = corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}}
			Expect(k8sClient.Create(ctx, c)).To(Succeed())
			DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, c))).To(Succeed()) })
			reconcileOnce("mon-none")
			err := k8sClient.Get(ctx, types.NamespacedName{Name: "mon-none-monitor", Namespace: namespace}, &corev1.Secret{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
			Expect(sql.dials).To(BeEmpty())
		})
	})

	Describe("setting it up", func() {
		It("connects to the leader on port 5432 as the superuser and runs the setup, once the monitor is refused", func() {
			prepare("mon-setup")
			startLeader("mon-setup")
			reconcileOnce("mon-setup")

			Expect(sql.usersDialed()).To(Equal([]string{monitorRoleName, testSuperuserName, monitorRoleName}),
				"try the role first; use the superuser only to put it right; then check it works")
			for _, target := range sql.dials {
				Expect(target.Host).To(Equal(testLeaderIP))
				Expect(target.Port).To(Equal(int32(5432)))
				Expect(target.Database).To(Equal("postgres"))
			}
			Expect(sql.executed).To(HaveLen(1))
			state := monitorState("mon-setup")
			Expect(state.State).To(Equal(dbv1alpha1.RoleReady))
			Expect(state.Name).To(Equal(monitorRoleName))
			Expect(state.SecretName).To(Equal("mon-setup-monitor"))
			Expect(state.Message).To(BeEmpty())
		})

		It("never sends the password, only a verifier of it", func() {
			prepare("mon-verifier")
			startLeader("mon-verifier")
			reconcileOnce("mon-verifier")

			password := string(secretOf("mon-verifier-monitor").Data[secretKeyPassword])
			Expect(sql.executed).To(HaveLen(1))
			Expect(sql.executed[0]).NotTo(ContainSubstring(password))
			Expect(sql.executed[0]).To(MatchRegexp(`password 'SCRAM-SHA-256\$4096:`))
			Expect(sql.monitorPassword).To(Equal(password), "the verifier is of the Secret's password")
		})

		It("gives the role the least it needs", func() {
			prepare("mon-grants")
			startLeader("mon-grants")
			reconcileOnce("mon-grants")

			script := sql.executed[0]
			Expect(script).To(ContainSubstring("nosuperuser nocreatedb nocreaterole noreplication nobypassrls"))
			Expect(script).To(ContainSubstring("connection limit 5"))
			Expect(script).To(ContainSubstring("grant pg_read_all_settings to pgoperator_monitor"))
			Expect(script).NotTo(ContainSubstring("pg_monitor"), "pg_monitor includes pg_read_all_stats")
			Expect(script).NotTo(ContainSubstring("pg_read_all_stats"))
			Expect(script).NotTo(MatchRegexp(`(?i)grant\s+select\s+on\s+\S*pg_authid`), "password hashes stay unreadable")
			Expect(script).To(ContainSubstring("grant select on pg_catalog.pg_hba_file_rules to pgoperator_monitor"))
			Expect(script).To(ContainSubstring("grant execute on function pg_catalog.pg_hba_file_rules() to pgoperator_monitor"))
			Expect(script).To(ContainSubstring("security definer set search_path = pg_catalog, pg_temp"))
			Expect(script).To(ContainSubstring("revoke all on function pgoperator.non_scram_roles() from public"))
			Expect(script).To(ContainSubstring("grant execute on function pgoperator.non_scram_roles() to pgoperator_monitor"))
		})

		It("marks the role as the cluster's", func() {
			prepare("mon-marker")
			startLeader("mon-marker")
			reconcileOnce("mon-marker")
			Expect(sql.executed[0]).To(ContainSubstring("comment on role pgoperator_monitor is 'Managed by postgres-operator for default/mon-marker'"))
		})

		It("runs the setup as one script that can be run again", func() {
			prepare("mon-again")
			startLeader("mon-again")
			reconcileOnce("mon-again")
			script := sql.executed[0]
			Expect(script).To(ContainSubstring("if not exists (select from pg_catalog.pg_roles where rolname = 'pgoperator_monitor')"))
			Expect(script).To(ContainSubstring("create schema if not exists pgoperator"))
			Expect(script).To(ContainSubstring("create or replace function pgoperator.non_scram_roles()"))
		})
	})

	Describe("keeping it", func() {
		It("does not touch the database again until the check interval has passed", func() {
			prepare("mon-throttle")
			startLeader("mon-throttle")
			reconcileOnce("mon-throttle")
			dials := len(sql.dials)
			reconcileOnce("mon-throttle")
			reconcileOnce("mon-throttle")
			Expect(sql.dials).To(HaveLen(dials), "logging in on every reconcile would fill the server log")
		})

		It("only logs in as the role itself when it is in order: the superuser is not used", func() {
			prepare("mon-ok")
			startLeader("mon-ok")
			sql.monitorPassword = testMonitorPasswords[0]
			sql.monitorPrivileged = true
			reconcileOnce("mon-ok")
			Expect(sql.usersDialed()).To(Equal([]string{monitorRoleName}))
			Expect(sql.executed).To(BeEmpty())
			Expect(monitorState("mon-ok").State).To(Equal(dbv1alpha1.RoleReady))
		})

		It("checks again after the interval", func() {
			prepare("mon-interval")
			startLeader("mon-interval")
			reconcileOnce("mon-interval")
			dials := len(sql.dials)
			rec.monitorVerified.Store("default/mon-interval", monitorTrust{
				secretVersion: secretOf("mon-interval-monitor").ResourceVersion,
				at:            time.Now().Add(-monitorVerifyInterval - time.Second),
			})
			reconcileOnce("mon-interval")
			Expect(len(sql.dials)).To(BeNumerically(">", dials))
		})

		It("checks at once when its Secret has changed, and sets the new password", func() {
			prepare("mon-changed")
			startLeader("mon-changed")
			reconcileOnce("mon-changed")
			old := string(secretOf("mon-changed-monitor").Data[secretKeyPassword])

			secret := secretOf("mon-changed-monitor")
			secret.Data[secretKeyPassword] = []byte("a-new-password-for-the-monitor-0123456789")
			Expect(k8sClient.Update(ctx, secret)).To(Succeed())
			testMonitorPasswords = append(testMonitorPasswords, "a-new-password-for-the-monitor-0123456789")
			reconcileOnce("mon-changed")

			Expect(sql.monitorPassword).To(Equal("a-new-password-for-the-monitor-0123456789"))
			Expect(sql.monitorPassword).NotTo(Equal(old))
			Expect(sql.executed).To(HaveLen(2))
		})

		It("puts right a role that lost a privilege", func() {
			prepare("mon-drift")
			startLeader("mon-drift")
			sql.monitorPassword = testMonitorPasswords[0]
			sql.monitorPrivileged = false
			reconcileOnce("mon-drift")
			Expect(sql.executed).To(HaveLen(1))
			Expect(monitorState("mon-drift").State).To(Equal(dbv1alpha1.RoleReady))
		})
	})

	Describe("when it cannot be set up", func() {
		It("waits, with a reason, while the cluster has no primary", func() {
			prepare("mon-nolead")
			res := reconcileOnce("mon-nolead")
			Expect(sql.dials).To(BeEmpty())
			state := monitorState("mon-nolead")
			Expect(state.State).To(Equal(dbv1alpha1.RolePending))
			Expect(state.Message).To(ContainSubstring("no primary"))
			Expect(res.RequeueAfter).To(Equal(unhealthyRefreshInterval))
		})

		It("reports the database being unreachable on port 5432, and looks again soon", func() {
			prepare("mon-down")
			startLeader("mon-down")
			sql.unreachable = errors.New("dial tcp 10.20.30.40:5432: i/o timeout")
			res := reconcileOnce("mon-down")
			state := monitorState("mon-down")
			Expect(state.State).To(Equal(dbv1alpha1.RoleError))
			Expect(state.Message).To(ContainSubstring("port 5432"))
			Expect(state.Message).To(ContainSubstring("i/o timeout"))
			Expect(res.RequeueAfter).To(Equal(unhealthyRefreshInterval))
		})

		It("reports a superuser the database refuses, and does not run anything", func() {
			prepare("mon-nosu")
			startLeader("mon-nosu")
			sql.superuserPassword = "not-the-secrets-password"
			reconcileOnce("mon-nosu")
			state := monitorState("mon-nosu")
			Expect(state.State).To(Equal(dbv1alpha1.RoleError))
			Expect(state.Message).To(ContainSubstring("could not set up the role"))
			Expect(sql.executed).To(BeEmpty())
		})

		It("does not stop the rest of the reconcile", func() {
			prepare("mon-rest")
			startLeader("mon-rest")
			sql.unreachable = errors.New("refused")
			reconcileOnce("mon-rest")
			sts := &corev1.Pod{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "mon-rest-0", Namespace: namespace}, sts)).To(Succeed())
			Expect(get("mon-rest").Status.Image).NotTo(BeEmpty(), "status was still written")
		})

		It("writes status only when it changed", func() {
			prepare("mon-quiet")
			startLeader("mon-quiet")
			sql.unreachable = errors.New("refused")
			reconcileOnce("mon-quiet")
			version := get("mon-quiet").ResourceVersion
			reconcileOnce("mon-quiet")
			Expect(get("mon-quiet").ResourceVersion).To(Equal(version))
		})
	})

	Describe("how soon the cluster is looked at again", func() {
		well := refreshInputs{synced: true, healthy: true, monitorReady: true, rolesReady: true}
		It("is slowly only when everything is well", func() {
			Expect(refreshDelay(well)).To(Equal(healthyRefreshInterval))
		})
		It("is quickly when any one thing is not", func() {
			in := well
			in.synced = false
			Expect(refreshDelay(in)).To(Equal(unhealthyRefreshInterval), "Patroni's configuration not synced")
			in = well
			in.healthy = false
			Expect(refreshDelay(in)).To(Equal(unhealthyRefreshInterval), "the cluster is unhealthy")
			in = well
			in.monitorReady = false
			Expect(refreshDelay(in)).To(Equal(unhealthyRefreshInterval), "the monitor role is not ready")
			in = well
			in.rolesReady = false
			Expect(refreshDelay(in)).To(Equal(unhealthyRefreshInterval), "an application role is not ready")
			in = well
			in.rotatingCredentials = true
			Expect(refreshDelay(in)).To(Equal(unhealthyRefreshInterval), "the superuser and replication passwords are being replaced")
			in = well
			in.securityPending = true
			Expect(refreshDelay(in)).To(Equal(unhealthyRefreshInterval), "a security setting is not settled")
		})
		It("is soonest during a rolling update", func() {
			in := well
			in.rolling = true
			Expect(refreshDelay(in)).To(Equal(rolloutRequeueDelay))
			Expect(refreshDelay(refreshInputs{rolling: true})).To(Equal(rolloutRequeueDelay))
		})
	})

	Describe("a cluster without the SQL channel", func() {
		It("is left alone", func() {
			create("mon-off")
			rec.SQL = nil
			startLeader("mon-off")
			reconcileOnce("mon-off")
			Expect(monitorState("mon-off")).To(Equal(dbv1alpha1.RoleStatus{}))
		})
	})

	Describe("the jobs", func() {
		// Every pod spec the operator builds for a job that is not the
		// database, as the CronJobs and the Job it creates.
		It("hold no reference to the superuser's credentials", func() {
			c := withCompliance(backupCluster(), dbv1alpha1.StandardHIPAA)
			c.Spec.Backup.Verification = verifySpec
			c.Name, c.Namespace = sourceName, sourceNamespace

			specs := map[string]corev1.PodSpec{
				"backup status":    backupStatusCronJobSpec(c, "img").JobTemplate.Spec.Template.Spec,
				"restore test":     restoreVerifyCronJobSpec(c, "img").JobTemplate.Spec.Template.Spec,
				"compliance check": complianceCheckCronJobSpec(c, "img").JobTemplate.Spec.Template.Spec,
				"network probe":    networkProbeCronJobSpec(c, "img").JobTemplate.Spec.Template.Spec,
				"restore check":    restoreCheckJob(c, "img").Template.Spec,
			}
			for name, pod := range specs {
				Expect(referencedSecrets(pod)).NotTo(ContainElement(credentialsSecretName(c)), name)
			}
		})

		It("do connect as the monitor role, wherever they read the database", func() {
			c := withCompliance(backupCluster(), dbv1alpha1.StandardHIPAA)
			c.Name, c.Namespace = sourceName, sourceNamespace
			for name, spec := range map[string]batchv1.CronJobSpec{
				"backup status":    backupStatusCronJobSpec(c, "img"),
				"compliance check": complianceCheckCronJobSpec(c, "img"),
			} {
				env := envByName(spec.JobTemplate.Spec.Template.Spec.Containers[0].Env)
				Expect(env["PGUSER"].ValueFrom.SecretKeyRef.Name).To(Equal(sourceName+"-monitor"), name)
				Expect(env["PGPASSWORD"].ValueFrom.SecretKeyRef.Name).To(Equal(sourceName+"-monitor"), name)
			}
		})

		It("the strings that are not Secret references are not the password either", func() {
			c := withCompliance(backupCluster(), dbv1alpha1.StandardHIPAA)
			for _, e := range complianceCheckCronJobSpec(c, "img").JobTemplate.Spec.Template.Spec.Containers[0].Env {
				Expect(strings.ToLower(e.Name)).NotTo(ContainSubstring("superuser"))
			}
		})
	})
})

// referencedSecrets lists every Secret a pod spec reads, however it reads it.
func referencedSecrets(pod corev1.PodSpec) []string {
	var names []string
	containers := append(append([]corev1.Container{}, pod.InitContainers...), pod.Containers...)
	for _, c := range containers {
		for _, e := range c.Env {
			if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
				names = append(names, e.ValueFrom.SecretKeyRef.Name)
			}
		}
		for _, from := range c.EnvFrom {
			if from.SecretRef != nil {
				names = append(names, from.SecretRef.Name)
			}
		}
	}
	for _, v := range pod.Volumes {
		if v.Secret != nil {
			names = append(names, v.Secret.SecretName)
		}
		if v.Projected != nil {
			for _, s := range v.Projected.Sources {
				if s.Secret != nil {
					names = append(names, s.Secret.Name)
				}
			}
		}
	}
	return names
}

func base64Decode(s string) ([]byte, error) { return base64.StdEncoding.DecodeString(s) }
