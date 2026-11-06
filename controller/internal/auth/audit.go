package auth

import (
	"context"
	"errors"

	"github.com/yourorg/ztna/controller/internal/audit"
	"github.com/yourorg/ztna/controller/internal/identity"
)

// AuditRecorder writes one admin-stream audit row (PENDING-11 P-9). main.go
// wires it to audit.Record; nil disables auth auditing (unit tests).
type AuditRecorder func(ctx context.Context, e audit.Entry)

type clientChannelKey struct{}

// WithClientChannel marks ctx as a Rust-client call (ClientService), so the
// shared session code records `client: true` (S-11).
func WithClientChannel(ctx context.Context) context.Context {
	return context.WithValue(ctx, clientChannelKey{}, true)
}

// IsClientChannel reports whether ctx was marked by WithClientChannel.
func IsClientChannel(ctx context.Context) bool {
	v, _ := ctx.Value(clientChannelKey{}).(bool)
	return v
}

// Auth audit actions (S-11: console and client, distinguished by details.client).
const (
	ActionSignIn       = "auth.sign_in"
	ActionSignInFailed = "auth.sign_in_failed"
	ActionSignOut      = "auth.sign_out"
)

// SignInFailedReasonNotPermitted covers "not invited" and "account not
// active" alike, so the audit row never tells the two apart.
const SignInFailedReasonNotPermitted = "not_permitted"

func (s *serviceImpl) record(ctx context.Context, e audit.Entry) {
	if s.cfg.Audit == nil || e.TenantID == "" {
		return
	}
	// The response is already decided; don't let a cancelled request drop
	// the row.
	s.cfg.Audit(context.WithoutCancel(ctx), e)
}

// AuditSignInFailure reports whether a refused sign-in is written to the
// workspace's admin stream (PENDING-11 P-9, note §7): only when the refusal is
// "not permitted" (not invited, or account not active; notInvited lists the
// caller's own not-invited errors) AND the connection is the workspace's own
// enterprise connection. On a platform connection any IdP account could name
// any workspace and append to its kept-forever stream.
func AuditSignInFailure(connTenantID *string, workspaceID string, err error, notInvited ...error) bool {
	if connTenantID == nil || workspaceID == "" || *connTenantID != workspaceID || err == nil {
		return false
	}
	if errors.Is(err, identity.ErrNotInvited) || errors.Is(err, identity.ErrUserNotActive) {
		return true
	}
	for _, e := range notInvited {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}
