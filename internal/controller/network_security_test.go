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
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	dbv1alpha1 "github.com/tusharshrivastava62/postgres-operator/api/v1alpha1"
)

const (
	testAPIServer = "172.19.0.2"
	testStoreCIDR = "203.0.113.0/24"

	testSecondAPIServer = "172.19.0.3"
)

var servers = []apiServerEndpoint{{IP: testAPIServer, Port: 6443}, {IP: testSecondAPIServer, Port: 6443}}

func securityOf(policy *dbv1alpha1.NetworkPolicySpec) *dbv1alpha1.PostgresCluster {
	c := testClusterT.DeepCopy()
	c.Spec.Security = &dbv1alpha1.SecuritySpec{NetworkPolicy: policy}
	return c
}

func restrictedEgress(allow ...networkingv1.NetworkPolicyEgressRule) *dbv1alpha1.NetworkPolicySpec {
	return &dbv1alpha1.NetworkPolicySpec{Egress: &dbv1alpha1.EgressPolicySpec{Mode: dbv1alpha1.NetworkPolicyRestricted, Allow: allow}}
}

func restrictedIngress(from ...networkingv1.NetworkPolicyPeer) *dbv1alpha1.NetworkPolicySpec {
	return &dbv1alpha1.NetworkPolicySpec{Ingress: &dbv1alpha1.IngressPolicySpec{Mode: dbv1alpha1.NetworkPolicyRestricted, AllowFrom: from}}
}

func TestWhichDirectionsAreLimited(t *testing.T) {
	cases := []struct {
		name            string
		cluster         *dbv1alpha1.PostgresCluster
		ingress, egress bool
	}{
		{"nothing asked", testClusterT.DeepCopy(), false, false},
		{"empty security", securityOf(nil), false, false},
		{"ingress", securityOf(restrictedIngress()), true, false},
		{"egress", securityOf(restrictedEgress()), false, true},
		{"both", securityOf(&dbv1alpha1.NetworkPolicySpec{
			Ingress: restrictedIngress().Ingress, Egress: restrictedEgress().Egress}), true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ingressRestricted(c.cluster); got != c.ingress {
				t.Errorf("ingress %v, want %v", got, c.ingress)
			}
			if got := egressRestricted(c.cluster); got != c.egress {
				t.Errorf("egress %v, want %v", got, c.egress)
			}
			if got := networkPolicyWanted(c.cluster); got != (c.ingress || c.egress) {
				t.Errorf("wanted %v", got)
			}
		})
	}
	// A compliance preset limits ingress by itself, and egress not at all.
	preset := testClusterT.DeepCopy()
	preset.Spec.Compliance = &dbv1alpha1.ComplianceSpec{Standard: dbv1alpha1.StandardHIPAA}
	if !ingressRestricted(preset) || egressRestricted(preset) {
		t.Error("a compliance preset limits ingress and not egress")
	}
}

func TestEgressIsLimitedToDNSTheClustersOwnPodsTheAPIServerAndWhatIsListed(t *testing.T) {
	store := networkingv1.NetworkPolicyEgressRule{
		To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: testStoreCIDR}}},
	}
	cluster := securityOf(restrictedEgress(store))
	policy, err := desiredNetworkPolicy(cluster, nil, servers)
	if err != nil {
		t.Fatal(err)
	}
	spec := policy.Spec
	if len(spec.PolicyTypes) != 1 || spec.PolicyTypes[0] != networkingv1.PolicyTypeEgress {
		t.Fatalf("types %v: only egress is limited", spec.PolicyTypes)
	}
	if len(spec.Ingress) != 0 {
		t.Error("ingress was not asked for, and a policy that lists none for it would refuse everything")
	}
	if got := spec.PodSelector.MatchLabels; got["app.kubernetes.io/component"] != componentPostgres || got["app.kubernetes.io/instance"] != sourceName {
		t.Errorf("selects %v: the cluster's database pods and nothing else", got)
	}
	if len(spec.Egress) != 4 {
		t.Fatalf("%d rules, want DNS, own pods, API server, and the one listed", len(spec.Egress))
	}

	dns, own, api, listed := spec.Egress[0], spec.Egress[1], spec.Egress[2], spec.Egress[3]
	if dns.To[0].PodSelector.MatchLabels["k8s-app"] != "kube-dns" ||
		dns.To[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "kube-system" {
		t.Errorf("DNS: %+v", dns.To[0])
	}
	if len(dns.Ports) != 2 || *dns.Ports[0].Protocol != corev1.ProtocolUDP || *dns.Ports[1].Protocol != corev1.ProtocolTCP ||
		dns.Ports[0].Port.IntVal != 53 || dns.Ports[1].Port.IntVal != 53 {
		t.Errorf("DNS ports: %+v", dns.Ports)
	}
	if len(own.To) != 1 || own.To[0].PodSelector.MatchLabels["app.kubernetes.io/component"] != componentPostgres ||
		own.To[0].NamespaceSelector != nil {
		t.Errorf("own pods: %+v", own.To)
	}
	ports := make([]int32, 0, len(own.Ports))
	for _, p := range own.Ports {
		ports = append(ports, p.Port.IntVal)
	}
	if len(ports) != 2 || ports[0] != 5432 || ports[1] != 8008 {
		t.Errorf("own pods' ports %v", ports)
	}
	if len(api.To) != 2 || api.To[0].IPBlock.CIDR != testAPIServer+"/32" || api.To[1].IPBlock.CIDR != "172.19.0.3/32" {
		t.Errorf("API server: %+v", api.To)
	}
	if len(api.Ports) != 1 || api.Ports[0].Port.IntVal != 6443 {
		t.Errorf("API server ports %+v: one port, listed once", api.Ports)
	}
	if listed.To[0].IPBlock.CIDR != testStoreCIDR {
		t.Errorf("what the spec lists comes last: %+v", listed)
	}

	// Copied: building the policy never changes the spec.
	spec.Egress[3].To[0].IPBlock.CIDR = "0.0.0.0/0"
	if cluster.Spec.Security.NetworkPolicy.Egress.Allow[0].To[0].IPBlock.CIDR != testStoreCIDR {
		t.Error("the spec was changed by what was built from it")
	}
}

func TestEgressNeedsTheAPIServerAddressesAndTheOperatorIsNotNeededForIt(t *testing.T) {
	cluster := securityOf(restrictedEgress())
	if _, err := desiredNetworkPolicy(cluster, nil, nil); err == nil {
		t.Error("a policy that could cut every pod off from the API server was built")
	}
	if _, err := desiredNetworkPolicy(cluster, nil, servers); err != nil {
		t.Errorf("egress does not name the operator: %v", err)
	}
	// Ingress does, and is refused without it.
	if _, err := desiredNetworkPolicy(securityOf(restrictedIngress()), nil, nil); err == nil {
		t.Error("an ingress policy was built that would cut the operator off")
	}
}

func TestIngressForAClusterWithoutAPresetIsThePresetsPolicy(t *testing.T) {
	peer := networkingv1.NetworkPolicyPeer{PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{labelApp: "reports"}}}
	policy, err := desiredNetworkPolicy(securityOf(restrictedIngress(peer)), testOperator, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(policy.Spec.PolicyTypes) != 1 || policy.Spec.PolicyTypes[0] != networkingv1.PolicyTypeIngress || len(policy.Spec.Egress) != 0 {
		t.Fatalf("types %v, egress %v: only ingress is limited", policy.Spec.PolicyTypes, policy.Spec.Egress)
	}
	if len(policy.Spec.Ingress) != 4 {
		t.Fatalf("%d ingress rules, the preset's four", len(policy.Spec.Ingress))
	}
	clients := policy.Spec.Ingress[0].From
	if len(clients) != 2 || clients[0].PodSelector.MatchLabels["db.youroperator.io/client"] != "true" ||
		clients[1].PodSelector.MatchLabels[labelApp] != "reports" {
		t.Errorf("who may connect: %+v", clients)
	}
}

func TestBothDirectionsAndAPresetListEveryPeerThatWasAsked(t *testing.T) {
	fromPreset := networkingv1.NetworkPolicyPeer{PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{labelApp: "from-preset"}}}
	fromSecurity := networkingv1.NetworkPolicyPeer{PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{labelApp: "from-security"}}}
	cluster := securityOf(&dbv1alpha1.NetworkPolicySpec{Ingress: restrictedIngress(fromSecurity).Ingress, Egress: restrictedEgress().Egress})
	cluster.Spec.Compliance = &dbv1alpha1.ComplianceSpec{Standard: dbv1alpha1.StandardHIPAA, Network: dbv1alpha1.ComplianceNetworkSpec{
		AllowFrom: []networkingv1.NetworkPolicyPeer{fromPreset}}}
	policy, err := desiredNetworkPolicy(cluster, testOperator, servers)
	if err != nil {
		t.Fatal(err)
	}
	if len(policy.Spec.PolicyTypes) != 2 || len(policy.Spec.Ingress) != 4 || len(policy.Spec.Egress) != 3 {
		t.Fatalf("types %v, %d ingress, %d egress rules", policy.Spec.PolicyTypes, len(policy.Spec.Ingress), len(policy.Spec.Egress))
	}
	clients := policy.Spec.Ingress[0].From
	if len(clients) != 3 || clients[1].PodSelector.MatchLabels[labelApp] != "from-preset" || clients[2].PodSelector.MatchLabels[labelApp] != "from-security" {
		t.Errorf("who may connect: %+v", clients)
	}
}

func TestAddressesAreWrittenAsCIDRsThatNameOnlyThem(t *testing.T) {
	if got := cidrOf("10.0.0.7"); got != "10.0.0.7/32" {
		t.Errorf("IPv4: %s", got)
	}
	if got := cidrOf("fd00::7"); got != "fd00::7/128" {
		t.Errorf("IPv6: %s", got)
	}
}

//nolint:staticcheck // the API server's addresses are published in Endpoints
func endpointsFor(subsets ...corev1.EndpointSubset) client.Reader {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	endpoints := &corev1.Endpoints{ //nolint:staticcheck // the API server's addresses are published here
		ObjectMeta: metav1.ObjectMeta{Namespace: apiServerNamespace, Name: apiServerName}, Subsets: subsets}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(endpoints).Build()
}

func TestTheAPIServerIsFoundWhereItsEndpointsSayItIs(t *testing.T) {
	reader := endpointsFor(corev1.EndpointSubset{ //nolint:staticcheck // see endpointsFor
		Addresses: []corev1.EndpointAddress{{IP: testSecondAPIServer}, {IP: testAPIServer}},
		Ports: []corev1.EndpointPort{
			{Port: 6443, Protocol: corev1.ProtocolTCP}, {Port: 53, Protocol: corev1.ProtocolUDP}, {Port: 8443},
		},
	})
	got, err := apiServerEndpoints(context.Background(), reader)
	if err != nil {
		t.Fatal(err)
	}
	want := []apiServerEndpoint{{testAPIServer, 6443}, {testAPIServer, 8443}, {testSecondAPIServer, 6443}, {testSecondAPIServer, 8443}}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v: TCP only, and no port-less default dropped", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got %v, want %v: in a fixed order, so the policy does not change on every look", got, want)
			break
		}
	}
}

func TestNoAPIServerAddressIsAnErrorAndNotAnEmptyList(t *testing.T) {
	if _, err := apiServerEndpoints(context.Background(), endpointsFor()); err == nil {
		t.Error("an empty list was accepted")
	}
	empty := fake.NewClientBuilder().Build()
	if _, err := apiServerEndpoints(context.Background(), empty); err == nil {
		t.Error("missing Endpoints were accepted")
	}
}

var _ = Describe("Limiting what the database pods connect to, through the reconciler", func() {
	const namespace = metav1.NamespaceDefault
	ctx := context.Background()
	key := func(name string) types.NamespacedName { return types.NamespacedName{Name: name, Namespace: namespace} }
	reconcileOnce := func(name string) {
		rec := &PostgresClusterReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), Patroni: &fakePatroni{live: map[string]any{}}, Operator: testOperator,
		}
		_, err := rec.Reconcile(ctx, reconcile.Request{NamespacedName: key(name)})
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
	}
	create := func(name string, policy *dbv1alpha1.NetworkPolicySpec) {
		c := &dbv1alpha1.PostgresCluster{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: dbv1alpha1.PostgresClusterSpec{
				Version: "17", Instances: 3,
				Storage:   dbv1alpha1.StorageSpec{Size: resource.MustParse("1Gi")},
				Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}},
				Security:  &dbv1alpha1.SecuritySpec{NetworkPolicy: policy},
			},
		}
		ExpectWithOffset(1, k8sClient.Create(ctx, c)).To(Succeed())
		DeferCleanup(func() { ExpectWithOffset(1, client.IgnoreNotFound(k8sClient.Delete(ctx, c))).To(Succeed()) })
	}
	policyOf := func(name string) (*networkingv1.NetworkPolicy, error) {
		p := &networkingv1.NetworkPolicy{}
		return p, k8sClient.Get(ctx, key(name), p)
	}
	update := func(name string, change func(*dbv1alpha1.PostgresCluster)) {
		c := &dbv1alpha1.PostgresCluster{}
		ExpectWithOffset(1, k8sClient.Get(ctx, key(name), c)).To(Succeed())
		change(c)
		ExpectWithOffset(1, k8sClient.Update(ctx, c)).To(Succeed())
	}

	It("writes an egress policy with the API server's real addresses, before the pods, owned by the cluster", func() {
		create("ne-create", restrictedEgress())
		reconcileOnce("ne-create")
		policy, err := policyOf("ne-create")
		Expect(err).NotTo(HaveOccurred())
		Expect(policy.OwnerReferences).NotTo(BeEmpty())
		Expect(policy.Spec.PolicyTypes).To(ConsistOf(networkingv1.PolicyTypeEgress))
		Expect(policy.Spec.Ingress).To(BeEmpty())

		real, err := apiServerEndpoints(ctx, k8sClient)
		Expect(err).NotTo(HaveOccurred())
		api := policy.Spec.Egress[2]
		Expect(api.To).To(HaveLen(len(real)))
		for i, server := range real {
			Expect(api.To[i].IPBlock.CIDR).To(Equal(cidrOf(server.IP)))
		}
		Expect(k8sClient.Get(ctx, key("ne-create"), &appsv1.StatefulSet{})).To(Succeed())
	})

	It("follows the list of where else the pods may connect", func() {
		create("ne-follow", restrictedEgress())
		reconcileOnce("ne-follow")
		policy, _ := policyOf("ne-follow")
		Expect(policy.Spec.Egress).To(HaveLen(3))

		port := intstr.FromInt32(8333)
		tcp := corev1.ProtocolTCP
		update("ne-follow", func(c *dbv1alpha1.PostgresCluster) {
			c.Spec.Security.NetworkPolicy.Egress.Allow = []networkingv1.NetworkPolicyEgressRule{{
				To:    []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{labelApp: "s3"}}}},
				Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &port}},
			}}
		})
		reconcileOnce("ne-follow")
		policy, _ = policyOf("ne-follow")
		Expect(policy.Spec.Egress).To(HaveLen(4))
		Expect(policy.Spec.Egress[3].Ports[0].Port.IntVal).To(Equal(int32(8333)))
	})

	It("is put back as it should be when it is edited by hand", func() {
		create("ne-repair", restrictedEgress())
		reconcileOnce("ne-repair")
		policy, _ := policyOf("ne-repair")
		policy.Spec.Egress = nil
		Expect(k8sClient.Update(ctx, policy)).To(Succeed())
		reconcileOnce("ne-repair")
		policy, _ = policyOf("ne-repair")
		Expect(policy.Spec.Egress).To(HaveLen(3))
	})

	It("gives a cluster that asked only for ingress the ingress policy, and a cluster that asked for neither none", func() {
		create("ne-ingress", restrictedIngress())
		reconcileOnce("ne-ingress")
		policy, err := policyOf("ne-ingress")
		Expect(err).NotTo(HaveOccurred())
		Expect(policy.Spec.PolicyTypes).To(ConsistOf(networkingv1.PolicyTypeIngress))
		Expect(policy.Spec.Ingress).To(HaveLen(4))
		Expect(policy.Spec.Egress).To(BeEmpty())

		create("ne-neither", nil)
		reconcileOnce("ne-neither")
		_, err = policyOf("ne-neither")
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})

	It("adds a direction to a policy that has one, and removes the policy when neither is asked for", func() {
		create("ne-both", restrictedIngress())
		reconcileOnce("ne-both")
		update("ne-both", func(c *dbv1alpha1.PostgresCluster) {
			c.Spec.Security.NetworkPolicy.Egress = restrictedEgress().Egress
		})
		reconcileOnce("ne-both")
		policy, _ := policyOf("ne-both")
		Expect(policy.Spec.PolicyTypes).To(ConsistOf(networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress))

		update("ne-both", func(c *dbv1alpha1.PostgresCluster) { c.Spec.Security = nil })
		reconcileOnce("ne-both")
		_, err := policyOf("ne-both")
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "no policy is left that limits nothing the spec asks for")
	})

	It("does not hold back an egress-only cluster when the operator does not know who it is", func() {
		create("ne-noident", restrictedEgress())
		rec := &PostgresClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Patroni: &fakePatroni{live: map[string]any{}}}
		_, err := rec.Reconcile(ctx, reconcile.Request{NamespacedName: key("ne-noident")})
		Expect(err).NotTo(HaveOccurred())
		_, err = policyOf("ne-noident")
		Expect(err).NotTo(HaveOccurred(), "egress rules do not name the operator")
	})
})
