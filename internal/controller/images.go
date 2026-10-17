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
	dbv1alpha1 "github.com/tusharshrivastava62/postgres-operator/api/v1alpha1"
)

// defaultImages maps spec.version to the image used when spec.image is
// unset. These are Spilo (Patroni plus PostgreSQL) images built from
// upstream by this project's image workflow; upstream's own images are
// published rarely and lag behind PostgreSQL minor releases.
//
// Bumping a tag here never changes an existing cluster: the image a cluster
// runs is recorded in status.image the first time it is reconciled and only
// follows spec.image after that. See resolveImage.
var defaultImages = map[string]string{
	"16": "ghcr.io/tusharshrivastava62/spilo-16:4.0-p3",
	"17": "ghcr.io/tusharshrivastava62/spilo-17:4.0-p3",
	"18": "ghcr.io/tusharshrivastava62/spilo-18:4.0-p3",
}

// resolveImage decides which image a cluster should run.
//
//   - spec.image, when set, always wins, so changing it is the one deliberate
//     way to move a cluster to a different image.
//   - Otherwise a cluster keeps whatever status.image already records, so an
//     operator upgrade that changes defaultImages cannot roll running
//     databases onto a new image.
//   - Only a cluster that has never been reconciled takes the built-in
//     default for its version.
//
// The second return value is false when the version has no default image and
// no image is recorded or given, which the webhook and CRD enum normally make
// impossible.
func resolveImage(cluster *dbv1alpha1.PostgresCluster) (string, bool) {
	if cluster.Spec.Image != "" {
		return cluster.Spec.Image, true
	}
	if cluster.Status.Image != "" {
		return cluster.Status.Image, true
	}
	image, ok := defaultImages[cluster.Spec.Version]
	return image, ok
}
