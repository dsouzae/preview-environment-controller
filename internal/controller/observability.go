package controller

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	stepDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "preview_environment_step_duration_seconds",
		Help:    "Duration of controller steps, including unsuccessful attempts.",
		Buckets: prometheus.DefBuckets,
	}, []string{"step", "outcome"})
	noopTracer = noop.NewTracerProvider().Tracer("edlab.dev/preview-environment-controller")
)

func init() { metrics.Registry.MustRegister(stepDuration) }

func (r *PreviewEnvironmentReconciler) tracer() trace.Tracer {
	if r.Tracer != nil {
		return r.Tracer
	}
	return noopTracer
}

// step is called only with fixed names in controller code. Kubernetes identity,
// repository URLs and raw errors never become metric labels or trace attributes.
func (r *PreviewEnvironmentReconciler) step(ctx context.Context, name string) (context.Context, func(error)) {
	ctx, span := r.tracer().Start(ctx, "preview."+name)
	started := time.Now()
	return ctx, func(err error) {
		outcome := errorOutcome(err)
		stepDuration.WithLabelValues(name, outcome).Observe(time.Since(started).Seconds())
		span.SetAttributes(attribute.String("outcome", outcome))
		if err != nil {
			span.SetStatus(codes.Error, outcome)
		}
		span.End()
	}
}

func errorOutcome(err error) string {
	switch {
	case err == nil:
		return "success"
	case apierrors.IsConflict(err):
		return "conflict"
	case apierrors.IsForbidden(err):
		return "forbidden"
	case apierrors.IsTimeout(err) || apierrors.IsServerTimeout(err):
		return "timeout"
	default:
		return "error"
	}
}
