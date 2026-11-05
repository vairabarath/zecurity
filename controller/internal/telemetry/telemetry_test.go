package telemetry

import (
	"context"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

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

// The service attributes must survive the merge with resource.Default(); a
// schema-URL conflict used to drop them ("unknown_service:controller").
func TestNewResourceKeepsServiceName(t *testing.T) {
	res, err := newResource("zecurity-controller")
	if err != nil {
		t.Fatalf("newResource: %v", err)
	}
	got, ok := res.Set().Value(semconv.ServiceNameKey)
	if !ok || got.AsString() != "zecurity-controller" {
		t.Fatalf("service.name = %q (present %v), want zecurity-controller", got.AsString(), ok)
	}
}

func TestExporterOptionsEndpointForms(t *testing.T) {
	cases := []struct {
		endpoint, insecureEnv string
		wantOpts              int // WithEndpoint (+ WithInsecure)
		wantErr               bool
	}{
		{"localhost:4317", "", 2, false},
		{"127.0.0.1:4317", "", 2, false},
		{"collector.internal:4317", "", 1, false},
		{"collector.internal:4317", "true", 2, false},
		{"http://localhost:4317", "", 2, false},
		{"http://collector.internal:4317", "", 2, false},
		{"https://collector.internal:4317", "", 1, false},
		{"ftp://collector:4317", "", 0, true},
		{"http://", "", 0, true},
	}
	for _, c := range cases {
		opts, err := exporterOptions(c.endpoint, c.insecureEnv)
		if (err != nil) != c.wantErr {
			t.Fatalf("%s: err = %v, wantErr %v", c.endpoint, err, c.wantErr)
		}
		if len(opts) != c.wantOpts {
			t.Fatalf("%s insecure=%q: %d options, want %d", c.endpoint, c.insecureEnv, len(opts), c.wantOpts)
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

// Span names use the ServeMux route pattern, never the raw path (IDs).
func TestHTTPMiddleware_SpanNamedByRoute(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	prev := tracer
	tracer = tp.Tracer(InstrumentationScope)
	defer func() { tracer = prev }()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /provider/relays/{id}", func(w http.ResponseWriter, r *http.Request) {})
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {})
	h := HTTPMiddleware(mux)

	for _, c := range []struct{ method, target string }{
		{http.MethodGet, "/provider/relays/7f9c2a11"},
		{http.MethodPost, "/graphql"},
		{http.MethodGet, "/nope/0b11d3"},
	} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(c.method, c.target, nil))
	}

	var names []string
	for _, s := range rec.Ended() {
		names = append(names, s.Name())
	}
	want := []string{"GET /provider/relays/{id}", "POST /graphql", "GET unmatched"}
	if strings.Join(names, "|") != strings.Join(want, "|") {
		t.Fatalf("span names = %v, want %v", names, want)
	}
}
