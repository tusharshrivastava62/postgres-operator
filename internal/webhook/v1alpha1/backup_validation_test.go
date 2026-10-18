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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	dbv1alpha1 "github.com/tusharshrivastava62/postgres-operator/api/v1alpha1"
)

// nightlyCron is the base backup schedule the specs use.
const nightlyCron = "0 2 * * *"

func testDestination() *dbv1alpha1.BackupDestination {
	return &dbv1alpha1.BackupDestination{Bucket: "acme-db-backups", CredentialsSecret: "backup-s3"}
}

var _ = Describe("PostgresCluster backup validation", func() {
	var (
		obj       *dbv1alpha1.PostgresCluster
		validator PostgresClusterCustomValidator
	)

	BeforeEach(func() {
		obj = &dbv1alpha1.PostgresCluster{
			Spec: dbv1alpha1.PostgresClusterSpec{
				Version: "16", Instances: 3,
				Storage:   dbv1alpha1.StorageSpec{Size: resource.MustParse("1Gi")},
				Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}},
				Backup: dbv1alpha1.BackupSpec{
					Enabled: true, Schedule: nightlyCron, Destination: testDestination(),
					Retention: dbv1alpha1.BackupRetention{Full: 7, Days: 14},
				},
			},
		}
		obj.Name, obj.Namespace = "orders", "prod"
		validator = PostgresClusterCustomValidator{}
	})

	Context("the destination", func() {
		It("is required when backups are enabled", func() {
			obj.Spec.Backup.Destination = nil
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("spec.backup.destination"))
		})

		It("is not required when backups are off", func() {
			obj.Spec.Backup = dbv1alpha1.BackupSpec{}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
		})

		It("is checked on update too", func() {
			obj.Spec.Backup.Destination = nil
			_, err := validator.ValidateUpdate(ctx, obj.DeepCopy(), obj)
			Expect(err).To(HaveOccurred())
		})
	})

	Context("restore testing", func() {
		It("needs a schedule", func() {
			obj.Spec.Backup.Verification.Enabled = true
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("spec.backup.verification.schedule"))
		})

		It("needs backups to be enabled", func() {
			obj.Spec.Backup = dbv1alpha1.BackupSpec{Verification: dbv1alpha1.BackupVerification{Enabled: true, Schedule: "0 4 * * 0"}}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("spec.backup.verification.enabled"))
		})

		It("is admitted with a schedule", func() {
			obj.Spec.Backup.Verification = dbv1alpha1.BackupVerification{Enabled: true, Schedule: "0 4 * * 0"}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("restoring from a backup", func() {
		recovery := func() *dbv1alpha1.BootstrapSpec {
			return &dbv1alpha1.BootstrapSpec{Recovery: &dbv1alpha1.RecoverySpec{
				SourceCluster: "orders-old",
				Destination:   dbv1alpha1.BackupDestination{Bucket: "acme-db-backups", CredentialsSecret: "backup-s3"},
			}}
		}

		It("rejects a target time in the future", func() {
			obj.Spec.Bootstrap = recovery()
			future := metav1.NewTime(time.Now().Add(time.Hour))
			obj.Spec.Bootstrap.Recovery.TargetTime = &future
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("spec.bootstrap.recovery.targetTime"))
		})

		It("admits a target time in the past", func() {
			obj.Spec.Bootstrap = recovery()
			past := metav1.NewTime(time.Now().Add(-time.Hour))
			obj.Spec.Bootstrap.Recovery.TargetTime = &past
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
		})

		It("rejects a cluster that would archive into the backups it was restored from", func() {
			obj.Spec.Bootstrap = recovery()
			obj.Spec.Backup.Destination.Path = "prod/orders-old"
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("spec.backup.destination.path"))
		})

		It("works out the source's default path from its namespace and name", func() {
			obj.Spec.Bootstrap = recovery()
			obj.Spec.Bootstrap.Recovery.SourceNamespace = "staging"
			obj.Name = "orders-old"
			obj.Spec.Backup.Destination.Path = "staging/orders-old"
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
		})

		It("admits the same store when the paths differ", func() {
			obj.Spec.Bootstrap = recovery()
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred(), "own path prod/orders, source path prod/orders-old")
		})

		It("admits the same path in a different bucket", func() {
			obj.Spec.Bootstrap = recovery()
			obj.Spec.Bootstrap.Recovery.Destination.Bucket = "another-bucket"
			obj.Spec.Backup.Destination.Path = "prod/orders-old"
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("warnings", func() {
		It("warns about a single retained backup", func() {
			obj.Spec.Backup.Retention.Full = 1
			warnings, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
			Expect(warnings).To(ConsistOf(ContainSubstring("retention.full is 1")))
		})

		It("warns about a one-day recovery window", func() {
			obj.Spec.Backup.Retention.Days = 1
			warnings, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
			Expect(warnings).To(ConsistOf(ContainSubstring("retention.days is 1")))
		})

		It("is quiet for sensible settings", func() {
			warnings, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
			Expect(warnings).To(BeEmpty())
		})

		It("warns when the destination moves, since a new history starts there", func() {
			old := obj.DeepCopy()
			obj.Spec.Backup.Destination.Bucket = "other-bucket"
			warnings, err := validator.ValidateUpdate(ctx, old, obj)
			Expect(err).NotTo(HaveOccurred())
			Expect(warnings).To(ConsistOf(ContainSubstring("new backup history")))
		})

		It("warns when only the path changes", func() {
			old := obj.DeepCopy()
			obj.Spec.Backup.Destination.Path = "elsewhere"
			warnings, err := validator.ValidateUpdate(ctx, old, obj)
			Expect(err).NotTo(HaveOccurred())
			Expect(warnings).To(ConsistOf(ContainSubstring("new backup history")))
		})

		It("warns when the encryption key changes", func() {
			old := obj.DeepCopy()
			old.Spec.Backup.Encryption = &dbv1alpha1.BackupEncryption{KeySecret: "key-a"}
			obj.Spec.Backup.Encryption = &dbv1alpha1.BackupEncryption{KeySecret: "key-b"}
			warnings, err := validator.ValidateUpdate(ctx, old, obj)
			Expect(err).NotTo(HaveOccurred())
			Expect(warnings).To(ConsistOf(ContainSubstring("encryption key changed")))
		})

		It("does not warn about an unchanged destination", func() {
			warnings, err := validator.ValidateUpdate(ctx, obj.DeepCopy(), obj)
			Expect(err).NotTo(HaveOccurred())
			Expect(warnings).To(BeEmpty())
		})
	})
})
