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
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	dbv1alpha1 "github.com/tusharshrivastava62/postgres-operator/api/v1alpha1"
)

const (
	// annotationRotate is set on a Secret by a person to ask for its password
	// to be replaced. Any value not used before asks again.
	annotationRotate = "db.youroperator.io/rotate"
	// annotationRotatedToken is the value of annotationRotate that the last
	// replacement answered, so that a request is answered once.
	annotationRotatedToken = "db.youroperator.io/rotated-token"
	// annotationRotatedAt is when the password was last replaced.
	annotationRotatedAt = "db.youroperator.io/rotated-at"
	// annotationCredentialsVersion counts the replacements of the superuser and
	// replication passwords. The pod template carries it, so a replacement
	// changes the template, and the rolling update that follows is what gives
	// every pod the new passwords.
	annotationCredentialsVersion = "db.youroperator.io/credentials-version"

	// While the superuser and replication passwords are being replaced, the
	// new ones wait in the credentials Secret under these keys.
	secretKeyPasswordNext            = "password-next"
	secretKeyReplicationPasswordNext = "replication-password-next"

	rotationMessageLimit = 512

	// Why a rotation is due.
	rotationRequested = "requested"
	rotationScheduled = "scheduled"
)

// rotationDue reports whether a Secret's password is to be replaced now: a
// person asked, with an annotation value not answered yet, or it is older than
// the schedule says.
func rotationDue(secret *corev1.Secret, every *metav1.Duration, now time.Time) (bool, string) {
	if token := secret.Annotations[annotationRotate]; token != "" && token != secret.Annotations[annotationRotatedToken] {
		return true, rotationRequested
	}
	if every != nil && every.Duration > 0 && now.Sub(lastRotated(secret)) >= every.Duration {
		return true, rotationScheduled
	}
	return false, ""
}

// lastRotated is when a Secret's password was last replaced. A Secret that
// never was has the age of its creation.
func lastRotated(secret *corev1.Secret) time.Time {
	if at, err := time.Parse(time.RFC3339, secret.Annotations[annotationRotatedAt]); err == nil {
		return at
	}
	return secret.CreationTimestamp.Time
}

// stampRotation records on a Secret that its password was replaced just now,
// and which request, if any, that answered.
func stampRotation(secret *corev1.Secret, now time.Time) {
	if secret.Annotations == nil {
		secret.Annotations = map[string]string{}
	}
	secret.Annotations[annotationRotatedAt] = now.UTC().Format(time.RFC3339)
	if token := secret.Annotations[annotationRotate]; token != "" {
		secret.Annotations[annotationRotatedToken] = token
	}
}

// patchSecret changes a Secret with an optimistic lock, so a change someone
// else made since it was read is not overwritten.
func (r *PostgresClusterReconciler) patchSecret(ctx context.Context, secret *corev1.Secret, change func(*corev1.Secret)) error {
	patch := client.MergeFromWithOptions(secret.DeepCopy(), client.MergeFromWithOptimisticLock{})
	change(secret)
	return r.Patch(ctx, secret, patch)
}

func rotationSpec(cluster *dbv1alpha1.PostgresCluster) dbv1alpha1.CredentialRotationSpec {
	if cluster.Spec.CredentialRotation == nil {
		return dbv1alpha1.CredentialRotationSpec{}
	}
	return *cluster.Spec.CredentialRotation
}

// credentialsVersion is the number of times the superuser and replication
// passwords have been replaced, as the credentials Secret says; empty for a
// cluster whose passwords never have been.
func credentialsVersion(secret *corev1.Secret) string {
	return secret.Annotations[annotationCredentialsVersion]
}

// credentialsReady is what has to be true before the superuser and replication
// passwords are replaced: a rotation changes every pod, so it waits for a
// cluster that is whole and not being changed already.
func credentialsReady(healthy, rolling bool) (bool, string) {
	switch {
	case rolling:
		return false, "the pods are being updated already"
	case !healthy:
		return false, "the cluster is not healthy: every member must be streaming"
	}
	return true, ""
}

func timePtr(t time.Time) *metav1.Time {
	m := metav1.NewTime(t)
	return &m
}

// reconcileCredentialRotation replaces the superuser and replication passwords
// when they are due, and records where that stands. It reports whether to look
// again soon.
//
// The passwords cannot simply be changed. Patroni reads them from each pod's
// environment when the pod starts, and a pod that starts with passwords the
// database does not have never becomes ready. So the sequence is:
//
//  1. Put the new passwords in the Secret, beside the old, as password-next.
//  2. Set them in the database. If the operator stops here, it finds out which
//     password the database accepts, and carries on from there.
//  3. In one change to the Secret, make them the passwords and raise the
//     credentials version.
//
// The version is on the pod template, so step 3 starts the rolling update that
// gives every pod the new passwords: replicas first, a switchover, then the old
// primary. Members that are running keep working meanwhile: an open connection
// survives a password change.
func (r *PostgresClusterReconciler) reconcileCredentialRotation(
	ctx context.Context, cluster *dbv1alpha1.PostgresCluster, healthy, rolling bool,
) (bool, error) {
	if r.SQL == nil {
		return false, nil
	}
	log := logf.FromContext(ctx)
	secret := &corev1.Secret{}
	err := r.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: credentialsSecretName(cluster)}, secret)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	now := time.Now()
	_, inProgress := secret.Data[secretKeyPasswordNext]
	due, why := rotationDue(secret, rotationSpec(cluster).SuperuserEvery, now)
	last := timePtr(lastRotated(secret))

	if !inProgress && !due {
		if err := r.dropPreviousAPIPassword(ctx, cluster, secret, rolling); err != nil {
			return false, err
		}
		state := dbv1alpha1.CredentialsIdle
		if rolling && credentialsVersion(secret) != "" {
			state = dbv1alpha1.CredentialsRolling
		}
		return rolling, r.setCredentialsStatus(ctx, cluster, state, "", last)
	}

	// A rotation that has begun is finished whatever the cluster looks like:
	// leaving it half done is what to avoid. One that has not begun waits.
	if !inProgress {
		if ok, reason := credentialsReady(healthy, rolling); !ok {
			return true, r.setCredentialsStatus(ctx, cluster, dbv1alpha1.CredentialsWaiting,
				"a rotation is due ("+why+") and is waiting: "+reason, last)
		}
	}
	_, address, err := r.leaderAddress(ctx, cluster)
	if err != nil {
		return false, err
	}
	if address == "" {
		return true, r.setCredentialsStatus(ctx, cluster, dbv1alpha1.CredentialsWaiting, "a rotation is waiting for a primary", last)
	}

	fail := func(message string) (bool, error) {
		return true, r.setCredentialsStatus(ctx, cluster, dbv1alpha1.CredentialsError, message, last)
	}

	// 1. The new passwords wait beside the old.
	if !inProgress {
		next, err := generatePassword()
		if err != nil {
			return false, err
		}
		nextReplication, err := generatePassword()
		if err != nil {
			return false, err
		}
		// The Patroni API's password is replaced with them, if the cluster has one:
		// it too is read from the environment when a pod starts.
		nextAPI := ""
		if apiAuthenticationOn(cluster) && len(secret.Data[secretKeyPatroniAPI]) > 0 {
			if nextAPI, err = generateAPIPassword(); err != nil {
				return false, err
			}
		}
		if err := r.patchSecret(ctx, secret, func(s *corev1.Secret) {
			s.Data[secretKeyPasswordNext] = []byte(next)
			s.Data[secretKeyReplicationPasswordNext] = []byte(nextReplication)
			if nextAPI != "" {
				s.Data[secretKeyPatroniAPINext] = []byte(nextAPI)
			}
		}); err != nil {
			return false, fmt.Errorf("writing the new passwords: %w", err)
		}
		log.Info("Started replacing the superuser and replication passwords", "reason", why)
	}
	if err := r.setCredentialsStatus(ctx, cluster, dbv1alpha1.CredentialsRotating, "setting the new passwords", last); err != nil {
		return false, err
	}

	// 2. Set them in the database, unless that is done already.
	superuser := string(secret.Data[secretKeyUsername])
	tlsCheck, err := sqlTLSFor(ctx, r.Client, cluster, address)
	if err != nil {
		return fail("could not check the database's certificate: " + err.Error())
	}
	target := SQLTarget{Host: address, Port: postgresPort, Database: monitorDatabase, User: superuser, TLS: tlsCheck}
	changed, err := r.superuserPasswordChanged(ctx, target, secret)
	if err != nil {
		return fail("could not tell which superuser password the database has: " + err.Error())
	}
	if !changed {
		if err := r.setNewPasswords(ctx, target, secret); err != nil {
			return fail("could not set the new passwords: " + err.Error())
		}
	}

	// 3. Make them the passwords, and raise the version, in one change.
	version := 1
	if n, err := strconv.Atoi(credentialsVersion(secret)); err == nil {
		version = n + 1
	}
	if err := r.patchSecret(ctx, secret, func(s *corev1.Secret) {
		s.Data[secretKeyPassword] = s.Data[secretKeyPasswordNext]
		s.Data[secretKeyReplicationPassword] = s.Data[secretKeyReplicationPasswordNext]
		delete(s.Data, secretKeyPasswordNext)
		delete(s.Data, secretKeyReplicationPasswordNext)
		// The pods that are not yet replaced still ask for the old API password,
		// so it is kept until they are.
		if next, ok := s.Data[secretKeyPatroniAPINext]; ok {
			s.Data[secretKeyPatroniAPIPrevious] = s.Data[secretKeyPatroniAPI]
			s.Data[secretKeyPatroniAPI] = next
			delete(s.Data, secretKeyPatroniAPINext)
		}
		stampRotation(s, now)
		s.Annotations[annotationCredentialsVersion] = strconv.Itoa(version)
	}); err != nil {
		return false, fmt.Errorf("promoting the new passwords: %w", err)
	}
	log.Info("Replaced the superuser and replication passwords; the pods will be replaced to pick them up", "version", version)
	return true, r.setCredentialsStatus(ctx, cluster, dbv1alpha1.CredentialsRolling,
		"the new passwords are set; the pods are being replaced to start with them", timePtr(now))
}

// superuserPasswordChanged tells whether the database already has the new
// superuser password, which is so when the operator stopped after setting it.
// It tries the old one first: a connection with the old password working means
// nothing has been done. An error means neither works.
func (r *PostgresClusterReconciler) superuserPasswordChanged(ctx context.Context, target SQLTarget, secret *corev1.Secret) (bool, error) {
	target.Password = string(secret.Data[secretKeyPassword])
	if session, err := r.SQL.Dial(ctx, target); err == nil {
		session.Close(ctx)
		return false, nil
	} else if !sqlRefusedCredentials(err) {
		return false, err
	}
	target.Password = string(secret.Data[secretKeyPasswordNext])
	session, err := r.SQL.Dial(ctx, target)
	if err != nil {
		return false, fmt.Errorf("neither the old nor the new password is accepted: %w", err)
	}
	session.Close(ctx)
	return true, nil
}

// setNewPasswords sets both roles in one statement, so both change or neither.
// It sends verifiers, never the passwords: the server logs the statement.
func (r *PostgresClusterReconciler) setNewPasswords(ctx context.Context, target SQLTarget, secret *corev1.Secret) error {
	superuserVerifier, err := newScramVerifier(string(secret.Data[secretKeyPasswordNext]))
	if err != nil {
		return err
	}
	replicationVerifier, err := newScramVerifier(string(secret.Data[secretKeyReplicationPasswordNext]))
	if err != nil {
		return err
	}
	target.Password = string(secret.Data[secretKeyPassword])
	session, err := r.SQL.Dial(ctx, target)
	if err != nil {
		return err
	}
	defer session.Close(ctx)
	if err := session.Exec(ctx, fmt.Sprintf("alter role %s password %s;\nalter role %s password %s",
		quoteIdent(target.User), sqlLiteral(superuserVerifier), quoteIdent(replicationUser), sqlLiteral(replicationVerifier))); err != nil {
		return err
	}
	// Check the change took, with the password the pods will start with.
	target.Password = string(secret.Data[secretKeyPasswordNext])
	check, err := r.SQL.Dial(ctx, target)
	if err != nil {
		return fmt.Errorf("the new superuser password was set but is not accepted: %w", err)
	}
	check.Close(ctx)
	return nil
}

// setCredentialsStatus records the state of the superuser and replication
// passwords, writing status only if it changed.
func (r *PostgresClusterReconciler) setCredentialsStatus(
	ctx context.Context, cluster *dbv1alpha1.PostgresCluster, state, message string, rotated *metav1.Time,
) error {
	if len(message) > rotationMessageLimit {
		message = message[:rotationMessageLimit]
	}
	want := dbv1alpha1.CredentialsStatus{State: state, Message: message, LastRotated: rotated}
	if equality.Semantic.DeepEqual(cluster.Status.AccessControl.Credentials, want) {
		return nil
	}
	cluster.Status.AccessControl.Credentials = want
	return r.Status().Update(ctx, cluster)
}

// The kinds of credential whose age is published.
const (
	credentialSuperuser   = "superuser"
	credentialReplication = "replication"
	credentialMonitor     = "monitor"
	credentialApplication = "application"
)

// recordCredentialAges publishes how long each kind of credential has been in
// use, so that a password nobody has replaced in a year can be alerted on. For
// application roles it is the oldest one.
func (r *PostgresClusterReconciler) recordCredentialAges(ctx context.Context, cluster *dbv1alpha1.PostgresCluster) {
	now := time.Now()
	set := func(kind string, secret *corev1.Secret) {
		credentialAge.WithLabelValues(cluster.Namespace, cluster.Name, kind).Set(now.Sub(lastRotated(secret)).Seconds())
	}
	get := func(name string) *corev1.Secret {
		secret := &corev1.Secret{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: name}, secret); err != nil {
			return nil
		}
		return secret
	}
	credentialAge.DeletePartialMatch(map[string]string{labelNamespace: cluster.Namespace, labelName: cluster.Name})
	if secret := get(credentialsSecretName(cluster)); secret != nil {
		set(credentialSuperuser, secret)
		set(credentialReplication, secret)
	}
	if monitorRoleWanted(cluster) {
		if secret := get(monitorSecretName(cluster)); secret != nil {
			set(credentialMonitor, secret)
		}
	}
	var oldest *corev1.Secret
	for i := range cluster.Spec.Roles {
		secret := get(cluster.Spec.Roles[i].RoleSecretName(cluster.Name))
		if secret != nil && roleSecretManaged(cluster, secret) && (oldest == nil || lastRotated(secret).Before(lastRotated(oldest))) {
			oldest = secret
		}
	}
	if oldest != nil {
		set(credentialApplication, oldest)
	}
}

// dropPreviousAPIPassword removes the Patroni API password the pods used before
// the last replacement, once every pod has been started with the new one.
func (r *PostgresClusterReconciler) dropPreviousAPIPassword(
	ctx context.Context, cluster *dbv1alpha1.PostgresCluster, secret *corev1.Secret, rolling bool,
) error {
	if _, ok := secret.Data[secretKeyPatroniAPIPrevious]; !ok || rolling {
		return nil
	}
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(cluster.Namespace), client.MatchingLabels(podSelectorLabels(cluster))); err != nil {
		return err
	}
	version := credentialsVersion(secret)
	if len(pods.Items) < int(cluster.Spec.Instances) {
		return nil
	}
	for i := range pods.Items {
		if pods.Items[i].Annotations[annotationCredentialsVersion] != version {
			return nil
		}
	}
	return r.patchSecret(ctx, secret, func(s *corev1.Secret) { delete(s.Data, secretKeyPatroniAPIPrevious) })
}
