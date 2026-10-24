/*
Copyright 2026.

Licensed under the Business Source License 1.1 (the "License"). You may not
use this file except in compliance with the License, a copy of which is in the
LICENSE file at the root of this repository. Production use requires a
commercial license from the Licensor. On the Change Date (2030-10-17) the
work becomes available under the Change License (Apache-2.0).
*/

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// PostgresClusterSpec defines the desired state of PostgresCluster
//
// +kubebuilder:validation:XValidation:rule="has(self.bootstrap) == has(oldSelf.bootstrap) && (!has(self.bootstrap) || self.bootstrap == oldSelf.bootstrap)",message="bootstrap is set when the cluster is created and cannot be added, changed or removed"
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

	// bootstrap says how the cluster's data is first created. Leave it unset
	// for an empty database. It can only be set when the cluster is created.
	// +optional
	Bootstrap *BootstrapSpec `json:"bootstrap,omitempty"`

	// compliance applies a named preset of audit, logging and network
	// settings and reports, in status, whether each of its controls holds in
	// the running database. It configures and verifies; it does not make a
	// cluster compliant with anything. Leave it unset for none. It can be set
	// or changed on a running cluster, which replaces the pods one at a time.
	// +optional
	Compliance *ComplianceSpec `json:"compliance,omitempty"`

	// roles lists the database roles for applications that the operator
	// creates and keeps, each with a generated password in a Secret. A role
	// can own a database made for it. Removing a role from the list disables
	// its login and ends its sessions; it is never dropped.
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=32
	// +optional
	Roles []RoleSpec `json:"roles,omitempty"`

	// credentialRotation says how often the operator's own database
	// credentials are replaced. Leave it unset to replace them only when asked,
	// by annotating the Secret that holds them with db.youroperator.io/rotate
	// and any value not used before. The passwords of the roles in spec.roles
	// are set one by one, with rotateEvery.
	// +optional
	CredentialRotation *CredentialRotationSpec `json:"credentialRotation,omitempty"`

	// emergencyAccess lets people with permission to create
	// PostgresEmergencyAccess resources ask for temporary superuser access to
	// the cluster, for an emergency. Leave it unset to allow none.
	// +optional
	EmergencyAccess *EmergencyAccessSpec `json:"emergencyAccess,omitempty"`

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

// ComplianceSpec selects a preset and says where its audit logs go.
type ComplianceSpec struct {
	// standard is the preset to apply: "hipaa", "soc2" or "pci". Each sets
	// which statement classes pgaudit logs and the default audit log
	// retention, and decides which controls are checked.
	// +kubebuilder:validation:Enum=hipaa;soc2;pci
	// +kubebuilder:validation:Required
	Standard string `json:"standard"`

	// auditLogs says where audit logs are delivered and how long they are
	// expected to be kept.
	// +kubebuilder:default={}
	// +optional
	AuditLogs AuditLogsSpec `json:"auditLogs,omitzero"`

	// network adjusts the NetworkPolicy the operator creates for the
	// database pods.
	// +optional
	Network ComplianceNetworkSpec `json:"network,omitzero"`
}

// AuditLogsSpec is where audit logs are delivered.
type AuditLogsSpec struct {
	// destination is the S3 or S3-compatible bucket audit logs are uploaded
	// to. It is required when spec.compliance is set. It must not share a
	// path with the cluster's backups.
	// +optional
	Destination *BackupDestination `json:"destination,omitempty"`

	// retentionDays is how long audit logs are expected to be kept. The
	// operator does not enforce it: retention is a property of the bucket's
	// own lifecycle rules, and the operator checks that they cover this many
	// days. Leave it unset to use the standard's default.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=7300
	// +optional
	RetentionDays int32 `json:"retentionDays,omitempty"`

	// logParameters makes pgaudit record the values bound to a statement's
	// parameters. They are written in clear text, so any sensitive value a
	// client sends ends up in the audit log. Leave it off unless the log's
	// destination is protected to the same level as the database.
	// +kubebuilder:default=false
	// +optional
	LogParameters bool `json:"logParameters,omitempty"`
}

// ComplianceNetworkSpec adjusts the cluster's NetworkPolicy.
type ComplianceNetworkSpec struct {
	// allowFrom lists additional peers that may connect to PostgreSQL. Pods
	// labelled db.youroperator.io/client=true in the cluster's own namespace
	// are always allowed.
	// +kubebuilder:validation:MaxItems=20
	// +optional
	AllowFrom []networkingv1.NetworkPolicyPeer `json:"allowFrom,omitempty"`
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

// BackupSpec configures continuous WAL archiving and scheduled base backups,
// which together allow a restore to any point in time since the oldest kept
// base backup.
type BackupSpec struct {
	// enabled turns backups on. WAL archiving starts as soon as it is set;
	// base backups then run on schedule. It requires destination.
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// schedule is a standard 5-field cron expression (e.g. "0 2 * * *")
	// describing when base backups run. It is required when enabled is true.
	// +kubebuilder:validation:Pattern=`^\S+\s+\S+\s+\S+\s+\S+\s+\S+$`
	// +kubebuilder:validation:MaxLength=256
	// +optional
	Schedule string `json:"schedule,omitempty"`

	// destination is where backups and archived WAL are stored.
	// +optional
	Destination *BackupDestination `json:"destination,omitempty"`

	// encryption encrypts backups and WAL before they leave the pod.
	// +optional
	Encryption *BackupEncryption `json:"encryption,omitempty"`

	// retention says which backups to keep.
	// +kubebuilder:default={}
	// +optional
	Retention BackupRetention `json:"retention,omitzero"`

	// verification restores the newest backup on a schedule to prove that it
	// can be restored, and records how long that took.
	// +optional
	Verification BackupVerification `json:"verification,omitzero"`
}

// BackupDestination is an S3 or S3-compatible bucket.
type BackupDestination struct {
	// type is the kind of store. Only "s3" is supported; it covers Amazon S3
	// and any store that speaks its API.
	// +kubebuilder:validation:Enum=s3
	// +kubebuilder:default=s3
	// +optional
	Type string `json:"type,omitempty"`

	// bucket is the name of an existing bucket.
	// +kubebuilder:validation:MinLength=3
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9][a-z0-9.-]*[a-z0-9]$`
	// +kubebuilder:validation:Required
	Bucket string `json:"bucket"`

	// region is the bucket's region. Leave it unset for stores that have none.
	// +kubebuilder:validation:MaxLength=64
	// +optional
	Region string `json:"region,omitempty"`

	// endpoint is the URL of an S3-compatible store such as MinIO or Ceph.
	// Leave it unset for Amazon S3.
	// +kubebuilder:validation:Pattern=`^https?://[^\s/]+(/\S*)?$`
	// +kubebuilder:validation:MaxLength=512
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// forcePathStyle addresses the bucket as host/bucket instead of
	// bucket.host, which most S3-compatible stores need.
	// +optional
	ForcePathStyle bool `json:"forcePathStyle,omitempty"`

	// path is where in the bucket this cluster's backups live, without
	// leading or trailing slashes. It defaults to <namespace>/<cluster name>.
	// Two clusters must not share a path.
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)*$`
	// +kubebuilder:validation:MaxLength=512
	// +kubebuilder:validation:XValidation:rule="!self.split('/').exists(s, s == '.' || s == '..')",message="path must not contain . or .. segments"
	// +optional
	Path string `json:"path,omitempty"`

	// credentialsSecret names a Secret in the cluster's namespace with the
	// keys AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Required
	CredentialsSecret string `json:"credentialsSecret"`
}

// BackupEncryption names the key that encrypts backups. Losing it makes the
// backups unreadable, and nothing can recover them.
type BackupEncryption struct {
	// keySecret names a Secret in the cluster's namespace whose
	// "libsodium-key" entry holds a 32-byte key written as 64 hexadecimal
	// characters. The operator never creates or changes it.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Required
	KeySecret string `json:"keySecret"`
}

// BackupRetention keeps the newest backups and everything recent enough to
// restore to. Both limits apply: a backup is deleted only when more than
// full backups exist and it is older than days.
type BackupRetention struct {
	// full is the smallest number of full base backups to keep.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=365
	// +kubebuilder:default=7
	// +optional
	Full int32 `json:"full,omitempty"`

	// days is how far back, in days, a restore must be possible.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=3650
	// +kubebuilder:default=14
	// +optional
	Days int32 `json:"days,omitempty"`
}

// BackupVerification configures restore testing.
type BackupVerification struct {
	// enabled turns restore testing on. It requires backup.enabled.
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// schedule is a standard 5-field cron expression for the restore test.
	// It is required when enabled is true.
	// +kubebuilder:validation:Pattern=`^\S+\s+\S+\s+\S+\s+\S+\s+\S+$`
	// +kubebuilder:validation:MaxLength=256
	// +optional
	Schedule string `json:"schedule,omitempty"`
}

// BootstrapSpec chooses how a new cluster gets its data.
type BootstrapSpec struct {
	// recovery restores the cluster from another cluster's backups instead of
	// starting empty. The new cluster gets its own new passwords and writes
	// its own backups to its own path, never to the source's.
	// +optional
	Recovery *RecoverySpec `json:"recovery,omitempty"`
}

// RecoverySpec describes the backups to restore from. The new cluster must
// use the same PostgreSQL major version as the source, since backups are
// stored per major version.
type RecoverySpec struct {
	// sourceCluster is the name the backups were taken under.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Required
	SourceCluster string `json:"sourceCluster"`

	// sourceNamespace is the namespace the source cluster ran in. It
	// defaults to this cluster's namespace.
	// +kubebuilder:validation:MaxLength=63
	// +optional
	SourceNamespace string `json:"sourceNamespace,omitempty"`

	// destination is where the source's backups are. It is the source's
	// backup destination, including the path it used, and credentialsSecret
	// names a Secret in this cluster's namespace.
	// +kubebuilder:validation:Required
	Destination BackupDestination `json:"destination"`

	// encryption names the key the source's backups were encrypted with, if
	// they were. It is independent of this cluster's own backup encryption.
	// +optional
	Encryption *BackupEncryption `json:"encryption,omitempty"`

	// targetTime restores to this moment instead of the latest. It is a
	// timestamp with a time zone.
	// +optional
	TargetTime *metav1.Time `json:"targetTime,omitempty"`
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

	// backup reports what can be restored and how healthy backing up is.
	// +optional
	Backup BackupStatus `json:"backup,omitzero"`

	// compliance reports which controls of the selected standard hold in the
	// running cluster, as last observed.
	// +optional
	Compliance ComplianceStatus `json:"compliance,omitzero"`

	// accessControl reports the database roles the operator manages.
	// +optional
	AccessControl AccessControlStatus `json:"accessControl,omitzero"`

	// conditions represent the current state of the PostgresCluster resource.
	// Each condition has a unique type and reflects the status of a specific aspect of the resource.
	//
	// Condition types:
	// - "Available": a Ready primary exists, so the cluster accepts reads and writes
	// - "Progressing": the resource is being created or updated
	// - "Degraded": a backup, WAL archiving or a restore test is unhealthy
	// - "BackupHealthy": the newest base backup is no older than two scheduled intervals
	// - "WALArchivingHealthy": WAL archiving keeps up with the primary's writes
	// - "ReplicationHealthy": exactly one primary, and every replica is streaming within the lag limit
	// - "FailoverInProgress": there is currently no primary, so a leader election is under way
	// - "ControlsMet": every control of the selected compliance standard is Met
	//
	// The status of each condition is one of True, False, or Unknown.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// RoleSpec is one database role for an application.
//
// Every role the operator makes can log in and nothing else: it is not a
// superuser and cannot create roles or databases, replicate, or bypass row
// security. What it owns is its own database, if it has one.
type RoleSpec struct {
	// name is the role's name in the database: lower-case letters, digits
	// and underscores, starting with a letter. Names starting with pg_ or
	// bg_, and the names PostgreSQL, Patroni and this operator use for their
	// own roles, are refused.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z][a-z0-9_]*$`
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// database names a database to create for the role to own. Everyone else
	// is refused connection to it. A database that already exists is left
	// alone, and is reported as an error unless this role owns it. Only one
	// role can name a database.
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z][a-z0-9_]*$`
	// +optional
	Database string `json:"database,omitempty"`

	// connectionLimit is how many connections the role may hold at once.
	// Leave it unset for no limit.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=10000
	// +optional
	ConnectionLimit *int32 `json:"connectionLimit,omitempty"`

	// rotateEvery replaces the role's password this often: a new one is
	// generated, set in the database, and written to the Secret. Open
	// connections are not affected, but new ones made with the old password
	// are refused until the application has read the Secret again. At least 24
	// hours. Leave it unset to replace the password only when asked.
	// +optional
	RotateEvery *metav1.Duration `json:"rotateEvery,omitempty"`

	// secretName is the Secret the role's connection details are written to:
	// username, password, host, port and, with a database, dbname. It
	// defaults to <cluster>-role-<name>, with underscores written as hyphens.
	// The Secret is not deleted with the cluster or with the role.
	// +kubebuilder:validation:MaxLength=253
	// +optional
	SecretName string `json:"secretName,omitempty"`
}

// CredentialRotationSpec is how often the operator's own credentials change.
type CredentialRotationSpec struct {
	// superuserEvery replaces the superuser's password and the replication
	// password, together. Every database pod is replaced, one at a time, to
	// pick up the new ones, replicas first, with a switchover before the
	// primary: the cost of a rolling update, about two seconds of refused
	// writes. It starts only when every member is streaming. At least 24 hours.
	// +optional
	SuperuserEvery *metav1.Duration `json:"superuserEvery,omitempty"`

	// monitorEvery replaces the password of the read-only role the operator's
	// own jobs connect as. Nothing else is affected. At least 24 hours.
	// +optional
	MonitorEvery *metav1.Duration `json:"monitorEvery,omitempty"`
}

// Role states.
const (
	// RoleReady means the role exists, with the privileges the operator gave
	// it and the password in its Secret.
	RoleReady = "Ready"
	// RolePending means the operator has not been able to set the role up
	// yet, for example because the cluster has no primary.
	RolePending = "Pending"
	// RoleError means the operator tried and could not.
	RoleError = "Error"
	// RoleDisabled means the role was made by the operator, is no longer in
	// the spec, and can no longer log in. It has not been dropped.
	RoleDisabled = "Disabled"
)

// ConditionRolesReady is true when every role in spec.roles is Ready.
const ConditionRolesReady = "RolesReady"

// AccessControlStatus is what the operator last observed about the database
// roles it manages.
type AccessControlStatus struct {
	// monitorRole is the read-only role the operator's own jobs connect as,
	// so that none of them holds the superuser password.
	// +optional
	MonitorRole RoleStatus `json:"monitorRole,omitzero"`

	// credentials is the state of the superuser and replication passwords.
	// +optional
	Credentials CredentialsStatus `json:"credentials,omitzero"`

	// roles is the state of each role in spec.roles, and of each role the
	// operator made that has since been removed from it.
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=64
	// +optional
	Roles []RoleStatus `json:"roles,omitempty"`
}

// Credential rotation states.
const (
	// CredentialsIdle means no rotation is under way or due.
	CredentialsIdle = "Idle"
	// CredentialsWaiting means one is due, and is held back until the cluster
	// is healthy and not being updated.
	CredentialsWaiting = "Waiting"
	// CredentialsRotating means the new passwords are being set.
	CredentialsRotating = "Rotating"
	// CredentialsRolling means the new passwords are set, and the pods are
	// being replaced so that they start with them.
	CredentialsRolling = "Rolling"
	// CredentialsError means a rotation could not be carried out.
	CredentialsError = "Error"
)

// CredentialsStatus is the state of the superuser and replication passwords.
type CredentialsStatus struct {
	// state is Idle, Waiting, Rotating, Rolling or Error.
	// +kubebuilder:validation:Enum=Idle;Waiting;Rotating;Rolling;Error
	// +optional
	State string `json:"state,omitempty"`

	// message says what a rotation is waiting for, or what went wrong.
	// +kubebuilder:validation:MaxLength=512
	// +optional
	Message string `json:"message,omitempty"`

	// lastRotated is when the passwords were last replaced by the operator,
	// or when they were first set.
	// +optional
	LastRotated *metav1.Time `json:"lastRotated,omitempty"`
}

// RoleStatus is the state of one role the operator manages.
type RoleStatus struct {
	// name is the role's name in the database.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// state is Ready, Pending, Error or Disabled.
	// +kubebuilder:validation:Enum=Ready;Pending;Error;Disabled
	// +optional
	State string `json:"state,omitempty"`

	// secretName is the Secret holding the role's username and password.
	// +optional
	SecretName string `json:"secretName,omitempty"`

	// lastRotated is when the role's password was last replaced by the
	// operator, or when it was first set.
	// +optional
	LastRotated *metav1.Time `json:"lastRotated,omitempty"`

	// message says what is wrong when the state is not Ready.
	// +kubebuilder:validation:MaxLength=512
	// +optional
	Message string `json:"message,omitempty"`
}

// ComplianceStatus is what the operator last observed about the controls of
// the selected standard.
type ComplianceStatus struct {
	// standard is the preset the controls below were checked against.
	// +optional
	Standard string `json:"standard,omitempty"`

	// lastChecked is when the controls were last read from the cluster.
	// +optional
	LastChecked *metav1.Time `json:"lastChecked,omitempty"`

	// controls is the result for each control the preset checks.
	// +listType=map
	// +listMapKey=id
	// +kubebuilder:validation:MaxItems=32
	// +optional
	Controls []ComplianceControl `json:"controls,omitempty"`
}

// ComplianceControl is one control and whether it held when last checked.
type ComplianceControl struct {
	// id names the control, for example "audit-logging".
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Required
	ID string `json:"id"`

	// status is Met when the evidence shows the control holds, NotMet when it
	// shows it does not, and Unknown when it could not be checked. Unknown
	// never counts as Met.
	// +kubebuilder:validation:Enum=Met;NotMet;Unknown
	// +kubebuilder:validation:Required
	Status string `json:"status"`

	// evidence is what was observed, in words.
	// +kubebuilder:validation:MaxLength=1024
	// +optional
	Evidence string `json:"evidence,omitempty"`

	// observedAt is when the evidence was read.
	// +optional
	ObservedAt *metav1.Time `json:"observedAt,omitempty"`
}

// BackupStatus is what the operator last observed in the backup destination.
type BackupStatus struct {
	// lastBackupTime is when the newest base backup finished.
	// +optional
	LastBackupTime *metav1.Time `json:"lastBackupTime,omitempty"`

	// lastBackupName is the newest base backup's name in the destination.
	// +optional
	LastBackupName string `json:"lastBackupName,omitempty"`

	// lastBackupSizeBytes is its compressed size.
	// +optional
	LastBackupSizeBytes int64 `json:"lastBackupSizeBytes,omitempty"`

	// backupCount is how many base backups exist.
	// +optional
	BackupCount int32 `json:"backupCount,omitempty"`

	// oldestRecoveryPoint is the earliest time a restore can reach.
	// +optional
	OldestRecoveryPoint *metav1.Time `json:"oldestRecoveryPoint,omitempty"`

	// walArchiving says whether WAL is reaching the destination.
	// +optional
	WALArchiving WALArchivingStatus `json:"walArchiving,omitzero"`

	// verification is the outcome of the last restore test.
	// +optional
	Verification VerificationStatus `json:"verification,omitzero"`
}

// WALArchivingStatus describes how far WAL archiving is behind.
type WALArchivingStatus struct {
	// lastArchivedTime is when a WAL segment was last archived.
	// +optional
	LastArchivedTime *metav1.Time `json:"lastArchivedTime,omitempty"`

	// lagBytes is how much WAL the primary has written that is not archived.
	// +optional
	LagBytes int64 `json:"lagBytes,omitempty"`
}

// VerificationStatus is the outcome of restore testing.
type VerificationStatus struct {
	// lastRunTime is when the last restore test finished.
	// +optional
	LastRunTime *metav1.Time `json:"lastRunTime,omitempty"`

	// lastSuccessTime is when a restore test last passed.
	// +optional
	LastSuccessTime *metav1.Time `json:"lastSuccessTime,omitempty"`

	// durationSeconds is how long the last passing restore took, which is the
	// measured time to recover.
	// +optional
	DurationSeconds int64 `json:"durationSeconds,omitempty"`

	// message describes a failure.
	// +optional
	Message string `json:"message,omitempty"`
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
