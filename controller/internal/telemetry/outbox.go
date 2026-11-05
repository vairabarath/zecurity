package telemetry

import (
	"context"

	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// traceparentOnly is used for outbox persistence instead of the global
// composite propagator: baggage and tracestate are client-supplied request data
// and are never written to the database.
var traceparentOnly = propagation.TraceContext{}

// CaptureTraceparent returns the W3C traceparent of the span in ctx, for storing
// with a durable outbox event. It returns nil when tracing is disabled (so no
// client-supplied trace IDs are persisted and the disabled path stays free) or
// when ctx carries no valid span context.
func CaptureTraceparent(ctx context.Context) *string {
	if !isTracing {
		return nil
	}
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return nil
	}
	// Inject only the IDs and flags: drop tracestate before injecting.
	sc = sc.WithTraceState(trace.TraceState{})
	carrier := propagation.MapCarrier{}
	traceparentOnly.Inject(trace.ContextWithSpanContext(context.Background(), sc), carrier)
	tp := carrier.Get("traceparent")
	if tp == "" {
		return nil
	}
	return &tp
}

// LinkFromTraceparent parses a stored traceparent into a span link. ok is false
// when traceparent is nil or not a valid W3C traceparent.
//
// Outbox dispatch spans link to the publishing span rather than use it as their
// parent: an event can be retried with backoff for hours, long after the
// publishing request's trace has ended.
func LinkFromTraceparent(traceparent *string) (link trace.Link, ok bool) {
	if traceparent == nil || *traceparent == "" {
		return trace.Link{}, false
	}
	ctx := traceparentOnly.Extract(context.Background(),
		propagation.MapCarrier{"traceparent": *traceparent})
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return trace.Link{}, false
	}
	return trace.Link{SpanContext: sc}, true
}

// UseTracerProviderForTest installs tp as the controller tracer with tracing
// marked enabled, and returns a function that restores the previous state.
// Tests only; not safe for concurrent use.
func UseTracerProviderForTest(tp trace.TracerProvider) (restore func()) {
	prevTracer, prevTracing := tracer, isTracing
	tracer = tp.Tracer(InstrumentationScope)
	isTracing = true
	return func() { tracer, isTracing = prevTracer, prevTracing }
}
