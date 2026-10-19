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
	"fmt"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	dbv1alpha1 "github.com/tusharshrivastava62/postgres-operator/api/v1alpha1"
)

const (
	settingLogConnections    = "log_connections"
	settingLogDisconnections = "log_disconnections"
	settingOn                = "on"

	// auditDeliveryWithin is how recently each pod must have shipped for audit
	// log delivery to count as working: three of the shipper's five minute
	// intervals.
	auditDeliveryWithin = 15 * time.Minute

	reasonAllControlsMet     = "AllControlsMet"
	reasonControlsNotMet     = "ControlsNotMet"
	reasonControlsNotObserve = "ControlsNotObserved"
)

// podFact is a database pod as the controls need to know it.
type podFact struct {
	Name    string
	Started time.Time
}

// policyState is whether the cluster's NetworkPolicy exists, and whether it is
// the one the operator would write.
type policyState struct {
	Found   bool
	Matches bool
}

// controlInputs is everything the controls are worked out from: the spec and
// conditions the operator already has, what the two jobs last reported, and
// the pods and policy as they are now.
type controlInputs struct {
	Cluster *dbv1alpha1.PostgresCluster
	Preset  dbv1alpha1.Preset
	Check   checked[complianceReport]
	Probe   checked[probeReport]
	Policy  policyState
	Pods    []podFact
	Now     time.Time
}

// result is one control's outcome.
type result struct {
	status   string
	evidence string
}

func met(format string, args ...any) result {
	return result{dbv1alpha1.ControlMet, fmt.Sprintf(format, args...)}
}

func notMet(format string, args ...any) result {
	return result{dbv1alpha1.ControlNotMet, fmt.Sprintf(format, args...)}
}

func unknown(format string, args ...any) result {
	return result{dbv1alpha1.ControlUnknown, fmt.Sprintf(format, args...)}
}

// evaluateControls works out each control the preset asks for. A control whose
// evidence could not be read is Unknown, never Met.
func evaluateControls(in controlInputs) []dbv1alpha1.ComplianceControl {
	checks := map[string]func(controlInputs) result{
		dbv1alpha1.ControlAuditLogging:        auditLogging,
		dbv1alpha1.ControlConnectionLogging:   connectionLogging,
		dbv1alpha1.ControlEncryptionInTransit: encryptionInTransit,
		dbv1alpha1.ControlAuthentication:      authentication,
		dbv1alpha1.ControlDataIntegrity:       dataIntegrity,
		dbv1alpha1.ControlAuditLogDelivery:    auditLogDelivery,
		dbv1alpha1.ControlAuditLogRetention:   auditLogRetention,
		dbv1alpha1.ControlNetworkIsolation:    networkIsolation,
		dbv1alpha1.ControlBackupRecovery:      backupRecovery,
		dbv1alpha1.ControlAvailability:        availability,
	}
	controls := make([]dbv1alpha1.ComplianceControl, 0, len(in.Preset.Controls))
	for _, id := range in.Preset.Controls {
		check, ok := checks[id]
		res := unknown("this control is not checked yet")
		if ok {
			res = check(in)
		}
		controls = append(controls, dbv1alpha1.ComplianceControl{ID: id, Status: res.status, Evidence: res.evidence})
	}
	return controls
}

// sql returns what the database said, or why there is nothing to go on.
func (in controlInputs) sql() (*sqlFacts, string) {
	report, why := in.usableCheck()
	if report == nil {
		return nil, why
	}
	if report.SQL == nil || !report.SQL.OK {
		reason := "the database could not be read"
		if report.SQL != nil && report.SQL.Error != "" {
			reason += ": " + report.SQL.Error
		}
		return nil, reason
	}
	return report.SQL, ""
}

func (in controlInputs) bucket() (*bucketFacts, string) {
	report, why := in.usableCheck()
	if report == nil {
		return nil, why
	}
	if report.Bucket == nil || !report.Bucket.OK {
		reason := "the audit bucket could not be read"
		if report.Bucket != nil && report.Bucket.Error != "" {
			reason += ": " + report.Bucket.Error
		}
		return nil, reason
	}
	return report.Bucket, ""
}

// usableCheck is the compliance check's report, unless there is none, it is
// unreadable, or it is too old to believe.
func (in controlInputs) usableCheck() (*complianceReport, string) {
	switch {
	case in.Check.at.IsZero():
		return nil, "waiting for the first compliance check"
	case in.Check.report == nil:
		return nil, "the compliance check's report could not be read"
	case in.Now.Sub(in.Check.at) > complianceStaleAfter:
		return nil, fmt.Sprintf("the compliance check has not reported since %s", in.Check.at.UTC().Format(time.RFC3339))
	}
	return in.Check.report, ""
}

func sameClasses(setting string, classes []string) bool {
	got := []string{}
	for c := range strings.SplitSeq(setting, ",") {
		if c = strings.TrimSpace(c); c != "" {
			got = append(got, c)
		}
	}
	want := slices.Clone(classes)
	slices.Sort(got)
	slices.Sort(want)
	return slices.Equal(got, want)
}

func auditLogging(in controlInputs) result {
	facts, why := in.sql()
	if facts == nil {
		return unknown("%s", why)
	}
	libraries := facts.Settings["shared_preload_libraries"]
	if !slices.Contains(strings.Split(libraries, ","), pgauditLibrary) {
		return notMet("pgaudit is not loaded: shared_preload_libraries is %q, and a restart may still be pending", libraries)
	}
	want := strings.Join(in.Preset.AuditClasses, ",")
	got, set := facts.Settings["pgaudit.log"]
	switch {
	case !set || got == "":
		return notMet("pgaudit is loaded but pgaudit.log is not set; it should be %s", want)
	case !sameClasses(got, in.Preset.AuditClasses):
		return notMet("pgaudit.log is %s, not %s", got, want)
	}
	return met("pgaudit is loaded and logs %s", want)
}

func connectionLogging(in controlInputs) result {
	facts, why := in.sql()
	if facts == nil {
		return unknown("%s", why)
	}
	var off []string
	for _, name := range []string{settingLogConnections, settingLogDisconnections} {
		if facts.Settings[name] != settingOn {
			off = append(off, name)
		}
	}
	if len(off) > 0 {
		return notMet("%s is not on", strings.Join(off, " and "))
	}
	return met("connections and disconnections are logged")
}

func encryptionInTransit(in controlInputs) result {
	facts, why := in.sql()
	if facts == nil {
		return unknown("%s", why)
	}
	if facts.Settings["ssl"] != "on" {
		return notMet("ssl is %q, not on", facts.Settings["ssl"])
	}
	if facts.PlainRuleCount > 0 {
		return notMet("%d connection rule(s) can accept a connection without TLS, such as: %s",
			facts.PlainRuleCount, strings.Join(facts.PlainRules, "; "))
	}
	return met("TLS is on and connections without it are refused; the server certificate is generated by the image, " +
		"not issued by a certificate authority, so clients cannot verify the server")
}

func authentication(in controlInputs) result {
	facts, why := in.sql()
	if facts == nil {
		return unknown("%s", why)
	}
	var problems []string
	if got := facts.Settings["password_encryption"]; got != "scram-sha-256" {
		problems = append(problems, fmt.Sprintf("password_encryption is %q, not scram-sha-256", got))
	}
	if facts.NonScramRoles > 0 {
		problems = append(problems, fmt.Sprintf("%d role(s) have a password that is not stored as scram-sha-256", facts.NonScramRoles))
	}
	if facts.WeakRuleCount > 0 {
		problems = append(problems, fmt.Sprintf("%d connection rule(s) need no password, or send it unprotected, such as: %s",
			facts.WeakRuleCount, strings.Join(facts.WeakRules, "; ")))
	}
	if facts.HBAErrors > 0 {
		problems = append(problems, fmt.Sprintf("%d connection rule(s) could not be read", facts.HBAErrors))
	}
	if len(problems) > 0 {
		return notMet("%s", strings.Join(problems, "; "))
	}
	return met("passwords are stored as scram-sha-256 and no network connection rule skips the password")
}

func dataIntegrity(in controlInputs) result {
	facts, why := in.sql()
	if facts == nil {
		return unknown("%s", why)
	}
	if facts.Settings["data_checksums"] != "on" {
		return notMet("data checksums are off; they can only be turned on by rebuilding the cluster or with the database stopped")
	}
	return met("data checksums are on")
}

func auditLogDelivery(in controlInputs) result {
	bucket, why := in.bucket()
	if bucket == nil {
		return unknown("%s", why)
	}
	if len(in.Pods) == 0 {
		return unknown("there are no database pods to ship from")
	}
	at := in.Check.at
	var late, waiting []string
	pods := slices.Clone(in.Pods)
	slices.SortFunc(pods, func(a, b podFact) int { return strings.Compare(a.Name, b.Name) })
	for _, pod := range pods {
		upload, shipped := bucket.Pods[pod.Name]
		if !shipped || upload.Newest == nil {
			if pod.Started.After(at.Add(-auditDeliveryWithin)) {
				waiting = append(waiting, pod.Name)
			} else {
				late = append(late, pod.Name+" has shipped nothing")
			}
			continue
		}
		// Measured against the check, not the clock, so the same report always
		// says the same thing.
		if age := at.Sub(*upload.Newest); age > auditDeliveryWithin {
			late = append(late, fmt.Sprintf("%s last shipped %s before the check", pod.Name, age.Round(time.Minute)))
		}
	}
	switch {
	case len(late) > 0:
		return notMet("%s", strings.Join(late, "; "))
	case len(waiting) > 0:
		return unknown("waiting for the first upload from %s", strings.Join(waiting, ", "))
	}
	return met("%d pod(s) each shipped within %s of the check", len(pods), auditDeliveryWithin)
}

// overlaps reports whether a lifecycle rule's prefix and the audit path can
// match the same objects. Objects are written under "<path>/".
func overlaps(rulePrefix, auditPath string) bool {
	objects := strings.Trim(auditPath, "/") + "/"
	return strings.HasPrefix(objects, rulePrefix) || strings.HasPrefix(rulePrefix, objects)
}

func auditLogRetention(in controlInputs) result {
	bucket, why := in.bucket()
	if bucket == nil {
		return unknown("%s", why)
	}
	days := int(in.Cluster.Spec.Compliance.EffectiveRetentionDays())
	path := in.Cluster.Spec.Compliance.AuditPathFor(in.Cluster.Namespace, in.Cluster.Name)

	if bucket.ObjectLock.Supported && bucket.ObjectLock.Days >= days {
		return met("the bucket's object lock keeps new objects for %d days, at least the %d expected", bucket.ObjectLock.Days, days)
	}
	if !bucket.Lifecycle.Supported {
		return unknown("the store would not say what its lifecycle rules do (%s), and its object lock is not enough on its own",
			firstNonEmpty(bucket.Lifecycle.Error, "not supported"))
	}
	var early []string
	for _, rule := range bucket.Lifecycle.Rules {
		if rule.Enabled && rule.Days > 0 && rule.Days < days && overlaps(rule.Prefix, path) {
			early = append(early, fmt.Sprintf("rule %q expires them after %d days", rule.ID, rule.Days))
		}
	}
	if len(early) > 0 {
		return notMet("audit logs are expected to be kept for %d days, but %s", days, strings.Join(early, "; "))
	}
	protection := "objects are not protected from deletion, as the bucket has no object lock"
	if bucket.ObjectLock.Supported && bucket.ObjectLock.Days > 0 {
		protection = fmt.Sprintf("the bucket's object lock keeps new objects for only %d days", bucket.ObjectLock.Days)
	}
	return met("no lifecycle rule expires audit logs before the %d days expected; %s", days, protection)
}

func networkIsolation(in controlInputs) result {
	switch {
	case !in.Policy.Found:
		return notMet("the cluster's NetworkPolicy does not exist")
	case !in.Policy.Matches:
		return notMet("the cluster's NetworkPolicy is not the one the operator writes; it will be put back")
	case in.Probe.at.IsZero():
		return unknown("the policy is in place; waiting for the first enforcement probe")
	case in.Probe.report == nil:
		return unknown("the enforcement probe's report could not be read")
	case in.Now.Sub(in.Probe.at) > complianceStaleAfter:
		return unknown("the enforcement probe has not reported since %s", in.Probe.at.UTC().Format(time.RFC3339))
	}
	switch in.Probe.report.Result {
	case probeOpen:
		return notMet("a pod with none of the allowed labels could connect to the database, so the policy is not being enforced " +
			"by the cluster's network plugin")
	case probeError:
		return unknown("the enforcement probe could not run: %s", in.Probe.report.Detail)
	}
	// A refusal is only the policy's doing if the database was up to be
	// reached: the compliance check, which the policy admits, reached it.
	if _, why := in.sql(); why != "" {
		return unknown("an unlabelled pod was refused, but the database could not be reached to show that the policy was the reason: %s", why)
	}
	return met("the policy is in place and a pod with none of the allowed labels was refused (%s)", in.Probe.report.Result)
}

func backupRecovery(in controlInputs) result {
	backup := in.Cluster.Spec.Backup
	var notYet, unknowns []string
	if !backup.Enabled {
		return notMet("backups are not enabled")
	}
	if backup.Encryption == nil {
		notYet = append(notYet, "backups are not encrypted")
	}
	if !backup.Verification.Enabled {
		notYet = append(notYet, "restore testing is not enabled")
	}
	for _, condition := range []string{conditionBackupHealthy, conditionRestoreVerified} {
		switch c := meta.FindStatusCondition(in.Cluster.Status.Conditions, condition); {
		case c == nil || c.Status == metav1.ConditionUnknown:
			if condition == conditionBackupHealthy || backup.Verification.Enabled {
				unknowns = append(unknowns, condition+" is not known yet")
			}
		case c.Status == metav1.ConditionFalse:
			notYet = append(notYet, condition+": "+c.Message)
		}
	}
	switch {
	case len(notYet) > 0:
		return notMet("%s", strings.Join(notYet, "; "))
	case len(unknowns) > 0:
		return unknown("%s", strings.Join(unknowns, "; "))
	}
	return met("backups are on, encrypted, healthy, and a restore test has passed")
}

func availability(in controlInputs) result {
	var problems, unknowns []string
	if in.Cluster.Spec.Instances < 2 {
		problems = append(problems, fmt.Sprintf("there is only %d instance, so nothing can take over if it is lost", max(in.Cluster.Spec.Instances, 1)))
	}
	if c := meta.FindStatusCondition(in.Cluster.Status.Conditions, conditionAvailable); c == nil || c.Status != metav1.ConditionTrue {
		problems = append(problems, "there is no ready primary")
	}
	switch c := meta.FindStatusCondition(in.Cluster.Status.Conditions, conditionReplicationHealthy); {
	case c == nil || c.Status == metav1.ConditionUnknown:
		if in.Cluster.Spec.Instances >= 2 {
			unknowns = append(unknowns, "replication is not known yet")
		}
	case c.Status == metav1.ConditionFalse:
		problems = append(problems, "replication: "+c.Message)
	}
	switch {
	case len(problems) > 0:
		return notMet("%s", strings.Join(problems, "; "))
	case len(unknowns) > 0:
		return unknown("%s", strings.Join(unknowns, "; "))
	}
	return met("%d instances, with a ready primary and replication healthy", in.Cluster.Spec.Instances)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// controlsMetCondition rolls the controls up: True when every one is Met,
// False when any is NotMet, Unknown while some are still not known.
func controlsMetCondition(controls []dbv1alpha1.ComplianceControl) metav1.Condition {
	cond := metav1.Condition{Type: dbv1alpha1.ConditionControlsMet}
	var notMetIDs, unknownIDs []string
	for _, c := range controls {
		switch c.Status {
		case dbv1alpha1.ControlNotMet:
			notMetIDs = append(notMetIDs, c.ID)
		case dbv1alpha1.ControlUnknown:
			unknownIDs = append(unknownIDs, c.ID)
		}
	}
	switch {
	case len(notMetIDs) > 0:
		cond.Status = metav1.ConditionFalse
		cond.Reason = reasonControlsNotMet
		cond.Message = fmt.Sprintf("%d of %d controls are not met: %s", len(notMetIDs), len(controls), strings.Join(notMetIDs, ", "))
		if len(unknownIDs) > 0 {
			cond.Message += fmt.Sprintf("; %d not yet known: %s", len(unknownIDs), strings.Join(unknownIDs, ", "))
		}
	case len(unknownIDs) > 0 || len(controls) == 0:
		cond.Status = metav1.ConditionUnknown
		cond.Reason = reasonControlsNotObserve
		cond.Message = fmt.Sprintf("%d of %d controls are not yet known: %s", len(unknownIDs), len(controls), strings.Join(unknownIDs, ", "))
	default:
		cond.Status = metav1.ConditionTrue
		cond.Reason = reasonAllControlsMet
		cond.Message = fmt.Sprintf("all %d controls are met, as last checked", len(controls))
	}
	return cond
}

// countControls tallies controls by result, for the metric.
func countControls(controls []dbv1alpha1.ComplianceControl) map[string]int {
	counts := map[string]int{dbv1alpha1.ControlMet: 0, dbv1alpha1.ControlNotMet: 0, dbv1alpha1.ControlUnknown: 0}
	for _, c := range controls {
		counts[c.Status]++
	}
	return counts
}
