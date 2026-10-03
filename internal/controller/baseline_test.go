package controller

import (
	"context"
	"errors"
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

func TestBaselineDefaults(t *testing.T) {
	p := testPreview()
	c := testClient(t, p)
	r := &PreviewEnvironmentReconciler{Client: c}
	if err := reconcile(t, r, p); err != nil {
		t.Fatal(err)
	}
	ns := &corev1.Namespace{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: p.Spec.Namespace}, ns); err != nil {
		t.Fatal(err)
	}
	for _, obj := range baselineResources(ns.Name) {
		if err := c.Get(context.Background(), client.ObjectKeyFromObject(obj), obj); err != nil {
			t.Fatal(err)
		}
		if obj.GetLabels()[ownerUIDLabel] != string(p.UID) {
			t.Fatal("missing preview association", obj)
		}
		owners := obj.GetOwnerReferences()
		if len(owners) != 1 || owners[0].Kind != "Namespace" || owners[0].UID != ns.UID || owners[0].BlockOwnerDeletion == nil || *owners[0].BlockOwnerDeletion {
			t.Fatal("invalid owner reference", owners)
		}
	}
	quota := &corev1.ResourceQuota{}
	key := types.NamespacedName{Name: baselineName, Namespace: ns.Name}
	if err := c.Get(context.Background(), key, quota); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[corev1.ResourceName]string{
		corev1.ResourceRequestsCPU: "2", corev1.ResourceLimitsCPU: "4",
		corev1.ResourceRequestsMemory: "2Gi", corev1.ResourceLimitsMemory: "4Gi",
		corev1.ResourcePods: "10", corev1.ResourceServices: "5", corev1.ResourceConfigMaps: "20", corev1.ResourceSecrets: "20",
		corev1.ResourcePersistentVolumeClaims: "0", corev1.ResourceServicesLoadBalancers: "0", corev1.ResourceServicesNodePorts: "0",
	} {
		got := quota.Spec.Hard[name]
		if got.Cmp(resource.MustParse(value)) != 0 {
			t.Fatalf("quota %s = %s, want %s", name, got.String(), value)
		}
	}
	limits := &corev1.LimitRange{}
	if err := c.Get(context.Background(), key, limits); err != nil {
		t.Fatal(err)
	}
	if len(limits.Spec.Limits) != 1 {
		t.Fatal("expected one container limit")
	}
	item := limits.Spec.Limits[0]
	if item.Type != corev1.LimitTypeContainer {
		t.Fatal(item.Type)
	}
	for _, check := range []struct {
		list        corev1.ResourceList
		cpu, memory string
	}{
		{item.DefaultRequest, "100m", "128Mi"}, {item.Default, "500m", "256Mi"}, {item.Min, "10m", "16Mi"}, {item.Max, "1", "1Gi"},
	} {
		cpu, memory := check.list[corev1.ResourceCPU], check.list[corev1.ResourceMemory]
		if cpu.Cmp(resource.MustParse(check.cpu)) != 0 || memory.Cmp(resource.MustParse(check.memory)) != 0 {
			t.Fatal("incorrect container limits")
		}
	}
	sa := &corev1.ServiceAccount{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: workloadServiceAccount, Namespace: ns.Name}, sa); err != nil {
		t.Fatal(err)
	}
	if sa.AutomountServiceAccountToken == nil || *sa.AutomountServiceAccountToken {
		t.Fatal("workload tokens should not automount")
	}
	// Verify the complete baseline can reconcile with all writes rejected.
	r.Client = &rejectWrites{Client: c}
	if err := reconcile(t, r, p); err != nil {
		t.Fatal("baseline is not idempotent", err)
	}
}

func TestNetworkPolicyBoundaries(t *testing.T) {
	spec := networkPolicySpec()
	if len(spec.PodSelector.MatchLabels) != 0 || len(spec.PodSelector.MatchExpressions) != 0 {
		t.Fatal("policy must select all preview pods")
	}
	if len(spec.PolicyTypes) != 2 || spec.PolicyTypes[0] != networkingv1.PolicyTypeIngress || spec.PolicyTypes[1] != networkingv1.PolicyTypeEgress {
		t.Fatal("both directions must be isolated")
	}
	if len(spec.Ingress) != 1 || len(spec.Ingress[0].From) != 1 || spec.Ingress[0].From[0].PodSelector == nil || spec.Ingress[0].From[0].NamespaceSelector != nil {
		t.Fatal("ingress must be limited to pods in the same namespace")
	}
	if len(spec.Egress) != 2 || len(spec.Egress[0].To) != 1 || spec.Egress[0].To[0].PodSelector == nil || spec.Egress[0].To[0].NamespaceSelector != nil {
		t.Fatal("local egress is not scoped to same namespace")
	}
	dns := spec.Egress[1]
	if len(dns.To) != 1 || dns.To[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "kube-system" || dns.To[0].PodSelector.MatchLabels["k8s-app"] != "kube-dns" {
		t.Fatal("DNS must use the intersection of namespace and pod selectors")
	}
	if len(dns.Ports) != 2 {
		t.Fatal("DNS needs UDP and TCP")
	}
	for i, protocol := range []corev1.Protocol{corev1.ProtocolUDP, corev1.ProtocolTCP} {
		if dns.Ports[i].Protocol == nil || *dns.Ports[i].Protocol != protocol || dns.Ports[i].Port == nil || dns.Ports[i].Port.IntVal != 53 {
			t.Fatal("incorrect DNS port")
		}
	}
}

func TestBaselineDeletionAndDriftRepair(t *testing.T) {
	for _, obj := range baselineResources("preview-feature-123") {
		t.Run(fmt.Sprintf("%T", obj), func(t *testing.T) {
			p := testPreview()
			c := testClient(t, p)
			r := &PreviewEnvironmentReconciler{Client: c}
			if err := reconcile(t, r, p); err != nil {
				t.Fatal(err)
			}
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(obj), obj); err != nil {
				t.Fatal(err)
			}
			original := obj.DeepCopyObject().(client.Object)
			obj.SetAnnotations(map[string]string{"example.com/note": "preserve"})
			switch typed := obj.(type) {
			case *corev1.ResourceQuota:
				typed.Spec.Hard[corev1.ResourcePods] = resource.MustParse("999")
			case *corev1.LimitRange:
				typed.Spec.Limits[0].Default[corev1.ResourceCPU] = resource.MustParse("9")
			case *networkingv1.NetworkPolicy:
				typed.Spec.Ingress = nil
				typed.Spec.Egress = nil
			case *corev1.ServiceAccount:
				enabled := true
				typed.AutomountServiceAccountToken = &enabled
				typed.ImagePullSecrets = []corev1.LocalObjectReference{{Name: "registry-credentials"}}
			}
			if err := c.Update(context.Background(), obj); err != nil {
				t.Fatal(err)
			}
			if err := reconcile(t, r, p); err != nil {
				t.Fatal(err)
			}
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(obj), obj); err != nil {
				t.Fatal(err)
			}
			if obj.GetAnnotations()["example.com/note"] != "preserve" {
				t.Fatal("unmanaged annotation removed")
			}
			switch typed := obj.(type) {
			case *corev1.ResourceQuota:
				if !equality.Semantic.DeepEqual(typed.Spec, original.(*corev1.ResourceQuota).Spec) {
					t.Fatal("quota drift not repaired")
				}
			case *corev1.LimitRange:
				if !equality.Semantic.DeepEqual(typed.Spec, original.(*corev1.LimitRange).Spec) {
					t.Fatal("limit drift not repaired")
				}
			case *networkingv1.NetworkPolicy:
				if !equality.Semantic.DeepEqual(typed.Spec, original.(*networkingv1.NetworkPolicy).Spec) {
					t.Fatal("policy drift not repaired")
				}
			case *corev1.ServiceAccount:
				if *typed.AutomountServiceAccountToken || len(typed.ImagePullSecrets) != 1 {
					t.Fatal("service account fields not reconciled/preserved")
				}
			}
			if err := c.Delete(context.Background(), obj); err != nil {
				t.Fatal(err)
			}
			if requests := r.baselineRequests(context.Background(), obj); len(requests) != 1 || requests[0].Name != p.Name {
				t.Fatal("child deletion event not mapped")
			}
			if err := reconcile(t, r, p); err != nil {
				t.Fatal(err)
			}
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(obj), obj); err != nil {
				t.Fatal("child not recreated", err)
			}
		})
	}
}

type failBaselineCreate struct{ client.Client }

func (c failBaselineCreate) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if _, ok := obj.(*corev1.ResourceQuota); ok {
		return errors.New("temporary quota API failure")
	}
	return c.Client.Create(ctx, obj, opts...)
}
func TestBaselinePartialFailure(t *testing.T) {
	p := testPreview()
	c := testClient(t, p)
	r := &PreviewEnvironmentReconciler{Client: failBaselineCreate{c}}
	if err := reconcile(t, r, p); err == nil {
		t.Fatal("expected retryable baseline error")
	}
	if reason := readyReason(t, c, p); reason != "BaselineProvisioningFailed" {
		t.Fatal(reason)
	}
	if err := c.Get(context.Background(), types.NamespacedName{Name: baselineName, Namespace: p.Spec.Namespace}, &networkingv1.NetworkPolicy{}); err != nil {
		t.Fatal("expected partial success", err)
	}
	r = &PreviewEnvironmentReconciler{Client: c}
	if err := reconcile(t, r, p); err != nil {
		t.Fatal(err)
	}
	if reason := readyReason(t, c, p); reason != "BaselineProvisioned" {
		t.Fatal(reason)
	}
}

func TestBaselineConflictAndTerminating(t *testing.T) {
	for _, mode := range []string{"unassociated", "different-owner", "terminating"} {
		t.Run(mode, func(t *testing.T) {
			p := testPreview()
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: p.Spec.Namespace, UID: "namespace-uid", Labels: map[string]string{ownerUIDLabel: string(p.UID)}}}
			child := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: baselineName, Namespace: ns.Name}}
			switch mode {
			case "different-owner":
				child.Labels = map[string]string{ownerUIDLabel: string(p.UID)}
				child.OwnerReferences = []metav1.OwnerReference{{APIVersion: "v1", Kind: "Namespace", Name: ns.Name, UID: "old-namespace-uid"}}
			case "terminating":
				child.Labels = map[string]string{ownerUIDLabel: string(p.UID)}
				now := metav1.Now()
				child.DeletionTimestamp = &now
				child.Finalizers = []string{"example.com/hold"}
			}
			c := testClient(t, p, ns, child)
			r := &PreviewEnvironmentReconciler{Client: c}
			result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(p)})
			if err != nil {
				t.Fatal(err)
			}
			want := "BaselineConflict"
			if mode == "terminating" {
				want = "BaselineTerminating"
				if result.RequeueAfter == 0 {
					t.Fatal("terminating resource needs retry")
				}
			}
			if reason := readyReason(t, c, p); reason != want {
				t.Fatal(reason)
			}
			current := &networkingv1.NetworkPolicy{}
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(child), current); err != nil {
				t.Fatal(err)
			}
			if !equality.Semantic.DeepEqual(current.Spec, child.Spec) {
				t.Fatal("conflicting child was modified")
			}
		})
	}
}

func TestBaselinePredicateFiltersQuotaStatus(t *testing.T) {
	old := &corev1.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Name: baselineName, ResourceVersion: "1"}, Spec: quotaSpec()}
	changed := old.DeepCopy()
	changed.ResourceVersion = "2"
	changed.Status.Used = corev1.ResourceList{corev1.ResourcePods: resource.MustParse("1")}
	if baselinePredicate().Update(event.UpdateEvent{ObjectOld: old, ObjectNew: changed}) {
		t.Fatal("quota accounting should not trigger reconcile")
	}
	changed.Spec.Hard[corev1.ResourcePods] = resource.MustParse("100")
	if !baselinePredicate().Update(event.UpdateEvent{ObjectOld: old, ObjectNew: changed}) {
		t.Fatal("quota spec drift must trigger reconcile")
	}
	changed = old.DeepCopy()
	changed.Labels = map[string]string{"extra": "label"}
	if !baselinePredicate().Update(event.UpdateEvent{ObjectOld: old, ObjectNew: changed}) {
		t.Fatal("metadata drift must trigger reconcile")
	}
}

type changingBaselineClient struct{ client.Client }

func (c changingBaselineClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	if _, ok := obj.(*corev1.ResourceQuota); ok {
		current := &corev1.ResourceQuota{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(obj), current); err != nil {
			return err
		}
		current.Labels[ownerUIDLabel] = "another-preview"
		if err := c.Update(ctx, current); err != nil {
			return err
		}
	}
	return c.Client.Patch(ctx, obj, patch, opts...)
}

func TestBaselinePatchOwnershipRace(t *testing.T) {
	p := testPreview()
	c := testClient(t, p)
	r := &PreviewEnvironmentReconciler{Client: c}
	if err := reconcile(t, r, p); err != nil {
		t.Fatal(err)
	}
	quota := &corev1.ResourceQuota{}
	key := types.NamespacedName{Name: baselineName, Namespace: p.Spec.Namespace}
	if err := c.Get(context.Background(), key, quota); err != nil {
		t.Fatal(err)
	}
	quota.Spec.Hard[corev1.ResourcePods] = resource.MustParse("100")
	if err := c.Update(context.Background(), quota); err != nil {
		t.Fatal(err)
	}
	r.Client = changingBaselineClient{c}
	if err := reconcile(t, r, p); !apierrors.IsConflict(err) {
		t.Fatalf("expected optimistic patch conflict, got %v", err)
	}
	if err := c.Get(context.Background(), key, quota); err != nil {
		t.Fatal(err)
	}
	pods := quota.Spec.Hard[corev1.ResourcePods]
	if pods.Cmp(resource.MustParse("100")) != 0 || quota.Labels[ownerUIDLabel] != "another-preview" {
		t.Fatal("changed a concurrently reassociated quota")
	}
	r.Client = c
	if err := reconcile(t, r, p); err != nil {
		t.Fatal(err)
	}
	if reason := readyReason(t, c, p); reason != "BaselineConflict" {
		t.Fatal(reason)
	}
}
