package connector

import (
	"context"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type bareStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *bareStream) Context() context.Context { return s.ctx }

// FU-8: the client's WatchChanges stream has no SPIFFE certificate, like the
// client's unary calls; the handler authenticates it (JWT + device). Every
// other stream still needs a verified peer certificate.
func TestStreamSPIFFEInterceptor_ClientServiceStreamsSkipCertificateCheck(t *testing.T) {
	icpt := StreamSPIFFEInterceptor(nil, nil, nil)
	called := false
	handler := func(any, grpc.ServerStream) error { called = true; return nil }
	stream := &bareStream{ctx: context.Background()} // no peer, no certificate

	err := icpt(nil, stream, &grpc.StreamServerInfo{FullMethod: "/client.v1.ClientService/WatchChanges"}, handler)
	if err != nil || !called {
		t.Fatalf("client stream: err=%v called=%v, want handler reached", err, called)
	}

	called = false
	err = icpt(nil, stream, &grpc.StreamServerInfo{FullMethod: "/connector.v1.ConnectorService/Control"}, handler)
	if status.Code(err) != codes.Unauthenticated || called {
		t.Fatalf("connector stream without certificate: err=%v called=%v, want Unauthenticated", err, called)
	}
}
