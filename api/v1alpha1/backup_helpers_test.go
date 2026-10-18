/*
Copyright 2026.

Licensed under the Business Source License 1.1 (the "License"). You may not
use this file except in compliance with the License, a copy of which is in the
LICENSE file at the root of this repository. Production use requires a
commercial license from the Licensor. On the Change Date (2030-10-17) the
work becomes available under the Change License (Apache-2.0).
*/

package v1alpha1

import "testing"

const (
	testPath  = "team/orders-db"
	testStore = "http://s3:9000"
)

func TestPathFor(t *testing.T) {
	tests := []struct {
		name string
		dest *BackupDestination
		want string
	}{
		{"nil destination uses the default", nil, "prod/orders"},
		{"empty path uses the default", &BackupDestination{}, "prod/orders"},
		{"explicit path wins", &BackupDestination{Path: testPath}, testPath},
		{"stray slashes are trimmed", &BackupDestination{Path: "/team/orders-db/"}, testPath},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.dest.PathFor("prod", "orders"); got != tt.want {
				t.Errorf("PathFor() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSameStore(t *testing.T) {
	a := &BackupDestination{Bucket: "b", Endpoint: testStore, Region: "r", Path: "x"}
	tests := []struct {
		name  string
		other *BackupDestination
		want  bool
	}{
		{"same store, different path", &BackupDestination{Bucket: "b", Endpoint: testStore, Region: "r", Path: "y"}, true},
		{"different bucket", &BackupDestination{Bucket: "c", Endpoint: testStore, Region: "r"}, false},
		{"different endpoint", &BackupDestination{Bucket: "b", Endpoint: "http://other:9000", Region: "r"}, false},
		{"nil", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := a.SameStore(tt.other); got != tt.want {
				t.Errorf("SameStore() = %v, want %v", got, tt.want)
			}
		})
	}
	var none *BackupDestination
	if none.SameStore(a) {
		t.Error("a nil destination is the same store as nothing")
	}
}
