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
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	dbv1alpha1 "github.com/tusharshrivastava62/postgres-operator/api/v1alpha1"
)

// reconcileTLSStatus connects to the primary with the certificate check on, and
// records what it presented: whether the operator verified it, who issued it and
// when it expires. It writes status only if something changed. A cluster with no
// certificate asked for has none of this.
//
// It returns whether that changes what the jobs do, which they find out on the
// next look.
func (r *PostgresClusterReconciler) reconcileTLSStatus(ctx context.Context, cluster *dbv1alpha1.PostgresCluster) (bool, error) {
	before := jobsVerifyTLS(cluster)
	err := r.checkTLSStatus(ctx, cluster)
	return jobsVerifyTLS(cluster) != before, err
}

func (r *PostgresClusterReconciler) checkTLSStatus(ctx context.Context, cluster *dbv1alpha1.PostgresCluster) error {
	if cluster.Spec.Security.TLSSecretName() == "" {
		changed := meta.RemoveStatusCondition(&cluster.Status.Conditions, dbv1alpha1.ConditionTLSReady)
		if cluster.Status.Security.TLS != nil {
			cluster.Status.Security.TLS = nil
			changed = true
		}
		if !changed {
			return nil
		}
		return r.Status().Update(ctx, cluster)
	}

	_, address, err := r.leaderAddress(ctx, cluster)
	if err != nil {
		return err
	}
	if address == "" {
		// A failover is no reason to say the certificate was not checked: keep
		// what was last seen, and say there is nothing to look at now.
		return r.setTLSStatus(ctx, cluster, cluster.Status.Security.TLS, metav1.ConditionUnknown, dbv1alpha1.TLSNoPrimary,
			"the cluster has no primary to check yet")
	}
	check, err := sqlTLSFor(ctx, r.Client, cluster, address)
	if err != nil {
		return err
	}
	if check == nil {
		return r.setTLSStatus(ctx, cluster, &dbv1alpha1.TLSStatus{}, metav1.ConditionFalse, dbv1alpha1.TLSWaitingForPods,
			"the primary has not been replaced with the certificate yet, and presents the one the image made")
	}

	target, err := r.superuserTarget(ctx, cluster, address)
	if err != nil {
		return err
	}
	session, err := r.SQL.Dial(ctx, target)
	if err != nil {
		var invalid *tls.CertificateVerificationError
		if !errors.As(err, &invalid) {
			return fmt.Errorf("connecting to the primary to check its certificate: %w", err)
		}
		reason := dbv1alpha1.TLSVerificationFailed
		var expired x509.CertificateInvalidError
		if errors.As(invalid.Err, &expired) && expired.Reason == x509.Expired {
			reason = dbv1alpha1.TLSExpired
		}
		return r.setTLSStatus(ctx, cluster, &dbv1alpha1.TLSStatus{}, metav1.ConditionFalse, reason,
			"the primary's certificate was not accepted: "+invalid.Error())
	}
	defer session.Close(ctx)

	peer := session.PeerCertificate()
	if peer == nil {
		return r.setTLSStatus(ctx, cluster, &dbv1alpha1.TLSStatus{}, metav1.ConditionFalse, dbv1alpha1.TLSVerificationFailed,
			"the connection to the primary did not use TLS")
	}
	notAfter := metav1.NewTime(peer.NotAfter.UTC().Truncate(time.Second))
	status := &dbv1alpha1.TLSStatus{Verified: true, Issuer: certificateIssuer(peer), NotAfter: &notAfter}
	return r.setTLSStatus(ctx, cluster, status, metav1.ConditionTrue, dbv1alpha1.TLSVerified,
		fmt.Sprintf("the primary presents a certificate issued by %s, valid until %s, and the operator verified it",
			status.Issuer, notAfter.UTC().Format(time.RFC3339)))
}

func (r *PostgresClusterReconciler) setTLSStatus(
	ctx context.Context, cluster *dbv1alpha1.PostgresCluster, status *dbv1alpha1.TLSStatus,
	cond metav1.ConditionStatus, reason, message string,
) error {
	changed := meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
		Type: dbv1alpha1.ConditionTLSReady, Status: cond, Reason: reason, Message: message,
	})
	if !equality.Semantic.DeepEqual(cluster.Status.Security.TLS, status) {
		cluster.Status.Security.TLS = status
		changed = true
	}
	if !changed {
		return nil
	}
	return r.Status().Update(ctx, cluster)
}
