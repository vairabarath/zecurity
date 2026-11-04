package telemetry

import (
	"net/http"

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

		spanName := req.Method + " " + req.URL.Path
		ctx, span := Tracer().Start(
			ctx,
			spanName,
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

		next.ServeHTTP(sw, req.WithContext(ctx))

		span.SetAttributes(attribute.Int("http.status_code", sw.statusCode))
		if sw.statusCode >= 500 {
			span.SetStatus(codes.Error, http.StatusText(sw.statusCode))
		} else {
			span.SetStatus(codes.Ok, "")
		}
	})
}
