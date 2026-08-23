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

	// version is the major PostgreSQL version to run, e.g. "16".
	// +kubebuilder:validation:Enum=14;15;16;17
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
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// For Kubernetes API conventions, see:
	// https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md#typical-status-properties

	// conditions represent the current state of the PostgresCluster resource.
	// Each condition has a unique type and reflects the status of a specific aspect of the resource.
	//
	// Standard condition types include:
	// - "Available": the resource is fully functional
	// - "Progressing": the resource is being created or updated
	// - "Degraded": the resource failed to reach or maintain its desired state
	//
	// The status of each condition is one of True, False, or Unknown.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

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
