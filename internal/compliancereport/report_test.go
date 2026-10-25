/*
Copyright 2026.

Licensed under the Business Source License 1.1 (the "License"). You may not
use this file except in compliance with the License, a copy of which is in the
LICENSE file at the root of this repository. Production use requires a
commercial license from the Licensor. On the Change Date (2030-10-17) the
work becomes available under the Change License (Apache-2.0).
*/

package compliancereport

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	dbv1alpha1 "github.com/tusharshrivastava62/postgres-operator/api/v1alpha1"
)

const testNamespace = "shop"

var checkedAt = time.Date(2026, 10, 25, 3, 40, 0, 0, time.UTC)

func mt(t time.Time) *metav1.Time { v := metav1.NewTime(t); return &v }

func clusterWith(controls ...dbv1alpha1.ComplianceControl) dbv1alpha1.PostgresCluster {
	return dbv1alpha1.PostgresCluster{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: "orders"},
		Spec:       dbv1alpha1.PostgresClusterSpec{Compliance: &dbv1alpha1.ComplianceSpec{Standard: dbv1alpha1.StandardHIPAA}},
		Status: dbv1alpha1.PostgresClusterStatus{Compliance: dbv1alpha1.ComplianceStatus{
			Standard: dbv1alpha1.StandardHIPAA, LastChecked: mt(checkedAt), Controls: controls,
		}},
	}
}

func control(id, status string) dbv1alpha1.ComplianceControl {
	return dbv1alpha1.ComplianceControl{ID: id, Status: status, Evidence: "evidence for " + id, ObservedAt: mt(checkedAt)}
}

func mustBuild(t *testing.T, in Input, at time.Time) Report {
	t.Helper()
	r, err := Build(in, at)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestAReportIsOutOfDateOnlyAfterThirtyFiveMinutes(t *testing.T) {
	in := Input{Cluster: clusterWith(
		control(dbv1alpha1.ControlAuditLogging, dbv1alpha1.ControlMet),
		control(dbv1alpha1.ControlAuthentication, dbv1alpha1.ControlNotMet),
		control(dbv1alpha1.ControlDataIntegrity, dbv1alpha1.ControlUnknown),
	)}
	cases := []struct {
		name      string
		after     time.Duration
		freshness string
		summary   Summary
	}{
		{"just checked", 0, Current, Summary{Met: 1, NotMet: 1, Unknown: 1}},
		{"34 minutes", 34 * time.Minute, Current, Summary{Met: 1, NotMet: 1, Unknown: 1}},
		{"exactly 35 minutes", 35 * time.Minute, Current, Summary{Met: 1, NotMet: 1, Unknown: 1}},
		{"36 minutes", 36 * time.Minute, OutOfDate, Summary{OutOfDate: 3}},
		{"a day", 24 * time.Hour, OutOfDate, Summary{OutOfDate: 3}},
		{"read before it was checked", -time.Hour, Current, Summary{Met: 1, NotMet: 1, Unknown: 1}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := mustBuild(t, in, checkedAt.Add(c.after))
			if r.Freshness != c.freshness {
				t.Errorf("freshness %q, want %q", r.Freshness, c.freshness)
			}
			if r.Summary != c.summary {
				t.Errorf("summary %+v, want %+v", r.Summary, c.summary)
			}
			if r.AgeSeconds == nil || *r.AgeSeconds < 0 {
				t.Errorf("age %v, want it set and not negative", r.AgeSeconds)
			}
		})
	}
}

func TestAnOutOfDateResultKeepsWhatWasRecorded(t *testing.T) {
	in := Input{Cluster: clusterWith(
		control(dbv1alpha1.ControlAuditLogging, dbv1alpha1.ControlMet),
		control(dbv1alpha1.ControlAuthentication, dbv1alpha1.ControlNotMet),
	)}
	r := mustBuild(t, in, checkedAt.Add(time.Hour))
	for _, c := range r.Controls {
		if c.Status != StatusOutOfDate {
			t.Errorf("%s shows %q, want it out of date, not the stored result", c.ID, c.Status)
		}
	}
	if r.Controls[0].Recorded != dbv1alpha1.ControlMet || r.Controls[1].Recorded != dbv1alpha1.ControlNotMet {
		t.Errorf("recorded results lost: %+v", r.Controls)
	}
}

func TestAClusterNotYetCheckedIsNeitherCurrentNorOutOfDate(t *testing.T) {
	c := clusterWith(control(dbv1alpha1.ControlAuditLogging, dbv1alpha1.ControlUnknown))
	c.Status.Compliance.LastChecked = nil
	r := mustBuild(t, Input{Cluster: c}, checkedAt.Add(100*time.Hour))
	if r.Freshness != NeverChecked || r.CheckedAt != nil || r.AgeSeconds != nil {
		t.Errorf("got freshness %q, checked %v, age %v", r.Freshness, r.CheckedAt, r.AgeSeconds)
	}
	if r.Summary != (Summary{Unknown: 1}) {
		t.Errorf("summary %+v", r.Summary)
	}
}

func TestAClusterWithNoPresetHasNoReport(t *testing.T) {
	c := clusterWith()
	c.Spec.Compliance = nil
	c.Status.Compliance = dbv1alpha1.ComplianceStatus{}
	if _, err := Build(Input{Cluster: c}, checkedAt); !errors.Is(err, ErrNoPreset) {
		t.Fatalf("got %v, want ErrNoPreset", err)
	}
}

func TestControlsAreShownInTheDocumentedOrder(t *testing.T) {
	in := Input{Cluster: clusterWith(
		control("zz-future-control", dbv1alpha1.ControlMet),
		control(dbv1alpha1.ControlAvailability, dbv1alpha1.ControlMet),
		control(dbv1alpha1.ControlAuditLogging, dbv1alpha1.ControlMet),
		control("aa-future-control", dbv1alpha1.ControlMet),
		control(dbv1alpha1.ControlAuthentication, dbv1alpha1.ControlMet),
	)}
	built := mustBuild(t, in, checkedAt).Controls
	got := make([]string, 0, len(built))
	for _, c := range built {
		got = append(got, c.ID)
	}
	want := []string{
		dbv1alpha1.ControlAuditLogging, dbv1alpha1.ControlAuthentication, dbv1alpha1.ControlAvailability,
		"zz-future-control", "aa-future-control",
	}
	if !slices.Equal(got, want) {
		t.Errorf("order %v, want %v", got, want)
	}
}

// Every control a preset asks for has a place in the order, so none is shown as
// an unknown one after the others.
func TestEveryPresetControlHasAPlace(t *testing.T) {
	for _, standard := range dbv1alpha1.Standards() {
		preset, ok := dbv1alpha1.PresetFor(standard)
		if !ok {
			t.Fatalf("no preset for %s", standard)
		}
		for _, id := range preset.Controls {
			if !slices.Contains(controlOrder, id) {
				t.Errorf("%s asks for %s, which the report has no place for", standard, id)
			}
		}
	}
}

func request(name, cluster, state string, started time.Time) dbv1alpha1.PostgresEmergencyAccess {
	return dbv1alpha1.PostgresEmergencyAccess{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: name, CreationTimestamp: metav1.NewTime(started)},
		Spec:       dbv1alpha1.PostgresEmergencyAccessSpec{ClusterRef: dbv1alpha1.ClusterReference{Name: cluster}, Reason: "reason of " + name},
		Status: dbv1alpha1.PostgresEmergencyAccessStatus{
			State: state, RequestedBy: "alice", Role: "bg_" + name, Message: "message of " + name, StartedAt: mt(started),
		},
	}
}

func TestEmergencyAccessListsOnlyThisClustersRequestsNewestFirst(t *testing.T) {
	other := request("elsewhere", "orders", dbv1alpha1.EmergencyActive, checkedAt)
	other.Namespace = "other"
	in := Input{
		Cluster: clusterWith(),
		Requests: []dbv1alpha1.PostgresEmergencyAccess{
			request("old", "orders", dbv1alpha1.EmergencyExpired, checkedAt.Add(-time.Hour)),
			request("not-ours", "billing", dbv1alpha1.EmergencyActive, checkedAt),
			other,
			request("new", "orders", dbv1alpha1.EmergencyActive, checkedAt),
			request("refused", "orders", dbv1alpha1.EmergencyDenied, checkedAt.Add(-2*time.Hour)),
		},
	}
	e := mustBuild(t, in, checkedAt).EmergencyAccess
	names := make([]string, 0, len(e.Requests))
	for _, q := range e.Requests {
		names = append(names, q.Name)
	}
	if want := []string{"new", "old", "refused"}; !slices.Equal(names, want) {
		t.Fatalf("requests %v, want %v", names, want)
	}
	wantCounts := []StateCount{{dbv1alpha1.EmergencyActive, 1}, {dbv1alpha1.EmergencyExpired, 1}, {dbv1alpha1.EmergencyDenied, 1}}
	if !slices.Equal(e.Counts, wantCounts) {
		t.Errorf("counts %v, want %v", e.Counts, wantCounts)
	}
	for _, q := range e.Requests {
		hasMessage := q.Message != ""
		if wantMessage := q.State == dbv1alpha1.EmergencyDenied; hasMessage != wantMessage {
			t.Errorf("%s (%s): message %q", q.Name, q.State, q.Message)
		}
	}
}

func TestTheEmergencyWarningIsThereWheneverItMatters(t *testing.T) {
	enabled := clusterWith()
	enabled.Spec.EmergencyAccess = &dbv1alpha1.EmergencyAccessSpec{Enabled: true}
	off := clusterWith()
	off.Spec.EmergencyAccess = &dbv1alpha1.EmergencyAccessSpec{Enabled: false}
	old := request("old", "orders", dbv1alpha1.EmergencyExpired, checkedAt)

	cases := []struct {
		name string
		in   Input
		want bool
	}{
		{"never configured", Input{Cluster: clusterWith()}, false},
		{"switched off, no requests", Input{Cluster: off}, false},
		{"enabled, no requests", Input{Cluster: enabled}, true},
		{"switched off but a request remains", Input{Cluster: off, Requests: []dbv1alpha1.PostgresEmergencyAccess{old}}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := mustBuild(t, c.in, checkedAt)
			if got := r.EmergencyAccess.Warning != nil; got != c.want {
				t.Fatalf("warning present %v, want %v", got, c.want)
			}
			// When it is there it is also in the opening wording, so nobody has to
			// reach the last section to learn it.
			inOpening := slices.ContainsFunc(r.ReadThisFirst, func(s string) bool { return strings.Contains(s, EmergencyNotInPgaudit) })
			if inOpening != c.want {
				t.Errorf("emergency sentence in the opening wording: %v, want %v", inOpening, c.want)
			}
		})
	}
}
