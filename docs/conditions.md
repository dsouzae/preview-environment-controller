# Status, phases and Conditions

Reference for everything the controller writes to `status`. Use it to read `kubectl get previewenvironment -o yaml` output and to decide whether to wait, fix something, or stop. The reconciliation behind each reason is described in [design.md](design.md).

## Status fields

| Field | Meaning |
|---|---|
| `observedGeneration` | `metadata.generation` the status was computed from. A Ready Condition with an older `observedGeneration` than `metadata.generation` describes the previous spec; `kubectl wait --for=condition=Ready` can return on such a stale Condition right after an edit. |
| `namespace` | Computed target namespace (`spec.namespace`, or `preview-<CR UID>`). Empty when the configuration is invalid. |
| `phase` | `Pending`, `Ready` or `Deleting`; see below. |
| `expiresAt` | `metadata.creationTimestamp + spec.ttl` at second precision. Absent without a TTL or when the TTL is invalid. |
| `application` | Name of the managed Argo CD Application (`preview-<CR UID>`) in the configured Argo namespace. Repository-backed previews only. |
| `syncStatus` | Mirror of the Application's `status.sync.status` (`Synced`, `OutOfSync`, `Unknown`). Cleared whenever the baseline is not ready, so an old `Synced` never sits next to a failed namespace. |
| `applicationHealth` | Mirror of the Application's `status.health.status` (`Healthy`, `Progressing`, `Degraded`, `Missing`, …). Cleared under the same rule. |
| `conditions` | `Ready`, plus `ApplicationCreated` and `ApplicationHealthy` for repository-backed previews. |

## Phases

| Phase | Set when | Leaves when |
|---|---|---|
| `Pending` | Any reconcile where `Ready` is `False` and the preview is not being deleted: provisioning in progress, waiting on Argo CD, a conflict, an API failure, or an invalid spec. The Ready reason says which. | Ready becomes `True`, or deletion starts. |
| `Ready` | The Ready Condition is `True`: namespace and all four baseline resources exist and match (`BaselineProvisioned`), and for repository-backed previews Argo CD reports the desired Application `Synced` and `Healthy` (`ApplicationHealthy`). It does not prove the CNI enforces the NetworkPolicy. | A spec change, child drift or deletion, an Argo status change, or deletion. |
| `Deleting` | `metadata.deletionTimestamp` is set and the cleanup finalizer is still present. | The finalizer is removed and the object disappears. |

A phase is a summary; the Ready reason is the diagnostic. Phase values are enforced by the CRD schema.

## Condition types

| Type | Present | True means | Reasons |
|---|---|---|---|
| `Ready` | Always, after the first reconcile. | The preview is usable: baseline provisioned and, if configured, Application synced and healthy. | Table below. |
| `ApplicationCreated` | Repository-backed previews, once the Application exists; removed when the baseline fails or the repository is absent. | The Application object exists in the Argo namespace. | `ApplicationProvisioned` |
| `ApplicationHealthy` | Same as `ApplicationCreated`. | Argo CD reports `Synced` and `Healthy` for the desired source and destination, with no error Conditions and no sync in progress. | `ApplicationError`, `ApplicationPending`, `ApplicationSyncing`, `ApplicationOutOfSync`, `ApplicationUnhealthy`, `ApplicationHealthy` (same meanings as the Ready reasons of those names). |

Transition times are preserved: a reconcile that computes an identical status writes nothing.

## Ready reasons

"Retry" is how the controller tries again:

- **backoff**: an error was returned, so controller-runtime requeues with exponential backoff.
- **5 s**: a timed requeue while something is terminating, plus any watch event on the object concerned.
- **event**: no timer. The next reconcile comes from a watch event on the primary (spec, deletionTimestamp or finalizer change) or on the related Namespace, baseline child or Application, including unlabelled conflicting objects, which are matched by name.
- **none**: a terminal state until the spec changes.

When `spec.ttl` is set, every reconcile that returns no error also schedules a wake-up at `status.expiresAt`, whatever the reason.

### Validation

| Reason | Phase | Retry | Recovers when | What to do |
|---|---|---|---|---|
| `InvalidTTL` | Pending | none | `spec.ttl` is corrected | The CRD schema rejects bad TTLs on admission, so this appears only for objects that bypassed validation (for example an older CRD). Fix `spec.ttl` to whole hours/minutes/seconds such as `24h` or `1h30m`. Nothing is provisioned meanwhile. |
| `InvalidConfiguration` | Pending | none | the spec or placement is corrected | The target name is not a `preview-` DNS label, the object has no UID, or the preview sits inside its own target namespace (which would deadlock deletion). `spec.namespace` is immutable, so recreate the preview with a valid name or in a control namespace. |

### Namespace

| Reason | Phase | Retry | Recovers when | What to do |
|---|---|---|---|---|
| `NamespaceProvisioningFailed` | Pending | backoff | the API call succeeds | Reading or creating the Namespace failed. An `AlreadyExists` right after a cached `NotFound` is a normal race and clears on its own. If it persists, check the controller's RBAC on `namespaces` and API server health; the message carries the API error. |
| `NamespaceConflict` | Pending | event | the unrelated namespace is removed | A namespace with the target name exists without this preview's `platform.ellery.dev/preview-uid` label. The controller never adopts it. Delete the namespace if it is disposable, or delete the preview and recreate it with another name. Relabelling the namespace by hand is a takeover, not a fix. |
| `NamespaceTerminating` | Pending | 5 s | the namespace is fully gone, then it is recreated | A previous incarnation is still being deleted. Wait. If it stays for minutes, read the namespace's `status.conditions` for resources or finalizers blocking deletion. |

### Baseline

| Reason | Phase | Retry | Recovers when | What to do |
|---|---|---|---|---|
| `BaselineProvisioningFailed` | Pending | backoff | the API call succeeds | A get/create/patch on `preview-baseline` (ResourceQuota, LimitRange, NetworkPolicy) or the `preview-workload` ServiceAccount failed. The message names the kind and object. Check RBAC for those kinds, including `networking.k8s.io`. Optimistic-lock conflicts here are transient. |
| `BaselineConflict` | Pending | event | the foreign object or its owner reference is removed | One of those objects exists with a different `preview-uid` label, or with an owner reference that does not point at this Namespace's UID (typically a leftover from a previous namespace incarnation, or a manifest that declares baseline resources itself). Remove it. Argo CD Applications must not declare these objects; the sample AppProject excludes them. |
| `BaselineTerminating` | Pending | 5 s | the child is gone, then it is recreated | Wait. |
| `BaselineProvisioned` | **Ready** | none | — | Steady state for baseline-only previews. Workloads may be deployed; set `serviceAccountName: preview-workload`. |

### Application (repository-backed previews)

| Reason | Phase | Retry | Recovers when | What to do |
|---|---|---|---|---|
| `ApplicationConfigurationInvalid` | Pending | none | the controller or spec is fixed | `spec.revision` is missing (the schema normally prevents this), or the controller runs without `--argo-namespace` or with the `default` project. Deploy an Argo-enabled overlay. `spec.repository` cannot be removed; to abandon the Application, delete the preview, which stays `CleanupBlocked` until integration is enabled. |
| `ApplicationProvisioningFailed` | Pending | backoff | the API call succeeds | Reading, creating or patching the Application failed. Check that the Argo CD CRDs are installed and that the controller's Role/RoleBinding in the Argo namespace (`config/argocd/application_rbac.yaml`) is applied. |
| `ApplicationConflict` | Pending | event | the foreign Application is removed | `preview-<CR UID>` exists in the Argo namespace with a different label or name/namespace annotation, or it has owner references. The controller refuses to adopt it. Remove it. |
| `ApplicationTerminating` | Pending | 5 s | the Application is gone, then it is recreated | Argo CD is still running its cascading deletion of a previous Application. Wait. |
| `ApplicationPending` | Pending | event | Argo CD updates `status.sync.comparedTo` | The spec was just created or changed, or Argo's last comparison was against an older source/destination, so an old `Synced`/`Healthy` is not trusted. Normal after creation or a revision change. If it never clears, the Argo application controller is not running or is not processing this Application. |
| `ApplicationError` | Pending | event | Argo CD clears the error Condition | Argo reported a `*Error` Condition (`ComparisonError`, `InvalidSpecError`, `SyncError`, …). The message carries Argo's type and text. Typical causes: wrong repository URL or revision, path missing, repository or destination not allowed by the AppProject, invalid manifests. Fix the spec or the project and the watch picks up the change. |
| `ApplicationSyncing` | Pending | event | the sync operation finishes | A sync is running or requested. Wait. |
| `ApplicationOutOfSync` | Pending | event | Argo reports `Synced` | Automated sync (prune, self-heal) should converge. If it stays OutOfSync, read the Application's `status.operationState` and `status.conditions`: `FailOnSharedResource=true` refuses resources tracked by another Application, and the project denies cluster-scoped resources and kinds outside Deployment/Service/ConfigMap. |
| `ApplicationUnhealthy` | Pending | event | Argo reports `Healthy` | Manifests applied but workloads are `Progressing`, `Degraded` or `Missing`. Inspect pods in the preview namespace: quota rejections (`exceeded quota`), LimitRange `max` violations, image pulls blocked by the NetworkPolicy (egress allows only same-namespace pods and DNS), or a missing `serviceAccountName: preview-workload`. |
| `ApplicationHealthy` | **Ready** | none | — | Steady state for repository-backed previews. This is Argo's last assessment, not a probe: a moving branch can change before Argo refreshes, and an Argo outage freezes it. |

### TTL

| Reason | Phase | Retry | Recovers when | What to do |
|---|---|---|---|---|
| `TTLExpired` | Pending | — | deletion is accepted | Transient: the deadline passed and the controller has requested deletion of the preview with UID/resourceVersion preconditions. The Cleanup reasons follow once `deletionTimestamp` is set. Nothing new is provisioned after expiry. |
| `TTLDeletionFailed` | Pending | backoff | the delete call succeeds | Deleting the PreviewEnvironment failed. A precondition conflict after a concurrent edit simply retries. A `Forbidden` means the controller lacks `delete` on `previewenvironments`, which the TTL milestone added to the generated RBAC: redeploy RBAC with the CRD. |

### Cleanup (phase Deleting)

| Reason | Phase | Retry | Recovers when | What to do |
|---|---|---|---|---|
| `ApplicationCleanupInProgress` | Deleting | 5 s | the Application disappears (uncached read) | The Application has been deleted with preconditions and Argo CD's `resources-finalizer.argocd.argoproj.io` is removing the workloads. Wait. If it stays, Argo CD may be down, or a workload carries its own finalizer; inspect the Application and the preview namespace. Never remove Argo's finalizer. |
| `CleanupInProgress` | Deleting | 5 s | the namespace disappears (uncached read) | The namespace has been deleted with preconditions and the namespace controller is removing its contents. Wait. If it stays, read the namespace's `status.conditions` (`NamespaceContentRemaining`, `NamespaceFinalizersRemaining`) to find what blocks it. Never clear the namespace's own finalizers. |
| `CleanupBlocked` | Deleting | event (namespace cases), 5 s (Application cases) | the association is resolved or the object is removed | The controller refuses to delete something it does not own. Causes, in the message: the target name is invalid; the namespace's `preview-uid` label does not match; the Application's association does not match or has owners; or Argo CD integration is disabled while the preview has a repository. Fix the controller configuration or remove the foreign object. If the namespace genuinely belongs to someone else and must be kept, removing `platform.ellery.dev/namespace-cleanup` from the preview releases it without touching the namespace; that is the one case where removing the preview finalizer by hand is appropriate, and it leaks anything the preview did own. |
| `CleanupFailed` | Deleting | backoff | the API call succeeds | An uncached read or a delete of the namespace or Application failed. Precondition conflicts (409) retry on their own. A persistent `Forbidden` points at missing `delete` RBAC on `namespaces` or `applications.argoproj.io`. |

## Reading status quickly

```sh
kubectl -n <control-namespace> get previewenvironments            # NAMESPACE and PHASE columns
kubectl -n <control-namespace> get previewenvironment <name> \
  -o jsonpath='{range .status.conditions[*]}{.type}{"\t"}{.status}{"\t"}{.reason}{"\t"}{.message}{"\n"}{end}'
```

Compare `status.observedGeneration` with `metadata.generation` after any spec edit. For `Application*` reasons, the Application itself has the detail: `kubectl -n argocd get application <status.application> -o yaml`.
