# Testing

Four layers, each proving something the previous one cannot.

| Layer | Command | Needs | Proves |
|---|---|---|---|
| Unit | `make test` | Go only (vendored deps) | Reconcile decisions against a fake client: ordering, idempotency, conflicts, failure classification, status content. |
| Integration (envtest) | `make test-integration` | `KUBEBUILDER_ASSETS` (real kube-apiserver + etcd) | CRD/CEL validation, real watches, indexes and predicates, optimistic locking against a real API server, manager restart. |
| Cluster smoke tests | manual, see below | A disposable cluster with an enforcing CNI | CNI enforcement, quota admission accounting, real namespace garbage collection, Argo CD sync and cascading deletion, collector ingestion. |
| Kind e2e | `make test-e2e` | Kind | Scaffold placeholder only; not yet meaningful. |

CI (`.forgejo/workflows/ci.yaml`) runs `make build test vet` and a `gofmt -l api cmd internal` check. Lint and envtest are not in CI yet because the runner has no pinned tooling provisioned; run them locally.

## Unit tests

Plain `testing` with the controller-runtime fake client; no cluster, no build tag. The Reconciler is constructed directly and `Reconcile` is called by hand. Deterministic time comes from the `Now` field (`PreviewEnvironmentReconciler{Now: func() time.Time {…}}`), and the uncached reader from the `APIReader` field, which unit tests leave nil so cleanup reads go through the same fake client.

Behaviour that a fake client cannot produce on its own is injected with small wrapper clients that embed `client.Client` and override one method:

| Wrapper | File | Simulates |
|---|---|---|
| `rejectWrites`, `rejectStatus` | `namespace_test.go` | Any write fails the test: proves a steady-state reconcile performs zero writes. |
| `failCreate`, `failBaselineCreate`, `failApplicationCreate` | `namespace_test.go`, `baseline_test.go`, `application_test.go` | API errors on create → `*ProvisioningFailed` with backoff, partial creation preserved. |
| `failDelete`, `ttlFailDelete` | `finalizer_test.go`, `ttl_test.go` | API errors on delete → `CleanupFailed`, `TTLDeletionFailed`. |
| `failRead` | `finalizer_test.go` | A failing `APIReader`: cleanup must not release the finalizer on a read error. |
| `staleNamespaceClient`, `staleApplicationClient` | `finalizer_test.go`, `application_test.go` | A cache that still returns an object the API has deleted, or vice versa: cleanup must trust only the uncached read. |
| `changingNamespaceClient`, `changingBaselineClient`, `changingApplicationClient` | same files | The object changes between the ownership check and the write: the UID/resourceVersion preconditions and optimistic locks must reject the stale decision. |
| `extendingTTLClient` | `ttl_test.go` | A concurrent TTL extension between the expiry decision and the delete. |

Coverage by file:

- `namespace_test.go`: creation, zero-write idempotency, recreation after deletion, conflicts and recovery, transient failures, target-name validation, invalid configuration.
- `finalizer_test.go`: finalizer persisted before provisioning, cleanup waiting and resuming after restart, ownership conflicts, fresh (uncached) reads, missing namespace, delete races, finalizer patch failures, the primary predicate table.
- `baseline_test.go`: default specs for all four kinds, NetworkPolicy selector shapes, deletion and drift repair, partial failure, conflicts and terminating children, the quota-status predicate, ownership races.
- `application_test.go`: Application creation and idempotency, readiness evaluation (errors, `comparedTo` freshness, syncing, out of sync, unhealthy), partial failure and conflicts, Application-before-namespace cleanup, disabled integration blocking readiness and cleanup, fresh reads during cleanup, ownership races.
- `ttl_test.go`: duration parsing, deadline scheduling and edits, expiry while conflicting, expiry through cleanup with delete retries, invalid TTL, preserved earlier retries, concurrent extension.
- `observability_test.go`: span nesting and attributes via an in-memory `tracetest` exporter, bounded metric labels, error classification never exporting raw errors, returned errors visible on the root span.
- `internal/telemetry/tracing_test.go`: disabled mode, kill switch and validation, protobuf export to a fake OTLP HTTP receiver, shutdown flush, collector failure not blocking span creation, shutdown deadline.

## Integration tests (envtest)

Files tagged `//go:build integration` (`previewenvironment_controller_test.go`, `application_integration_test.go`, `ttl_integration_test.go`, `suite_test.go`) start a real API server and etcd, install the CRD from `config/crd/bases` (and the Argo CD fixture from `test/fixtures/argocd` for the Application test), start a real manager with the reconciler, and drive it through the API like a user would.

```sh
make setup-envtest
export KUBEBUILDER_ASSETS="$(bin/setup-envtest use 1.37 --bin-dir bin -p path)"
make test-integration
```

What they cover: CEL validation (immutability, repository/revision rules, TTL pattern), status content and `observedGeneration`, API-level `resourceVersion` idempotency, mapped deletion watches, finalizer-driven cleanup and deletion-metadata events, manager restart during cleanup, baseline owner references, child recreation and spec drift repair, Application revision changes, drift repair, recreation and restart during ordered cleanup, and queue-driven TTL expiry after a TTL edit with no manual `Reconcile` call.

What envtest does **not** run, and how the tests compensate:

- No namespace controller or garbage collector: tests remove namespace contents and finalize the namespace themselves to exercise recreation, and they do this before finalization so a recreated namespace cannot meet stale children.
- No CNI, kubelet or quota-accounting controller: NetworkPolicy enforcement, resource defaulting at admission and quota rejection are not verified here.
- No Argo CD controller or repo-server: tests write `status.sync`, `status.health`, `status.conditions` and remove the Argo finalizer by hand. The fixture CRD validates field shapes against upstream v3.3.0; it is not a claim about the cluster's Argo version.
- controller-runtime 0.25 keeps a process-global controller-name registry, so a stopped manager does not release its name. The restart test sets `SkipNameValidation` in its manager options only; production keeps the default.

## Cluster smoke tests

Manual, documented step by step, each ending with "record the result in `docs/learning.md`":

- [smoke-test.md](smoke-test.md): publish and deploy, create a preview, networking allow/deny and DNS, resource defaults and workload identity, quota admission, drift repair and recreation, cleanup. Passed on edlab 2026-10-03 with image `790e0e6`.
- [argocd.md](argocd.md): Application creation, readiness, revision error reporting, drift, recreation, ordered cascading cleanup. Not yet run.
- [ttl.md](ttl.md): expiry, extension, removal, shortening, with and without an Application. Not yet run.
- [telemetry.md](telemetry.md): metrics endpoint, trace delivery, collector outage behaviour, graceful shutdown flush. Not yet run.

## Conventions

- Builds and tests use `-mod=vendor`; after dependency changes run `make tidy vendor` and commit `vendor/`.
- After editing `api/v1alpha1/*_types.go` or `+kubebuilder:rbac` markers run `make manifests generate` and commit the regenerated CRD and RBAC; integration tests load the CRD from disk, so a stale CRD fails them.
- `make lint-fix` (golangci-lint v2: errcheck, govet, staticcheck, unused, ineffassign) before committing Go changes.
- New status reasons need a row in [conditions.md](conditions.md) and a unit test that produces them.
