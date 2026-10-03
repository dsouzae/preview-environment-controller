# Argo CD Application integration

This milestone manages workload Applications. Deploying the controller itself through Argo CD and automating image-tag updates remain the GitOps delivery milestone.

## Contract

Baseline-only previews continue to use `BaselineProvisioned` readiness. Adding `spec.repository` requires `spec.revision` and enables an Application-backed preview. `spec.path` defaults to `.`. Repository/revision/path can change; the repository field cannot subsequently be removed, because doing so would abandon an Application. Namespace remains immutable. TTL and remote-cluster placement are not implemented.

The controller finishes the namespace baseline before creating `argocd/preview-<CR UID>`. The Application uses a fixed operator-configured project, one Git source, the local cluster server and the computed preview namespace. Automated sync prunes and self-heals; empty-source pruning is disabled and `FailOnSharedResource=true` guards resources tracked by another Application. Namespace auto-creation is deliberately not enabled. Application manifests must omit explicit namespaces and must not declare the controller-owned baseline resources.

The entire Application spec is reserved for this controller. Changes to project, source, destination or sync policy are repaired. Unrelated metadata, status and finalizers are preserved. The association requires the preview UID label plus its name/namespace annotations; existing foreign objects or objects with owner references cause `ApplicationConflict`. Applications in the Argo CD namespace cannot have an owner reference to a preview in another namespace. Their association and cleanup therefore use mapped watches and the existing preview finalizer.

Status includes Application name, sync status, application health, `ApplicationCreated`, `ApplicationHealthy` and `Ready` Conditions. Ready requires the baseline plus Argo reporting Synced and Healthy for the desired source and destination, without error Conditions or an active sync. Applications have no `status.observedGeneration`; the controller compares `status.sync.comparedTo` against the desired source/destination to reject old observations after a revision change. This is Argo's last reported assessment, not an independent workload probe or proof of freshness during an Argo outage. A pinned Git commit is reproducible; a branch can move before Argo refreshes its status.

In controller-runtime 0.25.0, unstructured client reads bypass the cache by default. The manager explicitly enables cached unstructured reads for normal reconciliation; finalizer decisions still use the uncached APIReader. Stale NotFound/AlreadyExists and optimistic-lock conflicts return through normal backoff. The mapped Application watch includes health/sync status changes, creation, deletion, spec and metadata changes. A name index maps even unlabelled conflicting Applications to their preview. Equal status is not patched; steady state has no polling. Temporary Kubernetes API errors set `ApplicationProvisioningFailed` and return errors for controller-runtime backoff. Argo's `*Error` Conditions become `ApplicationError` with its message. OutOfSync, unhealthy and pending states wait for Argo events; terminating Applications get a five-second retry. An enabled integration requires installed Argo CRDs and a valid restricted project. Missing CRDs or watch RBAC can prevent the manager from starting its caches.

Deletion uses uncached reads and UID/resourceVersion delete preconditions. It first deletes the associated Application and waits for Argo's foreground `resources-finalizer.argocd.argoproj.io` to finish workload cleanup. Only then does it delete the namespace and wait for its disappearance before releasing the preview finalizer. Ownership conflicts block cleanup. An Argo outage can leave deletion waiting; restore Argo and inspect its Application Conditions instead of clearing finalizers manually. Baseline-only cleanup continues to work without Argo. Do not disable integration or change its Argo namespace while Application-backed previews exist; clean them up before reconfiguring that deployment.

## Install on edlab

First push the implementation commit and wait for Forgejo validation. Argo must be installed and able to read the repository; credentials, if required, belong in Argo's repository configuration, not the PreviewEnvironment. Check the installed version/CRDs against the fields used here. Tests pin the upstream v3.3.0 Application CRD; this is not a claim about the cluster's version.

```sh
kubectl config current-context
kubectl get crd applications.argoproj.io appprojects.argoproj.io
kubectl -n argocd get deployment,statefulset
```

Build and publish a new controller image using the Podman commands in step 1 of [the baseline smoke test](smoke-test.md). Skip its `make deploy` command and deploy the Argo overlay below:

```sh
# PREVIEW_IMAGE must be the freshly published image, not the old baseline image.
(cd config/overlays/argocd && ../../../bin/kustomize edit set image controller="$PREVIEW_IMAGE")
bin/kustomize build config/overlays/argocd > /tmp/preview-argocd-controller.yaml
```

Review `config/argocd/project.yaml` before applying. It permits only this repo's internal Forgejo URL, the local cluster, `preview-*` destinations and Deployments, Services and ConfigMaps. It denies cluster resources and does not permit baseline policy, quota, ServiceAccount or RBAC manifests. Adjust the exact repository allowlist if using another trusted demo repository. Avoid the permissive default project.

The separate Role/RoleBinding permits Application operations only in `argocd`; the manager's Application cache is scoped to the same namespace. It grants no AppProject editing permission. If changing deployment or Argo namespaces, adjust the RoleBinding subject, Role namespace, overlay flags and project together. Kubernetes RBAC does not constrain Application creation by project or association label: treat controller credentials and PreviewEnvironment creation as trusted administrative access. A shared AppProject allowing `preview-*` is not a hostile tenant boundary; it is intended for trusted manifests without explicit namespace overrides.

```sh
kubectl apply -k config/argocd
kubectl apply -f /tmp/preview-argocd-controller.yaml
kubectl -n preview-environment-controller-system rollout status \
  deployment/preview-environment-controller-controller-manager --timeout=120s
kubectl auth can-i create applications.argoproj.io -n argocd \
  --as=system:serviceaccount:preview-environment-controller-system:preview-environment-controller-controller-manager
```

Expect `yes`. Inspect/commit any overlay image-pin change before publishing it through GitOps later. `make deploy` uses the baseline-only overlay; use the Argo overlay above for this milestone.

## Application smoke test

```sh
kubectl apply -f config/samples/platform_v1alpha1_previewenvironment_application.yaml
kubectl -n preview-environment-controller-system wait --for=condition=Ready \
  previewenvironment/application-smoke --timeout=300s
PREVIEW_APP=$(kubectl -n preview-environment-controller-system get previewenvironment application-smoke \
  -o jsonpath='{.status.application}')
kubectl -n argocd get application "$PREVIEW_APP" -o yaml
kubectl -n preview-application-smoke get deployment,service,pods
kubectl -n preview-environment-controller-system get previewenvironment application-smoke -o yaml
```

Expect Ready=True / ApplicationHealthy, Synced, Healthy and observedGeneration matching metadata.generation. The sample repo path `config/demo` contains one non-root nginx Deployment using `preview-workload` and one ClusterIP Service; it contains no Namespace/baseline. The demo image tag is for the learning exercise; pin a verified image digest for repeatable operation.

The baseline NetworkPolicy denies cross-namespace pod ingress. Test from inside the preview:

```sh
kubectl -n preview-application-smoke run probe --image=busybox:1.37 \
  --restart=Never --overrides='{"spec":{"serviceAccountName":"preview-workload"}}' --command -- sleep 3600
kubectl -n preview-application-smoke wait --for=condition=Ready pod/probe --timeout=120s
kubectl -n preview-application-smoke exec probe -- wget -T 5 -qO- http://preview-demo
kubectl -n preview-application-smoke delete pod probe
```

To test watch-driven failure reporting, change the preview revision to a nonexistent branch, inspect its Ready Condition and the Application's error message, then restore `main`:

```sh
kubectl -n preview-environment-controller-system patch previewenvironment application-smoke \
  --type=merge -p '{"spec":{"revision":"nonexistent-smoke-revision"}}'
kubectl -n preview-environment-controller-system get previewenvironment application-smoke -w
# Stop the watch after Ready=False; inspect full Conditions/Application errors.
kubectl -n argocd get application "$PREVIEW_APP" -o yaml
kubectl -n preview-environment-controller-system patch previewenvironment application-smoke \
  --type=merge -p '{"spec":{"revision":"main"}}'
kubectl -n preview-environment-controller-system wait --for=condition=Ready \
  previewenvironment/application-smoke --timeout=300s
```

Verify observedGeneration after the restored revision; `kubectl wait` can briefly see an earlier Ready Condition. To test drift, patch the Application's targetRevision and verify the controller restores the preview's revision. To test recreation, delete the Application and confirm a new Application UID appears after Argo completes cascading deletion, followed by deployment recovery. These require Argo to be healthy.

Finally:

```sh
kubectl -n preview-environment-controller-system delete previewenvironment application-smoke --wait=false
kubectl -n argocd wait --for=delete application/"$PREVIEW_APP" --timeout=300s
kubectl wait --for=delete namespace/preview-application-smoke --timeout=300s
kubectl -n preview-environment-controller-system wait --for=delete \
  previewenvironment/application-smoke --timeout=300s
```

Confirm all three disappear without manual finalizer removal. Record image tag, Argo version and outcomes in `docs/learning.md`. These new Argo checks have not yet been run on edlab.

## Test boundaries and references

Unit tests exercise zero-write idempotency, revision freshness, readiness errors, drift preservation, conflicts, transient Application creation/deletion failure, disabled integration and cleanup ordering/restarts. Envtest validates against the unchanged upstream Application CRD, runs the real mapped watch, updates source revisions, repairs Application drift, recreates manually deleted Applications and restarts during ordered cleanup. It simulates Argo status/finalizer completion and namespace finalization: it does not run repo-server, the Argo controller, a CNI or garbage collection.

The controller uses Kubernetes unstructured objects for this external CRD to avoid importing Argo CD's server dependencies. No additional Go dependencies were introduced. Fields are verified against the [pinned upstream API](https://github.com/argoproj/argo-cd/blob/v3.3.0/pkg/apis/application/v1alpha1/types.go) and the real CRD fixture. Upstream references: [Application specification](https://argo-cd.readthedocs.io/en/stable/user-guide/application-specification/), [AppProject restrictions](https://argo-cd.readthedocs.io/en/stable/user-guide/projects/), [cascading deletion](https://argo-cd.readthedocs.io/en/stable/user-guide/app_deletion/).
