package controller

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

func TestReconciliationSpansAndBoundedMetrics(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	defer func() { _ = provider.Shutdown(context.Background()) }()
	preview := testPreview()
	r := &PreviewEnvironmentReconciler{Client: testClient(t, preview), Tracer: provider.Tracer("test")}
	if err := reconcile(t, r, preview); err != nil {
		t.Fatal(err)
	}
	spans := recorder.Ended()
	var root sdktrace.ReadOnlySpan
	seen := map[string]bool{}
	for _, span := range spans {
		seen[span.Name()] = true
		if span.Name() == "preview.reconcile" {
			root = span
		}
	}
	for _, name := range []string{"preview.reconcile", "preview.namespace", "preview.baseline", "preview.status"} {
		if !seen[name] {
			t.Fatalf("missing %s", name)
		}
	}
	for _, span := range spans {
		if span.Name() != "preview.reconcile" && span.Parent().SpanID() != root.SpanContext().SpanID() {
			t.Fatalf("detached step: %s", span.Name())
		}
	}
	attrs := map[string]string{}
	for _, attr := range root.Attributes() {
		attrs[string(attr.Key)] = attr.Value.AsString()
	}
	for key, value := range map[string]string{"preview.name": preview.Name, "preview.namespace": preview.Namespace, "preview.uid": string(preview.UID), "ready.reason": "BaselineProvisioned"} {
		if attrs[key] != value {
			t.Fatalf("missing or incorrect root identity/outcome %s: %q", key, attrs[key])
		}
	}

	families, err := metrics.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	allowed := map[string]bool{"namespace": true, "baseline": true, "application": true, "status": true, "cleanup": true, "expiry": true}
	outcomes := map[string]bool{"success": true, "conflict": true, "forbidden": true, "timeout": true, "error": true}
	for _, family := range families {
		if family.GetName() != "preview_environment_step_duration_seconds" {
			continue
		}
		found = true
		for _, metric := range family.Metric {
			if len(metric.Label) != 2 {
				t.Fatalf("unexpected cardinality: %v", metric.Label)
			}
			for _, label := range metric.Label {
				if label.GetName() == "step" && !allowed[label.GetValue()] || label.GetName() == "outcome" && !outcomes[label.GetValue()] {
					t.Fatal(label)
				}
			}
		}
	}
	if !found {
		t.Fatal("histogram not registered")
	}
}

func TestStepErrorClassificationDoesNotExportRawErrors(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	defer func() { _ = provider.Shutdown(context.Background()) }()
	r := &PreviewEnvironmentReconciler{Tracer: provider.Tracer("test")}
	for _, tc := range []struct {
		err     error
		outcome string
	}{
		{nil, "success"}, {errors.New("sensitive-url"), "error"},
		{apierrors.NewConflict(schema.GroupResource{Resource: "namespaces"}, "secret-name", errors.New("detail")), "conflict"},
		{apierrors.NewForbidden(schema.GroupResource{Resource: "namespaces"}, "secret-name", errors.New("detail")), "forbidden"},
		{apierrors.NewTimeoutError("detail", 1), "timeout"},
	} {
		_, finish := r.step(context.Background(), "namespace")
		finish(tc.err)
		spans := recorder.Ended()
		span := spans[len(spans)-1]
		if tc.err != nil && (span.Status().Code != codes.Error || span.Status().Description != tc.outcome) {
			t.Fatal(span.Status())
		}
		if len(span.Events()) != 0 {
			t.Fatal("raw error event exported")
		}
	}
}

func TestReturnedReconcileErrorIsVisibleInRootSpan(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	defer func() { _ = provider.Shutdown(context.Background()) }()
	preview := testPreview()
	r := &PreviewEnvironmentReconciler{Client: failCreate{Client: testClient(t, preview)}, Tracer: provider.Tracer("test")}
	if err := reconcile(t, r, preview); err == nil {
		t.Fatal("expected namespace create error")
	}
	for _, span := range recorder.Ended() {
		if span.Name() == "preview.reconcile" {
			if span.Status().Code != codes.Error {
				t.Fatal("returned error was not marked in root span")
			}
			if len(span.Events()) != 0 {
				t.Fatal("raw error event exported")
			}
			return
		}
	}
	t.Fatal("missing root span")
}
