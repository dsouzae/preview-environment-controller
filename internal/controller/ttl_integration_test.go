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
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func TestEnvtestTTL(t *testing.T) {
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
	for _, register := range []func(*runtime.Scheme) error{corev1.AddToScheme, networkingv1.AddToScheme, platformv1alpha1.AddToScheme} {
		if err := register(scheme); err != nil {
			t.Fatal(err)
		}
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	startTestManager(t, cfg, scheme)
	ctx := context.Background()
	control := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "ttl-control"}}
	if err := c.Create(ctx, control); err != nil {
		t.Fatal(err)
	}
	for _, ttl := range []string{"0s", "1.5s", "1d", "999999999999999999h"} {
		invalid := &platformv1alpha1.PreviewEnvironment{ObjectMeta: metav1.ObjectMeta{Name: "invalid", Namespace: control.Name}, Spec: platformv1alpha1.PreviewEnvironmentSpec{TTL: ttl}}
		if err := c.Create(ctx, invalid); !apierrors.IsInvalid(err) {
			t.Fatalf("TTL %q accepted: %v", ttl, err)
		}
	}
	p := &platformv1alpha1.PreviewEnvironment{ObjectMeta: metav1.ObjectMeta{Name: "shortened", Namespace: control.Name}, Spec: platformv1alpha1.PreviewEnvironmentSpec{Namespace: "preview-ttl", TTL: "30s"}}
	if err := c.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	key := client.ObjectKeyFromObject(p)
	eventually(t, func() bool {
		current := &platformv1alpha1.PreviewEnvironment{}
		if c.Get(ctx, key, current) != nil {
			return false
		}
		ready := meta.FindStatusCondition(current.Status.Conditions, "Ready")
		return ready != nil && ready.Status == metav1.ConditionTrue && current.Status.ExpiresAt != nil
	})
	current := &platformv1alpha1.PreviewEnvironment{}
	if err := c.Get(ctx, key, current); err != nil {
		t.Fatal(err)
	}
	version := current.ResourceVersion
	r := &PreviewEnvironmentReconciler{Client: c}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, key, current); err != nil {
		t.Fatal(err)
	}
	if current.ResourceVersion != version {
		t.Fatal("TTL status changed during an already-correct real-API reconciliation")
	}
	current.Spec.TTL = "1s"
	if err := c.Update(ctx, current); err != nil {
		t.Fatal(err)
	}
	// No manually-triggered reconcile: generation events reschedule the deadline,
	// and the queue wakes despite status-only updates being filtered.
	eventually(t, func() bool {
		current := &platformv1alpha1.PreviewEnvironment{}
		if c.Get(ctx, key, current) != nil {
			return false
		}
		ns := &corev1.Namespace{}
		if c.Get(ctx, client.ObjectKey{Name: p.Spec.Namespace}, ns) != nil {
			return false
		}
		return !current.DeletionTimestamp.IsZero() && !ns.DeletionTimestamp.IsZero() && len(current.Finalizers) > 0
	})
	// Envtest has no namespace controller; the finalizer deliberately stays held.
}
