/*
Copyright 2026.

Licensed under the Business Source License 1.1 (the "License"). You may not
use this file except in compliance with the License, a copy of which is in the
LICENSE file at the root of this repository. Production use requires a
commercial license from the Licensor. On the Change Date (2030-10-17) the
work becomes available under the Change License (Apache-2.0).
*/

package v1alpha1

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	dbv1alpha1 "github.com/tusharshrivastava62/postgres-operator/api/v1alpha1"
)

const (
	auditDestinationField = "spec.compliance.auditLogs.destination"
	ordersBackupPath      = "prod/orders"
	minioEndpoint         = "http://minio:9000"
)

var _ = Describe("PostgresCluster compliance validation", func() {
	var (
		obj       *dbv1alpha1.PostgresCluster
		validator PostgresClusterCustomValidator
	)

	// auditDestination is a bucket for audit logs with a path of its own.
	auditDestination := func(path string) *dbv1alpha1.BackupDestination {
		d := testDestination()
		d.Path = path
		return d
	}

	BeforeEach(func() {
		obj = &dbv1alpha1.PostgresCluster{
			Spec: dbv1alpha1.PostgresClusterSpec{
				Version: "17", Instances: 3,
				Storage:   dbv1alpha1.StorageSpec{Size: resource.MustParse("1Gi")},
				Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}},
				Backup: dbv1alpha1.BackupSpec{
					Enabled: true, Schedule: nightlyCron, Destination: testDestination(),
					Retention: dbv1alpha1.BackupRetention{Full: 7, Days: 14},
				},
				Compliance: &dbv1alpha1.ComplianceSpec{
					Standard:  dbv1alpha1.StandardHIPAA,
					AuditLogs: dbv1alpha1.AuditLogsSpec{Destination: auditDestination("audit-logs/orders")},
				},
			},
		}
		obj.Name, obj.Namespace = "orders", "prod"
		validator = PostgresClusterCustomValidator{}
	})

	It("admits a cluster without compliance settings, and says nothing about them", func() {
		obj.Spec.Compliance = nil
		warnings, err := validator.ValidateCreate(ctx, obj)
		Expect(err).NotTo(HaveOccurred())
		Expect(warnings).To(BeEmpty())
	})

	It("admits a standard with a destination of its own", func() {
		warnings, err := validator.ValidateCreate(ctx, obj)
		Expect(err).NotTo(HaveOccurred())
		Expect(warnings).To(BeEmpty())
	})

	Context("the destination", func() {
		DescribeTable("is required for every standard",
			func(standard string) {
				obj.Spec.Compliance.Standard = standard
				obj.Spec.Compliance.AuditLogs.Destination = nil
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring(auditDestinationField))
			},
			Entry("hipaa", dbv1alpha1.StandardHIPAA),
			Entry("soc2", dbv1alpha1.StandardSOC2),
			Entry("pci", dbv1alpha1.StandardPCI),
		)

		It("is checked on update too", func() {
			obj.Spec.Compliance.AuditLogs.Destination = nil
			_, err := validator.ValidateUpdate(ctx, obj.DeepCopy(), obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(auditDestinationField))
		})

		It("needs a region for Amazon S3, as backups do", func() {
			obj.Spec.Compliance.AuditLogs.Destination.Region = ""
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(auditDestinationField + ".region"))
		})

		It("can leave the region out for a store with its own endpoint", func() {
			obj.Spec.Compliance.AuditLogs.Destination.Region = ""
			obj.Spec.Compliance.AuditLogs.Destination.Endpoint = minioEndpoint
			obj.Spec.Backup.Destination.Endpoint = minioEndpoint
			obj.Spec.Backup.Destination.Region = ""
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("sharing a path with the backups", func() {
		DescribeTable("is rejected in the same bucket when the paths overlap",
			func(auditPath, backupPath string) {
				obj.Spec.Compliance.AuditLogs.Destination.Path = auditPath
				obj.Spec.Backup.Destination.Path = backupPath
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring(auditDestinationField + ".path"))
				Expect(err.Error()).To(ContainSubstring("backups"))
			},
			Entry("the same path", ordersBackupPath, ordersBackupPath),
			Entry("the same path with stray slashes", "/prod/orders/", ordersBackupPath),
			Entry("inside the backup path", "prod/orders/audit", ordersBackupPath),
			Entry("the backup path inside it", "prod", ordersBackupPath),
		)

		It("rejects an audit path equal to the backup path the cluster gets by default", func() {
			obj.Spec.Backup.Destination.Path = ""
			obj.Spec.Compliance.AuditLogs.Destination.Path = ordersBackupPath
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
		})

		It("admits the default audit path next to the default backup path", func() {
			obj.Spec.Backup.Destination.Path = ""
			obj.Spec.Compliance.AuditLogs.Destination.Path = ""
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred(), "audit/prod/orders and prod/orders do not nest")
		})

		It("compares whole path segments, so a longer name is a different path", func() {
			obj.Spec.Backup.Destination.Path = ordersBackupPath
			obj.Spec.Compliance.AuditLogs.Destination.Path = "prod/orders-audit"
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
		})

		It("admits the same path in another bucket", func() {
			obj.Spec.Backup.Destination.Path = ordersBackupPath
			obj.Spec.Compliance.AuditLogs.Destination.Path = ordersBackupPath
			obj.Spec.Compliance.AuditLogs.Destination.Bucket = "acme-audit-logs"
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
		})

		It("has nothing to share with when backups are off", func() {
			obj.Spec.Backup = dbv1alpha1.BackupSpec{}
			obj.Spec.Compliance.AuditLogs.Destination.Path = ordersBackupPath
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("warnings", func() {
		It("warns when parameters are logged, since they are written in clear text", func() {
			obj.Spec.Compliance.AuditLogs.LogParameters = true
			warnings, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
			Expect(warnings).To(ConsistOf(And(ContainSubstring("logParameters is true"), ContainSubstring("clear text"))))
		})

		It("does not warn when they are not logged", func() {
			obj.Spec.Compliance.AuditLogs.LogParameters = false
			warnings, _ := validator.ValidateCreate(ctx, obj)
			Expect(warnings).To(BeEmpty())
		})

		It("says no restart is needed when one standard is swapped for another", func() {
			updated := obj.DeepCopy()
			updated.Spec.Compliance.Standard = dbv1alpha1.StandardPCI
			warnings, err := validator.ValidateUpdate(ctx, obj, updated)
			Expect(err).NotTo(HaveOccurred())
			Expect(warnings).To(ConsistOf(And(
				ContainSubstring(`from "hipaa" to "pci"`), ContainSubstring("without restarting"))))
			Expect(warnings[0]).NotTo(ContainSubstring("replaced"), "only the settings that reload change")
		})

		It("warns when a standard is added to a running cluster", func() {
			before := obj.DeepCopy()
			before.Spec.Compliance = nil
			warnings, err := validator.ValidateUpdate(ctx, before, obj)
			Expect(err).NotTo(HaveOccurred())
			Expect(warnings).To(ConsistOf(And(ContainSubstring(`from none to "hipaa"`), ContainSubstring("replaced one at a time"))))
		})

		It("warns when a standard is removed from a running cluster", func() {
			after := obj.DeepCopy()
			after.Spec.Compliance = nil
			warnings, err := validator.ValidateUpdate(ctx, obj, after)
			Expect(err).NotTo(HaveOccurred())
			Expect(warnings).To(ConsistOf(And(ContainSubstring(`from "hipaa" to none`), ContainSubstring("replaced one at a time"))))
		})

		It("does not warn when the standard is unchanged, whatever else changed", func() {
			updated := obj.DeepCopy()
			updated.Spec.Instances = 5
			updated.Spec.Compliance.AuditLogs.RetentionDays = 400
			warnings, err := validator.ValidateUpdate(ctx, obj, updated)
			Expect(err).NotTo(HaveOccurred())
			Expect(warnings).To(BeEmpty())
		})

		It("warns that a custom image must contain pgaudit and the preloaded extensions", func() {
			obj.Spec.Image = "registry.example/postgres:17"
			warnings, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
			Expect(warnings).To(ConsistOf(And(ContainSubstring("spec.image"), ContainSubstring("pgaudit"))))
		})

		It("does not warn about the image when there are no compliance settings", func() {
			obj.Spec.Image = "registry.example/postgres:17"
			obj.Spec.Compliance = nil
			warnings, _ := validator.ValidateCreate(ctx, obj)
			Expect(warnings).To(BeEmpty())
		})

		It("does not warn about a roll on creation", func() {
			warnings, _ := validator.ValidateCreate(ctx, obj)
			Expect(warnings).To(BeEmpty())
		})
	})
})
