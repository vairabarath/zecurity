package telemetry

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type metadataCarrier metadata.MD

func (c metadataCarrier) Get(key string) string {
	values := metadata.MD(c).Get(key)
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func (c metadataCarrier) Set(key, value string) {
	metadata.MD(c).Set(key, value)
}

func (c metadataCarrier) Keys() []string {
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	return keys
}

// UnaryServerInterceptor extracts W3C trace context and starts a server span for unary RPCs.
func UnaryServerInterceptor() grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req any,
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (any, error) {
		md, ok := metadata.FromIncomingContext(ctx)
		if !ok {
			md = metadata.MD{}
		}

		ctx = otel.GetTextMapPropagator().Extract(ctx, metadataCarrier(md))
		ctx, span := Tracer().Start(
			ctx,
			info.FullMethod,
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(
				attribute.String("rpc.system", "grpc"),
				attribute.String("rpc.method", info.FullMethod),
			),
		)
		defer span.End()

		resp, err := handler(ctx, req)
		if err != nil {
			s, _ := status.FromError(err)
			span.SetStatus(codes.Error, s.Message())
			span.SetAttributes(attribute.String("rpc.grpc.status_code", s.Code().String()))
		} else {
			span.SetStatus(codes.Ok, "")
		}

		return resp, err
	}
}

// wrappedStream overrides Context() on a ServerStream to pass the span context.
type wrappedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (w *wrappedStream) Context() context.Context {
	return w.ctx
}

// StreamServerInterceptor extracts W3C trace context and starts a server span for streaming RPCs.
func StreamServerInterceptor() grpc.StreamServerInterceptor {
	return func(
		srv any,
		ss grpc.ServerStream,
		info *grpc.StreamServerInfo,
		handler grpc.StreamHandler,
	) error {
		ctx := ss.Context()
		md, ok := metadata.FromIncomingContext(ctx)
		if !ok {
			md = metadata.MD{}
		}

		ctx = otel.GetTextMapPropagator().Extract(ctx, metadataCarrier(md))
		ctx, span := Tracer().Start(
			ctx,
			info.FullMethod,
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(
				attribute.String("rpc.system", "grpc"),
				attribute.String("rpc.method", info.FullMethod),
				attribute.Bool("rpc.streaming", true),
			),
		)
		defer span.End()

		ws := &wrappedStream{
			ServerStream: ss,
			ctx:          ctx,
		}

		err := handler(srv, ws)
		if err != nil {
			s, _ := status.FromError(err)
			span.SetStatus(codes.Error, s.Message())
			span.SetAttributes(attribute.String("rpc.grpc.status_code", s.Code().String()))
		} else {
			span.SetStatus(codes.Ok, "")
		}

		return err
	}
}
