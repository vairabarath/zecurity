package client

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	clientv1 "github.com/yourorg/ztna/controller/gen/go/proto/client/v1"
	"github.com/yourorg/ztna/controller/internal/posture"
)

func (s *Service) ReportDevicePosture(
	ctx context.Context,
	req *clientv1.ReportDevicePostureRequest,
) (*clientv1.ReportDevicePostureResponse, error) {
	if req == nil || req.GetAccessToken() == "" || req.GetDeviceId() == "" || req.GetReport() == nil {
		return nil, status.Error(codes.InvalidArgument, "access_token, device_id, and report are required")
	}

	claims, err := s.authSvc.VerifyAccessToken(req.GetAccessToken())
	if err != nil {
		return nil, status.Errorf(codes.Unauthenticated, "invalid access token: %v", err)
	}

	deviceID, err := parseCanonicalUUID(req.GetDeviceId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "device_id must be a canonical UUID")
	}
	workspaceID, err := uuid.Parse(claims.TenantID)
	if err != nil {
		return nil, status.Error(codes.Internal, "token contains an invalid workspace identifier")
	}
	if err := s.verifyPostureDevice(ctx, deviceID, workspaceID, claims.UserID); err != nil {
		return nil, err
	}

	wireReport := req.GetReport()
	if _, err := parseCanonicalUUID(wireReport.GetReportId()); err != nil {
		return nil, status.Error(codes.InvalidArgument, "report_id must be a canonical UUID")
	}

	// A retry of an accepted report remains idempotent after its timestamp ages
	// out. Evaluation is retried so a prior post-insert failure cannot leave the
	// report permanently unevaluated.
	existing, err := s.postureStore.ReportByClientID(ctx, wireReport.GetReportId())
	switch {
	case err == nil:
		return s.acceptExistingPostureReport(ctx, existing, workspaceID, deviceID)
	case errors.Is(err, posture.ErrNotFound):
		// New report: continue.
	default:
		return nil, status.Errorf(codes.Internal, "check report idempotency: %v", err)
	}

	checks := make([]posture.CheckInput, 0, len(wireReport.GetChecks()))
	for _, check := range wireReport.GetChecks() {
		checks = append(checks, posture.CheckInput{
			CheckID: check.GetCheckId(),
			Status:  postureStatusString(check.GetStatus()),
			Detail:  check.GetDetail(),
		})
	}

	validated, err := posture.ValidateReport(time.Now(), posture.ReportInput{
		ReportID:      wireReport.GetReportId(),
		ReportedAt:    time.Unix(wireReport.GetReportedAt(), 0).UTC(),
		ClientVersion: wireReport.GetClientVersion(),
		OSName:        wireReport.GetOsName(),
		OSVersion:     wireReport.GetOsVersion(),
		Checks:        checks,
	})
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	err = s.postureStore.InsertReport(ctx, workspaceID, deviceID, validated)
	switch {
	case err == nil:
		// Continue to evaluation.
	case errors.Is(err, posture.ErrDuplicateReport):
		existing, lookupErr := s.postureStore.ReportByClientID(ctx, wireReport.GetReportId())
		if lookupErr != nil {
			return nil, status.Errorf(codes.Internal, "reload duplicate posture report: %v", lookupErr)
		}
		return s.acceptExistingPostureReport(ctx, existing, workspaceID, deviceID)
	case errors.Is(err, posture.ErrWorkspaceMismatch):
		return nil, status.Error(codes.PermissionDenied, "device is revoked or belongs to another workspace")
	default:
		return nil, status.Errorf(codes.Internal, "store posture report: %v", err)
	}

	return s.evaluateAndAcceptPosture(ctx, workspaceID, deviceID)
}

func (s *Service) verifyPostureDevice(
	ctx context.Context,
	deviceID uuid.UUID,
	workspaceID uuid.UUID,
	userID string,
) error {
	var (
		deviceWorkspaceID uuid.UUID
		revokedAt         *time.Time
	)
	err := s.pool.QueryRow(ctx,
		`SELECT workspace_id, revoked_at
		   FROM client_devices
		  WHERE id = $1 AND user_id = $2`,
		deviceID, userID,
	).Scan(&deviceWorkspaceID, &revokedAt)

	if errors.Is(err, pgx.ErrNoRows) {
		return status.Error(codes.PermissionDenied, "device not found or does not belong to this user")
	}
	if err != nil {
		return status.Errorf(codes.Internal, "verify device ownership: %v", err)
	}
	if deviceWorkspaceID != workspaceID {
		return status.Error(codes.PermissionDenied, "device not found or does not belong to this user")
	}
	if revokedAt != nil {
		return status.Error(codes.PermissionDenied, "device has been revoked")
	}
	return nil
}

func (s *Service) acceptExistingPostureReport(
	ctx context.Context,
	existing *posture.Report,
	workspaceID uuid.UUID,
	deviceID uuid.UUID,
) (*clientv1.ReportDevicePostureResponse, error) {
	if existing.DeviceID != deviceID || existing.WorkspaceID != workspaceID {
		return nil, status.Error(codes.PermissionDenied, "report_id is already associated with another device")
	}
	return s.evaluateAndAcceptPosture(ctx, workspaceID, deviceID)
}

func (s *Service) evaluateAndAcceptPosture(
	ctx context.Context,
	workspaceID uuid.UUID,
	deviceID uuid.UUID,
) (*clientv1.ReportDevicePostureResponse, error) {
	if _, err := s.postureEvaluator.EvaluateDevice(ctx, workspaceID, deviceID); err != nil {
		return nil, status.Errorf(codes.Internal, "evaluate device posture: %v", err)
	}
	return &clientv1.ReportDevicePostureResponse{Accepted: true}, nil
}

func parseCanonicalUUID(value string) (uuid.UUID, error) {
	parsed, err := uuid.Parse(value)
	if err != nil || parsed.String() != value {
		return uuid.Nil, fmt.Errorf("not a canonical UUID")
	}
	return parsed, nil
}

func postureStatusString(value clientv1.CheckStatus) string {
	switch value {
	case clientv1.CheckStatus_PASS:
		return posture.StatusPass
	case clientv1.CheckStatus_FAIL:
		return posture.StatusFail
	case clientv1.CheckStatus_UNSUPPORTED:
		return posture.StatusUnsupported
	case clientv1.CheckStatus_UNKNOWN:
		return posture.StatusUnknown
	case clientv1.CheckStatus_ERROR:
		return posture.StatusError
	default:
		return fmt.Sprintf("INVALID_%d", value)
	}
}
