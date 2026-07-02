package policy

import (
	"context"
	"fmt"
	"net"
	"time"

	clientv1 "github.com/yourorg/ztna/controller/gen/go/proto/client/v1"
	"github.com/yourorg/ztna/controller/internal/appmeta"
)

const defaultRelayPort = "9093"

// CompileACLSnapshot builds a fresh ACLSnapshot for the given workspace.
//
// Routing model: Resource → remote_network_id → ACLRemoteNetwork → connectors[].
// Every referenced Remote Network appears in the snapshot even if it has no
// active connector — those appear with an empty connectors list and clients
// must treat their resources as temporarily unavailable.
//
// Returns an error (and no snapshot) on any DB failure — callers must default-deny.
func CompileACLSnapshot(ctx context.Context, store *Store, notifier *Notifier, workspaceID string) (*clientv1.ACLSnapshot, error) {
	rules, err := store.ListEnabledRulesWithResources(ctx, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("compile acl: list rules: %w", err)
	}

	// Collect unique group IDs and build the authoritative remote-network map
	// from rule rows (which carry RN id+name via JOIN).
	type entryKey struct {
		resourceID string
		address    string
		port       uint32
		protocol   string
	}
	names := make(map[entryKey]string)
	shieldIDs := make(map[entryKey]string)
	preferredConnectorIDs := make(map[entryKey]string)
	routeTypes := make(map[entryKey]string)
	rnByKey := make(map[entryKey]string)     // entryKey → remote_network_id
	keyGroups := make(map[entryKey][]string) // groups contributing to each entry

	groupIDSet := make(map[string]struct{})
	rnNames := make(map[string]string) // remote_network_id → name (authoritative set)

	for _, rule := range rules {
		key := entryKey{rule.ResourceID, rule.Address, rule.Port, rule.Protocol}
		if _, ok := names[key]; !ok {
			routeType, err := routeTypeForResource(rule.Status, rule.ShieldID)
			if err != nil {
				return nil, fmt.Errorf("compile acl: resource %s: %w", rule.ResourceID, err)
			}
			names[key] = rule.Name
			shieldIDs[key] = rule.ShieldID
			preferredConnectorIDs[key] = rule.ShieldConnectorID
			routeTypes[key] = routeType
			rnByKey[key] = rule.RemoteNetworkID
		}
		keyGroups[key] = append(keyGroups[key], rule.GroupID)
		groupIDSet[rule.GroupID] = struct{}{}
		rnNames[rule.RemoteNetworkID] = rule.RemoteNetworkName
	}

	groupIDs := make([]string, 0, len(groupIDSet))
	for id := range groupIDSet {
		groupIDs = append(groupIDs, id)
	}

	rnIDs := make([]string, 0, len(rnNames))
	for id := range rnNames {
		rnIDs = append(rnIDs, id)
	}

	// Batch SPIFFE fetch — one query for all groups.
	groupSPIFFEs, err := store.ListActiveDeviceSPIFFEsForGroups(ctx, workspaceID, groupIDs)
	if err != nil {
		return nil, fmt.Errorf("compile acl: batch spiffes: %w", err)
	}

	// Batch connector fetch — all active connectors for the referenced RNs.
	connectorRows, err := store.GetConnectorsForRemoteNetworks(ctx, rnIDs)
	if err != nil {
		return nil, fmt.Errorf("compile acl: connector lookup: %w", err)
	}

	// Seed ACLRemoteNetwork map from rnNames (every RN starts with empty connectors).
	rnMap := make(map[string]*clientv1.ACLRemoteNetwork, len(rnNames))
	for rnID, rnName := range rnNames {
		rnMap[rnID] = &clientv1.ACLRemoteNetwork{
			RemoteNetworkId: rnID,
			Name:            rnName,
			Connectors:      []*clientv1.ACLConnector{},
		}
	}
	// Populate connectors from query results.
	for _, row := range connectorRows {
		host := row.LanAddr
		if h, _, err := net.SplitHostPort(row.LanAddr); err == nil {
			host = h
		}
		tunnelAddr := ""
		if host != "" {
			tunnelAddr = host + ":9092"
		}
		spiffe := ""
		if row.ConnectorID != "" && row.TrustDomain != "" {
			spiffe = appmeta.ConnectorSPIFFEID(row.TrustDomain, row.ConnectorID)
		}
		relaySpiffe := ""
		if row.RelayID != "" {
			relaySpiffe = appmeta.RelaySPIFFEID(row.RelayID)
		}
		rnMap[row.RemoteNetworkID].Connectors = append(rnMap[row.RemoteNetworkID].Connectors, &clientv1.ACLConnector{
			ConnectorId:         row.ConnectorID,
			ConnectorTunnelAddr: tunnelAddr,
			ConnectorSpiffe:     spiffe,
			RelayAddr:           row.RelayAddr,
			RelaySpiffeId:       relaySpiffe,
		})
	}

	// Build ACL entries, aggregating SPIFFEs per resource across groups.
	spiffeSet := make(map[entryKey]map[string]struct{})
	for key, groups := range keyGroups {
		set := make(map[string]struct{})
		for _, gid := range groups {
			for _, s := range groupSPIFFEs[gid] {
				set[s] = struct{}{}
			}
		}
		spiffeSet[key] = set
	}

	entries := make([]*clientv1.ACLEntry, 0, len(spiffeSet))
	for key, set := range spiffeSet {
		ids := make([]string, 0, len(set))
		for id := range set {
			ids = append(ids, id)
		}
		entries = append(entries, &clientv1.ACLEntry{
			ResourceId:           key.resourceID,
			Name:                 names[key],
			Address:              key.address,
			Port:                 key.port,
			Protocol:             key.protocol,
			AllowedSpiffeIds:     ids,
			RouteType:            routeTypes[key],
			ShieldId:             shieldIDs[key],
			RemoteNetworkId:      rnByKey[key],
			PreferredConnectorId: preferredConnectorIDs[key],
		})
	}

	// Collect remote networks into a stable slice.
	remoteNetworks := make([]*clientv1.ACLRemoteNetwork, 0, len(rnMap))
	for _, rn := range rnMap {
		remoteNetworks = append(remoteNetworks, rn)
	}

	// Relay discovery — workspace-scoped.
	var relayAddr, relaySPIFFEID string
	relay, err := store.GetActiveRelay(ctx)
	if err != nil {
		return nil, fmt.Errorf("compile acl: relay lookup: %w", err)
	}
	if relay != nil {
		switch {
		case relay.PublicAddr != "":
			relayAddr = relay.PublicAddr
		case relay.AddressScope == "public" && relay.ObservedIP != "":
			relayAddr = net.JoinHostPort(relay.ObservedIP, defaultRelayPort)
		}
		if relayAddr != "" {
			relaySPIFFEID = appmeta.RelaySPIFFEID(relay.ID)
		}
	}

	return &clientv1.ACLSnapshot{
		WorkspaceId:    workspaceID,
		Version:        notifier.Version(workspaceID),
		GeneratedAt:    time.Now().Unix(),
		Entries:        entries,
		RemoteNetworks: remoteNetworks,
		RelayAddr:      relayAddr,
		RelaySpiffeId:  relaySPIFFEID,
	}, nil
}

func routeTypeForResource(status, shieldID string) (string, error) {
	switch status {
	case "pending", "unprotected":
		return "connector", nil
	case "protecting", "protected", "failed":
		if shieldID == "" {
			return "", fmt.Errorf("status %q requires a shield", status)
		}
		return "shield", nil
	default:
		return "", fmt.Errorf("unsupported resource status %q", status)
	}
}
