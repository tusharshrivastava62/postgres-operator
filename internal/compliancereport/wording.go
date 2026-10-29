/*
Copyright 2026.

Licensed under the Business Source License 1.1 (the "License"). You may not
use this file except in compliance with the License, a copy of which is in the
LICENSE file at the root of this repository. Production use requires a
commercial license from the Licensor. On the Change Date (2030-10-17) the
work becomes available under the Change License (Apache-2.0).
*/

// Package compliancereport turns the compliance status the operator writes
// into a report a person can read. It reads; it never connects to a database
// and never changes anything.
package compliancereport

// The fixed wording of the report. Every sentence here is copied from
// docs/compliance.md or docs/emergency-access.md, and wording_test.go fails if
// one of them stops appearing there word for word, so the report and the docs
// cannot drift apart. Change a sentence in the docs and here together.
const (
	// PresetIsNotCompliance opens docs/compliance.md.
	PresetIsNotCompliance = "Enabling a preset does not make a cluster compliant with HIPAA, SOC 2, PCI DSS or anything else."

	// MappingUnreviewed says nobody qualified has looked at the controls.
	MappingUnreviewed = "The mapping from these controls to any regulation has not been reviewed by anyone qualified."

	// ControlsMetMeaning says what the roll-up does and does not mean.
	ControlsMetMeaning = "ControlsMet being True means one thing: every check this operator performs passed the last time it ran. " +
		"It does not mean your cluster is compliant, secure, or audit-ready."

	// NotAProofForAuditors is the instruction that follows ControlsMetMeaning.
	NotAProofForAuditors = "Do not present it to an auditor as proof of compliance."

	// NotExhaustive opens the list of what the user remains responsible for.
	NotExhaustive = "Anything that is not in the table above is not checked."

	// ChecksStop is what happens when the jobs stop reporting.
	ChecksStop = "If the jobs stop running, the controls that rest on their reports go Unknown after 35 minutes."

	// EmergencyWarningTitle is the heading of the warning in docs/emergency-access.md.
	EmergencyWarningTitle = "WARNING: while you are escalated, your statements and their values are logged in clear text"

	// EmergencyNotInPgaudit says where escalated sessions are not recorded.
	EmergencyNotInPgaudit = "pgaudit does not record an escalated session's reads and writes."

	// EmergencyClearText says what the server log does hold.
	EmergencyClearText = "the server writes every statement you run to its log, and it writes the values you bind to a statement's " +
		"parameters there too, in clear text."

	// EmergencyWhateverTheSettings says the audit settings do not change it.
	EmergencyWhateverTheSettings = "This happens whatever the cluster's audit settings say: logParameters: false does not stop it."

	// EmergencyAuditors is the sentence for anyone using emergency access in an audit.
	EmergencyAuditors = "If you are using emergency access as part of an audit, your auditors will be able to read every value you used."
)

// NotChecked is the headings of docs/compliance.md's "What you remain
// responsible for", in the order the docs give them.
var NotChecked = []string{
	"People and process.",
	"The scope of the audit.",
	"Protecting the audit log.",
	"Retention.",
	"Encryption at rest.",
	"The network.",
	"Certificates.",
	"Roles and credentials.",
	"Emergency access, if you allow it.",
	"The cluster around it.",
	"Checks that stop.",
}

// DocsPointer says where the full lists are.
const DocsPointer = "docs/compliance.md (What you remain responsible for) and docs/emergency-access.md in the operator's repository"
