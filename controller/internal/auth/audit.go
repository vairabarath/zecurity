package auth

import (
	"context"

	"github.com/yourorg/ztna/controller/internal/audit"
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
