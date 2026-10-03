# Preview Environment Controller

A Go/Kubebuilder learning project for managing ephemeral application environments on edlab. The controller provisions a namespace and cleans it up through a finalizer. Argo CD integration, policies, TTL, and OpenTelemetry instrumentation are planned; they are not implemented yet.

## Current API

```yaml
apiVersion: platform.ellery.dev/v1alpha1
kind: PreviewEnvironment
metadata:
  name: feature-123
  namespace: default
spec:
  namespace: preview-feature-123
```

`spec.namespace` is optional and immutable, including its presence. Omit it to derive `preview-<CR UID>`; explicit names must be valid namespace names starting with `preview-`. Repository/revision fields will arrive with Argo CD integration rather than being accepted and ignored now.

Status contains the computed namespace, phase, observedGeneration, and a Ready Condition. Ready means only that the associated namespace exists and is not terminating.

## Reconciliation

The controller reads the preview and computes its target name on each reconcile. It creates a missing Namespace with a UID association label and informational annotations. Existing associated namespaces are left unchanged. Unrelated namespaces produce NamespaceConflict without adoption or mutation. Removing a conflict triggers another reconcile through a target-name index.

API errors produce NamespaceProvisioningFailed and return an error for controller-runtime backoff. Invalid configuration produces InvalidConfiguration without a timed retry. Terminating namespaces produce NamespaceTerminating and a five-second retry. There is no steady-state polling.

Primary creation/spec, deletion-timestamp, and finalizer events and namespace create/update/delete events enqueue reconciliation. The primary watch filters status-only updates; equal status is not patched. Reads use the manager cache, so stale NotFound / AlreadyExists and optimistic status conflicts are retried through the normal work queue.

## Deletion and finalizers

A namespaced preview cannot own a cluster-scoped Namespace, so cleanup uses `platform.ellery.dev/namespace-cleanup`. The controller persists this finalizer before creating a namespace. On deletion, it sets phase Deleting / Ready=False, requests namespace deletion, and retains its finalizer until an uncached API read confirms the namespace is absent. It never removes namespace finalizers; Kubernetes must finish cleaning up namespace contents.

Cleanup checks the preview UID association and deletes with both namespace UID and resourceVersion preconditions. A replaced namespace or changed metadata cannot be deleted using an earlier ownership check. Missing namespaces count as successful cleanup. Other controllers' finalizers are preserved.

Temporary read/delete errors produce CleanupFailed and normal error backoff. An ownership mismatch produces CleanupBlocked and retains the finalizer until the namespace association is resolved or the namespace is removed. Namespace events trigger recovery, with five-second retries while deletion is in progress. CleanupInProgress is idempotent: repeated waiting reconciles do not patch equal status or repeat deletion requests. A controller restart resumes from persisted finalizers and namespace state.

Editing association labels makes the namespace a conflict rather than authorizing takeover. These labels are association metadata, not an authorization boundary: limit RBAC access to previews and namespace metadata. Other namespace metadata is outside this milestone's managed state. A preview placed inside its own target namespace is rejected to avoid a deletion deadlock; keep preview CRs in a separate control namespace. Deployments should restrict who can create CRs and choose target names.

For an existing milestone-1 preview, let the upgraded controller reconcile and persist its finalizer before deleting it. Previews already deleted without a finalizer have no surviving resource to drive cleanup. Removing the preview finalizer by hand bypasses cleanup and can leak resources.

## Build and tests

Current pinned stack: Kubebuilder 4.16.0, Go 1.27.1, controller-runtime 0.25.0, Kubernetes libraries 0.37.0. The project was scaffolded on Go 1.26.3 and upgraded to Go 1.27.1; these pins are not a claim of compatibility with the current edlab cluster; verify its server version before deployment.

Dependencies are committed under `vendor/`. Application builds/tests use `-mod=vendor`. Go may download the pinned toolchain on first use if the workstation is older.

```sh
make build
make test
make vet
make lint
```

Generators/lint/envtest setup download pinned tooling; provision these ahead of time for a runner without Go proxy egress:

```sh
make generate manifests
make setup-envtest
export KUBEBUILDER_ASSETS="$(bin/setup-envtest use 1.37 --bin-dir bin -p path)"
make test-integration
```

Unit tests cover namespace creation, zero-write idempotency, recreation, conflicts, transient failures, and configuration errors, cleanup ownership conflicts, delete races, fresh reads, transient cleanup failures, missing namespaces, and preserved finalizers. Integration tests start a real API server and manager to test validation, status, API resourceVersion idempotency, mapped deletion watches, finalizer-driven cleanup, deletion metadata events, and manager restart during cleanup. envtest has no namespace controller or garbage collector; it explicitly finalizes the namespace during deletion testing. Generated Kind e2e tests are scaffold placeholders for the later cluster test milestone.

After dependency changes: `make tidy vendor`. After API/RBAC changes: `make generate manifests`. Generated artifacts are checked in; do not edit the CRD or DeepCopy code manually.

## Local manual exercise

These commands change the selected cluster. Use a disposable development cluster and check `kubectl config current-context` first.

```sh
make install
make run
# In another terminal:
kubectl apply -f config/samples/platform_v1alpha1_previewenvironment.yaml
kubectl get previewenvironment feature-123 -o yaml
kubectl get namespace preview-feature-123
kubectl delete namespace preview-feature-123
# Wait for deletion, then confirm a new namespace UID appears.
kubectl get namespace preview-feature-123 -o jsonpath='{.metadata.uid}'
```

With the controller still running, delete the preview and wait for namespace cleanup:

```sh
kubectl delete previewenvironment feature-123 --wait=false
kubectl get previewenvironment feature-123 -o yaml
kubectl wait --for=delete namespace/preview-feature-123 --timeout=120s
kubectl wait --for=delete previewenvironment/feature-123 --timeout=120s
```

If cleanup is stuck, inspect the preview Conditions and namespace Conditions/finalizers. No live cluster is required for unit tests or envtest.

## Packaging and delivery

The Dockerfile builds offline from vendor and runs as UID 1000 under tini. Manager manifests use a read-only root filesystem, dropped capabilities, no privilege escalation, and leader election. Health probes use :8081; default authenticated HTTPS metrics use :8443 when deployed. The edlab plaintext Prometheus/OTLP conventions will be configured explicitly during observability work.

The initial Forgejo workflow checks build, unit tests, vet, and formatting. Image publication, coverage gates, lint/envtest runner provisioning, and GitOps tag updates remain delivery work. No remote or registry credentials are configured.

Keep Kubebuilder's `api/`, `cmd/`, `internal/controller/`, and `config/` layout. `PROJECT` records generator provenance. The edlab overlay and Argo CD bootstrap configuration will be added after cluster conventions are verified against the [internal playbook](https://forgejo.lab.edlab.dev/edlab/internal-k8s-project-playbook/src/branch/main/docs/NEW_PROJECT_PLAYBOOK.md).

## Next milestones

1. Quota, limits, service account/RBAC, and network isolation.
2. Argo CD Application reconciliation, AppProject restrictions, health/sync watches.
3. TTL, additional failure cases, useful metrics and OTel traces.
4. Coverage gates, offline CI tooling, container publication, and GitOps deployment.

Multi-cluster placement, DNS, ingress, and automatic pull-request discovery are deferred. This project has not been exercised at meaningful scale. See `docs/learning.md` for design observations.
