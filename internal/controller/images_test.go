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
	"os"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// The default images are named after the release pinned in
// images/spilo/upstream.env. Keeping the two in step is a manual change, so
// this fails if one is edited without the other.
var _ = Describe("Default images", func() {
	pinned := func() map[string]string {
		raw, err := os.ReadFile("../../images/spilo/upstream.env")
		Expect(err).NotTo(HaveOccurred())
		values := map[string]string{}
		for line := range strings.SplitSeq(string(raw), "\n") {
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			key, value, found := strings.Cut(line, "=")
			Expect(found).To(BeTrue(), "line %q", line)
			values[key] = value
		}
		return values
	}

	It("covers every supported PostgreSQL version", func() {
		Expect(defaultImages).To(HaveLen(3))
		for _, version := range []string{"16", "17", "18"} {
			Expect(defaultImages).To(HaveKey(version))
		}
	})

	It("uses the release and revision pinned for the image build", func() {
		values := pinned()
		tag := values["SPILO_REF"] + "-r" + values["IMAGE_REVISION"]
		for version, image := range defaultImages {
			Expect(image).To(Equal("ghcr.io/tusharshrivastava62/spilo-"+version+":"+tag), "version %s", version)
		}
	})

	It("pins the upstream release to a full commit", func() {
		Expect(pinned()["SPILO_COMMIT"]).To(MatchRegexp("^[0-9a-f]{40}$"))
	})
})
