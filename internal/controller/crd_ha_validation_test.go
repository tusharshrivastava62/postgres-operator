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
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	dbv1alpha1 "github.com/tusharshrivastava62/postgres-operator/api/v1alpha1"
)

// Schema-level tests for spec.ha and spec.image, run against the envtest API
// server: defaults, bounds, and the explicit-zero round trip. As with the
// version tests, no reconciler or webhook is involved.
var _ = Describe("PostgresCluster CRD HA and image fields", func() {
	newCluster := func(name string) *dbv1alpha1.PostgresCluster {
		return &dbv1alpha1.PostgresCluster{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: crdTestNamespace},
			Spec: dbv1alpha1.PostgresClusterSpec{
				Version: "16",
				Storage: dbv1alpha1.StorageSpec{Size: resource.MustParse("1Gi")},
			},
		}
	}
	create := func(c *dbv1alpha1.PostgresCluster) error {
		err := k8sClient.Create(ctx, c)
		if err == nil {
			DeferCleanup(func() { Expect(k8sClient.Delete(ctx, c)).To(Succeed()) })
		}
		return err
	}

	It("fills in HA defaults when ha is omitted", func() {
		c := newCluster("crd-ha-defaults")
		Expect(create(c)).To(Succeed())

		got := &dbv1alpha1.PostgresCluster{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: c.Name, Namespace: crdTestNamespace}, got)).To(Succeed())
		Expect(got.Spec.HA.FailoverTimeout).To(Equal(int32(30)))
		Expect(got.Spec.HA.SynchronousMode).To(BeFalse())
		Expect(got.Spec.HA.MaximumLagOnFailover).NotTo(BeNil())
		Expect(*got.Spec.HA.MaximumLagOnFailover).To(Equal(int64(1048576)))
		Expect(got.Spec.Image).To(BeEmpty(), "image must stay unset so the operator can choose and record one")
	})

	DescribeTable("spec.ha.failoverTimeout bounds",
		func(seconds int32, accepted bool) {
			c := newCluster(fmt.Sprintf("crd-ha-timeout-%d", seconds))
			c.Spec.HA.FailoverTimeout = seconds
			err := create(c)
			if accepted {
				Expect(err).NotTo(HaveOccurred())
				return
			}
			Expect(err).To(HaveOccurred())
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected an Invalid error, got: %v", err)
			Expect(err.Error()).To(ContainSubstring("spec.ha.failoverTimeout"))
		},
		Entry("19 is below Patroni's floor of 20", int32(19), false),
		Entry("20 is the lowest allowed", int32(20), true),
		Entry("300 is the highest allowed", int32(300), true),
		Entry("301 is above the cap", int32(301), false),
	)

	Context("spec.ha.maximumLagOnFailover", func() {
		It("keeps an explicit 0 through a read-modify-write round trip", func() {
			c := newCluster("crd-ha-lag-zero")
			c.Spec.HA.MaximumLagOnFailover = ptr.To(int64(0))
			Expect(create(c)).To(Succeed())

			key := types.NamespacedName{Name: c.Name, Namespace: crdTestNamespace}
			got := &dbv1alpha1.PostgresCluster{}
			Expect(k8sClient.Get(ctx, key, got)).To(Succeed())
			Expect(got.Spec.HA.MaximumLagOnFailover).NotTo(BeNil())
			Expect(*got.Spec.HA.MaximumLagOnFailover).To(Equal(int64(0)))

			// The case the pointer type exists for: re-serializing the object
			// and sending it back must not turn 0 into the 1048576 default.
			got.Spec.Instances = 2
			Expect(k8sClient.Update(ctx, got)).To(Succeed())
			Expect(k8sClient.Get(ctx, key, got)).To(Succeed())
			Expect(*got.Spec.HA.MaximumLagOnFailover).To(Equal(int64(0)))
		})

		It("rejects a negative value", func() {
			c := newCluster("crd-ha-lag-negative")
			c.Spec.HA.MaximumLagOnFailover = ptr.To(int64(-1))
			err := create(c)
			Expect(err).To(HaveOccurred())
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected an Invalid error, got: %v", err)
			Expect(err.Error()).To(ContainSubstring("spec.ha.maximumLagOnFailover"))
		})
	})

	Context("spec.image", func() {
		It("accepts an explicit image", func() {
			c := newCluster("crd-image-set")
			c.Spec.Image = "ghcr.io/example/spilo-16:1.0"
			Expect(create(c)).To(Succeed())
		})

		// The typed client omits an empty string, so these go through
		// unstructured to actually send image: "".
		DescribeTable("rejects out-of-range lengths",
			func(name, image, wantMsg string) {
				u := &unstructured.Unstructured{Object: map[string]any{
					"spec": map[string]any{
						"version": "16",
						"storage": map[string]any{"size": "1Gi"},
						"image":   image,
					},
				}}
				u.SetGroupVersionKind(dbv1alpha1.GroupVersion.WithKind("PostgresCluster"))
				u.SetName(name)
				u.SetNamespace(crdTestNamespace)

				err := k8sClient.Create(ctx, u)
				Expect(err).To(HaveOccurred())
				Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected an Invalid error, got: %v", err)
				Expect(err.Error()).To(ContainSubstring("spec.image"))
				Expect(err.Error()).To(ContainSubstring(wantMsg))
			},
			Entry("empty string", "crd-image-empty", "", "at least 1"),
			Entry("longer than 512 characters", "crd-image-long", strings.Repeat("a", 513), "may not be more than 512"),
		)
	})
})
