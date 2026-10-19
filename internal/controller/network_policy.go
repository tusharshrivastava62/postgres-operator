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

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	dbv1alpha1 "github.com/tusharshrivastava62/postgres-operator/api/v1alpha1"
)

const (
	// labelClient marks a pod in the cluster's own namespace as one that may
	// connect to its PostgreSQL.
	labelClient      = "db.youroperator.io/client"
	labelClientValue = "true"

	// labelNamespaceName is set by Kubernetes on every namespace, which makes
	// a namespace selectable by name.
	labelNamespaceName = "kubernetes.io/metadata.name"

	// labelPodTemplateHash is added to a Deployment's pods by the Deployment
	// itself, and changes with every rollout. It must not be part of how the
	// operator's pods are selected.
	labelPodTemplateHash = "pod-template-hash"

	// reasonOperatorIdentity is the Progressing reason of a cluster that wants
	// a network policy the operator cannot write yet.
	reasonOperatorIdentity = "OperatorIdentityUnknown"
)

// OperatorIdentity says where the operator runs, so a cluster's network
// policy can let it through to Patroni's REST API: its namespace, and the
// labels of its own pods. They are read from the operator's own pod at start,
// not configured, so they are right for however it was deployed.
type OperatorIdentity struct {
	Namespace string
	PodLabels map[string]string
}

// DiscoverOperatorIdentity reads the operator's own pod. The pod's name and
// namespace come from the downward API (POD_NAME and POD_NAMESPACE), and the
// labels that vary with every rollout are left out.
func DiscoverOperatorIdentity(ctx context.Context, reader client.Reader, namespace, podName string) (*OperatorIdentity, error) {
	if namespace == "" || podName == "" {
		return nil, fmt.Errorf("POD_NAMESPACE and POD_NAME must both be set")
	}
	pod := &corev1.Pod{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: podName}, pod); err != nil {
		return nil, fmt.Errorf("reading the operator's own pod: %w", err)
	}
	labels := map[string]string{}
	for k, v := range pod.Labels {
		if k != labelPodTemplateHash {
			labels[k] = v
		}
	}
	if len(labels) == 0 {
		return nil, fmt.Errorf("the operator's pod %s/%s has no labels to select it by", namespace, podName)
	}
	return &OperatorIdentity{Namespace: namespace, PodLabels: labels}, nil
}

// clusterJobComponents are the jobs that connect to the database over SQL.
// The restore check does not: it reads a bucket, before any database exists.
func clusterJobComponents() []string {
	return []string{componentBackupStatus, componentRestoreVerify, componentComplianceCheck}
}

// networkPolicyWanted reports whether a cluster gets a NetworkPolicy.
func networkPolicyWanted(cluster *dbv1alpha1.PostgresCluster) bool {
	return cluster.Spec.Compliance != nil
}

// desiredNetworkPolicy lists everything that may reach a cluster's database
// pods. Once any policy selects a pod, whatever it does not list is refused,
// so a policy that named only the clients would also cut off replication,
// Patroni, the operator, and the cluster's own jobs. Only ingress is
// restricted; egress is left alone.
//
// Kubelet's probes come from the node and need no rule: measured with this
// policy on Kind, both pods stayed Ready.
func desiredNetworkPolicy(cluster *dbv1alpha1.PostgresCluster, operator *OperatorIdentity) (*networkingv1.NetworkPolicy, error) {
	if operator == nil {
		return nil, fmt.Errorf("the operator does not know which pods it runs as")
	}
	tcp := corev1.ProtocolTCP
	postgres := intstr.FromInt32(postgresPort)
	patroni := intstr.FromInt32(patroniPort)

	// Applications: labelled pods in this namespace, and whatever else the
	// spec lists. Copied, so the spec is never changed by what is built from it.
	clients := []networkingv1.NetworkPolicyPeer{{
		PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{labelClient: labelClientValue}},
	}}
	for i := range cluster.Spec.Compliance.Network.AllowFrom {
		clients = append(clients, *cluster.Spec.Compliance.Network.AllowFrom[i].DeepCopy())
	}

	ownPods := networkingv1.NetworkPolicyPeer{
		PodSelector: &metav1.LabelSelector{MatchLabels: podSelectorLabels(cluster)},
	}
	ownJobs := networkingv1.NetworkPolicyPeer{
		PodSelector: &metav1.LabelSelector{
			MatchLabels: map[string]string{labelAppName: appNameValue, labelAppInstance: cluster.Name},
			MatchExpressions: []metav1.LabelSelectorRequirement{{
				Key: labelAppComponent, Operator: metav1.LabelSelectorOpIn, Values: clusterJobComponents(),
			}},
		},
	}
	operatorPods := networkingv1.NetworkPolicyPeer{
		NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{labelNamespaceName: operator.Namespace}},
		PodSelector:       &metav1.LabelSelector{MatchLabels: operator.PodLabels},
	}

	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: cluster.Name, Namespace: cluster.Namespace, Labels: labelsForCluster(cluster)},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: podSelectorLabels(cluster)},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{
				{From: clients, Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &postgres}}},
				// Replication and pg_rewind on 5432; Patroni's members ask each
				// other about their state, and the failsafe, on 8008.
				{From: []networkingv1.NetworkPolicyPeer{ownPods}, Ports: []networkingv1.NetworkPolicyPort{
					{Protocol: &tcp, Port: &postgres}, {Protocol: &tcp, Port: &patroni}}},
				{From: []networkingv1.NetworkPolicyPeer{ownJobs}, Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &postgres}}},
				// The operator applies spec.ha and asks for a switchover.
				{From: []networkingv1.NetworkPolicyPeer{operatorPods}, Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &patroni}}},
			},
		},
	}, nil
}

// reconcileNetworkPolicy keeps the cluster's NetworkPolicy while it has
// compliance settings, and removes it when they go. It runs before the pods,
// so they are never reachable by more than the policy allows.
func (r *PostgresClusterReconciler) reconcileNetworkPolicy(ctx context.Context, cluster *dbv1alpha1.PostgresCluster) error {
	policy := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: cluster.Name, Namespace: cluster.Namespace}}
	if !networkPolicyWanted(cluster) {
		return client.IgnoreNotFound(r.Delete(ctx, policy))
	}
	desired, err := desiredNetworkPolicy(cluster, r.Operator)
	if err != nil {
		return err
	}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, policy, func() error {
		policy.Labels = desired.Labels
		policy.Spec = desired.Spec
		return controllerutil.SetControllerReference(cluster, policy, r.Scheme)
	})
	return err
}
