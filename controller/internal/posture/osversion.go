package posture

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ADR-031 D-11: "Minimum OS Version", one server-side check per platform.
// The controller compares the OS version of the device's latest posture
// report with the requirement's minimum (params.min_version). A client can
// never report these checks (they are not in recognizedChecks).
const (
	CheckWindowsMinOS = "windows.os.minimum"
	CheckAndroidMinOS = "android.os.minimum"
	CheckIOSMinOS     = "ios.os.minimum"
)

// ErrInvalidMinVersion is wrapped by every minimum-version validation error;
// the message after it is safe to show an admin.
var ErrInvalidMinVersion = errors.New("invalid minimum OS version")

// maxVersionParts caps a version at four numbers (Windows: 10.0.26100.1742).
const maxVersionParts = 4

// IsMinOSCheck reports whether checkID is a Minimum OS Version check.
func IsMinOSCheck(checkID string) bool {
	switch checkID {
	case CheckWindowsMinOS, CheckAndroidMinOS, CheckIOSMinOS:
		return true
	}
	return false
}

// parseVersion reads "N[.N[.N[.N]]]" (each part 1–9 digits).
func parseVersion(s string) ([]uint64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, false
	}
	parts := strings.Split(s, ".")
	if len(parts) > maxVersionParts {
		return nil, false
	}
	out := make([]uint64, len(parts))
	for i, p := range parts {
		if p == "" || len(p) > 9 {
			return nil, false
		}
		n, err := strconv.ParseUint(p, 10, 64)
		if err != nil {
			return nil, false
		}
		out[i] = n
	}
	return out, true
}

// NormalizeMinVersion validates an admin-entered minimum version and
// returns it trimmed.
func NormalizeMinVersion(s string) (string, error) {
	s = strings.TrimSpace(s)
	if _, ok := parseVersion(s); !ok {
		return "", fmt.Errorf("%w: %q must be numbers separated by dots, at most %d of them (for example 10.0.22631 or 14)",
			ErrInvalidMinVersion, s, maxVersionParts)
	}
	return s, nil
}

// compareVersions returns -1, 0 or 1; missing parts count as 0 (14 == 14.0).
func compareVersions(a, b []uint64) int {
	for i := 0; i < len(a) || i < len(b); i++ {
		var x, y uint64
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

type minVersionParams struct {
	MinVersion string `json:"min_version"`
}

func minVersionFromParams(raw []byte) (string, error) {
	var p minVersionParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", fmt.Errorf("decode minimum OS version: %w", err)
	}
	return p.MinVersion, nil
}

// reportedOSVersion is the version in a report's os_info ("" when missing).
func reportedOSVersion(raw json.RawMessage) string {
	var info osInfo
	if len(raw) == 0 || json.Unmarshal(raw, &info) != nil {
		return ""
	}
	return strings.TrimSpace(info.Version)
}

// osVersionObservation carries the reported OS version (in Detail) to
// EvaluateProfile, which judges it per requirement (like the country check).
func osVersionObservation(reportID uuid.UUID, version string) Observation {
	v := version
	return Observation{ReportID: reportID, Status: ObservationStatusPass, Detail: &v}
}

// minOSRequirementSatisfied: the reported version must parse and be at
// least the minimum. An unknown or unparsable version never passes.
func minOSRequirementSatisfied(requirement Requirement, observation Observation) (bool, string) {
	minimum, ok := parseVersion(requirement.MinVersion)
	if !ok {
		return false, fmt.Sprintf("check %s has no valid minimum version", requirement.CheckID)
	}
	reported := ""
	if observation.Detail != nil {
		reported = strings.TrimSpace(*observation.Detail)
	}
	have, ok := parseVersion(reported)
	if !ok {
		return false, fmt.Sprintf("check %s failed: OS version %q is unknown", requirement.CheckID, reported)
	}
	if compareVersions(have, minimum) < 0 {
		return false, fmt.Sprintf("check %s failed: OS version %s is older than %s", requirement.CheckID, reported, requirement.MinVersion)
	}
	return true, ""
}

// UpdateMinVersion replaces the minimum of a profile's Minimum OS Version
// requirement in place (no window without the check) and bumps the
// profile's revision. It returns the previous minimum; ErrNotFound when the
// profile has no such requirement.
func (s *Store) UpdateMinVersion(ctx context.Context, workspaceID, profileID uuid.UUID, checkID, minVersion string) (string, error) {
	if !IsMinOSCheck(checkID) {
		return "", ErrNotFound
	}
	params, err := json.Marshal(minVersionParams{MinVersion: minVersion})
	if err != nil {
		return "", fmt.Errorf("encode minimum OS version: %w", err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var previous []byte
	err = tx.QueryRow(ctx,
		`SELECT r.params
		   FROM device_profile_requirements r
		   JOIN device_profiles p ON p.id = r.profile_id
		  WHERE p.workspace_id = $1 AND p.id = $2 AND r.check_id = $3
		    FOR UPDATE OF r, p`,
		workspaceID, profileID, checkID,
	).Scan(&previous)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("lock minimum OS version requirement: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE device_profile_requirements SET params = $3
		  WHERE profile_id = $1 AND check_id = $2`,
		profileID, checkID, params,
	); err != nil {
		return "", fmt.Errorf("update minimum OS version: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE device_profiles SET revision = revision + 1, updated_at = NOW()
		  WHERE workspace_id = $1 AND id = $2`,
		workspaceID, profileID,
	); err != nil {
		return "", fmt.Errorf("update profile revision: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("commit minimum OS version: %w", err)
	}
	old, _ := minVersionFromParams(previous)
	return old, nil
}
