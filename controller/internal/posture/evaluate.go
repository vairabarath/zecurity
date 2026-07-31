package posture

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
	Mode      string
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

	observationByCheck := make(map[string]Observation, len(observations))
	for _, observation := range observations {
		observationByCheck[observation.CheckID] = observation
	}

	results := make([]ProfileResult, 0, len(profiles))
	policyChanged := false
	now := e.now()

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

func EvaluateProfile(
	now time.Time,
	reportReceivedAt time.Time,
	profile Profile,
	requirements []Requirement,
	observations map[string]Observation,
) ProfileResult {
	result := ProfileResult{
		ProfileID: profile.ID,
		Mode:      profile.Mode,
		Revision:  profile.Revision,
		ExpiresAt: reportReceivedAt.Add(MaxReportAge),
	}

	if len(requirements) == 0 {
		result.Reason = "profile has no requirements"
		return result
	}

	if now.After(result.ExpiresAt) {
		result.Reason = "posture report is stale"
		return result
	}

	for _, requirement := range requirements {
		observation, found := observations[requirement.CheckID]
		if !found {
			result.Reason = fmt.Sprintf(
				"missing required check %s",
				requirement.CheckID,
			)
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

func ResourceSatisfied(results []ProfileResult) bool {
	hasEnforceProfile := false

	for _, result := range results {
		if result.Mode != ModeEnforce {
			continue
		}

		hasEnforceProfile = true
		if result.Satisfied {
			return true
		}
	}

	// Identity-only behavior applies when no enforce-mode profile exists.
	return !hasEnforceProfile
}

func requirementSatisfied(
	requirement Requirement,
	observation Observation,
) bool {
	switch observation.Status {
	case StatusPass:
		return true
	case StatusUnsupported:
		return requirement.AllowUnsupported
	default:
		return false
	}
}

func requirementFailureReason(
	requirement Requirement,
	observation Observation,
) string {
	if observation.Status == StatusUnsupported && !requirement.AllowUnsupported {
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
