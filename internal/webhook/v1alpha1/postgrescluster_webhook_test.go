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
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dbv1alpha1 "github.com/tusharshrivastava62/postgres-operator/api/v1alpha1"
)

var _ = Describe("PostgresCluster Webhook", func() {
	var (
		obj       *dbv1alpha1.PostgresCluster
		oldObj    *dbv1alpha1.PostgresCluster
		validator PostgresClusterCustomValidator
	)

	BeforeEach(func() {
		obj = &dbv1alpha1.PostgresCluster{
			Spec: dbv1alpha1.PostgresClusterSpec{
				Version: "16",
				Storage: dbv1alpha1.StorageSpec{Size: resource.MustParse("1Gi")},
				// A valid memory limit, so each spec below isolates the one rule
				// it is about instead of tripping the mandatory-limit check.
				Resources: corev1.ResourceRequirements{
					Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
				},
			},
		}
		oldObj = obj.DeepCopy()
		validator = PostgresClusterCustomValidator{}
	})

	Context("backup.schedule cross-field validation", func() {
		It("rejects create when backup is enabled with no schedule", func() {
			obj.Spec.Backup.Enabled = true
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("spec.backup.schedule"))
		})

		It("admits create when backup is enabled with a schedule and a destination", func() {
			obj.Spec.Backup.Enabled = true
			obj.Spec.Backup.Schedule = nightlyCron
			obj.Spec.Backup.Destination = testDestination()
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
		})

		It("admits create when backup is disabled", func() {
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
		})

		It("rejects update when backup is enabled with no schedule", func() {
			obj.Spec.Backup.Enabled = true
			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("spec.backup.schedule"))
		})
	})

	Context("storage shrink validation on update", func() {
		It("rejects reducing storage size", func() {
			oldObj.Spec.Storage.Size = resource.MustParse("10Gi")
			obj.Spec.Storage.Size = resource.MustParse("5Gi")
			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("spec.storage.size"))
		})

		It("admits growing storage size", func() {
			oldObj.Spec.Storage.Size = resource.MustParse("1Gi")
			obj.Spec.Storage.Size = resource.MustParse("5Gi")
			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).NotTo(HaveOccurred())
		})

		It("admits unchanged storage size", func() {
			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("resources validation", func() {
		It("rejects create with no resources at all", func() {
			obj.Spec.Resources = corev1.ResourceRequirements{}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("spec.resources.limits[memory]"))
		})

		It("rejects create with only a cpu limit", func() {
			obj.Spec.Resources.Limits = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("spec.resources.limits[memory]"))
		})

		It("rejects create with only a memory request", func() {
			obj.Spec.Resources = corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")},
			}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("spec.resources.limits[memory]"))
		})

		It("rejects a zero memory limit", func() {
			obj.Spec.Resources.Limits[corev1.ResourceMemory] = resource.MustParse("0")
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("spec.resources.limits[memory]"))
		})

		It("rejects update that removes the memory limit", func() {
			obj.Spec.Resources = corev1.ResourceRequirements{}
			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("spec.resources.limits[memory]"))
		})

		DescribeTable("rejects a request above its limit",
			func(name corev1.ResourceName, request, limit string) {
				obj.Spec.Resources = corev1.ResourceRequirements{
					Requests: corev1.ResourceList{name: resource.MustParse(request)},
					Limits: corev1.ResourceList{
						corev1.ResourceMemory: resource.MustParse("4Gi"),
						name:                  resource.MustParse(limit),
					},
				}
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring(fmt.Sprintf("spec.resources.requests[%s]", name)))
			},
			Entry("memory", corev1.ResourceMemory, "2Gi", "1Gi"),
			Entry("cpu", corev1.ResourceCPU, "2", "500m"),
		)

		It("admits a request equal to its limit", func() {
			obj.Spec.Resources = corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
				Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
			}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
		})

		It("admits a cpu request with no cpu limit", func() {
			obj.Spec.Resources.Requests = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m")}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
		})

		It("reports a missing limit and a bad request together", func() {
			obj.Spec.Resources = corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
				Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
			}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("spec.resources.limits[memory]"))
			Expect(err.Error()).To(ContainSubstring("spec.resources.requests[cpu]"))
		})
	})

	Context("admission warnings", func() {
		It("warns, without rejecting, when the memory limit is below 512Mi", func() {
			obj.Spec.Resources.Limits[corev1.ResourceMemory] = resource.MustParse("256Mi")
			warnings, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
			Expect(warnings).To(HaveLen(1))
			Expect(warnings[0]).To(ContainSubstring("spec.resources.limits.memory"))
		})

		It("does not warn at exactly 512Mi", func() {
			obj.Spec.Resources.Limits[corev1.ResourceMemory] = resource.MustParse("512Mi")
			warnings, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
			Expect(warnings).To(BeEmpty())
		})

		It("warns that synchronousMode is pointless with one instance", func() {
			obj.Spec.Instances = 1
			obj.Spec.HA.SynchronousMode = true
			warnings, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
			Expect(warnings).To(ConsistOf(ContainSubstring("spec.ha.synchronousMode")))
		})

		It("treats unset instances as one for the synchronousMode warning", func() {
			obj.Spec.Instances = 0
			obj.Spec.HA.SynchronousMode = true
			warnings, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
			Expect(warnings).To(ConsistOf(ContainSubstring("spec.ha.synchronousMode")))
		})

		It("does not warn about synchronousMode with several instances", func() {
			obj.Spec.Instances = 3
			obj.Spec.HA.SynchronousMode = true
			warnings, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
			Expect(warnings).To(BeEmpty())
		})

		It("also warns on update", func() {
			obj.Spec.Resources.Limits[corev1.ResourceMemory] = resource.MustParse("256Mi")
			warnings, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).NotTo(HaveOccurred())
			Expect(warnings).To(HaveLen(1))
		})

		It("returns no warnings for a well-formed spec", func() {
			warnings, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
			Expect(warnings).To(BeEmpty())
		})
	})

	// These go through the API server and the real webhook server started in
	// the suite, so they prove the webhook is wired up and the field paths in
	// the errors survive admission, not just that the Go function is correct.
	Context("through the admission chain", func() {
		newCluster := func(name string) *dbv1alpha1.PostgresCluster {
			c := obj.DeepCopy()
			c.Name = name
			c.Namespace = "default"
			return c
		}

		It("rejects a cluster with no memory limit", func() {
			c := newCluster("webhook-no-limit")
			c.Spec.Resources = corev1.ResourceRequirements{}
			err := k8sClient.Create(ctx, c)
			Expect(err).To(HaveOccurred())
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected an Invalid error, got: %v", err)
			Expect(err.Error()).To(ContainSubstring("spec.resources.limits[memory]"))
		})

		It("rejects backups with no destination, which the schema alone cannot express", func() {
			c := newCluster("webhook-no-destination")
			c.Spec.Backup = dbv1alpha1.BackupSpec{Enabled: true, Schedule: nightlyCron}
			err := k8sClient.Create(ctx, c)
			Expect(err).To(HaveOccurred())
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected an Invalid error, got: %v", err)
			Expect(err.Error()).To(ContainSubstring("spec.backup.destination"))
		})

		It("admits a valid cluster, then rejects an update that drops the limit", func() {
			c := newCluster("webhook-drop-limit")
			Expect(k8sClient.Create(ctx, c)).To(Succeed())
			DeferCleanup(func() { Expect(k8sClient.Delete(ctx, c)).To(Succeed()) })

			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(c), c)).To(Succeed())
			c.Spec.Resources = corev1.ResourceRequirements{}
			err := k8sClient.Update(ctx, c)
			Expect(err).To(HaveOccurred())
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected an Invalid error, got: %v", err)
			Expect(err.Error()).To(ContainSubstring("spec.resources.limits[memory]"))
		})
	})
})
