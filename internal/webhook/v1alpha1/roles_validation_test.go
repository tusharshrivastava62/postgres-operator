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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	dbv1alpha1 "github.com/tusharshrivastava62/postgres-operator/api/v1alpha1"
)

const (
	appRole       = "app"
	prodNamespace = "prod"
)

var _ = Describe("PostgresCluster roles validation", func() {
	ctx := context.Background()
	var (
		cluster   *dbv1alpha1.PostgresCluster
		validator PostgresClusterCustomValidator
	)
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

	It("admits a cluster with ordinary roles, on create and on update", func() {
		cluster.Spec.Roles = []dbv1alpha1.RoleSpec{{Name: "app_orders", Database: ordersName}, {Name: "reporting"}}
		_, err := validator.ValidateCreate(ctx, cluster)
		Expect(err).NotTo(HaveOccurred())
		_, err = validator.ValidateUpdate(ctx, cluster.DeepCopy(), cluster)
		Expect(err).NotTo(HaveOccurred())
	})

	It("refuses a role named like one the system uses, with the field in the message", func() {
		cluster.Spec.Roles = []dbv1alpha1.RoleSpec{{Name: "postgres"}}
		_, err := validator.ValidateCreate(ctx, cluster)
		Expect(err).To(MatchError(ContainSubstring("spec.roles[0].name")))
		_, err = validator.ValidateUpdate(ctx, cluster.DeepCopy(), cluster)
		Expect(err).To(MatchError(ContainSubstring("spec.roles[0].name")))
	})

	It("refuses a role whose Secret would overwrite one the operator owns", func() {
		cluster.Spec.Roles = []dbv1alpha1.RoleSpec{{Name: appRole, SecretName: "orders-credentials"}}
		_, err := validator.ValidateCreate(ctx, cluster)
		Expect(err).To(MatchError(ContainSubstring("the cluster's own credentials")))
	})

	It("refuses two roles that name one database", func() {
		cluster.Spec.Roles = []dbv1alpha1.RoleSpec{{Name: "a", Database: "d"}, {Name: "b", Database: "d"}}
		_, err := validator.ValidateCreate(ctx, cluster)
		Expect(err).To(MatchError(ContainSubstring("one owner")))
	})

	Describe("what it warns about", func() {
		It("says nothing about a new cluster, or a role that is added", func() {
			cluster.Spec.Roles = []dbv1alpha1.RoleSpec{{Name: appRole}}
			warnings, err := validator.ValidateCreate(ctx, cluster)
			Expect(err).NotTo(HaveOccurred())
			Expect(warnings).To(BeEmpty())

			before := cluster.DeepCopy()
			cluster.Spec.Roles = append(cluster.Spec.Roles, dbv1alpha1.RoleSpec{Name: "another"})
			warnings, err = validator.ValidateUpdate(ctx, before, cluster)
			Expect(err).NotTo(HaveOccurred())
			Expect(warnings).To(BeEmpty())
		})

		It("warns that a removed role is disabled and kept, not dropped", func() {
			cluster.Spec.Roles = []dbv1alpha1.RoleSpec{{Name: appRole}, {Name: "old_app"}}
			before := cluster.DeepCopy()
			cluster.Spec.Roles = cluster.Spec.Roles[:1]
			warnings, err := validator.ValidateUpdate(ctx, before, cluster)
			Expect(err).NotTo(HaveOccurred())
			Expect(warnings).To(HaveLen(1))
			Expect(warnings[0]).To(ContainSubstring("role old_app was removed"))
			Expect(warnings[0]).To(ContainSubstring("login will be disabled"))
			Expect(warnings[0]).To(ContainSubstring("are kept"))
		})

		It("warns that a database a role stops naming is kept", func() {
			cluster.Spec.Roles = []dbv1alpha1.RoleSpec{{Name: appRole, Database: "first"}}
			before := cluster.DeepCopy()
			cluster.Spec.Roles[0].Database = "second"
			warnings, err := validator.ValidateUpdate(ctx, before, cluster)
			Expect(err).NotTo(HaveOccurred())
			Expect(warnings).To(HaveLen(1))
			Expect(warnings[0]).To(ContainSubstring("no longer names database first"))
		})

		It("does not warn when a role only gains a database or a connection limit", func() {
			cluster.Spec.Roles = []dbv1alpha1.RoleSpec{{Name: appRole}}
			before := cluster.DeepCopy()
			limit := int32(5)
			cluster.Spec.Roles[0].Database = "first"
			cluster.Spec.Roles[0].ConnectionLimit = &limit
			warnings, err := validator.ValidateUpdate(ctx, before, cluster)
			Expect(err).NotTo(HaveOccurred())
			Expect(warnings).To(BeEmpty())
		})
	})
})
