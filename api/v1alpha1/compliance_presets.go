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
	"slices"
	"strings"
)

// The compliance standards a preset exists for.
const (
	StandardHIPAA = "hipaa"
	StandardSOC2  = "soc2"
	StandardPCI   = "pci"
)

// ConditionControlsMet is true when every control of the selected standard is
// Met. A control that is Unknown does not count as Met.
const ConditionControlsMet = "ControlsMet"

// The values of ComplianceControl.Status.
const (
	ControlMet     = "Met"
	ControlNotMet  = "NotMet"
	ControlUnknown = "Unknown"
)

// The controls a preset can ask for. Each is checked against the running
// cluster, not against the spec, so that a restart that never happened or a
// stuck shipper shows up as a control that is not met.
const (
	// ControlAuditLogging: pgaudit is loaded and logs the preset's classes.
	ControlAuditLogging = "audit-logging"
	// ControlConnectionLogging: connections and disconnections are logged.
	ControlConnectionLogging = "connection-logging"
	// ControlEncryptionInTransit: TLS is on and plain connections are refused.
	ControlEncryptionInTransit = "encryption-in-transit"
	// ControlAuthentication: passwords are stored with scram-sha-256 and no
	// connection is accepted without one.
	ControlAuthentication = "authentication"
	// ControlDataIntegrity: data checksums are on.
	ControlDataIntegrity = "data-integrity"
	// ControlAuditLogDelivery: audit logs are reaching the destination.
	ControlAuditLogDelivery = "audit-log-delivery"
	// ControlAuditLogRetention: the destination keeps audit logs for as long
	// as retentionDays says.
	ControlAuditLogRetention = "audit-log-retention"
	// ControlNetworkIsolation: only the intended peers can reach PostgreSQL.
	ControlNetworkIsolation = "network-isolation"
	// ControlBackupRecovery: backups are on, encrypted and restore-tested.
	ControlBackupRecovery = "backup-recovery"
	// ControlAvailability: more than one instance, and replication healthy.
	ControlAvailability = "availability"
)

// pgaudit statement classes. "all" and "none" are left out on purpose: a
// preset names the classes it wants.
const (
	AuditRead  = "read"
	AuditWrite = "write"
	AuditDDL   = "ddl"
	AuditRole  = "role"
)

// Preset is what a compliance standard changes. It is data, so that what a
// standard means is one table that can be read, reviewed and tested.
//
// The standards differ only in which statements are audited and how long the
// logs are expected to be kept. What a control is said to support, in terms of
// a regulation's own clauses, is deliberately not recorded here yet: that
// wording needs review by someone qualified before it is published.
type Preset struct {
	// Standard is the value of spec.compliance.standard.
	Standard string
	// AuditClasses are the pgaudit statement classes to log, in order.
	AuditClasses []string
	// DefaultRetentionDays is how long audit logs are expected to be kept
	// when spec.compliance.auditLogs.retentionDays is unset. These are
	// defaults, not advice.
	DefaultRetentionDays int32
	// Controls are the IDs of the controls checked for this standard.
	Controls []string
}

// allControls is every control a preset asks for. It is the same list for all
// three standards for now: giving them different lists would be a claim about
// what each standard needs, which waits for the review above.
func allControls() []string {
	return []string{
		ControlAuditLogging, ControlConnectionLogging, ControlEncryptionInTransit, ControlAuthentication,
		ControlDataIntegrity, ControlAuditLogDelivery, ControlAuditLogRetention, ControlNetworkIsolation,
		ControlBackupRecovery, ControlAvailability,
	}
}

func presets() []Preset {
	return []Preset{
		{
			Standard:             StandardHIPAA,
			AuditClasses:         []string{AuditRead, AuditWrite, AuditDDL, AuditRole},
			DefaultRetentionDays: 2190,
			Controls:             allControls(),
		},
		{
			Standard:             StandardSOC2,
			AuditClasses:         []string{AuditRead, AuditWrite, AuditDDL},
			DefaultRetentionDays: 365,
			Controls:             allControls(),
		},
		{
			Standard:             StandardPCI,
			AuditClasses:         []string{AuditRead, AuditWrite, AuditDDL, AuditRole},
			DefaultRetentionDays: 365,
			Controls:             allControls(),
		},
	}
}

// Standards lists the standards that have a preset.
func Standards() []string {
	all := presets()
	names := make([]string, len(all))
	for i, p := range all {
		names[i] = p.Standard
	}
	return names
}

// PresetFor returns the preset for a standard, and false for one that has none.
func PresetFor(standard string) (Preset, bool) {
	for _, p := range presets() {
		if p.Standard == standard {
			return p, true
		}
	}
	return Preset{}, false
}

// Parameters returns the PostgreSQL settings the preset applies to a running
// cluster, by name. They take effect with a reload, no restart: pgaudit itself
// is loaded through the pod template, since the library list is read at start.
//
// logParameters is spec.compliance.auditLogs.logParameters. log_statement is
// switched off because pgaudit already records every statement it would, and
// leaving it on would write each DDL statement twice.
func (p Preset) Parameters(logParameters bool) map[string]string {
	return map[string]string{
		"pgaudit.log":           strings.Join(p.AuditClasses, ","),
		"pgaudit.log_catalog":   settingOff,
		"pgaudit.log_parameter": onOff(logParameters),
		"log_statement":         "none",
	}
}

const (
	settingOn  = "on"
	settingOff = "off"
)

func onOff(b bool) string {
	if b {
		return settingOn
	}
	return settingOff
}

// auditPathPrefix keeps a cluster's audit logs apart from its backups, which
// default to <namespace>/<name>. A path nested inside another would be caught
// by a lifecycle rule meant for the other.
const auditPathPrefix = "audit"

// AuditPathFor returns where in the bucket a cluster's audit logs are kept:
// the destination's path when it sets one, otherwise audit/<namespace>/<name>.
func (c *ComplianceSpec) AuditPathFor(namespace, name string) string {
	if c != nil && c.AuditLogs.Destination != nil && c.AuditLogs.Destination.Path != "" {
		return strings.Trim(c.AuditLogs.Destination.Path, "/")
	}
	return auditPathPrefix + "/" + namespace + "/" + name
}

// EffectiveRetentionDays is the retention audit logs are expected to have:
// the spec's value, or the standard's default.
func (c *ComplianceSpec) EffectiveRetentionDays() int32 {
	if c == nil {
		return 0
	}
	if c.AuditLogs.RetentionDays > 0 {
		return c.AuditLogs.RetentionDays
	}
	if p, ok := PresetFor(c.Standard); ok {
		return p.DefaultRetentionDays
	}
	return 0
}

// PathsOverlap reports whether two bucket paths are the same or one is inside
// the other, comparing whole segments, so "team/db" and "team/db2" do not.
func PathsOverlap(a, b string) bool {
	as := strings.Split(strings.Trim(a, "/"), "/")
	bs := strings.Split(strings.Trim(b, "/"), "/")
	n := min(len(as), len(bs))
	return slices.Equal(as[:n], bs[:n])
}
