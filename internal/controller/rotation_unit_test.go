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
	"encoding/json"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	dbv1alpha1 "github.com/tusharshrivastava62/postgres-operator/api/v1alpha1"
)

func secretWith(annotations map[string]string, created time.Time) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Annotations: annotations, CreationTimestamp: metav1.NewTime(created)}}
}

func every(d time.Duration) *metav1.Duration { return &metav1.Duration{Duration: d} }

func TestRotationDueWhenAskedForOnce(t *testing.T) {
	now := time.Now()
	created := now.Add(-time.Hour)
	tests := []struct {
		name        string
		annotations map[string]string
		want        bool
		why         string
	}{
		{"no request", nil, false, ""},
		{"a request", map[string]string{annotationRotate: "1"}, true, rotationRequested},
		{"a request already answered", map[string]string{annotationRotate: "1", annotationRotatedToken: "1"}, false, ""},
		{"a new request after one answered", map[string]string{annotationRotate: "2", annotationRotatedToken: "1"}, true, rotationRequested},
		{"an empty request", map[string]string{annotationRotate: ""}, false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, why := rotationDue(secretWith(tt.annotations, created), nil, now)
			if got != tt.want || why != tt.why {
				t.Errorf("rotationDue = %v, %q; want %v, %q", got, why, tt.want, tt.why)
			}
		})
	}
}

func TestRotationDueOnASchedule(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	tests := []struct {
		name        string
		rotatedAt   string
		created     time.Time
		every       *metav1.Duration
		want        bool
		description string
	}{
		{"no schedule, however old", "", now.Add(-400 * day), nil, false, ""},
		{"younger than the schedule", now.Add(-23 * time.Hour).Format(time.RFC3339), now.Add(-400 * day), every(day), false, ""},
		{"older than the schedule", now.Add(-25 * time.Hour).Format(time.RFC3339), now.Add(-400 * day), every(day), true, ""},
		{"exactly as old", now.Add(-day).Format(time.RFC3339), now.Add(-400 * day), every(day), true, ""},
		{"never rotated: the age of the Secret, young", "", now.Add(-time.Hour), every(day), false, ""},
		{"never rotated: the age of the Secret, old", "", now.Add(-2 * day), every(day), true, ""},
		{"a rotated-at nobody can read counts as never", "yesterday", now.Add(-2 * day), every(day), true, ""},
		{"a zero schedule is no schedule", "", now.Add(-400 * day), every(0), false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			annotations := map[string]string{}
			if tt.rotatedAt != "" {
				annotations[annotationRotatedAt] = tt.rotatedAt
			}
			got, why := rotationDue(secretWith(annotations, tt.created), tt.every, now)
			if got != tt.want {
				t.Errorf("rotationDue = %v, want %v", got, tt.want)
			}
			if got && why != rotationScheduled {
				t.Errorf("why = %q", why)
			}
		})
	}
}

func TestStampRotationRecordsWhenAndWhichRequestWasAnswered(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 30, 5, 0, time.UTC)
	secret := secretWith(map[string]string{annotationRotate: "please"}, now.Add(-time.Hour))
	stampRotation(secret, now)
	if got := secret.Annotations[annotationRotatedAt]; got != "2026-10-24T12:30:05Z" {
		t.Errorf("rotated-at = %q", got)
	}
	if got := secret.Annotations[annotationRotatedToken]; got != "please" {
		t.Errorf("rotated-token = %q", got)
	}
	if due, _ := rotationDue(secret, nil, now); due {
		t.Error("the request is still due after it was answered")
	}
	if !lastRotated(secret).Equal(now) {
		t.Errorf("lastRotated = %v", lastRotated(secret))
	}

	bare := &corev1.Secret{}
	stampRotation(bare, now)
	if bare.Annotations[annotationRotatedAt] == "" {
		t.Error("a Secret with no annotations was not stamped")
	}
	if _, has := bare.Annotations[annotationRotatedToken]; has {
		t.Error("a token was invented for a rotation nobody asked for")
	}
}

func TestCredentialsAreReplacedOnlyInAWholeUntouchedCluster(t *testing.T) {
	if ok, _ := credentialsReady(true, false); !ok {
		t.Error("a healthy cluster that is not being updated is not ready")
	}
	if ok, why := credentialsReady(true, true); ok || why == "" {
		t.Errorf("a cluster being updated is ready: %q", why)
	}
	if ok, why := credentialsReady(false, false); ok || why == "" {
		t.Errorf("an unhealthy cluster is ready: %q", why)
	}
}

func TestTheCredentialsVersionIsOnTheTemplateOnlyOnceThereIsOne(t *testing.T) {
	cluster := rolesCluster()
	cluster.Spec.Storage.Size = resourceQuantity("1Gi")
	plain, err := desiredStatefulSet(cluster, "img")
	if err != nil {
		t.Fatal(err)
	}
	if len(plain.Spec.Template.Annotations) != 0 {
		t.Errorf("a cluster whose passwords never were replaced has template annotations: %v", plain.Spec.Template.Annotations)
	}
	empty, _ := desiredStatefulSetAt(cluster, "img", "")
	if !equalJSON(t, plain.Spec.Template, empty.Spec.Template) {
		t.Error("no version is not the same template as before there was one: an upgrade would replace every pod")
	}
	one, _ := desiredStatefulSetAt(cluster, "img", "1")
	two, _ := desiredStatefulSetAt(cluster, "img", "2")
	if one.Spec.Template.Annotations[annotationCredentialsVersion] != "1" || two.Spec.Template.Annotations[annotationCredentialsVersion] != "2" {
		t.Errorf("versions: %v, %v", one.Spec.Template.Annotations, two.Spec.Template.Annotations)
	}
	if equalJSON(t, one.Spec.Template, two.Spec.Template) {
		t.Error("a new version leaves the template as it was, so no pod would be replaced")
	}
	if equalJSON(t, plain.Spec.Template, one.Spec.Template) {
		t.Error("the first version leaves the template as it was")
	}
}

func resourceQuantity(s string) resource.Quantity { return resource.MustParse(s) }

func equalJSON(t *testing.T, a, b any) bool {
	t.Helper()
	x, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	y, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return string(x) == string(y)
}

// observedJSON is what the role query would answer with for these roles.
func observedJSON(roles []observedRole) string {
	raw, err := json.Marshal(observedRoles{Roles: roles})
	if err != nil {
		panic(err)
	}
	return string(raw)
}

// desiredStatefulSet is the StatefulSet of a cluster whose passwords never
// have been replaced.
//
//nolint:unparam // the specs that use it all name the same image
func desiredStatefulSet(cluster *dbv1alpha1.PostgresCluster, image string) (*appsv1.StatefulSet, error) {
	return desiredStatefulSetAt(cluster, image, "")
}
