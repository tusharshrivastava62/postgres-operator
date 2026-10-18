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
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	dbv1alpha1 "github.com/tusharshrivastava62/postgres-operator/api/v1alpha1"
)

const (
	specField    = "spec"
	versionField = "version"
	storageField = "storage"
	sizeField    = "size"
	storageSize  = "1Gi"
)

// Schema-level tests for spec.compliance and status.compliance against the
// envtest API server. No webhook or reconciler is involved, so what holds here
// holds even when the webhook is down.
var _ = Describe("PostgresCluster CRD compliance fields", func() {
	newCluster := func(name string, mutate ...func(*dbv1alpha1.PostgresCluster)) *dbv1alpha1.PostgresCluster {
		c := &dbv1alpha1.PostgresCluster{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: crdTestNamespace},
			Spec: dbv1alpha1.PostgresClusterSpec{
				Version: "17",
				Storage: dbv1alpha1.StorageSpec{Size: resource.MustParse(storageSize)},
				Compliance: &dbv1alpha1.ComplianceSpec{
					Standard:  dbv1alpha1.StandardHIPAA,
					AuditLogs: dbv1alpha1.AuditLogsSpec{Destination: &dbv1alpha1.BackupDestination{Bucket: "audit-logs", CredentialsSecret: "audit-s3"}},
				},
			},
		}
		for _, m := range mutate {
			m(c)
		}
		return c
	}
	create := func(c *dbv1alpha1.PostgresCluster) error {
		err := k8sClient.Create(ctx, c)
		if err == nil {
			DeferCleanup(func() { Expect(k8sClient.Delete(ctx, c)).To(Succeed()) })
		}
		return err
	}
	key := func(name string) types.NamespacedName {
		return types.NamespacedName{Name: name, Namespace: crdTestNamespace}
	}
	expectInvalid := func(err error, field string) {
		GinkgoHelper()
		Expect(err).To(HaveOccurred())
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected an Invalid error, got: %v", err)
		Expect(err.Error()).To(ContainSubstring(field))
	}

	DescribeTable("spec.compliance.standard",
		func(name, standard string, accepted bool) {
			err := create(newCluster(name, func(c *dbv1alpha1.PostgresCluster) { c.Spec.Compliance.Standard = standard }))
			if accepted {
				Expect(err).NotTo(HaveOccurred())
				return
			}
			expectInvalid(err, "spec.compliance.standard")
		},
		Entry("hipaa", "crd-std-hipaa", "hipaa", true),
		Entry("soc2", "crd-std-soc2", "soc2", true),
		Entry("pci", "crd-std-pci", "pci", true),
		Entry("an unknown standard", "crd-std-other", "iso27001", false),
		Entry("upper case", "crd-std-upper", "HIPAA", false),
		Entry("empty", "crd-std-empty", "", false),
	)

	It("requires a standard when the compliance block is present", func() {
		u := &unstructured.Unstructured{Object: map[string]any{
			specField: map[string]any{
				versionField: "17", storageField: map[string]any{sizeField: storageSize},
				"compliance": map[string]any{},
			},
		}}
		u.SetGroupVersionKind(dbv1alpha1.GroupVersion.WithKind("PostgresCluster"))
		u.SetName("crd-std-missing")
		u.SetNamespace(crdTestNamespace)
		expectInvalid(k8sClient.Create(ctx, u), "standard")
	})

	It("is optional: a cluster without it is unchanged", func() {
		c := newCluster("crd-compliance-none", func(c *dbv1alpha1.PostgresCluster) { c.Spec.Compliance = nil })
		Expect(create(c)).To(Succeed())
		got := &dbv1alpha1.PostgresCluster{}
		Expect(k8sClient.Get(ctx, key(c.Name), got)).To(Succeed())
		Expect(got.Spec.Compliance).To(BeNil())
	})

	It("fills in the audit log defaults, with parameter logging off and no retention override", func() {
		c := newCluster("crd-audit-defaults", func(c *dbv1alpha1.PostgresCluster) { c.Spec.Compliance.AuditLogs.Destination = nil })
		Expect(create(c)).To(Succeed())

		got := &dbv1alpha1.PostgresCluster{}
		Expect(k8sClient.Get(ctx, key(c.Name), got)).To(Succeed())
		Expect(got.Spec.Compliance.AuditLogs.LogParameters).To(BeFalse())
		Expect(got.Spec.Compliance.AuditLogs.RetentionDays).To(BeZero(), "unset, so the standard's default applies")
		Expect(got.Spec.Compliance.AuditLogs.Destination).To(BeNil(), "the webhook, not the schema, requires it")
	})

	It("keeps an explicit logParameters through a round trip", func() {
		c := newCluster("crd-logparams", func(c *dbv1alpha1.PostgresCluster) { c.Spec.Compliance.AuditLogs.LogParameters = true })
		Expect(create(c)).To(Succeed())
		got := &dbv1alpha1.PostgresCluster{}
		Expect(k8sClient.Get(ctx, key(c.Name), got)).To(Succeed())
		Expect(got.Spec.Compliance.AuditLogs.LogParameters).To(BeTrue())
		got.Spec.Instances = 2
		Expect(k8sClient.Update(ctx, got)).To(Succeed())
		Expect(k8sClient.Get(ctx, key(c.Name), got)).To(Succeed())
		Expect(got.Spec.Compliance.AuditLogs.LogParameters).To(BeTrue())
	})

	DescribeTable("spec.compliance.auditLogs.retentionDays",
		func(name string, days int64, accepted bool) {
			// Through unstructured, so that an explicit 0 is sent instead of dropped.
			u := &unstructured.Unstructured{Object: map[string]any{
				specField: map[string]any{
					versionField: "17", storageField: map[string]any{sizeField: storageSize},
					"compliance": map[string]any{
						"standard": "hipaa",
						"auditLogs": map[string]any{
							"retentionDays": days,
							"destination":   map[string]any{"bucket": "audit-logs", "credentialsSecret": "audit-s3"},
						},
					},
				},
			}}
			u.SetGroupVersionKind(dbv1alpha1.GroupVersion.WithKind("PostgresCluster"))
			u.SetName(name)
			u.SetNamespace(crdTestNamespace)
			err := k8sClient.Create(ctx, u)
			if accepted {
				Expect(err).NotTo(HaveOccurred())
				DeferCleanup(func() { Expect(k8sClient.Delete(ctx, u)).To(Succeed()) })
				return
			}
			expectInvalid(err, "retentionDays")
		},
		Entry("0 is not a retention", "crd-ret-0", int64(0), false),
		Entry("negative", "crd-ret-neg", int64(-1), false),
		Entry("1 is the lowest", "crd-ret-1", int64(1), true),
		Entry("2190, the default for hipaa", "crd-ret-2190", int64(2190), true),
		Entry("7300 is the highest", "crd-ret-7300", int64(7300), true),
		Entry("7301 is over the cap", "crd-ret-7301", int64(7301), false),
	)

	It("checks the audit destination like any other destination", func() {
		err := create(newCluster("crd-audit-bucket", func(c *dbv1alpha1.PostgresCluster) {
			c.Spec.Compliance.AuditLogs.Destination.Bucket = "Audit-Logs"
		}))
		expectInvalid(err, "bucket")

		err = create(newCluster("crd-audit-nocreds", func(c *dbv1alpha1.PostgresCluster) {
			c.Spec.Compliance.AuditLogs.Destination.CredentialsSecret = ""
		}))
		expectInvalid(err, "credentialsSecret")
	})

	Context("spec.compliance.network.allowFrom", func() {
		peers := func(n int) []networkingv1.NetworkPolicyPeer {
			out := make([]networkingv1.NetworkPolicyPeer, n)
			for i := range out {
				out[i] = networkingv1.NetworkPolicyPeer{PodSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"app": fmt.Sprintf("client-%d", i)},
				}}
			}
			return out
		}

		It("accepts a peer", func() {
			Expect(create(newCluster("crd-allow-ok", func(c *dbv1alpha1.PostgresCluster) {
				c.Spec.Compliance.Network.AllowFrom = peers(1)
			}))).To(Succeed())
		})

		It("accepts 20 peers and rejects 21", func() {
			Expect(create(newCluster("crd-allow-20", func(c *dbv1alpha1.PostgresCluster) {
				c.Spec.Compliance.Network.AllowFrom = peers(20)
			}))).To(Succeed())
			expectInvalid(create(newCluster("crd-allow-21", func(c *dbv1alpha1.PostgresCluster) {
				c.Spec.Compliance.Network.AllowFrom = peers(21)
			})), "allowFrom")
		})
	})

	Context("changing the standard on an existing cluster", func() {
		// Unlike version and bootstrap, the standard is not immutable: it can be
		// added, changed and removed, and the operator rolls the pods to match.
		It("allows it to be added, changed and removed", func() {
			c := newCluster("crd-std-change", func(c *dbv1alpha1.PostgresCluster) { c.Spec.Compliance = nil })
			Expect(create(c)).To(Succeed())
			got := &dbv1alpha1.PostgresCluster{}

			Expect(k8sClient.Get(ctx, key(c.Name), got)).To(Succeed())
			got.Spec.Compliance = newCluster("x").Spec.Compliance
			Expect(k8sClient.Update(ctx, got)).To(Succeed())

			Expect(k8sClient.Get(ctx, key(c.Name), got)).To(Succeed())
			got.Spec.Compliance.Standard = dbv1alpha1.StandardPCI
			Expect(k8sClient.Update(ctx, got)).To(Succeed())
			Expect(k8sClient.Get(ctx, key(c.Name), got)).To(Succeed())
			Expect(got.Spec.Compliance.Standard).To(Equal(dbv1alpha1.StandardPCI))

			got.Spec.Compliance = nil
			Expect(k8sClient.Update(ctx, got)).To(Succeed())
			Expect(k8sClient.Get(ctx, key(c.Name), got)).To(Succeed())
			Expect(got.Spec.Compliance).To(BeNil())
		})
	})

	Context("status.compliance", func() {
		withControls := func(controls ...dbv1alpha1.ComplianceControl) *dbv1alpha1.PostgresCluster {
			c := newCluster(fmt.Sprintf("crd-status-%d", len(controls)))
			Expect(create(c)).To(Succeed())
			Expect(k8sClient.Get(ctx, key(c.Name), c)).To(Succeed())
			now := metav1.Now()
			c.Status.Compliance = dbv1alpha1.ComplianceStatus{Standard: "hipaa", LastChecked: &now, Controls: controls}
			return c
		}
		control := func(id, status string) dbv1alpha1.ComplianceControl {
			now := metav1.Now()
			return dbv1alpha1.ComplianceControl{ID: id, Status: status, Evidence: "seen", ObservedAt: &now}
		}

		It("stores the three control results", func() {
			c := withControls(
				control(dbv1alpha1.ControlAuditLogging, dbv1alpha1.ControlMet),
				control(dbv1alpha1.ControlNetworkIsolation, dbv1alpha1.ControlNotMet),
				control(dbv1alpha1.ControlAuditLogRetention, dbv1alpha1.ControlUnknown),
			)
			Expect(k8sClient.Status().Update(ctx, c)).To(Succeed())
			got := &dbv1alpha1.PostgresCluster{}
			Expect(k8sClient.Get(ctx, key(c.Name), got)).To(Succeed())
			Expect(got.Status.Compliance.Controls).To(HaveLen(3))
			Expect(got.Status.Compliance.Controls[1].Status).To(Equal(dbv1alpha1.ControlNotMet))
		})

		It("rejects a result that is not Met, NotMet or Unknown", func() {
			c := withControls(control(dbv1alpha1.ControlAuditLogging, "Passing"))
			expectInvalid(k8sClient.Status().Update(ctx, c), "status")
		})

		It("rejects a control with no id", func() {
			c := withControls(control("", dbv1alpha1.ControlMet))
			expectInvalid(k8sClient.Status().Update(ctx, c), "id")
		})

		It("rejects the same control twice, so a result is never ambiguous", func() {
			c := withControls(
				control(dbv1alpha1.ControlAuditLogging, dbv1alpha1.ControlMet),
				control(dbv1alpha1.ControlAuditLogging, dbv1alpha1.ControlNotMet),
				control(dbv1alpha1.ControlAvailability, dbv1alpha1.ControlMet),
			)
			Expect(k8sClient.Status().Update(ctx, c)).NotTo(Succeed())
		})

		It("rejects evidence that is too long to keep", func() {
			long := control(dbv1alpha1.ControlAuditLogging, dbv1alpha1.ControlMet)
			for len(long.Evidence) <= 1024 {
				long.Evidence += "x"
			}
			c := withControls(long)
			expectInvalid(k8sClient.Status().Update(ctx, c), "evidence")
		})
	})
})
