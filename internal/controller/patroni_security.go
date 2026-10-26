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
	"slices"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	dbv1alpha1 "github.com/tusharshrivastava62/postgres-operator/api/v1alpha1"
)

const (
	// patroniAPIUser is who the operator is to Patroni's REST API.
	patroniAPIUser = "operator"

	// The API password lives in the credentials Secret. While the passwords are
	// being replaced the new one waits beside it, and when the new one has been
	// made the password the pods that are not yet replaced still hold is kept,
	// as previous, until every pod has the new one.
	secretKeyPatroniAPI         = "patroni-api-password"
	secretKeyPatroniAPINext     = "patroni-api-password-next"
	secretKeyPatroniAPIPrevious = "patroni-api-password-previous"

	// envRESTAPIPassword carries the password into the pod. SPILO_CONFIGURATION
	// refers to it by name, so that the password is in a Secret and not in the
	// pod's spec. It has to come before SPILO_CONFIGURATION in the environment.
	envRESTAPIPassword = "REST_API_PASSWORD"

	// The image's own names for the files Patroni's REST API serves from.
	envRESTAPICertificate = "SSL_RESTAPI_CERTIFICATE_FILE"
	envRESTAPIPrivateKey  = "SSL_RESTAPI_PRIVATE_KEY_FILE"
	envRESTAPICA          = "SSL_RESTAPI_CA_FILE"

	// envRESTAPIConnectAddress is the address a member publishes for its API. It
	// is a name when the API is served over TLS, since a certificate can be valid
	// for a name that stays and not for an address that changes.
	envRESTAPIConnectAddress = "RESTAPI_CONNECT_ADDRESS"
	envPodName               = "POD_NAME"
)

// generateAPIPassword makes a password for Patroni's API. It starts with a
// letter, because it is written into a YAML document and one that looked like a
// number or a word YAML knows would be read as something else.
func generateAPIPassword() (string, error) {
	password, err := generatePassword()
	if err != nil {
		return "", err
	}
	return "pw" + password, nil
}

// apiAuthenticationOn is whether the API is to ask for the password.
func apiAuthenticationOn(cluster *dbv1alpha1.PostgresCluster) bool {
	return cluster.Spec.Security.PatroniAPIAuthentication()
}

// restAPIConfiguration is the part of SPILO_CONFIGURATION that makes the API ask
// for a password. The password is not in it: the pod's environment variable of
// that name is, which Kubernetes fills in from the Secret.
func restAPIConfiguration() map[string]any {
	return map[string]any{
		"authentication": map[string]any{
			"username": patroniAPIUser,
			"password": "$(" + envRESTAPIPassword + ")",
		},
	}
}

// useAPISecurity adds what the API needs to a database pod template: the
// password, and the certificate and the names to serve it by.
func useAPISecurity(sts *appsv1.StatefulSet, cluster *dbv1alpha1.PostgresCluster) {
	pod := &sts.Spec.Template.Spec
	container := &pod.Containers[0]
	if cluster.Spec.Security.PatroniAPITLS() {
		container.Env = append(container.Env,
			corev1.EnvVar{Name: envRESTAPICertificate, Value: tlsMountPath + "/" + tlsCertKey},
			corev1.EnvVar{Name: envRESTAPIPrivateKey, Value: tlsMountPath + "/" + tlsKeyKey},
			corev1.EnvVar{Name: envRESTAPICA, Value: tlsMountPath + "/" + tlsCAKey},
			corev1.EnvVar{Name: envPodName, ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}},
			corev1.EnvVar{Name: envRESTAPIConnectAddress, Value: fmt.Sprintf("$(%s).%s.%s.svc.cluster.local:%d",
				envPodName, headlessServiceName(cluster), cluster.Namespace, patroniPort)},
		)
		for _, probe := range []*corev1.Probe{container.ReadinessProbe, container.LivenessProbe} {
			if probe != nil && probe.HTTPGet != nil {
				probe.HTTPGet.Scheme = corev1.URISchemeHTTPS
			}
		}
	}
}

// podServesAPITLS is whether a pod was started with its API answering over TLS.
func podServesAPITLS(pod *corev1.Pod, secretName string) bool {
	return podServesTLS(pod, secretName) && podHasEnv(pod, envRESTAPICertificate)
}

// podRequiresAPIPassword is whether a pod was started with its API asking for the
// password.
func podRequiresAPIPassword(pod *corev1.Pod) bool {
	return podHasEnv(pod, envRESTAPIPassword)
}

func podHasEnv(pod *corev1.Pod, name string) bool {
	for _, c := range pod.Spec.Containers {
		if slices.ContainsFunc(c.Env, func(e corev1.EnvVar) bool { return e.Name == name }) {
			return true
		}
	}
	return false
}

// patroniPodName is the name a pod answers to under the headless Service, which
// the certificate must be valid for through its wildcard.
func patroniPodName(cluster *dbv1alpha1.PostgresCluster, pod string) string {
	return fmt.Sprintf("%s.%s.%s.svc.cluster.local", pod, headlessServiceName(cluster), cluster.Namespace)
}

// patroniTargetFor says how to call the API of one pod: with the password, if
// the cluster has one, and over TLS with the certificate checked, if the pod
// was started to serve it. It tries the previous password too while the pods are
// being replaced, since a pod that has not been is still asking for the old one.
// A pod's API is called over TLS only once the pod serves it; until then there
// is nothing to check.
func patroniTargetFor(
	ctx context.Context, reader client.Reader, cluster *dbv1alpha1.PostgresCluster, podName, address string,
) (PatroniTarget, error) {
	target := PatroniTarget{Address: address}

	creds := &corev1.Secret{}
	err := reader.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: credentialsSecretName(cluster)}, creds)
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		return target, err
	default:
		if current := string(creds.Data[secretKeyPatroniAPI]); current != "" {
			target.Username = patroniAPIUser
			target.Passwords = []string{current}
			if previous := string(creds.Data[secretKeyPatroniAPIPrevious]); previous != "" {
				target.Passwords = append(target.Passwords, previous)
			}
		}
	}

	if podName == "" {
		return target, nil
	}
	pod := &corev1.Pod{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: podName}, pod); err != nil {
		if apierrors.IsNotFound(err) {
			return target, nil
		}
		return target, err
	}
	// What decides is how the pod was started, not what the spec says now: a pod
	// that is yet to be replaced after the certificate was taken away from the
	// spec still answers over TLS only, and plain HTTP would never reach it.
	secretName := podTLSSecret(pod)
	if secretName == "" || !podHasEnv(pod, envRESTAPICertificate) {
		return target, nil
	}
	material, problem, err := readTLSSecret(ctx, reader, cluster, secretName, time.Now())
	switch {
	case err != nil:
		return target, err
	case material == nil:
		return target, fmt.Errorf("the Patroni API of %s answers over TLS, which cannot be checked: %s", podName, problem)
	}
	target.TLS = &SQLTLS{Roots: material.Roots, ServerName: patroniPodName(cluster, podName)}
	return target, nil
}

// reconcilePatroniAPIStatus records how the primary's API was started.
func (r *PostgresClusterReconciler) reconcilePatroniAPIStatus(ctx context.Context, cluster *dbv1alpha1.PostgresCluster) error {
	secure := cluster.Spec.Security
	want := (*dbv1alpha1.PatroniAPIStatus)(nil)
	if secure.PatroniAPIAuthentication() || secure.PatroniAPITLS() || cluster.Status.Security.PatroniAPI != nil {
		leader, address, err := r.leaderAddress(ctx, cluster)
		if err != nil {
			return err
		}
		if address == "" {
			return nil // keep what was last seen
		}
		pod := &corev1.Pod{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: leader}, pod); err != nil {
			return client.IgnoreNotFound(err)
		}
		want = &dbv1alpha1.PatroniAPIStatus{
			Authenticated: podRequiresAPIPassword(pod),
			TLS:           podHasEnv(pod, envRESTAPICertificate),
		}
		if !want.Authenticated && !want.TLS && !secure.PatroniAPIAuthentication() && !secure.PatroniAPITLS() {
			want = nil // it was switched off, and the pod is back as it was
		}
	}
	if equality.Semantic.DeepEqual(cluster.Status.Security.PatroniAPI, want) {
		return nil
	}
	cluster.Status.Security.PatroniAPI = want
	return r.Status().Update(ctx, cluster)
}

// reconcileAPICertificate makes the members' APIs present the certificate in the
// Secret. Patroni reads the file when it starts and does not notice that it has
// changed, so a renewed certificate would otherwise be presented only after the
// pod is replaced. It compares what each member presents with the Secret, and
// asks any that differs to reload, which is cheap and harmless to repeat. It
// reports whether it asked, so that the next look comes soon to see that it took.
func (r *PostgresClusterReconciler) reconcileAPICertificate(ctx context.Context, cluster *dbv1alpha1.PostgresCluster) (bool, error) {
	if !cluster.Spec.Security.PatroniAPITLS() {
		return false, nil
	}
	material, _, err := readTLS(ctx, r.Client, cluster, time.Now())
	if err != nil || material == nil {
		return false, err
	}
	api := r.Patroni
	if api == nil {
		api = defaultPatroni
	}
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(cluster.Namespace), client.MatchingLabels(podSelectorLabels(cluster))); err != nil {
		return false, err
	}
	log := logf.FromContext(ctx)
	asked := false
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Status.PodIP == "" || pod.DeletionTimestamp != nil || !podServesAPITLS(pod, cluster.Spec.Security.TLSSecretName()) {
			continue
		}
		served, err := api.ServedCertificate(ctx, PatroniTarget{Address: pod.Status.PodIP})
		if err != nil || served == nil || served.Equal(material.Leaf) {
			continue // not up yet, or already presenting it
		}
		target, err := patroniTargetFor(ctx, r.Client, cluster, pod.Name, pod.Status.PodIP)
		if err != nil {
			return asked, err
		}
		if err := api.Reload(ctx, target); err != nil {
			log.Info("could not ask a member to reload the certificate its API presents", "pod", pod.Name, "error", err.Error())
			continue
		}
		log.Info("Asked a member to reload the certificate its API presents", "pod", pod.Name)
		asked = true
	}
	return asked, nil
}
