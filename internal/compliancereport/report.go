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
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	dbv1alpha1 "github.com/tusharshrivastava62/postgres-operator/api/v1alpha1"
)

// SchemaVersion is the version of the JSON the report can be written as.
const SchemaVersion = 1

// ErrNoPreset is returned for a cluster that has no compliance preset: there is
// nothing for the operator to have checked.
var ErrNoPreset = errors.New("the cluster has no compliance preset (spec.compliance.standard is not set)")

// How current a report is.
const (
	// Current means the jobs reported within dbv1alpha1.ComplianceStaleAfter.
	Current = "current"
	// OutOfDate means the newest report is older than that.
	OutOfDate = "out-of-date"
	// NeverChecked means the jobs have not reported yet.
	NeverChecked = "never-checked"
)

// StatusOutOfDate is what a control shows when the report it comes from is too
// old to believe. The stored result is kept next to it.
const StatusOutOfDate = "OutOfDate"

// Input is everything the report is built from. Nothing else is read.
type Input struct {
	Cluster  dbv1alpha1.PostgresCluster
	Requests []dbv1alpha1.PostgresEmergencyAccess
	// Source says where the input came from, in words, for the header.
	Source string
}

// Report is the result, ready to be written in any format.
type Report struct {
	SchemaVersion int `json:"schemaVersion"`

	Cluster  ClusterName `json:"cluster"`
	Standard string      `json:"standard"`
	Source   string      `json:"source"`

	// ReadAt is the moment the age of the report is measured at.
	ReadAt time.Time `json:"readAt"`
	// CheckedAt is when the operator's jobs last reported. It is absent before
	// the first report.
	CheckedAt  *time.Time `json:"checkedAt,omitempty"`
	AgeSeconds *int64     `json:"ageSeconds,omitempty"`
	Freshness  string     `json:"freshness"`
	// StaleAfterSeconds is how old a report may be before it is out of date.
	StaleAfterSeconds int64 `json:"staleAfterSeconds"`

	// ReadThisFirst is the wording the report opens with, a paragraph each. It
	// cannot be left out.
	ReadThisFirst []string `json:"readThisFirst"`

	Summary  Summary   `json:"summary"`
	Controls []Control `json:"controls"`

	NotChecked NotCheckedSection `json:"notChecked"`

	EmergencyAccess EmergencyAccess `json:"emergencyAccess"`
}

// ClusterName names the cluster the report is about.
type ClusterName struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// Summary counts the controls by what they show.
type Summary struct {
	Met       int `json:"met"`
	NotMet    int `json:"notMet"`
	Unknown   int `json:"unknown"`
	OutOfDate int `json:"outOfDate"`
}

// Control is one control as the report shows it.
type Control struct {
	ID string `json:"id"`
	// Status is what the report shows: Met, NotMet, Unknown, or OutOfDate when
	// the report is too old to believe.
	Status string `json:"status"`
	// Recorded is the result the operator wrote down, whatever its age.
	Recorded   string     `json:"recorded"`
	Evidence   string     `json:"evidence,omitempty"`
	ObservedAt *time.Time `json:"observedAt,omitempty"`
}

// NotCheckedSection lists what the operator does not check.
type NotCheckedSection struct {
	Intro   string   `json:"intro"`
	Items   []string `json:"items"`
	Pointer string   `json:"pointer"`
}

// EmergencyAccess is the record of emergency access requests.
type EmergencyAccess struct {
	// Enabled is whether the cluster allows requests now.
	Enabled bool `json:"enabled"`
	// Warning is present whenever the cluster allows emergency access or any
	// request exists, and cannot be left out.
	Warning *EmergencyWarning `json:"warning,omitempty"`
	// Counts are the number of requests in each state, for the states present.
	Counts []StateCount `json:"counts,omitempty"`
	// Requests are the requests that exist now, newest first.
	Requests []Request `json:"requests"`
}

// EmergencyWarning is the wording about what emergency access leaves out of the
// audit record.
type EmergencyWarning struct {
	Title      string   `json:"title"`
	Statements []string `json:"statements"`
	// Record says what the list of requests is, and what it is not.
	Record string `json:"record"`
}

// StateCount is how many requests are in one state.
type StateCount struct {
	State string `json:"state"`
	Count int    `json:"count"`
}

// Request is one emergency access request.
type Request struct {
	Name        string     `json:"name"`
	State       string     `json:"state"`
	RequestedBy string     `json:"requestedBy,omitempty"`
	Reason      string     `json:"reason"`
	Role        string     `json:"role,omitempty"`
	StartedAt   *time.Time `json:"startedAt,omitempty"`
	ExpiresAt   *time.Time `json:"expiresAt,omitempty"`
	EndedAt     *time.Time `json:"endedAt,omitempty"`
	// Message is set only for requests that Failed or were Denied.
	Message string `json:"message,omitempty"`
}

// emergencyRecord says what the list of requests covers.
const emergencyRecord = "These are the requests that exist now. A request that was deleted leaves nothing here. " +
	"The server log, and the Kubernetes API server's audit log if you keep one, hold the rest of the record."

// controlOrder is the order the controls are shown in, the order of the table in
// docs/compliance.md. A control this version does not know follows, in the order
// the operator wrote it.
var controlOrder = []string{
	dbv1alpha1.ControlAuditLogging, dbv1alpha1.ControlConnectionLogging, dbv1alpha1.ControlEncryptionInTransit,
	dbv1alpha1.ControlAuthentication, dbv1alpha1.ControlDataIntegrity, dbv1alpha1.ControlAuditLogDelivery,
	dbv1alpha1.ControlAuditLogRetention, dbv1alpha1.ControlNetworkIsolation, dbv1alpha1.ControlBackupRecovery,
	dbv1alpha1.ControlAvailability,
}

// stateOrder is the order the states are counted in.
var stateOrder = []string{
	dbv1alpha1.EmergencyActive, dbv1alpha1.EmergencyPending, dbv1alpha1.EmergencyExpired,
	dbv1alpha1.EmergencyRevoked, dbv1alpha1.EmergencyDenied, dbv1alpha1.EmergencyFailed,
}

// Build makes the report from what the operator wrote, with the age of the
// report measured at the given moment.
func Build(in Input, at time.Time) (Report, error) {
	cluster := &in.Cluster
	standard := cluster.Status.Compliance.Standard
	if standard == "" && cluster.Spec.Compliance != nil {
		standard = cluster.Spec.Compliance.Standard
	}
	if standard == "" {
		return Report{}, ErrNoPreset
	}

	at = at.UTC().Truncate(time.Second)
	r := Report{
		SchemaVersion:     SchemaVersion,
		Cluster:           ClusterName{Namespace: cluster.Namespace, Name: cluster.Name},
		Standard:          standard,
		Source:            in.Source,
		ReadAt:            at,
		StaleAfterSeconds: int64(dbv1alpha1.ComplianceStaleAfter / time.Second),
		ReadThisFirst: []string{
			PresetIsNotCompliance, MappingUnreviewed, ControlsMetMeaning + " " + NotAProofForAuditors,
		},
		NotChecked: NotCheckedSection{Intro: NotExhaustive, Items: NotChecked, Pointer: DocsPointer},
	}

	r.Freshness = NeverChecked
	if checked := cluster.Status.Compliance.LastChecked; checked != nil {
		t := checked.UTC()
		age := max(at.Sub(t), 0)
		secs := int64(age / time.Second)
		r.CheckedAt, r.AgeSeconds = &t, &secs
		r.Freshness = Current
		if age > dbv1alpha1.ComplianceStaleAfter {
			r.Freshness = OutOfDate
		}
	}

	r.Controls = controls(cluster.Status.Compliance.Controls, r.Freshness)
	for _, c := range r.Controls {
		switch c.Status {
		case dbv1alpha1.ControlMet:
			r.Summary.Met++
		case dbv1alpha1.ControlNotMet:
			r.Summary.NotMet++
		case StatusOutOfDate:
			r.Summary.OutOfDate++
		default:
			r.Summary.Unknown++
		}
	}

	r.EmergencyAccess = emergencyAccess(cluster, in.Requests)
	if r.EmergencyAccess.Warning != nil {
		r.ReadThisFirst = append(r.ReadThisFirst, EmergencyNotInPgaudit+" Read the emergency access section before anyone uses it.")
	}
	return r, nil
}

func controls(stored []dbv1alpha1.ComplianceControl, freshness string) []Control {
	out := make([]Control, 0, len(stored))
	for _, c := range stored {
		shown := c.Status
		if freshness == OutOfDate {
			shown = StatusOutOfDate
		}
		out = append(out, Control{ID: c.ID, Status: shown, Recorded: c.Status, Evidence: c.Evidence, ObservedAt: utc(c.ObservedAt)})
	}
	rank := func(id string) int {
		if i := slices.Index(controlOrder, id); i >= 0 {
			return i
		}
		return len(controlOrder)
	}
	slices.SortStableFunc(out, func(a, b Control) int { return rank(a.ID) - rank(b.ID) })
	return out
}

func emergencyAccess(cluster *dbv1alpha1.PostgresCluster, all []dbv1alpha1.PostgresEmergencyAccess) EmergencyAccess {
	e := EmergencyAccess{Requests: []Request{}}
	if cluster.Spec.EmergencyAccess != nil {
		e.Enabled = cluster.Spec.EmergencyAccess.Enabled
	}

	var mine []dbv1alpha1.PostgresEmergencyAccess
	for _, req := range all {
		if req.Namespace == cluster.Namespace && req.Spec.ClusterRef.Name == cluster.Name {
			mine = append(mine, req)
		}
	}
	// Newest first: by when access began, or when the request was made if it never began.
	began := func(req dbv1alpha1.PostgresEmergencyAccess) time.Time {
		if s := req.Status.StartedAt; s != nil {
			return s.Time
		}
		return req.CreationTimestamp.Time
	}
	slices.SortStableFunc(mine, func(a, b dbv1alpha1.PostgresEmergencyAccess) int { return began(b).Compare(began(a)) })

	counts := map[string]int{}
	for _, req := range mine {
		st := req.Status.State
		if st == "" {
			st = dbv1alpha1.EmergencyPending
		}
		counts[st]++
		item := Request{
			Name: req.Name, State: st, RequestedBy: req.Status.RequestedBy, Reason: req.Spec.Reason, Role: req.Status.Role,
			StartedAt: utc(req.Status.StartedAt), ExpiresAt: utc(req.Status.ExpiresAt), EndedAt: utc(req.Status.EndedAt),
		}
		if st == dbv1alpha1.EmergencyFailed || st == dbv1alpha1.EmergencyDenied {
			item.Message = req.Status.Message
		}
		e.Requests = append(e.Requests, item)
	}
	for _, st := range stateOrder {
		if counts[st] > 0 {
			e.Counts = append(e.Counts, StateCount{State: st, Count: counts[st]})
		}
	}

	if e.Enabled || len(mine) > 0 {
		e.Warning = &EmergencyWarning{
			Title: EmergencyWarningTitle,
			Statements: []string{
				EmergencyNotInPgaudit, "While escalated, " + EmergencyClearText, EmergencyWhateverTheSettings, EmergencyAuditors,
			},
			Record: emergencyRecord,
		}
	}
	return e
}

func utc(t *metav1.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}
