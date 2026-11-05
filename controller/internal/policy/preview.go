package policy

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/yourorg/ztna/controller/internal/posture"
)

// PolicyPreview is what a resource-policy change would do to the devices that
// can reach the affected resources today through their groups (PENDING-16
// D-3: the preview replaces audit mode).
type PolicyPreview struct {
	// Devices: active devices of the resources' groups (each counted once).
	Devices int
	// Passing: those that would be allowed with the proposed profiles.
	Passing int
}

// PreviewResourcePolicy counts, for the given resources, how many of the
// devices their groups allow today would pass the proposed device profiles.
// It uses the compiler's own decision (applyPosture), so it can't drift from
// what is enforced. No profiles: every device passes. Unknown profile ids are
// an error; the caller passes only ids of the workspace.
func PreviewResourcePolicy(
	ctx context.Context,
	store *Store,
	postureStore *posture.Store,
	workspaceID string,
	resourceIDs []uuid.UUID,
	profileIDs []uuid.UUID,
) (*PolicyPreview, error) {
	workspaceUUID, err := uuid.Parse(workspaceID)
	if err != nil {
		return nil, fmt.Errorf("preview policy: invalid workspace id: %w", err)
	}
	wanted := make(map[string]struct{}, len(resourceIDs))
	for _, id := range resourceIDs {
		wanted[id.String()] = struct{}{}
	}

	rules, err := store.ListEnabledRulesWithResources(ctx, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("preview policy: list rules: %w", err)
	}
	groupSet := map[string]struct{}{}
	for _, rule := range rules {
		if _, ok := wanted[rule.ResourceID]; ok {
			groupSet[rule.GroupID] = struct{}{}
		}
	}
	groupIDs := make([]string, 0, len(groupSet))
	for id := range groupSet {
		groupIDs = append(groupIDs, id)
	}
	sort.Strings(groupIDs)
	groupSPIFFEs, err := store.ListActiveDeviceSPIFFEsForGroups(ctx, workspaceID, groupIDs)
	if err != nil {
		return nil, fmt.Errorf("preview policy: group devices: %w", err)
	}
	devices := map[uuid.UUID]string{}
	for _, list := range groupSPIFFEs {
		for _, d := range list {
			devices[d.DeviceID] = d.SPIFFEID
		}
	}

	all, err := postureStore.ListProfiles(ctx, workspaceUUID)
	if err != nil {
		return nil, fmt.Errorf("preview policy: list profiles: %w", err)
	}
	byID := make(map[uuid.UUID]posture.Profile, len(all))
	for _, p := range all {
		byID[p.ID] = p
	}
	required := make([]posture.Profile, 0, len(profileIDs))
	for _, id := range profileIDs {
		p, ok := byID[id]
		if !ok {
			return nil, posture.ErrPolicyProfileMissing
		}
		required = append(required, p)
	}

	deviceIDs := make([]uuid.UUID, 0, len(devices))
	for id := range devices {
		deviceIDs = append(deviceIDs, id)
	}
	evaluations, err := postureStore.EvaluationsForDevices(ctx, workspaceUUID, deviceIDs)
	if err != nil {
		return nil, fmt.Errorf("preview policy: evaluations: %w", err)
	}
	allowed, _, _ := applyPosture(time.Now(), devices, required, evaluations)
	return &PolicyPreview{Devices: len(devices), Passing: len(allowed)}, nil
}
