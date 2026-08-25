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
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

const (
	labelNamespace = "namespace"
	labelName      = "name"
	labelResult    = "result"
)

// Custom, per-cluster metrics. These are registered on the same registry
// controller-runtime already exposes at /metrics (see cmd/main.go), so
// nothing extra needs to be wired up to serve them - only to record them.
//
// All four are labeled by namespace/name rather than aggregated globally.
// For an operator this is the useful shape: "reconcileErrorsTotal is up"
// tells you the controller is unhealthy in general, but "which cluster's
// reconcileErrorsTotal is up" is what an on-call actually needs to act on.
// This does mean the label cardinality grows with the number of
// PostgresCluster objects, which is fine at the scale this operator
// targets but is worth knowing if that ever changes.
var (
	reconcileTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "postgrescluster_reconcile_total",
		Help: "Total number of reconciles per PostgresCluster, by result.",
	}, []string{labelNamespace, labelName, labelResult})

	reconcileDurationSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "postgrescluster_reconcile_duration_seconds",
		Help:    "Time each reconcile of a PostgresCluster took, in seconds.",
		Buckets: prometheus.DefBuckets,
	}, []string{labelNamespace, labelName})

	instancesReady = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "postgrescluster_instances_ready",
		Help: "Number of ready Postgres instances for a PostgresCluster, mirroring the Available status condition.",
	}, []string{labelNamespace, labelName})

	backupLastSuccessTimestampSeconds = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "postgrescluster_backup_last_success_timestamp_seconds",
		Help: "Unix timestamp of the most recently completed successful backup Job for a PostgresCluster.",
	}, []string{labelNamespace, labelName})
)

func init() {
	ctrlmetrics.Registry.MustRegister(
		reconcileTotal,
		reconcileDurationSeconds,
		instancesReady,
		backupLastSuccessTimestampSeconds,
	)
}
