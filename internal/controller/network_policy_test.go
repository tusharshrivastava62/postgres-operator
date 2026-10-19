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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	dbv1alpha1 "github.com/tusharshrivastava62/postgres-operator/api/v1alpha1"
)

// Label key and value the specs use for a pod that is not part of any cluster.
const (
	labelApp          = "app"
	apiApp            = "api"
	controlPlaneLabel = "control-plane"
	controllerManager = "controller-manager"
	operatorName      = "postgres-operator"
)

// testOperator is where the operator runs in the specs that build a network
// policy: the namespace and labels of a deployment made from config/.
var testOperator = &OperatorIdentity{
	Namespace: "postgres-operator-system",
	PodLabels: map[string]string{controlPlaneLabel: controllerManager, labelAppName: operatorName},
}

// matches reports whether a selector picks out a pod with the given labels.
func matches(selector *metav1.LabelSelector, podLabels map[string]string) bool {
	GinkgoHelper()
	s, err := metav1.LabelSelectorAsSelector(selector)
	Expect(err).NotTo(HaveOccurred())
	return s.Matches(labels.Set(podLabels))
}

var _ = Describe("The network policy", func() {
	cluster := func() *dbv1alpha1.PostgresCluster {
		c := withCompliance(plainCluster(), dbv1alpha1.StandardHIPAA)
		c.Name, c.Namespace = "orders", sourceNamespace
		return c
	}
	build := func(c *dbv1alpha1.PostgresCluster) *networkingv1.NetworkPolicy {
		GinkgoHelper()
		policy, err := desiredNetworkPolicy(c, testOperator)
		Expect(err).NotTo(HaveOccurred())
		return policy
	}
	// ruleFor finds the ingress rule that opens a port to a peer picked out by test.
	rulesOpening := func(policy *networkingv1.NetworkPolicy, port int32) []networkingv1.NetworkPolicyIngressRule {
		var out []networkingv1.NetworkPolicyIngressRule
		for _, rule := range policy.Spec.Ingress {
			for _, p := range rule.Ports {
				if p.Port.IntVal == port {
					out = append(out, rule)
					break
				}
			}
		}
		return out
	}
	// peerMatches reports whether any peer of a rule picks out a pod in the
	// given namespace with the given labels. namespaceLabels may be nil for a
	// peer with no namespace selector, which means the policy's own namespace.
	peerMatches := func(rule networkingv1.NetworkPolicyIngressRule, policyNamespace, namespace string, podLabels map[string]string) bool {
		for _, peer := range rule.From {
			if peer.IPBlock != nil {
				continue
			}
			if peer.NamespaceSelector == nil && namespace != policyNamespace {
				continue
			}
			if peer.NamespaceSelector != nil && !matches(peer.NamespaceSelector, map[string]string{labelNamespaceName: namespace}) {
				continue
			}
			if peer.PodSelector == nil || matches(peer.PodSelector, podLabels) {
				return true
			}
		}
		return false
	}
	// reaches reports whether the policy lets a pod in a namespace reach a port.
	reaches := func(policy *networkingv1.NetworkPolicy, namespace string, podLabels map[string]string, port int32) bool {
		for _, rule := range rulesOpening(policy, port) {
			if peerMatches(rule, policy.Namespace, namespace, podLabels) {
				return true
			}
		}
		return false
	}

	It("selects the cluster's database pods and nothing else, and restricts ingress only", func() {
		c := cluster()
		policy := build(c)
		Expect(policy.Spec.PolicyTypes).To(Equal([]networkingv1.PolicyType{networkingv1.PolicyTypeIngress}))
		Expect(policy.Spec.Egress).To(BeEmpty(), "egress is stage 14")
		Expect(matches(&policy.Spec.PodSelector, podSelectorLabels(c))).To(BeTrue())
		Expect(matches(&policy.Spec.PodSelector, backupStatusLabels(c))).To(BeFalse(), "a job's pods are not the database")
		Expect(matches(&policy.Spec.PodSelector, selectorLabelsFor("another"))).To(BeFalse())
		Expect(policy.Name).To(Equal("orders"))
		Expect(policy.Namespace).To(Equal(sourceNamespace))
	})

	It("opens only the ports PostgreSQL and Patroni listen on, over TCP", func() {
		for _, rule := range build(cluster()).Spec.Ingress {
			for _, port := range rule.Ports {
				Expect(*port.Protocol).To(Equal(corev1.ProtocolTCP))
				Expect(port.Port.IntVal).To(BeElementOf(postgresPort, patroniPort))
			}
		}
	})

	Describe("who may reach PostgreSQL (5432)", func() {
		It("lets pods labelled as clients in the cluster's own namespace in", func() {
			policy := build(cluster())
			client := map[string]string{labelClient: labelClientValue}
			Expect(reaches(policy, sourceNamespace, client, postgresPort)).To(BeTrue())
			Expect(reaches(policy, sourceNamespace, map[string]string{labelClient: "false"}, postgresPort)).To(BeFalse())
		})

		It("keeps out an unlabelled pod, and a labelled pod in another namespace", func() {
			policy := build(cluster())
			Expect(reaches(policy, sourceNamespace, map[string]string{labelApp: "anything"}, postgresPort)).To(BeFalse())
			Expect(reaches(policy, "staging", map[string]string{labelClient: labelClientValue}, postgresPort)).To(BeFalse(),
				"the client label counts only in the cluster's own namespace")
		})

		It("adds the peers the spec lists, as given", func() {
			c := cluster()
			c.Spec.Compliance.Network.AllowFrom = []networkingv1.NetworkPolicyPeer{{
				NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{labelNamespaceName: "apps"}},
				PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{labelApp: apiApp}},
			}}
			policy := build(c)
			Expect(reaches(policy, "apps", map[string]string{labelApp: apiApp}, postgresPort)).To(BeTrue())
			Expect(reaches(policy, "apps", map[string]string{labelApp: "other"}, postgresPort)).To(BeFalse())
			Expect(reaches(policy, sourceNamespace, map[string]string{labelApp: apiApp}, postgresPort)).To(BeFalse())
		})

		It("does not change the spec when it adds them", func() {
			c := cluster()
			c.Spec.Compliance.Network.AllowFrom = []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{}}}
			before := c.DeepCopy()
			policy := build(c)
			policy.Spec.Ingress[0].From[1].PodSelector.MatchLabels = map[string]string{"changed": "yes"}
			Expect(c.Spec).To(Equal(before.Spec))
		})
	})

	Describe("who may reach Patroni (8008)", func() {
		It("lets the cluster's own members in, for replication and for Patroni to talk to itself", func() {
			c := cluster()
			policy := build(c)
			Expect(reaches(policy, sourceNamespace, podSelectorLabels(c), patroniPort)).To(BeTrue())
			Expect(reaches(policy, sourceNamespace, podSelectorLabels(c), postgresPort)).To(BeTrue())
		})

		It("lets the operator in, found by its namespace and the labels of its own pods", func() {
			policy := build(cluster())
			Expect(reaches(policy, testOperator.Namespace, testOperator.PodLabels, patroniPort)).To(BeTrue())
		})

		It("keeps out the operator's namespace for any other pod, and any other namespace for a pod labelled like the operator", func() {
			policy := build(cluster())
			Expect(reaches(policy, testOperator.Namespace, map[string]string{labelApp: "other"}, patroniPort)).To(BeFalse())
			Expect(reaches(policy, sourceNamespace, testOperator.PodLabels, patroniPort)).To(BeFalse())
			Expect(reaches(policy, "staging", testOperator.PodLabels, patroniPort)).To(BeFalse())
		})

		It("keeps out clients and every extra peer, which reach PostgreSQL and nothing more", func() {
			c := cluster()
			c.Spec.Compliance.Network.AllowFrom = []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{}}}
			policy := build(c)
			Expect(reaches(policy, sourceNamespace, map[string]string{labelClient: labelClientValue}, patroniPort)).To(BeFalse())
			Expect(reaches(policy, sourceNamespace, map[string]string{labelApp: "anything"}, patroniPort)).To(BeFalse())
		})

		It("keeps out another cluster's members", func() {
			Expect(reaches(build(cluster()), sourceNamespace, selectorLabelsFor("another"), patroniPort)).To(BeFalse())
		})

		It("is opened by exactly two rules: the cluster's own members and the operator", func() {
			Expect(rulesOpening(build(cluster()), patroniPort)).To(HaveLen(2))
		})
	})

	Describe("the cluster's own jobs", func() {
		It("may reach PostgreSQL: the backup status check, the restore test and the compliance check", func() {
			c := cluster()
			policy := build(c)
			for _, component := range []string{componentBackupStatus, componentRestoreVerify, componentComplianceCheck} {
				Expect(reaches(policy, sourceNamespace, labelsWithComponent(c, component), postgresPort)).To(BeTrue(), component)
			}
		})

		It("may not reach Patroni", func() {
			c := cluster()
			Expect(reaches(build(c), sourceNamespace, labelsWithComponent(c, componentBackupStatus), patroniPort)).To(BeFalse())
		})

		It("do not include the restore check, which never opens a database connection", func() {
			c := cluster()
			Expect(reaches(build(c), sourceNamespace, labelsWithComponent(c, componentRestoreCheck), postgresPort)).To(BeFalse())
		})

		It("are only this cluster's, not another's", func() {
			c := cluster()
			other := withCompliance(plainCluster(), dbv1alpha1.StandardHIPAA)
			other.Name, other.Namespace = "billing", sourceNamespace
			Expect(reaches(build(c), sourceNamespace, labelsWithComponent(other, componentBackupStatus), postgresPort)).To(BeFalse())
		})

		It("are really matched by the labels the jobs carry", func() {
			c := cluster()
			c.Spec.Backup = backupCluster().Spec.Backup
			c.Spec.Backup.Verification = verifySpec
			policy := build(c)
			status := backupStatusCronJobSpec(c, "img").JobTemplate.Spec.Template.Labels
			verify := restoreVerifyCronJobSpec(c, "img").JobTemplate.Spec.Template.Labels
			Expect(reaches(policy, sourceNamespace, status, postgresPort)).To(BeTrue())
			Expect(reaches(policy, sourceNamespace, verify, postgresPort)).To(BeTrue())
		})
	})

	It("is refused, with a reason, when the operator does not know which pods it is", func() {
		_, err := desiredNetworkPolicy(cluster(), nil)
		Expect(err).To(MatchError(ContainSubstring("does not know which pods it runs as")))
	})

	It("is wanted for a cluster with compliance settings and no other", func() {
		Expect(networkPolicyWanted(cluster())).To(BeTrue())
		Expect(networkPolicyWanted(plainCluster())).To(BeFalse())
	})
})

var _ = Describe("The operator's identity", func() {
	const namespace = "default"
	ctx := context.Background()
	podWith := func(name string, podLabels map[string]string) {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: podLabels},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: testPodImage}}},
		}
		ExpectWithOffset(1, k8sClient.Create(ctx, pod)).To(Succeed())
		DeferCleanup(func() {
			ExpectWithOffset(1, client.IgnoreNotFound(k8sClient.Delete(ctx, pod, client.GracePeriodSeconds(0)))).To(Succeed())
		})
	}

	It("is read from the operator's own pod, without the label that changes with every rollout", func() {
		podWith("op-ok", map[string]string{controlPlaneLabel: controllerManager, "pod-template-hash": "7d9f8c", labelAppName: operatorName})
		id, err := DiscoverOperatorIdentity(ctx, k8sClient, namespace, "op-ok")
		Expect(err).NotTo(HaveOccurred())
		Expect(id.Namespace).To(Equal(namespace))
		Expect(id.PodLabels).To(Equal(map[string]string{controlPlaneLabel: controllerManager, labelAppName: operatorName}))
	})

	It("is refused when the pod's name or namespace is not known", func() {
		_, err := DiscoverOperatorIdentity(ctx, k8sClient, "", "op")
		Expect(err).To(MatchError(ContainSubstring("POD_NAMESPACE and POD_NAME")))
		_, err = DiscoverOperatorIdentity(ctx, k8sClient, namespace, "")
		Expect(err).To(HaveOccurred())
	})

	It("is refused when the pod cannot be read", func() {
		_, err := DiscoverOperatorIdentity(ctx, k8sClient, namespace, "no-such-pod")
		Expect(err).To(MatchError(ContainSubstring("reading the operator's own pod")))
	})

	It("is refused when the pod has no label to select it by, since an empty selector would match every pod", func() {
		podWith("op-bare", nil)
		_, err := DiscoverOperatorIdentity(ctx, k8sClient, namespace, "op-bare")
		Expect(err).To(MatchError(ContainSubstring("no labels")))
		podWith("op-hash-only", map[string]string{"pod-template-hash": "abc"})
		_, err = DiscoverOperatorIdentity(ctx, k8sClient, namespace, "op-hash-only")
		Expect(err).To(MatchError(ContainSubstring("no labels")))
	})
})

var _ = Describe("The network policy through the reconciler", func() {
	const namespace = "default"
	ctx := context.Background()
	key := func(name string) types.NamespacedName { return types.NamespacedName{Name: name, Namespace: namespace} }
	reconcileWith := func(operator *OperatorIdentity, name string) {
		rec := &PostgresClusterReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), Patroni: &fakePatroni{live: map[string]any{}}, Operator: operator,
		}
		_, err := rec.Reconcile(ctx, reconcile.Request{NamespacedName: key(name)})
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
	}
	reconcileOnce := func(name string) { reconcileWith(testOperator, name) }
	newCluster := func(name string, compliance bool) {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: auditSecret, Namespace: namespace},
			StringData: map[string]string{secretKeyAccessKey: "id", secretKeySecretKey: testSecretValue},
		}
		if err := k8sClient.Create(ctx, secret); err == nil {
			DeferCleanup(func() { ExpectWithOffset(1, client.IgnoreNotFound(k8sClient.Delete(ctx, secret))).To(Succeed()) })
		}
		c := &dbv1alpha1.PostgresCluster{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: dbv1alpha1.PostgresClusterSpec{
				Version: "17", Instances: 3,
				Storage:   dbv1alpha1.StorageSpec{Size: resource.MustParse("1Gi")},
				Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}},
			},
		}
		if compliance {
			withCompliance(c, dbv1alpha1.StandardHIPAA)
		}
		ExpectWithOffset(1, k8sClient.Create(ctx, c)).To(Succeed())
		DeferCleanup(func() { ExpectWithOffset(1, client.IgnoreNotFound(k8sClient.Delete(ctx, c))).To(Succeed()) })
	}
	policyOf := func(name string) (*networkingv1.NetworkPolicy, error) {
		p := &networkingv1.NetworkPolicy{}
		return p, k8sClient.Get(ctx, key(name), p)
	}
	progressing := func(name string) *metav1.Condition {
		c := &dbv1alpha1.PostgresCluster{}
		ExpectWithOffset(1, k8sClient.Get(ctx, key(name), c)).To(Succeed())
		return meta.FindStatusCondition(c.Status.Conditions, "Progressing")
	}

	It("is created for a cluster with compliance settings, owned by it, before the pods", func() {
		const name = "np-create"
		newCluster(name, true)
		reconcileOnce(name)
		policy, err := policyOf(name)
		Expect(err).NotTo(HaveOccurred())
		Expect(policy.OwnerReferences).NotTo(BeEmpty(), "it goes with the cluster")
		Expect(policy.Spec.PolicyTypes).To(ConsistOf(networkingv1.PolicyTypeIngress))
		Expect(policy.Spec.Ingress).To(HaveLen(4))
		Expect(k8sClient.Get(ctx, key(name), &appsv1.StatefulSet{})).To(Succeed())
	})

	It("is not created for a cluster without compliance settings, even when the operator knows who it is", func() {
		const name = "np-none"
		newCluster(name, false)
		reconcileOnce(name)
		_, err := policyOf(name)
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})

	It("does not hold back a cluster without compliance settings when the operator does not know who it is", func() {
		const name = "np-noident-plain"
		newCluster(name, false)
		reconcileWith(nil, name)
		Expect(k8sClient.Get(ctx, key(name), &appsv1.StatefulSet{})).To(Succeed())
	})

	It("holds back a cluster that wants one, and says why, when the operator does not know who it is", func() {
		const name = "np-noident"
		newCluster(name, true)
		reconcileWith(nil, name)
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key(name), &appsv1.StatefulSet{}))).To(BeTrue(),
			"nothing is built without the rule that lets the operator through")
		_, err := policyOf(name)
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
		cond := progressing(name)
		Expect(cond.Reason).To(Equal(reasonOperatorIdentity))
		Expect(cond.Message).To(ContainSubstring("POD_NAME"))
	})

	It("is put back as it should be when it is edited or deleted by hand", func() {
		const name = "np-repair"
		newCluster(name, true)
		reconcileOnce(name)
		policy, err := policyOf(name)
		Expect(err).NotTo(HaveOccurred())

		policy.Spec.Ingress = nil
		Expect(k8sClient.Update(ctx, policy)).To(Succeed())
		reconcileOnce(name)
		policy, err = policyOf(name)
		Expect(err).NotTo(HaveOccurred())
		Expect(policy.Spec.Ingress).To(HaveLen(4))

		Expect(k8sClient.Delete(ctx, policy)).To(Succeed())
		reconcileOnce(name)
		_, err = policyOf(name)
		Expect(err).NotTo(HaveOccurred())
	})

	It("follows the spec when the extra peers change", func() {
		const name = "np-peers"
		newCluster(name, true)
		reconcileOnce(name)
		c := &dbv1alpha1.PostgresCluster{}
		Expect(k8sClient.Get(ctx, key(name), c)).To(Succeed())
		c.Spec.Compliance.Network.AllowFrom = []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{labelApp: apiApp}}}}
		Expect(k8sClient.Update(ctx, c)).To(Succeed())
		reconcileOnce(name)
		policy, err := policyOf(name)
		Expect(err).NotTo(HaveOccurred())
		Expect(policy.Spec.Ingress[0].From).To(HaveLen(2))
	})

	It("is removed when the compliance settings go, and the pods are no longer isolated", func() {
		const name = "np-remove"
		newCluster(name, true)
		reconcileOnce(name)
		c := &dbv1alpha1.PostgresCluster{}
		Expect(k8sClient.Get(ctx, key(name), c)).To(Succeed())
		c.Spec.Compliance = nil
		Expect(k8sClient.Update(ctx, c)).To(Succeed())
		reconcileOnce(name)
		_, err := policyOf(name)
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})
})
