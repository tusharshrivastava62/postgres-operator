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
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dbv1alpha1 "github.com/tusharshrivastava62/postgres-operator/api/v1alpha1"
)

// complianceEvaluation is what one reconcile worked out about the controls.
type complianceEvaluation struct {
	standard string
	// checkedAt is when the newest of the two jobs last reported, and zero
	// before either has.
	checkedAt time.Time
	controls  []dbv1alpha1.ComplianceControl
}

// reconcileComplianceStatus works out the cluster's controls and writes them,
// the ControlsMet condition and the metric into the cluster object. It reports
// whether anything changed. With no compliance settings there is nothing to
// report, and what an earlier standard left is cleared.
func (r *PostgresClusterReconciler) reconcileComplianceStatus(
	ctx context.Context, cluster *dbv1alpha1.PostgresCluster, pods []corev1.Pod,
) (bool, error) {
	eval, err := r.evaluateComplianceNow(ctx, cluster, pods)
	if err != nil {
		return false, err
	}
	r.recordComplianceMetrics(cluster, eval)
	return applyComplianceStatus(cluster, eval, time.Now()), nil
}

// evaluateComplianceNow reads the two jobs' newest reports and the cluster's
// network policy, and works out each control. It returns nil for a cluster
// with no compliance settings.
func (r *PostgresClusterReconciler) evaluateComplianceNow(
	ctx context.Context, cluster *dbv1alpha1.PostgresCluster, pods []corev1.Pod,
) (*complianceEvaluation, error) {
	compliance := cluster.Spec.Compliance
	if compliance == nil {
		return nil, nil
	}
	preset, ok := dbv1alpha1.PresetFor(compliance.Standard)
	if !ok {
		return nil, nil
	}
	check, err := latestReport[complianceReport](ctx, r, cluster, complianceCheckLabels(cluster))
	if err != nil {
		return nil, err
	}
	probe, err := latestReport[probeReport](ctx, r, cluster, networkProbeLabels(cluster))
	if err != nil {
		return nil, err
	}
	policy, err := r.policyState(ctx, cluster)
	if err != nil {
		return nil, err
	}

	facts := make([]podFact, 0, len(pods))
	for i := range pods {
		started := pods[i].CreationTimestamp.Time
		if pods[i].Status.StartTime != nil {
			started = pods[i].Status.StartTime.Time
		}
		facts = append(facts, podFact{Name: pods[i].Name, Started: started})
	}

	eval := &complianceEvaluation{
		standard: compliance.Standard,
		controls: evaluateControls(controlInputs{
			Cluster: cluster, Preset: preset, Check: check, Probe: probe, Policy: policy, Pods: facts, Now: time.Now(),
		}),
	}
	eval.checkedAt = check.at
	if probe.at.After(eval.checkedAt) {
		eval.checkedAt = probe.at
	}
	return eval, nil
}

// policyState reads the cluster's NetworkPolicy and compares it with the one
// the operator would write now.
func (r *PostgresClusterReconciler) policyState(ctx context.Context, cluster *dbv1alpha1.PostgresCluster) (policyState, error) {
	existing := &networkingv1.NetworkPolicy{}
	err := r.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: cluster.Name}, existing)
	if apierrors.IsNotFound(err) {
		return policyState{}, nil
	}
	if err != nil {
		return policyState{}, err
	}
	desired, err := r.desiredPolicy(ctx, cluster)
	if err != nil {
		return policyState{Found: true}, nil //nolint:nilerr // without the operator's identity there is nothing to compare with
	}
	return policyState{Found: true, Matches: equality.Semantic.DeepEqual(existing.Spec, desired.Spec)}, nil
}

// applyComplianceStatus writes an evaluation into the cluster's status and
// reports whether anything changed.
//
// Each control is stamped with when the jobs last reported, which changes once
// per run and not at every reconcile. Before the jobs have reported there is
// no such time, so a control keeps the time it had if its result is unchanged.
func applyComplianceStatus(cluster *dbv1alpha1.PostgresCluster, eval *complianceEvaluation, now time.Time) bool {
	if eval == nil {
		changed := meta.RemoveStatusCondition(&cluster.Status.Conditions, dbv1alpha1.ConditionControlsMet)
		if !equality.Semantic.DeepEqual(cluster.Status.Compliance, dbv1alpha1.ComplianceStatus{}) {
			cluster.Status.Compliance = dbv1alpha1.ComplianceStatus{}
			changed = true
		}
		return changed
	}

	previous := map[string]dbv1alpha1.ComplianceControl{}
	for _, c := range cluster.Status.Compliance.Controls {
		previous[c.ID] = c
	}
	controls := make([]dbv1alpha1.ComplianceControl, len(eval.controls))
	for i, c := range eval.controls {
		observed := metav1.NewTime(now.UTC().Truncate(time.Second))
		switch before, seen := previous[c.ID]; {
		case !eval.checkedAt.IsZero():
			observed = metav1.NewTime(eval.checkedAt.UTC().Truncate(time.Second))
		case seen && before.Status == c.Status && before.Evidence == c.Evidence && before.ObservedAt != nil:
			observed = *before.ObservedAt
		}
		c.ObservedAt = &observed
		controls[i] = c
	}

	status := dbv1alpha1.ComplianceStatus{Standard: eval.standard, Controls: controls}
	if !eval.checkedAt.IsZero() {
		checked := metav1.NewTime(eval.checkedAt.UTC().Truncate(time.Second))
		status.LastChecked = &checked
	}
	changed := false
	if !equality.Semantic.DeepEqual(cluster.Status.Compliance, status) {
		cluster.Status.Compliance = status
		changed = true
	}
	return meta.SetStatusCondition(&cluster.Status.Conditions, controlsMetCondition(controls)) || changed
}

// recordComplianceMetrics publishes how many controls are in each state, or
// drops the series when the cluster has no compliance settings.
func (r *PostgresClusterReconciler) recordComplianceMetrics(cluster *dbv1alpha1.PostgresCluster, eval *complianceEvaluation) {
	match := map[string]string{labelNamespace: cluster.Namespace, labelName: cluster.Name}
	if eval == nil {
		complianceControls.DeletePartialMatch(match)
		return
	}
	for status, count := range countControls(eval.controls) {
		complianceControls.WithLabelValues(cluster.Namespace, cluster.Name, status).Set(float64(count))
	}
}
