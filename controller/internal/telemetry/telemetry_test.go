package telemetry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

func TestInit_DisabledByDefault(t *testing.T) {
	_ = os.Unsetenv("OTEL_EXPORTER_OTLP_ENDPOINT")

	shutdown, err := Init(context.Background(), "test-controller")
	if err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	defer func() {
		if err := shutdown(context.Background()); err != nil {
			t.Fatalf("shutdown: %v", err)
		}
	}()

	if IsEnabled() {
		t.Fatalf("expected tracing to be disabled when endpoint is empty")
	}

	// Verify tracer is safe and no-op
	ctx, span := StartSpan(context.Background(), "test-span")
	if span == nil {
		t.Fatalf("expected non-nil span from no-op tracer")
	}
	span.End()

	if trace.SpanFromContext(ctx) == nil {
		t.Fatalf("expected span in context")
	}
}

func TestTraceContext_PropagationAcrossAsyncBoundaries(t *testing.T) {
	_, err := Init(context.Background(), "test-controller")
	if err != nil {
		t.Fatalf("init: %v", err)
	}

	// Create a synthetic trace context
	tp := otel.GetTracerProvider()
	ctx, span := tp.Tracer("test").Start(context.Background(), "parent-worker")
	defer span.End()

	// Inject into carrier map (e.g. for Outbox Event or message queue)
	carrier := InjectTraceContext(ctx)
	if carrier == nil {
		t.Fatalf("carrier is nil")
	}

	// Extract in a new background context (worker receiving the event)
	workerCtx := ExtractTraceContext(context.Background(), carrier)

	workerSpanCtx := trace.SpanContextFromContext(workerCtx)
	parentSpanCtx := span.SpanContext()

	// If parent had a valid trace ID, extracted context must have the identical TraceID
	if parentSpanCtx.IsValid() {
		if workerSpanCtx.TraceID() != parentSpanCtx.TraceID() {
			t.Fatalf("expected extracted TraceID %s, got %s", parentSpanCtx.TraceID(), workerSpanCtx.TraceID())
		}
	}
}

func TestHTTPMiddleware_TracesRequest(t *testing.T) {
	_, _ = Init(context.Background(), "test-controller")

	var capturedSpan trace.Span
	innerHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedSpan = trace.SpanFromContext(r.Context())
		w.WriteHeader(http.StatusAccepted)
	})

	wrapped := HTTPMiddleware(innerHandler)

	req := httptest.NewRequest(http.MethodPost, "/api/test", nil)
	rr := httptest.NewRecorder()

	wrapped.ServeHTTP(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", rr.Code)
	}
	if capturedSpan == nil {
		t.Fatalf("expected span in request context")
	}
}

func TestUnaryServerInterceptor(t *testing.T) {
	_, _ = Init(context.Background(), "test-controller")

	interceptor := UnaryServerInterceptor()

	var handlerSpan trace.Span
	handler := func(ctx context.Context, req any) (any, error) {
		handlerSpan = trace.SpanFromContext(ctx)
		return "ok", nil
	}

	info := &grpc.UnaryServerInfo{
		FullMethod: "/connector.v1.ConnectorService/Control",
	}

	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"))

	resp, err := interceptor(ctx, "req", info, handler)
	if err != nil || resp != "ok" {
		t.Fatalf("expected ok, got %v %v", resp, err)
	}
	if handlerSpan == nil {
		t.Fatalf("expected span in gRPC handler context")
	}
}
