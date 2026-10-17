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

package controller

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	dbv1alpha1 "github.com/tusharshrivastava62/postgres-operator/api/v1alpha1"
)

// These tests exercise validation that lives in the CRD schema itself (the
// version enum and the version-immutability CEL rule), enforced by the
// envtest API server. No reconciler or webhook is involved, so a failure
// here means the CRD, not the controller, is wrong.
var _ = Describe("PostgresCluster CRD validation", func() {
	newCluster := func(name, version string) *dbv1alpha1.PostgresCluster {
		return &dbv1alpha1.PostgresCluster{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: dbv1alpha1.PostgresClusterSpec{
				Version: version,
				Storage: dbv1alpha1.StorageSpec{Size: resource.MustParse("1Gi")},
			},
		}
	}

	DescribeTable("spec.version enum",
		func(version string, accepted bool) {
			cluster := newCluster("crd-enum-"+version, version)
			err := k8sClient.Create(ctx, cluster)
			if accepted {
				Expect(err).NotTo(HaveOccurred())
				DeferCleanup(func() { Expect(k8sClient.Delete(ctx, cluster)).To(Succeed()) })
				return
			}
			Expect(err).To(HaveOccurred())
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected an Invalid error, got: %v", err)
			Expect(err.Error()).To(ContainSubstring("supported values"))
		},
		Entry("14 is no longer supported", "14", false),
		Entry("15 is no longer supported", "15", false),
		Entry("16 is supported", "16", true),
		Entry("17 is supported", "17", true),
		Entry("18 is supported", "18", true),
	)

	Context("spec.version immutability", func() {
		const name = "crd-version-immutable"
		key := types.NamespacedName{Name: name, Namespace: "default"}

		BeforeEach(func() {
			cluster := newCluster(name, "16")
			Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
			DeferCleanup(func() { Expect(k8sClient.Delete(ctx, cluster)).To(Succeed()) })
		})

		It("rejects changing the version after creation", func() {
			cluster := &dbv1alpha1.PostgresCluster{}
			Expect(k8sClient.Get(ctx, key, cluster)).To(Succeed())

			cluster.Spec.Version = "17"
			err := k8sClient.Update(ctx, cluster)

			Expect(err).To(HaveOccurred())
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected an Invalid error, got: %v", err)
			Expect(err.Error()).To(ContainSubstring("version is immutable"))
		})

		It("still allows updating other fields with the version unchanged", func() {
			cluster := &dbv1alpha1.PostgresCluster{}
			Expect(k8sClient.Get(ctx, key, cluster)).To(Succeed())

			cluster.Spec.Instances = 2
			Expect(k8sClient.Update(ctx, cluster)).To(Succeed())

			Expect(k8sClient.Get(ctx, key, cluster)).To(Succeed())
			Expect(cluster.Spec.Instances).To(Equal(int32(2)))
			Expect(cluster.Spec.Version).To(Equal("16"))
		})
	})
})
