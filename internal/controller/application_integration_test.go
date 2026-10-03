//go:build integration

package controller

import (
	"context"
	"testing"

	platformv1alpha1 "edlab.dev/preview-environment-controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func TestEnvtestApplicationLifecycle(t *testing.T) {
	environment := &envtest.Environment{CRDDirectoryPaths: []string{"../../config/crd/bases", "../../test/fixtures/argocd"}, ErrorIfCRDPathMissing: true}
	cfg, err := environment.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Error(err)
		}
	})
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, networkingv1.AddToScheme, platformv1alpha1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, name := range []string{"argocd", "preview-control"} {
		if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}); err != nil {
			t.Fatal(err)
		}
	}
	stop := startTestManager(t, cfg, scheme, "argocd")
	p := applicationPreview()
	p.Namespace = "preview-control"
	p.UID = ""
	p.ResourceVersion = ""
	p.Generation = 0
	if err := c.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	key := client.ObjectKeyFromObject(p)
	appkey := types.NamespacedName{Namespace: "argocd", Name: applicationName(p)}
	app := ApplicationObject()
	eventually(t, func() bool { return c.Get(ctx, appkey, app) == nil })
	if err := c.Get(ctx, key, p); err != nil {
		t.Fatal(err)
	}
	for _, obj := range baselineResources(p.Spec.Namespace) {
		if err := c.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
			t.Fatal("Application preceded baseline", err)
		}
	}
	waitReason := func(reason string) {
		t.Helper()
		eventually(t, func() bool {
			current := &platformv1alpha1.PreviewEnvironment{}
			if c.Get(ctx, key, current) != nil {
				return false
			}
			condition := meta.FindStatusCondition(current.Status.Conditions, "Ready")
			return condition != nil && condition.Reason == reason && condition.ObservedGeneration == current.Generation
		})
	}
	waitReason("ApplicationPending")
	healthyApplication(app)
	if err := c.Update(ctx, app); err != nil {
		t.Fatal(err)
	}
	waitReason("ApplicationHealthy") // A mapped status watch, without timed polling.
	current := &platformv1alpha1.PreviewEnvironment{}
	if err := c.Get(ctx, key, current); err != nil {
		t.Fatal(err)
	}
	if current.Status.SyncStatus != "Synced" || current.Status.ApplicationHealth != "Healthy" || current.Status.Application != appkey.Name {
		t.Fatal(current.Status)
	}
	// Actual CRD validation prevents source removal and missing revisions.
	invalid := current.DeepCopy()
	invalid.Spec.Repository = ""
	invalid.Spec.Revision = ""
	invalid.Spec.Path = ""
	if err := c.Update(ctx, invalid); !apierrors.IsInvalid(err) {
		t.Fatal("source removal accepted", err)
	}
	invalid = current.DeepCopy()
	invalid.Spec.Revision = ""
	if err := c.Update(ctx, invalid); !apierrors.IsInvalid(err) {
		t.Fatal("missing revision accepted", err)
	}
	current.Spec.Revision = "feature-456"
	if err := c.Update(ctx, current); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool {
		return c.Get(ctx, appkey, app) == nil && appString(app, "spec", "source", "targetRevision") == "feature-456"
	})
	waitReason("ApplicationPending") // Old health cannot satisfy the new source.
	healthyApplication(app)
	if err := c.Update(ctx, app); err != nil {
		t.Fatal(err)
	}
	waitReason("ApplicationHealthy")
	// Repair Application drift and preserve metadata.
	if err := c.Get(ctx, appkey, app); err != nil {
		t.Fatal(err)
	}
	annotations := app.GetAnnotations()
	annotations["example.com/note"] = "preserve"
	app.SetAnnotations(annotations)
	app.Object["spec"].(map[string]any)["project"] = "default"
	if err := c.Update(ctx, app); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool {
		return c.Get(ctx, appkey, app) == nil && appString(app, "spec", "project") == "preview-environments" && app.GetAnnotations()["example.com/note"] == "preserve"
	})
	app.Object["status"].(map[string]any)["conditions"] = []any{map[string]any{"type": "ComparisonError", "message": "revision not found"}}
	if err := c.Update(ctx, app); err != nil {
		t.Fatal(err)
	}
	waitReason("ApplicationError")
	if err := c.Get(ctx, appkey, app); err != nil {
		t.Fatal(err)
	}
	healthyApplication(app)
	if err := c.Update(ctx, app); err != nil {
		t.Fatal(err)
	}
	waitReason("ApplicationHealthy")
	// Manual Application deletion is repaired only after its finalizer completes.
	if err := c.Delete(ctx, app); err != nil {
		t.Fatal(err)
	}
	waitReason("ApplicationTerminating")
	if err := c.Get(ctx, appkey, app); err != nil {
		t.Fatal(err)
	}
	oldUID := app.GetUID()
	app.SetFinalizers(nil) // Simulate Argo CD; envtest has no Argo controller.
	if err := c.Update(ctx, app); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return c.Get(ctx, appkey, app) == nil && app.GetUID() != oldUID })
	// Delete the preview and restart while Application cleanup is waiting.
	if err := c.Get(ctx, key, current); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, current); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return c.Get(ctx, appkey, app) == nil && !app.GetDeletionTimestamp().IsZero() })
	ns := &corev1.Namespace{}
	nskey := types.NamespacedName{Name: p.Spec.Namespace}
	if err := c.Get(ctx, nskey, ns); err != nil || !ns.DeletionTimestamp.IsZero() {
		t.Fatal("namespace deleted before Application completion", err)
	}
	stop()
	startTestManager(t, cfg, scheme, "argocd")
	if err := c.Get(ctx, appkey, app); err != nil {
		t.Fatal(err)
	}
	app.SetFinalizers(nil)
	if err := c.Update(ctx, app); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return c.Get(ctx, nskey, ns) == nil && !ns.DeletionTimestamp.IsZero() })
	deleteNamespaceContents(t, c, ns.Name)
	ns.Spec.Finalizers = nil
	if err := c.SubResource("finalize").Update(ctx, ns); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool {
		return apierrors.IsNotFound(c.Get(ctx, key, current)) && apierrors.IsNotFound(c.Get(ctx, appkey, app))
	})
}
