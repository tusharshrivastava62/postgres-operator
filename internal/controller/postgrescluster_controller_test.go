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
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus/testutil"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	dbv1alpha1 "github.com/tusharshrivastava62/postgres-operator/api/v1alpha1"
)

var _ = Describe("PostgresCluster Controller", func() {
	Context("When reconciling a resource", func() {
		const (
			resourceName      = "test-resource"
			resourceNamespace = "default"
		)

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: resourceNamespace,
		}
		postgrescluster := &dbv1alpha1.PostgresCluster{}

		BeforeEach(func() {
			By("creating the custom resource for the Kind PostgresCluster")
			err := k8sClient.Get(ctx, typeNamespacedName, postgrescluster)
			if err != nil && errors.IsNotFound(err) {
				cluster := &dbv1alpha1.PostgresCluster{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: resourceNamespace,
					},
					Spec: dbv1alpha1.PostgresClusterSpec{
						Version: "16",
						Storage: dbv1alpha1.StorageSpec{
							Size: resource.MustParse("1Gi"),
						},
						Backup: dbv1alpha1.BackupSpec{
							Enabled:  true,
							Schedule: "0 2 * * *",
						},
					},
				}
				Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
			}
		})

		AfterEach(func() {
			cluster := &dbv1alpha1.PostgresCluster{}
			err := k8sClient.Get(ctx, typeNamespacedName, cluster)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance PostgresCluster")
			Expect(k8sClient.Delete(ctx, cluster)).To(Succeed())
		})
		It("should successfully reconcile the resource", func() {
			By("Reconciling the created resource")
			controllerReconciler := &PostgresClusterReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("creating the credentials Secret")
			secret := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: resourceName + "-credentials", Namespace: resourceNamespace}, secret)).To(Succeed())
			Expect(secret.Data).To(HaveKey("password"))

			By("creating the headless and client Services")
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: resourceName + "-headless", Namespace: resourceNamespace}, &corev1.Service{})).To(Succeed())
			Expect(k8sClient.Get(ctx, typeNamespacedName, &corev1.Service{})).To(Succeed())

			By("creating the StatefulSet")
			sts := &appsv1.StatefulSet{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, sts)).To(Succeed())
			Expect(*sts.Spec.Replicas).To(Equal(int32(1)))

			By("creating the backup CronJob and its PVC")
			backupNSName := types.NamespacedName{Name: resourceName + "-backup", Namespace: resourceNamespace}
			cj := &batchv1.CronJob{}
			Expect(k8sClient.Get(ctx, backupNSName, cj)).To(Succeed())
			Expect(cj.Spec.Schedule).To(Equal("0 2 * * *"))
			Expect(k8sClient.Get(ctx, backupNSName, &corev1.PersistentVolumeClaim{})).To(Succeed())

			By("reporting status conditions - not yet available, no backup finished yet")
			Expect(k8sClient.Get(ctx, typeNamespacedName, postgrescluster)).To(Succeed())
			Expect(postgrescluster.Status.ObservedGeneration).To(Equal(postgrescluster.Generation))
			available := meta.FindStatusCondition(postgrescluster.Status.Conditions, "Available")
			Expect(available).NotTo(BeNil())
			Expect(available.Status).To(Equal(metav1.ConditionFalse))
			degraded := meta.FindStatusCondition(postgrescluster.Status.Conditions, "Degraded")
			Expect(degraded).NotTo(BeNil())
			Expect(degraded.Reason).To(Equal("BackupPending"))

			By("recording reconcile metrics")
			Expect(testutil.ToFloat64(reconcileTotal.WithLabelValues(resourceNamespace, resourceName, "success"))).To(BeNumerically(">=", 1))
			Expect(testutil.ToFloat64(instancesReady.WithLabelValues(resourceNamespace, resourceName))).To(Equal(float64(0)))

			By("removing the CronJob when backup is disabled")
			Expect(k8sClient.Get(ctx, typeNamespacedName, postgrescluster)).To(Succeed())
			postgrescluster.Spec.Backup.Enabled = false
			Expect(k8sClient.Update(ctx, postgrescluster)).To(Succeed())

			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())
			err = k8sClient.Get(ctx, backupNSName, cj)
			Expect(errors.IsNotFound(err)).To(BeTrue())

			By("reflecting disabled backup in status")
			Expect(k8sClient.Get(ctx, typeNamespacedName, postgrescluster)).To(Succeed())
			degraded = meta.FindStatusCondition(postgrescluster.Status.Conditions, "Degraded")
			Expect(degraded).NotTo(BeNil())
			Expect(degraded.Reason).To(Equal("BackupDisabled"))
			Expect(degraded.Status).To(Equal(metav1.ConditionFalse))
		})
	})
})
