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
	"maps"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus/testutil"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	dbv1alpha1 "github.com/tusharshrivastava62/postgres-operator/api/v1alpha1"
)

// Values asking for a password to be replaced; any value not used before asks again.
const (
	tokenFirst  = "first"
	tokenSecond = "second"
	tokenNow    = "now"
)

var _ = Describe("Replacing credentials", func() {
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
			Client: k8sClient, Scheme: k8sClient.Scheme(), Patroni: &fakePatroni{live: map[string]any{}}, Operator: testOperator, SQL: sql,
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
	annotate := func(secret string, annotations map[string]string) {
		GinkgoHelper()
		s := secretOf(secret)
		if s.Annotations == nil {
			s.Annotations = map[string]string{}
		}
		maps.Copy(s.Annotations, annotations)
		Expect(k8sClient.Update(ctx, s)).To(Succeed())
	}
	create := func(name string, mutate func(*dbv1alpha1.PostgresCluster)) {
		GinkgoHelper()
		audit := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: auditSecret, Namespace: namespace},
			StringData: map[string]string{secretKeyAccessKey: "id", secretKeySecretKey: testSecretValue},
		}
		if err := k8sClient.Create(ctx, audit); err == nil {
			DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, audit))).To(Succeed()) })
		}
		c := plainCluster()
		c.Name = name
		c.Spec.Storage = dbv1alpha1.StorageSpec{Size: resource.MustParse("1Gi")}
		c.Spec.Resources = corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}}
		mutate(c)
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
	// ready makes a cluster, reconciles once so its Secrets exist, gives it a
	// leader, and gives the fake database the superuser password.
	ready := func(name string, mutate func(*dbv1alpha1.PostgresCluster)) {
		GinkgoHelper()
		create(name, mutate)
		reconcileOnce(name)
		startLeader(name)
		sql.superuserPassword = string(secretOf(name + "-credentials").Data[secretKeyPassword])
	}
	observe := func(roles ...observedRole) {
		GinkgoHelper()
		sql.observed = observedJSON(roles)
	}
	// ours is a role the way the operator leaves one, with the password in its Secret.
	ours := func(name, role, secret string) observedRole {
		GinkgoHelper()
		verifier, err := newScramVerifier(string(secretOf(secret).Data[secretKeyPassword]))
		Expect(err).NotTo(HaveOccurred())
		return observedRole{Name: role, Comment: roleMarker(get(name)), Login: true, Limit: -1, Password: verifier}
	}
	hoursAgo := func(h int) string { return time.Now().Add(-time.Duration(h) * time.Hour).UTC().Format(time.RFC3339) }
	// rotate calls the rotation directly, as a reconcile would.
	rotate := func(name string, healthy, rolling bool) bool {
		GinkgoHelper()
		soon, err := rec.reconcileCredentialRotation(ctx, get(name), healthy, rolling)
		Expect(err).NotTo(HaveOccurred())
		return soon
	}
	credentialsStatus := func(name string) dbv1alpha1.CredentialsStatus {
		return get(name).Status.AccessControl.Credentials
	}
	// followDatabase makes the fake database take the new superuser password,
	// when a statement sets it, as a real one would.
	followDatabase := func(name string) {
		sql.afterExec = func(statement string) {
			if !strings.Contains(statement, `alter role "postgres" password`) {
				return
			}
			next := string(secretOf(name + "-credentials").Data[secretKeyPasswordNext])
			verifier := verifierInSQL.FindString(statement)
			ExpectWithOffset(1, verifierMatches(verifier, next)).To(BeTrue(), "the statement sets the password waiting in the Secret")
			sql.superuserPassword = next
		}
	}

	Describe("an application role's password", func() {
		const role = "app"
		setup := func(name string, every *metav1.Duration) string {
			GinkgoHelper()
			ready(name, func(c *dbv1alpha1.PostgresCluster) {
				c.Spec.Roles = []dbv1alpha1.RoleSpec{{Name: role, RotateEvery: every}}
			})
			secret := name + "-role-app"
			observe(ours(name, role, secret))
			return secret
		}

		It("is replaced when asked: set in the database as a verifier, then written to the Secret", func() {
			secret := setup("rot-ask", nil)
			reconcileOnce("rot-ask")
			Expect(sql.executed).To(BeEmpty(), "nothing is due, so nothing changes")
			before := string(secretOf(secret).Data[secretKeyPassword])

			annotate(secret, map[string]string{annotationRotate: tokenFirst})
			reconcileOnce("rot-ask")

			after := secretOf(secret)
			Expect(string(after.Data[secretKeyPassword])).NotTo(Equal(before))
			Expect(sql.executed).To(HaveLen(1))
			Expect(sql.executed[0]).NotTo(ContainSubstring(string(after.Data[secretKeyPassword])), "never the password itself")
			Expect(verifierMatches(verifierInSQL.FindString(sql.executed[0]), string(after.Data[secretKeyPassword]))).To(BeTrue(),
				"the database was given the password the Secret now holds")
			Expect(after.Annotations[annotationRotatedToken]).To(Equal(tokenFirst))
			Expect(after.Annotations[annotationRotatedAt]).NotTo(BeEmpty())
			status := get("rot-ask").Status.AccessControl.Roles[0]
			Expect(status.State).To(Equal(dbv1alpha1.RoleReady))
			Expect(status.LastRotated).NotTo(BeNil())
			Expect(time.Since(status.LastRotated.Time)).To(BeNumerically("<", time.Minute))
		})

		It("is replaced once for each request, not on every look", func() {
			secret := setup("rot-once", nil)
			annotate(secret, map[string]string{annotationRotate: tokenFirst})
			reconcileOnce("rot-once")
			observe(ours("rot-once", role, secret))
			reconcileOnce("rot-once")
			reconcileOnce("rot-once")
			Expect(sql.executed).To(HaveLen(1))

			annotate(secret, map[string]string{annotationRotate: tokenSecond})
			observe(ours("rot-once", role, secret))
			reconcileOnce("rot-once")
			Expect(sql.executed).To(HaveLen(2), "a new request is answered")
			Expect(secretOf(secret).Annotations[annotationRotatedToken]).To(Equal(tokenSecond))
		})

		It("is replaced even when the last look found everything in order", func() {
			secret := setup("rot-trust", nil)
			reconcileOnce("rot-trust")
			dials := len(sql.dials)
			reconcileOnce("rot-trust")
			Expect(sql.dials).To(HaveLen(dials), "trusted")
			annotate(secret, map[string]string{annotationRotate: tokenNow})
			reconcileOnce("rot-trust")
			Expect(len(sql.dials)).To(BeNumerically(">", dials))
			Expect(sql.executed).To(HaveLen(1))
		})

		It("is replaced on a schedule, when the password is older than it says", func() {
			secret := setup("rot-old", every(24*time.Hour))
			annotate(secret, map[string]string{annotationRotatedAt: hoursAgo(25)})
			before := string(secretOf(secret).Data[secretKeyPassword])
			reconcileOnce("rot-old")
			Expect(string(secretOf(secret).Data[secretKeyPassword])).NotTo(Equal(before))
			Expect(sql.executed).To(HaveLen(1))
		})

		It("is left alone while it is younger than the schedule", func() {
			secret := setup("rot-young", every(24*time.Hour))
			annotate(secret, map[string]string{annotationRotatedAt: hoursAgo(23)})
			before := string(secretOf(secret).Data[secretKeyPassword])
			reconcileOnce("rot-young")
			Expect(string(secretOf(secret).Data[secretKeyPassword])).To(Equal(before))
			Expect(sql.executed).To(BeEmpty())
		})

		It("is not written to the Secret when the database refuses it", func() {
			secret := setup("rot-refused", nil)
			before := string(secretOf(secret).Data[secretKeyPassword])
			annotate(secret, map[string]string{annotationRotate: tokenFirst})
			sql.execFailure = func(string) error { return errors.New("permission denied") }
			reconcileOnce("rot-refused")
			after := secretOf(secret)
			Expect(string(after.Data[secretKeyPassword])).To(Equal(before), "the Secret still has the password the database has")
			Expect(after.Annotations).NotTo(HaveKey(annotationRotatedToken), "the request is not answered, so it is tried again")
			Expect(get("rot-refused").Status.AccessControl.Roles[0].State).To(Equal(dbv1alpha1.RoleError))

			sql.execFailure = nil
			sql.executed = nil
			reconcileOnce("rot-refused")
			Expect(string(secretOf(secret).Data[secretKeyPassword])).NotTo(Equal(before))
		})

		It("is not replaced for a role the operator did not make", func() {
			secret := setup("rot-theirs", nil)
			role := ours("rot-theirs", "app", secret)
			role.Comment = "somebody else's"
			observe(role)
			before := string(secretOf(secret).Data[secretKeyPassword])
			annotate(secret, map[string]string{annotationRotate: tokenFirst})
			reconcileOnce("rot-theirs")
			Expect(string(secretOf(secret).Data[secretKeyPassword])).To(Equal(before))
			Expect(sql.executed).To(BeEmpty())
		})

		It("does not touch the other roles", func() {
			ready("rot-two", func(c *dbv1alpha1.PostgresCluster) {
				c.Spec.Roles = []dbv1alpha1.RoleSpec{{Name: firstRole}, {Name: secondRole}}
			})
			observe(ours("rot-two", firstRole, "rot-two-role-first"), ours("rot-two", secondRole, "rot-two-role-second"))
			other := string(secretOf("rot-two-role-second").Data[secretKeyPassword])
			annotate("rot-two-role-first", map[string]string{annotationRotate: "x"})
			reconcileOnce("rot-two")
			Expect(string(secretOf("rot-two-role-second").Data[secretKeyPassword])).To(Equal(other))
			Expect(strings.Join(sql.executed, "\n")).To(ContainSubstring(`alter role "first"`))
			Expect(strings.Join(sql.executed, "\n")).NotTo(ContainSubstring(`alter role "second"`))
		})
	})

	Describe("the superuser and replication passwords", func() {
		const creds = "-credentials"
		setup := func(name string, mutate func(*dbv1alpha1.PostgresCluster)) {
			GinkgoHelper()
			ready(name, mutate)
			followDatabase(name)
		}
		nothing := func(*dbv1alpha1.PostgresCluster) {}

		It("are replaced when asked, in order, and every pod is made to start with the new ones", func() {
			setup("cr-ask", nothing)
			before := secretOf("cr-ask" + creds)
			oldSuper, oldRepl := string(before.Data[secretKeyPassword]), string(before.Data[secretKeyReplicationPassword])
			Expect(before.Annotations).NotTo(HaveKey(annotationCredentialsVersion))

			Expect(rotate("cr-ask", true, false)).To(BeFalse(), "nothing is due")
			Expect(credentialsStatus("cr-ask").State).To(Equal(dbv1alpha1.CredentialsIdle))
			Expect(sql.dials).To(BeEmpty())

			annotate("cr-ask"+creds, map[string]string{annotationRotate: tokenNow})
			Expect(rotate("cr-ask", true, false)).To(BeTrue())

			after := secretOf("cr-ask" + creds)
			Expect(string(after.Data[secretKeyPassword])).NotTo(Equal(oldSuper))
			Expect(string(after.Data[secretKeyReplicationPassword])).NotTo(Equal(oldRepl))
			Expect(after.Data).NotTo(HaveKey(secretKeyPasswordNext), "the waiting passwords are the passwords now")
			Expect(after.Data).NotTo(HaveKey(secretKeyReplicationPasswordNext))
			Expect(string(after.Data[secretKeyUsername])).To(Equal("postgres"))
			Expect(after.Annotations[annotationCredentialsVersion]).To(Equal("1"))
			Expect(after.Annotations[annotationRotatedToken]).To(Equal(tokenNow))
			Expect(after.Annotations[annotationRotatedAt]).NotTo(BeEmpty())

			By("setting both in the database in one statement, as verifiers")
			Expect(sql.executed).To(HaveLen(1))
			statement := sql.executed[0]
			Expect(statement).To(ContainSubstring(`alter role "postgres" password 'SCRAM-SHA-256$`))
			Expect(statement).To(ContainSubstring(`alter role "replicator" password 'SCRAM-SHA-256$`))
			Expect(statement).NotTo(ContainSubstring(string(after.Data[secretKeyPassword])))
			Expect(statement).NotTo(ContainSubstring(string(after.Data[secretKeyReplicationPassword])))
			verifiers := verifierInSQL.FindAllString(statement, -1)
			Expect(verifiers).To(HaveLen(2))
			Expect(verifierMatches(verifiers[0], string(after.Data[secretKeyPassword]))).To(BeTrue())
			Expect(verifierMatches(verifiers[1], string(after.Data[secretKeyReplicationPassword]))).To(BeTrue())

			By("saying so in status")
			status := credentialsStatus("cr-ask")
			Expect(status.State).To(Equal(dbv1alpha1.CredentialsRolling))
			Expect(status.LastRotated).NotTo(BeNil())
		})

		It("are replaced once for each request", func() {
			setup("cr-once", nothing)
			annotate("cr-once"+creds, map[string]string{annotationRotate: "a"})
			rotate("cr-once", true, false)
			rotate("cr-once", true, false)
			rotate("cr-once", true, false)
			Expect(sql.executed).To(HaveLen(1))
			Expect(secretOf("cr-once" + creds).Annotations[annotationCredentialsVersion]).To(Equal("1"))

			annotate("cr-once"+creds, map[string]string{annotationRotate: "b"})
			rotate("cr-once", true, false)
			Expect(sql.executed).To(HaveLen(2))
			Expect(secretOf("cr-once" + creds).Annotations[annotationCredentialsVersion]).To(Equal("2"), "each replacement raises the version")
		})

		It("are replaced on a schedule", func() {
			setup("cr-sched", func(c *dbv1alpha1.PostgresCluster) {
				c.Spec.CredentialRotation = &dbv1alpha1.CredentialRotationSpec{SuperuserEvery: every(24 * time.Hour)}
			})
			annotate("cr-sched"+creds, map[string]string{annotationRotatedAt: hoursAgo(23)})
			rotate("cr-sched", true, false)
			Expect(sql.executed).To(BeEmpty())
			annotate("cr-sched"+creds, map[string]string{annotationRotatedAt: hoursAgo(25)})
			rotate("cr-sched", true, false)
			Expect(sql.executed).To(HaveLen(1))
		})

		It("wait, changing nothing, while the cluster is unhealthy", func() {
			setup("cr-sick", nothing)
			before := secretOf("cr-sick" + creds)
			annotate("cr-sick"+creds, map[string]string{annotationRotate: tokenNow})
			Expect(rotate("cr-sick", false, false)).To(BeTrue(), "look again soon")
			status := credentialsStatus("cr-sick")
			Expect(status.State).To(Equal(dbv1alpha1.CredentialsWaiting))
			Expect(status.Message).To(ContainSubstring("not healthy"))
			Expect(sql.dials).To(BeEmpty())
			after := secretOf("cr-sick" + creds)
			Expect(after.Data).To(Equal(before.Data))
			Expect(after.Data).NotTo(HaveKey(secretKeyPasswordNext))
		})

		It("wait while the pods are being updated for another reason", func() {
			setup("cr-roll", nothing)
			annotate("cr-roll"+creds, map[string]string{annotationRotate: tokenNow})
			rotate("cr-roll", true, true)
			status := credentialsStatus("cr-roll")
			Expect(status.State).To(Equal(dbv1alpha1.CredentialsWaiting))
			Expect(status.Message).To(ContainSubstring("being updated"))
			Expect(sql.executed).To(BeEmpty())
		})

		It("wait for a primary", func() {
			create("cr-nolead", nothing)
			reconcileOnce("cr-nolead")
			annotate("cr-nolead"+creds, map[string]string{annotationRotate: tokenNow})
			rotate("cr-nolead", true, false)
			Expect(credentialsStatus("cr-nolead").State).To(Equal(dbv1alpha1.CredentialsWaiting))
			Expect(credentialsStatus("cr-nolead").Message).To(ContainSubstring("primary"))
			Expect(secretOf("cr-nolead" + creds).Data).NotTo(HaveKey(secretKeyPasswordNext))
		})

		It("pick up where an operator that stopped after writing the new passwords left off", func() {
			setup("cr-resume1", nothing)
			secret := secretOf("cr-resume1" + creds)
			secret.Data[secretKeyPasswordNext] = []byte("next-superuser-password-0123456789abcdef")
			secret.Data[secretKeyReplicationPasswordNext] = []byte("next-replication-password-0123456789abc")
			Expect(k8sClient.Update(ctx, secret)).To(Succeed())

			// Not due, not healthy: a rotation that has begun is finished anyway.
			rotate("cr-resume1", false, true)

			after := secretOf("cr-resume1" + creds)
			Expect(string(after.Data[secretKeyPassword])).To(Equal("next-superuser-password-0123456789abcdef"))
			Expect(string(after.Data[secretKeyReplicationPassword])).To(Equal("next-replication-password-0123456789abc"))
			Expect(after.Data).NotTo(HaveKey(secretKeyPasswordNext))
			Expect(sql.executed).To(HaveLen(1), "the database still had the old ones")
			Expect(after.Annotations[annotationCredentialsVersion]).To(Equal("1"))
		})

		It("pick up where an operator that stopped after setting them in the database left off", func() {
			setup("cr-resume2", nothing)
			secret := secretOf("cr-resume2" + creds)
			secret.Data[secretKeyPasswordNext] = []byte("next-superuser-password-0123456789abcdef")
			secret.Data[secretKeyReplicationPasswordNext] = []byte("next-replication-password-0123456789abc")
			Expect(k8sClient.Update(ctx, secret)).To(Succeed())
			sql.superuserPassword = "next-superuser-password-0123456789abcdef" // the database has them already

			rotate("cr-resume2", true, false)

			after := secretOf("cr-resume2" + creds)
			Expect(string(after.Data[secretKeyPassword])).To(Equal("next-superuser-password-0123456789abcdef"))
			Expect(after.Data).NotTo(HaveKey(secretKeyPasswordNext))
			Expect(sql.executed).To(BeEmpty(), "nothing is set twice")
			Expect(after.Annotations[annotationCredentialsVersion]).To(Equal("1"))
		})

		It("stop, and change nothing, when the database accepts neither", func() {
			setup("cr-neither", nothing)
			secret := secretOf("cr-neither" + creds)
			secret.Data[secretKeyPasswordNext] = []byte("next-superuser-password-0123456789abcdef")
			secret.Data[secretKeyReplicationPasswordNext] = []byte("next-replication-password-0123456789abc")
			Expect(k8sClient.Update(ctx, secret)).To(Succeed())
			before := string(secret.Data[secretKeyPassword])
			sql.superuserPassword = "something-else-entirely"

			rotate("cr-neither", true, false)

			status := credentialsStatus("cr-neither")
			Expect(status.State).To(Equal(dbv1alpha1.CredentialsError))
			Expect(status.Message).To(ContainSubstring("neither the old nor the new password is accepted"))
			after := secretOf("cr-neither" + creds)
			Expect(string(after.Data[secretKeyPassword])).To(Equal(before))
			Expect(after.Data).To(HaveKey(secretKeyPasswordNext), "kept, so a person can see what was tried")
			Expect(sql.executed).To(BeEmpty())
		})

		It("report a database that cannot be reached, and promote nothing", func() {
			setup("cr-down", nothing)
			annotate("cr-down"+creds, map[string]string{annotationRotate: tokenNow})
			sql.unreachable = errors.New("i/o timeout")
			before := string(secretOf("cr-down" + creds).Data[secretKeyPassword])
			rotate("cr-down", true, false)
			Expect(credentialsStatus("cr-down").State).To(Equal(dbv1alpha1.CredentialsError))
			Expect(string(secretOf("cr-down" + creds).Data[secretKeyPassword])).To(Equal(before))
			Expect(secretOf("cr-down" + creds).Annotations).NotTo(HaveKey(annotationCredentialsVersion))
		})

		It("do not promote passwords the database refused", func() {
			setup("cr-refuse", nothing)
			annotate("cr-refuse"+creds, map[string]string{annotationRotate: tokenNow})
			sql.execFailure = func(string) error { return errors.New("permission denied") }
			before := string(secretOf("cr-refuse" + creds).Data[secretKeyPassword])
			rotate("cr-refuse", true, false)
			Expect(credentialsStatus("cr-refuse").State).To(Equal(dbv1alpha1.CredentialsError))
			Expect(string(secretOf("cr-refuse" + creds).Data[secretKeyPassword])).To(Equal(before))
		})

		It("show as rolling while the pods are replaced, then as idle", func() {
			setup("cr-state", nothing)
			annotate("cr-state"+creds, map[string]string{annotationRotate: tokenNow})
			rotate("cr-state", true, false)
			Expect(rotate("cr-state", true, true)).To(BeTrue())
			Expect(credentialsStatus("cr-state").State).To(Equal(dbv1alpha1.CredentialsRolling))
			Expect(rotate("cr-state", true, false)).To(BeFalse())
			Expect(credentialsStatus("cr-state").State).To(Equal(dbv1alpha1.CredentialsIdle))
		})

		It("are not touched at all without the SQL channel", func() {
			setup("cr-nosql", nothing)
			rec.SQL = nil
			annotate("cr-nosql"+creds, map[string]string{annotationRotate: tokenNow})
			Expect(rotate("cr-nosql", true, false)).To(BeFalse())
			Expect(secretOf("cr-nosql" + creds).Data).NotTo(HaveKey(secretKeyPasswordNext))
		})

		It("put the version on the pod template, and nothing on a template that never had one", func() {
			create("cr-template", nothing)
			reconcileOnce("cr-template")
			sts := &appsv1.StatefulSet{}
			Expect(k8sClient.Get(ctx, key("cr-template"), sts)).To(Succeed())
			Expect(sts.Spec.Template.Annotations).NotTo(HaveKey(annotationCredentialsVersion))

			annotate("cr-template"+creds, map[string]string{annotationCredentialsVersion: "3"})
			reconcileOnce("cr-template")
			Expect(k8sClient.Get(ctx, key("cr-template"), sts)).To(Succeed())
			Expect(sts.Spec.Template.Annotations).To(HaveKeyWithValue(annotationCredentialsVersion, "3"))

			annotate("cr-template"+creds, map[string]string{annotationCredentialsVersion: "4"})
			reconcileOnce("cr-template")
			Expect(k8sClient.Get(ctx, key("cr-template"), sts)).To(Succeed())
			Expect(sts.Spec.Template.Annotations).To(HaveKeyWithValue(annotationCredentialsVersion, "4"))
		})

		It("are published by age, for each kind of credential", func() {
			ready("cr-age", func(c *dbv1alpha1.PostgresCluster) {
				withCompliance(c, dbv1alpha1.StandardHIPAA)
				c.Spec.Roles = []dbv1alpha1.RoleSpec{{Name: "young"}, {Name: "old"}}
			})
			reconcileOnce("cr-age")
			annotate("cr-age"+creds, map[string]string{annotationRotatedAt: hoursAgo(10)})
			annotate("cr-age-monitor", map[string]string{annotationRotatedAt: hoursAgo(20)})
			annotate("cr-age-role-young", map[string]string{annotationRotatedAt: hoursAgo(1)})
			annotate("cr-age-role-old", map[string]string{annotationRotatedAt: hoursAgo(30)})
			rec.recordCredentialAges(ctx, get("cr-age"))
			hour := float64(time.Hour / time.Second)
			age := func(kind string) float64 {
				return testutil.ToFloat64(credentialAge.WithLabelValues(namespace, "cr-age", kind))
			}
			Expect(age(credentialSuperuser)).To(BeNumerically("~", 10*hour, 120))
			Expect(age(credentialReplication)).To(BeNumerically("~", 10*hour, 120))
			Expect(age(credentialMonitor)).To(BeNumerically("~", 20*hour, 120))
			Expect(age(credentialApplication)).To(BeNumerically("~", 30*hour, 120), "the oldest of the roles")
		})
	})

	Describe("the monitor role's password", func() {
		setup := func(name string, mutate func(*dbv1alpha1.PostgresCluster)) {
			GinkgoHelper()
			ready(name, func(c *dbv1alpha1.PostgresCluster) {
				withCompliance(c, dbv1alpha1.StandardHIPAA)
				mutate(c)
			})
			secret := secretOf(name + "-monitor")
			sql.monitorPassword = string(secret.Data[secretKeyPassword])
			sql.monitorPrivileged = true
			testMonitorPasswords = []string{sql.monitorPassword}
		}
		BeforeEach(func() { testMonitorPasswords = nil })

		It("is replaced when asked: set up with the new password, checked by logging in with it, then written", func() {
			setup("rot-mon", func(*dbv1alpha1.PostgresCluster) {})
			old := sql.monitorPassword
			annotate("rot-mon-monitor", map[string]string{annotationRotate: tokenNow})
			reconcileOnce("rot-mon")

			secret := secretOf("rot-mon-monitor")
			fresh := string(secret.Data[secretKeyPassword])
			Expect(fresh).NotTo(Equal(old))
			Expect(secret.Annotations[annotationRotatedToken]).To(Equal(tokenNow))
			Expect(sql.executed).NotTo(BeEmpty())
			Expect(strings.Join(sql.executed, "\n")).NotTo(ContainSubstring(fresh))
			Expect(get("rot-mon").Status.AccessControl.MonitorRole.LastRotated).NotTo(BeNil())
			Expect(get("rot-mon").Status.AccessControl.MonitorRole.State).To(Equal(dbv1alpha1.RoleReady))
		})
	})
})
