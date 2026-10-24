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
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

const (
	labelNamespace = "namespace"
	labelName      = "name"
	labelResult    = "result"
	labelInstance  = "instance"
	labelStatus    = "status"
	labelState     = "state"
	labelKind      = "kind"
)

// Custom, per-cluster metrics. These are registered on the same registry
// controller-runtime already exposes at /metrics (see cmd/main.go), so
// nothing extra needs to be wired up to serve them - only to record them.
//
// All of them are labeled by namespace/name rather than aggregated globally.
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

	hasPrimary = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "postgrescluster_has_primary",
		Help: "1 when a PostgresCluster has a Ready primary holding a live leader lock, 0 otherwise. Mirrors the Available status condition.",
	}, []string{labelNamespace, labelName})

	primaryChangesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "postgrescluster_primary_changes_total",
		Help: "Number of times the primary of a PostgresCluster moved to a different instance, as seen by this operator process.",
	}, []string{labelNamespace, labelName})

	backupCount = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "postgrescluster_backup_count",
		Help: "Number of base backups in the destination, as of the last backup status check.",
	}, []string{labelNamespace, labelName})

	backupLastSizeBytes = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "postgrescluster_backup_last_size_bytes",
		Help: "Compressed size of the newest base backup.",
	}, []string{labelNamespace, labelName})

	walArchiveLagBytes = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "postgrescluster_wal_archive_lag_bytes",
		Help: "Bytes of WAL the primary has written that have not been archived, as of the last backup status check.",
	}, []string{labelNamespace, labelName})

	restoreVerificationLastSuccess = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "postgrescluster_restore_verification_last_success_timestamp_seconds",
		Help: "Unix timestamp of the last restore test that passed.",
	}, []string{labelNamespace, labelName})

	restoreVerificationDuration = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "postgrescluster_restore_verification_duration_seconds",
		Help: "How long the last passing restore test took: the measured time to recover.",
	}, []string{labelNamespace, labelName})

	complianceControls = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "postgrescluster_compliance_controls",
		Help: "Number of compliance controls of a PostgresCluster in each status (Met, NotMet, Unknown), as of the last compliance check.",
	}, []string{labelNamespace, labelName, labelStatus})

	emergencyAccessActive = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "postgrescluster_emergency_access_active",
		Help: "Number of emergency access grants of a PostgresCluster that are active now.",
	}, []string{labelNamespace, labelName})

	credentialAge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "postgrescluster_credential_age_seconds",
		Help: "Seconds since a kind of credential of a PostgresCluster was last replaced, or first set: superuser, replication, monitor, or, for the oldest of its roles, application.",
	}, []string{labelNamespace, labelName, labelKind})

	managedRoles = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "postgrescluster_roles",
		Help: "Number of database roles the operator manages for a PostgresCluster in each state (Ready, Pending, Error, Disabled).",
	}, []string{labelNamespace, labelName, labelState})

	replicationLagBytes = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "postgrescluster_replication_lag_bytes",
		Help: "Bytes of WAL each streaming replica is behind the primary. A replica that is not streaming has no series.",
	}, []string{labelNamespace, labelName, labelInstance})
)

func init() {
	ctrlmetrics.Registry.MustRegister(
		reconcileTotal,
		reconcileDurationSeconds,
		instancesReady,
		backupLastSuccessTimestampSeconds,
		hasPrimary,
		primaryChangesTotal,
		replicationLagBytes,
		backupCount,
		backupLastSizeBytes,
		walArchiveLagBytes,
		restoreVerificationLastSuccess,
		restoreVerificationDuration,
		complianceControls,
		managedRoles,
		credentialAge,
		emergencyAccessActive,
	)
}
