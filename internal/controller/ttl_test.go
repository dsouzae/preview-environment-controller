package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	platformv1alpha1 "edlab.dev/preview-environment-controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestPreviewExpiry(t *testing.T) {
	for _, tt := range []struct {
		ttl      string
		valid    bool
		duration time.Duration
	}{
		{"", true, 0}, {"24h", true, 24 * time.Hour}, {"1h30m5s", true, time.Hour + 30*time.Minute + 5*time.Second},
		{"0s", false, 0}, {"-1s", false, 0}, {"1.5s", false, 0}, {"1ms", false, 0}, {"1d", false, 0}, {"1m1h", false, 0}, {"999999999999999999h", false, 0},
	} {
		t.Run(tt.ttl, func(t *testing.T) {
			p := testPreview()
			p.Spec.TTL = tt.ttl
			p.CreationTimestamp = metav1.NewTime(time.Unix(1000, 123456789))
			expiry, err := previewExpiry(p)
			if (err == nil) != tt.valid {
				t.Fatalf("expiry=%v err=%v", expiry, err)
			}
			if tt.valid && tt.ttl != "" && !expiry.Equal(&metav1.Time{Time: time.Unix(1000, 0).Add(tt.duration)}) {
				t.Fatal(expiry)
			}
			if tt.ttl == "" && expiry != nil {
				t.Fatal(expiry)
			}
		})
	}
	p := testPreview()
	p.Spec.TTL = "1h"
	if _, err := previewExpiry(p); err == nil {
		t.Fatal("missing timestamp accepted")
	}
}

func TestTTLSchedulingAndEdits(t *testing.T) {
	p := testPreview()
	p.Spec.TTL = "1h"
	p.CreationTimestamp = metav1.NewTime(time.Unix(1000, 0))
	c := testClient(t, p)
	r := &PreviewEnvironmentReconciler{Client: c, Now: func() time.Time { return time.Unix(1000, 0).Add(10 * time.Minute) }}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(p)}
	result, err := r.Reconcile(context.Background(), request)
	if err != nil || result.RequeueAfter != 50*time.Minute {
		t.Fatalf("%v %v", result, err)
	}
	r.Client = &rejectWrites{Client: c}
	result, err = r.Reconcile(context.Background(), request)
	if err != nil || result.RequeueAfter != 50*time.Minute {
		t.Fatalf("idempotency %v %v", result, err)
	}
	r.Client = c
	for _, ttl := range []string{"2h", ""} {
		current := &platformv1alpha1.PreviewEnvironment{}
		if err := c.Get(context.Background(), request.NamespacedName, current); err != nil {
			t.Fatal(err)
		}
		current.Spec.TTL = ttl
		if err := c.Update(context.Background(), current); err != nil {
			t.Fatal(err)
		}
		result, err = r.Reconcile(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		if ttl == "2h" && result.RequeueAfter != 110*time.Minute {
			t.Fatal(result)
		}
		if ttl == "" && result.RequeueAfter != 0 {
			t.Fatal(result)
		}
		if err := c.Get(context.Background(), request.NamespacedName, current); err != nil {
			t.Fatal(err)
		}
		if ttl == "" && current.Status.ExpiresAt != nil {
			t.Fatal("expiry retained")
		}
	}
}

func TestTTLConflictStillSchedulesExpiry(t *testing.T) {
	p := testPreview()
	p.Spec.TTL = "1h"
	p.CreationTimestamp = metav1.NewTime(time.Unix(1000, 0))
	c := testClient(t, p, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: p.Spec.Namespace}})
	r := &PreviewEnvironmentReconciler{Client: c, Now: func() time.Time { return time.Unix(1000, 0) }}
	result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(p)})
	if err != nil || result.RequeueAfter != time.Hour || readyReason(t, c, p) != "NamespaceConflict" {
		t.Fatalf("%v %v", result, err)
	}
}

func TestTTLExpirationUsesCleanupAndRetriesDelete(t *testing.T) {
	p := testPreview()
	p.Spec.TTL = "1h"
	p.CreationTimestamp = metav1.NewTime(time.Unix(1000, 0))
	p.Finalizers = []string{cleanupFinalizer, "example.test/preserve"}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: p.Spec.Namespace, Labels: map[string]string{ownerUIDLabel: string(p.UID)}}}
	c := testClient(t, p, ns)
	failure := errors.New("temporary delete failure")
	r := &PreviewEnvironmentReconciler{Client: &ttlFailDelete{Client: c, err: failure}, Now: func() time.Time { return time.Unix(1000, 0).Add(time.Hour) }}
	if err := reconcile(t, r, p); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if reason := readyReason(t, c, p); reason != "TTLDeletionFailed" {
		t.Fatal(reason)
	}
	r.Client = c
	if err := reconcile(t, r, p); err != nil {
		t.Fatal(err)
	}
	current := &platformv1alpha1.PreviewEnvironment{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(p), current); err != nil {
		t.Fatal(err)
	}
	if current.DeletionTimestamp.IsZero() || len(current.Finalizers) != 2 {
		t.Fatal("cleanup bypassed")
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(ns), ns); err != nil {
		t.Fatal("namespace deleted before cleanup", err)
	}
	if err := reconcile(t, r, p); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(ns), ns); !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	if err := reconcile(t, r, p); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(p), current); err != nil {
		t.Fatal(err)
	}
	if len(current.Finalizers) != 1 || current.Finalizers[0] != "example.test/preserve" {
		t.Fatal(current.Finalizers)
	}
}

type ttlFailDelete struct {
	client.Client
	err error
}

func (c *ttlFailDelete) Delete(context.Context, client.Object, ...client.DeleteOption) error {
	return c.err
}

func TestInvalidTTLDoesNotProvision(t *testing.T) {
	p := testPreview()
	p.Spec.TTL = "0s"
	c := testClient(t, p)
	r := &PreviewEnvironmentReconciler{Client: c}
	if err := reconcile(t, r, p); err != nil {
		t.Fatal(err)
	}
	if readyReason(t, c, p) != "InvalidTTL" {
		t.Fatal("invalid TTL hidden")
	}
	if err := c.Get(context.Background(), client.ObjectKey{Name: p.Spec.Namespace}, &corev1.Namespace{}); !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
}

func TestTTLShortenedWhileConflicting(t *testing.T) {
	p := testPreview()
	p.Spec.TTL = "1h"
	p.CreationTimestamp = metav1.NewTime(time.Unix(1000, 0))
	c := testClient(t, p, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: p.Spec.Namespace}})
	r := &PreviewEnvironmentReconciler{Client: c, Now: func() time.Time { return time.Unix(1000, 0).Add(10 * time.Minute) }}
	if err := reconcile(t, r, p); err != nil {
		t.Fatal(err)
	}
	current := &platformv1alpha1.PreviewEnvironment{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(p), current); err != nil {
		t.Fatal(err)
	}
	current.Spec.TTL = "1m"
	if err := c.Update(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	if err := reconcile(t, r, p); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(p), current); err != nil {
		t.Fatal(err)
	}
	if current.DeletionTimestamp.IsZero() {
		t.Fatal("shortened TTL did not expire")
	}
	if err := reconcile(t, r, p); err != nil {
		t.Fatal(err)
	}
	if readyReason(t, c, p) != "CleanupBlocked" {
		t.Fatal("foreign namespace cleanup should remain blocked")
	}
}

func TestTTLPreservesEarlierRetry(t *testing.T) {
	p := testPreview()
	p.Spec.TTL = "1h"
	p.CreationTimestamp = metav1.NewTime(time.Unix(1000, 0))
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: p.Spec.Namespace, Labels: map[string]string{ownerUIDLabel: string(p.UID)}, Finalizers: []string{"example.test/hold"}, DeletionTimestamp: &metav1.Time{Time: time.Unix(1000, 0)}}}
	c := testClient(t, p, ns)
	r := &PreviewEnvironmentReconciler{Client: c, Now: func() time.Time { return time.Unix(1000, 0) }}
	result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(p)})
	if err != nil || result.RequeueAfter != 5*time.Second {
		t.Fatalf("%v %v", result, err)
	}
}

type extendingTTLClient struct {
	client.Client
	t *testing.T
}

func (c extendingTTLClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	options := &client.DeleteOptions{}
	options.ApplyOptions(opts)
	if options.Preconditions == nil || options.Preconditions.UID == nil || options.Preconditions.ResourceVersion == nil {
		c.t.Fatal("missing expiry delete preconditions")
	}
	current := &platformv1alpha1.PreviewEnvironment{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(obj), current); err != nil {
		return err
	}
	current.Spec.TTL = "2h"
	if err := c.Update(ctx, current); err != nil {
		return err
	}
	return c.Client.Delete(ctx, obj, opts...)
}
func TestTTLConcurrentExtensionPreventsDeletion(t *testing.T) {
	p := testPreview()
	p.Spec.TTL = "1h"
	p.CreationTimestamp = metav1.NewTime(time.Unix(1000, 0))
	p.Finalizers = []string{cleanupFinalizer}
	c := testClient(t, p)
	r := &PreviewEnvironmentReconciler{Client: extendingTTLClient{c, t}, Now: func() time.Time { return time.Unix(1000, 0).Add(time.Hour) }}
	if err := reconcile(t, r, p); !apierrors.IsConflict(err) {
		t.Fatalf("expected expiry conflict, got %v", err)
	}
	current := &platformv1alpha1.PreviewEnvironment{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(p), current); err != nil {
		t.Fatal(err)
	}
	if !current.DeletionTimestamp.IsZero() {
		t.Fatal("extended preview deleted")
	}
	r.Client = c
	result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(p)})
	if err != nil || result.RequeueAfter != time.Hour {
		t.Fatalf("%v %v", result, err)
	}
}
