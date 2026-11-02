package client

import (
	"context"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	clientv1 "github.com/yourorg/ztna/controller/gen/go/proto/client/v1"
	"github.com/yourorg/ztna/controller/internal/auth"
	"github.com/yourorg/ztna/controller/internal/changehint"
)

// hintRecheckInterval is how often an open WatchChanges stream re-checks the
// device (a revoke ends it within this bound) and re-sends the current
// versions as a heartbeat, so the client can tell a dead connection apart from
// a quiet workspace.
const hintRecheckInterval = 60 * time.Second

// changeHints is the FU-8 wake-up hint wiring (ADR-018 amendment 2026-11-02).
type changeHints struct {
	hub              *changehint.Hub
	aclVersion       func(workspaceID string) uint64
	transportVersion func(workspaceID string) uint64
	shutdown         <-chan struct{}
	recheck          time.Duration
	// gate is deviceGate against the pool; replaceable in tests.
	gate func(ctx context.Context, deviceID string, claims *auth.AccessTokenClaims) (clientv1.DeviceDirective, string, error)
}

// VersionSource reports a workspace's current version (policy.Notifier and
// transport.Notifier both satisfy it).
type VersionSource interface {
	Version(workspaceID string) uint64
}

// EnableChangeHints turns on WatchChanges. hub must be registered as a change
// listener on both notifiers; shutdown closes when the controller stops, so
// open streams end at once instead of holding GracefulStop (O-8).
func (s *Service) EnableChangeHints(hub *changehint.Hub, transportVersions VersionSource, shutdown <-chan struct{}) {
	s.hints = &changeHints{
		hub:              hub,
		aclVersion:       s.policyNotifier.Version,
		transportVersion: transportVersions.Version,
		shutdown:         shutdown,
		recheck:          hintRecheckInterval,
		gate: func(ctx context.Context, deviceID string, claims *auth.AccessTokenClaims) (clientv1.DeviceDirective, string, error) {
			return deviceGate(ctx, s.pool, deviceID, claims)
		},
	}
}

// WatchChanges streams advisory wake-up hints (FU-8). It sends the
// workspace's current ACL and transport versions on open and after every
// change, plus a heartbeat each recheck interval. It never sends rules or
// topology: the client reconciles through GetACLSnapshot /
// GetTransportSnapshot, which stay authoritative. Same authentication as
// GetACLSnapshot; the stream never outlives the access token or the device's
// clean standing.
func (s *Service) WatchChanges(req *clientv1.WatchChangesRequest, stream clientv1.ClientService_WatchChangesServer) error {
	h := s.hints
	if h == nil {
		return status.Error(codes.Unimplemented, "change hints are not enabled")
	}
	if req.GetAccessToken() == "" || req.GetDeviceId() == "" {
		return status.Error(codes.InvalidArgument, "access_token and device_id are required")
	}
	claims, err := s.authSvc.VerifyAccessToken(req.GetAccessToken())
	if err != nil {
		return status.Errorf(codes.Unauthenticated, "invalid access token: %v", err)
	}
	ctx := stream.Context()
	if err := checkHintDevice(ctx, h, req.GetDeviceId(), claims); err != nil {
		return err
	}

	var expired <-chan time.Time
	if !claims.ExpiresAt.IsZero() {
		left := time.Until(claims.ExpiresAt)
		if left <= 0 {
			return status.Error(codes.Unauthenticated, "access token expired")
		}
		timer := time.NewTimer(left)
		defer timer.Stop()
		expired = timer.C
	}

	workspaceID := claims.TenantID
	// Subscribe BEFORE reading the versions: a change after the read wakes
	// the subscription, a change before it is in the first hint. Nothing in
	// between can be missed.
	sub := h.hub.Subscribe(workspaceID, req.GetDeviceId())
	defer sub.Close()

	var last *clientv1.ChangeHint
	send := func(always bool) error {
		hint := &clientv1.ChangeHint{
			AclVersion:       h.aclVersion(workspaceID),
			TransportVersion: h.transportVersion(workspaceID),
		}
		if !always && last != nil && hint.AclVersion == last.AclVersion && hint.TransportVersion == last.TransportVersion {
			return nil
		}
		last = hint
		return stream.Send(hint)
	}
	if err := send(true); err != nil {
		return err
	}

	ticker := time.NewTicker(h.recheck)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return status.FromContextError(ctx.Err()).Err()
		case <-h.shutdown:
			return status.Error(codes.Unavailable, "controller is shutting down")
		case <-sub.Replaced:
			return status.Error(codes.Aborted, "replaced by a newer stream from this device")
		case <-expired:
			return status.Error(codes.Unauthenticated, "access token expired")
		case <-ticker.C:
			if err := checkHintDevice(ctx, h, req.GetDeviceId(), claims); err != nil {
				return err
			}
			if err := send(true); err != nil {
				return err
			}
		case <-sub.C:
			// Every device revoke path notifies the policy plane, so
			// re-checking on a wake ends a revoked device's stream at once;
			// the ticker above only backs up a notify that failed. Costs one
			// indexed lookup per stream per change, less than the snapshot
			// fetch the hint is about to trigger.
			if err := checkHintDevice(ctx, h, req.GetDeviceId(), claims); err != nil {
				return err
			}
			if err := send(false); err != nil {
				return err
			}
		}
	}
}

// checkHintDevice applies GetACLSnapshot's device gate. A directive that
// takes the device's standing away (revoked, re-enroll required, or any
// future one) ends the stream; the client's poll fetches and acts on it.
// RENEW_SOON does not: the device stays fully trusted while it renews, the
// client's poll already delivers that directive, and every certificate
// spends time in that window (the client renews at the same threshold with
// ±15 min jitter), so ending the stream there would switch hints off for it.
func checkHintDevice(ctx context.Context, h *changeHints, deviceID string, claims *auth.AccessTokenClaims) error {
	directive, _, err := h.gate(ctx, deviceID, claims)
	if err != nil {
		return status.Error(codes.PermissionDenied, "device not found or does not belong to this user")
	}
	switch directive {
	case clientv1.DeviceDirective_DIRECTIVE_NONE, clientv1.DeviceDirective_DIRECTIVE_RENEW_SOON:
		return nil
	default:
		return status.Errorf(codes.FailedPrecondition, "device directive %s: poll GetACLSnapshot", directive)
	}
}
