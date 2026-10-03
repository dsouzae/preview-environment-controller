//go:build integration

package controller

import (
	"context"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	platformv1alpha1 "edlab.dev/preview-environment-controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

func TestEnvtestLifecycle(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Fatal("KUBEBUILDER_ASSETS is required")
	}
	environment := &envtest.Environment{CRDDirectoryPaths: []string{"../../config/crd/bases"}, ErrorIfCRDPathMissing: true}
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
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := platformv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	stopManager := startTestManager(t, cfg, scheme)
	control := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "preview-control"}}
	if err := c.Create(ctx, control); err != nil {
		t.Fatal(err)
	}
	p := &platformv1alpha1.PreviewEnvironment{ObjectMeta: metav1.ObjectMeta{Name: "feature-123", Namespace: control.Name}, Spec: platformv1alpha1.PreviewEnvironmentSpec{Namespace: "preview-feature-123"}}
	if err := c.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	key := client.ObjectKeyFromObject(p)
	eventually(t, func() bool {
		current := &platformv1alpha1.PreviewEnvironment{}
		if c.Get(ctx, key, current) != nil {
			return false
		}
		condition := meta.FindStatusCondition(current.Status.Conditions, "Ready")
		return condition != nil && condition.Status == metav1.ConditionTrue && current.Status.ObservedGeneration == current.Generation
	})
	ns := &corev1.Namespace{}
	nskey := types.NamespacedName{Name: p.Spec.Namespace}
	if err := c.Get(ctx, nskey, ns); err != nil {
		t.Fatal(err)
	}
	originalUID := ns.UID
	// Direct repeated reconciliation verifies actual API resourceVersions stay stable.
	current := &platformv1alpha1.PreviewEnvironment{}
	if err := c.Get(ctx, key, current); err != nil {
		t.Fatal(err)
	}
	rv := current.ResourceVersion
	nsrv := ns.ResourceVersion
	r := &PreviewEnvironmentReconciler{Client: c, Scheme: scheme}
	for range 3 {
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Get(ctx, key, current); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, nskey, ns); err != nil {
		t.Fatal(err)
	}
	if current.ResourceVersion != rv || ns.ResourceVersion != nsrv {
		t.Fatal("repeated reconciliation wrote resources")
	}
	// CEL validation prevents moving the namespace and leaking the old one.
	current.Spec.Namespace = "preview-other"
	if err := c.Update(ctx, current); !apierrors.IsInvalid(err) {
		t.Fatalf("expected immutable namespace validation, got %v", err)
	}
	invalid := &platformv1alpha1.PreviewEnvironment{ObjectMeta: metav1.ObjectMeta{Name: "invalid", Namespace: control.Name}, Spec: platformv1alpha1.PreviewEnvironmentSpec{Namespace: "kube-system"}}
	if err := c.Create(ctx, invalid); !apierrors.IsInvalid(err) {
		t.Fatalf("expected reserved prefix validation, got %v", err)
	}
	// envtest has no namespace controller. Remove the built-in namespace finalizer
	// explicitly to simulate completed deletion; the watch must recreate it.
	if err := c.Delete(ctx, ns); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, nskey, ns); err != nil {
		t.Fatal(err)
	}
	ns.Spec.Finalizers = nil
	if err := c.SubResource("finalize").Update(ctx, ns); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool {
		fresh := &corev1.Namespace{}
		return c.Get(ctx, nskey, fresh) == nil && fresh.UID != originalUID && fresh.DeletionTimestamp.IsZero()
	})
	if err := c.Get(ctx, key, current); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, current); err != nil {
		t.Fatal(err)
	}
	// Deletion changes metadata, not generation. The manager must reconcile it.
	eventually(t, func() bool {
		fresh := &corev1.Namespace{}
		return c.Get(ctx, nskey, fresh) == nil && !fresh.DeletionTimestamp.IsZero()
	})
	if err := c.Get(ctx, key, current); err != nil {
		t.Fatal("preview removed before namespace cleanup", err)
	}
	if !slices.Contains(current.Finalizers, cleanupFinalizer) || current.Status.Phase != "Deleting" {
		t.Fatal("cleanup responsibility/status missing")
	}
	// Restart while cleanup is pending. Initial informer events must resume it.
	stopManager()
	startTestManager(t, cfg, scheme)
	if err := c.Get(ctx, nskey, ns); err != nil {
		t.Fatal(err)
	}
	ns.Spec.Finalizers = nil
	if err := c.SubResource("finalize").Update(ctx, ns); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return apierrors.IsNotFound(c.Get(ctx, key, &platformv1alpha1.PreviewEnvironment{})) })
	if err := c.Get(ctx, nskey, &corev1.Namespace{}); !apierrors.IsNotFound(err) {
		t.Fatalf("namespace not cleaned up: %v", err)
	}
}

func eventually(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("condition did not become true before timeout")
}

func startTestManager(t *testing.T, cfg *rest.Config, scheme *runtime.Scheme) func() {
	t.Helper()
	// Test managers restart sequentially in one process. controller-runtime's
	// name registry survives Stop, unlike a real process restart.
	skipNameValidation := true
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{Controller: config.Controller{SkipNameValidation: &skipNameValidation}, Scheme: scheme, Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0"})
	if err != nil {
		t.Fatal(err)
	}
	if err := (&PreviewEnvironmentReconciler{Client: mgr.GetClient(), Scheme: scheme}).SetupWithManager(mgr); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mgr.Start(ctx) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Error(err)
				}
			case <-time.After(10 * time.Second):
				t.Error("manager did not stop")
			}
		})
	}
	t.Cleanup(stop)
	if !mgr.GetCache().WaitForCacheSync(ctx) {
		t.Fatal("cache did not sync")
	}
	return stop
}
