package client

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	clientv1 "github.com/yourorg/ztna/controller/gen/go/proto/client/v1"
	"github.com/yourorg/ztna/controller/internal/auth"
)

// fakeSessionAuth implements only what RefreshSession / Logout use; any
// other auth.Service method would panic on the nil embedded interface.
type fakeSessionAuth struct {
	auth.Service
	refreshErr            error
	gotAccess, gotRefresh string
	refreshCalls          int
	ended                 []string
}

func (f *fakeSessionAuth) RefreshSession(_ context.Context, access, refresh string) (string, string, error) {
	f.refreshCalls++
	f.gotAccess, f.gotRefresh = access, refresh
	if f.refreshErr != nil {
		return "", "", f.refreshErr
	}
	return "new-access", "new-refresh", nil
}

func (f *fakeSessionAuth) EndSession(_ context.Context, access string) {
	f.ended = append(f.ended, access)
}

func TestRefreshSessionRPC(t *testing.T) {
	ctx := context.Background()
	req := &clientv1.RefreshSessionRequest{AccessToken: "old-access", RefreshToken: "old-refresh"}

	f := &fakeSessionAuth{}
	resp, err := (&Service{authSvc: f}).RefreshSession(ctx, req)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if resp.GetAccessToken() != "new-access" || resp.GetRefreshToken() != "new-refresh" {
		t.Fatalf("unexpected response: %+v", resp)
	}
	if f.gotAccess != "old-access" || f.gotRefresh != "old-refresh" {
		t.Fatalf("auth got access=%q refresh=%q", f.gotAccess, f.gotRefresh)
	}

	for _, tc := range []struct {
		name string
		err  error
		code codes.Code
	}{
		{"dead session", &auth.SessionError{Status: http.StatusUnauthorized, Msg: "refresh token mismatch"}, codes.Unauthenticated},
		{"server error", &auth.SessionError{Status: http.StatusInternalServerError, Msg: "server error"}, codes.Internal},
		{"unexpected error", errors.New("boom"), codes.Internal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := (&Service{authSvc: &fakeSessionAuth{refreshErr: tc.err}}).RefreshSession(ctx, req)
			if status.Code(err) != tc.code {
				t.Fatalf("expected %v, got %v", tc.code, err)
			}
		})
	}

	for _, missing := range []*clientv1.RefreshSessionRequest{
		{AccessToken: "a"}, {RefreshToken: "r"}, {},
	} {
		f := &fakeSessionAuth{}
		_, err := (&Service{authSvc: f}).RefreshSession(ctx, missing)
		if status.Code(err) != codes.Unauthenticated || f.refreshCalls != 0 {
			t.Fatalf("missing tokens %+v: code=%v calls=%d", missing, status.Code(err), f.refreshCalls)
		}
	}
}

func TestLogoutRPC(t *testing.T) {
	f := &fakeSessionAuth{}
	svc := &Service{authSvc: f}
	for _, token := range []string{"access-1", ""} {
		if _, err := svc.Logout(context.Background(), &clientv1.LogoutRequest{AccessToken: token}); err != nil {
			t.Fatalf("logout(%q): %v", token, err)
		}
	}
	if len(f.ended) != 2 || f.ended[0] != "access-1" {
		t.Fatalf("EndSession calls: %v", f.ended)
	}
}
