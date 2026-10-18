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
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	dbv1alpha1 "github.com/tusharshrivastava62/postgres-operator/api/v1alpha1"
)

var (
	firstBackup = time.Date(2026, 10, 1, 2, 0, 0, 0, time.UTC)
	lastBackup  = time.Date(2026, 10, 8, 2, 0, 0, 0, time.UTC)
)

func okReport() *restoreCheckReport {
	r := &restoreCheckReport{Verdict: verdictOK}
	r.Backup = &struct {
		Name string    `json:"name"`
		Time time.Time `json:"time"`
	}{Name: "base_B", Time: lastBackup}
	r.Oldest = &struct {
		Name string    `json:"name"`
		Time time.Time `json:"time"`
	}{Name: "base_A", Time: firstBackup}
	return r
}

var _ = Describe("What the restore check found", func() {
	withTarget := func(at time.Time) *dbv1alpha1.PostgresCluster {
		c := recoveryCluster()
		c.Spec.Bootstrap.Recovery.TargetTime = ptr.To(metav1.NewTime(at))
		return c
	}

	It("is true when a backup can be started from, and names it", func() {
		c := recoverySourceCondition(recoveryCluster(), okReport())
		Expect(c.Type).To(Equal(conditionRecoverySourceReady))
		Expect(c.Status).To(Equal(metav1.ConditionTrue))
		Expect(c.Message).To(ContainSubstring("base_B"))
		Expect(c.Message).NotTo(ContainSubstring("replaying WAL up to"))
	})

	It("says how far the WAL will be replayed when there is a target", func() {
		c := recoverySourceCondition(withTarget(lastBackup.Add(time.Hour)), okReport())
		Expect(c.Message).To(ContainSubstring("replaying WAL up to 2026-10-08T03:00:00Z"))
	})

	It("is false for a target before the oldest backup, and says what to do about it", func() {
		report := &restoreCheckReport{Verdict: verdictTargetBeforeFirst, Oldest: okReport().Oldest}
		c := recoverySourceCondition(withTarget(firstBackup.Add(-time.Hour)), report)
		Expect(c.Status).To(Equal(metav1.ConditionFalse))
		Expect(c.Reason).To(Equal("TargetBeforeOldestBackup"))
		Expect(c.Message).To(ContainSubstring("targetTime 2026-10-01T01:00:00Z is earlier than the oldest base backup"))
		Expect(c.Message).To(ContainSubstring("base_A"))
		Expect(c.Message).To(ContainSubstring("s3://acme-db-backups/prod/orders/wal/17"), "where it looked")
		Expect(c.Message).To(ContainSubstring("targetTime of 2026-10-01T02:00:00Z or later"))
		Expect(c.Message).To(ContainSubstring("cannot be changed"), "the spec is immutable, so say how to proceed")
	})

	It("is false when the source has no backups at all", func() {
		c := recoverySourceCondition(recoveryCluster(), &restoreCheckReport{Verdict: verdictNoBackups})
		Expect(c.Reason).To(Equal("NoBackups"))
		Expect(c.Message).To(ContainSubstring("s3://acme-db-backups/prod/orders/wal/17"))
		Expect(c.Message).To(ContainSubstring("major version is 17"))
	})

	It("is false, with the store's own error, when the backups cannot be read", func() {
		c := recoverySourceCondition(recoveryCluster(), &restoreCheckReport{Verdict: verdictUnreadable, Error: "wal-g backup-list failed: AccessDenied"})
		Expect(c.Reason).To(Equal("SourceUnreadable"))
		Expect(c.Message).To(ContainSubstring("AccessDenied"))
	})

	It("is false, and says the check will run again, when no report came back", func() {
		c := recoverySourceCondition(recoveryCluster(), nil)
		Expect(c.Status).To(Equal(metav1.ConditionFalse))
		Expect(c.Reason).To(Equal("CheckFailed"))
		c = recoverySourceCondition(recoveryCluster(), &restoreCheckReport{Verdict: "something new"})
		Expect(c.Reason).To(Equal("CheckFailed"))
	})

	It("is not claimed ok by a report that names no backup", func() {
		Expect(recoverySourceCondition(recoveryCluster(), &restoreCheckReport{Verdict: verdictOK}).Status).
			To(Equal(metav1.ConditionFalse))
	})
})

var _ = Describe("The restore check Job", func() {
	cluster := func() *dbv1alpha1.PostgresCluster {
		c := recoveryCluster()
		c.Spec.Bootstrap.Recovery.TargetTime = ptr.To(metav1.NewTime(time.Date(2026, 10, 5, 12, 30, 0, 0, time.FixedZone("x", 3600))))
		return c
	}

	It("reads the source's backups, not the cluster's own", func() {
		c := cluster()
		c.Spec.Backup = backupCluster().Spec.Backup
		env := envMap(restoreCheckJob(c, "img").Template.Spec.Containers[0].Env)
		Expect(env["WALG_S3_PREFIX"].Value).To(Equal("s3://acme-db-backups/prod/orders/wal/17"))
		Expect(env["AWS_ACCESS_KEY_ID"].ValueFrom.SecretKeyRef.Name).To(Equal("source-s3"))
	})

	It("hands the target over in UTC", func() {
		env := envMap(restoreCheckJob(cluster(), "img").Template.Spec.Containers[0].Env)
		Expect(env["TARGET_TIME"].Value).To(Equal("2026-10-05T11:30:00Z"))
	})

	It("has no target to hand over when none is set", func() {
		Expect(envMap(restoreCheckJob(recoveryCluster(), "img").Template.Spec.Containers[0].Env)).NotTo(HaveKey("TARGET_TIME"))
	})

	It("uses the source's encryption key", func() {
		c := recoveryCluster()
		c.Spec.Bootstrap.Recovery.Encryption = &dbv1alpha1.BackupEncryption{KeySecret: "source-key"}
		env := envMap(restoreCheckJob(c, "img").Template.Spec.Containers[0].Env)
		Expect(env["WALG_LIBSODIUM_KEY"].ValueFrom.SecretKeyRef.Name).To(Equal("source-key"))
		Expect(env["WALG_LIBSODIUM_KEY_TRANSFORM"].Value).To(Equal("hex"))
	})

	It("runs as the database user with the strictest settings, never retried, and with no API access", func() {
		spec := restoreCheckJob(cluster(), "registry.example/spilo:1")
		pod := spec.Template.Spec
		Expect(pod.Containers[0].Image).To(Equal("registry.example/spilo:1"))
		Expect(*pod.SecurityContext.RunAsNonRoot).To(BeTrue())
		Expect(*pod.Containers[0].SecurityContext.AllowPrivilegeEscalation).To(BeFalse())
		Expect(*pod.AutomountServiceAccountToken).To(BeFalse())
		Expect(*spec.BackoffLimit).To(BeZero())
		Expect(*spec.ActiveDeadlineSeconds).To(BeNumerically(">", 0))
	})

	It("is labelled as a restore check, apart from the database", func() {
		Expect(restoreCheckJob(cluster(), "img").Template.Labels[labelAppComponent]).To(Equal(componentRestoreCheck))
		Expect(clusterForObject(context.Background(), &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Namespace: sourceNamespace, Labels: restoreCheckLabels(cluster()),
		}})).NotTo(BeEmpty(), "its pod finishing wakes the reconciler")
	})
})

var _ = Describe("The restore check script", func() {
	var dir string

	BeforeEach(func() {
		if _, err := exec.LookPath("python3"); err != nil {
			Skip("python3 is not available")
		}
		dir = GinkgoT().TempDir()
	})

	// fakeWalg answers backup-list with the given JSON, or fails.
	fakeWalg := func(listing string, fails bool, sleep int) {
		body := fmt.Sprintf("#!/bin/sh\nif [ %d -gt 0 ]; then sleep %d; fi\nif [ \"%t\" = true ]; then echo \"AccessDenied: no\" >&2; exit 1; fi\necho '%s'\n",
			sleep, sleep, fails, listing)
		ExpectWithOffset(1, os.MkdirAll(filepath.Join(dir, "bin"), 0o755)).To(Succeed())                         //nolint:gosec // a test directory
		ExpectWithOffset(1, os.WriteFile(filepath.Join(dir, "bin", "wal-g"), []byte(body), 0o755)).To(Succeed()) //nolint:gosec // test scripts must be executable
	}
	run := func(extra ...string) map[string]any {
		cmd := exec.Command("python3", "scripts/restore_check.py")
		cmd.Env = append(os.Environ(), "PATH="+filepath.Join(dir, "bin")+":"+os.Getenv("PATH"),
			"TERMINATION_LOG="+filepath.Join(dir, "out.json"))
		cmd.Env = append(cmd.Env, extra...)
		out, err := cmd.CombinedOutput()
		ExpectWithOffset(1, err).NotTo(HaveOccurred(), string(out))
		raw, readErr := os.ReadFile(filepath.Join(dir, "out.json"))
		ExpectWithOffset(1, readErr).NotTo(HaveOccurred())
		var report map[string]any
		ExpectWithOffset(1, json.Unmarshal(raw, &report)).To(Succeed())
		return report
	}
	// Two backups. "time" is when the record was written, which is what Spilo
	// compares with the target; "finish_time" is earlier and must not be used.
	listing := `[{"backup_name":"base_A","time":"2026-10-01T02:00:05Z","finish_time":"2026-10-01T01:59:50Z"},` +
		`{"backup_name":"base_B","time":"2026-10-08T02:00:05.123456789Z","finish_time":"2026-10-08T01:59:50Z"}]`

	It("chooses the newest backup when there is no target", func() {
		fakeWalg(listing, false, 0)
		report := run()
		Expect(report["verdict"]).To(Equal("ok"))
		Expect(report["backup"]).To(HaveKeyWithValue("name", "base_B"))
		Expect(report["oldest"]).To(HaveKeyWithValue("name", "base_A"))
	})

	It("chooses the newest backup written before the target", func() {
		fakeWalg(listing, false, 0)
		report := run("TARGET_TIME=2026-10-05T00:00:00Z")
		Expect(report["verdict"]).To(Equal("ok"))
		Expect(report["backup"]).To(HaveKeyWithValue("name", "base_A"))
	})

	It("reports a target before every backup, with the oldest one", func() {
		fakeWalg(listing, false, 0)
		report := run("TARGET_TIME=2026-09-30T00:00:00Z")
		Expect(report["verdict"]).To(Equal("target-before-oldest"))
		Expect(report["oldest"]).To(HaveKeyWithValue("name", "base_A"))
		Expect(report["oldest"]).To(HaveKeyWithValue("time", "2026-10-01T02:00:05Z"))
	})

	It("treats a target at the very moment a backup was written as too early, as Spilo does", func() {
		fakeWalg(listing, false, 0)
		Expect(run("TARGET_TIME=2026-10-01T02:00:05Z")["verdict"]).To(Equal("target-before-oldest"))
		Expect(run("TARGET_TIME=2026-10-01T02:00:06Z")["verdict"]).To(Equal("ok"))
	})

	It("compares times in the target's own zone correctly", func() {
		fakeWalg(listing, false, 0)
		Expect(run("TARGET_TIME=2026-10-01T04:00:06+02:00")["verdict"]).To(Equal("ok"))
		Expect(run("TARGET_TIME=2026-10-01T04:00:05+02:00")["verdict"]).To(Equal("target-before-oldest"))
	})

	It("reports an empty bucket", func() {
		fakeWalg("[]", false, 0)
		Expect(run()["verdict"]).To(Equal("no-backups"))
		fakeWalg("", false, 0)
		Expect(run()["verdict"]).To(Equal("no-backups"))
	})

	It("reports a bucket it cannot read, with the error", func() {
		fakeWalg("", true, 0)
		report := run()
		Expect(report["verdict"]).To(Equal("unreadable"))
		Expect(report["error"]).To(ContainSubstring("AccessDenied"))
	})

	It("reports a store that does not answer instead of hanging", func() {
		fakeWalg(listing, false, 5)
		report := run("COMMAND_TIMEOUT=1")
		Expect(report["verdict"]).To(Equal("unreadable"))
		Expect(report["error"]).To(ContainSubstring("timed out"))
	})
})

// finishRestoreCheck stands in for the Job controller, which envtest does not
// run: it marks the check Job finished and adds the pod that ran it, with the
// given termination message.
func finishRestoreCheck(ctx context.Context, name, message string, finished time.Time) {
	const namespace = "default"
	GinkgoHelper()
	job := &batchv1.Job{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name + "-restore-check"}, job)).To(Succeed())
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("%s-restore-check-%d", name, finished.UnixNano()), Namespace: namespace,
			Labels: restoreCheckLabels(&dbv1alpha1.PostgresCluster{ObjectMeta: metav1.ObjectMeta{Name: name}}),
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "batch/v1", Kind: "Job", Name: job.Name, UID: job.UID, Controller: ptr.To(true),
			}},
		},
		Spec: corev1.PodSpec{RestartPolicy: corev1.RestartPolicyNever, Containers: []corev1.Container{{Name: checkContainerName, Image: testPodImage}}},
	}
	Expect(k8sClient.Create(ctx, pod)).To(Succeed())
	DeferCleanup(func() {
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, pod, client.GracePeriodSeconds(0)))).To(Succeed())
	})
	pod.Status.Phase = corev1.PodSucceeded
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: checkContainerName,
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			Message: message, FinishedAt: metav1.NewTime(finished),
		}},
	}}
	Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())

	job.Status.StartTime = ptr.To(metav1.NewTime(finished.Add(-10 * time.Second)))
	job.Status.CompletionTime = ptr.To(metav1.NewTime(finished))
	job.Status.Succeeded = 1
	job.Status.Conditions = []batchv1.JobCondition{
		{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(finished)},
		{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(finished)},
	}
	Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())
}

const okRestoreCheck = `{"verdict":"ok","backup":{"name":"base_B","time":"2026-10-08T02:00:05Z"},"oldest":{"name":"base_A","time":"2026-10-01T02:00:05Z"},"count":2}`

var _ = Describe("Restoring a cluster through the reconciler", func() {
	const namespace = "default"
	ctx := context.Background()
	key := func(name string) types.NamespacedName { return types.NamespacedName{Name: name, Namespace: namespace} }
	reconcileOnce := func(name string) reconcile.Result {
		rec := &PostgresClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Patroni: &fakePatroni{live: map[string]any{}}}
		res, err := rec.Reconcile(ctx, reconcile.Request{NamespacedName: key(name)})
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		return res
	}
	newRestore := func(name string) {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name + "-src", Namespace: namespace},
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
				Bootstrap: &dbv1alpha1.BootstrapSpec{Recovery: &dbv1alpha1.RecoverySpec{
					SourceCluster: sourceName,
					Destination:   dbv1alpha1.BackupDestination{Bucket: testBucket, Region: testRegion, CredentialsSecret: name + "-src"},
					TargetTime:    ptr.To(metav1.NewTime(firstBackup.Add(-time.Hour))),
				}},
			},
		}
		ExpectWithOffset(1, k8sClient.Create(ctx, c)).To(Succeed())
		DeferCleanup(func() { ExpectWithOffset(1, client.IgnoreNotFound(k8sClient.Delete(ctx, c))).To(Succeed()) })
	}
	get := func(name string) *dbv1alpha1.PostgresCluster {
		c := &dbv1alpha1.PostgresCluster{}
		ExpectWithOffset(1, k8sClient.Get(ctx, key(name), c)).To(Succeed())
		return c
	}
	cond := func(name, t string) *metav1.Condition {
		return meta.FindStatusCondition(get(name).Status.Conditions, t)
	}
	noStatefulSet := func(name string) {
		ExpectWithOffset(1, apierrors.IsNotFound(k8sClient.Get(ctx, key(name), &appsv1.StatefulSet{}))).To(BeTrue(),
			"no database pod may be created while the restore cannot work")
	}

	It("checks the backups first, creating nothing else", func() {
		const name = "rc-first"
		newRestore(name)
		res := reconcileOnce(name)

		Expect(res.RequeueAfter).To(Equal(restoreCheckPollDelay))
		noStatefulSet(name)
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key(name+"-credentials"), &corev1.Secret{}))).To(BeTrue())
		job := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, key(name+"-restore-check"), job)).To(Succeed())
		Expect(job.OwnerReferences).NotTo(BeEmpty(), "it goes with the cluster")
		Expect(k8sClient.Get(ctx, key(name+"-backup-scripts"), &corev1.ConfigMap{})).To(Succeed())
		source := cond(name, conditionRecoverySourceReady)
		Expect(source.Status).To(Equal(metav1.ConditionUnknown))
		Expect(source.Reason).To(Equal("Checking"))
		Expect(cond(name, "Progressing").Reason).To(Equal("CheckingRecoverySource"))
	})

	It("keeps waiting while the check is running", func() {
		const name = "rc-running"
		newRestore(name)
		reconcileOnce(name)
		res := reconcileOnce(name)
		Expect(res.RequeueAfter).To(Equal(restoreCheckPollDelay))
		noStatefulSet(name)
	})

	It("reports a target before the oldest backup in the conditions, and creates no pods", func() {
		const name = "rc-early"
		newRestore(name)
		reconcileOnce(name)
		finishRestoreCheck(ctx, name,
			`{"verdict":"target-before-oldest","oldest":{"name":"base_A","time":"2026-10-01T02:00:05Z"}}`, time.Now())
		res := reconcileOnce(name)

		noStatefulSet(name)
		Expect(res.RequeueAfter).To(BeNumerically(">", 0))
		Expect(res.RequeueAfter).To(BeNumerically("<=", restoreCheckRecheckDelay))
		source := cond(name, conditionRecoverySourceReady)
		Expect(source.Status).To(Equal(metav1.ConditionFalse))
		Expect(source.Reason).To(Equal("TargetBeforeOldestBackup"))
		Expect(source.Message).To(ContainSubstring("is earlier than the oldest base backup"))
		progressing := cond(name, "Progressing")
		Expect(progressing.Status).To(Equal(metav1.ConditionFalse))
		Expect(progressing.Reason).To(Equal("RecoverySourceProblem"))
	})

	It("runs the check again after a while, in case the bucket or a Secret has been fixed", func() {
		const name = "rc-retry"
		newRestore(name)
		reconcileOnce(name)
		finishRestoreCheck(ctx, name, `{"verdict":"unreadable","error":"AccessDenied"}`,
			time.Now().Add(-restoreCheckRecheckDelay-time.Second))
		old := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, key(name+"-restore-check"), old)).To(Succeed())

		reconcileOnce(name)
		Eventually(func() bool {
			err := k8sClient.Get(ctx, key(name+"-restore-check"), &batchv1.Job{})
			return apierrors.IsNotFound(err)
		}).Should(BeTrue(), "the finished check is removed")
		noStatefulSet(name)

		reconcileOnce(name)
		fresh := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, key(name+"-restore-check"), fresh)).To(Succeed())
		Expect(fresh.UID).NotTo(Equal(old.UID), "and a new one runs")
	})

	It("is not misled by the pod of a check that was removed to be run again", func() {
		const name = "rc-stale"
		newRestore(name)
		reconcileOnce(name)
		finishRestoreCheck(ctx, name, `{"verdict":"no-backups"}`, time.Now().Add(-time.Hour))
		reconcileOnce(name) // removes the old Job, leaving its pod behind
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, key(name+"-restore-check"), &batchv1.Job{}))
		}).Should(BeTrue())
		reconcileOnce(name) // a new Job

		// The new check dies without leaving a report. The old pod still
		// carries one, and must not be read as the answer.
		job := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, key(name+"-restore-check"), job)).To(Succeed())
		now := metav1.Now()
		job.Status.StartTime = &now
		job.Status.Failed = 1
		job.Status.Conditions = []batchv1.JobCondition{
			{Type: batchv1.JobFailureTarget, Status: corev1.ConditionTrue, Reason: "DeadlineExceeded", LastTransitionTime: now},
			{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "DeadlineExceeded", LastTransitionTime: now},
		}
		Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())
		reconcileOnce(name)
		Expect(cond(name, conditionRecoverySourceReady).Reason).To(Equal("CheckFailed"))
	})

	It("creates the cluster once the check passes, and says which backup it starts from", func() {
		const name = "rc-ok"
		newRestore(name)
		reconcileOnce(name)
		finishRestoreCheck(ctx, name, okRestoreCheck, time.Now())
		reconcileOnce(name)

		Expect(k8sClient.Get(ctx, key(name), &appsv1.StatefulSet{})).To(Succeed())
		source := cond(name, conditionRecoverySourceReady)
		Expect(source.Status).To(Equal(metav1.ConditionTrue))
		Expect(source.Message).To(ContainSubstring("base_B"))
		Expect(cond(name, "FailoverInProgress").Reason).To(Equal("Bootstrapping"))
	})

	It("does not check again once the cluster exists, so the source cannot stop a member restarting", func() {
		const name = "rc-after"
		newRestore(name)
		reconcileOnce(name)
		finishRestoreCheck(ctx, name, okRestoreCheck, time.Now())
		reconcileOnce(name)
		Expect(k8sClient.Delete(ctx, &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name + "-restore-check", Namespace: namespace}},
			client.PropagationPolicy(metav1.DeletePropagationBackground))).To(Succeed())

		reconcileOnce(name)
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key(name+"-restore-check"), &batchv1.Job{}))).To(BeTrue())
		Expect(cond(name, conditionRecoverySourceReady).Status).To(Equal(metav1.ConditionTrue))
	})

	It("does not run for a cluster that starts empty", func() {
		const name = "rc-none"
		c := &dbv1alpha1.PostgresCluster{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: dbv1alpha1.PostgresClusterSpec{
				Version: "16", Instances: 1,
				Storage:   dbv1alpha1.StorageSpec{Size: resource.MustParse("1Gi")},
				Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}},
			},
		}
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, c))).To(Succeed()) })
		reconcileOnce(name)
		Expect(k8sClient.Get(ctx, key(name), &appsv1.StatefulSet{})).To(Succeed())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key(name+"-restore-check"), &batchv1.Job{}))).To(BeTrue())
		Expect(cond(name, conditionRecoverySourceReady)).To(BeNil())
	})
})
