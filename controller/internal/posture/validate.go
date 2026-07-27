package posture

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

const (
	MaxChecks          = 32
	MaxDetailBytes     = 256
	MaxMetadataBytes   = 64
	MaxReportAge       = 10 * time.Minute
	MaxFutureClockSkew = 5 * time.Minute
)

const (
	CheckOSVersion    = "linux.os.version"
	CheckLUKS         = "linux.disk_encryption.luks"
	CheckFirewall     = "linux.firewall.active"
	CheckSecureBoot   = "linux.secure_boot.enabled"
	StatusPass        = "PASS"
	StatusFail        = "FAIL"
	StatusUnsupported = "UNSUPPORTED"
	StatusUnknown     = "UNKNOWN"
	StatusError       = "ERROR"
)

var (
	ErrInvalidReport       = errors.New("invalid posture report")
	ErrNoRecognizedChecks  = errors.New("posture report contains no valid recognized checks")
	ErrDuplicateCheck      = errors.New("duplicate posture check")
	ErrInvalidReportID     = errors.New("report_id must be a canonical UUID")
	ErrInvalidReportedTime = errors.New("reported_at is outside the accepted time window")
)

var recognizedChecks = map[string]struct{}{
	CheckOSVersion:  {},
	CheckLUKS:       {},
	CheckFirewall:   {},
	CheckSecureBoot: {},
}

var recognizedStatuses = map[string]struct{}{
	StatusPass:        {},
	StatusFail:        {},
	StatusUnsupported: {},
	StatusUnknown:     {},
	StatusError:       {},
}

type CheckInput struct {
	CheckID string
	Status  string
	Detail  string
}

type ReportInput struct {
	ReportID      string
	ReportedAt    time.Time
	ClientVersion string
	OSName        string
	OSVersion     string
	Checks        []CheckInput
}

type osInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// ValidateReport validates report-level fields and converts valid, recognized
// checks into the persistence representation.
//
// Unknown check IDs and oversized/invalid per-check details are filtered
// individually. Duplicate check IDs always reject the whole new report.
func ValidateReport(now time.Time, input ReportInput) (Report, error) {
	reportUUID, err := uuid.Parse(input.ReportID)
	if err != nil || reportUUID.String() != input.ReportID {
		return Report{}, ErrInvalidReportID
	}

	if input.ReportedAt.Before(now.Add(-MaxReportAge)) ||
		input.ReportedAt.After(now.Add(MaxFutureClockSkew)) {
		return Report{}, ErrInvalidReportedTime
	}

	if len(input.Checks) == 0 {
		return Report{}, ErrNoRecognizedChecks
	}
	if len(input.Checks) > MaxChecks {
		return Report{}, fmt.Errorf(
			"%w: too many checks: got %d, maximum %d",
			ErrInvalidReport,
			len(input.Checks),
			MaxChecks,
		)
	}

	for name, value := range map[string]string{
		"client_version": input.ClientVersion,
		"os_name":        input.OSName,
		"os_version":     input.OSVersion,
	} {
		if !utf8.ValidString(value) {
			return Report{}, fmt.Errorf("%w: %s is not valid UTF-8",
				ErrInvalidReport,
				name)
		}
		if len([]byte(value)) > MaxMetadataBytes {
			return Report{}, fmt.Errorf(
				"%w: %s exceeds %d bytes",
				ErrInvalidReport,
				name,
				MaxMetadataBytes,
			)
		}
	}

	seen := make(map[string]struct{}, len(input.Checks))
	observations := make([]Observation, 0, len(input.Checks))

	for _, check := range input.Checks {
		checkID := strings.TrimSpace(check.CheckID)
		if checkID == "" {
			return Report{}, fmt.Errorf("%w: empty check_id", ErrInvalidReport)
		}

		if _, duplicate := seen[checkID]; duplicate {
			return Report{}, fmt.Errorf("%w: %s", ErrDuplicateCheck,
				checkID)
		}
		seen[checkID] = struct{}{}

		if _, known := recognizedChecks[checkID]; !known {
			continue
		}

		if _, valid := recognizedStatuses[check.Status]; !valid {
			continue
		}

		if !utf8.ValidString(check.Detail) {
			continue
		}
		if len([]byte(check.Detail)) > MaxDetailBytes {
			continue
		}

		var detail *string
		if check.Detail != "" {
			value := check.Detail
			detail = &value
		}

		observations = append(observations, Observation{
			CheckID: checkID,
			Status:  check.Status,
			Detail:  detail,
		})
	}

	if len(observations) == 0 {
		return Report{}, ErrNoRecognizedChecks
	}

	encodedOSInfo, err := json.Marshal(osInfo{
		Name:    input.OSName,
		Version: input.OSVersion,
	})
	if err != nil {
		return Report{}, fmt.Errorf("encode posture OS information: %w",
			err)
	}

	return Report{
		ReportID:      input.ReportID,
		ClientVersion: input.ClientVersion,
		OSInfo:        encodedOSInfo,
		ReportedAt:    input.ReportedAt.UTC(),
		Observations:  observations,
	}, nil
}

func IsRecognizedCheck(checkID string) bool {
	_, ok := recognizedChecks[checkID]
	return ok
}
