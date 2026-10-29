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
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/yaml"

	dbv1alpha1 "github.com/tusharshrivastava62/postgres-operator/api/v1alpha1"
)

func indexOfRule(rules []string, rule string) int {
	for i, r := range rules {
		if r == rule {
			return i
		}
	}
	return -1
}

func TestTheRulesRefuseTheReplicationRoleEverythingButReplicationBeforeAnyRuleLetsItIn(t *testing.T) {
	rules := restrictedPgHBA()
	replicate := indexOfRule(rules, "hostssl replication replicator all md5")
	refuse := indexOfRule(rules, "hostssl all replicator all reject")
	anyone := indexOfRule(rules, "hostssl all all all md5")
	if replicate < 0 || refuse < 0 || anyone < 0 {
		t.Fatalf("rules %v", rules)
	}
	if refuse > anyone {
		t.Error("the replication role is refused after a rule that lets it in as anyone")
	}
	if replicate > anyone {
		t.Error("replicating comes after the catch-all")
	}
	if nossl := indexOfRule(rules, "hostnossl all all all reject"); nossl < 0 || nossl > refuse {
		t.Errorf("a connection without TLS must be refused, before the rules that let one in: %v", rules)
	}
	for _, rule := range rules {
		if strings.Contains(rule, "pam") {
			t.Errorf("a rule that authenticates through PAM: %s", rule)
		}
		if strings.Contains(rule, "trust") && !strings.HasPrefix(rule, "local ") {
			t.Errorf("a rule that trusts a connection from the network: %s", rule)
		}
	}
}

func TestTheRewindLoginCanDoNothingButReadWhatPgRewindReads(t *testing.T) {
	cluster := testClusterT.DeepCopy()
	sql := rewindSetupSQL(cluster, "SCRAM-SHA-256$4096:salt$stored:server")
	for _, want := range []string{
		"nosuperuser nocreatedb nocreaterole noreplication nobypassrls",
		"connection limit 3",
		"comment on role pgoperator_rewind is 'Managed by postgres-operator for shop/orders'",
		"password 'SCRAM-SHA-256$4096:salt$stored:server'",
		"grant execute on function pg_catalog.pg_ls_dir(text, boolean, boolean) to pgoperator_rewind;",
		"grant execute on function pg_catalog.pg_stat_file(text, boolean) to pgoperator_rewind;",
		"grant execute on function pg_catalog.pg_read_binary_file(text) to pgoperator_rewind;",
		"grant execute on function pg_catalog.pg_read_binary_file(text, bigint, bigint, boolean) to pgoperator_rewind;",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("the setup lacks: %s", want)
		}
	}
	for line := range strings.SplitSeq(sql, "\n") {
		if strings.HasPrefix(line, "grant ") && !strings.HasPrefix(line, "grant execute on function pg_catalog.") {
			t.Errorf("a grant beyond the four functions: %s", line)
		}
	}
	if strings.Contains(sql, " superuser") && !strings.Contains(sql, "nosuperuser") {
		t.Error("the login is a superuser")
	}
	verify := rewindVerifySQL(cluster)
	for _, want := range []string{"not rolsuper", "not rolreplication", "coalesce(pg_catalog.shobj_description(",
		"= 'Managed by postgres-operator for shop/orders', false)", "pg_ls_dir", "pg_stat_file", "pg_read_binary_file(text)"} {
		if !strings.Contains(verify, want) {
			t.Errorf("the check lacks: %s", want)
		}
	}
}

var _ = Describe("Limiting the replication role", func() {
	const (
		namespace = "default"
		creds     = "-credentials"
	)
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
	stsOf := func(name string) *appsv1.StatefulSet {
		GinkgoHelper()
		sts := &appsv1.StatefulSet{}
		Expect(k8sClient.Get(ctx, key(name), sts)).To(Succeed())
		return sts
	}
	secretOf := func(name string) *corev1.Secret {
		GinkgoHelper()
		s := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, key(name), s)).To(Succeed())
		return s
	}
	create := func(name string, mutate func(*dbv1alpha1.PostgresCluster)) {
		GinkgoHelper()
		c := plainCluster()
		c.Name = name
		c.Spec.Storage = dbv1alpha1.StorageSpec{Size: resource.MustParse("1Gi")}
		c.Spec.Resources = corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}}
		if mutate != nil {
			mutate(c)
		}
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		DeferCleanup(func() {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, c))).To(Succeed())
			for _, n := range []string{credentialsSecretName(c), monitorSecretName(c)} {
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: n, Namespace: namespace}}))).To(Succeed())
			}
		})
	}
	restrict := func(c *dbv1alpha1.PostgresCluster) {
		c.Spec.Security = &dbv1alpha1.SecuritySpec{RestrictReplicationRole: true}
	}
	startLeader := func(name string, shape func(*corev1.Pod)) {
		GinkgoHelper()
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name + "-0", Namespace: namespace, Labels: selectorLabelsFor(name)},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: testPodImage}}},
		}
		if shape != nil {
			shape(pod)
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
	usingRewind := func(pod *corev1.Pod) {
		pod.Spec.Containers[0].Env = append(pod.Spec.Containers[0].Env, corev1.EnvVar{Name: envRewindPassword, Value: "x"})
	}
	// ready makes a cluster that asks for it, with a primary, and gives the fake
	// database the superuser's password.
	ready := func(name string) {
		GinkgoHelper()
		create(name, restrict)
		reconcileOnce(name)
		startLeader(name, usingRewind)
		sql.superuserPassword = string(secretOf(name + creds).Data[secretKeyPassword])
	}
	status := func(name string) dbv1alpha1.RoleStatus { return get(name).Status.AccessControl.RewindRole }
	rewindStatements := func() []string {
		var ran []string
		for _, s := range sql.executed {
			if strings.Contains(s, rewindRoleName) {
				ran = append(ran, s)
			}
		}
		return ran
	}

	Describe("the pod", func() {
		It("is given the rules and the login pg_rewind is to run as, with the password from the Secret and not in the spec", func() {
			create("rr-pod", restrict)
			reconcileOnce("rr-pod")
			password := string(secretOf("rr-pod" + creds).Data[secretKeyRewind])
			Expect(password).To(HavePrefix("pw"))

			sts := stsOf("rr-pod")
			env := map[string]corev1.EnvVar{}
			podEnv := sts.Spec.Template.Spec.Containers[0].Env
			names := make([]string, 0, len(podEnv))
			for _, e := range podEnv {
				env[e.Name] = e
				names = append(names, e.Name)
			}
			ref := env[envRewindPassword].ValueFrom.SecretKeyRef
			Expect(ref.Name).To(Equal("rr-pod-credentials"))
			Expect(ref.Key).To(Equal(secretKeyRewind))
			Expect(indexOf(names, envRewindPassword)).To(BeNumerically("<", indexOf(names, envSpiloConfiguration)))
			Expect(strings.Contains(env[envSpiloConfiguration].Value, password)).To(BeFalse())

			var config struct {
				Postgresql struct {
					Authentication struct {
						Rewind struct{ Username, Password string }
					}
					PgHBA []string `json:"pg_hba"`
				}
			}
			Expect(yaml.Unmarshal([]byte(env[envSpiloConfiguration].Value), &config)).To(Succeed())
			Expect(config.Postgresql.Authentication.Rewind.Username).To(Equal("pgoperator_rewind"))
			Expect(config.Postgresql.Authentication.Rewind.Password).To(Equal("$(REWIND_PASSWORD)"))
			Expect(config.Postgresql.PgHBA).To(Equal(restrictedPgHBA()))
		})

		It("keeps the audit libraries a compliance preset asks for, beside the rules", func() {
			create("rr-both", func(c *dbv1alpha1.PostgresCluster) {
				withCompliance(c, dbv1alpha1.StandardHIPAA)
				restrict(c)
			})
			rendered, err := spiloConfiguration(get("rr-both"))
			Expect(err).NotTo(HaveOccurred())
			var config struct {
				Postgresql struct {
					Parameters map[string]string `json:"parameters"`
					PgHBA      []string          `json:"pg_hba"`
				}
			}
			Expect(yaml.Unmarshal([]byte(rendered), &config)).To(Succeed())
			Expect(config.Postgresql.Parameters).To(HaveKey(paramPreload))
			Expect(config.Postgresql.PgHBA).NotTo(BeEmpty())
		})

		It("is left as it was for a cluster that does not ask", func() {
			create("rr-off", nil)
			reconcileOnce("rr-off")
			Expect(secretOf("rr-off" + creds).Data).NotTo(HaveKey(secretKeyRewind))
			sts := stsOf("rr-off")
			for _, e := range sts.Spec.Template.Spec.Containers[0].Env {
				Expect(e.Name).NotTo(Equal(envRewindPassword))
				if e.Name == envSpiloConfiguration {
					Expect(e.Value).NotTo(ContainSubstring("pg_hba"))
					Expect(e.Value).NotTo(ContainSubstring("rewind"))
				}
			}
		})

		It("gets the password added to a Secret made before the cluster asked, and keeps the others", func() {
			create("rr-later", nil)
			reconcileOnce("rr-later")
			before := secretOf("rr-later" + creds)
			c := get("rr-later")
			restrict(c)
			Expect(k8sClient.Update(ctx, c)).To(Succeed())
			reconcileOnce("rr-later")
			after := secretOf("rr-later" + creds)
			Expect(string(after.Data[secretKeyRewind])).To(HavePrefix("pw"))
			Expect(after.Data[secretKeyPassword]).To(Equal(before.Data[secretKeyPassword]))
			Expect(after.Data[secretKeyReplicationPassword]).To(Equal(before.Data[secretKeyReplicationPassword]))

			By("and keeps it when the cluster stops asking")
			kept := after.Data[secretKeyRewind]
			c = get("rr-later")
			c.Spec.Security = nil
			Expect(k8sClient.Update(ctx, c)).To(Succeed())
			reconcileOnce("rr-later")
			Expect(secretOf("rr-later" + creds).Data[secretKeyRewind]).To(Equal(kept))
		})
	})

	Describe("the login in the database", func() {
		It("is made when it is missing, as the superuser, with a verifier of the Secret's password and never the password", func() {
			ready("rr-make")
			reconcileOnce("rr-make")

			ran := rewindStatements()
			Expect(ran).To(HaveLen(1))
			password := string(secretOf("rr-make" + creds).Data[secretKeyRewind])
			Expect(ran[0]).NotTo(ContainSubstring(password))
			verifier := verifierInSQL.FindString(ran[0])
			Expect(verifierMatches(verifier, password)).To(BeTrue())
			Expect(ran[0]).To(ContainSubstring("grant execute on function pg_catalog.pg_ls_dir"))
			Expect(status("rr-make").State).To(Equal(dbv1alpha1.RoleReady))
			Expect(status("rr-make").Name).To(Equal("pgoperator_rewind"))
			Expect(status("rr-make").SecretName).To(Equal("rr-make-credentials"))
		})

		It("is left alone, and the superuser not used, once it is as it should be", func() {
			ready("rr-quiet")
			reconcileOnce("rr-quiet")
			dials := len(sql.usersDialed())
			executed := len(sql.executed)
			reconcileOnce("rr-quiet")
			Expect(sql.executed).To(HaveLen(executed))
			for _, user := range sql.usersDialed()[dials:] {
				Expect(user).NotTo(Equal(testSuperuserName), "the login is checked by logging in as it")
			}
			Expect(status("rr-quiet").State).To(Equal(dbv1alpha1.RoleReady))
		})

		It("is put right when it has lost what pg_rewind needs", func() {
			ready("rr-drift")
			reconcileOnce("rr-drift")
			sql.rewindPrivileged = false
			reconcileOnce("rr-drift")
			Expect(rewindStatements()).To(HaveLen(2))
			Expect(sql.rewindPrivileged).To(BeTrue())
		})

		It("is put right when its password is not the Secret's", func() {
			ready("rr-pass")
			reconcileOnce("rr-pass")
			other, err := newScramVerifier("not-the-password")
			Expect(err).NotTo(HaveOccurred())
			sql.rewindVerifier = other
			reconcileOnce("rr-pass")
			Expect(rewindStatements()).To(HaveLen(2))
			Expect(verifierMatches(sql.rewindVerifier, string(secretOf("rr-pass" + creds).Data[secretKeyRewind]))).To(BeTrue())
		})

		It("waits for a primary, and says so", func() {
			create("rr-wait", restrict)
			reconcileOnce("rr-wait")
			Expect(status("rr-wait").State).To(Equal(dbv1alpha1.RolePending))
			Expect(status("rr-wait").Message).To(ContainSubstring("no primary"))
		})

		It("reports a database that cannot be reached, and does not fail the reconcile", func() {
			ready("rr-down")
			sql.unreachable = context.DeadlineExceeded
			reconcileOnce("rr-down")
			Expect(status("rr-down").State).To(Equal(dbv1alpha1.RoleError))
		})

		It("is looked at again soon while it is not settled", func() {
			ready("rr-soon")
			sql.unreachable = context.DeadlineExceeded
			Expect(reconcileOnce("rr-soon").RequeueAfter).To(Equal(unhealthyRefreshInterval))
		})

		It("makes the reconcile look again soon while it is not settled, and not once it is", func() {
			ready("rr-again")
			sql.unreachable = context.DeadlineExceeded
			soon, err := rec.reconcileSecurity(ctx, get("rr-again"))
			Expect(err).NotTo(HaveOccurred())
			Expect(soon).To(BeTrue(), "the login could not be checked")

			sql.unreachable = nil
			soon, err = rec.reconcileSecurity(ctx, get("rr-again"))
			Expect(err).NotTo(HaveOccurred())
			Expect(soon).To(BeFalse(), "the login is in the database as it should be")
		})

		It("is not touched at all by a cluster that never asked for it", func() {
			create("rr-never", nil)
			reconcileOnce("rr-never")
			startLeader("rr-never", nil)
			sql.superuserPassword = string(secretOf("rr-never" + creds).Data[secretKeyPassword])
			reconcileOnce("rr-never")
			Expect(rewindStatements()).To(BeEmpty())
			Expect(status("rr-never").State).To(BeEmpty())
		})

		It("is not touched without the SQL channel", func() {
			ready("rr-nosql")
			rec.SQL = nil
			reconcileOnce("rr-nosql")
			Expect(rewindStatements()).To(BeEmpty())
			Expect(sql.dials).To(BeEmpty())
		})
	})

	Describe("when the cluster stops asking", func() {
		It("loses its login only once no pod is left that was started to use it, and is not dropped", func() {
			ready("rr-off1")
			reconcileOnce("rr-off1")
			Expect(sql.rewindLogin).To(BeTrue())

			c := get("rr-off1")
			c.Spec.Security = nil
			Expect(k8sClient.Update(ctx, c)).To(Succeed())
			reconcileOnce("rr-off1")
			Expect(sql.rewindLogin).To(BeTrue(), "the pod still runs pg_rewind as it")
			Expect(status("rr-off1").State).To(Equal(dbv1alpha1.RoleReady))

			By("until the pod has been replaced")
			pod := &corev1.Pod{}
			Expect(k8sClient.Get(ctx, key("rr-off1-0"), pod)).To(Succeed())
			pod.Spec.Containers[0].Env = nil
			// A running pod's spec cannot be changed, so replace the pod.
			Expect(k8sClient.Delete(ctx, pod, client.GracePeriodSeconds(0))).To(Succeed())
			fresh := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "rr-off1-0", Namespace: namespace, Labels: selectorLabelsFor("rr-off1")},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: testPodImage}}},
			}
			Eventually(func() error { return k8sClient.Create(ctx, fresh) }, 10*time.Second).Should(Succeed())
			DeferCleanup(func() {
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, fresh, client.GracePeriodSeconds(0)))).To(Succeed())
			})
			fresh.Status.PodIP = testLeaderIP
			Expect(k8sClient.Status().Update(ctx, fresh)).To(Succeed())

			reconcileOnce("rr-off1")
			Expect(sql.rewindLogin).To(BeFalse())
			Expect(sql.rewindExists).To(BeTrue(), "the role is disabled, never dropped")
			Expect(status("rr-off1").State).To(Equal(dbv1alpha1.RoleDisabled))

			By("and nothing more is done after that")
			executed := len(sql.executed)
			reconcileOnce("rr-off1")
			Expect(sql.executed).To(HaveLen(executed))
		})
	})

	Describe("when the passwords are replaced", func() {
		rotate := func(name string) {
			GinkgoHelper()
			_, err := rec.reconcileCredentialRotation(ctx, get(name), true, false)
			Expect(err).NotTo(HaveOccurred())
		}
		annotate := func(name string) {
			GinkgoHelper()
			s := secretOf(name + creds)
			if s.Annotations == nil {
				s.Annotations = map[string]string{}
			}
			s.Annotations[annotationRotate] = tokenNow
			Expect(k8sClient.Update(ctx, s)).To(Succeed())
		}
		follow := func(name string) {
			sql.afterExec = func(statement string) {
				if strings.Contains(statement, `alter role "postgres" password`) {
					sql.superuserPassword = string(secretOf(name + creds).Data[secretKeyPasswordNext])
				}
			}
		}

		It("replaces the rewind login's with them: in the statement that sets theirs, and then in the Secret", func() {
			ready("rr-rot")
			follow("rr-rot")
			reconcileOnce("rr-rot")
			Expect(status("rr-rot").State).To(Equal(dbv1alpha1.RoleReady))
			before := secretOf("rr-rot" + creds)

			annotate("rr-rot")
			rotate("rr-rot")

			after := secretOf("rr-rot" + creds)
			Expect(after.Data[secretKeyRewind]).NotTo(Equal(before.Data[secretKeyRewind]))
			Expect(string(after.Data[secretKeyRewind])).To(HavePrefix("pw"))
			Expect(after.Data).NotTo(HaveKey(secretKeyRewindNext), "the waiting password is the password now")
			Expect(verifierMatches(sql.rewindVerifier, string(after.Data[secretKeyRewind]))).To(BeTrue(), "the database has it")

			statement := sql.executed[len(sql.executed)-1]
			Expect(statement).To(ContainSubstring(`alter role "postgres" password`))
			Expect(statement).To(ContainSubstring(`alter role "pgoperator_rewind" password`))
			Expect(statement).NotTo(ContainSubstring(string(after.Data[secretKeyRewind])))
			Expect(after.Annotations[annotationCredentialsVersion]).To(Equal("1"), "the pods are replaced to start with it")
		})

		It("leaves it out while the login is not in the database yet, since the statement would fail", func() {
			create("rr-early", restrict)
			reconcileOnce("rr-early")
			startLeader("rr-early", nil)
			sql.superuserPassword = string(secretOf("rr-early" + creds).Data[secretKeyPassword])
			follow("rr-early")
			before := secretOf("rr-early" + creds)
			annotate("rr-early")
			rotate("rr-early")
			after := secretOf("rr-early" + creds)
			Expect(after.Data[secretKeyRewind]).To(Equal(before.Data[secretKeyRewind]))
			Expect(sql.executed[len(sql.executed)-1]).NotTo(ContainSubstring(rewindRoleName))
			Expect(string(after.Data[secretKeyPassword])).NotTo(Equal(string(before.Data[secretKeyPassword])), "the others are replaced")
		})

		It("leaves it alone when the cluster does not ask for it", func() {
			create("rr-unasked", nil)
			reconcileOnce("rr-unasked")
			startLeader("rr-unasked", nil)
			sql.superuserPassword = string(secretOf("rr-unasked" + creds).Data[secretKeyPassword])
			follow("rr-unasked")
			annotate("rr-unasked")
			rotate("rr-unasked")
			Expect(secretOf("rr-unasked" + creds).Data).NotTo(HaveKey(secretKeyRewind))
			Expect(sql.executed[len(sql.executed)-1]).NotTo(ContainSubstring(rewindRoleName))
		})
	})
})
