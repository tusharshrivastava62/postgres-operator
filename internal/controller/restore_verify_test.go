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

const weeklyCron = "0 4 * * 0"

var verifySpec = dbv1alpha1.BackupVerification{Enabled: true, Schedule: weeklyCron}

var _ = Describe("The restore test result", func() {
	passed := func(at time.Time) *verifyOutcome {
		return &verifyOutcome{finishedAt: at, success: true, duration: 95, backup: "base_0A"}
	}

	Describe("the RestoreVerified condition", func() {
		It("is unknown until a test has run", func() {
			c := verificationCondition(verifySpec, nil, evalNow)
			Expect(c.Status).To(Equal(metav1.ConditionUnknown))
			Expect(c.Reason).To(Equal("NotRunYet"))
		})

		It("is true after a passing test, and says how long the restore took", func() {
			c := verificationCondition(verifySpec, passed(evalNow.Add(-time.Hour)), evalNow)
			Expect(c.Status).To(Equal(metav1.ConditionTrue))
			Expect(c.Message).To(ContainSubstring("base_0A"))
			Expect(c.Message).To(ContainSubstring("95 seconds"))
		})

		It("is false after a failed test, with the reason", func() {
			c := verificationCondition(verifySpec, &verifyOutcome{finishedAt: evalNow, message: "wal-g backup-fetch failed: no key"}, evalNow)
			Expect(c.Status).To(Equal(metav1.ConditionFalse))
			Expect(c.Reason).To(Equal("RestoreFailed"))
			Expect(c.Message).To(ContainSubstring("no key"))
		})

		It("is false when the last pass is more than two weekly intervals and a grace period old", func() {
			old := passed(evalNow.Add(-(14*24*time.Hour + 2*time.Hour)))
			Expect(verificationCondition(verifySpec, old, evalNow).Reason).To(Equal("RestoreTestOverdue"))
			recent := passed(evalNow.Add(-(14*24*time.Hour - time.Hour)))
			Expect(verificationCondition(verifySpec, recent, evalNow).Status).To(Equal(metav1.ConditionTrue))
		})

		It("does not put the clock in a message, so an unchanged condition is not rewritten", func() {
			at := evalNow.Add(-time.Hour)
			Expect(verificationCondition(verifySpec, passed(at), evalNow)).
				To(Equal(verificationCondition(verifySpec, passed(at), evalNow.Add(3*time.Hour))))
		})
	})

	Describe("the stored restore test", func() {
		It("records a pass, with how long it took", func() {
			got := mergeVerification(dbv1alpha1.VerificationStatus{}, passed(evalNow))
			Expect(got.LastRunTime.Time).To(Equal(evalNow))
			Expect(got.LastSuccessTime.Time).To(Equal(evalNow))
			Expect(got.DurationSeconds).To(Equal(int64(95)))
			Expect(got.Message).To(BeEmpty())
		})

		It("records a failure without forgetting the last pass", func() {
			before := mergeVerification(dbv1alpha1.VerificationStatus{}, passed(evalNow.Add(-24*time.Hour)))
			got := mergeVerification(before, &verifyOutcome{finishedAt: evalNow, message: "boom"})
			Expect(got.LastRunTime.Time).To(Equal(evalNow))
			Expect(got.LastSuccessTime.Time).To(Equal(evalNow.Add(-24 * time.Hour)))
			Expect(got.DurationSeconds).To(Equal(int64(95)))
			Expect(got.Message).To(Equal("boom"))
		})

		It("clears an old failure message when a test passes again", func() {
			failed := mergeVerification(dbv1alpha1.VerificationStatus{}, &verifyOutcome{finishedAt: evalNow, message: "boom"})
			Expect(mergeVerification(failed, passed(evalNow.Add(time.Hour))).Message).To(BeEmpty())
		})

		It("changes nothing when there is no test result", func() {
			current := dbv1alpha1.VerificationStatus{Message: "x"}
			Expect(mergeVerification(current, nil)).To(Equal(current))
		})
	})

	It("does not call backups healthy because a restore test passed before the status check has reported", func() {
		spec := dailySpec
		spec.Verification = verifySpec
		on := &dbv1alpha1.PostgresCluster{Spec: dbv1alpha1.PostgresClusterSpec{Backup: spec}}
		eval := evaluateBackup(spec, nil, time.Time{}, evalNow)
		eval.conditions = append(eval.conditions, verificationCondition(spec.Verification, passed(evalNow), evalNow))
		Expect(degradedCondition(on, eval).Status).To(Equal(metav1.ConditionUnknown))
	})

	It("rolls a failed restore test up into Degraded", func() {
		spec := dailySpec
		spec.Verification = verifySpec
		on := &dbv1alpha1.PostgresCluster{Spec: dbv1alpha1.PostgresClusterSpec{Backup: spec}}
		eval := evaluateBackup(spec, freshReport(), evalNow.Add(-time.Minute), evalNow)
		eval.conditions = append(eval.conditions,
			verificationCondition(spec.Verification, &verifyOutcome{finishedAt: evalNow, message: "no space left"}, evalNow))
		c := degradedCondition(on, eval)
		Expect(c.Status).To(Equal(metav1.ConditionTrue))
		Expect(c.Message).To(ContainSubstring("RestoreVerified"))
	})
})

var _ = Describe("The restore test CronJob", func() {
	cluster := func() *dbv1alpha1.PostgresCluster {
		c := backupCluster()
		c.Spec.Backup.Verification = verifySpec
		c.Spec.Storage.Size = resource.MustParse("20Gi")
		c.Spec.Resources = corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi")}}
		return c
	}

	It("runs on the schedule from the spec, one at a time", func() {
		spec := restoreVerifyCronJobSpec(cluster(), "img")
		Expect(spec.Schedule).To(Equal(weeklyCron))
		Expect(spec.ConcurrencyPolicy).To(Equal(batchv1.ForbidConcurrent))
		Expect(*spec.SuccessfulJobsHistoryLimit).To(Equal(int32(3)))
		Expect(*spec.FailedJobsHistoryLimit).To(Equal(int32(3)))
	})

	It("runs the cluster's image as the database user with the strictest settings", func() {
		pod := restoreVerifyCronJobSpec(cluster(), "registry.example/spilo:1").JobTemplate.Spec.Template.Spec
		Expect(pod.Containers[0].Image).To(Equal("registry.example/spilo:1"))
		Expect(*pod.SecurityContext.RunAsNonRoot).To(BeTrue())
		Expect(*pod.SecurityContext.RunAsUser).To(Equal(spiloUID))
		Expect(*pod.Containers[0].SecurityContext.AllowPrivilegeEscalation).To(BeFalse())
		Expect(*pod.AutomountServiceAccountToken).To(BeFalse())
	})

	It("has the cluster's own memory limit, and scratch space no larger than its data volume", func() {
		pod := restoreVerifyCronJobSpec(cluster(), "img").JobTemplate.Spec.Template.Spec
		Expect(pod.Containers[0].Resources.Limits.Memory().String()).To(Equal("2Gi"))
		var scratch *corev1.EmptyDirVolumeSource
		for _, v := range pod.Volumes {
			if v.Name == restoreVolumeName {
				scratch = v.EmptyDir
			}
		}
		Expect(scratch).NotTo(BeNil())
		Expect(scratch.SizeLimit.String()).To(Equal("20Gi"))
	})

	It("gets shared memory, which PostgreSQL needs, backed by memory and capped", func() {
		pod := restoreVerifyCronJobSpec(cluster(), "img").JobTemplate.Spec.Template.Spec
		var shm *corev1.EmptyDirVolumeSource
		for _, v := range pod.Volumes {
			if v.Name == shmVolumeName {
				shm = v.EmptyDir
			}
		}
		Expect(shm).NotTo(BeNil())
		Expect(shm.Medium).To(Equal(corev1.StorageMediumMemory))
		Expect(shm.SizeLimit.Value()).To(Equal(int64(256 << 20)))
	})

	It("is given enough time for a large restore, and never retried", func() {
		job := restoreVerifyCronJobSpec(cluster(), "img").JobTemplate.Spec
		Expect(*job.BackoffLimit).To(BeZero())
		Expect(*job.ActiveDeadlineSeconds).To(BeNumerically(">", 4*60*60))
	})

	It("knows the PostgreSQL version, where to restore to, and how to reach the bucket", func() {
		env := envMap(restoreVerifyCronJobSpec(cluster(), "img").JobTemplate.Spec.Template.Spec.Containers[0].Env)
		Expect(env["PGVERSION"].Value).To(Equal("17"))
		Expect(env["RESTORE_DIR"].Value).To(Equal("/restore"))
		Expect(env["WALG_S3_PREFIX"].Value).To(Equal("s3://acme-db-backups/prod/orders/wal/17"))
		Expect(env["AWS_SECRET_ACCESS_KEY"].ValueFrom.SecretKeyRef.Name).To(Equal("backup-s3"))
	})

	It("is labelled as a restore test, apart from the database and the status check", func() {
		labels := restoreVerifyCronJobSpec(cluster(), "img").JobTemplate.Spec.Template.Labels
		Expect(labels[labelAppComponent]).To(Equal(componentRestoreVerify))
	})
})

var _ = Describe("The restore test script", func() {
	var dir string

	BeforeEach(func() {
		if _, err := exec.LookPath("python3"); err != nil {
			Skip("python3 is not available")
		}
		dir = GinkgoT().TempDir()
	})

	type fakes struct {
		backups      string
		fetchFails   bool
		startFails   bool
		stillRecover bool // psql never leaves recovery
		emptyCatalog bool
		noDatabases  bool
	}
	setup := func(f fakes) {
		write := func(path, body string) {
			ExpectWithOffset(2, os.MkdirAll(filepath.Dir(path), 0o755)).To(Succeed())  //nolint:gosec // a test directory
			ExpectWithOffset(2, os.WriteFile(path, []byte(body), 0o755)).To(Succeed()) //nolint:gosec // test scripts must be executable
		}
		backups := f.backups
		if backups == "" {
			backups = `[{"backup_name":"base_01","finish_time":"2026-10-01T00:00:00Z"},{"backup_name":"base_02","finish_time":"2026-10-02T00:00:00Z"}]`
		}
		write(filepath.Join(dir, "bin", "wal-g"), fmt.Sprintf(`#!/bin/sh
echo "wal-g $@" >> %[1]s/calls.log
case "$1" in
  backup-list) echo '%[2]s' ;;
  backup-fetch)
    if [ "%[3]t" = true ]; then echo "no such key" >&2; exit 1; fi
    mkdir -p "$2" && echo 17 > "$2/PG_VERSION" ;;
esac
`, dir, backups, f.fetchFails))
		write(filepath.Join(dir, "bin", "pg_ctl"), fmt.Sprintf(`#!/bin/sh
echo "pg_ctl $@" >> %[1]s/calls.log
case "$*" in
  *" start"*)
    if [ "%[2]t" = true ]; then echo "FATAL: could not open file" > %[1]s/work/postgres.log; exit 1; fi ;;
esac
`, dir, f.startFails))
		counter := filepath.Join(dir, "recovery.count")
		write(filepath.Join(dir, "bin", "psql"), fmt.Sprintf(`#!/bin/sh
case "$*" in
  *pg_is_in_recovery*)
    n=$(cat %[1]s 2>/dev/null || echo 0); echo $((n+1)) > %[1]s
    if [ "%[2]t" = true ] || [ "$n" -lt 2 ]; then echo t; else echo f; fi ;;
  *datname*)
    if [ "%[3]t" = true ]; then :; else printf 'postgres\napp\n'; fi ;;
  *pg_class*)
    if [ "%[4]t" = true ]; then echo 0; else echo 5; fi ;;
esac
`, counter, f.stillRecover, f.noDatabases, f.emptyCatalog))
	}
	run := func(extra ...string) (map[string]any, int) {
		ExpectWithOffset(1, os.MkdirAll(filepath.Join(dir, "work"), 0o755)).To(Succeed()) //nolint:gosec // a test directory
		cmd := exec.Command("python3", "scripts/restore_verify.py")
		cmd.Env = append(os.Environ(), "PATH="+filepath.Join(dir, "bin")+":"+os.Getenv("PATH"),
			"PGBIN="+filepath.Join(dir, "bin"), "RESTORE_DIR="+filepath.Join(dir, "work"),
			"TERMINATION_LOG="+filepath.Join(dir, "out.json"), "POLL_SECONDS=0.05", "PGVERSION=17")
		cmd.Env = append(cmd.Env, extra...)
		out, err := cmd.CombinedOutput()
		code := 0
		if exitErr, ok := err.(*exec.ExitError); ok {
			code = exitErr.ExitCode()
		} else {
			ExpectWithOffset(1, err).NotTo(HaveOccurred(), string(out))
		}
		raw, readErr := os.ReadFile(filepath.Join(dir, "out.json"))
		ExpectWithOffset(1, readErr).NotTo(HaveOccurred())
		var report map[string]any
		ExpectWithOffset(1, json.Unmarshal(raw, &report)).To(Succeed())
		return report, code
	}
	calls := func() string {
		raw, _ := os.ReadFile(filepath.Join(dir, "calls.log"))
		return string(raw)
	}

	It("restores the newest backup, waits out the recovery, reads every database, and reports success", func() {
		setup(fakes{})
		report, code := run()
		Expect(code).To(BeZero())
		Expect(report["success"]).To(BeTrue())
		Expect(report["backup"]).To(Equal("base_02"), "the newest by finish time")
		Expect(report["databases"]).To(BeEquivalentTo(2))
		Expect(report).To(HaveKey("durationSeconds"))
		Expect(report).To(HaveKey("fetchSeconds"))
		Expect(report).To(HaveKey("replaySeconds"))
		Expect(calls()).To(ContainSubstring("wal-g backup-fetch " + filepath.Join(dir, "work", "data") + " base_02"))
		Expect(calls()).To(ContainSubstring("stop"), "the server is stopped afterwards")
	})

	It("configures the restored server so it cannot reach or affect anything outside the pod", func() {
		setup(fakes{})
		run()
		conf, err := os.ReadFile(filepath.Join(dir, "work", "data", "postgresql.auto.conf"))
		Expect(err).NotTo(HaveOccurred())
		for _, want := range []string{"restore_command = 'wal-g wal-fetch %f %p'", "listen_addresses = ''", "archive_mode = 'off'",
			"archive_command = '/bin/true'", "primary_conninfo = ''", "ssl = 'off'", "recovery_target_action = 'promote'"} {
			Expect(string(conf)).To(ContainSubstring(want))
		}
		Expect(filepath.Join(dir, "work", "data", "recovery.signal")).To(BeAnExistingFile())
		Expect(string(conf)).To(ContainSubstring(filepath.Join(dir, "work", "socket")))
	})

	It("fails, plainly, when there is no backup", func() {
		setup(fakes{backups: "[]"})
		report, code := run()
		Expect(code).To(Equal(1))
		Expect(report["success"]).To(BeFalse())
		Expect(report["message"]).To(ContainSubstring("no base backup"))
		Expect(calls()).NotTo(ContainSubstring("backup-fetch"))
	})

	It("fails when the backup cannot be fetched", func() {
		setup(fakes{fetchFails: true})
		report, code := run()
		Expect(code).To(Equal(1))
		Expect(report["message"]).To(ContainSubstring("backup-fetch failed: no such key"))
	})

	It("fails with the server's own words when PostgreSQL will not start", func() {
		setup(fakes{startFails: true})
		report, code := run()
		Expect(code).To(Equal(1))
		Expect(report["message"]).To(ContainSubstring("did not start"))
		Expect(report["message"]).To(ContainSubstring("could not open file"))
	})

	It("fails when recovery never finishes, and still stops the server", func() {
		setup(fakes{stillRecover: true})
		report, code := run("RECOVERY_TIMEOUT=1")
		Expect(code).To(Equal(1))
		Expect(report["message"]).To(ContainSubstring("recovery did not finish"))
		Expect(calls()).To(ContainSubstring("stop"))
	})

	It("fails when a database has an empty catalog", func() {
		setup(fakes{emptyCatalog: true})
		report, code := run()
		Expect(code).To(Equal(1))
		Expect(report["message"]).To(ContainSubstring("empty catalog"))
	})

	It("fails when the restored server holds no databases", func() {
		setup(fakes{noDatabases: true})
		report, _ := run()
		Expect(report["success"]).To(BeFalse())
		Expect(report["message"]).To(ContainSubstring("no databases"))
	})

	It("keeps its report small enough for a termination message", func() {
		setup(fakes{})
		run()
		raw, err := os.ReadFile(filepath.Join(dir, "out.json"))
		Expect(err).NotTo(HaveOccurred())
		Expect(len(raw)).To(BeNumerically("<", 1024))
	})
})

// The whole reconciler against the API server, with a finished restore-test pod
// standing in for a run.
var _ = Describe("Restore testing through the reconciler", func() {
	const namespace = "default"
	ctx := context.Background()
	key := func(name string) types.NamespacedName { return types.NamespacedName{Name: name, Namespace: namespace} }
	reconcileOnce := func(name string) {
		rec := &PostgresClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Patroni: &fakePatroni{live: map[string]any{}}}
		_, err := rec.Reconcile(ctx, reconcile.Request{NamespacedName: key(name)})
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
	}
	setup := func(name string, verification bool) {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name + "-s3", Namespace: namespace},
			StringData: map[string]string{secretKeyAccessKey: "id", secretKeySecretKey: testSecretValue},
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
					Enabled: true, Schedule: nightlyCron,
					Destination: &dbv1alpha1.BackupDestination{Bucket: testBucket, Region: testRegion, CredentialsSecret: name + "-s3"},
					Retention:   dbv1alpha1.BackupRetention{Full: 3, Days: 4},
					Verification: dbv1alpha1.BackupVerification{
						Enabled: verification, Schedule: weeklyCron,
					},
				},
			},
		}
		ExpectWithOffset(1, k8sClient.Create(ctx, c)).To(Succeed())
		DeferCleanup(func() { ExpectWithOffset(1, client.IgnoreNotFound(k8sClient.Delete(ctx, c))).To(Succeed()) })
		reconcileOnce(name)
	}
	finishedRun := func(name, message string, finished time.Time) {
		labels := restoreVerifyLabels(&dbv1alpha1.PostgresCluster{ObjectMeta: metav1.ObjectMeta{Name: name}})
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("%s-verify-%d", name, finished.Unix()), Namespace: namespace, Labels: labels},
			Spec:       corev1.PodSpec{RestartPolicy: corev1.RestartPolicyNever, Containers: []corev1.Container{{Name: verifyContainerName, Image: testPodImage}}},
		}
		ExpectWithOffset(1, k8sClient.Create(ctx, pod)).To(Succeed())
		DeferCleanup(func() {
			ExpectWithOffset(1, client.IgnoreNotFound(k8sClient.Delete(ctx, pod, client.GracePeriodSeconds(0)))).To(Succeed())
		})
		pod.Status.Phase = corev1.PodSucceeded
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name: verifyContainerName,
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

	It("creates the restore test CronJob only while verification is on", func() {
		const name = "rv-lifecycle"
		setup(name, true)
		cj := &batchv1.CronJob{}
		Expect(k8sClient.Get(ctx, key(name+"-restore-verify"), cj)).To(Succeed())
		Expect(cj.Spec.Schedule).To(Equal(weeklyCron))
		Expect(k8sClient.Get(ctx, key(name+"-backup-status"), &batchv1.CronJob{})).To(Succeed(), "the status check carries on")

		c := get(name)
		c.Spec.Backup.Verification.Enabled = false
		Expect(k8sClient.Update(ctx, c)).To(Succeed())
		reconcileOnce(name)
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key(name+"-restore-verify"), &batchv1.CronJob{}))).To(BeTrue())
		Expect(k8sClient.Get(ctx, key(name+"-backup-status"), &batchv1.CronJob{})).To(Succeed())
		Expect(meta.FindStatusCondition(get(name).Status.Conditions, "RestoreVerified")).To(BeNil())
	})

	It("creates no restore test when verification is off", func() {
		const name = "rv-off"
		setup(name, false)
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key(name+"-restore-verify"), &batchv1.CronJob{}))).To(BeTrue())
	})

	It("is Unknown until the first restore test has run", func() {
		const name = "rv-waiting"
		setup(name, true)
		c := get(name)
		cond := meta.FindStatusCondition(c.Status.Conditions, "RestoreVerified")
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionUnknown))
	})

	It("records a passing restore test in status, the condition and the metrics", func() {
		const name = "rv-pass"
		setup(name, true)
		now := time.Now()
		finishedRun(name, `{"success":true,"backup":"base_0A","durationSeconds":42,"fetchSeconds":10,"replaySeconds":30,"databases":2}`, now)
		reconcileOnce(name)

		c := get(name)
		v := c.Status.Backup.Verification
		Expect(v.LastRunTime).NotTo(BeNil())
		Expect(v.LastSuccessTime).NotTo(BeNil())
		Expect(v.DurationSeconds).To(Equal(int64(42)))
		Expect(v.Message).To(BeEmpty())
		cond := meta.FindStatusCondition(c.Status.Conditions, "RestoreVerified")
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(testutil.ToFloat64(restoreVerificationDuration.WithLabelValues(namespace, name))).To(Equal(float64(42)))
		Expect(testutil.ToFloat64(restoreVerificationLastSuccess.WithLabelValues(namespace, name))).To(BeNumerically(">", 0))
	})

	It("marks the cluster Degraded when the restore test failed, and keeps the last pass", func() {
		const name = "rv-fail"
		setup(name, true)
		first := time.Now().Add(-2 * time.Hour)
		finishedRun(name, `{"success":true,"backup":"base_0A","durationSeconds":42}`, first)
		reconcileOnce(name)
		finishedRun(name, `{"success":false,"message":"wal-g backup-fetch failed: access denied","durationSeconds":3}`, time.Now())
		reconcileOnce(name)

		c := get(name)
		Expect(meta.FindStatusCondition(c.Status.Conditions, "RestoreVerified").Status).To(Equal(metav1.ConditionFalse))
		Expect(meta.FindStatusCondition(c.Status.Conditions, "Degraded").Status).To(Equal(metav1.ConditionTrue))
		Expect(c.Status.Backup.Verification.Message).To(ContainSubstring("access denied"))
		Expect(c.Status.Backup.Verification.LastSuccessTime).NotTo(BeNil(), "the earlier pass is still on record")
		Expect(c.Status.Backup.Verification.DurationSeconds).To(Equal(int64(42)))
	})

	It("survives a termination message that is not a report", func() {
		const name = "rv-garbage"
		setup(name, true)
		finishedRun(name, "OOMKilled", time.Now())
		reconcileOnce(name)
		Expect(meta.FindStatusCondition(get(name).Status.Conditions, "RestoreVerified").Status).To(Equal(metav1.ConditionFalse))
	})
})
