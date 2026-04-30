package policy

import (
	"context"
	"fmt"
	"time"

	clientv1 "github.com/yourorg/ztna/controller/gen/go/proto/client/v1"
)

// CompileACLSnapshot builds a fresh ACLSnapshot for the given workspace by
// walking: enabled access_rules → groups → group members → client device SPIFFE IDs.
//
// Returns an error (and no snapshot) on any DB failure — callers must default-deny.
func CompileACLSnapshot(ctx context.Context, store *Store, notifier *Notifier, workspaceID string) (*clientv1.ACLSnapshot, error) {
	rules, err := store.ListEnabledRulesWithResources(ctx, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("compile acl: list rules: %w", err)
	}

	// Aggregate SPIFFE IDs per resource, deduplicating across groups.
	type entryKey struct {
		resourceID string
		address    string
		port       uint32
		protocol   string
	}
	spiffeSet := make(map[entryKey]map[string]struct{})
	names := make(map[entryKey]string)

	for _, rule := range rules {
		key := entryKey{
			resourceID: rule.ResourceID,
			address:    rule.Address,
			port:       rule.Port,
			protocol:   rule.Protocol,
		}
		if _, ok := spiffeSet[key]; !ok {
			spiffeSet[key] = make(map[string]struct{})
			names[key] = rule.Name
		}

		spiffes, err := store.ListActiveDeviceSPIFFEsForGroup(ctx, workspaceID, rule.GroupID)
		if err != nil {
			return nil, fmt.Errorf("compile acl: spiffes for group %s: %w", rule.GroupID, err)
		}
		for _, s := range spiffes {
			spiffeSet[key][s] = struct{}{}
		}
	}

	entries := make([]*clientv1.ACLEntry, 0, len(spiffeSet))
	for key, set := range spiffeSet {
		ids := make([]string, 0, len(set))
		for id := range set {
			ids = append(ids, id)
		}
		entries = append(entries, &clientv1.ACLEntry{
			ResourceId:       key.resourceID,
			Name:             names[key],
			Address:          key.address,
			Port:             key.port,
			Protocol:         key.protocol,
			AllowedSpiffeIds: ids,
		})
	}

	// Use the notifier's monotonic version so downstream clients can detect
	// policy changes. After a controller restart the counter resets to 0 but
	// increments on the next policy mutation — that is acceptable.
	version := notifier.Version(workspaceID)

	return &clientv1.ACLSnapshot{
		WorkspaceId: workspaceID,
		Version:     version,
		GeneratedAt: time.Now().Unix(),
		Entries:     entries,
	}, nil
}
