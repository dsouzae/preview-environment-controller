package controller

import (
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	baselineName           = "preview-baseline"
	workloadServiceAccount = "preview-workload"
)

var (
	errBaselineConflict    = errors.New("baseline resource belongs to another owner")
	errBaselineTerminating = errors.New("baseline resource is terminating")
)

// +kubebuilder:rbac:groups="",resources=resourcequotas;limitranges;serviceaccounts,verbs=get;list;watch;create;patch
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;patch

// reconcileBaseline establishes policies before application workloads are added.
// The namespace is a valid cluster-scoped owner of these namespaced resources.
// This fixed profile intentionally avoids growing the CR API before deployment
// experience demonstrates a need for configurable budgets or access rules.
func (r *PreviewEnvironmentReconciler) reconcileBaseline(ctx context.Context, namespace *corev1.Namespace) error {
	for _, obj := range baselineResources(namespace.Name) {
		err := r.reconcileBaselineResource(ctx, namespace, obj)
		if err != nil {
			return fmt.Errorf("reconcile %T %s/%s: %w", obj, obj.GetNamespace(), obj.GetName(), err)
		}
	}
	return nil
}

// Use an optimistic patch so a cached read cannot authorize changes to a child
// whose metadata/association was changed concurrently after our ownership check.
func (r *PreviewEnvironmentReconciler) reconcileBaselineResource(ctx context.Context, namespace *corev1.Namespace, obj client.Object) error {
	err := r.Get(ctx, client.ObjectKeyFromObject(obj), obj)
	exists := err == nil
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	before := obj.DeepCopyObject().(client.Object)

	if !obj.GetDeletionTimestamp().IsZero() {
		return errBaselineTerminating
	}
	if exists && obj.GetLabels()[ownerUIDLabel] != namespace.Labels[ownerUIDLabel] {
		return errBaselineConflict
	}
	// A matching preview label is insufficient to take over an object owned by
	// a different namespace incarnation or a different controller.
	for _, owner := range obj.GetOwnerReferences() {
		if owner.Kind != "Namespace" || owner.APIVersion != "v1" || owner.UID != namespace.UID {
			return errBaselineConflict
		}
	}
	labels := obj.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels[ownerUIDLabel] = namespace.Labels[ownerUIDLabel]
	obj.SetLabels(labels)
	// Namespace deletion is handled by the namespace controller. Do not request
	// blockOwnerDeletion, which would require extra namespace/finalizers RBAC.
	if err := controllerutil.SetOwnerReference(namespace, obj, r.Client.Scheme(), controllerutil.WithBlockOwnerDeletion(false)); err != nil {
		return err
	}
	switch typed := obj.(type) {
	case *corev1.ResourceQuota:
		typed.Spec = quotaSpec()
	case *corev1.LimitRange:
		typed.Spec = limitRangeSpec()
	case *networkingv1.NetworkPolicy:
		typed.Spec = networkPolicySpec()
	case *corev1.ServiceAccount:
		disabled := false
		typed.AutomountServiceAccountToken = &disabled
		// Preserve imagePullSecrets and token-controller fields; these are not owned
		// by the baseline controller. No workload RoleBindings are created.
	default:
		return fmt.Errorf("unsupported baseline resource %T", obj)
	}
	if !exists {
		return r.Create(ctx, obj)
	}
	if equality.Semantic.DeepEqual(before, obj) {
		return nil
	}
	return r.Patch(ctx, obj, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
}

func baselineResources(namespace string) []client.Object {
	metadata := metav1.ObjectMeta{Name: baselineName, Namespace: namespace}
	return []client.Object{
		&networkingv1.NetworkPolicy{ObjectMeta: metadata},
		&corev1.LimitRange{ObjectMeta: metadata},
		&corev1.ResourceQuota{ObjectMeta: metadata},
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: workloadServiceAccount, Namespace: namespace}},
	}
}

func quotaSpec() corev1.ResourceQuotaSpec {
	return corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{
		corev1.ResourceRequestsCPU:            resource.MustParse("2"),
		corev1.ResourceRequestsMemory:         resource.MustParse("2Gi"),
		corev1.ResourceLimitsCPU:              resource.MustParse("4"),
		corev1.ResourceLimitsMemory:           resource.MustParse("4Gi"),
		corev1.ResourcePods:                   resource.MustParse("10"),
		corev1.ResourceServices:               resource.MustParse("5"),
		corev1.ResourceServicesLoadBalancers:  resource.MustParse("0"),
		corev1.ResourceServicesNodePorts:      resource.MustParse("0"),
		corev1.ResourceConfigMaps:             resource.MustParse("20"),
		corev1.ResourceSecrets:                resource.MustParse("20"),
		corev1.ResourcePersistentVolumeClaims: resource.MustParse("0"),
	}}
}

func limitRangeSpec() corev1.LimitRangeSpec {
	return corev1.LimitRangeSpec{Limits: []corev1.LimitRangeItem{{
		Type:           corev1.LimitTypeContainer,
		DefaultRequest: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("128Mi")},
		Default:        corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("256Mi")},
		Min:            corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("16Mi")},
		Max:            corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("1Gi")},
	}}}
}

func networkPolicySpec() networkingv1.NetworkPolicySpec {
	tcp, udp := corev1.ProtocolTCP, corev1.ProtocolUDP
	dnsPort := intstr.FromInt32(53)
	localPods := networkingv1.NetworkPolicyPeer{PodSelector: &metav1.LabelSelector{}}
	return networkingv1.NetworkPolicySpec{
		PodSelector: metav1.LabelSelector{},
		PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
		Ingress:     []networkingv1.NetworkPolicyIngressRule{{From: []networkingv1.NetworkPolicyPeer{localPods}}},
		Egress: []networkingv1.NetworkPolicyEgressRule{
			{To: []networkingv1.NetworkPolicyPeer{localPods}},
			{To: []networkingv1.NetworkPolicyPeer{{
				NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "kube-system"}},
				PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{"k8s-app": "kube-dns"}},
			}}, Ports: []networkingv1.NetworkPolicyPort{{Protocol: &udp, Port: &dnsPort}, {Protocol: &tcp, Port: &dnsPort}}},
		},
	}
}
