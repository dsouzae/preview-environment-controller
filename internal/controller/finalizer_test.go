package controller

import (
	"context"
	"errors"
	"slices"
	"testing"

	platformv1alpha1 "edlab.dev/preview-environment-controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

func deletingPreview(t *testing.T, c client.Client, p *platformv1alpha1.PreviewEnvironment) {
	t.Helper()
	current := &platformv1alpha1.PreviewEnvironment{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(p), current); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(context.Background(), current); err != nil {
		t.Fatal(err)
	}
}

func assertFinalizer(t *testing.T, c client.Client, p *platformv1alpha1.PreviewEnvironment, want bool) {
	t.Helper()
	current := &platformv1alpha1.PreviewEnvironment{}
	err := c.Get(context.Background(), client.ObjectKeyFromObject(p), current)
	if !want && apierrors.IsNotFound(err) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(current.Finalizers, cleanupFinalizer) != want {
		t.Fatalf("finalizers = %v", current.Finalizers)
	}
}

func TestFinalizerPersistedBeforeProvisioning(t *testing.T) {
	p := testPreview()
	c := testClient(t, p)
	r := &PreviewEnvironmentReconciler{Client: failCreate{c}}
	if err := reconcile(t, r, p); err == nil {
		t.Fatal("expected create failure")
	}
	assertFinalizer(t, c, p, true)
	// A fresh reconciler after a simulated process exit resumes from stored state.
	r = &PreviewEnvironmentReconciler{Client: c}
	if err := reconcile(t, r, p); err != nil {
		t.Fatal(err)
	}
	if reason := readyReason(t, c, p); reason != "NamespaceProvisioned" {
		t.Fatal(reason)
	}
}

func TestCleanupWaitsAndResumesAfterRestart(t *testing.T) {
	p := testPreview()
	p.Finalizers = []string{cleanupFinalizer, "example.com/other-controller"}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: p.Spec.Namespace, UID: "namespace-uid", Labels: map[string]string{ownerUIDLabel: string(p.UID)}, Finalizers: []string{"example.com/namespace-cleanup"}}}
	c := testClient(t, p, ns)
	deletingPreview(t, c, p)
	r := &PreviewEnvironmentReconciler{Client: c}
	result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(p)})
	if err != nil || result.RequeueAfter == 0 {
		t.Fatal("cleanup should wait", result, err)
	}
	assertFinalizer(t, c, p, true)
	if reason := readyReason(t, c, p); reason != "CleanupInProgress" {
		t.Fatal(reason)
	}
	currentNS := &corev1.Namespace{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(ns), currentNS); err != nil {
		t.Fatal(err)
	}
	if currentNS.DeletionTimestamp.IsZero() || len(currentNS.Finalizers) != 1 {
		t.Fatal("namespace finalizers must not be removed by controller")
	}
	// Repeating a waiting cleanup must do no writes at all.
	r.Client = &rejectWrites{Client: c}
	if err := reconcile(t, r, p); err != nil {
		t.Fatal(err)
	}
	currentNS.Finalizers = nil
	if err := c.Update(context.Background(), currentNS); err != nil {
		t.Fatal(err)
	}
	// Resume cleanup from durable state with a new reconciler instance.
	r = &PreviewEnvironmentReconciler{Client: c}
	if err := reconcile(t, r, p); err != nil {
		t.Fatal(err)
	}
	assertFinalizer(t, c, p, false)
	current := &platformv1alpha1.PreviewEnvironment{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(p), current); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(current.Finalizers, []string{"example.com/other-controller"}) {
		t.Fatal("removed another controller's finalizer")
	}
}

type failDelete struct{ client.Client }

func (c failDelete) Delete(context.Context, client.Object, ...client.DeleteOption) error {
	return errors.New("temporary delete failure")
}
func TestCleanupTransientDeleteFailure(t *testing.T) {
	p := testPreview()
	p.Finalizers = []string{cleanupFinalizer}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: p.Spec.Namespace, UID: "namespace-uid", Labels: map[string]string{ownerUIDLabel: string(p.UID)}}}
	c := testClient(t, p, ns)
	deletingPreview(t, c, p)
	r := &PreviewEnvironmentReconciler{Client: failDelete{c}}
	if err := reconcile(t, r, p); err == nil {
		t.Fatal("cleanup error must trigger retry")
	}
	assertFinalizer(t, c, p, true)
	if reason := readyReason(t, c, p); reason != "CleanupFailed" {
		t.Fatal(reason)
	}
	r.Client = c
	if err := reconcile(t, r, p); err != nil {
		t.Fatal(err)
	}
	if err := reconcile(t, r, p); err != nil {
		t.Fatal(err)
	}
	assertFinalizer(t, c, p, false)
}

func TestCleanupOwnershipConflict(t *testing.T) {
	for _, uid := range []string{"", "another-preview"} {
		t.Run(uid, func(t *testing.T) {
			p := testPreview()
			p.Finalizers = []string{cleanupFinalizer}
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: p.Spec.Namespace, UID: "namespace-uid", Labels: map[string]string{ownerUIDLabel: uid}}}
			c := testClient(t, p, ns)
			deletingPreview(t, c, p)
			r := &PreviewEnvironmentReconciler{Client: c}
			if err := reconcile(t, r, p); err != nil {
				t.Fatal(err)
			}
			assertFinalizer(t, c, p, true)
			if reason := readyReason(t, c, p); reason != "CleanupBlocked" {
				t.Fatal(reason)
			}
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(ns), ns); err != nil {
				t.Fatal(err)
			}
			if !ns.DeletionTimestamp.IsZero() {
				t.Fatal("deleted unrelated namespace")
			}
			ns.Labels[ownerUIDLabel] = string(p.UID)
			if err := c.Update(context.Background(), ns); err != nil {
				t.Fatal(err)
			}
			if err := reconcile(t, r, p); err != nil {
				t.Fatal(err)
			}
			if err := reconcile(t, r, p); err != nil {
				t.Fatal(err)
			}
			assertFinalizer(t, c, p, false)
		})
	}
}

type staleNamespaceClient struct{ client.Client }

func (c staleNamespaceClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*corev1.Namespace); ok {
		return apierrors.NewNotFound(schema.GroupResource{Resource: "namespaces"}, key.Name)
	}
	return c.Client.Get(ctx, key, obj, opts...)
}
func TestCleanupUsesFreshReads(t *testing.T) {
	p := testPreview()
	p.Finalizers = []string{cleanupFinalizer}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: p.Spec.Namespace, UID: "namespace-uid", Labels: map[string]string{ownerUIDLabel: string(p.UID)}, Finalizers: []string{"example.com/hold"}}}
	c := testClient(t, p, ns)
	deletingPreview(t, c, p)
	r := &PreviewEnvironmentReconciler{Client: staleNamespaceClient{c}, APIReader: c}
	if err := reconcile(t, r, p); err != nil {
		t.Fatal(err)
	}
	assertFinalizer(t, c, p, true)
	if reason := readyReason(t, c, p); reason != "CleanupInProgress" {
		t.Fatal(reason)
	}
}

type failRead struct{ client.Reader }

func (r failRead) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return errors.New("temporary read failure")
}
func TestCleanupMissingNamespaceAndReadFailure(t *testing.T) {
	p := testPreview()
	p.Finalizers = []string{cleanupFinalizer}
	c := testClient(t, p)
	deletingPreview(t, c, p)
	r := &PreviewEnvironmentReconciler{Client: c, APIReader: failRead{c}}
	if err := reconcile(t, r, p); err == nil {
		t.Fatal("expected retryable read error")
	}
	assertFinalizer(t, c, p, true)
	if reason := readyReason(t, c, p); reason != "CleanupFailed" {
		t.Fatal(reason)
	}
	r.APIReader = c
	if err := reconcile(t, r, p); err != nil {
		t.Fatal(err)
	}
	assertFinalizer(t, c, p, false)
}

func TestPreviewPredicate(t *testing.T) {
	p := testPreview()
	pred := previewPredicate()
	changed := p.DeepCopy()
	changed.Status.Phase = "Ready"
	if pred.Update(event.UpdateEvent{ObjectOld: p, ObjectNew: changed}) {
		t.Fatal("status-only event should be filtered")
	}
	changed = p.DeepCopy()
	now := metav1.Now()
	changed.DeletionTimestamp = &now
	if !pred.Update(event.UpdateEvent{ObjectOld: p, ObjectNew: changed}) {
		t.Fatal("deletion transition must enqueue")
	}
	changed = p.DeepCopy()
	changed.Finalizers = []string{cleanupFinalizer}
	if !pred.Update(event.UpdateEvent{ObjectOld: p, ObjectNew: changed}) {
		t.Fatal("finalizer transition must enqueue")
	}
	changed = p.DeepCopy()
	changed.Generation++
	if !pred.Update(event.UpdateEvent{ObjectOld: p, ObjectNew: changed}) {
		t.Fatal("spec update must enqueue")
	}
}

type changingNamespaceClient struct {
	client.Client
	t *testing.T
}

func (c changingNamespaceClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	c.t.Helper()
	options := &client.DeleteOptions{}
	options.ApplyOptions(opts)
	if options.Preconditions == nil || options.Preconditions.UID == nil || options.Preconditions.ResourceVersion == nil {
		c.t.Fatal("missing delete preconditions")
	}
	// Simulate a namespace metadata change between ownership check and deletion.
	ns := &corev1.Namespace{}
	if err := c.Get(ctx, types.NamespacedName{Name: obj.GetName()}, ns); err != nil {
		return err
	}
	ns.Labels[ownerUIDLabel] = "another-preview"
	if err := c.Update(ctx, ns); err != nil {
		return err
	}
	return c.Client.Delete(ctx, obj, opts...)
}
func TestCleanupDeleteRace(t *testing.T) {
	p := testPreview()
	p.Finalizers = []string{cleanupFinalizer}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: p.Spec.Namespace, UID: "namespace-uid", Labels: map[string]string{ownerUIDLabel: string(p.UID)}}}
	c := testClient(t, p, ns)
	deletingPreview(t, c, p)
	r := &PreviewEnvironmentReconciler{Client: changingNamespaceClient{c, t}}
	if err := reconcile(t, r, p); !apierrors.IsConflict(err) {
		t.Fatalf("expected stale resourceVersion conflict, got %v", err)
	}
	assertFinalizer(t, c, p, true)
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(ns), ns); err != nil {
		t.Fatal("namespace was deleted after association changed", err)
	}
	if !ns.DeletionTimestamp.IsZero() {
		t.Fatal("namespace was deleted after association changed")
	}
}

func TestFinalizerRegistrationFailureDoesNotProvision(t *testing.T) {
	p := testPreview()
	c := testClient(t, p)
	r := &PreviewEnvironmentReconciler{Client: &rejectWrites{Client: c}}
	if err := reconcile(t, r, p); err == nil {
		t.Fatal("expected finalizer patch failure")
	}
	assertFinalizer(t, c, p, false)
	namespaces := &corev1.NamespaceList{}
	if err := c.List(context.Background(), namespaces); err != nil {
		t.Fatal(err)
	}
	if len(namespaces.Items) != 0 {
		t.Fatal("namespace created before finalizer persisted")
	}
}

func TestCleanupFinalizerPatchFailure(t *testing.T) {
	p := testPreview()
	p.Finalizers = []string{cleanupFinalizer}
	c := testClient(t, p)
	deletingPreview(t, c, p)
	r := &PreviewEnvironmentReconciler{Client: &rejectWrites{Client: c}, APIReader: c}
	if err := reconcile(t, r, p); err == nil {
		t.Fatal("expected finalizer removal failure")
	}
	assertFinalizer(t, c, p, true)
	r.Client = c
	if err := reconcile(t, r, p); err != nil {
		t.Fatal(err)
	}
	assertFinalizer(t, c, p, false)
}
