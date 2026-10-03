package controller

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	platformv1alpha1 "edlab.dev/preview-environment-controller/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var ttlPattern = regexp.MustCompile(`^([0-9]+h)?([0-9]+m)?([0-9]+s)?$`)

func previewExpiry(preview *platformv1alpha1.PreviewEnvironment) (*metav1.Time, error) {
	if preview.Spec.TTL == "" {
		return nil, nil
	}
	duration, err := time.ParseDuration(preview.Spec.TTL)
	if len(preview.Spec.TTL) > 32 || !ttlPattern.MatchString(preview.Spec.TTL) || err != nil || duration <= 0 {
		return nil, fmt.Errorf("ttl must be a positive whole hours/minutes/seconds duration within Go's duration range")
	}
	if preview.CreationTimestamp.IsZero() {
		return nil, fmt.Errorf("ttl requires metadata.creationTimestamp")
	}
	expiresAt := metav1.NewTime(preview.CreationTimestamp.Time.Truncate(time.Second).Add(duration))
	return &expiresAt, nil
}

func (r *PreviewEnvironmentReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *PreviewEnvironmentReconciler) expirePreview(ctx context.Context, preview *platformv1alpha1.PreviewEnvironment) error {
	if err := r.setStatus(ctx, preview, preview.Status.Namespace, metav1.ConditionFalse, "TTLExpired", "Preview lifetime elapsed; requesting deletion through normal finalizer cleanup"); err != nil {
		return err
	}
	// Optimistic preconditions prevent an expiry decision from deleting a preview
	// whose TTL was extended or removed after our read, or a replacement object.
	err := r.Delete(ctx, preview, client.Preconditions{UID: &preview.UID, ResourceVersion: &preview.ResourceVersion})
	if err = client.IgnoreNotFound(err); err != nil {
		return errors.Join(err, r.setStatus(ctx, preview, preview.Status.Namespace, metav1.ConditionFalse, "TTLDeletionFailed", err.Error()))
	}
	return nil
}
