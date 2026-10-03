package controller

import (
	"context"
	"errors"
	"testing"

	platformv1alpha1 "edlab.dev/preview-environment-controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func testClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := platformv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := networkingv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&platformv1alpha1.PreviewEnvironment{}).
		WithIndex(&platformv1alpha1.PreviewEnvironment{}, namespaceIndex, func(obj client.Object) []string {
			name, err := desiredNamespace(obj.(*platformv1alpha1.PreviewEnvironment))
			if err != nil {
				return nil
			}
			return []string{name}
		}).WithObjects(objects...).Build()
}

func testPreview() *platformv1alpha1.PreviewEnvironment {
	return &platformv1alpha1.PreviewEnvironment{ObjectMeta: metav1.ObjectMeta{
		Name: "feature-123", Namespace: "default", UID: "12345678-1234-1234-1234-123456789abc", Generation: 1,
	}, Spec: platformv1alpha1.PreviewEnvironmentSpec{Namespace: "preview-feature-123"}}
}

func reconcile(t *testing.T, r *PreviewEnvironmentReconciler, preview *platformv1alpha1.PreviewEnvironment) error {
	t.Helper()
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(preview)})
	return err
}

func readyReason(t *testing.T, c client.Client, preview *platformv1alpha1.PreviewEnvironment) string {
	t.Helper()
	current := &platformv1alpha1.PreviewEnvironment{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(preview), current); err != nil {
		t.Fatal(err)
	}
	cond := meta.FindStatusCondition(current.Status.Conditions, "Ready")
	if cond == nil {
		t.Fatal("missing Ready condition")
	}
	if current.Status.ObservedGeneration != preview.Generation || cond.ObservedGeneration != preview.Generation {
		t.Fatal("stale observedGeneration")
	}
	return cond.Reason
}

func TestNamespaceCreationAndIdempotency(t *testing.T) {
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
	if ns.Labels[ownerUIDLabel] != string(p.UID) || len(ns.OwnerReferences) != 0 {
		t.Fatal("incorrect namespace association")
	}
	if reason := readyReason(t, c, p); reason != "BaselineProvisioned" {
		t.Fatal(reason)
	}
	// Fail every mutation: an already correct reconcile must make no writes at all.
	r.Client = &rejectWrites{Client: c}
	if err := reconcile(t, r, p); err != nil {
		t.Fatalf("non-idempotent reconciliation: %v", err)
	}
	r.Client = c
	if err := c.Delete(context.Background(), ns); err != nil {
		t.Fatal(err)
	}
	if err := reconcile(t, r, p); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(ns), ns); err != nil {
		t.Fatal("namespace not recreated:", err)
	}
}

type rejectWrites struct{ client.Client }

func (c *rejectWrites) Create(context.Context, client.Object, ...client.CreateOption) error {
	return errors.New("unexpected create")
}
func (c *rejectWrites) Update(context.Context, client.Object, ...client.UpdateOption) error {
	return errors.New("unexpected update")
}
func (c *rejectWrites) Patch(context.Context, client.Object, client.Patch, ...client.PatchOption) error {
	return errors.New("unexpected patch")
}
func (c *rejectWrites) Status() client.SubResourceWriter { return rejectStatus{c.Client.Status()} }

type rejectStatus struct{ client.SubResourceWriter }

func (rejectStatus) Create(context.Context, client.Object, client.Object, ...client.SubResourceCreateOption) error {
	return errors.New("unexpected status create")
}
func (rejectStatus) Update(context.Context, client.Object, ...client.SubResourceUpdateOption) error {
	return errors.New("unexpected status update")
}
func (rejectStatus) Patch(context.Context, client.Object, client.Patch, ...client.SubResourcePatchOption) error {
	return errors.New("unexpected status patch")
}

func TestNamespaceConflictAndRecovery(t *testing.T) {
	p := testPreview()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: p.Spec.Namespace}}
	c := testClient(t, p, ns)
	r := &PreviewEnvironmentReconciler{Client: c}
	if err := reconcile(t, r, p); err != nil {
		t.Fatal(err)
	}
	if reason := readyReason(t, c, p); reason != "NamespaceConflict" {
		t.Fatal(reason)
	}
	if requests := r.namespaceRequests(context.Background(), ns); len(requests) != 1 || requests[0].Name != p.Name {
		t.Fatal("conflict event was not mapped", requests)
	}
	if err := c.Delete(context.Background(), ns); err != nil {
		t.Fatal(err)
	}
	if err := reconcile(t, r, p); err != nil {
		t.Fatal(err)
	}
	if reason := readyReason(t, c, p); reason != "BaselineProvisioned" {
		t.Fatal(reason)
	}
}

type failCreate struct{ client.Client }

func (c failCreate) Create(context.Context, client.Object, ...client.CreateOption) error {
	return errors.New("temporary API failure")
}
func TestTransientFailure(t *testing.T) {
	p := testPreview()
	c := testClient(t, p)
	r := &PreviewEnvironmentReconciler{Client: failCreate{c}}
	if err := reconcile(t, r, p); err == nil {
		t.Fatal("expected retryable error")
	}
	if reason := readyReason(t, c, p); reason != "NamespaceProvisioningFailed" {
		t.Fatal(reason)
	}
	r.Client = c
	if err := reconcile(t, r, p); err != nil {
		t.Fatal(err)
	}
	if reason := readyReason(t, c, p); reason != "BaselineProvisioned" {
		t.Fatal(reason)
	}
}

func TestDesiredNamespace(t *testing.T) {
	for _, name := range []string{"kube-system", "preview-", "preview-UPPER", "preview-a.b"} {
		p := testPreview()
		p.Spec.Namespace = name
		if _, err := desiredNamespace(p); err == nil {
			t.Errorf("accepted %q", name)
		}
	}
	sameNamespace := testPreview()
	sameNamespace.Namespace = sameNamespace.Spec.Namespace
	if _, err := desiredNamespace(sameNamespace); err == nil {
		t.Fatal("accepted preview inside its target namespace")
	}
	p := testPreview()
	p.Spec.Namespace = ""
	name, err := desiredNamespace(p)
	if err != nil || name != "preview-"+string(p.UID) {
		t.Fatal(name, err)
	}
}

func TestInvalidConfiguration(t *testing.T) {
	p := testPreview()
	p.Spec.Namespace = "kube-system"
	c := testClient(t, p)
	if err := reconcile(t, &PreviewEnvironmentReconciler{Client: c}, p); err != nil {
		t.Fatal(err)
	}
	if reason := readyReason(t, c, p); reason != "InvalidConfiguration" {
		t.Fatal(reason)
	}
	namespaces := &corev1.NamespaceList{}
	if err := c.List(context.Background(), namespaces); err != nil {
		t.Fatal(err)
	}
	if len(namespaces.Items) != 0 {
		t.Fatal("invalid configuration created namespace")
	}
}

func (c *rejectWrites) Apply(context.Context, runtime.ApplyConfiguration, ...client.ApplyOption) error {
	return errors.New("unexpected apply")
}
func (rejectStatus) Apply(context.Context, runtime.ApplyConfiguration, ...client.SubResourceApplyOption) error {
	return errors.New("unexpected status apply")
}

func (c *rejectWrites) Delete(context.Context, client.Object, ...client.DeleteOption) error {
	return errors.New("unexpected delete")
}
