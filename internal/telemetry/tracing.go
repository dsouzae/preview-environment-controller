// Package telemetry configures optional trace export without owning the manager lifecycle.
package telemetry

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// Enabled reads the edlab opt-in; the standard SDK kill switch takes precedence.
func Enabled() (bool, error) {
	for _, name := range []string{"OTEL_SDK_DISABLED", "OTEL_ENABLED"} {
		if value := os.Getenv(name); value != "" {
			enabled, err := strconv.ParseBool(value)
			if err != nil {
				return false, fmt.Errorf("%s must be a boolean", name)
			}
			if name == "OTEL_SDK_DISABLED" && enabled {
				return false, nil
			}
			if name == "OTEL_ENABLED" {
				return enabled, nil
			}
		}
	}
	return false, nil
}

// Start uses OTLP/HTTP environment configuration (endpoint, headers, TLS and
// sampling). A bounded, nonblocking batch queue decouples collector outages from
// reconciliation; full queues drop spans rather than blocking API operations.
func Start(ctx context.Context, enabled bool, version string) (trace.TracerProvider, func(context.Context) error, error) {
	if !enabled {
		return noop.NewTracerProvider(), func(context.Context) error { return nil }, nil
	}
	for _, name := range []string{"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"} {
		if value := os.Getenv(name); value != "" {
			endpoint, err := url.Parse(value)
			if err != nil || endpoint.Host == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.User != nil {
				return nil, nil, fmt.Errorf("%s must be an HTTP(S) URL without user credentials", name)
			}
		}
	}
	exporter, err := otlptracehttp.New(ctx, otlptracehttp.WithTimeout(3*time.Second), otlptracehttp.WithRetry(otlptracehttp.RetryConfig{Enabled: false}))
	if err != nil {
		return nil, nil, fmt.Errorf("initialize OTLP HTTP trace exporter: %w", err)
	}
	identity, err := resource.New(ctx,
		resource.WithAttributes(attribute.String("service.name", "preview-environment-controller"), attribute.String("service.version", version)),
		resource.WithFromEnv(),
	)
	if err != nil {
		_ = exporter.Shutdown(ctx)
		return nil, nil, err
	}
	provider := sdktrace.NewTracerProvider(sdktrace.WithResource(identity), sdktrace.WithBatcher(exporter,
		sdktrace.WithMaxQueueSize(2048), sdktrace.WithMaxExportBatchSize(256),
		sdktrace.WithBatchTimeout(2*time.Second), sdktrace.WithExportTimeout(3*time.Second)))
	return provider, provider.Shutdown, nil
}
