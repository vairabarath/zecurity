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
	CheckOSVersion  = "linux.os.version"
	CheckLUKS       = "linux.disk_encryption.luks"
	CheckFirewall   = "linux.firewall.active"
	CheckSecureBoot = "linux.secure_boot.enabled"

	// ADR-031 D-4: Android checks, collected by the app's VPN service.
	// Identical in client/src/posture.rs.
	CheckAndroidEncryption = "android.disk_encryption.enabled"
	CheckAndroidScreenLock = "android.screen_lock.enabled"
	CheckAndroidBiometric  = "android.biometric.enrolled"

	// ADR-031 amendment 2026-11-14: Windows checks, read by the client
	// daemon (LocalSystem). Identical in client/src/posture_windows.rs.
	CheckWindowsEncryption = "windows.disk_encryption.bitlocker"
	CheckWindowsScreenLock = "windows.screen_lock.enabled"
	CheckWindowsBiometric  = "windows.biometric.enrolled"
	CheckWindowsFirewall   = "windows.firewall.enabled"
	CheckWindowsAntivirus  = "windows.antivirus.enabled"
)

// CheckDeviceApproved is a server-side check (ADR-030): PASS when an admin
// approved the device. The controller computes it from client_devices; a
// client can never report it (ValidateReport drops it like any unknown check).
const CheckDeviceApproved = "zecurity.device.approved"

var (
	ErrInvalidReport       = errors.New("invalid posture report")
	ErrNoRecognizedChecks  = errors.New("posture report contains no valid recognized checks")
	ErrDuplicateCheck      = errors.New("duplicate posture check")
	ErrInvalidReportID     = errors.New("report_id must be a canonical UUID")
	ErrInvalidReportedTime = errors.New("reported_at is outside the accepted time window")
)

var recognizedStatuses = map[ObservationStatus]struct{}{
	ObservationStatusPass:        {},
	ObservationStatusFail:        {},
	ObservationStatusUnsupported: {},
	ObservationStatusUnknown:     {},
	ObservationStatusError:       {},
}

type CheckDescriptor struct {
	ID                         string
	Label                      string
	Platform                   string
	AllowUnsupportedMeaningful bool
}

var supportedCheckDescriptors = []CheckDescriptor{
	{
		ID:                         CheckOSVersion,
		Label:                      "OS version",
		Platform:                   "linux",
		AllowUnsupportedMeaningful: true,
	},
	{
		ID:                         CheckLUKS,
		Label:                      "Disk encryption (LUKS)",
		Platform:                   "linux",
		AllowUnsupportedMeaningful: true,
	},
	{
		ID:                         CheckFirewall,
		Label:                      "Firewall active",
		Platform:                   "linux",
		AllowUnsupportedMeaningful: true,
	},
	{
		ID:                         CheckSecureBoot,
		Label:                      "Secure Boot enabled",
		Platform:                   "linux",
		AllowUnsupportedMeaningful: true,
	},
	{
		ID:       CheckAndroidEncryption,
		Label:    "HD Encryption",
		Platform: "android",
	},
	{
		ID:       CheckAndroidScreenLock,
		Label:    "Screen Lock",
		Platform: "android",
	},
	{
		// UNSUPPORTED below Android 11 (no BIOMETRIC_STRONG query).
		ID:                         CheckAndroidBiometric,
		Label:                      "Biometric",
		Platform:                   "android",
		AllowUnsupportedMeaningful: true,
	},
	{
		// UNSUPPORTED on editions without BitLocker (Windows Home).
		ID:       CheckWindowsEncryption,
		Label:    "HD Encryption",
		Platform: "windows",
	},
	{
		ID:       CheckWindowsScreenLock,
		Label:    "Screen Lock",
		Platform: "windows",
	},
	{
		// UNSUPPORTED without a biometric sensor.
		ID:                         CheckWindowsBiometric,
		Label:                      "Biometric",
		Platform:                   "windows",
		AllowUnsupportedMeaningful: true,
	},
	{
		ID:       CheckWindowsFirewall,
		Label:    "Firewall",
		Platform: "windows",
	},
	{
		ID:       CheckWindowsAntivirus,
		Label:    "Antivirus",
		Platform: "windows",
	},
}

// KnownPlatforms are the device platforms (client_devices.os) a profile can
// allow (ADR-031 D-1). Mirrors the 060_ CHECK constraint.
var KnownPlatforms = []string{"linux", "android", "windows", "ios"}

// ErrUnknownPlatform rejects a platform outside KnownPlatforms.
var ErrUnknownPlatform = errors.New("unknown device platform")

// NormalizePlatforms lower-cases, de-duplicates and orders platforms as in
// KnownPlatforms, rejecting unknown ones.
func NormalizePlatforms(platforms []string) ([]string, error) {
	want := make(map[string]struct{}, len(platforms))
	for _, p := range platforms {
		p = strings.ToLower(strings.TrimSpace(p))
		known := false
		for _, k := range KnownPlatforms {
			if p == k {
				known = true
				break
			}
		}
		if !known {
			return nil, fmt.Errorf("%w: %q", ErrUnknownPlatform, p)
		}
		want[p] = struct{}{}
	}
	out := make([]string, 0, len(want))
	for _, k := range KnownPlatforms {
		if _, ok := want[k]; ok {
			out = append(out, k)
		}
	}
	return out, nil
}

// checkPlatform is the platform a check belongs to ("linux", "android",
// "any" for server-observed checks), or "" for a check with no descriptor.
func checkPlatform(checkID string) string {
	for _, d := range supportedCheckDescriptors {
		if d.ID == checkID {
			return d.Platform
		}
	}
	for _, d := range serverCheckDescriptors {
		if d.ID == checkID {
			return d.Platform
		}
	}
	return ""
}

// serverCheckDescriptors are checks a profile may require but only the
// controller observes (ADR-030).
var serverCheckDescriptors = []CheckDescriptor{
	{
		ID:       CheckDeviceApproved,
		Label:    "Approved by an admin",
		Platform: "any",
	},
	{
		// PENDING-20 G1: needs a country rule (allow/deny lists) as params.
		ID:       CheckNetworkCountry,
		Label:    "Device country (public IP)",
		Platform: "any",
	},
}

// recognizedChecks are the checks a client may report.
var recognizedChecks = func() map[string]struct{} {
	checks := make(map[string]struct{}, len(supportedCheckDescriptors))
	for _, descriptor := range supportedCheckDescriptors {
		checks[descriptor.ID] = struct{}{}
	}
	return checks
}()

type CheckInput struct {
	CheckID string
	Status  ObservationStatus
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

// IsRecognizedCheck reports whether a profile may require checkID: a client
// check or a server-side check.
func IsRecognizedCheck(checkID string) bool {
	if _, ok := recognizedChecks[checkID]; ok {
		return true
	}
	return IsServerCheck(checkID)
}

// IsServerCheck reports whether checkID is computed by the controller.
func IsServerCheck(checkID string) bool {
	for _, d := range serverCheckDescriptors {
		if d.ID == checkID {
			return true
		}
	}
	return false
}

func SupportedChecks() []string {
	checks := make([]string, len(supportedCheckDescriptors))
	for i, descriptor := range supportedCheckDescriptors {
		checks[i] = descriptor.ID
	}
	return checks
}

func SupportedCheckDescriptors() []CheckDescriptor {
	descriptors := make([]CheckDescriptor, 0, len(supportedCheckDescriptors)+len(serverCheckDescriptors))
	descriptors = append(descriptors, supportedCheckDescriptors...)
	return append(descriptors, serverCheckDescriptors...)
}
