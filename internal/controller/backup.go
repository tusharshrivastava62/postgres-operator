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
	"fmt"
	"regexp"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dbv1alpha1 "github.com/tusharshrivastava62/postgres-operator/api/v1alpha1"
)

const (
	// envTrue is the value of the boolean environment variables Spilo and wal-g read.
	envTrue = "true"

	secretKeyAccessKey = "AWS_ACCESS_KEY_ID"
	secretKeySecretKey = "AWS_SECRET_ACCESS_KEY"
	secretKeyEncKey    = "libsodium-key"
)

var hexKey = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

// backupPrefix is where in the bucket a cluster's WAL and base backups live:
// s3://<bucket>/<path>/wal/<major version>. It is always written out in full
// instead of letting Spilo derive it, because Spilo's own default is built
// from the namespace and a restore must be able to name the same place.
func backupPrefix(dest *dbv1alpha1.BackupDestination, namespace, name, version string) string {
	return fmt.Sprintf("s3://%s/%s/wal/%s", dest.Bucket, dest.PathFor(namespace, name), version)
}

// walgEndpoint is the endpoint in the form Spilo reads: protocol, a plus
// sign, the addressing style, then the rest ("https+path://host:port").
func walgEndpoint(dest *dbv1alpha1.BackupDestination) string {
	style := "virtual"
	if dest.ForcePathStyle {
		style = "path"
	}
	scheme, rest, found := strings.Cut(dest.Endpoint, "://")
	if !found {
		return dest.Endpoint
	}
	return scheme + "+" + style + "://" + rest
}

// spiloRetainAll is Spilo's single retention number, set high enough that its
// own backup script never deletes anything. Retention is the operator's job
// (see scripts/backup_status.py), because Spilo's number is used as both a
// count and a number of days and so cannot express "this many backups and this
// many days", as the spike showed.
const spiloRetainAll = "1000"

// backupEnv is the environment that makes Spilo archive WAL and take base
// backups with wal-g. It is empty when backups are off.
//
// WALG_PREVENT_WAL_OVERWRITE makes wal-g refuse to replace an archived WAL
// file with different content. Without it, two clusters pointed at one path
// silently overwrite each other's archive, which was measured to succeed.
func backupEnv(cluster *dbv1alpha1.PostgresCluster) []corev1.EnvVar {
	backup := cluster.Spec.Backup
	if !backup.Enabled || backup.Destination == nil {
		return nil
	}
	dest := backup.Destination
	env := []corev1.EnvVar{
		{Name: "USE_WALG_BACKUP", Value: envTrue},
		{Name: "USE_WALG_RESTORE", Value: envTrue},
		{Name: "WALG_S3_PREFIX", Value: backupPrefix(dest, cluster.Namespace, cluster.Name, cluster.Spec.Version)},
		{Name: "BACKUP_SCHEDULE", Value: backup.Schedule},
		{Name: "BACKUP_NUM_TO_RETAIN", Value: spiloRetainAll},
		{Name: "WALG_PREVENT_WAL_OVERWRITE", Value: envTrue},
		{Name: secretKeyAccessKey, ValueFrom: secretRef(dest.CredentialsSecret, secretKeyAccessKey)},
		{Name: secretKeySecretKey, ValueFrom: secretRef(dest.CredentialsSecret, secretKeySecretKey)},
	}
	if dest.Region != "" {
		env = append(env, corev1.EnvVar{Name: "AWS_REGION", Value: dest.Region})
	}
	if dest.Endpoint != "" {
		// Server-side encryption is turned off for a custom endpoint, since
		// most S3-compatible stores reject it and every upload then fails.
		// Spilo does not do this itself: its default of "true" is overwritten
		// by an empty value before it is read, which a live cluster showed as
		// WALG_S3_SSE=AES256 and no backup ever written.
		env = append(env,
			corev1.EnvVar{Name: "AWS_ENDPOINT", Value: dest.Endpoint},
			corev1.EnvVar{Name: "WALG_S3_ENDPOINT", Value: walgEndpoint(dest)},
			corev1.EnvVar{Name: "WALG_DISABLE_S3_SSE", Value: envTrue})
	}
	if backup.Encryption != nil {
		env = append(env,
			corev1.EnvVar{Name: "WALG_LIBSODIUM_KEY", ValueFrom: secretRef(backup.Encryption.KeySecret, secretKeyEncKey)},
			// The key is written as 64 hexadecimal characters. Without this
			// wal-g would take the characters themselves as the key bytes.
			corev1.EnvVar{Name: "WALG_LIBSODIUM_KEY_TRANSFORM", Value: "hex"})
	}
	return env
}

func secretRef(name, key string) *corev1.EnvVarSource {
	return &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: name}, Key: key,
	}}
}

// backupSecretProblem describes what is wrong with the Secrets the backup
// settings name, or returns "" when they are usable. A Secret that cannot be
// read at all is an error to retry; one that is absent or malformed is the
// user's to fix.
func (r *PostgresClusterReconciler) backupSecretProblem(ctx context.Context, cluster *dbv1alpha1.PostgresCluster) (string, error) {
	backup := cluster.Spec.Backup
	if !backup.Enabled || backup.Destination == nil {
		return "", nil
	}

	creds := &corev1.Secret{}
	err := r.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: backup.Destination.CredentialsSecret}, creds)
	if apierrors.IsNotFound(err) {
		return fmt.Sprintf("the backup credentials Secret %q does not exist in namespace %s",
			backup.Destination.CredentialsSecret, cluster.Namespace), nil
	}
	if err != nil {
		return "", err
	}
	for _, key := range []string{secretKeyAccessKey, secretKeySecretKey} {
		if len(creds.Data[key]) == 0 {
			return fmt.Sprintf("the backup credentials Secret %q has no %s entry", creds.Name, key), nil
		}
	}

	if backup.Encryption != nil {
		enc := &corev1.Secret{}
		err := r.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: backup.Encryption.KeySecret}, enc)
		if apierrors.IsNotFound(err) {
			return fmt.Sprintf("the backup encryption Secret %q does not exist in namespace %s",
				backup.Encryption.KeySecret, cluster.Namespace), nil
		}
		if err != nil {
			return "", err
		}
		if !hexKey.Match(enc.Data[secretKeyEncKey]) {
			return fmt.Sprintf("the %s entry of Secret %q must be 64 hexadecimal characters (a 32-byte key)",
				secretKeyEncKey, enc.Name), nil
		}
	}
	return "", nil
}

// removeLegacyBackupCronJob deletes the pg_dumpall CronJob that earlier
// versions created. Its volume and the dumps on it are left alone: nothing
// deletes a volume automatically.
func (r *PostgresClusterReconciler) removeLegacyBackupCronJob(ctx context.Context, cluster *dbv1alpha1.PostgresCluster) error {
	cj := &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: backupName(cluster), Namespace: cluster.Namespace}}
	return client.IgnoreNotFound(r.Delete(ctx, cj))
}

// walgClientEnv is what wal-g needs to reach the bucket when it is run
// directly, in a pod that is not a database member and so has no Spilo
// configuration step to write its environment for it.
func walgClientEnv(cluster *dbv1alpha1.PostgresCluster) []corev1.EnvVar {
	backup := cluster.Spec.Backup
	if backup.Destination == nil {
		return nil
	}
	dest := backup.Destination
	env := []corev1.EnvVar{
		{Name: "WALG_S3_PREFIX", Value: backupPrefix(dest, cluster.Namespace, cluster.Name, cluster.Spec.Version)},
		{Name: secretKeyAccessKey, ValueFrom: secretRef(dest.CredentialsSecret, secretKeyAccessKey)},
		{Name: secretKeySecretKey, ValueFrom: secretRef(dest.CredentialsSecret, secretKeySecretKey)},
	}
	if dest.Region != "" {
		env = append(env, corev1.EnvVar{Name: "AWS_REGION", Value: dest.Region})
	}
	if dest.Endpoint != "" {
		env = append(env,
			corev1.EnvVar{Name: "AWS_ENDPOINT", Value: dest.Endpoint},
			corev1.EnvVar{Name: "AWS_S3_FORCE_PATH_STYLE", Value: fmt.Sprint(dest.ForcePathStyle)},
			corev1.EnvVar{Name: "WALG_DISABLE_S3_SSE", Value: envTrue})
	}
	if backup.Encryption != nil {
		env = append(env,
			corev1.EnvVar{Name: "WALG_LIBSODIUM_KEY", ValueFrom: secretRef(backup.Encryption.KeySecret, secretKeyEncKey)},
			corev1.EnvVar{Name: "WALG_LIBSODIUM_KEY_TRANSFORM", Value: "hex"})
	}
	return env
}
