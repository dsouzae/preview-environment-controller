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
- Consequence: deletion currently retains the namespace. A later finalizer will provide real cleanup, tested against partial failure. The current API makes explicit namespace names immutable to avoid orphaning a previous target.

## Status and cache behavior

- A successful write can precede visibility in the manager cache. A cached NotFound followed by AlreadyExists is retryable, not evidence that the namespace should be adopted.
- Ready currently means namespace provisioned. It does not mean an application is deployed.
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
