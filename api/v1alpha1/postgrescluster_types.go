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

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// PostgresClusterSpec defines the desired state of PostgresCluster
type PostgresClusterSpec struct {
	// instances is the number of PostgreSQL pods to run in the StatefulSet.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=9
	// +kubebuilder:default=1
	// +optional
	Instances int32 `json:"instances,omitempty"`

	// version is the major PostgreSQL version to run, e.g. "16". The enum
	// values are quoted so controller-gen emits string literals in the CRD
	// schema; unquoted here, numeric-looking marker values like 16 are
	// parsed as JSON numbers, which then can never match a string field.
	//
	// It is immutable: changing the major version of an existing cluster
	// would start the new binaries on the old data directory, which
	// Postgres refuses. A real major-version upgrade (pg_upgrade) needs its
	// own design, so until then the API server rejects any change. This is
	// a CEL rule on the CRD, enforced by the API server itself, so it holds
	// even if the validating webhook is down.
	// +kubebuilder:validation:Enum="16";"17";"18"
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="version is immutable"
	// +kubebuilder:validation:Required
	Version string `json:"version"`

	// storage configures the per-instance persistent volume.
	// +kubebuilder:validation:Required
	Storage StorageSpec `json:"storage"`

	// backup configures scheduled backups for the cluster. Omit or leave
	// enabled=false to run without scheduled backups.
	// +optional
	Backup BackupSpec `json:"backup,omitzero"`

	// resources describes the compute resource requests/limits applied to
	// each PostgreSQL pod.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitzero"`

	// ha tunes high availability. Every cluster runs under Patroni, even a
	// single-instance one, so there is no on/off switch: scaling instances
	// from 1 to 3 later is the same code path as creating a 3-instance
	// cluster, and the two modes never need to be converted into each other
	// on existing data.
	// +kubebuilder:default={}
	// +optional
	HA HASpec `json:"ha,omitzero"`

	// image overrides the container image used for the PostgreSQL pods. When
	// unset, the operator picks its built-in image for spec.version. Either
	// way, the image actually in use is recorded in status.image, and an
	// operator upgrade never changes it for an existing cluster: changing
	// the image (for example to pick up a newer PostgreSQL minor release) is
	// always a deliberate edit to this field.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=512
	// +optional
	Image string `json:"image,omitempty"`
}

// HASpec tunes failover behavior.
type HASpec struct {
	// failoverTimeout is how long, in seconds, a primary that stops
	// responding keeps the leader lock before a replica may take over. It
	// becomes Patroni's ttl, with loop_wait and retry_timeout set to a third
	// of it so Patroni's rule loop_wait + 2*retry_timeout <= ttl always
	// holds. Lower values mean faster failover but more false failovers on
	// a slow control plane; Patroni's own floor is 20.
	// +kubebuilder:validation:Minimum=20
	// +kubebuilder:validation:Maximum=300
	// +kubebuilder:default=30
	// +optional
	FailoverTimeout int32 `json:"failoverTimeout,omitempty"`

	// synchronousMode makes commits wait for a synchronous standby, so an
	// acknowledged transaction survives the loss of the primary. The price
	// is write latency, and writes can block if no standby is available.
	// With the default (false) replication is asynchronous: after a hard
	// failure the most recent transactions the replicas had not yet
	// received can be lost.
	// +kubebuilder:default=false
	// +optional
	SynchronousMode bool `json:"synchronousMode,omitempty"`

	// maximumLagOnFailover is how many bytes of WAL a replica may be behind
	// the primary and still be eligible for promotion. A more-lagged replica
	// is never promoted, which trades availability for not discarding more
	// data. 0 means only a fully caught-up replica may be promoted.
	//
	// This is a pointer on purpose: 0 is a meaningful value but the default
	// is not 0, and with a plain int64 and omitempty a client that reads and
	// rewrites the object would drop an explicit 0 and have the API server
	// silently re-default it to 1048576.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=1048576
	// +optional
	MaximumLagOnFailover *int64 `json:"maximumLagOnFailover,omitempty"`
}

// StorageSpec defines the persistent storage requested for each PostgreSQL
// instance.
type StorageSpec struct {
	// size is the amount of storage requested per instance.
	// +kubebuilder:validation:Required
	Size resource.Quantity `json:"size"`

	// storageClassName selects the StorageClass backing the PVC. Left unset
	// to use the cluster's default StorageClass.
	// +optional
	StorageClassName *string `json:"storageClassName,omitempty"`
}

// BackupSpec defines the scheduled backup configuration for a
// PostgresCluster.
type BackupSpec struct {
	// enabled turns the scheduled backup CronJob on or off.
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// schedule is a standard 5-field cron expression (e.g. "0 2 * * *")
	// describing when backups run.
	// +kubebuilder:validation:Pattern=`^\S+\s+\S+\s+\S+\s+\S+\s+\S+$`
	// +optional
	Schedule string `json:"schedule,omitempty"`
}

// PostgresClusterStatus defines the observed state of PostgresCluster.
type PostgresClusterStatus struct {
	// For Kubernetes API conventions, see:
	// https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md#typical-status-properties

	// observedGeneration is the .metadata.generation this status was
	// computed from, so a reader can tell whether status reflects the
	// latest spec or is still catching up to it.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// primaryInstance is the name of the pod currently holding the leader
	// lock, i.e. the one the read-write Service routes to. It is empty while
	// no primary exists, for example mid-failover.
	// +optional
	PrimaryInstance string `json:"primaryInstance,omitempty"`

	// replicationLagBytes maps each replica pod to how many bytes of WAL it
	// is behind the primary. Pods that are not currently streaming are
	// omitted rather than reported as zero.
	// +optional
	ReplicationLagBytes map[string]int64 `json:"replicationLagBytes,omitempty"`

	// image is the container image the PostgreSQL pods are running. It is
	// recorded the first time the cluster is reconciled and only changes
	// when spec.image does, so upgrading the operator never silently rolls
	// existing databases onto a new image.
	// +optional
	Image string `json:"image,omitempty"`

	// conditions represent the current state of the PostgresCluster resource.
	// Each condition has a unique type and reflects the status of a specific aspect of the resource.
	//
	// Condition types:
	// - "Available": a Ready primary exists, so the cluster accepts reads and writes
	// - "Progressing": the resource is being created or updated
	// - "Degraded": the most recent scheduled backup failed
	// - "ReplicationHealthy": exactly one primary, and every replica is streaming within the lag limit
	// - "FailoverInProgress": there is currently no primary, so a leader election is under way
	//
	// The status of each condition is one of True, False, or Unknown.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.spec.version`
// +kubebuilder:printcolumn:name="Instances",type=integer,JSONPath=`.spec.instances`
// +kubebuilder:printcolumn:name="Primary",type=string,JSONPath=`.status.primaryInstance`
// +kubebuilder:printcolumn:name="Available",type=string,JSONPath=`.status.conditions[?(@.type=="Available")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// PostgresCluster is the Schema for the postgresclusters API
type PostgresCluster struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of PostgresCluster
	// +required
	Spec PostgresClusterSpec `json:"spec"`

	// status defines the observed state of PostgresCluster
	// +optional
	Status PostgresClusterStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// PostgresClusterList contains a list of PostgresCluster
type PostgresClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []PostgresCluster `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &PostgresCluster{}, &PostgresClusterList{})
		return nil
	})
}
