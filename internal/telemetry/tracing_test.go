package telemetry

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"
)

func TestDisabledTracingIgnoresExporterConfiguration(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "malformed")
	provider, shutdown, err := Start(context.Background(), false, "test")
	if err != nil {
		t.Fatal(err)
	}
	_, span := provider.Tracer("test").Start(context.Background(), "disabled")
	if span.IsRecording() {
		t.Fatal("disabled provider recorded a span")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestEnabledKillSwitchAndValidation(t *testing.T) {
	t.Setenv("OTEL_ENABLED", "true")
	t.Setenv("OTEL_SDK_DISABLED", "true")
	if enabled, err := Enabled(); err != nil || enabled {
		t.Fatalf("kill switch: %v %v", enabled, err)
	}
	t.Setenv("OTEL_SDK_DISABLED", "false")
	if enabled, err := Enabled(); err != nil || !enabled {
		t.Fatalf("opt in: %v %v", enabled, err)
	}
	t.Setenv("OTEL_ENABLED", "maybe")
	if _, err := Enabled(); err == nil {
		t.Fatal("invalid boolean accepted")
	}
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "https://user:secret@example.invalid")
	if _, _, err := Start(context.Background(), true, "test"); err == nil {
		t.Fatal("credential URL accepted")
	}
}

func TestHTTPExportAndShutdownFlush(t *testing.T) {
	requests := make(chan *collectortrace.ExportTraceServiceRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/traces" || r.Header.Get("test-header") != "value" {
			t.Errorf("export configuration: %s %s", r.URL.Path, r.Header.Get("test-header"))
		}
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		request := &collectortrace.ExportTraceServiceRequest{}
		if err := proto.Unmarshal(data, request); err != nil {
			t.Error(err)
			return
		}
		requests <- request
		w.Header().Set("Content-Type", "application/x-protobuf")
	}))
	defer server.Close()
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", server.URL)
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "test-header=value")
	t.Setenv("OTEL_SERVICE_NAME", "preview-test")
	t.Setenv("OTEL_TRACES_SAMPLER", "always_on")
	provider, shutdown, err := Start(context.Background(), true, "1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	_, span := provider.Tracer("test").Start(context.Background(), "flush-me")
	span.SetAttributes(attribute.String("outcome", "success"))
	span.End()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case request := <-requests:
		attrs := map[string]string{}
		for _, attr := range request.ResourceSpans[0].Resource.Attributes {
			attrs[attr.Key] = attr.Value.GetStringValue()
		}
		if attrs["service.name"] != "preview-test" || attrs["service.version"] != "1.2.3" {
			t.Fatalf("identity: %v", attrs)
		}
		if spans := request.ResourceSpans[0].ScopeSpans[0].Spans; len(spans) != 1 || spans[0].Name != "flush-me" {
			t.Fatalf("spans: %v", spans)
		}
	default:
		t.Fatal("shutdown did not flush")
	}
}

func TestCollectorFailureDoesNotBlockSpanCreation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer server.Close()
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", server.URL)
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	t.Setenv("OTEL_TRACES_SAMPLER", "always_on")
	provider, shutdown, err := Start(context.Background(), true, "test")
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, span := provider.Tracer("test").Start(context.Background(), "outage")
	span.End()
	if time.Since(started) > time.Second {
		t.Fatal("span creation blocked")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := provider.(*sdktrace.TracerProvider).ForceFlush(ctx); err == nil {
		t.Fatal("collector failure was hidden")
	}
	if err := shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestShutdownHonorsDeadline(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) { <-release }))
	defer server.Close()
	defer close(release)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", server.URL)
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	t.Setenv("OTEL_TRACES_SAMPLER", "always_on")
	provider, shutdown, err := Start(context.Background(), true, "test")
	if err != nil {
		t.Fatal(err)
	}
	_, span := provider.Tracer("test").Start(context.Background(), "queued")
	span.End()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	if err := shutdown(ctx); err == nil {
		t.Fatal("blocked collector did not exhaust shutdown deadline")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("shutdown ignored deadline: %s", elapsed)
	}
}
