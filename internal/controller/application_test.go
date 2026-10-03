package controller

import (
	"context"
	"errors"
	"testing"

	platformv1alpha1 "edlab.dev/preview-environment-controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func applicationPreview() *platformv1alpha1.PreviewEnvironment {
	p := testPreview()
	p.Spec.Repository = "https://example.com/preview.git"
	p.Spec.Revision = "main"
	p.Spec.Path = "deploy"
	return p
}

func argoReconciler(c client.Client) *PreviewEnvironmentReconciler {
	return &PreviewEnvironmentReconciler{Client: c, ArgoNamespace: "argocd", ArgoProject: "preview-environments"}
}

func getApplication(t *testing.T, c client.Client, p *platformv1alpha1.PreviewEnvironment) *unstructured.Unstructured {
	t.Helper()
	app := ApplicationObject()
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "argocd", Name: applicationName(p)}, app); err != nil {
		t.Fatal(err)
	}
	return app
}

// Argo CD does not expose a status subresource in the pinned CRD. Its own
// controller patches the root object; these helpers simulate that controller.
func healthyApplication(app *unstructured.Unstructured) {
	app.Object["status"] = map[string]any{
		"sync":   map[string]any{"status": "Synced", "comparedTo": map[string]any{"source": app.Object["spec"].(map[string]any)["source"], "destination": app.Object["spec"].(map[string]any)["destination"]}},
		"health": map[string]any{"status": "Healthy"},
	}
}

func TestApplicationLifecycleAndIdempotency(t *testing.T) {
	p := applicationPreview()
	c := testClient(t, p)
	r := argoReconciler(c)
	if err := reconcile(t, r, p); err != nil {
		t.Fatal(err)
	}
	if reason := readyReason(t, c, p); reason != "ApplicationPending" {
		t.Fatal(reason)
	}
	app := getApplication(t, c, p)
	if appString(app, "spec", "project") != "preview-environments" || appString(app, "spec", "source", "repoURL") != p.Spec.Repository || appString(app, "spec", "destination", "namespace") != p.Spec.Namespace || len(app.GetOwnerReferences()) != 0 {
		t.Fatal("incorrect Application specification or cross-namespace owner reference", app)
	}
	if len(app.GetFinalizers()) != 1 || app.GetFinalizers()[0] != argoFinalizer {
		t.Fatal(app.GetFinalizers())
	}
	healthyApplication(app)
	if err := c.Update(context.Background(), app); err != nil {
		t.Fatal(err)
	}
	if err := reconcile(t, r, p); err != nil {
		t.Fatal(err)
	}
	if reason := readyReason(t, c, p); reason != "ApplicationHealthy" {
		t.Fatal(reason)
	}
	r.Client = &rejectWrites{Client: c}
	if err := reconcile(t, r, p); err != nil {
		t.Fatalf("ready reconcile wrote: %v", err)
	}
	r.Client = c
	// Revision change must not inherit old Synced/Healthy readiness.
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(p), p); err != nil {
		t.Fatal(err)
	}
	p.Spec.Revision = "feature-456"
	p.Generation++
	if err := c.Update(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := reconcile(t, r, p); err != nil {
			t.Fatal(err)
		}
		if reason := readyReason(t, c, p); reason != "ApplicationPending" {
			t.Fatal("accepted stale readiness", reason)
		}
	}
	app = getApplication(t, c, p)
	healthyApplication(app)
	if err := c.Update(context.Background(), app); err != nil {
		t.Fatal(err)
	}
	if err := reconcile(t, r, p); err != nil {
		t.Fatal(err)
	}
	if reason := readyReason(t, c, p); reason != "ApplicationHealthy" {
		t.Fatal(reason)
	}
	// Repair spec drift without losing unrelated metadata/finalizers.
	app = getApplication(t, c, p)
	annotations := app.GetAnnotations()
	annotations["example.com/note"] = "keep"
	app.SetAnnotations(annotations)
	app.SetFinalizers(append(app.GetFinalizers(), "example.com/other"))
	app.Object["spec"].(map[string]any)["project"] = "default"
	if err := c.Update(context.Background(), app); err != nil {
		t.Fatal(err)
	}
	if err := reconcile(t, r, p); err != nil {
		t.Fatal(err)
	}
	app = getApplication(t, c, p)
	if appString(app, "spec", "project") != r.ArgoProject || app.GetAnnotations()["example.com/note"] != "keep" || len(app.GetFinalizers()) != 2 {
		t.Fatal("drift repair lost metadata")
	}
	// Simulate manual cascading deletion and its eventual completion.
	if err := c.Delete(context.Background(), app); err != nil {
		t.Fatal(err)
	}
	if err := reconcile(t, r, p); err != nil {
		t.Fatal(err)
	}
	if reason := readyReason(t, c, p); reason != "ApplicationTerminating" {
		t.Fatal(reason)
	}
	app = getApplication(t, c, p)
	app.SetFinalizers(nil)
	if err := c.Update(context.Background(), app); err != nil {
		t.Fatal(err)
	}
	if err := reconcile(t, r, p); err != nil {
		t.Fatal(err)
	}
	getApplication(t, c, p)
}

func TestApplicationReadinessFailures(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		mutate       func(*unstructured.Unstructured)
	}{
		{"healthy", "ApplicationHealthy", func(*unstructured.Unstructured) {}},
		{"out of sync", "ApplicationOutOfSync", func(a *unstructured.Unstructured) {
			a.Object["status"].(map[string]any)["sync"].(map[string]any)["status"] = "OutOfSync"
		}},
		{"degraded", "ApplicationUnhealthy", func(a *unstructured.Unstructured) {
			a.Object["status"].(map[string]any)["health"].(map[string]any)["status"] = "Degraded"
		}},
		{"invalid revision", "ApplicationError", func(a *unstructured.Unstructured) {
			a.Object["status"].(map[string]any)["conditions"] = []any{map[string]any{"type": "ComparisonError", "message": "revision not found"}}
		}},
		{"sync running", "ApplicationSyncing", func(a *unstructured.Unstructured) {
			a.Object["status"].(map[string]any)["operationState"] = map[string]any{"phase": "Running"}
		}},
		{"pending operation", "ApplicationSyncing", func(a *unstructured.Unstructured) { a.Object["operation"] = map[string]any{"sync": map[string]any{}} }},
		{"stale destination", "ApplicationPending", func(a *unstructured.Unstructured) {
			a.Object["status"].(map[string]any)["sync"].(map[string]any)["comparedTo"].(map[string]any)["destination"] = map[string]any{"namespace": "other"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := ApplicationObject()
			app.Object["spec"] = argoReconciler(nil).applicationSpec(applicationPreview(), "preview-feature-123")
			healthyApplication(app)
			tc.mutate(app)
			ready, reason, _ := applicationReadiness(app)
			if reason != tc.reason || (ready == metav1.ConditionTrue) != (tc.reason == "ApplicationHealthy") {
				t.Fatal(ready, reason)
			}
		})
	}
}

type failApplicationCreate struct{ client.Client }

func (c failApplicationCreate) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if obj.GetObjectKind().GroupVersionKind().Kind == "Application" {
		return errors.New("temporary Application API failure")
	}
	return c.Client.Create(ctx, obj, opts...)
}

func TestApplicationPartialFailureAndConflicts(t *testing.T) {
	p := applicationPreview()
	c := testClient(t, p)
	r := argoReconciler(failApplicationCreate{c})
	if err := reconcile(t, r, p); err == nil {
		t.Fatal("expected API failure")
	}
	if reason := readyReason(t, c, p); reason != "ApplicationProvisioningFailed" {
		t.Fatal(reason)
	}
	r.Client = c
	if err := reconcile(t, r, p); err != nil {
		t.Fatal(err)
	}
	app := getApplication(t, c, p)
	app.SetLabels(nil)
	if err := c.Update(context.Background(), app); err != nil {
		t.Fatal(err)
	}
	if err := reconcile(t, r, p); err != nil {
		t.Fatal(err)
	}
	if reason := readyReason(t, c, p); reason != "ApplicationConflict" {
		t.Fatal(reason)
	}
	// Unlabelled conflicts still map to the desired name, so removal can recover.
	requests := r.applicationRequests(context.Background(), app)
	if len(requests) != 1 || requests[0].NamespacedName != client.ObjectKeyFromObject(p) {
		t.Fatal(requests)
	}
	deletingPreview(t, c, p)
	if err := reconcile(t, r, p); err != nil {
		t.Fatal(err)
	}
	if reason := readyReason(t, c, p); reason != "CleanupBlocked" {
		t.Fatal(reason)
	}
	ns := &corev1.Namespace{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: p.Spec.Namespace}, ns); err != nil || !ns.DeletionTimestamp.IsZero() {
		t.Fatal("namespace deletion bypassed Application conflict", err)
	}
}

func TestApplicationCleanupPrecedesNamespace(t *testing.T) {
	p := applicationPreview()
	c := testClient(t, p)
	r := argoReconciler(c)
	if err := reconcile(t, r, p); err != nil {
		t.Fatal(err)
	}
	deletingPreview(t, c, p)
	r.Client = failDelete{c}
	if err := reconcile(t, r, p); err == nil {
		t.Fatal("expected transient deletion failure")
	}
	assertFinalizer(t, c, p, true)
	r.Client = c
	if err := reconcile(t, r, p); err != nil {
		t.Fatal(err)
	}
	app := getApplication(t, c, p)
	if app.GetDeletionTimestamp().IsZero() {
		t.Fatal("Application deletion not requested")
	}
	ns := &corev1.Namespace{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: p.Spec.Namespace}, ns); err != nil || !ns.DeletionTimestamp.IsZero() {
		t.Fatal("namespace deleted before Application completion", err)
	}
	// A restart must continue waiting and perform no writes on equal state.
	r = argoReconciler(&rejectWrites{Client: c})
	if err := reconcile(t, r, p); err != nil {
		t.Fatal(err)
	}
	app.SetFinalizers(nil)
	if err := c.Update(context.Background(), app); err != nil {
		t.Fatal(err)
	}
	r = argoReconciler(c)
	if err := reconcile(t, r, p); err != nil {
		t.Fatal(err)
	}
	if err := reconcile(t, r, p); err != nil {
		t.Fatal(err)
	}
	assertFinalizer(t, c, p, false)
	if err := c.Get(context.Background(), types.NamespacedName{Name: p.Spec.Namespace}, ns); !apierrors.IsNotFound(err) {
		t.Fatal("namespace leaked", err)
	}
}

func TestApplicationDisabledBlocksReadinessAndCleanup(t *testing.T) {
	p := applicationPreview()
	c := testClient(t, p)
	r := &PreviewEnvironmentReconciler{Client: c}
	if err := reconcile(t, r, p); err != nil {
		t.Fatal(err)
	}
	if reason := readyReason(t, c, p); reason != "ApplicationConfigurationInvalid" {
		t.Fatal(reason)
	}
	deletingPreview(t, c, p)
	if err := reconcile(t, r, p); err != nil {
		t.Fatal(err)
	}
	assertFinalizer(t, c, p, true)
	if reason := readyReason(t, c, p); reason != "CleanupBlocked" {
		t.Fatal(reason)
	}
}

type staleApplicationClient struct{ client.Client }

func (c staleApplicationClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if obj.GetObjectKind().GroupVersionKind().Kind == "Application" {
		return apierrors.NewNotFound(schema.GroupResource{Group: "argoproj.io", Resource: "applications"}, key.Name)
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

func TestApplicationCleanupUsesFreshReads(t *testing.T) {
	p := applicationPreview()
	c := testClient(t, p)
	r := argoReconciler(c)
	if err := reconcile(t, r, p); err != nil {
		t.Fatal(err)
	}
	deletingPreview(t, c, p)
	r.Client = staleApplicationClient{c}
	r.APIReader = c
	if err := reconcile(t, r, p); err != nil {
		t.Fatal(err)
	}
	app := getApplication(t, c, p)
	if app.GetDeletionTimestamp().IsZero() {
		t.Fatal("fresh Application read did not initiate deletion")
	}
	assertFinalizer(t, c, p, true)
	ns := &corev1.Namespace{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: p.Spec.Namespace}, ns); err != nil || !ns.DeletionTimestamp.IsZero() {
		t.Fatal("stale NotFound skipped Application cleanup", err)
	}
	r.APIReader = failRead{c}
	if err := reconcile(t, r, p); err == nil {
		t.Fatal("expected transient cleanup read error")
	}
	assertFinalizer(t, c, p, true)
}

type changingApplicationClient struct{ client.Client }

func (c changingApplicationClient) changeAssociation(ctx context.Context, obj client.Object) error {
	app := ApplicationObject()
	if err := c.Get(ctx, client.ObjectKeyFromObject(obj), app); err != nil {
		return err
	}
	labels := app.GetLabels()
	labels[ownerUIDLabel] = "another-preview"
	app.SetLabels(labels)
	return c.Update(ctx, app)
}

func (c changingApplicationClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	if obj.GetObjectKind().GroupVersionKind().Kind == "Application" {
		if err := c.changeAssociation(ctx, obj); err != nil {
			return err
		}
	}
	return c.Client.Patch(ctx, obj, patch, opts...)
}

func (c changingApplicationClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	if obj.GetObjectKind().GroupVersionKind().Kind == "Application" {
		if err := c.changeAssociation(ctx, obj); err != nil {
			return err
		}
	}
	return c.Client.Delete(ctx, obj, opts...)
}

func TestApplicationOwnershipRaces(t *testing.T) {
	for _, deletion := range []bool{false, true} {
		t.Run(map[bool]string{false: "patch", true: "delete"}[deletion], func(t *testing.T) {
			p := applicationPreview()
			c := testClient(t, p)
			r := argoReconciler(c)
			if err := reconcile(t, r, p); err != nil {
				t.Fatal(err)
			}
			if deletion {
				deletingPreview(t, c, p)
			} else {
				app := getApplication(t, c, p)
				app.Object["spec"].(map[string]any)["project"] = "default"
				if err := c.Update(context.Background(), app); err != nil {
					t.Fatal(err)
				}
			}
			r.Client = changingApplicationClient{c}
			if err := reconcile(t, r, p); !apierrors.IsConflict(err) {
				t.Fatal("expected resourceVersion conflict", err)
			}
			app := getApplication(t, c, p)
			if !app.GetDeletionTimestamp().IsZero() || app.GetLabels()[ownerUIDLabel] != "another-preview" {
				t.Fatal("modified a concurrently reassociated Application")
			}
			if !deletion && appString(app, "spec", "project") != "default" {
				t.Fatal("patch overwrote concurrent metadata change")
			}
			assertFinalizer(t, c, p, true)
		})
	}
}
