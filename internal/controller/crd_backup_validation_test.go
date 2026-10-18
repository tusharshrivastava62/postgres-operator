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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	dbv1alpha1 "github.com/tusharshrivastava62/postgres-operator/api/v1alpha1"
)

// nightlyCron is the base backup schedule the specs use.
const nightlyCron = "0 2 * * *"

// Schema-level tests for spec.backup and spec.bootstrap against the envtest
// API server: defaults, bounds, required fields and immutability. No webhook
// or reconciler is involved, so these hold even when the webhook is down.
var _ = Describe("PostgresCluster CRD backup fields", func() {
	validDestination := func() *dbv1alpha1.BackupDestination {
		return &dbv1alpha1.BackupDestination{Bucket: testBucket, CredentialsSecret: "backup-s3"}
	}
	newCluster := func(name string, mutate ...func(*dbv1alpha1.PostgresCluster)) *dbv1alpha1.PostgresCluster {
		c := &dbv1alpha1.PostgresCluster{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: crdTestNamespace},
			Spec: dbv1alpha1.PostgresClusterSpec{
				Version: "16",
				Storage: dbv1alpha1.StorageSpec{Size: resource.MustParse("1Gi")},
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

	It("fills in retention and the destination type when they are omitted", func() {
		c := newCluster("crd-backup-defaults", func(c *dbv1alpha1.PostgresCluster) {
			c.Spec.Backup = dbv1alpha1.BackupSpec{Enabled: true, Schedule: nightlyCron, Destination: validDestination()}
		})
		Expect(create(c)).To(Succeed())

		got := &dbv1alpha1.PostgresCluster{}
		Expect(k8sClient.Get(ctx, key(c.Name), got)).To(Succeed())
		Expect(got.Spec.Backup.Retention.Full).To(Equal(int32(7)))
		Expect(got.Spec.Backup.Retention.Days).To(Equal(int32(14)))
		Expect(got.Spec.Backup.Destination.Type).To(Equal("s3"))
		Expect(got.Spec.Backup.Encryption).To(BeNil())
	})

	DescribeTable("destination fields",
		func(name string, mutate func(*dbv1alpha1.BackupDestination), accepted bool, wantField string) {
			c := newCluster(name, func(c *dbv1alpha1.PostgresCluster) {
				c.Spec.Backup = dbv1alpha1.BackupSpec{Enabled: true, Schedule: nightlyCron, Destination: validDestination()}
				mutate(c.Spec.Backup.Destination)
			})
			err := create(c)
			if accepted {
				Expect(err).NotTo(HaveOccurred())
				return
			}
			Expect(err).To(HaveOccurred())
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected an Invalid error, got: %v", err)
			Expect(err.Error()).To(ContainSubstring(wantField))
		},
		Entry("a normal bucket", "crd-dest-ok", func(d *dbv1alpha1.BackupDestination) {}, true, ""),
		Entry("a bucket name with dots", "crd-dest-dots", func(d *dbv1alpha1.BackupDestination) { d.Bucket = "acme.backups.eu" }, true, ""),
		Entry("a bucket name that is too short", "crd-dest-short", func(d *dbv1alpha1.BackupDestination) { d.Bucket = "ab" }, false, "bucket"),
		Entry("a bucket name with capitals", "crd-dest-caps", func(d *dbv1alpha1.BackupDestination) { d.Bucket = "Acme-Backups" }, false, "bucket"),
		Entry("a bucket name ending in a dash", "crd-dest-dash", func(d *dbv1alpha1.BackupDestination) { d.Bucket = "backups-" }, false, "bucket"),
		Entry("an unsupported store type", "crd-dest-type", func(d *dbv1alpha1.BackupDestination) { d.Type = "gcs" }, false, "type"),
		Entry("an https endpoint", "crd-dest-https", func(d *dbv1alpha1.BackupDestination) { d.Endpoint = "https://s3.example.com:9000" }, true, ""),
		Entry("an endpoint that is not http", "crd-dest-ftp", func(d *dbv1alpha1.BackupDestination) { d.Endpoint = "ftp://s3.example.com" }, false, "endpoint"),
		Entry("an endpoint with a space", "crd-dest-space", func(d *dbv1alpha1.BackupDestination) { d.Endpoint = "http://s3 example.com" }, false, "endpoint"),
		Entry("a nested path", "crd-dest-path", func(d *dbv1alpha1.BackupDestination) { d.Path = teamPath }, true, ""),
		Entry("a path with a leading slash", "crd-dest-lead", func(d *dbv1alpha1.BackupDestination) { d.Path = "/team" }, false, "path"),
		Entry("a path with a trailing slash", "crd-dest-trail", func(d *dbv1alpha1.BackupDestination) { d.Path = "team/" }, false, "path"),
		Entry("a path that climbs", "crd-dest-up", func(d *dbv1alpha1.BackupDestination) { d.Path = "team/../other" }, false, "path"),
		Entry("a path of dots in a name", "crd-dest-dotname", func(d *dbv1alpha1.BackupDestination) { d.Path = "team/v1.2" }, true, ""),
		Entry("a path with a space", "crd-dest-pspace", func(d *dbv1alpha1.BackupDestination) { d.Path = "my team" }, false, "path"),
		Entry("no credentials secret", "crd-dest-nocreds", func(d *dbv1alpha1.BackupDestination) { d.CredentialsSecret = "" }, false, "credentialsSecret"),
		Entry("no bucket", "crd-dest-nobucket", func(d *dbv1alpha1.BackupDestination) { d.Bucket = "" }, false, "bucket"),
	)

	DescribeTable("retention bounds",
		func(name string, full, days int32, accepted bool, wantField string) {
			c := newCluster(name, func(c *dbv1alpha1.PostgresCluster) {
				c.Spec.Backup = dbv1alpha1.BackupSpec{
					Enabled: true, Schedule: nightlyCron, Destination: validDestination(),
					Retention: dbv1alpha1.BackupRetention{Full: full, Days: days},
				}
			})
			err := create(c)
			if accepted {
				Expect(err).NotTo(HaveOccurred())
				return
			}
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(wantField))
		},
		Entry("the smallest values", "crd-ret-min", int32(1), int32(1), true, ""),
		Entry("the largest values", "crd-ret-max", int32(365), int32(3650), true, ""),
		Entry("more than 365 full backups", "crd-ret-fullmax", int32(366), int32(14), false, "retention.full"),
		Entry("more than 3650 days", "crd-ret-daysmax", int32(7), int32(3651), false, "retention.days"),
	)

	It("rejects a negative number of full backups", func() {
		c := newCluster("crd-ret-neg", func(c *dbv1alpha1.PostgresCluster) {
			c.Spec.Backup = dbv1alpha1.BackupSpec{
				Enabled: true, Schedule: nightlyCron, Destination: validDestination(),
				Retention: dbv1alpha1.BackupRetention{Full: -1, Days: 14},
			}
		})
		err := create(c)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("retention.full"))
	})

	It("rejects a schedule that is not five fields", func() {
		c := newCluster("crd-backup-badcron", func(c *dbv1alpha1.PostgresCluster) {
			c.Spec.Backup = dbv1alpha1.BackupSpec{Enabled: true, Schedule: "every night", Destination: validDestination()}
		})
		err := create(c)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("schedule"))
	})

	It("rejects an encryption block with no key secret", func() {
		c := newCluster("crd-enc-empty", func(c *dbv1alpha1.PostgresCluster) {
			c.Spec.Backup = dbv1alpha1.BackupSpec{
				Enabled: true, Schedule: nightlyCron, Destination: validDestination(),
				Encryption: &dbv1alpha1.BackupEncryption{},
			}
		})
		err := create(c)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("keySecret"))
	})

	Context("spec.bootstrap.recovery", func() {
		recovery := func(source string) func(*dbv1alpha1.PostgresCluster) {
			return func(c *dbv1alpha1.PostgresCluster) {
				c.Spec.Bootstrap = &dbv1alpha1.BootstrapSpec{Recovery: &dbv1alpha1.RecoverySpec{
					SourceCluster: source,
					Destination:   *validDestination(),
					TargetTime:    ptr.To(metav1.Date(2026, 10, 18, 7, 49, 27, 0, metav1.Now().Location())),
				}}
			}
		}

		It("is accepted at creation and keeps its target time", func() {
			c := newCluster("crd-recovery-ok", recovery("orders-old"))
			Expect(create(c)).To(Succeed())
			got := &dbv1alpha1.PostgresCluster{}
			Expect(k8sClient.Get(ctx, key(c.Name), got)).To(Succeed())
			Expect(got.Spec.Bootstrap.Recovery.SourceCluster).To(Equal("orders-old"))
			Expect(got.Spec.Bootstrap.Recovery.TargetTime).NotTo(BeNil())
		})

		It("needs a source cluster and a destination", func() {
			c := newCluster("crd-recovery-nosrc", func(c *dbv1alpha1.PostgresCluster) {
				c.Spec.Bootstrap = &dbv1alpha1.BootstrapSpec{Recovery: &dbv1alpha1.RecoverySpec{Destination: *validDestination()}}
			})
			err := create(c)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("sourceCluster"))
		})

		It("rejects a source encryption block with no key secret", func() {
			c := newCluster("crd-recovery-noenckey", func(c *dbv1alpha1.PostgresCluster) {
				c.Spec.Bootstrap = &dbv1alpha1.BootstrapSpec{Recovery: &dbv1alpha1.RecoverySpec{
					SourceCluster: "orders-old", Destination: *validDestination(), Encryption: &dbv1alpha1.BackupEncryption{},
				}}
			})
			err := create(c)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("keySecret"))
		})

		It("cannot be changed after creation", func() {
			c := newCluster("crd-recovery-change", recovery("orders-old"))
			Expect(create(c)).To(Succeed())
			Expect(k8sClient.Get(ctx, key(c.Name), c)).To(Succeed())
			c.Spec.Bootstrap.Recovery.SourceCluster = "somebody-else"
			err := k8sClient.Update(ctx, c)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("bootstrap is set when the cluster is created"))
		})

		It("cannot be removed after creation", func() {
			c := newCluster("crd-recovery-remove", recovery("orders-old"))
			Expect(create(c)).To(Succeed())
			Expect(k8sClient.Get(ctx, key(c.Name), c)).To(Succeed())
			c.Spec.Bootstrap = nil
			err := k8sClient.Update(ctx, c)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("bootstrap is set when the cluster is created"))
		})

		It("cannot be added to a cluster that was created without it", func() {
			c := newCluster("crd-recovery-add")
			Expect(create(c)).To(Succeed())
			Expect(k8sClient.Get(ctx, key(c.Name), c)).To(Succeed())
			recovery("orders-old")(c)
			err := k8sClient.Update(ctx, c)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("bootstrap is set when the cluster is created"))
		})

		It("leaves other fields editable when the bootstrap is unchanged", func() {
			c := newCluster("crd-recovery-edit", recovery("orders-old"))
			Expect(create(c)).To(Succeed())
			Expect(k8sClient.Get(ctx, key(c.Name), c)).To(Succeed())
			c.Spec.Instances = 2
			Expect(k8sClient.Update(ctx, c)).To(Succeed())
		})

		It("is not demanded of an ordinary cluster being edited", func() {
			c := newCluster("crd-nobootstrap-edit")
			Expect(create(c)).To(Succeed())
			Expect(k8sClient.Get(ctx, key(c.Name), c)).To(Succeed())
			c.Spec.Instances = 3
			Expect(k8sClient.Update(ctx, c)).To(Succeed())
		})
	})
})
