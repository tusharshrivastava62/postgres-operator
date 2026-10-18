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
	"strings"
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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	dbv1alpha1 "github.com/tusharshrivastava62/postgres-operator/api/v1alpha1"
)

// testBucket is the bucket name the backup specs use.
const testBucket = "acme-db-backups"

// testSecretValue is a stand-in secret access key.
const testSecretValue = "secret"

// testRegion is the region the backup specs use.
const testRegion = "eu-west-1"

// Names shared by the backup and restore specs.
const (
	sourceName = "orders"
	teamPath   = "team/orders-db"
)

const validHexKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func envMap(env []corev1.EnvVar) map[string]corev1.EnvVar {
	out := map[string]corev1.EnvVar{}
	for _, e := range env {
		out[e.Name] = e
	}
	return out
}

func backupCluster() *dbv1alpha1.PostgresCluster {
	return &dbv1alpha1.PostgresCluster{
		ObjectMeta: metav1.ObjectMeta{Name: sourceName, Namespace: "prod"},
		Spec: dbv1alpha1.PostgresClusterSpec{
			Version: "17",
			Backup: dbv1alpha1.BackupSpec{
				Enabled: true, Schedule: nightlyCron,
				Destination: &dbv1alpha1.BackupDestination{
					Bucket: testBucket, Region: testRegion, CredentialsSecret: "backup-s3",
				},
				Retention: dbv1alpha1.BackupRetention{Full: 7, Days: 14},
			},
		},
	}
}

var _ = Describe("The backup environment", func() {
	It("is empty when backups are off", func() {
		c := backupCluster()
		c.Spec.Backup.Enabled = false
		Expect(backupEnv(c)).To(BeEmpty())
		Expect(envMap(mustEnv(c))).NotTo(HaveKey("USE_WALG_BACKUP"))
	})

	It("turns on wal-g archiving, restore and the schedule", func() {
		env := envMap(backupEnv(backupCluster()))
		Expect(env["USE_WALG_BACKUP"].Value).To(Equal("true"))
		Expect(env["USE_WALG_RESTORE"].Value).To(Equal("true"))
		Expect(env["BACKUP_SCHEDULE"].Value).To(Equal(nightlyCron))
		Expect(env["AWS_REGION"].Value).To(Equal(testRegion))
	})

	It("writes the storage prefix out in full, with the default path and the major version", func() {
		env := envMap(backupEnv(backupCluster()))
		Expect(env["WALG_S3_PREFIX"].Value).To(Equal("s3://acme-db-backups/prod/orders/wal/17"))
	})

	It("uses the path from the spec when there is one", func() {
		c := backupCluster()
		c.Spec.Backup.Destination.Path = teamPath
		Expect(envMap(backupEnv(c))["WALG_S3_PREFIX"].Value).To(Equal("s3://acme-db-backups/team/orders-db/wal/17"))
	})

	It("reads the credentials from the named Secret, never as literal values", func() {
		env := envMap(backupEnv(backupCluster()))
		for _, name := range []string{secretKeyAccessKey, secretKeySecretKey} {
			Expect(env[name].Value).To(BeEmpty())
			Expect(env[name].ValueFrom.SecretKeyRef.Name).To(Equal("backup-s3"))
			Expect(env[name].ValueFrom.SecretKeyRef.Key).To(Equal(name))
		}
	})

	It("refuses to overwrite WAL that is already archived with different content", func() {
		Expect(envMap(backupEnv(backupCluster()))["WALG_PREVENT_WAL_OVERWRITE"].Value).To(Equal("true"))
	})

	DescribeTable("a custom endpoint, in the form Spilo reads",
		func(forcePathStyle bool, want string) {
			c := backupCluster()
			c.Spec.Backup.Destination.Endpoint = "https://s3.example.com:9000"
			c.Spec.Backup.Destination.ForcePathStyle = forcePathStyle
			env := envMap(backupEnv(c))
			Expect(env["AWS_ENDPOINT"].Value).To(Equal("https://s3.example.com:9000"))
			Expect(env["WALG_S3_ENDPOINT"].Value).To(Equal(want))
			Expect(env["WALG_DISABLE_S3_SSE"].Value).To(Equal("true"), "S3-compatible stores generally reject server-side encryption")
		},
		Entry("path style", true, "https+path://s3.example.com:9000"),
		Entry("virtual-host style", false, "https+virtual://s3.example.com:9000"),
	)

	It("sets no endpoint for Amazon S3", func() {
		env := envMap(backupEnv(backupCluster()))
		Expect(env).NotTo(HaveKey("AWS_ENDPOINT"))
		Expect(env).NotTo(HaveKey("WALG_S3_ENDPOINT"))
		Expect(env).NotTo(HaveKey("WALG_DISABLE_S3_SSE"), "Amazon S3 supports server-side encryption")
	})

	It("reads the encryption key from its Secret and says it is hexadecimal", func() {
		c := backupCluster()
		c.Spec.Backup.Encryption = &dbv1alpha1.BackupEncryption{KeySecret: "backup-key"}
		env := envMap(backupEnv(c))
		Expect(env["WALG_LIBSODIUM_KEY"].ValueFrom.SecretKeyRef.Name).To(Equal("backup-key"))
		Expect(env["WALG_LIBSODIUM_KEY"].ValueFrom.SecretKeyRef.Key).To(Equal(secretKeyEncKey))
		Expect(env["WALG_LIBSODIUM_KEY_TRANSFORM"].Value).To(Equal("hex"))
	})

	It("sets no encryption variables without encryption", func() {
		Expect(envMap(backupEnv(backupCluster()))).NotTo(HaveKey("WALG_LIBSODIUM_KEY"))
	})

	It("tells Spilo to keep everything, since retention is the operator's job", func() {
		Expect(envMap(backupEnv(backupCluster()))["BACKUP_NUM_TO_RETAIN"].Value).To(Equal("1000"))
	})

	It("is part of the pod template, so changing it replaces the pods", func() {
		c := backupCluster()
		c.Spec.Resources = corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}}
		before, err := desiredStatefulSet(c, "img")
		Expect(err).NotTo(HaveOccurred())
		c.Spec.Backup.Schedule = "0 3 * * *"
		after, err := desiredStatefulSet(c, "img")
		Expect(err).NotTo(HaveOccurred())
		Expect(after.Spec.Template.Spec.Containers[0].Env).NotTo(Equal(before.Spec.Template.Spec.Containers[0].Env))
	})
})

func mustEnv(c *dbv1alpha1.PostgresCluster) []corev1.EnvVar {
	env, err := spiloEnv(c)
	Expect(err).NotTo(HaveOccurred())
	return env
}

func recoveryCluster() *dbv1alpha1.PostgresCluster {
	c := &dbv1alpha1.PostgresCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "orders-restored", Namespace: "prod"},
		Spec: dbv1alpha1.PostgresClusterSpec{
			Version: "17",
			Bootstrap: &dbv1alpha1.BootstrapSpec{Recovery: &dbv1alpha1.RecoverySpec{
				SourceCluster: sourceName,
				Destination:   dbv1alpha1.BackupDestination{Bucket: testBucket, Region: testRegion, CredentialsSecret: "source-s3"},
			}},
		},
	}
	return c
}

var _ = Describe("The recovery environment", func() {
	env := func(c *dbv1alpha1.PostgresCluster) map[string]corev1.EnvVar { return envMap(recoveryEnv(c)) }

	It("is empty for a cluster that starts empty", func() {
		Expect(recoveryEnv(backupCluster())).To(BeEmpty())
	})

	It("asks Spilo to clone from the source's backups", func() {
		e := env(recoveryCluster())
		Expect(e["CLONE_METHOD"].Value).To(Equal("CLONE_WITH_WALG"))
		Expect(e["CLONE_SCOPE"].Value).To(Equal("orders"))
	})

	It("names the source's storage prefix exactly as the source built it", func() {
		Expect(env(recoveryCluster())["CLONE_WALG_S3_PREFIX"].Value).To(Equal("s3://acme-db-backups/prod/orders/wal/17"))
	})

	It("takes the source's namespace and path into account", func() {
		c := recoveryCluster()
		c.Spec.Bootstrap.Recovery.SourceNamespace = "staging"
		Expect(env(c)["CLONE_WALG_S3_PREFIX"].Value).To(Equal("s3://acme-db-backups/staging/orders/wal/17"))
		c.Spec.Bootstrap.Recovery.Destination.Path = teamPath
		Expect(env(c)["CLONE_WALG_S3_PREFIX"].Value).To(Equal("s3://acme-db-backups/team/orders-db/wal/17"))
	})

	It("reads the source's credentials from the named Secret", func() {
		e := env(recoveryCluster())
		for _, name := range []string{"CLONE_AWS_ACCESS_KEY_ID", "CLONE_AWS_SECRET_ACCESS_KEY"} {
			Expect(e[name].Value).To(BeEmpty())
			Expect(e[name].ValueFrom.SecretKeyRef.Name).To(Equal("source-s3"))
		}
		Expect(e["CLONE_AWS_ACCESS_KEY_ID"].ValueFrom.SecretKeyRef.Key).To(Equal(secretKeyAccessKey))
		Expect(e["CLONE_AWS_REGION"].Value).To(Equal(testRegion))
	})

	It("addresses a custom endpoint and switches off server-side encryption, as for backups", func() {
		c := recoveryCluster()
		c.Spec.Bootstrap.Recovery.Destination.Endpoint = "http://s3:8333"
		c.Spec.Bootstrap.Recovery.Destination.ForcePathStyle = true
		e := env(c)
		Expect(e["CLONE_AWS_ENDPOINT"].Value).To(Equal("http://s3:8333"))
		Expect(e["CLONE_WALG_S3_ENDPOINT"].Value).To(Equal("http+path://s3:8333"))
		Expect(e["CLONE_WALG_DISABLE_S3_SSE"].Value).To(Equal("true"))
	})

	It("passes the source's encryption key, as hex", func() {
		c := recoveryCluster()
		c.Spec.Bootstrap.Recovery.Encryption = &dbv1alpha1.BackupEncryption{KeySecret: "source-key"}
		e := env(c)
		Expect(e["CLONE_WALG_LIBSODIUM_KEY"].ValueFrom.SecretKeyRef.Name).To(Equal("source-key"))
		Expect(e["CLONE_WALG_LIBSODIUM_KEY_TRANSFORM"].Value).To(Equal("hex"))
	})

	It("sets no target time, so the latest is restored, unless one is given", func() {
		Expect(env(recoveryCluster())).NotTo(HaveKey("CLONE_TARGET_TIME"))
	})

	DescribeTable("the target time, with an explicit offset as the restore script requires",
		func(given time.Time, want string) {
			c := recoveryCluster()
			t := metav1.NewTime(given)
			c.Spec.Bootstrap.Recovery.TargetTime = &t
			Expect(env(c)["CLONE_TARGET_TIME"].Value).To(Equal(want))
		},
		Entry("UTC", time.Date(2026, 10, 18, 7, 49, 27, 0, time.UTC), "2026-10-18T07:49:27+00:00"),
		Entry("another zone, converted to UTC", time.Date(2026, 10, 18, 9, 49, 27, 0, time.FixedZone("CEST", 2*3600)), "2026-10-18T07:49:27+00:00"),
	)

	It("is part of the pod environment alongside the cluster's own backup settings", func() {
		c := recoveryCluster()
		c.Spec.Backup = backupCluster().Spec.Backup
		e := envMap(mustEnv(c))
		Expect(e).To(HaveKey("CLONE_METHOD"))
		Expect(e).To(HaveKey("USE_WALG_BACKUP"))
		Expect(e["WALG_S3_PREFIX"].Value).To(Equal("s3://acme-db-backups/prod/orders-restored/wal/17"),
			"its own backups go to its own path, never the source's")
	})

	It("is present even when the restored cluster takes no backups of its own", func() {
		Expect(envMap(mustEnv(recoveryCluster()))).To(HaveKey("CLONE_METHOD"))
	})
})

var _ = Describe("Backups through the reconciler", func() {
	const namespace = "default"
	ctx := context.Background()
	key := func(name string) types.NamespacedName { return types.NamespacedName{Name: name, Namespace: namespace} }
	reconcileOnce := func(name string) reconcile.Result {
		rec := &PostgresClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Patroni: &fakePatroni{live: map[string]any{}}}
		res, err := rec.Reconcile(ctx, reconcile.Request{NamespacedName: key(name)})
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		return res
	}
	newCluster := func(name string, mutate func(*dbv1alpha1.PostgresCluster)) {
		c := &dbv1alpha1.PostgresCluster{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: dbv1alpha1.PostgresClusterSpec{
				Version: "16", Instances: 1,
				Storage:   dbv1alpha1.StorageSpec{Size: resource.MustParse("1Gi")},
				Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}},
				Backup: dbv1alpha1.BackupSpec{
					Enabled: true, Schedule: nightlyCron,
					Destination: &dbv1alpha1.BackupDestination{Bucket: testBucket, Region: testRegion, CredentialsSecret: name + "-s3"},
				},
			},
		}
		if mutate != nil {
			mutate(c)
		}
		ExpectWithOffset(1, k8sClient.Create(ctx, c)).To(Succeed())
		DeferCleanup(func() { ExpectWithOffset(1, client.IgnoreNotFound(k8sClient.Delete(ctx, c))).To(Succeed()) })
	}
	createSecret := func(name string, data map[string]string) {
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}, StringData: data}
		ExpectWithOffset(1, k8sClient.Create(ctx, secret)).To(Succeed())
		DeferCleanup(func() { ExpectWithOffset(1, client.IgnoreNotFound(k8sClient.Delete(ctx, secret))).To(Succeed()) })
	}
	goodCreds := map[string]string{secretKeyAccessKey: "id", secretKeySecretKey: testSecretValue}
	progressing := func(name string) *metav1.Condition {
		c := &dbv1alpha1.PostgresCluster{}
		ExpectWithOffset(1, k8sClient.Get(ctx, key(name), c)).To(Succeed())
		return meta.FindStatusCondition(c.Status.Conditions, "Progressing")
	}
	stsEnv := func(name string) map[string]corev1.EnvVar {
		sts := &appsv1.StatefulSet{}
		ExpectWithOffset(1, k8sClient.Get(ctx, key(name), sts)).To(Succeed())
		return envMap(sts.Spec.Template.Spec.Containers[0].Env)
	}

	It("waits, changing nothing, while the credentials Secret does not exist", func() {
		const name = "bk-nosecret"
		newCluster(name, nil)
		res := reconcileOnce(name)

		Expect(res.RequeueAfter).To(Equal(backupSecretRecheckDelay))
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key(name), &appsv1.StatefulSet{}))).To(BeTrue())
		cond := progressing(name)
		Expect(cond.Reason).To(Equal("BackupSecretProblem"))
		Expect(cond.Message).To(ContainSubstring(name + "-s3"))
	})

	It("carries on once the Secret appears", func() {
		const name = "bk-secretlater"
		newCluster(name, nil)
		reconcileOnce(name)
		createSecret(name+"-s3", goodCreds)
		reconcileOnce(name)

		env := stsEnv(name)
		Expect(env["USE_WALG_BACKUP"].Value).To(Equal("true"))
		Expect(env["WALG_S3_PREFIX"].Value).To(Equal("s3://acme-db-backups/default/" + name + "/wal/16"))
	})

	It("rejects a credentials Secret that lacks a key", func() {
		const name = "bk-halfsecret"
		createSecret(name+"-s3", map[string]string{"AWS_ACCESS_KEY_ID": "id"})
		newCluster(name, nil)
		reconcileOnce(name)
		Expect(progressing(name).Message).To(ContainSubstring("AWS_SECRET_ACCESS_KEY"))
	})

	It("rejects an encryption key that is not 64 hexadecimal characters", func() {
		const name = "bk-badkey"
		createSecret(name+"-s3", goodCreds)
		createSecret(name+"-key", map[string]string{secretKeyEncKey: "not-hex"})
		newCluster(name, func(c *dbv1alpha1.PostgresCluster) {
			c.Spec.Backup.Encryption = &dbv1alpha1.BackupEncryption{KeySecret: name + "-key"}
		})
		reconcileOnce(name)
		Expect(progressing(name).Message).To(ContainSubstring("64 hexadecimal characters"))
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key(name), &appsv1.StatefulSet{}))).To(BeTrue())
	})

	It("accepts a good encryption key and passes it on", func() {
		const name = "bk-goodkey"
		createSecret(name+"-s3", goodCreds)
		createSecret(name+"-key", map[string]string{secretKeyEncKey: validHexKey})
		newCluster(name, func(c *dbv1alpha1.PostgresCluster) {
			c.Spec.Backup.Encryption = &dbv1alpha1.BackupEncryption{KeySecret: name + "-key"}
		})
		reconcileOnce(name)
		Expect(stsEnv(name)["WALG_LIBSODIUM_KEY_TRANSFORM"].Value).To(Equal("hex"))
	})

	It("waits, changing nothing, while the restore credentials Secret does not exist", func() {
		const name = "bk-restore-nosecret"
		newCluster(name, func(c *dbv1alpha1.PostgresCluster) {
			c.Spec.Backup = dbv1alpha1.BackupSpec{}
			c.Spec.Bootstrap = &dbv1alpha1.BootstrapSpec{Recovery: &dbv1alpha1.RecoverySpec{
				SourceCluster: sourceName,
				Destination:   dbv1alpha1.BackupDestination{Bucket: testBucket, Region: testRegion, CredentialsSecret: name + "-src"},
			}}
		})
		reconcileOnce(name)
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key(name), &appsv1.StatefulSet{}))).To(BeTrue())
		cond := progressing(name)
		Expect(cond.Reason).To(Equal("BackupSecretProblem"))
		Expect(cond.Message).To(ContainSubstring("restore credentials"))
	})

	It("checks the restore encryption key too", func() {
		const name = "bk-restore-badkey"
		createSecret(name+"-src", goodCreds)
		createSecret(name+"-srckey", map[string]string{secretKeyEncKey: "short"})
		newCluster(name, func(c *dbv1alpha1.PostgresCluster) {
			c.Spec.Backup = dbv1alpha1.BackupSpec{}
			c.Spec.Bootstrap = &dbv1alpha1.BootstrapSpec{Recovery: &dbv1alpha1.RecoverySpec{
				SourceCluster: sourceName,
				Destination:   dbv1alpha1.BackupDestination{Bucket: testBucket, Region: testRegion, CredentialsSecret: name + "-src"},
				Encryption:    &dbv1alpha1.BackupEncryption{KeySecret: name + "-srckey"},
			}}
		})
		reconcileOnce(name)
		Expect(progressing(name).Message).To(ContainSubstring("restore"))
		Expect(progressing(name).Message).To(ContainSubstring("64 hexadecimal"))
	})

	It("creates a cluster that restores from the source's backups, and says so while it waits", func() {
		const name = "bk-restore-ok"
		createSecret(name+"-src", goodCreds)
		newCluster(name, func(c *dbv1alpha1.PostgresCluster) {
			c.Spec.Backup = dbv1alpha1.BackupSpec{}
			c.Spec.Bootstrap = &dbv1alpha1.BootstrapSpec{Recovery: &dbv1alpha1.RecoverySpec{
				SourceCluster: sourceName,
				Destination:   dbv1alpha1.BackupDestination{Bucket: testBucket, Region: testRegion, CredentialsSecret: name + "-src"},
			}}
		})
		reconcileOnce(name)

		env := stsEnv(name)
		Expect(env["CLONE_METHOD"].Value).To(Equal("CLONE_WITH_WALG"))
		Expect(env["CLONE_WALG_S3_PREFIX"].Value).To(Equal("s3://acme-db-backups/default/orders/wal/16"))
		Expect(env).NotTo(HaveKey("USE_WALG_BACKUP"))

		c := &dbv1alpha1.PostgresCluster{}
		Expect(k8sClient.Get(ctx, key(name), c)).To(Succeed())
		failover := meta.FindStatusCondition(c.Status.Conditions, "FailoverInProgress")
		Expect(failover.Reason).To(Equal("Bootstrapping"))
		Expect(failover.Message).To(ContainSubstring("restoring from the backups of orders"))
	})

	It("does not ask for any Secret when backups are off", func() {
		const name = "bk-off"
		newCluster(name, func(c *dbv1alpha1.PostgresCluster) { c.Spec.Backup = dbv1alpha1.BackupSpec{} })
		reconcileOnce(name)
		Expect(stsEnv(name)).NotTo(HaveKey("USE_WALG_BACKUP"))
	})

	It("updates the StatefulSet when the backup settings change", func() {
		const name = "bk-change"
		createSecret(name+"-s3", goodCreds)
		newCluster(name, nil)
		reconcileOnce(name)

		c := &dbv1alpha1.PostgresCluster{}
		Expect(k8sClient.Get(ctx, key(name), c)).To(Succeed())
		c.Spec.Backup.Schedule = "30 4 * * *"
		Expect(k8sClient.Update(ctx, c)).To(Succeed())
		reconcileOnce(name)
		Expect(stsEnv(name)["BACKUP_SCHEDULE"].Value).To(Equal("30 4 * * *"))
	})

	It("removes the pg_dumpall CronJob an earlier version created, and keeps its volume", func() {
		const name = "bk-legacy"
		createSecret(name+"-s3", goodCreds)
		newCluster(name, nil)
		cj := &batchv1.CronJob{
			ObjectMeta: metav1.ObjectMeta{Name: name + "-backup", Namespace: namespace},
			Spec: batchv1.CronJobSpec{
				Schedule: nightlyCron,
				JobTemplate: batchv1.JobTemplateSpec{Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers:    []corev1.Container{{Name: "backup", Image: testPodImage}},
				}}}},
			},
		}
		Expect(k8sClient.Create(ctx, cj)).To(Succeed())
		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: name + "-backup", Namespace: namespace},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}},
			},
		}
		Expect(k8sClient.Create(ctx, pvc)).To(Succeed())
		DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, pvc))).To(Succeed()) })

		reconcileOnce(name)

		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key(name+"-backup"), &batchv1.CronJob{}))).To(BeTrue())
		Expect(k8sClient.Get(ctx, key(name+"-backup"), &corev1.PersistentVolumeClaim{})).To(Succeed())
	})

	It("reports Degraded as unknown until a backup status check has reported", func() {
		const name = "bk-degraded"
		createSecret(name+"-s3", goodCreds)
		newCluster(name, nil)
		reconcileOnce(name)
		c := &dbv1alpha1.PostgresCluster{}
		Expect(k8sClient.Get(ctx, key(name), c)).To(Succeed())
		degraded := meta.FindStatusCondition(c.Status.Conditions, "Degraded")
		Expect(degraded).NotTo(BeNil())
		Expect(degraded.Status).To(Equal(metav1.ConditionUnknown))
		Expect(strings.ToLower(degraded.Message)).To(ContainSubstring("no recent backup status check"))
	})
})
