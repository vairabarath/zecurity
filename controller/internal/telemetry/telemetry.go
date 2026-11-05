// Package telemetry implements distributed tracing and W3C TraceContext propagation
// for the Zecurity platform using OpenTelemetry.
//
// Invariants (PENDING-10 Phase 3):
//  1. Tracing is opt-in: configured ONLY when OTEL_EXPORTER_OTLP_ENDPOINT is non-empty.
//  2. When disabled, uses a global no-op provider with zero background workers,
//     zero network connections, and negligible memory overhead.
//  3. Implements W3C TraceContext propagation across HTTP, gRPC unary and gRPC
//     streaming. Outbox dispatch spans start a new trace with a span link to
//     the publishing span; the outbox stores only its traceparent.
package telemetry

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/url"
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

	opts, err := exporterOptions(endpoint, os.Getenv("OTEL_EXPORTER_OTLP_INSECURE"))
	if err != nil {
		return nil, err
	}

	exporter, err := otlptracegrpc.New(ctx, opts...)
	if err != nil {
		return nil, err
	}

	// Export failures are otherwise dropped silently by the batch processor.
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		log.Printf("telemetry: %v", err)
	}))

	res, err := newResource(serviceName)
	if err != nil {
		log.Printf("telemetry: resource: %v (exporting with service.name=%s only)", err, serviceName)
	}
	log.Printf("telemetry: tracing enabled, OTLP gRPC export to %s as service %s", endpoint, serviceName)

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

// exporterOptions accepts OTEL_EXPORTER_OTLP_ENDPOINT as a URL
// ("http://collector:4317", "https://...") or as a bare "host:port". A URL's
// scheme decides TLS (http = plaintext); a bare host:port is plaintext only for
// localhost/127.0.0.1 or when OTEL_EXPORTER_OTLP_INSECURE=true.
func exporterOptions(endpoint, insecureEnv string) ([]otlptracegrpc.Option, error) {
	insecure := strings.EqualFold(strings.TrimSpace(insecureEnv), "true")
	if strings.Contains(endpoint, "://") {
		u, err := url.Parse(endpoint)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return nil, fmt.Errorf("OTEL_EXPORTER_OTLP_ENDPOINT %q: want http(s)://host:port or host:port", endpoint)
		}
		opts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(u.Host)}
		if u.Scheme == "http" || insecure {
			opts = append(opts, otlptracegrpc.WithInsecure())
		}
		return opts, nil
	}
	opts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(endpoint)}
	host := endpoint
	if h, _, err := net.SplitHostPort(endpoint); err == nil {
		host = h
	}
	if insecure || host == "localhost" || host == "127.0.0.1" || host == "::1" {
		opts = append(opts, otlptracegrpc.WithInsecure())
	}
	return opts, nil
}

// newResource merges the SDK defaults with the service attributes. The service
// attributes are schemaless so the merge can't fail on a schema-URL mismatch
// between resource.Default() and the semconv package (which previously made the
// code fall back to resource.Default() and report "unknown_service:controller").
func newResource(serviceName string) (*resource.Resource, error) {
	own := resource.NewSchemaless(
		semconv.ServiceNameKey.String(serviceName),
		attribute.String("platform", "zecurity"),
	)
	res, err := resource.Merge(resource.Default(), own)
	if err != nil {
		return own, err
	}
	return res, nil
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
