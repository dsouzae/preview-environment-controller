# Operations

Running, deploying, diagnosing and upgrading the controller. For what each status reason means see [conditions.md](conditions.md); for why the controller behaves as it does see [design.md](design.md).

Every `kubectl` and `make deploy` command below changes the selected cluster. Use a disposable development cluster and check `kubectl config current-context` first.

## Local development loop

No live cluster is required for unit tests or envtest (see [testing.md](testing.md)). To exercise the controller against a real API server from your workstation:

```sh
make install          # CRDs into the current context
make run              # controller on the host, baseline-only
# In another terminal:
kubectl apply -f config/samples/platform_v1alpha1_previewenvironment.yaml
kubectl get previewenvironment feature-123 -o yaml
kubectl get namespace preview-feature-123
kubectl -n preview-feature-123 get resourcequota,limitrange,networkpolicy,serviceaccount
```

Delete the namespace and watch it come back with a new UID:

```sh
kubectl delete namespace preview-feature-123
kubectl get namespace preview-feature-123 -o jsonpath='{.metadata.uid}'
```

With the controller still running, delete the preview and wait for cleanup:

```sh
kubectl delete previewenvironment feature-123 --wait=false
kubectl get previewenvironment feature-123 -o yaml           # phase Deleting, CleanupInProgress
kubectl wait --for=delete namespace/preview-feature-123 --timeout=120s
kubectl wait --for=delete previewenvironment/feature-123 --timeout=120s
```

To run with Argo CD integration locally, pass the flags: `go run -mod=vendor ./cmd --argo-namespace=argocd --argo-project=preview-environments`. The Argo CRDs must be installed in the cluster or the manager cannot start its caches.

Kind's default CNI (kindnet) does not enforce NetworkPolicy; use a cluster with Calico, Cilium or another enforcing CNI before trusting the deny checks in the smoke test.

## Deploying to a cluster

| Kustomization | Use |
|---|---|
| `config/default` | Baseline-only controller, authenticated HTTPS metrics on `:8443`, health probes on `:8081`, leader election. `make deploy IMG=…` renders this. |
| `config/overlays/argocd` | `config/default` plus `--argo-namespace=argocd --argo-project=preview-environments`. |
| `config/observability` | `config/default` with plaintext HTTP metrics on `:9090`, scrape annotations and OTLP tracing enabled. |
| `config/overlays/argocd-observability` | Both of the above. |
| `config/argocd` | The restricted AppProject and the namespace-scoped Application Role/RoleBinding. Apply separately with `kubectl apply -k config/argocd` whenever an Argo-enabled overlay is used. |
| `config/observability/servicemonitor.yaml` | Optional, Prometheus Operator only; apply separately after adding your `serviceMonitorSelector` labels. |

Procedure: build and publish the image (step 1 of [smoke-test.md](smoke-test.md); `a.sh` is the saved Podman helper), pin it into the chosen overlay with `kustomize edit set image`, render, `kubectl apply --dry-run=server`, then apply. Installation details for Argo CD are in [argocd.md](argocd.md) and for observability in [telemetry.md](telemetry.md). `make deploy` edits `config/manager/kustomization.yaml`; inspect that image-pin change before committing it.

Controller flags and environment:

| Setting | Default | Effect |
|---|---|---|
| `--argo-namespace` | empty (disabled) | Enables Application reconciliation and the Application watch and cache in this namespace. |
| `--argo-project` | `preview-environments` | AppProject for managed Applications. `default` is refused. |
| `--metrics-bind-address`, `--metrics-secure` | `0`, `true` | Metrics endpoint; overlays set `:8443`/HTTPS or `:9090`/HTTP. |
| `--health-probe-bind-address` | `:8081` | Liveness and readiness probes. |
| `--leader-elect` | `false` | Overlays enable it. |
| `OTEL_ENABLED` | unset | `true` installs the OTLP/HTTP trace exporter; `OTEL_SDK_DISABLED=true` wins. Standard `OTEL_EXPORTER_OTLP_*` and sampler variables apply. |

The image runs as UID 1000 under tini with a read-only root filesystem, dropped capabilities and no privilege escalation.

## RBAC and trust

The controller needs cluster-wide `namespaces` get/list/watch/create/delete, the baseline kinds (get/list/watch/create/patch), its own CRD (including `patch`, `delete`, status and finalizers), and, in the Argo namespace only, `applications.argoproj.io` get/list/watch/create/patch/delete. `config/rbac/role.yaml` is generated from the markers in `internal/controller`.

Whoever can create a PreviewEnvironment can create a namespace with any `preview-*` name and, with Argo CD enabled, deploy any path of an allowed repository into it. Restrict CR creation accordingly. Association labels are not an authorization mechanism, and the baseline is a default rather than a boundary (see [design.md](design.md#security-boundaries)). Credentials for private repositories belong in Argo CD's repository configuration, never in the PreviewEnvironment.

## Diagnosing a preview

```sh
kubectl -n <control-namespace> get previewenvironments
kubectl -n <control-namespace> get previewenvironment <name> \
  -o jsonpath='{range .status.conditions[*]}{.type}{"\t"}{.status}{"\t"}{.reason}{"\t"}{.message}{"\n"}{end}'
kubectl -n preview-environment-controller-system logs \
  deployment/preview-environment-controller-controller-manager --since=10m
```

Then look up the Ready reason in [conditions.md](conditions.md). Most reasons fall into four groups:

- **`*Failed`**: an API call failed and the controller is retrying with backoff. Transient `the object has been modified` conflicts in a short burst are normal, especially while quota accounting writes to the ResourceQuota; the smoke test observed them followed by Ready. Persistent failures while idle mean RBAC or API problems; the message carries the API error.
- **`*Conflict` / `CleanupBlocked`**: a resource with the expected name exists but is not this preview's. The controller will not touch it. Inspect it (`kubectl get namespace <target> -o yaml`, the `preview-baseline` objects, or the Application) and remove it if it is a leftover.
- **`*Terminating` / `*InProgress`**: waiting for Kubernetes or Argo CD. If it lasts minutes, the namespace's `status.conditions` or the Application's status say what is holding deletion.
- **`Application*`**: Argo CD's view. `kubectl -n argocd get application <status.application> -o yaml` has the error, sync and health detail.

After editing a spec, compare `status.observedGeneration` with `metadata.generation`; `kubectl wait --for=condition=Ready` can return on the previous generation's Condition.

Never remove the namespace's or the Application's finalizers to unblock cleanup. Removing the preview's `platform.ellery.dev/namespace-cleanup` finalizer is appropriate only when the namespace genuinely belongs to someone else and must be kept; it releases the preview without deleting anything, and leaks whatever the preview did own.

## Upgrading

- **Deploy CRD and RBAC together.** Each milestone extended both; a controller image newer than the installed RBAC fails with `Forbidden` (for example `TTLDeletionFailed` without `delete` on `previewenvironments`).
- **From the namespace-only milestone (`01d1c23`).** Let the upgraded controller reconcile existing previews before deleting them: the first reconcile adds the cleanup finalizer. Previews deleted before the finalizer existed left their namespaces behind; remove those by hand.
- **Argo CD integration.** Do not disable `--argo-namespace` or change it while repository-backed previews exist: their deletion becomes `CleanupBlocked` until integration is restored. Clean them up first.
- **Metrics Service.** The base Service is `preview-environment-controller-manager-metrics` since `a1f28f1`; update ServiceMonitors or scrape configs that referenced the scaffold name.
- **Argo CD versions.** Tests pin the upstream v3.3.0 Application CRD for schema validation only. Check the installed CRDs expose the fields used (`spec.source`, `spec.destination`, `spec.syncPolicy.automated`, `status.sync.comparedTo`, `status.health`, `status.conditions`).

## Verification status

Before trusting a cluster with application workloads, it must demonstrate what envtest cannot:

- A pod in the preview can reach another pod in the same preview and resolve DNS.
- A pod in another namespace cannot reach the preview app port, and the preview cannot reach another namespace's app port.
- Requests/limits are defaulted and over-budget workloads are rejected after quota accounting settles.
- Pods using `preview-workload` have no automounted API token and receive no application RBAC grants.
- Real namespace deletion removes all children without manually clearing namespace finalizers.

The seven-step baseline smoke test in [smoke-test.md](smoke-test.md) covers these and passed on edlab on 2026-10-03 with image `790e0e6` (networking allow/deny, resource defaults, token and RBAC checks, quota enforcement, drift repair, cleanup); evidence is in [learning.md](learning.md). The Argo CD ([argocd.md](argocd.md)), TTL ([ttl.md](ttl.md)) and telemetry ([telemetry.md](telemetry.md)) smoke tests have not yet been run on a cluster. The Kind e2e suite is a scaffold placeholder, so none of these results are automated cluster coverage, and the project has not been exercised at scale.
