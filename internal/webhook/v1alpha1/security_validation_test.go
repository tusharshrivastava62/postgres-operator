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

	It("warns that asking for the Patroni API password replaces every pod, and what it then refuses", func() {
		asking := cluster.DeepCopy()
		asking.Spec.Security = &dbv1alpha1.SecuritySpec{PatroniAPI: &dbv1alpha1.PatroniAPISpec{Authentication: true}}
		warnings, err := validator.ValidateUpdate(ctx, cluster, asking)
		Expect(err).NotTo(HaveOccurred())
		joined := strings.Join(warnings, " ")
		Expect(joined).To(And(ContainSubstring("patroniAPI.authentication"), ContainSubstring("every database pod is replaced"),
			ContainSubstring("patroni-api-password"), ContainSubstring("crosses the pod network unencrypted")))

		By("and not the last, when the API is also over TLS")
		both := withTLS("orders-tls")
		both.Spec.Security.TLS.PatroniAPI = true
		both.Spec.Security.PatroniAPI = &dbv1alpha1.PatroniAPISpec{Authentication: true}
		warnings, err = validator.ValidateUpdate(ctx, withTLS("orders-tls"), both)
		Expect(err).NotTo(HaveOccurred())
		joined = strings.Join(warnings, " ")
		Expect(joined).To(ContainSubstring("patroniAPI.authentication"))
		Expect(joined).NotTo(ContainSubstring("crosses the pod network unencrypted"))

		By("and what taking it away does")
		warnings, err = validator.ValidateUpdate(ctx, asking, cluster)
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.Join(warnings, " ")).To(ContainSubstring("anyone who can reach port 8008"))
	})

	It("warns that Patroni's API over TLS stops serving plain HTTP, and what that costs the image", func() {
		on := withTLS("orders-tls")
		on.Spec.Security.TLS.PatroniAPI = true
		warnings, err := validator.ValidateUpdate(ctx, withTLS("orders-tls"), on)
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.Join(warnings, " ")).To(And(ContainSubstring("tls.patroniAPI"), ContainSubstring("TLS only"),
			ContainSubstring("does not run"), ContainSubstring("every database pod is replaced")))

		warnings, err = validator.ValidateUpdate(ctx, on, withTLS("orders-tls"))
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.Join(warnings, " ")).To(ContainSubstring("goes back to plain HTTP"))
	})

	It("warns what limiting the replication role refuses, and what it does about pg_rewind", func() {
		limited := cluster.DeepCopy()
		limited.Spec.Security = &dbv1alpha1.SecuritySpec{RestrictReplicationRole: true}
		warnings, err := validator.ValidateUpdate(ctx, cluster, limited)
		Expect(err).NotTo(HaveOccurred())
		joined := strings.Join(warnings, " ")
		Expect(joined).To(And(ContainSubstring("restrictReplicationRole"), ContainSubstring("every database pod is replaced"),
			ContainSubstring("everything but replication"), ContainSubstring("pgoperator_rewind"), ContainSubstring("rewind-password")))

		warnings, err = validator.ValidateCreate(ctx, limited)
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.Join(warnings, " ")).To(ContainSubstring("restrictReplicationRole"))

		By("and what taking it away does")
		warnings, err = validator.ValidateUpdate(ctx, limited, cluster)
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.Join(warnings, " ")).To(And(ContainSubstring("log in like any other user again"), ContainSubstring("disabled, not dropped")))
	})

	It("warns that limiting ingress cuts off a pod without the client label, and that no pod is replaced", func() {
		limited := cluster.DeepCopy()
		limited.Spec.Security = &dbv1alpha1.SecuritySpec{NetworkPolicy: &dbv1alpha1.NetworkPolicySpec{
			Ingress: &dbv1alpha1.IngressPolicySpec{Mode: dbv1alpha1.NetworkPolicyRestricted}}}
		warnings, err := validator.ValidateUpdate(ctx, cluster, limited)
		Expect(err).NotTo(HaveOccurred())
		joined := strings.Join(warnings, " ")
		Expect(joined).To(And(ContainSubstring("networkPolicy.ingress"), ContainSubstring("db.youroperator.io/client=true"),
			ContainSubstring("loses its connection"), ContainSubstring("No pod is replaced"), ContainSubstring("enforces NetworkPolicy")))
		Expect(joined).NotTo(ContainSubstring("every database pod is replaced"))

		warnings, err = validator.ValidateUpdate(ctx, limited, cluster)
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.Join(warnings, " ")).To(ContainSubstring("any pod can connect to the database again"))
	})

	It("warns what limiting egress leaves the pods able to reach, and that a backup destination must be listed", func() {
		limited := cluster.DeepCopy()
		limited.Spec.Security = &dbv1alpha1.SecuritySpec{NetworkPolicy: &dbv1alpha1.NetworkPolicySpec{
			Egress: &dbv1alpha1.EgressPolicySpec{Mode: dbv1alpha1.NetworkPolicyRestricted}}}
		warnings, err := validator.ValidateCreate(ctx, limited)
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.Join(warnings, " ")).To(And(ContainSubstring("networkPolicy.egress"), ContainSubstring("only to DNS"),
			ContainSubstring("API server"), ContainSubstring("backup destination"), ContainSubstring("No pod is replaced")))

		warnings, err = validator.ValidateUpdate(ctx, limited, cluster)
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.Join(warnings, " ")).To(ContainSubstring("connect anywhere again"))
	})

	It("refuses limiting egress for a cluster with backups when nothing is listed, naming the field", func() {
		limited := cluster.DeepCopy()
		limited.Spec.Backup = dbv1alpha1.BackupSpec{Enabled: true, Destination: &dbv1alpha1.BackupDestination{
			Bucket: "b", CredentialsSecret: "s"}}
		limited.Spec.Security = &dbv1alpha1.SecuritySpec{NetworkPolicy: &dbv1alpha1.NetworkPolicySpec{
			Egress: &dbv1alpha1.EgressPolicySpec{Mode: dbv1alpha1.NetworkPolicyRestricted}}}
		_, err := validator.ValidateCreate(ctx, limited)
		Expect(err).To(MatchError(ContainSubstring("spec.security.networkPolicy.egress.allow")))
		_, err = validator.ValidateUpdate(ctx, cluster, limited)
		Expect(err).To(MatchError(ContainSubstring("backups go to an object store")))
	})

	It("does not warn about a change that leaves the certificate as it was", func() {
		changed := withTLS("orders-tls")
		changed.Spec.Instances = 3
		warnings, err := validator.ValidateUpdate(ctx, withTLS("orders-tls"), changed)
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.Join(warnings, " ")).NotTo(ContainSubstring("spec.security"))
	})
})
