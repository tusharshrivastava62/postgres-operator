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
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	dbv1alpha1 "github.com/tusharshrivastava62/postgres-operator/api/v1alpha1"
)

const (
	// tlsVolumeName and tlsMountPath are where the database pods read the
	// certificate the cluster was given.
	tlsVolumeName = "tls"
	tlsMountPath  = "/tls"

	// tlsKeyMode lets the postgres user's group read the private key and nobody
	// else: PostgreSQL refuses a key that is readable by more than that.
	tlsKeyMode int32 = 0o440

	// The keys of the Secret, as cert-manager writes them.
	tlsCertKey = "tls.crt"
	tlsKeyKey  = "tls.key"
	tlsCAKey   = "ca.crt"

	// tlsCAVolumeName and tlsCAMountPath are where the job pods read the CA,
	// and nothing else: they never hold the private key.
	tlsCAVolumeName = "tls-ca"
	tlsCAMountPath  = "/tls-ca"

	// tlsRecheckDelay is how long to wait before looking at a Secret that was
	// not right. Nothing watches it.
	tlsRecheckDelay = 30 * time.Second

	reasonTLSSecretProblem = "TLSSecretProblem"
)

// tlsMaterial is a certificate the operator has read and checked.
type tlsMaterial struct {
	// CA is the PEM the jobs trust, and Roots is the same as a pool.
	CA    []byte
	Roots *x509.CertPool
	// Expired is true when the certificate's time has run out. It is still
	// returned, so that its names and chain can be reported on.
	Expired bool
	Leaf    *x509.Certificate
}

// tlsServerName is the name a client checks the database's certificate for: the
// read-write Service.
func tlsServerName(cluster *dbv1alpha1.PostgresCluster) string {
	return fmt.Sprintf("%s.%s.svc", cluster.Name, cluster.Namespace)
}

// tlsPodName is a name a pod answers to under the headless Service, which the
// certificate must be valid for through its wildcard.
func tlsPodName(cluster *dbv1alpha1.PostgresCluster) string {
	return fmt.Sprintf("%s-0.%s.%s.svc.cluster.local", cluster.Name, headlessServiceName(cluster), cluster.Namespace)
}

func tlsCAConfigMapName(cluster *dbv1alpha1.PostgresCluster) string { return cluster.Name + "-tls-ca" }

// readTLS reads the Secret the cluster names and checks it. A problem is
// something to fix before anything is changed: the Secret is missing or
// incomplete, the key does not belong to the certificate, the certificate does
// not chain to the CA, or it is not valid for the names the database is reached
// by. An expired certificate is not a problem here, since nothing the operator
// would change makes it worse.
func readTLS(ctx context.Context, reader client.Reader, cluster *dbv1alpha1.PostgresCluster, now time.Time) (*tlsMaterial, string, error) {
	name := cluster.Spec.Security.TLSSecretName()
	if name == "" {
		return nil, "", nil
	}
	secret := &corev1.Secret{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: name}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Sprintf("spec.security.tls.secretName: the Secret %q does not exist in namespace %s", name, cluster.Namespace), nil
		}
		return nil, "", err
	}
	material, problem := checkTLS(secret, cluster, now)
	return material, problem, nil
}

func checkTLS(secret *corev1.Secret, cluster *dbv1alpha1.PostgresCluster, now time.Time) (*tlsMaterial, string) {
	about := fmt.Sprintf("spec.security.tls.secretName: the Secret %q", secret.Name)
	for _, key := range []string{tlsCertKey, tlsKeyKey, tlsCAKey} {
		if len(secret.Data[key]) == 0 {
			return nil, fmt.Sprintf("%s has no %s", about, key)
		}
	}
	pair, err := tls.X509KeyPair(secret.Data[tlsCertKey], secret.Data[tlsKeyKey])
	if err != nil {
		return nil, fmt.Sprintf("%s: %s is not a certificate with the private key in %s: %v", about, tlsCertKey, tlsKeyKey, err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(secret.Data[tlsCAKey]) {
		return nil, fmt.Sprintf("%s: %s holds no certificate", about, tlsCAKey)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, fmt.Sprintf("%s: reading the certificate: %v", about, err)
	}
	intermediates := x509.NewCertPool()
	for _, der := range pair.Certificate[1:] {
		if c, err := x509.ParseCertificate(der); err == nil {
			intermediates.AddCert(c)
		}
	}

	material := &tlsMaterial{CA: secret.Data[tlsCAKey], Roots: roots, Leaf: leaf}
	// Names and chain are checked as of a moment the certificate was valid, so
	// that an expired one is reported as expired and not as something else.
	at := now
	if now.After(leaf.NotAfter) {
		material.Expired = true
		at = leaf.NotAfter.Add(-time.Second)
	}
	for _, dns := range []string{tlsServerName(cluster), tlsPodName(cluster)} {
		_, err := leaf.Verify(x509.VerifyOptions{
			DNSName: dns, Roots: roots, Intermediates: intermediates, CurrentTime: at,
			KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		})
		if err != nil {
			return nil, fmt.Sprintf("%s: the certificate is not valid for %s: %v", about, dns, err)
		}
	}
	return material, ""
}

// holdBackForSecurity stops a reconcile, changing nothing, when spec.security
// cannot be acted on yet: it is not valid, or the certificate the pods are to
// mount is missing, incomplete, or not valid for the names the database is
// reached by. A pod restarted into such a Secret could not start or could not be
// verified. Nothing watches the Secret, so it is looked at again after a while.
func (r *PostgresClusterReconciler) holdBackForSecurity(ctx context.Context, cluster *dbv1alpha1.PostgresCluster) (ctrl.Result, bool, error) {
	log := logf.FromContext(ctx)
	if errs := cluster.Spec.ValidateSecurity(); len(errs) > 0 {
		message := errs.ToAggregate().Error()
		log.Info("not reconciling: invalid spec.security: " + message)
		return ctrl.Result{}, true, r.markInvalidSpec(ctx, cluster, "InvalidSecurity", message)
	}
	_, problem, err := readTLS(ctx, r.Client, cluster, time.Now())
	if err != nil {
		return ctrl.Result{}, true, fmt.Errorf("checking the TLS certificate: %w", err)
	}
	if problem == "" {
		return ctrl.Result{}, false, nil
	}
	log.Info("not reconciling: " + problem)
	return ctrl.Result{RequeueAfter: tlsRecheckDelay}, true, r.markInvalidSpec(ctx, cluster, reasonTLSSecretProblem, problem)
}

// podServesTLS is whether a pod was started with the cluster's certificate: its
// spec mounts the Secret the cluster names now. A pod that was not has the
// certificate the image made, and cannot be verified.
func podServesTLS(pod *corev1.Pod, secretName string) bool {
	for _, v := range pod.Spec.Volumes {
		if v.Name == tlsVolumeName && v.Secret != nil && v.Secret.SecretName == secretName {
			return true
		}
	}
	return false
}

// sqlTLSFor says how to check the certificate of the database at an address.
// It returns nothing when the cluster asks for no certificate, or when the pod
// at that address was started before it did: until the pods are replaced there
// is nothing to check. Once a pod serves the certificate, a Secret the operator
// cannot read is an error and not a reason to stop checking.
func sqlTLSFor(ctx context.Context, reader client.Reader, cluster *dbv1alpha1.PostgresCluster, address string) (*SQLTLS, error) {
	secretName := cluster.Spec.Security.TLSSecretName()
	if secretName == "" {
		return nil, nil
	}
	pods := &corev1.PodList{}
	if err := reader.List(ctx, pods, client.InNamespace(cluster.Namespace), client.MatchingLabels(podSelectorLabels(cluster))); err != nil {
		return nil, fmt.Errorf("finding the pod at %s: %w", address, err)
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Status.PodIP != address {
			continue
		}
		if !podServesTLS(pod, secretName) {
			return nil, nil
		}
		material, problem, err := readTLS(ctx, reader, cluster, time.Now())
		switch {
		case err != nil:
			return nil, err
		case material == nil:
			return nil, errors.New("the database serves the certificate in spec.security.tls, which cannot be checked: " + problem)
		}
		return &SQLTLS{Roots: material.Roots, ServerName: tlsServerName(cluster)}, nil
	}
	return nil, nil
}

// tlsVolume, tlsMount and tlsEnv are what the database pods get when the
// cluster has a certificate. The Secret is read in place, so a renewed
// certificate reaches the pod without a restart, and the image reloads the
// server when the files change.
func tlsVolume(secretName string) corev1.Volume {
	return corev1.Volume{Name: tlsVolumeName, VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
		SecretName:  secretName,
		DefaultMode: ptr.To(tlsKeyMode),
	}}}
}

func tlsMount() corev1.VolumeMount {
	return corev1.VolumeMount{Name: tlsVolumeName, MountPath: tlsMountPath, ReadOnly: true}
}

func tlsEnv() []corev1.EnvVar {
	return []corev1.EnvVar{
		{Name: "SSL_CERTIFICATE_FILE", Value: tlsMountPath + "/" + tlsCertKey},
		{Name: "SSL_PRIVATE_KEY_FILE", Value: tlsMountPath + "/" + tlsKeyKey},
		{Name: "SSL_CA_FILE", Value: tlsMountPath + "/" + tlsCAKey},
	}
}

// useTLS adds the certificate to a database pod template.
func useTLS(sts *appsv1.StatefulSet, secretName string) {
	pod := &sts.Spec.Template.Spec
	pod.Volumes = append(pod.Volumes, tlsVolume(secretName))
	container := &pod.Containers[0]
	container.VolumeMounts = append(container.VolumeMounts, tlsMount())
	container.Env = append(container.Env, tlsEnv()...)
}

// jobsVerifyTLS is whether the small jobs check the database's certificate:
// from the moment the operator has verified it, and not before, since until
// the primary serves it the check could only fail.
func jobsVerifyTLS(cluster *dbv1alpha1.PostgresCluster) bool {
	return cluster.Spec.Security.TLSSecretName() != "" &&
		cluster.Status.Security.TLS != nil && cluster.Status.Security.TLS.Verified
}

// jobHost is the name a job pod connects to. When it checks the certificate it
// must be a name the certificate is valid for, which the Service's short name
// is not.
func jobHost(cluster *dbv1alpha1.PostgresCluster) string {
	if jobsVerifyTLS(cluster) {
		return tlsServerName(cluster)
	}
	return cluster.Name
}

// jobSSLEnv is how a job pod connects to the database: with the certificate
// checked once the operator has verified it, and only encrypted before that.
func jobSSLEnv(cluster *dbv1alpha1.PostgresCluster) []corev1.EnvVar {
	if !jobsVerifyTLS(cluster) {
		return []corev1.EnvVar{{Name: "PGSSLMODE", Value: "require"}}
	}
	return []corev1.EnvVar{
		{Name: "PGSSLMODE", Value: "verify-full"},
		{Name: "PGSSLROOTCERT", Value: tlsCAMountPath + "/" + tlsCAKey},
	}
}

// trustTLSCA gives a job pod the CA, when it checks the certificate.
func trustTLSCA(cluster *dbv1alpha1.PostgresCluster, pod *corev1.PodSpec) {
	if !jobsVerifyTLS(cluster) {
		return
	}
	pod.Volumes = append(pod.Volumes, corev1.Volume{Name: tlsCAVolumeName, VolumeSource: corev1.VolumeSource{
		ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: tlsCAConfigMapName(cluster)}},
	}})
	pod.Containers[0].VolumeMounts = append(pod.Containers[0].VolumeMounts,
		corev1.VolumeMount{Name: tlsCAVolumeName, MountPath: tlsCAMountPath, ReadOnly: true})
}

// reconcileTLSCA keeps the ConfigMap with the CA the jobs trust, and only the
// CA, or removes it when the cluster asks for no certificate.
func (r *PostgresClusterReconciler) reconcileTLSCA(ctx context.Context, cluster *dbv1alpha1.PostgresCluster) error {
	meta := metav1.ObjectMeta{Name: tlsCAConfigMapName(cluster), Namespace: cluster.Namespace}
	material, _, err := readTLS(ctx, r.Client, cluster, time.Now())
	if err != nil {
		return err
	}
	if material == nil {
		if cluster.Spec.Security.TLSSecretName() != "" {
			return nil // the Secret is not right; tlsProblem has said so
		}
		return client.IgnoreNotFound(r.Delete(ctx, &corev1.ConfigMap{ObjectMeta: meta}))
	}
	cm := &corev1.ConfigMap{ObjectMeta: meta}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		cm.Labels = labelsForCluster(cluster)
		if !bytes.Equal([]byte(cm.Data[tlsCAKey]), material.CA) {
			cm.Data = map[string]string{tlsCAKey: string(material.CA)}
		}
		return controllerutil.SetControllerReference(cluster, cm, r.Scheme)
	})
	return err
}

// certificateIssuer names who issued a certificate, for a person to read.
func certificateIssuer(c *x509.Certificate) string {
	if c.Issuer.CommonName != "" {
		return c.Issuer.CommonName
	}
	return c.Issuer.String()
}
