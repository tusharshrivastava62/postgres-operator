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
	"os"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"sigs.k8s.io/yaml"

	dbv1alpha1 "github.com/tusharshrivastava62/postgres-operator/api/v1alpha1"
)

// The samples are what people copy first, so each has to be something the
// webhook admits, and each has to be in the kustomization that applies them.
var _ = Describe("The sample manifests", func() {
	const (
		dir            = "../../../config/samples"
		securitySample = "db_v1alpha1_postgrescluster_security.yaml"
	)

	samples := func() []string {
		matches, err := filepath.Glob(filepath.Join(dir, "db_v1alpha1_postgrescluster*.yaml"))
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		ExpectWithOffset(1, matches).NotTo(BeEmpty())
		return matches
	}

	It("are all admitted by the webhook, with no warnings that would be a surprise", func() {
		validator := PostgresClusterCustomValidator{}
		for _, path := range samples() {
			raw, err := os.ReadFile(path) //nolint:gosec // the repository's own samples
			Expect(err).NotTo(HaveOccurred(), path)
			cluster := &dbv1alpha1.PostgresCluster{}
			Expect(yaml.UnmarshalStrict(raw, cluster)).To(Succeed(), path)
			cluster.Namespace = defaultNamespace

			warnings, err := validator.ValidateCreate(ctx, cluster)
			Expect(err).NotTo(HaveOccurred(), "%s: %v", path, err)
			if filepath.Base(path) == securitySample {
				// It turns on the options that replace pods, and the webhook says so.
				for _, warning := range warnings {
					Expect(warning).To(HavePrefix("spec.security."), "%s: only the security options may warn", path)
				}
				continue
			}
			Expect(warnings).To(BeEmpty(), "%s: a sample should not warn", path)
		}
	})

	It("include a request for emergency access that the cluster sample would grant", func() {
		raw, err := os.ReadFile(filepath.Join(dir, "db_v1alpha1_postgresemergencyaccess.yaml")) //nolint:gosec // the repository's own sample
		Expect(err).NotTo(HaveOccurred())
		request := &dbv1alpha1.PostgresEmergencyAccess{}
		Expect(yaml.UnmarshalStrict(raw, request)).To(Succeed())

		clusterRaw, err := os.ReadFile(filepath.Join(dir, "db_v1alpha1_postgrescluster_compliance.yaml")) //nolint:gosec // the repository's own sample
		Expect(err).NotTo(HaveOccurred())
		cluster := &dbv1alpha1.PostgresCluster{}
		Expect(yaml.UnmarshalStrict(clusterRaw, cluster)).To(Succeed())

		Expect(request.Spec.ClusterRef.Name).To(Equal(cluster.Name), "the request names a cluster in the samples")
		Expect(cluster.Spec.EmergencyAccess.Allowed()).To(BeTrue(), "and that cluster allows it")
		Expect(request.Spec.Validate(cluster.Spec.EmergencyAccess.MaxAllowed())).To(BeEmpty())
		Expect(string(raw)).To(ContainSubstring("clear text"), "and the sample carries the warning")
	})

	It("have names of their own, so applying them all together does not collide", func() {
		seen := map[string]string{}
		for _, path := range samples() {
			raw, err := os.ReadFile(path) //nolint:gosec // the repository's own samples
			Expect(err).NotTo(HaveOccurred())
			cluster := &dbv1alpha1.PostgresCluster{}
			Expect(yaml.Unmarshal(raw, cluster)).To(Succeed())
			Expect(seen).NotTo(HaveKey(cluster.Name), "%s and %s share a name", path, seen[cluster.Name])
			seen[cluster.Name] = path
		}
	})

	It("are all listed in the kustomization that applies them", func() {
		raw, err := os.ReadFile(filepath.Join(dir, "kustomization.yaml")) //nolint:gosec // the repository's own file
		Expect(err).NotTo(HaveOccurred())
		for _, path := range samples() {
			Expect(string(raw)).To(ContainSubstring(strings.TrimPrefix(path, dir+"/")), path)
		}
	})
})
