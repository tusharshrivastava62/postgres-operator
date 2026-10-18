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
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus/testutil"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	dbv1alpha1 "github.com/tusharshrivastava62/postgres-operator/api/v1alpha1"
)

// previousBackup is a backup name already stored in status when a report arrives.
const previousBackup = "old"

var evalNow = time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)

func freshReport() *backupReport {
	r := &backupReport{}
	r.Backups = &struct {
		Count  int        `json:"count"`
		Newest *backupRef `json:"newest"`
		Oldest *backupRef `json:"oldest"`
	}{Count: 3,
		Newest: &backupRef{Name: "base_0A", Time: evalNow.Add(-2 * time.Hour), SizeBytes: 5000},
		Oldest: &backupRef{Name: "base_01", Time: evalNow.Add(-50 * time.Hour)}}
	r.Archiver = &struct {
		Archived          int64  `json:"archived"`
		Failed            int64  `json:"failed"`
		LastArchivedWAL   string `json:"lastArchivedWal"`
		LastArchivedEpoch int64  `json:"lastArchivedEpoch"`
		CurrentLSN        string `json:"currentLsn"`
		SegmentBytes      int64  `json:"segmentBytes"`
	}{Archived: 10, LastArchivedWAL: "000000010000000000000010", LastArchivedEpoch: evalNow.Add(-time.Minute).Unix(),
		CurrentLSN: "0/11000060", SegmentBytes: 16 << 20}
	return r
}

func condition(conds []metav1.Condition, t string) metav1.Condition {
	for _, c := range conds {
		if c.Type == t {
			return c
		}
	}
	Fail("no condition " + t)
	return metav1.Condition{}
}

var dailySpec = dbv1alpha1.BackupSpec{Enabled: true, Schedule: nightlyCron}

var _ = Describe("Reading the backup report", func() {
	Describe("archive lag", func() {
		DescribeTable("bytes between the last archived segment's end and the write position",
			func(lsn, wal string, segment, want int64) {
				Expect(archiveLagBytes(lsn, wal, segment)).To(Equal(want))
			},
			Entry("inside the segment after the last archived", "0/11000060", "000000010000000000000010", int64(16<<20), int64(0x60)),
			Entry("two segments ahead", "0/13000000", "000000010000000000000010", int64(16<<20), int64(2<<24)),
			Entry("never archived: everything written", "0/3000000", "", int64(16<<20), int64(0x3000000)),
			Entry("crossing into the next log file", "1/1000000", "0000000100000000000000FF", int64(16<<20), int64(0x1000000)),
			Entry("never negative", "0/1000000", "000000010000000000000010", int64(16<<20), int64(0)),
			Entry("an unreadable position", "garbage", "000000010000000000000010", int64(16<<20), int64(0)),
			Entry("a segment size that is not the default", "0/1800000", "000000010000000000000000", int64(1<<20), int64(0x1800000-1<<20)),
		)
	})

	It("reads PostgreSQL's log sequence numbers", func() {
		got, err := parseLSN("A/FF")
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal(int64(0xA<<32 | 0xFF)))
		_, err = parseLSN("nonsense")
		Expect(err).To(HaveOccurred())
	})

	It("works out the interval of a cron schedule", func() {
		Expect(scheduleInterval(nightlyCron, evalNow)).To(Equal(24 * time.Hour))
		Expect(scheduleInterval("*/10 * * * *", evalNow)).To(Equal(10 * time.Minute))
		Expect(scheduleInterval("not a schedule", evalNow)).To(BeZero())
	})

	Describe("what a report means", func() {
		eval := func(r *backupReport, at, now time.Time) backupEvaluation {
			return evaluateBackup(dailySpec, r, at, now)
		}
		at := evalNow.Add(-5 * time.Minute)

		It("is unknown until a run has reported", func() {
			e := eval(nil, time.Time{}, evalNow)
			Expect(condition(e.conditions, "BackupHealthy").Status).To(Equal(metav1.ConditionUnknown))
			Expect(condition(e.conditions, "WALArchivingHealthy").Reason).To(Equal("NotObserved"))
		})

		It("stops trusting a report that is too old", func() {
			e := eval(freshReport(), evalNow.Add(-2*time.Hour), evalNow)
			Expect(condition(e.conditions, "BackupHealthy").Reason).To(Equal("StatusStale"))
			Expect(e.status.LastBackupName).To(BeEmpty(), "stale data is not presented as current")
		})

		It("is healthy when the newest backup is recent and archiving keeps up", func() {
			e := eval(freshReport(), at, evalNow)
			Expect(condition(e.conditions, "BackupHealthy").Status).To(Equal(metav1.ConditionTrue))
			Expect(condition(e.conditions, "WALArchivingHealthy").Status).To(Equal(metav1.ConditionTrue))
			Expect(e.status.BackupCount).To(Equal(int32(3)))
			Expect(e.status.LastBackupName).To(Equal("base_0A"))
			Expect(e.status.LastBackupSizeBytes).To(Equal(int64(5000)))
			Expect(e.status.OldestRecoveryPoint.Time).To(Equal(evalNow.Add(-50 * time.Hour)))
			Expect(e.status.WALArchiving.LagBytes).To(Equal(int64(0x60)))
		})

		It("says there is no backup when the bucket holds none", func() {
			r := freshReport()
			r.Backups.Count, r.Backups.Newest, r.Backups.Oldest = 0, nil, nil
			c := condition(eval(r, at, evalNow).conditions, "BackupHealthy")
			Expect(c.Status).To(Equal(metav1.ConditionFalse))
			Expect(c.Reason).To(Equal("NoBackup"))
		})

		It("is overdue after two scheduled intervals and a grace period", func() {
			r := freshReport()
			r.Backups.Newest.Time = at.Add(-(2*24*time.Hour + 20*time.Minute))
			Expect(condition(eval(r, at, evalNow).conditions, "BackupHealthy").Reason).To(Equal("BackupOverdue"))
			r.Backups.Newest.Time = at.Add(-(2*24*time.Hour + 10*time.Minute))
			Expect(condition(eval(r, at, evalNow).conditions, "BackupHealthy").Status).To(Equal(metav1.ConditionTrue))
		})

		It("reports a bucket that cannot be read", func() {
			r := freshReport()
			r.Backups = nil
			r.Errors = []string{"wal-g backup-list failed: connection refused"}
			c := condition(eval(r, at, evalNow).conditions, "BackupHealthy")
			Expect(c.Status).To(Equal(metav1.ConditionFalse))
			Expect(c.Reason).To(Equal("BucketUnreadable"))
			Expect(c.Message).To(ContainSubstring("connection refused"))
		})

		It("flags archiving that is more than eight segments behind", func() {
			r := freshReport()
			r.Archiver.CurrentLSN = "0/22000000"
			c := condition(eval(r, at, evalNow).conditions, "WALArchivingHealthy")
			Expect(c.Status).To(Equal(metav1.ConditionFalse))
			Expect(c.Reason).To(Equal("ArchivingBehind"))
		})

		It("flags archiving that has stalled with a whole segment waiting", func() {
			r := freshReport()
			r.Archiver.CurrentLSN = "0/12000000"
			r.Archiver.LastArchivedEpoch = at.Add(-30 * time.Minute).Unix()
			c := condition(eval(r, at, evalNow).conditions, "WALArchivingHealthy")
			Expect(c.Reason).To(Equal("ArchivingStalled"))
		})

		It("does not call an idle database stalled, since little WAL is waiting", func() {
			r := freshReport()
			r.Archiver.LastArchivedEpoch = at.Add(-3 * time.Hour).Unix()
			Expect(condition(eval(r, at, evalNow).conditions, "WALArchivingHealthy").Status).To(Equal(metav1.ConditionTrue))
		})

		It("cannot judge archiving when the database could not be read", func() {
			r := freshReport()
			r.Archiver = nil
			r.Errors = []string{"psql failed: connection refused"}
			c := condition(eval(r, at, evalNow).conditions, "WALArchivingHealthy")
			Expect(c.Status).To(Equal(metav1.ConditionUnknown))
			Expect(c.Reason).To(Equal("ArchiverUnreadable"))
		})

		It("never puts the clock in a message, so an unchanged condition is not rewritten", func() {
			first := eval(freshReport(), at, evalNow).conditions
			second := eval(freshReport(), at, evalNow.Add(7*time.Minute)).conditions
			Expect(second).To(Equal(first))
		})
	})

	Describe("the Degraded roll-up", func() {
		on := &dbv1alpha1.PostgresCluster{Spec: dbv1alpha1.PostgresClusterSpec{Backup: dailySpec}}
		now := evalNow
		at := now.Add(-time.Minute)

		It("is False when backups are off", func() {
			c := degradedCondition(&dbv1alpha1.PostgresCluster{}, backupEvaluation{})
			Expect(c.Status).To(Equal(metav1.ConditionFalse))
			Expect(c.Reason).To(Equal("BackupDisabled"))
		})
		It("is Unknown before a report", func() {
			Expect(degradedCondition(on, evaluateBackup(dailySpec, nil, time.Time{}, now)).Status).To(Equal(metav1.ConditionUnknown))
		})
		It("is False when all is well", func() {
			Expect(degradedCondition(on, evaluateBackup(dailySpec, freshReport(), at, now)).Status).To(Equal(metav1.ConditionFalse))
		})
		It("is True, naming the problem, when archiving is behind", func() {
			r := freshReport()
			r.Archiver.CurrentLSN = "0/22000000"
			c := degradedCondition(on, evaluateBackup(dailySpec, r, at, now))
			Expect(c.Status).To(Equal(metav1.ConditionTrue))
			Expect(c.Message).To(ContainSubstring("WALArchivingHealthy"))
		})
		It("is True when retention could not delete", func() {
			r := freshReport()
			r.Errors = []string{"wal-g delete failed: access denied"}
			e := evaluateBackup(dailySpec, r, at, now)
			Expect(e.retentionFailed).To(BeTrue())
			c := degradedCondition(on, e)
			Expect(c.Status).To(Equal(metav1.ConditionTrue))
			Expect(c.Message).To(ContainSubstring("retention"))
		})
		It("does not blame retention for an unrelated error", func() {
			r := freshReport()
			r.Errors = []string{"psql failed: no route"}
			Expect(evaluateBackup(dailySpec, r, at, now).retentionFailed).To(BeFalse())
		})
	})

	Describe("the stored status", func() {
		It("keeps the restore-test fields when it refreshes the rest", func() {
			cluster := &dbv1alpha1.PostgresCluster{Spec: dbv1alpha1.PostgresClusterSpec{Backup: dailySpec}}
			cluster.Status.Backup.Verification.Message = "kept"
			updated, changed := mergeBackupStatus(cluster, evaluateBackup(dailySpec, freshReport(), evalNow.Add(-time.Minute), evalNow))
			Expect(changed).To(BeTrue())
			Expect(updated.Verification.Message).To(Equal("kept"))
			Expect(updated.LastBackupName).To(Equal("base_0A"))
		})
		It("keeps the last known backups when the bucket could not be read, and still updates the archiver", func() {
			cluster := &dbv1alpha1.PostgresCluster{Spec: dbv1alpha1.PostgresClusterSpec{Backup: dailySpec}}
			cluster.Status.Backup.LastBackupName = previousBackup
			cluster.Status.Backup.BackupCount = 4
			r := freshReport()
			r.Backups = nil
			r.Errors = []string{"wal-g backup-list failed: down"}
			updated, changed := mergeBackupStatus(cluster, evaluateBackup(dailySpec, r, evalNow.Add(-time.Minute), evalNow))
			Expect(changed).To(BeTrue())
			Expect(updated.LastBackupName).To(Equal(previousBackup))
			Expect(updated.BackupCount).To(Equal(int32(4)))
			Expect(updated.WALArchiving.LagBytes).To(Equal(int64(0x60)))
		})
		It("keeps the last known archiver when the database could not be read", func() {
			cluster := &dbv1alpha1.PostgresCluster{Spec: dbv1alpha1.PostgresClusterSpec{Backup: dailySpec}}
			cluster.Status.Backup.WALArchiving.LagBytes = 1234
			r := freshReport()
			r.Archiver = nil
			r.Errors = []string{"psql failed: down"}
			updated, _ := mergeBackupStatus(cluster, evaluateBackup(dailySpec, r, evalNow.Add(-time.Minute), evalNow))
			Expect(updated.WALArchiving.LagBytes).To(Equal(int64(1234)))
			Expect(updated.LastBackupName).To(Equal("base_0A"))
		})
		It("leaves the last known values alone when there is nothing fresh to say", func() {
			cluster := &dbv1alpha1.PostgresCluster{Spec: dbv1alpha1.PostgresClusterSpec{Backup: dailySpec}}
			cluster.Status.Backup.LastBackupName = previousBackup
			_, changed := mergeBackupStatus(cluster, evaluateBackup(dailySpec, nil, time.Time{}, evalNow))
			Expect(changed).To(BeFalse())
		})
		It("clears the backup status when backups are turned off", func() {
			cluster := &dbv1alpha1.PostgresCluster{}
			cluster.Status.Backup.LastBackupName = previousBackup
			updated, changed := mergeBackupStatus(cluster, backupEvaluation{})
			Expect(changed).To(BeTrue())
			Expect(updated.LastBackupName).To(BeEmpty())
		})
	})
})

var _ = Describe("The backup status CronJob", func() {
	cluster := func() *dbv1alpha1.PostgresCluster {
		c := backupCluster()
		c.Spec.Backup.Encryption = &dbv1alpha1.BackupEncryption{KeySecret: "backup-key"}
		return c
	}

	It("runs the cluster's own image as the database user, with the strictest security settings", func() {
		spec := backupStatusCronJobSpec(cluster(), "registry.example/spilo:1")
		pod := spec.JobTemplate.Spec.Template.Spec
		Expect(pod.Containers[0].Image).To(Equal("registry.example/spilo:1"))
		Expect(*pod.SecurityContext.RunAsNonRoot).To(BeTrue())
		Expect(*pod.SecurityContext.RunAsUser).To(Equal(spiloUID))
		Expect(*pod.Containers[0].SecurityContext.AllowPrivilegeEscalation).To(BeFalse())
		Expect(pod.Containers[0].SecurityContext.Capabilities.Drop).To(ConsistOf(corev1.Capability("ALL")))
		Expect(*pod.AutomountServiceAccountToken).To(BeFalse(), "it never talks to the Kubernetes API")
		Expect(pod.Containers[0].Resources.Limits.Memory().String()).To(Equal("256Mi"))
	})

	It("never runs two at once, and never retries a run that failed", func() {
		spec := backupStatusCronJobSpec(cluster(), "img")
		Expect(spec.ConcurrencyPolicy).To(Equal(batchv1.ForbidConcurrent))
		Expect(*spec.JobTemplate.Spec.BackoffLimit).To(BeZero())
		Expect(*spec.JobTemplate.Spec.ActiveDeadlineSeconds).To(BeNumerically(">", 0))
	})

	It("reaches the database over TLS with the cluster's own credentials", func() {
		env := envMap(backupStatusCronJobSpec(cluster(), "img").JobTemplate.Spec.Template.Spec.Containers[0].Env)
		Expect(env["PGHOST"].Value).To(Equal("orders"))
		Expect(env["PGSSLMODE"].Value).To(Equal("require"))
		Expect(env["PGPASSWORD"].ValueFrom.SecretKeyRef.Name).To(Equal("orders-credentials"))
		Expect(env["PGPASSWORD"].Value).To(BeEmpty())
	})

	It("carries the retention settings and the bucket access", func() {
		env := envMap(backupStatusCronJobSpec(cluster(), "img").JobTemplate.Spec.Template.Spec.Containers[0].Env)
		Expect(env["RETAIN_FULL"].Value).To(Equal("7"))
		Expect(env["RETAIN_DAYS"].Value).To(Equal("14"))
		Expect(env["WALG_S3_PREFIX"].Value).To(Equal("s3://acme-db-backups/prod/orders/wal/17"))
		Expect(env["WALG_LIBSODIUM_KEY_TRANSFORM"].Value).To(Equal("hex"))
		Expect(env["AWS_SECRET_ACCESS_KEY"].ValueFrom.SecretKeyRef.Name).To(Equal("backup-s3"))
	})

	It("addresses a custom endpoint the way wal-g reads it directly", func() {
		c := cluster()
		c.Spec.Backup.Destination.Endpoint = "http://s3:8333"
		c.Spec.Backup.Destination.ForcePathStyle = true
		env := envMap(backupStatusCronJobSpec(c, "img").JobTemplate.Spec.Template.Spec.Containers[0].Env)
		Expect(env["AWS_ENDPOINT"].Value).To(Equal("http://s3:8333"))
		Expect(env["AWS_S3_FORCE_PATH_STYLE"].Value).To(Equal("true"))
		Expect(env["WALG_DISABLE_S3_SSE"].Value).To(Equal("true"))
	})

	It("is not mistaken for a database member by anything that selects database pods", func() {
		labels := backupStatusCronJobSpec(cluster(), "img").JobTemplate.Spec.Template.Labels
		Expect(labels[labelAppComponent]).To(Equal(componentBackupStatus))
		Expect(labels[labelAppComponent]).NotTo(Equal(componentPostgres))
	})
})

var _ = Describe("The backup status script", func() {
	var dir string

	BeforeEach(func() {
		if _, err := exec.LookPath("python3"); err != nil {
			Skip("python3 is not available")
		}
		dir = GinkgoT().TempDir()
	})

	// fakeTools writes stand-ins for wal-g and psql. wal-g lists the backups
	// named in backups.json, and records every other invocation in calls.log.
	fakeTools := func(backupsJSON, psqlRow string, walgListFails, deleteFails, psqlFails bool) {
		write := func(name, body string) {
			ExpectWithOffset(2, os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755)).To(Succeed()) //nolint:gosec // test scripts must be executable
		}
		ExpectWithOffset(1, os.WriteFile(filepath.Join(dir, "backups.json"), []byte(backupsJSON), 0o600)).To(Succeed())
		write("wal-g", fmt.Sprintf(`#!/bin/sh
echo "$@" >> %[1]s/calls.log
case "$1 $2" in
  "backup-list --detail")
    if [ "%[2]t" = true ]; then echo "boom" >&2; exit 1; fi
    cat %[1]s/backups.json ;;
  "delete before")
    if [ "%[3]t" = true ]; then echo "delete boom" >&2; exit 1; fi ;;
esac
`, dir, walgListFails, deleteFails))
		write("psql", fmt.Sprintf(`#!/bin/sh
if [ "%t" = true ]; then echo "no connection" >&2; exit 2; fi
echo "%s"
`, psqlFails, psqlRow))
	}
	run := func(full, days string) map[string]any {
		cmd := exec.Command("python3", "scripts/backup_status.py")
		cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "TERMINATION_LOG="+filepath.Join(dir, "out.json"),
			"RETAIN_FULL="+full, "RETAIN_DAYS="+days)
		out, err := cmd.CombinedOutput()
		ExpectWithOffset(1, err).NotTo(HaveOccurred(), string(out))
		raw, err := os.ReadFile(filepath.Join(dir, "out.json"))
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		var report map[string]any
		ExpectWithOffset(1, json.Unmarshal(raw, &report)).To(Succeed())
		return report
	}
	calls := func() string {
		raw, _ := os.ReadFile(filepath.Join(dir, "calls.log"))
		return string(raw)
	}
	// daily builds a listing of n backups, the newest 12 hours old and one a
	// day older than the last.
	daily := func(n int) string {
		items := make([]string, 0, n)
		for i := range n {
			finished := time.Now().UTC().Add(-time.Duration(i*24+12) * time.Hour).Format("2006-01-02T15:04:05.000000Z")
			items = append(items, fmt.Sprintf(`{"backup_name":"b%d","finish_time":"%s","compressed_size":%d}`, i, finished, 1000+i))
		}
		return "[" + strings.Join(items, ",") + "]"
	}
	const goodRow = "10|0|000000010000000000000010|1760000000|0/11000060|16777216"

	It("reports the newest and oldest backup and the archiver", func() {
		fakeTools(daily(3), goodRow, false, false, false)
		report := run("7", "14")
		backups := report["backups"].(map[string]any)
		Expect(backups["count"]).To(BeEquivalentTo(3))
		Expect(backups["newest"].(map[string]any)["name"]).To(Equal("b0"))
		Expect(backups["newest"].(map[string]any)["sizeBytes"]).To(BeEquivalentTo(1000))
		Expect(backups["oldest"].(map[string]any)["name"]).To(Equal("b2"))
		archiver := report["archiver"].(map[string]any)
		Expect(archiver["currentLsn"]).To(Equal("0/11000060"))
		Expect(archiver["segmentBytes"]).To(BeEquivalentTo(16777216))
		Expect(report["errors"]).To(BeEmpty())
	})

	It("deletes what is past retention, keeping the newest backups and everything recent", func() {
		fakeTools(daily(10), goodRow, false, false, false)
		report := run("3", "4")
		// Newest three, plus anything under four days old: b0 to b3 stay.
		Expect(calls()).To(ContainSubstring("delete before FIND_FULL b3 --confirm"))
		Expect(report["retention"].(map[string]any)["deleted"]).To(BeEquivalentTo(6))
		Expect(report["backups"].(map[string]any)["count"]).To(BeEquivalentTo(4))
		Expect(report["backups"].(map[string]any)["oldest"].(map[string]any)["name"]).To(Equal("b3"))
	})

	It("keeps the newest few even when they are all older than the retention window", func() {
		fakeTools(daily(10), goodRow, false, false, false)
		run("6", "1")
		Expect(calls()).To(ContainSubstring("delete before FIND_FULL b5 --confirm"))
	})

	It("deletes nothing when no more than the minimum exist", func() {
		fakeTools(daily(3), goodRow, false, false, false)
		run("3", "1")
		Expect(calls()).NotTo(ContainSubstring("delete"))
	})

	It("deletes nothing when everything is inside the window", func() {
		fakeTools(daily(5), goodRow, false, false, false)
		run("1", "30")
		Expect(calls()).NotTo(ContainSubstring("delete"))
	})

	It("never deletes when the backups could not be listed, and still reports the archiver", func() {
		fakeTools(daily(10), goodRow, true, false, false)
		report := run("1", "1")
		Expect(calls()).NotTo(ContainSubstring("delete"))
		Expect(report).NotTo(HaveKey("backups"))
		Expect(report["archiver"]).NotTo(BeNil())
		Expect(report["errors"]).To(ContainElement(ContainSubstring("backup-list failed")))
	})

	It("still reports the backups when the database could not be read", func() {
		fakeTools(daily(3), goodRow, false, false, true)
		report := run("7", "14")
		Expect(report).To(HaveKey("backups"))
		Expect(report).NotTo(HaveKey("archiver"))
		Expect(report["errors"]).To(ContainElement(ContainSubstring("psql failed")))
	})

	It("records a failed delete instead of ending the run", func() {
		fakeTools(daily(10), goodRow, false, true, false)
		report := run("3", "4")
		Expect(report["errors"]).To(ContainElement(ContainSubstring("delete failed")))
		Expect(report["backups"].(map[string]any)["count"]).To(BeEquivalentTo(10), "nothing was deleted, so nothing is claimed deleted")
	})

	It("reports a store that does not answer plainly, and does not delete", func() {
		fakeTools(daily(3), goodRow, false, false, false)
		hang := "#!/bin/sh\nsleep 30\n"
		Expect(os.WriteFile(filepath.Join(dir, "wal-g"), []byte(hang), 0o755)).To(Succeed()) //nolint:gosec // test script must be executable
		cmd := exec.Command("python3", "scripts/backup_status.py")
		cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "TERMINATION_LOG="+filepath.Join(dir, "out.json"),
			"COMMAND_TIMEOUT=1")
		out, err := cmd.CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), string(out))
		raw, err := os.ReadFile(filepath.Join(dir, "out.json"))
		Expect(err).NotTo(HaveOccurred())
		Expect(string(raw)).To(ContainSubstring("wal-g backup-list timed out after 1s"))
		Expect(string(raw)).NotTo(ContainSubstring("['wal-g'"))
	})

	It("treats an empty bucket as zero backups, not an error", func() {
		fakeTools("", goodRow, false, false, false)
		report := run("7", "14")
		Expect(report["backups"].(map[string]any)["count"]).To(BeEquivalentTo(0))
		Expect(report["errors"]).To(BeEmpty())
	})

	It("keeps its report small enough for a termination message", func() {
		fakeTools(daily(200), goodRow, false, false, false)
		run("365", "3650")
		raw, err := os.ReadFile(filepath.Join(dir, "out.json"))
		Expect(err).NotTo(HaveOccurred())
		Expect(len(raw)).To(BeNumerically("<", 4096))
	})
})

// Everything above went through functions. This runs the whole reconciler
// against the API server, with a finished status pod standing in for a run.
var _ = Describe("Backup status through the reconciler", func() {
	const namespace = "default"
	ctx := context.Background()
	key := func(name string) types.NamespacedName { return types.NamespacedName{Name: name, Namespace: namespace} }
	reconcileOnce := func(name string) {
		rec := &PostgresClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Patroni: &fakePatroni{live: map[string]any{}}}
		_, err := rec.Reconcile(ctx, reconcile.Request{NamespacedName: key(name)})
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
	}
	setup := func(name string, enabled bool) {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name + "-s3", Namespace: namespace},
			StringData: map[string]string{secretKeyAccessKey: "id", secretKeySecretKey: "secret"},
		}
		ExpectWithOffset(1, k8sClient.Create(ctx, secret)).To(Succeed())
		DeferCleanup(func() { ExpectWithOffset(1, client.IgnoreNotFound(k8sClient.Delete(ctx, secret))).To(Succeed()) })
		c := &dbv1alpha1.PostgresCluster{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: dbv1alpha1.PostgresClusterSpec{
				Version: "16", Instances: 1,
				Storage:   dbv1alpha1.StorageSpec{Size: resource.MustParse("1Gi")},
				Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}},
				Backup: dbv1alpha1.BackupSpec{
					Enabled: enabled, Schedule: nightlyCron,
					Destination: &dbv1alpha1.BackupDestination{Bucket: testBucket, Region: testRegion, CredentialsSecret: name + "-s3"},
					Retention:   dbv1alpha1.BackupRetention{Full: 3, Days: 4},
				},
			},
		}
		ExpectWithOffset(1, k8sClient.Create(ctx, c)).To(Succeed())
		DeferCleanup(func() { ExpectWithOffset(1, client.IgnoreNotFound(k8sClient.Delete(ctx, c))).To(Succeed()) })
		reconcileOnce(name)
	}
	// finishedRun adds a status pod that has already finished, with the given
	// termination message.
	finishedRun := func(name, message string, finished time.Time) {
		labels := backupStatusLabels(&dbv1alpha1.PostgresCluster{ObjectMeta: metav1.ObjectMeta{Name: name}})
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("%s-status-%d", name, finished.Unix()), Namespace: namespace, Labels: labels},
			Spec:       corev1.PodSpec{RestartPolicy: corev1.RestartPolicyNever, Containers: []corev1.Container{{Name: statusContainerName, Image: testPodImage}}},
		}
		ExpectWithOffset(1, k8sClient.Create(ctx, pod)).To(Succeed())
		DeferCleanup(func() {
			ExpectWithOffset(1, client.IgnoreNotFound(k8sClient.Delete(ctx, pod, client.GracePeriodSeconds(0)))).To(Succeed())
		})
		pod.Status.Phase = corev1.PodSucceeded
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name: statusContainerName,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				Message: message, FinishedAt: metav1.NewTime(finished),
			}},
		}}
		ExpectWithOffset(1, k8sClient.Status().Update(ctx, pod)).To(Succeed())
	}
	get := func(name string) *dbv1alpha1.PostgresCluster {
		c := &dbv1alpha1.PostgresCluster{}
		ExpectWithOffset(1, k8sClient.Get(ctx, key(name), c)).To(Succeed())
		return c
	}
	healthyMessage := func(finished time.Time) string {
		return fmt.Sprintf(`{"backups":{"count":2,"newest":{"name":"base_B","time":%q,"sizeBytes":7000},"oldest":{"name":"base_A","time":%q}},`+
			`"archiver":{"archived":4,"failed":0,"lastArchivedWal":"000000010000000000000010","lastArchivedEpoch":%d,`+
			`"currentLsn":"0/11000060","segmentBytes":16777216},"retention":{"deleted":0},"errors":[]}`,
			finished.Add(-time.Hour).UTC().Format(time.RFC3339), finished.Add(-30*time.Hour).UTC().Format(time.RFC3339), finished.Unix()-30)
	}

	It("creates the status CronJob and its script when backups are on, and removes them when they are off", func() {
		const name = "st-lifecycle"
		setup(name, true)
		cj := &batchv1.CronJob{}
		Expect(k8sClient.Get(ctx, key(name+"-backup-status"), cj)).To(Succeed())
		Expect(cj.Spec.Schedule).To(Equal(backupStatusSchedule))
		script := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, key(name+"-backup-status"), script)).To(Succeed())
		Expect(script.Data["backup_status.py"]).To(Equal(backupStatusScript))

		c := get(name)
		c.Spec.Backup.Enabled = false
		Expect(k8sClient.Update(ctx, c)).To(Succeed())
		reconcileOnce(name)
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key(name+"-backup-status"), &batchv1.CronJob{}))).To(BeTrue())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key(name+"-backup-status"), &corev1.ConfigMap{}))).To(BeTrue())
	})

	It("creates nothing for a cluster without backups", func() {
		const name = "st-off"
		setup(name, false)
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key(name+"-backup-status"), &batchv1.CronJob{}))).To(BeTrue())
		c := get(name)
		Expect(meta.FindStatusCondition(c.Status.Conditions, "BackupHealthy")).To(BeNil())
	})

	It("fills status.backup, the conditions and the metrics from a finished run", func() {
		const name = "st-report"
		setup(name, true)
		finishedRun(name, healthyMessage(time.Now()), time.Now())
		reconcileOnce(name)

		c := get(name)
		Expect(c.Status.Backup.BackupCount).To(Equal(int32(2)))
		Expect(c.Status.Backup.LastBackupName).To(Equal("base_B"))
		Expect(c.Status.Backup.LastBackupSizeBytes).To(Equal(int64(7000)))
		Expect(c.Status.Backup.OldestRecoveryPoint).NotTo(BeNil())
		Expect(c.Status.Backup.WALArchiving.LastArchivedTime).NotTo(BeNil())
		Expect(meta.FindStatusCondition(c.Status.Conditions, "BackupHealthy").Status).To(Equal(metav1.ConditionTrue))
		Expect(meta.FindStatusCondition(c.Status.Conditions, "WALArchivingHealthy").Status).To(Equal(metav1.ConditionTrue))
		Expect(meta.FindStatusCondition(c.Status.Conditions, "Degraded").Status).To(Equal(metav1.ConditionFalse))
		Expect(testutil.ToFloat64(backupCount.WithLabelValues(namespace, name))).To(Equal(float64(2)))
		Expect(testutil.ToFloat64(backupLastSizeBytes.WithLabelValues(namespace, name))).To(Equal(float64(7000)))
		Expect(testutil.ToFloat64(walArchiveLagBytes.WithLabelValues(namespace, name))).To(Equal(float64(0x60)))
		Expect(testutil.ToFloat64(backupLastSuccessTimestampSeconds.WithLabelValues(namespace, name))).To(BeNumerically(">", 0))
	})

	It("marks the cluster Degraded when the run found archiving far behind", func() {
		const name = "st-behind"
		setup(name, true)
		bad := strings.Replace(healthyMessage(time.Now()), "0/11000060", "0/40000000", 1)
		finishedRun(name, bad, time.Now())
		reconcileOnce(name)
		c := get(name)
		Expect(meta.FindStatusCondition(c.Status.Conditions, "WALArchivingHealthy").Status).To(Equal(metav1.ConditionFalse))
		Expect(meta.FindStatusCondition(c.Status.Conditions, "Degraded").Status).To(Equal(metav1.ConditionTrue))
	})

	It("uses the newest run when several have finished", func() {
		const name = "st-newest"
		setup(name, true)
		old := time.Now().Add(-20 * time.Minute)
		finishedRun(name, strings.Replace(healthyMessage(old), "base_B", "base_OLD", 1), old)
		finishedRun(name, healthyMessage(time.Now()), time.Now())
		reconcileOnce(name)
		Expect(get(name).Status.Backup.LastBackupName).To(Equal("base_B"))
	})

	It("does not trust a run that finished long ago", func() {
		const name = "st-stale"
		setup(name, true)
		old := time.Now().Add(-3 * time.Hour)
		finishedRun(name, healthyMessage(old), old)
		reconcileOnce(name)
		c := get(name)
		Expect(meta.FindStatusCondition(c.Status.Conditions, "BackupHealthy").Reason).To(Equal("StatusStale"))
		Expect(c.Status.Backup.LastBackupName).To(BeEmpty())
	})

	It("survives a termination message that is not a report", func() {
		const name = "st-garbage"
		setup(name, true)
		finishedRun(name, "Killed", time.Now())
		reconcileOnce(name)
		c := get(name)
		Expect(meta.FindStatusCondition(c.Status.Conditions, "BackupHealthy").Status).To(Equal(metav1.ConditionFalse))
	})

	It("clears the backup conditions and status when backups are turned off", func() {
		const name = "st-clear"
		setup(name, true)
		finishedRun(name, healthyMessage(time.Now()), time.Now())
		reconcileOnce(name)
		Expect(get(name).Status.Backup.LastBackupName).To(Equal("base_B"))

		c := get(name)
		c.Spec.Backup.Enabled = false
		Expect(k8sClient.Update(ctx, c)).To(Succeed())
		reconcileOnce(name)
		c = get(name)
		Expect(c.Status.Backup.LastBackupName).To(BeEmpty())
		Expect(meta.FindStatusCondition(c.Status.Conditions, "BackupHealthy")).To(BeNil())
	})
})
