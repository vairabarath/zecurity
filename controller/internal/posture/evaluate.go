package posture

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

type PolicyNotifier interface {
	NotifyPolicyChange(ctx context.Context, workspaceID string) error
}

type Evaluator struct {
	store    *Store
	notifier PolicyNotifier
	now      func() time.Time

	// deviceLocks serializes evaluations of one device (process-local, D-02),
	// so an evaluation that read the device's approval can't store its result
	// after a later evaluation that saw the approval change (ADR-030).
	deviceLocks sync.Map // uuid.UUID -> *sync.Mutex
	// afterApprovalRead is a test hook.
	afterApprovalRead func()
}

func NewEvaluator(store *Store, notifier PolicyNotifier) *Evaluator {
	return &Evaluator{
		store:    store,
		notifier: notifier,
		now:      time.Now,
	}
}

type ProfileResult struct {
	ProfileID uuid.UUID
	Satisfied bool
	Reason    string
	Revision  int64
	ExpiresAt time.Time
}

func (e *Evaluator) EvaluateDevice(
	ctx context.Context,
	workspaceID uuid.UUID,
	deviceID uuid.UUID,
) ([]ProfileResult, error) {
	results, policyChanged, err := e.evaluateDevice(ctx, workspaceID, deviceID)
	if err != nil {
		return nil, err
	}
	if policyChanged && e.notifier != nil {
		if err := e.notifier.NotifyPolicyChange(ctx, workspaceID.String()); err != nil {
			return nil, fmt.Errorf("notify posture evaluation transition: %w", err)
		}
	}
	return results, nil
}

// ReevaluateWorkspace recomputes posture for every device that has submitted a
// report and emits at most one policy notification for the complete batch.
func (e *Evaluator) ReevaluateWorkspace(
	ctx context.Context,
	workspaceID uuid.UUID,
) error {
	deviceIDs, err := e.store.DeviceIDsWithReports(ctx, workspaceID)
	if err != nil {
		return fmt.Errorf("list devices for posture re-evaluation: %w", err)
	}
	return e.reevaluateDevices(
		ctx,
		workspaceID,
		deviceIDs,
		func(
			ctx context.Context,
			workspaceID uuid.UUID,
			deviceID uuid.UUID,
		) (bool, error) {
			_, changed, err := e.evaluateDevice(
				ctx,
				workspaceID,
				deviceID,
			)
			return changed, err
		},
	)

}

func (e *Evaluator) reevaluateDevices(
	ctx context.Context,
	workspaceID uuid.UUID,
	deviceIDs []uuid.UUID,
	evaluate func(
		context.Context,
		uuid.UUID,
		uuid.UUID,
	) (bool, error),
) error {
	var reevalErrors []error
	policyChanged := false

	for _, deviceID := range deviceIDs {
		changed, err := evaluate(ctx, workspaceID, deviceID)
		if err != nil {
			reevalErrors = append(
				reevalErrors,
				fmt.Errorf("device %s: %w", deviceID, err),
			)
			continue
		}

		if changed {
			policyChanged = true
		}
	}

	if policyChanged && e.notifier != nil {
		if err := e.notifier.NotifyPolicyChange(
			ctx,
			workspaceID.String(),
		); err != nil {
			reevalErrors = append(
				reevalErrors,
				fmt.Errorf(
					"notify posture re-evaluation transition: %w",
					err,
				),
			)
		}
	}

	if len(reevalErrors) > 0 {
		return fmt.Errorf(
			"workspace posture re-evaluation completed with %d error(s): %w",
			len(reevalErrors),
			errors.Join(reevalErrors...),
		)
	}

	return nil
}

func (e *Evaluator) evaluateDevice(
	ctx context.Context,
	workspaceID uuid.UUID,
	deviceID uuid.UUID,
) ([]ProfileResult, bool, error) {
	lock, _ := e.deviceLocks.LoadOrStore(deviceID, &sync.Mutex{})
	lock.(*sync.Mutex).Lock()
	defer lock.(*sync.Mutex).Unlock()

	report, err := e.store.LatestReport(ctx, workspaceID, deviceID)
	if err != nil {
		return nil, false, fmt.Errorf("load latest report for evaluation: %w", err)
	}

	observations, err := e.store.ListObservations(ctx, report.ID)
	if err != nil {
		return nil, false, fmt.Errorf("load observations for evaluation: %w", err)
	}

	profiles, err := e.store.ListProfiles(ctx, workspaceID)
	if err != nil {
		return nil, false, fmt.Errorf("load profiles for evaluation: %w", err)
	}

	approved, err := e.store.DeviceApproved(ctx, workspaceID, deviceID)
	if err != nil {
		return nil, false, fmt.Errorf("load device approval for evaluation: %w", err)
	}
	if e.afterApprovalRead != nil {
		e.afterApprovalRead()
	}

	// ADR-031 D-1: the platform the device enrolled as.
	platform, err := e.store.DevicePlatform(ctx, workspaceID, deviceID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, false, fmt.Errorf("load device platform for evaluation: %w", err)
	}

	// PENDING-20 G1: the country the controller last saw the device in.
	var netObs *NetworkObservation
	if o, err := e.store.LatestNetworkObservation(ctx, workspaceID, deviceID); err == nil {
		netObs = &o
	} else if !errors.Is(err, ErrNotFound) {
		return nil, false, fmt.Errorf("load network observation for evaluation: %w", err)
	}

	observationByCheck := make(map[string]Observation, len(observations)+2)
	for _, observation := range observations {
		observationByCheck[observation.CheckID] = observation
	}
	// Server-side check (ADR-030): always set here, never taken from a report.
	observationByCheck[CheckDeviceApproved] = approvalObservation(report.ID, approved)

	results := make([]ProfileResult, 0, len(profiles))
	policyChanged := false
	now := e.now()
	// Server-side check (PENDING-20): judged per requirement in EvaluateProfile.
	observationByCheck[CheckNetworkCountry] = countryObservation(report.ID, netObs, now)
	// ADR-031 D-11: the reported OS version, judged per requirement.
	osVersion := reportedOSVersion(report.OSInfo)
	for _, id := range []string{CheckWindowsMinOS, CheckAndroidMinOS, CheckIOSMinOS} {
		observationByCheck[id] = osVersionObservation(report.ID, osVersion)
	}

	for _, profile := range profiles {
		requirements, err := e.store.ListRequirements(ctx, workspaceID,
			profile.ID)
		if err != nil {
			return nil, false, fmt.Errorf(
				"load requirements for profile %s: %w",
				profile.ID,
				err,
			)
		}

		result := EvaluateProfile(
			now,
			report.ReceivedAt,
			profile,
			platform,
			requirements,
			observationByCheck,
		)

		previous, previousErr := e.store.LatestEvaluation(
			ctx,
			workspaceID,
			deviceID,
			profile.ID,
		)
		if previousErr != nil && !errors.Is(previousErr, ErrNotFound) {
			return nil, false, fmt.Errorf(
				"load previous evaluation for profile %s: %w",
				profile.ID,
				previousErr,
			)
		}

		reason := result.Reason
		if err := e.store.UpsertEvaluation(
			ctx,
			workspaceID,
			deviceID,
			profile.ID,
			profile.Revision,
			result.Satisfied,
			&reason,
			&report.ID,
		); err != nil {
			return nil, false, fmt.Errorf(
				"store evaluation for profile %s: %w",
				profile.ID,
				err,
			)
		}

		if previousErr != nil ||
			previous.Satisfied != result.Satisfied ||
			previous.ProfileRevision != profile.Revision ||
			stringValue(previous.Reason) != result.Reason {
			policyChanged = true
		}

		results = append(results, result)
	}

	return results, policyChanged, nil
}

// AllowsPlatform reports whether the profile allows devices of platform
// (ADR-031 D-1).
func (p Profile) AllowsPlatform(platform string) bool {
	for _, allowed := range p.AllowedPlatforms {
		if allowed == platform {
			return true
		}
	}
	return false
}

// EvaluateProfile decides whether a device of devicePlatform satisfies the
// profile (ADR-031):
//   - its platform must be allowed (an unknown platform never is);
//   - the report must be fresh;
//   - every requirement of its platform, and every server-observed or
//     unrecognized one (fail closed), must pass. Other platforms'
//     requirements don't apply to it.
func EvaluateProfile(
	now time.Time,
	reportReceivedAt time.Time,
	profile Profile,
	devicePlatform string,
	requirements []Requirement,
	observations map[string]Observation,
) ProfileResult {
	result := ProfileResult{
		ProfileID: profile.ID,
		Revision:  profile.Revision,
		ExpiresAt: reportReceivedAt.Add(MaxReportAge),
	}

	platform := strings.ToLower(strings.TrimSpace(devicePlatform))
	if platform == "" {
		result.Reason = "device platform unknown"
		return result
	}
	if !profile.AllowsPlatform(platform) {
		result.Reason = fmt.Sprintf("platform %s is not allowed by this profile", platform)
		return result
	}

	if now.After(result.ExpiresAt) {
		result.Reason = "posture report is stale"
		return result
	}

	for _, requirement := range requirements {
		if p := checkPlatform(requirement.CheckID); p != "" && p != "any" && p != platform {
			continue // another platform's check
		}
		observation, found := observations[requirement.CheckID]
		if !found {
			result.Reason = fmt.Sprintf(
				"missing required check %s",
				requirement.CheckID,
			)
			return result
		}

		if IsMinOSCheck(requirement.CheckID) {
			ok, reason := minOSRequirementSatisfied(requirement, observation)
			if ok {
				continue
			}
			result.Reason = reason
			return result
		}

		if requirement.CheckID == CheckNetworkCountry {
			ok, reason := countryRequirementSatisfied(requirement, observation)
			if ok {
				continue
			}
			result.Reason = reason
			return result
		}

		if requirementSatisfied(requirement, observation) {
			continue
		}

		result.Reason = requirementFailureReason(requirement, observation)
		return result
	}

	result.Satisfied = true
	result.Reason = "all requirements satisfied"
	return result
}

func requirementSatisfied(
	requirement Requirement,
	observation Observation,
) bool {
	switch observation.Status {
	case ObservationStatusPass:
		return true
	case ObservationStatusUnsupported:
		return requirement.AllowUnsupported
	default:
		return false
	}
}

func requirementFailureReason(
	requirement Requirement,
	observation Observation,
) string {
	if observation.Status == ObservationStatusUnsupported && !requirement.AllowUnsupported {
		return fmt.Sprintf(
			"check %s is unsupported",
			requirement.CheckID,
		)
	}

	detail := ""
	if observation.Detail != nil && strings.TrimSpace(*observation.Detail) != "" {
		detail = ": " + strings.TrimSpace(*observation.Detail)
	}

	return fmt.Sprintf(
		"check %s: %s%s",
		requirement.CheckID,
		observation.Status,
		detail,
	)
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func approvalObservation(reportID uuid.UUID, approved bool) Observation {
	status, detail := ObservationStatusFail, "device not approved by an admin"
	if approved {
		status, detail = ObservationStatusPass, "approved by an admin"
	}
	return Observation{
		ReportID: reportID,
		CheckID:  CheckDeviceApproved,
		Status:   status,
		Detail:   &detail,
	}
}
