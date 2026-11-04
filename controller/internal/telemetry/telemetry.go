// Package telemetry implements distributed tracing and W3C TraceContext propagation
// for the Zecurity platform using OpenTelemetry.
//
// Invariants (PENDING-10 Phase 3):
//  1. Tracing is opt-in: configured ONLY when OTEL_EXPORTER_OTLP_ENDPOINT is non-empty.
//  2. When disabled, uses a global no-op provider with zero background workers,
//     zero network connections, and negligible memory overhead.
//  3. Implements W3C TraceContext propagation across HTTP, gRPC unary, gRPC streaming,
//     and asynchronous outbox workers.
package telemetry

import (
	"context"
	"os"
	"strings"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

const (
	InstrumentationScope = "github.com/yourorg/ztna/controller"
	DefaultServiceName   = "zecurity-controller"
)

var (
	initOnce  sync.Once
	tracer    trace.Tracer
	isTracing bool
)

// Init initializes the global OpenTelemetry tracer and text map propagator.
//
// If OTEL_EXPORTER_OTLP_ENDPOINT is unset or empty, it initializes a no-op TracerProvider
// with zero background routines and returns an empty shutdown hook.
func Init(ctx context.Context, serviceName string) (shutdown func(context.Context) error, err error) {
	if serviceName == "" {
		serviceName = DefaultServiceName
	}

	// Always configure standard W3C TraceContext and Baggage propagators
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	endpoint := strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"))
	if endpoint == "" {
		// Opt-in tracing invariant: zero overhead when disabled
		tp := noop.NewTracerProvider()
		otel.SetTracerProvider(tp)
		tracer = tp.Tracer(InstrumentationScope)
		isTracing = false
		return func(context.Context) error { return nil }, nil
	}

	opts := []otlptracegrpc.Option{
		otlptracegrpc.WithEndpoint(endpoint),
	}

	// Support insecure plaintext endpoint in local development / testing
	if strings.EqualFold(os.Getenv("OTEL_EXPORTER_OTLP_INSECURE"), "true") ||
		strings.HasPrefix(endpoint, "localhost") ||
		strings.HasPrefix(endpoint, "127.0.0.1") {
		opts = append(opts, otlptracegrpc.WithInsecure())
	}

	exporter, err := otlptracegrpc.New(ctx, opts...)
	if err != nil {
		return nil, err
	}

	res, err := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceNameKey.String(serviceName),
			attribute.String("platform", "zecurity"),
		),
	)
	if err != nil {
		res = resource.Default()
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)

	otel.SetTracerProvider(tp)
	tracer = tp.Tracer(InstrumentationScope)
	isTracing = true

	return func(shutdownCtx context.Context) error {
		return tp.Shutdown(shutdownCtx)
	}, nil
}

// Tracer returns the standard controller tracer.
func Tracer() trace.Tracer {
	if tracer == nil {
		initOnce.Do(func() {
			if tracer == nil {
				tracer = otel.GetTracerProvider().Tracer(InstrumentationScope)
			}
		})
	}
	return tracer
}

// StartSpan starts a named span using the global tracer.
func StartSpan(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	return Tracer().Start(ctx, name, opts...)
}

// IsEnabled reports whether active OTLP exporting is configured.
func IsEnabled() bool {
	return isTracing
}

// ── Context Propagation Across Async Boundaries ─────────────────────────────

// InjectTraceContext serializes W3C trace headers from ctx into a map.
// Use this before writing asynchronous events (e.g., outbox events) to preserve
// parent trace linkage across worker boundaries.
func InjectTraceContext(ctx context.Context) map[string]string {
	carrier := make(propagation.MapCarrier)
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	return map[string]string(carrier)
}

// ExtractTraceContext restores W3C trace context from a carrier map into ctx.
// Use this in background workers (e.g., outbox consumers) to resume the parent span.
func ExtractTraceContext(ctx context.Context, carrier map[string]string) context.Context {
	if len(carrier) == 0 {
		return ctx
	}
	return otel.GetTextMapPropagator().Extract(ctx, propagation.MapCarrier(carrier))
}
