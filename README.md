# Preview Environment Controller

A Go/Kubebuilder learning project for managing ephemeral application environments on edlab. Each `PreviewEnvironment` gets its own namespace with resource limits, network isolation and a workload identity, and is cleaned up through a finalizer. Optional Argo CD integration deploys a Git-backed Application into that namespace and reports its sync and health. Optional TTL expires previews through the same cleanup path. Optional OpenTelemetry tracing exports reconciliation spans over OTLP/HTTP; controller-runtime's Prometheus metrics remain available.

## Current API

```yaml
apiVersion: platform.ellery.dev/v1alpha1
kind: PreviewEnvironment
metadata:
  name: feature-123
  namespace: default          # any namespace except the target namespace
spec:
  namespace: preview-feature-123   # optional, immutable; default preview-<CR UID>
  # repository: https://…/repo.git # optional; enables an Argo CD Application
  # revision: main                 # required with repository
  # path: config/demo              # defaults to "."
  # ttl: 24h                       # optional; measured from creationTimestamp
```

`spec.namespace` is optional and immutable, including its presence. Omit it to derive `preview-<CR UID>`; explicit names must be valid namespace names starting with `preview-`. Keep the CR in a control namespace, never inside its own target.

`spec.repository` enables a Git-backed Application and requires `spec.revision`; `spec.path` defaults to `.`. These fields can change, but repository cannot be removed once configured. See [docs/argocd.md](docs/argocd.md).

`spec.ttl` is optional: positive whole hours/minutes/seconds such as `24h`, `90m` or `1h30m`. Expiry is measured from `metadata.creationTimestamp`, including time spent waiting or failing; it does not start at Ready. `status.expiresAt` exposes the deadline. Editing TTL recalculates it from the original creation time; shortening it into the past requests deletion immediately, removing it disables expiry, and once deletion has started a TTL edit cannot cancel cleanup. See [docs/ttl.md](docs/ttl.md).

Status reports the computed namespace, a phase (`Pending`, `Ready`, `Deleting`), `observedGeneration`, Conditions and, for repository-backed previews, the Application name, sync status and health. Ready means the namespace and all baseline resources are provisioned and, when a repository is configured, that Argo CD reports the desired Application Synced and Healthy. It does not prove network enforcement. Every phase, Condition and reason is listed in [docs/conditions.md](docs/conditions.md).

Samples: [baseline](config/samples/platform_v1alpha1_previewenvironment.yaml), [derived name with TTL](config/samples/platform_v1alpha1_previewenvironment_ttl.yaml), [Argo CD application](config/samples/platform_v1alpha1_previewenvironment_application.yaml).

## Documentation

| Document | Contents |
|---|---|
| [docs/design.md](docs/design.md) | How reconciliation works: lifecycle, ownership and association, baseline, Application, TTL, finalizer cleanup, watches, status writes, security boundaries |
| [docs/conditions.md](docs/conditions.md) | Every phase, Condition and Ready reason with retry behaviour and what to do |
| [docs/operations.md](docs/operations.md) | Running locally, deployment overlays and flags, RBAC, diagnosing a preview, upgrade notes |
| [docs/testing.md](docs/testing.md) | Unit, envtest and cluster test layers, what each proves, helper patterns |
| [docs/smoke-test.md](docs/smoke-test.md) | Seven-step manual cluster smoke test (passed on edlab 2026-10-03) |
| [docs/argocd.md](docs/argocd.md) | Argo CD Application contract, installation and smoke test |
| [docs/ttl.md](docs/ttl.md) | Preview lifetime behaviour and smoke test |
| [docs/telemetry.md](docs/telemetry.md) | Tracing and metrics configuration, edlab observability overlay |
| [docs/learning.md](docs/learning.md) | Design journal: expectations, observations, decisions, cluster evidence |
| [CHANGELOG.md](CHANGELOG.md) | Milestone history and upgrade notes |

## Build and tests

Current pinned stack: Kubebuilder 4.16.0, Go 1.27.1, controller-runtime 0.25.0, Kubernetes libraries 0.37.0. The project was scaffolded on Go 1.26.3 and upgraded to Go 1.27.1; these pins are not a claim of compatibility with the current edlab cluster; verify its server version before deployment.

Dependencies are committed under `vendor/`. Builds and tests use `-mod=vendor`. Go may download the pinned toolchain on first use if the workstation is older.

```sh
make build
make test          # unit tests, no cluster
make vet
make lint
```

Generators, lint and envtest download pinned tooling; provision these ahead of time for a runner without Go proxy egress:

```sh
make generate manifests
make setup-envtest
export KUBEBUILDER_ASSETS="$(bin/setup-envtest use 1.37 --bin-dir bin -p path)"
make test-integration
```

After dependency changes: `make tidy vendor`. After API or RBAC marker changes: `make generate manifests`. Generated artifacts are checked in; do not edit the CRD, RBAC or DeepCopy code manually. What each test layer covers and where envtest stops is in [docs/testing.md](docs/testing.md).

## Deployment

| Kustomization | Use |
|---|---|
| `config/default` | Baseline-only controller; authenticated HTTPS metrics on `:8443`, probes on `:8081`, leader election (`make deploy IMG=…`) |
| `config/overlays/argocd` | Adds `--argo-namespace=argocd --argo-project=preview-environments` |
| `config/observability` | Plaintext HTTP metrics on `:9090` with scrape annotations, OTLP tracing enabled |
| `config/overlays/argocd-observability` | Both |
| `config/argocd` | Restricted AppProject and namespace-scoped Application RBAC; apply separately with any Argo-enabled overlay |

The Dockerfile builds offline from `vendor/` and runs as UID 1000 under tini; manager manifests use a read-only root filesystem, dropped capabilities, no privilege escalation and leader election. The Forgejo workflow checks build, unit tests, vet and formatting. Image publication, coverage gates, lint/envtest runner provisioning and GitOps tag updates remain delivery work; no remote or registry credentials are configured. Procedures are in [docs/operations.md](docs/operations.md), [docs/smoke-test.md](docs/smoke-test.md), [docs/argocd.md](docs/argocd.md) and [docs/telemetry.md](docs/telemetry.md).

Keep Kubebuilder's `api/`, `cmd/`, `internal/controller/` and `config/` layout; `PROJECT` records generator provenance. Controller GitOps bootstrap and automated image updates will follow the [internal playbook](https://forgejo.lab.edlab.dev/edlab/internal-k8s-project-playbook/src/branch/main/docs/NEW_PROJECT_PLAYBOOK.md).

## Status and next milestones

The baseline smoke test passed on edlab on 2026-10-03 with image `790e0e6`. The Argo CD, TTL and telemetry smoke tests have not yet been run on a cluster, and the Kind e2e suite is a scaffold placeholder.

1. Exercise Argo CD Application sync, error reporting, watches and cascading cleanup on edlab using [the application smoke test](docs/argocd.md).
2. Additional failure cases, useful metrics and OTel traces.
3. Automated cluster smoke tests, coverage gates, offline CI tooling, container publication and GitOps deployment.

Multi-cluster placement, DNS, ingress and automatic pull-request discovery are deferred. This project has not been exercised at meaningful scale.

## License

Apache License 2.0; see [LICENSE](LICENSE). The envtest fixture `test/fixtures/argocd/application-crd.yaml` is copied unchanged from Argo CD v3.3.0, also Apache-2.0.
