# Controller observability

Tracing is opt-in. Existing controller-runtime metrics and health probes keep their original lifecycle. The default deployment keeps authenticated HTTPS metrics on 8443; it does not export traces. No collector is required to run the controller.

## Tracing configuration

Set `OTEL_ENABLED=true` to install the OpenTelemetry Go SDK and OTLP/HTTP trace exporter, pinned to v1.44.0. `OTEL_SDK_DISABLED=true` takes precedence and disables tracing. Invalid booleans or invalid endpoint URLs fail startup rather than silently misconfiguring export.

The exporter reads standard SDK variables:

- `OTEL_EXPORTER_OTLP_ENDPOINT`: base HTTP(S) URL; the exporter appends `/v1/traces`.
- `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT`: full trace URL, which takes precedence.
- `OTEL_EXPORTER_OTLP_HEADERS` / `OTEL_EXPORTER_OTLP_TRACES_HEADERS`: collector authentication headers. Configure secrets with Kubernetes Secret references, not tracked plaintext.
- Standard exporter TLS certificate/client certificate variables and `OTEL_EXPORTER_OTLP_INSECURE` as supported by the pinned SDK.
- `OTEL_TRACES_SAMPLER` and `OTEL_TRACES_SAMPLER_ARG`: standard SDK sampling. The SDK default samples all traces; the edlab overlay uses `parentbased_traceidratio` with `0.1`.
- `OTEL_RESOURCE_ATTRIBUTES` and `OTEL_SERVICE_NAME`: resource identity overrides. Defaults are `service.name=preview-environment-controller` and `service.version` from the build's VERSION.

Transport is always HTTP/protobuf; this implementation does not select gRPC based on `OTEL_EXPORTER_OTLP_PROTOCOL`. For predictable outage behavior, exporter timeout is fixed at three seconds and exporter retries are disabled; timeout environment variables do not override it. Each failed batch is reported through the structured OTel error handler, without failing reconciliation. Subsequent batches attempt export again.

The batch processor has a 2,048-span queue, 256-span batches and a two-second interval. Queue overflow drops spans; it never blocks reconciliation. After the manager stops its workers, shutdown drains queued spans within a five-second context. Trace delivery is best effort; shutdown deadlines, sampling, crashes, and outages can lose spans.

## What is instrumented

Each reconciliation produces `preview.reconcile`, with nested steps `preview.namespace`, `preview.baseline`, `preview.application`, `preview.status`, `preview.cleanup`, and `preview.expiry` when those paths run. Status spans can be children of another step. Cleanup includes uncached checks and waiting for Argo CD and namespace deletion; baseline spans cover all four baseline resources.

Root attributes include preview name/control namespace/UID, resource generation, Ready Condition/reason, a requeue boolean and error outcome. These identities help locate one preview; they are never metric labels. Source repository URLs, Git revisions, Condition messages and raw Kubernetes/Argo errors are not exported in spans. Error status descriptions use fixed categories: `conflict`, `forbidden`, `timeout`, `error`. Consult Conditions and controller logs for the concrete failure.

A successful span means the API/controller step completed without a returned error; a pending or blocked Condition can still have successful spans. Check readiness attributes as well as span error status. The requeue attribute describes explicit scheduled retries, such as TTL scheduling or cleanup waiting. Returned errors use controller-runtime backoff even when this attribute is false; its presence alone does not indicate failure. Controller-runtime coalesces work-queue events, so a span cannot reliably identify the precise event that triggered reconciliation. Root traces do not claim to preserve incoming distributed trace context through the Kubernetes queue.

## Metrics

Use controller-runtime's `controller_runtime_reconcile_total`, `controller_runtime_reconcile_errors_total`, `controller_runtime_reconcile_time_seconds` and workqueue metrics for controller throughput, errors, duration and queue pressure. We do not duplicate these with OTel metrics.

`preview_environment_step_duration_seconds` adds step latency and attempt counts (`_count`), grouped only by six fixed step values and five error outcomes. Histogram observations include unchanged resources and status no-ops. It does not add preview names, namespaces, UIDs, URLs, revisions or arbitrary errors as labels. A baseline span identifies the stage; it does not isolate individual resource operations or instrument every Kubernetes HTTP request.

This deliberately adapts the edlab playbook: OTLP/HTTP carries traces; the existing native Prometheus registry carries metrics. A second MeterProvider/OTLP metrics exporter would duplicate runtime/controller metrics and add another collection lifecycle. Configure Prometheus or its collector receiver to scrape the endpoint when you need metrics forwarded centrally.

## Optional edlab deployment

Build/publish the new image with the tag below using the Podman instructions in the [cluster smoke test](smoke-test.md); skip its baseline `make deploy` command. For an Application-enabled controller, use this composed overlay so Application watches and cleanup remain enabled. Review the restricted AppProject and Application Role/RoleBinding described in [Argo CD setup](argocd.md) before applying them. Render and review before deployment:

```sh
export PREVIEW_IMAGE=container-registry.lab.edlab.dev/edlab/preview-environment-controller/manager:$(git rev-parse --short HEAD)
# Publish this image first; do not deploy a tag that does not exist.
(cd config/overlays/argocd-observability && ../../../bin/kustomize edit set image controller="$PREVIEW_IMAGE")
bin/kustomize build config/overlays/argocd-observability > /tmp/preview-observability.yaml
kubectl apply -k config/argocd
kubectl apply --dry-run=server -f /tmp/preview-observability.yaml
kubectl apply -f /tmp/preview-observability.yaml
```

This overlay enables tracing to `http://otel-collector-collector.opentelemetry-operator.svc.cluster.local:4318`, retains health probes, and replaces metrics serving with unauthenticated HTTP on `:9090`. The metrics Service exposes port 9090 named `http-metrics` and both the Service and Deployment pod template advertise matching HTTP scrape annotations (including the edlab pod-annotation convention). Annotation-based scraping requires a Prometheus scrape job that honors these annotations; annotations alone do not configure Prometheus. Configure one chosen discovery method or exclude duplicate targets if your installation honors both pod and Service annotations.

If using Prometheus Operator, apply `config/observability/servicemonitor.yaml` separately after adding any release labels required by your Prometheus `serviceMonitorSelector`. Its endpoint is HTTP with no bearer token or TLS settings. Do not combine it with the default HTTPS ServiceMonitor in `config/prometheus`.

Plaintext metrics are suitable only for a trusted cluster network with appropriate access controls; this overlay does not create a public ingress or enforce a NetworkPolicy for the manager. Keep the default authenticated HTTPS deployment if that is your required boundary. The composed `config/overlays/argocd-observability` overlay retains `--argo-namespace=argocd` and `--argo-project=preview-environments`; the commands install the separate namespace-scoped Application RBAC. For a deliberately baseline-only installation, instead pin the image with `(cd config/observability && ../../bin/kustomize edit set image controller="$PREVIEW_IMAGE")` and render `config/observability`. Do not switch an Application-enabled controller to baseline-only while managed Applications still require cleanup. Inspect/commit image-pin changes before publishing them through GitOps.

## Manual verification (not yet run on edlab)

1. Deploy the new image with tracing enabled. For a short verification window, override `OTEL_TRACES_SAMPLER=always_on` to avoid sampling ambiguity, then restore the intended ratio.
2. Port-forward the metrics Service and inspect the endpoint:

   ```sh
   kubectl -n preview-environment-controller-system port-forward service/preview-environment-controller-manager-metrics 9090:9090
   curl --fail http://127.0.0.1:9090/metrics
   ```

3. Create a preview, edit its spec, modify/delete a baseline resource, and delete the preview using the documented smoke tests. Check runtime reconcile metrics and `preview_environment_step_duration_seconds_count` increase for the relevant paths.
4. In the collector's configured tracing backend, find service `preview-environment-controller` and filter by `preview.name` and `preview.namespace`. Check the root and nested step spans, readiness and any conflict/error outcome. Collector deployment alone does not provide a trace UI; verify that its traces pipeline has a backend exporter, or temporarily inspect a debug exporter configured by the cluster administrator.
5. Temporarily point this controller's trace endpoint at an unreachable test address and repeat a preview operation. Check exporter failures in logs and confirm Ready/drift repair/cleanup still proceed. Restore the endpoint; new batches should appear again. Do not stop the shared collector to test one controller.
6. Restart the controller gracefully and verify queued sampled traces reach the collector, allowing for the five-second flush bound. Set `OTEL_SDK_DISABLED=true` and confirm preview behavior/Prometheus metrics still work with tracing disabled.

Local automated tests exercise disabled mode, identity/env overrides, protobuf HTTP export, shutdown flushing, collector failures, nested spans, error classification and fixed metric labels. They use a local fake OTLP receiver, not a live collector. Envtest checks existing lifecycle/TTL/Application behavior with instrumentation installed; it does not prove edlab collector connectivity, backend ingestion, production throughput or scale.

## Upstream references

- [Pinned OTLP/HTTP exporter options](https://pkg.go.dev/go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp@v1.44.0)
- [Pinned trace SDK and batch processor](https://pkg.go.dev/go.opentelemetry.io/otel/sdk/trace@v1.44.0)
- [Controller-runtime metrics](https://book.kubebuilder.io/reference/metrics.html)
