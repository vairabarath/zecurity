package telemetry

import (
	"net/http"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

type statusResponseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (w *statusResponseWriter) WriteHeader(code int) {
	w.statusCode = code
	w.ResponseWriter.WriteHeader(code)
}

// HTTPMiddleware traces incoming HTTP requests and propagates W3C TraceContext.
func HTTPMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		ctx := otel.GetTextMapPropagator().Extract(req.Context(), propagation.HeaderCarrier(req.Header))

		ctx, span := Tracer().Start(
			ctx,
			req.Method,
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(
				attribute.String("http.method", req.Method),
				attribute.String("http.target", req.URL.Path),
				attribute.String("http.host", req.Host),
			),
		)
		defer span.End()

		sw := &statusResponseWriter{
			ResponseWriter: w,
			statusCode:     http.StatusOK,
		}

		inner := req.WithContext(ctx)
		next.ServeHTTP(sw, inner)

		// The ServeMux sets Pattern on the request it receives (this same
		// pointer, as long as the handlers in between pass it through). Name the
		// span by route, not by raw path, which carries IDs.
		route := inner.Pattern
		if route == "" {
			route = "unmatched"
		}
		if strings.HasPrefix(route, req.Method+" ") {
			span.SetName(route)
		} else {
			span.SetName(req.Method + " " + route)
		}
		span.SetAttributes(
			attribute.String("http.route", route),
			attribute.Int("http.status_code", sw.statusCode),
		)
		if sw.statusCode >= 500 {
			span.SetStatus(codes.Error, http.StatusText(sw.statusCode))
		} else {
			span.SetStatus(codes.Ok, "")
		}
	})
}
