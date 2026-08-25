/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/resource"

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

		It("admits create when backup is enabled with a schedule", func() {
			obj.Spec.Backup.Enabled = true
			obj.Spec.Backup.Schedule = "0 2 * * *"
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
})
