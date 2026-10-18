/*
Copyright 2026.

Licensed under the Business Source License 1.1 (the "License"). You may not
use this file except in compliance with the License, a copy of which is in the
LICENSE file at the root of this repository. Production use requires a
commercial license from the Licensor. On the Change Date (2030-10-17) the
work becomes available under the Change License (Apache-2.0).
*/

package v1alpha1

import "strings"

// PathFor returns the location inside the bucket where a cluster's backups
// are kept: the destination's path when it sets one, otherwise
// <namespace>/<name>. The default is derived from where the cluster runs, so
// two clusters never share a location unless someone sets the same path on
// both.
func (d *BackupDestination) PathFor(namespace, name string) string {
	if d != nil && d.Path != "" {
		return strings.Trim(d.Path, "/")
	}
	return namespace + "/" + name
}

// SameStore reports whether two destinations name the same bucket on the same
// store, whatever paths they use inside it.
func (d *BackupDestination) SameStore(other *BackupDestination) bool {
	if d == nil || other == nil {
		return false
	}
	return d.Bucket == other.Bucket && d.Endpoint == other.Endpoint && d.Region == other.Region
}
