package controller

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	platformv1alpha1 "edlab.dev/preview-environment-controller/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	applicationIndex = "preview.applicationName"
	argoFinalizer    = "resources-finalizer.argocd.argoproj.io"
)

// ApplicationObject constructs an Argo CD Application without importing its server.
// Use the upstream CRD rather than importing Argo CD's server implementation.
// The v3.3.0 CRD fixture validates these unstructured fields in envtest.
func ApplicationObject() *unstructured.Unstructured {
	app := &unstructured.Unstructured{}
	app.SetGroupVersionKind(schema.GroupVersionKind{Group: "argoproj.io", Version: "v1alpha1", Kind: "Application"})
	return app
}

func applicationName(p *platformv1alpha1.PreviewEnvironment) string {
	return "preview-" + string(p.UID)
}

func applicationIndexValues(obj client.Object) []string {
	p := obj.(*platformv1alpha1.PreviewEnvironment)
	if p.Spec.Repository == "" {
		return nil
	}
	return []string{applicationName(p)}
}

func (r *PreviewEnvironmentReconciler) applicationSpec(p *platformv1alpha1.PreviewEnvironment, namespace string) map[string]any {
	path := p.Spec.Path
	if path == "" {
		path = "."
	}
	return map[string]any{
		"project":     r.ArgoProject,
		"source":      map[string]any{"repoURL": p.Spec.Repository, "targetRevision": p.Spec.Revision, "path": path},
		"destination": map[string]any{"server": "https://kubernetes.default.svc", "namespace": namespace},
		"syncPolicy": map[string]any{
			"automated":   map[string]any{"prune": true, "selfHeal": true, "allowEmpty": false},
			"syncOptions": []any{"FailOnSharedResource=true"},
		},
	}
}

func applicationAssociated(app *unstructured.Unstructured, p *platformv1alpha1.PreviewEnvironment) bool {
	return app.GetLabels()[ownerUIDLabel] == string(p.UID) &&
		app.GetAnnotations()[ownerNameAnnotation] == p.Name &&
		app.GetAnnotations()[ownerNamespaceAnnotation] == p.Namespace && len(app.GetOwnerReferences()) == 0
}

func (r *PreviewEnvironmentReconciler) reconcileApplication(ctx context.Context, p *platformv1alpha1.PreviewEnvironment, namespace string) (result ctrl.Result, stepErr error) {
	ctx, finish := r.step(ctx, "application")
	defer func() { finish(stepErr) }()
	if p.Spec.Revision == "" || r.ArgoNamespace == "" || r.ArgoProject == "" || r.ArgoProject == "default" {
		return ctrl.Result{}, r.setStatus(ctx, p, namespace, metav1.ConditionFalse, "ApplicationConfigurationInvalid", "A revision and enabled Argo CD integration with a restricted AppProject are required")
	}
	app := ApplicationObject()
	err := r.Get(ctx, types.NamespacedName{Namespace: r.ArgoNamespace, Name: applicationName(p)}, app)
	missing := apierrors.IsNotFound(err)
	if err != nil && !missing {
		return ctrl.Result{}, r.applicationError(ctx, p, namespace, err)
	}
	if !missing && !applicationAssociated(app, p) {
		return ctrl.Result{}, r.setStatus(ctx, p, namespace, metav1.ConditionFalse, "ApplicationConflict", "Application association differs or it has an owner; refusing adoption")
	}
	if !missing && !app.GetDeletionTimestamp().IsZero() {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, r.setStatus(ctx, p, namespace, metav1.ConditionFalse, "ApplicationTerminating", "Waiting for Application deletion before recreating it")
	}
	before := app.DeepCopy()
	if missing {
		app.SetName(applicationName(p))
		app.SetNamespace(r.ArgoNamespace)
		app.SetLabels(map[string]string{ownerUIDLabel: string(p.UID)})
		app.SetAnnotations(map[string]string{ownerNameAnnotation: p.Name, ownerNamespaceAnnotation: p.Namespace})
	}
	// This controller owns the entire Application spec. Metadata and status owned
	// by other actors are preserved, as are unrelated finalizers.
	app.Object["spec"] = r.applicationSpec(p, namespace)
	if !slices.Contains(app.GetFinalizers(), argoFinalizer) {
		app.SetFinalizers(append(app.GetFinalizers(), argoFinalizer))
	}
	changed := !equality.Semantic.DeepEqual(before.Object, app.Object)
	if missing {
		err = r.Create(ctx, app)
	} else if changed {
		err = r.Patch(ctx, app, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
	}
	if err != nil {
		return ctrl.Result{}, r.applicationError(ctx, p, namespace, err)
	}
	if changed {
		// A just-created/updated spec does not prove that Argo has deployed it.
		return ctrl.Result{}, r.setStatus(ctx, p, namespace, metav1.ConditionFalse, "ApplicationPending", "Waiting for Argo CD to reconcile the desired Application", app)
	}
	ready, reason, message := applicationReadiness(app)
	return ctrl.Result{}, r.setStatus(ctx, p, namespace, ready, reason, message, app)
}

func (r *PreviewEnvironmentReconciler) applicationError(ctx context.Context, p *platformv1alpha1.PreviewEnvironment, namespace string, err error) error {
	return errors.Join(err, r.setStatus(ctx, p, namespace, metav1.ConditionFalse, "ApplicationProvisioningFailed", err.Error()))
}

func appString(app *unstructured.Unstructured, fields ...string) string {
	s, _, _ := unstructured.NestedString(app.Object, fields...)
	return s
}

func applicationReadiness(app *unstructured.Unstructured) (metav1.ConditionStatus, string, string) {
	conditions, _, _ := unstructured.NestedSlice(app.Object, "status", "conditions")
	for _, item := range conditions {
		condition, ok := item.(map[string]any)
		if !ok {
			continue
		}
		typ, _ := condition["type"].(string)
		if strings.HasSuffix(typ, "Error") {
			message, _ := condition["message"].(string)
			return metav1.ConditionFalse, "ApplicationError", typ + ": " + message
		}
	}
	// Argo's Application has no status.observedGeneration. Check comparedTo to
	// avoid accepting old Healthy/Synced status after a Git source change.
	for _, field := range []string{"source", "destination"} {
		desired, _, _ := unstructured.NestedMap(app.Object, "spec", field)
		compared, _, _ := unstructured.NestedMap(app.Object, "status", "sync", "comparedTo", field)
		for key, value := range desired {
			if !equality.Semantic.DeepEqual(value, compared[key]) {
				return metav1.ConditionFalse, "ApplicationPending", "Waiting for Argo CD to compare the desired source and destination"
			}
		}
	}
	if appString(app, "status", "operationState", "phase") == "Running" || app.Object["operation"] != nil {
		return metav1.ConditionFalse, "ApplicationSyncing", "Argo CD sync is in progress"
	}
	if appString(app, "status", "sync", "status") != "Synced" {
		return metav1.ConditionFalse, "ApplicationOutOfSync", "Waiting for Application sync"
	}
	if appString(app, "status", "health", "status") != "Healthy" {
		return metav1.ConditionFalse, "ApplicationUnhealthy", "Waiting for Application health"
	}
	return metav1.ConditionTrue, "ApplicationHealthy", "Baseline is provisioned and Argo CD reports the desired Application Synced and Healthy"
}

func observeApplication(p *platformv1alpha1.PreviewEnvironment, app *unstructured.Unstructured) {
	p.Status.Application = app.GetName()
	p.Status.SyncStatus = appString(app, "status", "sync", "status")
	p.Status.ApplicationHealth = appString(app, "status", "health", "status")
	meta.SetStatusCondition(&p.Status.Conditions, metav1.Condition{Type: "ApplicationCreated", Status: metav1.ConditionTrue, Reason: "ApplicationProvisioned", Message: "Application exists in the configured Argo CD namespace", ObservedGeneration: p.Generation})
	healthy, reason, message := applicationReadiness(app)
	meta.SetStatusCondition(&p.Status.Conditions, metav1.Condition{Type: "ApplicationHealthy", Status: healthy, Reason: reason, Message: message, ObservedGeneration: p.Generation})
}

func (r *PreviewEnvironmentReconciler) cleanupApplication(ctx context.Context, p *platformv1alpha1.PreviewEnvironment, namespace string) (bool, error) {
	if r.ArgoNamespace == "" {
		return false, r.setStatus(ctx, p, namespace, metav1.ConditionFalse, "CleanupBlocked", "Enable Argo CD integration to confirm Application cleanup before deleting the namespace")
	}
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	app := ApplicationObject()
	err := reader.Get(ctx, types.NamespacedName{Namespace: r.ArgoNamespace, Name: applicationName(p)}, app)
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, r.cleanupError(ctx, p, namespace, err)
	}
	if !applicationAssociated(app, p) {
		return false, r.setStatus(ctx, p, namespace, metav1.ConditionFalse, "CleanupBlocked", "Application association differs or it has an owner; refusing deletion")
	}
	if err := r.setStatus(ctx, p, namespace, metav1.ConditionFalse, "ApplicationCleanupInProgress", "Waiting for Argo CD cascading Application deletion", app); err != nil {
		return false, err
	}
	if app.GetDeletionTimestamp().IsZero() {
		uid, rv := app.GetUID(), app.GetResourceVersion()
		err = r.Delete(ctx, app, client.Preconditions{UID: &uid, ResourceVersion: &rv})
		if err != nil && !apierrors.IsNotFound(err) {
			return false, r.cleanupError(ctx, p, namespace, err)
		}
	}
	// Argo CD removes its finalizer after deleting workloads. Never bypass it.
	return false, nil
}

func (r *PreviewEnvironmentReconciler) applicationRequests(ctx context.Context, obj client.Object) []ctrl.Request {
	if obj.GetNamespace() != r.ArgoNamespace {
		return nil
	}
	previews := &platformv1alpha1.PreviewEnvironmentList{}
	if err := r.List(ctx, previews, client.MatchingFields{applicationIndex: obj.GetName()}); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "Unable to map Application event")
		return nil
	}
	requests := make([]ctrl.Request, 0, len(previews.Items))
	for _, p := range previews.Items {
		requests = append(requests, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(&p)})
	}
	return requests
}
