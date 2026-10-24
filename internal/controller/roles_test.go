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
	"encoding/json"
	"errors"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	dbv1alpha1 "github.com/tusharshrivastava62/postgres-operator/api/v1alpha1"
)

var _ = Describe("Application roles through the reconciler", func() {
	const namespace = "default"
	ctx := context.Background()
	key := func(name string) types.NamespacedName { return types.NamespacedName{Name: name, Namespace: namespace} }

	var (
		sql *fakeSQL
		rec *PostgresClusterReconciler
	)
	BeforeEach(func() {
		sql = &fakeSQL{}
		rec = &PostgresClusterReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), Patroni: &fakePatroni{live: map[string]any{}}, SQL: sql,
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
	secretOf := func(name string) *corev1.Secret {
		GinkgoHelper()
		s := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, key(name), s)).To(Succeed())
		return s
	}
	create := func(name string, roles ...dbv1alpha1.RoleSpec) {
		GinkgoHelper()
		c := plainCluster()
		c.Name = name
		c.Spec.Storage = dbv1alpha1.StorageSpec{Size: resource.MustParse("1Gi")}
		c.Spec.Resources = corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}}
		c.Spec.Roles = roles
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		DeferCleanup(func() {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, c))).To(Succeed())
			secrets := &corev1.SecretList{}
			Expect(k8sClient.List(ctx, secrets, client.InNamespace(namespace), client.MatchingLabels{labelAppInstance: name})).To(Succeed())
			for i := range secrets.Items {
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &secrets.Items[i]))).To(Succeed())
			}
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name + "-credentials", Namespace: namespace}}))).To(Succeed())
		})
	}
	update := func(name string, mutate func(*dbv1alpha1.PostgresCluster)) {
		GinkgoHelper()
		c := get(name)
		mutate(c)
		Expect(k8sClient.Update(ctx, c)).To(Succeed())
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
	// ready makes a cluster, reconciles once so the Secrets exist, gives it a
	// leader, and gives the fake database the superuser password.
	ready := func(name string, roles ...dbv1alpha1.RoleSpec) {
		GinkgoHelper()
		create(name, roles...)
		// The leader comes after the first reconcile: a cluster that has never
		// been reconciled waits for the pods of a previous one of its name.
		reconcileOnce(name)
		startLeader(name)
		sql.superuserPassword = string(secretOf(name + "-credentials").Data[secretKeyPassword])
	}
	// observe sets what the database answers when the roles are read.
	observe := func(observed observedRoles) {
		GinkgoHelper()
		raw, err := json.Marshal(observed)
		Expect(err).NotTo(HaveOccurred())
		sql.observed = string(raw)
	}
	roleStatus := func(name, role string) dbv1alpha1.RoleStatus {
		GinkgoHelper()
		for _, s := range get(name).Status.AccessControl.Roles {
			if s.Name == role {
				return s
			}
		}
		Fail("no status for role " + role)
		return dbv1alpha1.RoleStatus{}
	}
	rolesCondition := func(name string) *metav1.Condition {
		return meta.FindStatusCondition(get(name).Status.Conditions, dbv1alpha1.ConditionRolesReady)
	}

	Describe("their Secrets", func() {
		It("are made with the connection details and a generated password, and not owned by the cluster", func() {
			create("rs-made", dbv1alpha1.RoleSpec{Name: appOrdersRole, Database: ordersDatabase})
			reconcileOnce("rs-made")
			secret := secretOf("rs-made-role-app-orders")
			Expect(string(secret.Data[secretKeyUsername])).To(Equal(appOrdersRole))
			Expect(string(secret.Data[secretKeyHost])).To(Equal("rs-made.default.svc"))
			Expect(string(secret.Data[secretKeyPort])).To(Equal("5432"))
			Expect(string(secret.Data[secretKeyDBName])).To(Equal("orders"))
			Expect(len(secret.Data[secretKeyPassword])).To(BeNumerically(">=", 32))
			Expect(secret.OwnerReferences).To(BeEmpty(), "the password lives in the database, which outlives the cluster")
			Expect(secret.Labels).To(HaveKeyWithValue(labelAppInstance, "rs-made"))
			Expect(secret.Labels).To(HaveKeyWithValue(labelAppComponent, componentRoleCredentials))
		})

		It("get no dbname when the role has no database, and the name the spec gives", func() {
			create("rs-named", dbv1alpha1.RoleSpec{Name: "reporting", SecretName: "reporting-db"})
			reconcileOnce("rs-named")
			secret := secretOf("reporting-db")
			Expect(secret.Data).NotTo(HaveKey(secretKeyDBName))
			Expect(secret.Data).To(HaveKey(secretKeyPassword))
			Expect(secret.Labels).To(HaveKeyWithValue(labelAppInstance, "rs-named"))
		})

		It("keep the password across reconciles, and follow the spec for everything else", func() {
			create("rs-follow", dbv1alpha1.RoleSpec{Name: appRole})
			reconcileOnce("rs-follow")
			first := string(secretOf("rs-follow-role-app").Data[secretKeyPassword])

			update("rs-follow", func(c *dbv1alpha1.PostgresCluster) { c.Spec.Roles[0].Database = "appdb" })
			reconcileOnce("rs-follow")
			secret := secretOf("rs-follow-role-app")
			Expect(string(secret.Data[secretKeyDBName])).To(Equal("appdb"))
			Expect(string(secret.Data[secretKeyPassword])).To(Equal(first))

			update("rs-follow", func(c *dbv1alpha1.PostgresCluster) { c.Spec.Roles[0].Database = "" })
			reconcileOnce("rs-follow")
			secret = secretOf("rs-follow-role-app")
			Expect(secret.Data).NotTo(HaveKey(secretKeyDBName))
			Expect(string(secret.Data[secretKeyPassword])).To(Equal(first))
		})

		It("get a new password when theirs goes missing, and put back a changed username", func() {
			create("rs-missing", dbv1alpha1.RoleSpec{Name: appRole})
			reconcileOnce("rs-missing")
			secret := secretOf("rs-missing-role-app")
			delete(secret.Data, secretKeyPassword)
			secret.Data[secretKeyUsername] = []byte("someone_else")
			Expect(k8sClient.Update(ctx, secret)).To(Succeed())
			reconcileOnce("rs-missing")
			secret = secretOf("rs-missing-role-app")
			Expect(secret.Data[secretKeyPassword]).NotTo(BeEmpty())
			Expect(string(secret.Data[secretKeyUsername])).To(Equal(appRole))
		})

		It("are not touched when a Secret of that name is not the operator's", func() {
			stranger := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "rs-clash-role-app", Namespace: namespace},
				Data:       map[string][]byte{"theirs": []byte("keep")},
			}
			Expect(k8sClient.Create(ctx, stranger)).To(Succeed())
			DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, stranger))).To(Succeed()) })
			ready("rs-clash", dbv1alpha1.RoleSpec{Name: appRole})
			observe(observedRoles{})
			reconcileOnce("rs-clash")

			Expect(secretOf("rs-clash-role-app").Data).To(Equal(map[string][]byte{"theirs": []byte("keep")}))
			Expect(sql.executed).To(BeEmpty(), "no role is made from a password the operator did not write")
			status := roleStatus("rs-clash", appRole)
			Expect(status.State).To(Equal(dbv1alpha1.RoleError))
			Expect(status.Message).To(ContainSubstring("was not made by this operator"))
			Expect(rolesCondition("rs-clash").Status).To(Equal(metav1.ConditionFalse))
		})
	})

	Describe("setting them up", func() {
		It("connects to the primary as the superuser, creates the role, its database and its password, and says so", func() {
			ready("rs-up", dbv1alpha1.RoleSpec{Name: appRole, Database: "appdb"})
			observe(observedRoles{})
			reconcileOnce("rs-up")

			Expect(sql.usersDialed()).To(Equal([]string{testSuperuserName}))
			Expect(sql.dials[0].Host).To(Equal(testLeaderIP))
			Expect(sql.dials[0].Port).To(Equal(int32(5432)))
			Expect(sql.executed).To(HaveLen(5), "each statement on its own, since CREATE DATABASE cannot share a transaction")
			Expect(sql.executed[0]).To(Equal(`create role "app"`))
			Expect(sql.executed[3]).To(Equal(`create database "appdb" owner "app"`))
			Expect(sql.executed[4]).To(Equal(`revoke connect on database "appdb" from public`))

			status := roleStatus("rs-up", appRole)
			Expect(status.LastRotated).NotTo(BeNil(), "a password that was just set has been rotated just now")
			status.LastRotated = nil
			Expect(status).To(Equal(dbv1alpha1.RoleStatus{Name: appRole, State: dbv1alpha1.RoleReady, SecretName: "rs-up-role-app"}))
			condition := rolesCondition("rs-up")
			Expect(condition.Status).To(Equal(metav1.ConditionTrue))
			Expect(condition.Reason).To(Equal("AllRolesReady"))
		})

		It("sends a verifier of the Secret's password, never the password", func() {
			ready("rs-hash", dbv1alpha1.RoleSpec{Name: appRole})
			observe(observedRoles{})
			reconcileOnce("rs-hash")
			password := string(secretOf("rs-hash-role-app").Data[secretKeyPassword])
			all := strings.Join(sql.executed, "\n")
			Expect(all).NotTo(ContainSubstring(password))
			verifier := verifierInSQL.FindString(all)
			Expect(verifier).NotTo(BeEmpty())
			Expect(verifierMatches(verifier, password)).To(BeTrue(), "the verifier is of the password in the Secret")
		})

		It("changes nothing in a database that is already right, and reads it only once in a while", func() {
			ready("rs-quiet", dbv1alpha1.RoleSpec{Name: appRole})
			password := string(secretOf("rs-quiet-role-app").Data[secretKeyPassword])
			verifier, err := newScramVerifier(password)
			Expect(err).NotTo(HaveOccurred())
			observe(observedRoles{Roles: []observedRole{{
				Name: appRole, Comment: roleMarker(get("rs-quiet")), Login: true, Limit: -1, Password: verifier,
			}}})
			reconcileOnce("rs-quiet")
			Expect(sql.executed).To(BeEmpty())
			Expect(roleStatus("rs-quiet", appRole).State).To(Equal(dbv1alpha1.RoleReady))

			dials := len(sql.dials)
			reconcileOnce("rs-quiet")
			reconcileOnce("rs-quiet")
			Expect(sql.dials).To(HaveLen(dials), "logging in and reading on every reconcile would fill the audit log")
		})

		It("looks again at once when the spec changes", func() {
			ready("rs-spec", dbv1alpha1.RoleSpec{Name: appRole})
			observe(observedRoles{})
			reconcileOnce("rs-spec")
			dials := len(sql.dials)
			update("rs-spec", func(c *dbv1alpha1.PostgresCluster) {
				c.Spec.Roles = append(c.Spec.Roles, dbv1alpha1.RoleSpec{Name: secondRole})
			})
			reconcileOnce("rs-spec")
			Expect(len(sql.dials)).To(BeNumerically(">", dials))
			Expect(roleStatus("rs-spec", "second").State).To(Equal(dbv1alpha1.RoleReady))
			Expect(strings.Join(sql.executed, "\n")).To(ContainSubstring(`create role "second"`))
		})

		It("looks again at once when a Secret changes, and sets the role to the new password", func() {
			ready("rs-pw", dbv1alpha1.RoleSpec{Name: appRole})
			password := string(secretOf("rs-pw-role-app").Data[secretKeyPassword])
			verifier, _ := newScramVerifier(password)
			observe(observedRoles{Roles: []observedRole{{
				Name: appRole, Comment: roleMarker(get("rs-pw")), Login: true, Limit: -1, Password: verifier,
			}}})
			reconcileOnce("rs-pw")
			Expect(sql.executed).To(BeEmpty())

			secret := secretOf("rs-pw-role-app")
			secret.Data[secretKeyPassword] = []byte("typed-in-by-someone-who-knows-best-0123456789")
			Expect(k8sClient.Update(ctx, secret)).To(Succeed())
			reconcileOnce("rs-pw")
			Expect(sql.executed).To(HaveLen(1))
			Expect(verifierMatches(verifierInSQL.FindString(sql.executed[0]), "typed-in-by-someone-who-knows-best-0123456789")).To(BeTrue())
		})

		It("asks for the verifiers of the spec's roles only", func() {
			ready("rs-only", dbv1alpha1.RoleSpec{Name: appRole})
			observe(observedRoles{})
			reconcileOnce("rs-only")
			Expect(sql.queried).To(HaveLen(1))
			Expect(sql.queried[0]).To(ContainSubstring("r.rolname in ('app')"))
		})
	})

	Describe("removing a role from the spec", func() {
		It("disables its login and ends its sessions, drops nothing, and keeps listing it", func() {
			ready("rs-off", dbv1alpha1.RoleSpec{Name: appRole}, dbv1alpha1.RoleSpec{Name: "old_app"})
			observe(observedRoles{})
			reconcileOnce("rs-off")
			Expect(roleStatus("rs-off", "old_app").State).To(Equal(dbv1alpha1.RoleReady))

			observe(observedRoles{Roles: []observedRole{
				{Name: appRole, Comment: roleMarker(get("rs-off")), Login: true, Limit: -1},
				{Name: "old_app", Comment: roleMarker(get("rs-off")), Login: true, Limit: -1},
			}})
			sql.executed = nil
			update("rs-off", func(c *dbv1alpha1.PostgresCluster) { c.Spec.Roles = c.Spec.Roles[:1] })
			reconcileOnce("rs-off")

			text := strings.Join(sql.executed, "\n")
			Expect(text).To(ContainSubstring(`alter role "old_app" nologin`))
			Expect(text).To(ContainSubstring("pg_terminate_backend"))
			Expect(strings.ToLower(text)).NotTo(ContainSubstring("drop "))
			status := roleStatus("rs-off", "old_app")
			Expect(status.State).To(Equal(dbv1alpha1.RoleDisabled))
			Expect(status.Message).To(ContainSubstring("kept"))
			Expect(roleStatus("rs-off", appRole).State).To(Equal(dbv1alpha1.RoleReady))
			Expect(rolesCondition("rs-off").Status).To(Equal(metav1.ConditionTrue), "a disabled role is not a role that is not ready")
		})

		It("still looks when the spec has no roles left, and drops the condition", func() {
			ready("rs-none", dbv1alpha1.RoleSpec{Name: appRole})
			observe(observedRoles{})
			reconcileOnce("rs-none")
			Expect(rolesCondition("rs-none")).NotTo(BeNil())

			observe(observedRoles{Roles: []observedRole{{Name: appRole, Comment: roleMarker(get("rs-none")), Login: true, Limit: -1}}})
			sql.executed = nil
			update("rs-none", func(c *dbv1alpha1.PostgresCluster) { c.Spec.Roles = nil })
			reconcileOnce("rs-none")
			Expect(strings.Join(sql.executed, "\n")).To(ContainSubstring(`alter role "app" nologin`))
			Expect(roleStatus("rs-none", appRole).State).To(Equal(dbv1alpha1.RoleDisabled))
			Expect(rolesCondition("rs-none")).To(BeNil(), "no roles are asked for, so there is nothing to be ready")
		})
	})

	Describe("a cluster restored from backups", func() {
		It("disables the roles it inherited that its own spec does not list, though the spec lists none", func() {
			Expect(k8sClient.Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: auditSecret, Namespace: namespace},
				StringData: map[string]string{secretKeyAccessKey: "id", secretKeySecretKey: testSecretValue},
			})).To(Or(Succeed(), MatchError(ContainSubstring("already exists"))))
			DeferCleanup(func() {
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: auditSecret, Namespace: namespace}}))).To(Succeed())
			})
			c := plainCluster()
			c.Name = "rs-restored"
			c.Spec.Storage = dbv1alpha1.StorageSpec{Size: resource.MustParse("1Gi")}
			c.Spec.Resources = corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}}
			c.Spec.Bootstrap = &dbv1alpha1.BootstrapSpec{Recovery: &dbv1alpha1.RecoverySpec{
				SourceCluster: "source",
				Destination:   dbv1alpha1.BackupDestination{Bucket: testBucket, Region: testRegion, CredentialsSecret: auditSecret},
			}}
			Expect(k8sClient.Create(ctx, c)).To(Succeed())
			DeferCleanup(func() {
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, c))).To(Succeed())
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: c.Name + "-credentials", Namespace: namespace}}))).To(Succeed())
			})
			// A restored cluster first checks the backups it restores from, and
			// is not reconciled further until that passes. What is wanted here
			// is the step that comes after, so it is called directly.
			credentials := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "rs-restored-credentials", Namespace: namespace},
				Data:       map[string][]byte{secretKeyUsername: []byte(testSuperuserName), secretKeyPassword: []byte("the-superuser-password")},
			}
			Expect(k8sClient.Create(ctx, credentials)).To(Succeed())
			startLeader("rs-restored")
			sql.superuserPassword = "the-superuser-password"
			observe(observedRoles{Roles: []observedRole{
				{Name: "from_the_source", Comment: roleMarkerPrefix + "default/source", Login: true, Limit: -1},
			}})
			ok, err := rec.reconcileRoles(ctx, get("rs-restored"), nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(ok).To(BeTrue())
			Expect(strings.Join(sql.executed, "\n")).To(ContainSubstring(`alter role "from_the_source" nologin`))
			Expect(roleStatus("rs-restored", "from_the_source").State).To(Equal(dbv1alpha1.RoleDisabled))
		})

		It("is the only kind of cluster with no roles that looks at all", func() {
			plain := plainCluster()
			Expect(rolesWanted(plain)).To(BeFalse())
			plain.Status.AccessControl.Roles = []dbv1alpha1.RoleStatus{{Name: "x", State: dbv1alpha1.RoleDisabled}}
			Expect(rolesWanted(plain)).To(BeTrue())
			plain.Status.AccessControl.Roles = nil
			plain.Spec.Roles = []dbv1alpha1.RoleSpec{{Name: "x"}}
			Expect(rolesWanted(plain)).To(BeTrue())
			plain.Spec.Roles = nil
			plain.Spec.Bootstrap = &dbv1alpha1.BootstrapSpec{Recovery: &dbv1alpha1.RecoverySpec{SourceCluster: "s"}}
			Expect(rolesWanted(plain)).To(BeTrue())
		})

		It("leaves a plain cluster with no roles alone, whatever is in its database", func() {
			ready("rs-plain")
			observe(observedRoles{Roles: []observedRole{{Name: "x", Comment: roleMarkerPrefix + "default/other", Login: true, Limit: -1}}})
			reconcileOnce("rs-plain")
			Expect(sql.dials).To(BeEmpty())
			Expect(sql.executed).To(BeEmpty())
		})
	})

	Describe("when it goes wrong", func() {
		It("waits, with a reason, while there is no primary, and looks again soon", func() {
			create("rs-nolead", dbv1alpha1.RoleSpec{Name: appRole})
			res := reconcileOnce("rs-nolead")
			Expect(sql.dials).To(BeEmpty())
			status := roleStatus("rs-nolead", appRole)
			Expect(status.State).To(Equal(dbv1alpha1.RolePending))
			Expect(status.Message).To(ContainSubstring("no primary"))
			Expect(res.RequeueAfter).To(Equal(unhealthyRefreshInterval))
		})

		It("reports a database it cannot reach on port 5432, for every role", func() {
			ready("rs-down", dbv1alpha1.RoleSpec{Name: "a"}, dbv1alpha1.RoleSpec{Name: "b"})
			sql.unreachable = errors.New("dial tcp 10.20.30.40:5432: i/o timeout")
			res := reconcileOnce("rs-down")
			for _, role := range []string{"a", "b"} {
				status := roleStatus("rs-down", role)
				Expect(status.State).To(Equal(dbv1alpha1.RoleError), role)
				Expect(status.Message).To(ContainSubstring("port 5432"))
				Expect(status.Message).To(ContainSubstring("i/o timeout"))
			}
			Expect(rolesCondition("rs-down").Status).To(Equal(metav1.ConditionFalse))
			Expect(res.RequeueAfter).To(Equal(unhealthyRefreshInterval))
		})

		It("reports a superuser the database refuses", func() {
			ready("rs-su", dbv1alpha1.RoleSpec{Name: appRole})
			sql.superuserPassword = "not-the-secrets"
			reconcileOnce("rs-su")
			Expect(roleStatus("rs-su", appRole).State).To(Equal(dbv1alpha1.RoleError))
			Expect(sql.executed).To(BeEmpty())
		})

		It("reports an answer it cannot read", func() {
			ready("rs-junk", dbv1alpha1.RoleSpec{Name: appRole})
			sql.observed = "this is not json"
			reconcileOnce("rs-junk")
			status := roleStatus("rs-junk", appRole)
			Expect(status.State).To(Equal(dbv1alpha1.RoleError))
			Expect(status.Message).To(ContainSubstring("could not read the roles"))
		})

		It("reports the role a statement failed for, and goes on with the others", func() {
			ready("rs-part", dbv1alpha1.RoleSpec{Name: "good"}, dbv1alpha1.RoleSpec{Name: "bad"}, dbv1alpha1.RoleSpec{Name: "also_good"})
			observe(observedRoles{})
			sql.execFailure = func(statement string) error {
				if strings.Contains(statement, `"bad"`) {
					return errors.New("permission denied")
				}
				return nil
			}
			res := reconcileOnce("rs-part")
			Expect(roleStatus("rs-part", "good").State).To(Equal(dbv1alpha1.RoleReady))
			Expect(roleStatus("rs-part", "also_good").State).To(Equal(dbv1alpha1.RoleReady))
			bad := roleStatus("rs-part", "bad")
			Expect(bad.State).To(Equal(dbv1alpha1.RoleError))
			Expect(bad.Message).To(ContainSubstring("could not apply the change: permission denied"))
			condition := rolesCondition("rs-part")
			Expect(condition.Status).To(Equal(metav1.ConditionFalse))
			Expect(condition.Message).To(ContainSubstring("1 of 3 roles are not ready (bad)"))
			Expect(res.RequeueAfter).To(Equal(unhealthyRefreshInterval))
		})

		It("does not put a statement's text in the status when it fails", func() {
			ready("rs-leak", dbv1alpha1.RoleSpec{Name: appRole})
			observe(observedRoles{})
			sql.execFailure = func(statement string) error { return errors.New("failed") }
			reconcileOnce("rs-leak")
			Expect(roleStatus("rs-leak", appRole).Message).NotTo(ContainSubstring("SCRAM"))
		})

		It("tries again at the next reconcile instead of trusting a failed one", func() {
			ready("rs-retry", dbv1alpha1.RoleSpec{Name: appRole})
			observe(observedRoles{})
			sql.execFailure = func(string) error { return errors.New("failed") }
			reconcileOnce("rs-retry")
			sql.execFailure = nil
			sql.executed = nil
			reconcileOnce("rs-retry")
			Expect(sql.executed).NotTo(BeEmpty())
			Expect(roleStatus("rs-retry", appRole).State).To(Equal(dbv1alpha1.RoleReady))
		})
	})

	Describe("a spec the operator will not act on", func() {
		It("is reported as InvalidRoles, and nothing is made", func() {
			create("rs-bad", dbv1alpha1.RoleSpec{Name: testSuperuserName})
			reconcileOnce("rs-bad")
			condition := meta.FindStatusCondition(get("rs-bad").Status.Conditions, "Progressing")
			Expect(condition).NotTo(BeNil())
			Expect(condition.Reason).To(Equal("InvalidRoles"))
			Expect(condition.Message).To(ContainSubstring("spec.roles[0].name"))
			err := k8sClient.Get(ctx, key("rs-bad-role-postgres"), &corev1.Secret{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
			Expect(sql.dials).To(BeEmpty())
		})
	})

	Describe("without the SQL channel", func() {
		It("makes the Secrets and nothing else", func() {
			create("rs-nosql", dbv1alpha1.RoleSpec{Name: appRole})
			rec.SQL = nil
			reconcileOnce("rs-nosql")
			startLeader("rs-nosql")
			reconcileOnce("rs-nosql")
			Expect(secretOf("rs-nosql-role-app").Data).To(HaveKey(secretKeyPassword))
			Expect(get("rs-nosql").Status.AccessControl.Roles).To(BeEmpty())
		})
	})

	Describe("the metric", func() {
		It("counts the roles in each state", func() {
			ready("rs-metric", dbv1alpha1.RoleSpec{Name: "a"}, dbv1alpha1.RoleSpec{Name: "b"})
			observe(observedRoles{})
			sql.execFailure = func(statement string) error {
				if strings.Contains(statement, `"b"`) {
					return errors.New("no")
				}
				return nil
			}
			reconcileOnce("rs-metric")
			Expect(testutil.ToFloat64(managedRoles.WithLabelValues(namespace, "rs-metric", dbv1alpha1.RoleReady))).To(Equal(float64(1)))
			Expect(testutil.ToFloat64(managedRoles.WithLabelValues(namespace, "rs-metric", dbv1alpha1.RoleError))).To(Equal(float64(1)))
			Expect(testutil.ToFloat64(managedRoles.WithLabelValues(namespace, "rs-metric", dbv1alpha1.RoleDisabled))).To(Equal(float64(0)))
		})
	})
})
