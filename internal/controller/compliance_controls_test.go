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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	dbv1alpha1 "github.com/tusharshrivastava62/postgres-operator/api/v1alpha1"
)

const (
	backupKeySecret = "backup-key"
	soc2Classes     = "read,write,ddl"
)

var checkedAt = time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

// healthySQL is what a correctly configured cluster says about itself.
func healthySQL() *sqlFacts {
	return &sqlFacts{
		OK: true,
		Settings: map[string]string{
			"shared_preload_libraries": spiloPreloadLibraries + ",pgaudit",
			paramPgauditLog:            hipaaClasses,
			settingLogConnections:      "on",
			settingLogDisconnections:   "on",
			"ssl":                      "on",
			"password_encryption":      "scram-sha-256",
			"data_checksums":           "on",
		},
	}
}

func healthyBucket() *bucketFacts {
	newest := checkedAt.Add(-3 * time.Minute)
	return &bucketFacts{
		OK: true,
		Pods: map[string]podUpload{
			"orders-0": {Newest: &newest, Chunks: 4},
			"orders-1": {Newest: &newest, Chunks: 4},
		},
		Lifecycle:  lifecycleFacts{Supported: true},
		ObjectLock: objectLockFacts{Supported: true},
	}
}

// inputs is a cluster in which every control holds, at the moment of a check.
func inputs() controlInputs {
	c := withCompliance(plainCluster(), dbv1alpha1.StandardHIPAA)
	c.Name, c.Namespace = sourceName, sourceNamespace
	c.Spec.Instances = 2
	c.Spec.Backup = backupCluster().Spec.Backup
	c.Spec.Backup.Verification = verifySpec
	c.Spec.Backup.Encryption = &dbv1alpha1.BackupEncryption{KeySecret: backupKeySecret}
	c.Status.Conditions = []metav1.Condition{
		{Type: conditionAvailable, Status: metav1.ConditionTrue},
		{Type: conditionReplicationHealthy, Status: metav1.ConditionTrue},
		{Type: conditionBackupHealthy, Status: metav1.ConditionTrue},
		{Type: conditionRestoreVerified, Status: metav1.ConditionTrue},
	}
	preset, _ := dbv1alpha1.PresetFor(dbv1alpha1.StandardHIPAA)
	started := checkedAt.Add(-time.Hour)
	return controlInputs{
		Cluster: c,
		Preset:  preset,
		Check:   checked[complianceReport]{report: &complianceReport{SQL: healthySQL(), Bucket: healthyBucket()}, at: checkedAt},
		Probe:   checked[probeReport]{report: &probeReport{Result: "timeout"}, at: checkedAt},
		Policy:  policyState{Found: true, Matches: true},
		Pods:    []podFact{{Name: "orders-0", Started: started}, {Name: "orders-1", Started: started}},
		Now:     checkedAt.Add(time.Minute),
	}
}

func resultOf(id string, in controlInputs) dbv1alpha1.ComplianceControl {
	GinkgoHelper()
	for _, c := range evaluateControls(in) {
		if c.ID == id {
			return c
		}
	}
	Fail("no control " + id)
	return dbv1alpha1.ComplianceControl{}
}

var _ = Describe("The compliance controls", func() {
	It("are all met for a correctly configured cluster", func() {
		for _, c := range evaluateControls(inputs()) {
			Expect(c.Status).To(Equal(dbv1alpha1.ControlMet), "%s: %s", c.ID, c.Evidence)
			Expect(c.Evidence).NotTo(BeEmpty(), c.ID)
		}
	})

	It("are the ones the preset asks for, in its order", func() {
		in := inputs()
		got := make([]string, 0, len(in.Preset.Controls))
		for _, c := range evaluateControls(in) {
			got = append(got, c.ID)
		}
		Expect(got).To(Equal(in.Preset.Controls))
	})

	It("say they are not checked, instead of passing, for a control the code has no check for", func() {
		in := inputs()
		in.Preset.Controls = []string{"something-new"}
		c := evaluateControls(in)[0]
		Expect(c.Status).To(Equal(dbv1alpha1.ControlUnknown))
	})

	It("give the same answer for the same facts, however much later they are read", func() {
		in := inputs()
		before := evaluateControls(in)
		in.Now = in.Now.Add(20 * time.Minute)
		Expect(evaluateControls(in)).To(Equal(before), "no clock reading may end up in the evidence")
	})

	Describe("a check that has not reported, or cannot be believed", func() {
		sqlControls := []string{
			dbv1alpha1.ControlAuditLogging, dbv1alpha1.ControlConnectionLogging, dbv1alpha1.ControlEncryptionInTransit,
			dbv1alpha1.ControlAuthentication, dbv1alpha1.ControlDataIntegrity,
			dbv1alpha1.ControlAuditLogDelivery, dbv1alpha1.ControlAuditLogRetention,
		}
		It("leaves every control that depends on it Unknown while it has not run yet", func() {
			in := inputs()
			in.Check = checked[complianceReport]{}
			for _, id := range sqlControls {
				c := resultOf(id, in)
				Expect(c.Status).To(Equal(dbv1alpha1.ControlUnknown), id)
				Expect(c.Evidence).To(ContainSubstring("waiting for the first compliance check"), id)
			}
		})

		It("leaves them Unknown, and says since when, when it has gone quiet for too long", func() {
			in := inputs()
			in.Now = checkedAt.Add(complianceStaleAfter + time.Minute)
			for _, id := range sqlControls {
				c := resultOf(id, in)
				Expect(c.Status).To(Equal(dbv1alpha1.ControlUnknown), id)
				Expect(c.Evidence).To(ContainSubstring("has not reported since 2026-10-19T12:00:00Z"), id)
			}
		})

		It("is still believed just inside the limit", func() {
			in := inputs()
			in.Now = checkedAt.Add(complianceStaleAfter - time.Second)
			Expect(resultOf(dbv1alpha1.ControlAuditLogging, in).Status).To(Equal(dbv1alpha1.ControlMet))
		})

		It("leaves them Unknown when its report could not be read", func() {
			in := inputs()
			in.Check.report = nil
			Expect(resultOf(dbv1alpha1.ControlDataIntegrity, in).Evidence).To(ContainSubstring("could not be read"))
		})

		It("leaves the database's controls Unknown, with the reason, when only the database could not be read", func() {
			in := inputs()
			in.Check.report.SQL = &sqlFacts{OK: false, Error: "psql timed out after 60s"}
			c := resultOf(dbv1alpha1.ControlAuditLogging, in)
			Expect(c.Status).To(Equal(dbv1alpha1.ControlUnknown))
			Expect(c.Evidence).To(ContainSubstring("psql timed out after 60s"))
			Expect(resultOf(dbv1alpha1.ControlAuditLogDelivery, in).Status).To(Equal(dbv1alpha1.ControlMet), "the bucket was read")
		})

		It("leaves the bucket's controls Unknown, with the reason, when only the bucket could not be read", func() {
			in := inputs()
			in.Check.report.Bucket = &bucketFacts{OK: false, Error: "EndpointConnectionError"}
			c := resultOf(dbv1alpha1.ControlAuditLogDelivery, in)
			Expect(c.Status).To(Equal(dbv1alpha1.ControlUnknown))
			Expect(c.Evidence).To(ContainSubstring("EndpointConnectionError"))
			Expect(resultOf(dbv1alpha1.ControlAuditLogRetention, in).Status).To(Equal(dbv1alpha1.ControlUnknown))
			Expect(resultOf(dbv1alpha1.ControlAuditLogging, in).Status).To(Equal(dbv1alpha1.ControlMet), "the database was read")
		})
	})

	Describe("audit-logging", func() {
		It("is not met when pgaudit is not loaded, which is what a restart that has not happened looks like", func() {
			in := inputs()
			in.Check.report.SQL.Settings["shared_preload_libraries"] = spiloPreloadLibraries
			c := resultOf(dbv1alpha1.ControlAuditLogging, in)
			Expect(c.Status).To(Equal(dbv1alpha1.ControlNotMet))
			Expect(c.Evidence).To(ContainSubstring("not loaded"))
			Expect(c.Evidence).To(ContainSubstring("restart may still be pending"))
		})

		It("is not met when pgaudit.log is not set", func() {
			in := inputs()
			delete(in.Check.report.SQL.Settings, paramPgauditLog)
			Expect(resultOf(dbv1alpha1.ControlAuditLogging, in).Evidence).To(ContainSubstring("pgaudit.log is not set"))
			in.Check.report.SQL.Settings[paramPgauditLog] = ""
			Expect(resultOf(dbv1alpha1.ControlAuditLogging, in).Status).To(Equal(dbv1alpha1.ControlNotMet))
		})

		It("is not met when it logs the wrong statements, naming both", func() {
			in := inputs()
			in.Check.report.SQL.Settings[paramPgauditLog] = "write"
			c := resultOf(dbv1alpha1.ControlAuditLogging, in)
			Expect(c.Status).To(Equal(dbv1alpha1.ControlNotMet))
			Expect(c.Evidence).To(And(ContainSubstring("write"), ContainSubstring(hipaaClasses)))
		})

		It("is met whatever order the classes are listed in", func() {
			in := inputs()
			in.Check.report.SQL.Settings[paramPgauditLog] = "role, ddl,write ,read"
			Expect(resultOf(dbv1alpha1.ControlAuditLogging, in).Status).To(Equal(dbv1alpha1.ControlMet))
		})

		It("asks for the classes of the standard in force", func() {
			in := inputs()
			soc2, _ := dbv1alpha1.PresetFor(dbv1alpha1.StandardSOC2)
			in.Preset = soc2
			Expect(resultOf(dbv1alpha1.ControlAuditLogging, in).Status).To(Equal(dbv1alpha1.ControlNotMet), "role is one class too many")
			in.Check.report.SQL.Settings[paramPgauditLog] = soc2Classes
			Expect(resultOf(dbv1alpha1.ControlAuditLogging, in).Status).To(Equal(dbv1alpha1.ControlMet))
		})
	})

	Describe("connection-logging", func() {
		It("is not met when either direction is off, and says which", func() {
			in := inputs()
			in.Check.report.SQL.Settings[settingLogDisconnections] = valueOff
			c := resultOf(dbv1alpha1.ControlConnectionLogging, in)
			Expect(c.Status).To(Equal(dbv1alpha1.ControlNotMet))
			Expect(c.Evidence).To(Equal("log_disconnections is not on"))
			in.Check.report.SQL.Settings[settingLogConnections] = valueOff
			Expect(resultOf(dbv1alpha1.ControlConnectionLogging, in).Evidence).To(Equal("log_connections and log_disconnections is not on"))
		})
	})

	Describe("encryption-in-transit", func() {
		It("is met, and says plainly that the certificate is not one a client can verify", func() {
			c := resultOf(dbv1alpha1.ControlEncryptionInTransit, inputs())
			Expect(c.Status).To(Equal(dbv1alpha1.ControlMet))
			Expect(c.Evidence).To(ContainSubstring("not issued by a certificate authority"))
		})

		It("is not met when ssl is off", func() {
			in := inputs()
			in.Check.report.SQL.Settings["ssl"] = valueOff
			Expect(resultOf(dbv1alpha1.ControlEncryptionInTransit, in).Status).To(Equal(dbv1alpha1.ControlNotMet))
		})

		It("is not met when a rule can accept a connection without TLS, naming it", func() {
			in := inputs()
			in.Check.report.SQL.PlainRuleCount = 2
			in.Check.report.SQL.PlainRules = []string{"host all all 0.0.0.0/0 md5", "hostnossl all all all trust"}
			c := resultOf(dbv1alpha1.ControlEncryptionInTransit, in)
			Expect(c.Status).To(Equal(dbv1alpha1.ControlNotMet))
			Expect(c.Evidence).To(And(ContainSubstring("2 connection rule(s)"), ContainSubstring("host all all 0.0.0.0/0 md5")))
		})
	})

	Describe("authentication", func() {
		It("is not met when new passwords are not hashed with scram", func() {
			in := inputs()
			in.Check.report.SQL.Settings["password_encryption"] = "md5"
			Expect(resultOf(dbv1alpha1.ControlAuthentication, in).Evidence).To(ContainSubstring(`"md5", not scram-sha-256`))
		})

		It("is not met when a role still has an old-style password hash", func() {
			in := inputs()
			in.Check.report.SQL.NonScramRoles = 2
			c := resultOf(dbv1alpha1.ControlAuthentication, in)
			Expect(c.Status).To(Equal(dbv1alpha1.ControlNotMet))
			Expect(c.Evidence).To(ContainSubstring("2 role(s)"))
		})

		It("is not met when a connection rule needs no password, naming it", func() {
			in := inputs()
			in.Check.report.SQL.WeakRuleCount = 1
			in.Check.report.SQL.WeakRules = []string{"hostssl all all 10.0.0.0/8 trust"}
			Expect(resultOf(dbv1alpha1.ControlAuthentication, in).Evidence).To(ContainSubstring("hostssl all all 10.0.0.0/8 trust"))
		})

		It("is not met when a connection rule could not be read", func() {
			in := inputs()
			in.Check.report.SQL.HBAErrors = 1
			Expect(resultOf(dbv1alpha1.ControlAuthentication, in).Status).To(Equal(dbv1alpha1.ControlNotMet))
		})

		It("reports every problem at once", func() {
			in := inputs()
			in.Check.report.SQL.Settings["password_encryption"] = "md5"
			in.Check.report.SQL.NonScramRoles = 1
			in.Check.report.SQL.WeakRuleCount = 1
			in.Check.report.SQL.WeakRules = []string{"x"}
			Expect(resultOf(dbv1alpha1.ControlAuthentication, in).Evidence).To(MatchRegexp(`md5.*; 1 role.*; 1 connection rule`))
		})
	})

	Describe("data-integrity", func() {
		It("is not met when checksums are off, and says they cannot just be turned on", func() {
			in := inputs()
			in.Check.report.SQL.Settings["data_checksums"] = valueOff
			c := resultOf(dbv1alpha1.ControlDataIntegrity, in)
			Expect(c.Status).To(Equal(dbv1alpha1.ControlNotMet))
			Expect(c.Evidence).To(ContainSubstring("database stopped"))
		})
	})

	Describe("audit-log-delivery", func() {
		It("is met when every pod shipped within fifteen minutes of the check", func() {
			c := resultOf(dbv1alpha1.ControlAuditLogDelivery, inputs())
			Expect(c.Status).To(Equal(dbv1alpha1.ControlMet))
			Expect(c.Evidence).To(ContainSubstring("2 pod(s)"))
		})

		It("is met exactly at the limit and not met just past it", func() {
			in := inputs()
			edge := checkedAt.Add(-auditDeliveryWithin)
			in.Check.report.Bucket.Pods["orders-0"] = podUpload{Newest: &edge}
			Expect(resultOf(dbv1alpha1.ControlAuditLogDelivery, in).Status).To(Equal(dbv1alpha1.ControlMet))
			late := edge.Add(-time.Second)
			in.Check.report.Bucket.Pods["orders-0"] = podUpload{Newest: &late}
			Expect(resultOf(dbv1alpha1.ControlAuditLogDelivery, in).Status).To(Equal(dbv1alpha1.ControlNotMet))
		})

		It("is not met, naming the pod, when one has stopped shipping", func() {
			in := inputs()
			old := checkedAt.Add(-47 * time.Minute)
			in.Check.report.Bucket.Pods["orders-1"] = podUpload{Newest: &old}
			c := resultOf(dbv1alpha1.ControlAuditLogDelivery, in)
			Expect(c.Status).To(Equal(dbv1alpha1.ControlNotMet))
			Expect(c.Evidence).To(Equal("orders-1 last shipped 47m0s before the check"))
		})

		It("is not met when a pod that has been up a while has shipped nothing at all", func() {
			in := inputs()
			delete(in.Check.report.Bucket.Pods, "orders-1")
			c := resultOf(dbv1alpha1.ControlAuditLogDelivery, in)
			Expect(c.Status).To(Equal(dbv1alpha1.ControlNotMet))
			Expect(c.Evidence).To(Equal("orders-1 has shipped nothing"))
		})

		It("is not met when a pod's listing has no upload time", func() {
			in := inputs()
			in.Check.report.Bucket.Pods["orders-1"] = podUpload{}
			Expect(resultOf(dbv1alpha1.ControlAuditLogDelivery, in).Status).To(Equal(dbv1alpha1.ControlNotMet))
		})

		It("is Unknown, not failing, while a pod that has only just started has not had its first upload", func() {
			in := inputs()
			delete(in.Check.report.Bucket.Pods, "orders-1")
			in.Pods[1].Started = checkedAt.Add(-2 * time.Minute)
			c := resultOf(dbv1alpha1.ControlAuditLogDelivery, in)
			Expect(c.Status).To(Equal(dbv1alpha1.ControlUnknown))
			Expect(c.Evidence).To(ContainSubstring("waiting for the first upload from orders-1"))
		})

		It("is not met by a late pod even while another is only waiting", func() {
			in := inputs()
			old := checkedAt.Add(-time.Hour)
			in.Check.report.Bucket.Pods["orders-0"] = podUpload{Newest: &old}
			delete(in.Check.report.Bucket.Pods, "orders-1")
			in.Pods[1].Started = checkedAt
			Expect(resultOf(dbv1alpha1.ControlAuditLogDelivery, in).Status).To(Equal(dbv1alpha1.ControlNotMet))
		})

		It("ignores uploads from pods that no longer exist", func() {
			in := inputs()
			old := checkedAt.Add(-72 * time.Hour)
			in.Check.report.Bucket.Pods["orders-2"] = podUpload{Newest: &old}
			Expect(resultOf(dbv1alpha1.ControlAuditLogDelivery, in).Status).To(Equal(dbv1alpha1.ControlMet))
		})

		It("is Unknown when there are no pods to ship from", func() {
			in := inputs()
			in.Pods = nil
			Expect(resultOf(dbv1alpha1.ControlAuditLogDelivery, in).Status).To(Equal(dbv1alpha1.ControlUnknown))
		})
	})

	Describe("audit-log-retention", func() {
		rule := func(id, prefix string, days int, enabled bool) lifecycleRule {
			return lifecycleRule{ID: id, Enabled: enabled, Prefix: prefix, Days: days}
		}

		It("is met when no lifecycle rule expires the logs, and says they are not protected from deletion", func() {
			c := resultOf(dbv1alpha1.ControlAuditLogRetention, inputs())
			Expect(c.Status).To(Equal(dbv1alpha1.ControlMet))
			Expect(c.Evidence).To(And(ContainSubstring("2190 days"), ContainSubstring("not protected from deletion")))
		})

		It("is met by an object lock that holds objects long enough", func() {
			in := inputs()
			in.Check.report.Bucket.ObjectLock = objectLockFacts{Supported: true, Days: 2555, Mode: "COMPLIANCE"}
			c := resultOf(dbv1alpha1.ControlAuditLogRetention, in)
			Expect(c.Status).To(Equal(dbv1alpha1.ControlMet))
			Expect(c.Evidence).To(ContainSubstring("object lock keeps new objects for 2555 days"))
		})

		It("is met by an object lock even when the lifecycle rules could not be read", func() {
			in := inputs()
			in.Check.report.Bucket.Lifecycle = lifecycleFacts{Supported: false, Error: "NotImplemented"}
			in.Check.report.Bucket.ObjectLock = objectLockFacts{Supported: true, Days: 3000}
			Expect(resultOf(dbv1alpha1.ControlAuditLogRetention, in).Status).To(Equal(dbv1alpha1.ControlMet))
		})

		It("is not met when a rule expires the logs too soon, naming it", func() {
			in := inputs()
			in.Check.report.Bucket.Lifecycle.Rules = []lifecycleRule{rule("tidy-up", "audit/", 30, true)}
			c := resultOf(dbv1alpha1.ControlAuditLogRetention, in)
			Expect(c.Status).To(Equal(dbv1alpha1.ControlNotMet))
			Expect(c.Evidence).To(And(ContainSubstring(`"tidy-up"`), ContainSubstring("30 days"), ContainSubstring("2190")))
		})

		DescribeTable("a rule counts when it can match the audit logs, and not otherwise",
			func(prefix string, enabled bool, days int, wantNotMet bool) {
				in := inputs()
				in.Check.report.Bucket.Lifecycle.Rules = []lifecycleRule{rule("r", prefix, days, enabled)}
				got := resultOf(dbv1alpha1.ControlAuditLogRetention, in).Status
				if wantNotMet {
					Expect(got).To(Equal(dbv1alpha1.ControlNotMet))
				} else {
					Expect(got).To(Equal(dbv1alpha1.ControlMet))
				}
			},
			Entry("a rule for the whole bucket", "", true, 30, true),
			Entry("a rule for exactly the audit path", "audit/prod/orders/", true, 30, true),
			Entry("a rule for a parent of it", "audit/", true, 30, true),
			Entry("a rule narrower than it, under it", "audit/prod/orders/orders-0/", true, 30, true),
			Entry("a rule for the backups next door", "prod/orders/", true, 30, false),
			Entry("a rule for another prefix", "tmp/", true, 1, false),
			Entry("a similar name that is not a parent", "audit/prod/orders-2", true, 30, false),
			Entry("a rule that is switched off", "audit/", false, 30, false),
			Entry("a rule that keeps them long enough", "audit/", true, 2190, false),
			Entry("a rule that keeps them longer", "audit/", true, 3650, false),
			Entry("a rule with no expiry in days", "audit/", true, 0, false),
		)

		It("is Unknown when the store cannot say what its lifecycle rules are", func() {
			in := inputs()
			in.Check.report.Bucket.Lifecycle = lifecycleFacts{Supported: false, Error: "NotImplemented"}
			c := resultOf(dbv1alpha1.ControlAuditLogRetention, in)
			Expect(c.Status).To(Equal(dbv1alpha1.ControlUnknown))
			Expect(c.Evidence).To(ContainSubstring("NotImplemented"))
		})

		It("says an object lock that is too short is too short", func() {
			in := inputs()
			in.Check.report.Bucket.ObjectLock = objectLockFacts{Supported: true, Days: 30}
			Expect(resultOf(dbv1alpha1.ControlAuditLogRetention, in).Evidence).To(ContainSubstring("only 30 days"))
		})

		It("follows retentionDays, not the preset's default, when it is set", func() {
			in := inputs()
			in.Cluster.Spec.Compliance.AuditLogs.RetentionDays = 20
			in.Check.report.Bucket.Lifecycle.Rules = []lifecycleRule{rule("r", "audit/", 30, true)}
			Expect(resultOf(dbv1alpha1.ControlAuditLogRetention, in).Status).To(Equal(dbv1alpha1.ControlMet))
		})

		It("looks under the destination's own path when it sets one", func() {
			in := inputs()
			in.Cluster.Spec.Compliance.AuditLogs.Destination.Path = "team/audit"
			in.Check.report.Bucket.Lifecycle.Rules = []lifecycleRule{rule("r", "audit/", 30, true)}
			Expect(resultOf(dbv1alpha1.ControlAuditLogRetention, in).Status).To(Equal(dbv1alpha1.ControlMet), "audit/ is not a parent of team/audit")
		})
	})

	Describe("network-isolation", func() {
		It("is met when the policy is in place and a stranger was turned away", func() {
			c := resultOf(dbv1alpha1.ControlNetworkIsolation, inputs())
			Expect(c.Status).To(Equal(dbv1alpha1.ControlMet))
			Expect(c.Evidence).To(ContainSubstring("(timeout)"))
		})

		It("is met when the stranger was refused outright, which some network plugins do instead of dropping", func() {
			for _, answer := range []string{"refused", "unreachable"} {
				in := inputs()
				in.Probe.report.Result = answer
				Expect(resultOf(dbv1alpha1.ControlNetworkIsolation, in).Status).To(Equal(dbv1alpha1.ControlMet), answer)
			}
		})

		It("is not met when the policy does not exist", func() {
			in := inputs()
			in.Policy = policyState{}
			Expect(resultOf(dbv1alpha1.ControlNetworkIsolation, in).Evidence).To(ContainSubstring("does not exist"))
		})

		It("is not met when the policy is not the one the operator writes", func() {
			in := inputs()
			in.Policy.Matches = false
			c := resultOf(dbv1alpha1.ControlNetworkIsolation, in)
			Expect(c.Status).To(Equal(dbv1alpha1.ControlNotMet))
			Expect(c.Evidence).To(ContainSubstring("will be put back"))
		})

		It("is not met when a stranger got through, which is the network plugin ignoring the policy", func() {
			in := inputs()
			in.Probe.report.Result = probeOpen
			c := resultOf(dbv1alpha1.ControlNetworkIsolation, in)
			Expect(c.Status).To(Equal(dbv1alpha1.ControlNotMet))
			Expect(c.Evidence).To(ContainSubstring("not being enforced"))
		})

		It("is Unknown until the probe has run, though the policy is there", func() {
			in := inputs()
			in.Probe = checked[probeReport]{}
			c := resultOf(dbv1alpha1.ControlNetworkIsolation, in)
			Expect(c.Status).To(Equal(dbv1alpha1.ControlUnknown))
			Expect(c.Evidence).To(ContainSubstring("waiting for the first enforcement probe"))
		})

		It("is Unknown when the probe's report is old, or unreadable, or says it could not run", func() {
			in := inputs()
			in.Now = checkedAt.Add(complianceStaleAfter + time.Minute)
			Expect(resultOf(dbv1alpha1.ControlNetworkIsolation, in).Status).To(Equal(dbv1alpha1.ControlUnknown))
			in = inputs()
			in.Probe.report = nil
			Expect(resultOf(dbv1alpha1.ControlNetworkIsolation, in).Status).To(Equal(dbv1alpha1.ControlUnknown))
			in = inputs()
			in.Probe.report.Result = probeError
			in.Probe.report.Detail = "could not resolve orders"
			c := resultOf(dbv1alpha1.ControlNetworkIsolation, in)
			Expect(c.Status).To(Equal(dbv1alpha1.ControlUnknown))
			Expect(c.Evidence).To(ContainSubstring("could not resolve orders"))
		})

		It("is Unknown, not met, when the stranger was refused but the database could not be reached either: a database that is down looks the same", func() {
			in := inputs()
			in.Check.report.SQL = &sqlFacts{OK: false, Error: "connection refused"}
			c := resultOf(dbv1alpha1.ControlNetworkIsolation, in)
			Expect(c.Status).To(Equal(dbv1alpha1.ControlUnknown))
			Expect(c.Evidence).To(ContainSubstring("could not be reached to show that the policy was the reason"))
		})

		It("is still not met when a stranger got through, whether or not the database could be read", func() {
			in := inputs()
			in.Probe.report.Result = probeOpen
			in.Check.report.SQL = &sqlFacts{OK: false}
			Expect(resultOf(dbv1alpha1.ControlNetworkIsolation, in).Status).To(Equal(dbv1alpha1.ControlNotMet))
		})
	})

	Describe("backup-recovery", func() {
		It("is not met when backups are off", func() {
			in := inputs()
			in.Cluster.Spec.Backup = dbv1alpha1.BackupSpec{}
			Expect(resultOf(dbv1alpha1.ControlBackupRecovery, in).Evidence).To(Equal("backups are not enabled"))
		})

		It("is not met when backups are unencrypted or restores are not tested, saying both", func() {
			in := inputs()
			in.Cluster.Spec.Backup.Encryption = nil
			in.Cluster.Spec.Backup.Verification = dbv1alpha1.BackupVerification{}
			c := resultOf(dbv1alpha1.ControlBackupRecovery, in)
			Expect(c.Status).To(Equal(dbv1alpha1.ControlNotMet))
			Expect(c.Evidence).To(And(ContainSubstring("not encrypted"), ContainSubstring("restore testing is not enabled")))
		})

		It("is not met when backups are unhealthy or the last restore test failed, with the reason", func() {
			in := inputs()
			meta.SetStatusCondition(&in.Cluster.Status.Conditions, metav1.Condition{
				Type: conditionBackupHealthy, Status: metav1.ConditionFalse, Reason: "BackupOverdue", Message: "the newest base backup is too old"})
			meta.SetStatusCondition(&in.Cluster.Status.Conditions, metav1.Condition{
				Type: conditionRestoreVerified, Status: metav1.ConditionFalse, Reason: "RestoreFailed", Message: "the last restore test failed: no space"})
			c := resultOf(dbv1alpha1.ControlBackupRecovery, in)
			Expect(c.Status).To(Equal(dbv1alpha1.ControlNotMet))
			Expect(c.Evidence).To(And(ContainSubstring("too old"), ContainSubstring("no space")))
		})

		It("is Unknown while the backup conditions have not been worked out", func() {
			in := inputs()
			in.Cluster.Status.Conditions = nil
			Expect(resultOf(dbv1alpha1.ControlBackupRecovery, in).Status).To(Equal(dbv1alpha1.ControlUnknown))
			in = inputs()
			meta.SetStatusCondition(&in.Cluster.Status.Conditions, metav1.Condition{
				Type: conditionRestoreVerified, Status: metav1.ConditionUnknown, Reason: "NotRunYet"})
			Expect(resultOf(dbv1alpha1.ControlBackupRecovery, in).Status).To(Equal(dbv1alpha1.ControlUnknown))
		})
	})

	Describe("availability", func() {
		It("is not met with a single instance", func() {
			in := inputs()
			in.Cluster.Spec.Instances = 1
			c := resultOf(dbv1alpha1.ControlAvailability, in)
			Expect(c.Status).To(Equal(dbv1alpha1.ControlNotMet))
			Expect(c.Evidence).To(ContainSubstring("only 1 instance"))
		})

		It("counts an instance count that was never set as one", func() {
			in := inputs()
			in.Cluster.Spec.Instances = 0
			Expect(resultOf(dbv1alpha1.ControlAvailability, in).Evidence).To(ContainSubstring("only 1 instance"))
		})

		It("is not met without a ready primary", func() {
			in := inputs()
			meta.SetStatusCondition(&in.Cluster.Status.Conditions, metav1.Condition{Type: conditionAvailable, Status: metav1.ConditionFalse})
			Expect(resultOf(dbv1alpha1.ControlAvailability, in).Evidence).To(ContainSubstring("no ready primary"))
		})

		It("is not met when replication is unhealthy, with the reason", func() {
			in := inputs()
			meta.SetStatusCondition(&in.Cluster.Status.Conditions, metav1.Condition{
				Type: conditionReplicationHealthy, Status: metav1.ConditionFalse, Message: "orders-1 is not streaming"})
			Expect(resultOf(dbv1alpha1.ControlAvailability, in).Evidence).To(ContainSubstring("orders-1 is not streaming"))
		})

		It("is Unknown while replication has not been worked out, but only when there is a replica to work it out for", func() {
			in := inputs()
			meta.SetStatusCondition(&in.Cluster.Status.Conditions, metav1.Condition{Type: conditionReplicationHealthy, Status: metav1.ConditionUnknown})
			Expect(resultOf(dbv1alpha1.ControlAvailability, in).Status).To(Equal(dbv1alpha1.ControlUnknown))
		})
	})
})

var _ = Describe("The ControlsMet condition", func() {
	control := func(id, status string) dbv1alpha1.ComplianceControl {
		return dbv1alpha1.ComplianceControl{ID: id, Status: status}
	}
	It("is true only when every control is met", func() {
		c := controlsMetCondition([]dbv1alpha1.ComplianceControl{control("a", "Met"), control("b", "Met")})
		Expect(c.Type).To(Equal("ControlsMet"))
		Expect(c.Status).To(Equal(metav1.ConditionTrue))
		Expect(c.Reason).To(Equal("AllControlsMet"))
		Expect(c.Message).To(Equal("all 2 controls are met, as last checked"))
	})

	It("is false when any control is not met, and names them", func() {
		c := controlsMetCondition([]dbv1alpha1.ComplianceControl{control("a", "Met"), control("b", "NotMet"), control("c", "NotMet")})
		Expect(c.Status).To(Equal(metav1.ConditionFalse))
		Expect(c.Reason).To(Equal("ControlsNotMet"))
		Expect(c.Message).To(Equal("2 of 3 controls are not met: b, c"))
	})

	It("is false, and also says what is not known yet, when some are not met and some unknown", func() {
		c := controlsMetCondition([]dbv1alpha1.ComplianceControl{control("a", "NotMet"), control("b", "Unknown")})
		Expect(c.Status).To(Equal(metav1.ConditionFalse))
		Expect(c.Message).To(ContainSubstring("1 not yet known: b"))
	})

	It("is Unknown, never true, while any control is not known", func() {
		c := controlsMetCondition([]dbv1alpha1.ComplianceControl{control("a", "Met"), control("b", "Unknown")})
		Expect(c.Status).To(Equal(metav1.ConditionUnknown))
		Expect(c.Reason).To(Equal("ControlsNotObserved"))
		Expect(c.Message).To(Equal("1 of 2 controls are not yet known: b"))
	})

	It("is Unknown when there are no controls at all, since nothing has been shown", func() {
		Expect(controlsMetCondition(nil).Status).To(Equal(metav1.ConditionUnknown))
	})

	It("tallies the controls by result for the metric", func() {
		counts := countControls([]dbv1alpha1.ComplianceControl{control("a", "Met"), control("b", "Met"), control("c", "NotMet")})
		Expect(counts).To(Equal(map[string]int{"Met": 2, "NotMet": 1, "Unknown": 0}))
	})
})

var _ = Describe("Writing the controls into status", func() {
	evaluation := func(at time.Time, statuses ...string) *complianceEvaluation {
		controls := make([]dbv1alpha1.ComplianceControl, len(statuses))
		for i, s := range statuses {
			controls[i] = dbv1alpha1.ComplianceControl{ID: string(rune('a' + i)), Status: s, Evidence: "seen " + s}
		}
		return &complianceEvaluation{standard: dbv1alpha1.StandardHIPAA, checkedAt: at, controls: controls}
	}
	now := checkedAt.Add(5 * time.Minute)

	It("stores the standard, when it was checked, each control, and the condition", func() {
		c := plainCluster()
		Expect(applyComplianceStatus(c, evaluation(checkedAt, "Met", "NotMet"), now)).To(BeTrue())
		Expect(c.Status.Compliance.Standard).To(Equal("hipaa"))
		Expect(c.Status.Compliance.LastChecked.Time).To(Equal(checkedAt))
		Expect(c.Status.Compliance.Controls).To(HaveLen(2))
		Expect(c.Status.Compliance.Controls[1].ObservedAt.Time).To(Equal(checkedAt))
		Expect(meta.FindStatusCondition(c.Status.Conditions, "ControlsMet").Status).To(Equal(metav1.ConditionFalse))
	})

	It("writes nothing the second time when nothing has changed, so the status is not rewritten at every reconcile", func() {
		c := plainCluster()
		applyComplianceStatus(c, evaluation(checkedAt, "Met", "Met"), now)
		Expect(applyComplianceStatus(c, evaluation(checkedAt, "Met", "Met"), now.Add(time.Hour))).To(BeFalse())
	})

	It("is stamped again when the jobs report again, even with the same answers", func() {
		c := plainCluster()
		applyComplianceStatus(c, evaluation(checkedAt, "Met"), now)
		Expect(applyComplianceStatus(c, evaluation(checkedAt.Add(10*time.Minute), "Met"), now)).To(BeTrue())
		Expect(c.Status.Compliance.Controls[0].ObservedAt.Time).To(Equal(checkedAt.Add(10 * time.Minute)))
	})

	It("keeps a control's time, before any job has reported, while its result is the same, and moves it when it changes", func() {
		c := plainCluster()
		applyComplianceStatus(c, evaluation(time.Time{}, "Unknown"), now)
		first := c.Status.Compliance.Controls[0].ObservedAt.Time
		Expect(first).To(Equal(now))
		Expect(applyComplianceStatus(c, evaluation(time.Time{}, "Unknown"), now.Add(time.Hour))).To(BeFalse())
		Expect(applyComplianceStatus(c, evaluation(time.Time{}, "Met"), now.Add(2*time.Hour))).To(BeTrue())
		Expect(c.Status.Compliance.Controls[0].ObservedAt.Time).To(Equal(now.Add(2 * time.Hour)))
	})

	It("has no check time before the jobs have reported", func() {
		c := plainCluster()
		applyComplianceStatus(c, evaluation(time.Time{}, "Unknown"), now)
		Expect(c.Status.Compliance.LastChecked).To(BeNil())
	})

	It("clears what a standard left behind, the condition included, when the compliance settings go", func() {
		c := plainCluster()
		applyComplianceStatus(c, evaluation(checkedAt, "Met"), now)
		Expect(applyComplianceStatus(c, nil, now)).To(BeTrue())
		Expect(c.Status.Compliance).To(Equal(dbv1alpha1.ComplianceStatus{}))
		Expect(meta.FindStatusCondition(c.Status.Conditions, "ControlsMet")).To(BeNil())
		Expect(applyComplianceStatus(c, nil, now)).To(BeFalse(), "and there is nothing to clear the second time")
	})

	It("keeps other conditions when it removes its own", func() {
		c := plainCluster()
		meta.SetStatusCondition(&c.Status.Conditions, metav1.Condition{Type: conditionAvailable, Status: metav1.ConditionTrue, Reason: "x"})
		applyComplianceStatus(c, evaluation(checkedAt, "Met"), now)
		applyComplianceStatus(c, nil, now)
		Expect(c.Status.Conditions).To(HaveLen(1))
	})

	It("does not mix up the pointer a control's time was copied from", func() {
		c := plainCluster()
		e := evaluation(checkedAt, "Met")
		e.controls[0].ObservedAt = ptr.To(metav1.NewTime(checkedAt.Add(-time.Hour)))
		applyComplianceStatus(c, e, now)
		Expect(c.Status.Compliance.Controls[0].ObservedAt.Time).To(Equal(checkedAt))
	})
})
