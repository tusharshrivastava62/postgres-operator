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
	"context"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	dbv1alpha1 "github.com/tusharshrivastava62/postgres-operator/api/v1alpha1"
)

var _ = Describe("PostgresCluster security validation", func() {
	ctx := context.Background()
	var (
		cluster   *dbv1alpha1.PostgresCluster
		validator PostgresClusterCustomValidator
	)
	withTLS := func(secret string) *dbv1alpha1.PostgresCluster {
		c := cluster.DeepCopy()
		c.Spec.Security = &dbv1alpha1.SecuritySpec{TLS: &dbv1alpha1.TLSSpec{SecretName: secret}}
		return c
	}
	BeforeEach(func() {
		cluster = &dbv1alpha1.PostgresCluster{
			ObjectMeta: metav1.ObjectMeta{Name: ordersName, Namespace: prodNamespace},
			Spec: dbv1alpha1.PostgresClusterSpec{
				Version: "17",
				Storage: dbv1alpha1.StorageSpec{Size: resource.MustParse("1Gi")},
				Resources: corev1.ResourceRequirements{
					Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
				},
			},
		}
		validator = PostgresClusterCustomValidator{}
	})

	It("admits a cluster with no security settings, saying nothing about them", func() {
		warnings, err := validator.ValidateCreate(ctx, cluster)
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.Join(warnings, " ")).NotTo(ContainSubstring("spec.security"))
	})

	It("admits a Secret that does not exist yet, since cert-manager may make it after the cluster", func() {
		_, err := validator.ValidateCreate(ctx, withTLS("orders-tls"))
		Expect(err).NotTo(HaveOccurred())
	})

	It("refuses a Secret name that is not a name, with the field in the message, on create and on update", func() {
		_, err := validator.ValidateCreate(ctx, withTLS("Not A Name"))
		Expect(err).To(MatchError(ContainSubstring("spec.security.tls.secretName")))
		_, err = validator.ValidateUpdate(ctx, cluster, withTLS("Not A Name"))
		Expect(err).To(MatchError(ContainSubstring("spec.security.tls.secretName")))
	})

	It("warns that every pod is replaced when a certificate is first asked for, and when another is", func() {
		warnings, err := validator.ValidateCreate(ctx, withTLS("orders-tls"))
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.Join(warnings, " ")).To(ContainSubstring("every database pod is replaced"))

		warnings, err = validator.ValidateUpdate(ctx, cluster, withTLS("orders-tls"))
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.Join(warnings, " ")).To(ContainSubstring("every database pod is replaced"))

		warnings, err = validator.ValidateUpdate(ctx, withTLS("orders-tls"), withTLS("orders-tls-2"))
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.Join(warnings, " ")).To(ContainSubstring("every database pod is replaced"))
	})

	It("warns that taking the certificate away does the same, and what the server goes back to", func() {
		warnings, err := validator.ValidateUpdate(ctx, withTLS("orders-tls"), cluster)
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.Join(warnings, " ")).To(And(ContainSubstring("replaces every database pod"), ContainSubstring("made for itself")))
	})

	It("does not warn about a change that leaves the certificate as it was", func() {
		changed := withTLS("orders-tls")
		changed.Spec.Instances = 3
		warnings, err := validator.ValidateUpdate(ctx, withTLS("orders-tls"), changed)
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.Join(warnings, " ")).NotTo(ContainSubstring("spec.security"))
	})
})
