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
	"errors"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	dbv1alpha1 "github.com/tusharshrivastava62/postgres-operator/api/v1alpha1"
)

const (
	alice       = "alice@example.com"
	reasonOrder = "orders database is out of connections and the application is down"
)

var _ = Describe("Emergency access", func() {
	const namespace = "default"
	ctx := context.Background()
	key := func(name string) types.NamespacedName { return types.NamespacedName{Name: name, Namespace: namespace} }

	var (
		sql      *fakeSQL
		rec      *PostgresEmergencyAccessReconciler
		recorder *events.FakeRecorder
		clock    time.Time
	)
	BeforeEach(func() {
		sql = &fakeSQL{}
		recorder = events.NewFakeRecorder(50)
		clock = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
		rec = &PostgresEmergencyAccessReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), SQL: sql, Recorder: recorder, Now: func() time.Time { return clock },
		}
		sql.textFor = func(_, query string) string {
			if query == listDatabasesSQL {
				return `["orders","` + monitorDatabase + `"]`
			}
			return "[]"
		}
		sql.boolFor = func(_, _ string) bool { return true }
	})

	reconcileOnce := func(name string) reconcile.Result {
		GinkgoHelper()
		res, err := rec.Reconcile(ctx, reconcile.Request{NamespacedName: key(name)})
		Expect(err).NotTo(HaveOccurred())
		return res
	}
	get := func(name string) *dbv1alpha1.PostgresEmergencyAccess {
		GinkgoHelper()
		r := &dbv1alpha1.PostgresEmergencyAccess{}
		Expect(k8sClient.Get(ctx, key(name), r)).To(Succeed())
		return r
	}
	secretOf := func(name string) (*corev1.Secret, error) {
		s := &corev1.Secret{}
		err := k8sClient.Get(ctx, key(name), s)
		return s, err
	}
	// cluster makes a cluster and what the operator needs to reach its primary.
	cluster := func(name string, access *dbv1alpha1.EmergencyAccessSpec) {
		GinkgoHelper()
		c := plainCluster()
		c.Name = name
		c.Spec.Storage = dbv1alpha1.StorageSpec{Size: resource.MustParse("1Gi")}
		c.Spec.Resources = corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}}
		c.Spec.EmergencyAccess = access
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, c))).To(Succeed()) })

		creds := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name + "-credentials", Namespace: namespace},
			Data:       map[string][]byte{secretKeyUsername: []byte(testSuperuserName), secretKeyPassword: []byte("the-superuser-password")},
		}
		Expect(k8sClient.Create(ctx, creds)).To(Succeed())
		DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, creds))).To(Succeed()) })
		sql.superuserPassword = "the-superuser-password"

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
	allowing := &dbv1alpha1.EmergencyAccessSpec{Enabled: true, MaxDuration: &metav1.Duration{Duration: 2 * time.Hour}}

	// request makes a request, stamped as the webhook would stamp it.
	request := func(name, clusterName, by string, mutate ...func(*dbv1alpha1.PostgresEmergencyAccess)) {
		GinkgoHelper()
		r := &dbv1alpha1.PostgresEmergencyAccess{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: dbv1alpha1.PostgresEmergencyAccessSpec{
				ClusterRef: dbv1alpha1.ClusterReference{Name: clusterName},
				Duration:   metav1.Duration{Duration: 30 * time.Minute},
				Reason:     reasonOrder,
			},
		}
		if by != "" {
			r.Annotations = map[string]string{dbv1alpha1.AnnotationRequestedBy: by}
		}
		for _, m := range mutate {
			m(r)
		}
		Expect(k8sClient.Create(ctx, r)).To(Succeed())
		DeferCleanup(func() {
			latest := &dbv1alpha1.PostgresEmergencyAccess{}
			if err := k8sClient.Get(ctx, key(name), latest); err == nil {
				controllerutil.RemoveFinalizer(latest, finalizerEmergencyAccess)
				Expect(k8sClient.Update(ctx, latest)).To(Succeed())
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, latest))).To(Succeed())
			}
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name + "-access", Namespace: namespace}}))).To(Succeed())
		})
	}
	statementsIn := func(database string) string {
		var out []string
		for _, r := range sql.ran {
			if r.database == database {
				out = append(out, r.sql)
			}
		}
		return strings.Join(out, "\n")
	}
	sawEvent := func(parts ...string) bool {
		for {
			select {
			case e := <-recorder.Events:
				ok := true
				for _, p := range parts {
					ok = ok && strings.Contains(e, p)
				}
				if ok {
					return true
				}
			default:
				return false
			}
		}
	}

	Describe("granting", func() {
		It("makes a role, a login for it, and the right to become the superuser in every database, then says when it ends", func() {
			cluster("ea-grant", allowing)
			request("ea-grant-1", "ea-grant", alice)
			res := reconcileOnce("ea-grant-1")

			got := get("ea-grant-1")
			Expect(got.Status.State).To(Equal(dbv1alpha1.EmergencyActive))
			Expect(got.Status.RequestedBy).To(Equal(alice))
			role := emergencyRoleName(got.UID)
			Expect(role).To(MatchRegexp(`^bg_[0-9a-f]{8}$`))
			Expect(got.Status.Role).To(Equal(role))
			Expect(got.Status.SecretName).To(Equal("ea-grant-1-access"))
			Expect(got.Status.StartedAt.Time).To(BeTemporally("~", clock, time.Second))
			Expect(got.Status.ExpiresAt.Time).To(BeTemporally("~", clock.Add(30*time.Minute), time.Second))
			Expect(got.Status.Message).To(ContainSubstring("Active until"))
			Expect(got.Status.Message).To(ContainSubstring("SELECT set_user_u('postgres')"))
			Expect(got.Status.Message).To(ContainSubstring("WARNING:"))
			Expect(got.Status.Message).To(ContainSubstring("clear text"))
			Expect(got.Finalizers).To(ContainElement(finalizerEmergencyAccess))
			Expect(res.RequeueAfter).To(BeNumerically("~", 30*time.Minute, 5*time.Second), "look again when it ends")

			By("a login in a Secret that the request owns")
			secret, err := secretOf("ea-grant-1-access")
			Expect(err).NotTo(HaveOccurred())
			Expect(string(secret.Data[secretKeyUsername])).To(Equal(role))
			Expect(len(secret.Data[secretKeyPassword])).To(BeNumerically(">=", 32))
			Expect(string(secret.Data[secretKeyHost])).To(Equal("ea-grant.default.svc"))
			Expect(string(secret.Data["warning"])).To(Equal(dbv1alpha1.ParameterLoggingWarning))
			Expect(secret.OwnerReferences).To(HaveLen(1))
			Expect(secret.OwnerReferences[0].Kind).To(Equal("PostgresEmergencyAccess"))
			Expect(secret.OwnerReferences[0].Name).To(Equal("ea-grant-1"))

			By("a role that can log in until the end and do nothing, with a limit on connections")
			setup := statementsIn(monitorDatabase)
			Expect(setup).To(ContainSubstring("nosuperuser nocreatedb nocreaterole noreplication nobypassrls"))
			Expect(setup).To(ContainSubstring("connection limit 2"))
			Expect(setup).To(ContainSubstring("valid until '2026-10-24 12:30:00+00'"))
			Expect(setup).To(ContainSubstring("comment on role \"" + role + "\" is 'Emergency access for default/ea-grant: request ea-grant-1'"))
			Expect(setup).NotTo(ContainSubstring(string(secret.Data[secretKeyPassword])), "never the password itself")
			Expect(verifierMatches(verifierInSQL.FindString(setup), string(secret.Data[secretKeyPassword]))).To(BeTrue())

			By("the right to become the superuser, and to connect, in each database")
			for _, db := range []string{"orders", monitorDatabase} {
				granted := statementsIn(db)
				Expect(granted).To(ContainSubstring("create extension if not exists set_user"), db)
				Expect(granted).To(ContainSubstring(`grant connect on database "`+db+`" to "`+role+`"`), db)
				Expect(granted).To(ContainSubstring(`grant execute on function set_user_u(text) to "`+role+`"`), db)
				Expect(granted).To(ContainSubstring(`grant execute on function reset_user() to "`+role+`"`), db)
			}
			Expect(sawEvent("AccessGranted", alice)).To(BeTrue())
		})

		It("connects to the primary on port 5432 as the superuser, to each database in turn", func() {
			cluster("ea-dial", allowing)
			request("ea-dial-1", "ea-dial", alice)
			reconcileOnce("ea-dial-1")
			databases := map[string]bool{}
			for _, d := range sql.dials {
				Expect(d.Host).To(Equal(testLeaderIP))
				Expect(d.Port).To(Equal(int32(5432)))
				Expect(d.User).To(Equal(testSuperuserName))
				databases[d.Database] = true
			}
			Expect(databases).To(HaveKey("orders"))
			Expect(databases).To(HaveKey(monitorDatabase))
		})

		It("grants what is asked, for as long as is asked", func() {
			cluster("ea-long", allowing)
			request("ea-long-1", "ea-long", alice, func(r *dbv1alpha1.PostgresEmergencyAccess) {
				r.Spec.Duration = metav1.Duration{Duration: 2 * time.Hour}
			})
			reconcileOnce("ea-long-1")
			Expect(get("ea-long-1").Status.ExpiresAt.Time).To(BeTemporally("~", clock.Add(2*time.Hour), time.Second))
			Expect(statementsIn(monitorDatabase)).To(ContainSubstring("valid until '2026-10-24 14:00:00+00'"))
		})

		It("uses the default of an hour when the request does not say", func() {
			cluster("ea-default", allowing)
			request("ea-default-1", "ea-default", alice, func(r *dbv1alpha1.PostgresEmergencyAccess) { r.Spec.Duration = metav1.Duration{} })
			reconcileOnce("ea-default-1")
			Expect(get("ea-default-1").Status.ExpiresAt.Time).To(BeTemporally("~", clock.Add(time.Hour), time.Second))
		})

		It("gives two requests two roles", func() {
			cluster("ea-two", allowing)
			request("ea-two-1", "ea-two", alice)
			request("ea-two-2", "ea-two", "bob")
			reconcileOnce("ea-two-1")
			reconcileOnce("ea-two-2")
			Expect(get("ea-two-1").Status.Role).NotTo(Equal(get("ea-two-2").Status.Role))
		})

		It("leaves an active grant alone when looked at again", func() {
			cluster("ea-again", allowing)
			request("ea-again-1", "ea-again", alice)
			reconcileOnce("ea-again-1")
			ran := len(sql.ran)
			password, _ := secretOf("ea-again-1-access")
			res := reconcileOnce("ea-again-1")
			Expect(sql.ran).To(HaveLen(ran), "nothing is done a second time")
			secret, _ := secretOf("ea-again-1-access")
			Expect(secret.Data[secretKeyPassword]).To(Equal(password.Data[secretKeyPassword]))
			Expect(res.RequeueAfter).To(BeNumerically(">", 29*time.Minute))
		})
	})

	Describe("refusing", func() {
		denied := func(name, why string) {
			GinkgoHelper()
			got := get(name)
			Expect(got.Status.State).To(Equal(dbv1alpha1.EmergencyDenied))
			Expect(got.Status.Message).To(ContainSubstring(why))
			Expect(got.Status.EndedAt).NotTo(BeNil())
			Expect(sql.ran).To(BeEmpty(), "nothing is done to the database")
			Expect(sql.dials).To(BeEmpty())
			_, err := secretOf(name + "-access")
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "no login is made")
			Expect(sawEvent("Warning", "AccessDenied")).To(BeTrue())
		}

		It("denies a request that names nobody", func() {
			cluster("ea-anon", allowing)
			request("ea-anon-1", "ea-anon", "")
			reconcileOnce("ea-anon-1")
			denied("ea-anon-1", "could not tell who made this request")
		})

		It("denies a request to a cluster that does not allow it", func() {
			cluster("ea-off", &dbv1alpha1.EmergencyAccessSpec{})
			request("ea-off-1", "ea-off", alice)
			reconcileOnce("ea-off-1")
			denied("ea-off-1", "does not allow emergency access")
		})

		It("denies a request to a cluster with no setting at all", func() {
			cluster("ea-unset", nil)
			request("ea-unset-1", "ea-unset", alice)
			reconcileOnce("ea-unset-1")
			denied("ea-unset-1", "does not allow emergency access")
		})

		It("denies a request for longer than the cluster allows", func() {
			cluster("ea-toolong", allowing)
			request("ea-toolong-1", "ea-toolong", alice, func(r *dbv1alpha1.PostgresEmergencyAccess) {
				r.Spec.Duration = metav1.Duration{Duration: 3 * time.Hour}
			})
			reconcileOnce("ea-toolong-1")
			denied("ea-toolong-1", "allows at most 2h0m0s")
		})

		It("does not look at a denied request again", func() {
			cluster("ea-final", &dbv1alpha1.EmergencyAccessSpec{})
			request("ea-final-1", "ea-final", alice)
			reconcileOnce("ea-final-1")
			cl := &dbv1alpha1.PostgresCluster{}
			Expect(k8sClient.Get(ctx, key("ea-final"), cl)).To(Succeed())
			cl.Spec.EmergencyAccess = allowing
			Expect(k8sClient.Update(ctx, cl)).To(Succeed())
			reconcileOnce("ea-final-1")
			Expect(get("ea-final-1").Status.State).To(Equal(dbv1alpha1.EmergencyDenied), "a refused request stays refused: ask again")
		})
	})

	Describe("a cluster that is not there yet, or cannot be reached", func() {
		It("waits for the cluster, and grants once it exists", func() {
			request("ea-late-1", "ea-late", alice)
			res := reconcileOnce("ea-late-1")
			got := get("ea-late-1")
			Expect(got.Status.State).To(Equal(dbv1alpha1.EmergencyFailed))
			Expect(got.Status.Message).To(ContainSubstring("there is no cluster ea-late"))
			Expect(res.RequeueAfter).To(Equal(emergencyRetryDelay))

			cluster("ea-late", allowing)
			reconcileOnce("ea-late-1")
			Expect(get("ea-late-1").Status.State).To(Equal(dbv1alpha1.EmergencyActive))
		})

		It("tries again when the database cannot be reached, with the same end time, and the same password", func() {
			cluster("ea-retry", allowing)
			request("ea-retry-1", "ea-retry", alice)
			sql.unreachable = errors.New("dial tcp 10.20.30.40:5432: i/o timeout")
			res := reconcileOnce("ea-retry-1")
			failed := get("ea-retry-1")
			Expect(failed.Status.State).To(Equal(dbv1alpha1.EmergencyFailed))
			Expect(failed.Status.Message).To(ContainSubstring("i/o timeout"))
			Expect(res.RequeueAfter).To(Equal(emergencyRetryDelay))
			first, _ := secretOf("ea-retry-1-access")

			clock = clock.Add(5 * time.Minute)
			sql.unreachable = nil
			reconcileOnce("ea-retry-1")
			got := get("ea-retry-1")
			Expect(got.Status.State).To(Equal(dbv1alpha1.EmergencyActive))
			Expect(got.Status.ExpiresAt.Time).To(Equal(failed.Status.ExpiresAt.Time), "the clock started at the first try, not the last")
			second, _ := secretOf("ea-retry-1-access")
			Expect(second.Data[secretKeyPassword]).To(Equal(first.Data[secretKeyPassword]))
		})

		It("says the time ran out if it could not grant until then", func() {
			cluster("ea-late-grant", allowing)
			request("ea-late-grant-1", "ea-late-grant", alice)
			sql.unreachable = errors.New("down")
			reconcileOnce("ea-late-grant-1")
			clock = clock.Add(31 * time.Minute)
			sql.unreachable = nil
			reconcileOnce("ea-late-grant-1")
			got := get("ea-late-grant-1")
			Expect(got.Status.State).To(Equal(dbv1alpha1.EmergencyExpired))
			Expect(got.Status.Message).To(ContainSubstring("before the access could be granted"))
		})

		It("has no access to grant without the SQL channel", func() {
			cluster("ea-nosql", allowing)
			request("ea-nosql-1", "ea-nosql", alice)
			rec.SQL = nil
			reconcileOnce("ea-nosql-1")
			Expect(get("ea-nosql-1").Status.State).To(Equal(dbv1alpha1.EmergencyFailed))
			Expect(get("ea-nosql-1").Status.Message).To(ContainSubstring("no SQL channel"))
		})

		It("reports a database that refuses the setup, and does not say it is active", func() {
			cluster("ea-refused", allowing)
			request("ea-refused-1", "ea-refused", alice)
			sql.execFailure = func(statement string) error {
				if strings.Contains(statement, "set_user_u") {
					return errors.New("permission denied")
				}
				return nil
			}
			reconcileOnce("ea-refused-1")
			got := get("ea-refused-1")
			Expect(got.Status.State).To(Equal(dbv1alpha1.EmergencyFailed))
			Expect(got.Status.Message).To(ContainSubstring("granting access to database"))
			Expect(got.Status.StartedAt).To(BeNil())
		})
	})

	Describe("ending", func() {
		activate := func(name, clusterName string) string {
			GinkgoHelper()
			cluster(clusterName, allowing)
			request(name, clusterName, alice)
			reconcileOnce(name)
			Expect(get(name).Status.State).To(Equal(dbv1alpha1.EmergencyActive))
			sql.ran, sql.dials = nil, nil
			return get(name).Status.Role
		}

		It("takes the access away when the time runs out: login and sessions first, then what was granted in each database, then the role", func() {
			role := activate("ea-expire-1", "ea-expire")
			clock = clock.Add(31 * time.Minute)
			reconcileOnce("ea-expire-1")

			got := get("ea-expire-1")
			Expect(got.Status.State).To(Equal(dbv1alpha1.EmergencyExpired))
			Expect(got.Status.EndedAt.Time).To(BeTemporally("~", clock, time.Second))
			Expect(got.Status.SecretName).To(BeEmpty())
			Expect(got.Status.Message).To(ContainSubstring("Expired: the time ran out"))
			_, err := secretOf("ea-expire-1-access")
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the login is deleted")

			var order []string
			for _, r := range sql.ran {
				switch {
				case strings.Contains(r.sql, "nologin"):
					order = append(order, "end in "+r.database)
				case strings.HasPrefix(r.sql, "drop owned by"):
					order = append(order, "take back in "+r.database)
				case strings.HasPrefix(r.sql, "drop role"):
					order = append(order, "drop in "+r.database)
				}
			}
			Expect(order).To(Equal([]string{"end in " + monitorDatabase, "take back in orders", "take back in " + monitorDatabase, "drop in " + monitorDatabase}))
			all := statementsIn(monitorDatabase)
			Expect(all).To(ContainSubstring(`alter role "` + role + `" nologin valid until '1970-01-01 00:00:00+00'`))
			Expect(all).To(ContainSubstring("pg_terminate_backend"))
			Expect(all).To(ContainSubstring("usename = '" + role + "'"))
			Expect(sawEvent("AccessEnded", "Expired")).To(BeTrue())
		})

		It("does nothing before the time is up", func() {
			activate("ea-early-1", "ea-early")
			clock = clock.Add(29 * time.Minute)
			res := reconcileOnce("ea-early-1")
			Expect(get("ea-early-1").Status.State).To(Equal(dbv1alpha1.EmergencyActive))
			Expect(sql.ran).To(BeEmpty())
			Expect(res.RequeueAfter).To(BeNumerically("~", time.Minute, 5*time.Second))
		})

		It("ends at the very moment, not a moment after", func() {
			activate("ea-exact-1", "ea-exact")
			clock = clock.Add(30 * time.Minute)
			reconcileOnce("ea-exact-1")
			Expect(get("ea-exact-1").Status.State).To(Equal(dbv1alpha1.EmergencyExpired))
		})

		It("revokes at once when the cluster stops allowing it", func() {
			activate("ea-switch-1", "ea-switch")
			cl := &dbv1alpha1.PostgresCluster{}
			Expect(k8sClient.Get(ctx, key("ea-switch"), cl)).To(Succeed())
			cl.Spec.EmergencyAccess = &dbv1alpha1.EmergencyAccessSpec{}
			Expect(k8sClient.Update(ctx, cl)).To(Succeed())
			reconcileOnce("ea-switch-1")
			got := get("ea-switch-1")
			Expect(got.Status.State).To(Equal(dbv1alpha1.EmergencyRevoked))
			Expect(got.Status.Message).To(ContainSubstring("no longer allows"))
			Expect(statementsIn(monitorDatabase)).To(ContainSubstring("drop role"))
		})

		It("revokes when the cluster is gone, with nothing to connect to", func() {
			activate("ea-gone-1", "ea-gone")
			cl := &dbv1alpha1.PostgresCluster{}
			Expect(k8sClient.Get(ctx, key("ea-gone"), cl)).To(Succeed())
			Expect(k8sClient.Delete(ctx, cl)).To(Succeed())
			reconcileOnce("ea-gone-1")
			Expect(get("ea-gone-1").Status.State).To(Equal(dbv1alpha1.EmergencyRevoked))
			Expect(sql.ran).To(BeEmpty())
			_, err := secretOf("ea-gone-1-access")
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		})

		It("stays active, and says so, when the access cannot be taken away yet", func() {
			activate("ea-stuck-1", "ea-stuck")
			clock = clock.Add(31 * time.Minute)
			sql.unreachable = errors.New("i/o timeout")
			res := reconcileOnce("ea-stuck-1")
			got := get("ea-stuck-1")
			Expect(got.Status.State).To(Equal(dbv1alpha1.EmergencyActive), "it is not over until it is taken away")
			Expect(got.Status.Message).To(ContainSubstring("could not be taken away yet"))
			Expect(res.RequeueAfter).To(Equal(emergencyRetryDelay))
			Expect(sawEvent("Warning", "AccessFailed")).To(BeTrue())

			sql.unreachable = nil
			reconcileOnce("ea-stuck-1")
			Expect(get("ea-stuck-1").Status.State).To(Equal(dbv1alpha1.EmergencyExpired))
		})

		It("does not touch an ended request again", func() {
			activate("ea-done-1", "ea-done")
			clock = clock.Add(time.Hour)
			reconcileOnce("ea-done-1")
			sql.ran, sql.dials = nil, nil
			reconcileOnce("ea-done-1")
			Expect(sql.ran).To(BeEmpty())
			Expect(sql.dials).To(BeEmpty())
		})

		It("makes the login again, with a new password, if it is deleted, and keeps the end time", func() {
			activate("ea-lost-1", "ea-lost")
			before, _ := secretOf("ea-lost-1-access")
			expires := get("ea-lost-1").Status.ExpiresAt.Time
			Expect(k8sClient.Delete(ctx, before)).To(Succeed())
			reconcileOnce("ea-lost-1")
			after, err := secretOf("ea-lost-1-access")
			Expect(err).NotTo(HaveOccurred())
			Expect(after.Data[secretKeyPassword]).NotTo(Equal(before.Data[secretKeyPassword]))
			Expect(get("ea-lost-1").Status.ExpiresAt.Time).To(Equal(expires))
			Expect(verifierMatches(verifierInSQL.FindString(statementsIn(monitorDatabase)), string(after.Data[secretKeyPassword]))).To(BeTrue(),
				"the role has the new password")
		})
	})

	Describe("deleting a request", func() {
		It("takes the access away first, and then lets the request go", func() {
			cluster("ea-del", allowing)
			request("ea-del-1", "ea-del", alice)
			reconcileOnce("ea-del-1")
			role := get("ea-del-1").Status.Role
			sql.ran = nil
			Expect(k8sClient.Delete(ctx, get("ea-del-1"))).To(Succeed())
			Expect(get("ea-del-1").Finalizers).To(ContainElement(finalizerEmergencyAccess), "it is held")
			reconcileOnce("ea-del-1")
			Expect(statementsIn(monitorDatabase)).To(ContainSubstring(`drop role "` + role + `"`))
			err := k8sClient.Get(ctx, key("ea-del-1"), &dbv1alpha1.PostgresEmergencyAccess{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "and then it is gone")
			Expect(sawEvent("AccessEnded", "request was deleted")).To(BeTrue())
		})

		It("is held while the access cannot be taken away", func() {
			cluster("ea-del-held", allowing)
			request("ea-del-held-1", "ea-del-held", alice)
			reconcileOnce("ea-del-held-1")
			Expect(k8sClient.Delete(ctx, get("ea-del-held-1"))).To(Succeed())
			sql.unreachable = errors.New("down")
			res := reconcileOnce("ea-del-held-1")
			Expect(res.RequeueAfter).To(Equal(emergencyRetryDelay))
			Expect(get("ea-del-held-1").Finalizers).To(ContainElement(finalizerEmergencyAccess))

			sql.unreachable = nil
			reconcileOnce("ea-del-held-1")
			err := k8sClient.Get(ctx, key("ea-del-held-1"), &dbv1alpha1.PostgresEmergencyAccess{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		})

		It("goes without delay when nothing was granted", func() {
			cluster("ea-del-denied", &dbv1alpha1.EmergencyAccessSpec{})
			request("ea-del-denied-1", "ea-del-denied", alice)
			reconcileOnce("ea-del-denied-1")
			Expect(k8sClient.Delete(ctx, get("ea-del-denied-1"))).To(Succeed())
			reconcileOnce("ea-del-denied-1")
			Expect(sql.dials).To(BeEmpty())
			err := k8sClient.Get(ctx, key("ea-del-denied-1"), &dbv1alpha1.PostgresEmergencyAccess{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		})

		It("goes when the cluster is gone, which leaves nothing to take away", func() {
			cluster("ea-del-nocluster", allowing)
			request("ea-del-nocluster-1", "ea-del-nocluster", alice)
			reconcileOnce("ea-del-nocluster-1")
			cl := &dbv1alpha1.PostgresCluster{}
			Expect(k8sClient.Get(ctx, key("ea-del-nocluster"), cl)).To(Succeed())
			Expect(k8sClient.Delete(ctx, cl)).To(Succeed())
			sql.dials = nil
			Expect(k8sClient.Delete(ctx, get("ea-del-nocluster-1"))).To(Succeed())
			reconcileOnce("ea-del-nocluster-1")
			Expect(sql.dials).To(BeEmpty())
			err := k8sClient.Get(ctx, key("ea-del-nocluster-1"), &dbv1alpha1.PostgresEmergencyAccess{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		})
	})

	Describe("which requests a cluster's change concerns", func() {
		It("finds the requests that name it, and no others", func() {
			cluster("ea-map", allowing)
			cluster("ea-map-other", allowing)
			request("ea-map-1", "ea-map", alice)
			request("ea-map-2", "ea-map", "bob")
			request("ea-map-3", "ea-map-other", alice)
			cl := &dbv1alpha1.PostgresCluster{}
			Expect(k8sClient.Get(ctx, key("ea-map"), cl)).To(Succeed())
			found := rec.requestsOfCluster(ctx, cl)
			names := make([]string, 0, len(found))
			for _, r := range found {
				names = append(names, r.Name)
			}
			Expect(names).To(ConsistOf("ea-map-1", "ea-map-2"))
		})
	})

	Describe("a duration written the way people write it", func() {
		It("is taken back by the schema when the operator writes it the way Go does", func() {
			cluster("ea-text", allowing)
			raw := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "db.youroperator.io/v1alpha1", "kind": "PostgresEmergencyAccess",
				"metadata": map[string]any{
					labelName: "ea-text-1", "namespace": namespace,
					"annotations": map[string]any{dbv1alpha1.AnnotationRequestedBy: alice},
				},
				// "30m", where Go writes "30m0s".
				"spec": map[string]any{"clusterRef": map[string]any{labelName: "ea-text"}, "duration": "30m", "reason": reasonOrder},
			}}
			Expect(k8sClient.Create(ctx, raw)).To(Succeed())
			DeferCleanup(func() {
				latest := &dbv1alpha1.PostgresEmergencyAccess{}
				if err := k8sClient.Get(ctx, key("ea-text-1"), latest); err == nil {
					controllerutil.RemoveFinalizer(latest, finalizerEmergencyAccess)
					Expect(k8sClient.Update(ctx, latest)).To(Succeed())
					Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, latest))).To(Succeed())
				}
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "ea-text-1-access", Namespace: namespace}}))).To(Succeed())
			})
			reconcileOnce("ea-text-1")
			got := get("ea-text-1")
			Expect(got.Finalizers).To(ContainElement(finalizerEmergencyAccess), "the finalizer was added")
			Expect(got.Status.State).To(Equal(dbv1alpha1.EmergencyActive))
			Expect(got.Spec.Duration.Duration).To(Equal(30 * time.Minute))
		})
	})

	Describe("the cluster's side", func() {
		var clusterRec *PostgresClusterReconciler
		BeforeEach(func() {
			clusterRec = &PostgresClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), SQL: sql}
		})
		getCluster := func(name string) *dbv1alpha1.PostgresCluster {
			GinkgoHelper()
			c := &dbv1alpha1.PostgresCluster{}
			Expect(k8sClient.Get(ctx, key(name), c)).To(Succeed())
			return c
		}
		// state puts a request in a state, as the controller would.
		state := func(name, to string) {
			GinkgoHelper()
			r := get(name)
			r.Status.State = to
			Expect(k8sClient.Status().Update(ctx, r)).To(Succeed())
		}
		orphans := func(roles ...string) {
			sql.textFor = func(_, query string) string {
				switch query {
				case listDatabasesSQL:
					return `["orders","` + monitorDatabase + `"]`
				case emergencyRolesSQL:
					quoted := make([]string, len(roles))
					for i, r := range roles {
						quoted[i] = `"` + r + `"`
					}
					return "[" + strings.Join(quoted, ",") + "]"
				}
				return "[]"
			}
		}
		ran := func(contains string) bool {
			for _, r := range sql.ran {
				if strings.Contains(r.sql, contains) {
					return true
				}
			}
			return false
		}

		It("does nothing for a cluster that neither allows emergency access nor is a restore", func() {
			cluster("ea-c-off", nil)
			c := getCluster("ea-c-off")
			meta.SetStatusCondition(&c.Status.Conditions, metav1.Condition{
				Type: dbv1alpha1.ConditionEmergencyAccessActive, Status: metav1.ConditionTrue, Reason: "left", Message: "left"})
			Expect(clusterRec.reconcileEmergencyAccess(ctx, c)).To(Succeed())
			Expect(sql.dials).To(BeEmpty())
			Expect(meta.FindStatusCondition(getCluster("ea-c-off").Status.Conditions, dbv1alpha1.ConditionEmergencyAccessActive)).To(BeNil(),
				"a condition left from when it was on goes")
		})

		It("says no grant is active, then which are, in a condition and a metric", func() {
			cluster("ea-c-cond", allowing)
			Expect(clusterRec.reconcileEmergencyAccess(ctx, getCluster("ea-c-cond"))).To(Succeed())
			condition := meta.FindStatusCondition(getCluster("ea-c-cond").Status.Conditions, dbv1alpha1.ConditionEmergencyAccessActive)
			Expect(condition).NotTo(BeNil())
			Expect(condition.Status).To(Equal(metav1.ConditionFalse))
			Expect(testutil.ToFloat64(emergencyAccessActive.WithLabelValues(namespace, "ea-c-cond"))).To(BeZero())

			request("ea-c-cond-b", "ea-c-cond", "bob")
			request("ea-c-cond-a", "ea-c-cond", alice)
			request("ea-c-cond-over", "ea-c-cond", alice)
			request("ea-c-cond-elsewhere", "ea-c-cond-other", alice)
			state("ea-c-cond-a", dbv1alpha1.EmergencyActive)
			state("ea-c-cond-b", dbv1alpha1.EmergencyActive)
			state("ea-c-cond-over", dbv1alpha1.EmergencyExpired)
			state("ea-c-cond-elsewhere", dbv1alpha1.EmergencyActive)
			Expect(clusterRec.reconcileEmergencyAccess(ctx, getCluster("ea-c-cond"))).To(Succeed())
			condition = meta.FindStatusCondition(getCluster("ea-c-cond").Status.Conditions, dbv1alpha1.ConditionEmergencyAccessActive)
			Expect(condition.Status).To(Equal(metav1.ConditionTrue))
			Expect(condition.Message).To(Equal("2 active: ea-c-cond-a, ea-c-cond-b"))
			Expect(testutil.ToFloat64(emergencyAccessActive.WithLabelValues(namespace, "ea-c-cond"))).To(Equal(float64(2)))
		})

		It("removes a role that no request owns, taking back what it was granted", func() {
			cluster("ea-c-orphan", allowing)
			startLeaderFor := getCluster("ea-c-orphan")
			orphans("bg_deadbeef")
			Expect(clusterRec.reconcileEmergencyAccess(ctx, startLeaderFor)).To(Succeed())
			Expect(ran(`alter role "bg_deadbeef" nologin`)).To(BeTrue())
			Expect(ran(`drop owned by "bg_deadbeef"`)).To(BeTrue())
			Expect(ran(`drop role "bg_deadbeef"`)).To(BeTrue())
		})

		It("keeps the role of a request in progress, and removes that of one that is over", func() {
			cluster("ea-c-keep", allowing)
			request("ea-c-keep-live", "ea-c-keep", alice)
			request("ea-c-keep-over", "ea-c-keep", alice)
			state("ea-c-keep-live", dbv1alpha1.EmergencyActive)
			state("ea-c-keep-over", dbv1alpha1.EmergencyExpired)
			live, over := emergencyRoleName(get("ea-c-keep-live").UID), emergencyRoleName(get("ea-c-keep-over").UID)
			orphans(live, over, "bg_stranger")
			Expect(clusterRec.reconcileEmergencyAccess(ctx, getCluster("ea-c-keep"))).To(Succeed())
			Expect(ran(`drop role "`+live+`"`)).To(BeFalse(), "a grant in progress is not swept")
			Expect(ran(`drop role "`+over+`"`)).To(BeTrue(), "one that is over, and left a role behind, is")
			Expect(ran(`drop role "bg_stranger"`)).To(BeTrue())
		})

		It("keeps the role of a request that is still being granted, which has no role in status yet", func() {
			cluster("ea-c-new", allowing)
			request("ea-c-new-1", "ea-c-new", alice)
			role := emergencyRoleName(get("ea-c-new-1").UID)
			orphans(role)
			Expect(clusterRec.reconcileEmergencyAccess(ctx, getCluster("ea-c-new"))).To(Succeed())
			Expect(ran(`drop role "` + role + `"`)).To(BeFalse())
		})

		It("does not count a request that is being deleted as keeping its role", func() {
			cluster("ea-c-deleting", allowing)
			request("ea-c-deleting-1", "ea-c-deleting", alice)
			reconcileOnce("ea-c-deleting-1")
			role := get("ea-c-deleting-1").Status.Role
			Expect(k8sClient.Delete(ctx, get("ea-c-deleting-1"))).To(Succeed())
			sql.ran = nil
			orphans(role)
			Expect(clusterRec.reconcileEmergencyAccess(ctx, getCluster("ea-c-deleting"))).To(Succeed())
			Expect(ran(`drop role "` + role + `"`)).To(BeTrue())
		})

		It("looks only every few minutes", func() {
			cluster("ea-c-trust", allowing)
			orphans()
			Expect(clusterRec.reconcileEmergencyAccess(ctx, getCluster("ea-c-trust"))).To(Succeed())
			dials := len(sql.dials)
			Expect(dials).To(BeNumerically(">", 0))
			Expect(clusterRec.reconcileEmergencyAccess(ctx, getCluster("ea-c-trust"))).To(Succeed())
			Expect(sql.dials).To(HaveLen(dials), "trusted")
			clusterRec.emergencyVerified.Store("default/ea-c-trust", time.Now().Add(-emergencySweepInterval-time.Second))
			Expect(clusterRec.reconcileEmergencyAccess(ctx, getCluster("ea-c-trust"))).To(Succeed())
			Expect(len(sql.dials)).To(BeNumerically(">", dials))
		})

		It("is not stopped by a database it cannot reach", func() {
			cluster("ea-c-down", allowing)
			sql.unreachable = errors.New("down")
			Expect(clusterRec.reconcileEmergencyAccess(ctx, getCluster("ea-c-down"))).To(Succeed())
			sql.unreachable = nil
			Expect(clusterRec.reconcileEmergencyAccess(ctx, getCluster("ea-c-down"))).To(Succeed())
			Expect(sql.dials).NotTo(BeEmpty(), "and it looks again, since it could not look before")
		})

		It("sweeps a restored cluster, though it does not allow emergency access, of the roles it inherited", func() {
			cluster("ea-c-restored", nil)
			c := getCluster("ea-c-restored")
			c.Spec.Bootstrap = &dbv1alpha1.BootstrapSpec{Recovery: &dbv1alpha1.RecoverySpec{SourceCluster: "source"}}
			orphans("bg_fromsource")
			Expect(emergencyAccessWanted(c)).To(BeTrue())
			Expect(clusterRec.reconcileEmergencyAccess(ctx, c)).To(Succeed())
			Expect(ran(`drop role "bg_fromsource"`)).To(BeTrue())
		})

		It("sweeps nothing without the SQL channel", func() {
			cluster("ea-c-nosql", allowing)
			clusterRec.SQL = nil
			Expect(clusterRec.reconcileEmergencyAccess(ctx, getCluster("ea-c-nosql"))).To(Succeed())
			Expect(sql.dials).To(BeEmpty())
			Expect(meta.FindStatusCondition(getCluster("ea-c-nosql").Status.Conditions, dbv1alpha1.ConditionEmergencyAccessActive)).NotTo(BeNil(),
				"but the condition is still kept")
		})
	})

	Describe("the names and statements", func() {
		It("make a role name from the request's identity, the same each time, and different for each", func() {
			a, b := emergencyRoleName("uid-a"), emergencyRoleName("uid-b")
			Expect(a).NotTo(Equal(b))
			Expect(emergencyRoleName("uid-a")).To(Equal(a))
			Expect(a).To(HavePrefix("bg_"))
			Expect(a).To(HaveLen(11))
		})

		It("keep an emergency role out of the sight of the code that manages application roles", func() {
			Expect(ownedByOperator(emergencyMarkerPrefix + "default/x: request y")).To(BeFalse())
			Expect(ownedByOperator(roleMarkerPrefix + "default/x")).To(BeTrue())
		})

		It("quote what they must", func() {
			Expect(emergencyGrantSQL(`bg_a"b`, `my"db`)).To(ContainSubstring(`on database "my""db" to "bg_a""b"`))
		})
	})
})
