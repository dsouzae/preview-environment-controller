/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	platformv1alpha1 "edlab.dev/preview-environment-controller/api/v1alpha1"
)

const (
	cleanupFinalizer         = "platform.ellery.dev/namespace-cleanup"
	namespaceIndex           = "preview.targetNamespace"
	ownerUIDLabel            = "platform.ellery.dev/preview-uid"
	ownerNameAnnotation      = "platform.ellery.dev/preview-name"
	ownerNamespaceAnnotation = "platform.ellery.dev/preview-namespace"
)

// PreviewEnvironmentReconciler computes desired state on every event.
type PreviewEnvironmentReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// APIReader bypasses the cache for cleanup decisions. Set by SetupWithManager.
	APIReader client.Reader
}

// +kubebuilder:rbac:groups=platform.ellery.dev,resources=previewenvironments,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=platform.ellery.dev,resources=previewenvironments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=platform.ellery.dev,resources=previewenvironments/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;create;delete

func (r *PreviewEnvironmentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	preview := &platformv1alpha1.PreviewEnvironment{}
	if err := r.Get(ctx, req.NamespacedName, preview); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !preview.DeletionTimestamp.IsZero() {
		return r.reconcileDeletion(ctx, preview)
	}
	name, err := desiredNamespace(preview)
	if err != nil {
		return ctrl.Result{}, r.setStatus(ctx, preview, "", metav1.ConditionFalse, "InvalidConfiguration", err.Error())
	}
	// Persist cleanup responsibility before creating any external resource. If the
	// process stops after this patch, a subsequent reconcile resumes provisioning.
	if !controllerutil.ContainsFinalizer(preview, cleanupFinalizer) {
		before := preview.DeepCopy()
		controllerutil.AddFinalizer(preview, cleanupFinalizer)
		if err := r.Patch(ctx, preview, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return ctrl.Result{}, err
		}
	}

	namespace := &corev1.Namespace{}
	err = r.Get(ctx, types.NamespacedName{Name: name}, namespace)
	if apierrors.IsNotFound(err) {
		namespace = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Labels:      map[string]string{ownerUIDLabel: string(preview.UID)},
			Annotations: map[string]string{ownerNameAnnotation: preview.Name, ownerNamespaceAnnotation: preview.Namespace},
		}}
		err = r.Create(ctx, namespace)
	}
	if err != nil {
		statusErr := r.setStatus(ctx, preview, name, metav1.ConditionFalse, "NamespaceProvisioningFailed", err.Error())
		if statusErr != nil {
			return ctrl.Result{}, fmt.Errorf("namespace operation: %w; status update: %v", err, statusErr)
		}
		// Includes AlreadyExists from a stale cached NotFound; retry with fresh state.
		return ctrl.Result{}, err
	}
	if namespace.Labels[ownerUIDLabel] != string(preview.UID) {
		return ctrl.Result{}, r.setStatus(ctx, preview, name, metav1.ConditionFalse, "NamespaceConflict", "Target namespace is not associated with this PreviewEnvironment; refusing adoption")
	}
	if !namespace.DeletionTimestamp.IsZero() {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, r.setStatus(ctx, preview, name, metav1.ConditionFalse, "NamespaceTerminating", "Waiting for namespace deletion before recreating it")
	}
	return ctrl.Result{}, r.setStatus(ctx, preview, name, metav1.ConditionTrue, "NamespaceProvisioned", "Preview namespace exists; application reconciliation is not implemented yet")
}

func (r *PreviewEnvironmentReconciler) reconcileDeletion(ctx context.Context, preview *platformv1alpha1.PreviewEnvironment) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(preview, cleanupFinalizer) {
		return ctrl.Result{}, nil
	}
	name, err := desiredNamespace(preview)
	if err != nil {
		return ctrl.Result{}, r.setStatus(ctx, preview, "", metav1.ConditionFalse, "CleanupBlocked", err.Error())
	}
	// A stale cached NotFound must never release cleanup responsibility while the
	// namespace still exists. Production uses the manager's uncached APIReader.
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	} // Direct clients in unit tests.
	namespace := &corev1.Namespace{}
	err = reader.Get(ctx, types.NamespacedName{Name: name}, namespace)
	if apierrors.IsNotFound(err) {
		before := preview.DeepCopy()
		controllerutil.RemoveFinalizer(preview, cleanupFinalizer)
		return ctrl.Result{}, r.Patch(ctx, preview, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
	}
	if err != nil {
		return ctrl.Result{}, r.cleanupError(ctx, preview, name, err)
	}
	if namespace.Labels[ownerUIDLabel] != string(preview.UID) {
		return ctrl.Result{}, r.setStatus(ctx, preview, name, metav1.ConditionFalse, "CleanupBlocked", "Namespace association does not match this preview; cleanup is blocked until ownership is resolved")
	}
	if err := r.setStatus(ctx, preview, name, metav1.ConditionFalse, "CleanupInProgress", "Waiting for preview namespace deletion to complete"); err != nil {
		return ctrl.Result{}, err
	}
	if namespace.DeletionTimestamp.IsZero() {
		// Protect against both replacement and metadata changes after the ownership
		// check. A conflict returns through normal controller-runtime backoff.
		err := r.Delete(ctx, namespace, client.Preconditions{UID: &namespace.UID, ResourceVersion: &namespace.ResourceVersion})
		if err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, r.cleanupError(ctx, preview, name, err)
		}
	}
	// Namespace deletion is asynchronous; never clear its own finalizers. Watches
	// drive progress, with a bounded retry if an event or mapping read is missed.
	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

func (r *PreviewEnvironmentReconciler) cleanupError(ctx context.Context, preview *platformv1alpha1.PreviewEnvironment, namespace string, err error) error {
	return errors.Join(err, r.setStatus(ctx, preview, namespace, metav1.ConditionFalse, "CleanupFailed", err.Error()))
}

func previewPredicate() predicate.Predicate {
	return predicate.Funcs{UpdateFunc: func(e event.UpdateEvent) bool {
		return e.ObjectOld.GetGeneration() != e.ObjectNew.GetGeneration() ||
			!e.ObjectOld.GetDeletionTimestamp().Equal(e.ObjectNew.GetDeletionTimestamp()) ||
			!slices.Equal(e.ObjectOld.GetFinalizers(), e.ObjectNew.GetFinalizers())
	}}
}

func desiredNamespace(preview *platformv1alpha1.PreviewEnvironment) (string, error) {
	name := preview.Spec.Namespace
	if name == "" {
		name = "preview-" + string(preview.UID)
	}
	if preview.UID == "" || !strings.HasPrefix(name, "preview-") || len(validation.IsDNS1123Label(name)) != 0 {
		return "", fmt.Errorf("namespace must be a DNS label with the preview- prefix and the preview must have a UID")
	}
	if name == preview.Namespace {
		return "", fmt.Errorf("PreviewEnvironment must live outside its target namespace to avoid blocking namespace cleanup")
	}
	return name, nil
}

func (r *PreviewEnvironmentReconciler) setStatus(ctx context.Context, preview *platformv1alpha1.PreviewEnvironment, namespace string, ready metav1.ConditionStatus, reason, message string) error {
	before := preview.DeepCopy()
	preview.Status.ObservedGeneration = preview.Generation
	preview.Status.Namespace = namespace
	preview.Status.Phase = "Pending"
	if !preview.DeletionTimestamp.IsZero() {
		preview.Status.Phase = "Deleting"
	}
	if ready == metav1.ConditionTrue {
		preview.Status.Phase = "Ready"
	}
	meta.SetStatusCondition(&preview.Status.Conditions, metav1.Condition{
		Type: "Ready", Status: ready, Reason: reason, Message: message,
		ObservedGeneration: preview.Generation,
	})
	// Preserve transition times and avoid writes caused only by our own status.
	if equality.Semantic.DeepEqual(before.Status, preview.Status) {
		return nil
	}
	return r.Status().Patch(ctx, preview, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
}

func (r *PreviewEnvironmentReconciler) namespaceRequests(ctx context.Context, obj client.Object) []ctrl.Request {
	// Indexing by desired name also finds conflicting namespaces with no association
	// annotations, so removing a conflict triggers recovery without polling.
	previews := &platformv1alpha1.PreviewEnvironmentList{}
	if err := r.List(ctx, previews, client.MatchingFields{namespaceIndex: obj.GetName()}); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "Unable to map namespace event")
		return nil
	}
	requests := make([]ctrl.Request, 0, len(previews.Items))
	for _, preview := range previews.Items {
		requests = append(requests, ctrl.Request{NamespacedName: types.NamespacedName{Name: preview.Name, Namespace: preview.Namespace}})
	}
	return requests
}

// SetupWithManager uses a mapped watch because a namespaced CR cannot own a
// cluster-scoped Namespace. The primary predicate includes deletion transitions.
func (r *PreviewEnvironmentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.APIReader == nil {
		r.APIReader = mgr.GetAPIReader()
	}
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &platformv1alpha1.PreviewEnvironment{}, namespaceIndex, func(obj client.Object) []string {
		name, err := desiredNamespace(obj.(*platformv1alpha1.PreviewEnvironment))
		if err != nil {
			return nil
		}
		return []string{name}
	}); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&platformv1alpha1.PreviewEnvironment{}, builder.WithPredicates(previewPredicate())).
		Watches(&corev1.Namespace{}, handler.EnqueueRequestsFromMapFunc(r.namespaceRequests)).
		Named("previewenvironment").Complete(r)
}
