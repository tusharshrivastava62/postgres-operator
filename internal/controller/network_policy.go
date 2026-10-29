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
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

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

// ingressRestricted reports whether what can connect to the cluster's database
// pods is limited: a compliance preset does it, and so does spec.security.
func ingressRestricted(cluster *dbv1alpha1.PostgresCluster) bool {
	return cluster.Spec.Compliance != nil || cluster.Spec.Security.RestrictsIngress()
}

// egressRestricted reports whether what the cluster's database pods can
// connect to is limited.
func egressRestricted(cluster *dbv1alpha1.PostgresCluster) bool {
	return cluster.Spec.Security.RestrictsEgress()
}

// networkPolicyWanted reports whether a cluster gets a NetworkPolicy.
func networkPolicyWanted(cluster *dbv1alpha1.PostgresCluster) bool {
	return ingressRestricted(cluster) || egressRestricted(cluster)
}

const (
	// The cluster's DNS: what its pods look names up with. Another DNS than the
	// usual one is listed in spec.security.networkPolicy.egress.allow.
	dnsNamespace = "kube-system"
	dnsLabelKey  = "k8s-app"
	dnsLabelVal  = "kube-dns"
	dnsPort      = 53

	// The Kubernetes API server's own Endpoints, which list where it is.
	apiServerNamespace = "default"
	apiServerName      = "kubernetes"
)

// apiServerEndpoint is one address the Kubernetes API server answers on.
type apiServerEndpoint struct {
	IP   string
	Port int32
}

// apiServerEndpoints reads where the API server is. Patroni keeps its leader
// lock there, so a pod that cannot reach it loses its lock, and a policy that
// forgot it would take the cluster down. A policy cannot name a Service, only
// the addresses behind it, so they are read and written into the policy, and
// read again at every look. It is an error to find none: a policy is not
// written that could cut every pod off.
func apiServerEndpoints(ctx context.Context, reader client.Reader) ([]apiServerEndpoint, error) {
	endpoints := &corev1.Endpoints{} //nolint:staticcheck // the API server's addresses are only published here and in EndpointSlices
	if err := reader.Get(ctx, client.ObjectKey{Namespace: apiServerNamespace, Name: apiServerName}, endpoints); err != nil {
		return nil, fmt.Errorf("reading where the Kubernetes API server is: %w", err)
	}
	var found []apiServerEndpoint
	for _, subset := range endpoints.Subsets {
		for _, address := range subset.Addresses {
			for _, port := range subset.Ports {
				if port.Protocol == "" || port.Protocol == corev1.ProtocolTCP {
					found = append(found, apiServerEndpoint{IP: address.IP, Port: port.Port})
				}
			}
		}
	}
	if len(found) == 0 {
		return nil, fmt.Errorf("the Endpoints %s/%s list no address for the Kubernetes API server", apiServerNamespace, apiServerName)
	}
	slices.SortFunc(found, func(a, b apiServerEndpoint) int {
		return cmp.Or(cmp.Compare(a.IP, b.IP), cmp.Compare(a.Port, b.Port))
	})
	return found, nil
}

// cidrOf is an address as a CIDR that names only it.
func cidrOf(ip string) string {
	if strings.Contains(ip, ":") {
		return ip + "/128"
	}
	return ip + "/32"
}

// desiredNetworkPolicy lists everything that may reach a cluster's database
// pods, and, if the cluster limits it, everything they may reach. Once any
// policy selects a pod, whatever it does not list is refused, so a policy that
// named only the clients would also cut off replication, Patroni, the operator,
// and the cluster's own jobs; and one that named only DNS would cut off the
// pods from each other and from the API server.
//
// Kubelet's probes come from the node and need no rule: measured with this
// policy on Kind, both pods stayed Ready.
func desiredNetworkPolicy(cluster *dbv1alpha1.PostgresCluster, operator *OperatorIdentity, apiServers []apiServerEndpoint) (*networkingv1.NetworkPolicy, error) {
	spec := networkingv1.NetworkPolicySpec{PodSelector: metav1.LabelSelector{MatchLabels: podSelectorLabels(cluster)}}
	if ingressRestricted(cluster) {
		if operator == nil {
			return nil, fmt.Errorf("the operator does not know which pods it runs as")
		}
		spec.PolicyTypes = append(spec.PolicyTypes, networkingv1.PolicyTypeIngress)
		spec.Ingress = ingressRules(cluster, operator)
	}
	if egressRestricted(cluster) {
		if len(apiServers) == 0 {
			return nil, fmt.Errorf("the policy needs the Kubernetes API server's addresses, and has none")
		}
		spec.PolicyTypes = append(spec.PolicyTypes, networkingv1.PolicyTypeEgress)
		spec.Egress = egressRules(cluster, apiServers)
	}
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: cluster.Name, Namespace: cluster.Namespace, Labels: labelsForCluster(cluster)},
		Spec:       spec,
	}, nil
}

func ingressRules(cluster *dbv1alpha1.PostgresCluster, operator *OperatorIdentity) []networkingv1.NetworkPolicyIngressRule {
	tcp := corev1.ProtocolTCP
	postgres := intstr.FromInt32(postgresPort)
	patroni := intstr.FromInt32(patroniPort)

	// Applications: labelled pods in this namespace, and whatever else the
	// spec lists. Copied, so the spec is never changed by what is built from it.
	clients := []networkingv1.NetworkPolicyPeer{{
		PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{labelClient: labelClientValue}},
	}}
	if cluster.Spec.Compliance != nil {
		for i := range cluster.Spec.Compliance.Network.AllowFrom {
			clients = append(clients, *cluster.Spec.Compliance.Network.AllowFrom[i].DeepCopy())
		}
	}
	for _, peer := range cluster.Spec.Security.IngressAllowFrom() {
		clients = append(clients, *peer.DeepCopy())
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
	return []networkingv1.NetworkPolicyIngressRule{
		{From: clients, Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &postgres}}},
		// Replication and pg_rewind on 5432; Patroni's members ask each
		// other about their state, and the failsafe, on 8008.
		{From: []networkingv1.NetworkPolicyPeer{ownPods}, Ports: []networkingv1.NetworkPolicyPort{
			{Protocol: &tcp, Port: &postgres}, {Protocol: &tcp, Port: &patroni}}},
		{From: []networkingv1.NetworkPolicyPeer{ownJobs}, Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &postgres}}},
		// The operator applies spec.ha and asks for a switchover on
		// 8008, and manages the database's roles over SQL on 5432.
		{From: []networkingv1.NetworkPolicyPeer{operatorPods}, Ports: []networkingv1.NetworkPolicyPort{
			{Protocol: &tcp, Port: &postgres}, {Protocol: &tcp, Port: &patroni}}},
	}
}

// egressRules is where the database pods may connect: DNS, each other, the API
// server, and what the spec adds.
func egressRules(cluster *dbv1alpha1.PostgresCluster, apiServers []apiServerEndpoint) []networkingv1.NetworkPolicyEgressRule {
	tcp, udp := corev1.ProtocolTCP, corev1.ProtocolUDP
	dns := intstr.FromInt32(dnsPort)
	postgres := intstr.FromInt32(postgresPort)
	patroni := intstr.FromInt32(patroniPort)

	blocks := make([]networkingv1.NetworkPolicyPeer, 0, len(apiServers))
	var ports []networkingv1.NetworkPolicyPort
	seen := map[int32]bool{}
	for _, server := range apiServers {
		blocks = append(blocks, networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: cidrOf(server.IP)}})
		if !seen[server.Port] {
			seen[server.Port] = true
			port := intstr.FromInt32(server.Port)
			ports = append(ports, networkingv1.NetworkPolicyPort{Protocol: &tcp, Port: &port})
		}
	}

	listed := cluster.Spec.Security.EgressAllow()
	rules := append(make([]networkingv1.NetworkPolicyEgressRule, 0, 3+len(listed)), []networkingv1.NetworkPolicyEgressRule{
		{
			To: []networkingv1.NetworkPolicyPeer{{
				NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{labelNamespaceName: dnsNamespace}},
				PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{dnsLabelKey: dnsLabelVal}},
			}},
			Ports: []networkingv1.NetworkPolicyPort{{Protocol: &udp, Port: &dns}, {Protocol: &tcp, Port: &dns}},
		},
		// Replication, pg_rewind and Patroni's members asking each other.
		{
			To: []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{MatchLabels: podSelectorLabels(cluster)}}},
			Ports: []networkingv1.NetworkPolicyPort{
				{Protocol: &tcp, Port: &postgres}, {Protocol: &tcp, Port: &patroni}},
		},
		// Patroni's leader lock.
		{To: blocks, Ports: ports},
	}...)
	for _, rule := range listed {
		rules = append(rules, *rule.DeepCopy())
	}
	return rules
}

// reconcileNetworkPolicy keeps the cluster's NetworkPolicy while it limits
// anything, and removes it when it does not. It runs before the pods, so they
// are never reachable by more than the policy allows.
func (r *PostgresClusterReconciler) reconcileNetworkPolicy(ctx context.Context, cluster *dbv1alpha1.PostgresCluster) error {
	policy := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: cluster.Name, Namespace: cluster.Namespace}}
	if !networkPolicyWanted(cluster) {
		return client.IgnoreNotFound(r.Delete(ctx, policy))
	}
	desired, err := r.desiredPolicy(ctx, cluster)
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

// desiredPolicy is desiredNetworkPolicy with the API server's addresses read, if
// the policy limits egress and so has to name them.
func (r *PostgresClusterReconciler) desiredPolicy(ctx context.Context, cluster *dbv1alpha1.PostgresCluster) (*networkingv1.NetworkPolicy, error) {
	var servers []apiServerEndpoint
	if egressRestricted(cluster) {
		var err error
		if servers, err = apiServerEndpoints(ctx, r.Client); err != nil {
			return nil, err
		}
	}
	return desiredNetworkPolicy(cluster, r.Operator, servers)
}
