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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/yaml"

	dbv1alpha1 "github.com/tusharshrivastava62/postgres-operator/api/v1alpha1"
)

// auditBucket is the bucket the audit log destinations name.
const (
	hipaaClasses          = "read,write,ddl,role"
	paramPgauditLog       = "pgaudit.log"
	paramPgauditCatalog   = "pgaudit.log_catalog"
	paramPgauditParameter = "pgaudit.log_parameter"
	valueOff              = "off"
	auditBucket           = "audit-logs"
	auditSecret           = "audit-s3"
)

// withCompliance returns a cluster with the given standard and a destination.
func withCompliance(c *dbv1alpha1.PostgresCluster, standard string) *dbv1alpha1.PostgresCluster {
	c.Spec.Compliance = &dbv1alpha1.ComplianceSpec{
		Standard: standard,
		AuditLogs: dbv1alpha1.AuditLogsSpec{
			Destination: &dbv1alpha1.BackupDestination{Bucket: auditBucket, Region: testRegion, CredentialsSecret: auditSecret},
		},
	}
	return c
}

func plainCluster() *dbv1alpha1.PostgresCluster {
	return &dbv1alpha1.PostgresCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "audited", Namespace: "default"},
		Spec:       dbv1alpha1.PostgresClusterSpec{Version: "17", Instances: 3},
	}
}

// preloadOf reads shared_preload_libraries out of the rendered
// SPILO_CONFIGURATION, and whether the cluster sets one at all.
func preloadOf(c *dbv1alpha1.PostgresCluster) (string, bool) {
	rendered, err := spiloConfiguration(c)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	var config struct {
		Postgresql struct {
			Parameters map[string]string `json:"parameters"`
		} `json:"postgresql"`
	}
	ExpectWithOffset(1, yaml.Unmarshal([]byte(rendered), &config)).To(Succeed())
	value, ok := config.Postgresql.Parameters[paramPreload]
	return value, ok
}

var _ = Describe("Loading pgaudit through the pod template", func() {
	It("leaves Spilo's own library list alone for a cluster without compliance settings", func() {
		_, set := preloadOf(plainCluster())
		Expect(set).To(BeFalse())
		rendered, err := spiloConfiguration(plainCluster())
		Expect(err).NotTo(HaveOccurred())
		Expect(rendered).NotTo(ContainSubstring(dcsPostgresql), "nothing is added to the Spilo configuration")
	})

	It("writes Spilo's default list in full, then pgaudit, once", func() {
		libraries, set := preloadOf(withCompliance(plainCluster(), dbv1alpha1.StandardHIPAA))
		Expect(set).To(BeTrue())
		Expect(libraries).To(Equal(spiloPreloadLibraries + ",pgaudit"))
		Expect(strings.Count(libraries, "pgaudit")).To(Equal(1))
		for _, kept := range []string{"bg_mon", "pg_stat_statements", "pgextwlist", "pg_auth_mon", "set_user", "timescaledb", "pg_cron", "pg_stat_kcache"} {
			Expect(strings.Split(libraries, ",")).To(ContainElement(kept), "an extension left out would stop loading")
		}
	})

	It("keeps what the configuration already carried", func() {
		rendered, err := spiloConfiguration(withCompliance(plainCluster(), dbv1alpha1.StandardPCI))
		Expect(err).NotTo(HaveOccurred())
		Expect(rendered).To(ContainSubstring("follower_label_value"))
		Expect(rendered).To(ContainSubstring("failsafe_mode"))
	})

	envOf := func(c *dbv1alpha1.PostgresCluster) []corev1.EnvVar {
		env, err := spiloEnv(c)
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		return env
	}

	It("changes the pod template when the settings are turned on, so the pods are replaced", func() {
		Expect(envOf(withCompliance(plainCluster(), dbv1alpha1.StandardHIPAA))).NotTo(Equal(envOf(plainCluster())))
	})

	DescribeTable("leaves the pod template alone, so the pods are not replaced, when",
		func(mutate func(*dbv1alpha1.PostgresCluster)) {
			before := withCompliance(plainCluster(), dbv1alpha1.StandardHIPAA)
			after := before.DeepCopy()
			mutate(after)
			Expect(envOf(after)).To(Equal(envOf(before)))
		},
		Entry("the standard changes", func(c *dbv1alpha1.PostgresCluster) { c.Spec.Compliance.Standard = dbv1alpha1.StandardSOC2 }),
		Entry("parameter logging is switched on", func(c *dbv1alpha1.PostgresCluster) { c.Spec.Compliance.AuditLogs.LogParameters = true }),
		Entry("the retention changes", func(c *dbv1alpha1.PostgresCluster) { c.Spec.Compliance.AuditLogs.RetentionDays = 400 }),
	)
})

var _ = Describe("The audit settings sent to Patroni", func() {
	hipaaParams := func(logParameters bool) map[string]any {
		preset, _ := dbv1alpha1.PresetFor(dbv1alpha1.StandardHIPAA)
		out := map[string]any{}
		for name, value := range preset.Parameters(logParameters) {
			out[name] = value
		}
		return out
	}
	liveWith := func(params map[string]any) map[string]any {
		return map[string]any{dcsPostgresql: map[string]any{dcsParameters: params}}
	}
	patched := func(patch map[string]any) map[string]any {
		GinkgoHelper()
		Expect(patch).To(HaveKey(dcsPostgresql))
		return patch[dcsPostgresql].(map[string]any)[dcsParameters].(map[string]any)
	}
	hipaa := func() *dbv1alpha1.PostgresCluster { return withCompliance(plainCluster(), dbv1alpha1.StandardHIPAA) }

	It("sends every setting to a cluster that has none of them", func() {
		got := patched(complianceParameterPatch(hipaa(), map[string]any{}))
		Expect(got).To(Equal(map[string]any{
			paramPgauditLog: hipaaClasses, paramPgauditCatalog: valueOff, paramPgauditParameter: valueOff, paramLogStatement: logStatementNone,
		}))
	})

	It("sends nothing when they are all in place", func() {
		Expect(complianceParameterPatch(hipaa(), liveWith(hipaaParams(false)))).To(BeNil())
	})

	It("turns parameter logging on only when the spec asks", func() {
		c := hipaa()
		c.Spec.Compliance.AuditLogs.LogParameters = true
		Expect(patched(complianceParameterPatch(c, liveWith(hipaaParams(false))))).To(Equal(map[string]any{paramPgauditParameter: "on"}))
	})

	It("turns parameter logging back off", func() {
		Expect(patched(complianceParameterPatch(hipaa(), liveWith(hipaaParams(true))))).
			To(Equal(map[string]any{paramPgauditParameter: valueOff}))
	})

	It("changes only the classes when one standard is swapped for another", func() {
		soc2 := withCompliance(plainCluster(), dbv1alpha1.StandardSOC2)
		Expect(patched(complianceParameterPatch(soc2, liveWith(hipaaParams(false))))).
			To(Equal(map[string]any{paramPgauditLog: "read,write,ddl"}))
	})

	It("puts back what is different, and leaves other settings alone", func() {
		live := hipaaParams(false)
		live[paramLogStatement] = spiloLogStatement
		live["max_connections"] = "200"
		Expect(patched(complianceParameterPatch(hipaa(), liveWith(live)))).To(Equal(map[string]any{paramLogStatement: logStatementNone}))
	})

	It("does nothing for a standard it has no preset for", func() {
		Expect(complianceParameterPatch(withCompliance(plainCluster(), "other"), map[string]any{})).To(BeNil())
	})

	Describe("after the compliance settings are removed", func() {
		It("deletes the pgaudit settings the operator wrote and restores Spilo's log_statement", func() {
			got := patched(complianceParameterPatch(plainCluster(), liveWith(hipaaParams(false))))
			Expect(got).To(Equal(map[string]any{
				paramPgauditLog: nil, paramPgauditCatalog: nil, paramPgauditParameter: nil, paramLogStatement: spiloLogStatement,
			}))
		})

		It("sends nothing to a cluster that never had them", func() {
			Expect(complianceParameterPatch(plainCluster(), map[string]any{})).To(BeNil())
			Expect(complianceParameterPatch(plainCluster(), liveWith(map[string]any{paramLogStatement: spiloLogStatement}))).To(BeNil())
		})

		It("leaves log_statement alone when there is no pgaudit setting to show the operator changed it", func() {
			Expect(complianceParameterPatch(plainCluster(), liveWith(map[string]any{paramLogStatement: logStatementNone}))).To(BeNil())
		})

		It("leaves a pgaudit setting someone else made, and a log_statement they chose", func() {
			live := map[string]any{"pgaudit.role": "auditor", paramLogStatement: "all"}
			Expect(complianceParameterPatch(plainCluster(), liveWith(live))).To(BeNil())
		})

		It("does not touch log_statement when it is no longer none", func() {
			live := hipaaParams(false)
			live[paramLogStatement] = "mod"
			Expect(patched(complianceParameterPatch(plainCluster(), liveWith(live)))).NotTo(HaveKey(paramLogStatement))
		})

		It("is idempotent: once deleted, nothing more is sent", func() {
			Expect(complianceParameterPatch(plainCluster(), liveWith(map[string]any{paramLogStatement: spiloLogStatement}))).To(BeNil())
		})
	})
})

// Through the reconciler, with a fake Patroni standing in for the leader.
var _ = Describe("Compliance settings through the reconciler", func() {
	const namespace = "default"
	ctx := context.Background()
	key := func(name string) types.NamespacedName { return types.NamespacedName{Name: name, Namespace: namespace} }

	var fake *fakePatroni
	var rec *PostgresClusterReconciler
	BeforeEach(func() {
		fake = &fakePatroni{live: map[string]any{
			dcsTTL: float64(30), dcsLoopWait: float64(10), dcsRetryTimeout: float64(10),
			dcsMaximumLagOnFailover: float64(1048576), dcsFailsafeMode: true, dcsSynchronousMode: false,
			dcsPostgresql: map[string]any{dcsParameters: map[string]any{paramLogStatement: spiloLogStatement}},
		}}
		rec = &PostgresClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Patroni: fake}
	})
	reconcileOnce := func(name string) {
		_, err := rec.Reconcile(ctx, reconcile.Request{NamespacedName: key(name)})
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
	}
	create := func(name string) {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: auditSecret, Namespace: namespace},
			StringData: map[string]string{secretKeyAccessKey: "id", secretKeySecretKey: testSecretValue},
		}
		if err := k8sClient.Create(ctx, secret); err == nil {
			DeferCleanup(func() { ExpectWithOffset(1, client.IgnoreNotFound(k8sClient.Delete(ctx, secret))).To(Succeed()) })
		} else {
			ExpectWithOffset(1, apierrors.IsAlreadyExists(err)).To(BeTrue())
		}
		c := &dbv1alpha1.PostgresCluster{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: dbv1alpha1.PostgresClusterSpec{
				Version: "17", Instances: 3,
				Storage:   dbv1alpha1.StorageSpec{Size: resource.MustParse("1Gi")},
				Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}},
			},
		}
		ExpectWithOffset(1, k8sClient.Create(ctx, c)).To(Succeed())
		DeferCleanup(func() { ExpectWithOffset(1, client.IgnoreNotFound(k8sClient.Delete(ctx, c))).To(Succeed()) })
	}
	startLeader := func(name, ip string) {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name + "-0", Namespace: namespace, Labels: selectorLabelsFor(name)},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: testPodImage}}},
		}
		ExpectWithOffset(1, k8sClient.Create(ctx, pod)).To(Succeed())
		DeferCleanup(func() {
			ExpectWithOffset(1, client.IgnoreNotFound(k8sClient.Delete(ctx, pod, client.GracePeriodSeconds(0)))).To(Succeed())
		})
		pod.Status.PodIP = ip
		ExpectWithOffset(1, k8sClient.Status().Update(ctx, pod)).To(Succeed())
		endpoints := &patroniEndpoints{ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: namespace, Labels: selectorLabelsFor(name),
			Annotations: map[string]string{annotationLeader: pod.Name},
		}}
		ExpectWithOffset(1, k8sClient.Create(ctx, endpoints)).To(Succeed())
		DeferCleanup(func() { ExpectWithOffset(1, client.IgnoreNotFound(k8sClient.Delete(ctx, endpoints))).To(Succeed()) })
	}
	update := func(name string, mutate func(*dbv1alpha1.PostgresCluster)) {
		c := &dbv1alpha1.PostgresCluster{}
		ExpectWithOffset(1, k8sClient.Get(ctx, key(name), c)).To(Succeed())
		mutate(c)
		ExpectWithOffset(1, k8sClient.Update(ctx, c)).To(Succeed())
	}
	templateEnv := func(name string) string {
		sts := &appsv1.StatefulSet{}
		ExpectWithOffset(1, k8sClient.Get(ctx, key(name), sts)).To(Succeed())
		for _, e := range sts.Spec.Template.Spec.Containers[0].Env {
			if e.Name == "SPILO_CONFIGURATION" {
				return e.Value
			}
		}
		return ""
	}
	patchedParameters := func() map[string]any {
		GinkgoHelper()
		Expect(fake.patched).To(HaveKey(dcsPostgresql))
		return fake.patched[dcsPostgresql].(map[string]any)[dcsParameters].(map[string]any)
	}

	It("loads pgaudit through the template and sends the settings to the leader, when the standard is set", func() {
		const name = "audit-enable"
		create(name)
		reconcileOnce(name)
		Expect(templateEnv(name)).NotTo(ContainSubstring("shared_preload_libraries"))

		update(name, func(c *dbv1alpha1.PostgresCluster) { withCompliance(c, dbv1alpha1.StandardHIPAA) })
		reconcileOnce(name)
		startLeader(name, "10.2.0.1")
		reconcileOnce(name)

		Expect(templateEnv(name)).To(ContainSubstring(spiloPreloadLibraries + ",pgaudit"))
		Expect(patchedParameters()).To(HaveKeyWithValue(paramPgauditLog, hipaaClasses))
		Expect(patchedParameters()).To(HaveKeyWithValue(paramLogStatement, logStatementNone))
	})

	It("does not change the template when one standard is swapped for another, and only reloads the classes", func() {
		const name = "audit-swap"
		create(name)
		update(name, func(c *dbv1alpha1.PostgresCluster) { withCompliance(c, dbv1alpha1.StandardHIPAA) })
		reconcileOnce(name)
		before := templateEnv(name)
		startLeader(name, "10.2.0.2")
		fake.live[dcsPostgresql] = map[string]any{dcsParameters: map[string]any{
			paramPgauditLog: hipaaClasses, paramPgauditCatalog: valueOff, paramPgauditParameter: valueOff, paramLogStatement: logStatementNone,
		}}

		update(name, func(c *dbv1alpha1.PostgresCluster) { c.Spec.Compliance.Standard = dbv1alpha1.StandardSOC2 })
		reconcileOnce(name)

		Expect(templateEnv(name)).To(Equal(before), "no template change, so no pod is replaced")
		Expect(patchedParameters()).To(Equal(map[string]any{paramPgauditLog: "read,write,ddl"}))
	})

	It("takes pgaudit back out of the template and deletes its settings, when the standard is removed", func() {
		const name = "audit-disable"
		create(name)
		update(name, func(c *dbv1alpha1.PostgresCluster) { withCompliance(c, dbv1alpha1.StandardPCI) })
		reconcileOnce(name)
		startLeader(name, "10.2.0.3")
		fake.live[dcsPostgresql] = map[string]any{dcsParameters: map[string]any{
			paramPgauditLog: hipaaClasses, paramPgauditCatalog: valueOff, paramPgauditParameter: valueOff, paramLogStatement: logStatementNone,
		}}
		Expect(templateEnv(name)).To(ContainSubstring("pgaudit"))

		update(name, func(c *dbv1alpha1.PostgresCluster) { c.Spec.Compliance = nil })
		reconcileOnce(name)

		Expect(templateEnv(name)).NotTo(ContainSubstring("pgaudit"))
		Expect(patchedParameters()).To(HaveKeyWithValue(paramPgauditLog, BeNil()))
		Expect(patchedParameters()).To(HaveKeyWithValue(paramLogStatement, spiloLogStatement))
	})

	It("sends nothing about the audit settings to a cluster that never had compliance settings", func() {
		const name = "audit-never"
		create(name)
		reconcileOnce(name)
		startLeader(name, "10.2.0.4")
		reconcileOnce(name)
		Expect(fake.patched).To(BeNil())
	})
})
