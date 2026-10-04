# Changelog

One entry per milestone, newest first, each identified by its commit. There is no tagged release yet: `VERSION` is `0.1.0` and images are tagged with the short commit hash. Design reasoning behind each milestone is in [docs/learning.md](docs/learning.md).

## Unreleased

### Documentation restructure and license

- Added `LICENSE` (Apache License 2.0, matching the existing source headers) for the public repository.
- Split the README into a front page plus `docs/design.md`, `docs/conditions.md`, `docs/operations.md` and `docs/testing.md`; added this changelog and a project-specific section at the top of `AGENTS.md`.
- CRD field descriptions: `status.phase` now documents its three values and is schema-restricted to `Pending`, `Ready`, `Deleting`; `observedGeneration`, `syncStatus`, `applicationHealth` and `conditions` gained descriptions. No behaviour change.
- Samples are commented and a derived-name TTL sample was added.
- Public GitHub release: the CI workflow moved to `.github/workflows/ci.yaml` (GitHub Actions, `ubuntu-latest`), and the Argo CD AppProject allowlist and application sample point at `https://github.com/dsouzae/preview-environment-controller.git` instead of an internal Git server. Argo CD must be able to reach GitHub, or substitute an internal mirror in both places.

### OpenTelemetry tracing — `3ce4bff`

Added
- Opt-in tracing (`OTEL_ENABLED=true`) over OTLP/HTTP: a `preview.reconcile` root span with nested `preview.namespace`, `preview.baseline`, `preview.application`, `preview.status`, `preview.cleanup` and `preview.expiry` steps; bounded, non-blocking batch export; flush on shutdown. Preview identity is a span attribute only; URLs, revisions, raw errors and Condition messages are never exported.
- `preview_environment_step_duration_seconds` histogram with fixed `step`/`outcome` labels alongside the controller-runtime metrics.
- `config/observability` overlay (plaintext metrics on `:9090`, OTLP endpoint and sampling env), `config/overlays/argocd-observability`, optional HTTP `ServiceMonitor`, `internal/telemetry` package, vendored `otlptracehttp` exporter.

Not yet verified: ingestion by a live collector or backend. See `docs/telemetry.md`.

### TTL expiry — `5580603`

Added
- `spec.ttl` (whole hours/minutes/seconds) and `status.expiresAt`, measured from `metadata.creationTimestamp`. Expiry deletes the PreviewEnvironment with preconditions and the existing finalizer performs cleanup. Ready reasons `InvalidTTL`, `TTLExpired`, `TTLDeletionFailed`.

Changed
- Controller RBAC gained `delete` on `previewenvironments`. Deploy the regenerated RBAC together with the CRD.

Not yet verified on a cluster. See `docs/ttl.md`.

### Argo CD Applications — `31e55eb`

Added
- `spec.repository`, `spec.revision`, `spec.path`; a managed Application `preview-<CR UID>` in the Argo namespace with automated prune/self-heal sync and `FailOnSharedResource=true`.
- Status fields `application`, `syncStatus`, `applicationHealth`; Conditions `ApplicationCreated` and `ApplicationHealthy`; Ready reasons `Application*`. Readiness distrusts Argo status whose `comparedTo` does not match the desired source and destination.
- Flags `--argo-namespace` and `--argo-project`; `config/overlays/argocd`; `config/argocd` (restricted AppProject and namespace-scoped Application Role/RoleBinding); `config/demo` sample workload; envtest fixture `test/fixtures/argocd/application-crd.yaml` (upstream v3.3.0, unchanged).
- Deletion now removes the Application and waits for Argo CD's cascading finalizer before deleting the namespace. Ready reason `ApplicationCleanupInProgress`.

Changed
- `spec.repository` cannot be removed once configured (CEL rule).
- The manager enables cached unstructured reads and scopes the Application informer to the Argo namespace.

Operational note: do not disable the integration or change its Argo namespace while repository-backed previews exist. Argo smoke test not yet run. See `docs/argocd.md`.

### Cluster smoke test documented — `d5d8355`

- `docs/smoke-test.md` records the seven-step manual procedure; all steps passed on edlab on 2026-10-03 with image `790e0e6`.

### Metrics Service rename — `a1f28f1`

Changed
- Base metrics Service renamed to `manager-metrics` (rendered `preview-environment-controller-manager-metrics`) because the scaffold name exceeded the 63-character limit and the first deployment rejected it. Update any references to the old name.

### Namespace baseline — `790e0e6`

Added
- Per-preview `preview-baseline` ResourceQuota, LimitRange and NetworkPolicy and a `preview-workload` ServiceAccount (token automount disabled), owned by the Namespace, drift-repaired through mapped watches that ignore quota usage updates. Ready reasons `BaselineProvisioningFailed`, `BaselineConflict`, `BaselineTerminating`.

Changed
- The Ready=True reason is `BaselineProvisioned` (was `NamespaceProvisioned`) and requires all four children.
- Controller RBAC adds get/list/watch/create/patch on `resourcequotas`, `limitranges`, `serviceaccounts` and `networkpolicies`.

### Finalizer cleanup — `e245fc2`

Added
- Finalizer `platform.ellery.dev/namespace-cleanup`, persisted before the namespace is created and released only after an uncached read confirms the namespace is gone. Namespace deletion uses UID and resourceVersion preconditions. Phase `Deleting`; Ready reasons `CleanupInProgress`, `CleanupBlocked`, `CleanupFailed`.

Changed
- The primary watch admits `deletionTimestamp` and finalizer changes, not only generation changes.
- A preview inside its own target namespace is rejected (`InvalidConfiguration`).
- Controller RBAC adds `delete` on `namespaces`, `patch` on `previewenvironments` and `update` on `previewenvironments/finalizers`.

Upgrade
- Let the upgraded controller reconcile existing previews (which adds the finalizer) before deleting them. Previews deleted before this milestone left their namespaces behind; remove those by hand.

### Initial scaffold and namespace provisioning — `01d1c23`

- Kubebuilder 4.16.0 scaffold, Go 1.27.1, controller-runtime 0.25.0, Kubernetes 0.37.0, vendored dependencies.
- `PreviewEnvironment` CRD with optional, immutable `spec.namespace`; namespace creation with a UID association label; no adoption of unrelated namespaces. Ready reasons `InvalidConfiguration`, `NamespaceProvisioningFailed`, `NamespaceConflict`, `NamespaceTerminating`, `NamespaceProvisioned`.
- CI workflow running build, unit tests, vet and gofmt.
