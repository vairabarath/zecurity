package policy

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
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
	rnByKey := make(map[entryKey]string) // entryKey → remote_network_id
	hostnames := make(map[entryKey]string)
	resolvers := make(map[entryKey]*clientv1.ACLResolver)
	keyGroups := make(map[entryKey][]string) // groups contributing to each entry

	groupIDSet := make(map[string]struct{})
	rnNames := make(map[string]string) // remote_network_id → name (authoritative set)

	// Drop rules whose resource cannot be routed BEFORE building any entry.
	//
	// This used to `return nil, err` on the first such resource, which took the
	// ENTIRE workspace offline: no snapshot was produced, so every other resource
	// was reported by connectors as `unknown_resource`. Failing closed for the
	// offending resource is correct; failing closed for its neighbours is a blast
	// radius nobody would choose — and the diagnostic named the wrong thing, since
	// the error mentions a resource that is not the one the user cannot reach.
	//
	// The same principle is already documented for `parseResolver`: "a single
	// malformed resolver must degrade one resource, not take every user offline."
	// `routeTypeForResource` simply did not follow it.
	rules, skipped := partitionRoutable(rules)
	for resourceID, reason := range skipped {
		log.Printf("acl compile: workspace %s: SKIPPING resource %s: %s — that resource is unreachable until it is fixed; all others are unaffected", workspaceID, resourceID, reason)
	}

	for _, rule := range rules {
		key := entryKey{rule.ResourceID, rule.Address, rule.Port, rule.Protocol}
		if _, ok := names[key]; !ok {
			// Cannot fail: partitionRoutable has already removed every rule whose
			// status/shield pair is unroutable.
			routeType, _ := routeTypeForResource(rule.Status, rule.ShieldID)
			names[key] = rule.Name
			shieldIDs[key] = rule.ShieldID
			preferredConnectorIDs[key] = rule.ShieldConnectorID
			routeTypes[key] = routeType
			rnByKey[key] = rule.RemoteNetworkID
			hostnames[key] = rule.Hostname
			resolvers[key] = parseResolver(rule.Resolver)
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
			// JoinHostPort brackets IPv6 correctly (e.g. "[2001:db8::1]:9092").
			tunnelAddr = net.JoinHostPort(host, "9092")
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
			RelayAddr:           resolveConnectorRelayAddr(row.RelayPublicAddr, row.RelayObservedHost),
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
			Hostname:             hostnames[key],
			Resolver:             resolvers[key],
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

// resolveConnectorRelayAddr builds the per-connector relay address for an
// ACLConnector. A configured public_addr wins as-is; otherwise the observed IP
// of a public-scope relay is joined with the default relay port via
// net.JoinHostPort so IPv6 is bracketed correctly (e.g. "[2001:db8::1]:9093").
// Returns "" when the connector has no relay coordinates.
func resolveConnectorRelayAddr(publicAddr, observedHost string) string {
	if publicAddr != "" {
		return publicAddr
	}
	if observedHost != "" {
		return net.JoinHostPort(observedHost, defaultRelayPort)
	}
	return ""
}

// parseResolver converts the resolver JSON stored on a resource into its proto
// form. The stored shape is {"type": "...", "config": {"k": "v", ...}}; the DB
// only guarantees the "type" key exists (resources_resolver_shape_check).
//
// A malformed, empty, or type-less value yields nil rather than an error. This
// is deliberate: CompileACLSnapshot returning an error makes the caller
// default-deny the ENTIRE workspace, so one bad resolver would take every user
// offline. Returning nil instead confines the blast radius to the one resource
// — the connector treats an absent resolver on a hostname-addressed entry as
// unresolvable and denies just that dial.
func parseResolver(raw string) *clientv1.ACLResolver {
	if raw == "" {
		return nil
	}
	var v struct {
		Type   string            `json:"type"`
		Config map[string]string `json:"config"`
	}
	if err := json.Unmarshal([]byte(raw), &v); err != nil || v.Type == "" {
		return nil
	}
	return &clientv1.ACLResolver{Type: v.Type, Config: v.Config}
}

// partitionRoutable splits rules into those that can be compiled and those that
// must be skipped, keyed by resource ID with a human-readable reason.
//
// Pure and self-contained on purpose: `Store` wraps a *pgxpool.Pool, so anything
// that touches it is DB-gated. The decision that determines whether a workspace
// stays online should not be.
//
// A resource may appear on several rules (one per group). All of its rules are
// dropped together — keeping some would leave `keyGroups` holding a key that has
// no corresponding entry.
func partitionRoutable(rules []*CompilerResourceRow) ([]*CompilerResourceRow, map[string]string) {
	skipped := make(map[string]string)
	// First pass: decide per resource, so the reason is recorded once even when the
	// resource has many rules.
	for _, rule := range rules {
		if _, seen := skipped[rule.ResourceID]; seen {
			continue
		}
		if _, err := routeTypeForResource(rule.Status, rule.ShieldID); err != nil {
			skipped[rule.ResourceID] = err.Error()
		}
	}
	if len(skipped) == 0 {
		return rules, skipped
	}
	kept := make([]*CompilerResourceRow, 0, len(rules))
	for _, rule := range rules {
		if _, bad := skipped[rule.ResourceID]; !bad {
			kept = append(kept, rule)
		}
	}
	return kept, skipped
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
