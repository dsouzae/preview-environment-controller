# Learning journal

## Initial scaffold and namespace reconciliation

- Expected: the installed Go toolchain would be enough to scaffold the project.
- Observed: Kubebuilder v4.16.0 generated a Go 1.26 / controller-runtime v0.25.0 project; the workstation had Go 1.23.5. Scaffolding wrote files before dependency installation failed.
- Resolution: completed scaffolding in a temporary directory, preserved the existing AGENTS.md, pinned Go 1.26.3, and provisioned dependencies separately. Generated CRDs, RBAC, and DeepCopy code remain separate from handwritten logic.
- Next time: verify the scaffold's dependency/toolchain matrix before starting initialization.

## Namespace ownership and watches

- Expected: a dependent resource could use the preview as its Kubernetes owner.
- Kubernetes constraint: a namespaced PreviewEnvironment cannot own a cluster-scoped Namespace. Such an owner reference does not give valid garbage collection semantics.
- Design: store the preview UID as association metadata; never adopt a namespace with a different or absent UID. Map namespace events through an index of computed target names, including conflicting namespaces without annotations.
- At the initial namespace milestone, deletion retained the namespace. The later finalizer milestone added real cleanup and partial-failure tests. Explicit namespace names remain immutable to avoid orphaning a previous target.

## Status and cache behavior

- A successful write can precede visibility in the manager cache. A cached NotFound followed by AlreadyExists is retryable, not evidence that the namespace should be adopted.
- Ready initially meant namespace provisioned; it now also requires the baseline resources. It does not mean an application is deployed.
- Condition transition times are preserved when readiness does not change. Equal status is not patched; the primary watch filters status-only updates. Namespace watches are not generation filtered.
- envtest runs the API server and etcd, not the namespace controller. Tests must explicitly finalize namespace deletion to exercise recreation; actual cleanup and garbage collection need a real-cluster test later.

## Go toolchain update

- The workstation was upgraded to Go 1.27.1. Confirmed with `GOTOOLCHAIN=local go version`.
- Aligned go.mod, Forgejo setup-go, Dockerfile, and devcontainer pins. Kubernetes dependencies remain at the initial scaffold versions.

## Finalizer cleanup

- Expected: a generation-filtered primary watch would notice deletion. Kubernetes sets deletionTimestamp in metadata without incrementing the CR generation, so that predicate can leave a finalizer stuck indefinitely.
- Resolution: the primary watch explicitly admits generation, deletionTimestamp, and finalizer changes while filtering status-only updates. envtest verifies deletion is handled by the running manager.
- Cleanup responsibility is persisted before namespace creation. The finalizer survives partial success and restarts, and remains until namespace deletion actually completes. Other finalizers remain intact.
- A cached NotFound is not strong enough to prove cleanup completed. Use the manager APIReader for deletion decisions, and UID/resourceVersion delete preconditions to reject replacement or metadata changes after the ownership check. Unit tests simulate stale cache reads and a concurrent association change.
- Cleanup waits for Kubernetes namespace finalization rather than removing namespace finalizers itself. envtest must simulate that separate controller; the manager is restarted while cleanup is pending to verify recovery.
- Ownership conflicts intentionally block cleanup with a visible Condition. The next design step is restricting preview creation and target namespace selection through deployment/RBAC conventions, before introducing application workloads.
- Restart testing exposed controller-runtime v0.25's process-global controller name registry: stopping a manager does not release its name. The sequential restart test disables name validation only in its manager options; production keeps the default validation.

## Namespace baseline and resource watches

- Expected: every child would share the preview's owner reference. Namespaced previews cannot own children across namespace boundaries; the cluster-scoped Namespace can validly own the namespaced quota, limits, policy, and ServiceAccount. Association labels tie them back to the preview.
- Resolution: use Namespace owner references without blockOwnerDeletion, avoiding unnecessary namespace/finalizers permissions; retain the real preview finalizer to delete the namespace.
- Partial resource creation is normal. Fixed desired specs are recomputed on every reconcile and optimistic patches preserve unrelated metadata and reject concurrent changes after an ownership check. A conflicting resource is surfaced through BaselineConflict rather than adopted. A newly recreated namespace gets a new UID; children from an older incarnation cannot be taken over.
- Quota status changes on normal usage accounting. Filter status-only updates on secondary watches to avoid unnecessary reconciliation, while admitting child spec/metadata drift and deletion. Tests cover all four kinds, partial failure, conflicts, and zero-write idempotency.
- NetworkPolicy namespaceSelector and podSelector in the same peer are ANDed. Use that intersection for cluster DNS; a podSelector alone selects local pods. DNS permits both UDP and TCP. Policies are additive, and creating them does not prove the CNI has enforced them. Real traffic tests remain necessary.
- The preview workload currently needs no Kubernetes API access. A dedicated ServiceAccount with token automount disabled and no RoleBinding is the least-privilege starting point. Do not grant read access merely to demonstrate an RBAC object; add permissions when workload requirements justify them.
- envtest lacks namespace cleanup and garbage collection. Its lifecycle test removes baseline contents before namespace finalization so a recreated namespace cannot encounter stale children from its predecessor. It verifies resource API behavior and watches, not CNI enforcement or real quota accounting.

## Rendered Service name exceeded the API limit

- The first cluster deployment rejected the metrics Service because the long project prefix plus Kubebuilder's controller-manager-metrics-service name exceeded 63 characters. Kustomize rendered it successfully; rendering does not validate Kubernetes resource-name constraints.
- Shortened the base Service name to manager-metrics and updated certificate replacement references and the e2e lookup. The rendered name is preview-environment-controller-manager-metrics.
- Next time: inspect rendered names and run a server-side dry-run before deployment, especially when choosing a long project prefix. Reapplying the corrected manifests recovers from a partially successful deployment.

## First edlab cluster smoke test — 2026-10-03

Evidence: user-run deployment and smoke test with controller image tag `790e0e6`, preview `smoke` in `preview-environment-controller-system`, and target namespace `preview-smoke`.

- Expected: changing the quota would be repaired without error logs. Observed: the pod quota returned from `100` to `10`, but controller-runtime logged object-modified conflicts for both ResourceQuota and PreviewEnvironment during reconciliation.
- Why: optimistic patches include resourceVersion, so Kubernetes rejects a write when the object changed after the read. Manager cache lag and concurrent writes can cause this. Quota accounting is a possible concurrent writer; the logs alone do not identify which write caused these particular conflicts.
- Recovery: the controller returned the conflicts for normal work-queue retries; no code change or manual overwrite was needed. The subsequent Ready Condition was True with reason BaselineProvisioned and observedGeneration 1. This confirms eventual drift repair despite transient concurrency errors.
- The apparent hang at `kubectl get networkpolicy --watch` was an open-ended observation command, not a completion check. Use bounded waits or one-shot reads when testing recovery.
- Deletion: the user reported that everything cleaned up after deleting the preview and running the namespace/preview deletion waits. No manual finalizer removal was reported. This provides a real-cluster cleanup observation beyond envtest's simulated namespace finalization.
- Next time: capture before/after resource versions, Conditions, and logs together. A short burst of conflicts followed by readiness is recoverable; persistent conflicts during idle operation require investigation rather than being dismissed as normal.
- The user subsequently confirmed that all seven smoke-test steps passed: image publication and deployment, preview creation, same-namespace connectivity and DNS, blocked cross-namespace ingress/egress, container resource defaults, absent workload API token and denied Secret-list access, quota admission rejection, quota drift repair, NetworkPolicy recreation, and finalizer cleanup.
- Evidence limits: these are user-reported manual cluster-test results, with the Ready Condition and conflict logs captured in the conversation. They are not an automated cluster test suite. This was one preview environment, not a scale test. At the time of this baseline test, Argo CD Application reconciliation was the next implementation milestone.

## Argo CD Application reconciliation — 2026-10-03

- Expected: an Application's Healthy/Synced status could directly determine PreviewEnvironment readiness. The upstream Application API has no `status.observedGeneration`; old status can survive a source update. The controller now checks `status.sync.comparedTo` against the desired source/destination, reports Pending immediately after a spec write, and exposes Ready plus ApplicationCreated/ApplicationHealthy Conditions. This remains the last Argo observation; a moving branch or an Argo outage needs further freshness/observability work.
- Applications live in `argocd`, while previews live in a separate control namespace. A cross-namespace owner reference would be invalid. Use a deterministic preview-UID name, UID/name/namespace association, mapped watches and finalizer cleanup instead. Removing `repository` once configured is rejected so source changes cannot abandon an Application.
- Deleting the namespace first could race Argo sync and recreation. Cleanup now uses uncached Application reads, ownership checks and UID/resourceVersion delete preconditions, waits for Argo's cascading finalizer, then deletes the namespace. Outages intentionally hold deletion; the controller never removes Argo or namespace finalizers.
- Existing baseline-only deployments remain independent of Argo. Integration is opt-in through namespace/project flags and a separate overlay. The Application cache and Role share one namespace; the controller cannot edit AppProjects. The sample project permits only the trusted demo repository, local preview destinations and Deployment/Service/ConfigMap resources. It excludes the controller-owned baseline. Shared project restrictions are not a hostile tenant boundary.
- Used Kubernetes unstructured objects for the external Application CRD rather than importing Argo server packages. Added the unchanged upstream v3.3.0 CRD fixture to validate actual API fields in envtest without expanding the vendored dependency graph. The fixture is not an assertion of the installed edlab Argo version.
- controller-runtime 0.25.0 bypasses cached reads for unstructured objects by default. Enabled cached unstructured reads explicitly for normal reconciliation and scoped its informer to the Argo namespace; deletion still uses APIReader. Stale reads and optimistic conflicts use normal controller-runtime retries.
- A test initially attempted to preserve an annotation by mutating `Unstructured.GetAnnotations()` directly. That accessor returns a copy; explicitly call `SetAnnotations()` after editing it. This matters when using unstructured Kubernetes objects.
- Validation: unit tests, build, vet, lint and real API/manager envtest passed, including status watches, revision changes, drift, recreation and manager restart during ordered cleanup. Envtest explicitly simulates Argo status/cascading completion and namespace finalization. Real Git rendering/sync and workload deletion still need the saved application smoke test on edlab; no live-cluster or scale result is claimed.

## TTL expiry uses reconciliation and the existing cleanup path

- What changed: optional positive whole-second `spec.ttl` computes a deadline from creationTimestamp and reports `status.expiresAt`. The controller requests CR deletion at expiry and leaves finalizers to perform the already-tested Application-before-namespace cleanup.
- Original expectation to avoid: a healthy preview has no further events, and primary-resource status events are filtered. A status deadline alone therefore would never reliably cause expiration. A TTL also must not start only after Ready, otherwise a failing preview could live forever.
- Design: every successful reconciliation schedules the deadline, including conflict/pending states, while preserving earlier retries. Spec edits and restart recompute from creation time; removing TTL disables expiry. API delete preconditions prevent a stale decision from deleting a concurrently extended preview. An in-progress deletion cannot be cancelled by changing TTL.
- Idempotency detail: metav1.Time serialization has second precision. Both supported durations and deadline computation use that precision so repeated real-API reads do not keep patching a subsecond status timestamp.
- Validation: unit tests cover timing, edits, conflicts, invalid durations, delete failure and finalizer preservation. Envtest exercises schema rejection and real queue-driven expiry after a TTL edit. Namespace finalization and actual Argo cascading cleanup still need real-cluster testing; TTL is a deletion-request deadline rather than an exact completion time.
- Next design question: if the API later needs a lifetime measured from readiness or an explicit absolute deadline, give it a distinct field/contract rather than silently changing TTL semantics.
