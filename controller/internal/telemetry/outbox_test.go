package telemetry

import (
	"context"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

func TestCaptureTraceparent(t *testing.T) {
	restore := UseTracerProviderForTest(sdktrace.NewTracerProvider())
	defer restore()

	if got := CaptureTraceparent(context.Background()); got != nil {
		t.Fatalf("no span: got %q, want nil", *got)
	}

	ctx, span := StartSpan(context.Background(), "op")
	defer span.End()
	// A client-supplied tracestate must not be persisted.
	ts, _ := trace.ParseTraceState("vendor=secret")
	ctx = trace.ContextWithSpanContext(ctx, span.SpanContext().WithTraceState(ts))

	got := CaptureTraceparent(ctx)
	sc := span.SpanContext()
	want := "00-" + sc.TraceID().String() + "-" + sc.SpanID().String() + "-01"
	if got == nil || *got != want {
		t.Fatalf("got %v, want %s", got, want)
	}

	link, ok := LinkFromTraceparent(got)
	if !ok || link.SpanContext.TraceID() != sc.TraceID() || link.SpanContext.SpanID() != sc.SpanID() {
		t.Fatalf("round trip: ok=%v link=%v", ok, link.SpanContext)
	}
	if link.SpanContext.TraceState().Len() != 0 {
		t.Fatalf("link carries tracestate %q", link.SpanContext.TraceState())
	}
}

// With tracing disabled, a remote (client-supplied) span context in ctx is not
// captured.
func TestCaptureTraceparent_DisabledStoresNothing(t *testing.T) {
	prev := isTracing
	isTracing = false
	defer func() { isTracing = prev }()

	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{1},
		SpanID:     trace.SpanID{1},
		TraceFlags: trace.FlagsSampled,
		Remote:     true,
	})
	ctx := trace.ContextWithSpanContext(context.Background(), sc)
	if got := CaptureTraceparent(ctx); got != nil {
		t.Fatalf("got %q, want nil", *got)
	}
}

func TestLinkFromTraceparent_Invalid(t *testing.T) {
	for _, s := range []string{"", "garbage", "00-00000000000000000000000000000000-0000000000000000-01"} {
		s := s
		if _, ok := LinkFromTraceparent(&s); ok {
			t.Fatalf("%q accepted", s)
		}
	}
	if _, ok := LinkFromTraceparent(nil); ok {
		t.Fatal("nil accepted")
	}
}
